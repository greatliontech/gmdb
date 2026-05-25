package gmdb

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/btree"
)

// --- Index.Lookup (unique) ----------------------------------------

// TestIndexLookupUniqueReturnsSingleMatch verifies that Lookup on
// a unique index returns the matching (pk, value) pair.
func TestIndexLookupUniqueReturnsSingleMatch(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Unique = true
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("k1"), []byte{0x42, 'x', 'y'}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	var pks, values [][]byte
	for pk, value := range idx.Lookup([]byte{0x42}) {
		pks = append(pks, pk)
		values = append(values, value)
	}
	if idx.Err() != nil {
		t.Fatalf("idx.Err: %v", idx.Err())
	}
	if len(pks) != 1 || string(pks[0]) != "k1" {
		t.Errorf("Lookup pks: got %v want [k1]", byteSlicesString(pks))
	}
	if len(values) != 1 || !bytes.Equal(values[0], []byte{0x42, 'x', 'y'}) {
		t.Errorf("Lookup values: got %x want [42 78 79]", values[0])
	}
}

// TestIndexLookupUniqueNoMatchEmptySeq verifies that Lookup on
// a unique index with no matching entry yields no pairs.
func TestIndexLookupUniqueNoMatchEmptySeq(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Unique = true
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("k1"), []byte{0x42}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, _ := ks.Index("by_color")
	n := 0
	for range idx.Lookup([]byte{0x99}) {
		n++
	}
	if n != 0 {
		t.Errorf("Lookup no-match: got %d pairs want 0", n)
	}
	if idx.Err() != nil {
		t.Errorf("Err on no-match: %v", idx.Err())
	}
}

// --- Index.Lookup (non-unique) ------------------------------------

// TestIndexLookupNonUniqueReturnsAllMatches verifies that Lookup
// on a non-unique index returns every row whose extractor produced
// the matching column tuple. The PK is appended to the index key
// per the chunk-7.6 non-unique encoding.
func TestIndexLookupNonUniqueReturnsAllMatches(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// 3 rows, all with same first-byte color → 3 entries under
	// the same column tuple.
	for _, k := range []string{"a", "b", "c"} {
		if err := ks.Put([]byte(k), []byte{0x42, 'x'}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	idx, _ := ks.Index("by_color")
	var pks []string
	for pk := range idx.Lookup([]byte{0x42}) {
		pks = append(pks, string(pk))
	}
	if idx.Err() != nil {
		t.Fatalf("idx.Err: %v", idx.Err())
	}
	sort.Strings(pks)
	want := []string{"a", "b", "c"}
	if !stringSlicesEqual(pks, want) {
		t.Errorf("Lookup pks: got %v want %v", pks, want)
	}
}

// TestIndexLookupNonUniqueDifferentColorsIsolated verifies that
// Lookup's prefix-walk stops correctly at the boundary between
// distinct column tuples — a non-matching color appears in the
// index but is NOT yielded.
func TestIndexLookupNonUniqueDifferentColorsIsolated(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// Two color groups: 0x42 → {a, b}; 0x43 → {c}.
	if err := ks.Put([]byte("a"), []byte{0x42}); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if err := ks.Put([]byte("b"), []byte{0x42}); err != nil {
		t.Fatalf("Put b: %v", err)
	}
	if err := ks.Put([]byte("c"), []byte{0x43}); err != nil {
		t.Fatalf("Put c: %v", err)
	}
	idx, _ := ks.Index("by_color")
	var pks []string
	for pk := range idx.Lookup([]byte{0x42}) {
		pks = append(pks, string(pk))
	}
	sort.Strings(pks)
	if !stringSlicesEqual(pks, []string{"a", "b"}) {
		t.Errorf("Lookup 0x42: got %v want [a b] — prefix boundary violated", pks)
	}
}

// --- Index.LookupKeys (no back-lookup) ----------------------------

// TestIndexLookupKeysReturnsPKsWithoutBackLookup verifies that
// LookupKeys returns matching PKs without touching the row
// keyspace. Even if the row was somehow deleted, the index PK is
// still returned (unlike Lookup which silently skips).
func TestIndexLookupKeysReturnsPKsWithoutBackLookup(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for _, k := range []string{"a", "b"} {
		if err := ks.Put([]byte(k), []byte{0x42}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	idx, _ := ks.Index("by_color")
	var pks []string
	for pk := range idx.LookupKeys([]byte{0x42}) {
		pks = append(pks, string(pk))
	}
	sort.Strings(pks)
	if !stringSlicesEqual(pks, []string{"a", "b"}) {
		t.Errorf("LookupKeys: got %v want [a b]", pks)
	}
}

// --- Index.Range --------------------------------------------------

// TestIndexRangeOpenBoundsScansAll verifies that Range(nil, nil)
// iterates every index entry.
func TestIndexRangeOpenBoundsScansAll(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for i, k := range []string{"a", "b", "c"} {
		if err := ks.Put([]byte(k), []byte{byte('A' + i)}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	idx, _ := ks.Index("by_color")
	var pks []string
	for pk := range idx.Range(nil, nil) {
		pks = append(pks, string(pk))
	}
	sort.Strings(pks)
	if !stringSlicesEqual(pks, []string{"a", "b", "c"}) {
		t.Errorf("Range full: got %v want [a b c]", pks)
	}
}

// TestIndexRangeBoundedExcludesEnd verifies the [start, end)
// boundary semantics on a unique index.
func TestIndexRangeBoundedExcludesEnd(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Unique = true
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// Colors 0x41, 0x42, 0x43.
	for i, c := range []byte{0x41, 0x42, 0x43} {
		k := []byte{byte('a' + i)}
		if err := ks.Put(k, []byte{c}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	idx, _ := ks.Index("by_color")
	var pks []string
	// Range [0x42, 0x43) — only the 0x42 entry.
	for pk := range idx.Range([][]byte{{0x42}}, [][]byte{{0x43}}) {
		pks = append(pks, string(pk))
	}
	if !stringSlicesEqual(pks, []string{"b"}) {
		t.Errorf("Range [0x42, 0x43): got %v want [b]", pks)
	}
}

// --- Index.Prefix -------------------------------------------------

// TestIndexPrefixMatchesLeadingCols verifies that Prefix matches
// rows whose leading columns equal the prefix.
func TestIndexPrefixMatchesLeadingCols(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	// 2-column index: (color, size).
	decl := &IndexDecl{
		Name:    "by_color_size",
		Columns: []IndexColumn{{Name: "color"}, {Name: "size"}},
		Extract: func(_, value []byte) []IndexEntry {
			if len(value) < 2 {
				return nil
			}
			return []IndexEntry{{Cols: [][]byte{{value[0]}, {value[1]}}}}
		},
	}
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// Two rows with color=0x42, different sizes; one with color=0x43.
	if err := ks.Put([]byte("a"), []byte{0x42, 0x10}); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if err := ks.Put([]byte("b"), []byte{0x42, 0x20}); err != nil {
		t.Fatalf("Put b: %v", err)
	}
	if err := ks.Put([]byte("c"), []byte{0x43, 0x10}); err != nil {
		t.Fatalf("Put c: %v", err)
	}
	idx, _ := ks.Index("by_color_size")
	var pks []string
	for pk := range idx.Prefix([]byte{0x42}) {
		pks = append(pks, string(pk))
	}
	sort.Strings(pks)
	if !stringSlicesEqual(pks, []string{"a", "b"}) {
		t.Errorf("Prefix [0x42]: got %v want [a b]", pks)
	}
}

// --- Index.Get ----------------------------------------------------

// TestIndexGetUniqueReturnsSingle verifies the Get shorthand on
// a unique index.
func TestIndexGetUniqueReturnsSingle(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Unique = true
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("k1"), []byte{0x42, 'v'}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, _ := ks.Index("by_color")
	pk, value, err := idx.Get([]byte{0x42})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(pk) != "k1" {
		t.Errorf("Get pk: got %q want k1", pk)
	}
	if !bytes.Equal(value, []byte{0x42, 'v'}) {
		t.Errorf("Get value: got %x want [42 76]", value)
	}
}

// TestIndexGetUniqueMissReturnsErrNotFound verifies Get on a miss.
func TestIndexGetUniqueMissReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Unique = true
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	idx, _ := ks.Index("by_color")
	_, _, err = idx.Get([]byte{0x99})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get miss: got %v want ErrNotFound", err)
	}
}

// TestIndexGetOnNonUniqueReturnsErrIndexNotUnique verifies that
// Get refuses non-unique indexes.
func TestIndexGetOnNonUniqueReturnsErrIndexNotUnique(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	// Unique = false.
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	idx, _ := ks.Index("by_color")
	_, _, err = idx.Get([]byte{0x42})
	if !errors.Is(err, ErrIndexNotUnique) {
		t.Errorf("Get on non-unique: got %v want ErrIndexNotUnique", err)
	}
}

// --- Regression: Round-1 H-1 (partial-cols validation) ------------

// TestIndexLookupPartialColsReturnsErrInvalidOptions verifies the
// chunk-7.7 Round-1 H-1 fix: Lookup with fewer columns than the
// index declares sets idx.Err() to a wrapped ErrInvalidOptions
// and yields nothing. Use Prefix for partial-cols semantics.
func TestIndexLookupPartialColsReturnsErrInvalidOptions(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := &IndexDecl{
		Name:    "by_color_size",
		Columns: []IndexColumn{{Name: "color"}, {Name: "size"}},
		Extract: func(_, value []byte) []IndexEntry {
			if len(value) < 2 {
				return nil
			}
			return []IndexEntry{{Cols: [][]byte{{value[0]}, {value[1]}}}}
		},
	}
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("a"), []byte{0x42, 0x10}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := ks.Put([]byte("b"), []byte{0x42, 0x20}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, _ := ks.Index("by_color_size")

	// Partial cols (1 supplied vs 2 declared) must NOT widen to Prefix.
	n := 0
	for range idx.Lookup([]byte{0x42}) {
		n++
	}
	if n != 0 {
		t.Errorf("partial-cols Lookup yielded %d entries (spec violation)", n)
	}
	if !errors.Is(idx.Err(), ErrInvalidOptions) {
		t.Errorf("idx.Err: got %v want ErrInvalidOptions wrap", idx.Err())
	}

	// Zero cols (H-2 footgun) — same outcome.
	n = 0
	for range idx.Lookup() {
		n++
	}
	if n != 0 {
		t.Errorf("zero-cols Lookup yielded %d entries", n)
	}
	if !errors.Is(idx.Err(), ErrInvalidOptions) {
		t.Errorf("zero-cols idx.Err: got %v want ErrInvalidOptions wrap", idx.Err())
	}
}

// TestIndexGetPartialColsReturnsErrInvalidOptions mirrors H-1 for Get.
func TestIndexGetPartialColsReturnsErrInvalidOptions(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := &IndexDecl{
		Name:    "by_color_size",
		Columns: []IndexColumn{{Name: "color"}, {Name: "size"}},
		Unique:  true,
		Extract: func(_, value []byte) []IndexEntry {
			if len(value) < 2 {
				return nil
			}
			return []IndexEntry{{Cols: [][]byte{{value[0]}, {value[1]}}}}
		},
	}
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	idx, _ := ks.Index("by_color_size")
	_, _, err = idx.Get([]byte{0x42})
	if !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("partial-cols Get: got %v want ErrInvalidOptions wrap", err)
	}
}

// --- Regression: Round-1 M-2 (per-sequence Err reset) -------------

// TestIndexLookupResetsErrOnNewSequence verifies the chunk-7.7
// M-2 fix: a fresh Lookup/Range/Prefix call resets idx.Err()
// (per api-surface.md "first error encountered during the **last**
// sequence's iteration").
func TestIndexLookupResetsErrOnNewSequence(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("a"), []byte{0x42}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, _ := ks.Index("by_color")
	// First call: partial-cols → idx.err set.
	for range idx.Lookup() {
	}
	if idx.Err() == nil {
		t.Fatalf("expected idx.Err after partial-cols Lookup")
	}
	// Second call: valid Lookup → idx.err must reset.
	n := 0
	for range idx.Lookup([]byte{0x42}) {
		n++
	}
	if idx.Err() != nil {
		t.Errorf("idx.Err not reset on new sequence: %v", idx.Err())
	}
	if n != 1 {
		t.Errorf("valid Lookup after error: got %d entries want 1", n)
	}
}

// --- Regression: Round-1 M-3 (covering write path) ---------------

// TestIndexedPutWritesCoveringBytes verifies that chunk-7.7's
// extended index entry value format encodes the IndexEntry.Cover
// bytes when the IndexDecl declares Covering. Unique value =
// uvarint(len(pk)) || pk || encoded_covering. The test reads the
// raw index value via btree.Get and decodes it.
func TestIndexedPutWritesCoveringBytes(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := &IndexDecl{
		Name:     "by_color",
		Columns:  []IndexColumn{{Name: "color"}},
		Covering: []CoveringColumn{{Name: "size"}},
		Unique:   true,
		Extract: func(_, value []byte) []IndexEntry {
			if len(value) < 2 {
				return nil
			}
			return []IndexEntry{
				{
					Cols:  [][]byte{{value[0]}},
					Cover: [][]byte{{value[1]}},
				},
			}
		},
	}
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("k1"), []byte{0x42, 0xAB}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Decode the index entry value directly.
	encodedIndexKey := encodeIndexKey([][]byte{{0x42}})
	p := ks.indexes["by_color"]
	val, found, err := btree.Get(ks.tx.pgr, ks.tx.pgr.Config(), p.root, encodedIndexKey)
	if err != nil {
		t.Fatalf("btree.Get index entry: %v", err)
	}
	if !found {
		t.Fatalf("index entry not found")
	}
	pk, encodedCov, err := decodeUniqueIndexValue(val)
	if err != nil {
		t.Fatalf("decodeUniqueIndexValue: %v", err)
	}
	if string(pk) != "k1" {
		t.Errorf("decoded pk: got %q want k1", pk)
	}
	// Decoded covering should be encodeIndexKey([{0xAB}]) =
	// 0xAB 0x00 0x00.
	wantCov := []byte{0xAB, 0x00, 0x00}
	if !bytes.Equal(encodedCov, wantCov) {
		t.Errorf("encoded covering: got %x want %x", encodedCov, wantCov)
	}
}

// --- Regression: Round-1 M-4 (decodeUniqueIndexValue errors) -----

// TestDecodeUniqueIndexValueRejectsEmpty verifies the malformed-
// input branch.
func TestDecodeUniqueIndexValueRejectsEmpty(t *testing.T) {
	_, _, err := decodeUniqueIndexValue(nil)
	if !errors.Is(err, errIndexValueShort) {
		t.Errorf("empty input: got %v want errIndexValueShort", err)
	}
}

// TestDecodeUniqueIndexValueRejectsTruncatedPK verifies that a
// uvarint claiming length > remaining bytes returns
// errIndexValueShort.
func TestDecodeUniqueIndexValueRejectsTruncatedPK(t *testing.T) {
	// uvarint(100) = 0x64 0x... wait, 100 < 128 so uvarint(100) = 0x64.
	// Then need 100 bytes of pk; supply only 2.
	val := []byte{0x64, 0x41, 0x42}
	_, _, err := decodeUniqueIndexValue(val)
	if !errors.Is(err, errIndexValueShort) {
		t.Errorf("truncated PK: got %v want errIndexValueShort", err)
	}
}

// TestDecodeUniqueIndexValueAcceptsZeroLengthPK verifies the
// degenerate-but-valid case: uvarint(0) + no pk + optional
// covering.
func TestDecodeUniqueIndexValueAcceptsZeroLengthPK(t *testing.T) {
	// uvarint(0) = 0x00; followed by no pk; covering = 3 bytes.
	val := []byte{0x00, 0x41, 0x42, 0x43}
	pk, cov, err := decodeUniqueIndexValue(val)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pk) != 0 {
		t.Errorf("pk: got %x want empty", pk)
	}
	if !bytes.Equal(cov, []byte{0x41, 0x42, 0x43}) {
		t.Errorf("cov: got %x want [41 42 43]", cov)
	}
}

// --- Persistence across Commit ------------------------------------

// TestIndexLookupReturnsExpectedAcrossCommit verifies Lookup works
// after Commit + re-Open.
func TestIndexLookupReturnsExpectedAcrossCommit(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx, true)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		decl := testDecl("by_color", "color")
		decl.Unique = true
		decl.Extract = firstByteExtract
		ks, err := tx.CreateKeyspace("items", decl)
		if err != nil {
			t.Fatalf("CreateKeyspace: %v", err)
		}
		if err := ks.Put([]byte("k1"), []byte{0x42, 'v'}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		_ = db.Close()
	}
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open #2: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin #2: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Unique = true
	decl.Extract = firstByteExtract
	ks, err := tx.OpenKeyspace("items", decl)
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	idx, _ := ks.Index("by_color")
	pk, value, err := idx.Get([]byte{0x42})
	if err != nil {
		t.Fatalf("Get after re-open: %v", err)
	}
	if string(pk) != "k1" {
		t.Errorf("post-reopen pk: got %q want k1", pk)
	}
	if !bytes.Equal(value, []byte{0x42, 'v'}) {
		t.Errorf("post-reopen value: got %x want [42 76]", value)
	}
}

// helper
func byteSlicesString(s [][]byte) string {
	parts := make([]string, len(s))
	for i, b := range s {
		parts[i] = string(b)
	}
	return "[" + stringJoinList(parts) + "]"
}

func stringJoinList(parts []string) string {
	return strings.Join(parts, ", ")
}
