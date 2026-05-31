package page

import (
	"bytes"
	"fmt"
	"sort"
)

// Branch-page layout per page-formats.md §Branch Page. Separators are
// prefix-truncated WITHIN the page: the single common prefix P of all
// separators on the page is stored once at the content tail, and each cell
// stores only the suffix key[len(P):] followed by its right child pointer.
//
//	+-----------------------+ offset 0
//	| Page Header (8 bytes) | Type=TypeBranch, Count=N (cell count)
//	+-----------------------+ offset 8
//	| Ptr[0] (uint64)       | leftmost child pointer
//	+-----------------------+ offset 16
//	| PrefixLen uint16      | length of the page-wide shared prefix P
//	| Reserved  uint16      | zero on write (keeps the directory at offset 20)
//	+-----------------------+ offset 20
//	| Cell Directory        | N × (Offset uint16, SuffixLen uint16) = N × 4 bytes
//	| ...                   | grows forward
//	+-----------------------+
//	|       free space      |
//	+-----------------------+
//	| Cell Data 1           | each cell: SuffixBytes || ChildPtr(uint64),
//	| Cell Data 0           | packed below the prefix region, growing backward
//	+-----------------------+ ContentEnd - PrefixLen
//	| Prefix bytes P        | the page-wide shared prefix
//	+-----------------------+ ContentEnd (PageSize - optional 8-byte footer)
//
// The full separator of cell i is P || Suffix[i]. Cells are stored in sorted
// key order, so P = commonPrefix(cell[0], cell[N-1]) — the common prefix of
// the whole sorted set. For N cells there are N+1 child pointers: Ptr[0]
// (leftmost) + N ChildPtrs (one per cell). When the separators share no
// prefix, PrefixLen is 0 and each cell stores its full key (the uncompressed
// case, no penalty beyond the fixed header fields).

const (
	// branchChildPtrSize is the trailing child-pointer byte length on each
	// cell.
	branchChildPtrSize = 8

	// branchDirEntrySize is the per-cell directory size:
	// (Offset uint16, SuffixLen uint16).
	branchDirEntrySize = 4

	// branchLeftmostOff is the byte offset of the leftmost child pointer
	// Ptr[0] within the page.
	branchLeftmostOff = HeaderSize // 8

	// branchPrefixLenOff is the byte offset of the PrefixLen uint16 (the
	// page-wide shared-separator-prefix length), after the leftmost child
	// pointer. A Reserved uint16 follows it (zero on write).
	branchPrefixLenOff = HeaderSize + branchChildPtrSize // 16

	// branchHeaderEnd is the byte offset where the cell directory begins:
	// after the page header (8), the leftmost child pointer (8), and the
	// PrefixLen + Reserved uint16 pair (4).
	branchHeaderEnd = branchPrefixLenOff + 4 // 20
)

// BranchCell is the decoded form of one branch cell: the full separator key
// + the right child pointer. The Key is the reconstructed full separator
// (P || suffix); DecodeBranch / BranchCellAt return it as an owned slice (the
// on-page prefix and suffix are non-contiguous, so the full key cannot alias
// the buffer), so callers may retain it past the page-buffer lifetime.
type BranchCell struct {
	Key   []byte
	Child uint64
}

// branchPrefixLen reads the page-wide shared-prefix length from the header.
func branchPrefixLen(buf []byte) int { return int(le.Uint16(buf[branchPrefixLenOff:])) }

// setBranchPrefixLen writes the page-wide shared-prefix length.
func setBranchPrefixLen(buf []byte, n int) { le.PutUint16(buf[branchPrefixLenOff:], uint16(n)) }

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

// SetBranchCellChild rewrites the child pointer of cell i in place.
// Used by the btree's in-place CoW updates when only a child
// pointer changes (no cell directory churn). i must be a valid
// cell index [0, N); the leftmost child (Ptr[0]) is rewritten via
// SetBranchLeftmostChild instead.
//
// The child pointer sits immediately after the cell's stored suffix, so a
// child-only rewrite is independent of the page-wide prefix and needs no
// re-encode.
func SetBranchCellChild(buf []byte, cfg Config, i uint16, child uint64) {
	cfg.mustValidate()
	n := BranchCellCount(buf)
	if i >= n {
		panic(fmt.Sprintf("page: SetBranchCellChild(%d) out of range [0, %d)", i, n))
	}
	dirOff := branchHeaderEnd + int(i)*branchDirEntrySize
	off := le.Uint16(buf[dirOff:])
	slen := le.Uint16(buf[dirOff+2:])
	le.PutUint64(buf[int(off)+int(slen):], child)
}

// BranchCellAt returns the i-th branch cell with its full reconstructed
// separator key (P || suffix). Panics on a malformed page (cell directory
// entry points outside the page) or on out-of-range index.
//
// The returned BranchCell.Key is a freshly-allocated owned slice (the
// on-page prefix and suffix are non-contiguous), so it is safe to retain and
// to modify. This is off the hot descent path — BranchSearch / BranchChildAt
// read the page directly without reconstructing keys.
func BranchCellAt(buf []byte, cfg Config, i uint16) BranchCell {
	cfg.mustValidate()
	n := BranchCellCount(buf)
	if i >= n {
		panic(fmt.Sprintf("page: BranchCellAt(%d) out of range [0, %d)", i, n))
	}
	ce := cfg.ContentEnd()
	m := branchPrefixLen(buf)
	prefixStart := ce - m
	dirOff := branchHeaderEnd + int(i)*branchDirEntrySize
	off := int(le.Uint16(buf[dirOff:]))
	slen := int(le.Uint16(buf[dirOff+2:]))
	end := off + slen + branchChildPtrSize
	if off < branchHeaderEnd+int(n)*branchDirEntrySize || end > prefixStart {
		panic(fmt.Sprintf("page: BranchCellAt(%d) offset/len out of range: off=%d slen=%d prefixStart=%d", i, off, slen, prefixStart))
	}
	key := make([]byte, m+slen)
	copy(key, buf[prefixStart:ce])
	copy(key[m:], buf[off:off+slen])
	return BranchCell{
		Key:   key,
		Child: le.Uint64(buf[off+slen:]),
	}
}

// branchCellChild reads the right child pointer of cell i (0-based) directly
// from the directory + cell data, without reconstructing the separator key.
// Hot-path helper for BranchChildAt.
func branchCellChild(buf []byte, i uint16) uint64 {
	dirOff := branchHeaderEnd + int(i)*branchDirEntrySize
	off := int(le.Uint16(buf[dirOff:]))
	slen := int(le.Uint16(buf[dirOff+2:]))
	return le.Uint64(buf[off+slen:])
}

// BranchFreeSpace returns the number of unused bytes between the
// end of the cell directory and the start of the first packed cell.
// Used by branch insert/split logic to decide when to split.
//
// The page-wide prefix region sits above all cells (at the content tail), so
// the free-space window is [dirEnd, lowestCellOffset); the walk starts from
// the prefix region's low edge (ContentEnd - PrefixLen) so an empty page
// (no cells) reports the correct free span.
//
// Implementation note: EncodeBranch packs cells in directory-index
// order with strictly-decreasing offsets, so the last directory
// entry (index N-1) always points to the lowest packed offset.
// The function nonetheless walks all N entries to find the minimum
// — defense in depth against future encoders that may not preserve the
// monotonic-offset invariant. The O(N) walk is acceptable: branch
// pages hold tens of cells at typical key sizes, and the splitter
// path that consumes BranchFreeSpace is not in the read hot path.
func BranchFreeSpace(buf []byte, cfg Config) int {
	cfg.mustValidate()
	n := int(BranchCellCount(buf))
	dirEnd := branchHeaderEnd + n*branchDirEntrySize
	low := cfg.ContentEnd() - branchPrefixLen(buf) // walking backward from the prefix region
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
	// PrefixLen + Reserved are left zero by clear.
}

// EncodeBranch writes cells + leftmost into buf in sorted-key order, applying
// within-page prefix truncation (page-formats.md §Branch Page). The page-wide
// prefix P = commonPrefix(cells[0], cells[N-1]) is stored once at the content
// tail; each cell stores its suffix key[len(P):] + child pointer. The
// directory is laid out contiguously after the header; cells are packed from
// just below the prefix region downward in the SAME order as the directory
// (cell 0 highest offset → cell N-1 lowest), so the on-disk iteration order
// matches the cell-directory index.
//
// Returns an error if the cells don't fit cfg.ContentEnd(). The caller
// computes "will this fit?" with BranchEncodedSize and acts BEFORE calling
// EncodeBranch — the error here is a defense-in-depth guard.
//
// Keys MUST be in ascending byte order; EncodeBranch verifies via a sort
// check and returns an error on violation (callers compose the cell slice;
// the codec doesn't reorder). The output is a pure function of (cfg,
// leftmost, cells): P, suffixes, directory, and packing order are all
// deterministic (page-formats.md §Leaf Split deterministic-encoding
// invariant, for branch pages).
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
	ce := cfg.ContentEnd()
	need := BranchEncodedSize(cfg, cells)
	if need > ce {
		return fmt.Errorf("page: EncodeBranch %d cells need %d bytes, content end is %d", len(cells), need, ce)
	}
	clear(buf)
	WriteHeader(buf, TypeBranch, uint16(len(cells)), 0)
	SetBranchLeftmostChild(buf, leftmost)

	var m int
	if len(cells) > 0 {
		m = sharedPrefixLen(cells[0].Key, cells[len(cells)-1].Key)
		// Prefix bytes at the content tail: [ContentEnd-m, ContentEnd).
		// Guarded by len(cells) > 0 so cells[0] is never indexed on an
		// empty (leftmost-only) branch, where m is 0 anyway.
		copy(buf[ce-m:ce], cells[0].Key[:m])
	}
	setBranchPrefixLen(buf, m)

	// Cell data packs from just below the prefix region downward; entries
	// are placed so iteration index i lands at successively LOWER offsets
	// (cell 0 highest, cell N-1 lowest). That keeps "find lowest packed
	// offset" trivially equal to "last cell's offset" for the free-space
	// check.
	off := ce - m
	for i, c := range cells {
		suffix := c.Key[m:]
		cellSize := len(suffix) + branchChildPtrSize
		off -= cellSize
		copy(buf[off:], suffix)
		le.PutUint64(buf[off+len(suffix):], c.Child)
		dirOff := branchHeaderEnd + i*branchDirEntrySize
		le.PutUint16(buf[dirOff:], uint16(off))
		le.PutUint16(buf[dirOff+2:], uint16(len(suffix)))
	}
	return nil
}

// BranchEncodedSizeOf returns the encoded byte size of a branch page holding
// n cells whose full separator keys total keyLenSum bytes and share a common
// prefix of prefixLen bytes. It is the single source of truth for the
// (non-additive) branch sizing: the shared prefix is stored once, so a cell's
// stored cost is its suffix (len(key)-prefixLen) + child pointer + directory
// entry. BranchEncodedSize, the byte-balanced branch splitter, and the
// bulk-load branch builder all size prospective pages through it.
//
// Branch sizing is non-additive because prefixLen depends on the whole cell
// set: adding a separator that shares less prefix lengthens every other
// cell's stored suffix. There is therefore no fixed per-cell cost (the prior
// BranchCellCost) — callers track (n, keyLenSum, prefixLen) and recompute.
func BranchEncodedSizeOf(n, keyLenSum, prefixLen int) int {
	// header + N directory entries + N child pointers + the suffixes
	// (keyLenSum - N*prefixLen) + the shared prefix stored once (prefixLen):
	//   = branchHeaderEnd + N*(dir+child) + keyLenSum - (N-1)*prefixLen
	return branchHeaderEnd + n*(branchDirEntrySize+branchChildPtrSize) + keyLenSum - (n-1)*prefixLen
}

// BranchEncodedSize returns the byte size a branch page with the given cells
// would occupy under within-page prefix truncation. Used by the splitter and
// bulk-load to decide when a proposed cell set fits a page (compared against
// cfg.ContentEnd(), which already excludes the optional footer). cfg is
// accepted for call-site stability; the size is configuration-independent.
func BranchEncodedSize(cfg Config, cells []BranchCell) int {
	if len(cells) == 0 {
		return BranchEncodedSizeOf(0, 0, 0)
	}
	m := sharedPrefixLen(cells[0].Key, cells[len(cells)-1].Key)
	keyLenSum := 0
	for _, c := range cells {
		keyLenSum += len(c.Key)
	}
	return BranchEncodedSizeOf(len(cells), keyLenSum, m)
}

// BranchLogicalSize returns the branch's UNCOMPRESSED content size — the
// bytes the cells would occupy with NO within-page prefix truncation
// (Σ full-key + child-pointer + directory costs; equals
// BranchEncodedSizeOf(n, keyLenSum, 0)). This is the measure for the
// range-delete.md §Invariants fill-floor: within-page compression is a
// storage optimization that must not, by itself, make a logically-dense
// branch look underfull (a fanout-many same-cluster branch compresses to few
// bytes yet carries plenty of children). So "is this branch underfull?" is
// asked of the LOGICAL content (this function), while "does this cell set fit
// a page?" uses the PHYSICAL compressed size (BranchEncodedSize). The two
// notions coincide exactly when the separators share no prefix.
func BranchLogicalSize(cells []BranchCell) int {
	keyLenSum := 0
	for _, c := range cells {
		keyLenSum += len(c.Key)
	}
	return BranchEncodedSizeOf(len(cells), keyLenSum, 0)
}

// DecodeBranch returns all cells from a branch page in directory order, each
// with its full reconstructed key. Convenience for tests + tree-walk /
// split / merge consumers; hot-path search uses BranchSearch + BranchChildAt
// to avoid materializing keys.
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

// BranchSearch binary-searches the branch's separators for the first one
// strictly greater than `target`, returning the descent index `i` (or `n` if
// every separator is <= target) per page-formats.md §Branch Page.
//
// Separators are prefix-truncated within the page, so the search is two-step
// and entirely zero-copy (no full-key reconstruction):
//
//	1. Compare target against the page-wide prefix P. If target does not start
//	   with P, it sorts before every separator (descend leftmost, i=0) or
//	   after every separator (descend rightmost, i=n) — decided by
//	   bytes.Compare(target, P).
//	2. Otherwise binary-search target[len(P):] against the cells' suffixes.
//
// The descent caller uses i to pick the next child:
//   - i == 0   → Ptr[0]    (leftmost child)
//   - 0 < i ≤ N → ChildPtr of cell i-1 (separators are right-child lower
//     bounds, so i-1's child holds keys < separators[i])
//   - i == N   → ChildPtr of last cell  (rightmost child)
//
// When target == Key[k], step 2 returns k+1 (the suffix search finds the
// LEAST i with target[len(P):] < suffix[i]), so the target descends into that
// separator's right child — separators are right-child lower bounds.
func BranchSearch(buf []byte, cfg Config, target []byte) uint16 {
	cfg.mustValidate()
	n := int(BranchCellCount(buf))
	if n == 0 {
		return 0
	}
	ce := cfg.ContentEnd()
	m := branchPrefixLen(buf)
	prefix := buf[ce-m : ce]

	// Step 1: locate target relative to the page-wide prefix.
	if len(target) < m || !bytes.Equal(target[:m], prefix) {
		// target does not start with P.
		if bytes.Compare(target, prefix) < 0 {
			return 0 // target < every separator → leftmost child
		}
		return uint16(n) // target > every separator → rightmost child
	}

	// Step 2: binary-search the suffix tail against the cells' suffixes.
	// f(i) := tail < suffix[i]; sort.Search returns the least such i.
	tail := target[m:]
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		dirOff := branchHeaderEnd + mid*branchDirEntrySize
		off := int(le.Uint16(buf[dirOff:]))
		slen := int(le.Uint16(buf[dirOff+2:]))
		if bytes.Compare(tail, buf[off:off+slen]) < 0 {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return uint16(lo)
}

// ShortestSeparator returns the shortest byte string S satisfying
// `left < S <= right` — the prefix-truncated separator used at
// branch insertion time per page-formats.md §Prefix-Truncated
// Branch Keys. Constructed as the common prefix of left and right
// extended by exactly one byte from right at the first divergence
// position.
//
// This is the CROSS-LEVEL truncation (the separator distinguishes left from
// right subtree); it is independent of the WITHIN-page prefix truncation that
// EncodeBranch applies to the resulting separator set.
//
// Precondition: left < right (strict). Callers compute this from
// the boundary keys of a freshly-split pair (last key of left leaf,
// first key of right leaf), which are always strictly ordered
// because the source leaf had no duplicate keys.
//
// Edge cases:
//   - left is a strict prefix of right: returns right[:len(left)+1]
//     — one byte past the prefix.
//   - left and right differ at position p < min(len): returns
//     right[:p+1]. The separator equals right at the divergence
//     byte, so right ≥ S (with right == S iff right's length is
//     p+1). left differs at p, so left < S (left's byte at p is
//     strictly less than right's byte at p, since left < right).
//
// Panics if left >= right — that violates the precondition and
// indicates the caller (splitter) generated an invalid boundary.
func ShortestSeparator(left, right []byte) []byte {
	if bytes.Compare(left, right) >= 0 {
		panic(fmt.Sprintf("page: ShortestSeparator left >= right: left=%q right=%q", left, right))
	}
	n := min(len(left), len(right))
	for i := range n {
		if left[i] != right[i] {
			// Divergence at i — separator is right[:i+1].
			sep := make([]byte, i+1)
			copy(sep, right[:i+1])
			return sep
		}
	}
	// Common prefix exhausted without divergence — left is a strict
	// prefix of right (we ruled out left==right via the precondition
	// check). Extend by the byte at position n in right.
	sep := make([]byte, n+1)
	copy(sep, right[:n+1])
	return sep
}

// ValidateBranch reports whether buf is a structurally well-formed branch
// page: the header type is TypeBranch, the buffer covers the page content
// region, the page-wide prefix region fits above the directory, and every
// cell's (Offset, SuffixLen) points within the cell-data region [dirEnd,
// ContentEnd-PrefixLen) with room for the trailing 8-byte child pointer. It
// returns a non-nil error (wrapping ErrCorrupted) on any violation and never
// panics — the decoder-robustness contract ("total over input, never panics
// on a forged page"), so reachability walks (Check, FreeSubtree) and the
// btree readers can call BranchSearch / BranchCellAt / BranchChildAt safely
// after Validate passes. Mirrors the per-cell bounds those readers assume.
func ValidateBranch(buf []byte, cfg Config) error {
	contentEnd := cfg.ContentEnd()
	if len(buf) < contentEnd {
		return fmt.Errorf("%w: branch buffer len %d < content end %d", ErrCorrupted, len(buf), contentEnd)
	}
	typ, _, n, _ := ReadHeader(buf)
	if typ != TypeBranch {
		return fmt.Errorf("%w: branch page has type %d (want %d)", ErrCorrupted, typ, TypeBranch)
	}
	m := branchPrefixLen(buf)
	prefixStart := contentEnd - m
	dirEnd := branchHeaderEnd + int(n)*branchDirEntrySize
	if prefixStart < dirEnd {
		// Covers an over-long PrefixLen and a directory that overruns the
		// content/prefix region (prefixStart <= contentEnd always since m>=0).
		return fmt.Errorf("%w: branch prefix region (PrefixLen=%d) overlaps cell directory (%d cells): prefixStart=%d dirEnd=%d",
			ErrCorrupted, m, n, prefixStart, dirEnd)
	}
	for i := 0; i < int(n); i++ {
		dirOff := branchHeaderEnd + i*branchDirEntrySize
		off := int(le.Uint16(buf[dirOff:]))
		slen := int(le.Uint16(buf[dirOff+2:]))
		end := off + slen + branchChildPtrSize
		if off < dirEnd || end > prefixStart {
			return fmt.Errorf("%w: branch cell %d offset/len out of range: off=%d slen=%d end=%d (dirEnd=%d prefixStart=%d)",
				ErrCorrupted, i, off, slen, end, dirEnd, prefixStart)
		}
	}
	return nil
}

// BranchChildAt returns the child pointer at descent index `i` from
// BranchSearch:
//   - i == 0 → leftmost (Ptr[0])
//   - 0 < i ≤ N → ChildPtr of cell i-1
//
// Reads the child pointer directly (no separator-key reconstruction), so it
// is allocation-free on the hot descent path.
func BranchChildAt(buf []byte, cfg Config, i uint16) uint64 {
	cfg.mustValidate()
	if i == 0 {
		return BranchLeftmostChild(buf)
	}
	return branchCellChild(buf, i-1)
}
