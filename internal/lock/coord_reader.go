package lock

import (
	"context"
	"errors"
	"time"
)

// AcquireReader is the Coord-mediated reader-slot acquisition path.
// Composes File.AcquireReaderSlot with the active-slot bookkeeping
// (Close-time cleanup, in-process reader count) and the per-Coord
// scan-start hint. Same-handle acquirer serialization — two
// try-locks through one hold description would otherwise both
// "win" one slot — lives inside File.AcquireReaderSlot, beside the
// description it protects (cross-process.md §Reader Table,
// same-description caveat).
//
// ctx semantics: slot acquisition is a try-lock scan, not a
// blocking syscall — the only failure mode is "all slots held".
// With a deadline-bearing context, the caller retries with short
// backoff until a slot frees; with no deadline, an immediate
// ErrReadersFull is returned. ctx fires surface as
// context.Cause(ctx).
//
// txnID must be > 0 (the per-slot "TxnID == 0 means free"
// sentinel). Callers wrap a snapshot meta's TxnID via
// max(meta.TxnID, 1) — the *ReadTx wiring does this.
//
// Returns the slot index. The caller MUST eventually call
// ReleaseReader(idx) — a leaked slot stays HELD (its lock rides
// this process's descriptor) and pins RPL reclamation until the
// process exits; the leak-detection cleanup path is what prevents
// that for abandoned transactions.
func (c *Coord) AcquireReader(ctx context.Context, txnID uint64) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return NoSlot, context.Cause(ctx)
	}
	if txnID == 0 {
		// Programmer-error precondition mirrored on
		// AcquireReaderSlot; surface as a structured error rather
		// than the file-level panic so the *ReadTx wrapper can map
		// it cleanly.
		return NoSlot, errors.New("lock: AcquireReader requires txnID > 0")
	}
	for {
		hint := c.readerSlotHint.Load()
		idx, err := c.f.AcquireReaderSlot(hint, txnID, c.pid)
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
		select {
		case <-ctx.Done():
			return NoSlot, context.Cause(ctx)
		case <-c.stopCh:
			return NoSlot, ErrClosed
		case <-time.After(1 * time.Millisecond):
		}
	}
}

// RaiseReaderSlotTxnID raises an owned slot's pinned TxnID — the
// post-publish snapshot-restabilization step, an owner-only
// overwrite made trivially exclusive by the held slot lock. No-op
// on NoSlot (lock-free read-only path).
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

// ReleaseReader is the Coord-mediated reader-slot release path:
// unregister from the active list, zero the slot, drop its lock.
//
// idx must be a valid slot index returned by a prior
// AcquireReader; passing NoSlot is a no-op (covers the leaked-Tx
// cleanup path that can race a normal Release).
func (c *Coord) ReleaseReader(idx uint32) {
	if idx == NoSlot {
		return
	}
	c.UnregisterReaderSlot(idx)
	c.f.ReleaseReaderSlot(idx)
}

// OldestReaderTxnID returns the minimum TxnID held by any occupied
// reader slot, or NoReaderTxnID if none — a pure memory scan
// (cross-process.md §Reader Table, bound scans stay pure memory
// reads). Stale slots conservatively pin the bound until a reap
// clears them; callers whose reclamation bound looks pinned run
// ReapStaleReaderSlots first.
func (c *Coord) OldestReaderTxnID() uint64 {
	return c.f.OldestReaderTxnID()
}

// CountActiveReaders returns the number of occupied reader slots across
// the whole lock-file reader table (cluster-wide — every process's
// readers, not just this handle's). A slot is occupied iff its TxnID is
// non-zero (the "free" sentinel). The scan is a lock-free, per-slot
// atomic load: it takes no locks and does NOT clear stale slots, so the
// count can be off by ±N for N reader acquire/release transitions in
// flight during the scan. As a metrics/health signal
// (DBStats.ActiveReaders) it is never a synchronization barrier. The
// recovery-commit gate (durability.md §Recovery step 5) also consults
// it — soundly, but NOT because the scan is race-free: reader
// acquire/release are lock-free against it and race the count freely.
// Soundness rests on the error directions instead: a lingering
// mid-transition slot inflates the count, which merely defers recovery
// to a later Open; a reader that acquires AFTER the scan passed its
// slot is missed, which is the lock-free acquisition window
// durability.md's unrecovered-window contract already covers (the
// reader restabilizes against the post-recovery meta). Stale slots
// from crashed peers count until a reap clears them.
func (c *Coord) CountActiveReaders() int {
	return c.f.CountActiveReaders()
}

// ReapStaleReaderSlots probes every occupied slot and clears the
// dead owners' (background-maintenance.md §Stale Reader Slot
// Cleanup): the verdict (probe acquired ⇒ owner gone) and the clear
// are one act under the held probe, so the reap needs NO write
// grant — clearers serialize on the slot lock (cross-handle) and a
// per-handle mutex (same-handle), and read-only handles reap too.
// The maintenance goroutine calls it every pass; a writer whose
// reclamation bound looks pinned runs it before recomputing.
// Returns the slots cleared and the UNDECIDED probe count — see
// File.ReapStaleReaderSlots for why the latter must not be
// swallowed.
func (c *Coord) ReapStaleReaderSlots(ctx context.Context) (cleared, undecided int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, context.Cause(ctx)
	}
	cleared, undecided = c.f.ReapStaleReaderSlots()
	return cleared, undecided, nil
}
