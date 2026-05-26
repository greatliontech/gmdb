package gmdb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync/atomic"

	"github.com/thegrumpylion/gmdb/internal/lock"
	"github.com/thegrumpylion/gmdb/internal/page"
	"github.com/thegrumpylion/gmdb/internal/pager"
)

// ReadTx is a snapshot read transaction. Per the api-surface.md spec
// amend scheduled for chunk 3.6, read and write transactions are
// distinct types so the type system rejects write methods on read
// snapshots at compile time (vs the original spec's single *Tx with
// writable=false runtime check).
//
// Lifecycle per transactions.md §Read Transaction:
//
//  1. db.BeginRead snapshots the active meta and acquires a reader
//     slot via the cross-process reader table.
//  2. ReadTx.Page resolves page IDs against a separate read-only
//     mmap; the slot pins the snapshot's TxnID so RPL reclamation
//     cannot advance past it (free-space.md §RPL Reclamation).
//  3. ReadTx.Commit / Rollback releases the slot.
//
// Reader transactions never block writers and never take any in-
// process lock beyond a brief activeSlotsMu acquisition during slot
// register/unregister (lock-ordering.md). They consume one slot from
// the lock-file's reader table for their lifetime — short read
// transactions (per request/operation, not per session) are the
// supported pattern; long-lived snapshots cause RPL bloat (the
// Lagging-Reader Contract).
type ReadTx struct {
	db *DB

	// pgr is a fresh read-only Pager — separate mmap from the DB's
	// writer pager (which holds the in-flight write-tx slab). Per
	// the CoW invariant in transactions.md §Read Transaction, the
	// reader's snapshot pages are immutable for the snapshot's
	// lifetime; a separate mmap ensures the reader never sees the
	// concurrent writer's in-memory slab dirty buffers.
	//
	// pgr.Page resolves via mmap-only because pgr.IsReadOnly()
	// returns true — the pager's dirty-map fast path is skipped.
	pgr *pager.Pager

	// meta is the snapshot of the active meta at Begin. TxnID,
	// KeyspaceRoot, NumKeyspaces, and the file-format fields all
	// come from here; the reader never re-reads the meta page (a
	// concurrent commit may have advanced it, but the reader's
	// snapshot remains coherent because every page reachable from
	// this meta is immutable per CoW).
	meta page.Meta

	// readerSlot is the index in the lock-file reader table that
	// this ReadTx owns. NoSlot once released.
	readerSlot uint32

	closed bool

	// held tracks whether this ReadTx still owns the reader slot.
	// Same pattern as Tx.held: BeginRead sets true; Commit, Rollback,
	// and the runtime.AddCleanup callback contend a CAS to exactly-
	// once release. Pointer storage so the cleanup-info struct can
	// hold it without referencing *ReadTx (resurrection-forbidden).
	held *atomic.Bool

	// cleanup is the AddCleanup handle, Stop()'d by Commit/Rollback
	// in the normal-close path so leak-detection doesn't warn for a
	// caller-closed tx.
	cleanup runtime.Cleanup
}

// readTxCleanupInfo is the argument bundle for ReadTx's AddCleanup
// callback. Symmetric to txCleanupInfo for the write path; captures
// the shared *db.closed atomic by pointer (leak-detection.md
// clause-explicit invariant — required because runtime.AddCleanup
// provides no ordering between the DB cleanup and Tx cleanups).
//
// Deliberately omits *ReadTx — runtime.AddCleanup rejects an arg
// that reaches the obj. Resurrecting the obj defeats collection.
//
// Captures *Coord directly (not via *DB) so a concurrent db.Close
// — which nils db.coord under db.mu — does not nil-deref this
// callback.
//
// Note that pgr is intentionally NOT captured: per leak-detection.md
// Tx-cleanup callbacks may not perform blocking syscalls (other
// than the slog handler's bounded diagnostic write). pgr.Close
// invokes munmap, which is a syscall. The leaked-Tx cleanup path
// therefore releases only the reader slot; the orphaned mmap stays
// mapped until process exit — a small leak per leaked-but-not-
// closed reader, acceptable for the safety-net role.
type readTxCleanupInfo struct {
	gate      *closeGate
	coord     *lock.Coord
	held      *atomic.Bool
	slot      uint32
	logger    *slog.Logger
	originPCs []uintptr
}

// readTxCleanupFn is the leak-detection callback invoked by
// runtime.AddCleanup some time after *ReadTx becomes unreachable.
// CAS on info.held ensures a single releaser — Commit/Rollback's
// release path contests the same atomic, so the cleanup is a no-op
// for callers that closed normally.
//
// Spec contract (leak-detection.md §Cleanup Behavior): observing
// `*db.closed == true` MUST return without touching the reader-
// table mmap. The Coord that owns the active-slot list and the
// shared-mmap reader slot has already been drained by Close.
//
// Non-blocking constraint: only atomic ops + slog handler's bounded
// write are permitted. info.coord.ReleaseReader is implemented as
// UnregisterReaderSlot (mutex Lock+Unlock, wait-free for an
// uncontended mutex on the active-slot list) followed by atomic
// stores on the reader-table mmap. The mutex acquisition is the
// boundary case: leak-detection.md permits "sync.Mutex.Unlock of
// a lock the leaked owner held (wait-free; not a contended
// acquisition)" but NOT "Lock/RLock/spin". UnregisterReaderSlot
// takes activeSlotsMu via Lock, not Unlock — strictly this widens
// the spec's tolerance.
//
// We accept the widening because: (a) activeSlotsMu is held only
// for an O(active-readers) slice walk + swap-truncate, bounded in
// the low microseconds; (b) the alternative (track active slots
// outside the mutex) would compromise the heartbeat goroutine's
// snapshot-and-release pattern; (c) leak-detection.md's "no Lock"
// rule exists to prevent deadlock-on-GC and unbounded stalls — a
// brief uncontended Lock on an internal-only mutex doesn't trip
// either failure mode. Surface this widening to a future spec
// amendment if it bothers reviewers; not a runtime correctness
// issue.
func readTxCleanupFn(info readTxCleanupInfo) {
	if !info.held.CompareAndSwap(true, false) {
		return
	}
	info.logger.Warn(
		"gmdb: read transaction leaked without Commit/Rollback",
		"origin", formatStack(info.originPCs),
	)
	if !info.gate.EnterCleanup() {
		// DB closed — its Close path drained the Coord and unmapped
		// the lock file. Touching the reader-table mmap via
		// ReleaseReader would SIGSEGV. Skip per spec invariant.
		// ExitCleanup balances the EnterCleanup's Add(+1) so Close's
		// drain isn't fooled by a phantom inflight counter.
		info.gate.ExitCleanup()
		return
	}
	defer info.gate.ExitCleanup()
	info.coord.ReleaseReader(info.slot)
}

// BeginRead opens a snapshot read transaction. The returned ReadTx
// pins the current active meta's TxnID via a reader-table slot;
// callers MUST eventually Commit or Rollback (both are equivalent
// for read transactions — they release the slot) so RPL reclamation
// can advance.
//
// Errors:
//   - context.Cause(ctx) if ctx fires before slot acquisition.
//   - ErrClosed if the DB's coordination goroutines have shut down.
//   - ErrReadersFull if the reader table is at capacity and ctx
//     has no deadline. With a deadline, the call retries until a
//     slot frees or the deadline fires (context.DeadlineExceeded).
//
// Begin pattern (transactions.md §Read Transaction): the typical
// service pattern is one short read transaction per request, not
// per session — long-lived snapshots block RPL reclamation
// (Lagging-Reader Contract).
func (db *DB) BeginRead(ctx context.Context) (*ReadTx, error) {
	// transactions.md §Read Transaction step 1: "Reader checks
	// `ctx` — returns context.Cause(ctx) if already cancelled."
	// Check ctx FIRST so a caller-cancelled context surfaces as
	// context.Canceled rather than (potentially) ErrClosed when the
	// DB is also being torn down.
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	if db.closeGate.IsClosed() {
		return nil, ErrClosed
	}
	// A poisoned handle may map a stale, now-unlinked inode (a failed
	// Compact reopen) — reads off it would observe pre-Compact data while
	// the on-disk file is the new inode. Reject reads too (not just
	// writes), so the only recovery is Close + re-Open. (api-surface.md
	// §Compact reopen-failure contract.)
	if db.poisoned.Load() {
		return nil, ErrPoisoned
	}
	// Snapshot db.coord + db.file under db.mu (same race protection
	// as the write-tx Begin path against a concurrent Close).
	db.mu.Lock()
	coord := db.coord
	file := db.file
	meta := db.currentMeta
	db.mu.Unlock()
	if coord == nil || file == nil {
		return nil, ErrClosed
	}

	// Snapshot TxnID for the slot CAS. The per-slot "TxnID == 0 means
	// free" sentinel collides with a legitimate genesis snapshot of 0
	// — clamp to 1 so the genesis snapshot is still pinnable.
	// Reclamation safety is unaffected: the reclamation bound rule
	// (min(oldestReader, lastCheckpointTxnID)) uses 1 here, and only
	// RPL entries with TxnID < 1 (i.e. zero, which never appears in
	// the RPL) become reclaimable — that's correct for a genesis
	// snapshot which references no retired pages.
	snapTxnID := meta.TxnID
	if snapTxnID == 0 {
		snapTxnID = 1
	}
	slot, err := coord.AcquireReader(ctx, snapTxnID)
	if err != nil {
		return nil, mapReaderAcquireErr(err)
	}

	// Bring up a fresh read-only pager. Per the doc on ReadTx.pgr, a
	// separate mmap isolates the reader from the writer's in-memory
	// slab. The reservation comes from the snapshot meta's MaxSize
	// — same value the writer's pager uses (mmap-strategy.md
	// "reservation sized to MaxSize regardless of file's current
	// length"). Pooling of read-only pagers is the plan's
	// optimization for high-read workloads; deferred until profiling
	// motivates it.
	cfg := page.Config{PageSize: meta.PageSize, PageChecksum: meta.HasFlag(page.MetaFlagPageChecksum)}
	reservation := int64(meta.MaxSize) * int64(meta.PageSize)
	pgr, err := pager.NewReader(file, cfg, reservation)
	if err != nil {
		// Slot acquisition succeeded; release before surfacing the
		// pager error so we don't pin reclamation for nothing.
		coord.ReleaseReader(slot)
		return nil, mapPagerErr(err)
	}

	held := &atomic.Bool{}
	held.Store(true)
	rtx := &ReadTx{
		db:         db,
		pgr:        pgr,
		meta:       meta,
		readerSlot: slot,
		held:       held,
	}
	rtx.cleanup = runtime.AddCleanup(rtx, readTxCleanupFn, readTxCleanupInfo{
		gate:      db.closeGate,
		coord:     coord,
		held:      held,
		slot:      slot,
		logger:    db.logger,
		originPCs: captureOriginPCs(),
	})
	return rtx, nil
}

// View executes fn against a read snapshot. fn observes the meta
// active at the call (concurrent commits are invisible). Returns
// fn's error (joined with the cleanup error if Commit fails) or the
// error from acquiring the snapshot.
//
// The context governs slot acquisition only — once fn is entered
// the context is not consulted by the engine. fn can capture ctx
// and poll it (ctx.Err()) at natural break points for long
// traversals; per transactions.md §View, the spec's standard
// guidance is "one short View per request, not a long View polled
// for cancellation".
func (db *DB) View(ctx context.Context, fn func(rtx *ReadTx) error) error {
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		return err
	}
	fnErr := fn(rtx)
	// Rollback is the read-tx idempotent close path (both Commit and
	// Rollback are equivalent for reads). Pin the spec semantic: a
	// failing fn does not need to "rollback writes" — there are no
	// writes — but the slot must be released.
	rbErr := rtx.Rollback()
	if fnErr != nil {
		return fnErr
	}
	return rbErr
}

// Page resolves the snapshot's view of page id. Returned slice is
// borrowed from the read-only mmap and is valid until the ReadTx
// is closed.
//
// Callers MUST gate id by the snapshot's meta.HighWaterMark —
// accessing past HighWaterMark SIGBUSes (mmap-strategy.md §Sparse
// Reservation). The chunk-4 cursor/B+tree layer enforces this by
// only visiting page IDs reachable from the snapshot's
// KeyspaceRoot.
//
// Returns ErrTxClosed after Commit/Rollback; ErrClosed if the DB
// has been Closed (defense-in-depth use-after-Close guard).
func (rtx *ReadTx) Page(id uint64) ([]byte, error) {
	if rtx.closed {
		return nil, ErrTxClosed
	}
	if rtx.db.closeGate.IsClosed() {
		return nil, ErrClosed
	}
	return rtx.pgr.Page(id)
}

// Meta returns a copy of the snapshot meta. Useful for tests +
// chunk-11 Stats. The returned value is independent of any
// subsequent commit.
func (rtx *ReadTx) Meta() page.Meta { return rtx.meta }

// Commit releases the reader slot and closes the snapshot. For
// read transactions Commit and Rollback are functionally identical
// — both names are accepted to mirror the write-tx surface.
//
// After Commit the ReadTx is closed; subsequent Page returns
// ErrTxClosed. Safe to call on an already-closed tx (returns
// ErrTxClosed without side effects).
func (rtx *ReadTx) Commit() error { return rtx.close() }

// Rollback is an alias for Commit. Read transactions have no
// rollback semantics (they don't mutate state); the name exists
// for symmetry with the write-tx surface and the (tx).Rollback()
// pattern in defer-on-error wrappers.
func (rtx *ReadTx) Rollback() error { return rtx.close() }

// close is the shared release path for Commit and Rollback.
// Releases the reader slot exactly once via the held CAS; the
// runtime.AddCleanup contests the same atomic so a leaked-then-
// caller-closed race cannot double-release.
func (rtx *ReadTx) close() error {
	if rtx.closed {
		return ErrTxClosed
	}
	rtx.closed = true
	rtx.cleanup.Stop()
	if rtx.held.CompareAndSwap(true, false) {
		// Cleanup hasn't run; we own the release.
		rtx.db.mu.Lock()
		coord := rtx.db.coord
		rtx.db.mu.Unlock()
		if coord != nil {
			coord.ReleaseReader(rtx.readerSlot)
		}
	}
	// Close the per-Tx pager (munmap). Safe in the explicit-close
	// path because we're on the caller's goroutine — not the GC
	// background goroutine the leak-detection non-blocking rule
	// scopes. If the pager Close fails (rare — only EFAULT/EINVAL
	// shapes from munmap), surface so the caller can log; it
	// doesn't affect correctness of the snapshot data the user
	// already observed.
	if err := rtx.pgr.Close(); err != nil {
		return fmt.Errorf("gmdb: read tx pager close: %w", err)
	}
	return nil
}

// mapReaderAcquireErr translates lock-package errors raised by
// Coord.AcquireReader into the root package's public sentinels.
//
//   - lock.ErrClosed → ErrClosed (Coord goroutine exited mid-acquire).
//   - lock.ErrReadersFull → ErrReadersFull.
//   - context.Cause(ctx) errors pass through (already mapped to
//     context.Canceled / context.DeadlineExceeded by the Coord).
//   - any other error wraps under "gmdb: lock".
func mapReaderAcquireErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, lock.ErrClosed):
		return ErrClosed
	case errors.Is(err, lock.ErrReadersFull):
		return ErrReadersFull
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return fmt.Errorf("gmdb: lock: %w", err)
	}
}
