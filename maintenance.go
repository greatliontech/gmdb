package gmdb

import (
	"context"
	"errors"
	"time"

	"github.com/thegrumpylion/gmdb/internal/lock"
	"github.com/thegrumpylion/gmdb/internal/page"
)

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
func (db *DB) maintenanceLoop(ctx context.Context, immediate bool) {
	defer close(db.maint.done)
	if immediate {
		db.runMaintenancePass(ctx)
	}
	t := time.NewTicker(db.opts.Maintenance.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			db.runMaintenancePass(ctx)
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
	// (Task 4 added in a later sub-chunk.)
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
	if !meta.HasFlag(page.MetaFlagPageChecksum) {
		return // checksums disabled — no footers to verify
	}
	pageSize := meta.PageSize
	c := &checker{pgr: rtx.pgr, cfg: page.Config{PageSize: pageSize, PageChecksum: true}, meta: meta}
	bm, ok := c.snapshotBitmap()
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
// page's bitmap bit is clear (allocated), so no allocator can hand it out
// and no writer can reference it — it cannot become un-leaked between the
// two phases (background-maintenance.md §Invariants). As with Repair, reclamation is gated on a clean walk
// (no structural CheckError/CheckFatal): a walk-aborting corrupt subtree
// would leave its live pages unvisited and thus mis-classified as leaked,
// so on any structural finding the pass reclaims nothing and logs.
func (db *DB) maintReclaimLeaks(ctx context.Context) {
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		return // closing / cancelled — skip silently
	}
	meta := rtx.meta
	c := &checker{
		pgr:    rtx.pgr,
		cfg:    page.Config{PageSize: meta.PageSize, PageChecksum: meta.HasFlag(page.MetaFlagPageChecksum)},
		meta:   meta,
		yield:  func(CheckIssue) bool { return true }, // detection only — discard issues
		repair: true,                                  // collect c.leaked instead of emitting
	}
	c.run()
	leaked, sawError, stopped := c.leaked, c.sawError, c.stopped
	_ = rtx.Rollback()

	if stopped || sawError {
		// Structural issues present: the reachable set is unreliable, so
		// reclaiming "leaked" pages could free live ones. Skip + log.
		db.logger.Warn("gmdb: maintenance leak reclamation skipped — structural issues present in the snapshot")
		return
	}
	if len(leaked) == 0 {
		return
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return // closing / cancelled / poisoned — skip silently
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	freed := 0
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
		return // nothing actually freed; defer rolls back
	}
	if err := tx.Commit(); err != nil {
		db.logger.Warn("gmdb: maintenance leak reclamation commit failed", "err", err)
		return
	}
	committed = true
	db.logger.Info("gmdb: maintenance reclaimed leaked pages", "count", freed)
}
