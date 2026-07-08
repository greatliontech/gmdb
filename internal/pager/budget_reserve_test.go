package pager

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// setupWriterMaxBytes is setupWriter with a caller-chosen slab budget,
// for tests that exercise the budget admission math directly.
func setupWriterMaxBytes(t *testing.T, pages, maxBytes int) (*Pager, *bitmap.Bitmap, *os.File) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.gmdb")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := f.Truncate(int64(pages) * int64(testPageSize)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	pool := NewBufPool(testPageSize)
	cfg := page.Config{PageSize: testPageSize}
	p, err := NewWriter(f, cfg, int64(pages)*int64(testPageSize), pool, maxBytes)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	bm := bitmap.New(make([]byte, testPageSize), testPageSize, 1, uint64(pages))
	p.AttachBitmap(bm)
	p.SetCommitState(bm.FirstDataPage(), uint64(pages), 0)
	return p, bm, f
}

// TestRetireBudgetGuard pins the FreePage retire-branch admission
// check: retiring prior-tx pages grows the commit-time RPL segment
// projection, and the retire that would make the transaction unable
// to afford its own commit fails ErrTxTooLarge instead of deferring
// the failure to Commit. With maxBytes = 2 pages and zero dirty
// bytes, the reserve may grow to exactly 2 segment pages; the retire
// opening a third segment must be rejected.
func TestRetireBudgetGuard(t *testing.T) {
	const maxPages = 2
	p, _, f := setupWriterMaxBytes(t, 32, maxPages*int(testPageSize))
	defer p.Close()
	defer f.Close()

	cfg := page.Config{PageSize: testPageSize}
	capPerSeg := RPLEntriesPerSegment(cfg)
	if capPerSeg <= 0 {
		t.Fatalf("RPLEntriesPerSegment = %d", capPerSeg)
	}

	// Prior-tx page IDs: anything not in dirty/pendingAllocs. IDs need
	// not be backed by file content — FreePage only does bookkeeping.
	next := uint64(1 << 20)
	retire := func() error {
		next++
		return p.FreePage(next)
	}

	// Two segments' worth of retires fit: reserve reaches exactly
	// maxBytes with dirtyBytes = 0.
	for i := 0; i < 2*capPerSeg; i++ {
		if err := retire(); err != nil {
			t.Fatalf("retire %d: %v (reserve %d)", i, err, p.CommitReserveBytes())
		}
	}
	if got, want := p.CommitReserveBytes(), maxPages*int(testPageSize); got != want {
		t.Fatalf("reserve after 2 segments = %d, want %d", got, want)
	}

	// The retire opening the third segment must fail, leaving the
	// retired set unchanged.
	before := len(p.RetiredPages())
	if err := retire(); !errors.Is(err, ErrTxTooLarge) {
		t.Fatalf("third-segment retire: err = %v, want ErrTxTooLarge", err)
	}
	if after := len(p.RetiredPages()); after != before {
		t.Fatalf("failed retire mutated retiredPages: %d -> %d", before, after)
	}

	// Ops-phase slab admission is fully consumed by the reserve:
	// any CoW/AllocSlab must fail even though dirtyBytes is zero.
	if _, err := p.AllocSlab(3); !errors.Is(err, ErrTxTooLarge) {
		t.Fatalf("AllocSlab under full reserve: err = %v, want ErrTxTooLarge", err)
	}

	// Commit-phase admission draws from the reserve: the same
	// allocation succeeds with the commit flag set, up to the raw cap.
	p.SetCommitPhase(true)
	defer p.SetCommitPhase(false)
	if _, err := p.AllocSlab(3); err != nil {
		t.Fatalf("AllocSlab in commit phase: %v", err)
	}
	if _, err := p.AllocSlab(4); err != nil {
		t.Fatalf("second commit-phase AllocSlab: %v", err)
	}
	// Raw cap remains the backstop.
	if _, err := p.AllocSlab(5); !errors.Is(err, ErrTxTooLarge) {
		t.Fatalf("commit-phase AllocSlab past raw cap: err = %v, want ErrTxTooLarge", err)
	}
}
