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

// assertNoZeroChildren walks every branch in the tree and fails if any
// branch has Ptr0==0 or a cell with childPtr==0.
func assertNoZeroChildren(t *testing.T, tr *Tree, pageID uint64) {
	t.Helper()
	if pageID == 0 {
		return
	}
	buf := tr.pageSlice(pageID)
	typ, _, _, _ := page.ReadHeader(buf)
	if typ == page.TypeLeaf {
		return
	}
	br := page.NewBranchReader(buf)
	if br.Ptr0() == 0 {
		t.Fatalf("branch page %d has zero Ptr0", pageID)
	}
	assertNoZeroChildren(t, tr, br.Ptr0())
	for i := range br.Count() {
		if br.ChildPtr(i) == 0 {
			t.Fatalf("branch page %d cell %d has zero childPtr", pageID, i)
		}
		assertNoZeroChildren(t, tr, br.ChildPtr(i))
	}
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
		assertNoZeroChildren(t, tr, tr.Root())
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
		assertNoZeroChildren(t, tr, tr.Root())
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
		assertNoZeroChildren(t, tr, tr.Root())
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
			assertNoZeroChildren(t, tr, tr.Root())
		}
	}
	assertNoZeroChildren(t, tr, tr.Root())
}

// TestRebalanceChildRemoveEmptyPtr0 exercises removeChild: Ptr0's child
// becomes empty and the sibling is promoted to Ptr0.
func TestRebalanceChildRemoveEmptyPtr0(t *testing.T) {
	tr := newTestTree(t, 64)
	bigVal := bytes.Repeat([]byte("v"), 3000)

	tr.Put(page.LeafEntry{Key: []byte("aaa"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("bbb"), Value: bigVal})
	tr.Put(page.LeafEntry{Key: []byte("ccc"), Value: bigVal})
	assertNoZeroChildren(t, tr, tr.Root())

	// Delete Ptr0's only entry → empty child → removeChild promotes sibling.
	tr.Delete([]byte("aaa"))
	assertNoZeroChildren(t, tr, tr.Root())

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
			assertNoZeroChildren(t, tr, tr.Root())

			// Full cursor scan to verify no corruption.
			c := tr.NewCursor()
			for k, _ := c.First(); k != nil; k, _ = c.Next() {
			}
		})
	}
}
