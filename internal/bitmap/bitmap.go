// Package bitmap implements an allocation bitmap with a two-level summary
// for fast free-page discovery. It operates on a []byte slice (the mmap
// bitmap region) with no I/O or OS dependencies. The mmap data is treated
// as read-only during transactions; mutations are tracked in overlay maps.
package bitmap

import (
	"encoding/binary"
	"math/bits"
)

var le = binary.LittleEndian

// Bitmap manages a flat bitfield (one bit per database page) with an
// in-memory summary for accelerated allocation scans.
//
// A set bit means the page is free. A clear bit means in-use or retired.
//
// The underlying []byte data (from the mmap) is read-only during
// transactions. Mutations are tracked in pendingAllocs/pendingFrees maps
// and applied to the on-disk data at commit time via DirtyPages.
type Bitmap struct {
	data []byte // mmap bitmap region (read-only during tx)

	// summary is the level-1 acceleration array. One bit per uint64 word
	// of the detail level. A summary bit is set if the corresponding
	// 64-page detail word has any free pages (considering the overlay).
	summary []uint64

	totalPages    uint64 // total pages in the database
	totalWords    uint64 // ceil(totalPages / 64)
	reservedPages uint64 // 2 + bitmapPages (always clear)

	// Pending overlay: tracked per page ID and per word mask.
	pendingAllocs map[uint64]struct{} // pages allocated this tx
	pendingFrees  map[uint64]struct{} // pages freed this tx
	allocMasks    map[uint64]uint64   // per-word clear masks
	freeMasks     map[uint64]uint64   // per-word set masks

	hint      uint64 // detail word index for LIFO scan start
	freeCount uint64 // logical free count (on-disk + overlay)
}

// New creates a Bitmap over the given contiguous mmap bitmap data.
// totalPages is the total number of pages in the database (MaxSize / PageSize).
// reservedPages is the number of permanently reserved pages (2 + bitmapPages)
// whose bits are always clear. The summary is rebuilt from the on-disk data.
func New(data []byte, totalPages, reservedPages uint64) *Bitmap {
	totalWords := (totalPages + 63) / 64
	summaryWords := (totalWords + 63) / 64

	b := &Bitmap{
		data:          data,
		summary:       make([]uint64, summaryWords),
		totalPages:    totalPages,
		totalWords:    totalWords,
		reservedPages: reservedPages,
		pendingAllocs: make(map[uint64]struct{}),
		pendingFrees:  make(map[uint64]struct{}),
		allocMasks:    make(map[uint64]uint64),
		freeMasks:     make(map[uint64]uint64),
	}
	b.rebuildSummary()
	return b
}

// Reset clears all pending state for a new write transaction.
// Rebuilds the summary and freeCount from the on-disk data.
func (b *Bitmap) Reset() {
	clear(b.pendingAllocs)
	clear(b.pendingFrees)
	clear(b.allocMasks)
	clear(b.freeMasks)
	b.rebuildSummary()
}

// SetHint sets the LIFO allocation scan start to the word containing pageID.
func (b *Bitmap) SetHint(pageID uint64) {
	b.hint = pageID / 64
}

// Hint returns the current hint as a page ID (first page in the hint word).
func (b *Bitmap) Hint() uint64 {
	return b.hint * 64
}

// FreeCount returns the cached logical number of free pages.
func (b *Bitmap) FreeCount() uint64 {
	return b.freeCount
}

// TotalPages returns the total number of pages in the database.
func (b *Bitmap) TotalPages() uint64 {
	return b.totalPages
}

// PendingAllocs returns the set of pages allocated in this transaction.
// The caller must not modify the returned map.
func (b *Bitmap) PendingAllocs() map[uint64]struct{} {
	return b.pendingAllocs
}

// PendingFrees returns the set of pages freed in this transaction.
// The caller must not modify the returned map.
func (b *Bitmap) PendingFrees() map[uint64]struct{} {
	return b.pendingFrees
}

// word reads the i-th uint64 word from the on-disk bitmap data.
func (b *Bitmap) word(i uint64) uint64 {
	off := i * 8
	if off+8 > uint64(len(b.data)) {
		return 0
	}
	return le.Uint64(b.data[off:])
}

// logicalWord returns the effective value of detail word i after applying
// the pending overlay and boundary masks. This is O(1) via the per-word
// mask caches.
func (b *Bitmap) logicalWord(i uint64) uint64 {
	w := b.word(i)
	if mask, ok := b.allocMasks[i]; ok {
		w &^= mask
	}
	if mask, ok := b.freeMasks[i]; ok {
		w |= mask
	}
	return b.applyBoundaryMask(i, w)
}

// applyBoundaryMask clears bits that must never appear free: reserved
// pages (meta + bitmap) and bits beyond totalPages in the last word.
func (b *Bitmap) applyBoundaryMask(wordIdx, w uint64) uint64 {
	// Mask out reserved page bits.
	if wordIdx*64 < b.reservedPages {
		endBit := b.reservedPages - wordIdx*64
		if endBit >= 64 {
			return 0
		}
		w &^= (1 << endBit) - 1
	}
	// Mask out bits beyond totalPages in the last word.
	if wordIdx == b.totalWords-1 && b.totalPages%64 != 0 {
		w &= (1 << (b.totalPages % 64)) - 1
	}
	return w
}

// rebuildSummary scans all detail words and rebuilds the summary array
// and freeCount from the on-disk bitmap data (ignoring pending changes).
func (b *Bitmap) rebuildSummary() {
	b.freeCount = 0
	for i := range b.summary {
		b.summary[i] = 0
	}
	for i := uint64(0); i < b.totalWords; i++ {
		w := b.applyBoundaryMask(i, b.word(i))
		b.freeCount += uint64(bits.OnesCount64(w))
		if w != 0 {
			si := i / 64
			bit := i % 64
			b.summary[si] |= 1 << bit
		}
	}
}

// updateSummaryBit recomputes the summary bit for the given detail word index.
func (b *Bitmap) updateSummaryBit(wordIdx uint64) {
	si := wordIdx / 64
	bit := wordIdx % 64
	if b.logicalWord(wordIdx) != 0 {
		b.summary[si] |= 1 << bit
	} else {
		b.summary[si] &^= 1 << bit
	}
}
