package btree

import (
	"slices"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// mergeThresholdPercent is the fill ratio (as a percentage of usable space)
// below which a leaf or branch triggers merge/redistribute with a sibling.
const mergeThresholdPercent = 25

// Delete removes a key from the tree. Returns the deleted entry and whether
// the key was found. The deleted entry has owned (cloned) slices.
func (t *Tree) Delete(key []byte) (page.LeafEntry, bool, error) {
	if t.root == 0 {
		return page.LeafEntry{}, false, nil
	}

	newRoot, old, found, _, err := t.remove(t.root, key)
	if err != nil || !found {
		return old, found, err
	}

	if newRoot == 0 {
		t.root = 0
	} else {
		t.root = newRoot
		t.shrinkRoot()
	}

	t.mutated()
	return old, true, nil
}

// remove recursively deletes a key from the subtree rooted at pageID.
func (t *Tree) remove(pageID uint64, key []byte) (
	newPageID uint64, old page.LeafEntry, found bool, underflow bool, err error,
) {
	buf := t.pageSlice(pageID)
	typ, _, _, _ := page.ReadHeader(buf)

	if typ == page.TypeLeaf {
		return t.removeFromLeaf(pageID, key)
	}

	br := page.NewBranchReader(buf)
	childPtr, childIdx := br.Search(key)

	newChildID, old, found, childUnderflow, err := t.remove(childPtr, key)
	if err != nil || !found {
		return pageID, old, found, false, err
	}

	newPageID, err = t.cowPage(pageID)
	if err != nil {
		return 0, page.LeafEntry{}, false, false, err
	}

	ptr0, cells := t.collectBranchCells(newPageID)
	if childIdx == -1 {
		ptr0 = newChildID
	} else {
		cells[childIdx].childPtr = newChildID
	}

	if childUnderflow && len(cells) > 0 {
		ptr0, cells, err = t.rebalanceChild(ptr0, cells, childIdx)
		if err != nil {
			return 0, page.LeafEntry{}, false, false, err
		}
	}

	freeSpace := t.rebuildBranch(newPageID, ptr0, cells)
	underflow = t.isUnderfull(len(cells), freeSpace)
	return newPageID, old, true, underflow, nil
}

// removeFromLeaf searches the leaf for the key. If found, CoWs the page,
// removes the entry, and rebuilds.
func (t *Tree) removeFromLeaf(pageID uint64, key []byte) (
	newPageID uint64, old page.LeafEntry, found bool, underflow bool, err error,
) {
	buf := t.pageSlice(pageID)
	lr := page.NewLeafReader(buf, t.cfg)
	searchIdx, entry, found := lr.SearchLeaf(key, nil)
	if !found {
		return pageID, page.LeafEntry{}, false, false, nil
	}

	old = cloneEntry(entry)

	newPageID, err = t.cowPage(pageID)
	if err != nil {
		return 0, page.LeafEntry{}, false, false, err
	}

	entries := t.collectEntries(newPageID)
	entries = slices.Delete(entries, searchIdx, searchIdx+1)

	if len(entries) == 0 {
		t.freePage(newPageID)
		return 0, old, true, true, nil
	}

	freeSpace := t.rebuildLeaf(newPageID, entries)
	underflow = t.isUnderfull(len(entries), freeSpace)
	return newPageID, old, true, underflow, nil
}

// rebalanceChild handles an underfull child by merging or redistributing
// with a sibling. Tries both left and right siblings, preferring the one
// that enables a merge.
func (t *Tree) rebalanceChild(ptr0 uint64, cells []branchCell, childIdx int) (
	newPtr0 uint64, newCells []branchCell, err error,
) {
	// Empty Ptr0 child: promote sibling and remove separator.
	if childIdx == -1 {
		leftChildID := childFromIndex(ptr0, cells, -1)
		if leftChildID == 0 {
			return t.removeChild(ptr0, cells, 0)
		}
	}

	// Try left sibling first, then right. Prefer the pair that can merge.
	type siblingPair struct {
		leftIdx, rightIdx, sepCellIdx int
	}
	var candidates []siblingPair

	if childIdx > -1 {
		// Left sibling exists.
		candidates = append(candidates, siblingPair{childIdx - 1, childIdx, childIdx})
	}
	if childIdx+1 < len(cells) {
		// Right sibling exists (childIdx+1 is a valid cell index).
		candidates = append(candidates, siblingPair{childIdx, childIdx + 1, childIdx + 1})
	}
	if childIdx == -1 && len(cells) > 0 {
		// Underfull child is Ptr0, right sibling is cells[0].
		candidates = append(candidates, siblingPair{-1, 0, 0})
	}

	if len(candidates) == 0 {
		// No sibling (single-child branch) — nothing to rebalance.
		return ptr0, cells, nil
	}

	// Pick the first pair where merge is possible. If none, use the first.
	best := candidates[0]
	for _, c := range candidates {
		leftID := childFromIndex(ptr0, cells, c.leftIdx)
		rightID := childFromIndex(ptr0, cells, c.rightIdx)
		if t.canMerge(leftID, rightID) {
			best = c
			break
		}
	}

	leftChildID := childFromIndex(ptr0, cells, best.leftIdx)
	rightChildID := childFromIndex(ptr0, cells, best.rightIdx)

	leftChildID, err = t.cowPage(leftChildID)
	if err != nil {
		return 0, nil, err
	}
	rightChildID, err = t.cowPage(rightChildID)
	if err != nil {
		return 0, nil, err
	}

	buf := t.pageSlice(leftChildID)
	typ, _, _, _ := page.ReadHeader(buf)

	if typ == page.TypeLeaf {
		return t.rebalanceLeaves(ptr0, cells, best.leftIdx, best.rightIdx, best.sepCellIdx, leftChildID, rightChildID)
	}
	return t.rebalanceBranches(ptr0, cells, best.leftIdx, best.rightIdx, best.sepCellIdx, leftChildID, rightChildID)
}

// canMerge returns true if two sibling pages can be merged into one.
// This is a quick check without CoW — it reads both pages and estimates
// whether combined content fits in one page.
func (t *Tree) canMerge(leftID, rightID uint64) bool {
	leftBuf := t.pageSlice(leftID)
	leftTyp, _, _, _ := page.ReadHeader(leftBuf)

	if leftTyp == page.TypeLeaf {
		leftEntries := t.collectEntries(leftID)
		rightEntries := t.collectEntries(rightID)
		combined := append(leftEntries, rightEntries...)
		buf := make([]byte, t.cfg.PageSize)
		lb := page.NewLeafBuilder(buf, t.cfg)
		for _, e := range combined {
			if !addEntry(lb, e) {
				return false
			}
		}
		return true
	}

	// Branch: collect cells, add demoted separator placeholder.
	_, leftCells := t.collectBranchCells(leftID)
	rightPtr0, rightCells := t.collectBranchCells(rightID)
	// The demoted separator is unknown here (it's in the parent), but we can
	// estimate with a zero-length key. If even that doesn't fit, merge is impossible.
	combined := make([]branchCell, 0, len(leftCells)+1+len(rightCells))
	combined = append(combined, leftCells...)
	combined = append(combined, branchCell{childPtr: rightPtr0})
	combined = append(combined, rightCells...)
	buf := make([]byte, t.cfg.PageSize)
	bb := page.NewBranchBuilder(buf, t.cfg)
	bb.SetPtr0(0)
	for _, c := range combined {
		if !bb.AddCell(c.key, c.childPtr) {
			return false
		}
	}
	return true
}

// rebalanceLeaves attempts to merge two leaf siblings. If they don't fit
// in one page, redistributes entries using a byte-balanced split.
func (t *Tree) rebalanceLeaves(
	ptr0 uint64, cells []branchCell,
	leftIdx, rightIdx, sepCellIdx int,
	leftChildID, rightChildID uint64,
) (uint64, []branchCell, error) {
	leftEntries := t.collectEntries(leftChildID)
	rightEntries := t.collectEntries(rightChildID)
	combined := append(leftEntries, rightEntries...)

	if t.rebuildLeaf(leftChildID, combined) >= 0 {
		t.freePage(rightChildID)
		return t.removeSepAndChild(ptr0, cells, leftIdx, rightIdx, sepCellIdx, leftChildID)
	}

	// Redistribute with byte-balanced split.
	split := t.findLeafSplitPoint(combined)
	t.rebuildLeaf(leftChildID, combined[:split])
	t.rebuildLeaf(rightChildID, combined[split:])

	newSep := computeSeparator(combined[split-1].Key, combined[split].Key)

	ptr0, cells = updateChild(ptr0, cells, leftIdx, leftChildID)
	ptr0, cells = updateChild(ptr0, cells, rightIdx, rightChildID)
	cells[sepCellIdx].key = newSep

	return ptr0, cells, nil
}

// rebalanceBranches attempts to merge two branch siblings. If they don't
// fit in one page, redistributes cells using a byte-balanced split.
func (t *Tree) rebalanceBranches(
	ptr0 uint64, cells []branchCell,
	leftIdx, rightIdx, sepCellIdx int,
	leftChildID, rightChildID uint64,
) (uint64, []branchCell, error) {
	leftPtr0, leftCells := t.collectBranchCells(leftChildID)
	rightPtr0, rightCells := t.collectBranchCells(rightChildID)

	demotedSep := cells[sepCellIdx].key
	combined := make([]branchCell, 0, len(leftCells)+1+len(rightCells))
	combined = append(combined, leftCells...)
	combined = append(combined, branchCell{key: demotedSep, childPtr: rightPtr0})
	combined = append(combined, rightCells...)

	if t.rebuildBranch(leftChildID, leftPtr0, combined) >= 0 {
		t.freePage(rightChildID)
		return t.removeSepAndChild(ptr0, cells, leftIdx, rightIdx, sepCellIdx, leftChildID)
	}

	split := t.findBranchSplitPoint(leftPtr0, combined)
	t.rebuildBranch(leftChildID, leftPtr0, combined[:split])
	promotedSep := combined[split].key
	t.rebuildBranch(rightChildID, combined[split].childPtr, combined[split+1:])

	ptr0, cells = updateChild(ptr0, cells, leftIdx, leftChildID)
	ptr0, cells = updateChild(ptr0, cells, rightIdx, rightChildID)
	cells[sepCellIdx].key = promotedSep

	return ptr0, cells, nil
}

// removeSepAndChild removes the separator and the merged-away child from
// the parent branch. The surviving child replaces the merged pair.
func (t *Tree) removeSepAndChild(
	ptr0 uint64, cells []branchCell,
	leftIdx, _, sepCellIdx int,
	survivorID uint64,
) (uint64, []branchCell, error) {
	cells = slices.Delete(cells, sepCellIdx, sepCellIdx+1)

	if leftIdx == -1 {
		ptr0 = survivorID
	} else {
		cells[leftIdx].childPtr = survivorID
	}

	return ptr0, cells, nil
}

// removeChild removes an empty Ptr0 child by promoting the sibling to Ptr0
// and removing the separator.
func (t *Tree) removeChild(
	ptr0 uint64, cells []branchCell, sepCellIdx int,
) (uint64, []branchCell, error) {
	ptr0 = cells[sepCellIdx].childPtr
	cells = slices.Delete(cells, sepCellIdx, sepCellIdx+1)
	return ptr0, cells, nil
}

// shrinkRoot replaces the root with its only child if the root is a
// branch with zero cells (only Ptr0).
func (t *Tree) shrinkRoot() {
	for t.root != 0 {
		buf := t.pageSlice(t.root)
		typ, _, count, _ := page.ReadHeader(buf)
		if typ != page.TypeBranch || count > 0 {
			break
		}
		br := page.NewBranchReader(buf)
		newRoot := br.Ptr0()
		t.freePage(t.root)
		t.root = newRoot
	}
}

// childFromIndex returns the child page ID at the given index (-1 = Ptr0).
func childFromIndex(ptr0 uint64, cells []branchCell, idx int) uint64 {
	if idx == -1 {
		return ptr0
	}
	return cells[idx].childPtr
}

// updateChild sets the child page ID at the given index.
func updateChild(ptr0 uint64, cells []branchCell, idx int, childID uint64) (uint64, []branchCell) {
	if idx == -1 {
		return childID, cells
	}
	cells[idx].childPtr = childID
	return ptr0, cells
}
