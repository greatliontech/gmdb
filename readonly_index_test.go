package gmdb

import (
	"context"
	"errors"
	"testing"
)

// Read-only opens never loaded declared indexes, so the spec'd
// backup/inspector pattern (indexing.md §Open Semantics: "Index
// lookups still work — they read stored index entries directly")
// was unreachable: ks.Index(name) returned ErrIndexNotFound for
// every declared index (audit M1, auditor reproducer ported). RO
// opens now synthesize extractor-less pinned decls from the on-disk
// registry.

func seedIndexedFixture(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	extract := func(key, value []byte) []IndexEntry {
		return []IndexEntry{{Cols: [][]byte{value[:1]}, Cover: [][]byte{value[1:]}}}
	}
	extract2 := func(key, value []byte) []IndexEntry {
		return []IndexEntry{{Cols: [][]byte{value[:1]}}}
	}
	decl := &IndexDecl{
		Name:     "by_c",
		Columns:  []IndexColumn{{Name: "c"}},
		Covering: []IndexCoveringColumn{{Name: "rest"}},
		Extract:  extract,
	}
	setExtract := func(setKey, member []byte) []IndexEntry {
		return []IndexEntry{{Cols: [][]byte{member[:1]}}}
	}
	setDecl := &IndexDecl{Name: "by_m", Columns: []IndexColumn{{Name: "m"}}, Extract: setExtract}
	// Non-covering (back-lookup) and unique variants — the RO decl
	// synthesis must serve both from persisted fields alone.
	plainDecl := &IndexDecl{
		Name:    "plain",
		Columns: []IndexColumn{{Name: "c"}},
		Extract: extract2,
	}
	uniqDecl := &IndexDecl{
		Name:    "uniq",
		Columns: []IndexColumn{{Name: "u"}},
		Unique:  true,
		Extract: func(key, value []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{key}}}
		},
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := tx.CreateKeyspace("t", decl, plainDecl, uniqDecl)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Put([]byte("k1"), []byte("abc")); err != nil {
		t.Fatal(err)
	}
	sks, err := tx.CreateSetKeyspace("s", nil, setDecl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sks.Put([]byte("set"), []byte("member")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestReadOnlyOpenIndexLookup: the write-tx RO surface, both kinds,
// including a covering index (the synthesized decl must carry the
// persisted Covering schema for DecodeCoveringTuple round-trips).
func TestReadOnlyOpenIndexLookup(t *testing.T) {
	ctx := context.Background()
	db := seedIndexedFixture(t)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	roks, err := tx.OpenKeyspaceReadOnly("t")
	if err != nil {
		t.Fatal(err)
	}
	h, err := roks.Index("by_c")
	if err != nil {
		t.Fatalf("Index on read-only keyspace: %v (indexing.md §Open Semantics)", err)
	}
	n, cover := 0, ""
	for _, v := range h.Lookup([]byte("a")) {
		n++
		cols, derr := DecodeCoveringTuple(v)
		if derr != nil {
			t.Fatal(derr)
		}
		cover = string(cols[0])
	}
	if err := h.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 1 || cover != "bc" {
		t.Errorf("RO covering Lookup = %d rows, cover %q; want 1, \"bc\"", n, cover)
	}
	st, err := h.Stats()
	if err != nil {
		t.Fatalf("RO Stats: %v", err)
	}
	if st.Entries != 1 {
		t.Errorf("RO Stats.Entries = %d, want 1", st.Entries)
	}

	rosks, err := tx.OpenSetKeyspaceReadOnly("s")
	if err != nil {
		t.Fatal(err)
	}
	sh, err := rosks.Index("by_m")
	if err != nil {
		t.Fatalf("Index on read-only set keyspace: %v", err)
	}
	sn := 0
	for range sh.Lookup([]byte("m")) {
		sn++
	}
	if err := sh.Err(); err != nil {
		t.Fatal(err)
	}
	if sn != 1 {
		t.Errorf("RO set index Lookup = %d, want 1", sn)
	}
	// Non-covering index: the back-lookup path returns the ROW value
	// through the same RO keyspace.
	ph, err := roks.Index("plain")
	if err != nil {
		t.Fatalf("Index(plain): %v", err)
	}
	var rowVal string
	for _, v := range ph.Lookup([]byte("a")) {
		rowVal = string(v)
	}
	if err := ph.Err(); err != nil {
		t.Fatal(err)
	}
	if rowVal != "abc" {
		t.Errorf("RO back-lookup value = %q, want %q", rowVal, "abc")
	}
	// Unique index: value decode needs only the persisted Unique flag.
	uh, err := roks.Index("uniq")
	if err != nil {
		t.Fatalf("Index(uniq): %v", err)
	}
	un := 0
	for k := range uh.Lookup([]byte("k1")) {
		un++
		if string(k) != "k1" {
			t.Errorf("RO unique Lookup key = %q, want k1", k)
		}
	}
	if err := uh.Err(); err != nil {
		t.Fatal(err)
	}
	if un != 1 {
		t.Errorf("RO unique Lookup = %d rows, want 1", un)
	}
	// Unknown index still errors.
	if _, err := roks.Index("nope"); !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("Index(nope) = %v, want ErrIndexNotFound", err)
	}
}

// TestReadTxOpenIndexLookup: the ReadTx surface delegates to the
// same RO open path.
func TestReadTxOpenIndexLookup(t *testing.T) {
	ctx := context.Background()
	db := seedIndexedFixture(t)
	if err := db.View(ctx, func(rtx *ReadTx) error {
		ks, err := rtx.OpenKeyspaceReadOnly("t")
		if err != nil {
			return err
		}
		h, err := ks.Index("by_c")
		if err != nil {
			t.Fatalf("Index on ReadTx keyspace: %v", err)
		}
		n := 0
		for range h.Lookup([]byte("a")) {
			n++
		}
		if err := h.Err(); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("ReadTx index Lookup = %d, want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
