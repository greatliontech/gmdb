package btree

import (
	"errors"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// boundedReader mimics the pager's run accessor bounds (Inv-RV3): it
// returns ErrCorrupted for any head at or beyond limit (the
// file-resident extent) and otherwise serves a minimal single-page run
// (a valid TypeOverflow head with AdditionalPages = 0) — the physical
// truth a forged reference must be caught against.
type boundedReader struct{ limit uint64 }

func (b boundedReader) Page(id uint64) ([]byte, error) {
	if id >= b.limit {
		return nil, ErrCorrupted
	}
	return make([]byte, 4096), nil
}

func (b boundedReader) PageRun(headID uint64) ([]byte, error) {
	if headID >= b.limit {
		return nil, ErrCorrupted
	}
	run := make([]byte, 4096)
	page.WriteHeader(run, page.TypeOverflow, 0, 0)
	return run, nil
}

// TestReadOverflowValueForgedTotalLenNoOOM (Inv-RV4):
// a forged overflow TotalLen implying a multi-terabyte run must NOT
// drive make([]byte, TotalLen). readOverflowValue computes the
// reference-derived run length in uint64 and rejects it against the
// physical (file-bounded) run's AdditionalPages — ErrCorrupted with no
// TotalLen-sized allocation anywhere on the path. OverflowRunLength
// (uint32) truncates this TotalLen to a tiny run, which is exactly what
// made the pre-fix code allocate ~16 TB.
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
	// A forged head beyond the file-resident extent aborts at the
	// bounds gate regardless of TotalLen.
	entry.OverflowPage = 9
	if _, err := readOverflowValue(boundedReader{limit: 8}, cfg, entry); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("readOverflowValue on out-of-range head = %v, want ErrCorrupted", err)
	}
}

// multiPageRunReader serves a fixed 3-page run image regardless of
// head id — the harness for the reference-vs-physical cross-check.
type multiPageRunReader struct{ cfg page.Config }

func (m multiPageRunReader) Page(uint64) ([]byte, error) { return make([]byte, m.cfg.PageSize), nil }
func (m multiPageRunReader) PageRun(uint64) ([]byte, error) {
	run := make([]byte, 3*m.cfg.PageSize)
	page.WriteHeader(run, page.TypeOverflow, 0, 2)
	return run, nil
}

// TestReadOverflowValueRunLengthMismatch (checksums.md §Structural and
// Allocation Bounds): a reference whose extent length disagrees with
// the physical run's AdditionalPages — in EITHER direction — is
// rejected as ErrCorrupted even when the forged length would fit the
// physical capacity. Without the cross-check, a stale or corrupt
// reference silently reads the wrong byte count out of a mismatched
// run.
func TestReadOverflowValueRunLengthMismatch(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pr := multiPageRunReader{cfg: cfg}
	for _, totalLen := range []uint64{
		100,  // implies a 1-page run; physical run is 3 pages
		5000, // implies 2 pages
		uint64(page.OverflowFirstPageCapacity(cfg)) + 3*uint64(page.OverflowFollowerCapacity(cfg)), // implies 4
	} {
		entry := page.LeafEntry{Flags: page.CellFlagOverflow, OverflowPage: 1, TotalLen: totalLen}
		if _, err := readOverflowValue(pr, cfg, entry); !errors.Is(err, ErrCorrupted) {
			t.Errorf("TotalLen=%d against a 3-page run: err=%v, want ErrCorrupted", totalLen, err)
		}
	}
	// Agreement passes: a TotalLen whose derived run IS 3 pages.
	okLen := uint64(page.OverflowFirstPageCapacity(cfg)) + uint64(page.OverflowFollowerCapacity(cfg)) + 5
	entry := page.LeafEntry{Flags: page.CellFlagOverflow, OverflowPage: 1, TotalLen: okLen}
	if _, err := readOverflowValue(pr, cfg, entry); err != nil {
		t.Errorf("matching TotalLen=%d: err=%v, want nil", okLen, err)
	}
}
