package btree

import (
	"bytes"
	"fmt"
	"slices"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

func TestDeleteRangeEmptyTree(t *testing.T) {
	tr := newTestTree(t, 64)
	n, err := tr.DeleteRange([]byte("a"), []byte("z"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("deleted = %d, want 0", n)
	}
}

func TestDeleteRangeAll(t *testing.T) {
	tr := newTestTree(t, 256)
	for i := range 100 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	n, err := tr.DeleteRange(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Errorf("deleted = %d, want 100", n)
	}
	if tr.Root() != 0 {
		t.Error("tree should be empty")
	}
}

func TestDeleteRangeSameLeaf(t *testing.T) {
	tr := newTestTree(t, 64)
	for i := range 10 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Delete keys 3-6 (inclusive of 3, exclusive of 7).
	n, err := tr.DeleteRange(testKey(3), testKey(7))
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("deleted = %d, want 4", n)
	}

	// Verify remaining.
	for i := range 10 {
		_, found := tr.Get(testKey(i))
		if i >= 3 && i < 7 {
			if found {
				t.Errorf("key %d should be deleted", i)
			}
		} else {
			if !found {
				t.Errorf("key %d should exist", i)
			}
		}
	}
}

func TestDeleteRangeCrossLeaf(t *testing.T) {
	tr := newTestTree(t, 512)
	n := 500
	for i := range n {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Delete middle third.
	start := testKey(150)
	end := testKey(350)
	deleted, err := tr.DeleteRange(start, end)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 200 {
		t.Errorf("deleted = %d, want 200", deleted)
	}

	// Verify via cursor scan.
	c := tr.NewCursor()
	var remaining []string
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		remaining = append(remaining, string(k))
	}
	if len(remaining) != 300 {
		t.Errorf("remaining = %d, want 300", len(remaining))
	}
	if !slices.IsSorted(remaining) {
		t.Error("remaining keys not sorted")
	}
}

func TestDeleteRangeNilStart(t *testing.T) {
	tr := newTestTree(t, 256)
	for i := range 100 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	deleted, err := tr.DeleteRange(nil, testKey(50))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 50 {
		t.Errorf("deleted = %d, want 50", deleted)
	}

	// First key should be testKey(50).
	c := tr.NewCursor()
	k, _ := c.First()
	if !bytes.Equal(k, testKey(50)) {
		t.Errorf("first key = %q, want %q", k, testKey(50))
	}
}

func TestDeleteRangeNilEnd(t *testing.T) {
	tr := newTestTree(t, 256)
	for i := range 100 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	deleted, err := tr.DeleteRange(testKey(50), nil)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 50 {
		t.Errorf("deleted = %d, want 50", deleted)
	}

	// Last key should be testKey(49).
	c := tr.NewCursor()
	k, _ := c.Last()
	if !bytes.Equal(k, testKey(49)) {
		t.Errorf("last key = %q, want %q", k, testKey(49))
	}
}

func TestDeleteRangeEmptyRange(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := 0; i < 100; i += 2 { // even keys only
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Range that contains no keys.
	deleted, err := tr.DeleteRange(testKey(1), testKey(2))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 (empty range)", deleted)
	}
}

func TestDeleteRangeWithOverflow(t *testing.T) {
	tr := newTestTree(t, 256)

	// Insert some overflow entries.
	for i := range 20 {
		key := testKey(i)
		if i%5 == 0 {
			tr.Put(page.LeafEntry{Key: key, CellFlags: page.CellFlagOverflow, OvflPage: uint64(100 + i), TotalLen: 50000})
		} else {
			tr.Put(inlineEntry(key, testVal(i)))
		}
	}

	deleted, err := tr.DeleteRange(testKey(0), testKey(10))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 10 {
		t.Errorf("deleted = %d, want 10", deleted)
	}

	// Verify remaining.
	for i := 10; i < 20; i++ {
		_, found := tr.Get(testKey(i))
		if !found {
			t.Errorf("key %d should exist", i)
		}
	}
}

func TestDeleteRangeWithNestedTree(t *testing.T) {
	tr := newTestTree(t, 256)

	// Build a real nested tree that the subtree retirement can walk.
	nested := New(tr.data, tr.cfg, tr.bm, 0)
	nested.Put(inlineEntry([]byte("v1"), []byte{}))
	nested.Put(inlineEntry([]byte("v2"), []byte{}))
	nestedRoot := nested.Root()

	// Insert the nested tree reference into the main tree.
	tr.Put(page.LeafEntry{
		Key:         []byte("key_with_nested"),
		CellFlags:   page.CellFlagMultiValue | page.CellFlagNestedTree,
		NestedRoot:  nestedRoot,
		NestedCount: 2,
	})
	tr.Put(inlineEntry([]byte("other_key"), []byte("val")))

	deleted, err := tr.DeleteRange(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	if tr.Root() != 0 {
		t.Error("tree should be empty")
	}
}

func TestDeleteRangeWithMixedCellTypes(t *testing.T) {
	tr := newTestTree(t, 256)

	for i := range 20 {
		key := testKey(i)
		if i%5 == 0 {
			// Overflow entry (fake page ID — the overflow chain retirement
			// reads the page header which will be zeroed, so AdditionalPages=0).
			tr.Put(page.LeafEntry{
				Key:       key,
				CellFlags: page.CellFlagOverflow,
				OvflPage:  uint64(200 + i),
				TotalLen:  50000,
			})
		} else {
			tr.Put(inlineEntry(key, testVal(i)))
		}
	}

	deleted, err := tr.DeleteRange(testKey(0), testKey(10))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 10 {
		t.Errorf("deleted = %d, want 10", deleted)
	}
}

func TestDeleteRangeAllocFail(t *testing.T) {
	tr := newTinyTree(t, 1)
	tr.Put(inlineEntry([]byte("a"), []byte("1")))
	tr.Reset(tr.Root())

	_, err := tr.DeleteRange(nil, nil)
	if err == nil {
		t.Error("expected ErrNoSpace")
	}
}

func TestDeleteRangeBranchCowFail(t *testing.T) {
	// 0 free pages → branch CoW fails (line 111).
	tr := newTinyTree(t, 3)
	bigVal := bytes.Repeat([]byte("v"), 1400)
	tr.Put(page.LeafEntry{Key: []byte("aaa"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("bbb"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("ccc"), Value: bigVal})
	tr.Reset(tr.Root())

	_, err := tr.DeleteRange(nil, nil)
	if err == nil {
		t.Error("expected ErrNoSpace on branch CoW")
	}
}

func TestDeleteRangeLeftChildCowFail(t *testing.T) {
	// 1 free page → branch CoW succeeds (0 left) → left child CoW fails (line 133).
	tr := newTinyTree(t, 4)
	bigVal := bytes.Repeat([]byte("v"), 1400)
	tr.Put(page.LeafEntry{Key: []byte("aaa"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("bbb"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("ccc"), Value: bigVal})
	// 3 pages used, 1 free.
	tr.Reset(tr.Root())

	_, err := tr.DeleteRange(nil, nil)
	if err == nil {
		t.Error("expected ErrNoSpace on left child CoW")
	}
}

func TestDeleteRangeCrossLeafAllocFail(t *testing.T) {
	// Tree: left=["aaa","aab"], right=["bbb","ccc"], branch. 3 pages, 2 free.
	// DeleteRange("aab", "ccc"): branch CoW(1) + left CoW(1) + right CoW → 0 free → error.
	tr := newTinyTree(t, 5)
	bigVal := bytes.Repeat([]byte("v"), 1400)
	tr.Put(page.LeafEntry{Key: []byte("aaa"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("bbb"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("ccc"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("aab"), Value: bigVal})

	tr.Reset(tr.Root())
	// Delete partial range: left leaf keeps "aaa" (not freed), right leaf needs CoW → fails.
	_, err := tr.DeleteRange([]byte("aab"), []byte("ccc"))
	if err == nil {
		t.Error("expected ErrNoSpace on right boundary CoW")
	}
}

func TestDeleteRangeLargeTree(t *testing.T) {
	tr := newTestTree(t, 4096)
	bigVal := bytes.Repeat([]byte("v"), 500)
	n := 2000
	for i := range n {
		key := fmt.Appendf(nil, "key:%05d", i)
		tr.Put(page.LeafEntry{Key: key, Value: bigVal})
	}

	// Delete first half.
	start := fmt.Appendf(nil, "key:%05d", 0)
	end := fmt.Appendf(nil, "key:%05d", 1000)
	deleted, err := tr.DeleteRange(start, end)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1000 {
		t.Errorf("deleted = %d, want 1000", deleted)
	}

	// Full scan to verify remaining.
	c := tr.NewCursor()
	count := 0
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		count++
	}
	if count != 1000 {
		t.Errorf("remaining = %d, want 1000", count)
	}
}
