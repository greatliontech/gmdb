package gmdb

import (
	"context"
	"fmt"
	"testing"
)

// The index registry's Root field is a flush-time projection of the
// live pinned root: same-tx index growth (Puts on a cached handle)
// updates pinned.root in memory and syncs the registry only at
// Commit. TxIndexes.Drop / Rebuild must therefore free the LIVE tree,
// not the registry's stale Root — freeing the stale root orphaned
// every page the tree gained this tx (BitmapLeak on the committed
// image; found by the randomized commit-reserve property walk).

func liveRootFixtureDecl() *IndexDecl {
	return &IndexDecl{
		Name:    "ix",
		Columns: []IndexColumn{{Name: "c"}},
		Extract: func(key, value []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{value[:1]}}}
		},
	}
}

// TestDropFreesLiveSameTxIndexTree: create an indexed keyspace, grow
// the index in the same tx, Drop it, Commit — the grown tree's pages
// must be released (Check clean).
func TestDropFreesLiveSameTxIndexTree(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("x", liveRootFixtureDecl())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if err := ks.Put(fmt.Appendf(nil, "k%d", i), []byte("vv")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Indexes().Drop("x", "ix"); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	// A post-drop write keeps the tx realistic (the drop must not
	// have freed live row pages).
	if err := ks.Put([]byte("k9"), []byte("zz")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for iss := range db.Check() {
		t.Errorf("Check after same-tx grow+drop: %+v", iss)
	}
}

// TestRebuildFreesLiveSameTxIndexTree: the Rebuild sibling — the
// same-tx-grown OLD tree must be freed when the rebuild swaps in the
// new one.
func TestRebuildFreesLiveSameTxIndexTree(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("x", liveRootFixtureDecl())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if err := ks.Put(fmt.Appendf(nil, "k%d", i), []byte("vv")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Indexes().Rebuild("x", liveRootFixtureDecl()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// The rebuilt index serves lookups.
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rtx.Rollback()
	rks, err := rtx.OpenKeyspaceReadOnly("x")
	if err != nil {
		t.Fatal(err)
	}
	h, err := rks.Index("ix")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range h.Lookup([][]byte{[]byte("v")}) {
		n++
	}
	if err := h.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("rebuilt index lookup = %d entries, want 5", n)
	}
	if err := rtx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for iss := range db.Check() {
		t.Errorf("Check after same-tx grow+rebuild: %+v", iss)
	}
}

// TestSetDropFreesLiveSameTxIndexTree: Kind=1 variant of the Drop
// shape (the set-member extractor path shares the pinned-root
// machinery).
func TestSetDropFreesLiveSameTxIndexTree(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sdecl := &IndexDecl{
		Name:    "ixm",
		Columns: []IndexColumn{{Name: "m"}},
		Extract: func(setKey, member []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{member[:1]}}}
		},
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	sks, err := tx.CreateSetKeyspace("s", nil, sdecl)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if _, err := sks.Put([]byte("set"), fmt.Appendf(nil, "m%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Indexes().Drop("s", "ixm"); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for iss := range db.Check() {
		t.Errorf("Check after same-tx set grow+drop: %+v", iss)
	}
}
