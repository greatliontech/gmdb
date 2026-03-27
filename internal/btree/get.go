package btree

import "github.com/thegrumpylion/gmdb/internal/page"

// Get searches for a key in the tree. Returns the leaf entry and whether
// the key was found. The returned entry has borrowed slices (Key from
// internal buffer, Value/SubpageData from the page buffer) valid until
// the next tree mutation.
func (t *Tree) Get(key []byte) (page.LeafEntry, bool) {
	if t.root == 0 {
		return page.LeafEntry{}, false
	}
	leafPageID := t.descendToLeaf(key)
	buf := t.pageSlice(leafPageID)
	lr := page.NewLeafReader(buf, t.cfg.Page)
	_, entry, found := lr.SearchLeaf(key, nil)
	return entry, found
}

// descendToLeaf traverses from root to the leaf containing the target key.
func (t *Tree) descendToLeaf(key []byte) uint64 {
	pageID := t.root
	for {
		buf := t.pageSlice(pageID)
		typ, _, _, _ := page.ReadHeader(buf)
		if typ == page.TypeLeaf {
			return pageID
		}
		br := page.NewBranchReader(buf)
		childPtr, _ := br.Search(key)
		pageID = childPtr
	}
}
