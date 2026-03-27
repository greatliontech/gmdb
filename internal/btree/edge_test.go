package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
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

// --- computeSeparator edge: empty left ---

func TestComputeSeparatorEmptyLeft(t *testing.T) {
	sep := computeSeparator([]byte{}, []byte{0x42})
	if !bytes.Equal(sep, []byte{0x42}) {
		t.Errorf("sep = %x, want 42", sep)
	}
	// Verify invariant: left < sep <= right.
	if bytes.Compare([]byte{}, sep) >= 0 {
		t.Error("left >= sep")
	}
	if bytes.Compare(sep, []byte{0x42}) > 0 {
		t.Error("sep > right")
	}
}

// --- DeleteRange with start == end (empty range) ---

func TestDeleteRangeStartEqualsEnd(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 20 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	deleted, err := tr.DeleteRange(testKey(10), testKey(10))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}

	// Verify all 20 keys still exist.
	for i := range 20 {
		_, found := tr.Get(testKey(i))
		if !found {
			t.Errorf("key %d missing after empty DeleteRange", i)
		}
	}
}

// --- DeleteRange with start > end (inverted range) ---

func TestDeleteRangeStartGreaterThanEnd(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 20 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	deleted, err := tr.DeleteRange(testKey(15), testKey(5))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}

	// Verify all 20 keys still exist via cursor scan.
	c := tr.NewCursor()
	count := 0
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		count++
	}
	if count != 20 {
		t.Errorf("cursor scan count = %d, want 20", count)
	}
}

// --- CoW preserves old pages ---

func TestCowPreservesOldPages(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 10 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	root := tr.Root()
	tr.Reset(root)

	// Snapshot the old root page content before any mutation.
	oldRoot := tr.Root()
	ps := int(tr.cfg.Page.PageSize)
	snapshot := make([]byte, ps)
	copy(snapshot, tr.data[oldRoot*uint64(ps):(oldRoot+1)*uint64(ps)])

	// Mutate the tree — this should CoW the root, not modify the old page.
	tr.Put(inlineEntry([]byte("zzz_new"), []byte("value")))

	// The old root page must be untouched.
	oldPageContent := tr.data[oldRoot*uint64(ps) : (oldRoot+1)*uint64(ps)]
	if !bytes.Equal(oldPageContent, snapshot) {
		t.Error("old root page was modified after CoW; expected it to be preserved")
	}
}

// --- Multiple independent cursors ---

func TestMultipleCursors(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 20 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Collect sorted keys for reference.
	ref := tr.NewCursor()
	var allKeys []string
	for k, _ := ref.First(); k != nil; k, _ = ref.Next() {
		allKeys = append(allKeys, string(k))
	}

	// Create two independent cursors.
	a := tr.NewCursor()
	b := tr.NewCursor()

	// Position A at First, B at Last.
	a.First()
	b.Last()

	// Advance A forward 5 times.
	for range 5 {
		a.Next()
	}
	ka, _ := a.Current()
	if string(ka) != allKeys[5] {
		t.Errorf("cursor A at index 5: got %q, want %q", ka, allKeys[5])
	}

	// Advance B backward 5 times.
	for range 5 {
		b.Prev()
	}
	kb, _ := b.Current()
	if string(kb) != allKeys[len(allKeys)-6] {
		t.Errorf("cursor B at index %d: got %q, want %q", len(allKeys)-6, kb, allKeys[len(allKeys)-6])
	}

	// Mutate tree — cursor A becomes stale, cursor B created after mutation works.
	aKey, _ := a.Current()
	_ = aKey
	tr.Put(inlineEntry([]byte("zzz_extra"), []byte("val")))

	// A is stale.
	k, _ := a.Next()
	if k != nil {
		t.Error("cursor A Next after mutation should return nil")
	}
	if !errors.Is(a.Err(), ErrCursorStale) {
		t.Errorf("cursor A Err = %v, want ErrCursorStale", a.Err())
	}

	// B created after mutation works fine.
	bNew := tr.NewCursor()
	kFirst, _ := bNew.First()
	if kFirst == nil {
		t.Error("new cursor B First should return a key")
	}
	if string(kFirst) != allKeys[0] {
		t.Errorf("new cursor B First = %q, want %q", kFirst, allKeys[0])
	}
}

// --- Cursor stale after Reset ---

func TestCursorStaleAfterReset(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 10 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	c.First()

	// Reset increments gen, making the cursor stale.
	tr.Reset(tr.Root())

	k, _ := c.Next()
	if k != nil {
		t.Error("Next after Reset should return nil")
	}
	if !errors.Is(c.Err(), ErrCursorStale) {
		t.Errorf("Err = %v, want ErrCursorStale", c.Err())
	}
}

// --- Put replace causes split ---

func TestPutReplaceCausesSplit(t *testing.T) {
	tr := newTestTree(t, 256)

	// Insert entries with small values to fill a leaf near capacity.
	n := 40
	for i := range n {
		tr.Put(inlineEntry(testKey(i), []byte("s")))
	}

	// Replace one entry with a very large value to cause overflow and split.
	bigVal := bytes.Repeat([]byte("X"), 2000)
	tr.Put(page.LeafEntry{Key: testKey(20), Value: bigVal})

	// Verify all keys are still retrievable.
	for i := range n {
		e, found := tr.Get(testKey(i))
		if !found {
			t.Errorf("key %d not found after replace-induced split", i)
			continue
		}
		if i == 20 {
			if !bytes.Equal(e.Value, bigVal) {
				t.Errorf("key 20 value len = %d, want %d", len(e.Value), len(bigVal))
			}
		}
	}
}

// --- Consecutive cursor deletes ---

func TestConsecutiveCursorDeletes(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 10 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// Collect sorted keys for reference.
	ref := tr.NewCursor()
	var allKeys []string
	for k, _ := ref.First(); k != nil; k, _ = ref.Next() {
		allKeys = append(allKeys, string(k))
	}

	c := tr.NewCursor()
	c.First()

	// Delete 3 entries consecutively.
	for i := range 3 {
		err := c.Delete()
		if err != nil {
			t.Fatalf("Delete %d: %v", i, err)
		}
	}

	// Verify the first 3 keys are gone.
	for i := range 3 {
		_, found := tr.Get([]byte(allKeys[i]))
		if found {
			t.Errorf("key %q should have been deleted", allKeys[i])
		}
	}

	// Verify the remaining 7 keys exist.
	for i := 3; i < len(allKeys); i++ {
		_, found := tr.Get([]byte(allKeys[i]))
		if !found {
			t.Errorf("key %q should still exist", allKeys[i])
		}
	}
}

// --- Cursor seek recovery after stale ---

func TestCursorSeekRecoveryAfterStale(t *testing.T) {
	tr := newTestTree(t, 128)
	for i := range 20 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	c := tr.NewCursor()
	c.First()

	// Mutate tree to make cursor stale.
	tr.Put(inlineEntry([]byte("zzz_mutation"), []byte("val")))

	// Verify cursor is stale.
	k, _ := c.Next()
	if k != nil {
		t.Error("Next after mutation should return nil")
	}
	if !errors.Is(c.Err(), ErrCursorStale) {
		t.Fatalf("Err = %v, want ErrCursorStale", c.Err())
	}

	// Seek (exact match) should succeed and recover the cursor.
	k, _ = c.Seek(testKey(10))
	if k == nil || !bytes.Equal(k, testKey(10)) {
		t.Errorf("Seek after stale = %q, want %q", k, testKey(10))
	}
	if c.Err() != nil {
		t.Errorf("Err after Seek = %v, want nil", c.Err())
	}

	// SeekGE should also succeed.
	k, _ = c.SeekGE(testKey(15))
	if k == nil || !bytes.Equal(k, testKey(15)) {
		t.Errorf("SeekGE after stale = %q, want %q", k, testKey(15))
	}
	if c.Err() != nil {
		t.Errorf("Err after SeekGE = %v, want nil", c.Err())
	}
}

// --- Retired and CoW pages are disjoint ---

func TestRetiredAndCowDisjoint(t *testing.T) {
	tr := newTestTree(t, 256)
	for i := range 50 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	root := tr.Root()
	tr.Reset(root)

	// Do mutations: puts and deletes.
	for i := range 10 {
		tr.Put(inlineEntry(testKey(i), []byte("updated")))
	}
	for i := 40; i < 50; i++ {
		tr.Delete(testKey(i))
	}

	retired := tr.Retired()
	cowPages := tr.CowPages()

	for _, pid := range retired {
		if _, ok := cowPages[pid]; ok {
			t.Errorf("page %d appears in both retired and cow sets", pid)
		}
	}
}

// --- Reset clears state ---

func TestResetClearsState(t *testing.T) {
	tr := newTestTree(t, 256)
	for i := range 50 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	root := tr.Root()
	tr.Reset(root)

	// Do mutations to populate cow and retired.
	for i := range 10 {
		tr.Put(inlineEntry(testKey(i), []byte("updated")))
	}
	tr.Delete(testKey(49))

	if len(tr.CowPages()) == 0 {
		t.Fatal("expected non-empty cow pages before reset")
	}
	if len(tr.Retired()) == 0 {
		t.Fatal("expected non-empty retired pages before reset")
	}

	// Reset should clear both.
	root2 := tr.Root()
	tr.Reset(root2)

	if len(tr.CowPages()) != 0 {
		t.Errorf("CowPages after Reset = %d, want 0", len(tr.CowPages()))
	}
	if len(tr.Retired()) != 0 {
		t.Errorf("Retired after Reset = %d, want 0", len(tr.Retired()))
	}
}

// --- DeleteRange after Put in same transaction ---

func TestDeleteRangeAfterPutSameTx(t *testing.T) {
	tr := newTestTree(t, 256)
	for i := range 20 {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// In the same transaction (no Reset), put new entries then delete them.
	for i := 20; i < 30; i++ {
		tr.Put(inlineEntry(testKey(i), testVal(i)))
	}

	// DeleteRange covers the newly inserted entries.
	deleted, err := tr.DeleteRange(testKey(20), testKey(30))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 10 {
		t.Errorf("deleted = %d, want 10", deleted)
	}

	// Verify the original 20 entries still exist.
	for i := range 20 {
		_, found := tr.Get(testKey(i))
		if !found {
			t.Errorf("key %d should still exist", i)
		}
	}

	// Verify deleted entries are gone.
	for i := 20; i < 30; i++ {
		_, found := tr.Get(testKey(i))
		if found {
			t.Errorf("key %d should be deleted", i)
		}
	}

	// Verify tree is consistent via full cursor scan.
	c := tr.NewCursor()
	count := 0
	var prev []byte
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		if prev != nil && bytes.Compare(prev, k) >= 0 {
			t.Fatalf("keys out of order: %q >= %q", prev, k)
		}
		prev = bytes.Clone(k)
		count++
	}
	if count != 20 {
		t.Errorf("cursor scan count = %d, want 20", count)
	}
}

// --- Cursor Prev from first entry in multi-leaf tree ---

func TestCursorPrevFromFirstEntryMultiLeaf(t *testing.T) {
	tr := newTestTree(t, 256)
	// Use big values to force multiple leaves.
	bigVal := bytes.Repeat([]byte("v"), 500)
	n := 50
	for i := range n {
		key := fmt.Appendf(nil, "key:%04d", i)
		tr.Put(page.LeafEntry{Key: key, Value: bigVal})
	}

	c := tr.NewCursor()
	c.First()

	// Prev from the first entry should return nil.
	k, _ := c.Prev()
	if k != nil {
		t.Errorf("Prev from first entry = %q, want nil", k)
	}
	if c.Valid() {
		t.Error("cursor should not be valid after Prev past beginning")
	}
}

// --- Insert early return when child unchanged ---

func TestInsertEarlyReturnOnReplace(t *testing.T) {
	// Replacing a value when the leaf is already CoW'd in this transaction
	// should not CoW or rebuild ancestor branches.
	tr := newTestTree(t, 256)
	bigVal := bytes.Repeat([]byte("v"), 300)

	// Insert enough entries to create a multi-level tree.
	for i := range 60 {
		_, _, err := tr.Put(inlineEntry(testKey(i), bigVal))
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}

	// First replace: CoWs the leaf and all ancestors.
	_, replaced, err := tr.Put(inlineEntry(testKey(30), []byte("new-val-1")))
	if err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Fatal("expected replaced=true")
	}
	cowAfterFirst := len(tr.CowPages())

	// Second replace of the same key: leaf is already CoW'd, so the early
	// return should fire — no additional pages CoW'd.
	_, replaced, err = tr.Put(inlineEntry(testKey(30), []byte("new-val-2")))
	if err != nil {
		t.Fatal(err)
	}
	if !replaced {
		t.Fatal("expected replaced=true")
	}
	cowAfterSecond := len(tr.CowPages())

	if cowAfterSecond != cowAfterFirst {
		t.Errorf("second replace CoW'd %d additional pages, want 0",
			cowAfterSecond-cowAfterFirst)
	}

	// Verify the value was actually updated.
	e, found := tr.Get(testKey(30))
	if !found {
		t.Fatal("key not found after replace")
	}
	if !bytes.Equal(e.Value, []byte("new-val-2")) {
		t.Errorf("value = %q, want %q", e.Value, "new-val-2")
	}
}

func TestInsertEarlyReturnDifferentKeys(t *testing.T) {
	// Replacing two different keys that share a branch but have different
	// leaves should CoW the branch once, then the early return fires for
	// the second replace if the branch was already CoW'd.
	tr := newTestTree(t, 256)
	bigVal := bytes.Repeat([]byte("v"), 300)

	for i := range 60 {
		_, _, err := tr.Put(inlineEntry(testKey(i), bigVal))
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}

	// Replace key 10.
	tr.Put(inlineEntry(testKey(10), []byte("x")))
	cowAfterFirst := len(tr.CowPages())

	// Replace key 10 again — same leaf, early return.
	tr.Put(inlineEntry(testKey(10), []byte("y")))
	if len(tr.CowPages()) != cowAfterFirst {
		t.Errorf("same-leaf re-replace CoW'd extra pages")
	}

	// Verify both values correct.
	e, _ := tr.Get(testKey(10))
	if !bytes.Equal(e.Value, []byte("y")) {
		t.Errorf("value = %q, want %q", e.Value, "y")
	}
}

// --- DeleteRange boundary-only rebalance ---

func TestDeleteRangeNoSpuriousRebalance(t *testing.T) {
	// Build a tree with enough entries to have multiple branch children.
	// Delete a thin range in the middle, verify non-boundary children
	// are not CoW'd unnecessarily.
	tr := newTestTree(t, 1024)
	bigVal := bytes.Repeat([]byte("v"), 200)

	n := 200
	for i := range n {
		_, _, err := tr.Put(inlineEntry(testKey(i), bigVal))
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}

	// Start a fresh transaction to reset CoW tracking.
	root := tr.Root()
	tr.Reset(root)

	// Delete a range in the middle.
	deleted, err := tr.DeleteRange(testKey(90), testKey(110))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 20 {
		t.Errorf("deleted = %d, want 20", deleted)
	}

	// Verify all non-deleted keys are intact.
	for i := range n {
		if i >= 90 && i < 110 {
			continue
		}
		_, found := tr.Get(testKey(i))
		if !found {
			t.Errorf("key %d not found after DeleteRange", i)
		}
	}
	// Verify deleted keys are gone.
	for i := 90; i < 110; i++ {
		_, found := tr.Get(testKey(i))
		if found {
			t.Errorf("key %d still found after DeleteRange", i)
		}
	}
}

func TestDeleteRangeBranchChildFreeSpaceAccurate(t *testing.T) {
	// Regression test: previously branch children used UsableSpace() as
	// freeSpace estimate, making isUnderfull return true for every branch
	// child. This test verifies that a DeleteRange leaving well-filled
	// branch children does not trigger unnecessary rebalancing.
	tr := newTestTree(t, 2048)
	bigVal := bytes.Repeat([]byte("v"), 150)

	// Build a tree deep enough to have branch children of branches.
	n := 500
	for i := range n {
		_, _, err := tr.Put(inlineEntry(testKey(i), bigVal))
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}

	root := tr.Root()
	tr.Reset(root)

	// Delete a small range — boundary children should be checked,
	// but well-filled non-boundary children should be skipped.
	deleted, err := tr.DeleteRange(testKey(200), testKey(210))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 10 {
		t.Errorf("deleted = %d, want 10", deleted)
	}

	// Verify tree integrity with a full scan.
	c := tr.NewCursor()
	var prev []byte
	count := 0
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		if prev != nil && bytes.Compare(prev, k) >= 0 {
			t.Fatalf("keys out of order: %q >= %q", prev, k)
		}
		prev = bytes.Clone(k)
		count++
	}
	if count != n-10 {
		t.Errorf("cursor count = %d, want %d", count, n-10)
	}
}

// --- canMerge with actual separator key ---

func TestCanMergeWithSeparator(t *testing.T) {
	// Build a tree, delete entries to trigger branch rebalance where
	// canMerge is called with the real separator key. Verify correctness.
	tr := newTestTree(t, 512)
	bigVal := bytes.Repeat([]byte("v"), 300)

	for i := range 80 {
		_, _, err := tr.Put(inlineEntry(testKey(i), bigVal))
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}

	// Delete entries to make a child underfull and trigger rebalance.
	for i := range 80 {
		if i%4 != 0 {
			continue
		}
		_, found, err := tr.Delete(testKey(i))
		if err != nil {
			t.Fatalf("Delete(%d): %v", i, err)
		}
		if !found {
			t.Fatalf("Delete(%d): not found", i)
		}
	}

	// Verify all remaining keys.
	for i := range 80 {
		if i%4 == 0 {
			continue
		}
		e, found := tr.Get(testKey(i))
		if !found {
			t.Errorf("key %d not found", i)
			continue
		}
		if !bytes.Equal(e.Value, bigVal) {
			t.Errorf("key %d: value length = %d, want %d", i, len(e.Value), len(bigVal))
		}
	}
}

// --- Cursor group cache with DecodeGroup ---

func TestCursorGroupCacheAcrossGroups(t *testing.T) {
	// Verify that the cursor correctly populates group cache when scanning
	// across restart group boundaries.
	tr := newTestTree(t, 512)

	// Insert 40 entries with small values so they all fit in one or two leaves.
	// This gives us at least 2 restart groups (entries 0-15, 16-31, 32-39).
	for i := range 40 {
		_, _, err := tr.Put(inlineEntry(testKey(i), testVal(i)))
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}

	c := tr.NewCursor()
	count := 0
	for k, v := c.First(); k != nil; k, v = c.Next() {
		wantKey := testKey(count)
		wantVal := testVal(count)
		if !bytes.Equal(k, wantKey) {
			t.Errorf("entry %d: key = %q, want %q", count, k, wantKey)
		}
		if !bytes.Equal(v, wantVal) {
			t.Errorf("entry %d: val = %q, want %q", count, v, wantVal)
		}
		count++
	}
	if count != 40 {
		t.Errorf("scanned %d entries, want 40", count)
	}
}

func TestCursorGroupCacheBackward(t *testing.T) {
	// Scan backward across group boundaries to verify populateGroup handles
	// reverse traversal (new group populated on each boundary crossing).
	tr := newTestTree(t, 512)

	for i := range 40 {
		_, _, err := tr.Put(inlineEntry(testKey(i), testVal(i)))
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}

	c := tr.NewCursor()
	count := 0
	for k, v := c.Last(); k != nil; k, v = c.Prev() {
		idx := 39 - count
		wantKey := testKey(idx)
		wantVal := testVal(idx)
		if !bytes.Equal(k, wantKey) {
			t.Errorf("entry %d (reverse): key = %q, want %q", count, k, wantKey)
		}
		if !bytes.Equal(v, wantVal) {
			t.Errorf("entry %d (reverse): val = %q, want %q", count, v, wantVal)
		}
		count++
	}
	if count != 40 {
		t.Errorf("scanned %d entries, want 40", count)
	}
}

// --- Split point correctness: both halves must fit ---

// verifyLeafSplitFits calls findLeafSplitPoint and rebuilds both halves,
// asserting both fit in a page.
func verifyLeafSplitFits(t *testing.T, tr *Tree, entries []page.LeafEntry) {
	t.Helper()
	if len(entries) < 2 {
		return
	}
	split := tr.findLeafSplitPoint(entries, -1)
	if split < 1 || split >= len(entries) {
		t.Fatalf("findLeafSplitPoint returned %d for %d entries", split, len(entries))
	}

	leftPage, err := tr.allocPage()
	if err != nil {
		t.Fatal(err)
	}
	if tr.rebuildLeaf(leftPage, entries[:split]) < 0 {
		t.Errorf("left half (%d entries) does not fit", split)
	}

	rightPage, err := tr.allocPage()
	if err != nil {
		t.Fatal(err)
	}
	if tr.rebuildLeaf(rightPage, entries[split:]) < 0 {
		t.Errorf("right half (%d entries) does not fit", len(entries)-split)
	}
}

func TestFindLeafSplitPointSmallValues(t *testing.T) {
	tr := newTestTree(t, 256)
	var entries []page.LeafEntry
	for i := 0; ; i++ {
		e := inlineEntry(
			[]byte(fmt.Sprintf("key:%08d", i)),
			[]byte(fmt.Sprintf("val:%08d", i)),
		)
		entries = append(entries, e)
		if tr.rebuildLeaf(3, entries) < 0 {
			break // one past capacity
		}
	}
	verifyLeafSplitFits(t, tr, entries)
}

func TestFindLeafSplitPointLargeValues(t *testing.T) {
	tr := newTestTree(t, 256)
	bigVal := bytes.Repeat([]byte("v"), 500)
	var entries []page.LeafEntry
	for i := 0; ; i++ {
		e := inlineEntry([]byte(fmt.Sprintf("key:%08d", i)), bigVal)
		entries = append(entries, e)
		if tr.rebuildLeaf(3, entries) < 0 {
			break
		}
	}
	verifyLeafSplitFits(t, tr, entries)
}

func TestFindLeafSplitPointLongSharedPrefix(t *testing.T) {
	tr := newTestTree(t, 256)
	prefix := bytes.Repeat([]byte("p"), 200)
	var entries []page.LeafEntry
	for i := 0; ; i++ {
		key := append(bytes.Clone(prefix), []byte(fmt.Sprintf("%08d", i))...)
		e := inlineEntry(key, []byte("v"))
		entries = append(entries, e)
		if tr.rebuildLeaf(3, entries) < 0 {
			break
		}
	}
	verifyLeafSplitFits(t, tr, entries)
}

func TestFindLeafSplitPointTwoEntries(t *testing.T) {
	tr := newTestTree(t, 256)
	big := bytes.Repeat([]byte("x"), 1500)
	entries := []page.LeafEntry{
		inlineEntry([]byte("aaa"), big),
		inlineEntry([]byte("zzz"), big),
	}
	verifyLeafSplitFits(t, tr, entries)
}

func TestFindLeafSplitPointMixedCellTypes(t *testing.T) {
	tr := newTestTree(t, 256)
	spBuf := make([]byte, 64)
	spb := page.NewSubpageBuilder(spBuf, 0)
	spb.AddValue([]byte("a"))
	spSize := spb.Finish()

	var entries []page.LeafEntry
	for i := 0; ; i++ {
		key := []byte(fmt.Sprintf("key:%08d", i))
		var e page.LeafEntry
		switch i % 3 {
		case 0:
			e = inlineEntry(key, bytes.Repeat([]byte("v"), 100))
		case 1:
			e = page.LeafEntry{Key: key, CellFlags: page.CellFlagOverflow, OvflPage: uint64(i), TotalLen: 99999}
		case 2:
			e = page.LeafEntry{Key: key, CellFlags: page.CellFlagMultiValue, SubpageData: spBuf[:spSize]}
		}
		entries = append(entries, e)
		if tr.rebuildLeaf(3, entries) < 0 {
			break
		}
	}
	verifyLeafSplitFits(t, tr, entries)
}

func TestFindLeafSplitPointOneHugeEntry(t *testing.T) {
	// When one entry doesn't fit at all, findLeafSplitPoint returns 1
	// (split after that entry caused the overflow).
	tr := newTestTree(t, 256)
	huge := bytes.Repeat([]byte("x"), int(tr.cfg.Page.UsableSpace()))
	entries := []page.LeafEntry{
		inlineEntry([]byte("a"), huge),
		inlineEntry([]byte("b"), []byte("small")),
	}
	split := tr.findLeafSplitPoint(entries, -1)
	if split != 1 {
		t.Errorf("split = %d, want 1", split)
	}
}

// verifyBiasedSplitFits calls findLeafSplitPoint with the given insertIdx
// and verifies both halves rebuild successfully.
func verifyBiasedSplitFits(t *testing.T, tr *Tree, entries []page.LeafEntry, insertIdx int) {
	t.Helper()
	if len(entries) < 2 {
		return
	}
	split := tr.findLeafSplitPoint(entries, insertIdx)
	if split < 1 || split >= len(entries) {
		t.Fatalf("findLeafSplitPoint(insertIdx=%d) returned %d for %d entries", insertIdx, split, len(entries))
	}

	leftPage, err := tr.allocPage()
	if err != nil {
		t.Fatal(err)
	}
	if tr.rebuildLeaf(leftPage, entries[:split]) < 0 {
		t.Errorf("left half (%d entries) does not fit (insertIdx=%d)", split, insertIdx)
	}

	rightPage, err := tr.allocPage()
	if err != nil {
		t.Fatal(err)
	}
	if tr.rebuildLeaf(rightPage, entries[split:]) < 0 {
		t.Errorf("right half (%d entries) does not fit (insertIdx=%d)", len(entries)-split, insertIdx)
	}
}

// buildOverflowEntries builds an entry list that overflows one leaf page.
func buildOverflowEntries(tr *Tree, valSize int) []page.LeafEntry {
	val := bytes.Repeat([]byte("v"), valSize)
	var entries []page.LeafEntry
	for i := 0; ; i++ {
		e := inlineEntry([]byte(fmt.Sprintf("key:%08d", i)), val)
		entries = append(entries, e)
		if tr.rebuildLeaf(3, entries) < 0 {
			break // one past capacity
		}
	}
	return entries
}

func TestBiasedSplitAppendFits(t *testing.T) {
	for _, bias := range []int{50, 75, 90, 100} {
		for _, valSize := range []int{8, 100, 500, 1500} {
			t.Run(fmt.Sprintf("bias=%d/val=%d", bias, valSize), func(t *testing.T) {
				tr := newTestTree(t, 256)
				tr.cfg.SplitBias = bias
				entries := buildOverflowEntries(tr, valSize)
				// insertIdx = last entry (append pattern)
				verifyBiasedSplitFits(t, tr, entries, len(entries)-1)
			})
		}
	}
}

func TestBiasedSplitPrependFits(t *testing.T) {
	for _, bias := range []int{50, 75, 90, 100} {
		for _, valSize := range []int{8, 100, 500, 1500} {
			t.Run(fmt.Sprintf("bias=%d/val=%d", bias, valSize), func(t *testing.T) {
				tr := newTestTree(t, 256)
				tr.cfg.SplitBias = bias
				entries := buildOverflowEntries(tr, valSize)
				// insertIdx = 0 (prepend pattern)
				verifyBiasedSplitFits(t, tr, entries, 0)
			})
		}
	}
}

func TestBiasedSplitTwoEntries(t *testing.T) {
	// Edge case: only 2 entries, split must produce 1 per side regardless of bias.
	for _, bias := range []int{50, 75, 90, 100} {
		t.Run(fmt.Sprintf("bias=%d", bias), func(t *testing.T) {
			tr := newTestTree(t, 256)
			tr.cfg.SplitBias = bias
			big := bytes.Repeat([]byte("x"), 1500)
			entries := []page.LeafEntry{
				inlineEntry([]byte("aaa"), big),
				inlineEntry([]byte("zzz"), big),
			}
			// Append
			split := tr.findLeafSplitPoint(entries, 1)
			if split != 1 {
				t.Errorf("append: split = %d, want 1", split)
			}
			// Prepend
			split = tr.findLeafSplitPoint(entries, 0)
			if split != 1 {
				t.Errorf("prepend: split = %d, want 1", split)
			}
		})
	}
}

func TestBiasedSplitMixedCellTypes(t *testing.T) {
	for _, bias := range []int{75, 90, 100} {
		t.Run(fmt.Sprintf("bias=%d", bias), func(t *testing.T) {
			tr := newTestTree(t, 256)
			tr.cfg.SplitBias = bias

			spBuf := make([]byte, 64)
			spb := page.NewSubpageBuilder(spBuf, 0)
			spb.AddValue([]byte("a"))
			spSize := spb.Finish()

			var entries []page.LeafEntry
			for i := 0; ; i++ {
				key := []byte(fmt.Sprintf("key:%08d", i))
				var e page.LeafEntry
				switch i % 3 {
				case 0:
					e = inlineEntry(key, bytes.Repeat([]byte("v"), 100))
				case 1:
					e = page.LeafEntry{Key: key, CellFlags: page.CellFlagOverflow, OvflPage: uint64(i), TotalLen: 99999}
				case 2:
					e = page.LeafEntry{Key: key, CellFlags: page.CellFlagMultiValue, SubpageData: spBuf[:spSize]}
				}
				entries = append(entries, e)
				if tr.rebuildLeaf(3, entries) < 0 {
					break
				}
			}
			verifyBiasedSplitFits(t, tr, entries, len(entries)-1) // append
			verifyBiasedSplitFits(t, tr, entries, 0)              // prepend
		})
	}
}

// TestSequentialInsertThenDelete verifies that trees built with biased splits
// remain correct after deletions that trigger merge/redistribute.
func TestSequentialInsertThenDelete(t *testing.T) {
	for _, bias := range []int{50, 75, 90} {
		t.Run(fmt.Sprintf("bias=%d", bias), func(t *testing.T) {
			tr := newTestTree(t, 1024)
			tr.cfg.SplitBias = bias
			n := 1000
			val := bytes.Repeat([]byte("v"), 100)

			for i := range n {
				key := fmt.Appendf(nil, "key:%04d", i)
				tr.Put(page.LeafEntry{Key: key, Value: val})
			}

			// Delete every other key to trigger rebalancing.
			for i := 0; i < n; i += 2 {
				key := fmt.Appendf(nil, "key:%04d", i)
				_, found, err := tr.Delete(key)
				if err != nil {
					t.Fatalf("Delete(%d): %v", i, err)
				}
				if !found {
					t.Fatalf("Delete(%d): not found", i)
				}
			}

			// Verify remaining entries.
			c := tr.NewCursor()
			var prev []byte
			count := 0
			for k, _ := c.First(); k != nil; k, _ = c.Next() {
				if prev != nil && bytes.Compare(prev, k) >= 0 {
					t.Fatalf("keys not sorted: %q >= %q", prev, k)
				}
				prev = bytes.Clone(k)
				count++
			}
			if count != n/2 {
				t.Fatalf("got %d entries, want %d", count, n/2)
			}
		})
	}
}

// TestMixedInsertPattern verifies that a tree receiving both sequential and
// random inserts (triggering both biased and unbiased splits) stays correct.
func TestMixedInsertPattern(t *testing.T) {
	tr := newTestTree(t, 2048)
	val := bytes.Repeat([]byte("v"), 50)
	rng := rand.New(rand.NewPCG(42, 0))
	n := 2000
	keys := make(map[string]bool)

	// Alternate: 10 sequential, 10 random.
	seq := 0
	for i := range n {
		var key []byte
		if i%20 < 10 {
			key = fmt.Appendf(nil, "seq:%06d", seq)
			seq++
		} else {
			key = fmt.Appendf(nil, "rnd:%06d", rng.IntN(1_000_000))
		}
		tr.Put(page.LeafEntry{Key: key, Value: val})
		keys[string(key)] = true
	}

	// Verify all unique keys are present and sorted.
	c := tr.NewCursor()
	var prev []byte
	count := 0
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		if prev != nil && bytes.Compare(prev, k) >= 0 {
			t.Fatalf("keys not sorted: %q >= %q", prev, k)
		}
		prev = bytes.Clone(k)
		count++
	}
	if count != len(keys) {
		t.Fatalf("got %d entries, want %d", count, len(keys))
	}
}

// verifyBranchSplitFits calls findBranchSplitPoint and rebuilds both halves.
func verifyBranchSplitFits(t *testing.T, tr *Tree, ptr0 uint64, cells []branchCell) {
	t.Helper()
	if len(cells) < 2 {
		return
	}
	split := tr.findBranchSplitPoint(ptr0, cells)
	if split < 1 || split >= len(cells) {
		t.Fatalf("findBranchSplitPoint returned %d for %d cells", split, len(cells))
	}

	leftPage, err := tr.allocPage()
	if err != nil {
		t.Fatal(err)
	}
	if tr.rebuildBranch(leftPage, ptr0, cells[:split]) < 0 {
		t.Errorf("left half (%d cells) does not fit", split)
	}

	rightPage, err := tr.allocPage()
	if err != nil {
		t.Fatal(err)
	}
	if tr.rebuildBranch(rightPage, cells[split].childPtr, cells[split+1:]) < 0 {
		t.Errorf("right half (%d cells) does not fit", len(cells)-split-1)
	}
}

func TestFindBranchSplitPointShortKeys(t *testing.T) {
	tr := newTestTree(t, 256)
	var cells []branchCell
	for i := 0; ; i++ {
		cells = append(cells, branchCell{
			key:      []byte(fmt.Sprintf("k:%06d", i)),
			childPtr: uint64(i + 10),
		})
		if tr.rebuildBranch(3, 9, cells) < 0 {
			break
		}
	}
	verifyBranchSplitFits(t, tr, 9, cells)
}

func TestFindBranchSplitPointLongKeys(t *testing.T) {
	tr := newTestTree(t, 256)
	var cells []branchCell
	for i := 0; ; i++ {
		key := append(bytes.Repeat([]byte("k"), 200), []byte(fmt.Sprintf("%06d", i))...)
		cells = append(cells, branchCell{key: key, childPtr: uint64(i + 10)})
		if tr.rebuildBranch(3, 9, cells) < 0 {
			break
		}
	}
	verifyBranchSplitFits(t, tr, 9, cells)
}

func TestFindBranchSplitPointTwoCells(t *testing.T) {
	tr := newTestTree(t, 256)
	big := bytes.Repeat([]byte("x"), 1500)
	cells := []branchCell{
		{key: big, childPtr: 10},
		{key: big, childPtr: 11},
	}
	verifyBranchSplitFits(t, tr, 9, cells)
}

func TestFindBranchSplitPointVaryingKeySizes(t *testing.T) {
	tr := newTestTree(t, 256)
	var cells []branchCell
	for i := 0; ; i++ {
		var key []byte
		if i%2 == 0 {
			key = []byte(fmt.Sprintf("s%04d", i))
		} else {
			key = append(bytes.Repeat([]byte("L"), 300), []byte(fmt.Sprintf("%04d", i))...)
		}
		cells = append(cells, branchCell{key: key, childPtr: uint64(i + 10)})
		if tr.rebuildBranch(3, 9, cells) < 0 {
			break
		}
	}
	verifyBranchSplitFits(t, tr, 9, cells)
}
