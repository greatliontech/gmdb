package btree

import (
	"bytes"
	"slices"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// branchCell holds a separator key and its right child pointer.
type branchCell struct {
	key      []byte
	childPtr uint64
}

// pathEntry records a position in the tree during traversal.
type pathEntry struct {
	pageID uint64
	index  int // child index in branch: -1 = Ptr0, 0..Count-1 = cell i's child
}

// computeSeparator returns the shortest separator S such that left < S <= right.
// S = right[0:commonPrefixLen+1], always a prefix of right.
func computeSeparator(left, right []byte) []byte {
	n := min(len(left), len(right))
	i := 0
	for i < n && left[i] == right[i] {
		i++
	}
	// S = right[0:i+1]. This is always valid because left < right implies
	// either a divergence (left[i] < right[i]) or left is a proper prefix.
	return bytes.Clone(right[:i+1])
}

// collectEntries reads all entries from a leaf page, returning entries with
// owned (cloned) keys and values. The returned entries are safe to use after
// the page buffer is overwritten.
func (t *Tree) collectEntries(pageID uint64) []page.LeafEntry {
	buf := t.pageSlice(pageID)
	lr := page.NewLeafReader(buf, t.cfg)
	count := lr.Count()
	entries := make([]page.LeafEntry, 0, count)
	var keyBuf []byte
	keyBuf = lr.IterEntries(keyBuf, func(_ int, e page.LeafEntry) bool {
		e.Key = bytes.Clone(e.Key)
		if e.CellFlags == 0 && e.Value != nil {
			e.Value = bytes.Clone(e.Value)
		}
		if e.CellFlags&page.CellFlagMultiValue != 0 &&
			e.CellFlags&page.CellFlagNestedTree == 0 &&
			e.SubpageData != nil {
			e.SubpageData = bytes.Clone(e.SubpageData)
		}
		entries = append(entries, e)
		return true
	})
	_ = keyBuf
	return entries
}

// collectBranchCells reads Ptr0 and all cells from a branch page,
// returning owned (cloned) keys.
func (t *Tree) collectBranchCells(pageID uint64) (ptr0 uint64, cells []branchCell) {
	buf := t.pageSlice(pageID)
	br := page.NewBranchReader(buf)
	ptr0 = br.Ptr0()
	count := br.Count()
	cells = make([]branchCell, count)
	for i := range count {
		cells[i] = branchCell{
			key:      bytes.Clone(br.Key(i)),
			childPtr: br.ChildPtr(i),
		}
	}
	return
}

// addEntry adds a LeafEntry to a LeafBuilder, dispatching by CellFlags.
func addEntry(lb *page.LeafBuilder, e page.LeafEntry) bool {
	switch {
	case e.CellFlags&page.CellFlagOverflow != 0:
		return lb.AddOverflow(e.Key, e.OvflPage, e.TotalLen)
	case e.CellFlags&page.CellFlagNestedTree != 0:
		return lb.AddNestedTree(e.Key, e.NestedRoot, e.NestedCount)
	case e.CellFlags&page.CellFlagMultiValue != 0:
		return lb.AddSubpage(e.Key, e.SubpageData)
	default:
		return lb.AddInline(e.Key, e.Value)
	}
}

// rebuildLeaf writes all entries into the leaf page with fresh prefix
// compression. Returns the free space remaining after writing, or -1
// if the entries don't fit in the page.
func (t *Tree) rebuildLeaf(pageID uint64, entries []page.LeafEntry) int {
	buf := t.pageSlice(pageID)
	lb := page.NewLeafBuilder(buf, t.cfg)
	for _, e := range entries {
		if !addEntry(lb, e) {
			return -1
		}
	}
	free := lb.FreeSpace()
	lb.Finish()
	return free
}

// rebuildBranch writes ptr0 and all cells into the branch page.
// Returns the free space remaining, or -1 if the cells don't fit.
func (t *Tree) rebuildBranch(pageID uint64, ptr0 uint64, cells []branchCell) int {
	buf := t.pageSlice(pageID)
	bb := page.NewBranchBuilder(buf, t.cfg)
	bb.SetPtr0(ptr0)
	for _, c := range cells {
		if !bb.AddCell(c.key, c.childPtr) {
			return -1
		}
	}
	free := bb.FreeSpace()
	bb.Finish()
	return free
}

// findKey returns the index where key would be inserted in sorted entries.
// If the key exists, returns its index and true.
func findKey(entries []page.LeafEntry, key []byte) (int, bool) {
	idx, found := slices.BinarySearchFunc(entries, key, func(e page.LeafEntry, target []byte) int {
		return bytes.Compare(e.Key, target)
	})
	return idx, found
}

// isUnderfull returns true if freeSpace indicates the page uses less than
// the merge threshold of usable space.
func (t *Tree) isUnderfull(count int, freeSpace int) bool {
	if count == 0 {
		return true
	}
	usable := t.cfg.UsableSpace()
	return freeSpace*100 > usable*(100-mergeThresholdPercent)
}

// findLeafSplitPoint finds the byte-balanced split point for leaf entries.
// Returns the index where the right half starts. Both halves will fit in
// one page after fresh prefix compression encoding.
func (t *Tree) findLeafSplitPoint(entries []page.LeafEntry) int {
	target := t.cfg.UsableSpace() / 2
	buf := make([]byte, t.cfg.PageSize)
	lb := page.NewLeafBuilder(buf, t.cfg)
	for i, e := range entries {
		if !addEntry(lb, e) {
			if i == 0 {
				return 1
			}
			return i
		}
		if lb.FreeSpace() < target && i > 0 {
			return i + 1
		}
	}
	// All entries fit — caller only splits on overflow, so this means the
	// entries fit with fresh prefix compression. Split at midpoint.
	return len(entries) / 2
}

// findBranchSplitPoint finds the byte-balanced split point for branch cells.
func (t *Tree) findBranchSplitPoint(ptr0 uint64, cells []branchCell) int {
	target := t.cfg.UsableSpace() / 2
	buf := make([]byte, t.cfg.PageSize)
	bb := page.NewBranchBuilder(buf, t.cfg)
	bb.SetPtr0(ptr0)
	for i, c := range cells {
		if !bb.AddCell(c.key, c.childPtr) {
			if i == 0 {
				return 1
			}
			return i
		}
		if bb.FreeSpace() < target && i > 0 {
			return i + 1
		}
	}
	return len(cells) / 2
}

// cloneEntry returns a deep copy of a LeafEntry with all borrowed slices cloned.
func cloneEntry(e page.LeafEntry) page.LeafEntry {
	e.Key = bytes.Clone(e.Key)
	if e.CellFlags == 0 && e.Value != nil {
		e.Value = bytes.Clone(e.Value)
	}
	if e.CellFlags&page.CellFlagMultiValue != 0 &&
		e.CellFlags&page.CellFlagNestedTree == 0 &&
		e.SubpageData != nil {
		e.SubpageData = bytes.Clone(e.SubpageData)
	}
	return e
}
