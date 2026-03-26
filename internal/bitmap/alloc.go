package bitmap

import "math/bits"

// FindFirstFree finds a single free page starting from the LIFO hint.
// Scans summary words to skip empty regions, then detail words within
// non-empty regions. Wraps around if needed. The page is automatically
// Clear'd (added to pendingAllocs). Returns (pageID, true) or (0, false).
func (b *Bitmap) FindFirstFree() (uint64, bool) {
	if b.freeCount == 0 {
		return 0, false
	}

	// First pass: hint to end.
	if pageID, ok := b.scanForFree(b.hint, b.totalWords); ok {
		return pageID, true
	}
	// Second pass: 0 to hint.
	return b.scanForFree(0, b.hint)
}

// scanForFree scans detail words [from, to) for the first set bit,
// using the summary to skip empty 64-word groups. Clears the found bit.
func (b *Bitmap) scanForFree(from, to uint64) (uint64, bool) {
	di := from
	for di < to {
		si := di / 64
		sBit := di % 64

		// Read summary word and mask out bits below our starting position.
		sw := b.summary[si] & ^((1 << sBit) - 1)
		if sw == 0 {
			di = (si + 1) * 64
			continue
		}

		// Jump to the first set bit in the (masked) summary word.
		nextBit := uint64(bits.TrailingZeros64(sw))
		di = si*64 + nextBit
		if di >= to {
			break
		}

		dw := b.logicalWord(di)
		// logicalWord already masks out reserved pages and bits
		// beyond totalPages, so any set bit is a valid free page.
		if dw != 0 {
			bitPos := uint64(bits.TrailingZeros64(dw))
			pageID := di*64 + bitPos
			b.Clear(pageID)
			b.hint = di
			return pageID, true
		}
		di++
	}
	return 0, false
}

// FindContiguous finds and allocates a contiguous run of n free pages.
// Returns (startPageID, true) or (0, false). All n pages are Clear'd.
func (b *Bitmap) FindContiguous(n int) (uint64, bool) {
	if n <= 0 {
		return 0, false
	}
	if n == 1 {
		return b.FindFirstFree()
	}
	if uint64(n) > b.freeCount {
		return 0, false
	}

	// First pass: hint to end.
	if pageID, ok := b.findRun(b.hint, b.totalWords, n); ok {
		return pageID, true
	}
	// Second pass: 0 to hint (a run can't wrap around, but may start before hint).
	return b.findRun(0, b.hint+uint64((n+63)/64), n)
}

// findRun scans detail words [from, to) for a contiguous run of n set bits.
// Uses carry-forward of trailing ones across word boundaries.
func (b *Bitmap) findRun(from, to uint64, n int) (uint64, bool) {
	if to > b.totalWords {
		to = b.totalWords
	}

	run := 0          // current consecutive free count
	runStart := uint64(0)

	for di := from; di < to; di++ {
		dw := b.logicalWord(di)
		if dw == 0 {
			run = 0
			continue
		}

		base := di * 64

		if dw == ^uint64(0) {
			// All 64 bits free — extend run.
			if run == 0 {
				runStart = base
			}
			run += 64
			if run >= n {
				return b.allocRun(runStart, n), true
			}
			continue
		}

		// Partial word. Check leading ones (from LSB) to extend carry-forward.
		leading := bits.TrailingZeros64(^dw)
		if run > 0 && leading > 0 {
			run += leading
			if run >= n {
				return b.allocRun(runStart, n), true
			}
		}

		// Scan internal runs within this word.
		if pageID, ok := b.scanWordRuns(dw, base, n); ok {
			return pageID, true
		}

		// Trailing ones (from MSB) carry forward to next word.
		trailing := bits.LeadingZeros64(^dw) // leading zeros of complement = trailing ones
		if trailing > 0 {
			run = trailing
			runStart = base + uint64(64-trailing)
		} else {
			run = 0
		}
	}
	return 0, false
}

// scanWordRuns searches for a contiguous run of n set bits entirely within
// a single word. Returns (startPageID, true) if found.
func (b *Bitmap) scanWordRuns(w uint64, base uint64, n int) (uint64, bool) {
	// Skip leading ones (already handled by carry-forward).
	// Find each gap (0-bit), skip it, measure the next run of 1s.
	remaining := w
	pos := 0

	for remaining != 0 {
		// Skip zeros.
		zeros := bits.TrailingZeros64(remaining)
		remaining >>= zeros
		pos += zeros

		// Count ones.
		if remaining == 0 {
			break
		}
		ones := bits.TrailingZeros64(^remaining)
		if ones >= n {
			return b.allocRun(base+uint64(pos), n), true
		}
		if ones >= 64-pos {
			break // rest of word is all ones, handled by trailing carry
		}
		remaining >>= ones
		pos += ones
	}
	return 0, false
}

// allocRun clears n consecutive pages starting at startPageID.
func (b *Bitmap) allocRun(startPageID uint64, n int) uint64 {
	for i := range n {
		b.Clear(startPageID + uint64(i))
	}
	b.hint = (startPageID + uint64(n)) / 64
	return startPageID
}
