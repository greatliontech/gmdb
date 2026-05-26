package gmdb

import (
	"context"
	"errors"
	"time"

	"github.com/thegrumpylion/gmdb/internal/lock"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// stopMaintenance cancels the maintenance goroutine and waits for it to
// exit. Called by Close before the Coord / pager teardown so an in-flight
// pass unwinds first (maintCancel aborts a pass blocked in AcquireWriter)
// and no goroutine outlives the unmap (Inv-M6). Idempotent-safe via the
// CAS-guarded Close; a no-op when maintenance was disabled.
func (db *DB) stopMaintenance() {
	if !db.maintStarted {
		return
	}
	db.maintCancel()
	<-db.maintDone
}

// maintenanceLoop is the per-DB background maintenance goroutine
// (background-maintenance.md). It runs a pass every Interval (each pass
// self-coordinates cross-process via LastMaintenanceTime, so the global
// rate is ≤1 pass / Interval — Inv-M1) until ctx is cancelled by Close.
// When immediate is set (an unclean prior shutdown), the first pass runs at
// startup instead of waiting a full interval.
func (db *DB) maintenanceLoop(ctx context.Context, immediate bool) {
	defer close(db.maintDone)
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
// it — Inv-M1), then runs the enabled tasks. A pass is skipped entirely on
// a closing or poisoned handle. The captured coord / lockFile stay valid
// for the pass's duration because Close blocks in stopMaintenance (waiting
// for this goroutine) before tearing them down (Inv-M6).
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
	// (Tasks 3/4 added in later sub-chunks.)
	if err := coord.ReapStaleReaderSlots(ctx); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, lock.ErrClosed) {
		// Closing / cancelled handles are expected and silent; anything
		// else (e.g. a raw flock() syscall failure) is abnormal — log it,
		// matching Task 1's discipline. The next pass retries regardless.
		db.logger.Warn("gmdb: maintenance stale-reader cleanup skipped", "err", err)
	}
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
// two phases (Inv-M2). As with Repair, reclamation is gated on a clean walk
// (no structural CheckError/CheckFatal): a walk-aborting corrupt subtree
// would leave its live pages unvisited and thus mis-classified as leaked,
// so on any structural finding the pass reclaims nothing and logs.
func (db *DB) maintReclaimLeaks(ctx context.Context) {
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		return // closing / cancelled — skip silently
	}
	meta := rtx.Meta()
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

	tx, err := db.Begin(ctx, true)
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
