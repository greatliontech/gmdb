package gmdb

import (
	"context"
	"errors"
	"testing"
)

// A child-created IndexHandle (obtained from the child's own
// OpenKeyspace) must stop serving queries the instant the child
// transaction resolves: the BeginChild contract (nested.go) promises
// ErrTxClosed on every child handle once the child ends. The iter
// closures (Lookup/LookupKeys/Range/Prefix) and Get lacked the
// requireOpen(false) probe Stats already performs, so after a child
// Commit the handle kept serving lookups (Err()==nil) and after a
// child Rollback it descended savepoint-reverted pages
// (ErrCorrupted, or silently wrong data if the freed page parsed as a
// valid leaf).

func childHandleLifetimeFixture(t *testing.T) (*Tx, func() *IndexDecl) {
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
	if _, err := tx.CreateKeyspace("t", mkDecl()); err != nil {
		t.Fatal(err)
	}
	return tx, mkDecl
}

// childIndexHandle opens keyspace "t" in a fresh child, seeds a row so
// the index tree is non-empty, and returns the child plus a handle
// obtained from the child's own OpenKeyspace.
func childIndexHandle(t *testing.T, tx *Tx, mkDecl func() *IndexDecl) (*Tx, *IndexHandle) {
	t.Helper()
	child, err := tx.BeginChild()
	if err != nil {
		t.Fatal(err)
	}
	cks, err := child.OpenKeyspace("t", mkDecl())
	if err != nil {
		t.Fatal(err)
	}
	if err := cks.Put([]byte("k1"), []byte("a1")); err != nil {
		t.Fatal(err)
	}
	h, err := cks.Index("by_c")
	if err != nil {
		t.Fatal(err)
	}
	return child, h
}

// drainSeq2 exhausts an iter.Seq2 and returns the row count.
func drainSeq2(seq func(func([]byte, []byte) bool)) int {
	n := 0
	seq(func(_, _ []byte) bool { n++; return true })
	return n
}

// TestChildIndexHandleErrsAfterChildCommit: after the child commits,
// every query surface on the child-created handle must surface
// ErrTxClosed, not serve stale rows.
func TestChildIndexHandleErrsAfterChildCommit(t *testing.T) {
	tx, mkDecl := childHandleLifetimeFixture(t)
	child, h := childIndexHandle(t, tx, mkDecl)
	if err := child.Commit(); err != nil {
		t.Fatal(err)
	}
	assertHandleClosed(t, h, "after child Commit")
}

// TestChildIndexHandleErrsAfterChildRollback: after the child rolls
// back, the handle's pinned root points at savepoint-reverted pages;
// every query surface must surface ErrTxClosed rather than descend
// them.
func TestChildIndexHandleErrsAfterChildRollback(t *testing.T) {
	tx, mkDecl := childHandleLifetimeFixture(t)
	child, h := childIndexHandle(t, tx, mkDecl)
	if err := child.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertHandleClosed(t, h, "after child Rollback")
}

// assertHandleClosed asserts every IndexHandle query surface reports
// ErrTxClosed (via Err() for the iter surfaces, directly for Get) and
// yields nothing.
func assertHandleClosed(t *testing.T, h *IndexHandle, when string) {
	t.Helper()

	// Bare Err() poll with no intervening iter (Inv-IHS5): the
	// tx-state sentinel must surface even without a query call.
	if err := h.Err(); !errors.Is(err, ErrTxClosed) {
		t.Errorf("bare Err() %s: %v, want ErrTxClosed", when, err)
	}

	if n := drainSeq2(h.Lookup([]byte("a"))); n != 0 {
		t.Errorf("Lookup %s yielded %d rows, want 0", when, n)
	}
	if err := h.Err(); !errors.Is(err, ErrTxClosed) {
		t.Errorf("Lookup %s: Err=%v, want ErrTxClosed", when, err)
	}

	n := 0
	for range h.LookupKeys([]byte("a")) {
		n++
	}
	if n != 0 {
		t.Errorf("LookupKeys %s yielded %d keys, want 0", when, n)
	}
	if err := h.Err(); !errors.Is(err, ErrTxClosed) {
		t.Errorf("LookupKeys %s: Err=%v, want ErrTxClosed", when, err)
	}

	if n := drainSeq2(h.Range(nil, nil)); n != 0 {
		t.Errorf("Range %s yielded %d rows, want 0", when, n)
	}
	if err := h.Err(); !errors.Is(err, ErrTxClosed) {
		t.Errorf("Range %s: Err=%v, want ErrTxClosed", when, err)
	}

	if n := drainSeq2(h.Prefix([]byte("a"))); n != 0 {
		t.Errorf("Prefix %s yielded %d rows, want 0", when, n)
	}
	if err := h.Err(); !errors.Is(err, ErrTxClosed) {
		t.Errorf("Prefix %s: Err=%v, want ErrTxClosed", when, err)
	}

	if _, _, err := h.Get([]byte("a")); !errors.Is(err, ErrTxClosed) {
		t.Errorf("Get %s: err=%v, want ErrTxClosed", when, err)
	}

	if _, err := h.Stats(); !errors.Is(err, ErrTxClosed) {
		t.Errorf("Stats %s: err=%v, want ErrTxClosed", when, err)
	}
}

// TestParentIndexHandleFrozenByActiveChild: while a child transaction
// is unresolved the parent is frozen (transactions.md §Nested
// Transactions parent-freeze). A parent-held IndexHandle's query
// surfaces — and the bare Err() poll — must surface ErrChildActive,
// the ErrChildActive arm of Inv-IHS5.
func TestParentIndexHandleFrozenByActiveChild(t *testing.T) {
	tx, mkDecl := childHandleLifetimeFixture(t)
	// Seed a row + obtain the handle on the PARENT before the child
	// opens, so pinned.root != 0 (the closures don't early-return).
	pks, err := tx.OpenKeyspace("t", mkDecl())
	if err != nil {
		t.Fatal(err)
	}
	if err := pks.Put([]byte("k1"), []byte("a1")); err != nil {
		t.Fatal(err)
	}
	h, err := pks.Index("by_c")
	if err != nil {
		t.Fatal(err)
	}
	child, err := tx.BeginChild()
	if err != nil {
		t.Fatal(err)
	}
	defer child.Rollback()

	// Truly-bare Err() poll (idx.err still nil, no intervening iter):
	// pins the ErrChildActive arm of Err()'s own requireLive probe,
	// independent of the sticky idx.err the iters below would stamp.
	if err := h.Err(); !errors.Is(err, ErrChildActive) {
		t.Errorf("truly-bare Err() during active child: %v, want ErrChildActive", err)
	}

	if n := drainSeq2(h.Lookup([]byte("a"))); n != 0 {
		t.Errorf("Lookup during active child yielded %d rows, want 0", n)
	}
	if err := h.Err(); !errors.Is(err, ErrChildActive) {
		t.Errorf("Lookup during active child: Err=%v, want ErrChildActive", err)
	}
	if n := drainSeq2(h.Range(nil, nil)); n != 0 {
		t.Errorf("Range during active child yielded %d rows, want 0", n)
	}
	if err := h.Err(); !errors.Is(err, ErrChildActive) {
		t.Errorf("Range during active child: Err=%v, want ErrChildActive", err)
	}
	if _, err := h.Stats(); !errors.Is(err, ErrChildActive) {
		t.Errorf("Stats during active child: %v, want ErrChildActive", err)
	}

	// After the child resolves, the parent handle recovers (a fresh
	// iter serves the pre-child state).
	if err := child.Rollback(); err != nil {
		t.Fatal(err)
	}
	n := drainSeq2(h.Lookup([]byte("a")))
	if err := h.Err(); err != nil {
		t.Fatalf("parent handle Err after child resolve: %v", err)
	}
	if n != 1 {
		t.Errorf("parent Lookup after child resolve = %d rows, want 1", n)
	}
}
