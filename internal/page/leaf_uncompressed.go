package page

import "bytes"

// Uncompressed-leaf decode + search helpers. Page layout per
// page-formats.md §Uncompressed Leaf:
//
//	+-----------------------+ offset 0
//	| Page Header (8 bytes) | Type=TypeLeafUncompressed, Count=N
//	+-----------------------+ offset 8
//	| DataEnd      uint16   |
//	| Reserved     uint16   |
//	+-----------------------+ offset 12
//	| Entry 0               |  entries forward
//	| Entry 1               |
//	| ...                   |
//	+-----------------------+ DataEnd
//	|       free space      |
//	+-----------------------+
//	| Offset Table          |  N × 2 bytes (positional)
//	+-----------------------+ ContentEnd
//
// Each entry stores a full key with no shared/unshared distinction:
//
//	[CellFlags u8][KeyLen u16][ValueLen u32][Key bytes][Value bytes]
//
// Overflow form replaces ValueLen + Value with OvflPage u64 + TotalLen
// u64 (matches the compressed restart-entry overflow form).

// ucOffset returns the byte offset of entry idx. Reads from the
// positional offset table at the page tail.
func (r LeafReader) ucOffset(idx int) int {
	return int(le.Uint16(r.buf[r.ucTableOff+idx*ucOffsetEntrySize:]))
}

// ucDecodeEntry decodes the entry at byte offset off. Returns the entry
// and the offset of the next entry's first byte. Key and Value (for
// inline values) borrow from the page buffer.
func (r LeafReader) ucDecodeEntry(off int) (LeafEntry, int) {
	var e LeafEntry
	e.Flags = r.buf[off]
	off++
	keyLen := int(le.Uint16(r.buf[off:]))
	off += 2
	if e.Flags&CellFlagOverflow != 0 {
		// [Flags][KeyLen][Key][OvflPage u64][TotalLen u64]
		e.Key = r.buf[off : off+keyLen]
		off += keyLen
		e.OverflowPage = le.Uint64(r.buf[off:])
		off += 8
		e.TotalLen = le.Uint64(r.buf[off:])
		off += 8
		return e, off
	}
	// [Flags][KeyLen][ValueLen][Key][Value]
	valLen := int(le.Uint32(r.buf[off:]))
	off += 4
	e.Key = r.buf[off : off+keyLen]
	off += keyLen
	e.Value = r.buf[off : off+valLen]
	off += valLen
	return e, off
}

// ucSearchLeaf performs O(log N) binary search over the positional offset
// table. Returns the entry's index (or the insertion point on miss), the
// entry value-side fields (Key cleared on found — caller has target),
// and the found flag.
func (r LeafReader) ucSearchLeaf(target []byte) (int, LeafEntry, bool) {
	lo, hi := 0, r.count
	for lo < hi {
		mid := lo + (hi-lo)/2
		e, _ := r.ucDecodeEntry(r.ucOffset(mid))
		cmp := bytes.Compare(e.Key, target)
		switch {
		case cmp < 0:
			lo = mid + 1
		case cmp == 0:
			e.Key = nil
			return mid, e, true
		default:
			hi = mid
		}
	}
	return lo, LeafEntry{}, false
}

// ucSearchLeafIter mirrors compressedSearchLeafIter: returns the lookup
// result plus a LeafIter positioned past the found / successor entry. On
// uncompressed pages "past" is just idx+1 — no group walk, no delta
// state to seed.
func (r LeafReader) ucSearchLeafIter(target, keyBuf, bufKeys []byte, bufEnts []LeafEntry) (int, LeafEntry, bool, LeafIter) {
	lo, hi := 0, r.count
	for lo < hi {
		mid := lo + (hi-lo)/2
		e, _ := r.ucDecodeEntry(r.ucOffset(mid))
		cmp := bytes.Compare(e.Key, target)
		switch {
		case cmp < 0:
			lo = mid + 1
		case cmp == 0:
			it := LeafIter{
				r:          r,
				idx:        mid + 1,
				endIdx:     r.count,
				off:        r.ucOffset(mid + 1), // 0 if mid+1 == count; benign
				compressed: false,
				keyBuf:     keyBuf,
				bufKeys:    bufKeys[:0],
				bufEnts:    bufEnts[:0],
			}
			// At end-of-leaf we'd index past the offset table; guard.
			if mid+1 >= r.count {
				it.off = 0
			}
			ret := e
			ret.Key = nil
			return mid, ret, true, it
		default:
			hi = mid
		}
	}
	if lo >= r.count {
		// past end of leaf; successor not in this page.
		it := LeafIter{
			r:          r,
			idx:        r.count,
			endIdx:     r.count,
			compressed: false,
			keyBuf:     keyBuf,
			bufKeys:    bufKeys[:0],
			bufEnts:    bufEnts[:0],
		}
		return r.count, LeafEntry{}, false, it
	}
	// successor at lo
	sucOff := r.ucOffset(lo)
	e, nextOff := r.ucDecodeEntry(sucOff)
	it := LeafIter{
		r:          r,
		idx:        lo + 1,
		endIdx:     r.count,
		off:        nextOff,
		compressed: false,
		keyBuf:     keyBuf,
		bufKeys:    bufKeys[:0],
		bufEnts:    bufEnts[:0],
	}
	return lo, e, false, it
}

// ucSkipEntry advances past an uncompressed entry at off, returning the
// next entry's byte offset. Standalone helper for the in-place splice
// machinery in chunk-4.6β.
func ucSkipEntry(buf []byte, off int) int {
	flags := buf[off]
	off++
	keyLen := int(le.Uint16(buf[off:]))
	off += 2 + keyLen
	if flags&CellFlagOverflow != 0 {
		return off + 16
	}
	valLen := int(le.Uint32(buf[off:]))
	return off + 4 + valLen
}
