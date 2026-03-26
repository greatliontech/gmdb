package bitmap

import (
	"cmp"
	"slices"
)

// DirtyPage represents a modified bitmap page ready for pwrite.
type DirtyPage struct {
	// PageIndex is the 0-based bitmap page index (database page = 2 + PageIndex).
	PageIndex uint32
	// Data is a page-sized buffer with pending changes applied.
	Data []byte
}

// DirtyPages returns the bitmap pages modified by pending allocs and frees.
// Each DirtyPage contains a fresh []byte with on-disk content plus pending
// changes applied. Sorted by PageIndex for sequential I/O.
func (b *Bitmap) DirtyPages(pageSize uint32) []DirtyPage {
	wordsPerPage := uint64(pageSize) / 8
	dirtySet := make(map[uint32]struct{})

	for wordIdx := range b.allocMasks {
		dirtySet[uint32(wordIdx/wordsPerPage)] = struct{}{}
	}
	for wordIdx := range b.freeMasks {
		dirtySet[uint32(wordIdx/wordsPerPage)] = struct{}{}
	}

	result := make([]DirtyPage, 0, len(dirtySet))
	for pageIdx := range dirtySet {
		buf := make([]byte, pageSize)
		b.applyToPage(buf, pageIdx, pageSize)
		result = append(result, DirtyPage{PageIndex: pageIdx, Data: buf})
	}

	slices.SortFunc(result, func(a, b DirtyPage) int {
		return cmp.Compare(a.PageIndex, b.PageIndex)
	})
	return result
}

// applyToPage copies the on-disk bitmap page content into dst and applies
// all pending changes that affect this page. dst must be pageSize bytes.
func (b *Bitmap) applyToPage(dst []byte, bitmapPageIndex uint32, pageSize uint32) {
	srcOff := uint64(bitmapPageIndex) * uint64(pageSize)
	srcEnd := srcOff + uint64(pageSize)
	if srcEnd > uint64(len(b.data)) {
		// Partial page at the end — copy what exists, zero the rest.
		n := copy(dst, b.data[srcOff:])
		clear(dst[n:])
	} else {
		copy(dst, b.data[srcOff:srcEnd])
	}

	wordsPerPage := uint64(pageSize) / 8
	wordBase := uint64(bitmapPageIndex) * wordsPerPage

	for wi, mask := range b.allocMasks {
		if wi >= wordBase && wi < wordBase+wordsPerPage {
			off := (wi - wordBase) * 8
			w := le.Uint64(dst[off:])
			w &^= mask
			le.PutUint64(dst[off:], w)
		}
	}

	for wi, mask := range b.freeMasks {
		if wi >= wordBase && wi < wordBase+wordsPerPage {
			off := (wi - wordBase) * 8
			w := le.Uint64(dst[off:])
			w |= mask
			le.PutUint64(dst[off:], w)
		}
	}
}
