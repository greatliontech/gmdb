package pager

import (
	"errors"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// Chunk-5.5 LaggingReader callback tests promote Inv-F:
// the callback fires at most once per AllocPage call when bitmap is
// exhausted AND the RPL is non-empty AND reclamation is blocked by the
// bound. Wait → one refresh+retry-bitmap; Abort → ErrDBFull.

// setupBlockedReclamationWriter prepares a writer pager with:
//   - A pre-written RPL segment carrying one entry (page id 12 with
//     TxnID 100).
//   - reclamationBound = 50, so the segment's TxnID 100 is NOT
//     reclaimable (the reader pinning TxnID 50 blocks advance).
//   - Bitmap exhausted (no pages set free).
//   - HWM at maxSizePages, so file extension also fails.
//
// AllocPage hits the callback path.
func setupBlockedReclamationWriter(t *testing.T) (*Pager, *bitmap.Bitmap) {
	t.Helper()
	f, _ := makeFile(t, 32)
	t.Cleanup(func() { f.Close() })

	cfg := page.Config{PageSize: testPageSize}
	rplBuf := make([]byte, testPageSize)
	page.EncodeRPLSegment(rplBuf, cfg, 100, 0, []uint64{12})
	if _, err := f.WriteAt(rplBuf, 10*testPageSize); err != nil {
		t.Fatalf("write RPL seg: %v", err)
	}
	f.Sync()

	pool := NewBufPool(testPageSize)
	p, err := NewWriter(f, cfg, 32*testPageSize, pool, 16<<20)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	bm := bitmap.New(make([]byte, testPageSize), testPageSize, 1, 32)
	p.AttachBitmap(bm)
	// HWM at maxSizePages so file extension is also blocked.
	p.SetCommitState(32, 32, 50)
	p.SetRPLChain([]RPLSegmentRef{{PageID: 10, TxnID: 100, Count: 1}})
	p.SetCurrentTxnID(101) // for Lag computation
	return p, bm
}

func TestAllocPageLaggingReaderAbortReturnsErrDBFull(t *testing.T) {
	p, _ := setupBlockedReclamationWriter(t)
	calls := 0
	p.SetLaggingReaderCallback(func(info LaggingReaderInfo) LaggingReaderAction {
		calls++
		if info.TxnID != 50 {
			t.Errorf("info.TxnID = %d, want 50 (the reclamationBound)", info.TxnID)
		}
		if info.Lag != 51 {
			t.Errorf("info.Lag = %d, want 51 (currentTxnID - bound)", info.Lag)
		}
		return LaggingReaderAbort
	})

	_, err := p.AllocPage()
	if !errors.Is(err, ErrDBFull) {
		t.Errorf("AllocPage with Abort: got %v, want ErrDBFull", err)
	}
	if calls != 1 {
		t.Errorf("callback invoked %d times, want exactly 1", calls)
	}
}

func TestAllocPageLaggingReaderWaitNoRefreshFallsThroughToErrDBFull(t *testing.T) {
	p, _ := setupBlockedReclamationWriter(t)
	calls := 0
	p.SetLaggingReaderCallback(func(info LaggingReaderInfo) LaggingReaderAction {
		calls++
		return LaggingReaderWait
	})
	// No refresh closure — Wait falls through to file extension,
	// which is blocked (HWM at maxSizePages) → ErrDBFull.
	p.SetReclamationBoundRefresh(nil)

	_, err := p.AllocPage()
	if !errors.Is(err, ErrDBFull) {
		t.Errorf("AllocPage with Wait + no-refresh: got %v, want ErrDBFull", err)
	}
	if calls != 1 {
		t.Errorf("callback invoked %d times, want exactly 1 (at-most-once-per-AllocPage)", calls)
	}
}

func TestAllocPageLaggingReaderWaitRefreshSucceeds(t *testing.T) {
	p, _ := setupBlockedReclamationWriter(t)
	calls := 0
	p.SetLaggingReaderCallback(func(info LaggingReaderInfo) LaggingReaderAction {
		calls++
		return LaggingReaderWait
	})
	// Refresh closure: the "reader" has now advanced past TxnID 100,
	// so the bound moves up and the RPL becomes reclaimable.
	p.SetReclamationBoundRefresh(func() uint64 { return 200 })

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage with Wait + advancing bound: %v", err)
	}
	// Reclamation frees both the RPL segment page (10) and the entry
	// inside it (12); FindFirst returns whichever bit is lowest. Both
	// are valid outcomes — the test pins that an alloc succeeded
	// after the Wait→refresh→retry path, not the specific id.
	if id != 10 && id != 12 {
		t.Errorf("AllocPage = %d, want 10 or 12 (reclaimed pages)", id)
	}
	if calls != 1 {
		t.Errorf("callback invoked %d times, want 1", calls)
	}
}

func TestAllocPageLaggingReaderNilCallbackFallsThroughToFileExtension(t *testing.T) {
	// Standard setup but with file-extension headroom: bitmap empty,
	// RPL non-empty + bound-blocked, but no callback installed.
	// AllocPage should fall through to file extension.
	f, _ := makeFile(t, 32)
	defer f.Close()
	cfg := page.Config{PageSize: testPageSize}
	rplBuf := make([]byte, testPageSize)
	page.EncodeRPLSegment(rplBuf, cfg, 100, 0, []uint64{12})
	if _, err := f.WriteAt(rplBuf, 10*testPageSize); err != nil {
		t.Fatal(err)
	}
	pool := NewBufPool(testPageSize)
	p, _ := NewWriter(f, cfg, 32*testPageSize, pool, 16<<20)
	defer p.Close()
	bm := bitmap.New(make([]byte, testPageSize), testPageSize, 1, 32)
	p.AttachBitmap(bm)
	p.SetCommitState(bm.FirstDataPage(), 32, 50)
	p.SetRPLChain([]RPLSegmentRef{{PageID: 10, TxnID: 100, Count: 1}})

	// No callback installed.
	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage with no callback: %v", err)
	}
	if id != bm.FirstDataPage() {
		t.Errorf("AllocPage = %d, want %d (file extension first page)",
			id, bm.FirstDataPage())
	}
}
