package lock

import (
	"context"
	"errors"
	"math"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// openTestFile mints an *os.Root over a fresh temp dir, opens a lock
// file with the given MaxReaders, and registers cleanup. Used by the
// reader-slot tests that want direct *File access without going
// through a Coord.
func openTestFile(t *testing.T, maxReaders uint32) *File {
	t.Helper()
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0xAB, 0xCD, 0xEF},
		MaxReaders: maxReaders,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestAcquireReaderSlotBasic(t *testing.T) {
	// Sanity: acquire returns a valid index, slot fields hold the
	// stamped identity, TxnID = txnID, PID = pid. End-to-end ordering
	// invariant exercised more aggressively below.
	f := openTestFile(t, 4)
	idx, err := f.AcquireReaderSlot(0, 42, 7777, 12345, 99, 100)
	if err != nil {
		t.Fatalf("AcquireReaderSlot: %v", err)
	}
	if idx >= 4 {
		t.Fatalf("slot index %d out of range", idx)
	}
	slot := f.Slot(idx)
	if got := Load64(&slot.TxnID); got != 42 {
		t.Errorf("TxnID = %d, want 42", got)
	}
	if got := Load64(&slot.PID); got != 7777 {
		t.Errorf("PID = %d, want 7777", got)
	}
	if got := Load64(&slot.ProcessStartTime); got != 12345 {
		t.Errorf("PST = %d, want 12345", got)
	}
	if got := Load64(&slot.PIDNamespace); got != 99 {
		t.Errorf("PIDNamespace = %d, want 99", got)
	}
	if got := Load64(&slot.Heartbeat); got != 100 {
		t.Errorf("Heartbeat = %d, want 100", got)
	}
	if got := Load64(&slot.HintEpoch); got != 0 {
		t.Errorf("HintEpoch = %d, want 0", got)
	}
}

func TestAcquireReaderSlotFull(t *testing.T) {
	f := openTestFile(t, 2)
	a, err := f.AcquireReaderSlot(0, 1, 1, 1, 1, 1)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	b, err := f.AcquireReaderSlot(0, 2, 1, 1, 1, 1)
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if a == b {
		t.Errorf("acquire collided on idx %d", a)
	}
	_, err = f.AcquireReaderSlot(0, 3, 1, 1, 1, 1)
	if !errors.Is(err, ErrReadersFull) {
		t.Errorf("acquire 3: got %v, want ErrReadersFull", err)
	}
}

func TestAcquireReaderSlotHintSeeds(t *testing.T) {
	// hint=k seeds the scan to start at slot k. Pin the directional
	// behavior (separate from the per-Coord hint *advancement*, which
	// has its own test below).
	f := openTestFile(t, 4)
	// Occupy slot 0 via a raw store (skips the spec-ordered acquire
	// path; this is a deliberate manufactured pre-state).
	Store64(&f.Slot(0).TxnID, 99)
	idx, err := f.AcquireReaderSlot(2, 1, 1, 1, 1, 1)
	if err != nil {
		t.Fatalf("AcquireReaderSlot hint=2: %v", err)
	}
	if idx != 2 {
		t.Errorf("hint=2 landed on slot %d, want 2", idx)
	}
}

func TestCoordAcquireReaderHintAdvances(t *testing.T) {
	// Per cross-process.md §Reader Table: "Under steady-state load,
	// the hint points to a recently-freed slot." Coord.AcquireReader
	// stores the just-allocated slot index back into c.readerSlotHint
	// so the next scan starts from a fresh-ish region. Pin that
	// post-condition.
	c, _ := newTestCoord(t, 10*time.Millisecond)
	ctx := context.Background()
	idx, err := c.AcquireReader(ctx, 1)
	if err != nil {
		t.Fatalf("AcquireReader: %v", err)
	}
	if got := c.readerSlotHint.Load(); got != idx {
		t.Errorf("post-Acquire hint = %d, want %d (idx)", got, idx)
	}
	c.ReleaseReader(idx)
}

func TestAcquireReaderSlotTxnIDZeroPanics(t *testing.T) {
	// Documented precondition: txnID=0 collides with the per-slot
	// "free" sentinel. Surface as panic so masking is impossible.
	f := openTestFile(t, 1)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on txnID=0")
		}
	}()
	_, _ = f.AcquireReaderSlot(0, 0, 1, 1, 1, 1)
}

func TestReleaseReaderSlotOrdering(t *testing.T) {
	// Pin the release-ordered atomic stores: PID first, TxnID last.
	// We can't directly observe ordering of completed stores, but we
	// CAN observe the post-condition: TxnID transitions to 0 after
	// every other field is already 0. We assert the final state and
	// rely on the per-store atomicity for correctness.
	//
	// The TestStaleClearOrdering test below provides the
	// race-against-acquire angle that actually exercises the
	// ordering constraint (HintEpoch first prevents acquirer
	// inheritance of stale epoch).
	f := openTestFile(t, 1)
	idx, err := f.AcquireReaderSlot(0, 42, 7777, 12345, 99, 100)
	if err != nil {
		t.Fatalf("AcquireReaderSlot: %v", err)
	}
	f.ReleaseReaderSlot(idx)
	slot := f.Slot(idx)
	for name, p := range map[string]*uint64{
		"TxnID": &slot.TxnID, "PID": &slot.PID,
		"Heartbeat": &slot.Heartbeat, "HintEpoch": &slot.HintEpoch,
	} {
		if got := Load64(p); got != 0 {
			t.Errorf("post-release %s = %d, want 0", name, got)
		}
	}
}

func TestClearStaleReaderSlotOrdering(t *testing.T) {
	// Pin HintEpoch-first ordering: after ClearStaleReaderSlot the
	// slot is free (TxnID == 0) AND any prior epoch is cleared
	// (HintEpoch == 0). The reverse order would let a fresh acquirer
	// CAS-win TxnID after the TxnID=0 store but before the
	// HintEpoch=0 store and inherit a stale epoch — see the spec's
	// per-occupant timer invariant.
	f := openTestFile(t, 1)
	// Manufacture a stuck mid-acquire state: TxnID != 0, PID == 0,
	// Heartbeat == 0, HintEpoch = old-stale-value.
	slot := f.Slot(0)
	Store64(&slot.TxnID, 5)
	Store64(&slot.HintEpoch, 12345)
	f.ClearStaleReaderSlot(0)
	if got := Load64(&slot.TxnID); got != 0 {
		t.Errorf("post-clear TxnID = %d, want 0", got)
	}
	if got := Load64(&slot.HintEpoch); got != 0 {
		t.Errorf("post-clear HintEpoch = %d, want 0", got)
	}
}

func TestOldestReaderTxnIDEmpty(t *testing.T) {
	f := openTestFile(t, 4)
	got := f.OldestReaderTxnID(99, 0, uint64(DefaultStaleTimeout))
	if got != math.MaxUint64 {
		t.Errorf("empty table OldestReaderTxnID = %d, want MaxUint64", got)
	}
}

func TestOldestReaderTxnIDMinOfMany(t *testing.T) {
	f := openTestFile(t, 4)
	// All slots in this process's namespace, all alive.
	myPID := uint64(os.Getpid())
	myPST, _ := ProcessStartTime(os.Getpid())
	myNS, _ := PIDNamespace()
	// PIDNamespace may be 0 on non-Linux — that routes through the
	// cross-namespace heartbeat path; we provide a recent heartbeat
	// so the slots aren't classified stale.
	now := uint64(time.Now().UnixNano())
	for i, txn := range []uint64{50, 10, 30, 25} {
		idx, err := f.AcquireReaderSlot(uint32(i), txn, myPID, myPST, myNS, now)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		_ = idx
	}
	got := f.OldestReaderTxnID(myNS, now, uint64(DefaultStaleTimeout))
	if got != 10 {
		t.Errorf("OldestReaderTxnID = %d, want 10", got)
	}
}

func TestOldestReaderTxnIDClearsStaleHeartbeat(t *testing.T) {
	// Cross-namespace + aged heartbeat ⇒ stale ⇒ clear in-place.
	// Slot's PID is a foreign value in a "different" namespace so
	// the heartbeat path is taken.
	f := openTestFile(t, 2)
	now := uint64(time.Now().UnixNano())
	stale := now - 2*uint64(DefaultStaleTimeout)
	// Both slots use foreign namespace stamps (different from
	// ourPIDNS=99 in the scan below). Stamping PIDNS=99 here would
	// trigger the same-namespace IsAlive(pid) path against PIDs
	// 9999/8888 which don't exist in this test process, classifying
	// both as stale and defeating the heartbeat-path intent.
	if _, err := f.AcquireReaderSlot(0, 7, 9999, 1, 42, stale); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Live slot also cross-namespace, fresh heartbeat.
	if _, err := f.AcquireReaderSlot(1, 11, 8888, 1, 77, now); err != nil {
		t.Fatalf("acquire live: %v", err)
	}
	// First call should clear the stale slot (its TxnID=7) and
	// leave the live one (TxnID=11). For the live slot to count as
	// alive in our scan we need IsAlive(8888) to return false (no
	// such PID) — and on the cross-namespace path that triggers
	// the heartbeat fallback. Since slot 1 has a fresh heartbeat,
	// it stays.
	got := f.OldestReaderTxnID(99, now, uint64(DefaultStaleTimeout))
	if got != 11 {
		t.Errorf("after stale clear, OldestReaderTxnID = %d, want 11", got)
	}
	if Load64(&f.Slot(0).TxnID) != 0 {
		t.Errorf("stale slot 0 not cleared; TxnID = %d", Load64(&f.Slot(0).TxnID))
	}
}

func TestOldestReaderTxnIDCase0aFreshHBSkipsClear(t *testing.T) {
	// Case 0a: TxnID != 0, PID == 0, Heartbeat != 0 and fresh.
	// The acquirer made it past step 4a (Heartbeat store) but is
	// still mid-publish (PID not yet stamped). Treat as live —
	// TxnID enters min; slot is NOT cleared.
	f := openTestFile(t, 1)
	slot := f.Slot(0)
	now := uint64(1_000_000_000)
	stale := uint64(time.Second)
	Store64(&slot.TxnID, 42)
	Store64(&slot.Heartbeat, now-stale/2) // fresh
	got := f.OldestReaderTxnID(99, now, stale)
	if got != 42 {
		t.Errorf("case 0a: got %d, want 42 (live mid-publish)", got)
	}
	if Load64(&slot.TxnID) != 42 {
		t.Errorf("case 0a: slot cleared spuriously")
	}
}

func TestOldestReaderTxnIDCase0bStaleHBClears(t *testing.T) {
	// Case 0b: TxnID != 0, PID == 0, Heartbeat != 0 but aged out.
	// The acquirer crashed between step 4a (Heartbeat store) and
	// step 4e (PID store); heartbeat is now older than StaleTimeout.
	// Clear the slot.
	f := openTestFile(t, 1)
	slot := f.Slot(0)
	// now must exceed 2*stale so the "aged out" heartbeat is a genuine PAST
	// value. (The earlier now=1s, hb=now-2*stale underflowed to a ~2^64
	// future stamp; the pre-future-guard code cleared it only via a second
	// underflow in the comparison — a pass for the wrong reason. With the
	// future-heartbeat guard a future stamp is correctly treated as fresh,
	// so the stale case must be constructed as an actual past timestamp.)
	stale := uint64(time.Second)
	now := 10 * stale
	Store64(&slot.TxnID, 42)
	Store64(&slot.Heartbeat, now-2*stale) // aged out: now-hb = 2*stale > stale
	got := f.OldestReaderTxnID(99, now, stale)
	if got != math.MaxUint64 {
		t.Errorf("case 0b: got %d, want MaxUint64 (slot cleared)", got)
	}
	if Load64(&slot.TxnID) != 0 {
		t.Errorf("case 0b: slot not cleared")
	}
}

func TestOldestReaderTxnIDCase0aFutureHBSkipsClear(t *testing.T) {
	// Regression (reader-stale-detection future-heartbeat underflow): a
	// mid-publish reader (PID==0) whose Heartbeat is AHEAD of the scanner's
	// nowNanos must be treated as live. Pre-fix nowNanos-hb underflowed
	// (unsigned) to ~2^64, which is not <= staleTimeout, so the slot fell
	// through to the 0b clear and a live reader was evicted — its pinned
	// TxnID left the table and RPL reclamation could free pages it was about
	// to read. The HB-first/PID-last acquire ordering exists precisely to
	// keep this mid-publish window safe; the underflow defeated it.
	f := openTestFile(t, 1)
	slot := f.Slot(0)
	now := uint64(1_000_000_000)
	stale := uint64(time.Second)
	Store64(&slot.TxnID, 42)
	Store64(&slot.Heartbeat, now+stale) // FUTURE — reader's clock read raced ahead of the scan
	got := f.OldestReaderTxnID(99, now, stale)
	if got != 42 {
		t.Errorf("case 0a future HB: got %d, want 42 (live mid-publish, not evicted)", got)
	}
	if Load64(&slot.TxnID) != 42 {
		t.Errorf("case 0a future HB: live slot cleared (underflow eviction)")
	}
}

func TestOldestReaderTxnIDCase0cFutureEpochSkipsClear(t *testing.T) {
	// Case 0c (PID==0, Heartbeat==0) with a HintEpoch orphan anchor in the
	// future relative to this scanner (a peer writer stamped it on a clock
	// ahead of ours). nowNanos-epoch must not underflow-clear a slot whose
	// orphan timer has not actually elapsed.
	f := openTestFile(t, 1)
	slot := f.Slot(0)
	now := uint64(1_000_000_000)
	stale := uint64(time.Second)
	Store64(&slot.TxnID, 9)
	Store64(&slot.HintEpoch, now+stale) // FUTURE epoch
	got := f.OldestReaderTxnID(99, now, stale)
	if got != 9 {
		t.Errorf("case 0c future epoch: got %d, want 9 (orphan timer not elapsed)", got)
	}
	if Load64(&slot.TxnID) != 9 {
		t.Errorf("case 0c future epoch: live slot cleared (underflow eviction)")
	}
}

func TestOldestReaderTxnIDCase2FutureHBSkipsClear(t *testing.T) {
	// Case 2 (cross-namespace, heartbeat-only liveness) with a future
	// heartbeat — same underflow guard. On darwin/freebsd the codebase's own
	// model has per-process CLOCK_MONOTONIC origins so a peer's stamp can
	// routinely exceed our nowNanos, making this directly reachable.
	f := openTestFile(t, 1)
	now := uint64(time.Now().UnixNano())
	future := now + uint64(DefaultStaleTimeout) // ahead of the scan clock
	// Foreign namespace (≠ ourPIDNS=99 in the scan) so the heartbeat path is
	// taken rather than same-namespace IsAlive against a nonexistent PID.
	if _, err := f.AcquireReaderSlot(0, 7, 9999, 1, 42, future); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	got := f.OldestReaderTxnID(99, now, uint64(DefaultStaleTimeout))
	if got != 7 {
		t.Errorf("case 2 future HB: got %d, want 7 (live, not evicted)", got)
	}
	if Load64(&f.Slot(0).TxnID) != 7 {
		t.Errorf("case 2 future HB: live slot cleared (underflow eviction)")
	}
}

func TestOldestReaderTxnIDHintEpochAnchor(t *testing.T) {
	// Case 0c: PID==0 AND Heartbeat==0 — first observer CAS-stores
	// HintEpoch=nowNanos; the slot is NOT cleared until
	// (now - HintEpoch) > staleTimeout. Two scans separated in
	// "wall time" pin the protocol.
	f := openTestFile(t, 1)
	slot := f.Slot(0)
	// Manufacture the stuck-mid-acquire state directly.
	Store64(&slot.TxnID, 9)
	// First scan: anchors HintEpoch.
	t0 := uint64(1_000_000_000)
	staleNs := uint64(time.Second)
	got := f.OldestReaderTxnID(99, t0, staleNs)
	// Skip — slot remains observably non-free, treated as live for
	// reclamation safety (TxnID=9 enters min).
	if got != 9 {
		t.Errorf("first scan: got %d, want 9 (anchor-set, treat as live)", got)
	}
	if Load64(&slot.HintEpoch) != t0 {
		t.Errorf("HintEpoch after first scan = %d, want %d", Load64(&slot.HintEpoch), t0)
	}
	if Load64(&slot.TxnID) != 9 {
		t.Errorf("first scan must NOT clear slot; TxnID = %d", Load64(&slot.TxnID))
	}
	// Second scan within timeout: still skip.
	got = f.OldestReaderTxnID(99, t0+staleNs/2, staleNs)
	if got != 9 {
		t.Errorf("second scan (in-window): got %d, want 9", got)
	}
	if Load64(&slot.TxnID) != 9 {
		t.Errorf("in-window scan cleared slot")
	}
	// Third scan past timeout: clear.
	got = f.OldestReaderTxnID(99, t0+2*staleNs, staleNs)
	if Load64(&slot.TxnID) != 0 {
		t.Errorf("post-timeout scan must clear; TxnID = %d", Load64(&slot.TxnID))
	}
	if got != math.MaxUint64 {
		t.Errorf("post-clear with no other slots: got %d, want MaxUint64", got)
	}
	if Load64(&slot.HintEpoch) != 0 {
		t.Errorf("HintEpoch not reset by ClearStaleReaderSlot")
	}
}

func TestOldestReaderTxnIDSameNamespaceLiveSkipsClear(t *testing.T) {
	// Same-namespace PID==!0 path: kill(pid, 0) on our own PID
	// returns success — slot must NOT be cleared and TxnID must
	// participate in min.
	f := openTestFile(t, 1)
	myPID := uint64(os.Getpid())
	myPST, err := ProcessStartTime(os.Getpid())
	if err != nil {
		t.Skipf("ProcessStartTime unavailable on this platform: %v", err)
	}
	myNS, _ := PIDNamespace()
	if myNS == 0 {
		// Cross-namespace path would be taken; skip the same-NS
		// assertion. Heartbeat fallback covers it elsewhere.
		t.Skip("PIDNamespace = 0 on this host; same-namespace path not exercised")
	}
	now := uint64(time.Now().UnixNano())
	if _, err := f.AcquireReaderSlot(0, 77, myPID, myPST, myNS, now); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	got := f.OldestReaderTxnID(myNS, now, uint64(DefaultStaleTimeout))
	if got != 77 {
		t.Errorf("same-NS live slot dropped: got %d, want 77", got)
	}
	if Load64(&f.Slot(0).TxnID) != 77 {
		t.Errorf("same-NS live slot cleared spuriously")
	}
}

func TestOldestReaderTxnIDSameNamespacePSTMismatchClears(t *testing.T) {
	// Same-namespace path step (b): PID alive but ProcessStartTime
	// mismatch ⇒ PID was recycled ⇒ stale, clear.
	f := openTestFile(t, 1)
	myPID := uint64(os.Getpid())
	myPST, err := ProcessStartTime(os.Getpid())
	if err != nil {
		t.Skipf("ProcessStartTime unavailable: %v", err)
	}
	myNS, _ := PIDNamespace()
	if myNS == 0 {
		t.Skip("PIDNamespace = 0; same-namespace path not exercised")
	}
	now := uint64(time.Now().UnixNano())
	// Acquire with a deliberately wrong PST so the rescan sees a
	// mismatch and treats the slot as PID-recycled-since-acquire.
	if _, err := f.AcquireReaderSlot(0, 33, myPID, myPST+1, myNS, now); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	got := f.OldestReaderTxnID(myNS, now, uint64(DefaultStaleTimeout))
	if Load64(&f.Slot(0).TxnID) != 0 {
		t.Errorf("PST-mismatch slot not cleared")
	}
	if got != math.MaxUint64 {
		t.Errorf("after sole stale clear: got %d, want MaxUint64", got)
	}
}

func TestOldestReaderTxnIDSameNamespaceDeadPIDClears(t *testing.T) {
	// Same-namespace path step (a): we need a PID that is
	// definitively dead. Approach: spawn a child, capture its PID,
	// wait for it to exit + reap it (so its PID is freed at the
	// kernel level), then stamp the slot with the dead PID + a
	// matching PST recorded BEFORE the wait.
	//
	// Race robustness: between Wait and the OldestReaderTxnID scan
	// the kernel could recycle the freed PID to an unrelated
	// process. If that happens, IsAlive(pid) returns true and the
	// code falls through to the ProcessStartTime check — the
	// recycled process's PST will not equal the recorded one, so
	// the PST-mismatch branch (reader.go) clears the slot. The
	// test is therefore deterministic against PID-recycle, not
	// merely probabilistic.
	myNS, _ := PIDNamespace()
	if myNS == 0 {
		t.Skip("PIDNamespace = 0; same-namespace path not exercised")
	}
	// /bin/true exits immediately. Wait reaps the zombie. PID is
	// then a "dead PID" until the OS recycles it. With 4 M PIDs
	// (Linux default), recycle in a 1-tick window is improbable.
	cmd := exec("/bin/true")
	if cmd == nil {
		t.Skip("no /bin/true available")
	}
	pid := uint64(cmd.Process.Pid)
	pst, _ := ProcessStartTime(int(pid))
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	f := openTestFile(t, 1)
	now := uint64(time.Now().UnixNano())
	if _, err := f.AcquireReaderSlot(0, 55, pid, pst, myNS, now); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	got := f.OldestReaderTxnID(myNS, now, uint64(DefaultStaleTimeout))
	if Load64(&f.Slot(0).TxnID) != 0 {
		t.Errorf("dead-PID slot not cleared")
	}
	if got != math.MaxUint64 {
		t.Errorf("after dead-PID clear: got %d, want MaxUint64", got)
	}
}

// exec is a thin wrapper around os/exec.Cmd.Start with stdout/stderr
// muted; keeps the dead-PID test compact. Returns nil on
// non-existence of the binary so the caller can Skip.
func exec(path string) *cmdProcess {
	c := &cmdProcess{}
	if _, err := osStatFile(path); err != nil {
		return nil
	}
	if err := c.start(path); err != nil {
		return nil
	}
	return c
}

// cmdProcess avoids importing os/exec at the package level for one
// test — keeps imports minimal. We just need PID + Wait().
type cmdProcess struct {
	Process *os.Process
}

func (c *cmdProcess) start(path string) error {
	attr := &os.ProcAttr{}
	p, err := os.StartProcess(path, []string{path}, attr)
	if err != nil {
		return err
	}
	c.Process = p
	return nil
}

func (c *cmdProcess) Wait() error {
	_, err := c.Process.Wait()
	return err
}

func osStatFile(path string) (os.FileInfo, error) { return os.Stat(path) }

func TestCoordAcquireReleaseReader(t *testing.T) {
	c, f := newTestCoord(t, 10*time.Millisecond)
	ctx := context.Background()
	idx, err := c.AcquireReader(ctx, 5)
	if err != nil {
		t.Fatalf("AcquireReader: %v", err)
	}
	if idx == NoSlot {
		t.Fatal("AcquireReader returned NoSlot")
	}
	slot := f.Slot(idx)
	if got := Load64(&slot.TxnID); got != 5 {
		t.Errorf("slot TxnID = %d, want 5", got)
	}
	if got := Load64(&slot.PID); got != 4242 {
		t.Errorf("slot PID = %d, want 4242", got)
	}
	c.ReleaseReader(idx)
	if got := Load64(&slot.TxnID); got != 0 {
		t.Errorf("post-Release TxnID = %d, want 0", got)
	}
}

func TestCoordAcquireReaderRespectsCtxCancel(t *testing.T) {
	c, _ := newTestCoord(t, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.AcquireReader(ctx, 1)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("AcquireReader on cancelled ctx: got %v, want context.Canceled", err)
	}
}

func TestCoordAcquireReaderFullNoDeadline(t *testing.T) {
	// Table size 1; one acquire fills it; second acquire surfaces
	// ErrReadersFull immediately when ctx has no deadline.
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0x1}, MaxReaders: 1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := NewCoord(f, CoordOptions{PID: 1, RetryInterval: 10 * time.Millisecond})
	t.Cleanup(func() {
		_ = c.Close()
		_ = f.Close()
	})
	if _, err := c.AcquireReader(context.Background(), 1); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	_, err = c.AcquireReader(context.Background(), 2)
	if !errors.Is(err, ErrReadersFull) {
		t.Errorf("no-deadline second acquire: got %v, want ErrReadersFull", err)
	}
}

func TestCoordAcquireReaderFullWithDeadline(t *testing.T) {
	// With a deadline, AcquireReader retries until a slot frees or
	// the deadline fires. Verify deadline-driven timeout.
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0x2}, MaxReaders: 1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := NewCoord(f, CoordOptions{PID: 1, RetryInterval: 10 * time.Millisecond})
	t.Cleanup(func() {
		_ = c.Close()
		_ = f.Close()
	})
	if _, err := c.AcquireReader(context.Background(), 1); err != nil {
		t.Fatalf("hold: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = c.AcquireReader(ctx, 2)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadlined second acquire: got %v, want DeadlineExceeded", err)
	}
}

func TestCoordOldestReaderTxnIDLiveWithFlock(t *testing.T) {
	// Caller-side LOCK_EX precondition test. We take LOCK_EX on
	// the fd ourselves (mimicking the writer-path flock goroutine)
	// before calling OldestReaderTxnID, then assert the value
	// reflects the live readers.
	//
	// The Coord under test uses the real os.Getpid() / Process
	// StartTime so the same-namespace IsAlive path classifies the
	// slots as live. newTestCoord's fake PID=4242 would be killed
	// by IsAlive (no such PID) — wrong harness for this test.
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0xAB}, MaxReaders: 8,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	myPST, _ := ProcessStartTime(os.Getpid())
	myNS, _ := PIDNamespace()
	c := NewCoord(f, CoordOptions{
		PID:              uint64(os.Getpid()),
		ProcessStartTime: myPST,
		PIDNamespace:     myNS,
		RetryInterval:    10 * time.Millisecond,
	})
	t.Cleanup(func() {
		_ = c.Close()
		_ = f.Close()
	})
	ctx := context.Background()
	// Two concurrent reader-tx Begins -> two slots.
	idxA, err := c.AcquireReader(ctx, 10)
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	idxB, err := c.AcquireReader(ctx, 7)
	if err != nil {
		t.Fatalf("acquire B: %v", err)
	}
	// Take LOCK_EX so OldestReaderTxnID is invoked in its
	// production state.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock: %v", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	got := c.OldestReaderTxnID()
	if got != 7 {
		t.Errorf("OldestReaderTxnID = %d, want 7", got)
	}
	c.ReleaseReader(idxA)
	c.ReleaseReader(idxB)
}

func TestCoordStaleTimeoutThreadsToOldestReaderTxnID(t *testing.T) {
	// The configured CoordOptions.StaleTimeout — not the hard-coded
	// DefaultStaleTimeout — governs reader-slot stale eviction in
	// OldestReaderTxnID (the stale-detection window is caller-tunable
	// via Options.StaleTimeout). Manufacture one cross-namespace reader slot
	// whose heartbeat lags the scan clock by `age`, then read it back
	// under two coords whose StaleTimeouts straddle `age`: the short
	// window evicts the slot (no live reader ⇒ MaxUint64 sentinel); the
	// long window keeps it (returns its TxnID).
	const now uint64 = 1_000_000_000_000 // 1000 s in ns
	const age = 5 * time.Second
	aged := now - uint64(age)

	read := func(stale time.Duration) uint64 {
		f := openTestFile(t, 4)
		// Manufacture a slot whose namespace stays 0 so the scan (with
		// ourPIDNS = 0) takes the cross-/unknown-namespace heartbeat
		// path rather than the same-NS kill() path.
		s := f.Slot(0)
		Store64(&s.TxnID, 42)
		Store64(&s.PID, 9999)
		Store64(&s.Heartbeat, aged)
		c := NewCoord(f, CoordOptions{
			PID:           1,
			PIDNamespace:  0,
			RetryInterval: 10 * time.Millisecond,
			StaleTimeout:  stale,
			Clock:         func() uint64 { return now },
		})
		t.Cleanup(func() { _ = c.Close() })
		// OldestReaderTxnID's documented precondition: caller holds
		// LOCK_EX. The Coord's flock goroutine never flocks here (no
		// writer request is issued), so taking it directly is safe.
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatalf("flock: %v", err)
		}
		defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
		return c.OldestReaderTxnID()
	}

	if got := read(age - 2*time.Second); got != math.MaxUint64 {
		t.Errorf("StaleTimeout (%v) < slot age (%v): got %d, want MaxUint64 (slot evicted)", age-2*time.Second, age, got)
	}
	if got := read(age + 2*time.Second); got != 42 {
		t.Errorf("StaleTimeout (%v) > slot age (%v): got %d, want 42 (slot retained)", age+2*time.Second, age, got)
	}
}

// TestAcquireReaderConcurrentNoSlotAliasing exercises the CAS-
// serialisation: N goroutines acquire concurrently; assert no two
// land on the same slot. Pinpoints invariant: the CAS on TxnID
// gates ownership.
func TestAcquireReaderConcurrentNoSlotAliasing(t *testing.T) {
	const N = 64
	f := openTestFile(t, N)
	var wg sync.WaitGroup
	slots := make([]uint32, N)
	var errCount atomic.Int32
	now := uint64(time.Now().UnixNano())
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			idx, err := f.AcquireReaderSlot(0, uint64(i+1), uint64(i+1), 1, 99, now)
			if err != nil {
				errCount.Add(1)
				return
			}
			slots[i] = idx
		}(i)
	}
	wg.Wait()
	if e := errCount.Load(); e != 0 {
		t.Fatalf("%d acquire errors", e)
	}
	seen := make(map[uint32]int)
	for i, s := range slots {
		if prev, ok := seen[s]; ok {
			t.Errorf("slot %d aliased: i=%d and i=%d", s, prev, i)
		}
		seen[s] = i
	}
}

// TestCoordReaderHeartbeatRegistration confirms AcquireReader
// registers the slot with the heartbeat goroutine's active list,
// ReleaseReader unregisters, and (the structural test) the
// goroutine's tick refreshes the slot's Heartbeat field while
// active.
func TestCoordReaderHeartbeatRegistration(t *testing.T) {
	clk := atomic.Uint64{}
	clk.Store(1)
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{
		Root: root, Base: base, DataUUID: [16]byte{0x3}, MaxReaders: 2,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := NewCoord(f, CoordOptions{
		PID:               4242,
		RetryInterval:     10 * time.Millisecond,
		HeartbeatInterval: 5 * time.Millisecond, // tight cadence for the test
		Clock:             func() uint64 { return clk.Load() },
	})
	t.Cleanup(func() {
		_ = c.Close()
		_ = f.Close()
	})
	idx, err := c.AcquireReader(context.Background(), 1)
	if err != nil {
		t.Fatalf("AcquireReader: %v", err)
	}
	// Advance the test clock; wait for the heartbeat tick to fire
	// and overwrite the slot's Heartbeat with the new value.
	clk.Store(999)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if Load64(&f.Slot(idx).Heartbeat) == 999 {
			break
		}
		time.Sleep(time.Millisecond)
		runtime.Gosched()
	}
	if got := Load64(&f.Slot(idx).Heartbeat); got != 999 {
		t.Errorf("heartbeat goroutine did not refresh slot: got %d, want 999", got)
	}
	c.ReleaseReader(idx)
	// After ReleaseReader the slot must NOT be refreshed on the
	// next tick. Bump the clock and verify the slot stays at 0.
	clk.Store(7777)
	time.Sleep(20 * time.Millisecond)
	if got := Load64(&f.Slot(idx).Heartbeat); got != 0 {
		t.Errorf("post-Release Heartbeat = %d, want 0 (active-list unregister failed)", got)
	}
}
