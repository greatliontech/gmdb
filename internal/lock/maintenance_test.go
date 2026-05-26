package lock

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTryClaimMaintenance: the claim succeeds when no pass has run within
// the interval and stamps the time; it fails (without changing the stamp)
// within the interval, and succeeds again once the interval elapses.
func TestTryClaimMaintenance(t *testing.T) {
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{1}, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	const interval = 500
	if !f.TryClaimMaintenance(1000, interval) { // last==0 ⇒ claim
		t.Fatal("first claim should succeed")
	}
	if got := f.LastMaintenanceTime(); got != 1000 {
		t.Errorf("LastMaintenanceTime = %d, want 1000", got)
	}
	if f.TryClaimMaintenance(1200, interval) { // 1200-1000 = 200 < 500 ⇒ skip
		t.Error("claim within the interval should fail")
	}
	if got := f.LastMaintenanceTime(); got != 1000 {
		t.Errorf("failed claim changed the stamp: %d", got)
	}
	if !f.TryClaimMaintenance(1500, interval) { // 1500-1000 = 500 >= 500 ⇒ claim
		t.Error("claim at the interval boundary should succeed")
	}
	if got := f.LastMaintenanceTime(); got != 1500 {
		t.Errorf("LastMaintenanceTime = %d, want 1500", got)
	}
}

// TestTryClaimMaintenanceAtMostOneWinner (Inv-M1): with many concurrent
// claimers at the same (stale) instant, exactly one wins the CAS — the
// cross-process "≤1 pass per interval" guarantee.
func TestTryClaimMaintenanceAtMostOneWinner(t *testing.T) {
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{2}, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	const n = 64
	var wins atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if f.TryClaimMaintenance(1000, 500) {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("concurrent claims: %d winners, want exactly 1", wins.Load())
	}
}

// TestCoordReapStaleReaderSlotsClearsStaleKeepsLive (Task 2 — background-
// maintenance.md §Stale Reader Slot Cleanup): a maintenance reap acquires
// LOCK_EX itself and clears a dead-process slot while leaving a live one.
// Cross-namespace heartbeat path (slot NS = 0) with an injected fixed
// clock so "aged" vs "fresh" is deterministic — no dependence on the
// CLOCK_BOOTTIME magnitude.
func TestCoordReapStaleReaderSlotsClearsStaleKeepsLive(t *testing.T) {
	root, base, _ := tmpLock(t)
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: [16]byte{0x2A}, MaxReaders: 8})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const now uint64 = 1_000_000_000_000 // 1000 s in ns; >> StaleTimeout
	c := NewCoord(f, CoordOptions{
		PID:           4242,
		PIDNamespace:  99,
		RetryInterval: 10 * time.Millisecond,
		Clock:         func() uint64 { return now },
	})
	t.Cleanup(func() {
		_ = c.Close()
		_ = f.Close()
	})

	stale := staleTimeoutNanos()
	// Slot 0: stale — cross-NS (NS=0 ⇒ heartbeat path), heartbeat aged
	// past StaleTimeout. Raw stores: a deliberate manufactured pre-state.
	s0 := f.Slot(0)
	Store64(&s0.TxnID, 7)
	Store64(&s0.PID, 9999)
	Store64(&s0.Heartbeat, now-2*stale)
	// Slot 1: live — cross-NS, fresh heartbeat.
	s1 := f.Slot(1)
	Store64(&s1.TxnID, 11)
	Store64(&s1.PID, 8888)
	Store64(&s1.Heartbeat, now)

	if err := c.ReapStaleReaderSlots(context.Background()); err != nil {
		t.Fatalf("ReapStaleReaderSlots: %v", err)
	}
	if got := Load64(&s0.TxnID); got != 0 {
		t.Errorf("stale slot not cleared: TxnID = %d, want 0", got)
	}
	if got := Load64(&s1.TxnID); got != 11 {
		t.Errorf("live slot cleared spuriously: TxnID = %d, want 11", got)
	}
	// The first reap did not leak its grant. (Not provable via flock
	// itself: re-locking the same OFD is a kernel no-op success even
	// with no intervening LOCK_UN.) The proof is the flock goroutine's
	// in-process serialisation: it serves one writerRequest at a time
	// off an unbuffered channel and cannot receive this second request
	// until process() for the first has returned — which happens only
	// after its step-4 release (clear-header + LOCK_UN) runs. A leaked
	// grant would wedge the goroutine in its step-4 select, so this
	// second AcquireWriter would hang (test timeout) instead of nil.
	if err := c.ReapStaleReaderSlots(context.Background()); err != nil {
		t.Fatalf("second reap (proves the first released its grant): %v", err)
	}
}

// TestCoordReapStaleReaderSlotsCtxCancel: a cancelled ctx surfaces from
// AcquireWriter and the reader table is left untouched — no clear ever
// happens without the grant (the LOCK_EX precondition holds even on the
// error path).
func TestCoordReapStaleReaderSlotsCtxCancel(t *testing.T) {
	c, f := newTestCoord(t, 10*time.Millisecond)
	// Forge a stale-looking slot; it must survive a cancelled reap.
	s := f.Slot(0)
	Store64(&s.TxnID, 5)
	Store64(&s.PID, 9999)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.ReapStaleReaderSlots(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ReapStaleReaderSlots on cancelled ctx: got %v, want context.Canceled", err)
	}
	if got := Load64(&s.TxnID); got != 5 {
		t.Errorf("reader table mutated despite no grant: TxnID = %d, want 5", got)
	}
}
