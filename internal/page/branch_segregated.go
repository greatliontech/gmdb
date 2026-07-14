package page

import (
	"bytes"
	"fmt"
)

// Segregated-branch page layout per page-formats.md §Segregated Branch:
// the separators' single shared prefix stored once at the content tail,
// suffix bytes packed in a heap addressed by an offsets-only directory,
// and child pointers in a separate array.
//
//	+-----------------------+ offset 0
//	| Page Header (8 bytes) | Type=TypeBranchSegregated, Count=N
//	+-----------------------+ offset 8
//	| Ptr[0] (uint64)       | leftmost child pointer
//	+-----------------------+ offset 16
//	| PrefixLen uint16      | length of the page-wide shared prefix P
//	| Reserved  uint16      | zero on write (keeps the directory at offset 20)
//	+-----------------------+ offset 20
//	| Offsets Directory     | (N+1) × uint16, heap-relative, growing
//	| ...                   | forward; slot N is the heap-end sentinel
//	+-----------------------+ heap base = 20 + 2×(N+1)
//	| Suffix Heap           | suffix bytes packed forward in key order
//	+-----------------------+
//	|       free space      |
//	+-----------------------+
//	| Child Pointer Array   | N × uint64, packed ending at the prefix
//	+-----------------------+ ContentEnd - PrefixLen - 8×N
//	| Prefix bytes P        | the page-wide shared prefix
//	+-----------------------+ ContentEnd (PageSize - optional 8-byte footer)
//
// Cell i's suffix occupies heap span [Off[i], Off[i+1]) — lengths derive
// from adjacent slots, slot N naming the heap's end. Offsets are
// heap-relative, so growing the directory by one slot moves the heap
// base without rewriting stored offsets. The full separator of a
// (non-overflow) cell is P || heap[Off[i]:Off[i+1]]; cell i's child
// pointer sits at ContentEnd - PrefixLen - 8×(N - i).
//
// An OVERFLOW branch cell is marked by bit 63 of its CHILD POINTER
// (page-formats.md §Segregated Branch — bit 63 of every stored page ID
// is reserved by the format; the offset field cannot carry the marker
// because heap-end sentinels need the full uint16 range at 64 KB
// pages). A marked cell's heap span is
// Inline(T-PrefixLen) || KeyExtPage(u64) || KeyTotalLen(u32) — span
// length exactly (T-PrefixLen)+12 — and the child pointer is the array
// word with bit 63 cleared.

const (
	// segBranchPrefixLenOff is the byte offset of the PrefixLen uint16;
	// a Reserved uint16 follows (zero on write).
	segBranchPrefixLenOff = branchHeaderEnd // 16

	// segBranchDirOff is the byte offset where the offsets directory
	// begins.
	segBranchDirOff = segBranchPrefixLenOff + 4 // 20

	// segBranchDirSlotSize is the per-slot directory width (offset u16).
	segBranchDirSlotSize = 2

	// segBranchChildOverflowBit marks an overflow branch cell in its
	// child-pointer array word (bit 63, reserved by the format).
	segBranchChildOverflowBit = uint64(1) << 63
)

// segBranchPrefixLen reads the page-wide shared-prefix length.
func segBranchPrefixLen(buf []byte) int { return int(le.Uint16(buf[segBranchPrefixLenOff:])) }

// segBranchHeapBase returns the byte offset of the suffix heap for a
// page holding n cells: after the (n+1)-slot directory.
func segBranchHeapBase(n int) int { return segBranchDirOff + (n+1)*segBranchDirSlotSize }

// segBranchDirSlot reads directory slot i (heap-relative offset).
func segBranchDirSlot(buf []byte, i int) int {
	return int(le.Uint16(buf[segBranchDirOff+i*segBranchDirSlotSize:]))
}

// segBranchChildOff returns the byte offset of cell i's child-pointer
// array word on a page with n cells and prefix length m.
func segBranchChildOff(contentEnd, m, n, i int) int {
	return contentEnd - m - 8*(n-i)
}

// segBranchChildRaw reads cell i's RAW child-pointer word (marker bit
// included).
func segBranchChildRaw(buf []byte, contentEnd, m, n, i int) uint64 {
	return le.Uint64(buf[segBranchChildOff(contentEnd, m, n, i):])
}

// segBranchCellAt decodes cell i of a segregated branch page. Returned
// Key is an owned slice: P || suffix for ordinary cells (reconstructed
// — the two regions are non-contiguous), P || inline for overflow
// cells (the resident sep[0:T]).
func segBranchCellAt(buf []byte, cfg Config, i uint16, n uint16) BranchCell {
	ce := cfg.ContentEnd()
	m := segBranchPrefixLen(buf)
	hb := segBranchHeapBase(int(n))
	off := hb + segBranchDirSlot(buf, int(i))
	end := hb + segBranchDirSlot(buf, int(i)+1)
	raw := segBranchChildRaw(buf, ce, m, int(n), int(i))
	ovk := raw&segBranchChildOverflowBit != 0
	c := BranchCell{Child: raw &^ segBranchChildOverflowBit}
	span := end - off
	if ovk {
		inline := span - branchKeyExtRefSize
		key := make([]byte, m+inline)
		copy(key, buf[ce-m:ce])
		copy(key[m:], buf[off:off+inline])
		c.Key = key
		c.KeyExtPage = le.Uint64(buf[off+inline:])
		c.KeyTotalLen = le.Uint32(buf[off+inline+8:])
		return c
	}
	key := make([]byte, m+span)
	copy(key, buf[ce-m:ce])
	copy(key[m:], buf[off:end])
	c.Key = key
	return c
}

// segEncodeBranch writes cells + leftmost as a segregated branch page.
// Same caller contract as EncodeBranch (sorted cells, resident-slice
// overflow keys, size pre-checked; the error is defense in depth). A
// pure function of (cfg, leftmost, cells).
func segEncodeBranch(buf []byte, cfg Config, leftmost uint64, cells []BranchCell) error {
	ce := cfg.ContentEnd()
	need := segBranchEncodedSize(cells)
	if need > ce {
		return fmt.Errorf("page: EncodeBranch(segregated) %d cells need %d bytes, content end is %d", len(cells), need, ce)
	}
	clear(buf)
	WriteHeader(buf, TypeBranchSegregated, uint16(len(cells)), 0)
	SetBranchLeftmostChild(buf, leftmost)

	n := len(cells)
	m := 0
	if n > 0 {
		m = sharedPrefixLen(cells[0].Key, cells[n-1].Key)
		// PrefixLen <= T holds by construction (every stored Key is
		// <= T bytes), matching the spec cap.
		copy(buf[ce-m:ce], cells[0].Key[:m])
	}
	le.PutUint16(buf[segBranchPrefixLenOff:], uint16(m))

	hb := segBranchHeapBase(n)
	ho := 0
	for i, c := range cells {
		le.PutUint16(buf[segBranchDirOff+i*segBranchDirSlotSize:], uint16(ho))
		suffix := c.Key[m:]
		copy(buf[hb+ho:], suffix)
		ho += len(suffix)
		child := c.Child
		if c.IsOverflowKey() {
			le.PutUint64(buf[hb+ho:], c.KeyExtPage)
			le.PutUint32(buf[hb+ho+8:], c.KeyTotalLen)
			ho += branchKeyExtRefSize
			child |= segBranchChildOverflowBit
		}
		le.PutUint64(buf[segBranchChildOff(ce, m, n, i):], child)
	}
	le.PutUint16(buf[segBranchDirOff+n*segBranchDirSlotSize:], uint16(ho)) // sentinel
	return nil
}

// segBranchEncodedSize returns the byte size a segregated branch page
// with the given cells would occupy: header + PrefixLen/Reserved +
// (n+1)-slot directory + suffix heap (Σ resident bytes − n×P, plus one
// 12-byte extent reference per overflow cell) + 8-byte child array +
// the prefix stored once.
func segBranchEncodedSize(cells []BranchCell) int {
	n := len(cells)
	if n == 0 {
		return segBranchHeapBase(0)
	}
	m := sharedPrefixLen(cells[0].Key, cells[n-1].Key)
	keyLenSum, extRefs := 0, 0
	for _, c := range cells {
		keyLenSum += len(c.Key)
		if c.IsOverflowKey() {
			extRefs++
		}
	}
	return segBranchEncodedSizeOf(n, keyLenSum, m, extRefs)
}

// segBranchEncodedSizeOf is the segregated sizing formula over the
// aggregate quantities: n cells whose resident keys total keyLenSum
// bytes sharing an m-byte prefix, extRefs of them overflow.
// Non-additive: m depends on the whole cell set.
func segBranchEncodedSizeOf(n, keyLenSum, m, extRefs int) int {
	return segBranchHeapBase(n) + (keyLenSum - n*m) + extRefs*branchKeyExtRefSize + 8*n + m
}

// segBranchSearch is the segregated arm of BranchSearch: a prefix gate
// against P, then a binary search over the heap suffixes via adjacent
// directory slots. Overflow cells compare their inline portion first
// and consult the key extent via tailCmp exactly on a full resident
// tie with a longer target (page-formats.md §Overflow-Key Cells).
func segBranchSearch(buf []byte, cfg Config, target []byte, tailCmp TailCompare) (uint16, error) {
	_, _, count, _ := ReadHeader(buf)
	n := int(count)
	if n == 0 {
		return 0, nil
	}
	ce := cfg.ContentEnd()
	m := segBranchPrefixLen(buf)
	prefix := buf[ce-m : ce]

	// Prefix gate: P is a genuine prefix of every separator (P ||
	// Inline covers sep[0:T] for overflow cells), so a target that
	// does not start with P is decided against P alone.
	if len(target) < m || !bytes.Equal(target[:m], prefix) {
		if bytes.Compare(target, prefix) < 0 {
			return 0, nil
		}
		return uint16(n), nil
	}

	tail := target[m:]
	hb := segBranchHeapBase(n)
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		off := hb + segBranchDirSlot(buf, mid)
		end := hb + segBranchDirSlot(buf, mid+1)
		raw := segBranchChildRaw(buf, ce, m, n, mid)
		var targetLess bool
		if raw&segBranchChildOverflowBit == 0 {
			targetLess = bytes.Compare(tail, buf[off:end]) < 0
		} else {
			inline := (end - off) - branchKeyExtRefSize
			suffix := buf[off : off+inline]
			k := min(len(tail), inline)
			switch c := bytes.Compare(tail[:k], suffix[:k]); {
			case c != 0:
				targetLess = c < 0
			case len(tail) <= inline:
				// target is a (possibly full) prefix of sep[0:T]; the
				// separator is strictly longer — target < sep.
				targetLess = true
			default:
				extPage := le.Uint64(buf[off+inline:])
				totalLen := le.Uint32(buf[off+inline+8:])
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

// segValidateBranch is the segregated arm of ValidateBranch: total
// over its input, never panics. Checks the directory + heap + child
// array + prefix regions for containment and non-overlap, offset
// monotonicity, the PrefixLen <= T cap, and the overflow cells'
// derivable-length policy (span == (T-PrefixLen)+12, nonzero extent
// page, KeyTotalLen > T).
func segValidateBranch(buf []byte, cfg Config) error {
	contentEnd := cfg.ContentEnd()
	if len(buf) < contentEnd {
		return fmt.Errorf("%w: branch buffer len %d < content end %d", ErrCorrupted, len(buf), contentEnd)
	}
	_, _, count, _ := ReadHeader(buf)
	n := int(count)
	m := segBranchPrefixLen(buf)
	t := cfg.InlineThreshold()
	if m > t {
		return fmt.Errorf("%w: segregated branch PrefixLen %d exceeds inline threshold %d", ErrCorrupted, m, t)
	}
	hb := segBranchHeapBase(n)
	childBase := contentEnd - m - 8*n
	if hb > childBase {
		return fmt.Errorf("%w: segregated branch directory (%d cells) overlaps child array/prefix: heapBase=%d childBase=%d",
			ErrCorrupted, n, hb, childBase)
	}
	prev := 0
	for i := 0; i <= n; i++ {
		off := segBranchDirSlot(buf, i)
		if off < prev {
			return fmt.Errorf("%w: segregated branch directory slot %d offset %d < previous %d (monotonicity)",
				ErrCorrupted, i, off, prev)
		}
		prev = off
	}
	heapEnd := hb + segBranchDirSlot(buf, n)
	if heapEnd > childBase {
		return fmt.Errorf("%w: segregated branch heap end %d overruns child array base %d", ErrCorrupted, heapEnd, childBase)
	}
	for i := 0; i < n; i++ {
		span := segBranchDirSlot(buf, i+1) - segBranchDirSlot(buf, i)
		raw := segBranchChildRaw(buf, contentEnd, m, n, i)
		if raw&segBranchChildOverflowBit != 0 {
			// Derivable-length read policy (page-formats.md
			// §Overflow-Key Cells): the marked span is exactly the
			// inline (T - PrefixLen) plus the 12-byte reference.
			if span != (t-m)+branchKeyExtRefSize {
				return fmt.Errorf("%w: segregated branch overflow cell %d span %d != (T-PrefixLen)+12 = %d",
					ErrCorrupted, i, span, (t-m)+branchKeyExtRefSize)
			}
			off := hb + segBranchDirSlot(buf, i)
			inline := span - branchKeyExtRefSize
			extPage := le.Uint64(buf[off+inline:])
			totalLen := int(le.Uint32(buf[off+inline+8:]))
			if extPage == 0 {
				return fmt.Errorf("%w: segregated branch overflow cell %d extent page is 0", ErrCorrupted, i)
			}
			if totalLen <= t {
				return fmt.Errorf("%w: segregated branch overflow cell %d KeyTotalLen %d does not exceed inline threshold %d",
					ErrCorrupted, i, totalLen, t)
			}
		} else if m+span > t {
			// A plain cell's full separator (P || suffix) can never
			// exceed T — over-T keys must take the overflow form.
			return fmt.Errorf("%w: segregated branch cell %d full-key length %d exceeds inline threshold %d",
				ErrCorrupted, i, m+span, t)
		}
	}
	return nil
}
