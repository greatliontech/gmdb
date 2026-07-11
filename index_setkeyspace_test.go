package gmdb

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/indexing"

	"github.com/thegrumpylion/gmdb/internal/btree"
)

// setKeyspaceFirstByteExtract emits one IndexEntry whose column is
// the setValue's first byte (or no entry if setValue is empty).
// SetKeyspace extractors receive (setKey, setValue) per indexing.md
// §Indexes on SetKeyspaces.
func setKeyspaceFirstByteExtract(_, setValue []byte) []IndexEntry {
	if len(setValue) == 0 {
		return nil
	}
	return []IndexEntry{{Cols: [][]byte{{setValue[0]}}}}
}

// --- Compound-PK codec roundtrip ----------------------------------

// TestEncodeSetKeyspaceCompoundPKRoundtrip verifies the
// compound PK codec roundtrips through encode + decode.
func TestEncodeSetKeyspaceCompoundPKRoundtrip(t *testing.T) {
	cases := []struct {
		name, sk, sv string
	}{
		{"plain", "user1", "topic_a"},
		{"empty key", "", "topic_a"},
		{"empty value", "user1", ""},
		{"both empty", "", ""},
		{"high bytes", string([]byte{0xFE, 0xFF}), string([]byte{0xFF, 0xFE})},
		{"with NUL bytes", string([]byte{0x00, 0x01}), string([]byte{0x02, 0x00})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			encoded := indexing.EncodeSetCompoundPK([]byte(c.sk), []byte(c.sv))
			sk, sv, err := indexing.DecodeSetCompoundPK(encoded)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !bytes.Equal(sk, []byte(c.sk)) {
				t.Errorf("sk: got %x want %x", sk, []byte(c.sk))
			}
			if !bytes.Equal(sv, []byte(c.sv)) {
				t.Errorf("sv: got %x want %x", sv, []byte(c.sv))
			}
		})
	}
}

// TestSetKeyspaceCompoundPKSeparatorPrefixFree verifies the
// set-keyspace.md Inv-6 invariant: the 0x00 0x01 separator
// is lex-distinct from the column terminator 0x00 0x00 and the
// escape sequence 0x00 0xFF. Inside the escaped halves, every
// 0x00 is escaped to 0x00 0xFF, so the only 0x00 0x01 in the
// compound PK is the separator. (Spec-tier invariant promoted to
// enforced tests.)
func TestSetKeyspaceCompoundPKSeparatorPrefixFree(t *testing.T) {
	// Pathological input: setKey contains a literal 0x00, setValue
	// contains 0xFF + 0x01. These bytes would NOT collide with the
	// 0x00 0x01 separator after escaping.
	sk := []byte{0x00, 0xAB}             // escape: 00 FF AB
	sv := []byte{0xFF, 0x01, 0x00, 0x01} // escape: FF 01 00 FF 01
	encoded := indexing.EncodeSetCompoundPK(sk, sv)
	// Count 0x00 0x01 occurrences in the encoded bytes; should
	// be exactly ONE (the separator).
	count := 0
	for i := 0; i < len(encoded)-1; i++ {
		if encoded[i] == 0x00 && encoded[i+1] == 0x01 {
			count++
		}
	}
	if count != 1 {
		t.Errorf("0x00 0x01 separator count: got %d want 1; encoded=%x", count, encoded)
	}
	// Roundtrip preserves both halves.
	gotSK, gotSV, err := indexing.DecodeSetCompoundPK(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(gotSK, sk) {
		t.Errorf("sk roundtrip: got %x want %x", gotSK, sk)
	}
	if !bytes.Equal(gotSV, sv) {
		t.Errorf("sv roundtrip: got %x want %x", gotSV, sv)
	}
}

// TestEncodeSetKeyspaceIndexKeyNonUniqueShape verifies the
// non-unique key shape: indexing.EncodeKey(cols) || compoundPK || 0x00 0x00.
func TestEncodeSetKeyspaceIndexKeyNonUniqueShape(t *testing.T) {
	cols := [][]byte{{0x42}}
	sk := []byte("user1")
	sv := []byte("topic_a")
	got := indexing.EncodeSetEntryKey(cols, sk, sv, false)
	// Expected: indexing.EncodeKey([{0x42}]) + compoundPK + 00 00
	wantPrefix := indexing.EncodeKey(cols)
	wantCompound := indexing.EncodeSetCompoundPK(sk, sv)
	want := make([]byte, 0, len(wantPrefix)+len(wantCompound)+2)
	want = append(want, wantPrefix...)
	want = append(want, wantCompound...)
	want = append(want, 0x00, 0x00)
	if !bytes.Equal(got, want) {
		t.Errorf("non-unique key shape: got %x want %x", got, want)
	}
}

// TestEncodeSetKeyspaceIndexKeyUniqueShape verifies that for
// unique indexes, the key is just indexing.EncodeKey(cols) — compound
// PK lives in the value.
func TestEncodeSetKeyspaceIndexKeyUniqueShape(t *testing.T) {
	cols := [][]byte{{0x42}}
	got := indexing.EncodeSetEntryKey(cols, []byte("any"), []byte("any"), true)
	want := indexing.EncodeKey(cols)
	if !bytes.Equal(got, want) {
		t.Errorf("unique key shape: got %x want %x", got, want)
	}
}

// --- SetKeyspace indexed Put -------------------------------------

// TestSetKeyspaceIndexedPutAddsIndexEntries verifies the
// atomic Put: each added (key, value) pair invokes the extractor
// and writes index entries.
func TestSetKeyspaceIndexedPutAddsIndexEntries(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_topic", "topic")
	decl.Extract = setKeyspaceFirstByteExtract
	sks, err := tx.CreateSetKeyspace("subs", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	// Three (user, topic) pairs sharing topic prefix 'a'.
	if _, err := sks.Put([]byte("u1"), []byte("alpha")); err != nil {
		t.Fatalf("Put u1/alpha: %v", err)
	}
	if _, err := sks.Put([]byte("u2"), []byte("apple")); err != nil {
		t.Fatalf("Put u2/apple: %v", err)
	}
	if _, err := sks.Put([]byte("u1"), []byte("bee")); err != nil { // different first byte
		t.Fatalf("Put u1/bee: %v", err)
	}
	p := sks.indexes["by_topic"]
	if p.count != 3 {
		t.Errorf("post-Put count: got %d want 3", p.count)
	}
	if p.root == 0 {
		t.Errorf("post-Put root: still 0")
	}
}

// TestSetKeyspaceIndexedPutDuplicateNoOp verifies that a Put of an
// already-present (key, value) pair is a no-op for the index too
// (added=false, no index entry written).
func TestSetKeyspaceIndexedPutDuplicateNoOp(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_topic", "topic")
	decl.Extract = setKeyspaceFirstByteExtract
	sks, err := tx.CreateSetKeyspace("subs", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if added, err := sks.Put([]byte("u1"), []byte("alpha")); err != nil || !added {
		t.Fatalf("first Put: added=%v err=%v", added, err)
	}
	if added, err := sks.Put([]byte("u1"), []byte("alpha")); err != nil || added {
		t.Errorf("duplicate Put: added=%v err=%v want added=false err=nil", added, err)
	}
	if sks.indexes["by_topic"].count != 1 {
		t.Errorf("count after duplicate Put: got %d want 1", sks.indexes["by_topic"].count)
	}
}

// TestSetKeyspaceIndexedPutUniqueViolation verifies unique-index
// rejection on a SetKeyspace.
func TestSetKeyspaceIndexedPutUniqueViolation(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_topic", "topic")
	decl.Unique = true
	decl.Extract = setKeyspaceFirstByteExtract
	sks, err := tx.CreateSetKeyspace("subs", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if _, err := sks.Put([]byte("u1"), []byte("alpha")); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	// Different (key, value) but same first byte → unique conflict.
	_, err = sks.Put([]byte("u2"), []byte("apple"))
	if !errors.Is(err, ErrIndexUniqueViolation) {
		t.Errorf("got %v want ErrIndexUniqueViolation", err)
	}
	// The SetKeyspace's row was NOT added.
	has, _ := sks.HasValue([]byte("u2"), []byte("apple"))
	if has {
		t.Errorf("unique-violated Put leaked into the SetKeyspace")
	}
}

// --- SetKeyspace indexed DeleteValue -----------------------------

// TestSetKeyspaceIndexedDeleteValueRemovesIndexEntry verifies that
// DeleteValue on an indexed SetKeyspace also removes the matching
// index entry.
func TestSetKeyspaceIndexedDeleteValueRemovesIndexEntry(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_topic", "topic")
	decl.Extract = setKeyspaceFirstByteExtract
	sks, err := tx.CreateSetKeyspace("subs", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if _, err := sks.Put([]byte("u1"), []byte("alpha")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := sks.DeleteValue([]byte("u1"), []byte("alpha")); err != nil {
		t.Fatalf("DeleteValue: %v", err)
	}
	if sks.indexes["by_topic"].count != 0 {
		t.Errorf("post-DeleteValue count: got %d want 0", sks.indexes["by_topic"].count)
	}
}

// --- SetKeyspace indexed bulk-key Delete -------------------------

// TestSetKeyspaceIndexedDeleteWalksAllMembers verifies the
// indexed bulk-key delete: every set member's index entries
// are removed before the row's leaf cell is dropped. Per
// indexing.md §Indexes on SetKeyspaces "Bulk-free of a key's
// nested B+tree (via Delete(key)) reverts to a per-member walk
// when the SetKeyspace has indexes."
func TestSetKeyspaceIndexedDeleteWalksAllMembers(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_topic", "topic")
	decl.Extract = setKeyspaceFirstByteExtract
	sks, err := tx.CreateSetKeyspace("subs", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	// Three values under one key — all 3 index entries should
	// vanish when the key is bulk-deleted.
	for _, v := range []string{"alpha", "bee", "carrot"} {
		if _, err := sks.Put([]byte("u1"), []byte(v)); err != nil {
			t.Fatalf("Put %q: %v", v, err)
		}
	}
	if sks.indexes["by_topic"].count != 3 {
		t.Fatalf("pre-Delete count: got %d want 3", sks.indexes["by_topic"].count)
	}
	if err := sks.Delete([]byte("u1")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if sks.indexes["by_topic"].count != 0 {
		t.Errorf("post-Delete count: got %d want 0", sks.indexes["by_topic"].count)
	}
}

// --- SetKeyspace.Index handle Lookup -----------------------------

// TestSetKeyspaceIndexLookupReturnsSetKeyValuePair verifies the
// SetKeyspace Lookup contract: yields (setKey, setValue) pairs
// (NOT (rowKey, rowValue) as for Keyspace).
func TestSetKeyspaceIndexLookupReturnsSetKeyValuePair(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_topic", "topic")
	decl.Extract = setKeyspaceFirstByteExtract
	sks, err := tx.CreateSetKeyspace("subs", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if _, err := sks.Put([]byte("u1"), []byte("alpha")); err != nil {
		t.Fatalf("Put u1/alpha: %v", err)
	}
	if _, err := sks.Put([]byte("u2"), []byte("apple")); err != nil {
		t.Fatalf("Put u2/apple: %v", err)
	}
	if _, err := sks.Put([]byte("u3"), []byte("bee")); err != nil { // different first byte
		t.Fatalf("Put u3/bee: %v", err)
	}
	idx, err := sks.Index("by_topic")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	type pair struct {
		setKey, setValue string
	}
	var got []pair
	for sk, sv := range idx.Lookup([]byte{'a'}) {
		got = append(got, pair{string(sk), string(sv)})
	}
	if idx.Err() != nil {
		t.Fatalf("idx.Err: %v", idx.Err())
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].setKey != got[j].setKey {
			return got[i].setKey < got[j].setKey
		}
		return got[i].setValue < got[j].setValue
	})
	want := []pair{{"u1", "alpha"}, {"u2", "apple"}}
	if len(got) != len(want) {
		t.Fatalf("Lookup yielded %d pairs, want %d: got=%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pair %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

// TestSetKeyspaceIndexGetUniqueReturnsSetKeyValue verifies Get on
// a unique SetKeyspace index returns the single (setKey,
// setValue) pair.
func TestSetKeyspaceIndexGetUniqueReturnsSetKeyValue(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_topic", "topic")
	decl.Unique = true
	decl.Extract = setKeyspaceFirstByteExtract
	sks, err := tx.CreateSetKeyspace("subs", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if _, err := sks.Put([]byte("u1"), []byte("alpha")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, _ := sks.Index("by_topic")
	sk, sv, err := idx.Get([]byte{'a'})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(sk) != "u1" || string(sv) != "alpha" {
		t.Errorf("Get: got (%q, %q) want (u1, alpha)", sk, sv)
	}
}

// --- Regression: LookupKeys broken on SetKeyspace ----------------

// TestSetKeyspaceIndexLookupKeysRejected verifies that
// LookupKeys on a SetKeyspace *IndexHandle sets
// idx.Err() to a wrapped ErrInvalidOptions and yields nothing.
// LookupKeys' iter.Seq[[]byte] surface cannot represent the
// compound (setKey, setValue) PK; callers use Lookup
// (iter.Seq2) instead.
func TestSetKeyspaceIndexLookupKeysRejected(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_topic", "topic")
	decl.Extract = setKeyspaceFirstByteExtract
	sks, err := tx.CreateSetKeyspace("subs", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if _, err := sks.Put([]byte("u1"), []byte("alpha")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, _ := sks.Index("by_topic")
	n := 0
	for range idx.LookupKeys([]byte{'a'}) {
		n++
	}
	if n != 0 {
		t.Errorf("LookupKeys yielded %d entries on SetKeyspace; want 0 (gated)", n)
	}
	if !errors.Is(idx.Err(), ErrInvalidOptions) {
		t.Errorf("idx.Err: got %v want ErrInvalidOptions", idx.Err())
	}
}

// --- Range + Prefix coverage on SetKeyspace ----------------------

// TestSetKeyspaceIndexRangeYieldsSetKeyValuePairs verifies the
// Range surface on a SetKeyspace yields (setKey,
// setValue) pairs in encoded-key order.
func TestSetKeyspaceIndexRangeYieldsSetKeyValuePairs(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_topic", "topic")
	decl.Extract = setKeyspaceFirstByteExtract
	sks, err := tx.CreateSetKeyspace("subs", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	// First-byte tags: 0x41 (A), 0x42 (B), 0x43 (C).
	pairs := []struct{ k, v string }{
		{"u1", "alpha"},  // first byte 0x61 (a)
		{"u2", "beta"},   // first byte 0x62 (b)
		{"u3", "carrot"}, // first byte 0x63 (c)
	}
	for _, p := range pairs {
		if _, err := sks.Put([]byte(p.k), []byte(p.v)); err != nil {
			t.Fatalf("Put %q/%q: %v", p.k, p.v, err)
		}
	}
	idx, _ := sks.Index("by_topic")
	// Range [0x62, 0x63) → only b-prefixed (u2, beta).
	var got []string
	for sk, sv := range idx.Range([][]byte{{0x62}}, [][]byte{{0x63}}) {
		got = append(got, string(sk)+"/"+string(sv))
	}
	if len(got) != 1 || got[0] != "u2/beta" {
		t.Errorf("Range [0x62, 0x63): got %v want [u2/beta]", got)
	}
}

// TestSetKeyspaceIndexPrefixYieldsSetKeyValuePairs verifies the
// Prefix surface on a SetKeyspace.
func TestSetKeyspaceIndexPrefixYieldsSetKeyValuePairs(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	// 2-column index (topic-first-byte, topic-second-byte).
	decl := &IndexDecl{
		Name: "by_topic_bytes",
		Columns: []IndexColumn{
			{Name: "b0"},
			{Name: "b1"},
		},
		Extract: func(_, setValue []byte) []IndexEntry {
			if len(setValue) < 2 {
				return nil
			}
			return []IndexEntry{{Cols: [][]byte{{setValue[0]}, {setValue[1]}}}}
		},
	}
	sks, err := tx.CreateSetKeyspace("subs", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	pairs := []struct{ k, v string }{
		{"u1", "ab"},
		{"u2", "ac"},
		{"u3", "bb"},
	}
	for _, p := range pairs {
		if _, err := sks.Put([]byte(p.k), []byte(p.v)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	idx, _ := sks.Index("by_topic_bytes")
	// Prefix on first byte 'a' (0x61) → u1/ab and u2/ac.
	var got []string
	for sk, sv := range idx.Prefix([]byte{0x61}) {
		got = append(got, string(sk)+"/"+string(sv))
	}
	sort.Strings(got)
	want := []string{"u1/ab", "u2/ac"}
	if len(got) != len(want) {
		t.Fatalf("Prefix yielded %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Prefix[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

// --- Nested-tree promotion path coverage -------------------------

// TestSetKeyspaceIndexedBulkDeleteAcrossNestedTree verifies the
// bulk-key Delete walks every member when the key's set
// has been promoted to a nested B+tree (per set-keyspace.md promotion
// rules). Forces enough Puts to cross the subpage→nested-tree
// promotion threshold.
func TestSetKeyspaceIndexedBulkDeleteAcrossNestedTree(t *testing.T) {
	ctx := context.Background()
	// MaxSize large enough to hold the parent tree + a nested tree
	// + the index data tree + their CoW shadow copies.
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 1024})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_value", "v")
	// Extract returns one entry whose column is the FULL setValue.
	decl.Extract = func(_, setValue []byte) []IndexEntry {
		if len(setValue) == 0 {
			return nil
		}
		return []IndexEntry{{Cols: [][]byte{setValue}}}
	}
	sks, err := tx.CreateSetKeyspace("subs", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	// Put enough distinct values under one key to cross the
	// subpage→nested-tree promotion threshold. The
	// threshold is 50% of the page's usable space (~2020 bytes
	// at PageSize=4096); use 64-byte values so ~32 entries
	// reaches the threshold. 600 entries comfortably crosses.
	const n = 600
	for i := range n {
		v := make([]byte, 64)
		v[0] = byte(i / 256)
		v[1] = byte(i % 256)
		// Fill remaining with deterministic bytes for distinct values.
		for j := 2; j < 64; j++ {
			v[j] = byte((i + j) % 256)
		}
		if _, err := sks.Put([]byte("k"), v); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	if got := sks.indexes["by_value"].count; got != n {
		t.Fatalf("pre-Delete count: got %d want %d", got, n)
	}
	// Verify the cell promoted to nested-tree.
	cfg := sks.builderCfg()
	e, found, err := btree.GetEntry(sks.tx.pgr, cfg, sks.desc.Root, []byte("k"))
	if err != nil || !found {
		t.Fatalf("GetEntry: found=%v err=%v", found, err)
	}
	if !e.IsNestedTree() {
		t.Fatalf("expected nested-tree promotion after %d × 64-byte Puts; cell is still subpage", n)
	}
	// Bulk-key Delete must walk every member of the nested tree
	// and clear all index entries.
	if err := sks.Delete([]byte("k")); err != nil {
		t.Fatalf("Delete (nested-tree path): %v", err)
	}
	if got := sks.indexes["by_value"].count; got != 0 {
		t.Errorf("post-Delete count: got %d want 0 (bulk-key-delete nested-tree walk)", got)
	}
}

// --- Persistence across Commit ----------------------------------

// TestSetKeyspaceIndexedPersistsAcrossCommit verifies that the
// SetKeyspace pinned index state survives Commit + re-Open
// (flushIndexRegistry Step 2b integration).
func TestSetKeyspaceIndexedPersistsAcrossCommit(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, _ := db.Begin(ctx)
		decl := testDecl("by_topic", "topic")
		decl.Extract = setKeyspaceFirstByteExtract
		sks, err := tx.CreateSetKeyspace("subs", nil, decl)
		if err != nil {
			t.Fatalf("CreateSetKeyspace: %v", err)
		}
		for _, p := range [][2]string{{"u1", "alpha"}, {"u2", "bee"}} {
			if _, err := sks.Put([]byte(p[0]), []byte(p[1])); err != nil {
				t.Fatalf("Put: %v", err)
			}
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
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_topic", "topic")
	decl.Extract = setKeyspaceFirstByteExtract
	sks, err := tx.OpenSetKeyspace("subs", decl)
	if err != nil {
		t.Fatalf("OpenSetKeyspace: %v", err)
	}
	if sks.indexes["by_topic"].count != 2 {
		t.Errorf("post-reopen count: got %d want 2", sks.indexes["by_topic"].count)
	}
	idx, _ := sks.Index("by_topic")
	n := 0
	for range idx.Lookup([]byte{'a'}) {
		n++
	}
	if n != 1 {
		t.Errorf("Lookup post-reopen: got %d want 1", n)
	}
}

// --- per-row index-maintenance atomicity (SetKeyspace) ----------
//
// SetKeyspace counterparts of the Keyspace.Put / Delete /
// Cursor.Delete tests in index_maintain_test.go. Same contract
// (transactions.md §Write-helper error contract): after a per-op
// error followed by Tx.Commit, db.Check() reports zero BitmapLeak
// — the caller-site savepoint Restore reverts the
// partial allocations the helper made before the injection.

// setKeyspaceTwoIndexDecls returns two decls with distinct
// extractor outputs so each Put/RemoveValue runs ≥2 btree
// mutations on index data trees, letting the fail hook fire on
// the second after the first allocated pages.
func setKeyspaceTwoIndexDecls() (*IndexDecl, *IndexDecl) {
	da := testDecl("by_a", "a")
	da.Extract = setKeyspaceFirstByteExtract
	db := testDecl("by_b", "b")
	// Distinct extractor so b's index data tree is non-empty and a
	// btree.Put on it allocates separately from a's tree.
	db.Extract = func(_, setValue []byte) []IndexEntry {
		if len(setValue) < 2 {
			return nil
		}
		return []IndexEntry{{Cols: [][]byte{{setValue[1]}}}}
	}
	return da, db
}

func TestApplyIndexMaintenanceAtomicOnSetKeyspacePut(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	{
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin setup: %v", err)
		}
		da, db2 := setKeyspaceTwoIndexDecls()
		// Seed one pair so the index data trees are non-empty;
		// a Put of a new pair then exercises btree.Put on both.
		sks, err := tx.CreateSetKeyspace("subs", nil, da, db2)
		if err != nil {
			tx.Rollback()
			t.Fatalf("CreateSetKeyspace: %v", err)
		}
		if _, err := sks.Put([]byte("u1"), []byte{'x', 'y'}); err != nil {
			tx.Rollback()
			t.Fatalf("Put seed: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit setup: %v", err)
		}
	}

	injected := errors.New("injected index-maintenance failure (SetKeyspace Put)")
	setIndexMaintenanceFailHookForTest(func(i int) error {
		if i >= 0 {
			return injected
		}
		return nil
	})
	t.Cleanup(func() { setIndexMaintenanceFailHookForTest(nil) })

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	da, db2 := setKeyspaceTwoIndexDecls()
	sks, err := tx.OpenSetKeyspace("subs", da, db2)
	if err != nil {
		tx.Rollback()
		t.Fatalf("OpenSetKeyspace: %v", err)
	}
	_, err = sks.Put([]byte("u2"), []byte{'p', 'q'})
	if !errors.Is(err, injected) {
		tx.Rollback()
		t.Fatalf("Put err = %v, want injected", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after injected failure: %v", err)
	}

	assertNoBitmapLeak(t, db)
}

func TestApplyIndexMaintenanceAtomicOnSetKeyspaceDeleteValue(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	{
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin setup: %v", err)
		}
		da, db2 := setKeyspaceTwoIndexDecls()
		sks, err := tx.CreateSetKeyspace("subs", nil, da, db2)
		if err != nil {
			tx.Rollback()
			t.Fatalf("CreateSetKeyspace: %v", err)
		}
		if _, err := sks.Put([]byte("u1"), []byte{'x', 'y'}); err != nil {
			tx.Rollback()
			t.Fatalf("Put seed1: %v", err)
		}
		if _, err := sks.Put([]byte("u1"), []byte{'p', 'q'}); err != nil {
			tx.Rollback()
			t.Fatalf("Put seed2: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit setup: %v", err)
		}
	}

	injected := errors.New("injected index-maintenance failure (SetKeyspace DeleteValue)")
	setIndexMaintenanceFailHookForTest(func(i int) error {
		if i >= 0 {
			return injected
		}
		return nil
	})
	t.Cleanup(func() { setIndexMaintenanceFailHookForTest(nil) })

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	da, db2 := setKeyspaceTwoIndexDecls()
	sks, err := tx.OpenSetKeyspace("subs", da, db2)
	if err != nil {
		tx.Rollback()
		t.Fatalf("OpenSetKeyspace: %v", err)
	}
	err = sks.DeleteValue([]byte("u1"), []byte{'x', 'y'})
	if !errors.Is(err, injected) {
		tx.Rollback()
		t.Fatalf("DeleteValue err = %v, want injected", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after injected failure: %v", err)
	}

	assertNoBitmapLeak(t, db)
}

func TestApplyIndexMaintenanceAtomicOnSetKeyspaceBulkKeyDelete(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	{
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin setup: %v", err)
		}
		da, db2 := setKeyspaceTwoIndexDecls()
		sks, err := tx.CreateSetKeyspace("subs", nil, da, db2)
		if err != nil {
			tx.Rollback()
			t.Fatalf("CreateSetKeyspace: %v", err)
		}
		// Seed two values under the same key so SetKeyspace.Delete
		// walks both via applyIndexMaintenanceOnBulkKeyDelete →
		// applyIndexMaintenanceOnRemoveValue per member.
		if _, err := sks.Put([]byte("u1"), []byte{'x', 'y'}); err != nil {
			tx.Rollback()
			t.Fatalf("Put seed1: %v", err)
		}
		if _, err := sks.Put([]byte("u1"), []byte{'p', 'q'}); err != nil {
			tx.Rollback()
			t.Fatalf("Put seed2: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit setup: %v", err)
		}
	}

	injected := errors.New("injected index-maintenance failure (SetKeyspace Delete bulk-key)")
	setIndexMaintenanceFailHookForTest(func(i int) error {
		if i >= 0 {
			return injected
		}
		return nil
	})
	t.Cleanup(func() { setIndexMaintenanceFailHookForTest(nil) })

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	da, db2 := setKeyspaceTwoIndexDecls()
	sks, err := tx.OpenSetKeyspace("subs", da, db2)
	if err != nil {
		tx.Rollback()
		t.Fatalf("OpenSetKeyspace: %v", err)
	}
	err = sks.Delete([]byte("u1"))
	if !errors.Is(err, injected) {
		tx.Rollback()
		t.Fatalf("Delete err = %v, want injected", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after injected failure: %v", err)
	}

	assertNoBitmapLeak(t, db)
}

// --- Mid-loop pinned-state restore (caller-owned rowSnap) -------

// TestSetKeyspaceBulkDeletePinnedStateRevertsAfterMidLoopFailure
// pins the invariant that SetKeyspace.Delete's outer `rowSnap`
// protects: post-bulk-failure pinned == pre-call pinned. The
// per-member helper applyIndexMaintenanceOnRemoveValue does NOT
// snapshot pinned state; SetKeyspace.Delete's outer rowSnap (taken
// ONCE before the bulk loop) is the sole restore on per-member
// failure.
//
// Concretely: with a 2-member key and 2 indexes, the fail hook fires
// on the first member's first btree.Delete on an index data tree.
// pinned for the first index has been mutated by the helper before
// failure; the outer rowSnap restore must un-mutate.
//
// Historical context (kept-current anchor: this test's neuter clause
// + `git log` on this file): the
// wrapper-internal snapshot an earlier design added was a belt over
// these braces and was removed when the wrapper layer was deleted.
// This test is the regression backstop against future removal of
// the outer SetKeyspace.Delete restoreIndexes call.
//
// Neuter: remove `restoreIndexes(ks.indexes, rowSnap)` from
// SetKeyspace.Delete's helper-error branch (a pre-existing line,
// unchanged in the change set that introduced this test). Test
// fails with idx_a's pinned count decremented below the pre-Delete
// value.
func TestSetKeyspaceBulkDeletePinnedStateRevertsAfterMidLoopFailure(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	{
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin setup: %v", err)
		}
		da, db2 := setKeyspaceTwoIndexDecls()
		sks, err := tx.CreateSetKeyspace("subs", nil, da, db2)
		if err != nil {
			tx.Rollback()
			t.Fatalf("CreateSetKeyspace: %v", err)
		}
		// Seed two values under the same key so SetKeyspace.Delete
		// walks both via applyIndexMaintenanceOnBulkKeyDelete →
		// applyIndexMaintenanceOnRemoveValue per member.
		if _, err := sks.Put([]byte("u1"), []byte{'x', 'y'}); err != nil {
			tx.Rollback()
			t.Fatalf("Put seed1: %v", err)
		}
		if _, err := sks.Put([]byte("u1"), []byte{'p', 'q'}); err != nil {
			tx.Rollback()
			t.Fatalf("Put seed2: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit setup: %v", err)
		}
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	da, db2 := setKeyspaceTwoIndexDecls()
	sks, err := tx.OpenSetKeyspace("subs", da, db2)
	if err != nil {
		t.Fatalf("OpenSetKeyspace: %v", err)
	}

	type pinnedSnapshot struct{ root, count uint64 }
	pre := map[string]pinnedSnapshot{}
	for name, p := range sks.indexes {
		pre[name] = pinnedSnapshot{root: p.root, count: p.count}
	}

	injected := errors.New("injected index-maintenance failure (SetKeyspace bulk delete mid-loop)")
	setIndexMaintenanceFailHookForTest(func(i int) error {
		if i >= 0 {
			return injected
		}
		return nil
	})
	t.Cleanup(func() { setIndexMaintenanceFailHookForTest(nil) })

	err = sks.Delete([]byte("u1"))
	if !errors.Is(err, injected) {
		t.Fatalf("Delete err = %v, want injected", err)
	}

	for name, p := range sks.indexes {
		want := pre[name]
		if p.root != want.root {
			t.Errorf("post-failure pinned[%q].root: got %d want %d — bulk rowSnap restore regression",
				name, p.root, want.root)
		}
		if p.count != want.count {
			t.Errorf("post-failure pinned[%q].count: got %d want %d — bulk rowSnap restore regression",
				name, p.count, want.count)
		}
	}
}

// TestSetKeyspacePutPinnedStateRevertsAfterMidLoopFailure is the
// SetKeyspace.Put counterpart of
// TestIndexedPutPinnedStateRevertsAfterMidLoopFailure. Pins the
// invariant that SetKeyspace.Put's added rowSnap-restore on the
// helper-error branch is the sole atomicity-rollback for in-memory
// pinned state (the helper itself is snapshot-less; see its godoc).
//
// Neuter: remove `restoreIndexes(ks.indexes, rowSnap)` from
// SetKeyspace.Put's helper-error branch. Test fails with the first
// processed index's pinned root mutated forward and count incremented.
func TestSetKeyspacePutPinnedStateRevertsAfterMidLoopFailure(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	{
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin setup: %v", err)
		}
		da, db2 := setKeyspaceTwoIndexDecls()
		// Seed one pair so both index data trees are non-empty and
		// a subsequent Put runs btree.Put on both — the fail hook
		// fires after the first.
		sks, err := tx.CreateSetKeyspace("subs", nil, da, db2)
		if err != nil {
			tx.Rollback()
			t.Fatalf("CreateSetKeyspace: %v", err)
		}
		if _, err := sks.Put([]byte("u1"), []byte{'x', 'y'}); err != nil {
			tx.Rollback()
			t.Fatalf("Put seed: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit setup: %v", err)
		}
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	da, db2 := setKeyspaceTwoIndexDecls()
	sks, err := tx.OpenSetKeyspace("subs", da, db2)
	if err != nil {
		t.Fatalf("OpenSetKeyspace: %v", err)
	}

	type pinnedSnapshot struct{ root, count uint64 }
	pre := map[string]pinnedSnapshot{}
	for name, p := range sks.indexes {
		pre[name] = pinnedSnapshot{root: p.root, count: p.count}
	}

	injected := errors.New("injected index-maintenance failure (SetKeyspace.Put mid-loop)")
	setIndexMaintenanceFailHookForTest(func(i int) error {
		if i >= 0 {
			return injected
		}
		return nil
	})
	t.Cleanup(func() { setIndexMaintenanceFailHookForTest(nil) })

	_, err = sks.Put([]byte("u2"), []byte{'p', 'q'})
	if !errors.Is(err, injected) {
		t.Fatalf("Put err = %v, want injected", err)
	}

	for name, p := range sks.indexes {
		want := pre[name]
		if p.root != want.root {
			t.Errorf("post-failure pinned[%q].root: got %d want %d — caller rowSnap restore regression",
				name, p.root, want.root)
		}
		if p.count != want.count {
			t.Errorf("post-failure pinned[%q].count: got %d want %d — caller rowSnap restore regression",
				name, p.count, want.count)
		}
	}
}

// TestSetKeyspaceDeleteValuePinnedStateRevertsAfterMidLoopFailure
// is the SetKeyspace.DeleteValue counterpart. Pins the invariant
// that DeleteValue's added rowSnap-restore on the helper-error
// branch is the sole atomicity-rollback.
//
// Neuter: remove `restoreIndexes(ks.indexes, rowSnap)` from
// SetKeyspace.DeleteValue's helper-error branch.
func TestSetKeyspaceDeleteValuePinnedStateRevertsAfterMidLoopFailure(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	{
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin setup: %v", err)
		}
		da, db2 := setKeyspaceTwoIndexDecls()
		sks, err := tx.CreateSetKeyspace("subs", nil, da, db2)
		if err != nil {
			tx.Rollback()
			t.Fatalf("CreateSetKeyspace: %v", err)
		}
		// Seed two values so the index data trees have entries to
		// delete (DeleteValue's helper is the per-(setKey,setValue)
		// remove path; needs the index entries present).
		if _, err := sks.Put([]byte("u1"), []byte{'x', 'y'}); err != nil {
			tx.Rollback()
			t.Fatalf("Put seed1: %v", err)
		}
		if _, err := sks.Put([]byte("u2"), []byte{'p', 'q'}); err != nil {
			tx.Rollback()
			t.Fatalf("Put seed2: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit setup: %v", err)
		}
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	da, db2 := setKeyspaceTwoIndexDecls()
	sks, err := tx.OpenSetKeyspace("subs", da, db2)
	if err != nil {
		t.Fatalf("OpenSetKeyspace: %v", err)
	}

	type pinnedSnapshot struct{ root, count uint64 }
	pre := map[string]pinnedSnapshot{}
	for name, p := range sks.indexes {
		pre[name] = pinnedSnapshot{root: p.root, count: p.count}
	}

	injected := errors.New("injected index-maintenance failure (SetKeyspace.DeleteValue mid-loop)")
	setIndexMaintenanceFailHookForTest(func(i int) error {
		if i >= 0 {
			return injected
		}
		return nil
	})
	t.Cleanup(func() { setIndexMaintenanceFailHookForTest(nil) })

	err = sks.DeleteValue([]byte("u1"), []byte{'x', 'y'})
	if !errors.Is(err, injected) {
		t.Fatalf("DeleteValue err = %v, want injected", err)
	}

	for name, p := range sks.indexes {
		want := pre[name]
		if p.root != want.root {
			t.Errorf("post-failure pinned[%q].root: got %d want %d — caller rowSnap restore regression",
				name, p.root, want.root)
		}
		if p.count != want.count {
			t.Errorf("post-failure pinned[%q].count: got %d want %d — caller rowSnap restore regression",
				name, p.count, want.count)
		}
	}
}
