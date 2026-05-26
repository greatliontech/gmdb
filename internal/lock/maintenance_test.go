package lock

import (
	"sync"
	"sync/atomic"
	"testing"
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
