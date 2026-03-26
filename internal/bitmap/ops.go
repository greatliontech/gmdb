package bitmap

import "math/bits"

// Set marks pageID as free (sets the bit in the overlay). Updates the
// summary and freeCount. Panics if pageID < reservedPages or >= totalPages.
func (b *Bitmap) Set(pageID uint64) {
	if pageID < b.reservedPages || pageID >= b.totalPages {
		panic("bitmap: Set on reserved or out-of-range page")
	}

	// Check current logical state before mutation.
	wasFree := b.isSet(pageID)

	wordIdx := pageID / 64
	bitPos := uint(pageID % 64)
	mask := uint64(1) << bitPos

	// Remove from pendingAllocs if present.
	delete(b.pendingAllocs, pageID)
	if m, ok := b.allocMasks[wordIdx]; ok {
		m &^= mask
		if m == 0 {
			delete(b.allocMasks, wordIdx)
		} else {
			b.allocMasks[wordIdx] = m
		}
	}

	// Add to pendingFrees.
	b.pendingFrees[pageID] = struct{}{}
	b.freeMasks[wordIdx] |= mask

	b.updateSummaryBit(wordIdx)

	if !wasFree {
		b.freeCount++
	}
}

// Clear marks pageID as allocated (clears the bit in the overlay). Updates
// the summary and freeCount. Panics if pageID < reservedPages or >= totalPages.
func (b *Bitmap) Clear(pageID uint64) {
	if pageID < b.reservedPages || pageID >= b.totalPages {
		panic("bitmap: Clear on reserved or out-of-range page")
	}

	wasFree := b.isSet(pageID)

	wordIdx := pageID / 64
	bitPos := uint(pageID % 64)
	mask := uint64(1) << bitPos

	// Remove from pendingFrees if present.
	delete(b.pendingFrees, pageID)
	if m, ok := b.freeMasks[wordIdx]; ok {
		m &^= mask
		if m == 0 {
			delete(b.freeMasks, wordIdx)
		} else {
			b.freeMasks[wordIdx] = m
		}
	}

	// Add to pendingAllocs.
	b.pendingAllocs[pageID] = struct{}{}
	b.allocMasks[wordIdx] |= mask

	b.updateSummaryBit(wordIdx)

	if wasFree {
		b.freeCount--
	}
}

// IsSet returns true if pageID is logically free (considering the overlay).
func (b *Bitmap) IsSet(pageID uint64) bool {
	if pageID >= b.totalPages {
		return false
	}
	return b.isSet(pageID)
}

// isSet is the internal implementation without bounds check.
func (b *Bitmap) isSet(pageID uint64) bool {
	wordIdx := pageID / 64
	bitPos := uint(pageID % 64)
	return b.logicalWord(wordIdx)&(1<<bitPos) != 0
}

// CountFree performs a full popcount scan over all logical words.
// Use for verification; FreeCount() returns the cached value.
func (b *Bitmap) CountFree() uint64 {
	var count uint64
	for i := uint64(0); i < b.totalWords; i++ {
		count += uint64(bits.OnesCount64(b.logicalWord(i)))
	}
	return count
}
