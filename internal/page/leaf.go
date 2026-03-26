package page

import "bytes"

// Leaf pages store key-value pairs using prefix compression.
//
// Layout:
//
//	Page Header (8 bytes)
//	RestartInterval uint16    fixed: 16
//	RestartCount    uint16    number of restart points
//	Entry 0 (restart)         entries in forward order, starting at fixed offset 12
//	Entry 1 (delta)
//	...
//	Entry N
//	     free space
//	Restart Table             array of (Offset uint16), one per restart point
//	                          packed at content end (before optional CRC32C footer)
//
// Entries at positions 0, 16, 32, ... are restart points (full keys).
// All other entries are delta entries (shared prefix + unshared suffix).
// The restart table is at the end so entries can be written directly as
// they are added without knowing the final restart count upfront.
// The reader locates the restart table at contentEnd - RestartCount * 2.

// Leaf header offsets (relative to start of page).
const (
	leafOffRestartInterval = HeaderSize     // 8
	leafOffRestartCount    = HeaderSize + 2 // 10
	leafEntryStart         = HeaderSize + 4 // 12 — fixed start of entry data
)

// Restart table entry size.
const restartTableEntrySize = 2 // uint16 offset

// LeafEntry holds decoded fields for a single leaf entry.
type LeafEntry struct {
	CellFlags uint8
	Key       []byte // fully reconstructed key

	// Inline value (CellFlags == 0).
	Value []byte // borrowed slice, nil for overflow/multi-value

	// Overflow (CellFlagOverflow set).
	OvflPage uint64
	TotalLen uint64

	// Subpage (CellFlagMultiValue set, CellFlagNestedTree clear).
	SubpageData []byte // raw subpage bytes (borrowed)

	// Nested B+tree (CellFlagMultiValue | CellFlagNestedTree).
	NestedRoot  uint64
	NestedCount uint64
}

// LeafReader provides read access to a prefix-compressed leaf page.
type LeafReader struct {
	buf             []byte
	cfg             PageConfig
	count           int
	restartCount    int
	restartInterval int
	restartTableOff int // byte offset of the restart table
}

// NewLeafReader wraps buf as a leaf page reader.
func NewLeafReader(buf []byte, cfg PageConfig) LeafReader {
	_, _, count, _ := ReadHeader(buf)
	ri := int(le.Uint16(buf[leafOffRestartInterval:]))
	rc := int(le.Uint16(buf[leafOffRestartCount:]))
	return LeafReader{
		buf:             buf,
		cfg:             cfg,
		count:           int(count),
		restartCount:    rc,
		restartInterval: ri,
		restartTableOff: cfg.ContentEnd() - rc*restartTableEntrySize,
	}
}

// Count returns the number of entries in the leaf.
func (r LeafReader) Count() int { return r.count }

// RestartCount returns the number of restart points.
func (r LeafReader) RestartCount() int { return r.restartCount }

// RestartInterval returns the restart interval.
func (r LeafReader) RestartInterval() int { return r.restartInterval }

// restartOffset returns the byte offset of the i-th restart point entry.
func (r LeafReader) restartOffset(i int) int {
	off := r.restartTableOff + i*restartTableEntrySize
	return int(le.Uint16(r.buf[off:]))
}

// decodeRestartEntry decodes a restart entry (full key) at the given byte
// offset. Returns the entry and the next byte offset after the entry.
func (r LeafReader) decodeRestartEntry(off int) (LeafEntry, int) {
	var e LeafEntry
	e.CellFlags = r.buf[off]
	off++

	keyLen := int(le.Uint16(r.buf[off:]))
	off += 2

	e.Key = r.buf[off : off+keyLen]
	off += keyLen

	return r.decodeValue(e, off)
}

// decodeDeltaEntry decodes a delta entry at the given byte offset, using
// prevKey to reconstruct the full key. keyBuf is used for key reconstruction
// (appended to, may be resliced). Returns the entry and the next byte offset.
func (r LeafReader) decodeDeltaEntry(off int, prevKey, keyBuf []byte) (LeafEntry, int, []byte) {
	var e LeafEntry
	e.CellFlags = r.buf[off]
	off++

	sharedLen := int(le.Uint16(r.buf[off:]))
	off += 2
	unsharedLen := int(le.Uint16(r.buf[off:]))
	off += 2

	// Reconstruct key in keyBuf.
	keyBuf = append(keyBuf[:0], prevKey[:sharedLen]...)
	keyBuf = append(keyBuf, r.buf[off:off+unsharedLen]...)
	off += unsharedLen
	e.Key = keyBuf

	e, off = r.decodeValue(e, off)
	return e, off, keyBuf
}

// decodeValue decodes the value portion of an entry starting at off.
func (r LeafReader) decodeValue(e LeafEntry, off int) (LeafEntry, int) {
	switch {
	case e.CellFlags&CellFlagOverflow != 0:
		e.OvflPage = le.Uint64(r.buf[off:])
		off += 8
		e.TotalLen = le.Uint64(r.buf[off:])
		off += 8

	case e.CellFlags&CellFlagMultiValue != 0:
		if e.CellFlags&CellFlagNestedTree != 0 {
			e.NestedRoot = le.Uint64(r.buf[off:])
			off += 8
			e.NestedCount = le.Uint64(r.buf[off:])
			off += 8
		} else {
			// Subpage: read Count+DataSize to determine total size.
			spDataSize := int(le.Uint16(r.buf[off+2:]))
			spTotalSize := subpageHeaderSize + spDataSize
			e.SubpageData = r.buf[off : off+spTotalSize]
			off += spTotalSize
		}

	default:
		// Inline value.
		valueLen := int(le.Uint32(r.buf[off:]))
		off += 4
		e.Value = r.buf[off : off+valueLen]
		off += valueLen
	}
	return e, off
}

// SearchLeaf performs the two-phase leaf lookup:
//  1. Binary search over restart table to find the restart group.
//  2. Linear scan within the group to find the exact key.
//
// keyBuf is a reusable buffer for key reconstruction. Returns the entry index
// (0-based within the page), the entry, and whether an exact match was found.
// If not found, index is the insertion point.
func (r LeafReader) SearchLeaf(target, keyBuf []byte) (index int, entry LeafEntry, found bool) {
	if r.count == 0 {
		return 0, LeafEntry{}, false
	}

	// Phase 1: binary search over restart points.
	lo, hi := 0, r.restartCount
	for lo < hi {
		mid := lo + (hi-lo)/2
		off := r.restartOffset(mid)
		e, _ := r.decodeRestartEntry(off)
		cmp := bytes.Compare(e.Key, target)
		if cmp < 0 {
			lo = mid + 1
		} else if cmp == 0 {
			return mid * r.restartInterval, e, true
		} else {
			hi = mid
		}
	}

	// lo is the first restart group whose first key > target.
	// The target is in group lo-1 (or doesn't exist).
	group := lo - 1
	if group < 0 {
		return 0, LeafEntry{}, false
	}

	// Phase 2: linear scan within the restart group.
	off := r.restartOffset(group)
	startIdx := group * r.restartInterval

	e, off := r.decodeRestartEntry(off)
	cmp := bytes.Compare(e.Key, target)
	if cmp == 0 {
		return startIdx, e, true
	}
	if cmp > 0 {
		return startIdx, LeafEntry{}, false
	}

	prevKey := e.Key
	endIdx := min(startIdx+r.restartInterval, r.count)

	for idx := startIdx + 1; idx < endIdx; idx++ {
		// After the first iteration, prevKey and keyBuf alias the same
		// backing array. decodeDeltaEntry handles this correctly: the
		// shared prefix is already in place (self-copy is a no-op),
		// and the unshared suffix is appended after it.
		e, off, keyBuf = r.decodeDeltaEntry(off, prevKey, keyBuf)
		cmp = bytes.Compare(e.Key, target)
		if cmp == 0 {
			return idx, e, true
		}
		if cmp > 0 {
			return idx, LeafEntry{}, false
		}
		prevKey = keyBuf
	}

	return endIdx, LeafEntry{}, false
}

// EntryAt decodes the entry at position idx. keyBuf is used for key
// reconstruction of delta entries.
func (r LeafReader) EntryAt(idx int, keyBuf []byte) (LeafEntry, []byte) {
	group := idx / r.restartInterval
	groupStart := group * r.restartInterval
	off := r.restartOffset(group)

	e, off := r.decodeRestartEntry(off)
	if groupStart == idx {
		return e, keyBuf
	}

	prevKey := e.Key
	for i := groupStart + 1; i <= idx; i++ {
		e, off, keyBuf = r.decodeDeltaEntry(off, prevKey, keyBuf)
		prevKey = keyBuf
	}
	return e, keyBuf
}

// IterEntries decodes all entries in the leaf, calling fn for each.
// fn receives the 0-based index and the decoded entry. If fn returns false,
// iteration stops. keyBuf is reused across delta entries.
func (r LeafReader) IterEntries(keyBuf []byte, fn func(idx int, e LeafEntry) bool) []byte {
	if r.count == 0 {
		return keyBuf
	}

	off := leafEntryStart
	var prevKey []byte

	for idx := range r.count {
		var e LeafEntry
		if idx%r.restartInterval == 0 {
			e, off = r.decodeRestartEntry(off)
			prevKey = e.Key
		} else {
			e, off, keyBuf = r.decodeDeltaEntry(off, prevKey, keyBuf)
			prevKey = keyBuf
		}
		if !fn(idx, e) {
			break
		}
	}
	return keyBuf
}

// LeafBuilder constructs a prefix-compressed leaf page by writing entries
// directly into the page buffer in forward order starting at offset 12.
// The restart table is written at Finish() time at the content end.
type LeafBuilder struct {
	buf            []byte
	cfg            PageConfig
	count          int
	restartOffsets []uint16 // absolute byte offsets of restart entries
	dataPos        int      // next write position (grows forward from leafEntryStart)
	prevKey        []byte   // previous key for delta computation
}

// NewLeafBuilder initializes a builder for writing into buf.
func NewLeafBuilder(buf []byte, cfg PageConfig) *LeafBuilder {
	return &LeafBuilder{
		buf:            buf,
		cfg:            cfg,
		dataPos:        leafEntryStart,
		restartOffsets: make([]uint16, 0, 32),
	}
}

// isRestart returns true if the next entry will be a restart point.
func (b *LeafBuilder) isRestart() bool {
	return b.count%RestartInterval == 0
}

// restartTableSize returns the byte size of the restart table with the
// given number of extra entries beyond what's already recorded.
func (b *LeafBuilder) restartTableSize(extra int) int {
	return (len(b.restartOffsets) + extra) * restartTableEntrySize
}

// AddInline adds an inline key-value entry. Returns false if insufficient space.
func (b *LeafBuilder) AddInline(key, value []byte) bool {
	return b.addEntry(key, 0, value, 0, 0, nil, 0, 0)
}

// AddOverflow adds an overflow reference entry. Returns false if insufficient space.
func (b *LeafBuilder) AddOverflow(key []byte, ovflPage, totalLen uint64) bool {
	return b.addEntry(key, CellFlagOverflow, nil, ovflPage, totalLen, nil, 0, 0)
}

// AddSubpage adds a multi-value subpage entry. Returns false if insufficient space.
func (b *LeafBuilder) AddSubpage(key, subpageData []byte) bool {
	return b.addEntry(key, CellFlagMultiValue, nil, 0, 0, subpageData, 0, 0)
}

// AddNestedTree adds a nested B+tree reference entry. Returns false if insufficient space.
func (b *LeafBuilder) AddNestedTree(key []byte, root, count uint64) bool {
	return b.addEntry(key, CellFlagMultiValue|CellFlagNestedTree, nil, 0, 0, nil, root, count)
}

func (b *LeafBuilder) addEntry(key []byte, cellFlags uint8, value []byte,
	ovflPage, totalLen uint64, subpageData []byte, nestedRoot, nestedCount uint64) bool {

	isRestart := b.isRestart()

	var sharedLen int
	unsharedKey := key
	if !isRestart && b.prevKey != nil {
		sharedLen = sharedPrefixLen(b.prevKey, key)
		unsharedKey = key[sharedLen:]
	}

	// Compute entry size.
	size := 1 // CellFlags
	if isRestart {
		size += 2 // KeyLen
		size += len(key)
	} else {
		size += 2 + 2 // SharedLen + UnsharedLen
		size += len(unsharedKey)
	}
	size += valuePartSize(cellFlags, value, subpageData)

	// Check space: entries grow forward from dataPos, restart table grows
	// backward from contentEnd. They must not overlap.
	extraRestart := 0
	if isRestart {
		extraRestart = 1
	}
	tableSize := b.restartTableSize(extraRestart)
	if b.dataPos+size+tableSize > b.cfg.ContentEnd() {
		return false
	}

	// Record restart offset before writing.
	if isRestart {
		b.restartOffsets = append(b.restartOffsets, uint16(b.dataPos))
	}

	// Write entry directly into the page buffer.
	off := b.dataPos

	b.buf[off] = cellFlags
	off++

	if isRestart {
		le.PutUint16(b.buf[off:], uint16(len(key)))
		off += 2
		copy(b.buf[off:], key)
		off += len(key)
	} else {
		le.PutUint16(b.buf[off:], uint16(sharedLen))
		off += 2
		le.PutUint16(b.buf[off:], uint16(len(unsharedKey)))
		off += 2
		copy(b.buf[off:], unsharedKey)
		off += len(unsharedKey)
	}

	off = writeValuePart(b.buf, off, cellFlags, value, ovflPage, totalLen, subpageData, nestedRoot, nestedCount)
	b.dataPos = off

	b.prevKey = append(b.prevKey[:0], key...)
	b.count++
	return true
}

// FreeSpace returns remaining usable bytes in the leaf page.
func (b *LeafBuilder) FreeSpace() int {
	return b.cfg.ContentEnd() - b.dataPos - b.restartTableSize(0)
}

// Finish writes the page header and restart table. Returns the entry count.
func (b *LeafBuilder) Finish() uint16 {
	count := uint16(b.count)
	WriteHeader(b.buf, TypeLeaf, 0, count, 0)
	le.PutUint16(b.buf[leafOffRestartInterval:], RestartInterval)
	le.PutUint16(b.buf[leafOffRestartCount:], uint16(len(b.restartOffsets)))

	// Write restart table at the content end.
	tableStart := b.cfg.ContentEnd() - len(b.restartOffsets)*restartTableEntrySize
	for i, off := range b.restartOffsets {
		le.PutUint16(b.buf[tableStart+i*restartTableEntrySize:], off)
	}

	return count
}

// Count returns the current number of entries added.
func (b *LeafBuilder) Count() int {
	return b.count
}

// valuePartSize returns the encoded size of the value portion.
func valuePartSize(cellFlags uint8, value, subpageData []byte) int {
	switch {
	case cellFlags&CellFlagOverflow != 0:
		return 8 + 8 // OvflPage + TotalLen
	case cellFlags&CellFlagMultiValue != 0:
		if cellFlags&CellFlagNestedTree != 0 {
			return 8 + 8 // Root + Count
		}
		return len(subpageData) // raw subpage bytes
	default:
		return 4 + len(value) // ValueLen + value bytes
	}
}

// writeValuePart writes the value portion into buf at off. Returns next offset.
func writeValuePart(buf []byte, off int, cellFlags uint8, value []byte,
	ovflPage, totalLen uint64, subpageData []byte, nestedRoot, nestedCount uint64) int {

	switch {
	case cellFlags&CellFlagOverflow != 0:
		le.PutUint64(buf[off:], ovflPage)
		off += 8
		le.PutUint64(buf[off:], totalLen)
		off += 8
	case cellFlags&CellFlagMultiValue != 0:
		if cellFlags&CellFlagNestedTree != 0 {
			le.PutUint64(buf[off:], nestedRoot)
			off += 8
			le.PutUint64(buf[off:], nestedCount)
			off += 8
		} else {
			copy(buf[off:], subpageData)
			off += len(subpageData)
		}
	default:
		le.PutUint32(buf[off:], uint32(len(value)))
		off += 4
		copy(buf[off:], value)
		off += len(value)
	}
	return off
}

// sharedPrefixLen returns the length of the common prefix between a and b.
func sharedPrefixLen(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
