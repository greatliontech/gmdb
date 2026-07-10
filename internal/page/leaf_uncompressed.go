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

// ucSearchLeaf performs O(log N) binary search over the positional offset
// table. Returns the entry's index (or the insertion point on miss), the
// entry value-side fields (Key cleared on found — caller has target),
// and the found flag.
func (r LeafReader) ucSearchLeaf(target []byte) (int, LeafEntry, bool) {
	lo, hi := 0, r.count
	for lo < hi {
		mid := lo + (hi-lo)/2
		e, _ := r.decodeFullKeyEntry(r.ucOffset(mid))
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
		e, _ := r.decodeFullKeyEntry(r.ucOffset(mid))
		cmp := bytes.Compare(e.Key, target)
		switch {
		case cmp < 0:
			lo = mid + 1
		case cmp == 0:
			// idx == count is a valid past-end position: Next's bounds
			// check fires before any offset-table read, so no table
			// slot is touched past the last entry.
			it := LeafIter{
				r:          r,
				idx:        mid + 1,
				endIdx:     r.count,
				compressed: false,
				keyBuf:     keyBuf,
				bufKeys:    bufKeys[:0],
				bufEnts:    bufEnts[:0],
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
	e, _ := r.decodeFullKeyEntry(r.ucOffset(lo))
	it := LeafIter{
		r:          r,
		idx:        lo + 1,
		endIdx:     r.count,
		compressed: false,
		keyBuf:     keyBuf,
		bufKeys:    bufKeys[:0],
		bufEnts:    bufEnts[:0],
	}
	return lo, e, false, it
}
