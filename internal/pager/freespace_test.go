package pager

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// setupWriter creates a fresh writer pager with a 4 KB / 1 bitmap page
// configuration over a tmp file of pages*pageSize bytes. The bitmap is
// attached but contains no free pages by default — tests Set bits as
// needed.
func setupWriter(t *testing.T, pages int) (*Pager, *bitmap.Bitmap, *os.File) {
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
	p, err := NewWriter(f, cfg, int64(pages)*int64(testPageSize), pool, 16<<20)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	// 1 bitmap page covers 4096*8 = 32 768 pages. We'll declare totalPages
	// to match the file size.
	bm := bitmap.New(make([]byte, testPageSize), testPageSize, 1, uint64(pages))
	p.AttachBitmap(bm)
	// HWM = firstDataPage (no data pages yet). MaxSize = pages.
	// Reclamation bound = 0 by default (no prior commits).
	p.SetCommitState(bm.FirstDataPage(), uint64(pages), 0)
	return p, bm, f
}

func TestAllocPageFromBitmap(t *testing.T) {
	p, bm, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	// Mark pages first+5 and first+10 as free.
	bm.Set(first + 5)
	bm.Set(first + 10)

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if id != first+5 {
		t.Errorf("AllocPage = %d, want %d", id, first+5)
	}
	if bm.IsSet(first + 5) {
		t.Error("bit still set after AllocPage")
	}
	if _, ok := p.pendingAllocs[id]; !ok {
		t.Error("id not in pendingAllocs")
	}

	id2, err := p.AllocPage()
	if err != nil {
		t.Fatalf("second AllocPage: %v", err)
	}
	if id2 != first+10 {
		t.Errorf("second AllocPage = %d, want %d", id2, first+10)
	}
}

func TestAllocPageFromFileExtension(t *testing.T) {
	p, bm, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	hwm := p.HighWaterMark()
	if hwm != first {
		t.Fatalf("initial HWM = %d, want %d", hwm, first)
	}
	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if id != first {
		t.Errorf("AllocPage extended = %d, want %d", id, first)
	}
	if p.HighWaterMark() != first+1 {
		t.Errorf("HWM after extension = %d, want %d", p.HighWaterMark(), first+1)
	}
}

func TestAllocPageReturnsErrDBFull(t *testing.T) {
	p, bm, f := setupWriter(t, 4)
	defer p.Close()
	defer f.Close()

	// firstDataPage = 3, maxSizePages = 4 → one page available.
	_ = bm
	if _, err := p.AllocPage(); err != nil {
		t.Fatalf("first AllocPage: %v", err)
	}
	if _, err := p.AllocPage(); !errors.Is(err, ErrDBFull) {
		t.Errorf("second AllocPage: got %v, want ErrDBFull", err)
	}
}

func TestAllocPageLooseFirst(t *testing.T) {
	p, bm, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	// Manually mark a page as loose.
	p.loosePages[first+7] = struct{}{}
	// Also mark a bitmap page free, lower-id.
	bm.Set(first + 5)

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if id != first+7 {
		t.Errorf("AllocPage = %d (want loose %d before bitmap %d)", id, first+7, first+5)
	}
	if _, ok := p.loosePages[first+7]; ok {
		t.Error("loose page not removed from set")
	}
}

func TestFreePageSameTxBecomesLoose(t *testing.T) {
	p, bm, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	// Allocate via file extension.
	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	// CoW to put it in p.dirty.
	if _, err := p.CoW(first, id); err != nil {
		// First call uses Page(first) which reads mmap (file was
		// truncated to zero bytes; that's still readable).
		t.Fatalf("CoW: %v", err)
	}
	if err := p.FreePage(id); err != nil {
		t.Fatalf("FreePage: %v", err)
	}
	if _, ok := p.loosePages[id]; !ok {
		t.Error("FreePage did not add to loosePages")
	}
	if _, ok := p.pendingAllocs[id]; ok {
		t.Error("pendingAllocs not cleared on Free")
	}
}

func TestFreePagePriorTxRetires(t *testing.T) {
	p, bm, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()
	_ = bm

	// Page 7 is not in p.dirty (prior-tx). FreePage retires it.
	if err := p.FreePage(7); err != nil {
		t.Fatalf("FreePage: %v", err)
	}
	if len(p.retiredPages) != 1 || p.retiredPages[0] != 7 {
		t.Errorf("retiredPages = %v, want [7]", p.retiredPages)
	}
	if _, ok := p.loosePages[7]; ok {
		t.Error("prior-tx page added to loosePages")
	}
}

func TestTailRefundFreeBitmap(t *testing.T) {
	p, bm, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	// Set HWM to first+5; mark pages first+3, first+4 free in bitmap.
	p.SetCommitState(first+5, 16, 0)
	bm.Set(first + 3)
	bm.Set(first + 4)

	if err := p.TailRefund(); err != nil {
		t.Fatalf("TailRefund: %v", err)
	}
	if p.HighWaterMark() != first+3 {
		t.Errorf("HWM after refund = %d, want %d", p.HighWaterMark(), first+3)
	}
	if bm.IsSet(first + 3) {
		t.Error("bit still set after tail refund")
	}
	if bm.IsSet(first + 4) {
		t.Error("bit still set after tail refund")
	}
}

func TestTailRefundLoosePages(t *testing.T) {
	p, bm, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	p.SetCommitState(first+5, 16, 0)
	p.loosePages[first+4] = struct{}{}
	p.loosePages[first+3] = struct{}{}

	if err := p.TailRefund(); err != nil {
		t.Fatalf("TailRefund: %v", err)
	}
	if p.HighWaterMark() != first+3 {
		t.Errorf("HWM after loose-tail refund = %d, want %d", p.HighWaterMark(), first+3)
	}
	if _, ok := p.loosePages[first+4]; ok {
		t.Error("loose page first+4 not consumed")
	}
}

func TestTailRefundStopsAtNonFree(t *testing.T) {
	p, bm, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	p.SetCommitState(first+5, 16, 0)
	bm.Set(first + 4)
	// first+3 is not free → refund stops at HWM = first+4.

	if err := p.TailRefund(); err != nil {
		t.Fatalf("TailRefund: %v", err)
	}
	if p.HighWaterMark() != first+4 {
		t.Errorf("HWM = %d, want %d", p.HighWaterMark(), first+4)
	}
}

func TestReclaimRPL(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	// Write an RPL segment to page 10 on disk, with TxnID=5 and PageID
	// entries [20, 21, 22, 23]. (These ids are below maxSizePages.)
	cfg := page.Config{PageSize: testPageSize}
	rplBuf := make([]byte, testPageSize)
	pageIDs := []uint64{20, 21, 22, 23}
	page.EncodeRPLSegment(rplBuf, cfg, 5, 0, pageIDs)
	if _, err := f.WriteAt(rplBuf, 10*testPageSize); err != nil {
		t.Fatalf("write RPL seg: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Re-open the pager so the mmap picks up the on-disk write. (Linux
	// MAP_SHARED + pwrite same-fd is visible without remap, but this
	// test deliberately keeps the open-then-write order simple.)
	p.Close()
	pool := NewBufPool(testPageSize)
	p, err := NewWriter(f, cfg, 32*testPageSize, pool, 16<<20)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	bm = bitmap.New(make([]byte, testPageSize), testPageSize, 1, 32)
	p.AttachBitmap(bm)
	p.SetCommitState(bm.FirstDataPage(), 32, 10) // reclamationBound = 10 > seg.TxnID = 5
	p.SetRPLChain([]RPLSegmentRef{{PageID: 10, TxnID: 5, Count: 4}})

	// Trigger reclamation: bitmap is empty, so AllocPage will go to RPL.
	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage after reclaim: %v", err)
	}
	// Reclaimed pages: 20, 21, 22, 23, plus segment page 10 → 5 set bits.
	// AllocPage picks one of them; the rest stay free.
	free := []uint64{}
	for _, candidate := range []uint64{10, 20, 21, 22, 23} {
		if bm.IsSet(candidate) {
			free = append(free, candidate)
		}
	}
	if len(free) != 4 {
		t.Errorf("after reclaim+alloc, free = %v (want 4 of [10,20,21,22,23] still free)", free)
	}
	if id != 10 && id != 20 && id != 21 && id != 22 && id != 23 {
		t.Errorf("AllocPage = %d, want one of [10, 20, 21, 22, 23]", id)
	}
	if len(p.RPLChain()) != 0 {
		t.Errorf("RPL chain after full reclaim = %v, want empty", p.RPLChain())
	}
}

func TestReclaimRPLRespectsBound(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	cfg := page.Config{PageSize: testPageSize}
	rplBuf := make([]byte, testPageSize)
	page.EncodeRPLSegment(rplBuf, cfg, 100, 0, []uint64{20})
	if _, err := f.WriteAt(rplBuf, 10*testPageSize); err != nil {
		t.Fatalf("write RPL seg: %v", err)
	}
	f.Sync()
	p.Close()

	pool := NewBufPool(testPageSize)
	p, err := NewWriter(f, cfg, 32*testPageSize, pool, 16<<20)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	bm = bitmap.New(make([]byte, testPageSize), testPageSize, 1, 32)
	p.AttachBitmap(bm)
	// reclamationBound = 50 < seg.TxnID = 100. Segment NOT reclaimable.
	p.SetCommitState(bm.FirstDataPage(), 32, 50)
	p.SetRPLChain([]RPLSegmentRef{{PageID: 10, TxnID: 100, Count: 1}})

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	// RPL not reclaimable → file extension. First file extension id is
	// firstDataPage.
	if id != bm.FirstDataPage() {
		t.Errorf("AllocPage = %d, want extension from %d", id, bm.FirstDataPage())
	}
	if len(p.RPLChain()) != 1 {
		t.Errorf("RPL chain consumed despite bound: %v", p.RPLChain())
	}
}

// TestRPLChainOrientationMultiSegment pins the chain-orientation
// invariant defined at SetRPLChain (internal/pager/pager.go) and
// free-space.md §RPL in-memory segment list: tail = index 0 = oldest
// TxnID, head = last index = newest TxnID. A future refactor that
// reverses the ordering — by repurposing head/tail to mean the slice's
// other end — would silently produce wrong-result reclamation (e.g.
// draining the newest segments first, leaving older ones unreachable
// to readers that need them) and break OlderSegment encoding in
// appendRPL. Single-segment fixtures (TestReclaimRPL,
// TestReclaimRPLRespectsBound, the lagging-reader tests) cannot
// distinguish tail-first from head-first drain; this test uses three
// segments with a partial reclamation bound so the surviving segment's
// identity is the discriminator.
func TestRPLChainOrientationMultiSegment(t *testing.T) {
	p, _, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	cfg := page.Config{PageSize: testPageSize}

	// Encode three RPL segments on disk so reclaimRPL's
	// DecodeRPLSegment succeeds for each. PageIDs ascend with TxnIDs
	// purely as a convention-neutral test layout — what matters is
	// (TxnID, expected drain order) and which slice index a given
	// segment lives at after SetRPLChain.
	const (
		tailPageID  = 10 // ordered tail per convention: index 0, oldest
		midPageID   = 11 // middle: index 1
		headPageID  = 12 // ordered head per convention: last index, newest
		tailPayload = 20
		midPayload  = 21
		headPayload = 22
		tailTxnID   = 100
		midTxnID    = 200
		headTxnID   = 300
	)
	segments := []struct {
		pageID, payload uint64
		txnID           uint64
	}{
		{tailPageID, tailPayload, tailTxnID},
		{midPageID, midPayload, midTxnID},
		{headPageID, headPayload, headTxnID},
	}
	for _, seg := range segments {
		buf := make([]byte, testPageSize)
		page.EncodeRPLSegment(buf, cfg, seg.txnID, 0, []uint64{seg.payload})
		if _, err := f.WriteAt(buf, int64(seg.pageID)*int64(testPageSize)); err != nil {
			t.Fatalf("write seg page %d: %v", seg.pageID, err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// Re-open so the mmap picks up the on-disk segments. Re-seed the
	// chain tail-first per the SetRPLChain convention.
	p.Close()
	pool := NewBufPool(testPageSize)
	p, err := NewWriter(f, cfg, 32*testPageSize, pool, 16<<20)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	bm := bitmap.New(make([]byte, testPageSize), testPageSize, 1, 32)
	p.AttachBitmap(bm)
	p.SetRPLChain([]RPLSegmentRef{
		{PageID: tailPageID, TxnID: tailTxnID, Count: 1},
		{PageID: midPageID, TxnID: midTxnID, Count: 1},
		{PageID: headPageID, TxnID: headTxnID, Count: 1},
	})

	// Property 1: head = last index per the convention.
	// headPageID() must return the newest segment's PageID, which is
	// the last index of the seeded slice. A refactor that swapped to
	// rplSegments[0].PageID would return tailPageID and fail here.
	if got := p.headPageID(); got != headPageID {
		t.Errorf("headPageID() = %d, want %d (last index per chain convention)", got, headPageID)
	}

	// Property 2: reclaimRPL drains the tail first.
	// Set the bound between midTxnID and headTxnID so the tail
	// (TxnID=100) and middle (TxnID=200) drain; the head (TxnID=300)
	// survives. If the implementation drained head-first instead,
	// the surviving entry would be the tail (TxnID=100), and the
	// count would still be 4 (same number of segments below bound),
	// so the survivor's identity is what distinguishes orientations.
	p.SetCommitState(bm.FirstDataPage(), 32, 250)

	count := p.reclaimRPL()
	// Expected: 2 payload pages (tailPayload, midPayload) + 2 segment
	// pages (tailPageID, midPageID) = 4 bits set.
	if count != 4 {
		t.Errorf("reclaimRPL count = %d, want 4 (2 payloads + 2 segment pages)", count)
	}
	chain := p.RPLChain()
	if len(chain) != 1 {
		t.Fatalf("post-reclaim chain length = %d, want 1 (head survives)", len(chain))
	}
	// The surviving entry is the original head — index 0 of the
	// post-reclaim slice, but identity-tested via its PageID/TxnID so
	// a reversed-orientation refactor that left a different segment
	// at index 0 would fail loudly.
	if chain[0].PageID != headPageID {
		t.Errorf("surviving segment PageID = %d, want %d (head per convention)", chain[0].PageID, headPageID)
	}
	if chain[0].TxnID != headTxnID {
		t.Errorf("surviving segment TxnID = %d, want %d (head per convention)", chain[0].TxnID, headTxnID)
	}
	// Property 1 again on the reduced chain: headPageID() still
	// returns the (now sole) head; this is also the last index.
	if got := p.headPageID(); got != headPageID {
		t.Errorf("post-reclaim headPageID() = %d, want %d", got, headPageID)
	}
}

func TestResetFreespace(t *testing.T) {
	p, bm, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()
	_ = bm

	p.pendingAllocs[1] = struct{}{}
	p.pendingFrees[2] = struct{}{}
	p.loosePages[3] = struct{}{}
	p.retiredPages = append(p.retiredPages, 4)

	p.ResetFreespace()

	if len(p.pendingAllocs) != 0 || len(p.pendingFrees) != 0 || len(p.loosePages) != 0 || len(p.retiredPages) != 0 {
		t.Errorf("ResetFreespace left state: %v %v %v %v",
			p.pendingAllocs, p.pendingFrees, p.loosePages, p.retiredPages)
	}
}

func TestAllocPageUnconfiguredErrors(t *testing.T) {
	f, _ := makeFile(t, 2)
	defer f.Close()
	pool := NewBufPool(testPageSize)
	p, err := NewWriter(f, page.Config{PageSize: testPageSize}, 2*testPageSize, pool, 1<<20)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer p.Close()

	if _, err := p.AllocPage(); !errors.Is(err, ErrFreespaceUnconfigured) {
		t.Errorf("AllocPage without bitmap: got %v, want ErrFreespaceUnconfigured", err)
	}
	if err := p.FreePage(0); !errors.Is(err, ErrFreespaceUnconfigured) {
		t.Errorf("FreePage without bitmap: got %v, want ErrFreespaceUnconfigured", err)
	}
	if err := p.TailRefund(); !errors.Is(err, ErrFreespaceUnconfigured) {
		t.Errorf("TailRefund without bitmap: got %v, want ErrFreespaceUnconfigured", err)
	}
}

// TestAllocContiguousFragmentationStats exercises the contiguous-allocation
// failure-rate instrumentation that drives incremental compaction
// (background-maintenance.md §Incremental Compaction Trigger). Each subtest
// uses a fresh pager so counters don't bleed across cases.
func TestAllocContiguousFragmentationStats(t *testing.T) {
	// Non-adjacent free pages with sufficient total free ⇒ the first scan
	// misses and a fragmentation failure is counted.
	t.Run("fragmented", func(t *testing.T) {
		p, bm, f := setupWriter(t, 32)
		defer p.Close()
		defer f.Close()
		first := bm.FirstDataPage()
		bm.Set(first + 5)
		bm.Set(first + 10)
		// First scan misses (no 2-run); the request is then satisfied via
		// file extension (HWM=first, room to 32). The frag failure is still
		// counted — the "counted regardless of later success" path.
		if _, err := p.AllocContiguous(2); err != nil {
			t.Fatalf("AllocContiguous(2): %v", err)
		}
		if a, frag := p.ConsumeContiguousAllocStats(); a != 1 || frag != 1 {
			t.Errorf("attempts=%d fragFails=%d, want 1,1", a, frag)
		}
		// Consume reset the counters.
		if a, frag := p.ConsumeContiguousAllocStats(); a != 0 || frag != 0 {
			t.Errorf("consume did not reset: attempts=%d fragFails=%d", a, frag)
		}
	})
	// An adjacent free run satisfies the first scan ⇒ an attempt, no
	// fragmentation failure.
	t.Run("contiguous", func(t *testing.T) {
		p, bm, f := setupWriter(t, 32)
		defer p.Close()
		defer f.Close()
		first := bm.FirstDataPage()
		bm.Set(first + 6)
		bm.Set(first + 7)
		if _, err := p.AllocContiguous(2); err != nil {
			t.Fatalf("AllocContiguous(2): %v", err)
		}
		if a, frag := p.ConsumeContiguousAllocStats(); a != 1 || frag != 0 {
			t.Errorf("attempts=%d fragFails=%d, want 1,0", a, frag)
		}
	})
	// Genuine fullness (total free < n) is NOT a fragmentation failure —
	// the "despite sufficient total free pages" gate.
	t.Run("insufficient_free", func(t *testing.T) {
		p, bm, f := setupWriter(t, 32)
		defer p.Close()
		defer f.Close()
		first := bm.FirstDataPage()
		bm.Set(first + 8) // exactly one free page; request 2 ⇒ NumFree < n
		if _, err := p.AllocContiguous(2); err != nil {
			t.Fatalf("AllocContiguous(2): %v", err)
		}
		if a, frag := p.ConsumeContiguousAllocStats(); a != 1 || frag != 0 {
			t.Errorf("attempts=%d fragFails=%d, want 1,0 (fullness, not fragmentation)", a, frag)
		}
	})
	// n==1 routes through AllocPage and is not a contiguous attempt.
	t.Run("n_eq_1", func(t *testing.T) {
		p, bm, f := setupWriter(t, 32)
		defer p.Close()
		defer f.Close()
		first := bm.FirstDataPage()
		bm.Set(first + 9)
		if _, err := p.AllocContiguous(1); err != nil {
			t.Fatalf("AllocContiguous(1): %v", err)
		}
		if a, frag := p.ConsumeContiguousAllocStats(); a != 0 || frag != 0 {
			t.Errorf("attempts=%d fragFails=%d, want 0,0", a, frag)
		}
	})
}
