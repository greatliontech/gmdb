package btree

import (
	"slices"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// DeleteRange removes all keys in the range [start, end). Returns the number
// of deleted entries. If start is nil, deletes from the first key. If end is
// nil, deletes through the last key. If both are nil, deletes all keys.
//
// DeleteRange retires entire B+tree subtrees that fall within the range
// without visiting individual entries — O(pages) not O(entries). Overflow
// page chains and nested B+trees referenced by deleted entries are retired
// as part of the operation.
func (t *Tree) DeleteRange(start, end []byte) (int, error) {
	if t.root == 0 {
		return 0, nil
	}

	newRoot, deleted, err := t.deleteRange(t.root, start, end)
	if err != nil {
		return 0, err
	}

	if newRoot == 0 {
		t.root = 0
	} else {
		t.root = newRoot
		t.shrinkRoot()
	}
	if deleted > 0 {
		t.mutated()
	}
	return deleted, nil
}

// deleteRange recursively deletes keys in [start, end) from the subtree
// rooted at pageID. Returns the new page ID (0 if empty), the number of
// deleted entries, and any error.
func (t *Tree) deleteRange(pageID uint64, start, end []byte) (uint64, int, error) {
	buf := t.pageSlice(pageID)
	typ, _, _, _ := page.ReadHeader(buf)

	if typ == page.TypeLeaf {
		return t.deleteRangeLeaf(pageID, start, end)
	}
	return t.deleteRangeBranch(pageID, start, end)
}

// deleteRangeLeaf removes entries in [start, end) from a leaf page.
func (t *Tree) deleteRangeLeaf(pageID uint64, start, end []byte) (uint64, int, error) {
	buf := t.pageSlice(pageID)
	lr := page.NewLeafReader(buf, t.cfg)

	// Find the index range to delete.
	leftIdx := 0
	if start != nil {
		leftIdx, _, _ = lr.SearchLeaf(start, nil)
	}

	rightIdx := lr.Count() - 1
	if end != nil {
		idx, _, found := lr.SearchLeaf(end, nil)
		if found {
			rightIdx = idx - 1 // end is exclusive
		} else {
			rightIdx = idx - 1 // idx = insertion point, last key < end = idx - 1
		}
	}

	if leftIdx > rightIdx {
		return pageID, 0, nil // empty range in this leaf
	}

	newPageID, err := t.cowPage(pageID)
	if err != nil {
		return 0, 0, err
	}

	entries := t.collectEntries(newPageID)
	deleted := rightIdx - leftIdx + 1

	for i := leftIdx; i <= rightIdx; i++ {
		t.retireEntryPages(entries[i])
	}

	entries = slices.Delete(entries, leftIdx, rightIdx+1)

	if len(entries) == 0 {
		t.freePage(newPageID)
		return 0, deleted, nil
	}

	t.rebuildLeaf(newPageID, entries)
	return newPageID, deleted, nil
}

// deleteRangeBranch removes keys in [start, end) from a branch's subtree.
func (t *Tree) deleteRangeBranch(pageID uint64, start, end []byte) (uint64, int, error) {
	buf := t.pageSlice(pageID)
	br := page.NewBranchReader(buf)

	// Find which children overlap with the range [start, end).
	// leftChildIdx = child containing start (or leftmost if start is nil)
	// rightChildIdx = child containing end-1 (or rightmost if end is nil)
	leftChildIdx := -1 // default: Ptr0
	if start != nil {
		_, leftChildIdx = br.Search(start)
	}

	rightChildIdx := br.Count() - 1 // default: rightmost
	if end != nil {
		_, rightChildIdx = br.Search(end)
	}

	// CoW this branch.
	newPageID, err := t.cowPage(pageID)
	if err != nil {
		return 0, 0, err
	}

	ptr0, cells := t.collectBranchCells(newPageID)
	totalDeleted := 0

	// Process children from left to right.
	// Children strictly between leftChildIdx and rightChildIdx are fully
	// within the range: retire their subtrees entirely.
	// leftChildIdx and rightChildIdx are boundary children: recurse into them.

	// Process left boundary child.
	// When the range spans multiple children, the left boundary child needs
	// everything >= start deleted — pass nil as end so its right spine is
	// retired in bulk rather than recursed level by level.
	leftChildPageID := childFromIndex(ptr0, cells, leftChildIdx)
	leftEnd := end
	if leftChildIdx != rightChildIdx {
		leftEnd = nil
	}
	newLeftChild, deleted, err := t.deleteRange(leftChildPageID, start, leftEnd)
	if err != nil {
		return 0, 0, err
	}
	totalDeleted += deleted
	ptr0, cells = updateChildInCells(ptr0, cells, leftChildIdx, newLeftChild)

	// Track boundary children that may need rebalancing after cleanup.
	var boundaryIDs [2]uint64
	boundaryCount := 0
	if newLeftChild != 0 {
		boundaryIDs[boundaryCount] = newLeftChild
		boundaryCount++
	}

	if leftChildIdx != rightChildIdx {
		// Process interior children: retire their subtrees entirely.
		for ci := leftChildIdx + 1; ci < rightChildIdx; ci++ {
			childPageID := childFromIndex(ptr0, cells, ci)
			totalDeleted += t.retireSubtree(childPageID)
			ptr0, cells = updateChildInCells(ptr0, cells, ci, 0) // mark for removal
		}

		// Process right boundary child.
		rightChildPageID := childFromIndex(ptr0, cells, rightChildIdx)
		newRightChild, deleted, err := t.deleteRange(rightChildPageID, nil, end)
		if err != nil {
			return 0, 0, err
		}
		totalDeleted += deleted
		ptr0, cells = updateChildInCells(ptr0, cells, rightChildIdx, newRightChild)
		if newRightChild != 0 {
			boundaryIDs[boundaryCount] = newRightChild
			boundaryCount++
		}
	}

	// Remove cells for retired/empty children. Walk in reverse to preserve indices.
	for ci := len(cells) - 1; ci >= 0; ci-- {
		if cells[ci].childPtr == 0 {
			cells = slices.Delete(cells, ci, ci+1)
		}
	}
	// Handle empty Ptr0.
	if ptr0 == 0 {
		if len(cells) > 0 {
			ptr0 = cells[0].childPtr
			cells = slices.Delete(cells, 0, 1)
		} else {
			t.freePage(newPageID)
			return 0, totalDeleted, nil
		}
	}

	// Rebalance underfull boundary children with their surviving siblings.
	// Only boundary children (those we recursed into) can be underfull;
	// other surviving children were not modified.
	if len(cells) > 0 && boundaryCount > 0 {
		for ci := len(cells) - 1; ci >= -1; ci-- {
			childID := childFromIndex(ptr0, cells, ci)
			if childID != boundaryIDs[0] && (boundaryCount < 2 || childID != boundaryIDs[1]) {
				continue
			}
			buf := t.pageSlice(childID)
			typ, _, count, _ := page.ReadHeader(buf)
			var freeSpace int
			if typ == page.TypeLeaf {
				entries := t.collectEntries(childID)
				tempBuf := make([]byte, t.cfg.PageSize)
				lb := page.NewLeafBuilder(tempBuf, t.cfg)
				for _, e := range entries {
					addEntry(lb, e)
				}
				freeSpace = lb.FreeSpace()
			} else {
				brPtr0, brCells := t.collectBranchCells(childID)
				tempBuf := make([]byte, t.cfg.PageSize)
				bb := page.NewBranchBuilder(tempBuf, t.cfg)
				bb.SetPtr0(brPtr0)
				for _, c := range brCells {
					bb.AddCell(c.key, c.childPtr)
				}
				freeSpace = bb.FreeSpace()
				count = uint16(len(brCells))
			}
			if t.isUnderfull(int(count), freeSpace) {
				var err error
				ptr0, cells, err = t.rebalanceChild(ptr0, cells, ci)
				if err != nil {
					return 0, 0, err
				}
			}
		}
	}

	t.rebuildBranch(newPageID, ptr0, cells)
	return newPageID, totalDeleted, nil
}

// retireSubtree recursively retires all pages in a subtree, including
// overflow chains and nested B+trees. Returns the number of leaf entries.
func (t *Tree) retireSubtree(pageID uint64) int {
	buf := t.pageSlice(pageID)
	typ, _, count, _ := page.ReadHeader(buf)

	if typ == page.TypeLeaf {
		lr := page.NewLeafReader(buf, t.cfg)
		lr.IterEntries(nil, func(_ int, e page.LeafEntry) bool {
			t.retireEntryPages(e)
			return true
		})
		t.retirePage(pageID)
		return int(count)
	}

	// Branch: recurse into all children, then retire this page.
	br := page.NewBranchReader(buf)
	total := t.retireSubtree(br.Ptr0())
	for i := range br.Count() {
		total += t.retireSubtree(br.ChildPtr(i))
	}
	t.retirePage(pageID)
	return total
}

// retireEntryPages retires overflow chains and nested B+trees referenced
// by a leaf entry.
func (t *Tree) retireEntryPages(e page.LeafEntry) {
	if e.CellFlags&page.CellFlagOverflow != 0 {
		t.retireOverflowChain(e.OvflPage)
	}
	if e.CellFlags&page.CellFlagNestedTree != 0 {
		t.retireSubtree(e.NestedRoot)
	}
}

// retireOverflowChain retires all pages in an overflow chain.
func (t *Tree) retireOverflowChain(firstPage uint64) {
	buf := t.pageSlice(firstPage)
	_, _, _, additional := page.ReadHeader(buf)
	for i := uint32(0); i <= additional; i++ {
		t.retirePage(firstPage + uint64(i))
	}
}

// updateChildInCells updates the child pointer at the given index.
func updateChildInCells(ptr0 uint64, cells []branchCell, idx int, childID uint64) (uint64, []branchCell) {
	if idx == -1 {
		return childID, cells
	}
	cells[idx].childPtr = childID
	return ptr0, cells
}
