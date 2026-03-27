package btree

import (
	"bytes"
	"errors"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// Cursor provides bidirectional iteration over B+tree entries.
//
// The cursor detects tree mutations (Put, Delete, DeleteRange) via a
// generation counter. If the tree is mutated after the cursor was
// positioned, subsequent navigation returns ErrCursorStale. The cursor
// must be re-positioned (First, Last, Seek, SeekGE) after a mutation.
//
// Key ownership: the key returned by cursor operations is borrowed from
// the cursor's internal keyBuf. It is valid until the next cursor movement.
// Value/SubpageData are borrowed from the page buffer.
type Cursor struct {
	tree  *Tree
	stack []pathEntry // branch path from root to leaf's parent
	leaf  uint64      // current leaf page ID (0 = unpositioned)
	idx   int         // current entry index within leaf
	count int         // entry count of current leaf

	keyBuf       []byte             // reusable key reconstruction buffer
	keyArena     []byte             // reusable arena for group cache keys
	decodeKeyBuf []byte             // reusable buffer for delta key reconstruction
	groupCache   [16]page.LeafEntry // cached decoded entries for current restart group
	groupBase    int                // first entry index of the cached group (-1 = invalid)
	groupLen     int                // number of valid entries in groupCache

	gen   uint64 // tree generation at last positioning
	valid bool   // cursor is positioned on a valid entry
	err   error  // first error encountered during navigation
}

// NewCursor creates a new cursor for the tree.
func (t *Tree) NewCursor() *Cursor {
	return &Cursor{
		tree:      t,
		groupBase: -1,
	}
}

// First positions the cursor at the first (smallest) key in the tree.
func (c *Cursor) First() (key, value []byte) {
	c.invalidate()
	if c.err != nil || c.tree.root == 0 {
		return nil, nil
	}
	c.stack = c.stack[:0]
	c.descendFirst(c.tree.root)
	c.gen = c.tree.gen
	return c.entryAt()
}

// Last positions the cursor at the last (largest) key in the tree.
func (c *Cursor) Last() (key, value []byte) {
	c.invalidate()
	if c.err != nil || c.tree.root == 0 {
		return nil, nil
	}
	c.stack = c.stack[:0]
	c.descendLast(c.tree.root)
	c.gen = c.tree.gen
	return c.entryAt()
}

// Next advances the cursor to the next key.
func (c *Cursor) Next() (key, value []byte) {
	if !c.valid || c.err != nil {
		return nil, nil
	}
	if c.gen != c.tree.gen {
		c.err = ErrCursorStale
		return nil, nil
	}
	c.idx++
	if c.idx < c.count {
		return c.cachedCurrent()
	}
	if !c.advanceLeaf() {
		c.invalidate()
		return nil, nil
	}
	return c.cachedCurrent()
}

// Prev moves the cursor to the previous key.
func (c *Cursor) Prev() (key, value []byte) {
	if !c.valid || c.err != nil {
		return nil, nil
	}
	if c.gen != c.tree.gen {
		c.err = ErrCursorStale
		return nil, nil
	}
	c.idx--
	if c.idx >= 0 {
		return c.cachedCurrent()
	}
	if !c.retreatLeaf() {
		c.invalidate()
		return nil, nil
	}
	return c.cachedCurrent()
}

// Seek positions the cursor at the exact key. Returns nil if not found.
func (c *Cursor) Seek(target []byte) (key, value []byte) {
	c.invalidate()
	if c.err != nil || c.tree.root == 0 {
		return nil, nil
	}
	c.stack = c.stack[:0]
	leafPageID := c.descendSearch(c.tree.root, target)
	c.setLeaf(leafPageID)

	buf := c.tree.pageSlice(leafPageID)
	lr := page.NewLeafReader(buf, c.tree.cfg.Page)
	idx, entry, found := lr.SearchLeaf(target, c.keyBuf)
	if !found {
		return nil, nil
	}
	c.idx = idx
	c.valid = true
	c.gen = c.tree.gen
	return c.keyFromEntry(entry), entry.Value
}

// SeekGE positions the cursor at the first key >= target.
func (c *Cursor) SeekGE(target []byte) (key, value []byte) {
	c.invalidate()
	if c.err != nil || c.tree.root == 0 {
		return nil, nil
	}
	c.stack = c.stack[:0]
	leafPageID := c.descendSearch(c.tree.root, target)
	c.setLeaf(leafPageID)

	buf := c.tree.pageSlice(leafPageID)
	lr := page.NewLeafReader(buf, c.tree.cfg.Page)
	idx, entry, found := lr.SearchLeaf(target, c.keyBuf)

	if found {
		c.idx = idx
		c.valid = true
		c.gen = c.tree.gen
		return c.keyFromEntry(entry), entry.Value
	}

	if idx < c.count {
		c.idx = idx
		c.valid = true
		c.gen = c.tree.gen
		return c.entryAt()
	}

	if !c.advanceLeaf() {
		return nil, nil
	}
	c.gen = c.tree.gen
	return c.entryAt()
}

// Current returns the key-value pair at the current position.
func (c *Cursor) Current() (key, value []byte) {
	if !c.valid || c.err != nil {
		return nil, nil
	}
	if c.gen != c.tree.gen {
		c.err = ErrCursorStale
		return nil, nil
	}
	return c.cachedCurrent()
}

// Valid returns true if the cursor is positioned on a valid entry.
func (c *Cursor) Valid() bool {
	return c.valid && c.err == nil
}

// Err returns the first error encountered during cursor navigation.
func (c *Cursor) Err() error {
	return c.err
}

// Delete removes the current key-value pair from the tree. After deletion,
// the cursor is positioned at the next key (the successor of the deleted
// key). If the deleted key was the last key, the cursor becomes
// unpositioned.
func (c *Cursor) Delete() error {
	if c.err != nil {
		return c.err
	}
	if !c.valid {
		return errors.New("btree: cursor not positioned")
	}

	key := bytes.Clone(c.keyBuf)

	_, _, err := c.tree.Delete(key)
	if err != nil {
		c.err = err
		c.invalidate()
		return err
	}

	// Reposition to the successor. Delete incremented gen, SeekGE updates it.
	c.groupBase = -1
	c.err = nil // clear stale error that would be set by SeekGE's gen check
	c.SeekGE(key)
	return nil
}

// entryAt reads the entry at the current position via EntryAt.
// Used for positioning operations (First, Last, SeekGE) where the
// group cache is not yet populated.
func (c *Cursor) entryAt() (key, value []byte) {
	buf := c.tree.pageSlice(c.leaf)
	lr := page.NewLeafReader(buf, c.tree.cfg.Page)
	entry, kb := lr.EntryAt(c.idx, c.keyBuf)
	c.keyBuf = kb
	return c.keyFromEntry(entry), entry.Value
}

// cachedCurrent returns the entry at the current position from the group
// cache. If the current group is not cached, all entries in the restart
// group are decoded once (O(K) where K=16) and cached. Subsequent accesses
// within the group are O(1).
func (c *Cursor) cachedCurrent() (key, value []byte) {
	ri := 16 // restart interval
	base := (c.idx / ri) * ri

	if c.groupBase != base {
		c.populateGroup(base)
	}

	ce := c.groupCache[c.idx-c.groupBase]
	c.keyBuf = append(c.keyBuf[:0], ce.Key...)
	return c.keyBuf, ce.Value
}

// populateGroup decodes all entries in the restart group starting at base
// and caches them. Uses a pull-based iterator with cursor-owned buffers —
// zero allocations after warmup.
func (c *Cursor) populateGroup(base int) {
	buf := c.tree.pageSlice(c.leaf)
	lr := page.NewLeafReader(buf, c.tree.cfg.Page)

	c.keyArena = c.keyArena[:0]
	var keyOff [16]uint32
	n := 0

	it := lr.GroupIter(base/16, c.decodeKeyBuf)
	for e, ok := it.Next(); ok; e, ok = it.Next() {
		keyOff[n] = uint32(len(c.keyArena))
		c.keyArena = append(c.keyArena, e.Key...)
		e.Key = nil
		c.groupCache[n] = e
		n++
	}
	c.decodeKeyBuf = it.KeyBuf()

	// Resolve Key slices into the final (stable) arena.
	for i := range n {
		start := keyOff[i]
		var end uint32
		if i+1 < n {
			end = keyOff[i+1]
		} else {
			end = uint32(len(c.keyArena))
		}
		c.groupCache[i].Key = c.keyArena[start:end:end]
	}

	c.groupBase = base
	c.groupLen = n
}

// keyFromEntry copies the entry key into the cursor's keyBuf for stability.
func (c *Cursor) keyFromEntry(e page.LeafEntry) []byte {
	c.keyBuf = append(c.keyBuf[:0], e.Key...)
	return c.keyBuf
}

// invalidate marks the cursor as unpositioned. Clears stale errors
// (positioning operations can recover from staleness) but preserves
// permanent errors (e.g., allocation failures from Delete).
func (c *Cursor) invalidate() {
	c.valid = false
	c.leaf = 0
	c.idx = 0
	c.count = 0
	c.groupBase = -1
	if c.err == ErrCursorStale {
		c.err = nil
	}
}

// setLeaf positions the cursor on a leaf page.
func (c *Cursor) setLeaf(pageID uint64) {
	c.leaf = pageID
	buf := c.tree.pageSlice(pageID)
	lr := page.NewLeafReader(buf, c.tree.cfg.Page)
	c.count = lr.Count()
	c.groupBase = -1
}

// descendFirst descends to the first (leftmost) entry in the subtree.
func (c *Cursor) descendFirst(pageID uint64) {
	for {
		buf := c.tree.pageSlice(pageID)
		typ, _, _, _ := page.ReadHeader(buf)
		if typ == page.TypeLeaf {
			c.setLeaf(pageID)
			c.idx = 0
			c.valid = c.count > 0
			return
		}
		br := page.NewBranchReader(buf)
		c.stack = append(c.stack, pathEntry{pageID: pageID, index: -1})
		pageID = br.Ptr0()
	}
}

// descendLast descends to the last (rightmost) entry in the subtree.
func (c *Cursor) descendLast(pageID uint64) {
	for {
		buf := c.tree.pageSlice(pageID)
		typ, _, _, _ := page.ReadHeader(buf)
		if typ == page.TypeLeaf {
			c.setLeaf(pageID)
			c.idx = c.count - 1
			c.valid = c.count > 0
			return
		}
		br := page.NewBranchReader(buf)
		lastIdx := br.Count() - 1
		c.stack = append(c.stack, pathEntry{pageID: pageID, index: lastIdx})
		pageID = br.ChildPtr(lastIdx)
	}
}

// descendSearch descends to the leaf containing the target key.
func (c *Cursor) descendSearch(pageID uint64, target []byte) uint64 {
	for {
		buf := c.tree.pageSlice(pageID)
		typ, _, _, _ := page.ReadHeader(buf)
		if typ == page.TypeLeaf {
			return pageID
		}
		br := page.NewBranchReader(buf)
		childPtr, idx := br.Search(target)
		c.stack = append(c.stack, pathEntry{pageID: pageID, index: idx})
		pageID = childPtr
	}
}

// advanceLeaf moves to the first entry of the next leaf page.
func (c *Cursor) advanceLeaf() bool {
	for len(c.stack) > 0 {
		parent := c.stack[len(c.stack)-1]
		c.stack = c.stack[:len(c.stack)-1]

		buf := c.tree.pageSlice(parent.pageID)
		br := page.NewBranchReader(buf)

		nextIdx := parent.index + 1
		if nextIdx < br.Count() {
			c.stack = append(c.stack, pathEntry{pageID: parent.pageID, index: nextIdx})
			c.descendFirst(br.ChildPtr(nextIdx))
			return true
		}
	}
	return false
}

// retreatLeaf moves to the last entry of the previous leaf page.
func (c *Cursor) retreatLeaf() bool {
	for len(c.stack) > 0 {
		parent := c.stack[len(c.stack)-1]
		c.stack = c.stack[:len(c.stack)-1]

		buf := c.tree.pageSlice(parent.pageID)
		br := page.NewBranchReader(buf)

		prevIdx := parent.index - 1
		if prevIdx >= -1 {
			var childPageID uint64
			if prevIdx == -1 {
				childPageID = br.Ptr0()
			} else {
				childPageID = br.ChildPtr(prevIdx)
			}
			c.stack = append(c.stack, pathEntry{pageID: parent.pageID, index: prevIdx})
			c.descendLast(childPageID)
			return true
		}
	}
	return false
}
