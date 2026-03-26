package btree

import (
	"bytes"
	"testing"
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
