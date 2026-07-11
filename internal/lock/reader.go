package lock

import (
	"errors"
	"fmt"
	"math"
	"sync/atomic"
)

// acquirePublishHookForTest, when set, fires after AcquireReaderSlot's
// five publish stores and BEFORE the post-publish ownership verify —
// the only window in which a stale-detection scan can age the acquire
// out from under a frozen acquirer. Tests use it to simulate that
// eviction deterministically. The callback must be non-blocking.
var acquirePublishHookForTest atomic.Pointer[func(idx uint32)]

// staleClearHookForTest, when set, fires after a scan classifies a
// slot as stale and BEFORE the guarded clear re-validates it — the
// window in which the classified occupant can be released and the
// slot re-won. Tests use it to interleave a re-win deterministically.
// The callback must be non-blocking.
var staleClearHookForTest atomic.Pointer[func(idx uint32)]

// NoSlot is the sentinel returned by AcquireReaderSlot / stored on a
// Tx with no reader slot. ^uint32(0) is outside any legal MaxReaders
// range (capped at MaxMaxReaders = 65536).
const NoSlot uint32 = math.MaxUint32

// ErrReadersFull is returned by AcquireReaderSlot after a full
// scan-with-wraparound finds no free slot. Surfaces at the public API
// as gmdb.ErrReadersFull so callers can
// distinguish "table at capacity" from other Open/Begin failures.
var ErrReadersFull = errors.New("lock: reader table full")

// AcquireReaderSlot scans the reader table from `hint` (wrapping at
// MaxReaders) for a slot whose TxnID is 0, CAS-wins it, and stamps
// the caller's identity in the field order required by
// cross-process.md §Reader Table (slot acquire):
//
//	a. Store Heartbeat = now()                (atomic; the clock is
//	    read HERE, not by the caller — a caller-supplied value could
//	    be arbitrarily old by store time if the acquirer was
//	    descheduled or frozen after reading it, and a
//	    stale-at-birth heartbeat lets the next scan age the
//	    mid-publish window out immediately)
//	b. Store HintEpoch = 0                    (atomic; clears any
//	    leftover orphan anchor from a prior stale clear)
//	c. Store PIDNamespace = pidNS             (atomic)
//	d. Store ProcessStartTime = pst           (atomic)
//	e. Store PID = pid                        (atomic; final identity
//	    publish, gates the stale-detector's same-namespace PID path)
//	f. Ownership verify: re-load TxnID. If it no longer holds this
//	    acquisition's value, a stale-detection scan aged the window
//	    out mid-publish (reachable only when this goroutine was
//	    frozen > StaleTimeout between the CAS and here). Slot still
//	    FREE: RE-CLAIM it (CAS) and re-publish — walking away would
//	    strand this identity on a TxnID==0 slot forever, breaking
//	    the free⇒PID==0 premise the detector's mid-publish grace
//	    rests on. Slot re-won: ABANDON — zeroing would evict the
//	    winner (the exact cascading eviction the guarded clear
//	    prevents); the winner's own publish overwrites the ghost
//	    stores, so no junk survives. Residuals: transient ghost
//	    clobber of a re-winner's fields, and a same-TxnID re-win
//	    passing the verify (two owners) — closable only by a
//	    versioned slot layout (cross-process.md §Slot acquire
//	    residual note).
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
// with a legitimate genesis snapshot of 0; the caller passes
// max(activeMeta.TxnID, 1) to dodge this — documenting the precondition
// here rather than silently coercing).
func (f *File) AcquireReaderSlot(hint uint32, txnID, pid, pst, pidNS uint64, now func() uint64) (uint32, uint64, error) {
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
		return NoSlot, 0, ErrReadersFull
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
		// Won the CAS — bump the acquisition generation FIRST (the
		// ownership token is the (TxnID, Gen) pair; every later
		// owner-side touch re-checks it), then finalise identity in
		// the spec-required order, heartbeat stamped at store time
		// (step a rationale above).
		myGen := Add64(&slot.Gen, 1)
		won := false
		for {
			Store64(&slot.Heartbeat, now())
			Store64(&slot.HintEpoch, 0)
			Store64(&slot.PIDNamespace, pidNS)
			Store64(&slot.ProcessStartTime, pst)
			Store64(&slot.PID, pid)
			if hook := acquirePublishHookForTest.Load(); hook != nil {
				(*hook)(i)
			}
			// Step f — ownership verify (doc above): the (TxnID,
			// Gen) pair distinguishes even a re-win that pinned the
			// SAME TxnID (the re-winner bumped Gen past ours).
			if Load64(&slot.TxnID) == txnID && Load64(&slot.Gen) == myGen {
				won = true
				break
			}
			// Lost mid-publish. If the slot is still FREE (aged out
			// and cleared, not re-won), RE-CLAIM it and re-publish:
			// walking away would strand our identity stores on a
			// free slot forever — nothing scrubs a TxnID==0 slot, and
			// the junk breaks the free⇒PID==0 premise the detector's
			// mid-publish grace period rests on (the next acquirer's
			// CAS window would present its TxnID under OUR dead PID
			// and be evicted through the identity path with no aging
			// grace). Each re-publish needs another >StaleTimeout
			// freeze to fail again, so the loop terminates in
			// practice after one pass. If the re-claim CAS loses, the
			// slot was re-won: the winner's own publish overwrites
			// all five fields, so no junk survives — ABANDON (zeroing
			// would evict the winner) and continue scanning.
			if !CAS64(&slot.TxnID, 0, txnID) {
				break
			}
			myGen = Add64(&slot.Gen, 1)
		}
		if !won {
			continue
		}
		return i, myGen, nil
	}
	return NoSlot, 0, ErrReadersFull
}

// ReleaseReaderSlot performs the strict release-ordered atomic stores
// from cross-process.md §Reader Table (slot release):
//
//  1. PID = 0          — first, so a stale-detector scan between the
//     next acquirer's CAS and its PID-store sees PID == 0 and falls
//     through to the heartbeat / HintEpoch path rather than running
//     kill() against this (about-to-be-exited) PID.
//  2. Heartbeat = 0    — reset the heartbeat-liveness marker so the
//     next acquirer starts clean.
//  3. HintEpoch = 0    — clear any orphan-detection anchor.
//  4. TxnID = 0        — final observable-free signal.
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
// gen must be the (TxnID, Gen) ownership token's generation from the
// acquire: a slot lost to an aging clear and re-won carries a HIGHER
// generation, and releasing it would zero the re-winner's live pin
// (the cascading eviction the guarded clear exists to prevent) — the
// release is skipped instead; the slot belongs to the re-winner.
func (f *File) ReleaseReaderSlot(idx uint32, gen uint64) {
	if f.slots == nil {
		panic("lock: ReleaseReaderSlot on closed *File")
	}
	if idx >= uint32(len(f.slots)) {
		panic("lock: ReleaseReaderSlot index out of range")
	}
	if Load64(&f.slots[idx].Gen) != gen {
		return // lost to an aging clear + re-win: not ours to release
	}
	clearReaderSlot(&f.slots[idx])
}

// clearReaderSlot is the one four-store slot-clear body shared by the
// owner release and the writer-side stale clear. The store ORDER is
// the load-bearing part (PID → Heartbeat → HintEpoch → TxnID) and is
// identical for both — each caller's doc carries its own rationale
// for why that order matters on its path.
func clearReaderSlot(slot *ReaderSlot) {
	Store64(&slot.PID, 0)
	Store64(&slot.Heartbeat, 0)
	Store64(&slot.HintEpoch, 0)
	Store64(&slot.TxnID, 0)
}

// ClearStaleReaderSlot implements the writer-side stale-clear ordering
// from cross-process.md §Reader Table (clear ordering) — the SAME
// four-store order as ReleaseReaderSlot:
//
//  1. PID = 0         — the dead occupant's identity must not survive
//     the clear: the next acquirer's CAS→publish window is otherwise
//     observably `TxnID=fresh, PID=dead, Heartbeat=stale`, and a
//     concurrent scan classifies by pid != 0 — same-namespace
//     IsAlive(deadPID)=false or cross-namespace stale heartbeat —
//     and immediately evicts the LIVE acquirer (its snapshot leaves
//     the table, the reclamation bound advances past it, and its own
//     later release zeroes a slot a third reader may have won).
//  2. Heartbeat = 0   — same reason for the pid == 0 sub-cases: a
//     leftover stale heartbeat would put the mid-publish acquirer in
//     case 0b (immediate clear) instead of case 0c (epoch-anchored,
//     StaleTimeout-bounded).
//  3. HintEpoch = 0   — clears the orphan-detection anchor while the
//     slot is still observably non-free, preventing a fresh acquirer
//     from inheriting a stale epoch.
//  4. TxnID = 0       — final release; only after this store can an
//     acquirer CAS the slot, so acquirers never observe a partial
//     clear (scans are excluded by LOCK_EX).
//
// ProcessStartTime/PIDNamespace are left as-is, exactly like the
// release path: the classification consults them only when PID != 0.
//
// The HintEpoch-before-TxnID ordering is load-bearing; reversed, a
// fresh acquirer could CAS-win TxnID, crash before its Heartbeat
// store, and then be re-cleared by the next stale-detection scan via
// the already-aged HintEpoch — evicting the (genuinely dead) new
// acquirer faster than StaleTimeout and violating the per-occupant
// timer invariant.
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
	clearReaderSlot(&f.slots[idx])
}

// readerSlotObservation is the full field tuple a stale-detection
// scan loaded to classify a slot's occupant. The guarded clear
// re-validates against it so a clear only ever zeroes the occupant
// the scan classified — never a successor that released-and-re-won
// the slot between classification (which runs syscalls: kill(2),
// /proc reads) and the clear stores.
type readerSlotObservation struct {
	TxnID, PID, Heartbeat, HintEpoch, PST, PIDNS, Gen uint64
}

// clearStaleReaderSlotIfUnchanged clears the slot iff every field
// still holds the observed value; a changed slot is skipped (the next
// scan re-classifies the new occupant from scratch). Reports whether
// the clear happened.
//
// Soundness: a stale verdict means the classified occupant is a dead
// process instance (same-namespace ESRCH / start-time mismatch), an
// aged-out crashed acquirer, or a heartbeat-expired cross-namespace
// identity. A dead or crashed occupant cannot mutate its slot, and no
// acquirer can CAS a slot whose TxnID is non-zero, so an unchanged
// tuple means the classified occupant still holds the slot and the
// clear stores cannot race a release. ABA is excluded field-by-field:
// a release-then-re-win zeroes then re-stamps Heartbeat/HintEpoch/PID,
// so a re-winner (even one pinning the same TxnID) presents at least
// one differing field — a fresh heartbeat, a zeroed HintEpoch, or a
// different PID/start-time. Residuals: a same-namespace re-winner
// whose PID was recycled AND whose start time collides within the
// clock tick (the start-time-collision hardening tracks that class),
// and the cross-namespace frozen-but-alive occupant (the documented
// longer-window trade — see §Stale-reader detection).
//
// Caller MUST hold flock(LOCK_EX), same as ClearStaleReaderSlot.
func (f *File) clearStaleReaderSlotIfUnchanged(idx uint32, obs readerSlotObservation) bool {
	slot := &f.slots[idx]
	if Load64(&slot.TxnID) != obs.TxnID ||
		Load64(&slot.PID) != obs.PID ||
		Load64(&slot.Heartbeat) != obs.Heartbeat ||
		Load64(&slot.HintEpoch) != obs.HintEpoch ||
		Load64(&slot.ProcessStartTime) != obs.PST ||
		Load64(&slot.PIDNamespace) != obs.PIDNS ||
		Load64(&slot.Gen) != obs.Gen {
		return false
	}
	clearReaderSlot(slot)
	return true
}

// RaiseReaderSlotTxnID overwrites an OWNED slot's pinned TxnID with a
// higher one — the post-publish snapshot-restabilization step of the
// reader-begin protocol (cross-process.md §Reader Table, slot
// acquire): after the acquirer's CAS is visible it re-reads the
// latest meta and raises the slot to match until the meta is stable.
// Owner-only (the slot must have been acquired by this process and
// not released) and monotonic (txnID must be >= the current value):
// a concurrent scan reading the OLD value computes a lower — strictly
// conservative — reclamation bound, so no ordering beyond the single
// atomic store is needed.
// gen is the acquire's ownership token; a mismatch means the slot was
// aged out and (possibly) re-won mid-restabilization — the raise is
// refused (returns false) so the caller can abandon the acquisition
// instead of stomping the re-winner's pin.
func (f *File) RaiseReaderSlotTxnID(idx uint32, gen uint64, txnID uint64) bool {
	if f.slots == nil {
		panic("lock: RaiseReaderSlotTxnID on closed *File")
	}
	if idx >= uint32(len(f.slots)) {
		panic("lock: RaiseReaderSlotTxnID index out of range")
	}
	if txnID == 0 {
		panic("lock: RaiseReaderSlotTxnID called with txnID=0")
	}
	slot := &f.slots[idx]
	if Load64(&slot.Gen) != gen {
		return false // lost mid-restabilization: not ours to raise
	}
	if cur := Load64(&slot.TxnID); txnID < cur {
		panic(fmt.Sprintf("lock: RaiseReaderSlotTxnID(%d) would lower pinned TxnID %d -> %d", idx, cur, txnID))
	}
	Store64(&slot.TxnID, txnID)
	return true
}

// NoReaderTxnID is OldestReaderTxnID's "no live reader occupies a
// slot" result. Consumers compare against this constant rather than
// re-declaring the sentinel value.
const NoReaderTxnID = ^uint64(0)

// OldestReaderTxnID scans the reader table and returns the minimum
// TxnID across all live (non-stale) reader slots. Returns
// NoReaderTxnID when no live readers occupy slots — the writer's RPL
// reclamation bound calculator then uses min(this, anchoredEpoch)
// which reduces to the anchored epoch when no readers are present.
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
func (f *File) OldestReaderTxnID(ourPIDNS uint64, nowNanos uint64, staleTimeoutNanos, crossNSTimeoutNanos uint64) uint64 {
	if f.slots == nil {
		panic("lock: OldestReaderTxnID on closed *File")
	}
	min := NoReaderTxnID
	// clearStale runs the guarded clear for a slot the classification
	// below judged stale. Classification runs syscalls (kill(2),
	// /proc reads) between the loads and this point, so the occupant
	// may have released and the slot been re-won by a LIVE reader —
	// clearing unconditionally would evict it (its snapshot leaves
	// the table, the reclamation bound advances past it:
	// use-after-reclaim). On a failed guard the CURRENT TxnID joins
	// the min instead — the new occupant is a fresh acquirer whose
	// pin must floor the bound; the next scan re-classifies it.
	clearStale := func(i int, obs readerSlotObservation) {
		if hook := staleClearHookForTest.Load(); hook != nil {
			(*hook)(uint32(i))
		}
		if !f.clearStaleReaderSlotIfUnchanged(uint32(i), obs) {
			if cur := Load64(&f.slots[i].TxnID); cur != 0 && cur < min {
				min = cur
			}
		}
	}
	for i := range f.slots {
		slot := &f.slots[i]
		txnID := Load64(&slot.TxnID)
		if txnID == 0 {
			continue
		}
		// Load the FULL tuple up front: classification and the
		// guarded clear must judge the same observation.
		obs := readerSlotObservation{
			TxnID:     txnID,
			PID:       Load64(&slot.PID),
			Heartbeat: Load64(&slot.Heartbeat),
			HintEpoch: Load64(&slot.HintEpoch),
			PST:       Load64(&slot.ProcessStartTime),
			PIDNS:     Load64(&slot.PIDNamespace),
			Gen:       Load64(&slot.Gen),
		}
		if obs.PID == 0 {
			// Case 0: mid-acquire / mid-release / orphan.
			switch {
			case obs.Heartbeat != 0 && !heartbeatStale(nowNanos, obs.Heartbeat, staleTimeoutNanos):
				// 0a: live owner mid-acquire/release (incl. a
				// future-stamped heartbeat — a mid-publish reader whose
				// clock read raced ahead of ours). Honour the
				// TxnID — the owner has CAS'd it and will either
				// release cleanly or, on crash, the next scan will
				// catch the aged-out heartbeat.
				if txnID < min {
					min = txnID
				}
			case obs.Heartbeat != 0:
				// 0b: heartbeat aged out; clear (guarded).
				clearStale(i, obs)
			default:
				// 0c: zero heartbeat. Use HintEpoch as cross-process
				// orphan anchor.
				switch {
				case obs.HintEpoch == 0:
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
				case heartbeatStale(nowNanos, obs.HintEpoch, staleTimeoutNanos):
					clearStale(i, obs)
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
		// pid != 0: the shared identity classification
		// (zeroHeartbeatFresh — a pid!=0/heartbeat==0 slot is a
		// release in flight; clearing it could stomp a third
		// reader's fresh CAS).
		if !identityLive(obs.PID, obs.PST, obs.PIDNS,
			obs.Heartbeat, ourPIDNS, nowNanos, staleTimeoutNanos, crossNSTimeoutNanos, true) {
			clearStale(i, obs)
			continue
		}
		if txnID < min {
			min = txnID
		}
	}
	return min
}
