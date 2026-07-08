package lock

import (
	"context"
	"errors"
	"time"
)

// staleTimeoutNanos converts this Coord's effective StaleTimeout to the
// uint64 nanoseconds the reader-table helpers expect. Per
// cross-process.md §Heartbeat Goroutine the default is 10 s
// (DefaultStaleTimeout), tunable via Options.StaleTimeout /
// CoordOptions.StaleTimeout; the reader-stale-detection callers
// (OldestReaderTxnID, and ReapStaleReaderSlots through it) all reuse
// this one value so a single per-process window governs eviction.
func (c *Coord) staleTimeoutNanos() uint64 {
	return uint64(c.staleTimeout / time.Nanosecond)
}

// crossNSTimeoutNanos is the cross-namespace classification window
// (cross-process.md §Stale-reader detection, cross-namespace window)
// in the uint64 nanoseconds the classification helpers expect.
func (c *Coord) crossNSTimeoutNanos() uint64 {
	return uint64(c.crossNSTimeout / time.Nanosecond)
}

// AcquireReader is the Coord-mediated reader-slot acquisition path.
// Composes File.AcquireReaderSlot with the bookkeeping the heartbeat
// goroutine needs (active-slot list registration) and the per-Coord
// scan-start hint.
//
// ctx semantics: per cross-process.md §Read Transaction step 2, slot
// acquisition is a CAS-on-shared-memory operation, not a blocking
// syscall — the only failure mode is "all slots occupied". With a
// deadline-bearing context, the caller retries with short backoff
// until a slot becomes free; with no deadline, an immediate
// ErrReadersFull is returned. ctx fires also surface as
// context.Cause(ctx).
//
// txnID must be > 0 (the per-slot "TxnID == 0 means free" sentinel).
// Callers wrap a snapshot meta's TxnID via max(meta.TxnID, 1) — the
// *ReadTx wiring does this.
//
// Returns the slot index. The caller MUST eventually call
// ReleaseReader(idx) — leaking the slot pins RPL reclamation until
// stale-detection ages out the slot (DefaultStaleTimeout = 10 s).
func (c *Coord) AcquireReader(ctx context.Context, txnID uint64) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return NoSlot, context.Cause(ctx)
	}
	if txnID == 0 {
		// Programmer-error precondition mirrored on AcquireReaderSlot;
		// surface as a structured error rather than the file-level
		// panic so the *ReadTx wrapper can map it cleanly.
		return NoSlot, errors.New("lock: AcquireReader requires txnID > 0")
	}
	hint := c.readerSlotHint.Load()
	now := c.clock()
	for {
		idx, err := c.f.AcquireReaderSlot(hint, txnID, c.pid, c.startTime, c.pidNS, now)
		if err == nil {
			c.readerSlotHint.Store(idx)
			c.RegisterReaderSlot(idx)
			return idx, nil
		}
		if !errors.Is(err, ErrReadersFull) {
			return NoSlot, err
		}
		// Table full. With no deadline, surface ErrReadersFull
		// immediately per transactions.md §Read Transaction step 2.
		// With a deadline, back off briefly and retry until a slot
		// frees or the context expires.
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			return NoSlot, ErrReadersFull
		}
		// Small backoff — releases are atomic stores so a slot can
		// free within a few microseconds; 1 ms gives the contending
		// scheduler room without busy-spinning shared memory.
		select {
		case <-ctx.Done():
			return NoSlot, context.Cause(ctx)
		case <-c.stopCh:
			return NoSlot, ErrClosed
		case <-time.After(1 * time.Millisecond):
		}
		now = c.clock()
	}
}

// RaiseReaderSlotTxnID raises an owned slot's pinned TxnID — the
// post-publish snapshot-restabilization step (see
// File.RaiseReaderSlotTxnID). No-op on NoSlot (lock-free read-only
// path).
func (c *Coord) RaiseReaderSlotTxnID(idx uint32, txnID uint64) {
	if idx == NoSlot {
		return
	}
	c.f.RaiseReaderSlotTxnID(idx, txnID)
}

// ReaderSlotTxnID returns the slot's currently pinned TxnID —
// observability for the restabilization tests.
func (c *Coord) ReaderSlotTxnID(idx uint32) uint64 {
	if idx == NoSlot {
		return 0
	}
	return Load64(&c.f.Slot(idx).TxnID)
}

// ReleaseReader is the Coord-mediated reader-slot release path. It
// unregisters the slot from the heartbeat goroutine's active list
// BEFORE clearing the on-mmap fields (cross-process.md §Heartbeat
// Goroutine race note: the active-list removal must precede the
// release stores so a racing tick cannot stomp Heartbeat after we
// publish "slot free").
//
// idx must be a valid slot index returned by a prior AcquireReader;
// passing NoSlot is a no-op (covers the leaked-Tx cleanup path that
// can race a normal Release).
func (c *Coord) ReleaseReader(idx uint32) {
	if idx == NoSlot {
		return
	}
	c.UnregisterReaderSlot(idx)
	c.f.ReleaseReaderSlot(idx)
}

// OldestReaderTxnID returns the minimum TxnID held by any live
// reader, or NoReaderTxnID if no live readers occupy slots. Stale
// slots (per the cross-process.md §Reader Table stale-detection
// rules) are reclaimed in place as a side effect.
//
// Caller MUST hold flock(LOCK_EX) on the lock-file fd. The
// wiring point is the write transaction's pre-commit state: the flock
// goroutine has just granted LOCK_EX, so the calling goroutine (the
// writer) can safely invoke this. A general-purpose helper without
// the LOCK_EX precondition would race two concurrent recoveries.
//
// The Coord does not assert the LOCK_EX precondition (no
// process-portable way to query flock state without taking it); the
// invariant is enforced by call-site discipline. Tests that exercise
// stale-clearing acquire flock first.
func (c *Coord) OldestReaderTxnID() uint64 {
	return c.f.OldestReaderTxnID(c.pidNS, c.clock(), c.staleTimeoutNanos(), c.crossNSTimeoutNanos())
}

// CountActiveReaders returns the number of occupied reader slots across
// the whole lock-file reader table (cluster-wide — every process's
// readers, not just this handle's). A slot is occupied iff its TxnID is
// non-zero (the "free" sentinel). The scan is a lock-free, per-slot
// atomic load: it takes no flock and does NOT clear stale slots, so the
// count can be off by ±N for N reader acquire/release transitions in
// flight during the scan. As a metrics/health signal
// (DBStats.ActiveReaders) it is never a synchronization barrier. The
// recovery-commit gate (durability.md §Recovery step 5) also consults
// it — soundly: the gate holds flock(LOCK_EX) (no acquire/release can
// race the scan) after a stale-slot reap, and counting a lingering
// mid-acquire slot errs in the conservative direction (recovery is
// merely deferred to a later Open). Stale slots
// from crashed peers count until a writer or maintenance pass reaps
// them.
func (c *Coord) CountActiveReaders() int {
	max := c.f.MaxReaders()
	n := 0
	for i := range max {
		if Load64(&c.f.Slot(i).TxnID) != 0 {
			n++
		}
	}
	return n
}

// ReapStaleReaderSlots acquires the write lock and scans the reader
// table, clearing slots owned by dead processes
// (background-maintenance.md §Stale Reader Slot Cleanup). The
// background-maintenance goroutine calls it every pass: a writer
// already clears stale slots during RPL reclamation, but only when it
// needs free pages, so a database with no active writer would let
// stale slots from crashed peers pin RPL reclamation indefinitely.
//
// It performs the SAME namespace-aware classification + clear as
// OldestReaderTxnID's documented side effect (the oldest-TxnID return
// is irrelevant to cleanup and is discarded), but acquires LOCK_EX
// *itself* via AcquireWriter rather than relying on a caller-held grant
// — the maintenance goroutine runs it with no transaction in flight.
//
// Holding LOCK_EX is mandatory and is the entire reason this routes
// through AcquireWriter: the clear stores and the HintEpoch
// first-observer CAS race any other clearer (a peer process's
// RPL-reclamation scan, or RecoverStaleWriter) without it. A phantom
// eviction of a freshly-acquired slot would orphan a live reader's
// snapshot and let RPL reclamation free pages it is still reading. A
// lock-free scan is therefore unsafe by construction — see
// OldestReaderTxnID's precondition.
//
// No write transaction is taken: clearing a slot is a single atomic
// store on the lock-file mmap, independent of the data file. Returns
// the AcquireWriter error (ctx cancellation / ErrClosed) unchanged so
// the maintenance pass can skip silently on a closing handle; on
// success the grant is always released before returning.
func (c *Coord) ReapStaleReaderSlots(ctx context.Context) error {
	g, err := c.AcquireWriter(ctx)
	if err != nil {
		return err
	}
	defer g.Release()
	c.OldestReaderTxnID() // discard min; the in-place stale-clear is the goal
	return nil
}
