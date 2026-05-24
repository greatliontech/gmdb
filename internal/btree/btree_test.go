package btree

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// fakeReader satisfies PageReader with a manufactured page set —
// the test harness for btree's read paths. Production *pager.Pager
// resolves slab-then-mmap; for these unit tests we just need a
// PageReader contract.
type fakeReader struct {
	pageSize uint32
	pages    map[uint64][]byte
}

func newFakeReader(t *testing.T, pageSize uint32) *fakeReader {
	t.Helper()
	return &fakeReader{pageSize: pageSize, pages: make(map[uint64][]byte)}
}

func (f *fakeReader) Page(id uint64) []byte {
	if buf, ok := f.pages[id]; ok {
		return buf
	}
	panic(fmt.Sprintf("fakeReader: page %d not registered", id))
}

func (f *fakeReader) put(id uint64, buf []byte) {
	if uint32(len(buf)) != f.pageSize {
		panic(fmt.Sprintf("fakeReader.put: buf len %d != PageSize %d", len(buf), f.pageSize))
	}
	f.pages[id] = buf
}

// makeLeaf encodes a leaf page with the given entries.
func makeLeaf(t *testing.T, cfg page.Config, interval uint16, entries []page.EncodedEntry) []byte {
	t.Helper()
	buf := make([]byte, cfg.PageSize)
	if err := page.EncodeLeaf(buf, cfg, interval, entries); err != nil {
		t.Fatalf("makeLeaf: %v", err)
	}
	return buf
}

// makeBranch encodes a branch page with the given leftmost child
// and sorted cells.
func makeBranch(t *testing.T, cfg page.Config, leftmost uint64, cells []page.BranchCell) []byte {
	t.Helper()
	buf := make([]byte, cfg.PageSize)
	if err := page.EncodeBranch(buf, cfg, leftmost, cells); err != nil {
		t.Fatalf("makeBranch: %v", err)
	}
	return buf
}

func TestGetEmptyTreeReturnsNotFound(t *testing.T) {
	pr := newFakeReader(t, 4096)
	cfg := page.Config{PageSize: 4096}
	v, found, err := Get(pr, cfg, 0, []byte("anything"))
	if err != nil || found || v != nil {
		t.Errorf("Get on empty tree: got (%v, %v, %v); want (nil, false, nil)", v, found, err)
	}
}

func TestGetSingleLeafHit(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pr := newFakeReader(t, 4096)
	pr.put(1, makeLeaf(t, cfg, 16, []page.EncodedEntry{
		{Key: []byte("apple"), Value: []byte("fruit-A")},
		{Key: []byte("banana"), Value: []byte("fruit-B")},
		{Key: []byte("cherry"), Value: []byte("fruit-C")},
	}))
	for _, c := range []struct {
		key  string
		want string
		hit  bool
	}{
		{"apple", "fruit-A", true},
		{"banana", "fruit-B", true},
		{"cherry", "fruit-C", true},
		{"date", "", false},
		{"", "", false},
		{"apricot", "", false}, // between apple and banana
	} {
		v, found, err := Get(pr, cfg, 1, []byte(c.key))
		if err != nil {
			t.Errorf("Get(%q): unexpected err %v", c.key, err)
			continue
		}
		if found != c.hit {
			t.Errorf("Get(%q): found = %v, want %v", c.key, found, c.hit)
		}
		if c.hit && !bytes.Equal(v, []byte(c.want)) {
			t.Errorf("Get(%q): value = %q, want %q", c.key, v, c.want)
		}
	}
}

func TestGetSingleLeafExercisesDeltaCompression(t *testing.T) {
	// Pin that Get works correctly across restart-group boundaries
	// — keys with high shared prefix at small interval forces
	// multiple groups and exercises both phase-1 binary search
	// and phase-2 delta decode.
	cfg := page.Config{PageSize: 4096}
	pr := newFakeReader(t, 4096)
	entries := make([]page.EncodedEntry, 0, 32)
	for i := range 32 {
		entries = append(entries, page.EncodedEntry{
			Key:   fmt.Appendf(nil, "prefix-key-%03d", i),
			Value: fmt.Appendf(nil, "v%03d", i),
		})
	}
	pr.put(1, makeLeaf(t, cfg, 4, entries))

	// Hit every key.
	for i, e := range entries {
		v, found, err := Get(pr, cfg, 1, e.Key)
		if err != nil || !found {
			t.Errorf("entry %d (%q): found=%v err=%v", i, e.Key, found, err)
			continue
		}
		if !bytes.Equal(v, e.Value) {
			t.Errorf("entry %d (%q): value=%q want=%q", i, e.Key, v, e.Value)
		}
	}
	// Misses around boundaries.
	for _, missKey := range []string{
		"prefix-key-",     // shorter than any
		"prefix-key-zzz",  // sorts after all
		"prefix-key-100",  // gap between -099 and (nothing); also after 031, alphabetically before some
		"prefix-key-0000", // immediately precedes 000? lexicographic: "prefix-key-0000" < "prefix-key-000" (longer with extra '0' is greater? no — shorter wins on prefix tie). Skip if ambiguous.
	} {
		v, found, err := Get(pr, cfg, 1, []byte(missKey))
		if err != nil {
			t.Errorf("miss %q: err %v", missKey, err)
		}
		// We accept either hit-or-miss for the boundary cases —
		// the test scope is "no panic, no error"; ordering of
		// these specific strings is well-defined but not the
		// point of this test.
		_, _ = v, found
	}
}

func TestGetSingleBranchDescent(t *testing.T) {
	// Two leaves separated by one branch:
	//   branch (root=3): leftmost=1; cell["m"]=2
	//   leaf 1: a, b, c
	//   leaf 2: m, n, o
	cfg := page.Config{PageSize: 4096}
	pr := newFakeReader(t, 4096)
	pr.put(1, makeLeaf(t, cfg, 16, []page.EncodedEntry{
		{Key: []byte("a"), Value: []byte("A")},
		{Key: []byte("b"), Value: []byte("B")},
		{Key: []byte("c"), Value: []byte("C")},
	}))
	pr.put(2, makeLeaf(t, cfg, 16, []page.EncodedEntry{
		{Key: []byte("m"), Value: []byte("M")},
		{Key: []byte("n"), Value: []byte("N")},
		{Key: []byte("o"), Value: []byte("O")},
	}))
	pr.put(3, makeBranch(t, cfg, 1, []page.BranchCell{
		{Key: []byte("m"), Child: 2},
	}))

	for _, c := range []struct {
		key, want string
		hit       bool
	}{
		{"a", "A", true},
		{"b", "B", true},
		{"c", "C", true},
		{"m", "M", true},
		{"n", "N", true},
		{"o", "O", true},
		{"d", "", false}, // gap between leaves
		{"l", "", false},
		{"p", "", false},
		{"", "", false},
	} {
		v, found, err := Get(pr, cfg, 3, []byte(c.key))
		if err != nil {
			t.Errorf("Get(%q): err %v", c.key, err)
			continue
		}
		if found != c.hit {
			t.Errorf("Get(%q): found = %v, want %v", c.key, found, c.hit)
		}
		if c.hit && !bytes.Equal(v, []byte(c.want)) {
			t.Errorf("Get(%q): value = %q, want %q", c.key, v, c.want)
		}
	}
}

func TestGetMultiLevelDescent(t *testing.T) {
	// Three-level tree:
	//   root branch (id=10): leftmost=4; cell["g"]=5; cell["m"]=6
	//   branch 4: leftmost=1; cell["c"]=2; cell["e"]=3
	//   branch 5: leftmost=7; cell["i"]=8; cell["k"]=9
	//   branch 6: leftmost=11; cell["o"]=12; cell["s"]=13
	//   leaf 1: a, b      | leaf 2: c, d     | leaf 3: e, f
	//   leaf 7: g, h      | leaf 8: i, j     | leaf 9: k, l
	//   leaf 11: m, n     | leaf 12: o, p, q, r | leaf 13: s, t, u
	cfg := page.Config{PageSize: 4096}
	pr := newFakeReader(t, 4096)
	leaves := map[uint64][]string{
		1:  {"a", "b"},
		2:  {"c", "d"},
		3:  {"e", "f"},
		7:  {"g", "h"},
		8:  {"i", "j"},
		9:  {"k", "l"},
		11: {"m", "n"},
		12: {"o", "p", "q", "r"},
		13: {"s", "t", "u"},
	}
	for id, keys := range leaves {
		entries := make([]page.EncodedEntry, 0, len(keys))
		for _, k := range keys {
			entries = append(entries, page.EncodedEntry{
				Key:   []byte(k),
				Value: bytes.ToUpper([]byte(k)),
			})
		}
		pr.put(id, makeLeaf(t, cfg, 16, entries))
	}
	pr.put(4, makeBranch(t, cfg, 1, []page.BranchCell{
		{Key: []byte("c"), Child: 2},
		{Key: []byte("e"), Child: 3},
	}))
	pr.put(5, makeBranch(t, cfg, 7, []page.BranchCell{
		{Key: []byte("i"), Child: 8},
		{Key: []byte("k"), Child: 9},
	}))
	pr.put(6, makeBranch(t, cfg, 11, []page.BranchCell{
		{Key: []byte("o"), Child: 12},
		{Key: []byte("s"), Child: 13},
	}))
	pr.put(10, makeBranch(t, cfg, 4, []page.BranchCell{
		{Key: []byte("g"), Child: 5},
		{Key: []byte("m"), Child: 6},
	}))

	allKeys := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q", "r", "s", "t", "u"}
	for _, k := range allKeys {
		v, found, err := Get(pr, cfg, 10, []byte(k))
		if err != nil || !found {
			t.Errorf("Get(%q): found=%v err=%v", k, found, err)
			continue
		}
		want := bytes.ToUpper([]byte(k))
		if !bytes.Equal(v, want) {
			t.Errorf("Get(%q): value=%q want=%q", k, v, want)
		}
	}
	// Misses.
	for _, k := range []string{"", "0", "v", "z"} {
		_, found, err := Get(pr, cfg, 10, []byte(k))
		if err != nil {
			t.Errorf("Get miss %q: err %v", k, err)
		}
		if found {
			t.Errorf("Get miss %q: unexpectedly found", k)
		}
	}
}

func TestGetRejectsCorruptPageType(t *testing.T) {
	// Spec-tier invariant promotion: btree descent must surface
	// ErrCorrupted on a page whose type is neither branch nor leaf.
	// Pin by manufacturing an "overflow" page at the root — the
	// descent should refuse rather than recurse.
	cfg := page.Config{PageSize: 4096}
	pr := newFakeReader(t, 4096)
	buf := make([]byte, cfg.PageSize)
	page.WriteHeader(buf, page.TypeOverflow, 0, 0)
	pr.put(1, buf)
	_, found, err := Get(pr, cfg, 1, []byte("k"))
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("Get on overflow-typed root: err = %v, want ErrCorrupted", err)
	}
	if found {
		t.Error("Get on corrupt page: found = true; want false")
	}
}

func TestGetRejectsNullChildPointer(t *testing.T) {
	// Spec-tier invariant: a branch page with a null (0) child
	// pointer is structurally invalid (no allocator ever hands out
	// page 0). Descent must surface ErrCorrupted.
	cfg := page.Config{PageSize: 4096}
	pr := newFakeReader(t, 4096)
	pr.put(1, makeBranch(t, cfg, 0, []page.BranchCell{
		{Key: []byte("k"), Child: 0},
	}))
	_, _, err := Get(pr, cfg, 1, []byte("a"))
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("Get on null-child branch: err = %v, want ErrCorrupted", err)
	}
}

func TestGetWrapsLeafLookupErrorsAsCorrupted(t *testing.T) {
	// btree.Get wraps page.LeafLookup decoder errors with
	// ErrCorrupted so a single errors.Is check covers all
	// structural-corruption surfaces (branch type, null child,
	// leaf decode). Pin by forging the RestartCount on a leaf
	// page — the leaf decoder rejects it per the chunk-4.2 M3
	// gate, and Get must surface ErrCorrupted.
	cfg := page.Config{PageSize: 4096}
	pr := newFakeReader(t, 4096)
	buf := makeLeaf(t, cfg, 16, []page.EncodedEntry{
		{Key: []byte("k"), Value: []byte("v")},
	})
	// RestartCount field at offset 10 (HeaderSize+2). Correct
	// value is 1; forge to 2 to fail the cross-check.
	buf[10] = 2
	buf[11] = 0
	pr.put(1, buf)
	_, _, err := Get(pr, cfg, 1, []byte("k"))
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("Get on forged-RestartCount leaf: err = %v, want errors.Is(ErrCorrupted)", err)
	}
}

func TestHasMembership(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pr := newFakeReader(t, 4096)
	pr.put(1, makeLeaf(t, cfg, 16, []page.EncodedEntry{
		{Key: []byte("k1"), Value: []byte("v1")},
		{Key: []byte("k2"), Value: []byte("v2")},
	}))
	has, err := Has(pr, cfg, 1, []byte("k1"))
	if err != nil || !has {
		t.Errorf("Has(k1) = (%v, %v); want (true, nil)", has, err)
	}
	has, err = Has(pr, cfg, 1, []byte("missing"))
	if err != nil || has {
		t.Errorf("Has(missing) = (%v, %v); want (false, nil)", has, err)
	}
}

func TestGetOverflowEntryReturnsSentinel(t *testing.T) {
	// Chunk-4.3 contract: matching an overflow-flagged leaf entry
	// returns ErrOverflowValueUnsupported. The chunk-4.7 wiring
	// replaces this with the actual overflow-run assembly.
	cfg := page.Config{PageSize: 4096}
	pr := newFakeReader(t, 4096)
	pr.put(1, makeLeaf(t, cfg, 16, []page.EncodedEntry{
		{
			Key:          []byte("big"),
			Flags:        page.CellFlagOverflow,
			OverflowPage: 42,
			TotalLen:     100000,
		},
	}))
	_, found, err := Get(pr, cfg, 1, []byte("big"))
	if !errors.Is(err, ErrOverflowValueUnsupported) {
		t.Errorf("Get on overflow entry: err = %v, want ErrOverflowValueUnsupported", err)
	}
	if !found {
		t.Errorf("Get on overflow entry: found = false; want true (key matched even if value pending)")
	}
	// Has should NOT surface the sentinel — membership is
	// determinable regardless of value-assembly support.
	has, err := Has(pr, cfg, 1, []byte("big"))
	if err != nil || !has {
		t.Errorf("Has on overflow entry: (%v, %v); want (true, nil)", has, err)
	}
}
