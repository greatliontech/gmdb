package lock

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"

	"github.com/greatliontech/gmdb/internal/flock"
)

// Reader-slot liveness (cross-process.md §Reader Table): a slot is
// alive exactly while its owner holds the slot's kernel lock,
// released at process death. Slot CONTENT (TxnID, diagnostic PID)
// uses cross-process atomics; slot OWNERSHIP is the held lock, and
// every field mutation happens under it — acquisition takes the
// lock before its first store, release zeroes TxnID before
// dropping, and a stale clear stores only inside a successful probe
// acquisition. No heartbeat, no identity, no timer.

// acquirePublishHookForTest fires after an acquisition's publish
// stores land — the seam DST uses to interleave scans against the
// post-publish window. Never set outside tests.
var acquirePublishHookForTest atomic.Pointer[func(idx uint32)]

// staleClearHookForTest fires after a probe-based stale clear's
// TxnID store, while the probe is still held. Never set outside
// tests.
var staleClearHookForTest atomic.Pointer[func(idx uint32)]

// NoSlot is the sentinel slot index for the lock-free read-only
// path.
const NoSlot uint32 = math.MaxUint32

// ErrReadersFull is returned when every reader slot is held by a
// live owner.
var ErrReadersFull = errors.New("lock: reader table full")

// errSlotBusy is the slot-lock backends' contention signal: a live
// holder owns the slot.
var errSlotBusy = errors.New("lock: slot lock held")

// slotLocks is the per-slot kernel-lock backend (cross-process.md
// §Reader Table, slot locks). tryHold acquires through a hold
// description (the owner keeps the returned release until its
// transaction ends); tryProbe is the judge-only variant through a
// description distinct from every hold description — mandatory,
// because two try-locks through ONE description do not conflict and
// a probe through a hold description would read this process's own
// live reader as dead. Both return errSlotBusy on contention;
// any other error is undecided and never a stale verdict.
type slotLocks interface {
	tryHold(idx uint32) (release func(), err error)
	tryProbe(idx uint32) (release func(), err error)
	close() error
}

// rangeLocks is the Linux backend: OFD byte-range write locks over
// each slot's own 56 bytes of the lock file, on two dedicated
// descriptions (hold + probe) opened beside the mmap descriptor.
type rangeLocks struct {
	hold  *os.File
	probe *os.File
}

func slotRangeOff(idx uint32) int64 {
	return int64(HeaderSize) + int64(SlotSize)*int64(idx)
}

func (r *rangeLocks) try(f *os.File, idx uint32) (func(), error) {
	err := flock.TryExclusiveRange(f.Fd(), slotRangeOff(idx), SlotSize)
	if err == nil {
		return func() { _ = flock.UnlockRange(f.Fd(), slotRangeOff(idx), SlotSize) }, nil
	}
	if flock.ErrRangeContended(err) {
		return nil, errSlotBusy
	}
	return nil, err
}

func (r *rangeLocks) tryHold(idx uint32) (func(), error)  { return r.try(r.hold, idx) }
func (r *rangeLocks) tryProbe(idx uint32) (func(), error) { return r.try(r.probe, idx) }

func (r *rangeLocks) close() error {
	err := r.hold.Close()
	if perr := r.probe.Close(); err == nil {
		err = perr
	}
	return err
}

// fileLocks is the darwin/freebsd backend: flock on a per-slot lock
// FILE, opened per acquisition — each acquisition owns its
// description, so hold/probe distinctness (and Close-outlive) come
// for free at the cost of one descriptor per active reader. The
// directory and every slot file are created EAGERLY by the
// lock-file creator (populateReadersDir); this handle only opens.
type fileLocks struct {
	root *os.Root
	dir  string
}

func (l *fileLocks) open(idx uint32) (*os.File, error) {
	// O_RDWR without O_CREATE, and no mkdir: the table was created
	// whole at lock-file creation, so an ENOENT here means the
	// directory or slot file vanished under a live incarnation — an
	// external sweep. Recreating would mint a fresh inode while
	// surviving holders' locks ride the unlinked one (a silent
	// double-claim), so it surfaces as an undecided error instead:
	// fail-closed for EVERY handle, fresh or long-lived. Removing
	// live coordination state is outside the protection boundary,
	// exactly as unlinking the live lock file is; superseded
	// directories are removed by the lifecycle itself
	// (cross-process.md §Reader Table, slot locks).
	return l.root.OpenFile(fmt.Sprintf("%s/%d", l.dir, idx), os.O_RDWR, 0)
}

func (l *fileLocks) try(idx uint32) (func(), error) {
	f, err := l.open(idx)
	if err != nil {
		return nil, err
	}
	err = flock.TryExclusive(f.Fd())
	if err == nil {
		return func() { _ = f.Close() }, nil
	}
	f.Close()
	if flock.ErrContended(err) {
		return nil, errSlotBusy
	}
	return nil, err
}

func (l *fileLocks) tryHold(idx uint32) (func(), error)  { return l.try(idx) }
func (l *fileLocks) tryProbe(idx uint32) (func(), error) { return l.try(idx) }

// close is a no-op: unlike rangeLocks there are no long-lived
// descriptions to drop (each acquisition owns its own), and the slot
// files themselves are left in place — bounded by MaxReaders, inert
// once their locks die with their holders.
func (l *fileLocks) close() error { return nil }

// holdSet tracks this File's own held slots: release closures by
// index, plus the guard that keeps a handle's second acquisition
// pass from probing its own live readers (same-description
// try-locks do not conflict on the range backend).
type holdSet struct {
	mu    sync.Mutex
	holds map[uint32]func()
}

func (h *holdSet) put(idx uint32, release func()) {
	h.mu.Lock()
	if h.holds == nil {
		h.holds = map[uint32]func(){}
	}
	h.holds[idx] = release
	h.mu.Unlock()
}

func (h *holdSet) has(idx uint32) bool {
	h.mu.Lock()
	_, ok := h.holds[idx]
	h.mu.Unlock()
	return ok
}

func (h *holdSet) take(idx uint32) func() {
	h.mu.Lock()
	rel := h.holds[idx]
	delete(h.holds, idx)
	h.mu.Unlock()
	return rel
}

// AcquireReaderSlot claims a reader slot: lock-then-store. Pass one
// scans from hint for free-looking slots (TxnID == 0); pass two
// try-locks EVERY slot — a nonzero one whose lock acquires is a
// dead owner's residue, taken over in place (the inline form of
// stale reclamation), and a zero slot is re-tried so a peer
// releasing between the passes cannot yield a spurious
// ErrReadersFull (full means every slot's lock is HELD, never "the
// table churned mid-scan"; cross-process.md §Slot acquire). Own
// held slots are skipped structurally (the hold set), never probed.
// In-process acquisition is serialized (acquireMu): two same-handle
// acquirers racing one slot through one hold description would
// otherwise both "win". txnID must be > 0 (0 is the free sentinel).
func (f *File) AcquireReaderSlot(hint uint32, txnID, pid uint64) (uint32, error) {
	if txnID == 0 {
		panic("lock: AcquireReaderSlot requires txnID > 0")
	}
	f.acquireMu.Lock()
	defer f.acquireMu.Unlock()
	maxSlots := f.MaxReaders()
	// A non-busy tryHold error is UNDECIDED (I/O failure, not a live
	// holder). The slot is skipped — never stolen on an error — but
	// the outcome class must survive to the caller: an undecided
	// scan must not report ErrReadersFull, which asserts every slot
	// has a live owner (three-valued verdicts, cross-process.md
	// §Reader Table).
	var undecided error
	take := func(idx uint32) bool {
		if f.holds.has(idx) {
			return false
		}
		release, err := f.locks.tryHold(idx)
		if err != nil {
			if !errors.Is(err, errSlotBusy) && undecided == nil {
				undecided = err
			}
			return false
		}
		slot := f.Slot(idx)
		Store64(&slot.TxnID, txnID)
		Store64(&slot.PID, pid)
		Store64(&slot.Reserved1, 0)
		Store64(&slot.Reserved2, 0)
		Store64(&slot.Reserved3, 0)
		Store64(&slot.Reserved4, 0)
		Store64(&slot.Reserved5, 0)
		f.holds.put(idx, release)
		if hook := acquirePublishHookForTest.Load(); hook != nil {
			(*hook)(idx)
		}
		return true
	}
	idx := hint % maxSlots
	for range maxSlots {
		if Load64(&f.Slot(idx).TxnID) == 0 && take(idx) {
			return idx, nil
		}
		idx = (idx + 1) % maxSlots
	}
	for range maxSlots {
		if take(idx) {
			return idx, nil
		}
		idx = (idx + 1) % maxSlots
	}
	if undecided != nil {
		return NoSlot, fmt.Errorf("lock: reader slot acquisition undecided: %w", undecided)
	}
	return NoSlot, ErrReadersFull
}

// ReleaseReaderSlot frees an owned slot: zero the content, then
// drop the lock — the slot is observably free before it is
// claimable, mirroring acquire's lock-before-store.
func (f *File) ReleaseReaderSlot(idx uint32) {
	release := f.holds.take(idx)
	if release == nil {
		return
	}
	slot := f.Slot(idx)
	Store64(&slot.TxnID, 0)
	Store64(&slot.PID, 0)
	release()
}

// RaiseReaderSlotTxnID raises an owned slot's pinned TxnID — the
// post-publish snapshot-restabilization step, an owner-only
// overwrite made trivially exclusive by the held slot lock. The
// ownership precondition is enforced: raising a slot this File does
// not hold would stomp another owner's pin (or a free slot's
// sentinel), so a violation panics as a programmer error, like the
// txnID==0 precondition on acquire.
func (f *File) RaiseReaderSlotTxnID(idx uint32, txnID uint64) {
	if !f.holds.has(idx) {
		panic("lock: RaiseReaderSlotTxnID on a slot this File does not hold")
	}
	Store64(&f.Slot(idx).TxnID, txnID)
}

// NoReaderTxnID is OldestReaderTxnID's empty-table sentinel.
const NoReaderTxnID = ^uint64(0)

// OldestReaderTxnID returns the minimum TxnID across occupied
// slots — a PURE memory scan, no syscalls on the write path
// (cross-process.md §Reader Table, bound scans stay pure memory
// reads). A stale slot conservatively pins the bound until a reap
// clears it; a mid-acquire slot needs no floor (the restabilization
// argument is scanner-independent).
func (f *File) OldestReaderTxnID() uint64 {
	if f.slots == nil {
		return NoReaderTxnID
	}
	minTxn := NoReaderTxnID
	maxSlots := f.MaxReaders()
	for i := uint32(0); i < maxSlots; i++ {
		if txn := Load64(&f.Slot(i).TxnID); txn != 0 && txn < minTxn {
			minTxn = txn
		}
	}
	return minTxn
}

// ReapStaleReaderSlots probes every occupied slot and clears the
// dead owners' (cross-process.md §Reader Table, stale-slot
// reclamation): the verdict (probe acquired ⇒ owner gone) and the
// clear (TxnID = 0 under the held probe) are one act. Held or
// undecided probes never clear. Needs NO write grant — the slot
// lock itself serializes clearers — so read-only handles reap too.
// Own held slots are skipped structurally.
//
// Returns the number of slots cleared plus the number of UNDECIDED
// probes: an occupied slot whose probe errors (not busy) can be
// neither judged nor cleared, and its nonzero TxnID keeps pinning
// OldestReaderTxnID — conservative for safety, but a PERSISTENT
// undecided (an externally swept slot file under residue) silently
// halts reclamation, so callers surface the count instead of
// swallowing it (background maintenance logs it).
func (f *File) ReapStaleReaderSlots() (cleared, undecided int) {
	if f.slots == nil {
		return 0, 0
	}
	// Same-File clearers are NOT serialized by the slot lock: the
	// range backend probes through one shared description, and two
	// same-description try-locks both "succeed" — one reaper's
	// unlock would strip the other's protection mid-clear. reapMu
	// closes that hole in-process; cross-File clearers hold distinct
	// descriptions and the kernel serializes them.
	f.reapMu.Lock()
	defer f.reapMu.Unlock()
	maxSlots := f.MaxReaders()
	for i := uint32(0); i < maxSlots; i++ {
		if Load64(&f.Slot(i).TxnID) == 0 || f.holds.has(i) {
			continue
		}
		release, err := f.locks.tryProbe(i)
		if err != nil {
			if !errors.Is(err, errSlotBusy) {
				undecided++
			}
			continue // held (live) or undecided: never a stale verdict
		}
		slot := f.Slot(i)
		if Load64(&slot.TxnID) != 0 {
			Store64(&slot.TxnID, 0)
			Store64(&slot.PID, 0)
			cleared++
			if hook := staleClearHookForTest.Load(); hook != nil {
				(*hook)(i)
			}
		}
		release()
	}
	return cleared, undecided
}

// CountActiveReaders returns the number of occupied reader slots
// across the whole table (every process's readers). Lock-free
// per-slot atomic loads: the count can be off by ±N for N
// transitions in flight — a metrics/health signal, never a
// synchronization barrier. The recovery-commit gate also consults
// it; soundness rests on the error directions (a lingering slot
// defers recovery; a post-scan acquirer is the lock-free window
// durability.md's unrecovered-window contract covers).
func (f *File) CountActiveReaders() int {
	max := f.MaxReaders()
	n := 0
	for i := uint32(0); i < max; i++ {
		if Load64(&f.Slot(i).TxnID) != 0 {
			n++
		}
	}
	return n
}
