package pager

import (
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// TestColdTrackingRecordsAccessRange verifies the Options.ReclaimOnClose
// accessed-page tracking: once enabled, Page / pageRaw fold every
// touched id into [accessMin, accessMax], and AdviseColdAccessed issues
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
	if p.trackCold {
		t.Fatal("trackCold set before EnableColdTracking")
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

	if got := p.accessMin.Load(); got != 2 {
		t.Errorf("accessMin = %d, want 2", got)
	}
	if got := p.accessMax.Load(); got != 5 {
		t.Errorf("accessMax = %d, want 5", got)
	}
	if err := p.AdviseColdAccessed(); err != nil {
		t.Errorf("AdviseColdAccessed over [2,5]: %v", err)
	}
}

// TestColdTrackingNoAccessNoAdvise confirms that with tracking enabled
// but no page read, AdviseColdAccessed is a no-op (accessMin stays
// MaxUint64 > accessMax) rather than issuing a bogus full-mmap MADV_COLD.
func TestColdTrackingNoAccessNoAdvise(t *testing.T) {
	f, _ := makeFile(t, 4)
	defer f.Close()
	p, err := NewReader(f, page.Config{PageSize: testPageSize}, 4*testPageSize)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer p.Close()

	p.EnableColdTracking()
	if got := p.accessMin.Load(); got != ^uint64(0) {
		t.Errorf("accessMin = %d, want MaxUint64 (no access)", got)
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
