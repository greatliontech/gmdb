package page

import (
	"bytes"
	"fmt"
	"sort"
)

// Branch-page layout per page-formats.md §Branch Page:
//
//	+-----------------------+ offset 0
//	| Page Header (8 bytes) | Type=TypeBranch, Count=N (cell count)
//	+-----------------------+ offset 8
//	| Ptr[0] (uint64)       | leftmost child pointer
//	+-----------------------+ offset 16
//	| Cell Directory        | N × (Offset uint16, KeyLen uint16) = N × 4 bytes
//	| ...                   |
//	+-----------------------+ grows forward
//	|       free space      |
//	+-----------------------+
//	| Cell Data 1           | each cell: KeyBytes || ChildPtr(uint64)
//	| Cell Data 0           | packed from end of page, grows backward
//	+-----------------------+ ContentEnd (PageSize - optional 8-byte footer)
//
// Cells store (Key, ChildPtr) — the ChildPtr is the right child of the
// separator key. For N cells there are N+1 child pointers: Ptr[0]
// (leftmost) + N ChildPtrs (one per cell).

const (
	// branchHeaderEnd is the byte offset where the cell directory
	// begins: after the common page header (8 bytes) and the
	// leftmost child pointer (8 bytes).
	branchHeaderEnd = 16

	// branchDirEntrySize is the per-cell directory size:
	// (Offset uint16, KeyLen uint16).
	branchDirEntrySize = 4

	// branchChildPtrSize is the trailing-pointer byte length on
	// each cell.
	branchChildPtrSize = 8

	// branchLeftmostOff is the byte offset of the leftmost child
	// pointer Ptr[0] within the page.
	branchLeftmostOff = HeaderSize
)

// BranchCell is the decoded form of one branch cell: a separator
// key + the right child pointer. The Key slice borrows from the
// underlying page buffer — do not retain past page-buffer lifetime.
type BranchCell struct {
	Key   []byte
	Child uint64
}

// BranchLeftmostChild returns Ptr[0] (the leftmost child pointer)
// from a branch page. Panics on a buffer too small to hold the
// leftmost-pointer field.
func BranchLeftmostChild(buf []byte) uint64 {
	_ = buf[branchLeftmostOff+branchChildPtrSize-1]
	return le.Uint64(buf[branchLeftmostOff:])
}

// SetBranchLeftmostChild stores Ptr[0]. Used by branch-page builders
// (insert / split / merge) to set the leftmost-child pointer
// directly without going through encode-all.
func SetBranchLeftmostChild(buf []byte, child uint64) {
	_ = buf[branchLeftmostOff+branchChildPtrSize-1]
	le.PutUint64(buf[branchLeftmostOff:], child)
}

// BranchCellCount returns the cell count N from the page header.
// Equivalent to ReadHeader(buf).count for a branch page; provided
// as a typed convenience so callers don't risk reading the header
// of a non-branch page.
func BranchCellCount(buf []byte) uint16 {
	typ, _, count, _ := ReadHeader(buf)
	if typ != TypeBranch {
		panic(fmt.Sprintf("page: BranchCellCount on type %d (want %d)", typ, TypeBranch))
	}
	return count
}

// BranchCellAt returns the i-th branch cell. Panics on a malformed
// page (cell directory entry points outside the page) or on
// out-of-range index.
//
// The returned BranchCell.Key slice references mmap-backed bytes
// within buf — callers MUST NOT modify and MUST NOT retain past
// the page buffer's lifetime.
func BranchCellAt(buf []byte, cfg Config, i uint16) BranchCell {
	cfg.mustValidate()
	n := BranchCellCount(buf)
	if i >= n {
		panic(fmt.Sprintf("page: BranchCellAt(%d) out of range [0, %d)", i, n))
	}
	dirOff := branchHeaderEnd + int(i)*branchDirEntrySize
	off := le.Uint16(buf[dirOff:])
	klen := le.Uint16(buf[dirOff+2:])
	end := int(off) + int(klen) + branchChildPtrSize
	if int(off) < branchHeaderEnd+int(n)*branchDirEntrySize || end > cfg.ContentEnd() {
		panic(fmt.Sprintf("page: BranchCellAt(%d) offset/len out of range: off=%d klen=%d", i, off, klen))
	}
	return BranchCell{
		Key:   buf[off : off+uint16(klen)],
		Child: le.Uint64(buf[off+uint16(klen):]),
	}
}

// BranchFreeSpace returns the number of unused bytes between the
// end of the cell directory and the start of the first packed cell.
// Used by branch insert/split logic to decide when to split.
//
// Implementation note: EncodeBranch packs cells in directory-index
// order with strictly-decreasing offsets, so the last directory
// entry (index N-1) always points to the lowest packed offset.
// The function nonetheless walks all N entries to find the minimum
// — defense in depth against future encoders (e.g., in-place
// insert that pre-allocates a hole) that may not preserve the
// monotonic-offset invariant. The O(N) walk is acceptable: branch
// pages hold tens of cells at typical key sizes, and the splitter
// path that consumes BranchFreeSpace is not in the read hot path.
func BranchFreeSpace(buf []byte, cfg Config) int {
	cfg.mustValidate()
	n := int(BranchCellCount(buf))
	dirEnd := branchHeaderEnd + n*branchDirEntrySize
	low := cfg.ContentEnd() // walking backward from here
	for i := range n {
		dirOff := branchHeaderEnd + i*branchDirEntrySize
		off := int(le.Uint16(buf[dirOff:]))
		if off < low {
			low = off
		}
	}
	return low - dirEnd
}

// EncodeBranchEmpty initialises a branch page with a single leftmost
// child pointer and zero cells. Used at root creation and at branch
// splits.
func EncodeBranchEmpty(buf []byte, cfg Config, leftmost uint64) {
	cfg.mustValidate()
	if len(buf) != int(cfg.PageSize) {
		panic(fmt.Sprintf("page: EncodeBranchEmpty buf len %d != PageSize %d", len(buf), cfg.PageSize))
	}
	clear(buf)
	WriteHeader(buf, TypeBranch, 0, 0)
	SetBranchLeftmostChild(buf, leftmost)
}

// EncodeBranch writes cells + leftmost into buf in sorted-key order.
// Convenience entry point for fresh branch pages (split products,
// initial root). The directory is laid out contiguously after the
// header+leftmost; cells are packed from the high end downward in
// the SAME order as the cell directory (cell 0 lowest offset → cell
// N-1 highest), so the iteration order on disk matches the
// cell-directory index.
//
// Returns an error if the cells don't fit. The caller computes
// "will this fit?" with BranchEncodedSize and acts BEFORE calling
// EncodeBranch — the error here is a defense-in-depth guard.
//
// Keys MUST be in ascending byte order; EncodeBranch verifies via a
// sort check and returns an error on violation (callers compose the
// cell slice; the codec doesn't reorder).
func EncodeBranch(buf []byte, cfg Config, leftmost uint64, cells []BranchCell) error {
	cfg.mustValidate()
	if len(buf) != int(cfg.PageSize) {
		return fmt.Errorf("page: EncodeBranch buf len %d != PageSize %d", len(buf), cfg.PageSize)
	}
	if !sort.SliceIsSorted(cells, func(i, j int) bool {
		return bytes.Compare(cells[i].Key, cells[j].Key) < 0
	}) {
		return fmt.Errorf("page: EncodeBranch cells not sorted by Key")
	}
	need := BranchEncodedSize(cfg, cells)
	if need > int(cfg.PageSize) {
		return fmt.Errorf("page: EncodeBranch %d cells need %d bytes, page is %d", len(cells), need, cfg.PageSize)
	}
	clear(buf)
	WriteHeader(buf, TypeBranch, uint16(len(cells)), 0)
	SetBranchLeftmostChild(buf, leftmost)

	// Cell data packs from ContentEnd downward; entries are placed
	// so iteration index i lands at successively LOWER offsets (so
	// the cell at index 0 has the highest offset, cell N-1 the
	// lowest). That keeps "find lowest packed offset" trivially
	// equal to "last cell's offset" for the free-space check.
	off := cfg.ContentEnd()
	for i, c := range cells {
		cellSize := len(c.Key) + branchChildPtrSize
		off -= cellSize
		copy(buf[off:], c.Key)
		le.PutUint64(buf[off+len(c.Key):], c.Child)
		dirOff := branchHeaderEnd + i*branchDirEntrySize
		le.PutUint16(buf[dirOff:], uint16(off))
		le.PutUint16(buf[dirOff+2:], uint16(len(c.Key)))
	}
	return nil
}

// BranchEncodedSize returns the byte size a branch page with the
// given cells would occupy. Used by the splitter to decide when a
// proposed cell set fits a page. Includes header(8) + leftmost(8) +
// cell directory + cells (key + child pointer each). DOES NOT
// include the optional footer — callers compare against
// cfg.ContentEnd() (which already excludes the footer).
func BranchEncodedSize(cfg Config, cells []BranchCell) int {
	size := branchHeaderEnd + len(cells)*branchDirEntrySize
	for _, c := range cells {
		size += len(c.Key) + branchChildPtrSize
	}
	return size
}

// DecodeBranch returns all cells from a branch page in directory
// order. Convenience for tests + tree-walk consumers; hot-path
// search uses BranchCellAt + binary search to avoid materializing
// the full slice.
func DecodeBranch(buf []byte, cfg Config) (leftmost uint64, cells []BranchCell) {
	cfg.mustValidate()
	n := BranchCellCount(buf)
	leftmost = BranchLeftmostChild(buf)
	if n == 0 {
		return leftmost, nil
	}
	cells = make([]BranchCell, n)
	for i := range n {
		cells[i] = BranchCellAt(buf, cfg, i)
	}
	return leftmost, cells
}

// BranchSearch binary-searches the branch's cells for the first
// separator key strictly greater than `target`. Returns the index
// `i` of that separator, or `n` if every separator is <= target.
//
// The descent caller uses i to pick the next child:
//   - i == 0   → Ptr[0]    (leftmost child)
//   - 0 < i ≤ N → ChildPtr of cell i-1 (separators are right-child
//     lower bounds, so i-1's child holds keys < separators[i])
//   - i == N   → ChildPtr of last cell  (rightmost child)
//
// Per page-formats.md: "binary-search the cell directory for the
// first separator Key[i] such that target < Key[i]". When target ==
// Key[i], the target belongs in the right child (descend via cell
// i's right child), which the i+1 → ChildPtr[i] mapping above
// captures because the binary search finds the LEAST i with target <
// Key[i], i.e., for target == Key[k] it returns k+1.
func BranchSearch(buf []byte, cfg Config, target []byte) uint16 {
	cfg.mustValidate()
	n := BranchCellCount(buf)
	// sort.Search returns the smallest i in [0, n] such that f(i)
	// is true; we use f(i) := target < Key[i].
	lo, hi := 0, int(n)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		c := BranchCellAt(buf, cfg, uint16(mid))
		if bytes.Compare(target, c.Key) < 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return uint16(lo)
}

// BranchChildAt returns the child pointer at descent index `i` from
// BranchSearch:
//   - i == 0 → leftmost (Ptr[0])
//   - 0 < i ≤ N → ChildPtr of cell i-1
func BranchChildAt(buf []byte, cfg Config, i uint16) uint64 {
	cfg.mustValidate()
	if i == 0 {
		return BranchLeftmostChild(buf)
	}
	return BranchCellAt(buf, cfg, i-1).Child
}

