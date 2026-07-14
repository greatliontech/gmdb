package page

// LeafIter is the bidirectional iterator the btree cursor delegates leaf
// traversal to. It owns all per-leaf decode state — keyBuf, group cache,
// stream position — so the cursor stack stays slim and per-variant
// machinery stays inside the page package.
//
// Two modes per page-formats.md §Cursor Iteration:
//
//   - Forward-streaming (initial state on compressed leaves, interleaved
//     and segregated alike). Maintains keyBuf carrying the current full
//     key. Next() reads the next delta entry, truncates keyBuf to
//     SharedLen, appends UnsharedKey — O(1) amortized.
//   - Buffered (entered on first Prev / At on a compressed leaf).
//     Decodes the containing restart group into bufEnts + bufKeys; all
//     subsequent Next/Prev/At serve from the buffer; group-boundary
//     crossings reload the adjacent group.
//
// Uncompressed leaves don't need the streaming/buffered distinction —
// Next/Prev/At are all O(1) via the positional offset table.
//
// Buffer ownership: keyBuf, bufKeys, bufEnts are caller-supplied scratch
// slices (passed in via IterForReuse / IterAtForReuse / SearchLeafIter)
// and returned via KeyBuf() / BufKeys() / BufEnts(). The cursor reclaims
// them across leaf transitions so per-cursor allocation amortizes to
// zero in the steady-state loop.
type LeafIter struct {
	r        LeafReader
	idx      int   // index of the next entry Next() will return
	startIdx int   // lower bound (inclusive) for Prev / At
	endIdx   int   // upper bound (exclusive) for Next
	off      int   // current byte offset (compressed forward-streaming only)
	variant  uint8 // TypeLeaf | TypeLeafSegregated | TypeLeafUncompressed

	// Compressed forward-streaming fields (interleaved + segregated).
	prevKey     []byte // the previous entry's full key (alias keyBuf for delta entries)
	keyBuf      []byte // delta reconstruction buffer
	nextRestart int    // absolute idx of the next restart entry
	groupIdx    int    // index of the group nextRestart belongs to

	// Segregated only: the value-span end of the LAST entry in this
	// iter's range — ValueEnd for whole-leaf iters; the next group's
	// restart VOff for group-scoped iters (loadBufferedGroup).
	segEndVOff int

	// Compressed buffered-mode fields (populated on first Prev/At).
	buffered bool
	bufStart int         // first absolute idx in bufEnts
	bufEnd   int         // one-past-last absolute idx in bufEnts
	bufGroup int         // group index whose entries are buffered
	bufKeys  []byte      // bump-allocated backing for bufEnts[i].Key
	bufEnts  []LeafEntry // decoded entries for the buffered group
}

// IterForReuse returns an iterator over all entries in the leaf using the
// supplied scratch buffers for delta-key reconstruction (keyBuf) and
// group buffering on Prev (bufEnts, bufKeys). The buffers are returned
// from the iter via KeyBuf / BufKeys / BufEnts so the caller can reclaim
// them on the next iter construction.
func (r LeafReader) IterForReuse(keyBuf, bufKeys []byte, bufEnts []LeafEntry) LeafIter {
	it := LeafIter{
		r:       r,
		endIdx:  r.count,
		variant: r.variant,
		keyBuf:  keyBuf,
		bufKeys: bufKeys[:0],
		bufEnts: bufEnts[:0],
	}
	switch r.variant {
	case TypeLeaf:
		it.off = leafEntryStart
	case TypeLeafSegregated:
		it.off = segLeafEntryStart
		it.segEndVOff = r.valueEnd
	}
	return it
}

// IterAtForReuse returns an iterator positioned so the next Next() call
// returns the entry at startIdx. For uncompressed pages this is O(1)
// (offset table lookup). For compressed pages it's O(K) (walks the
// containing restart group from its restart point to build delta state).
//
// startIdx == count is a valid "past end" setup: Next() will fail and
// At(k) for k<count will populate on demand. Used by the cursor's Last()
// to initialize a buffered-mode iter for backward traversal.
func (r LeafReader) IterAtForReuse(startIdx int, keyBuf, bufKeys []byte, bufEnts []LeafEntry) LeafIter {
	if startIdx >= r.count {
		return LeafIter{
			r:          r,
			idx:        r.count,
			endIdx:     r.count,
			variant:    r.variant,
			segEndVOff: r.valueEnd,
			keyBuf:     keyBuf,
			bufKeys:    bufKeys[:0],
			bufEnts:    bufEnts[:0],
		}
	}
	if r.uc() {
		// Position is idx alone: uncompressed Next/Prev/At all resolve
		// their byte offset from the positional offset table, so there
		// is no stream state to seed.
		return LeafIter{
			r:       r,
			idx:     startIdx,
			endIdx:  r.count,
			variant: r.variant,
			keyBuf:  keyBuf,
			bufKeys: bufKeys[:0],
			bufEnts: bufEnts[:0],
		}
	}

	// Compressed (interleaved / segregated): find the group containing
	// startIdx, walk from its restart point to build keyBuf state up to
	// (but not including) startIdx, so Next() returns the entry at
	// startIdx.
	groupIdx := 0
	accum := 0
	for g := 0; g < r.rt.RestartCount(); g++ {
		gc := r.rt.GroupEntryCount(g)
		if accum+gc > startIdx {
			groupIdx = g
			break
		}
		accum += gc
	}
	it := LeafIter{
		r:           r,
		idx:         accum,
		endIdx:      r.count,
		off:         r.rt.Offset(groupIdx),
		variant:     r.variant,
		segEndVOff:  r.valueEnd,
		keyBuf:      keyBuf,
		nextRestart: accum,
		groupIdx:    groupIdx,
		bufKeys:     bufKeys[:0],
		bufEnts:     bufEnts[:0],
	}
	for it.idx < startIdx {
		it.Next()
	}
	return it
}

// groupIter returns an iterator scoped to a single restart group on a
// compressed leaf (interleaved or segregated). Panics on uncompressed
// pages — the abstraction is undefined there. Package-private: only
// loadBufferedGroup calls it. The bounds (startIdx and endIdx) make
// At() reject out-of-group access, preserving the "scoped to one group"
// contract — callers that want a whole-leaf iter use IterAtForReuse.
func (r LeafReader) groupIter(groupIdx int, keyBuf []byte) LeafIter {
	if r.uc() {
		panic("page: groupIter called on uncompressed leaf")
	}
	startIdx := r.rt.GroupStartIndex(groupIdx)
	gc := r.rt.GroupEntryCount(groupIdx)
	it := LeafIter{
		r:           r,
		idx:         startIdx,
		startIdx:    startIdx,
		endIdx:      startIdx + gc,
		off:         r.rt.Offset(groupIdx),
		variant:     r.variant,
		keyBuf:      keyBuf,
		nextRestart: startIdx,
		groupIdx:    groupIdx,
	}
	if r.seg() {
		// The group's last entry's value span ends at the NEXT group's
		// restart VOff (value spans are contiguous in entry order), or
		// at ValueEnd when this is the page's last group.
		if groupIdx+1 < r.rt.RestartCount() {
			it.segEndVOff = segReadVOff(r.buf, r.rt.Offset(groupIdx+1), true)
		} else {
			it.segEndVOff = r.valueEnd
		}
	}
	return it
}

// Next decodes the next entry and advances. Returns (LeafEntry{}, false)
// at end-of-iteration. The returned entry.Key aliases either the page
// buffer (restart entries / uncompressed) or the iterator's keyBuf
// (compressed deltas) or bufKeys (buffered mode); valid until the next
// iterator move.
func (it *LeafIter) Next() (LeafEntry, bool) {
	if it.idx >= it.endIdx {
		return LeafEntry{}, false
	}
	if it.variant == TypeLeafUncompressed {
		// Table-driven (page-formats.md §Cursor Iteration): idx is the
		// single source of position, so Prev/At repositioning can never
		// desynchronize a separately-tracked stream offset.
		e, _ := it.r.decodeFullKeyEntry(it.r.ucOffset(it.idx))
		it.idx++
		return e, true
	}
	if it.buffered {
		if it.idx >= it.bufEnd {
			it.loadBufferedGroup(it.bufGroup + 1)
		}
		e := it.bufEnts[it.idx-it.bufStart]
		it.idx++
		return e, true
	}
	if it.variant == TypeLeafSegregated {
		var e LeafEntry
		var voff int
		if it.idx == it.nextRestart {
			var key []byte
			e.Flags, key, voff, it.off = it.r.decodeSegRestart(it.off)
			e.Key = key
			it.prevKey = key
			it.nextRestart += it.r.rt.GroupEntryCount(it.groupIdx)
			it.groupIdx++
		} else {
			e.Flags, e.Key, voff, it.off, it.keyBuf = it.r.decodeSegDelta(it.off, it.prevKey, it.keyBuf)
			it.prevKey = it.keyBuf
		}
		// Value span end: the next entry's VOff (restart iff it opens
		// the next group), or the iter-range bound for the last entry.
		vend := it.segEndVOff
		if it.idx+1 < it.endIdx {
			vend = segReadVOff(it.r.buf, it.off, it.idx+1 == it.nextRestart)
		}
		it.r.segValueHalf(&e, voff, vend)
		it.idx++
		return e, true
	}
	var e LeafEntry
	if it.idx == it.nextRestart {
		e, it.off = it.r.decodeFullKeyEntry(it.off)
		it.prevKey = e.Key
		it.nextRestart += it.r.rt.GroupEntryCount(it.groupIdx)
		it.groupIdx++
	} else {
		e, it.off, it.keyBuf = it.r.decodeDeltaEntry(it.off, it.prevKey, it.keyBuf)
		it.prevKey = it.keyBuf
	}
	it.idx++
	return e, true
}

// At returns the entry at absolute index idx. Triggers buffered mode on
// the first call on a compressed page (and on every group-boundary
// crossing thereafter). For uncompressed pages At is O(1) via the
// offset table with no mode transition. After At, the iter's stream
// position is set to idx+1, so a subsequent Next returns idx+1.
func (it *LeafIter) At(idx int) (LeafEntry, bool) {
	if idx < it.startIdx || idx >= it.endIdx {
		return LeafEntry{}, false
	}
	if it.variant == TypeLeafUncompressed {
		e, _ := it.r.decodeFullKeyEntry(it.r.ucOffset(idx))
		it.idx = idx + 1
		return e, true
	}
	if !it.buffered || idx < it.bufStart || idx >= it.bufEnd {
		var target int
		switch {
		case !it.buffered:
			target = it.r.rt.FindGroupContaining(idx)
		case idx+1 == it.bufStart:
			target = it.bufGroup - 1
		case idx == it.bufEnd:
			target = it.bufGroup + 1
		default:
			target = it.r.rt.FindGroupContaining(idx)
		}
		it.loadBufferedGroup(target)
	}
	it.idx = idx + 1
	return it.bufEnts[idx-it.bufStart], true
}

// Prev returns the entry preceding the current Next-position. With
// `it.idx == N+1` (one past the last entry Next returned), Prev returns
// the entry at index `N-1` and sets `it.idx = N`. Two consecutive Prevs
// walk strictly backward by one entry each:
//
//	Next() → entry N  (it.idx = N+1)
//	Prev() → entry N-1 (it.idx = N)
//	Prev() → entry N-2 (it.idx = N-1)
//
// In a Prev-Next-Prev alternation, Prev re-issues *its own* prior
// return (not Next's prior return): after `Prev() → N-1`, `Next()`
// returns the entry at the new position — `Next() → N` (it.idx is
// N) — and a following `Prev()` yields `N-1` again.
//
// Backwards iteration is buffered on compressed pages — first Prev
// transitions the iter to buffered mode for the containing group.
//
// Cursor semantic: caller uses Prev like the inverse of Next; gmdb's
// Cursor.Prev wrapper handles the state-machine transitions
// (Unpositioned → Last, End-of-iteration → End sticky, etc.).
func (it *LeafIter) Prev() (LeafEntry, bool) {
	prevIdx := it.idx - 1
	if prevIdx <= it.startIdx-1 {
		return LeafEntry{}, false
	}
	// We want to return the entry at prevIdx-1 (the one BEFORE the next
	// Next would have returned). Special case prevIdx == 0 means the
	// iter would be at the very first entry — there's no Prev.
	target := prevIdx - 1
	if target < it.startIdx {
		// At the first entry; no prev available.
		return LeafEntry{}, false
	}
	if it.variant == TypeLeafUncompressed {
		e, _ := it.r.decodeFullKeyEntry(it.r.ucOffset(target))
		it.idx = target + 1
		return e, true
	}
	if !it.buffered || target < it.bufStart || target >= it.bufEnd {
		var groupTarget int
		switch {
		case !it.buffered:
			groupTarget = it.r.rt.FindGroupContaining(target)
		case target+1 == it.bufStart:
			groupTarget = it.bufGroup - 1
		default:
			groupTarget = it.r.rt.FindGroupContaining(target)
		}
		it.loadBufferedGroup(groupTarget)
	}
	it.idx = target + 1
	return it.bufEnts[target-it.bufStart], true
}

// loadBufferedGroup decodes all entries of the given restart group into
// bufEnts; keys are copied into bufKeys so the buffered entries remain
// stable across subsequent iter calls (the page buffer's borrowed slices
// for delta entries would be clobbered by re-using keyBuf). Buffered
// VALUES keep borrowing the page buffer directly — inline values live in
// the entry stream (interleaved) or the value region (segregated), both
// stable page bytes.
func (it *LeafIter) loadBufferedGroup(groupIdx int) {
	it.bufEnts = it.bufEnts[:0]
	it.bufKeys = it.bufKeys[:0]

	groupStart := it.r.rt.GroupStartIndex(groupIdx)
	count := it.r.rt.GroupEntryCount(groupIdx)

	gi := it.r.groupIter(groupIdx, it.keyBuf)
	for e, ok := gi.Next(); ok; e, ok = gi.Next() {
		off := len(it.bufKeys)
		it.bufKeys = append(it.bufKeys, e.Key...)
		// Re-slice into bufKeys with explicit cap so subsequent
		// growth of bufKeys can't alias this entry's view.
		e.Key = it.bufKeys[off:len(it.bufKeys):len(it.bufKeys)]
		it.bufEnts = append(it.bufEnts, e)
	}
	it.keyBuf = gi.KeyBuf()

	it.buffered = true
	it.bufGroup = groupIdx
	it.bufStart = groupStart
	it.bufEnd = groupStart + count
}

// KeyBuf returns the iterator's key reconstruction buffer for reuse by
// the next iter construction.
func (it *LeafIter) KeyBuf() []byte { return it.keyBuf }

// BufKeys returns the iterator's buffered-mode key storage for reuse.
func (it *LeafIter) BufKeys() []byte { return it.bufKeys }

// BufEnts returns the iterator's buffered-mode entry slice for reuse.
func (it *LeafIter) BufEnts() []LeafEntry { return it.bufEnts }

// Idx returns the index of the entry that the next Next() call would
// return. Used by the cursor to record its absolute position and to
// detect end-of-iteration.
func (it *LeafIter) Idx() int { return it.idx }

// Count returns the iterator's exclusive upper bound (`endIdx`) —
// the absolute index one past the last entry the iter ranges
// over. For whole-leaf iters constructed via IterForReuse /
// IterAtForReuse this equals LeafReader.Count(); for group-scoped
// iters from the package-private groupIter it equals
// `startIdx + groupEntryCount` (NOT the group's relative entry
// count). Used by the btree cursor's Last() to position at the
// last entry via At(Count()-1), since LeafIter.Prev's "step back
// from just-Nexted" semantic doesn't directly support "position
// at last entry."
func (it *LeafIter) Count() int { return it.endIdx }
