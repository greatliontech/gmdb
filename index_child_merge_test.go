package gmdb

import (
	"context"
	"errors"
	"testing"
)

// A child commit swaps the parent keyspace's pinned-index map for the
// child's, but every *IndexHandle the parent handed out kept pointing
// at the PRE-child pinnedIndex (indexing.md §Handle Invalidation; the
// BeginChild contract: "the parent's handles reflect the committed
// child work"). The audit's failing reproducer read 1 row through the
// parent handle vs 201 through a fresh one — silently, Err()==nil —
// and after a child Drop the parent handle descended a FreeSubtree'd
// root. The merges now reconcile openIndexHandles.

func childMergeFixture(t *testing.T) (*Tx, *Keyspace, *IndexHandle, func() *IndexDecl) {
	t.Helper()
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
	t.Cleanup(func() { tx.Rollback() })
	extract := func(key, value []byte) []IndexEntry {
		return []IndexEntry{{Cols: [][]byte{value[:1]}}}
	}
	mkDecl := func() *IndexDecl {
		return &IndexDecl{Name: "by_c", Columns: []IndexColumn{{Name: "c"}}, Extract: extract}
	}
	ks, err := tx.CreateKeyspace("t", mkDecl())
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Put([]byte("k1"), []byte("a1")); err != nil {
		t.Fatal(err)
	}
	h, err := ks.Index("by_c")
	if err != nil {
		t.Fatal(err)
	}
	return tx, ks, h, mkDecl
}

// TestIndexHandleSeesChildCommit: the parent-held handle must serve
// the merged (child-committed) tree, byte-for-byte with a fresh one.
func TestIndexHandleSeesChildCommit(t *testing.T) {
	tx, ks, h, mkDecl := childMergeFixture(t)
	child, err := tx.BeginChild()
	if err != nil {
		t.Fatal(err)
	}
	cks, err := child.OpenKeyspace("t", mkDecl())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 200 { // enough rows to force index-tree CoW churn
		key := []byte{byte('A' + i%26), byte('0' + i%10), byte(i)}
		val := append([]byte("a"), key...)
		if err := cks.Put(key, val); err != nil {
			t.Fatal(err)
		}
	}
	if err := child.Commit(); err != nil {
		t.Fatal(err)
	}

	n := 0
	for range h.Lookup([][]byte{[]byte("a")}) {
		n++
	}
	if err := h.Err(); err != nil {
		t.Fatalf("parent handle Err after child commit: %v", err)
	}
	if n != 201 {
		t.Errorf("parent handle Lookup after child commit = %d rows, want 201 (stale pinned root)", n)
	}
	st, err := h.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Entries != 201 {
		t.Errorf("parent handle Stats.Entries = %d, want 201", st.Entries)
	}
	_ = ks
}

// TestIndexHandleDeadAfterChildDrop: a child Drop+FreeSubtree must
// mark the parent's handle dead (Inv-IHS2) — ErrIndexNotFound, never
// a descent of freed pages.
func TestIndexHandleDeadAfterChildDrop(t *testing.T) {
	tx, _, h, _ := childMergeFixture(t)
	child, err := tx.BeginChild()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Indexes().Drop("t", "by_c"); err != nil {
		t.Fatalf("child Drop: %v", err)
	}
	if err := child.Commit(); err != nil {
		t.Fatal(err)
	}
	for range h.Lookup([][]byte{[]byte("a")}) {
	}
	if err := h.Err(); !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("parent handle after child Drop: Err=%v, want ErrIndexNotFound", err)
	}
	if _, err := h.Stats(); !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("parent handle Stats after child Drop: %v, want ErrIndexNotFound", err)
	}
}

// TestSetIndexHandleSeesChildCommit: the SetKeyspace mirror.
func TestSetIndexHandleSeesChildCommit(t *testing.T) {
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
	extract := func(setKey, member []byte) []IndexEntry {
		return []IndexEntry{{Cols: [][]byte{member[:1]}}}
	}
	mkDecl := func() *IndexDecl {
		return &IndexDecl{Name: "by_m", Columns: []IndexColumn{{Name: "m"}}, Extract: extract}
	}
	sks, err := tx.CreateSetKeyspace("s", nil, mkDecl())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sks.Put([]byte("set"), []byte("a-seed")); err != nil {
		t.Fatal(err)
	}
	h, err := sks.Index("by_m")
	if err != nil {
		t.Fatal(err)
	}
	child, err := tx.BeginChild()
	if err != nil {
		t.Fatal(err)
	}
	csks, err := child.OpenSetKeyspace("s", mkDecl())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		member := []byte{'a', byte('A' + i%26), byte('0' + i%10), byte(i)}
		if _, err := csks.Put([]byte("set"), member); err != nil {
			t.Fatal(err)
		}
	}
	if err := child.Commit(); err != nil {
		t.Fatal(err)
	}
	n := 0
	for range h.Lookup([][]byte{[]byte("a")}) {
		n++
	}
	if err := h.Err(); err != nil {
		t.Fatalf("set parent handle Err after child commit: %v", err)
	}
	if n != 201 {
		t.Errorf("set parent handle Lookup after child commit = %d, want 201", n)
	}
}

// TestIndexIterAcrossChildCommit pins Inv-IHS4's cursor clause: an
// index iter in flight across BeginChild/commit is stale-marked with
// the merged root — it must NOT keep yielding from the pre-child
// tree (the sequence ends on the stale; a fresh lookup sees the
// merged data).
func TestIndexIterAcrossChildCommit(t *testing.T) {
	tx, _, h, mkDecl := childMergeFixture(t)
	seen := 0
	for range h.Lookup([][]byte{[]byte("a")}) {
		seen++
		if seen == 1 {
			child, err := tx.BeginChild()
			if err != nil {
				t.Fatal(err)
			}
			cks, err := child.OpenKeyspace("t", mkDecl())
			if err != nil {
				t.Fatal(err)
			}
			for i := range 300 {
				key := []byte{byte('A' + i%26), byte('0' + i%10), byte(i)}
				if err := cks.Put(key, append([]byte("a"), key...)); err != nil {
					t.Fatal(err)
				}
			}
			if err := child.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if seen != 1 {
		t.Errorf("in-flight iter across child commit yielded %d rows, want 1 (stale must end the sequence)", seen)
	}
	// A fresh pass through the SAME handle serves the merged tree.
	n := 0
	for range h.Lookup([][]byte{[]byte("a")}) {
		n++
	}
	if err := h.Err(); err != nil {
		t.Fatalf("handle Err after re-lookup: %v", err)
	}
	if n != 301 {
		t.Errorf("re-lookup after child commit = %d rows, want 301", n)
	}
}

// TestIndexHandleAfterChildRollback pins the complement: a child
// ROLLBACK leaves the parent handle serving exactly the pre-child
// state.
func TestIndexHandleAfterChildRollback(t *testing.T) {
	tx, _, h, mkDecl := childMergeFixture(t)
	child, err := tx.BeginChild()
	if err != nil {
		t.Fatal(err)
	}
	cks, err := child.OpenKeyspace("t", mkDecl())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		key := []byte{byte('A' + i%26), byte(i)}
		if err := cks.Put(key, append([]byte("a"), key...)); err != nil {
			t.Fatal(err)
		}
	}
	if err := child.Rollback(); err != nil {
		t.Fatal(err)
	}
	n := 0
	for range h.Lookup([][]byte{[]byte("a")}) {
		n++
	}
	if err := h.Err(); err != nil {
		t.Fatalf("handle Err after child rollback: %v", err)
	}
	if n != 1 {
		t.Errorf("parent handle after child rollback = %d rows, want 1 (pre-child state)", n)
	}
}

// TestParentCursorsInvalidatedByChildDeleteKeyspace pins the merge's
// deleted-keyspace branch: a
// parent row cursor and index iter in flight when a child
// DeleteKeyspace commits must terminate via the staleness/dead
// machinery — never keep yielding from the FreeSubtree'd trees.
func TestParentCursorsInvalidatedByChildDeleteKeyspace(t *testing.T) {
	tx, ks, h, _ := childMergeFixture(t)
	// More parent rows so both iterators are genuinely mid-flight.
	for i := range 50 {
		if err := ks.Put([]byte{byte('B' + i%20), byte(i)}, []byte("aXX")); err != nil {
			t.Fatal(err)
		}
	}

	rowSeen, idxSeen := 0, 0
	c := ks.Cursor()
	kb, _ := c.First()
	if kb == nil {
		t.Fatal("empty cursor")
	}
	rowSeen++

	for range h.Lookup([][]byte{[]byte("a")}) {
		idxSeen++
		if idxSeen == 1 {
			child, err := tx.BeginChild()
			if err != nil {
				t.Fatal(err)
			}
			if err := child.DeleteKeyspace("t"); err != nil {
				t.Fatalf("child DeleteKeyspace: %v", err)
			}
			// Post-delete writes in the child (reuse of the freed
			// pages is suspended under the savepoint, so without
			// the fix the fault manifests as silent stale yields
			// from the intact freed tree — 199 rows in this
			// reproducer — not reused-page garbage).
			nks, err := child.CreateKeyspace("t2")
			if err != nil {
				t.Fatal(err)
			}
			for i := range 300 {
				if err := nks.Put([]byte{byte('A' + i%26), byte('0' + i%10), byte(i)}, []byte("zzzz")); err != nil {
					t.Fatal(err)
				}
			}
			if err := child.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if idxSeen != 1 {
		t.Errorf("index iter after child DeleteKeyspace yielded %d rows, want 1 (freed-page reads)", idxSeen)
	}
	if err := h.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("index iter Err after child DeleteKeyspace = %v, want ErrKeyspaceClosed", err)
	}
	// The row cursor: next op must not read freed pages — dead check
	// or stale, surfaced via Err.
	for kb, _ = c.Next(); kb != nil; kb, _ = c.Next() {
		rowSeen++
	}
	if err := c.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("row cursor Err after child DeleteKeyspace = %v, want ErrKeyspaceClosed", err)
	}
	if rowSeen != 1 {
		t.Errorf("row cursor yielded %d rows after child DeleteKeyspace, want 1", rowSeen)
	}
}

// TestIndexIterAcrossChildRebuild pins the reconcile live-arm's
// cursor stale-marking where nothing else covers it: a child
// Indexes().Rebuild replaces the index tree WITHOUT moving the row
// root, so the merge's rootMoved-gated markCursorsStale (whose
// delegation re-stales index cursors) never runs — only
// reconcileIndexHandles stands between the parent's in-flight iter
// and the FreeSubtree'd pre-rebuild tree.
func TestIndexIterAcrossChildRebuild(t *testing.T) {
	tx, ks, h, mkDecl := childMergeFixture(t)
	for i := range 50 {
		if err := ks.Put([]byte{byte('B' + i%20), byte(i)}, []byte("aXX")); err != nil {
			t.Fatal(err)
		}
	}
	seen := 0
	for range h.Lookup([][]byte{[]byte("a")}) {
		seen++
		if seen == 1 {
			child, err := tx.BeginChild()
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Indexes().Rebuild("t", mkDecl()); err != nil {
				t.Fatalf("child Rebuild: %v", err)
			}
			if err := child.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if seen != 1 {
		t.Errorf("iter across child rebuild yielded %d rows, want 1 (stale must end the sequence)", seen)
	}
	// Fresh pass through the same handle: merged (rebuilt) tree.
	n := 0
	for range h.Lookup([][]byte{[]byte("a")}) {
		n++
	}
	if err := h.Err(); err != nil {
		t.Fatalf("handle Err after re-lookup: %v", err)
	}
	if n != 51 {
		t.Errorf("re-lookup after child rebuild = %d rows, want 51", n)
	}
}
