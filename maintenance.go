package gmdb

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
	"weak"

	"github.com/thegrumpylion/gmdb/internal/pager"
	"github.com/thegrumpylion/gmdb/internal/verify"

	"github.com/thegrumpylion/gmdb/internal/lock"
	"github.com/thegrumpylion/gmdb/internal/page"
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

	// scrubCursor is the next page id the checksum scrubber verifies,
	// wrapping at HighWaterMark (background-maintenance.md §Checksum
	// Scrubbing). Touched only by the maintenance goroutine.
	scrubCursor uint64
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
	// §Stale Reader Slot Cleanup). Acquire LOCK_EX (via the coord) and
	// run the reader-table stale-clear scan. This is NOT lock-free: the
	// clear races peer clearers (a writer's RPL-reclamation scan,
	// stale-writer recovery) without LOCK_EX, which could evict a live
	// reader's slot and let RPL reclamation free pages it is still
	// reading. No write transaction is taken — clearing a slot is a
	// lock-file mmap store, independent of the data file. Errors (ctx
	// cancel on Close, coord closed) are benign: the next pass retries.
	if err := coord.ReapStaleReaderSlots(ctx); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, lock.ErrClosed) {
		// Closing / cancelled handles are expected and silent; anything
		// else (e.g. a raw flock() syscall failure) is abnormal — log it,
		// matching Task 1's discipline. The next pass retries regardless.
		db.logger.Warn("gmdb: maintenance stale-reader cleanup skipped", "err", err)
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

// maintScrubChecksums verifies the xxhash64 footers of a bounded batch of
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
// Footer-bearing gate: only pages the engine guarantees carry a footer are
// verified — allocated pages (the snapshot bitmap's bit is clear) in
// [firstData, hwm). The meta/bitmap region (< firstData) carries no
// xxhash64 footer (checksums.md §Storage), and a free page holds no valid
// footer; verifying either would emit a spurious BadPageChecksum per page
// on any non-full database, flooding the log and burying real bitrot.
//
// The cursor scans the page-id space (free ids are advanced over, not
// verified), wrapping at hwm; a full cycle covers the data region over
// ceil((hwm-firstData)/ScrubBatchSize) passes.
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
	c := &verify.Checker{Pgr: rtx.pgr, Cfg: page.Config{PageSize: pageSize, PageChecksum: true}, Meta: meta}
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
	// Clamp the persistent cursor into [firstData, hwm): the data region's
	// bounds can move between passes (hwm grows, BitmapPages changes).
	cursor := db.maint.scrubCursor
	if cursor < firstData || cursor >= hwm {
		cursor = firstData
	}
	// Scan at most the whole data region once per pass — for a region
	// smaller than the batch this avoids re-verifying the same pages
	// repeatedly within one pass (one pass = one full cycle, per spec).
	span := hwm - firstData
	nScan := min(uint64(db.opts.Maintenance.ScrubBatchSize), span)
	for range nScan {
		if ctx.Err() != nil {
			break // Close / cancel — persist progress and stop
		}
		id := cursor
		cursor++
		if cursor >= hwm {
			cursor = firstData
		}
		if bm.IsSet(id) {
			continue // free page (bit set) — no valid footer
		}
		if !page.VerifyPageFooter(rtx.pgr.PageRaw(id), pageSize) {
			// Re-verify once: a newer concurrent writer's in-flight pwrite of
			// a page it allocated below the snapshot's hwm can be observed
			// torn through the live mmap. A transient torn read clears on
			// re-read; genuine bitrot (or an unwritten allocated page, which
			// has no footer) persists.
			if !page.VerifyPageFooter(rtx.pgr.PageRaw(id), pageSize) {
				// Report-only and logged with page id (background-maintenance.md §Invariants).
				db.logger.Warn("gmdb: scrub detected bad page checksum", "page", id)
			}
		}
	}
	db.maint.scrubCursor = cursor
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
