package pager

import (
	"errors"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// Chunk-5.2 tests pin three invariants over AllocContiguous / AllocSlabRun /
// FreeRun on a real *pager.Pager (fakeWriter's chunk-4.7 contract translated
// to the real implementation):
//
//   Inv-1 (atomicity): AllocContiguous(n) either reserves all n pages
//          (bitmap bits cleared + pendingAllocs entries) or returns an
//          error with no state change.
//   Inv-2 (alloc+free round-trip): AllocContiguous(n) immediately followed
//          by FreeRun(firstID, n) (without intervening AllocSlabRun) restores
//          the bitmap bits and drops pendingAllocs without retiring same-tx
//          pages to retiredPages.
//   Inv-3 (PageWriter parity): the chunk-4.7 overflow-chain rollback shape —
//          AllocContiguous then FreeRun on AllocSlabRun budget error —
//          succeeds on *pager.Pager identically to fakeWriter.

func TestAllocContiguousBitmapHit(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	// Mark a 3-page contiguous run free.
	bm.Set(first + 5)
	bm.Set(first + 6)
	bm.Set(first + 7)

	got, err := p.AllocContiguous(3)
	if err != nil {
		t.Fatalf("AllocContiguous(3): %v", err)
	}
	if got != first+5 {
		t.Errorf("AllocContiguous = %d, want %d", got, first+5)
	}
	for i := uint64(0); i < 3; i++ {
		id := first + 5 + i
		if bm.IsSet(id) {
			t.Errorf("page %d still bitmap-set after alloc", id)
		}
		if _, ok := p.pendingAllocs[id]; !ok {
			t.Errorf("page %d not in pendingAllocs", id)
		}
	}
	if len(p.retiredPages) != 0 {
		t.Errorf("retiredPages grew on alloc: %v", p.retiredPages)
	}
}

func TestAllocContiguousHWMExtension(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	hwm0 := p.HighWaterMark()
	if hwm0 != first {
		t.Fatalf("initial HWM = %d, want %d", hwm0, first)
	}
	// Bitmap is empty (no pages marked free) → HWM extension is the only path.
	got, err := p.AllocContiguous(4)
	if err != nil {
		t.Fatalf("AllocContiguous(4): %v", err)
	}
	if got != first {
		t.Errorf("AllocContiguous (HWM) = %d, want %d", got, first)
	}
	if p.HighWaterMark() != first+4 {
		t.Errorf("HWM after extension = %d, want %d", p.HighWaterMark(), first+4)
	}
	for i := uint64(0); i < 4; i++ {
		if _, ok := p.pendingAllocs[first+i]; !ok {
			t.Errorf("page %d not in pendingAllocs after HWM extension", first+i)
		}
	}
}

func TestAllocContiguousErrDBFull(t *testing.T) {
	// File has only 4 pages: 0 meta, 1 meta, 2 bitmap, 3 firstDataPage.
	// HWM starts at firstDataPage=3, maxSizePages=4. A run of 2 would
	// require HWM advance to 5 > maxSizePages.
	p, _, f := setupWriter(t, 4)
	defer p.Close()
	defer f.Close()

	_, err := p.AllocContiguous(2)
	if !errors.Is(err, ErrDBFull) {
		t.Errorf("AllocContiguous(2) at HWM=maxSize-1: got %v, want ErrDBFull", err)
	}
	if p.HighWaterMark() != 3 {
		t.Errorf("HWM bumped on ErrDBFull: %d, want 3", p.HighWaterMark())
	}
	if len(p.pendingAllocs) != 0 {
		t.Errorf("pendingAllocs populated on ErrDBFull: %v", p.pendingAllocs)
	}
}

// TestAllocContiguousFreeRunRoundTrip promotes Inv-2: alloc + immediate
// FreeRun (no AllocSlabRun) restores the bitmap + drops pendingAllocs and
// does NOT retire same-tx pages. Demonstrated-fault anchor for the
// chunk-5.2 FreePage extension that handles the
// allocated-but-never-written case.
func TestAllocContiguousFreeRunRoundTrip(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	bm.Set(first + 0)
	bm.Set(first + 1)
	bm.Set(first + 2)

	got, err := p.AllocContiguous(3)
	if err != nil {
		t.Fatalf("AllocContiguous: %v", err)
	}
	if err := p.FreeRun(got, 3); err != nil {
		t.Fatalf("FreeRun: %v", err)
	}

	for i := uint64(0); i < 3; i++ {
		id := first + i
		if !bm.IsSet(id) {
			t.Errorf("page %d bitmap bit not restored on FreeRun", id)
		}
		if _, ok := p.pendingAllocs[id]; ok {
			t.Errorf("page %d still in pendingAllocs after FreeRun", id)
		}
	}
	if len(p.retiredPages) != 0 {
		t.Errorf("retiredPages grew after alloc+FreeRun round-trip: %v — "+
			"same-tx pages must not enter the RPL", p.retiredPages)
	}
	if len(p.loosePages) != 0 {
		t.Errorf("loosePages populated after alloc+FreeRun round-trip "+
			"(no AllocSlab was called, no slab to retain): %v", p.loosePages)
	}
}

// TestAllocContiguousHWMExtensionThenFreeRun pins the HWM-extension
// branch of Inv-2. AllocContiguous via HWM extension does not call
// bitmap.Clear (the bits past HWM are already clear); FreeRun on those
// same-tx allocated-but-never-written pages routes through the chunk-5.2
// FreePage extension and bitmap.Set'es each bit. Post-state: numFree
// grew by n, HWM unchanged, pendingAllocs empty. The pages are now
// allocator-reachable as bitmap-free + HWM-covered (TailRefund per
// free-space.md §Tail Page Refund handles file shrinkage).
func TestAllocContiguousHWMExtensionThenFreeRun(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	hwm0 := p.HighWaterMark()
	// Bitmap empty → AllocContiguous must use HWM extension.
	got, err := p.AllocContiguous(3)
	if err != nil {
		t.Fatalf("AllocContiguous: %v", err)
	}
	if got != first {
		t.Fatalf("AllocContiguous (HWM) = %d, want %d", got, first)
	}
	if p.HighWaterMark() != hwm0+3 {
		t.Fatalf("HWM after extension = %d, want %d", p.HighWaterMark(), hwm0+3)
	}

	if err := p.FreeRun(got, 3); err != nil {
		t.Fatalf("FreeRun: %v", err)
	}
	if p.HighWaterMark() != hwm0+3 {
		t.Errorf("FreeRun rolled back HWM (it should not): %d, want %d",
			p.HighWaterMark(), hwm0+3)
	}
	for i := uint64(0); i < 3; i++ {
		id := got + i
		if !bm.IsSet(id) {
			t.Errorf("page %d bitmap bit not set after FreeRun on HWM-extended run", id)
		}
		if _, ok := p.pendingAllocs[id]; ok {
			t.Errorf("page %d still in pendingAllocs after FreeRun", id)
		}
	}
	if len(p.retiredPages) != 0 {
		t.Errorf("retiredPages grew after HWM-extension alloc+FreeRun: %v",
			p.retiredPages)
	}
}

// TestAllocContiguousAfterRPLReclamation pins free-space.md §Page
// Allocation Priority step 3 on the n>1 path: when the bitmap has no
// contiguous run, reclaimRPL is invoked and the retry hits.
func TestAllocContiguousAfterRPLReclamation(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	cfg := page.Config{PageSize: testPageSize}
	// Write a synthetic RPL segment to a fixed page id that, when
	// reclaimed, frees 3 contiguous pages in the bitmap.
	first := bm.FirstDataPage()
	rplPage := first + 20
	rplBuf := make([]byte, testPageSize)
	page.EncodeRPLSegment(rplBuf, cfg, 100, 0, []uint64{first + 5, first + 6, first + 7})
	if _, err := f.WriteAt(rplBuf, int64(rplPage)*testPageSize); err != nil {
		t.Fatalf("write RPL seg: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// Re-open the pager so the mmap sees the new RPL segment bytes.
	p.Close()
	pool := NewBufPool(testPageSize)
	p, err := NewWriter(f, cfg, 32*testPageSize, pool, 16<<20)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	bm = bitmap.New(make([]byte, testPageSize), testPageSize, 1, 32)
	p.AttachBitmap(bm)
	// reclamationBound > seg.TxnID so the segment is reclaimable.
	p.SetCommitState(bm.FirstDataPage(), 32, 200)
	p.SetRPLChain([]RPLSegmentRef{{PageID: rplPage, TxnID: 100, Count: 3}})

	got, err := p.AllocContiguous(3)
	if err != nil {
		t.Fatalf("AllocContiguous: %v", err)
	}
	if got != first+5 {
		t.Errorf("AllocContiguous after reclaim = %d, want %d (the 3-page run "+
			"the RPL segment freed)", got, first+5)
	}
	if len(p.RPLChain()) != 0 {
		t.Errorf("RPL segment not consumed: %v", p.RPLChain())
	}
}

func TestAllocContiguousHWMRollbackOnTruncateFailure(t *testing.T) {
	// HWM extension that crosses maxSizePages is caught BEFORE the
	// truncate call by the explicit check; this test pins the
	// roll-back path for the rare case where ensureFileCovers itself
	// fails (e.g. ENOSPC). Force the path by:
	//   (a) shrinking p.fileSize so ensureFileCovers must call
	//       Truncate (otherwise need <= fileSize short-circuits);
	//   (b) closing the underlying file so Truncate's syscall errors.
	p, _, f := setupWriter(t, 32)
	defer p.Close()

	first := p.HighWaterMark()
	p.fileSize = 0 // force ensureFileCovers to invoke Truncate
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
	_, err := p.AllocContiguous(4)
	if err == nil {
		t.Fatal("AllocContiguous after close: want error, got nil")
	}
	if p.HighWaterMark() != first {
		t.Errorf("HWM not rolled back: %d, want %d", p.HighWaterMark(), first)
	}
	if len(p.pendingAllocs) != 0 {
		t.Errorf("pendingAllocs not rolled back: %v", p.pendingAllocs)
	}
}

func TestAllocContiguousZeroN(t *testing.T) {
	p, _, f := setupWriter(t, 4)
	defer p.Close()
	defer f.Close()
	if _, err := p.AllocContiguous(0); err == nil {
		t.Error("AllocContiguous(0): want error, got nil")
	}
}

func TestAllocContiguousSingleDelegatesToAllocPage(t *testing.T) {
	// n=1 delegates to AllocPage so loose-page priority + LIFO hint
	// behaviour is preserved. The contract is observable: a loose
	// page must be reused before falling through to the bitmap.
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	bm.Set(first + 10) // bitmap has a free page here

	// Synthesize a loose page (would normally come from a prior alloc+CoW+
	// FreePage cycle in the same tx).
	p.loosePages[first+3] = struct{}{}

	id, err := p.AllocContiguous(1)
	if err != nil {
		t.Fatalf("AllocContiguous(1): %v", err)
	}
	if id != first+3 {
		t.Errorf("AllocContiguous(1) = %d, want loose-page %d", id, first+3)
	}
}

func TestAllocSlabRunBudgetExceeded(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	// Shrink the slab budget to a single page so any n>1 AllocSlabRun
	// trips the budget check.
	p.maxBytes = testPageSize

	first := bm.FirstDataPage()
	bm.Set(first + 0)
	bm.Set(first + 1)
	bm.Set(first + 2)

	got, err := p.AllocContiguous(3)
	if err != nil {
		t.Fatalf("AllocContiguous: %v", err)
	}
	_, err = p.AllocSlabRun(got, 3)
	if !errors.Is(err, ErrTxTooLarge) {
		t.Errorf("AllocSlabRun(budget=1page, n=3): got %v, want ErrTxTooLarge", err)
	}
	// No slab buffers should have been installed.
	if p.DirtyBytes() != 0 {
		t.Errorf("dirtyBytes after failed AllocSlabRun = %d, want 0", p.DirtyBytes())
	}
	// Caller's responsibility is FreeRun to rollback the prior
	// AllocContiguous.
	if err := p.FreeRun(got, 3); err != nil {
		t.Fatalf("FreeRun rollback: %v", err)
	}
}

func TestAllocSlabRunIdempotent(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	bm.Set(first + 0)
	bm.Set(first + 1)
	bm.Set(first + 2)

	got, err := p.AllocContiguous(3)
	if err != nil {
		t.Fatalf("AllocContiguous: %v", err)
	}
	pages1, err := p.AllocSlabRun(got, 3)
	if err != nil {
		t.Fatalf("AllocSlabRun #1: %v", err)
	}
	dirtyAfter1 := p.DirtyBytes()

	pages2, err := p.AllocSlabRun(got, 3)
	if err != nil {
		t.Fatalf("AllocSlabRun #2: %v", err)
	}
	if p.DirtyBytes() != dirtyAfter1 {
		t.Errorf("idempotent AllocSlabRun re-billed budget: %d → %d",
			dirtyAfter1, p.DirtyBytes())
	}
	// Identical backing buffers.
	for i := range pages1 {
		if &pages1[i][0] != &pages2[i][0] {
			t.Errorf("idempotent AllocSlabRun returned new buffer at i=%d", i)
		}
	}
}

func TestAllocSlabRunZeroN(t *testing.T) {
	p, _, f := setupWriter(t, 4)
	defer p.Close()
	defer f.Close()
	if _, err := p.AllocSlabRun(3, 0); err == nil {
		t.Error("AllocSlabRun(_, 0): want error, got nil")
	}
}

func TestFreeRunSameTxAllocSlabBecomesLoose(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	bm.Set(first + 0)
	bm.Set(first + 1)

	got, err := p.AllocContiguous(2)
	if err != nil {
		t.Fatalf("AllocContiguous: %v", err)
	}
	if _, err := p.AllocSlabRun(got, 2); err != nil {
		t.Fatalf("AllocSlabRun: %v", err)
	}
	if err := p.FreeRun(got, 2); err != nil {
		t.Fatalf("FreeRun: %v", err)
	}
	// AllocSlabRun installed the buffers in p.dirty → FreeRun routes
	// each through the same-tx loose path.
	for i := uint64(0); i < 2; i++ {
		id := got + i
		if _, ok := p.loosePages[id]; !ok {
			t.Errorf("page %d not in loosePages after FreeRun (alloc+slab path)", id)
		}
		if _, ok := p.pendingAllocs[id]; ok {
			t.Errorf("page %d still in pendingAllocs after FreeRun", id)
		}
	}
	if len(p.retiredPages) != 0 {
		t.Errorf("retiredPages grew: %v — same-tx pages must not enter RPL",
			p.retiredPages)
	}
}

func TestFreeRunPriorTxRetiresToRPL(t *testing.T) {
	// A FreeRun over pages neither in p.dirty nor in pendingAllocs
	// (e.g. mmap-backed prior-tx pages opened via a fresh writer) must
	// route to retiredPages for RPL retirement.
	p, _, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	// Synthesize "prior-tx allocated pages": IDs that the bitmap shows
	// as in-use (bit clear, the default) and not in p.dirty / pendingAllocs.
	priorIDs := []uint64{10, 11, 12}
	for _, id := range priorIDs {
		if _, ok := p.dirty[id]; ok {
			t.Fatalf("test bug: page %d is in p.dirty", id)
		}
		if _, ok := p.pendingAllocs[id]; ok {
			t.Fatalf("test bug: page %d is in pendingAllocs", id)
		}
	}
	if err := p.FreeRun(priorIDs[0], 3); err != nil {
		t.Fatalf("FreeRun prior-tx: %v", err)
	}
	if got, want := len(p.retiredPages), 3; got != want {
		t.Fatalf("retiredPages length = %d, want %d", got, want)
	}
	for i, id := range priorIDs {
		if p.retiredPages[i] != id {
			t.Errorf("retiredPages[%d] = %d, want %d", i, p.retiredPages[i], id)
		}
	}
}

func TestFreeRunZeroN(t *testing.T) {
	p, _, f := setupWriter(t, 4)
	defer p.Close()
	defer f.Close()
	if err := p.FreeRun(3, 0); err == nil {
		t.Error("FreeRun(_, 0): want error, got nil")
	}
}

func TestFreeRunReadOnlyErrors(t *testing.T) {
	f, _ := makeFile(t, 4)
	defer f.Close()
	p, err := NewReader(f, pageConfig(), 4*testPageSize)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer p.Close()
	if err := p.FreeRun(2, 2); !errors.Is(err, ErrReadOnly) {
		t.Errorf("FreeRun on read-only: got %v, want ErrReadOnly", err)
	}
	if _, err := p.AllocContiguous(2); !errors.Is(err, ErrReadOnly) {
		t.Errorf("AllocContiguous on read-only: got %v, want ErrReadOnly", err)
	}
	if _, err := p.AllocSlabRun(2, 2); !errors.Is(err, ErrReadOnly) {
		t.Errorf("AllocSlabRun on read-only: got %v, want ErrReadOnly", err)
	}
}

// TestFreePageAllocatedButNeverWrittenRestoresBitmap pins the
// chunk-5.2 FreePage extension at the single-page level. The chunk-4.7
// overflow rollback path can theoretically reach this state for a
// single-page run too if a caller AllocPage's then FreePage's without
// CoW / AllocSlab — observable here.
func TestFreePageAllocatedButNeverWrittenRestoresBitmap(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	bm.Set(first + 7)

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if id != first+7 {
		t.Fatalf("AllocPage = %d, want %d", id, first+7)
	}
	if _, ok := p.pendingAllocs[id]; !ok {
		t.Fatal("alloc did not record pendingAllocs")
	}

	if err := p.FreePage(id); err != nil {
		t.Fatalf("FreePage: %v", err)
	}
	if !bm.IsSet(id) {
		t.Error("FreePage did not restore bitmap bit on " +
			"allocated-but-never-written page")
	}
	if _, ok := p.pendingAllocs[id]; ok {
		t.Error("FreePage did not drop pendingAllocs entry")
	}
	if _, ok := p.loosePages[id]; ok {
		t.Error("FreePage routed never-written page to loosePages — " +
			"no slab to retain, should be bitmap restore")
	}
	if len(p.retiredPages) != 0 {
		t.Errorf("FreePage retired never-written same-tx page to RPL: %v",
			p.retiredPages)
	}
}

func pageConfig() page.Config {
	return page.Config{PageSize: testPageSize}
}
