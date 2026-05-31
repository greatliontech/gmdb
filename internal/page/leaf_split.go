package page

// No-decode compressed-leaf split (page-formats.md §Leaf Split). On overflow a
// compressed leaf splits at a restart-group boundary: FindSplitGroup picks the
// boundary nearest 50% of the data bytes, and the two-phase carve
// (SplitLeafRightHalf + TruncateLeafToGroups) splits the page into two WITHOUT
// decoding/re-encoding entries — each group's bytes are retained/copied verbatim
// and only the restart-table offsets are rebased. Each
// half is therefore byte-identical to a LeafBuilder rebuild of its entry subset
// (we split at a builder-produced group boundary), so the split is canonical and
// deterministic. The entry-precise byte-balanced split (btree.findLeafSplitIndex)
// remains the fallback for size-skewed insert-overflow sets that no group
// boundary can balance to fit (page-formats.md §Leaf Split + the byte-balanced
// split contract).

// FindSplitGroup returns the restart-group index in [1, RestartCount) at which
// to split a compressed leaf: the boundary whose left-side data span is closest
// to 50% of the leaf's data bytes (page-formats.md §Leaf Split "typically 50% of
// data bytes"; tiebreak — the lower index wins). The caller must ensure buf is a
// compressed leaf with RestartCount > 1. A pure function of the page bytes, so
// the choice is deterministic (the determinism invariant).
func FindSplitGroup(buf []byte, cfg Config) int {
	rc := int(le.Uint16(buf[leafOffRestartCount:]))
	dataEnd := int(le.Uint16(buf[leafOffDataEnd:]))
	restartTableOff := cfg.ContentEnd() - rc*restartTableEntrySize

	dataStart := leafEntryStart
	target := (dataEnd - dataStart) / 2 // 50% bias (matches findLeafSplitIndex)

	best, bestDist := 1, dataEnd-dataStart+1
	for g := 1; g < rc; g++ {
		leftBytes := int(le.Uint16(buf[restartTableOff+g*restartTableEntrySize:])) - dataStart
		dist := leftBytes - target
		if dist < 0 {
			dist = -dist
		}
		if dist < bestDist { // strict < ⇒ the lower index wins ties
			bestDist = dist
			best = g
		}
	}
	return best
}

// The compressed group split is two-phase so the caller can mutate the source
// page only after the split is committed: SplitLeafRightHalf is READ-ONLY on src
// (it fills dst with the right half), and TruncateLeafToGroups mutates src into
// the left half. trySplitLeafByGroup builds the right half, appends the new entry
// to it, and only on success truncates the left — so a decline (the right half
// can't absorb the appended entry) leaves src untouched for the decode-split
// fallback, which reads that same buffer. Folding both phases into one would
// truncate src before the decline is known and corrupt the fallback's input.
//
// Each half is byte-identical to a LeafBuilder rebuild of its entry subset (we
// split at a builder-produced group boundary; entry bytes are retained/copied
// verbatim, only restart-table offsets are rebased). Both halves keep free space
// [DataEnd, restart-table-start) zeroed (the carve bypasses LeafBuilder.Finish,
// and src is a reused buffer carrying stale tail bytes), so the splice helpers'
// free-space-zeroed invariant holds.

// SplitLeafRightHalf copies groups [splitGroup, rc) of the compressed leaf src
// into dst as a standalone right-half leaf (offsets rebased to leafEntryStart),
// READ-ONLY on src. Returns the two halves' entry counts. dst must be a distinct
// page-sized buffer (a split supplies a fresh, zeroed page).
func SplitLeafRightHalf(src, dst []byte, cfg Config, splitGroup int) (leftCount, rightCount int) {
	_, _, count, _ := ReadHeader(src)
	rc := int(le.Uint16(src[leafOffRestartCount:]))
	contentEnd := cfg.ContentEnd()
	srcDataEnd := int(le.Uint16(src[leafOffDataEnd:]))
	srcRestartTableOff := contentEnd - rc*restartTableEntrySize

	for g := range splitGroup {
		leftCount += int(src[srcRestartTableOff+g*restartTableEntrySize+2])
	}
	rightCount = int(count) - leftCount

	rightRC := rc - splitGroup
	srcGroupStart := int(le.Uint16(src[srcRestartTableOff+splitGroup*restartTableEntrySize:]))
	rightDataLen := srcDataEnd - srcGroupStart
	copy(dst[leafEntryStart:leafEntryStart+rightDataLen], src[srcGroupStart:srcDataEnd])

	adjust := leafEntryStart - srcGroupStart
	rightRestartTableOff := contentEnd - rightRC*restartTableEntrySize
	for i := range rightRC {
		srcOff := srcRestartTableOff + (splitGroup+i)*restartTableEntrySize
		dstOff := rightRestartTableOff + i*restartTableEntrySize
		le.PutUint16(dst[dstOff:], uint16(int(le.Uint16(src[srcOff:]))+adjust))
		dst[dstOff+2] = src[srcOff+2] // group count
		dst[dstOff+3] = 0             // reserved
	}
	rightDataEnd := leafEntryStart + rightDataLen
	WriteHeader(dst, TypeLeaf, uint16(rightCount), 0)
	le.PutUint16(dst[leafOffRestartCount:], uint16(rightRC))
	le.PutUint16(dst[leafOffDataEnd:], uint16(rightDataEnd))
	// dst is a fresh (zeroed) buffer; clear its free region defensively so the
	// function doesn't depend on the caller's buffer state.
	clear(dst[rightDataEnd:rightRestartTableOff])

	return leftCount, rightCount
}

// TruncateLeafToGroups truncates the compressed leaf buf in place to groups
// [0, splitGroup) — the left half of a group split. The restart table shrinks
// (rc → splitGroup) and relocates toward ContentEnd; the freed region (the
// dropped right-half data + the old table tail) is zeroed so the free-space-
// zeroed invariant holds. Returns the left half's entry count.
func TruncateLeafToGroups(buf []byte, cfg Config, splitGroup int) (leftCount int) {
	rc := int(le.Uint16(buf[leafOffRestartCount:]))
	contentEnd := cfg.ContentEnd()
	srcRestartTableOff := contentEnd - rc*restartTableEntrySize

	for g := range splitGroup {
		leftCount += int(buf[srcRestartTableOff+g*restartTableEntrySize+2])
	}
	// New DataEnd = the offset of group splitGroup (start of the dropped right).
	leftDataEnd := int(le.Uint16(buf[srcRestartTableOff+splitGroup*restartTableEntrySize:]))
	leftRestartTableOff := contentEnd - splitGroup*restartTableEntrySize
	copy(buf[leftRestartTableOff:leftRestartTableOff+splitGroup*restartTableEntrySize], buf[srcRestartTableOff:srcRestartTableOff+splitGroup*restartTableEntrySize])
	WriteHeader(buf, TypeLeaf, uint16(leftCount), 0)
	le.PutUint16(buf[leafOffRestartCount:], uint16(splitGroup))
	le.PutUint16(buf[leafOffDataEnd:], uint16(leftDataEnd))
	clear(buf[leftDataEnd:leftRestartTableOff])
	return leftCount
}

// ---------------------------------------------------------------------------
// Uncompressed leaf split (entry boundary) — the uncompressed analog. An
// uncompressed leaf is a sorted, packed entry array + sorted offset table, so it
// splits at an ENTRY boundary (FindUCSplitIndex), and the two-phase byte-carve
// (SplitUCRightHalf read-only + TruncateUCToEntries mutate-on-success) moves the
// right entries' bytes verbatim + rebases the offset table — no decode/re-encode,
// each half byte-identical to a rebuild. (grove's splitUCLeafAt instead
// decode-rebuilds via LeafBuilder; this byte-carve is the actual no-decode form.)
// Same canonical + free-space-zeroed + decline-safety properties as the
// compressed group split above.
// ---------------------------------------------------------------------------

// FindUCSplitIndex returns the entry index in [1, Count) at which to split an
// uncompressed leaf: the boundary whose left-side data span is closest to 50% of
// the data bytes (page-formats.md §Leaf Split FindSplitIndex; lower-index
// tiebreak). The caller must ensure buf is an uncompressed leaf with Count > 1.
// Pure function of the page bytes → deterministic.
func FindUCSplitIndex(buf []byte, cfg Config) int {
	_, _, count16, _ := ReadHeader(buf)
	count := int(count16)
	dataEnd := int(le.Uint16(buf[ucLeafOffDataEnd:]))
	tableOff := cfg.ContentEnd() - count*ucOffsetEntrySize

	target := (dataEnd - leafEntryStart) / 2
	best, bestDist := 1, dataEnd-leafEntryStart+1
	for i := 1; i < count; i++ {
		leftBytes := int(le.Uint16(buf[tableOff+i*ucOffsetEntrySize:])) - leafEntryStart
		dist := leftBytes - target
		if dist < 0 {
			dist = -dist
		}
		if dist < bestDist { // strict < ⇒ the lower index wins ties
			bestDist = dist
			best = i
		}
	}
	return best
}

// SplitUCRightHalf copies entries [splitIdx, Count) of the uncompressed leaf src
// into dst as a standalone right-half leaf (offsets rebased to leafEntryStart),
// READ-ONLY on src. Returns the two halves' entry counts. dst must be a distinct
// page-sized buffer (a split supplies a fresh, zeroed page).
func SplitUCRightHalf(src, dst []byte, cfg Config, splitIdx int) (leftCount, rightCount int) {
	_, _, count16, _ := ReadHeader(src)
	count := int(count16)
	contentEnd := cfg.ContentEnd()
	srcDataEnd := int(le.Uint16(src[ucLeafOffDataEnd:]))
	srcTableOff := contentEnd - count*ucOffsetEntrySize

	rightCount = count - splitIdx
	rightStart := int(le.Uint16(src[srcTableOff+splitIdx*ucOffsetEntrySize:]))
	rightDataLen := srcDataEnd - rightStart
	copy(dst[leafEntryStart:leafEntryStart+rightDataLen], src[rightStart:srcDataEnd])

	adjust := leafEntryStart - rightStart
	dstTableOff := contentEnd - rightCount*ucOffsetEntrySize
	for j := range rightCount {
		srcSlot := srcTableOff + (splitIdx+j)*ucOffsetEntrySize
		le.PutUint16(dst[dstTableOff+j*ucOffsetEntrySize:], uint16(int(le.Uint16(src[srcSlot:]))+adjust))
	}
	rightDataEnd := leafEntryStart + rightDataLen
	WriteHeader(dst, TypeLeafUncompressed, uint16(rightCount), 0)
	le.PutUint16(dst[ucLeafOffDataEnd:], uint16(rightDataEnd))
	le.PutUint16(dst[ucLeafOffDataEnd+2:], 0) // reserved
	clear(dst[rightDataEnd:dstTableOff])

	return splitIdx, rightCount
}

// TruncateUCToEntries truncates the uncompressed leaf buf in place to entries
// [0, splitIdx) — the left half of an entry split. The offset table shrinks
// (count → splitIdx) and relocates toward ContentEnd; the freed region (dropped
// right-half data + old table tail) is zeroed. Returns the left entry count.
func TruncateUCToEntries(buf []byte, cfg Config, splitIdx int) (leftCount int) {
	_, _, count16, _ := ReadHeader(buf)
	count := int(count16)
	contentEnd := cfg.ContentEnd()
	srcTableOff := contentEnd - count*ucOffsetEntrySize

	// New DataEnd = the offset of entry splitIdx (start of the dropped right).
	leftDataEnd := int(le.Uint16(buf[srcTableOff+splitIdx*ucOffsetEntrySize:]))
	newTableOff := contentEnd - splitIdx*ucOffsetEntrySize
	copy(buf[newTableOff:newTableOff+splitIdx*ucOffsetEntrySize], buf[srcTableOff:srcTableOff+splitIdx*ucOffsetEntrySize])
	WriteHeader(buf, TypeLeafUncompressed, uint16(splitIdx), 0)
	le.PutUint16(buf[ucLeafOffDataEnd:], uint16(leftDataEnd))
	clear(buf[leftDataEnd:newTableOff])
	return splitIdx
}
