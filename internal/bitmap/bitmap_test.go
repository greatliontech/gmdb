package bitmap

import (
	"encoding/binary"
	"testing"
)

// makeBitmapData creates a zeroed bitmap data slice for totalPages pages.
func makeBitmapData(totalPages uint64) []byte {
	totalWords := (totalPages + 63) / 64
	return make([]byte, totalWords*8)
}

// setBitInData sets a bit directly in the on-disk data (simulating mmap state).
func setBitInData(data []byte, pageID uint64) {
	wordIdx := pageID / 64
	bitPos := uint(pageID % 64)
	off := wordIdx * 8
	w := binary.LittleEndian.Uint64(data[off:])
	w |= 1 << bitPos
	binary.LittleEndian.PutUint64(data[off:], w)
}

func TestNewBitmap(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10) // 10 reserved pages

	if b.totalPages != 256 {
		t.Errorf("TotalPages() = %d, want 256", b.totalPages)
	}
	if b.FreeCount() != 0 {
		t.Errorf("FreeCount() = %d, want 0", b.FreeCount())
	}
	if b.hint * 64 != 0 {
		t.Errorf("Hint() = %d, want 0", b.hint * 64)
	}
}

func TestNewBitmapWithFreePages(t *testing.T) {
	data := makeBitmapData(256)
	// Set pages 10, 11, 12 as free in on-disk data.
	setBitInData(data, 10)
	setBitInData(data, 11)
	setBitInData(data, 12)

	b := New(data, 256, 10)
	if b.FreeCount() != 3 {
		t.Errorf("FreeCount() = %d, want 3", b.FreeCount())
	}
}

func TestSetHint(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10)

	b.SetHint(130) // word 2 (130/64 = 2)
	if b.hint * 64 != 128 { // word 2 * 64 = 128
		t.Errorf("Hint() = %d, want 128", b.hint * 64)
	}
}

func TestReset(t *testing.T) {
	data := makeBitmapData(256)
	setBitInData(data, 20)
	b := New(data, 256, 10)

	// Allocate page 20 to create pending state.
	b.Clear(20)
	if len(b.PendingAllocs()) != 1 {
		t.Fatalf("PendingAllocs len = %d, want 1", len(b.PendingAllocs()))
	}

	b.Reset()
	if len(b.PendingAllocs()) != 0 {
		t.Errorf("after Reset: PendingAllocs len = %d, want 0", len(b.PendingAllocs()))
	}
	if len(b.PendingFrees()) != 0 {
		t.Errorf("after Reset: PendingFrees len = %d, want 0", len(b.PendingFrees()))
	}
	// freeCount should be rebuilt from on-disk (page 20 is set in data).
	if b.FreeCount() != 1 {
		t.Errorf("after Reset: FreeCount() = %d, want 1", b.FreeCount())
	}
}

func TestSummaryRebuilt(t *testing.T) {
	data := makeBitmapData(256)
	// Set page 100 free (word 1, since 100/64 = 1).
	setBitInData(data, 100)

	b := New(data, 256, 10)

	// Summary word 0 should have bit 1 set (for detail word 1).
	if b.summary[0]&(1<<1) == 0 {
		t.Error("summary bit for detail word 1 not set")
	}
	// Summary bit for detail word 0 should be clear (no free pages in word 0).
	if b.summary[0]&(1<<0) != 0 {
		t.Error("summary bit for detail word 0 should be clear")
	}
}

func TestPartialLastWord(t *testing.T) {
	// 100 pages: totalWords = 2 (words 0 and 1). Word 1 covers pages 64-99.
	// Bits 100-127 in word 1 are beyond totalPages and should not count.
	data := makeBitmapData(100)
	// Set all bits in word 1 (including bits beyond totalPages).
	for i := range 8 {
		data[8+i] = 0xFF
	}

	b := New(data, 100, 10)
	// Only pages 64-99 = 36 pages should be counted, not 64.
	// But pages 64-99 includes reserved pages? Reserved is 10, so pages 10-99 are valid.
	// All 36 pages in word 1 (64-99) are free.
	if b.FreeCount() != 36 {
		t.Errorf("FreeCount() = %d, want 36 (partial last word)", b.FreeCount())
	}
}

func TestLogicalWordOverlay(t *testing.T) {
	data := makeBitmapData(256)
	setBitInData(data, 20) // page 20 free on disk

	b := New(data, 256, 10)

	// On-disk: page 20 is free.
	if !b.IsSet(20) {
		t.Error("page 20 should be free")
	}

	// Clear page 20 (allocate).
	b.Clear(20)
	if b.IsSet(20) {
		t.Error("page 20 should be allocated after Clear")
	}

	// Set page 30 (free it, was not free on disk).
	b.Set(30)
	if !b.IsSet(30) {
		t.Error("page 30 should be free after Set")
	}
}

func TestIsSetOutOfRange(t *testing.T) {
	data := makeBitmapData(100)
	b := New(data, 100, 10)

	if b.IsSet(100) {
		t.Error("IsSet(100) should be false for totalPages=100")
	}
	if b.IsSet(1000) {
		t.Error("IsSet(1000) should be false")
	}
}
