package btree

import (
	"bytes"
	"testing"
)

func TestGetEmptyTree(t *testing.T) {
	tr := newTestTree(t, 64)
	_, found := tr.Get([]byte("anything"))
	if found {
		t.Error("Get on empty tree should return not found")
	}
}

func TestPutAndGetSingle(t *testing.T) {
	tr := newTestTree(t, 64)

	old, replaced, err := tr.Put(inlineEntry([]byte("hello"), []byte("world")))
	if err != nil {
		t.Fatal(err)
	}
	if replaced {
		t.Error("first Put should not replace")
	}
	if old.Key != nil {
		t.Error("old entry key should be nil")
	}
	if tr.Root() == 0 {
		t.Fatal("root should be set after Put")
	}

	entry, found := tr.Get([]byte("hello"))
	if !found {
		t.Fatal("Get should find the key")
	}
	if !bytes.Equal(entry.Value, []byte("world")) {
		t.Errorf("value = %q, want %q", entry.Value, "world")
	}
}

func TestPutReplace(t *testing.T) {
	tr := newTestTree(t, 64)

	tr.Put(inlineEntry([]byte("key"), []byte("v1")))
	old, replaced, err := tr.Put(inlineEntry([]byte("key"), []byte("v2")))
	if err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Error("second Put should replace")
	}
	if !bytes.Equal(old.Value, []byte("v1")) {
		t.Errorf("old value = %q, want %q", old.Value, "v1")
	}

	entry, found := tr.Get([]byte("key"))
	if !found {
		t.Fatal("Get should find the key")
	}
	if !bytes.Equal(entry.Value, []byte("v2")) {
		t.Errorf("value = %q, want %q", entry.Value, "v2")
	}
}

func TestPutMultiple(t *testing.T) {
	tr := newTestTree(t, 64)

	for i := range 20 {
		_, _, err := tr.Put(inlineEntry(testKey(i), testVal(i)))
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}

	// Verify all keys.
	for i := range 20 {
		entry, found := tr.Get(testKey(i))
		if !found {
			t.Errorf("Get(%d) not found", i)
			continue
		}
		if !bytes.Equal(entry.Value, testVal(i)) {
			t.Errorf("Get(%d) value = %q, want %q", i, entry.Value, testVal(i))
		}
	}

	// Non-existent key.
	_, found := tr.Get([]byte("nonexistent"))
	if found {
		t.Error("Get for nonexistent key should return false")
	}
}

func TestPutLeafSplit(t *testing.T) {
	tr := newTestTree(t, 256)

	// Insert enough entries to force a leaf split.
	// At 4KB page size with ~9-byte keys and ~9-byte values, a leaf holds
	// roughly 150-200 entries. Insert 300 to guarantee at least one split.
	n := 300
	for i := range n {
		_, _, err := tr.Put(inlineEntry(testKey(i), testVal(i)))
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}

	// Verify all keys survive the split.
	for i := range n {
		entry, found := tr.Get(testKey(i))
		if !found {
			t.Errorf("Get(%d) not found after split", i)
			continue
		}
		if !bytes.Equal(entry.Value, testVal(i)) {
			t.Errorf("Get(%d) value = %q, want %q", i, entry.Value, testVal(i))
		}
	}
}

func TestPutBranchSplit(t *testing.T) {
	tr := newTestTree(t, 1024)

	// Insert a large number of entries to force branch splits (depth > 2).
	// With 4KB pages, a branch holds ~200-300 separators. So we need
	// many leaf splits to fill a branch.
	n := 5000
	for i := range n {
		_, _, err := tr.Put(inlineEntry(testKey(i), testVal(i)))
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}

	// Verify all keys.
	for i := range n {
		entry, found := tr.Get(testKey(i))
		if !found {
			t.Errorf("Get(%d) not found after branch split", i)
			continue
		}
		if !bytes.Equal(entry.Value, testVal(i)) {
			t.Errorf("Get(%d) value mismatch", i)
		}
	}
}

func TestPutNoSpace(t *testing.T) {
	// Tiny tree with very few pages: 2 meta + 1 bitmap + 1 data = 4 pages.
	// First Put uses 1 data page. Second Put that causes a split has no space.
	tr := newTestTree(t, 5) // 2 meta + 1 bitmap + 2 data pages

	// First entry works.
	_, _, err := tr.Put(inlineEntry([]byte("a"), []byte("1")))
	if err != nil {
		t.Fatal(err)
	}

	// Fill the first leaf until it needs to split.
	var lastErr error
	for i := 1; i < 1000; i++ {
		_, _, err := tr.Put(inlineEntry(testKey(i), testVal(i)))
		if err != nil {
			lastErr = err
			break
		}
	}
	if lastErr == nil {
		t.Error("expected ErrNoSpace eventually")
	}
}
