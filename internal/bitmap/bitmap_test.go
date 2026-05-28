package bitmap

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func newBitmap(t *testing.T, totalPages uint64) *Bitmap {
	t.Helper()
	pageSize := uint32(4096)
	bitsPerPage := uint64(pageSize) * 8
	bitmapPages := uint32((totalPages + bitsPerPage - 1) / bitsPerPage)
	if bitmapPages == 0 {
		bitmapPages = 1
	}
	detail := make([]byte, uint64(bitmapPages)*uint64(pageSize))
	return New(detail, pageSize, bitmapPages, totalPages)
}

func TestNewInitiallyEmpty(t *testing.T) {
	b := newBitmap(t, 4096)
	if got := b.NumFree(); got != 0 {
		t.Errorf("NumFree = %d, want 0", got)
	}
	first := b.FirstDataPage()
	if first != 3 { // 2 meta + 1 bitmap page
		t.Errorf("FirstDataPage = %d, want 3", first)
	}
	if _, ok := b.FindFirst(); ok {
		t.Error("FindFirst returned ok on empty bitmap")
	}
}

func TestSetClearIsSet(t *testing.T) {
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()

	b.Set(first)
	if !b.IsSet(first) {
		t.Errorf("IsSet(%d) = false after Set", first)
	}
	if got := b.NumFree(); got != 1 {
		t.Errorf("NumFree = %d, want 1", got)
	}

	b.Set(first) // idempotent
	if got := b.NumFree(); got != 1 {
		t.Errorf("NumFree after duplicate Set = %d, want 1", got)
	}

	b.Clear(first)
	if b.IsSet(first) {
		t.Errorf("IsSet(%d) = true after Clear", first)
	}
	if got := b.NumFree(); got != 0 {
		t.Errorf("NumFree after Clear = %d, want 0", got)
	}

	b.Clear(first) // idempotent
}

func TestSetRejectsPermanentlyClearRegion(t *testing.T) {
	b := newBitmap(t, 4096)
	for _, p := range []uint64{0, 1, 2} { // meta 0, meta 1, bitmap page 0
		func(page uint64) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("Set(%d) did not panic", page)
				}
			}()
			b.Set(page)
		}(p)
	}
}

func TestSetRejectsOutOfRange(t *testing.T) {
	b := newBitmap(t, 4096)
	defer func() {
		if r := recover(); r == nil {
			t.Error("Set past totalPages did not panic")
		}
	}()
	b.Set(b.TotalPages())
}

func TestFindFirstLowest(t *testing.T) {
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()
	for _, p := range []uint64{first + 100, first + 5, first + 200} {
		b.Set(p)
	}
	p, ok := b.FindFirst()
	if !ok || p != first+5 {
		t.Errorf("FindFirst = (%d, %v), want (%d, true)", p, ok, first+5)
	}
}

func TestFindFirstWrap(t *testing.T) {
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()
	b.Set(first + 2) // page near the start
	b.SetHint(first + 1000)
	p, ok := b.FindFirst()
	if !ok || p != first+2 {
		t.Errorf("FindFirst with hint past free = (%d, %v), want (%d, true)", p, ok, first+2)
	}
}

func TestFindFirstSummarySkipping(t *testing.T) {
	// Larger bitmap to exercise the summary skip path.
	b := newBitmap(t, 200_000)
	first := b.FirstDataPage()
	target := first + 150_000
	b.Set(target)
	p, ok := b.FindFirst()
	if !ok || p != target {
		t.Errorf("FindFirst sparse = (%d, %v), want (%d, true)", p, ok, target)
	}
}

func TestFindContiguousSinglePage(t *testing.T) {
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()
	b.Set(first)
	p, ok := b.FindContiguous(1)
	if !ok || p != first {
		t.Errorf("FindContiguous(1) = (%d, %v), want (%d, true)", p, ok, first)
	}
}

func TestFindContiguousRun(t *testing.T) {
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()
	// Free pages: [first, first+2], [first+10, first+14] (5 in a row), isolated.
	for _, p := range []uint64{first, first + 1, first + 2} {
		b.Set(p)
	}
	for p := first + 10; p < first+15; p++ {
		b.Set(p)
	}
	if p, ok := b.FindContiguous(3); !ok || p != first {
		t.Errorf("FindContiguous(3) = (%d, %v), want (%d, true)", p, ok, first)
	}
	if p, ok := b.FindContiguous(5); !ok || p != first+10 {
		t.Errorf("FindContiguous(5) = (%d, %v), want (%d, true)", p, ok, first+10)
	}
	if _, ok := b.FindContiguous(6); ok {
		t.Error("FindContiguous(6) should fail — no run of 6")
	}
}

func TestFindContiguousFailWhenNotEnoughFree(t *testing.T) {
	b := newBitmap(t, 4096)
	if _, ok := b.FindContiguous(1); ok {
		t.Error("FindContiguous(1) on empty bitmap should fail")
	}
}

func TestDirtyPagesTracking(t *testing.T) {
	// totalPages chosen so multiple bitmap pages cover the range.
	pageSize := uint32(4096)
	totalPages := uint64(pageSize)*8*3 + 5 // spans 4 bitmap pages
	bitsPerPage := uint64(pageSize) * 8
	bitmapPages := uint32((totalPages + bitsPerPage - 1) / bitsPerPage)
	detail := make([]byte, uint64(bitmapPages)*uint64(pageSize))
	b := New(detail, pageSize, bitmapPages, totalPages)

	first := b.FirstDataPage()
	// Pages in different bitmap pages: page in idx 0, 2, 3.
	p0 := first
	p1 := uint64(pageSize)*8*2 + 100 // idx 2
	p2 := totalPages - 2             // idx 3

	b.Set(p0)
	b.Set(p1)
	b.Set(p2)

	dirty := b.DirtyPages()
	want := []uint32{0, 2, 3}
	if !equalUint32(dirty, want) {
		t.Errorf("DirtyPages = %v, want %v", dirty, want)
	}

	b.ClearDirty()
	if got := b.DirtyPages(); len(got) != 0 {
		t.Errorf("DirtyPages after ClearDirty = %v, want empty", got)
	}

	// Idempotent Set must not re-dirty.
	b.Set(p0) // already set, no-op
	if got := b.DirtyPages(); len(got) != 0 {
		t.Errorf("DirtyPages after no-op Set = %v, want empty", got)
	}
}

func TestRecountMatchesIncremental(t *testing.T) {
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()
	for p := first; p < first+50; p++ {
		b.Set(p)
	}
	b.Clear(first + 5)
	b.Clear(first + 10)
	want := uint64(48)
	if got := b.NumFree(); got != want {
		t.Errorf("incremental NumFree = %d, want %d", got, want)
	}
	if got := b.Recount(); got != want {
		t.Errorf("Recount = %d, want %d", got, want)
	}
}

func TestSetHintBoundaries(t *testing.T) {
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()
	b.SetHint(0)
	if got := b.Hint(); got != first {
		t.Errorf("Hint clamped low: got %d, want %d", got, first)
	}
	b.SetHint(b.TotalPages())
	if got := b.Hint(); got != first {
		t.Errorf("Hint clamped high: got %d, want %d", got, first)
	}
	b.SetHint(first + 50)
	if got := b.Hint(); got != first+50 {
		t.Errorf("Hint = %d, want %d", got, first+50)
	}
}

func TestRandomSetClearMatchesPopcount(t *testing.T) {
	b := newBitmap(t, 100_000)
	first := b.FirstDataPage()
	rng := rand.New(rand.NewPCG(0xC0FFEE, 0xFACADE))
	free := make(map[uint64]struct{})
	for range 50_000 {
		page := first + uint64(rng.IntN(int(b.TotalPages()-first)))
		switch rng.IntN(3) {
		case 0:
			b.Set(page)
			free[page] = struct{}{}
		case 1:
			b.Clear(page)
			delete(free, page)
		case 2:
			if b.IsSet(page) {
				if _, ok := free[page]; !ok {
					t.Fatalf("IsSet(%d)=true but not in oracle", page)
				}
			} else if _, ok := free[page]; ok {
				t.Fatalf("IsSet(%d)=false but in oracle", page)
			}
		}
	}
	if got, want := b.NumFree(), uint64(len(free)); got != want {
		t.Errorf("incremental NumFree=%d, oracle=%d", got, want)
	}
	if got := b.Recount(); got != uint64(len(free)) {
		t.Errorf("Recount=%d, oracle=%d", got, len(free))
	}

	// FindFirst should return the minimum free page.
	if len(free) > 0 {
		min := ^uint64(0)
		for p := range free {
			if p < min {
				min = p
			}
		}
		b.SetHint(first)
		p, ok := b.FindFirst()
		if !ok || p != min {
			t.Errorf("FindFirst = (%d, %v), want (%d, true)", p, ok, min)
		}
	}
}

func TestPageBytesAliasingForward(t *testing.T) {
	b := newBitmap(t, 8192)
	first := b.FirstDataPage()
	b.Set(first)
	bytes := b.PageBytes(0)
	if len(bytes) != int(b.pageSize) {
		t.Fatalf("PageBytes length = %d, want %d", len(bytes), b.pageSize)
	}
	// The bit for `first` should be set in the returned bytes.
	byteIdx := first / 8
	bitIdx := first % 8
	if bytes[byteIdx]&(1<<bitIdx) == 0 {
		t.Error("PageBytes does not reflect Set")
	}
}

func TestNewPanicsOnDetailMismatch(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on wrong-size detail")
		}
	}()
	New(make([]byte, 100), 4096, 1, 100)
}

func TestNewPanicsOnTotalPagesExceedingCapacity(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on totalPages exceeding capacity")
		}
	}()
	New(make([]byte, 4096), 4096, 1, 4096*8+1)
}

func TestIsSetOutOfRange(t *testing.T) {
	b := newBitmap(t, 4096)
	if b.IsSet(b.TotalPages()) {
		t.Error("IsSet past totalPages = true")
	}
	if b.IsSet(b.TotalPages() + 1000) {
		t.Error("IsSet far past totalPages = true")
	}
}

func TestPageBytesPanicsOnOutOfRange(t *testing.T) {
	b := newBitmap(t, 4096)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	b.PageBytes(b.bitmapPages)
}

func TestClearDirtyNoOp(t *testing.T) {
	b := newBitmap(t, 4096)
	b.ClearDirty() // already empty
	if got := b.DirtyPages(); len(got) != 0 {
		t.Errorf("DirtyPages after no-op ClearDirty = %v", got)
	}
}

func TestFindContiguousWrap(t *testing.T) {
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()
	// Pre-load a run of 3 near the start.
	for p := first; p < first+3; p++ {
		b.Set(p)
	}
	// Hint past the run forces the wrap pass to find it.
	b.SetHint(first + 1000)
	p, ok := b.FindContiguous(3)
	if !ok || p != first {
		t.Errorf("FindContiguous(3) with wrap = (%d, %v), want (%d, true)", p, ok, first)
	}
}

func TestClearRejectsPermanentlyClearRegion(t *testing.T) {
	b := newBitmap(t, 4096)
	for _, p := range []uint64{0, 1, 2} {
		func(page uint64) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("Clear(%d) did not panic", page)
				}
			}()
			b.Clear(page)
		}(p)
	}
}

func TestNewMasksOutOfRangeDetail(t *testing.T) {
	pageSize := uint32(4096)
	bitmapPages := uint32(1)
	// firstDataPage = 3. totalPages = 100 means valid range is [3, 100).
	totalPages := uint64(100)
	detail := make([]byte, uint64(bitmapPages)*uint64(pageSize))

	// Bit in permanently-clear region (page 1): byte 0 bit 1.
	detail[0] = 1 << 1
	// Bit at page totalPages (out of range): byte 12 bit 4.
	detail[totalPages>>3] |= 1 << (totalPages & 7)
	// Bit far past totalPages (last byte of bitmap page).
	detail[len(detail)-1] = 0xFF
	// Sole valid in-range bit: page 50.
	detail[50>>3] |= 1 << (50 & 7)

	b := New(detail, pageSize, bitmapPages, totalPages)

	if b.IsSet(1) {
		t.Error("bit 1 (in permanently-clear region) still set after New mask")
	}
	if b.IsSet(totalPages) {
		t.Error("bit at totalPages still set after New mask")
	}
	if !b.IsSet(50) {
		t.Error("valid in-range bit dropped by New mask")
	}
	if b.NumFree() != 1 {
		t.Errorf("NumFree after mask = %d, want 1", b.NumFree())
	}
}

func TestNewPanicsOnInvalidPageSize(t *testing.T) {
	for _, sz := range []uint32{0, 2048, 4097, 131072} {
		func(s uint32) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("New(pageSize=%d) did not panic", s)
				}
			}()
			New(make([]byte, s), s, 1, 0)
		}(sz)
	}
}

func TestPageBytesWritesAlias(t *testing.T) {
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()
	bytes := b.PageBytes(0)
	// Set a bit via the aliased slice; the bitmap should observe it.
	target := first + 10
	byteIdx := target / 8
	bitIdx := target % 8
	bytes[byteIdx] |= 1 << bitIdx
	if !b.IsSet(target) {
		t.Error("write via PageBytes alias not observable via IsSet")
	}
	// Conversely, Set via the bitmap should be observable via the alias.
	target2 := first + 20
	b.Set(target2)
	byteIdx = target2 / 8
	bitIdx = target2 % 8
	if bytes[byteIdx]&(1<<bitIdx) == 0 {
		t.Error("Set not observable via PageBytes alias")
	}
}

func TestFindContiguousStraddleWrap(t *testing.T) {
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()
	// Set a run of 4 contiguous pages starting near the start.
	for p := first; p < first+4; p++ {
		b.Set(p)
	}
	// Set the hint inside that run so pass 1 doesn't see the start.
	b.SetHint(first + 2)
	p, ok := b.FindContiguous(4)
	if !ok || p != first {
		t.Errorf("FindContiguous(4) straddle = (%d, %v), want (%d, true)", p, ok, first)
	}
}

func TestFindContiguousCrossWord(t *testing.T) {
	// Free a 70-page run spanning a word boundary; ensure the math/bits
	// scan carries across.
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()
	for p := first; p < first+70; p++ {
		b.Set(p)
	}
	p, ok := b.FindContiguous(70)
	if !ok || p != first {
		t.Errorf("FindContiguous(70) cross-word = (%d, %v), want (%d, true)", p, ok, first)
	}
	// A 71-page run requires one more bit; should fail.
	if _, ok := b.FindContiguous(71); ok {
		t.Error("FindContiguous(71) should fail (only 70 set)")
	}
}

func TestFindContiguousLargeMultiWord(t *testing.T) {
	// Run spanning multiple full words to exercise the all-ones fast path.
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()
	for p := first; p < first+200; p++ {
		b.Set(p)
	}
	if p, ok := b.FindContiguous(200); !ok || p != first {
		t.Errorf("FindContiguous(200) = (%d, %v), want (%d, true)", p, ok, first)
	}
	if p, ok := b.FindContiguous(150); !ok || p != first {
		t.Errorf("FindContiguous(150) = (%d, %v), want (%d, true)", p, ok, first)
	}
	if _, ok := b.FindContiguous(201); ok {
		t.Error("FindContiguous(201) should fail")
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	// Direct unit test for the Snapshot/Restore mechanism that the
	// pager relies on for tx rollback. Walks the round-trip independently
	// of any pager integration so a subtle copy-vs-alias bug in
	// Snapshot() surfaces here instead of leaking into a downstream
	// integration test.
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()
	// Pre-populate state.
	for _, p := range []uint64{first + 1, first + 5, first + 50, first + 100} {
		b.Set(p)
	}
	b.SetHint(first + 50)

	snap := b.Snapshot()
	prevNumFree := b.NumFree()
	prevHint := b.Hint()
	prevDirty := b.DirtyPages()

	// Mutate after snapshot: two clears, one set → net -1 free.
	b.Clear(first + 5)
	b.Clear(first + 50)
	b.Set(first + 200)
	b.SetHint(first + 200)
	if b.NumFree() == prevNumFree {
		t.Fatal("test setup: mutations did not change NumFree")
	}

	b.Restore(snap)

	if b.NumFree() != prevNumFree {
		t.Errorf("NumFree = %d, want %d", b.NumFree(), prevNumFree)
	}
	if b.Hint() != prevHint {
		t.Errorf("Hint = %d, want %d", b.Hint(), prevHint)
	}
	if !b.IsSet(first + 5) {
		t.Error("post-Restore: bit cleared during mutation not restored")
	}
	if b.IsSet(first + 200) {
		t.Error("post-Restore: bit set during mutation not undone")
	}
	got := b.DirtyPages()
	if !equalUint32(got, prevDirty) {
		t.Errorf("DirtyPages post-Restore = %v, want %v", got, prevDirty)
	}
}

func TestSnapshotIsolationFromSubsequentMutations(t *testing.T) {
	// A snapshot must not alias the bitmap's storage — mutations after
	// Snapshot must not change snapshot bytes.
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()
	b.Set(first + 1)
	snap := b.Snapshot()

	// Tamper post-snapshot.
	b.Set(first + 999)
	// Restore must still yield the snapshot's state, not a mix.
	b.Restore(snap)
	if b.IsSet(first + 999) {
		t.Error("Restore left a post-snapshot mutation visible — snapshot aliased the detail")
	}
	if !b.IsSet(first + 1) {
		t.Error("Restore lost a pre-snapshot mutation")
	}
}

func TestClearOnAlreadyClearIsNoOp(t *testing.T) {
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()
	// Clear a never-set page.
	b.Clear(first)
	if b.IsSet(first) {
		t.Error("IsSet true after Clear of clear bit")
	}
	if b.NumFree() != 0 {
		t.Errorf("NumFree changed by Clear of clear bit: %d", b.NumFree())
	}
	if len(b.DirtyPages()) != 0 {
		t.Error("Clear of clear bit dirtied the page")
	}
}

// TestSnapshotNestedLIFORoundTrip exercises the entailed-invariant
// "Restore(s) is observationally equivalent to undoing every flip
// since Snapshot returned s" for nested Snapshots (the pager's
// BeginTx + BeginSavepoint usage; see transactions.md §Why this is
// cheap). The undo-log substrate shares one flip log across all open
// Snapshots; a bug in the cascade-on-restore or in indexOfSnapshot
// would surface here as either a stale flip persisting past an outer
// Restore, or an inner Restore disturbing the outer's view.
func TestSnapshotNestedLIFORoundTrip(t *testing.T) {
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()

	// Initial state captured for the outer Snapshot.
	initialNumFree := b.NumFree()
	initialDirty := b.DirtyPages()

	outer := b.Snapshot()

	// Outer-window mutations.
	b.Set(first + 1)
	b.Set(first + 100)
	b.Clear(first + 1) // re-clear: a same-bit toggle within the outer window
	b.Set(first + 200)
	afterOuterMutations := b.NumFree()
	if afterOuterMutations == initialNumFree {
		t.Fatal("test setup: outer mutations did not change NumFree")
	}

	inner := b.Snapshot()
	// Capture inner's begin state to assert nested-restore returns there.
	innerBeginNumFree := b.NumFree()

	// Inner-window mutations.
	b.Set(first + 300)
	b.Clear(first + 100) // touch a page set in outer window
	b.Set(first + 400)

	b.Restore(inner)

	// After inner restore: state == inner-begin state. The outer's
	// flips (first+100 set, first+200 set, first+1 toggled clear) must
	// remain; the inner's flips (first+300, first+400 set; first+100
	// cleared) must be reverted.
	if b.NumFree() != innerBeginNumFree {
		t.Errorf("post-inner-restore NumFree = %d, want %d (inner-begin)",
			b.NumFree(), innerBeginNumFree)
	}
	if !b.IsSet(first + 100) {
		t.Error("post-inner-restore: outer-window Set(first+100) not preserved")
	}
	if !b.IsSet(first + 200) {
		t.Error("post-inner-restore: outer-window Set(first+200) not preserved")
	}
	if b.IsSet(first + 1) {
		t.Error("post-inner-restore: outer-window toggle to clear not preserved")
	}
	if b.IsSet(first + 300) {
		t.Error("post-inner-restore: inner-window Set(first+300) not reverted")
	}
	if b.IsSet(first + 400) {
		t.Error("post-inner-restore: inner-window Set(first+400) not reverted")
	}

	// After-inner mutations land in the outer's revert window.
	b.Set(first + 500)
	b.Clear(first + 100) // re-clear inside outer revert window

	b.Restore(outer)

	// After outer restore: state == initial.
	if b.NumFree() != initialNumFree {
		t.Errorf("post-outer-restore NumFree = %d, want %d (initial)",
			b.NumFree(), initialNumFree)
	}
	for _, p := range []uint64{first + 1, first + 100, first + 200, first + 300, first + 400, first + 500} {
		if b.IsSet(p) {
			t.Errorf("post-outer-restore: page %d still set, want clear", p)
		}
	}
	got := b.DirtyPages()
	if !equalUint32(got, initialDirty) {
		t.Errorf("post-outer-restore DirtyPages = %v, want %v", got, initialDirty)
	}
}

// TestSnapshotRestoreCrossBitmapPageDirtySet pins the wholesale-dirty-
// restore path across distinct bitmap pages. The single-bitmap-page
// round-trip tests (4096-totalPages newBitmap) leave b.dirty at most
// {0} — they don't exercise the case where a post-Snapshot mutation
// dirties a new bitmap page that the captured-at-Snapshot dirty set
// must NOT contain after Restore. A regression that aliased b.dirty to
// the live set (rather than capturing a clone at Snapshot time) would
// pass the single-page tests and fail here.
func TestSnapshotRestoreCrossBitmapPageDirtySet(t *testing.T) {
	// pageSize=4096 → bitsPerPage=32768. 1<<20 totalPages → 32 bitmap pages.
	const totalPages = uint64(1) << 20
	b := newBitmap(t, totalPages)
	first := b.FirstDataPage()

	// Pre-Snapshot: mutate a data page in bitmap-page 0.
	pageInBM0 := first + 1
	b.Set(pageInBM0)
	preDirty := b.DirtyPages()
	// Sanity-check: pre-state lives only in bitmap-page 0.
	if len(preDirty) != 1 || preDirty[0] != 0 {
		t.Fatalf("test setup: preDirty = %v, want [0]", preDirty)
	}

	snap := b.Snapshot()

	// Post-Snapshot: mutate a data page in bitmap-page 5 — markDirty
	// will index = page / (pageSize*8) = page / 32768.
	pageInBM5 := uint64(5*32768 + 100)
	if pageInBM5 >= totalPages {
		t.Fatalf("test setup: pageInBM5 %d out of range", pageInBM5)
	}
	b.Set(pageInBM5)
	postDirty := b.DirtyPages()
	if len(postDirty) != 2 {
		t.Fatalf("test setup: postDirty = %v, want 2 entries (BM-page 0 and 5)", postDirty)
	}

	b.Restore(snap)

	// After Restore: pageInBM5 bit reverted; bitmap-page 5 dropped from
	// dirty (it was added only during this Snapshot's window).
	if b.IsSet(pageInBM5) {
		t.Error("post-Restore: pageInBM5 bit not reverted")
	}
	got := b.DirtyPages()
	if !equalUint32(got, preDirty) {
		t.Errorf("post-Restore DirtyPages = %v, want %v (bitmap-page 5 should have been dropped)",
			got, preDirty)
	}
}

// TestSnapshotDiscardIsObservableNoOp pins the entailed invariant
// "Discard(s) releases per-Snapshot tracking without changing
// observable Bitmap state": after a Snapshot is Discarded (the
// ReleaseSavepoint / commit-success path), every IsSet/NumFree/Hint/
// DirtyPages reading matches the pre-Discard value. A regression that
// (e.g.) accidentally re-applied undo entries on Discard would surface
// here.
func TestSnapshotDiscardIsObservableNoOp(t *testing.T) {
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()

	b.Set(first + 1)
	b.Set(first + 7)
	b.SetHint(first + 5)

	snap := b.Snapshot()
	b.Set(first + 50)
	b.Clear(first + 7)
	b.SetHint(first + 50)

	preDiscardNumFree := b.NumFree()
	preDiscardHint := b.Hint()
	preDiscardDirty := b.DirtyPages()
	preDiscardSet1 := b.IsSet(first + 1)
	preDiscardSet7 := b.IsSet(first + 7)
	preDiscardSet50 := b.IsSet(first + 50)

	b.Discard(snap)

	if b.NumFree() != preDiscardNumFree {
		t.Errorf("Discard changed NumFree: %d → %d", preDiscardNumFree, b.NumFree())
	}
	if b.Hint() != preDiscardHint {
		t.Errorf("Discard changed Hint: %d → %d", preDiscardHint, b.Hint())
	}
	if !equalUint32(b.DirtyPages(), preDiscardDirty) {
		t.Errorf("Discard changed DirtyPages: %v → %v", preDiscardDirty, b.DirtyPages())
	}
	if b.IsSet(first+1) != preDiscardSet1 {
		t.Error("Discard changed IsSet(first+1)")
	}
	if b.IsSet(first+7) != preDiscardSet7 {
		t.Error("Discard changed IsSet(first+7)")
	}
	if b.IsSet(first+50) != preDiscardSet50 {
		t.Error("Discard changed IsSet(first+50)")
	}
}

// TestDiscardReleasesUndoLogTracking asserts that after a Discard of
// the last open Snapshot, the bitmap's undo-log tracking is fully
// released — a subsequent Set on the same page must NOT be recorded
// (no Snapshot exists to replay it). Validated indirectly by opening
// a fresh Snapshot and verifying Restore from that Snapshot does not
// revert pre-Snapshot mutations.
func TestDiscardReleasesUndoLogTracking(t *testing.T) {
	b := newBitmap(t, 4096)
	first := b.FirstDataPage()

	// Pre-snapshot baseline mutations.
	b.Set(first + 1)

	// Open + Discard a snapshot to exercise the "log truncates on last
	// Discard" path.
	snap := b.Snapshot()
	b.Set(first + 2)
	b.Discard(snap)

	// At this point the undo log should be released (len == 0).
	// Subsequent mutations with NO snapshot open should not be tracked.
	b.Set(first + 3)

	// Now open a fresh snapshot, do nothing, restore. The fresh restore
	// must not revert first+1, first+2, or first+3 — none of those were
	// inside an open snapshot's window.
	snap2 := b.Snapshot()
	b.Restore(snap2)

	for _, p := range []uint64{first + 1, first + 2, first + 3} {
		if !b.IsSet(p) {
			t.Errorf("page %d lost after Snapshot/Restore of an empty window — undo log was not properly released by Discard", p)
		}
	}
}

// TestRestoreOnNonOpenSnapshotPanics pins the strict-LIFO contract:
// calling Restore on a Snapshot that has already been Restored or
// Discarded (or never returned by this Bitmap's Snapshot()) panics.
// Surfacing the misuse loudly is safer than silently producing wrong
// state when openSnapshots tracking is bypassed.
func TestRestoreOnNonOpenSnapshotPanics(t *testing.T) {
	b := newBitmap(t, 4096)
	snap := b.Snapshot()
	b.Discard(snap)

	defer func() {
		if r := recover(); r == nil {
			t.Error("Restore on a discarded Snapshot did not panic")
		}
	}()
	b.Restore(snap)
}

// TestDiscardOnNonOpenSnapshotPanics mirrors the above for Discard.
func TestDiscardOnNonOpenSnapshotPanics(t *testing.T) {
	b := newBitmap(t, 4096)
	snap := b.Snapshot()
	b.Restore(snap)

	defer func() {
		if r := recover(); r == nil {
			t.Error("Discard on a restored Snapshot did not panic")
		}
	}()
	b.Discard(snap)
}

func equalUint32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	x := slices.Clone(a)
	y := slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}
