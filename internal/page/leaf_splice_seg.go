package page

import "bytes"

// In-place splice helpers for the SEGREGATED leaf variant — the
// end-anchored model per page-formats.md §Segregated Leaf: ValueEnd is
// fixed across value splices and growth claims free space at the value
// region's LOW end, so a splice that touches entry i's value shifts the
// value bytes and VOff fields of entries BEFORE i (which occupy the low
// end) and leaves entries after it untouched. A splice that grows the
// restart table moves the whole value region down by 4 (every VOff and
// ValueEnd decrease by 4); one that shrinks it leaves a gap above
// ValueEnd. The stream-side surgery (group grow/shrink, displaced-
// successor re-encode) mirrors the interleaved helpers in
// leaf_splice.go — same group decisions, same decline conditions — with
// the segregated wire forms (VOff field instead of inline values).
//
// Every helper is a strict fast path: false ⇒ page byte-unchanged,
// caller falls back to decode + re-encode via LeafBuilder.

// writeSegRestartStream writes a segregated restart entry's stream half
// at off and returns the offset just past it.
func writeSegRestartStream(buf []byte, off int, flags uint8, key []byte, voff int) int {
	buf[off] = flags
	le.PutUint16(buf[off+1:], uint16(len(key)))
	le.PutUint16(buf[off+3:], uint16(voff))
	copy(buf[off+5:], key)
	return off + 5 + len(key)
}

// writeSegDeltaStream writes a segregated delta entry's stream half at
// off and returns the offset just past it.
func writeSegDeltaStream(buf []byte, off int, flags uint8, sharedLen int, unshared []byte, voff int) int {
	buf[off] = flags
	le.PutUint16(buf[off+1:], uint16(sharedLen))
	le.PutUint16(buf[off+3:], uint16(len(unshared)))
	le.PutUint16(buf[off+5:], uint16(voff))
	copy(buf[off+7:], unshared)
	return off + 7 + len(unshared)
}

// writeSegValueContent writes an entry's value-region content at voff:
// key-extent reference (overflow-key), then trailer or raw value bytes.
// flags must already be EmptyValue-normalized.
func writeSegValueContent(buf []byte, voff int, flags uint8, value []byte, t0, t1 uint64, keyExtPage uint64, keyTotalLen uint32) {
	if flags&CellFlagOverflowKey != 0 {
		le.PutUint64(buf[voff:], keyExtPage)
		le.PutUint32(buf[voff+8:], keyTotalLen)
		voff += 12
	}
	if cellHasTrailerOnly(flags) {
		le.PutUint64(buf[voff:], t0)
		le.PutUint64(buf[voff+8:], t1)
		return
	}
	copy(buf[voff:], value)
}

// segVOffOf returns the VOff VALUE of the entry at absolute index idx —
// a header-only group walk, no key reconstruction.
func segVOffOf(r LeafReader, idx int) int {
	accum := 0
	for g := 0; g < r.rt.RestartCount(); g++ {
		gc := r.rt.GroupEntryCount(g)
		if accum+gc <= idx {
			accum += gc
			continue
		}
		off := r.rt.Offset(g)
		if idx == accum {
			return segReadVOff(r.buf, off, true)
		}
		// Skip the restart, then deltas up to idx.
		keyLen := int(le.Uint16(r.buf[off+1:]))
		off += 5 + keyLen
		for pos := accum + 1; ; pos++ {
			if pos == idx {
				return segReadVOff(r.buf, off, false)
			}
			unsharedLen := int(le.Uint16(r.buf[off+3:]))
			off += 7 + unsharedLen
		}
	}
	panic("page: segVOffOf index out of range")
}

// segShiftVOffs adds delta to the VOff field of every entry with
// absolute index in [0, uptoIdx) — a header-only stream walk over the
// (pre-splice) group structure.
func segShiftVOffs(r LeafReader, uptoIdx, delta int) {
	if delta == 0 || uptoIdx <= 0 {
		return
	}
	idx := 0
	for g := 0; g < r.rt.RestartCount() && idx < uptoIdx; g++ {
		gc := r.rt.GroupEntryCount(g)
		off := r.rt.Offset(g)
		// Restart entry.
		segWriteVOff(r.buf, off, true, segReadVOff(r.buf, off, true)+delta)
		idx++
		keyLen := int(le.Uint16(r.buf[off+1:]))
		off += 5 + keyLen
		for i := 1; i < gc && idx < uptoIdx; i++ {
			segWriteVOff(r.buf, off, false, segReadVOff(r.buf, off, false)+delta)
			idx++
			unsharedLen := int(le.Uint16(r.buf[off+3:]))
			off += 7 + unsharedLen
		}
	}
}

// trySegAppend appends e to a segregated leaf in place. e.Key must sort
// after every existing key; prevKey is the leaf's last key. The group
// decision mirrors tryAppendCompressed exactly. Returns false (page
// byte-unchanged) when the entry does not fit.
func trySegAppend(buf []byte, cfg Config, e LeafEntry, prevKey []byte) bool {
	r := NewLeafReader(buf, cfg)
	count := r.Count()
	rc := r.rt.RestartCount()
	contentEnd := cfg.ContentEnd()
	dataEnd := r.DataEnd()
	valueEnd := r.valueEnd
	tableOff := contentEnd - rc*restartTableEntrySize

	flags := e.Flags &^ CellFlagEmptyValue // no EmptyValue form in this layout
	target := int(cfg.EffectiveRestartGroupTarget())

	lastGroupSlot := tableOff + (rc-1)*restartTableEntrySize
	lastGroupCount := int(buf[lastGroupSlot+2])
	lastEntryOff := int(le.Uint16(buf[lastGroupSlot:]))
	lastIsOvk := lastGroupCount == 1 && buf[lastEntryOff]&CellFlagOverflowKey != 0
	shared := sharedPrefixLen(prevKey, e.Key)
	isRestart := lastGroupCount >= target || shared == 0 || flags&CellFlagOverflowKey != 0 || lastIsOvk

	var streamSize int
	if isRestart {
		streamSize = 5 + len(e.Key)
	} else {
		streamSize = 7 + (len(e.Key) - shared)
	}
	valSize := segValueContentSize(flags, e.Value)

	tg := 0
	newRC := rc
	if isRestart {
		newRC++
		tg = restartTableEntrySize
	}
	shift := valSize + tg
	voff0 := segReadVOff(buf, segLeafEntryStart, true)
	newDataEnd := dataEnd + streamSize
	if newDataEnd > voff0-shift {
		return false
	}

	// Patch existing VOffs first (header-only walk over the pre-splice
	// structure; independent of the byte moves below).
	segShiftVOffs(r, count, -shift)

	// Move the value region down by shift, then write the new value at
	// the region's (new) top; the table-growth slack above it is zeroed.
	newValueEnd := valueEnd - tg
	copy(buf[voff0-shift:valueEnd-shift], buf[voff0:valueEnd])
	newVOff := newValueEnd - valSize
	t0, t1 := entryTrailer(e)
	writeSegValueContent(buf, newVOff, flags, e.Value, t0, t1, e.KeyExtPage, e.KeyTotalLen)
	if tg > 0 {
		clear(buf[newValueEnd:valueEnd])
	}

	// Stream half at the old DataEnd.
	if isRestart {
		writeSegRestartStream(buf, dataEnd, flags, e.Key, newVOff)
	} else {
		writeSegDeltaStream(buf, dataEnd, flags, shared, e.Key[shared:], newVOff)
	}

	// Restart table: grow by one slot (shift the existing rc entries 4
	// bytes toward DataEnd, preserving order) or bump the last group's
	// count — identical to tryAppendCompressed.
	if isRestart {
		newTableOff := contentEnd - newRC*restartTableEntrySize
		copy(buf[newTableOff:], buf[tableOff:tableOff+rc*restartTableEntrySize])
		newSlot := newTableOff + rc*restartTableEntrySize
		le.PutUint16(buf[newSlot:], uint16(dataEnd))
		buf[newSlot+2] = 1
		buf[newSlot+3] = 0
	} else {
		buf[lastGroupSlot+2] = uint8(lastGroupCount + 1)
	}

	WriteHeader(buf, TypeLeafSegregated, uint16(count+1), 0)
	le.PutUint16(buf[leafOffRestartCount:], uint16(newRC))
	le.PutUint16(buf[leafOffDataEnd:], uint16(newDataEnd))
	le.PutUint16(buf[segLeafOffValueEnd:], uint16(newValueEnd))
	return true
}

// trySegInsertAt inserts e at absolute index insertIdx in a segregated
// leaf by growing the containing restart group — the segregated analog
// of tryInsertAtCompressed (same I-B/I-C/I-D positions, same growth cap
// and overflow-key declines). The new entry's value content claims
// space at the region's low side of the successor's value (entries
// before insertIdx shift down); entries at or after insertIdx keep
// their VOffs.
func trySegInsertAt(buf []byte, cfg Config, insertIdx int, e LeafEntry) bool {
	r := NewLeafReader(buf, cfg)
	count := r.Count()
	rc := r.rt.RestartCount()
	contentEnd := cfg.ContentEnd()
	dataEnd := r.DataEnd()
	valueEnd := r.valueEnd
	tableOff := contentEnd - rc*restartTableEntrySize
	target := int(cfg.EffectiveRestartGroupTarget())
	flags := e.Flags &^ CellFlagEmptyValue

	// Find the group containing insertIdx (>= rule: boundary inserts
	// route into the earlier group as p == gc).
	accum, targetGroup, p, gc := 0, rc-1, 0, 0
	for g := range rc {
		c := r.rt.GroupEntryCount(g)
		if accum+c >= insertIdx {
			targetGroup, p, gc = g, insertIdx-accum, c
			break
		}
		accum += c
	}

	maxGroup := min(2*target, 255)
	if gc+1 > maxGroup {
		return false
	}
	if flags&CellFlagOverflowKey != 0 || buf[r.rt.Offset(targetGroup)]&CellFlagOverflowKey != 0 {
		return false
	}

	// Walk the target group for newShared (E[p-1] vs new key) and the
	// displaced successor (stream extent, flags, full key, VOff value).
	var (
		newShared, succShared int
		succKey               []byte
		succFlags             uint8
		succVOff              int
		spliceOff, succEnd    int
		hasSucc               bool
	)
	keyBuf := make([]byte, 0, 64)
	walkOff := r.rt.Offset(targetGroup)
	var prevKey []byte
	for i := 0; i <= p && i < gc; i++ {
		entryStart := walkOff
		var fl uint8
		var key []byte
		var voff int
		if i == 0 {
			fl, key, voff, walkOff = r.decodeSegRestart(walkOff)
		} else {
			fl, key, voff, walkOff, keyBuf = r.decodeSegDelta(walkOff, prevKey, keyBuf)
		}
		if i == p-1 {
			newShared = sharedPrefixLen(key, e.Key)
		}
		if i == p {
			spliceOff, succEnd, hasSucc = entryStart, walkOff, true
			succFlags = fl
			succKey = bytes.Clone(key)
			succVOff = voff
			succShared = sharedPrefixLen(e.Key, key)
		}
		prevKey = key
	}
	if p == gc {
		spliceOff, succEnd = walkOff, walkOff
	}

	// The successor's VOff VALUE (the split point of the value region)
	// is the entry at insertIdx — inside this group, or the next
	// group's restart in the I-D case.
	vsucc := valueEnd
	if hasSucc {
		vsucc = succVOff
	} else if insertIdx < count {
		vsucc = segVOffOf(r, insertIdx)
	}

	// Stream sizes.
	var newEntrySize int
	if p == 0 {
		newEntrySize = 5 + len(e.Key)
	} else {
		newEntrySize = 7 + (len(e.Key) - newShared)
	}
	newSuccSize := 0
	if hasSucc {
		newSuccSize = 7 + (len(succKey) - succShared)
	}
	oldSuccSize := succEnd - spliceOff
	byteDelta := newEntrySize + newSuccSize - oldSuccSize
	newDataEnd := dataEnd + byteDelta

	valSize := segValueContentSize(flags, e.Value)
	voff0 := segReadVOff(buf, segLeafEntryStart, true)
	if newDataEnd > voff0-valSize {
		return false
	}

	// Value side first: shift the values of entries [0, insertIdx)
	// down by valSize and patch their VOffs, then write the new value
	// content just below the successor's (unmoved) content.
	segShiftVOffs(r, insertIdx, -valSize)
	copy(buf[voff0-valSize:vsucc-valSize], buf[voff0:vsucc])
	newVOff := vsucc - valSize
	t0, t1 := entryTrailer(e)
	writeSegValueContent(buf, newVOff, flags, e.Value, t0, t1, e.KeyExtPage, e.KeyTotalLen)

	// Stream side: shift the tail after the old successor in one move,
	// then write the new entry and the re-encoded successor (which
	// keeps its VOff — its value did not move).
	if byteDelta != 0 {
		copy(buf[succEnd+byteDelta:newDataEnd], buf[succEnd:dataEnd])
	}
	var off int
	if p == 0 {
		off = writeSegRestartStream(buf, spliceOff, flags, e.Key, newVOff)
	} else {
		off = writeSegDeltaStream(buf, spliceOff, flags, newShared, e.Key[newShared:], newVOff)
	}
	if hasSucc {
		writeSegDeltaStream(buf, off, succFlags, succShared, succKey[succShared:], succVOff)
	}
	if newDataEnd < dataEnd {
		clear(buf[newDataEnd:dataEnd])
	}

	// Restart table: bump the target group's count; shift later groups'
	// stream offsets by byteDelta. RestartCount is unchanged.
	buf[tableOff+targetGroup*restartTableEntrySize+2] = uint8(gc + 1)
	for g := targetGroup + 1; g < rc; g++ {
		slot := tableOff + g*restartTableEntrySize
		le.PutUint16(buf[slot:], uint16(int(le.Uint16(buf[slot:]))+byteDelta))
	}

	WriteHeader(buf, TypeLeafSegregated, uint16(count+1), 0)
	le.PutUint16(buf[leafOffDataEnd:], uint16(newDataEnd))
	return true
}

// trySegDeleteAt removes the entry at deleteIdx from a segregated leaf
// — the segregated analog of tryDeleteAtCompressed (same D-A/B/C/D
// positions). The deleted entry's value span is closed by shifting the
// values of entries [0, deleteIdx) UP and patching their VOffs;
// entries after it keep theirs. Always shrinks both regions, so it
// never declines for space (same triangle-inequality bound as the
// interleaved delete; the +2-byte VOff field appears in both the
// removed and re-encoded forms and cancels).
func trySegDeleteAt(buf []byte, cfg Config, deleteIdx int) bool {
	r := NewLeafReader(buf, cfg)
	count := r.Count()
	rc := r.rt.RestartCount()
	contentEnd := cfg.ContentEnd()
	dataEnd := r.DataEnd()
	valueEnd := r.valueEnd
	tableOff := contentEnd - rc*restartTableEntrySize

	// Find the group containing deleteIdx (strict > rule).
	accum, targetGroup, p, gc := 0, rc-1, 0, 0
	for g := range rc {
		c := r.rt.GroupEntryCount(g)
		if accum+c > deleteIdx {
			targetGroup, p, gc = g, deleteIdx-accum, c
			break
		}
		accum += c
	}

	groupStart := r.rt.Offset(targetGroup)
	groupEnd := dataEnd
	if targetGroup+1 < rc {
		groupEnd = r.rt.Offset(targetGroup + 1)
	}

	// Deleted entry's value span.
	vdel := segVOffOf(r, deleteIdx)
	vend := valueEnd
	if deleteIdx+1 < count {
		vend = segVOffOf(r, deleteIdx+1)
	}
	valSize := vend - vdel
	voff0 := segReadVOff(buf, segLeafEntryStart, true)

	// Value side: patch VOffs of entries [0, deleteIdx), close the span
	// by shifting their bytes up, zero the freed low end.
	segShiftVOffs(r, deleteIdx, valSize)
	copy(buf[voff0+valSize:vdel+valSize], buf[voff0:vdel])
	clear(buf[voff0 : voff0+valSize])

	// Case D-A: single-entry group — remove it entirely; the restart
	// table shrinks one slot toward ContentEnd (mirrors the interleaved
	// D-A surgery; the gap above ValueEnd grows by 4 and persists,
	// page-formats.md §Segregated Leaf).
	if gc == 1 {
		oldGroupBytes := groupEnd - groupStart
		if groupEnd < dataEnd {
			copy(buf[groupStart:], buf[groupEnd:dataEnd])
		}
		newDataEnd := dataEnd - oldGroupBytes
		newRC := rc - 1
		newTableOff := contentEnd - newRC*restartTableEntrySize

		for g := targetGroup + 1; g < rc; g++ {
			slot := tableOff + g*restartTableEntrySize
			le.PutUint16(buf[slot:], uint16(int(le.Uint16(buf[slot:]))-oldGroupBytes))
		}
		for g := targetGroup - 1; g >= 0; g-- {
			src := tableOff + g*restartTableEntrySize
			dst := newTableOff + g*restartTableEntrySize
			copy(buf[dst:dst+restartTableEntrySize], buf[src:src+restartTableEntrySize])
		}
		// Zero the freed regions on either side of the live value
		// region: the stream tail up to the (shifted) region start
		// [newDataEnd, voff0+valSize), and the vacated low table slot
		// above ValueEnd [valueEnd, newTableOff). The value region
		// between them is live and untouched.
		clear(buf[newDataEnd : voff0+valSize])
		clear(buf[valueEnd:newTableOff])

		WriteHeader(buf, TypeLeafSegregated, uint16(count-1), 0)
		le.PutUint16(buf[leafOffRestartCount:], uint16(newRC))
		le.PutUint16(buf[leafOffDataEnd:], uint16(newDataEnd))
		return true
	}

	// gc > 1: D-B / D-C / D-D. Walk the group for the predecessor key,
	// the deleted entry's extent, and the displaced successor.
	var (
		predKey              []byte
		succFlags            uint8
		succKey              []byte
		succVOff             int
		newShared            int
		spliceOff, spliceEnd int
		hasSucc              bool
	)
	keyBuf := make([]byte, 0, 64)
	walkOff := groupStart
	var prevKey []byte
	for i := 0; i < gc; i++ {
		entryStart := walkOff
		var fl uint8
		var key []byte
		var voff int
		if i == 0 {
			fl, key, voff, walkOff = r.decodeSegRestart(walkOff)
		} else {
			fl, key, voff, walkOff, keyBuf = r.decodeSegDelta(walkOff, prevKey, keyBuf)
		}
		if i == p-1 {
			predKey = bytes.Clone(key)
		}
		if i == p {
			spliceOff = entryStart
			if p == gc-1 { // D-D
				spliceEnd = walkOff
				break
			}
		} else if i == p+1 { // D-B or D-C
			spliceEnd = walkOff
			hasSucc = true
			succFlags = fl
			succKey = bytes.Clone(key)
			succVOff = voff
			if p > 0 {
				newShared = sharedPrefixLen(predKey, succKey)
			}
			break
		}
		prevKey = key
	}

	newReplaceSize := 0
	if hasSucc {
		if p == 0 { // D-B: promote to restart
			newReplaceSize = 5 + len(succKey)
		} else { // D-C: delta vs E[p-1]
			newReplaceSize = 7 + (len(succKey) - newShared)
		}
	}
	oldReplaceSize := spliceEnd - spliceOff
	byteDelta := newReplaceSize - oldReplaceSize
	newDataEnd := dataEnd + byteDelta

	if byteDelta != 0 {
		copy(buf[spliceEnd+byteDelta:newDataEnd], buf[spliceEnd:dataEnd])
	}
	if hasSucc {
		if p == 0 {
			writeSegRestartStream(buf, spliceOff, succFlags, succKey, succVOff)
		} else {
			writeSegDeltaStream(buf, spliceOff, succFlags, newShared, succKey[newShared:], succVOff)
		}
	}
	// Zero the freed stream tail. byteDelta < 0 always (the delete
	// shrinks); [newDataEnd, dataEnd) is stale stream bytes strictly
	// below the (shifted) value-region start voff0+valSize, since
	// dataEnd <= voff0 on any valid page.
	clear(buf[newDataEnd:dataEnd])

	buf[tableOff+targetGroup*restartTableEntrySize+2] = uint8(gc - 1)
	for g := targetGroup + 1; g < rc; g++ {
		slot := tableOff + g*restartTableEntrySize
		le.PutUint16(buf[slot:], uint16(int(le.Uint16(buf[slot:]))+byteDelta))
	}

	WriteHeader(buf, TypeLeafSegregated, uint16(count-1), 0)
	le.PutUint16(buf[leafOffDataEnd:], uint16(newDataEnd))
	return true
}
