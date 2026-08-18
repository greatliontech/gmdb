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

// TestCoordReapStaleReaderSlotsClearsStaleKeepsLive (Task 2 —
// background-maintenance.md §Stale Reader Slot Cleanup): the
// probe-based reap clears a dead owner's residue while leaving a
// live peer's held slot untouched — no grant, no identity, no
// clock.
func TestCoordReapStaleReaderSlotsClearsStaleKeepsLive(t *testing.T) {
	root, base, _ := tmpLock(t)
	params := OpenParams{Root: root, Base: base, DataUUID: [16]byte{0x2A}, MaxReaders: 8}
	f, err := Open(params)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := NewCoord(f, CoordOptions{
		PID:           4242,
		RetryInterval: 10 * time.Millisecond,
	})
	t.Cleanup(func() {
		_ = c.Close()
		_ = f.Close()
	})
	// A live PEER handle (distinct open file descriptions — its
	// locks conflict with c's exactly as another process's would)
	// holds slot occupancy for real.
	peer, err := Open(params)
	if err != nil {
		t.Fatalf("Open peer: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	liveIdx, err := peer.AcquireReaderSlot(1, 11, 8888)
	if err != nil {
		t.Fatalf("peer acquire: %v", err)
	}

	// Dead residue: raw stores with no lock held anywhere.
	s0 := f.Slot(0)
	Store64(&s0.TxnID, 7)
	Store64(&s0.PID, 9999)

	if _, _, err := c.ReapStaleReaderSlots(context.Background()); err != nil {
		t.Fatalf("ReapStaleReaderSlots: %v", err)
	}
	if got := Load64(&s0.TxnID); got != 0 {
		t.Errorf("stale slot not cleared: TxnID = %d, want 0", got)
	}
	if got := Load64(&f.Slot(liveIdx).TxnID); got != 11 {
		t.Errorf("live slot cleared spuriously: TxnID = %d, want 11", got)
	}
	// Idempotent, grant-free: an immediate second reap runs clean.
	if _, _, err := c.ReapStaleReaderSlots(context.Background()); err != nil {
		t.Fatalf("second reap: %v", err)
	}
	peer.ReleaseReaderSlot(liveIdx)
}

// TestCoordReapStaleReaderSlotsCtxCancel: a cancelled ctx surfaces
// before any probe and the reader table is left untouched.
func TestCoordReapStaleReaderSlotsCtxCancel(t *testing.T) {
	c, f := newTestCoord(t, 10*time.Millisecond)
	// Forge a stale-looking slot; it must survive a cancelled reap.
	s := f.Slot(0)
	Store64(&s.TxnID, 5)
	Store64(&s.PID, 9999)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.ReapStaleReaderSlots(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ReapStaleReaderSlots on cancelled ctx: got %v, want context.Canceled", err)
	}
	if got := Load64(&s.TxnID); got != 5 {
		t.Errorf("reader table mutated despite cancelled ctx: TxnID = %d, want 5", got)
	}
}
