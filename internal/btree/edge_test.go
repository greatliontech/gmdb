package btree

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// --- Separator computation ---

func TestComputeSeparatorNoCommonPrefix(t *testing.T) {
	sep := computeSeparator([]byte("aaa"), []byte("zzz"))
	if !bytes.Equal(sep, []byte("z")) {
		t.Errorf("sep = %q, want %q", sep, "z")
	}
	// Verify invariant: left < sep <= right.
	if bytes.Compare([]byte("aaa"), sep) >= 0 {
		t.Error("left >= sep")
	}
	if bytes.Compare(sep, []byte("zzz")) > 0 {
		t.Error("sep > right")
	}
}

func TestComputeSeparatorIdenticalExceptLastByte(t *testing.T) {
	sep := computeSeparator([]byte("abcX"), []byte("abcY"))
	if !bytes.Equal(sep, []byte("abcY")) {
		t.Errorf("sep = %q, want %q", sep, "abcY")
	}
	if bytes.Compare([]byte("abcX"), sep) >= 0 {
		t.Error("left >= sep")
	}
	if bytes.Compare(sep, []byte("abcY")) > 0 {
		t.Error("sep > right")
	}
}

func TestComputeSeparatorPrefixOfRight(t *testing.T) {
	// Left is proper prefix of right.
	sep := computeSeparator([]byte("ab"), []byte("abc"))
	// Common prefix is "ab" (2 bytes). sep = right[0:3] = "abc".
	if !bytes.Equal(sep, []byte("abc")) {
		t.Errorf("sep = %q, want %q", sep, "abc")
	}
	if bytes.Compare([]byte("ab"), sep) >= 0 {
		t.Error("left >= sep")
	}
	if bytes.Compare(sep, []byte("abc")) > 0 {
		t.Error("sep > right")
	}
}

func TestComputeSeparatorSingleByte(t *testing.T) {
	sep := computeSeparator([]byte{0x00}, []byte{0xFF})
	if !bytes.Equal(sep, []byte{0xFF}) {
		t.Errorf("sep = %x, want ff", sep)
	}
}

// --- Prefix-of-prefix keys ---

func TestPrefixKeys(t *testing.T) {
	tr := newTestTree(t, 128)

	keys := []string{"a", "ab", "abc", "abcd", "abcde", "b", "ba"}
	for _, k := range keys {
		tr.Put(inlineEntry([]byte(k), []byte("v:"+k)))
	}

	// Verify all keys retrievable.
	for _, k := range keys {
		entry, found := tr.Get([]byte(k))
		if !found {
			t.Errorf("Get(%q) not found", k)
			continue
		}
		if !bytes.Equal(entry.Value, []byte("v:"+k)) {
			t.Errorf("Get(%q) = %q, want %q", k, entry.Value, "v:"+k)
		}
	}

	// Verify cursor order.
	c := tr.NewCursor()
	var got []string
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		got = append(got, string(k))
	}
	expected := slices.Clone(keys)
	slices.Sort(expected)
	if !slices.Equal(got, expected) {
		t.Errorf("cursor order = %v, want %v", got, expected)
	}

	// Delete prefix keys and verify others survive.
	tr.Delete([]byte("ab"))
	tr.Delete([]byte("abcd"))

	for _, k := range []string{"a", "abc", "abcde", "b", "ba"} {
		_, found := tr.Get([]byte(k))
		if !found {
			t.Errorf("Get(%q) not found after prefix delete", k)
		}
	}
	for _, k := range []string{"ab", "abcd"} {
		_, found := tr.Get([]byte(k))
		if found {
			t.Errorf("Get(%q) should be deleted", k)
		}
	}
}

// --- Empty values ---

func TestEmptyValues(t *testing.T) {
	tr := newTestTree(t, 128)

	// Put with empty value.
	tr.Put(inlineEntry([]byte("a"), []byte{}))
	tr.Put(inlineEntry([]byte("b"), nil))
	tr.Put(inlineEntry([]byte("c"), []byte("notempty")))

	entry, found := tr.Get([]byte("a"))
	if !found {
		t.Fatal("Get(a) not found")
	}
	if len(entry.Value) != 0 {
		t.Errorf("Get(a) value len = %d, want 0", len(entry.Value))
	}

	entry, found = tr.Get([]byte("b"))
	if !found {
		t.Fatal("Get(b) not found")
	}
	if len(entry.Value) != 0 {
		t.Errorf("Get(b) value len = %d, want 0", len(entry.Value))
	}

	// Delete empty value entry.
	old, found, _ := tr.Delete([]byte("a"))
	if !found {
		t.Fatal("Delete(a) not found")
	}
	if len(old.Value) != 0 {
		t.Error("deleted value should be empty")
	}
}

// --- Full cursor scan after structural changes ---

func TestFullScanAfterSplits(t *testing.T) {
	tr := newTestTree(t, 512)
	n := 500
	for i := range n {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Full forward scan: verify count and order.
	c := tr.NewCursor()
	var keys []string
	for k, v := c.First(); k != nil; k, v = c.Next() {
		keys = append(keys, string(k))
		// Verify value matches.
		idx := -1
		for j := range n {
			if bytes.Equal([]byte(keys[len(keys)-1]), testKey(j)) {
				idx = j
				break
			}
		}
		if idx >= 0 && !bytes.Equal(v, testVal(idx)) {
			t.Errorf("value mismatch at key %q", k)
		}
	}
	if len(keys) != n {
		t.Fatalf("scan count = %d, want %d", len(keys), n)
	}
	if !slices.IsSorted(keys) {
		t.Fatal("keys not sorted after splits")
	}
}

func TestFullScanAfterDeletesAndMerges(t *testing.T) {
	tr := newTestTree(t, 512)
	n := 500
	for i := range n {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Delete half.
	alive := make(map[string]string)
	for i := range n {
		if i%2 == 0 {
			tr.Delete(testKey(i))
		} else {
			alive[string(testKey(i))] = string(testVal(i))
		}
	}

	// Full scan: verify exactly the alive keys remain in order.
	c := tr.NewCursor()
	var keys []string
	for k, v := c.First(); k != nil; k, v = c.Next() {
		sk := string(k)
		keys = append(keys, sk)
		if expected, ok := alive[sk]; ok {
			if string(v) != expected {
				t.Errorf("value mismatch for %q", sk)
			}
		} else {
			t.Errorf("cursor returned deleted key %q", sk)
		}
	}
	if len(keys) != len(alive) {
		t.Errorf("scan count = %d, want %d", len(keys), len(alive))
	}
	if !slices.IsSorted(keys) {
		t.Fatal("keys not sorted after merges")
	}
}

// --- Cursor at leaf/group boundaries ---

func TestCursorPrevNextAtLeafBoundary(t *testing.T) {
	tr := newTestTree(t, 256)
	// Large values to reduce entries per leaf, making boundaries predictable.
	bigVal := bytes.Repeat([]byte("v"), 500)
	n := 50
	for i := range n {
		key := fmt.Appendf(nil, "key:%04d", i)
		tr.Put(page.LeafEntry{Key: key, Value: bigVal})
	}

	c := tr.NewCursor()
	// Walk forward, then backward, then forward, repeating across boundaries.
	c.First()
	for range 10 {
		c.Next()
	}
	k10, _ := c.Current()
	k10 = bytes.Clone(k10) // keys are borrowed from cursor, clone to keep

	// Back 5.
	for range 5 {
		c.Prev()
	}
	k5, _ := c.Current()
	k5 = bytes.Clone(k5)

	// Forward 5 — should return to same position.
	for range 5 {
		c.Next()
	}
	kAgain, _ := c.Current()

	if !bytes.Equal(k10, kAgain) {
		t.Errorf("after Prev×5 + Next×5: got %q, want %q", kAgain, k10)
	}
	if bytes.Compare(k5, k10) >= 0 {
		t.Errorf("k5=%q should be < k10=%q", k5, k10)
	}
}

func TestCursorGroupBoundary(t *testing.T) {
	tr := newTestTree(t, 256)
	// 50 entries with short values — all fit in one leaf with multiple restart groups.
	for i := range 50 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	// Position at entry 17 (second group, index 1 within group).
	var allKeys []string
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		allKeys = append(allKeys, string(k))
	}

	// SeekGE to entry at index 16 (first entry of second restart group).
	c.SeekGE([]byte(allKeys[16]))
	k, _ := c.Current()
	if string(k) != allKeys[16] {
		t.Errorf("at group boundary: got %q, want %q", k, allKeys[16])
	}

	// Prev to index 15 (last entry of first group).
	k, _ = c.Prev()
	if string(k) != allKeys[15] {
		t.Errorf("Prev from group boundary: got %q, want %q", k, allKeys[15])
	}

	// Next back to 16.
	k, _ = c.Next()
	if string(k) != allKeys[16] {
		t.Errorf("Next after Prev at boundary: got %q, want %q", k, allKeys[16])
	}
}

// --- Root shrink depth 3 → depth 1 ---

func TestRootShrinkDeepToShallow(t *testing.T) {
	tr := newTestTree(t, 4096)
	bigVal := bytes.Repeat([]byte("x"), 500)

	// Insert enough to create depth 3.
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
		t.Error("tree should be empty after deleting all")
	}
}

// --- Mixed cell types ---

func TestMixedCellTypes(t *testing.T) {
	tr := newTestTree(t, 128)

	// Insert different cell types in the same leaf.
	tr.Put(inlineEntry([]byte("aaa"), []byte("inline")))
	tr.Put(page.LeafEntry{Key: []byte("bbb"), CellFlags: page.CellFlagOverflow, OvflPage: 42, TotalLen: 99999})

	subpage := make([]byte, 4+2+3)
	binary.LittleEndian.PutUint16(subpage[0:], 1)
	binary.LittleEndian.PutUint16(subpage[2:], 5)
	binary.LittleEndian.PutUint16(subpage[4:], 3)
	copy(subpage[6:], "xyz")
	tr.Put(page.LeafEntry{Key: []byte("ccc"), CellFlags: page.CellFlagMultiValue, SubpageData: subpage})

	tr.Put(page.LeafEntry{Key: []byte("ddd"), CellFlags: page.CellFlagMultiValue | page.CellFlagNestedTree, NestedRoot: 77, NestedCount: 10})

	// Verify all entries.
	e, _ := tr.Get([]byte("aaa"))
	if e.CellFlags != 0 || !bytes.Equal(e.Value, []byte("inline")) {
		t.Error("aaa mismatch")
	}
	e, _ = tr.Get([]byte("bbb"))
	if e.CellFlags&page.CellFlagOverflow == 0 || e.OvflPage != 42 {
		t.Error("bbb mismatch")
	}
	e, _ = tr.Get([]byte("ccc"))
	if e.CellFlags&page.CellFlagMultiValue == 0 || !bytes.Equal(e.SubpageData, subpage) {
		t.Error("ccc mismatch")
	}
	e, _ = tr.Get([]byte("ddd"))
	if e.NestedRoot != 77 || e.NestedCount != 10 {
		t.Error("ddd mismatch")
	}

	// Delete overflow entry and verify others survive.
	old, found, _ := tr.Delete([]byte("bbb"))
	if !found || old.OvflPage != 42 {
		t.Error("delete bbb mismatch")
	}

	// Replace inline with subpage.
	tr.Put(page.LeafEntry{Key: []byte("aaa"), CellFlags: page.CellFlagMultiValue, SubpageData: subpage})
	e, _ = tr.Get([]byte("aaa"))
	if e.CellFlags&page.CellFlagMultiValue == 0 {
		t.Error("aaa should be multi-value after replace")
	}
}

// --- Delete non-existent in multi-leaf tree ---

func TestDeleteNonExistentMultiLeaf(t *testing.T) {
	tr := newTestTree(t, 256)
	for i := 0; i < 100; i += 2 { // insert even keys only
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Delete odd keys (non-existent, between existing keys).
	for i := 1; i < 100; i += 2 {
		_, found, err := tr.Delete(testKey(i))
		if err != nil {
			t.Fatalf("Delete(%d): %v", i, err)
		}
		if found {
			t.Errorf("Delete(%d) should not find odd key", i)
		}
	}

	// Verify even keys still exist.
	for i := 0; i < 100; i += 2 {
		_, found := tr.Get(testKey(i))
		if !found {
			t.Errorf("Get(%d) not found after non-existent deletes", i)
		}
	}
}

// --- Double CoW prevention ---

func TestDoubleCowSameTransaction(t *testing.T) {
	tr := newTestTree(t, 128)

	// Insert entries.
	for i := range 10 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Multiple modifications to same leaf in same tx.
	cowBefore := len(tr.CowPages())
	tr.Put(inlineEntry(testKey(0), []byte("updated1")))
	cowAfter1 := len(tr.CowPages())
	tr.Put(inlineEntry(testKey(0), []byte("updated2")))
	cowAfter2 := len(tr.CowPages())

	// Second Put should not allocate additional CoW pages.
	if cowAfter2 != cowAfter1 {
		t.Errorf("CoW pages grew from %d to %d on second Put to same leaf", cowAfter1, cowAfter2)
	}
	_ = cowBefore

	entry, _ := tr.Get(testKey(0))
	if !bytes.Equal(entry.Value, []byte("updated2")) {
		t.Errorf("value = %q, want updated2", entry.Value)
	}
}

// --- Cursor Delete then Prev ---

func TestCursorDeleteThenPrev(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 20 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	c.SeekGE(testKey(10))

	// Delete current (key 10), cursor advances to key 11.
	c.Delete()
	k, _ := c.Current()
	if k == nil {
		t.Fatal("cursor should be positioned after delete")
	}

	// Note: after Delete, the cursor's stack is rebuilt via SeekGE.
	// Prev should work and return key 9 (key 10 was deleted).
	k, _ = c.Prev()
	if k == nil {
		t.Fatal("Prev after Delete should return key 9")
	}
	if !bytes.Equal(k, testKey(9)) {
		t.Errorf("Prev after Delete = %q, want %q", k, testKey(9))
	}
}

// --- Retired pages correctness ---

func TestRetiredNoDuplicates(t *testing.T) {
	tr := newTestTree(t, 256)
	for i := range 50 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}
	root := tr.Root()
	tr.Reset(root)

	// Modifications generate retired pages.
	for i := range 10 {
		tr.Put(inlineEntry(testKey(i), []byte("updated")))
	}

	retired := tr.Retired()
	seen := make(map[uint64]bool)
	for _, pid := range retired {
		if seen[pid] {
			t.Errorf("duplicate retired page: %d", pid)
		}
		seen[pid] = true
	}
}

// --- Stale cursor after external modification ---

func TestCursorAfterExternalModification(t *testing.T) {
	tr := newTestTree(t, 256)
	for i := range 50 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	c.First()

	// Modify tree externally (not via cursor).
	tr.Put(inlineEntry([]byte("zzz"), []byte("new")))
	tr.Delete(testKey(25))

	// Cursor may be stale but should not panic. The exact behavior
	// (returning old data, returning updated data, or returning nil)
	// depends on whether the cursor's leaf was CoW'd. The key invariant:
	// no crash or data corruption.
	count := 0
	for k, _ := c.Current(); k != nil; k, _ = c.Next() {
		count++
		if count > 100 {
			break // safety
		}
	}
	// Just verify it didn't panic or loop forever.
}

// --- Seek between existing keys ---

func TestSeekBetweenKeys(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := 0; i < 50; i += 2 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()

	// Seek exact → found.
	k, _ := c.Seek(testKey(10))
	if !bytes.Equal(k, testKey(10)) {
		t.Errorf("Seek(10) = %q, want %q", k, testKey(10))
	}

	// Seek between keys → not found.
	k, _ = c.Seek(testKey(11))
	if k != nil {
		t.Errorf("Seek(11) should return nil (odd keys don't exist), got %q", k)
	}

	// SeekGE between keys → next key.
	k, _ = c.SeekGE(testKey(11))
	if !bytes.Equal(k, testKey(12)) {
		t.Errorf("SeekGE(11) = %q, want %q", k, testKey(12))
	}
}

// --- SeekGE to first and last key ---

func TestSeekGEFirstAndLast(t *testing.T) {
	tr := newTestTree(t, 256)
	n := 100
	for i := range n {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()

	// SeekGE to first key.
	k, _ := c.SeekGE(testKey(0))
	if !bytes.Equal(k, testKey(0)) {
		t.Errorf("SeekGE(first) = %q, want %q", k, testKey(0))
	}

	// SeekGE to last key.
	k, _ = c.SeekGE(testKey(n - 1))
	if !bytes.Equal(k, testKey(n-1)) {
		t.Errorf("SeekGE(last) = %q, want %q", k, testKey(n-1))
	}

	// SeekGE before first.
	k, _ = c.SeekGE([]byte("a"))
	if !bytes.Equal(k, testKey(0)) {
		t.Errorf("SeekGE(before first) = %q, want %q", k, testKey(0))
	}

	// SeekGE after last.
	k, _ = c.SeekGE([]byte("zzz"))
	if k != nil {
		t.Error("SeekGE(after last) should return nil")
	}
}

// --- Cell format cycling ---

func TestCellFormatCycle(t *testing.T) {
	tr := newTestTree(t, 64)

	// inline → overflow → subpage → nested → inline
	tr.Put(inlineEntry([]byte("key"), []byte("inline1")))
	e, _ := tr.Get([]byte("key"))
	if e.CellFlags != 0 {
		t.Error("should be inline")
	}

	tr.Put(page.LeafEntry{Key: []byte("key"), CellFlags: page.CellFlagOverflow, OvflPage: 10, TotalLen: 5000})
	e, _ = tr.Get([]byte("key"))
	if e.CellFlags&page.CellFlagOverflow == 0 {
		t.Error("should be overflow")
	}

	subpage := make([]byte, 4+2+3)
	binary.LittleEndian.PutUint16(subpage[0:], 1)
	binary.LittleEndian.PutUint16(subpage[2:], 5)
	binary.LittleEndian.PutUint16(subpage[4:], 3)
	copy(subpage[6:], "xyz")
	tr.Put(page.LeafEntry{Key: []byte("key"), CellFlags: page.CellFlagMultiValue, SubpageData: subpage})
	e, _ = tr.Get([]byte("key"))
	if e.CellFlags&page.CellFlagMultiValue == 0 || e.CellFlags&page.CellFlagNestedTree != 0 {
		t.Error("should be subpage")
	}

	tr.Put(page.LeafEntry{Key: []byte("key"), CellFlags: page.CellFlagMultiValue | page.CellFlagNestedTree, NestedRoot: 99, NestedCount: 50})
	e, _ = tr.Get([]byte("key"))
	if e.CellFlags&page.CellFlagNestedTree == 0 {
		t.Error("should be nested tree")
	}

	tr.Put(inlineEntry([]byte("key"), []byte("inline2")))
	e, _ = tr.Get([]byte("key"))
	if e.CellFlags != 0 || !bytes.Equal(e.Value, []byte("inline2")) {
		t.Error("should be inline again")
	}
}

// --- Cursor staleness detection ---

func TestCursorStaleAfterPut(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 10 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	c.First()
	if !c.Valid() {
		t.Fatal("cursor should be valid")
	}

	// Mutate tree externally.
	tr.Put(inlineEntry([]byte("zzz"), []byte("new")))

	// Next should detect staleness.
	k, _ := c.Next()
	if k != nil {
		t.Error("Next after mutation should return nil")
	}
	if c.Err() != ErrCursorStale {
		t.Errorf("Err = %v, want ErrCursorStale", c.Err())
	}

	// Re-positioning clears the error.
	k, _ = c.First()
	if k == nil {
		t.Error("First after re-positioning should work")
	}
	if c.Err() != nil {
		t.Error("Err should be nil after re-positioning")
	}
}

func TestCursorStaleAfterDelete(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 10 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	c.Last()

	tr.Delete(testKey(5))

	k, _ := c.Prev()
	if k != nil {
		t.Error("Prev after mutation should return nil")
	}
	if c.Err() != ErrCursorStale {
		t.Errorf("Err = %v, want ErrCursorStale", c.Err())
	}
}

func TestCursorStaleAfterDeleteRange(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 20 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	c.SeekGE(testKey(10))

	tr.DeleteRange(testKey(0), testKey(5))

	k, _ := c.Current()
	if k != nil {
		t.Error("Current after mutation should return nil")
	}
	if c.Err() != ErrCursorStale {
		t.Errorf("Err = %v, want ErrCursorStale", c.Err())
	}
}

func TestCursorDeleteDoesNotStale(t *testing.T) {
	// Cursor.Delete() mutates the tree but re-positions the cursor.
	// The cursor should NOT be stale after its own Delete.
	tr := newTestTree(t, 128)
	for i := range 10 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	c.SeekGE(testKey(5))
	err := c.Delete()
	if err != nil {
		t.Fatal(err)
	}

	// Cursor should be valid and positioned at successor.
	if !c.Valid() {
		t.Error("cursor should be valid after its own Delete")
	}
	k, _ := c.Next()
	if k == nil {
		t.Error("Next after cursor Delete should work")
	}
}

// --- Multiple Reset cycles ---

func TestMultipleResetCycles(t *testing.T) {
	tr := newTestTree(t, 256)

	// Tx 1: insert.
	for i := range 20 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}
	root1 := tr.Root()

	// Tx 2: modify.
	tr.Reset(root1)
	tr.Put(inlineEntry(testKey(0), []byte("tx2")))
	tr.Delete(testKey(19))
	root2 := tr.Root()

	// Tx 3: more modifications.
	tr.Reset(root2)
	tr.Put(inlineEntry(testKey(100), testVal(100)))
	root3 := tr.Root()

	// Verify tx 3 state.
	tr.Reset(root3)
	e, found := tr.Get(testKey(0))
	if !found || !bytes.Equal(e.Value, []byte("tx2")) {
		t.Errorf("tx3: key 0 = %q, want tx2", e.Value)
	}
	_, found = tr.Get(testKey(19))
	if found {
		t.Error("tx3: key 19 should be deleted")
	}
	_, found = tr.Get(testKey(100))
	if !found {
		t.Error("tx3: key 100 should exist")
	}
}
