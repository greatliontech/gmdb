package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

// Chunk-5.7 public-surface tests for Keyspace.DeleteRange. The
// btree-layer tests in internal/btree/range_delete_test.go pin the
// algorithm invariants directly; these tests pin the public surface
// against the chunk-5.6 deferred-flush machinery.

// TestKeyspaceDeleteRangeEmptyRange pins the chunk-5.1 user-locked
// "DeleteRange returns (0, nil) for an empty range" decision.
func TestKeyspaceDeleteRangeEmptyRange(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	_ = ks.Put([]byte("a"), []byte("v"))
	for _, pair := range []struct{ s, e []byte }{
		{[]byte("k"), []byte("k")},
		{[]byte("z"), []byte("a")},
	} {
		n, err := ks.DeleteRange(pair.s, pair.e)
		if err != nil {
			t.Errorf("DeleteRange(%q,%q): %v", pair.s, pair.e, err)
		}
		if n != 0 {
			t.Errorf("DeleteRange(%q,%q) = %d, want 0", pair.s, pair.e, n)
		}
	}
}

// TestKeyspaceDeleteRangeEmptyKeyspace asserts DeleteRange on a
// fresh (desc.Root == 0) keyspace returns (0, nil) without invoking
// the btree layer.
func TestKeyspaceDeleteRangeEmptyKeyspace(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	n, err := ks.DeleteRange([]byte("a"), []byte("z"))
	if err != nil {
		t.Fatalf("DeleteRange on empty keyspace: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

// TestKeyspaceDeleteRangeRoundTrip promotes range-delete.md Inv-A at
// the public surface: insert {a..e}, DeleteRange(b, d), Get assertions.
func TestKeyspaceDeleteRangeRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		if err := ks.Put([]byte(k), []byte("v")); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	n, err := ks.DeleteRange([]byte("b"), []byte("d"))
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
	for _, tc := range []struct {
		k    string
		want error
	}{
		{"a", nil},
		{"b", ErrNotFound},
		{"c", ErrNotFound},
		{"d", nil},
		{"e", nil},
	} {
		_, err := ks.Get([]byte(tc.k))
		if (tc.want == nil && err != nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("Get(%q) err = %v, want %v", tc.k, err, tc.want)
		}
	}
	// Descriptor in-memory count: started at 5, minus 2 = 3.
	if ks.desc.Count != 3 {
		t.Errorf("desc.Count = %d, want 3", ks.desc.Count)
	}
}

// TestKeyspaceDeleteRangeAllKeys promotes range-delete.md Inv-A
// (nil, nil) clause + Inv-C root-collapse: deleting every key
// leaves desc.Root=0 and the keyspace empty.
func TestKeyspaceDeleteRangeAllKeys(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	for i := range 30 {
		_ = ks.Put([]byte(fmt.Sprintf("k%03d", i)), []byte("v"))
	}
	n, err := ks.DeleteRange(nil, nil)
	if err != nil {
		t.Fatalf("DeleteRange(nil, nil): %v", err)
	}
	if n != 30 {
		t.Errorf("count = %d, want 30", n)
	}
	if ks.desc.Root != 0 {
		t.Errorf("desc.Root = %d, want 0 (collapsed empty tree)", ks.desc.Root)
	}
	if ks.desc.Count != 0 {
		t.Errorf("desc.Count = %d, want 0", ks.desc.Count)
	}
	// Get on a previously-existing key returns ErrNotFound.
	if _, err := ks.Get([]byte("k000")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(k000) post-DeleteRange: got %v, want ErrNotFound", err)
	}
}

// TestKeyspaceDeleteRangePersistsAcrossCommit promotes Inv-A
// across-commit clause: post-Commit, a fresh tx sees the deleted
// range absent.
func TestKeyspaceDeleteRangePersistsAcrossCommit(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, _ := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()

	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("ks")
	for i := range 20 {
		_ = ks.Put([]byte(fmt.Sprintf("k%02d", i)), []byte("v"))
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit #1: %v", err)
	}

	tx2, _ := db.Begin(ctx)
	ks2, _ := tx2.OpenKeyspace("ks")
	n, err := ks2.DeleteRange([]byte("k05"), []byte("k15"))
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if n != 10 {
		t.Errorf("count = %d, want 10", n)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit #2: %v", err)
	}

	tx3, _ := db.Begin(ctx)
	defer tx3.Rollback()
	ks3, _ := tx3.OpenKeyspace("ks")
	for i := range 20 {
		key := []byte(fmt.Sprintf("k%02d", i))
		_, err := ks3.Get(key)
		shouldExist := i < 5 || i >= 15
		if shouldExist && err != nil {
			t.Errorf("Get(%s) = %v, want present", key, err)
		}
		if !shouldExist && !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%s) = %v, want ErrNotFound", key, err)
		}
	}
}

// TestKeyspaceDeleteRangeKeyspaceClosedAfterDeleteKeyspace promotes
// chunk-5.6 Inv-D for the new DeleteRange surface: an invalidated
// handle returns ErrKeyspaceClosed.
func TestKeyspaceDeleteRangeKeyspaceClosedAfterDeleteKeyspace(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	_ = ks.Put([]byte("a"), []byte("v"))
	_ = tx.DeleteKeyspace("ks")
	if _, err := ks.DeleteRange([]byte("a"), []byte("z")); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("DeleteRange on dead handle: got %v, want ErrKeyspaceClosed", err)
	}
}

// TestKeyspaceDeleteRangeMarksCursorsStale promotes the
// sibling-mutation contract: an open cursor on the keyspace is
// MarkStale'd by DeleteRange.
func TestKeyspaceDeleteRangeMarksCursorsStale(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	for _, k := range []string{"a", "b", "c"} {
		_ = ks.Put([]byte(k), []byte("v"))
	}
	c := ks.Cursor()
	if k, _ := c.First(); !bytes.Equal(k, []byte("a")) {
		t.Fatalf("First = %q, want a", k)
	}
	n, err := ks.DeleteRange([]byte("a"), []byte("c"))
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
	// Cursor must be stale on next nav.
	if _, _ = c.Next(); c.Err() == nil || !errors.Is(c.Err(), ErrCursorStale) {
		t.Errorf("Cursor.Next post-DeleteRange Err = %v, want ErrCursorStale", c.Err())
	}
	// Re-position works.
	if k, _ := c.First(); !bytes.Equal(k, []byte("c")) {
		t.Errorf("Re-First post-DeleteRange = %q, want c", k)
	}
}

// TestKeyspaceDeleteRangeNoOpDoesNotMarkCursorsStale asserts the
// efficiency optimisation in Keyspace.DeleteRange: when count == 0
// (no rows fell in the range), no cursor invalidation happens.
func TestKeyspaceDeleteRangeNoOpDoesNotMarkCursorsStale(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	_ = ks.Put([]byte("a"), []byte("v"))
	_ = ks.Put([]byte("z"), []byte("v"))
	c := ks.Cursor()
	if k, _ := c.First(); !bytes.Equal(k, []byte("a")) {
		t.Fatalf("First = %q, want a", k)
	}
	// Range that touches no key.
	n, err := ks.DeleteRange([]byte("m"), []byte("n"))
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
	// Cursor still valid — Next without re-position.
	if k, _ := c.Next(); !bytes.Equal(k, []byte("z")) {
		t.Errorf("Cursor.Next post-no-op-DeleteRange = %q, want z", k)
	}
	if err := c.Err(); err != nil {
		t.Errorf("Cursor.Err post-no-op-DeleteRange = %v, want nil", err)
	}
}

// TestKeyspaceDeleteRangeRejectsEmptyByteBoundary pins the chunk-5.7
// L-2 fix: `nil` boundaries are open per range-delete.md invariant
// #1, but a non-nil zero-length boundary is rejected with
// ErrKeyEmpty (consistent with every other name-taking API per the
// chunk-5.1 user-locked empty-key policy).
func TestKeyspaceDeleteRangeRejectsEmptyByteBoundary(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	_ = ks.Put([]byte("a"), []byte("v"))
	// nil boundaries are open — sanity check still works.
	n, err := ks.DeleteRange(nil, nil)
	if err != nil {
		t.Fatalf("DeleteRange(nil, nil): %v", err)
	}
	if n != 1 {
		t.Errorf("DeleteRange(nil, nil) count = %d, want 1", n)
	}
	// []byte{} bounds must be rejected.
	_ = ks.Put([]byte("a"), []byte("v"))
	for _, pair := range []struct {
		s, e []byte
		name string
	}{
		{[]byte{}, []byte("z"), "empty start"},
		{[]byte("a"), []byte{}, "empty end"},
		{[]byte{}, []byte{}, "both empty"},
	} {
		if _, err := ks.DeleteRange(pair.s, pair.e); !errors.Is(err, ErrKeyEmpty) {
			t.Errorf("DeleteRange %s: got %v, want ErrKeyEmpty", pair.name, err)
		}
	}
	// Verify the keyspace is unmodified by the rejected calls.
	if _, err := ks.Get([]byte("a")); err != nil {
		t.Errorf("Get(a) after rejected DeleteRange: %v", err)
	}
}

// TestKeyspaceDeleteRangeReadOnlyTxReturnsErrReadOnly: read snapshots
// can't mutate.
func TestKeyspaceDeleteRangeReadOnlyTxReturnsErrReadOnly(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	// Pre-commit a keyspace.
	tx, _ := db.Begin(ctx)
	if _, err := tx.CreateKeyspace("ks"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}
