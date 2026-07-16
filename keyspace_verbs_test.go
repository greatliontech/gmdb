package gmdb

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// Keyspace.Insert / Replace (api-surface.md §Keyspace API): insert-only
// and update-only duals of the Put upsert. The load-bearing property
// beyond the sentinels is NO-OP PURITY — a verb whose presence
// requirement fails mutates nothing: no page write, no descriptor
// change (root/count), no cursor invalidation, no index maintenance.

func verbFixture(t *testing.T, indexes ...*IndexDecl) (*DB, *Tx, *Keyspace) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	tx, _ := db.Begin(ctx)
	ks, err := tx.CreateKeyspace("k", indexes...)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	return db, tx, ks
}

func TestInsertReplaceVerbSemantics(t *testing.T) {
	_, _, ks := verbFixture(t)

	// Replace on an empty tree: ErrNotFound, nothing stored.
	if err := ks.Replace([]byte("a"), []byte("v0")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Replace on empty tree = %v, want ErrNotFound", err)
	}
	if n := ks.desc.Count; n != 0 {
		t.Fatalf("Count after no-op Replace = %d, want 0", n)
	}

	// Insert absent: stored.
	if err := ks.Insert([]byte("a"), []byte("v1")); err != nil {
		t.Fatalf("Insert absent: %v", err)
	}
	if v, err := ks.Get([]byte("a")); err != nil || !bytes.Equal(v, []byte("v1")) {
		t.Fatalf("Get after Insert: %q err=%v", v, err)
	}
	if n := ks.desc.Count; n != 1 {
		t.Fatalf("Count after Insert = %d, want 1", n)
	}

	// Insert present: ErrKeyExists, value + root + count unchanged.
	rootBefore := ks.desc.Root
	if err := ks.Insert([]byte("a"), []byte("v2")); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("Insert present = %v, want ErrKeyExists", err)
	}
	if ks.desc.Root != rootBefore {
		t.Fatal("no-op Insert changed the tree root")
	}
	if v, _ := ks.Get([]byte("a")); !bytes.Equal(v, []byte("v1")) {
		t.Fatalf("no-op Insert changed the value: %q", v)
	}
	if n := ks.desc.Count; n != 1 {
		t.Fatalf("Count after no-op Insert = %d, want 1", n)
	}

	// Replace present: replaced, count unchanged.
	if err := ks.Replace([]byte("a"), []byte("v3")); err != nil {
		t.Fatalf("Replace present: %v", err)
	}
	if v, _ := ks.Get([]byte("a")); !bytes.Equal(v, []byte("v3")) {
		t.Fatalf("Get after Replace: %q", v)
	}
	if n := ks.desc.Count; n != 1 {
		t.Fatalf("Count after Replace = %d, want 1", n)
	}

	// Replace absent (non-empty tree): ErrNotFound, pure no-op.
	rootBefore = ks.desc.Root
	if err := ks.Replace([]byte("b"), []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Replace absent = %v, want ErrNotFound", err)
	}
	if ks.desc.Root != rootBefore {
		t.Fatal("no-op Replace changed the tree root")
	}
	if _, err := ks.Get([]byte("b")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no-op Replace stored the key: %v", err)
	}

	// Put stays the upsert.
	if err := ks.Put([]byte("a"), []byte("v4")); err != nil {
		t.Fatalf("Put upsert: %v", err)
	}
	if err := ks.Put([]byte("b"), []byte("v5")); err != nil {
		t.Fatalf("Put insert: %v", err)
	}
	if n := ks.desc.Count; n != 2 {
		t.Fatalf("Count after upserts = %d, want 2", n)
	}
}

// TestVerbNoOpDoesNotInvalidateCursors: a failing verb leaves open
// cursors positioned (Put's markCursorsStale must not run on the
// no-op paths).
func TestVerbNoOpDoesNotInvalidateCursors(t *testing.T) {
	_, _, ks := verbFixture(t)
	for _, k := range []string{"a", "b", "c"} {
		if err := ks.Put([]byte(k), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	c := ks.Cursor()
	if k, _ := c.First(); string(k) != "a" {
		t.Fatalf("First = %q", k)
	}
	if err := ks.Insert([]byte("b"), []byte("x")); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("Insert present = %v", err)
	}
	if err := ks.Replace([]byte("zz"), []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Replace absent = %v", err)
	}
	// The cursor advances normally — a stale-marked cursor would
	// error (stale cursors require re-positioning).
	if k, _ := c.Next(); string(k) != "b" {
		t.Fatalf("Next after no-op verbs = %q, want b (cursor spuriously invalidated?)", k)
	}
	if err := c.Err(); err != nil {
		t.Fatalf("cursor err: %v", err)
	}
}

// TestInsertReplaceIndexedMaintenance: on the indexed path the verbs
// gate BEFORE index maintenance — a no-op verb leaves the index rows
// untouched; a successful verb maintains them exactly like Put.
func TestInsertReplaceIndexedMaintenance(t *testing.T) {
	extract := func(_, v []byte) []IndexEntry {
		return []IndexEntry{{Cols: [][]byte{bytes.Clone(v)}}}
	}
	decl := &IndexDecl{Name: "byv", Columns: []IndexColumn{{Name: "v"}}, Extract: extract}
	_, _, ks := verbFixture(t, decl)

	if err := ks.Insert([]byte("a"), []byte("red")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	idx, err := ks.Index("byv")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	countByVal := func(v string) int {
		n := 0
		for range idx.LookupKeys([][]byte{[]byte(v)}) {
			n++
		}
		if err := idx.Err(); err != nil {
			t.Fatalf("lookup: %v", err)
		}
		return n
	}
	if n := countByVal("red"); n != 1 {
		t.Fatalf("index rows for red = %d, want 1", n)
	}

	// No-op Insert with a DIFFERENT value: the index must not gain a
	// ghost row for the never-stored value.
	if err := ks.Insert([]byte("a"), []byte("blue")); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("Insert present = %v", err)
	}
	if n := countByVal("blue"); n != 0 {
		t.Fatalf("no-op Insert ghost-added %d index row(s) for blue", n)
	}
	if n := countByVal("red"); n != 1 {
		t.Fatalf("no-op Insert disturbed red rows: %d", n)
	}

	// No-op Replace likewise.
	if err := ks.Replace([]byte("zz"), []byte("green")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Replace absent = %v", err)
	}
	if n := countByVal("green"); n != 0 {
		t.Fatalf("no-op Replace ghost-added %d index row(s)", n)
	}

	// Successful Replace re-points the index (old row out, new in).
	if err := ks.Replace([]byte("a"), []byte("blue")); err != nil {
		t.Fatalf("Replace present: %v", err)
	}
	if n := countByVal("red"); n != 0 {
		t.Fatalf("Replace left the stale red row: %d", n)
	}
	if n := countByVal("blue"); n != 1 {
		t.Fatalf("Replace missing blue row: %d", n)
	}
}
