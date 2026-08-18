package gmdb

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
	"weak"

	"github.com/greatliontech/gmdb/internal/bitmap"
	"github.com/greatliontech/gmdb/internal/pager"
	"github.com/greatliontech/gmdb/internal/verify"

	"github.com/greatliontech/gmdb/internal/lock"
	"github.com/greatliontech/gmdb/internal/page"
	"github.com/zeebo/xxh3"
)

// maintDetectHookForTest fires inside maintReclaimLeaks's detection
// window (after the snapshot read tx begins, before the walk) — the
// seam for injecting a racing commit.
var maintDetectHookForTest atomic.Pointer[func()]

// maintPreReclaimHookForTest fires between detection (read tx closed,
// leaked set computed) and the reclamation Begin — the seam for
// injecting a Compact or realigning commits.
var maintPreReclaimHookForTest atomic.Pointer[func()]

// maintenance holds the background-maintenance goroutine's lifecycle
// state (background-maintenance.md). Started at Open unless
// Options.Maintenance.Disable; stopped by Close (stopMaintenance)
// before the Coord / pager teardown (leak-detection.md §Close() Ordering). cancel aborts a pass
// blocked in AcquireWriter so it unwinds on Close. Single start (Open)
// + single stop (CAS-guarded Close) ⇒ no lifecycle mutex needed.
type maintenance struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	started bool

	// scrubAnchor is the checksum scrubber's cursor ANCHOR — the id and
	// content digest of the last allocated object the previous pass
	// examined (background-maintenance.md §Checksum Scrubbing, Cursor
	// re-anchoring). Follower pages of an overflow run are classifiable
	// only by scanning forward from a position known not to be inside a
	// run, so a bare next-page id is unsound across passes: the anchor
	// is revalidated against the new snapshot at pass start and the
	// scan resumes at its end + 1; an invalid anchor silently restarts
	// the cycle from the first data page. Touched only by the
	// maintenance goroutine.
	scrubAnchor scrubAnchor
}

// scrubAnchor identifies the last object the scrubber examined —
// verified or reported — as a node/RPL page (digest = XXH3-64 over the
// full page bytes, footer included) or a whole overflow run (digest =
// the recomputed whole-run digest, equal to the head-resident stored
// value when the run verified). Recording the digest even on failure
// lets the next pass resume PAST a persistently corrupt object (one
// warning per cycle) instead of pinning the cycle on it. Zero value =
// no anchor (cycle starts at firstData).
type scrubAnchor struct {
	valid bool
	id    uint64
	isRun bool
	// digest pins the anchor's content: a later pass revalidates by
	// recomputing over the page — or over the run range the head's
	// CURRENT AdditionalPages describes — and comparing. A run
	// absorbing the anchor position must overwrite the anchor's own
	// bytes and thereby change this digest (up to content mimicry,
	// the accepted residual).
	digest uint64
}

// stopMaintenance cancels the maintenance goroutine and waits for it to
// exit. Called by Close before the Coord / pager teardown so an in-flight
// pass unwinds first (maint.cancel aborts a pass blocked in AcquireWriter)
// and no goroutine outlives the unmap (leak-detection.md §Close() Ordering). Idempotent-safe via the
// CAS-guarded Close; a no-op when maintenance was disabled.
func (db *DB) stopMaintenance() {
	if !db.maint.started {
		return
	}
	db.maint.cancel()
	<-db.maint.done
}

// maintenanceLoop is the per-DB background maintenance goroutine
// (background-maintenance.md). It runs a pass every Interval (each pass
// self-coordinates cross-process via LastMaintenanceTime, so the global
// rate is ≤1 pass / Interval — background-maintenance.md §Invariants) until ctx is cancelled by Close.
// When immediate is set (an unclean prior shutdown), the first pass runs at
// startup instead of waiting a full interval.
// maintenanceLoop holds the *DB WEAKLY (leak-detection.md §Database
// Handle Leak Detection): a strong receiver would pin an abandoned
// handle reachable forever, making the DB leak cleanup structurally
// unable to fire under default options. The loop takes a strong
// reference only for the duration of one pass — a mid-pass handle is
// reachable by definition, so the cleanup can never race a pass —
// and exits when the handle was collected (Value() == nil; the
// cleanup's teardown owns the resources from there). done/interval
// are passed by value for the same reason.
func maintenanceLoop(wp weak.Pointer[DB], ctx context.Context, done chan struct{}, interval time.Duration, immediate bool) {
	defer close(done)
	pass := func() bool {
		db := wp.Value()
		if db == nil {
			return false // handle collected: the DB cleanup tears down
		}
		db.runMaintenancePass(ctx)
		return true
	}
	if immediate {
		if !pass() {
			return
		}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !pass() {
				return
			}
		}
	}
}

// runMaintenancePass runs one maintenance pass: it claims this interval's
// slot cross-process (skipping if another process or a recent pass holds
// it — background-maintenance.md §Invariants), then runs the enabled tasks. A pass is skipped entirely on
// a closing or poisoned handle. The captured coord / lockFile stay valid
// for the pass's duration because Close blocks in stopMaintenance (waiting
// for this goroutine) before tearing them down (leak-detection.md §Close() Ordering).
func (db *DB) runMaintenancePass(ctx context.Context) {
	db.mu.Lock()
	coord := db.coord
	lockFile := db.lockFile
	db.mu.Unlock()
	if coord == nil || lockFile == nil || db.closeGate.IsClosed() || db.poisoned.Load() {
		return
	}
	now := coord.Clock()
	intervalNanos := uint64(db.opts.Maintenance.Interval)
	if !lockFile.TryClaimMaintenance(now, intervalNanos) {
		return // a recent pass (this or another process) holds the interval
	}
	// Task 1 — bitmap leak reclamation.
	db.maintReclaimLeaks(ctx)

	// Task 2 — stale reader-slot cleanup (background-maintenance.md
	// §Stale Reader Slot Cleanup),
	// probe-based: the slot lock itself serializes clearers, so no
	// write grant is taken and no live reader can be evicted (a live
	// owner holds the very lock the probe needs). No write
	// transaction is taken — clearing a slot is a lock-file mmap
	// store under the held probe. Errors (ctx cancel on Close) are
	// benign: the next pass retries.
	if _, undecided, err := coord.ReapStaleReaderSlots(ctx); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, lock.ErrClosed) {
		// Closing / cancelled handles are expected and silent; anything
		// else (e.g. a raw flock() syscall failure) is abnormal — log it,
		// matching Task 1's discipline. The next pass retries regardless.
		db.logger.Warn("gmdb: maintenance stale-reader cleanup skipped", "err", err)
	} else if undecided > 0 {
		// An occupied slot whose probe errors can be neither judged
		// live nor cleared; its residue pins the RPL reclamation bound
		// (conservative — never an eviction). Persistent counts here
		// mean an unprobeable slot (e.g. an externally removed slot
		// file under nonzero residue) is silently halting reclamation
		// — surface it every pass rather than let the file grow
		// without a trace.
		db.logger.Warn("gmdb: reader-slot probes undecided; a stale slot may be pinning page reclamation", "slots", undecided)
	}

	// Task 3 — checksum scrubbing (background-maintenance.md §Checksum
	// Scrubbing). Read-only; verifies a batch of page footers and reports
	// (never repairs) any mismatch.
	db.maintScrubChecksums(ctx)

	// Task 4 — incremental compaction (background-maintenance.md §Incremental
	// Compaction). When the contiguous-allocation fragmentation rate exceeds
	// CompactionThreshold, relocates a budgeted batch of high-watermark pages
	// to consolidate free space (background-maintenance.md §Invariants: never surfaces ErrTxTooLarge).
	db.maintCompact(ctx)
}

// maintScrubChecksums verifies the XXH3-64 footers of a bounded batch of
// allocated data pages, advancing a persistent cursor across passes
// (background-maintenance.md §Checksum Scrubbing). Its purpose is to catch
// silent bitrot proactively — before a user transaction reads the page and
// hits ErrBadPageChecksum.
//
// Read-only and report-only (background-maintenance.md §Invariants): a mismatch is logged as a
// CheckWarning carrying the page id (background-maintenance.md §Invariants) and nothing is rewritten —
// repair is the explicit CheckWithOptions(Repair) / CopyTo(compact=true)
// path. Skipped entirely when PageChecksum is disabled.
//
// Footer-bearing gate: only pages the engine guarantees carry a checksum
// are verified — allocated pages (the snapshot bitmap's bit is clear) in
// [firstData, hwm). The meta/bitmap region (< firstData) carries no
// XXH3-64 footer (checksums.md §Storage), and a free page holds no valid
// footer; verifying either would emit a spurious BadPageChecksum per page
// on any non-full database, flooding the log and burying real bitrot. An
// allocated page whose type byte is TypeOverflow heads an overflow run:
// the run is verified STANDALONE by its head-resident whole-run digest
// over the AdditionalPages-determined content range (checksums.md
// §Overflow-Run Digest — no referencing cell needed) and the scan
// advances past the follower pages, which carry neither header nor
// footer and are identifiable only from their head.
//
// Cursor re-anchoring (background-maintenance.md §Checksum Scrubbing):
// because followers are classifiable only by scanning forward from a
// position proven not to be inside a run, the persistent cursor is an
// ANCHOR — id + digest of the last examined object — revalidated at pass
// start; the pass resumes at the anchor's end + 1, and an invalid anchor
// silently restarts the cycle from firstData (never a follower: a
// follower always sits above its head). Content mimicry (extent bytes
// byte-replicating the old anchor image) revalidating a stale anchor is
// the accepted residual: report-only subsystem, bounded spurious
// warnings, Check remains the authority.
//
// ScrubBatchSize is a verification target, not a bound: free ids are
// advanced over without consuming budget (a free window larger than the
// batch would otherwise pin the anchor and starve the region's tail —
// the anchor only advances on examined objects), a pass never ends
// inside a run, and total iteration is capped at one full cycle of the
// data region.
//
// Best-effort: the allocated/free gate uses a bitmap snapshot copied once
// at pass start (consistent for the whole pass), but page content is read
// live through the read tx's mmap. Pages allocated in this reader's snapshot
// are stable (reachable
// ones are pinned by the read tx's slot; leaked ones are absent from the
// RPL, so neither is reclaimed under the reader). A page a *newer*
// concurrent writer allocates in a hole below the snapshot's hwm is not
// pinned, so its in-flight pwrite can be observed torn; a mismatch is
// therefore re-verified once (a transient torn read clears on re-read)
// before warning. Genuine bitrot — or a page allocated via Tx.AllocPage and
// committed unwritten, which carries no footer — persists across the
// re-read and is reported truthfully. Check / CheckWithOptions(Repair) is
// the authority for confirming and repairing.
func (db *DB) maintScrubChecksums(ctx context.Context) {
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		return // closing / cancelled / poisoned — skip silently
	}
	defer rtx.Rollback()
	meta := rtx.meta
	if !meta.HasFlag(pager.MetaFlagPageChecksum) {
		return // checksums disabled — no footers to verify
	}
	pageSize := meta.PageSize
	cfg := page.Config{PageSize: pageSize, PageChecksum: true}
	c := &verify.Checker{Pgr: rtx.pgr, Cfg: cfg, Meta: meta}
	bm, ok := c.SnapshotBitmap()
	if !ok {
		return // no bitmap pages — empty database
	}
	firstData := uint64(2) + uint64(meta.BitmapPages)
	hwm := meta.HighWaterMark
	// Clamp hwm to the file-resident extent (same defence as checker.run):
	// a forged/over-large hwm must not drive PageRaw past the mapped file.
	if bound := min(uint64(rtx.pgr.FileSize())/uint64(pageSize), meta.MaxSize); hwm > bound {
		hwm = bound
	}
	if hwm <= firstData {
		return // no data pages to scrub
	}

	// Revalidate the anchor against the new snapshot; resume at its
	// end + 1, or restart the cycle at firstData (silently — an
	// invalidated anchor is expected under churn, not a fault).
	cursor := firstData
	if a := db.maint.scrubAnchor; a.valid {
		if end, ok := revalidateScrubAnchor(rtx.pgr, cfg, bm, a, firstData, hwm); ok {
			cursor = end
			if cursor >= hwm {
				cursor = firstData
			}
		}
	}

	// nScan verified-object pages per pass (target); span caps total
	// iteration at one full cycle of the data region.
	span := hwm - firstData
	nScan := min(uint64(db.opts.Maintenance.ScrubBatchSize), span)
	verified := uint64(0)
	anchor := db.maint.scrubAnchor
	for iter := uint64(0); iter < span && verified < nScan; iter++ {
		if ctx.Err() != nil {
			break // Close / cancel — persist progress and stop
		}
		id := cursor
		if bm.IsSet(id) {
			// Free page (bit set) — no valid checksum; skipped without
			// consuming verification budget.
			cursor++
			if cursor >= hwm {
				cursor = firstData
			}
			continue
		}
		buf := rtx.pgr.PageRaw(id)
		if buf[0] == page.TypeOverflow {
			// Overflow-run head: verify the whole run standalone and
			// advance past its followers ("a pass never ends inside a
			// run" — the run is one indivisible scan quantum).
			adv, v := db.scrubRun(rtx, cfg, id, hwm, &anchor)
			verified += v
			cursor += adv
		} else {
			if !scrubNodePage(rtx, pageSize, id) {
				// Report-only and logged with page id (background-maintenance.md §Invariants).
				db.logger.Warn("gmdb: scrub detected bad page checksum", "page", id)
			}
			// Anchor on the examined object regardless of outcome —
			// the digest pins the CURRENT content, so a persistently
			// corrupt page is resumed past (re-warned once per cycle,
			// not once per pass) instead of pinning the cycle and
			// starving everything behind it.
			anchor = scrubAnchor{valid: true, id: id, digest: xxh3.Hash(buf)}
			verified++
			cursor++
		}
		if cursor >= hwm {
			cursor = firstData
		}
	}
	db.maint.scrubAnchor = anchor
}

// scrubNodePage footer-verifies one node/RPL page with the
// re-verify-once discipline (a newer concurrent writer's in-flight
// pwrite of a page it allocated below the snapshot's hwm can be
// observed torn through the live mmap; a transient torn read clears on
// re-read — genuine bitrot, or an unwritten allocated page, persists).
func scrubNodePage(rtx *ReadTx, pageSize uint32, id uint64) bool {
	if page.VerifyPageFooter(rtx.pgr.PageRaw(id), pageSize) {
		return true
	}
	return page.VerifyPageFooter(rtx.pgr.PageRaw(id), pageSize)
}

// scrubRun digest-verifies the overflow run headed at id, anchoring on
// the examined object whether or not it verified (a persistently
// corrupt run must not pin the cycle — see the node branch). Returns
// the pages to advance the cursor by and the count charged against the
// verification budget. A run whose claimed extent overruns the
// snapshot region is reported and advanced over as a single page — its
// AdditionalPages is untrustworthy, so the followers cannot be
// classified; the head anchors as a bare page (whole-page hash) and
// the next objects re-align at the following pass (the accepted
// best-effort residual).
func (db *DB) scrubRun(rtx *ReadTx, cfg page.Config, id, hwm uint64, anchor *scrubAnchor) (advance, verified uint64) {
	head := rtx.pgr.PageRaw(id)
	_, _, _, additional := page.ReadHeader(head)
	if id+1+uint64(additional) > hwm {
		db.logger.Warn("gmdb: scrub detected overflow run overrunning the data region",
			"page", id, "runPages", 1+uint64(additional))
		*anchor = scrubAnchor{valid: true, id: id, digest: xxh3.Hash(head)}
		return 1, 1
	}
	// Each attempt re-fetches the run so a torn first header read
	// (in-flight pwrite of a page a newer writer allocated below the
	// snapshot's hwm) re-reads AdditionalPages too, not just content —
	// the same torn-read re-verify discipline as node pages.
	var run []byte
	attempt := func() bool {
		r, err := rtx.pgr.PageRunRaw(id)
		if err != nil {
			return false
		}
		run = r
		return page.VerifyOverflowRun(r, cfg)
	}
	if !(attempt() || attempt()) {
		if run == nil {
			db.logger.Warn("gmdb: scrub detected unreadable overflow run", "page", id)
		} else {
			db.logger.Warn("gmdb: scrub detected bad overflow-run digest", "page", id)
		}
	}
	if run == nil {
		// PageRunRaw itself failed (bounds/forgery) — anchor the head
		// as a bare page and advance one; nothing more is classifiable.
		*anchor = scrubAnchor{valid: true, id: id, digest: xxh3.Hash(head)}
		return 1, 1
	}
	// Anchor with the recomputed (current-content) digest — equal to
	// the stored digest when the run verified — and advance by the
	// fetched image's true length (the freshest AdditionalPages view).
	*anchor = scrubAnchor{valid: true, id: id, isRun: true,
		digest: page.OverflowRunDigest(run, cfg)}
	total := uint64(len(run)) / uint64(cfg.PageSize)
	return total, total
}

// revalidateScrubAnchor checks a previous pass's anchor against the
// current snapshot: still inside the data region, still allocated, and
// its digest — recomputed over the page, or over the run range the
// head's CURRENT AdditionalPages describes — unchanged. On success the
// scan may resume at the returned end position (anchor end + 1): a run
// absorbing that boundary would have had to overwrite the anchor's own
// bytes and change the digest (background-maintenance.md §Checksum
// Scrubbing, up to content mimicry).
func revalidateScrubAnchor(pgr *pager.Pager, cfg page.Config, bm *bitmap.Bitmap, a scrubAnchor, firstData, hwm uint64) (end uint64, ok bool) {
	if a.id < firstData || a.id >= hwm || bm.IsSet(a.id) {
		return 0, false
	}
	head := pgr.PageRaw(a.id)
	if a.isRun {
		if head[0] != page.TypeOverflow {
			return 0, false
		}
		_, _, _, additional := page.ReadHeader(head)
		total := 1 + uint64(additional)
		if a.id+total > hwm {
			return 0, false
		}
		run, err := pgr.PageRunRaw(a.id)
		if err != nil || page.OverflowRunDigest(run, cfg) != a.digest {
			return 0, false
		}
		return a.id + total, true
	}
	if xxh3.Hash(head) != a.digest {
		return 0, false
	}
	return a.id + 1, true
}

// maintReclaimLeaks reclaims bitmap-leaked pages — allocated in the bitmap
// but unreferenced by any tree and absent from the RPL — in two phases
// (background-maintenance.md §Bitmap Leak Reclamation):
//
//  1. Detection (read snapshot, non-blocking): run the Check structural
//     walk in collect-leaks mode to derive the leaked-page set from the
//     snapshot's immutable tree.
//  2. Reclamation (write tx): free exactly that set in the bitmap.
//
// Unlike CheckWithOptions(Repair) this needs NO exclusive access: a leaked
// page's bitmap bit is clear (allocated), so no allocator can hand it
// out — but that argument only covers pages already leaked at snapshot
// time. The classification itself reads the LIVE bitmap, so a commit
// landing inside the detection window makes fresh allocations look
// leaked; the reclamation tx's snapshot-currency guard (TxnID
// equality plus writer-pager identity, under the write grant) is
// what makes the set trustworthy (background-maintenance.md
// §Invariants). As with Repair, reclamation is gated on a clean walk
// (no structural CheckError/CheckFatal) AND an RPL chain walk that
// reached its authoritative tail or a reclaimed boundary: a
// walk-aborting corrupt subtree leaves live pages unvisited, and an
// RPL walk truncated at a corrupt-segment boundary hides still-pending
// segments — both mis-classify live pages as leaked, so the pass
// reclaims nothing and logs.
//
// Returns (freed, discarded): discarded=true means the guard rejected
// a non-empty leaked set.
func (db *DB) maintReclaimLeaks(ctx context.Context) (freed int, discarded bool) {
	db.mu.Lock()
	detPgr := db.pgr // writer-pager identity at detection time
	db.mu.Unlock()
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		return 0, false // closing / cancelled — skip silently
	}
	if hook := maintDetectHookForTest.Load(); hook != nil {
		(*hook)() // test seam: a commit landing inside the detection window
	}
	meta := rtx.meta
	c := &verify.Checker{
		Pgr:    rtx.pgr,
		Cfg:    page.Config{PageSize: meta.PageSize, PageChecksum: meta.HasFlag(pager.MetaFlagPageChecksum)},
		Meta:   meta,
		Yield:  func(verify.Issue) bool { return true }, // detection only — discard issues
		Repair: true,                                    // collect c.Leaked instead of emitting
	}
	c.Run()
	leaked, sawError, stopped, rplBoundary := c.Leaked, c.SawError, c.Stopped, c.RPLBoundary
	_ = rtx.Rollback()

	if stopped || sawError {
		// Structural issues present: the reachable set is unreliable, so
		// reclaiming "leaked" pages could free live ones. Skip + log.
		db.logger.Warn("gmdb: maintenance leak reclamation skipped — structural issues present in the snapshot")
		return 0, false
	}
	if rplBoundary {
		// The RPL walk truncated at a corrupt-segment boundary
		// (footer/decode): segments behind it may still be in the live
		// writer's in-memory chain — their entries classify as leaked
		// only because the walk could not see them pending. Freeing
		// them double-frees once the writer's own reclamation reaches
		// the intact segments (background-maintenance.md §Bitmap Leak
		// Reclamation: the walk must reach the authoritative tail or a
		// reclaimed boundary). Skip; the set becomes reclaimable after
		// the writer quarantines the corrupt segment.
		db.logger.Warn("gmdb: maintenance leak reclamation skipped — RPL chain walk stopped at a corrupt-segment boundary")
		return 0, false
	}
	if len(leaked) == 0 {
		return 0, false
	}

	if hook := maintPreReclaimHookForTest.Load(); hook != nil {
		(*hook)() // test seam: activity between detection and reclamation
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, false // closing / cancelled / poisoned — skip silently
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	// Snapshot-currency guard (background-maintenance.md §Bitmap Leak
	// Reclamation): the leaked classification compared the SNAPSHOT
	// tree against the LIVE bitmap (which has no MVCC — a concurrent
	// commit pwrites it in place), so a page that was a free hole at
	// snapshot time and was allocated into the live tree by any
	// commit since — this process's writers or a peer's, including a
	// peer's own maintenance pass — classifies as leaked and would be
	// freed out from under the live tree. Begin holds the write grant
	// and Resync'd to the latest meta: if any commit landed after the
	// detection snapshot, the set is stale — discard it; the next
	// pass recomputes. While the grant is held no further commit can
	// land, so equality here makes the set exact.
	db.mu.Lock()
	cur := db.currentMeta.TxnID
	curPgr := db.pgr
	db.mu.Unlock()
	// The pager-identity term catches a same-process Compact() that
	// completed between detection and this Begin: Compact rebuilds
	// the file densely and RESETS TxnID (fresh MVCC counter), so
	// TxnID comparison alone could spuriously pass while every
	// detected id now names a different page. (A peer process's
	// Compact replaces the inode out from under this handle — that
	// divergence is governed by Compact's own cross-process
	// contract, not this guard.)
	if cur != meta.TxnID || curPgr != detPgr {
		db.logger.Info("gmdb: maintenance leak reclamation discarded — commits or a compaction landed during detection",
			"detectedAt", meta.TxnID, "current", cur)
		return 0, true
	}
	for _, id := range leaked {
		if err := tx.pgr.FreeLeakedPage(id); err != nil {
			// Stale leak entry (a concurrent reclaimer, or the page is no
			// longer allocated) — skip it, keep going.
			db.logger.Warn("gmdb: maintenance skip leaked page", "page", id, "err", err)
			continue
		}
		freed++
	}
	if freed == 0 {
		return 0, false // nothing actually freed; defer rolls back
	}
	if err := tx.Commit(); err != nil {
		db.logger.Warn("gmdb: maintenance leak reclamation commit failed", "err", err)
		return 0, false
	}
	committed = true
	db.logger.Info("gmdb: maintenance reclaimed leaked pages", "count", freed)
	return freed, false
}
