package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
)

// lookupPKs returns the sorted primary keys an index maps the given
// single-byte column value to.
func lookupPKs(idx *IndexHandle, col byte) []string {
	var pks []string
	for pk := range idx.LookupKeys([]byte{col}) {
		pks = append(pks, string(pk))
	}
	sort.Strings(pks)
	return pks
}

// openNestedTestDB opens a fresh DB sized for the nested-tx tests.
func openNestedTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func mustGet(t *testing.T, ks *Keyspace, key string) []byte {
	t.Helper()
	v, err := ks.Get([]byte(key))
	if err != nil {
		t.Fatalf("Get %q: %v", key, err)
	}
	return v
}

// TestChildCommitMergesIntoParent: a committed child's writes are visible
// through a parent handle the caller still holds, and survive the
// top-level Commit + re-Open.
func TestChildCommitMergesIntoParent(t *testing.T) {
	ctx := context.Background()
	db := openNestedTestDB(t)

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("ks")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatalf("parent Put: %v", err)
	}

	child, err := tx.BeginChild()
	if err != nil {
		t.Fatalf("BeginChild: %v", err)
	}
	cks, err := child.OpenKeyspace("ks")
	if err != nil {
		t.Fatalf("child OpenKeyspace: %v", err)
	}
	if err := cks.Put([]byte("b"), []byte("2")); err != nil {
		t.Fatalf("child Put: %v", err)
	}
	if err := child.Commit(); err != nil {
		t.Fatalf("child Commit: %v", err)
	}

	// Parent handle reflects the committed child write.
	if got := mustGet(t, ks, "b"); !bytes.Equal(got, []byte("2")) {
		t.Errorf("parent ks.Get(b) = %q, want 2", got)
	}
	if got := mustGet(t, ks, "a"); !bytes.Equal(got, []byte("1")) {
		t.Errorf("parent ks.Get(a) = %q, want 1", got)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("top Commit: %v", err)
	}

	// Re-open: durable.
	tx2, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin 2: %v", err)
	}
	defer tx2.Rollback()
	ks2, err := tx2.OpenKeyspace("ks")
	if err != nil {
		t.Fatalf("OpenKeyspace 2: %v", err)
	}
	if got := mustGet(t, ks2, "a"); !bytes.Equal(got, []byte("1")) {
		t.Errorf("reopen a = %q, want 1", got)
	}
	if got := mustGet(t, ks2, "b"); !bytes.Equal(got, []byte("2")) {
		t.Errorf("reopen b = %q, want 2", got)
	}
}

// TestChildRollbackDiscardsWork (Inv-N1 / Inv-N2): a rolled-back child's
// writes are invisible afterward, the parent's pre-child data is intact,
// and the top-level tx can still commit cleanly.
func TestChildRollbackDiscardsWork(t *testing.T) {
	ctx := context.Background()
	db := openNestedTestDB(t)

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("ks")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// Parent writes several keys (creates same-tx pages an ancestor
	// references — the Inv-N1 hazard surface).
	for i := range 50 {
		if err := ks.Put(fmt.Appendf(nil, "k%03d", i), fmt.Appendf(nil, "v%03d", i)); err != nil {
			t.Fatalf("parent Put %d: %v", i, err)
		}
	}

	child, err := tx.BeginChild()
	if err != nil {
		t.Fatalf("BeginChild: %v", err)
	}
	cks, err := child.OpenKeyspace("ks")
	if err != nil {
		t.Fatalf("child OpenKeyspace: %v", err)
	}
	// Child overwrites and adds enough keys to force allocation churn.
	for i := range 50 {
		if err := cks.Put(fmt.Appendf(nil, "k%03d", i), []byte("OVERWRITTEN")); err != nil {
			t.Fatalf("child Put %d: %v", i, err)
		}
		if err := cks.Put(fmt.Appendf(nil, "c%03d", i), []byte("child-only")); err != nil {
			t.Fatalf("child Put c %d: %v", i, err)
		}
	}
	if err := child.Rollback(); err != nil {
		t.Fatalf("child Rollback: %v", err)
	}

	// Parent's pre-child data is intact (Inv-N1: ancestor pages not
	// overwritten by the child's now-discarded allocations).
	for i := range 50 {
		want := fmt.Sprintf("v%03d", i)
		if got := mustGet(t, ks, fmt.Sprintf("k%03d", i)); !bytes.Equal(got, []byte(want)) {
			t.Fatalf("parent k%03d = %q after child rollback, want %q", i, got, want)
		}
	}
	// Child-only keys are gone.
	if _, err := ks.Get([]byte("c000")); !errors.Is(err, ErrNotFound) {
		t.Errorf("child-only key c000 present after rollback: err=%v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("top Commit after child rollback: %v", err)
	}
}

// TestParentFrozenWhileChildActive (Inv-N5): every parent op returns
// ErrChildActive while a child is unresolved; the parent resumes once
// the child commits.
func TestParentFrozenWhileChildActive(t *testing.T) {
	ctx := context.Background()
	db := openNestedTestDB(t)

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("ks")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}

	child, err := tx.BeginChild()
	if err != nil {
		t.Fatalf("BeginChild: %v", err)
	}

	// Parent data op, second BeginChild, Commit, and Rollback all frozen.
	if err := ks.Put([]byte("x"), []byte("1")); !errors.Is(err, ErrChildActive) {
		t.Errorf("frozen parent Put err = %v, want ErrChildActive", err)
	}
	if _, err := ks.Get([]byte("x")); !errors.Is(err, ErrChildActive) {
		t.Errorf("frozen parent Get err = %v, want ErrChildActive", err)
	}
	if _, err := tx.BeginChild(); !errors.Is(err, ErrChildActive) {
		t.Errorf("frozen parent BeginChild err = %v, want ErrChildActive", err)
	}
	if err := tx.Commit(); !errors.Is(err, ErrChildActive) {
		t.Errorf("frozen parent Commit err = %v, want ErrChildActive", err)
	}
	if err := tx.Rollback(); !errors.Is(err, ErrChildActive) {
		t.Errorf("frozen parent Rollback err = %v, want ErrChildActive", err)
	}

	// Resolve the child; parent resumes.
	if err := child.Commit(); err != nil {
		t.Fatalf("child Commit: %v", err)
	}
	if err := ks.Put([]byte("x"), []byte("1")); err != nil {
		t.Errorf("parent Put after child resolve: %v", err)
	}
}

// TestParentCursorFreezeIsTransient: a parent cursor touched during the
// child-active freeze reports ErrChildActive transiently (via nav-noop +
// Err) and recovers once the child resolves — it is NOT permanently
// killed (ErrChildActive must not stick in closeErr).
func TestParentCursorFreezeIsTransient(t *testing.T) {
	ctx := context.Background()
	db := openNestedTestDB(t)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("ks")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	cur := ks.Cursor()

	child, err := tx.BeginChild()
	if err != nil {
		t.Fatalf("BeginChild: %v", err)
	}
	// Touch the parent cursor during the freeze.
	if k, _ := cur.First(); k != nil {
		t.Errorf("frozen cursor First returned %q, want nil", k)
	}
	if err := cur.Err(); !errors.Is(err, ErrChildActive) {
		t.Errorf("frozen cursor Err = %v, want ErrChildActive", err)
	}

	if err := child.Commit(); err != nil {
		t.Fatalf("child Commit: %v", err)
	}

	// Cursor recovers: ErrChildActive did not stick in closeErr, so a
	// re-navigation succeeds. (If it had stuck, First would no-op
	// forever.)
	if k, v := cur.First(); !bytes.Equal(k, []byte("a")) || !bytes.Equal(v, []byte("1")) {
		t.Errorf("post-resolve cursor First = (%q,%q), want (a,1)", k, v)
	}
	if err := cur.Err(); err != nil {
		t.Errorf("post-resolve cursor Err after First = %v, want nil", err)
	}
}

// TestChildClosedAfterResolve: a child is unusable after Commit/Rollback.
func TestChildClosedAfterResolve(t *testing.T) {
	ctx := context.Background()
	db := openNestedTestDB(t)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.CreateKeyspace("ks"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}

	child, err := tx.BeginChild()
	if err != nil {
		t.Fatalf("BeginChild: %v", err)
	}
	if err := child.Commit(); err != nil {
		t.Fatalf("child Commit: %v", err)
	}
	if err := child.Commit(); !errors.Is(err, ErrTxClosed) {
		t.Errorf("double child Commit err = %v, want ErrTxClosed", err)
	}
	if err := child.Rollback(); !errors.Is(err, ErrTxClosed) {
		t.Errorf("child Rollback after Commit err = %v, want ErrTxClosed", err)
	}
}

// TestNestedGrandchild: arbitrary-depth nesting — grandchild commit then
// child commit composes; grandchild rollback is isolated.
func TestNestedGrandchild(t *testing.T) {
	ctx := context.Background()
	db := openNestedTestDB(t)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("ks")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatalf("Put a: %v", err)
	}

	child, err := tx.BeginChild()
	if err != nil {
		t.Fatalf("BeginChild: %v", err)
	}
	cks, _ := child.OpenKeyspace("ks")
	if err := cks.Put([]byte("b"), []byte("2")); err != nil {
		t.Fatalf("child Put b: %v", err)
	}

	// child is now frozen while grandchild is open.
	gc, err := child.BeginChild()
	if err != nil {
		t.Fatalf("BeginChild (grandchild): %v", err)
	}
	if _, err := cks.Get([]byte("b")); !errors.Is(err, ErrChildActive) {
		t.Errorf("frozen child Get err = %v, want ErrChildActive", err)
	}
	gks, _ := gc.OpenKeyspace("ks")
	if err := gks.Put([]byte("c"), []byte("3")); err != nil {
		t.Fatalf("grandchild Put c: %v", err)
	}
	// Roll back the grandchild — its "c" is discarded.
	if err := gc.Rollback(); err != nil {
		t.Fatalf("grandchild Rollback: %v", err)
	}

	// child resumes; sees a + b, not c.
	if got := mustGet(t, cks, "a"); !bytes.Equal(got, []byte("1")) {
		t.Errorf("child a = %q, want 1", got)
	}
	if got := mustGet(t, cks, "b"); !bytes.Equal(got, []byte("2")) {
		t.Errorf("child b = %q, want 2", got)
	}
	if _, err := cks.Get([]byte("c")); !errors.Is(err, ErrNotFound) {
		t.Errorf("grandchild key c present after rollback: %v", err)
	}

	// A fresh grandchild that commits propagates up.
	gc2, err := child.BeginChild()
	if err != nil {
		t.Fatalf("BeginChild (grandchild 2): %v", err)
	}
	gks2, _ := gc2.OpenKeyspace("ks")
	if err := gks2.Put([]byte("d"), []byte("4")); err != nil {
		t.Fatalf("grandchild2 Put d: %v", err)
	}
	if err := gc2.Commit(); err != nil {
		t.Fatalf("grandchild2 Commit: %v", err)
	}
	if err := child.Commit(); err != nil {
		t.Fatalf("child Commit: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("top Commit: %v", err)
	}

	// Verify final durable state: a, b, d present; c absent.
	tx2, _ := db.Begin(ctx)
	defer tx2.Rollback()
	ks2, _ := tx2.OpenKeyspace("ks")
	for k, v := range map[string]string{"a": "1", "b": "2", "d": "4"} {
		if got := mustGet(t, ks2, k); !bytes.Equal(got, []byte(v)) {
			t.Errorf("final %q = %q, want %q", k, got, v)
		}
	}
	if _, err := ks2.Get([]byte("c")); !errors.Is(err, ErrNotFound) {
		t.Errorf("final c present: %v", err)
	}
}

// TestChildCreatesKeyspace: a keyspace created inside a committed child
// is visible to the parent; a keyspace created in a rolled-back child is
// not.
func TestChildCreatesKeyspace(t *testing.T) {
	ctx := context.Background()
	db := openNestedTestDB(t)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	// Committed child creates "kept".
	c1, _ := tx.BeginChild()
	cks, err := c1.CreateKeyspace("kept")
	if err != nil {
		t.Fatalf("child CreateKeyspace: %v", err)
	}
	if err := cks.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("child Put: %v", err)
	}
	if err := c1.Commit(); err != nil {
		t.Fatalf("child Commit: %v", err)
	}

	// Rolled-back child creates "dropped".
	c2, _ := tx.BeginChild()
	if _, err := c2.CreateKeyspace("dropped"); err != nil {
		t.Fatalf("child2 CreateKeyspace: %v", err)
	}
	if err := c2.Rollback(); err != nil {
		t.Fatalf("child2 Rollback: %v", err)
	}

	// The child's own handle is invalid after the child resolves
	// (handles are tx-scoped — no promotion of the child handle object).
	if _, err := cks.Get([]byte("k")); !errors.Is(err, ErrTxClosed) {
		t.Errorf("child handle post-commit err = %v, want ErrTxClosed", err)
	}

	// Parent sees "kept" with its data via a parent-opened handle, not
	// "dropped".
	pks, err := tx.OpenKeyspace("kept")
	if err != nil {
		t.Fatalf("parent OpenKeyspace kept: %v", err)
	}
	if got := mustGet(t, pks, "k"); !bytes.Equal(got, []byte("v")) {
		t.Errorf("kept k = %q, want v", got)
	}
	if _, err := tx.OpenKeyspace("dropped"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenKeyspace dropped err = %v, want ErrNotFound", err)
	}
}

// TestChildMutatesIndexedKeyspace (L-2 coverage): a child that performs
// index maintenance (Put on an indexed keyspace) merges its pinned-index
// root/count into the parent; the index is queryable through the parent
// handle and survives the top-level Commit + re-open.
func TestChildMutatesIndexedKeyspace(t *testing.T) {
	ctx := context.Background()
	db := openNestedTestDB(t)

	decl := &IndexDecl{
		Name:    "by_color",
		Columns: []IndexColumn{{Name: "color"}},
		Extract: func(_, value []byte) []IndexEntry {
			if len(value) == 0 {
				return nil
			}
			return []IndexEntry{{Cols: [][]byte{{value[0]}}}}
		},
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("a"), []byte{0x42}); err != nil {
		t.Fatalf("parent Put a: %v", err)
	}

	child, err := tx.BeginChild()
	if err != nil {
		t.Fatalf("BeginChild: %v", err)
	}
	cks, err := child.OpenKeyspace("items", decl)
	if err != nil {
		t.Fatalf("child OpenKeyspace: %v", err)
	}
	if err := cks.Put([]byte("b"), []byte{0x42}); err != nil {
		t.Fatalf("child Put b: %v", err)
	}
	if err := cks.Put([]byte("c"), []byte{0x43}); err != nil {
		t.Fatalf("child Put c: %v", err)
	}
	if err := child.Commit(); err != nil {
		t.Fatalf("child Commit: %v", err)
	}

	// Query the index through the parent handle: 0x42 → {a, b}.
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("parent Index: %v", err)
	}
	got := lookupPKs(idx, 0x42)
	if !stringSlicesEqual(got, []string{"a", "b"}) {
		t.Errorf("parent index 0x42 = %v, want [a b]", got)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("top Commit: %v", err)
	}

	// Durable: re-open and re-query.
	tx2, _ := db.Begin(ctx)
	defer tx2.Rollback()
	ks2, err := tx2.OpenKeyspace("items", decl)
	if err != nil {
		t.Fatalf("reopen OpenKeyspace: %v", err)
	}
	idx2, _ := ks2.Index("by_color")
	if got := lookupPKs(idx2, 0x42); !stringSlicesEqual(got, []string{"a", "b"}) {
		t.Errorf("reopen index 0x42 = %v, want [a b]", got)
	}
	if got := lookupPKs(idx2, 0x43); !stringSlicesEqual(got, []string{"c"}) {
		t.Errorf("reopen index 0x43 = %v, want [c]", got)
	}
}

// TestChildCreatesIndexedKeyspace (L-3 coverage): a child that *creates*
// an indexed keyspace exercises the fresh-parent-handle merge branch that
// carries pinned-index state (indexes: cks.indexes). The index must flush
// via the parent's installed handle at the top-level Commit and survive a
// re-open.
func TestChildCreatesIndexedKeyspace(t *testing.T) {
	ctx := context.Background()
	db := openNestedTestDB(t)

	decl := &IndexDecl{
		Name:    "by_color",
		Columns: []IndexColumn{{Name: "color"}},
		Extract: func(_, value []byte) []IndexEntry {
			if len(value) == 0 {
				return nil
			}
			return []IndexEntry{{Cols: [][]byte{{value[0]}}}}
		},
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	child, err := tx.BeginChild()
	if err != nil {
		t.Fatalf("BeginChild: %v", err)
	}
	cks, err := child.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("child CreateKeyspace: %v", err)
	}
	if err := cks.Put([]byte("a"), []byte{0x42}); err != nil {
		t.Fatalf("child Put a: %v", err)
	}
	if err := cks.Put([]byte("b"), []byte{0x42}); err != nil {
		t.Fatalf("child Put b: %v", err)
	}
	if err := child.Commit(); err != nil {
		t.Fatalf("child Commit: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("top Commit: %v", err)
	}

	// Re-open: the child-created index is durable.
	tx2, _ := db.Begin(ctx)
	defer tx2.Rollback()
	ks2, err := tx2.OpenKeyspace("items", decl)
	if err != nil {
		t.Fatalf("reopen OpenKeyspace: %v", err)
	}
	idx2, _ := ks2.Index("by_color")
	if got := lookupPKs(idx2, 0x42); !stringSlicesEqual(got, []string{"a", "b"}) {
		t.Errorf("reopen index 0x42 = %v, want [a b]", got)
	}
}

// TestChildDeletesKeyspace: a child deleting a keyspace the parent had
// open invalidates the parent's handle on commit (ErrKeyspaceClosed).
func TestChildDeletesKeyspace(t *testing.T) {
	ctx := context.Background()
	db := openNestedTestDB(t)

	// Set up "victim" durably.
	tx0, _ := db.Begin(ctx)
	if _, err := tx0.CreateKeyspace("victim"); err != nil {
		t.Fatalf("create victim: %v", err)
	}
	if err := tx0.Commit(); err != nil {
		t.Fatalf("commit setup: %v", err)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	pks, err := tx.OpenKeyspace("victim")
	if err != nil {
		t.Fatalf("parent OpenKeyspace: %v", err)
	}

	child, _ := tx.BeginChild()
	if err := child.DeleteKeyspace("victim"); err != nil {
		t.Fatalf("child DeleteKeyspace: %v", err)
	}
	if err := child.Commit(); err != nil {
		t.Fatalf("child Commit: %v", err)
	}

	// Parent's handle is now dead.
	if _, err := pks.Get([]byte("x")); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("deleted-keyspace parent handle err = %v, want ErrKeyspaceClosed", err)
	}
	if _, err := tx.OpenKeyspace("victim"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenKeyspace deleted err = %v, want ErrNotFound", err)
	}
}

// TestChildSetKeyspaceRoundTrip: nested-tx merge works for Kind=1
// set-keyspaces too.
func TestChildSetKeyspaceRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openNestedTestDB(t)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	sks, err := tx.CreateSetKeyspace("set", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if _, err := sks.Put([]byte("k"), []byte("a")); err != nil {
		t.Fatalf("parent set Put: %v", err)
	}

	child, _ := tx.BeginChild()
	csks, err := child.OpenSetKeyspace("set")
	if err != nil {
		t.Fatalf("child OpenSetKeyspace: %v", err)
	}
	if _, err := csks.Put([]byte("k"), []byte("b")); err != nil {
		t.Fatalf("child set Put: %v", err)
	}
	if err := child.Commit(); err != nil {
		t.Fatalf("child Commit: %v", err)
	}

	// Parent sees both members.
	has, err := sks.HasValue([]byte("k"), []byte("b"))
	if err != nil {
		t.Fatalf("HasValue b: %v", err)
	}
	if !has {
		t.Error("parent set missing child-added member b")
	}
	hasA, _ := sks.HasValue([]byte("k"), []byte("a"))
	if !hasA {
		t.Error("parent set missing pre-child member a")
	}
}

// TestChildRollbackSetKeyspaceIsolated: a rolled-back child set-keyspace
// write leaves the parent's set intact.
func TestChildRollbackSetKeyspaceIsolated(t *testing.T) {
	ctx := context.Background()
	db := openNestedTestDB(t)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	sks, err := tx.CreateSetKeyspace("set", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if _, err := sks.Put([]byte("k"), []byte("a")); err != nil {
		t.Fatalf("parent set Put: %v", err)
	}

	child, _ := tx.BeginChild()
	csks, _ := child.OpenSetKeyspace("set")
	if _, err := csks.Put([]byte("k"), []byte("b")); err != nil {
		t.Fatalf("child set Put: %v", err)
	}
	if err := child.Rollback(); err != nil {
		t.Fatalf("child Rollback: %v", err)
	}

	if has, _ := sks.HasValue([]byte("k"), []byte("b")); has {
		t.Error("parent set has rolled-back member b")
	}
	if has, _ := sks.HasValue([]byte("k"), []byte("a")); !has {
		t.Error("parent set lost member a after child rollback")
	}
}
