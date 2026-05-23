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
