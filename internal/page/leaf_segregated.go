package page

import (
	"bytes"
	"fmt"
)

// Segregated-leaf decode + search + entry-walk helpers. Page layout per
// page-formats.md §Segregated Leaf:
//
//	+-----------------------+ offset 0
//	| Page Header (8 bytes) | Type=TypeLeafSegregated, Count=N
//	+-----------------------+ offset 8
//	| RestartCount uint16   |
//	| DataEnd      uint16   |
//	| ValueEnd     uint16   |
//	+-----------------------+ offset 14
//	| Entry 0 (restart)     |  entry stream: headers + key bytes, forward
//	| Entry 1 (delta)       |
//	| ...                   |
//	+-----------------------+ DataEnd
//	|       free space      |
//	+-----------------------+ VOff of entry 0
//	| Value Region          |  value content, packed in entry order
//	+-----------------------+ ValueEnd (<= restart table base)
//	| Restart Table         |  RestartCount × 4 bytes (Offset + Count + Reserved)
//	+-----------------------+ ContentEnd
//
// Entry-stream forms (restart table identical to §Compressed Leaf):
//
//	restart: [Flags u8][KeyLen u16][VOff u16][Key bytes]
//	delta:   [Flags u8][SharedLen u16][UnsharedLen u16][VOff u16][UnsharedKey]
//
// Value LENGTHS are stored nowhere: entry i's value-region content is
// [VOff[i], VOff[i+1]) in ENTRY order — [VOff[N-1], ValueEnd) for the
// last entry — so VOff is monotone non-decreasing with no gaps inside
// the region, and length derivation is BY ENTRY ORDER, never by value
// address (zero-length values make adjacent entries share a VOff).
// The region is END-ANCHORED: ValueEnd is fixed across value splices;
// growth claims free space at the region's LOW end.
//
// Value-region content by CellFlags: inline value → raw bytes;
// overflow → [OvflPage u64][TotalLen u64]; nested-tree →
// [Root u64][Count u64]; subpage → raw subpage bytes; overflow key →
// the 12-byte key-extent reference PREPENDED to the above. An empty
// value is a zero-length span — CellFlagEmptyValue is never set by the
// segregated encoder and is rejected at the Validate trust boundary
// (length derivation makes the flag redundant state).

// Segregated-leaf header field offsets and entry-data start.
const (
	segLeafOffValueEnd = HeaderSize + 4 // ValueEnd uint16
	segLeafEntryStart  = HeaderSize + 6 // 14
)

// Fixed offsets of the VOff field within the two entry-stream forms.
const (
	segRestartVOffAt = 3 // [Flags u8][KeyLen u16] → VOff
	segDeltaVOffAt   = 5 // [Flags u8][SharedLen u16][UnsharedLen u16] → VOff
)

// segReadVOff reads the VOff field of the entry whose stream offset is
// off, given whether that entry is a restart (group-first) entry.
func segReadVOff(buf []byte, off int, isRestart bool) int {
	if isRestart {
		return int(le.Uint16(buf[off+segRestartVOffAt:]))
	}
	return int(le.Uint16(buf[off+segDeltaVOffAt:]))
}

// segWriteVOff writes the VOff field of the entry at stream offset off.
func segWriteVOff(buf []byte, off int, isRestart bool, voff int) {
	if isRestart {
		le.PutUint16(buf[off+segRestartVOffAt:], uint16(voff))
		return
	}
	le.PutUint16(buf[off+segDeltaVOffAt:], uint16(voff))
}

// segValueContentLen returns the value-region byte length of an entry's
// FIXED-length content per flags, or -1 when the content is variable
// (inline / subpage values, whose length only entry-order derivation
// knows). The 12-byte key-extent prefix of overflow-key cells is
// included.
func segValueContentLen(flags uint8) int {
	n := 0
	if flags&CellFlagOverflowKey != 0 {
		n += 12
	}
	if cellHasTrailerOnly(flags) {
		return n + 16
	}
	return -1
}

// decodeSegRestart decodes the header + key of a restart entry at off.
// The value half is NOT materialized (it needs the next entry's VOff);
// callers use segValueHalf with a caller-derived vend. Returns flags,
// the key (borrowing the page buffer), the entry's VOff, and the next
// entry's stream offset. NOT bounds-checked — Validate is the trust
// boundary, exactly as for the interleaved decoders.
func (r LeafReader) decodeSegRestart(off int) (flags uint8, key []byte, voff, next int) {
	flags = r.buf[off]
	keyLen := int(le.Uint16(r.buf[off+1:]))
	voff = int(le.Uint16(r.buf[off+3:]))
	keyStart := off + 5
	return flags, r.buf[keyStart : keyStart+keyLen], voff, keyStart + keyLen
}

// decodeSegDelta decodes the header of a delta entry at off and
// reconstructs its full key into keyBuf (truncate-to-SharedLen +
// append-UnsharedKey, as the interleaved decodeDeltaEntry does).
// Returns flags, the reconstructed key (aliasing keyBuf), the entry's
// VOff, the next stream offset, and the grown keyBuf.
func (r LeafReader) decodeSegDelta(off int, prevKey, keyBuf []byte) (flags uint8, key []byte, voff, next int, outKeyBuf []byte) {
	flags = r.buf[off]
	sharedLen := int(le.Uint16(r.buf[off+1:]))
	unsharedLen := int(le.Uint16(r.buf[off+3:]))
	voff = int(le.Uint16(r.buf[off+5:]))
	unsharedStart := off + 7
	keyBuf = append(keyBuf[:0], prevKey[:sharedLen]...)
	keyBuf = append(keyBuf, r.buf[unsharedStart:unsharedStart+unsharedLen]...)
	return flags, keyBuf, voff, unsharedStart + unsharedLen, keyBuf
}

// segDeltaHeader reads a delta entry's fixed fields without touching
// the unshared bytes — the segregated analog of deltaHeader, used by
// the kcpl skip-scan.
func (r LeafReader) segDeltaHeader(off int) (flags uint8, sharedLen, voff, unsharedOff, unsharedLen, next int) {
	flags = r.buf[off]
	sharedLen = int(le.Uint16(r.buf[off+1:]))
	unsharedLen = int(le.Uint16(r.buf[off+3:]))
	voff = int(le.Uint16(r.buf[off+5:]))
	unsharedOff = off + 7
	next = unsharedOff + unsharedLen
	return
}

// segValueHalf materializes an entry's value half from its value-region
// span [voff, vend): the key-extent reference (overflow-key cells),
// then the value content per flags. Mirrors the field semantics of
// decodeFullKeyEntry's value half.
func (r LeafReader) segValueHalf(e *LeafEntry, voff, vend int) {
	if e.Flags&CellFlagOverflowKey != 0 {
		e.KeyExtPage = le.Uint64(r.buf[voff:])
		e.KeyTotalLen = le.Uint32(r.buf[voff+8:])
		voff += 12
	}
	switch {
	case e.Flags&CellFlagOverflow != 0:
		e.OverflowPage = le.Uint64(r.buf[voff:])
		e.TotalLen = le.Uint64(r.buf[voff+8:])
	case e.IsNestedTree():
		e.NestedRoot = le.Uint64(r.buf[voff:])
		e.NestedCount = le.Uint64(r.buf[voff+8:])
	default:
		e.Value = r.buf[voff:vend]
	}
}

// segKeyExtOnly materializes ONLY the key-extent reference of an
// overflow-key entry (the first 12 bytes of its value-region span).
// Used by search probes, which need the reference for extent-tie
// comparison but never the value.
func (r LeafReader) segKeyExtOnly(e *LeafEntry, voff int) {
	if e.Flags&CellFlagOverflowKey != 0 {
		e.KeyExtPage = le.Uint64(r.buf[voff:])
		e.KeyTotalLen = le.Uint32(r.buf[voff+8:])
	}
}

// segNextVOff resolves the value-span END of the entry at absolute
// index idx whose successor's stream offset is nextStreamOff: the next
// entry's VOff in entry order, or ValueEnd for the page's last entry.
// nextIsRestart says whether the successor (if any) opens a group.
func (r LeafReader) segNextVOff(idx, nextStreamOff int, nextIsRestart bool) int {
	if idx+1 >= r.count {
		return r.valueEnd
	}
	return segReadVOff(r.buf, nextStreamOff, nextIsRestart)
}

// segEntryAt decodes the entry at absolute index idx — the segregated
// arm of LeafReader.EntryAt. O(K) group walk from the restart point.
func (r LeafReader) segEntryAt(idx int, keyBuf []byte) (LeafEntry, []byte) {
	accum := 0
	for g := range r.rt.RestartCount() {
		gc := r.rt.GroupEntryCount(g)
		if accum+gc <= idx {
			accum += gc
			continue
		}
		off := r.rt.Offset(g)
		flags, key, voff, next := r.decodeSegRestart(off)
		pos := accum
		prevKey := key
		for pos < idx {
			off = next
			flags, key, voff, next, keyBuf = r.decodeSegDelta(off, prevKey, keyBuf)
			prevKey = key
			pos++
		}
		e := LeafEntry{Flags: flags, Key: key}
		vend := r.segNextVOff(idx, next, pos+1 == accum+gc)
		r.segValueHalf(&e, voff, vend)
		return e, keyBuf
	}
	panic("page: segEntryAt index out of range after restart walk")
}

// segLastKey returns the last entry's full key by walking the final
// restart group — the segregated arm of LeafReader.LastKey.
func (r LeafReader) segLastKey(keyBuf []byte) ([]byte, []byte) {
	lastGroup := r.rt.RestartCount() - 1
	gc := r.rt.GroupEntryCount(lastGroup)
	off := r.rt.Offset(lastGroup)
	_, key, _, next := r.decodeSegRestart(off)
	if gc == 1 {
		return key, keyBuf
	}
	prevKey := key
	for range gc - 1 {
		_, key, _, next, keyBuf = r.decodeSegDelta(next, prevKey, keyBuf)
		prevKey = key
	}
	return key, keyBuf
}

// segSearchLeaf is the two-phase segregated-leaf lookup — the same
// restart-binary-search + kcpl skip-scan as compressedSearchLeaf (see
// its doc for the skip soundness argument), over the pure headers+keys
// entry stream. Values are touched only for the found entry's
// materialization and for overflow-key extent ties (the 12-byte
// reference at the span's front).
func (r LeafReader) segSearchLeaf(target []byte, tail TailCompare) (index int, entry LeafEntry, found bool, err error) {
	lo, hi := 0, r.rt.RestartCount()
	for lo < hi {
		mid := lo + (hi-lo)/2
		off := r.rt.Offset(mid)
		flags, key, voff, next := r.decodeSegRestart(off)
		e := LeafEntry{Flags: flags, Key: key}
		r.segKeyExtOnly(&e, voff)
		cmp, cerr := compareEntryKey(e, target, tail)
		if cerr != nil {
			return 0, LeafEntry{}, false, cerr
		}
		switch {
		case cmp < 0:
			lo = mid + 1
		case cmp == 0:
			idx := r.rt.GroupStartIndex(mid)
			gc := r.rt.GroupEntryCount(mid)
			vend := r.segNextVOff(idx, next, gc == 1)
			ret := LeafEntry{Flags: flags}
			r.segValueHalf(&ret, voff, vend)
			return idx, ret, true, nil
		default:
			hi = mid
		}
	}

	group := lo - 1
	if group < 0 {
		return 0, LeafEntry{}, false, nil
	}

	startIdx := r.rt.GroupStartIndex(group)
	off := r.rt.Offset(group)
	gc := r.rt.GroupEntryCount(group)
	endIdx := startIdx + gc

	_, rkey, _, next := r.decodeSegRestart(off)
	kcpl := sharedPrefixLen(rkey, target)
	off = next
	for idx := startIdx + 1; idx < endIdx; idx++ {
		flags, sharedLen, voff, unsharedOff, unsharedLen, next := r.segDeltaHeader(off)
		switch {
		case sharedLen > kcpl:
			off = next
			continue
		case sharedLen < kcpl:
			return idx, LeafEntry{}, false, nil
		}
		unshared := r.buf[unsharedOff : unsharedOff+unsharedLen]
		cmp := bytes.Compare(unshared, target[kcpl:])
		switch {
		case cmp == 0:
			vend := r.segNextVOff(idx, next, idx+1 == endIdx)
			ret := LeafEntry{Flags: flags}
			r.segValueHalf(&ret, voff, vend)
			return idx, ret, true, nil
		case cmp > 0:
			return idx, LeafEntry{}, false, nil
		}
		kcpl += sharedPrefixLen(unshared, target[kcpl:])
		off = next
	}
	return endIdx, LeafEntry{}, false, nil
}

// segSearchLeafIter mirrors compressedSearchLeafIter for the segregated
// layout: the lookup result plus a LeafIter positioned past the
// found/successor entry. Phase 2 walks the group with full key
// reconstruction (the returned iter needs continuous keyBuf state).
func (r LeafReader) segSearchLeafIter(target, keyBuf, bufKeys []byte, bufEnts []LeafEntry, tail TailCompare) (int, LeafEntry, bool, LeafIter, error) {
	lo, hi := 0, r.rt.RestartCount()
	for lo < hi {
		mid := lo + (hi-lo)/2
		off := r.rt.Offset(mid)
		flags, key, voff, next := r.decodeSegRestart(off)
		e := LeafEntry{Flags: flags, Key: key}
		r.segKeyExtOnly(&e, voff)
		cmp, cerr := compareEntryKey(e, target, tail)
		if cerr != nil {
			return 0, LeafEntry{}, false, LeafIter{}, cerr
		}
		switch {
		case cmp < 0:
			lo = mid + 1
		case cmp == 0:
			idx := r.rt.GroupStartIndex(mid)
			gc := r.rt.GroupEntryCount(mid)
			it := LeafIter{
				r:           r,
				idx:         idx + 1,
				endIdx:      r.count,
				off:         next,
				variant:     TypeLeafSegregated,
				segEndVOff:  r.valueEnd,
				prevKey:     key,
				keyBuf:      keyBuf[:0],
				nextRestart: idx + gc,
				groupIdx:    mid + 1,
				bufKeys:     bufKeys[:0],
				bufEnts:     bufEnts[:0],
			}
			vend := r.segNextVOff(idx, next, gc == 1)
			ret := LeafEntry{Flags: flags}
			r.segValueHalf(&ret, voff, vend)
			return idx, ret, true, it, nil
		default:
			hi = mid
		}
	}

	group := lo - 1
	if group < 0 {
		i, e, f, it := r.segIterFromGroupRestart(0, keyBuf, bufKeys, bufEnts)
		return i, e, f, it, nil
	}

	startIdx := r.rt.GroupStartIndex(group)
	off := r.rt.Offset(group)
	gc := r.rt.GroupEntryCount(group)
	nextRestart := startIdx + gc
	endIdx := startIdx + gc

	_, rkey, _, next := r.decodeSegRestart(off)
	prevKeyBuf := append(keyBuf[:0], rkey...)
	prevKey := prevKeyBuf
	off = next

	for idx := startIdx + 1; idx < endIdx; idx++ {
		var flags uint8
		var voff int
		flags, prevKeyBuf, voff, next, prevKeyBuf = r.decodeSegDelta(off, prevKey, prevKeyBuf)
		prevKey = prevKeyBuf
		cmp := bytes.Compare(prevKeyBuf, target)
		if cmp >= 0 {
			it := LeafIter{
				r:           r,
				idx:         idx + 1,
				endIdx:      r.count,
				off:         next,
				variant:     TypeLeafSegregated,
				segEndVOff:  r.valueEnd,
				prevKey:     prevKeyBuf,
				keyBuf:      prevKeyBuf,
				nextRestart: nextRestart,
				groupIdx:    group + 1,
				bufKeys:     bufKeys[:0],
				bufEnts:     bufEnts[:0],
			}
			vend := r.segNextVOff(idx, next, idx+1 == endIdx)
			ret := LeafEntry{Flags: flags}
			r.segValueHalf(&ret, voff, vend)
			if cmp == 0 {
				return idx, ret, true, it, nil
			}
			ret.Key = prevKeyBuf
			return idx, ret, false, it, nil
		}
		off = next
	}

	if endIdx >= r.count {
		it := LeafIter{
			r:          r,
			idx:        r.count,
			endIdx:     r.count,
			variant:    TypeLeafSegregated,
			segEndVOff: r.valueEnd,
			keyBuf:     keyBuf[:0],
			bufKeys:    bufKeys[:0],
			bufEnts:    bufEnts[:0],
		}
		return endIdx, LeafEntry{}, false, it, nil
	}
	i, e2, f, it := r.segIterFromGroupRestart(group+1, keyBuf, bufKeys, bufEnts)
	return i, e2, f, it, nil
}

// segIterFromGroupRestart decodes the first (restart) entry of groupIdx
// as the successor, plus an iter positioned past it — the segregated
// analog of iterFromGroupRestart.
func (r LeafReader) segIterFromGroupRestart(groupIdx int, keyBuf, bufKeys []byte, bufEnts []LeafEntry) (int, LeafEntry, bool, LeafIter) {
	groupStart := r.rt.GroupStartIndex(groupIdx)
	gc := r.rt.GroupEntryCount(groupIdx)
	off := r.rt.Offset(groupIdx)
	flags, key, voff, next := r.decodeSegRestart(off)
	it := LeafIter{
		r:           r,
		idx:         groupStart + 1,
		endIdx:      r.count,
		off:         next,
		variant:     TypeLeafSegregated,
		segEndVOff:  r.valueEnd,
		prevKey:     key,
		keyBuf:      keyBuf[:0],
		nextRestart: groupStart + gc,
		groupIdx:    groupIdx + 1,
		bufKeys:     bufKeys[:0],
		bufEnts:     bufEnts[:0],
	}
	e := LeafEntry{Flags: flags, Key: key}
	vend := r.segNextVOff(groupStart, next, gc == 1)
	r.segValueHalf(&e, voff, vend)
	return groupStart, e, false, it
}

// segValidate is the segregated arm of LeafReader.Validate: the
// checked structural walk over the entry stream, the restart table,
// and the value region. Total over its input — returns a wrapped
// ErrCorrupted, never panics, on any byte sequence.
func (r LeafReader) segValidate() error {
	contentEnd := r.cfg.ContentEnd()
	rc := r.rt.RestartCount()
	tableBase := contentEnd - rc*restartTableEntrySize

	if r.valueEnd < segLeafEntryStart || r.valueEnd > tableBase {
		return fmt.Errorf("%w: segregated leaf ValueEnd %d outside [%d, %d]", ErrCorrupted, r.valueEnd, segLeafEntryStart, tableBase)
	}
	if r.dataEnd < segLeafEntryStart || r.dataEnd > tableBase {
		return fmt.Errorf("%w: segregated leaf DataEnd %d outside [%d, %d]", ErrCorrupted, r.dataEnd, segLeafEntryStart, tableBase)
	}
	if rc == 0 && r.count > 0 {
		return fmt.Errorf("%w: segregated leaf has %d entries but 0 restart groups", ErrCorrupted, r.count)
	}
	sum := 0
	for g := range rc {
		c := r.rt.GroupEntryCount(g)
		if c == 0 {
			return fmt.Errorf("%w: segregated leaf restart group %d has Count=0 (spec invariant)", ErrCorrupted, g)
		}
		sum += c
	}
	if sum != r.count {
		return fmt.Errorf("%w: segregated leaf sum-of-group-counts %d != header Count %d", ErrCorrupted, sum, r.count)
	}
	if r.count == 0 {
		if r.dataEnd != segLeafEntryStart {
			return fmt.Errorf("%w: empty segregated leaf DataEnd %d != entry-data start %d", ErrCorrupted, r.dataEnd, segLeafEntryStart)
		}
		if r.valueEnd != segLeafEntryStart {
			return fmt.Errorf("%w: empty segregated leaf ValueEnd %d != entry-data start %d", ErrCorrupted, r.valueEnd, segLeafEntryStart)
		}
		return nil
	}

	// Per-entry walk: stream contiguity + flag rules + VOff collection.
	// VOffs must be monotone non-decreasing with the first at or above
	// DataEnd; per-entry spans must match the fixed-length forms.
	expected := segLeafEntryStart
	prevVOff := -1
	firstVOff := -1
	// checkSpan validates the derived span [voff, vend) of the entry
	// whose flags were already flag-mask/combo-checked.
	checkSpan := func(flags uint8, voff, vend int, ctx string) error {
		if voff < r.dataEnd || vend > r.valueEnd || voff > vend {
			return fmt.Errorf("%s value span [%d, %d) outside region [DataEnd %d, ValueEnd %d]", ctx, voff, vend, r.dataEnd, r.valueEnd)
		}
		if fixed := segValueContentLen(flags); fixed >= 0 {
			if vend-voff != fixed {
				return fmt.Errorf("%s value span %d != fixed content length %d for flags 0x%x", ctx, vend-voff, fixed, flags)
			}
		} else if flags&CellFlagOverflowKey != 0 && vend-voff < 12 {
			return fmt.Errorf("%s overflow-key value span %d < 12-byte key-extent reference", ctx, vend-voff)
		}
		if flags&CellFlagOverflowKey != 0 {
			extPage := le.Uint64(r.buf[voff:])
			totalLen := int(le.Uint32(r.buf[voff+8:]))
			if extPage == 0 {
				return fmt.Errorf("%s overflow-key extent page is 0", ctx)
			}
			if totalLen <= r.cfg.InlineThreshold() {
				return fmt.Errorf("%s overflow-key KeyTotalLen %d does not exceed inline threshold %d", ctx, totalLen, r.cfg.InlineThreshold())
			}
		}
		return nil
	}
	// The walk collects each entry's (flags, voff); spans resolve one
	// entry in arrears (entry i's end is entry i+1's voff).
	var pendFlags uint8
	var pendVOff int
	havePend := false
	flushPend := func(vend int, ctx string) error {
		if !havePend {
			return nil
		}
		havePend = false
		return checkSpan(pendFlags, pendVOff, vend, ctx)
	}

	idx := 0
	for g := range rc {
		gc := r.rt.GroupEntryCount(g)
		off := r.rt.Offset(g)
		if off != expected {
			return fmt.Errorf("%w: segregated leaf restart[%d] offset %d != entry stream position %d", ErrCorrupted, g, off, expected)
		}
		prevKeyLen := 0
		for i := range gc {
			ctx := fmt.Sprintf("segregated leaf group %d entry %d:", g, i)
			if err := r.segEnsureStream(off, 1); err != nil {
				return fmt.Errorf("%w: %s %w", ErrCorrupted, ctx, err)
			}
			flags := r.buf[off]
			if flags&^cellFlagKnownMask != 0 {
				return fmt.Errorf("%w: %s unknown CellFlags 0x%x", ErrCorrupted, ctx, flags&^cellFlagKnownMask)
			}
			if flags&CellFlagEmptyValue != 0 {
				// Derived lengths make the flag redundant state; the
				// segregated encoder never sets it (page-formats.md
				// §Segregated Leaf).
				return fmt.Errorf("%w: %s CellFlagEmptyValue set on a segregated-leaf entry (rejected: value emptiness is derived)", ErrCorrupted, ctx)
			}
			if err := validateCellFlagsCombo(flags); err != nil {
				return fmt.Errorf("%w: %s %w", ErrCorrupted, ctx, err)
			}
			var voff, next, keyLen int
			if i == 0 {
				if err := r.segEnsureStream(off, 5); err != nil {
					return fmt.Errorf("%w: %s %w", ErrCorrupted, ctx, err)
				}
				keyLen = int(le.Uint16(r.buf[off+1:]))
				voff = int(le.Uint16(r.buf[off+3:]))
				if err := r.segEnsureStream(off+5, keyLen); err != nil {
					return fmt.Errorf("%w: %s key bytes: %w", ErrCorrupted, ctx, err)
				}
				next = off + 5 + keyLen
				if flags&CellFlagOverflowKey != 0 {
					if t := r.cfg.InlineThreshold(); keyLen != t {
						return fmt.Errorf("%w: %s overflow-key resident length %d != inline threshold %d", ErrCorrupted, ctx, keyLen, t)
					}
					if gc != 1 {
						return fmt.Errorf("%w: %s overflow-key restart entry in group with Count=%d (must be a singleton group)", ErrCorrupted, ctx, gc)
					}
				} else if t := r.cfg.InlineThreshold(); keyLen > t {
					// Over-threshold keys MUST take the overflow-key
					// form (page-formats.md §Overflow-Key Cells).
					return fmt.Errorf("%w: %s inline KeyLen %d exceeds inline threshold %d (over-threshold keys take the overflow-key form)", ErrCorrupted, ctx, keyLen, t)
				}
			} else {
				if flags&CellFlagOverflowKey != 0 {
					return fmt.Errorf("%w: %s delta entry carries CellFlagOverflowKey (overflow-key entries are restart-only singleton groups)", ErrCorrupted, ctx)
				}
				if err := r.segEnsureStream(off, 7); err != nil {
					return fmt.Errorf("%w: %s %w", ErrCorrupted, ctx, err)
				}
				sharedLen := int(le.Uint16(r.buf[off+1:]))
				unsharedLen := int(le.Uint16(r.buf[off+3:]))
				voff = int(le.Uint16(r.buf[off+5:]))
				if sharedLen > prevKeyLen {
					return fmt.Errorf("%w: %s delta SharedLen %d exceeds previous full-key length %d", ErrCorrupted, ctx, sharedLen, prevKeyLen)
				}
				if err := r.segEnsureStream(off+7, unsharedLen); err != nil {
					return fmt.Errorf("%w: %s unshared bytes: %w", ErrCorrupted, ctx, err)
				}
				keyLen = sharedLen + unsharedLen
				if t := r.cfg.InlineThreshold(); keyLen > t {
					// Deltas never carry the overflow-key form, so the
					// reconstructed full key must be within the inline
					// threshold (page-formats.md §Overflow-Key Cells).
					return fmt.Errorf("%w: %s delta full-key length %d exceeds inline threshold %d", ErrCorrupted, ctx, keyLen, t)
				}
				next = off + 7 + unsharedLen
			}
			// VOff monotonicity + previous entry's span resolution.
			if voff < prevVOff {
				return fmt.Errorf("%w: %s VOff %d < previous entry's VOff %d (entry-order monotonicity)", ErrCorrupted, ctx, voff, prevVOff)
			}
			if err := flushPend(voff, ctx); err != nil {
				return fmt.Errorf("%w: %s", ErrCorrupted, err)
			}
			if firstVOff < 0 {
				firstVOff = voff
			}
			pendFlags, pendVOff, havePend = flags, voff, true
			prevVOff = voff
			prevKeyLen = keyLen
			off = next
			idx++
		}
		expected = off
	}
	if expected != r.dataEnd {
		return fmt.Errorf("%w: segregated leaf entry stream ends at %d, DataEnd %d", ErrCorrupted, expected, r.dataEnd)
	}
	if err := flushPend(r.valueEnd, "last entry:"); err != nil {
		return fmt.Errorf("%w: %s", ErrCorrupted, err)
	}
	if firstVOff >= 0 && firstVOff < r.dataEnd {
		return fmt.Errorf("%w: segregated leaf first VOff %d precedes DataEnd %d", ErrCorrupted, firstVOff, r.dataEnd)
	}
	_ = idx
	return nil
}

// segEnsureStream verifies r.buf[off : off+n] lies within the entry
// stream [segLeafEntryStart, dataEnd).
func (r LeafReader) segEnsureStream(off, n int) error {
	if off < segLeafEntryStart {
		return fmt.Errorf("read at offset %d (n=%d) precedes entry-data start %d", off, n, segLeafEntryStart)
	}
	if n < 0 {
		return fmt.Errorf("read length %d negative", n)
	}
	if off+n > r.dataEnd {
		return fmt.Errorf("read at offset %d (n=%d) exceeds DataEnd %d", off, n, r.dataEnd)
	}
	return nil
}

// segPatchRefs is the segregated arm of patchEntryRefs: page references
// live in the value region at fixed offsets from each entry's VOff —
// the key-extent u64 at VOff (overflow-key cells), the trailer's first
// u64 at VOff (+12 with a key-extent prefix) for overflow / nested
// cells. Entry keys are reconstructed for the callback exactly as the
// interleaved walk does; values are materialized span-accurately.
func (r LeafReader) segPatchRefs(refAt, keyExtAt func(idx int, e LeafEntry) uint64) {
	var keyBuf []byte
	idx := 0
	for g := 0; g < r.rt.RestartCount(); g++ {
		gc := r.rt.GroupEntryCount(g)
		off := r.rt.Offset(g)
		var flags uint8
		var key []byte
		var voff, next int
		flags, key, voff, next = r.decodeSegRestart(off)
		prevKey := key
		for i := 0; i < gc; i++ {
			if i > 0 {
				off = next
				flags, key, voff, next, keyBuf = r.decodeSegDelta(off, prevKey, keyBuf)
				prevKey = key
			}
			e := LeafEntry{Flags: flags, Key: key}
			vend := r.segNextVOff(idx, next, i+1 == gc)
			r.segValueHalf(&e, voff, vend)
			trailerOff := voff
			if keyExtAt != nil && e.IsOverflowKey() {
				le.PutUint64(r.buf[voff:], keyExtAt(idx, e))
			}
			if e.IsOverflowKey() {
				trailerOff += 12
			}
			if refAt != nil && (e.Flags&CellFlagOverflow != 0 || e.IsNestedTree()) {
				le.PutUint64(r.buf[trailerOff:], refAt(idx, e))
			}
			idx++
		}
	}
}
