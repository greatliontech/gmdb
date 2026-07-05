package gmdb

import (
	"context"
	"testing"
)

// Covering payloads are extracted from the ROW VALUE; replacing the
// value while the index key stays unchanged must rewrite the stored
// covering entry (indexing.md §Covering Indexes). The pre-fix diff
// compared keys only, so lookups served the stale covering forever
// while Check(CheckIndexes) reported FingerprintDrift — the audit's
// H-severity reproducers, ported here.

// TestCoveringValueRewrittenOnUpdate: typed CoverValue index — the
// row value changes, the index key does not.
func TestCoveringValueRewrittenOnUpdate(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	firstLetter := func(k uint64, v string) []string {
		if len(v) == 0 {
			return nil
		}
		return []string{v[:1]}
	}
	tks := NewTypedKeyspace[uint64, string]("users", Uint64Encoder{}, StringEncoder{})
	idx := &TypedIndex[uint64, string, string]{
		Name: "by_first", IKEnc: StringEncoder{}, Extract: firstLetter, CoverValue: true,
	}
	ks, err := tks.Create(tx, idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Put(1, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := ks.Put(1, "anna"); err != nil { // same IK "a", new value
		t.Fatal(err)
	}
	h, err := ks.Index("by_first")
	if err != nil {
		t.Fatal(err)
	}
	q := NewTypedIndexQuery[uint64, string, string](h, StringEncoder{})
	got := map[uint64]string{}
	for k, v := range q.Lookup("a") {
		got[k] = v
	}
	if err := q.Err(); err != nil {
		t.Fatal(err)
	}
	if got[1] != "anna" {
		t.Errorf("covering Lookup after update = %q, want %q (stale covering value)", got[1], "anna")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Structural pass (the typed layer owns its synthesized decl, so
	// extractor-equivalence runs in the byte-API test below instead).
	for iss := range db.Check() {
		t.Errorf("Check issue: %+v", iss)
	}
}

func assertCheckIndexesClean(t *testing.T, db *DB, ks string, decls ...*IndexDecl) {
	t.Helper()
	for iss := range db.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{ks: decls},
	}) {
		t.Errorf("CheckIndexes issue: %+v", iss)
	}
}

// TestByteCoveringRewrittenOnUpdate: byte-API Cover columns change
// while Cols stay fixed.
func TestByteCoveringRewrittenOnUpdate(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	extract := func(key, value []byte) []IndexEntry {
		return []IndexEntry{{
			Cols:  [][]byte{value[:1]},
			Cover: [][]byte{value[2:]},
		}}
	}
	decl := &IndexDecl{
		Name:     "by_c",
		Columns:  []IndexColumn{{Name: "c"}},
		Covering: []IndexCoveringColumn{{Name: "cov"}},
		Extract:  extract,
	}
	ks, err := tx.CreateKeyspace("t", decl)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Put([]byte("k1"), []byte("a:old")); err != nil {
		t.Fatal(err)
	}
	if err := ks.Put([]byte("k1"), []byte("a:new")); err != nil {
		t.Fatal(err)
	}
	h, err := ks.Index("by_c")
	if err != nil {
		t.Fatal(err)
	}
	var cover string
	for _, v := range h.Lookup([]byte("a")) {
		cols, err := DecodeCoveringTuple(v)
		if err != nil {
			t.Fatal(err)
		}
		cover = string(cols[0])
	}
	if err := h.Err(); err != nil {
		t.Fatal(err)
	}
	if cover != "new" {
		t.Errorf("byte covering Lookup after update = %q, want %q (stale covering value)", cover, "new")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Row+index byte agreement: a rewrite of the WRONG bytes would
	// surface as FingerprintDrift here even with the lookup passing.
	assertCheckIndexesClean(t, db, "t", decl)
}

// TestRebuildMatchesLiveDuplicateCollapse pins the last-wins set
// semantic across the live path and RebuildIndex: an extractor
// emitting two entries with the same encoded key but different
// covering payloads must leave a rebuilt index byte-identical to the
// live-maintained one (first-wins in rebuild diverged the covering
// and produced FingerprintDrift false positives).
func TestRebuildMatchesLiveDuplicateCollapse(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	// Two entries, same Cols (same encoded key), different Cover.
	extract := func(key, value []byte) []IndexEntry {
		return []IndexEntry{
			{Cols: [][]byte{{'x'}}, Cover: [][]byte{[]byte("first")}},
			{Cols: [][]byte{{'x'}}, Cover: [][]byte{[]byte("last")}},
		}
	}
	decl := &IndexDecl{
		Name:     "dup",
		Columns:  []IndexColumn{{Name: "c"}},
		Covering: []IndexCoveringColumn{{Name: "cov"}},
		Extract:  extract,
	}
	ks, err := tx.CreateKeyspace("t", decl)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Put([]byte("k1"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	readCover := func() string {
		h, err := ks.Index("dup")
		if err != nil {
			t.Fatal(err)
		}
		var cover string
		for _, v := range h.Lookup([]byte{'x'}) {
			cols, err := DecodeCoveringTuple(v)
			if err != nil {
				t.Fatal(err)
			}
			cover = string(cols[0])
		}
		if err := h.Err(); err != nil {
			t.Fatal(err)
		}
		return cover
	}
	if got := readCover(); got != "last" {
		t.Fatalf("live-maintained duplicate collapse = %q, want %q (last-wins)", got, "last")
	}
	if err := tx.Indexes().Rebuild("t", decl); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	if got := readCover(); got != "last" {
		t.Errorf("rebuilt duplicate collapse = %q, want %q (must match the live path)", got, "last")
	}
}
