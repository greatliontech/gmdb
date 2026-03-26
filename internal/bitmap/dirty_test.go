package bitmap

import (
	"encoding/binary"
	"testing"
)

func TestDirtyPagesEmpty(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10)

	pages := b.DirtyPages(4096)
	if len(pages) != 0 {
		t.Errorf("DirtyPages len = %d, want 0", len(pages))
	}
}

func TestDirtyPagesSingle(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10)

	b.Set(20) // page 20 is in word 0, which is in bitmap page 0

	pages := b.DirtyPages(4096)
	if len(pages) != 1 {
		t.Fatalf("DirtyPages len = %d, want 1", len(pages))
	}
	if pages[0].PageIndex != 0 {
		t.Errorf("PageIndex = %d, want 0", pages[0].PageIndex)
	}
	if len(pages[0].Data) != 4096 {
		t.Errorf("Data len = %d, want 4096", len(pages[0].Data))
	}

	// Verify the bit is set in the dirty page data.
	w := binary.LittleEndian.Uint64(pages[0].Data[0:]) // word 0
	if w&(1<<20) == 0 {
		t.Error("bit 20 should be set in dirty page data")
	}
}

func TestDirtyPagesMultiple(t *testing.T) {
	// Use small page size to create multiple bitmap pages.
	// 64 bytes per page = 512 bits = 512 pages per bitmap page = 8 words per page.
	pageSize := uint32(64)
	data := makeBitmapData(2048)
	b := New(data, 2048, 10)

	b.Set(20)   // word 0, bitmap page 0
	b.Set(600)  // word 9 (600/64=9), bitmap page 1 (9/8=1)

	pages := b.DirtyPages(pageSize)
	if len(pages) != 2 {
		t.Fatalf("DirtyPages len = %d, want 2", len(pages))
	}
	// Should be sorted by PageIndex.
	if pages[0].PageIndex != 0 || pages[1].PageIndex != 1 {
		t.Errorf("PageIndices = [%d, %d], want [0, 1]",
			pages[0].PageIndex, pages[1].PageIndex)
	}
}

func TestDirtyPagesWithAlloc(t *testing.T) {
	data := makeBitmapData(256)
	setBitInData(data, 20) // free on disk
	b := New(data, 256, 10)

	b.Clear(20) // allocate → creates allocMask entry

	pages := b.DirtyPages(4096)
	if len(pages) != 1 {
		t.Fatalf("DirtyPages len = %d, want 1", len(pages))
	}

	// Bit 20 should be clear in the dirty page (allocated).
	w := binary.LittleEndian.Uint64(pages[0].Data[0:])
	if w&(1<<20) != 0 {
		t.Error("bit 20 should be clear in dirty page (allocated)")
	}
}

func TestApplyToPagePartialData(t *testing.T) {
	// Data is shorter than one full page. ApplyToPage should handle gracefully.
	data := make([]byte, 100) // less than a 4096-byte page
	b := New(data, 100, 4)

	b.Set(50) // page 50 in word 0

	dst := make([]byte, 4096)
	b.applyToPage(dst, 0, 4096)

	// Bit 50 should be set.
	w := binary.LittleEndian.Uint64(dst[0:])
	if w&(1<<50) == 0 {
		t.Error("bit 50 should be set in applied page")
	}
}

func TestApplyToPage(t *testing.T) {
	data := makeBitmapData(256)
	setBitInData(data, 20) // on-disk: page 20 free
	b := New(data, 256, 10)

	b.Clear(20) // allocate page 20 (pending alloc)
	b.Set(30)   // free page 30 (pending free)

	dst := make([]byte, 4096)
	b.applyToPage(dst, 0, 4096)

	// Page 20 should be clear in dst (allocated).
	w0 := binary.LittleEndian.Uint64(dst[0:])
	if w0&(1<<20) != 0 {
		t.Error("bit 20 should be clear in applied page (allocated)")
	}
	// Page 30 should be set in dst (freed).
	if w0&(1<<30) == 0 {
		t.Error("bit 30 should be set in applied page (freed)")
	}
}
