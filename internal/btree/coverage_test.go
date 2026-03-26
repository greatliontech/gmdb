package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// TestBranchSplit uses large values to reduce entries per leaf, forcing
// many leaf splits and eventually a branch split.
func TestBranchSplit(t *testing.T) {
	tr := newTestTree(t, 4096)

	// 500-byte values: ~7 entries per leaf → branch fills after ~240 leaves.
	bigVal := bytes.Repeat([]byte("x"), 500)
	n := 2000
	for i := range n {
		key := fmt.Appendf(nil, "key:%05d", i)
		_, _, err := tr.Put(page.LeafEntry{Key: key, Value: bigVal})
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}

	// Verify all keys.
	for i := range n {
		key := fmt.Appendf(nil, "key:%05d", i)
		entry, found := tr.Get(key)
		if !found {
			t.Fatalf("Get(%d) not found after branch split", i)
		}
		if !bytes.Equal(entry.Value, bigVal) {
			t.Fatalf("Get(%d) value mismatch", i)
		}
	}
}

// TestBranchMerge deletes enough entries to trigger branch-level merges.
func TestBranchMerge(t *testing.T) {
	tr := newTestTree(t, 4096)

	bigVal := bytes.Repeat([]byte("x"), 500)
	n := 2000
	for i := range n {
		key := fmt.Appendf(nil, "key:%05d", i)
		tr.Put(page.LeafEntry{Key: key, Value: bigVal})
	}

	// Delete all entries.
	for i := range n {
		key := fmt.Appendf(nil, "key:%05d", i)
		_, found, err := tr.Delete(key)
		if err != nil {
			t.Fatalf("Delete(%d): %v", i, err)
		}
		if !found {
			t.Fatalf("Delete(%d) not found", i)
		}
	}

	if tr.Root() != 0 {
		t.Error("root should be 0 after deleting all entries")
	}
}

// TestCowPageFromPreviousTx simulates a second transaction by resetting
// the cow set, forcing cowPage to allocate new copies.
func TestCowPageFromPreviousTx(t *testing.T) {
	tr := newTestTree(t, 256)

	// First "transaction": insert entries.
	for i := range 50 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Simulate new transaction: clear cow set, keep root.
	root := tr.Root()
	tr.Reset(root)

	// Now modifications must CoW from "mmap" pages.
	tr.Put(inlineEntry(testKey(50), testVal(50)))
	tr.Put(inlineEntry(testKey(0), []byte("updated")))

	// Verify.
	entry, found := tr.Get(testKey(0))
	if !found {
		t.Fatal("Get(0) not found")
	}
	if !bytes.Equal(entry.Value, []byte("updated")) {
		t.Errorf("value = %q, want %q", entry.Value, "updated")
	}

	entry, found = tr.Get(testKey(50))
	if !found {
		t.Fatal("Get(50) not found")
	}
	if !bytes.Equal(entry.Value, testVal(50)) {
		t.Errorf("value = %q, want %q", entry.Value, testVal(50))
	}

	// Retired should have pages from the first "transaction".
	if len(tr.Retired()) == 0 {
		t.Error("Retired() should have entries after modifying mmap pages")
	}

	// CowPages should have the modified pages.
	if len(tr.CowPages()) == 0 {
		t.Error("CowPages() should have entries")
	}
}

// TestDeleteFromPreviousTx simulates deleting in a second transaction.
func TestDeleteFromPreviousTx(t *testing.T) {
	tr := newTestTree(t, 256)

	for i := range 50 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	root := tr.Root()
	tr.Reset(root)

	// Delete in the new "transaction".
	old, found, err := tr.Delete(testKey(25))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("Delete should find key from previous tx")
	}
	if !bytes.Equal(old.Value, testVal(25)) {
		t.Errorf("old value = %q, want %q", old.Value, testVal(25))
	}

	// Should have retired pages.
	if len(tr.Retired()) == 0 {
		t.Error("Retired() should have entries")
	}
}

// TestOverflowEntry tests inserting and retrieving an overflow entry.
func TestOverflowEntry(t *testing.T) {
	tr := newTestTree(t, 64)

	e := page.LeafEntry{
		Key:       []byte("bigkey"),
		CellFlags: page.CellFlagOverflow,
		OvflPage:  42,
		TotalLen:  1000000,
	}
	_, _, err := tr.Put(e)
	if err != nil {
		t.Fatal(err)
	}

	got, found := tr.Get([]byte("bigkey"))
	if !found {
		t.Fatal("Get not found")
	}
	if got.CellFlags&page.CellFlagOverflow == 0 {
		t.Error("expected overflow flag")
	}
	if got.OvflPage != 42 {
		t.Errorf("OvflPage = %d, want 42", got.OvflPage)
	}
	if got.TotalLen != 1000000 {
		t.Errorf("TotalLen = %d, want 1000000", got.TotalLen)
	}
}

// TestSubpageEntry tests inserting and retrieving a subpage entry.
func TestSubpageEntry(t *testing.T) {
	tr := newTestTree(t, 64)

	// Create a minimal subpage: Count=1, DataSize=5, one 5-byte entry.
	subpage := make([]byte, 4+2+5) // header(4) + valueLen(2) + value(5)
	binary.LittleEndian.PutUint16(subpage[0:], 1)
	binary.LittleEndian.PutUint16(subpage[2:], 7) // DataSize = 2+5
	binary.LittleEndian.PutUint16(subpage[4:], 5) // ValueLen
	copy(subpage[6:], "hello")

	e := page.LeafEntry{
		Key:         []byte("setkey"),
		CellFlags:   page.CellFlagMultiValue,
		SubpageData: subpage,
	}
	_, _, err := tr.Put(e)
	if err != nil {
		t.Fatal(err)
	}

	got, found := tr.Get([]byte("setkey"))
	if !found {
		t.Fatal("Get not found")
	}
	if got.CellFlags&page.CellFlagMultiValue == 0 {
		t.Error("expected multi-value flag")
	}
	if !bytes.Equal(got.SubpageData, subpage) {
		t.Errorf("SubpageData mismatch")
	}
}

// TestNestedTreeEntry tests inserting and retrieving a nested tree entry.
func TestNestedTreeEntry(t *testing.T) {
	tr := newTestTree(t, 64)

	e := page.LeafEntry{
		Key:         []byte("nested"),
		CellFlags:   page.CellFlagMultiValue | page.CellFlagNestedTree,
		NestedRoot:  99,
		NestedCount: 500,
	}
	_, _, err := tr.Put(e)
	if err != nil {
		t.Fatal(err)
	}

	got, found := tr.Get([]byte("nested"))
	if !found {
		t.Fatal("Get not found")
	}
	if got.CellFlags != page.CellFlagMultiValue|page.CellFlagNestedTree {
		t.Errorf("CellFlags = %d, want %d", got.CellFlags, page.CellFlagMultiValue|page.CellFlagNestedTree)
	}
	if got.NestedRoot != 99 {
		t.Errorf("NestedRoot = %d, want 99", got.NestedRoot)
	}
	if got.NestedCount != 500 {
		t.Errorf("NestedCount = %d, want 500", got.NestedCount)
	}
}

// TestCursorValid tests the Valid() method.
func TestCursorValid(t *testing.T) {
	tr := newTestTree(t, 64)
	c := tr.NewCursor()

	if c.Valid() {
		t.Error("new cursor should not be valid")
	}

	tr.Put(inlineEntry([]byte("a"), []byte("1")))
	c.First()
	if !c.Valid() {
		t.Error("cursor should be valid after First")
	}

	c.Next()
	if c.Valid() {
		t.Error("cursor should not be valid past end")
	}
}

// TestCursorCurrentUnpositioned tests Current on an unpositioned cursor.
func TestCursorCurrentUnpositioned(t *testing.T) {
	tr := newTestTree(t, 64)
	c := tr.NewCursor()
	k, v := c.Current()
	if k != nil || v != nil {
		t.Error("Current on unpositioned cursor should return nil")
	}
}

// TestSeekGEAdvanceToNextLeaf verifies SeekGE correctly advances to the
// next leaf when the target is beyond all keys in the first matching leaf.
func TestSeekGEAdvanceToNextLeaf(t *testing.T) {
	tr := newTestTree(t, 256)

	// Insert enough entries to have multiple leaves.
	n := 300
	for i := range n {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// SeekGE for a key that should be in the second or later leaf.
	k, _ := tr.NewCursor().SeekGE(testKey(200))
	if !bytes.Equal(k, testKey(200)) {
		t.Errorf("SeekGE(200) = %q, want %q", k, testKey(200))
	}
}

// TestFreePage tests the loose page path (freeing a page that was cow'd
// in the same transaction).
func TestFreePage(t *testing.T) {
	tr := newTestTree(t, 128)

	// Insert entries, then delete to trigger freePage on cow'd pages.
	for i := range 30 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	initialFree := tr.bm.FreeCount()

	// Delete all — the leaf pages are cow'd and then freed (loose pages).
	for i := range 30 {
		tr.Delete(testKey(i))
	}

	// After deletion, freed pages should be returned to bitmap.
	if tr.bm.FreeCount() <= initialFree {
		t.Error("bitmap free count should increase after freeing cow'd pages")
	}
}

// TestPutReplaceAllCellTypes tests replacing entries across all cell formats.
func TestPutReplaceAllCellTypes(t *testing.T) {
	tr := newTestTree(t, 64)

	// Insert inline.
	tr.Put(inlineEntry([]byte("key"), []byte("inline")))

	// Replace with overflow.
	old, replaced, err := tr.Put(page.LeafEntry{
		Key:       []byte("key"),
		CellFlags: page.CellFlagOverflow,
		OvflPage:  10,
		TotalLen:  50000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Error("should replace")
	}
	if !bytes.Equal(old.Value, []byte("inline")) {
		t.Errorf("old value = %q, want %q", old.Value, "inline")
	}

	// Replace overflow with nested tree.
	old, replaced, err = tr.Put(page.LeafEntry{
		Key:         []byte("key"),
		CellFlags:   page.CellFlagMultiValue | page.CellFlagNestedTree,
		NestedRoot:  77,
		NestedCount: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Error("should replace")
	}
	if old.OvflPage != 10 {
		t.Errorf("old OvflPage = %d, want 10", old.OvflPage)
	}
}

// TestCursorSeekSingleLevelTree tests Seek on a tree with only a root leaf.
func TestCursorSeekSingleLevelTree(t *testing.T) {
	tr := newTestTree(t, 64)
	for i := range 5 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	k, _ := c.Seek(testKey(2))
	if !bytes.Equal(k, testKey(2)) {
		t.Errorf("Seek(2) = %q, want %q", k, testKey(2))
	}

	// SeekGE on single-level tree.
	k, _ = c.SeekGE(testKey(3))
	if !bytes.Equal(k, testKey(3)) {
		t.Errorf("SeekGE(3) = %q, want %q", k, testKey(3))
	}
}

// TestDeleteEmptyLeafChild tests the removeChild path: a non-root leaf
// becomes completely empty (had exactly 1 entry after split).
func TestDeleteEmptyLeafChild(t *testing.T) {
	tr := newTestTree(t, 64)

	// Use values large enough that a single entry exceeds 50% of page
	// capacity, so byte-based split gives left=[1 entry], right=[2 entries].
	bigVal := bytes.Repeat([]byte("v"), 3000)

	tr.Put(page.LeafEntry{Key: []byte("aaa"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("bbb"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("ccc"), Value: bigVal})

	// Tree should now have a branch root with 2 leaf children.
	// Left leaf has 1 entry ("aaa"), right leaf has 2 entries ("bbb", "ccc").

	// Delete "aaa" — the left leaf becomes empty.
	old, found, err := tr.Delete([]byte("aaa"))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("should find aaa")
	}
	if !bytes.Equal(old.Key, []byte("aaa")) {
		t.Errorf("old key = %q, want aaa", old.Key)
	}

	// Remaining entries should still be accessible.
	_, found = tr.Get([]byte("bbb"))
	if !found {
		t.Fatal("bbb should still exist")
	}
	_, found = tr.Get([]byte("ccc"))
	if !found {
		t.Fatal("ccc should still exist")
	}
}

// TestDeleteSubpageEntry tests deleting a subpage entry exercises the
// subpage clone path in cloneEntry.
func TestDeleteSubpageEntry(t *testing.T) {
	tr := newTestTree(t, 64)

	subpage := make([]byte, 4+2+5)
	binary.LittleEndian.PutUint16(subpage[0:], 1)
	binary.LittleEndian.PutUint16(subpage[2:], 7)
	binary.LittleEndian.PutUint16(subpage[4:], 5)
	copy(subpage[6:], "hello")

	tr.Put(page.LeafEntry{Key: []byte("setkey"), CellFlags: page.CellFlagMultiValue, SubpageData: subpage})

	old, found, err := tr.Delete([]byte("setkey"))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("should find setkey")
	}
	if old.CellFlags&page.CellFlagMultiValue == 0 {
		t.Error("old should have multi-value flag")
	}
	if !bytes.Equal(old.SubpageData, subpage) {
		t.Error("old SubpageData mismatch")
	}
}

// TestCollectEntriesWithSubpage ensures collectEntries clones SubpageData
// when reading a leaf that contains subpage entries.
func TestCollectEntriesWithSubpage(t *testing.T) {
	tr := newTestTree(t, 64)

	subpage := make([]byte, 4+2+5)
	binary.LittleEndian.PutUint16(subpage[0:], 1)
	binary.LittleEndian.PutUint16(subpage[2:], 7)
	binary.LittleEndian.PutUint16(subpage[4:], 5)
	copy(subpage[6:], "hello")

	// Insert a subpage entry, then another entry into the same leaf.
	// The second Put triggers collectEntries on the leaf containing the subpage.
	tr.Put(page.LeafEntry{Key: []byte("aaa"), CellFlags: page.CellFlagMultiValue, SubpageData: subpage})
	tr.Put(inlineEntry([]byte("bbb"), []byte("val")))

	// Verify both entries survive.
	got, found := tr.Get([]byte("aaa"))
	if !found {
		t.Fatal("aaa not found")
	}
	if !bytes.Equal(got.SubpageData, subpage) {
		t.Error("SubpageData mismatch after second Put")
	}
}

// TestDeleteLeafToEmpty tests deleting entries until a non-root leaf is empty.
func TestDeleteLeafToEmpty(t *testing.T) {
	tr := newTestTree(t, 512)
	bigVal := bytes.Repeat([]byte("v"), 500)

	// Insert enough for multiple leaves.
	n := 100
	for i := range n {
		key := fmt.Appendf(nil, "key:%05d", i)
		tr.Put(page.LeafEntry{Key: key, Value: bigVal})
	}

	// Delete entries one by one. This should exercise empty leaf handling.
	for i := range n {
		key := fmt.Appendf(nil, "key:%05d", i)
		_, found, err := tr.Delete(key)
		if err != nil {
			t.Fatalf("Delete(%d): %v", i, err)
		}
		if !found {
			t.Fatalf("Delete(%d) not found", i)
		}
	}

	if tr.Root() != 0 {
		t.Error("tree should be empty")
	}
}

// TestFreePagePanicsOnNonCow verifies the invariant guard in freePage:
// calling freePage on a non-CoW'd page panics.
func TestFreePagePanicsOnNonCow(t *testing.T) {
	tr := newTestTree(t, 64)
	// Allocate a page so the tree has valid data.
	tr.Put(inlineEntry([]byte("a"), []byte("1")))

	defer func() {
		if r := recover(); r == nil {
			t.Error("freePage on non-CoW'd page should panic")
		}
	}()

	// Reset clears the CoW set. Page 3 (the leaf) is no longer CoW'd.
	root := tr.Root()
	tr.Reset(root)
	tr.freePage(root) // should panic
}

// TestRebalanceChildNoCandidates calls rebalanceChild with a single-child
// branch (Ptr0 only, no cells). No sibling exists, so len(candidates)==0
// and the function returns early.
func TestRebalanceChildNoCandidates(t *testing.T) {
	tr := newTestTree(t, 64)
	ptr0, cells, err := tr.rebalanceChild(42, nil, -1)
	if err != nil {
		t.Fatal(err)
	}
	if ptr0 != 42 {
		t.Errorf("ptr0 = %d, want 42", ptr0)
	}
	if cells != nil {
		t.Errorf("cells should be nil")
	}
}

// TestFindLeafSplitPointOverflowFirst tests findLeafSplitPoint when the
// first entry is too large to fit on an empty page. Returns 1 (split after
// the oversized entry).
func TestFindLeafSplitPointOverflowFirst(t *testing.T) {
	tr := newTestTree(t, 64)
	hugeKey := bytes.Repeat([]byte("x"), testPageSize)
	entries := []page.LeafEntry{
		{Key: hugeKey, Value: []byte("v")},
		{Key: []byte("b"), Value: []byte("v")},
	}
	idx := tr.findLeafSplitPoint(entries)
	if idx != 1 {
		t.Errorf("split point = %d, want 1", idx)
	}
}

// TestFindLeafSplitPointAllFit tests findLeafSplitPoint when all entries
// fit on a single page with fresh compression. Falls back to len/2.
func TestFindLeafSplitPointAllFit(t *testing.T) {
	tr := newTestTree(t, 64)
	entries := []page.LeafEntry{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte("2")},
		{Key: []byte("c"), Value: []byte("3")},
		{Key: []byte("d"), Value: []byte("4")},
	}
	idx := tr.findLeafSplitPoint(entries)
	if idx != len(entries)/2 {
		t.Errorf("split point = %d, want %d", idx, len(entries)/2)
	}
}

// TestFindBranchSplitPointOverflowFirst tests findBranchSplitPoint when
// the first cell is too large to fit on an empty branch page. Returns 1.
func TestFindBranchSplitPointOverflowFirst(t *testing.T) {
	tr := newTestTree(t, 64)
	hugeKey := bytes.Repeat([]byte("x"), testPageSize)
	cells := []branchCell{
		{key: hugeKey, childPtr: 10},
		{key: []byte("b"), childPtr: 11},
	}
	idx := tr.findBranchSplitPoint(1, cells)
	if idx != 1 {
		t.Errorf("split point = %d, want 1", idx)
	}
}

// TestFindBranchSplitPointOverflowMid tests findBranchSplitPoint when the
// second cell overflows (first fits but second doesn't). Returns i=1.
func TestFindBranchSplitPointOverflowMid(t *testing.T) {
	tr := newTestTree(t, 64)
	hugeKey := bytes.Repeat([]byte("x"), testPageSize)
	cells := []branchCell{
		{key: []byte("a"), childPtr: 10},
		{key: hugeKey, childPtr: 11},
		{key: []byte("c"), childPtr: 12},
	}
	idx := tr.findBranchSplitPoint(1, cells)
	if idx != 1 {
		t.Errorf("split point = %d, want 1", idx)
	}
}

// TestFindBranchSplitPointAllFit tests findBranchSplitPoint when all cells
// fit on a single branch page. Falls back to len/2.
func TestFindBranchSplitPointAllFit(t *testing.T) {
	tr := newTestTree(t, 64)
	cells := []branchCell{
		{key: []byte("a"), childPtr: 10},
		{key: []byte("b"), childPtr: 11},
		{key: []byte("c"), childPtr: 12},
		{key: []byte("d"), childPtr: 13},
	}
	idx := tr.findBranchSplitPoint(1, cells)
	if idx != len(cells)/2 {
		t.Errorf("split point = %d, want %d", idx, len(cells)/2)
	}
}

// TestDeleteRangeRightBoundaryError tests error propagation when the right
// boundary child's cowPage fails during deleteRangeBranch.
func TestDeleteRangeRightBoundaryError(t *testing.T) {
	// Tree: Ptr0→[aaa,bbb](3), cell0=("c",→[ccc](4)), branch(5).
	// 3 pages used, 2 free. After Reset, cowPage(branch)+cowPage(left)=2 pages.
	// Right boundary cowPage → 0 free → error.
	tr := newTinyTree(t, 5)
	bigVal := bytes.Repeat([]byte("v"), 1400)
	tr.Put(page.LeafEntry{Key: []byte("aaa"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("bbb"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("ccc"), Value: bigVal})

	tr.Reset(tr.Root())
	// DeleteRange("bbb", nil): deletes bbb from left, ccc from right.
	// Branch CoW + left leaf CoW exhaust the 2 free pages.
	// Right boundary cowPage fails.
	_, err := tr.DeleteRange([]byte("bbb"), nil)
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("expected ErrNoSpace from right boundary, got %v", err)
	}
}
