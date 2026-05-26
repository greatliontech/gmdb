package btree

import (
	"errors"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// boundedReader mimics the pager's verifying Page (Inv-RV3): it returns
// ErrCorrupted for any id at or beyond limit (the file-resident extent)
// instead of reading an out-of-range page. Page content is irrelevant for
// the forged-run test below — the read aborts before any assembly.
type boundedReader struct{ limit uint64 }

func (b boundedReader) Page(id uint64) ([]byte, error) {
	if id >= b.limit {
		return nil, ErrCorrupted
	}
	return make([]byte, 4096), nil
}

// TestReadOverflowValueForgedTotalLenNoOOM (Inv-RV4 / chunk-11.2 round-3
// H-1): a forged overflow TotalLen implying a multi-terabyte run must NOT
// drive make([]byte, TotalLen). readOverflowValue computes the run length
// in uint64 and reads pages one at a time, so the run walks past the
// file-resident bound and aborts with ErrCorrupted long before the
// allocation. OverflowRunLength (uint32) truncates this TotalLen to a
// tiny run, which is exactly what made the pre-fix code allocate ~16 TB.
func TestReadOverflowValueForgedTotalLenNoOOM(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	entry := page.LeafEntry{
		Flags:        page.CellFlagOverflow,
		OverflowPage: 1,
		TotalLen:     17_557_826_330_571, // ~16 TB → run of ~4 billion pages
	}
	// Only 8 pages are file-resident; the forged run is billions of pages.
	_, err := readOverflowValue(boundedReader{limit: 8}, cfg, entry)
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("readOverflowValue on forged TotalLen = %v, want ErrCorrupted (no OOM)", err)
	}
}
