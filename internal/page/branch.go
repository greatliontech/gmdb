package page

import "bytes"

// Branch pages store keys and child page pointers. Layout:
//
//	Page Header (8 bytes)
//	Ptr[0] (uint64)            — leftmost child pointer
//	Cell Directory             — array of (Offset uint16, KeyLen uint16), grows forward
//	     free space
//	Cell Data                  — packed from end of page, grows backward
//
// Each cell in the data area: Key bytes + ChildPtr (uint64).

// Branch header offsets (relative to start of page).
const (
	branchOffPtr0    = headerSize            // 8
	branchDirStart   = branchOffPtr0 + 8     // 16
)

// BranchReader provides zero-copy read access to a branch page.
type BranchReader struct {
	buf   []byte
	count int
}

// NewBranchReader wraps buf as a branch page reader.
func NewBranchReader(buf []byte) BranchReader {
	_, _, count, _ := ReadHeader(buf)
	return BranchReader{buf: buf, count: int(count)}
}

// Count returns the number of cells (keys) in the branch page.
func (r BranchReader) Count() int {
	return r.count
}

// Ptr0 returns the leftmost child page ID.
func (r BranchReader) Ptr0() uint64 {
	return le.Uint64(r.buf[branchOffPtr0:])
}

// cellDir returns (offset, keyLen) for cell i from the cell directory.
func (r BranchReader) cellDir(i int) (offset uint16, keyLen uint16) {
	dirOff := branchDirStart + i*cellDirEntrySize
	offset = le.Uint16(r.buf[dirOff:])
	keyLen = le.Uint16(r.buf[dirOff+2:])
	return
}

// Key returns the key bytes for cell i (borrowed slice into buf).
func (r BranchReader) Key(i int) []byte {
	offset, keyLen := r.cellDir(i)
	return r.buf[offset : offset+keyLen]
}

// ChildPtr returns the child page ID for cell i (the right child of key i).
func (r BranchReader) ChildPtr(i int) uint64 {
	offset, keyLen := r.cellDir(i)
	ptrOff := int(offset) + int(keyLen)
	return le.Uint64(r.buf[ptrOff:])
}

// Search performs binary search over the branch page to find the child to
// descend into for the given target key. Returns the child page ID and the
// cell index. The index is -1 if descending to Ptr0 (leftmost child), or
// 0..Count()-1 for the right child of cell i.
//
// Algorithm: find first separator Key[i] where target < Key[i].
// If found at i: descend to child left of i (ChildPtr[i-1] or Ptr0).
// If not found: descend to rightmost child (ChildPtr[Count-1]).
func (r BranchReader) Search(target []byte) (childPageID uint64, index int) {
	lo, hi := 0, r.count
	for lo < hi {
		mid := lo + (hi-lo)/2
		k := r.Key(mid)
		if bytes.Compare(target, k) < 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	// lo is the first index where target < Key[lo].
	if lo == 0 {
		return r.Ptr0(), -1
	}
	return r.ChildPtr(lo - 1), lo - 1
}

// BranchBuilder constructs a branch page in a []byte buffer.
// Cell directory grows forward from branchDirStart; cell data grows backward
// from the content end (PageSize or PageSize-CRC32Size).
type BranchBuilder struct {
	buf     []byte
	cfg     PageConfig
	count   int
	dirEnd  int // next byte after cell directory
	dataPos int // next data write position (grows backward)
}

// NewBranchBuilder initializes a builder for writing into buf.
func NewBranchBuilder(buf []byte, cfg PageConfig) *BranchBuilder {
	return &BranchBuilder{
		buf:     buf,
		cfg:     cfg,
		dirEnd:  branchDirStart,
		dataPos: cfg.ContentEnd(),
	}
}

// SetPtr0 writes the leftmost child pointer.
func (b *BranchBuilder) SetPtr0(pageID uint64) {
	le.PutUint64(b.buf[branchOffPtr0:], pageID)
}

// AddCell appends a cell (key + child pointer) to the branch.
// Cells must be added in sorted key order. Returns false if insufficient space.
func (b *BranchBuilder) AddCell(key []byte, childPtr uint64) bool {
	cellSize := len(key) + childPtrSize
	needed := cellDirEntrySize + cellSize
	if b.FreeSpace() < needed {
		return false
	}

	// Write cell data (backward from dataPos).
	b.dataPos -= cellSize
	copy(b.buf[b.dataPos:], key)
	le.PutUint64(b.buf[b.dataPos+len(key):], childPtr)

	// Write cell directory entry.
	le.PutUint16(b.buf[b.dirEnd:], uint16(b.dataPos))
	le.PutUint16(b.buf[b.dirEnd+2:], uint16(len(key)))
	b.dirEnd += cellDirEntrySize

	b.count++
	return true
}

// FreeSpace returns remaining bytes between directory and data area.
func (b *BranchBuilder) FreeSpace() int {
	return b.dataPos - b.dirEnd
}

// Finish writes the page header and returns the number of cells written.
func (b *BranchBuilder) Finish() uint16 {
	count := uint16(b.count)
	WriteHeader(b.buf, TypeBranch, 0, count, 0)
	return count
}

// Count returns the current number of cells added.
func (b *BranchBuilder) Count() int {
	return b.count
}
