package gmdb

import (
	"bytes"
	"context"
	"errors"
	"github.com/thegrumpylion/gmdb/internal/indexing"
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
	tx, err := db.Begin(ctx)
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
	tx, err := db.Begin(ctx)
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
// per the non-unique index-key encoding.
func TestIndexLookupNonUniqueReturnsAllMatches(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
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
	tx, err := db.Begin(ctx)
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
	tx, err := db.Begin(ctx)
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
	tx, err := db.Begin(ctx)
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
	tx, err := db.Begin(ctx)
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
	tx, err := db.Begin(ctx)
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
	tx, err := db.Begin(ctx)
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
	tx, err := db.Begin(ctx)
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
	tx, err := db.Begin(ctx)
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

// --- Regression: partial-cols validation ---------------------------

// TestIndexLookupPartialColsReturnsErrInvalidOptions verifies
// that Lookup with fewer columns than the
// index declares sets idx.Err() to a wrapped ErrInvalidOptions
// and yields nothing. Use Prefix for partial-cols semantics.
func TestIndexLookupPartialColsReturnsErrInvalidOptions(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
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

	// Zero cols — same outcome.
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

// TestIndexGetPartialColsReturnsErrInvalidOptions mirrors the
// partial-cols rejection for Get.
func TestIndexGetPartialColsReturnsErrInvalidOptions(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
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

// --- Regression: per-sequence Err reset ----------------------------

// TestIndexLookupResetsErrOnNewSequence verifies that
// a fresh Lookup/Range/Prefix call resets idx.Err()
// (per api-surface.md "first error encountered during the **last**
// sequence's iteration").
func TestIndexLookupResetsErrOnNewSequence(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
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

// --- Regression: covering write path -------------------------------

// TestIndexedPutWritesCoveringBytes verifies that the
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
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := &IndexDecl{
		Name:     "by_color",
		Columns:  []IndexColumn{{Name: "color"}},
		Covering: []IndexCoveringColumn{{Name: "size"}},
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
	encodedIndexKey := indexing.EncodeKey([][]byte{{0x42}})
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
	// Decoded covering should be indexing.EncodeKey([{0xAB}]) =
	// 0xAB 0x00 0x00.
	wantCov := []byte{0xAB, 0x00, 0x00}
	if !bytes.Equal(encodedCov, wantCov) {
		t.Errorf("encoded covering: got %x want %x", encodedCov, wantCov)
	}
}

// --- Byte-API covering return (indexing.md §Covering Indexes) ---

// TestByteAPIUniqueCoveringLookupReturnsCovering verifies the spec
// promise (indexing.md §Covering Indexes): for a byte-API index
// declaring Covering, Lookup returns the encoded covering blob —
// NOT the row value via back-lookup. The row value and the
// covering content are deliberately distinct, so a regression
// reverting to back-lookup would yield the row value and fail
// this assertion.
func TestByteAPIUniqueCoveringLookupReturnsCovering(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := &IndexDecl{
		Name:     "by_color",
		Columns:  []IndexColumn{{Name: "color"}},
		Covering: []IndexCoveringColumn{{Name: "size"}},
		Unique:   true,
		Extract: func(_, value []byte) []IndexEntry {
			if len(value) < 2 {
				return nil
			}
			// Cover = [value[1]]; deliberately distinct from row
			// value (which is value[0..]) so back-lookup vs covering
			// return are observably different.
			return []IndexEntry{{
				Cols:  [][]byte{{value[0]}},
				Cover: [][]byte{{value[1]}},
			}}
		},
	}
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	rowValue := []byte{0x42, 0xAB, 0xCD, 0xEF}
	if err := ks.Put([]byte("k1"), rowValue); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	var gotPK, gotVal []byte
	count := 0
	for pk, v := range idx.Lookup([]byte{0x42}) {
		gotPK = append([]byte(nil), pk...)
		gotVal = append([]byte(nil), v...)
		count++
	}
	if err := idx.Err(); err != nil {
		t.Fatalf("Lookup Err: %v", err)
	}
	if count != 1 {
		t.Fatalf("Lookup count: got %d want 1", count)
	}
	if !bytes.Equal(gotPK, []byte("k1")) {
		t.Errorf("pk: got %q want k1", gotPK)
	}
	// The byte-API contract: returned value is the encoded covering
	// tuple. Cover=[[0xAB]] ⇒ indexing.EncodeKey([[0xAB]]) = 0xAB 0x00 0x00.
	wantEncoded := []byte{0xAB, 0x00, 0x00}
	if !bytes.Equal(gotVal, wantEncoded) {
		t.Errorf("Lookup value: got %x want %x (expected encoded covering tuple; got the row value via back-lookup?)",
			gotVal, wantEncoded)
	}
	// And DecodeCoveringTuple round-trips to the extractor's Cover.
	decoded, err := DecodeCoveringTuple(gotVal)
	if err != nil {
		t.Fatalf("DecodeCoveringTuple: %v", err)
	}
	if len(decoded) != 1 || !bytes.Equal(decoded[0], []byte{0xAB}) {
		t.Errorf("decoded covering: got %x want [[0xAB]]", decoded)
	}
	// Sanity: the returned value is NOT the row value (would imply
	// back-lookup regression).
	if bytes.Equal(gotVal, rowValue) {
		t.Errorf("Lookup returned row value %x; expected covering tuple, not back-lookup", gotVal)
	}
}

// TestByteAPIUniqueCoveringMultiColumnRoundTrip stresses the
// NUL-escape codec on the byte-API covering return path: two
// covering columns with embedded 0x00 bytes round-trip through
// DecodeCoveringTuple.
func TestByteAPIUniqueCoveringMultiColumnRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	// Cover columns deliberately include 0x00 bytes to stress NUL
	// escaping; deliberately distinct from the row value bytes.
	want := [][]byte{
		{0xCC, 0x00, 0xDD},
		{0x00, 0xEE},
	}
	decl := &IndexDecl{
		Name:     "by_lead",
		Columns:  []IndexColumn{{Name: "lead"}},
		Covering: []IndexCoveringColumn{{Name: "a"}, {Name: "b"}},
		Unique:   true,
		Extract: func(_, value []byte) []IndexEntry {
			if len(value) < 1 {
				return nil
			}
			return []IndexEntry{{
				Cols:  [][]byte{{value[0]}},
				Cover: want,
			}}
		},
	}
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("k1"), []byte{0x42, 0xFF, 0xFF}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := ks.Index("by_lead")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	var gotVal []byte
	for _, v := range idx.Lookup([]byte{0x42}) {
		gotVal = append([]byte(nil), v...)
	}
	if err := idx.Err(); err != nil {
		t.Fatalf("Lookup Err: %v", err)
	}
	if len(gotVal) == 0 {
		t.Fatal("Lookup yielded no rows")
	}
	// Belt-and-suspenders: assert the raw blob equals
	// indexing.EncodeKey(want) before decoding. A hypothetical bug
	// returning wrong bytes that happen to decode correctly would
	// pass the per-column assertion below but fail this one.
	expectedBlob := indexing.EncodeKey(want)
	if !bytes.Equal(gotVal, expectedBlob) {
		t.Errorf("raw covering blob: got %x want %x", gotVal, expectedBlob)
	}
	decoded, err := DecodeCoveringTuple(gotVal)
	if err != nil {
		t.Fatalf("DecodeCoveringTuple: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("decoded covering columns: got %d want 2", len(decoded))
	}
	for i, w := range want {
		if !bytes.Equal(decoded[i], w) {
			t.Errorf("col %d: got %x want %x", i, decoded[i], w)
		}
	}
}

// TestByteAPINonUniqueCoveringLookupReturnsCovering verifies the
// non-unique path also returns covering bytes — the entry value
// for a non-unique index IS the covering blob (no PK prefix), and
// the byte-API return contract applies identically.
func TestByteAPINonUniqueCoveringLookupReturnsCovering(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := &IndexDecl{
		Name:     "by_color",
		Columns:  []IndexColumn{{Name: "color"}},
		Covering: []IndexCoveringColumn{{Name: "size"}},
		Unique:   false,
		Extract: func(_, value []byte) []IndexEntry {
			if len(value) < 2 {
				return nil
			}
			return []IndexEntry{{
				Cols:  [][]byte{{value[0]}},
				Cover: [][]byte{{value[1]}},
			}}
		},
	}
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// Two rows sharing the same indexed color — non-unique.
	if err := ks.Put([]byte("k1"), []byte{0x42, 0xAB}); err != nil {
		t.Fatalf("Put k1: %v", err)
	}
	if err := ks.Put([]byte("k2"), []byte{0x42, 0xCD}); err != nil {
		t.Fatalf("Put k2: %v", err)
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	got := map[string][]byte{}
	for pk, v := range idx.Lookup([]byte{0x42}) {
		got[string(pk)] = append([]byte(nil), v...)
	}
	if err := idx.Err(); err != nil {
		t.Fatalf("Lookup Err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Lookup count: got %d want 2", len(got))
	}
	// Per-row covering tuple should match the extractor's Cover bytes.
	if !bytes.Equal(got["k1"], []byte{0xAB, 0x00, 0x00}) {
		t.Errorf("k1 covering: got %x want [AB 00 00]", got["k1"])
	}
	if !bytes.Equal(got["k2"], []byte{0xCD, 0x00, 0x00}) {
		t.Errorf("k2 covering: got %x want [CD 00 00]", got["k2"])
	}
}

// TestByteAPICoveringGetReturnsCovering verifies Get() (unique-
// shorthand) goes through the same byte-API covering path as
// Lookup.
func TestByteAPICoveringGetReturnsCovering(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := &IndexDecl{
		Name:     "by_color",
		Columns:  []IndexColumn{{Name: "color"}},
		Covering: []IndexCoveringColumn{{Name: "size"}},
		Unique:   true,
		Extract: func(_, value []byte) []IndexEntry {
			if len(value) < 2 {
				return nil
			}
			return []IndexEntry{{
				Cols:  [][]byte{{value[0]}},
				Cover: [][]byte{{value[1]}},
			}}
		},
	}
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("k1"), []byte{0x42, 0xAB, 0xCD}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	gotPK, gotVal, err := idx.Get([]byte{0x42})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(gotPK, []byte("k1")) {
		t.Errorf("pk: got %q want k1", gotPK)
	}
	if !bytes.Equal(gotVal, []byte{0xAB, 0x00, 0x00}) {
		t.Errorf("Get covering: got %x want [AB 00 00] (back-lookup regression?)", gotVal)
	}
}

// TestByteAPICoveringRangeReturnsCovering verifies Range() goes
// through the same byte-API covering path (single extractPKAndValue
// call site is shared, but the cursor walk vs single-Get probe
// is a distinct code path).
func TestByteAPICoveringRangeReturnsCovering(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := &IndexDecl{
		Name:     "by_color",
		Columns:  []IndexColumn{{Name: "color"}},
		Covering: []IndexCoveringColumn{{Name: "size"}},
		Unique:   true,
		Extract: func(_, value []byte) []IndexEntry {
			if len(value) < 2 {
				return nil
			}
			return []IndexEntry{{
				Cols:  [][]byte{{value[0]}},
				Cover: [][]byte{{value[1]}},
			}}
		},
	}
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("k1"), []byte{0x10, 0xAA}); err != nil {
		t.Fatalf("Put k1: %v", err)
	}
	if err := ks.Put([]byte("k2"), []byte{0x20, 0xBB}); err != nil {
		t.Fatalf("Put k2: %v", err)
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	got := map[string][]byte{}
	for pk, v := range idx.Range([][]byte{{0x10}}, [][]byte{{0x30}}) {
		got[string(pk)] = append([]byte(nil), v...)
	}
	if err := idx.Err(); err != nil {
		t.Fatalf("Range Err: %v", err)
	}
	if !bytes.Equal(got["k1"], []byte{0xAA, 0x00, 0x00}) {
		t.Errorf("k1 covering: got %x want [AA 00 00]", got["k1"])
	}
	if !bytes.Equal(got["k2"], []byte{0xBB, 0x00, 0x00}) {
		t.Errorf("k2 covering: got %x want [BB 00 00]", got["k2"])
	}
}

// TestNonCoveringLookupStillBackLookupsRowValue is the negative
// control: an index without Covering must continue to back-lookup
// the row value. Guards against an over-broad fix that
// accidentally short-circuits the back-lookup for non-covering
// indexes.
func TestNonCoveringLookupStillBackLookupsRowValue(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
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
	rowValue := []byte{0x42, 0xAB, 0xCD, 0xEF}
	if err := ks.Put([]byte("k1"), rowValue); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	gotPK, gotVal, err := idx.Get([]byte{0x42})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(gotPK, []byte("k1")) {
		t.Errorf("pk: got %q want k1", gotPK)
	}
	if !bytes.Equal(gotVal, rowValue) {
		t.Errorf("non-covering Get value: got %x want %x (back-lookup regression)", gotVal, rowValue)
	}
}

// TestDecodeCoveringTupleRejectsMalformed verifies the public
// decoder wraps malformed-input errors in
// ErrCoveringTupleMalformed — a neutral sentinel distinct from
// ErrCorrupted, since at the byte-stream level the decoder cannot
// distinguish on-disk corruption from caller misuse (applying it
// to non-covering Lookup bytes). Authoritative corruption
// diagnosis is via Check().
func TestDecodeCoveringTupleRejectsMalformed(t *testing.T) {
	// Lone 0x00 at end: no terminator, no escape byte.
	_, err := DecodeCoveringTuple([]byte{0xAA, 0x00})
	if err == nil {
		t.Fatal("expected error for malformed input, got nil")
	}
	if !errors.Is(err, ErrCoveringTupleMalformed) {
		t.Errorf("error: got %v, want wrapping ErrCoveringTupleMalformed", err)
	}
	// And NOT wrapping ErrCorrupted — the neutral-sentinel
	// invariant: ErrCorrupted is the engine's "I found corruption"
	// signal, not the public decoder's malformed-input class.
	if errors.Is(err, ErrCorrupted) {
		t.Errorf("error: %v wraps ErrCorrupted; neutral sentinel was the design choice", err)
	}
}

// TestDecodeCoveringTupleAcceptsEmpty verifies an empty input
// (the engine stores empty covering for an extractor producing
// no Cover bytes) decodes to nil without error.
func TestDecodeCoveringTupleAcceptsEmpty(t *testing.T) {
	cols, err := DecodeCoveringTuple(nil)
	if err != nil {
		t.Fatalf("DecodeCoveringTuple(nil): %v", err)
	}
	if cols != nil {
		t.Errorf("cols: got %v want nil", cols)
	}
}

// TestByteAPICoveringNilCoverReturnsEmpty verifies the spec
// statement (indexing.md §Covering Indexes): when the extractor
// returns Cover=nil despite the IndexDecl declaring Covering,
// Lookup yields an empty value. Engine round-trip, not just a
// decoder-level check.
func TestByteAPICoveringNilCoverReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := &IndexDecl{
		Name:     "by_color",
		Columns:  []IndexColumn{{Name: "color"}},
		Covering: []IndexCoveringColumn{{Name: "size"}}, // declared, but...
		Unique:   true,
		Extract: func(_, value []byte) []IndexEntry {
			if len(value) < 1 {
				return nil
			}
			// ... extractor returns nil Cover despite Covering being
			// declared (programmer error per the IndexEntry contract,
			// but the spec specifies the read shape: empty value).
			return []IndexEntry{{
				Cols:  [][]byte{{value[0]}},
				Cover: nil,
			}}
		},
	}
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	rowValue := []byte{0x42, 0xAB, 0xCD}
	if err := ks.Put([]byte("k1"), rowValue); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	gotPK, gotVal, err := idx.Get([]byte{0x42})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(gotPK, []byte("k1")) {
		t.Errorf("pk: got %q want k1", gotPK)
	}
	// Spec: "stored as empty bytes and Lookup returns an empty value"
	if len(gotVal) != 0 {
		t.Errorf("Get value: got %x (len %d) want empty (len 0); back-lookup regression?",
			gotVal, len(gotVal))
	}
	// Sanity: empty bytes decode to nil cols via DecodeCoveringTuple.
	cols, decErr := DecodeCoveringTuple(gotVal)
	if decErr != nil {
		t.Errorf("DecodeCoveringTuple(empty): %v", decErr)
	}
	if cols != nil {
		t.Errorf("decoded: got %v want nil (zero-tuple)", cols)
	}
}

// --- Regression: decodeUniqueIndexValue errors ---------------------

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
		tx, err := db.Begin(ctx)
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
	tx, err := db.Begin(ctx)
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

// TestIndexRangeArityValidation pins the Range tuple-arity check
// (api-surface.md §Index Lookup API; matches Lookup / LookupKeys /
// Prefix): a bound with more columns than the index declares can
// never match the encoding and must surface ErrInvalidOptions, not
// a silent empty range.
func TestIndexRangeArityValidation(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("t", &IndexDecl{
		Name:    "by_c",
		Columns: []IndexColumn{{Name: "c"}},
		Extract: func(_, value []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{value[:1]}}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Put([]byte("k"), []byte("a")); err != nil {
		t.Fatal(err)
	}
	h, err := ks.Index("by_c")
	if err != nil {
		t.Fatal(err)
	}
	for range h.Range([][]byte{[]byte("a"), []byte("extra")}, nil) {
		t.Fatal("over-arity start yielded")
	}
	if err := h.Err(); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("over-arity start: Err=%v, want ErrInvalidOptions", err)
	}
	for range h.Range(nil, [][]byte{[]byte("a"), []byte("extra")}) {
		t.Fatal("over-arity end yielded")
	}
	if err := h.Err(); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("over-arity end: Err=%v, want ErrInvalidOptions", err)
	}
	// Fewer columns = prefix semantics, still valid.
	n := 0
	for range h.Range([][]byte{}, nil) {
		n++
	}
	if err := h.Err(); err != nil {
		t.Errorf("empty-tuple Range: %v", err)
	}
	if n != 1 {
		t.Errorf("empty-tuple Range yielded %d, want 1", n)
	}
}
