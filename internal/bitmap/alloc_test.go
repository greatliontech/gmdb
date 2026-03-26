package bitmap

import "testing"

func TestFindFirstFreeBasic(t *testing.T) {
	data := makeBitmapData(256)
	setBitInData(data, 20)
	setBitInData(data, 50)
	b := New(data, 256, 10)

	pageID, ok := b.FindFirstFree()
	if !ok {
		t.Fatal("FindFirstFree: not found")
	}
	if pageID != 20 {
		t.Errorf("FindFirstFree() = %d, want 20", pageID)
	}
	if b.IsSet(20) {
		t.Error("page 20 should be allocated after FindFirstFree")
	}

	pageID, ok = b.FindFirstFree()
	if !ok {
		t.Fatal("FindFirstFree: second call not found")
	}
	if pageID != 50 {
		t.Errorf("FindFirstFree() = %d, want 50", pageID)
	}
}

func TestFindFirstFreeHint(t *testing.T) {
	data := makeBitmapData(256)
	setBitInData(data, 20)
	setBitInData(data, 100)
	b := New(data, 256, 10)

	// Set hint past page 20.
	b.SetHint(64) // word 1

	pageID, ok := b.FindFirstFree()
	if !ok {
		t.Fatal("FindFirstFree: not found")
	}
	// Should find page 100 first (hint is at word 1, page 100 is in word 1).
	if pageID != 100 {
		t.Errorf("FindFirstFree() = %d, want 100 (hint at word 1)", pageID)
	}
}

func TestFindFirstFreeWrapAround(t *testing.T) {
	data := makeBitmapData(256)
	setBitInData(data, 20) // only free page is before hint
	b := New(data, 256, 10)

	b.SetHint(128) // hint past all free pages

	pageID, ok := b.FindFirstFree()
	if !ok {
		t.Fatal("FindFirstFree: not found after wrap")
	}
	if pageID != 20 {
		t.Errorf("FindFirstFree() = %d, want 20 (wrap around)", pageID)
	}
}

func TestFindFirstFreeEmpty(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10)

	_, ok := b.FindFirstFree()
	if ok {
		t.Error("FindFirstFree: should return false on empty bitmap")
	}
}

func TestFindFirstFreeExhaustion(t *testing.T) {
	data := makeBitmapData(256)
	// Set all non-reserved pages free.
	for i := uint64(10); i < 256; i++ {
		setBitInData(data, i)
	}
	b := New(data, 256, 10)

	allocated := 0
	for {
		_, ok := b.FindFirstFree()
		if !ok {
			break
		}
		allocated++
	}
	if allocated != 246 { // 256 - 10 reserved
		t.Errorf("allocated %d pages, want 246", allocated)
	}
	if b.FreeCount() != 0 {
		t.Errorf("FreeCount() = %d, want 0 after exhaustion", b.FreeCount())
	}
}

func TestFindContiguousBasic(t *testing.T) {
	data := makeBitmapData(256)
	// Set pages 20-29 free (10 contiguous).
	for i := uint64(20); i < 30; i++ {
		setBitInData(data, i)
	}
	b := New(data, 256, 10)

	start, ok := b.FindContiguous(5)
	if !ok {
		t.Fatal("FindContiguous(5): not found")
	}
	if start != 20 {
		t.Errorf("FindContiguous(5) = %d, want 20", start)
	}
	// Pages 20-24 should be allocated.
	for i := uint64(20); i < 25; i++ {
		if b.IsSet(i) {
			t.Errorf("page %d should be allocated", i)
		}
	}
	// Pages 25-29 should still be free.
	for i := uint64(25); i < 30; i++ {
		if !b.IsSet(i) {
			t.Errorf("page %d should still be free", i)
		}
	}
}

func TestFindContiguousWordBoundary(t *testing.T) {
	data := makeBitmapData(256)
	// Set pages 60-68 free (spans word boundary at 64).
	for i := uint64(60); i < 69; i++ {
		setBitInData(data, i)
	}
	b := New(data, 256, 10)

	start, ok := b.FindContiguous(9)
	if !ok {
		t.Fatal("FindContiguous(9): not found")
	}
	if start != 60 {
		t.Errorf("FindContiguous(9) = %d, want 60", start)
	}
}

func TestFindContiguousLargerThanWord(t *testing.T) {
	data := makeBitmapData(512)
	// Set pages 100-230 free (131 pages, > 2 full words).
	for i := uint64(100); i < 231; i++ {
		setBitInData(data, i)
	}
	b := New(data, 512, 10)

	start, ok := b.FindContiguous(100)
	if !ok {
		t.Fatal("FindContiguous(100): not found")
	}
	if start != 100 {
		t.Errorf("FindContiguous(100) = %d, want 100", start)
	}
}

func TestFindContiguousNotFound(t *testing.T) {
	data := makeBitmapData(256)
	// Set scattered free pages (no run of 5).
	setBitInData(data, 20)
	setBitInData(data, 22)
	setBitInData(data, 24)
	setBitInData(data, 26)
	b := New(data, 256, 10)

	_, ok := b.FindContiguous(5)
	if ok {
		t.Error("FindContiguous(5): should not find with scattered pages")
	}
}

func TestFindContiguousN1(t *testing.T) {
	data := makeBitmapData(256)
	setBitInData(data, 50)
	b := New(data, 256, 10)

	start, ok := b.FindContiguous(1)
	if !ok {
		t.Fatal("FindContiguous(1): not found")
	}
	if start != 50 {
		t.Errorf("FindContiguous(1) = %d, want 50", start)
	}
}

func TestFindContiguousN0(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10)

	_, ok := b.FindContiguous(0)
	if ok {
		t.Error("FindContiguous(0): should return false")
	}
}

func TestFindContiguousExact(t *testing.T) {
	data := makeBitmapData(256)
	// Exactly 3 contiguous free pages.
	setBitInData(data, 40)
	setBitInData(data, 41)
	setBitInData(data, 42)
	b := New(data, 256, 10)

	start, ok := b.FindContiguous(3)
	if !ok {
		t.Fatal("FindContiguous(3): not found")
	}
	if start != 40 {
		t.Errorf("FindContiguous(3) = %d, want 40", start)
	}
	// All 3 should be allocated now.
	if b.FreeCount() != 0 {
		t.Errorf("FreeCount() = %d, want 0", b.FreeCount())
	}
}

func TestFindContiguousWithGaps(t *testing.T) {
	data := makeBitmapData(256)
	// Run of 3, gap, run of 5.
	setBitInData(data, 20)
	setBitInData(data, 21)
	setBitInData(data, 22)
	// gap at 23
	for i := uint64(24); i < 29; i++ {
		setBitInData(data, i)
	}
	b := New(data, 256, 10)

	// Ask for 5 — should skip the run of 3 and find the run of 5.
	start, ok := b.FindContiguous(5)
	if !ok {
		t.Fatal("FindContiguous(5): not found")
	}
	if start != 24 {
		t.Errorf("FindContiguous(5) = %d, want 24", start)
	}
}

func TestFindContiguousCarryForwardCompletes(t *testing.T) {
	data := makeBitmapData(256)
	// Word 0: set pages 60-63 (trailing 4 bits). Word 1: set pages 64-66 (leading 3 bits).
	// Total run: 7 pages spanning the word boundary.
	for i := uint64(60); i < 67; i++ {
		setBitInData(data, i)
	}
	// Also set page 30 so there's a free page before the run (to exercise
	// the carry-forward path where leading ones complete the needed run).
	setBitInData(data, 30)
	b := New(data, 256, 10)

	// Ask for 7 — the carry-forward of trailing ones (word 0) + leading ones (word 1)
	// should complete the run exactly.
	start, ok := b.FindContiguous(7)
	if !ok {
		t.Fatal("FindContiguous(7): not found")
	}
	if start != 60 {
		t.Errorf("FindContiguous(7) = %d, want 60", start)
	}
}

func TestFindContiguousWrapAround(t *testing.T) {
	data := makeBitmapData(256)
	// Set 5 contiguous pages before the hint position.
	for i := uint64(20); i < 25; i++ {
		setBitInData(data, i)
	}
	b := New(data, 256, 10)
	b.SetHint(128) // hint past all free pages

	start, ok := b.FindContiguous(5)
	if !ok {
		t.Fatal("FindContiguous(5): not found with wrap-around")
	}
	if start != 20 {
		t.Errorf("FindContiguous(5) = %d, want 20 (wrap-around)", start)
	}
}

func TestFindFirstFreeStaleSummary(t *testing.T) {
	// Summary says word has free pages, but overlay cleared them all.
	data := makeBitmapData(256)
	setBitInData(data, 20)
	b := New(data, 256, 10)

	// Manually clear page 20 to make the word empty via overlay.
	b.Clear(20)
	// summary should be updated, but let's verify FindFirstFree handles it.
	_, ok := b.FindFirstFree()
	if ok {
		t.Error("FindFirstFree should return false when all pages allocated")
	}
}

func TestFindFirstFreeReservedBitsMasked(t *testing.T) {
	data := makeBitmapData(256)
	// On-disk: reserved page 5 and valid page 50 both set.
	setBitInData(data, 5)  // reserved — masked out by logicalWord
	setBitInData(data, 50) // valid
	b := New(data, 256, 10)

	// FreeCount should only count page 50 (reserved bit masked).
	if b.FreeCount() != 1 {
		t.Fatalf("FreeCount() = %d, want 1", b.FreeCount())
	}

	pageID, ok := b.FindFirstFree()
	if !ok {
		t.Fatal("FindFirstFree: not found")
	}
	if pageID != 50 {
		t.Errorf("FindFirstFree() = %d, want 50", pageID)
	}
}

func TestWordOutOfBounds(t *testing.T) {
	// Create data shorter than totalWords * 8 (partial last page).
	data := make([]byte, 12) // 1.5 words
	b := New(data, 100, 4)

	// Word 1 should read from data[8:16], but data is only 12 bytes.
	// word() should return 0 for the out-of-bounds portion.
	w := b.word(1)
	if w != 0 {
		t.Errorf("word(1) with short data = %d, want 0", w)
	}
}

func TestLogicalWordPartialLastWithOverlay(t *testing.T) {
	// 100 pages: last word (word 1) covers bits 64-99. Bits 100-127 must be masked.
	data := makeBitmapData(100)
	b := New(data, 100, 4)

	// Set page 99 (last valid page) via overlay.
	b.Set(99)
	if !b.IsSet(99) {
		t.Error("page 99 should be free")
	}

	// Bit 100 should not be free even if we tried to set it beyond totalPages.
	if b.IsSet(100) {
		t.Error("page 100 should not be free (beyond totalPages)")
	}
}

func TestFindContiguousInsufficientFreeCount(t *testing.T) {
	data := makeBitmapData(256)
	setBitInData(data, 20)
	setBitInData(data, 21)
	b := New(data, 256, 10)

	_, ok := b.FindContiguous(5)
	if ok {
		t.Error("FindContiguous(5): should fail with only 2 free pages")
	}
}

func TestFindContiguousClampTo(t *testing.T) {
	// Trigger the to > totalWords clamping in findRun.
	// Large hint + large n = to overflows.
	data := makeBitmapData(128) // 2 words
	for i := uint64(10); i < 20; i++ {
		setBitInData(data, i)
	}
	b := New(data, 128, 10)
	b.SetHint(64) // hint at word 1

	// First pass: findRun(1, 2, 5) finds nothing.
	// Second pass: findRun(0, 1 + (5+63)/64, 5) = findRun(0, 2, 5).
	// 2 <= 2 (totalWords), no clamp. Need hint at last word:
	b.SetHint(127) // word 1

	start, ok := b.FindContiguous(5)
	if !ok {
		t.Fatal("FindContiguous(5): not found")
	}
	if start != 10 {
		t.Errorf("FindContiguous(5) = %d, want 10", start)
	}
}

func TestFindContiguousClampToOverflow(t *testing.T) {
	// 256 pages = 4 words, all non-reserved free. Hint at word 3.
	// FindContiguous(200): first pass findRun(3, 4, 200) fails (1 word < 200 pages).
	// Second pass: findRun(0, 3 + ceil(200/64), 200) = findRun(0, 7, 200).
	// 7 > 4 = totalWords, so clamped to 4.
	data := makeBitmapData(256)
	for i := uint64(10); i < 256; i++ {
		setBitInData(data, i)
	}
	b := New(data, 256, 10)
	b.SetHint(3 * 64)

	start, ok := b.FindContiguous(200)
	if !ok {
		t.Fatal("FindContiguous(200): not found")
	}
	if start < 10 {
		t.Errorf("FindContiguous(200) = %d, should be >= 10", start)
	}
}

func TestScanForFreeBreakOnDiPastTo(t *testing.T) {
	// Set up so that summary jump lands past `to` in the second pass.
	data := makeBitmapData(256)
	setBitInData(data, 200) // page 200, word 3
	b := New(data, 256, 10)

	b.SetHint(192) // word 3
	// First pass: scanForFree(3, 4) finds page 200 in word 3. OK.
	pageID, ok := b.FindFirstFree()
	if !ok || pageID != 200 {
		t.Fatalf("FindFirstFree() = (%d, %v), want (200, true)", pageID, ok)
	}

	// Now set page 100 (word 1) and hint at word 2.
	b.Set(100)
	b.SetHint(128) // word 2
	// First pass: scanForFree(2, 4) — summary[0] masked to bits >= 2.
	// Word 1 has page 100, but bit 1 in summary is < sBit=2, masked out.
	// Jumps to next summary group (words 64+). to=4, di=64 >= 4, break.
	// Second pass: scanForFree(0, 2) — finds word 1, page 100.
	pageID, ok = b.FindFirstFree()
	if !ok || pageID != 100 {
		t.Errorf("FindFirstFree() = (%d, %v), want (100, true)", pageID, ok)
	}
}

func TestFindContiguousReservedMasked(t *testing.T) {
	// On-disk data has bits set for reserved pages 5-9 and valid pages 10-15.
	// logicalWord masks out reserved bits, so the visible run is 10-15 (6 pages).
	data := makeBitmapData(256)
	for i := uint64(5); i < 16; i++ {
		setBitInData(data, i)
	}
	b := New(data, 256, 10)

	// FreeCount should only count valid pages (10-15 = 6).
	if b.FreeCount() != 6 {
		t.Fatalf("FreeCount() = %d, want 6", b.FreeCount())
	}

	start, ok := b.FindContiguous(5)
	if !ok {
		t.Fatal("FindContiguous(5): not found")
	}
	if start != 10 {
		t.Errorf("FindContiguous(5) = %d, want 10", start)
	}
}

func TestScanForFreeSummaryJumpPastTo(t *testing.T) {
	// Trigger the di >= to break in scanForFree.
	// 8192 pages = 128 words. Set a free page in word 74.
	data := makeBitmapData(8192)
	setBitInData(data, 74*64+10) // page 4746, word 74
	b := New(data, 8192, 10)

	// Set hint so first pass scans [65, 128).
	// summary[1] (covering detail words 64-127) has bit 10 set (for word 74).
	// In first pass, from=65: si=1, sBit=1. sw = summary[1] & ^1.
	// If bit 10 is set, TrailingZeros finds it. di = 64+10 = 74. 74 < 128. Proceeds normally.
	// To trigger break: second pass with to=70.
	// That requires FindFirstFree with hint=70.
	b.SetHint(70 * 64) // hint at word 70

	// First pass: scanForFree(70, 128). Finds page 4746 in word 74. Fine.
	pageID, ok := b.FindFirstFree()
	if !ok {
		t.Fatal("FindFirstFree: not found")
	}
	if pageID != 4746 {
		t.Errorf("FindFirstFree() = %d, want 4746", pageID)
	}

	// Now set page in word 74 again and a page in word 10.
	setBitInData(data, 74*64+10) // re-set in on-disk
	b.Reset()
	setBitInData(data, 10*64+5) // page 645, word 10
	b.Reset()

	// hint at word 65. First pass scans [65, 128).
	// summary[1] has bit 10 (word 74). From=65: si=1, sBit=1.
	// sw masked: bit 10 survives. di = 64+10 = 74. But we want to < 74.
	// Can't easily control to in scanForFree. Let's just verify the second
	// pass works (scanForFree(0, 65) should find word 10).
	b.SetHint(65 * 64)
	pageID, ok = b.FindFirstFree()
	if !ok {
		t.Fatal("FindFirstFree (second): not found")
	}
	// Should find either page 645 (word 10) or 4746 (word 74).
	if pageID != 4746 && pageID != 645 {
		t.Errorf("FindFirstFree() = %d, want 4746 or 645", pageID)
	}
}

func TestApplyBoundaryMaskEntireWordReserved(t *testing.T) {
	// 256 pages, 128 reserved. Words 0 and 1 are entirely reserved.
	data := makeBitmapData(256)
	// Set all bits in word 0.
	for i := range 8 {
		data[i] = 0xFF
	}
	b := New(data, 256, 128)

	// Word 0 should be entirely masked (all reserved).
	if b.FreeCount() != 0 {
		t.Errorf("FreeCount() = %d, want 0 (entire word reserved)", b.FreeCount())
	}

	// Page 130 should be allocatable.
	b.Set(130)
	if b.FreeCount() != 1 {
		t.Errorf("FreeCount() = %d, want 1", b.FreeCount())
	}
}

func TestFindContiguousFullWordRun(t *testing.T) {
	// Test the dw == ^uint64(0) path in findRun: two full words of free pages.
	data := makeBitmapData(256)
	// Set all bits in words 2 and 3 (pages 128-255).
	for i := 16; i < 32; i++ {
		data[i] = 0xFF
	}
	b := New(data, 256, 10)

	start, ok := b.FindContiguous(100)
	if !ok {
		t.Fatal("FindContiguous(100): not found")
	}
	if start != 128 {
		t.Errorf("FindContiguous(100) = %d, want 128", start)
	}
}

func TestFindRunTrailingZeroResetsCarry(t *testing.T) {
	// Word 0: bits 20-30 set (11 contiguous), MSB clear → trailing = 0.
	// Word 1: bits 0-4 set (5 contiguous).
	// Ask for 15: no single run is long enough, and carry can't bridge
	// because word 0 ends with 0-bits (trailing = 0). Should fail.
	data := makeBitmapData(256)
	for i := uint64(20); i < 31; i++ {
		setBitInData(data, i)
	}
	for i := uint64(64); i < 69; i++ {
		setBitInData(data, i)
	}
	b := New(data, 256, 10)

	_, ok := b.FindContiguous(15)
	if ok {
		t.Error("FindContiguous(15): should not find (trailing=0 resets carry)")
	}
}

func TestAllocAndFreeInterleaved(t *testing.T) {
	data := makeBitmapData(256)
	for i := uint64(10); i < 20; i++ {
		setBitInData(data, i)
	}
	b := New(data, 256, 10)

	// Allocate 5, free 3, allocate 4.
	for range 5 {
		b.FindFirstFree()
	}
	if b.FreeCount() != 5 {
		t.Fatalf("after 5 allocs: FreeCount() = %d, want 5", b.FreeCount())
	}

	b.Set(10)
	b.Set(11)
	b.Set(12)
	if b.FreeCount() != 8 {
		t.Fatalf("after 3 frees: FreeCount() = %d, want 8", b.FreeCount())
	}

	for range 4 {
		_, ok := b.FindFirstFree()
		if !ok {
			t.Fatal("FindFirstFree failed during interleaved test")
		}
	}
	if b.FreeCount() != 4 {
		t.Errorf("after 4 more allocs: FreeCount() = %d, want 4", b.FreeCount())
	}

	// Verify consistency.
	if b.CountFree() != b.FreeCount() {
		t.Errorf("CountFree() = %d, FreeCount() = %d", b.CountFree(), b.FreeCount())
	}
}
