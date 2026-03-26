package btree

import (
	"fmt"
	"slices"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// Put inserts or updates an entry in the tree. If the key already exists,
// the old entry is returned with replaced=true. The old entry has owned
// (cloned) slices safe to keep beyond the next tree operation.
func (t *Tree) Put(e page.LeafEntry) (old page.LeafEntry, replaced bool, err error) {
	if t.root == 0 {
		pageID, err := t.allocPage()
		if err != nil {
			return page.LeafEntry{}, false, err
		}
		buf := t.pageSlice(pageID)
		lb := page.NewLeafBuilder(buf, t.cfg)
		if !addEntry(lb, e) {
			t.freePage(pageID)
			return page.LeafEntry{}, false, fmt.Errorf("btree: entry too large for page")
		}
		lb.Finish()
		t.root = pageID
		t.mutated()
		return page.LeafEntry{}, false, nil
	}

	newRoot, splitSep, splitRight, old, replaced, err := t.insert(t.root, e)
	if err != nil {
		return page.LeafEntry{}, false, err
	}

	if splitSep != nil {
		rootID, err := t.allocPage()
		if err != nil {
			return page.LeafEntry{}, false, err
		}
		buf := t.pageSlice(rootID)
		bb := page.NewBranchBuilder(buf, t.cfg)
		bb.SetPtr0(newRoot)
		bb.AddCell(splitSep, splitRight)
		bb.Finish()
		t.root = rootID
	} else {
		t.root = newRoot
	}

	t.mutated()
	return old, replaced, nil
}

// insert recursively inserts an entry into the subtree rooted at pageID.
// Returns: the (possibly new) page ID, split info if the page split,
// and the old entry if the key was replaced.
func (t *Tree) insert(pageID uint64, e page.LeafEntry) (
	newPageID uint64, splitSep []byte, splitRight uint64,
	old page.LeafEntry, replaced bool, err error,
) {
	buf := t.pageSlice(pageID)
	typ, _, _, _ := page.ReadHeader(buf)

	if typ == page.TypeLeaf {
		return t.insertIntoLeaf(pageID, e)
	}

	// Branch: descend to the correct child.
	br := page.NewBranchReader(buf)
	childPtr, childIdx := br.Search(e.Key)

	// Recursively insert into the child.
	newChildID, childSplitSep, childSplitRight, old, replaced, err := t.insert(childPtr, e)
	if err != nil {
		return 0, nil, 0, page.LeafEntry{}, false, err
	}

	// CoW this branch.
	newPageID, err = t.cowPage(pageID)
	if err != nil {
		return 0, nil, 0, page.LeafEntry{}, false, err
	}

	// Read cells and update the child pointer.
	ptr0, cells := t.collectBranchCells(newPageID)
	if childIdx == -1 {
		ptr0 = newChildID
	} else {
		cells[childIdx].childPtr = newChildID
	}

	// If the child split, insert the new separator.
	if childSplitSep != nil {
		insertAt := childIdx + 1
		cells = slices.Insert(cells, insertAt, branchCell{
			key:      childSplitSep,
			childPtr: childSplitRight,
		})
	}

	// Rebuild the branch.
	if t.rebuildBranch(newPageID, ptr0, cells) >= 0 {
		return newPageID, nil, 0, old, replaced, nil
	}

	// Branch overflows: split it.
	promotedSep, rightID, err := t.splitBranch(newPageID, ptr0, cells)
	if err != nil {
		return 0, nil, 0, page.LeafEntry{}, false, err
	}
	return newPageID, promotedSep, rightID, old, replaced, nil
}

// insertIntoLeaf inserts an entry into a leaf page.
func (t *Tree) insertIntoLeaf(pageID uint64, e page.LeafEntry) (
	newPageID uint64, splitSep []byte, splitRight uint64,
	old page.LeafEntry, replaced bool, err error,
) {
	newPageID, err = t.cowPage(pageID)
	if err != nil {
		return 0, nil, 0, page.LeafEntry{}, false, err
	}

	entries := t.collectEntries(newPageID)

	idx, found := findKey(entries, e.Key)
	if found {
		old = entries[idx]
		replaced = true
		entries[idx] = e
		// Ensure the replaced entry's key is the cloned version from collectEntries.
		entries[idx].Key = old.Key
	} else {
		entries = slices.Insert(entries, idx, e)
	}

	// Try to rebuild in place.
	if t.rebuildLeaf(newPageID, entries) >= 0 {
		return newPageID, nil, 0, old, replaced, nil
	}

	// Leaf overflows: split.
	splitSep, splitRight, err = t.splitLeaf(newPageID, entries)
	if err != nil {
		return 0, nil, 0, page.LeafEntry{}, false, err
	}
	return newPageID, splitSep, splitRight, old, replaced, nil
}

// splitLeaf splits entries across the current page (left) and a new page
// (right) using a byte-balanced split point. Returns the separator and
// the right page ID.
func (t *Tree) splitLeaf(pageID uint64, entries []page.LeafEntry) (sep []byte, rightPageID uint64, err error) {
	split := t.findLeafSplitPoint(entries)

	t.rebuildLeaf(pageID, entries[:split])

	rightPageID, err = t.allocPage()
	if err != nil {
		return nil, 0, err
	}
	t.rebuildLeaf(rightPageID, entries[split:])

	sep = computeSeparator(entries[split-1].Key, entries[split].Key)
	return sep, rightPageID, nil
}

// splitBranch splits cells across the current page (left) and a new page
// (right) using a byte-balanced split point. The middle cell's key is
// promoted as the separator for the parent.
func (t *Tree) splitBranch(pageID uint64, ptr0 uint64, cells []branchCell) (promotedSep []byte, rightPageID uint64, err error) {
	split := t.findBranchSplitPoint(ptr0, cells)

	t.rebuildBranch(pageID, ptr0, cells[:split])

	promotedSep = cells[split].key

	rightPageID, err = t.allocPage()
	if err != nil {
		return nil, 0, err
	}
	t.rebuildBranch(rightPageID, cells[split].childPtr, cells[split+1:])

	return promotedSep, rightPageID, nil
}
