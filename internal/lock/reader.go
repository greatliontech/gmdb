package lock

import (
	"errors"
	"math"
)

// NoSlot is the sentinel returned by AcquireReaderSlot / stored on a
// Tx with no reader slot. ^uint32(0) is outside any legal MaxReaders
// range (capped at MaxMaxReaders = 65536).
const NoSlot uint32 = math.MaxUint32

// ErrReadersFull is returned by AcquireReaderSlot after a full
// scan-with-wraparound finds no free slot. Surfaces at the public API
// as gmdb.ErrReadersFull (chunk 3.3 adds the sentinel) so callers can
// distinguish "table at capacity" from other Open/Begin failures.
var ErrReadersFull = errors.New("lock: reader table full")

// AcquireReaderSlot scans the reader table from `hint` (wrapping at
// MaxReaders) for a slot whose TxnID is 0, CAS-wins it, and stamps
// the caller's identity in the field order required by
// cross-process.md §Reader Table (slot acquire):
//
//	a. Store Heartbeat = heartbeat            (atomic)
//	b. Store HintEpoch = 0                    (atomic; clears any
//	    leftover orphan anchor from a prior stale clear)
//	c. Store PIDNamespace = pidNS             (atomic)
//	d. Store ProcessStartTime = pst           (atomic)
//	e. Store PID = pid                        (atomic; final identity
//	    publish, gates the stale-detector's same-namespace PID path)
//
// The ordering is load-bearing — see the slot-acquire invariant in
// cross-process.md. Heartbeat first gives a crash-mid-acquire slot a
// liveness anchor that ages out; PID last keeps the detector's PID
// fast path inert until the full identity has been written.
//
// On success returns (slot index, nil) and the slot is observably
// owned by this caller (TxnID = txnID, PID = pid). On a full-scan
// wrap-around with no free slot, returns (NoSlot, ErrReadersFull).
//
// The hint is a process-local optimisation (cross-process.md notes
// hint updates are relaxed atomic stores with no cross-process
// coordination) — passing 0 is always correct, just slower under
// steady state.
//
// txnID is the snapshot TxnID from the active meta. txnID == 0 is
// rejected (the per-slot "TxnID == 0 means free" sentinel collides
// with a legitimate genesis snapshot of 0; the chunk-3 caller passes
// max(activeMeta.TxnID, 1) to dodge this — documenting the precondition
// here rather than silently coercing).
func (f *File) AcquireReaderSlot(hint uint32, txnID, pid, pst, pidNS, heartbeat uint64) (uint32, error) {
	if f.slots == nil {
		panic("lock: AcquireReaderSlot on closed *File")
	}
	if txnID == 0 {
		// Programmer error — see doc. We surface as a panic rather
		// than ErrReadersFull because masking it would corrupt the
		// reader table (a slot with TxnID=0 is considered free; we'd
		// be racing every subsequent acquirer).
		panic("lock: AcquireReaderSlot called with txnID=0")
	}
	n := uint32(len(f.slots))
	if n == 0 {
		return NoSlot, ErrReadersFull
	}
	if hint >= n {
		hint = 0
	}
	for off := range n {
		i := hint + off
		if i >= n {
			i -= n
		}
		slot := &f.slots[i]
		if Load64(&slot.TxnID) != 0 {
			continue
		}
		if !CAS64(&slot.TxnID, 0, txnID) {
			continue
		}
		// Won the CAS — finalise identity in the spec-required order.
		Store64(&slot.Heartbeat, heartbeat)
		Store64(&slot.HintEpoch, 0)
		Store64(&slot.PIDNamespace, pidNS)
		Store64(&slot.ProcessStartTime, pst)
		Store64(&slot.PID, pid)
		return i, nil
	}
	return NoSlot, ErrReadersFull
}

// ReleaseReaderSlot performs the strict release-ordered atomic stores
// from cross-process.md §Reader Table (slot release):
//
//	1. PID = 0          — first, so a stale-detector scan between the
//	   next acquirer's CAS and its PID-store sees PID == 0 and falls
//	   through to the heartbeat / HintEpoch path rather than running
//	   kill() against this (about-to-be-exited) PID.
//	2. Heartbeat = 0    — reset the heartbeat-liveness marker so the
//	   next acquirer starts clean.
//	3. HintEpoch = 0    — clear any orphan-detection anchor.
//	4. TxnID = 0        — final observable-free signal.
//
// Caller MUST have unregistered the slot from the heartbeat goroutine's
// active list BEFORE invoking this (the active-list removal happens
// before step 2 so a racing heartbeat tick cannot stomp Heartbeat
// after the release — see the heartbeat goroutine's race note in
// coord.go). The lock package does not enforce this — Coord.ReleaseReader
// composes the two correctly.
//
// idx must be a valid slot index; out-of-range is a programmer bug
// and panics.
func (f *File) ReleaseReaderSlot(idx uint32) {
	if f.slots == nil {
		panic("lock: ReleaseReaderSlot on closed *File")
	}
	if idx >= uint32(len(f.slots)) {
		panic("lock: ReleaseReaderSlot index out of range")
	}
	slot := &f.slots[idx]
	Store64(&slot.PID, 0)
	Store64(&slot.Heartbeat, 0)
	Store64(&slot.HintEpoch, 0)
	Store64(&slot.TxnID, 0)
}

// ClearStaleReaderSlot implements the writer-side stale-clear ordering
// from cross-process.md §Reader Table (clear ordering):
//
//	1. HintEpoch = 0   — clears the orphan-detection anchor while the
//	   slot is still observably non-free, preventing a fresh acquirer
//	   from inheriting a stale epoch.
//	2. TxnID = 0       — final release.
//
// The HintEpoch-first ordering is load-bearing; reversed, a fresh
// acquirer could CAS-win TxnID, crash before its Heartbeat store,
// and then be re-cleared by the next stale-detection scan via the
// already-aged HintEpoch — evicting the (genuinely dead) new acquirer
// faster than StaleTimeout and violating the per-occupant timer
// invariant.
//
// Caller MUST hold flock(LOCK_EX) — only one process at a time may
// clear stale slots, or two concurrent recoveries could race and
// produce phantom acquirer evictions.
func (f *File) ClearStaleReaderSlot(idx uint32) {
	if f.slots == nil {
		panic("lock: ClearStaleReaderSlot on closed *File")
	}
	if idx >= uint32(len(f.slots)) {
		panic("lock: ClearStaleReaderSlot index out of range")
	}
	slot := &f.slots[idx]
	Store64(&slot.HintEpoch, 0)
	Store64(&slot.TxnID, 0)
}

// OldestReaderTxnID scans the reader table and returns the minimum
// TxnID across all live (non-stale) reader slots. Returns ^uint64(0)
// when no live readers occupy slots — the writer's RPL reclamation
// bound calculator then uses min(this, lastCheckpointTxnID) which
// reduces to lastCheckpointTxnID when no readers are present.
//
// During the scan, slots in detectable stale states are reclaimed in
// place (the stale-clear ordering of ClearStaleReaderSlot), or in the
// PID==0+Heartbeat==0 case the first observer CAS-stores `nowNanos`
// into HintEpoch to anchor the orphan timer (cross-process.md §Reader
// Table stale-detection case 0c). Subsequent calls (from this or any
// other process holding flock) compare against the stored epoch and
// clear the slot once `nowNanos - HintEpoch > staleTimeoutNanos`.
//
// Caller MUST hold flock(LOCK_EX) on the lock-file fd — both the
// stale-clear and the HintEpoch first-observer CAS would race
// concurrent writers otherwise.
//
// Classification (matches §Reader Table stale-detection literally):
//
//  0. TxnID != 0 AND PID == 0 — slot is mid-acquire / mid-release /
//     orphaned. Sub-cases:
//     a. Heartbeat != 0, fresh (now - HB <= staleTimeout): live owner
//     mid-acquire/release. Skip (include TxnID in oldest-min calc).
//     b. Heartbeat != 0, stale (now - HB > staleTimeout): acquirer
//     made it past the Heartbeat store but crashed before the
//     PID store; the heartbeat aged out. Clear.
//     c. Heartbeat == 0: acquirer crashed before the Heartbeat store.
//     If HintEpoch == 0, CAS-store nowNanos to anchor; skip this
//     round. If HintEpoch != 0 and now - HintEpoch > staleTimeout,
//     clear. Otherwise skip.
//
//  1. PID != 0 AND PIDNamespace == ourPIDNS (both non-zero):
//     a. !IsAlive(PID): stale, clear.
//     b. ProcessStartTime mismatch: PID recycled, stale, clear.
//     c. Otherwise: live, include in min.
//
//  2. PID != 0 AND PIDNamespace != ourPIDNS (or either zero):
//     now - Heartbeat > staleTimeout: stale, clear. Else live,
//     include in min.
func (f *File) OldestReaderTxnID(ourPIDNS uint64, nowNanos uint64, staleTimeoutNanos uint64) uint64 {
	if f.slots == nil {
		panic("lock: OldestReaderTxnID on closed *File")
	}
	min := uint64(math.MaxUint64)
	for i := range f.slots {
		slot := &f.slots[i]
		txnID := Load64(&slot.TxnID)
		if txnID == 0 {
			continue
		}
		pid := Load64(&slot.PID)
		hb := Load64(&slot.Heartbeat)
		if pid == 0 {
			// Case 0: mid-acquire / mid-release / orphan.
			switch {
			case hb != 0 && !heartbeatStale(nowNanos, hb, staleTimeoutNanos):
				// 0a: live owner mid-acquire/release (incl. a
				// future-stamped heartbeat — a mid-publish reader whose
				// clock read raced ahead of ours). Honour the
				// TxnID — the owner has CAS'd it and will either
				// release cleanly or, on crash, the next scan will
				// catch the aged-out heartbeat.
				if txnID < min {
					min = txnID
				}
			case hb != 0:
				// 0b: heartbeat aged out; clear.
				f.ClearStaleReaderSlot(uint32(i))
			default:
				// 0c: zero heartbeat. Use HintEpoch as cross-process
				// orphan anchor.
				epoch := Load64(&slot.HintEpoch)
				switch {
				case epoch == 0:
					// First observer: CAS-store now. If the CAS
					// loses (another writer raced us in some
					// future protocol extension), the next scan
					// catches it via the != 0 branch. Treat the
					// slot as live for reclamation purposes (TxnID
					// enters the min): the acquirer may be a live
					// mid-publish reader about to stamp PID, and
					// advancing the bound past its TxnID would let
					// the writer reclaim pages it's about to read.
					CAS64(&slot.HintEpoch, 0, nowNanos)
					if txnID < min {
						min = txnID
					}
				case heartbeatStale(nowNanos, epoch, staleTimeoutNanos):
					f.ClearStaleReaderSlot(uint32(i))
				default:
					// Epoch set but not yet aged out. Skip
					// (slot remains non-free; treat as live so
					// the writer doesn't reclaim pages a fresh
					// acquirer mid-CAS might still snapshot via
					// its just-CAS'd TxnID).
					if txnID < min {
						min = txnID
					}
				}
			}
			continue
		}
		// pid != 0 paths.
		slotNS := Load64(&slot.PIDNamespace)
		sameNS := slotNS != 0 && ourPIDNS != 0 && slotNS == ourPIDNS
		if sameNS {
			if !IsAlive(int(pid)) {
				f.ClearStaleReaderSlot(uint32(i))
				continue
			}
			// Alive in this namespace; PID-reuse check via start time.
			actualPST, err := ProcessStartTime(int(pid))
			if err != nil {
				// Process exists but start time unreadable; fall
				// back to heartbeat path (conservative).
				if hb != 0 && heartbeatStale(nowNanos, hb, staleTimeoutNanos) {
					f.ClearStaleReaderSlot(uint32(i))
					continue
				}
				if txnID < min {
					min = txnID
				}
				continue
			}
			recorded := Load64(&slot.ProcessStartTime)
			if recorded != actualPST {
				// PID recycled to a different process lifetime.
				f.ClearStaleReaderSlot(uint32(i))
				continue
			}
			if txnID < min {
				min = txnID
			}
			continue
		}
		// Case 2: cross-namespace or namespace-unknown. Heartbeat-only.
		if hb != 0 && heartbeatStale(nowNanos, hb, staleTimeoutNanos) {
			f.ClearStaleReaderSlot(uint32(i))
			continue
		}
		if txnID < min {
			min = txnID
		}
	}
	return min
}
