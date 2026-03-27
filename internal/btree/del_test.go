package btree

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

func TestDeleteEmptyTree(t *testing.T) {
	tr := newTestTree(t, 64)
	_, found, err := tr.Delete([]byte("anything"))
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("Delete on empty tree should return not found")
	}
}

func TestDeleteNotFound(t *testing.T) {
	tr := newTestTree(t, 64)
	tr.Put(inlineEntry([]byte("a"), []byte("1")))

	_, found, err := tr.Delete([]byte("b"))
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("Delete of nonexistent key should return not found")
	}
}

func TestDeleteSingleEntry(t *testing.T) {
	tr := newTestTree(t, 64)
	tr.Put(inlineEntry([]byte("key"), []byte("val")))

	old, found, err := tr.Delete([]byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("Delete should find the key")
	}
	if !bytes.Equal(old.Value, []byte("val")) {
		t.Errorf("old value = %q, want %q", old.Value, "val")
	}
	if tr.Root() != 0 {
		t.Error("root should be 0 after deleting last entry")
	}

	_, found = tr.Get([]byte("key"))
	if found {
		t.Error("Get after Delete should return not found")
	}
}

func TestDeleteMultiple(t *testing.T) {
	tr := newTestTree(t, 128)

	n := 50
	for i := range n {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Delete every other key.
	for i := 0; i < n; i += 2 {
		old, found, err := tr.Delete(testKey(i))
		if err != nil {
			t.Fatalf("Delete(%d): %v", i, err)
		}
		if !found {
			t.Fatalf("Delete(%d) not found", i)
		}
		if !bytes.Equal(old.Value, testVal(i)) {
			t.Errorf("Delete(%d) old value mismatch", i)
		}
	}

	// Verify remaining keys.
	for i := range n {
		_, found := tr.Get(testKey(i))
		if i%2 == 0 {
			if found {
				t.Errorf("Get(%d) should be deleted", i)
			}
		} else {
			if !found {
				t.Errorf("Get(%d) not found", i)
			}
		}
	}
}

func TestDeleteAll(t *testing.T) {
	tr := newTestTree(t, 128)

	n := 100
	for i := range n {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Delete all keys.
	for i := range n {
		_, found, err := tr.Delete(testKey(i))
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

func TestDeleteWithSplitAndMerge(t *testing.T) {
	tr := newTestTree(t, 512)

	// Insert enough to cause splits.
	n := 500
	for i := range n {
		_, _, err := tr.Put(inlineEntry(testKey(i), testVal(i)))
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}

	// Delete all keys — this will trigger merges and root shrinks.
	for i := range n {
		_, found, err := tr.Delete(testKey(i))
		if err != nil {
			t.Fatalf("Delete(%d): %v", i, err)
		}
		if !found {
			t.Fatalf("Delete(%d) not found", i)
		}

		// Verify remaining keys still work.
		for j := i + 1; j < n; j += 50 { // spot check
			_, found := tr.Get(testKey(j))
			if !found {
				t.Fatalf("Get(%d) not found after deleting %d", j, i)
			}
		}
	}

	if tr.Root() != 0 {
		t.Error("root should be 0 after deleting all entries")
	}
}

func TestDeleteReverseOrder(t *testing.T) {
	tr := newTestTree(t, 512)

	n := 500
	for i := range n {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Delete in reverse order.
	for i := n - 1; i >= 0; i-- {
		_, found, err := tr.Delete(testKey(i))
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

// subtreeKeyRange returns the smallest and largest key in the subtree
// rooted at pageID. Panics if pageID is 0 or the subtree is empty.
func subtreeKeyRange(tr *Tree, pageID uint64) (minKey, maxKey []byte) {
	buf := tr.pageSlice(pageID)
	typ, _, _, _ := page.ReadHeader(buf)
	if typ == page.TypeLeaf {
		lr := page.NewLeafReader(buf, tr.cfg.Page)
		if lr.Count() == 0 {
			return nil, nil
		}
		first, _ := lr.EntryAt(0, nil)
		last, _ := lr.EntryAt(lr.Count()-1, nil)
		return bytes.Clone(first.Key), bytes.Clone(last.Key)
	}
	br := page.NewBranchReader(buf)
	// Min key is in the leftmost subtree (Ptr0).
	minKey, _ = subtreeKeyRange(tr, br.Ptr0())
	// Max key is in the rightmost subtree (last child pointer).
	if br.Count() > 0 {
		_, maxKey = subtreeKeyRange(tr, br.ChildPtr(br.Count()-1))
	} else {
		_, maxKey = subtreeKeyRange(tr, br.Ptr0())
	}
	return minKey, maxKey
}

// assertTreeValid checks all structural invariants of the B+tree:
//  1. No zero children: every branch has non-zero Ptr0 and child pointers.
//  2. Separator correctness: for each branch, every key in the left subtree
//     of a separator is strictly less than the separator, and every key in
//     the right subtree is >= the separator.
//  3. Uniform leaf depth: all leaves are at the same depth from the root.
//  4. Retired and CoW pages are disjoint sets.
func assertTreeValid(t *testing.T, tr *Tree) {
	t.Helper()
	root := tr.Root()
	if root == 0 {
		return
	}

	// Invariant 4: retired and CoW pages are disjoint.
	cow := tr.CowPages()
	for _, rp := range tr.Retired() {
		if _, ok := cow[rp]; ok {
			t.Fatalf("retired page %d is also in CoW set", rp)
		}
	}

	// Walk the tree checking invariants 1-3.
	leafDepth := -1
	var walk func(pageID uint64, depth int)
	walk = func(pageID uint64, depth int) {
		t.Helper()
		buf := tr.pageSlice(pageID)
		typ, _, _, _ := page.ReadHeader(buf)

		if typ == page.TypeLeaf {
			// Invariant 3: all leaves at the same depth.
			if leafDepth == -1 {
				leafDepth = depth
			} else if depth != leafDepth {
				t.Fatalf("leaf page %d at depth %d, expected %d", pageID, depth, leafDepth)
			}
			return
		}

		br := page.NewBranchReader(buf)

		// Invariant 1: no zero children.
		if br.Ptr0() == 0 {
			t.Fatalf("branch page %d has zero Ptr0", pageID)
		}
		for i := range br.Count() {
			if br.ChildPtr(i) == 0 {
				t.Fatalf("branch page %d cell %d has zero childPtr", pageID, i)
			}
		}

		// Invariant 2: separator correctness.
		// For separator Key[i]:
		//   - The left child (Ptr0 for i==0, ChildPtr(i-1) for i>0) must
		//     have all keys < Key[i].
		//   - The right child ChildPtr(i) must have all keys >= Key[i].
		for i := range br.Count() {
			sep := br.Key(i)

			// Left subtree of separator i.
			var leftChild uint64
			if i == 0 {
				leftChild = br.Ptr0()
			} else {
				leftChild = br.ChildPtr(i - 1)
			}
			_, leftMax := subtreeKeyRange(tr, leftChild)
			if leftMax != nil && bytes.Compare(leftMax, sep) >= 0 {
				t.Fatalf("branch page %d separator[%d]=%q: left subtree max key %q >= separator",
					pageID, i, sep, leftMax)
			}

			rightChild := br.ChildPtr(i)
			rightMin, _ := subtreeKeyRange(tr, rightChild)
			if rightMin != nil && bytes.Compare(rightMin, sep) < 0 {
				t.Fatalf("branch page %d separator[%d]=%q: right subtree min key %q < separator",
					pageID, i, sep, rightMin)
			}
		}

		// Recurse into all children.
		walk(br.Ptr0(), depth+1)
		for i := range br.Count() {
			walk(br.ChildPtr(i), depth+1)
		}
	}
	walk(root, 0)
}

// TestRebalanceChildLeafMerge exercises leaf merge and verifies no zero
// children after each deletion.
func TestRebalanceChildLeafMerge(t *testing.T) {
	tr := newTestTree(t, 256)
	bigVal := bytes.Repeat([]byte("v"), 500)
	n := 100
	for i := range n {
		key := fmt.Appendf(nil, "key:%05d", i)
		tr.Put(page.LeafEntry{Key: key, Value: bigVal})
	}

	for i := range n / 2 {
		key := fmt.Appendf(nil, "key:%05d", i)
		tr.Delete(key)
		assertTreeValid(t, tr)
	}
}

// TestRebalanceChildLeafRedistribute exercises leaf redistribute by
// deleting entries that make leaves underfull but too large to merge
// with their sibling.
func TestRebalanceChildLeafRedistribute(t *testing.T) {
	tr := newTestTree(t, 512)
	val := bytes.Repeat([]byte("v"), 400)
	n := 200
	for i := range n {
		key := fmt.Appendf(nil, "key:%05d", i)
		tr.Put(page.LeafEntry{Key: key, Value: val})
	}

	for i := 0; i < n; i += 3 {
		key := fmt.Appendf(nil, "key:%05d", i)
		tr.Delete(key)
		assertTreeValid(t, tr)
	}
}

// TestRebalanceChildBranchMerge exercises branch merge via cascading
// deletes of all entries.
func TestRebalanceChildBranchMerge(t *testing.T) {
	tr := newTestTree(t, 4096)
	bigVal := bytes.Repeat([]byte("x"), 500)
	n := 2000
	for i := range n {
		key := fmt.Appendf(nil, "key:%05d", i)
		tr.Put(page.LeafEntry{Key: key, Value: bigVal})
	}

	for i := range n {
		key := fmt.Appendf(nil, "key:%05d", i)
		tr.Delete(key)
		assertTreeValid(t, tr)
	}
}

// TestRebalanceChildBranchRedistribute exercises branch redistribute
// with long keys (large separators reduce branch capacity, forcing
// redistribute instead of merge).
func TestRebalanceChildBranchRedistribute(t *testing.T) {
	tr := newTestTree(t, 16384)
	prefix := bytes.Repeat([]byte("a"), 300)
	n := 6000
	for i := range n {
		key := fmt.Appendf(prefix[:100:100], "%04d", i)
		tr.Put(page.LeafEntry{Key: key, Value: []byte("v")})
	}

	for i := range n / 3 {
		key := fmt.Appendf(prefix[:100:100], "%04d", i)
		tr.Delete(key)
		if i%100 == 0 {
			assertTreeValid(t, tr)
		}
	}
	assertTreeValid(t, tr)
}

// TestRebalanceChildRemoveEmptyPtr0 exercises removeChild: Ptr0's child
// becomes empty and the sibling is promoted to Ptr0.
func TestRebalanceChildRemoveEmptyPtr0(t *testing.T) {
	tr := newTestTree(t, 64)
	bigVal := bytes.Repeat([]byte("v"), 3000)

	tr.Put(page.LeafEntry{Key: []byte("aaa"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("bbb"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("ccc"), Value: bigVal})
	assertTreeValid(t, tr)

	// Delete Ptr0's only entry → empty child → removeChild promotes sibling.
	tr.Delete([]byte("aaa"))
	assertTreeValid(t, tr)

	if _, found := tr.Get([]byte("bbb")); !found {
		t.Error("bbb should exist")
	}
	if _, found := tr.Get([]byte("ccc")); !found {
		t.Error("ccc should exist")
	}
}

// TestRebalanceChildAfterDeleteRange exercises the rebalance loop in
// deleteRangeBranch across various range shapes.
func TestRebalanceChildAfterDeleteRange(t *testing.T) {
	for _, tc := range []struct {
		name       string
		n          int
		valSize    int
		start, end int
	}{
		{"left-quarter", 200, 500, 0, 50},
		{"middle-half", 200, 500, 50, 150},
		{"right-quarter", 200, 500, 150, 200},
		{"single-leaf-range", 200, 500, 10, 20},
		{"all-but-edges", 200, 500, 5, 195},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestTree(t, 4096)
			val := bytes.Repeat([]byte("v"), tc.valSize)
			for i := range tc.n {
				key := fmt.Appendf(nil, "key:%05d", i)
				tr.Put(page.LeafEntry{Key: key, Value: val})
			}

			var start, end []byte
			if tc.start > 0 {
				start = fmt.Appendf(nil, "key:%05d", tc.start)
			}
			if tc.end < tc.n {
				end = fmt.Appendf(nil, "key:%05d", tc.end)
			}

			_, err := tr.DeleteRange(start, end)
			if err != nil {
				t.Fatalf("DeleteRange: %v", err)
			}
			assertTreeValid(t, tr)

			// Full cursor scan to verify no corruption.
			c := tr.NewCursor()
			for k, _ := c.First(); k != nil; k, _ = c.Next() {
			}
		})
	}
}
