package page

import (
	"bytes"
	"fmt"
)

// Plain-branch page layout per page-formats.md §Plain Branch: full
// separator bytes per cell, addressed by an offset+length directory.
//
//	+-----------------------+ offset 0
//	| Page Header (8 bytes) | Type=TypeBranch, Count=N (cell count)
//	+-----------------------+ offset 8
//	| Ptr[0] (uint64)       | leftmost child pointer
//	+-----------------------+ offset 16
//	| Cell Directory        | N × (Offset uint16, KeyLen uint16) = N × 4 bytes
//	| ...                   | grows forward
//	+-----------------------+
//	|       free space      |
//	+-----------------------+
//	| Cell Data 1           | each cell: KeyBytes || ChildPtr(uint64),
//	| Cell Data 0           | packed backward from ContentEnd
//	+-----------------------+ ContentEnd (PageSize - optional 8-byte footer)
//
// Cells are stored in sorted key order. For N cells there are N+1 child
// pointers: Ptr[0] (leftmost) + N ChildPtrs (one per cell). Every
// binary-search probe compares directly against stored separator bytes
// — no prefix gate, no reconstruction (the latency-floor property the
// layout exists for; the density alternative is the segregated branch).
//
// An OVERFLOW branch cell (separator longer than the inline threshold
// T) is marked in bit 15 of the directory's KeyLen; the low 15 bits
// give the inline length — always exactly T — and the cell data is
// Inline(T bytes) || KeyExtPage(u64) || KeyTotalLen(u32) ||
// ChildPtr(u64) (page-formats.md §Overflow-Key Cells).

const (
	// branchChildPtrSize is the trailing child-pointer byte length on each
	// cell.
	branchChildPtrSize = 8

	// branchDirEntrySize is the per-cell directory size:
	// (Offset uint16, KeyLen uint16).
	branchDirEntrySize = 4

	// branchLeftmostOff is the byte offset of the leftmost child pointer
	// Ptr[0] within the page.
	branchLeftmostOff = HeaderSize // 8

	// branchHeaderEnd is the byte offset where the cell directory begins:
	// after the page header (8) and the leftmost child pointer (8).
	branchHeaderEnd = branchLeftmostOff + branchChildPtrSize // 16
)

// BranchCell is the decoded form of one branch cell: the separator key +
// the right child pointer. DecodeBranch / BranchCellAt return Key as an
// owned slice, so callers may retain it past the page-buffer lifetime.
//
// For OVERFLOW branch cells (KeyExtPage != 0 — page-formats.md
// §Overflow-Key Cells) Key holds only the RESIDENT first
// InlineThreshold bytes of the separator (`sep[0:T]`); the extent run
// at KeyExtPage holds sep[T:], and KeyTotalLen is the full separator
// length. The extent cut is fixed at byte T of the FULL key,
// page-independent, so moving a cell between pages carries the extent
// by reference.
type BranchCell struct {
	Key         []byte
	Child       uint64
	KeyExtPage  uint64
	KeyTotalLen uint32
}

// IsOverflowKey reports whether the cell's separator exceeds the inline
// threshold and carries a key-extent reference.
func (c BranchCell) IsOverflowKey() bool { return c.KeyExtPage != 0 }

// branchDirKeyOverflowBit marks an overflow branch cell in the
// directory's KeyLen field (bit 15); the low 15 bits give the inline
// length, always exactly InlineThreshold (page-formats.md §Plain
// Branch). InlineThreshold fits 15 bits at every page size.
const branchDirKeyOverflowBit = 0x8000

// branchKeyExtRefSize is the byte length of the key-extent reference in
// an overflow branch cell's data: KeyExtPage(u64) + KeyTotalLen(u32).
const branchKeyExtRefSize = 12

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
// The child pointer sits immediately after the cell's stored key
// bytes (and extent reference, for overflow cells), so a child-only
// rewrite needs no re-encode.
func SetBranchCellChild(buf []byte, cfg Config, i uint16, child uint64) {
	cfg.MustValidate()
	n := BranchCellCount(buf)
	if i >= n {
		panic(fmt.Sprintf("page: SetBranchCellChild(%d) out of range [0, %d)", i, n))
	}
	dirOff := branchHeaderEnd + int(i)*branchDirEntrySize
	off := int(le.Uint16(buf[dirOff:]))
	off += branchCellChildSkip(le.Uint16(buf[dirOff+2:]))
	le.PutUint64(buf[off:], child)
}

// SetBranchCellKeyExtPage rewrites the KeyExtPage field of overflow
// branch cell i in place — the branch analog of the leaf
// PatchKeyExtRefs primitive (size-identical: the reference is a fixed
// u64 after the inline key bytes; KeyTotalLen is immutable). Panics
// on an out-of-range index or a non-overflow cell — a caller asking to
// repoint a cell with no extent is a programming error.
func SetBranchCellKeyExtPage(buf []byte, cfg Config, i uint16, extPage uint64) {
	cfg.MustValidate()
	n := BranchCellCount(buf)
	if i >= n {
		panic(fmt.Sprintf("page: SetBranchCellKeyExtPage(%d) out of range [0, %d)", i, n))
	}
	dirOff := branchHeaderEnd + int(i)*branchDirEntrySize
	raw := le.Uint16(buf[dirOff+2:])
	if raw&branchDirKeyOverflowBit == 0 {
		panic(fmt.Sprintf("page: SetBranchCellKeyExtPage(%d) on a non-overflow cell", i))
	}
	off := int(le.Uint16(buf[dirOff:])) + int(raw&^branchDirKeyOverflowBit)
	le.PutUint64(buf[off:], extPage)
}

// branchCellChildSkip returns the byte distance from a cell's data start
// to its child pointer, given the raw directory KeyLen field: the
// inline key bytes plus the 12-byte key-extent reference when the
// overflow marker (bit 15) is set.
func branchCellChildSkip(rawKeyLen uint16) int {
	n := int(rawKeyLen &^ branchDirKeyOverflowBit)
	if rawKeyLen&branchDirKeyOverflowBit != 0 {
		n += branchKeyExtRefSize
	}
	return n
}

// BranchCellAt returns the i-th branch cell. Panics on a malformed page
// (cell directory entry points outside the page) or on out-of-range
// index.
//
// The returned BranchCell.Key is a freshly-allocated owned slice, so it
// is safe to retain and to modify. This is off the hot descent path —
// BranchSearch / BranchChildAt read the page directly without
// materializing keys.
func BranchCellAt(buf []byte, cfg Config, i uint16) BranchCell {
	cfg.MustValidate()
	n := BranchCellCount(buf)
	if i >= n {
		panic(fmt.Sprintf("page: BranchCellAt(%d) out of range [0, %d)", i, n))
	}
	ce := cfg.ContentEnd()
	dirOff := branchHeaderEnd + int(i)*branchDirEntrySize
	off := int(le.Uint16(buf[dirOff:]))
	raw := le.Uint16(buf[dirOff+2:])
	klen := int(raw &^ branchDirKeyOverflowBit)
	ovk := raw&branchDirKeyOverflowBit != 0
	end := off + branchCellChildSkip(raw) + branchChildPtrSize
	if off < branchHeaderEnd+int(n)*branchDirEntrySize || end > ce {
		panic(fmt.Sprintf("page: BranchCellAt(%d) offset/len out of range: off=%d klen=%d contentEnd=%d", i, off, klen, ce))
	}
	c := BranchCell{Key: bytes.Clone(buf[off : off+klen])}
	p := off + klen
	if ovk {
		c.KeyExtPage = le.Uint64(buf[p:])
		c.KeyTotalLen = le.Uint32(buf[p+8:])
		p += branchKeyExtRefSize
	}
	c.Child = le.Uint64(buf[p:])
	return c
}

// branchCellChild reads the right child pointer of cell i (0-based) directly
// from the directory + cell data, without materializing the separator key.
// Hot-path helper for BranchChildAt.
func branchCellChild(buf []byte, i uint16) uint64 {
	dirOff := branchHeaderEnd + int(i)*branchDirEntrySize
	off := int(le.Uint16(buf[dirOff:]))
	off += branchCellChildSkip(le.Uint16(buf[dirOff+2:]))
	return le.Uint64(buf[off:])
}

// BranchFreeSpace returns the number of unused bytes between the
// end of the cell directory and the start of the lowest packed cell.
// Used by branch insert/split logic to decide when to split.
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
	cfg.MustValidate()
	n := int(BranchCellCount(buf))
	dirEnd := branchHeaderEnd + n*branchDirEntrySize
	low := cfg.ContentEnd()
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
	cfg.MustValidate()
	if len(buf) != int(cfg.PageSize) {
		panic(fmt.Sprintf("page: EncodeBranchEmpty buf len %d != PageSize %d", len(buf), cfg.PageSize))
	}
	clear(buf)
	WriteHeader(buf, TypeBranch, 0, 0)
	SetBranchLeftmostChild(buf, leftmost)
}

// EncodeBranch writes cells + leftmost into buf in sorted-key order
// (page-formats.md §Plain Branch). The directory is laid out
// contiguously after the header; cells are packed from ContentEnd
// downward in the SAME order as the directory (cell 0 highest offset →
// cell N-1 lowest), so the on-disk iteration order matches the
// cell-directory index.
//
// Returns an error if the cells don't fit cfg.ContentEnd(). The caller
// computes "will this fit?" with BranchEncodedSize and acts BEFORE calling
// EncodeBranch — the error here is a defense-in-depth guard.
//
// Keys MUST be in ascending byte order; EncodeBranch verifies via a sort
// check and returns an error on violation (callers compose the cell slice;
// the codec doesn't reorder). The output is a pure function of (cfg,
// leftmost, cells): directory and packing order are deterministic
// (page-formats.md §Leaf Split deterministic-encoding invariant, for
// branch pages).
func EncodeBranch(buf []byte, cfg Config, leftmost uint64, cells []BranchCell) error {
	cfg.MustValidate()
	if len(buf) != int(cfg.PageSize) {
		return fmt.Errorf("page: EncodeBranch buf len %d != PageSize %d", len(buf), cfg.PageSize)
	}
	// Sort check over the RESIDENT keys. Two adjacent overflow cells may
	// legitimately share their resident first-T bytes (their order lives
	// in the extents, unreadable here); any other equality or inversion
	// is a caller bug.
	for i := 1; i < len(cells); i++ {
		c := bytes.Compare(cells[i-1].Key, cells[i].Key)
		if c > 0 || (c == 0 && !(cells[i-1].IsOverflowKey() && cells[i].IsOverflowKey())) {
			return fmt.Errorf("page: EncodeBranch cells not sorted by Key")
		}
	}
	t := cfg.InlineThreshold()
	for i, c := range cells {
		if c.IsOverflowKey() && len(c.Key) != t {
			return fmt.Errorf("page: EncodeBranch overflow cell %d resident length %d != inline threshold %d", i, len(c.Key), t)
		}
		if !c.IsOverflowKey() && len(c.Key) > t {
			return fmt.Errorf("page: EncodeBranch cell %d inline key length %d exceeds inline threshold %d", i, len(c.Key), t)
		}
	}
	ce := cfg.ContentEnd()
	need := BranchEncodedSize(cfg, cells)
	if need > ce {
		return fmt.Errorf("page: EncodeBranch %d cells need %d bytes, content end is %d", len(cells), need, ce)
	}
	clear(buf)
	WriteHeader(buf, TypeBranch, uint16(len(cells)), 0)
	SetBranchLeftmostChild(buf, leftmost)

	// Cell data packs from ContentEnd downward; entries are placed so
	// iteration index i lands at successively LOWER offsets (cell 0
	// highest, cell N-1 lowest).
	off := ce
	for i, c := range cells {
		cellSize := len(c.Key) + branchChildPtrSize
		rawKeyLen := uint16(len(c.Key))
		if c.IsOverflowKey() {
			cellSize += branchKeyExtRefSize
			rawKeyLen |= branchDirKeyOverflowBit
		}
		off -= cellSize
		copy(buf[off:], c.Key)
		p := off + len(c.Key)
		if c.IsOverflowKey() {
			le.PutUint64(buf[p:], c.KeyExtPage)
			le.PutUint32(buf[p+8:], c.KeyTotalLen)
			p += branchKeyExtRefSize
		}
		le.PutUint64(buf[p:], c.Child)
		dirOff := branchHeaderEnd + i*branchDirEntrySize
		le.PutUint16(buf[dirOff:], uint16(off))
		le.PutUint16(buf[dirOff+2:], rawKeyLen)
	}
	return nil
}

// BranchEncodedSizeOf returns the encoded byte size of a plain-branch
// page holding n cells whose stored (resident) key bytes total
// keyLenSum, extRefs of which carry a 12-byte key-extent reference.
// Plain-branch sizing is additive: header + per-cell directory entry +
// key bytes + child pointer (+ extent reference).
func BranchEncodedSizeOf(n, keyLenSum, extRefs int) int {
	return branchHeaderEnd + n*(branchDirEntrySize+branchChildPtrSize) + keyLenSum + extRefs*branchKeyExtRefSize
}

// BranchEncodedSize returns the byte size a plain-branch page with the
// given cells would occupy. Used by the splitter and bulk-load to
// decide when a proposed cell set fits a page (compared against
// cfg.ContentEnd(), which already excludes the optional footer). cfg is
// accepted for call-site stability; the size is configuration-independent.
// Overflow cells contribute their RESIDENT key bytes (Key holds sep[0:T])
// plus the 12-byte extent reference.
func BranchEncodedSize(cfg Config, cells []BranchCell) int {
	keyLenSum, extRefs := 0, 0
	for _, c := range cells {
		keyLenSum += len(c.Key)
		if c.IsOverflowKey() {
			extRefs++
		}
	}
	return BranchEncodedSizeOf(len(cells), keyLenSum, extRefs)
}

// BranchLogicalSize returns the branch's LOGICAL content size — the
// bytes the cells occupy with no within-page compression. This is the
// measure for the range-delete.md §Invariants fill-floor: within-page
// compression is a storage optimization that must not, by itself, make
// a logically-dense branch look underfull. For the PLAIN branch,
// logical and physical content coincide (full separators are stored;
// there is no within-page compression), so this equals
// BranchEncodedSize; the segregated branch layout diverges. An
// overflow cell's logical content is its RESIDENT bytes (the first-T
// key slice + the 12-byte extent reference), never KeyTotalLen: the
// floor and the redistribute balance measure page utilisation, and
// counting extent bytes would let one giant separator satisfy any
// floor while the page is physically near-empty (range-delete.md
// §Invariants).
func BranchLogicalSize(cells []BranchCell) int {
	keyLenSum, extRefs := 0, 0
	for _, c := range cells {
		keyLenSum += len(c.Key)
		if c.IsOverflowKey() {
			extRefs++
		}
	}
	return BranchEncodedSizeOf(len(cells), keyLenSum, extRefs)
}

// DecodeBranch returns all cells from a branch page in directory order.
// Convenience for tests + tree-walk / split / merge consumers; hot-path
// search uses BranchSearch + BranchChildAt to avoid materializing keys.
func DecodeBranch(buf []byte, cfg Config) (leftmost uint64, cells []BranchCell) {
	cfg.MustValidate()
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
// every separator is <= target) per page-formats.md §Plain Branch. Every
// probe compares target directly against the stored separator bytes —
// single-phase, zero-copy.
//
// The descent caller uses i to pick the next child:
//   - i == 0   → Ptr[0]    (leftmost child)
//   - 0 < i ≤ N → ChildPtr of cell i-1 (separators are right-child lower
//     bounds, so i-1's child holds keys < separators[i])
//   - i == N   → ChildPtr of last cell  (rightmost child)
//
// When target == Key[k], the search returns k+1 (the least i with
// target < separator[i]), so the target descends into that separator's
// right child — separators are right-child lower bounds.
//
// For an overflow cell the stored bytes are sep[0:T]; a tie through
// them with a still-longer target consults the key extent via tailCmp
// (the full-key comparison rule, page-formats.md §Overflow-Key Cells).
func BranchSearch(buf []byte, cfg Config, target []byte, tailCmp TailCompare) (uint16, error) {
	cfg.MustValidate()
	n := int(BranchCellCount(buf))
	if n == 0 {
		return 0, nil
	}
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		dirOff := branchHeaderEnd + mid*branchDirEntrySize
		off := int(le.Uint16(buf[dirOff:]))
		raw := le.Uint16(buf[dirOff+2:])
		klen := int(raw &^ branchDirKeyOverflowBit)
		key := buf[off : off+klen]
		var targetLess bool
		if raw&branchDirKeyOverflowBit == 0 {
			targetLess = bytes.Compare(target, key) < 0
		} else {
			k := min(len(target), klen)
			switch c := bytes.Compare(target[:k], key[:k]); {
			case c != 0:
				targetLess = c < 0
			case len(target) <= klen:
				// target is a (possibly full) prefix of sep[0:T];
				// the separator is strictly longer — target < sep.
				targetLess = true
			default:
				extPage := le.Uint64(buf[off+klen:])
				totalLen := le.Uint32(buf[off+klen+8:])
				c, err := tailCmp(target, extPage, totalLen)
				if err != nil {
					return 0, err
				}
				targetLess = c < 0
			}
		}
		if targetLess {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return uint16(lo), nil
}

// ShortestSeparator returns the shortest byte string S satisfying
// `left < S <= right` — the cross-level-truncated separator used at
// branch insertion time per page-formats.md §Separator Computation
// (Cross-Level Truncation). Constructed as the common prefix of left
// and right extended by exactly one byte from right at the first
// divergence position.
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
// region, and every cell's (Offset, KeyLen) points within the cell-data
// region [dirEnd, ContentEnd) with room for the trailing 8-byte child
// pointer. It returns a non-nil error (wrapping ErrCorrupted) on any
// violation and never panics — the decoder-robustness contract ("total
// over input, never panics on a forged page"), so reachability walks
// (Check, FreeSubtree) and the btree readers can call BranchSearch /
// BranchCellAt / BranchChildAt safely after Validate passes. Mirrors
// the per-cell bounds those readers assume.
func ValidateBranch(buf []byte, cfg Config) error {
	contentEnd := cfg.ContentEnd()
	if len(buf) < contentEnd {
		return fmt.Errorf("%w: branch buffer len %d < content end %d", ErrCorrupted, len(buf), contentEnd)
	}
	typ, _, n, _ := ReadHeader(buf)
	if typ != TypeBranch {
		return fmt.Errorf("%w: branch page has type %d (want %d)", ErrCorrupted, typ, TypeBranch)
	}
	dirEnd := branchHeaderEnd + int(n)*branchDirEntrySize
	if dirEnd > contentEnd {
		return fmt.Errorf("%w: branch cell directory (%d cells) overruns content end %d", ErrCorrupted, n, contentEnd)
	}
	t := cfg.InlineThreshold()
	for i := 0; i < int(n); i++ {
		dirOff := branchHeaderEnd + i*branchDirEntrySize
		off := int(le.Uint16(buf[dirOff:]))
		raw := le.Uint16(buf[dirOff+2:])
		klen := int(raw &^ branchDirKeyOverflowBit)
		end := off + branchCellChildSkip(raw) + branchChildPtrSize
		if off < dirEnd || end > contentEnd {
			return fmt.Errorf("%w: branch cell %d offset/len out of range: off=%d klen=%d end=%d (dirEnd=%d contentEnd=%d)",
				ErrCorrupted, i, off, klen, end, dirEnd, contentEnd)
		}
		if raw&branchDirKeyOverflowBit != 0 {
			// Derivable-length read policy (page-formats.md
			// §Overflow-Key Cells): an overflow cell's inline length is
			// EXACTLY InlineThreshold; its extent reference must name a
			// nonzero page and a full length strictly past the
			// threshold. Divergence is structural corruption.
			if klen != t {
				return fmt.Errorf("%w: branch overflow cell %d inline length %d != inline threshold %d",
					ErrCorrupted, i, klen, t)
			}
			extPage := le.Uint64(buf[off+klen:])
			totalLen := int(le.Uint32(buf[off+klen+8:]))
			if extPage == 0 {
				return fmt.Errorf("%w: branch overflow cell %d extent page is 0", ErrCorrupted, i)
			}
			if totalLen <= t {
				return fmt.Errorf("%w: branch overflow cell %d KeyTotalLen %d does not exceed inline threshold %d",
					ErrCorrupted, i, totalLen, t)
			}
		} else if klen > t {
			return fmt.Errorf("%w: branch cell %d inline key length %d exceeds inline threshold %d",
				ErrCorrupted, i, klen, t)
		}
	}
	return nil
}

// BranchChildAt returns the child pointer at descent index `i` from
// BranchSearch:
//   - i == 0 → leftmost (Ptr[0])
//   - 0 < i ≤ N → ChildPtr of cell i-1
//
// Reads the child pointer directly (no separator-key materialization), so it
// is allocation-free on the hot descent path.
func BranchChildAt(buf []byte, cfg Config, i uint16) uint64 {
	cfg.MustValidate()
	if i == 0 {
		return BranchLeftmostChild(buf)
	}
	return branchCellChild(buf, i-1)
}
