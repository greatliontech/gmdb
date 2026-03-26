package btree

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

func TestCursorEmptyTree(t *testing.T) {
	tr := newTestTree(t, 64)
	c := tr.NewCursor()

	k, _ := c.First()
	if k != nil {
		t.Error("First on empty tree should return nil")
	}
	k, _ = c.Last()
	if k != nil {
		t.Error("Last on empty tree should return nil")
	}
	k, _ = c.Next()
	if k != nil {
		t.Error("Next on empty tree should return nil")
	}
	k, _ = c.Prev()
	if k != nil {
		t.Error("Prev on empty tree should return nil")
	}
	k, _ = c.Seek([]byte("x"))
	if k != nil {
		t.Error("Seek on empty tree should return nil")
	}
	k, _ = c.SeekGE([]byte("x"))
	if k != nil {
		t.Error("SeekGE on empty tree should return nil")
	}
}

func TestCursorForwardScan(t *testing.T) {
	tr := newTestTree(t, 256)
	n := 100
	for i := range n {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	var keys []string
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		keys = append(keys, string(k))
	}
	if len(keys) != n {
		t.Fatalf("forward scan got %d keys, want %d", len(keys), n)
	}
	// Keys should be sorted.
	for i := 1; i < len(keys); i++ {
		if keys[i-1] >= keys[i] {
			t.Fatalf("keys not sorted at index %d: %q >= %q", i, keys[i-1], keys[i])
		}
	}
}

func TestCursorReverseScan(t *testing.T) {
	tr := newTestTree(t, 256)
	n := 100
	for i := range n {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	var keys []string
	for k, _ := c.Last(); k != nil; k, _ = c.Prev() {
		keys = append(keys, string(k))
	}
	if len(keys) != n {
		t.Fatalf("reverse scan got %d keys, want %d", len(keys), n)
	}
	// Keys should be in reverse sorted order.
	for i := 1; i < len(keys); i++ {
		if keys[i-1] <= keys[i] {
			t.Fatalf("keys not reverse sorted at %d: %q <= %q", i, keys[i-1], keys[i])
		}
	}
}

func TestCursorSeek(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 50 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()

	// Seek for existing key.
	k, v := c.Seek(testKey(25))
	if k == nil {
		t.Fatal("Seek(25) returned nil")
	}
	if !bytes.Equal(k, testKey(25)) {
		t.Errorf("Seek key = %q, want %q", k, testKey(25))
	}
	if !bytes.Equal(v, testVal(25)) {
		t.Errorf("Seek value = %q, want %q", v, testVal(25))
	}

	// Seek for nonexistent key.
	k, _ = c.Seek([]byte("nonexistent"))
	if k != nil {
		t.Errorf("Seek for nonexistent key should return nil, got %q", k)
	}
}

func TestCursorSeekGE(t *testing.T) {
	tr := newTestTree(t, 128)
	// Insert even-numbered keys.
	for i := 0; i < 100; i += 2 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()

	// SeekGE for exact match.
	k, _ := c.SeekGE(testKey(10))
	if !bytes.Equal(k, testKey(10)) {
		t.Errorf("SeekGE(10) = %q, want %q", k, testKey(10))
	}

	// SeekGE for key between existing keys.
	k, _ = c.SeekGE(testKey(11))
	if !bytes.Equal(k, testKey(12)) {
		t.Errorf("SeekGE(11) = %q, want %q", k, testKey(12))
	}

	// SeekGE past all keys.
	k, _ = c.SeekGE(testKey(9999))
	if k != nil {
		t.Errorf("SeekGE past end should return nil, got %q", k)
	}

	// SeekGE before all keys.
	k, _ = c.SeekGE([]byte("a"))
	if !bytes.Equal(k, testKey(0)) {
		t.Errorf("SeekGE before start = %q, want %q", k, testKey(0))
	}
}

func TestCursorFirstLast(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 50 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	k, _ := c.First()
	if !bytes.Equal(k, testKey(0)) {
		t.Errorf("First = %q, want %q", k, testKey(0))
	}

	k, _ = c.Last()
	if !bytes.Equal(k, testKey(49)) {
		t.Errorf("Last = %q, want %q", k, testKey(49))
	}
}

func TestCursorNextPrevBoundary(t *testing.T) {
	tr := newTestTree(t, 256)
	// Insert enough to span multiple leaves.
	n := 300
	for i := range n {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	// Forward to end.
	k, _ := c.First()
	count := 0
	for k != nil {
		count++
		k, _ = c.Next()
	}
	if count != n {
		t.Errorf("forward count = %d, want %d", count, n)
	}

	// Next after end should be nil.
	k, _ = c.Next()
	if k != nil {
		t.Error("Next past end should return nil")
	}

	// Prev after end should also return nil (cursor invalidated).
	k, _ = c.Prev()
	if k != nil {
		t.Error("Prev on invalidated cursor should return nil")
	}
}

func TestCursorReverseThenForward(t *testing.T) {
	tr := newTestTree(t, 256)
	n := 100
	for i := range n {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	// Go to middle.
	c.SeekGE(testKey(50))
	k, _ := c.Current()
	if !bytes.Equal(k, testKey(50)) {
		t.Fatalf("Current = %q, want %q", k, testKey(50))
	}

	// Go back a few.
	k, _ = c.Prev()
	if !bytes.Equal(k, testKey(49)) {
		t.Errorf("Prev = %q, want %q", k, testKey(49))
	}
	k, _ = c.Prev()
	if !bytes.Equal(k, testKey(48)) {
		t.Errorf("Prev = %q, want %q", k, testKey(48))
	}

	// Go forward.
	k, _ = c.Next()
	if !bytes.Equal(k, testKey(49)) {
		t.Errorf("Next = %q, want %q", k, testKey(49))
	}
}

func TestCursorSingleEntry(t *testing.T) {
	tr := newTestTree(t, 64)
	tr.Put(inlineEntry([]byte("only"), []byte("one")))

	c := tr.NewCursor()
	k, v := c.First()
	if !bytes.Equal(k, []byte("only")) || !bytes.Equal(v, []byte("one")) {
		t.Errorf("First = (%q, %q), want (only, one)", k, v)
	}
	k, _ = c.Next()
	if k != nil {
		t.Error("Next after single entry should return nil")
	}

	k, v = c.Last()
	if !bytes.Equal(k, []byte("only")) || !bytes.Equal(v, []byte("one")) {
		t.Errorf("Last = (%q, %q), want (only, one)", k, v)
	}
	k, _ = c.Prev()
	if k != nil {
		t.Error("Prev before single entry should return nil")
	}
}

func TestCursorCrossLeafBoundary(t *testing.T) {
	tr := newTestTree(t, 512)
	n := 500
	for i := range n {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Forward scan and verify keys are contiguous.
	c := tr.NewCursor()
	prev := ""
	count := 0
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		if prev != "" && string(k) <= prev {
			t.Fatalf("keys not monotonically increasing: %q after %q", k, prev)
		}
		prev = string(k)
		count++
	}
	if count != n {
		t.Errorf("forward count = %d, want %d", count, n)
	}

	// Reverse scan.
	prev = ""
	count = 0
	for k, _ := c.Last(); k != nil; k, _ = c.Prev() {
		if prev != "" && string(k) >= prev {
			t.Fatalf("keys not monotonically decreasing: %q after %q", k, prev)
		}
		prev = string(k)
		count++
	}
	if count != n {
		t.Errorf("reverse count = %d, want %d", count, n)
	}
}

func TestCursorErr(t *testing.T) {
	tr := newTestTree(t, 64)
	c := tr.NewCursor()

	if c.Err() != nil {
		t.Error("new cursor should have nil Err")
	}

	tr.Put(inlineEntry([]byte("a"), []byte("1")))
	c.First()
	if c.Err() != nil {
		t.Error("Err should be nil after successful First")
	}
}

func TestCursorDelete(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 50 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	k, _ := c.Seek(testKey(25))
	if k == nil {
		t.Fatal("Seek(25) returned nil")
	}

	// Delete the current entry.
	err := c.Delete()
	if err != nil {
		t.Fatal(err)
	}

	// Cursor should be positioned at the next key (26).
	k, _ = c.Current()
	if !bytes.Equal(k, testKey(26)) {
		t.Errorf("after Delete, Current = %q, want %q", k, testKey(26))
	}

	// Key 25 should be gone.
	_, found := tr.Get(testKey(25))
	if found {
		t.Error("key 25 should be deleted")
	}
}

func TestCursorDeleteLast(t *testing.T) {
	tr := newTestTree(t, 64)
	tr.Put(inlineEntry([]byte("only"), []byte("one")))

	c := tr.NewCursor()
	c.First()
	err := c.Delete()
	if err != nil {
		t.Fatal(err)
	}

	// Cursor should be unpositioned (was the last key).
	if c.Valid() {
		t.Error("cursor should be invalid after deleting last key")
	}
	if tr.Root() != 0 {
		t.Error("tree should be empty")
	}
}

func TestCursorDeleteForwardScan(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 20 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Delete every other entry via cursor scan.
	c := tr.NewCursor()
	k, _ := c.First()
	i := 0
	for k != nil {
		if i%2 == 0 {
			err := c.Delete()
			if err != nil {
				t.Fatal(err)
			}
			// After Delete, cursor is at the next key.
			k, _ = c.Current()
		} else {
			k, _ = c.Next()
		}
		i++
	}

	// Verify only odd keys remain.
	for i := range 20 {
		_, found := tr.Get(testKey(i))
		if i%2 == 0 {
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

func TestCursorDeleteUnpositioned(t *testing.T) {
	tr := newTestTree(t, 64)
	c := tr.NewCursor()

	err := c.Delete()
	if err == nil {
		t.Error("Delete on unpositioned cursor should return error")
	}
}

func TestCursorDeleteWithPriorError(t *testing.T) {
	tr := newTinyTree(t, 1)
	tr.Put(inlineEntry([]byte("a"), []byte("1")))
	tr.Reset(tr.Root())

	c := tr.NewCursor()
	c.First()
	c.Delete()                 // fails → sets c.err
	err := c.Delete()          // should return c.err immediately
	if err == nil {
		t.Error("Delete with prior error should return error")
	}
}

func TestSeekGECrossLeafBoundary(t *testing.T) {
	tr := newTestTree(t, 256)
	bigVal := bytes.Repeat([]byte("v"), 1400)
	// Create enough entries to span multiple leaves.
	for i := range 20 {
		key := fmt.Appendf(nil, "key:%04d", i)
		tr.Put(page.LeafEntry{Key: key, Value: bigVal})
	}
	c := tr.NewCursor()
	// Seek for a key between the last key of one leaf and the first of the next.
	// With 2 entries per leaf: leaf 0 has keys 0,1; leaf 1 has 2,3; etc.
	// SeekGE for a key that's > last key of leaf 0 but < first key of leaf 1.
	// Actually, with sorted keys, the branch separator guides descent to the
	// correct leaf. But if the key is > all keys in the descended leaf,
	// advanceLeaf is needed.
	// Try seeking just past the last key in the tree.
	k, _ := c.SeekGE([]byte("key:9999"))
	if k != nil {
		t.Errorf("SeekGE past end should return nil, got %q", k)
	}
	// SeekGE for a key between two leaves' ranges.
	// With big values, each leaf has ~2 entries. Keys: 0000,0001 | 0002,0003 | ...
	// The branch separator between leaf 0 and leaf 1 determines descent.
	// If we search for "key:0001z" (between 0001 and 0002), the branch descent
	// goes to the leaf containing keys ≤ "key:0001z". That leaf has 0000,0001.
	// SearchLeaf finds idx=2 (past end). advanceLeaf → next leaf → first entry "key:0002".
	k, _ = c.SeekGE([]byte("key:0001z"))
	if k == nil {
		t.Fatal("SeekGE should find next key")
	}
	if string(k) != "key:0002" {
		t.Errorf("SeekGE = %q, want key:0002", k)
	}
}

func TestCursorGroupCachePrev(t *testing.T) {
	tr := newTestTree(t, 256)
	// Insert more than 16 entries to span restart groups.
	n := 50
	for i := range n {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	// Position at entry 30 and walk backward.
	c.SeekGE(testKey(30))
	var keys []string
	for k, _ := c.Current(); k != nil; k, _ = c.Prev() {
		keys = append(keys, string(k))
	}

	// Should have collected keys 30, 29, 28, ..., 0.
	if len(keys) != 31 {
		t.Fatalf("backward from 30: got %d keys, want 31", len(keys))
	}
	// Verify values are correct (Prev returns value from cache).
	c.SeekGE(testKey(30))
	for k, v := c.Current(); k != nil; k, v = c.Prev() {
		idx := -1
		for i := range n {
			if bytes.Equal(k, testKey(i)) {
				idx = i
				break
			}
		}
		if idx == -1 {
			t.Fatalf("unexpected key %q", k)
		}
		if !bytes.Equal(v, testVal(idx)) {
			t.Errorf("Prev value for key %d = %q, want %q", idx, v, testVal(idx))
		}
	}
}
