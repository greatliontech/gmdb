package page

import "bytes"

// Compressed-leaf decode + search + entry-walk helpers. Page layout per
// page-formats.md §Compressed Leaf:
//
//	+-----------------------+ offset 0
//	| Page Header (8 bytes) | Type=TypeLeaf, Count=N
//	+-----------------------+ offset 8
//	| RestartCount uint16   |
//	| DataEnd      uint16   |
//	+-----------------------+ offset 12
//	| Entry 0 (restart)     |  entries forward
//	| Entry 1 (delta)       |
//	| ...                   |
//	+-----------------------+ DataEnd
//	|       free space      |
//	+-----------------------+
//	| Restart Table         |  RestartCount × 4 bytes (Offset + Count + Reserved)
//	+-----------------------+ ContentEnd
//
// Per-entry encoding (restart vs delta, inline vs overflow) is identical
// to the entries the former EncodeLeaf produced — only the page-level
// framing (variable-group restart table, no per-page RestartInterval
// field) changed.

// decodeDeltaEntry decodes a delta entry at the given byte offset, using
// prevKey to reconstruct the full key. keyBuf is the in-place
// reconstruction buffer — truncated to SharedLen and appended with
// UnsharedKey per page-formats.md §Cursor Iteration. Returns the decoded
// entry, the byte offset of the next entry, and the (possibly grown)
// keyBuf. Returned entry.Key aliases keyBuf.
//
// On-disk layout per page-formats.md §Compressed Leaf delta entry:
//
//	inline:   [Flags u8][SharedLen u16][UnsharedLen u16][ValueLen u32][UnsharedKey][Value]
//	overflow: [Flags u8][SharedLen u16][UnsharedLen u16][UnsharedKey][OvflPage u64][TotalLen u64]
//
// The order differs between inline and overflow because ValueLen is only
// present in the inline form — the spec's positioning is constrained by
// the requirement that variable-length fields (UnsharedKey) come after
// all fixed-length fields so the reader can compute UnsharedKey's start
// offset without reading it first.
func (r LeafReader) decodeDeltaEntry(off int, prevKey, keyBuf []byte) (LeafEntry, int, []byte) {
	var e LeafEntry
	e.Flags = r.buf[off]
	off++
	sharedLen := int(le.Uint16(r.buf[off:]))
	off += 2
	unsharedLen := int(le.Uint16(r.buf[off:]))
	off += 2
	if e.Flags&CellFlagOverflow != 0 {
		// Overflow: UnsharedKey then OvflPage + TotalLen.
		keyBuf = append(keyBuf[:0], prevKey[:sharedLen]...)
		keyBuf = append(keyBuf, r.buf[off:off+unsharedLen]...)
		e.Key = keyBuf
		off += unsharedLen
		e.OverflowPage = le.Uint64(r.buf[off:])
		off += 8
		e.TotalLen = le.Uint64(r.buf[off:])
		off += 8
		return e, off, keyBuf
	}
	if e.IsNestedTree() {
		// NestedTree delta: UnsharedKey then Root + Count. Same wire
		// shape as overflow delta; different decoded-view fields.
		keyBuf = append(keyBuf[:0], prevKey[:sharedLen]...)
		keyBuf = append(keyBuf, r.buf[off:off+unsharedLen]...)
		e.Key = keyBuf
		off += unsharedLen
		e.NestedRoot = le.Uint64(r.buf[off:])
		off += 8
		e.NestedCount = le.Uint64(r.buf[off:])
		off += 8
		return e, off, keyBuf
	}
	// Inline: ValueLen comes BEFORE UnsharedKey, then Value at the tail.
	valLen := int(le.Uint32(r.buf[off:]))
	off += 4
	keyBuf = append(keyBuf[:0], prevKey[:sharedLen]...)
	keyBuf = append(keyBuf, r.buf[off:off+unsharedLen]...)
	e.Key = keyBuf
	off += unsharedLen
	e.Value = r.buf[off : off+valLen]
	off += valLen
	return e, off, keyBuf
}

// compressedLastKey returns the last entry's full key by walking the
// final restart group, reusing keyBuf to reconstruct delta keys. Values
// returned by decodeDeltaEntry are discarded — caller wants only the
// boundary key. Per page-formats.md §Leaf Split: used by the splitter
// to compute boundary keys for the parent branch's separator
// computation.
func (r LeafReader) compressedLastKey(keyBuf []byte) ([]byte, []byte) {
	lastGroup := r.rt.RestartCount() - 1
	gc := r.rt.GroupEntryCount(lastGroup)
	off := r.rt.Offset(lastGroup)

	re, off := r.decodeFullKeyEntry(off)
	prevKey := re.Key
	if gc == 1 {
		return prevKey, keyBuf
	}
	var e LeafEntry
	for range gc - 1 {
		e, off, keyBuf = r.decodeDeltaEntry(off, prevKey, keyBuf)
		prevKey = e.Key
	}
	return keyBuf, keyBuf
}

// compressedSearchLeaf is the two-phase compressed-leaf lookup:
//  1. binary search over restart-table offsets (each probe decodes one
//     restart-point key, full decode);
//  2. linear scan within the matched group, decoding each delta entry
//     fully and byte-comparing its key against target.
//
// The kcpl skip-without-decode optimization is intentionally absent.
// The spec layout puts ValueLen between UnsharedLen and UnsharedKey for
// inline deltas (so the scanner can't compute next-entry offset from
// SharedLen + UnsharedLen alone), and the simpler "decode every delta
// in the matched group" loop is the more robust implementation at the
// default RestartGroupTarget = 16. Profiling can revisit this if
// SearchLeaf becomes a hot spot.
func (r LeafReader) compressedSearchLeaf(target []byte) (index int, entry LeafEntry, found bool) {
	// Phase 1: binary search over restart points.
	lo, hi := 0, r.rt.RestartCount()
	for lo < hi {
		mid := lo + (hi-lo)/2
		off := r.rt.Offset(mid)
		e, _ := r.decodeFullKeyEntry(off)
		cmp := bytes.Compare(e.Key, target)
		switch {
		case cmp < 0:
			lo = mid + 1
		case cmp == 0:
			idx := r.rt.GroupStartIndex(mid)
			ret := e
			ret.Key = nil
			return idx, ret, true
		default:
			hi = mid
		}
	}

	group := lo - 1
	if group < 0 {
		// target precedes every restart key — insertion point is entry 0.
		return 0, LeafEntry{}, false
	}

	// Phase 2: scan within the matched group.
	startIdx := r.rt.GroupStartIndex(group)
	off := r.rt.Offset(group)
	gc := r.rt.GroupEntryCount(group)
	endIdx := startIdx + gc

	re, off := r.decodeFullKeyEntry(off)
	// restart key is strictly < target here (phase 1 would have returned
	// on equality, and binary search wouldn't have descended here on
	// strictly-greater).
	prevKey := re.Key
	var keyBuf []byte
	var e LeafEntry
	for idx := startIdx + 1; idx < endIdx; idx++ {
		e, off, keyBuf = r.decodeDeltaEntry(off, prevKey, keyBuf)
		cmp := bytes.Compare(e.Key, target)
		switch {
		case cmp == 0:
			ret := e
			ret.Key = nil
			return idx, ret, true
		case cmp > 0:
			return idx, LeafEntry{}, false
		}
		prevKey = e.Key
	}
	return endIdx, LeafEntry{}, false
}

// compressedSearchLeafIter mirrors compressedSearchLeaf but also returns a
// LeafIter positioned past the found/successor entry so the cursor's
// Seek / SeekGE doesn't pay the cost of a second group walk to resume
// streaming. Iter semantics per page-formats.md §Leaf Lookup:
//   - found==true: idx is the matching entry's index; iter.Next() returns
//     idx+1. entry has the matched value (entry.Key is nil; caller owns
//     target).
//   - found==false, idx<count: idx is the successor's index; entry has
//     the successor's value AND key (key aliases keyBuf — caller copies
//     before reusing the iter).
//   - found==false, idx==count: past the leaf's last entry (no successor
//     in this leaf); entry is the zero value; caller advances to the
//     next leaf.
func (r LeafReader) compressedSearchLeafIter(target, keyBuf, bufKeys []byte, bufEnts []LeafEntry) (int, LeafEntry, bool, LeafIter) {
	// Phase 1.
	lo, hi := 0, r.rt.RestartCount()
	for lo < hi {
		mid := lo + (hi-lo)/2
		off := r.rt.Offset(mid)
		e, afterValOff := r.decodeFullKeyEntry(off)
		cmp := bytes.Compare(e.Key, target)
		switch {
		case cmp < 0:
			lo = mid + 1
		case cmp == 0:
			// Exact-match on restart entry. Position the iter so the
			// next Next() returns the entry AFTER this restart point.
			idx := r.rt.GroupStartIndex(mid)
			gc := r.rt.GroupEntryCount(mid)
			it := LeafIter{
				r:           r,
				idx:         idx + 1,
				endIdx:      r.count,
				off:         afterValOff,
				compressed:  true,
				prevKey:     e.Key,
				keyBuf:      keyBuf[:0],
				nextRestart: idx + gc,
				groupIdx:    mid + 1,
				bufKeys:     bufKeys[:0],
				bufEnts:     bufEnts[:0],
			}
			ret := e
			ret.Key = nil
			return idx, ret, true, it
		default:
			hi = mid
		}
	}

	group := lo - 1
	if group < 0 {
		// target precedes every restart key; successor is entry 0.
		// Position iter at group 0 from its restart.
		return r.iterFromGroupRestart(0, keyBuf, bufKeys, bufEnts)
	}

	// Phase 2. Walk the group fully — no kcpl-skip shortcut here,
	// because the iter we return needs accurate keyBuf / prevKey state
	// continuous with the scan, and the skip-without-decode branch in
	// compressedSearchLeaf can't be reused as-is without rebuilding
	// per-skipped-entry state. Cost: at most K full delta decodes per
	// SeekGE per leaf, with K ≤ RestartGroupTarget (default 16). Hot
	// SeekGE-then-Next workloads pay this once per leaf transition.
	startIdx := r.rt.GroupStartIndex(group)
	off := r.rt.Offset(group)
	gc := r.rt.GroupEntryCount(group)
	nextRestart := startIdx + gc
	endIdx := startIdx + gc

	re, off := r.decodeFullKeyEntry(off)

	// Cmp restart key vs target — if restart key > target we'd have
	// stopped in phase 1; equality is a phase-1 case. So restart key
	// is strictly < target here, and the successor (if any) is in this
	// group's deltas or the next group's restart.
	prevKeyBuf := keyBuf
	prevKeyBuf = append(prevKeyBuf[:0], re.Key...)
	prevKey := prevKeyBuf

	for idx := startIdx + 1; idx < endIdx; idx++ {
		var e LeafEntry
		e, off, prevKeyBuf = r.decodeDeltaEntry(off, prevKey, prevKeyBuf)
		prevKey = prevKeyBuf
		cmp := bytes.Compare(e.Key, target)
		if cmp == 0 {
			it := LeafIter{
				r:           r,
				idx:         idx + 1,
				endIdx:      r.count,
				off:         off,
				compressed:  true,
				prevKey:     prevKeyBuf,
				keyBuf:      prevKeyBuf,
				nextRestart: nextRestart,
				groupIdx:    group + 1,
				bufKeys:     bufKeys[:0],
				bufEnts:     bufEnts[:0],
			}
			ret := e
			ret.Key = nil
			return idx, ret, true, it
		}
		if cmp > 0 {
			it := LeafIter{
				r:           r,
				idx:         idx + 1,
				endIdx:      r.count,
				off:         off,
				compressed:  true,
				prevKey:     prevKeyBuf,
				keyBuf:      prevKeyBuf,
				nextRestart: nextRestart,
				groupIdx:    group + 1,
				bufKeys:     bufKeys[:0],
				bufEnts:     bufEnts[:0],
			}
			return idx, e, false, it
		}
	}

	// Scan exhausted with no match. Successor (if any) is at endIdx,
	// the first entry of the next group.
	if endIdx >= r.count {
		it := LeafIter{
			r:          r,
			idx:        r.count,
			endIdx:     r.count,
			compressed: true,
			keyBuf:     keyBuf[:0],
			bufKeys:    bufKeys[:0],
			bufEnts:    bufEnts[:0],
		}
		return endIdx, LeafEntry{}, false, it
	}
	return r.iterFromGroupRestart(group+1, keyBuf, bufKeys, bufEnts)
}

// iterFromGroupRestart decodes the first (restart) entry of groupIdx and
// returns it as the successor, along with an iter positioned past it.
// Used when the SeekGE successor is the first entry of a group.
//
// Edge case for `gc == 1`: the returned iter has `idx == groupStart+1`
// and `nextRestart == groupStart+gc == groupStart+1`. Subsequent
// `Next()` immediately takes the restart-boundary branch
// (`it.idx == it.nextRestart`) and reads the first entry of the *next*
// group — correct behavior, but the off-by-zero arithmetic is easy to
// misread; verified against `LeafIter.Next` line "if it.idx ==
// it.nextRestart".
func (r LeafReader) iterFromGroupRestart(groupIdx int, keyBuf, bufKeys []byte, bufEnts []LeafEntry) (int, LeafEntry, bool, LeafIter) {
	groupStart := r.rt.GroupStartIndex(groupIdx)
	gc := r.rt.GroupEntryCount(groupIdx)
	e, afterValOff := r.decodeFullKeyEntry(r.rt.Offset(groupIdx))
	it := LeafIter{
		r:           r,
		idx:         groupStart + 1,
		endIdx:      r.count,
		off:         afterValOff,
		compressed:  true,
		prevKey:     e.Key,
		keyBuf:      keyBuf[:0],
		nextRestart: groupStart + gc,
		groupIdx:    groupIdx + 1,
		bufKeys:     bufKeys[:0],
		bufEnts:     bufEnts[:0],
	}
	return groupStart, e, false, it
}

// compressedEntryAt decodes the entry at absolute index idx by finding
// its containing group and walking forward from the restart point.
// O(K) where K = group size. Hot callers use a LeafIter and stream
// through At() instead.
func (r LeafReader) compressedEntryAt(idx int, keyBuf []byte) (LeafEntry, []byte) {
	accum := 0
	for g := range r.rt.RestartCount() {
		gc := r.rt.GroupEntryCount(g)
		if accum+gc > idx {
			off := r.rt.Offset(g)
			e, off := r.decodeFullKeyEntry(off)
			if accum == idx {
				return e, keyBuf
			}
			prevKey := e.Key
			for i := accum + 1; i <= idx; i++ {
				e, off, keyBuf = r.decodeDeltaEntry(off, prevKey, keyBuf)
				prevKey = keyBuf
			}
			return e, keyBuf
		}
		accum += gc
	}
	// Unreachable — EntryAt's caller bounds-checks idx.
	panic("page: compressedEntryAt index out of range after restart walk")
}

// (compareAndCommonLen was used by the kcpl-skip optimization in
// compressedSearchLeaf; dropped along with that optimization in a
// later simplification. Restore here when profiling justifies the
// kcpl revival.)
