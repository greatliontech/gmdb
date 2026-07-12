package pager

import (
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// TestColdTrackingRecordsAccessRange verifies the Options.ReclaimOnClose
// accessed-page tracking: once enabled, Page / pageRaw fold every
// touched id into [cold.min, cold.max], and AdviseColdAccessed issues
// the MADV_COLD over that range without error (advisory; tolerated where
// the kernel lacks MADV_COLD).
func TestColdTrackingRecordsAccessRange(t *testing.T) {
	f, _ := makeFile(t, 8)
	defer f.Close()
	p, err := NewReader(f, page.Config{PageSize: testPageSize}, 8*testPageSize)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer p.Close()

	// Reads before EnableColdTracking are not recorded.
	_ = p.pageRaw(3)
	if p.cold.enabled {
		t.Fatal("cold.enabled set before EnableColdTracking")
	}

	p.EnableColdTracking()
	// Touch pages 2, 5, 3 out of order, via both access methods.
	if _, err := p.Page(2); err != nil {
		t.Fatalf("Page(2): %v", err)
	}
	_ = p.pageRaw(5)
	if _, err := p.Page(3); err != nil {
		t.Fatalf("Page(3): %v", err)
	}

	if got := p.cold.min.Load(); got != 2 {
		t.Errorf("cold.min = %d, want 2", got)
	}
	if got := p.cold.max.Load(); got != 5 {
		t.Errorf("cold.max = %d, want 5", got)
	}
	if err := p.AdviseColdAccessed(); err != nil {
		t.Errorf("AdviseColdAccessed over [2,5]: %v", err)
	}
}

// TestColdTrackingNoAccessNoAdvise confirms that with tracking enabled
// but no page read, AdviseColdAccessed is a no-op (cold.min stays
// MaxUint64 > cold.max) rather than issuing a bogus full-mmap MADV_COLD.
func TestColdTrackingNoAccessNoAdvise(t *testing.T) {
	f, _ := makeFile(t, 4)
	defer f.Close()
	p, err := NewReader(f, page.Config{PageSize: testPageSize}, 4*testPageSize)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer p.Close()

	p.EnableColdTracking()
	if got := p.cold.min.Load(); got != ^uint64(0) {
		t.Errorf("cold.min = %d, want MaxUint64 (no access)", got)
	}
	if err := p.AdviseColdAccessed(); err != nil {
		t.Errorf("AdviseColdAccessed with no access: %v", err)
	}
}

// TestAdviseHugePagesAndPreloadTolerated confirms the open-time hints
// run without error on a real mapping (issued on supporting kernels,
// silently tolerated otherwise).
func TestAdviseHugePagesAndPreloadTolerated(t *testing.T) {
	f, _ := makeFile(t, 4)
	defer f.Close()
	p, err := NewReader(f, page.Config{PageSize: testPageSize}, 4*testPageSize)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer p.Close()

	if err := p.AdviseHugePages(); err != nil {
		t.Errorf("AdviseHugePages: %v", err)
	}
	if err := p.AdvisePreload(4); err != nil { // prefault [0,4)
		t.Errorf("AdvisePreload: %v", err)
	}
}

// TestColdTrackerRange pins the coldTracker state machine directly:
// disabled and enabled-but-untouched trackers report no range (the
// no-access case must never advise — the madvise shim's bounds guard
// also defends, but the flag is the contract), and recorded accesses
// fold into an exact [min, max].
func TestColdTrackerRange(t *testing.T) {
	var c coldTracker
	if _, _, ok := c.accessedRange(); ok {
		t.Fatal("accessedRange ok on a disabled tracker")
	}
	c.enable()
	if _, _, ok := c.accessedRange(); ok {
		t.Fatal("accessedRange ok with no access recorded")
	}
	c.record(7)
	c.record(3)
	c.record(9)
	minID, maxID, ok := c.accessedRange()
	if !ok || minID != 3 || maxID != 9 {
		t.Errorf("accessedRange = (%d, %d, %v), want (3, 9, true)", minID, maxID, ok)
	}
}
