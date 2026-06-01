package gmdb

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// Chunk-5.4 tests promote four invariants over the keyspace surface:
//
//   Inv-A (clause-explicit): OpenKeyspace missing → ErrNotFound;
//          CreateKeyspace duplicate → ErrKeyExists.
//   Inv-B (entailed): CoW propagation — Create + Commit advances
//          meta.KeyspaceRoot + numKeyspaces; re-Open reads them back.
//   Inv-C (entailed): numKeyspaces == count of leaf entries in the
//          keyspace B+tree.
//   Inv-D (clause-explicit): ListKeyspaces filters Kind=2.
//
// Plus API-level inheritance of the codec-level keyspaces.md
// invariants: forged Kind=3 / Kind=1 / Kind=2 / FixedValueSize≠0
// descriptors on disk produce the right wrapped errors on OpenKeyspace.

func TestCreateOpenKeyspaceRoundTrip(t *testing.T) {
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
	ks, err := tx.CreateKeyspace("users")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if got := ks.Name(); got != "users" {
		t.Errorf("ks.Name() = %q, want %q", got, "users")
	}
	// Same-tx OpenKeyspace returns the cached handle (same pointer).
	ks2, err := tx.OpenKeyspace("users")
	if err != nil {
		t.Fatalf("OpenKeyspace same-tx: %v", err)
	}
	if ks2 != ks {
		t.Errorf("OpenKeyspace same-tx returned a different handle (cache miss)")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestCreateKeyspaceDuplicateReturnsErrKeyExists(t *testing.T) {
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

	if _, err := tx.CreateKeyspace("users"); err != nil {
		t.Fatalf("CreateKeyspace #1: %v", err)
	}
	if _, err := tx.CreateKeyspace("users"); !errors.Is(err, ErrKeyExists) {
		t.Errorf("CreateKeyspace duplicate: got %v, want ErrKeyExists", err)
	}
}

func TestOpenKeyspaceMissingReturnsErrNotFound(t *testing.T) {
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

	if _, err := tx.OpenKeyspace("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenKeyspace missing: got %v, want ErrNotFound", err)
	}
}

func TestCreateKeyspaceIfNotExistsOpenOrCreate(t *testing.T) {
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

	ks1, err := tx.CreateKeyspaceIfNotExists("ks")
	if err != nil {
		t.Fatalf("CreateKeyspaceIfNotExists #1 (create): %v", err)
	}
	ks2, err := tx.CreateKeyspaceIfNotExists("ks")
	if err != nil {
		t.Fatalf("CreateKeyspaceIfNotExists #2 (open): %v", err)
	}
	if ks2 != ks1 {
		t.Errorf("CreateKeyspaceIfNotExists second call returned a different handle (cache miss)")
	}
}

func TestEmptyNameReturnsErrKeyEmpty(t *testing.T) {
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

	if _, err := tx.OpenKeyspace(""); !errors.Is(err, ErrKeyEmpty) {
		t.Errorf("OpenKeyspace(empty): got %v, want ErrKeyEmpty", err)
	}
	if _, err := tx.CreateKeyspace(""); !errors.Is(err, ErrKeyEmpty) {
		t.Errorf("CreateKeyspace(empty): got %v, want ErrKeyEmpty", err)
	}
	if _, err := tx.CreateKeyspaceIfNotExists(""); !errors.Is(err, ErrKeyEmpty) {
		t.Errorf("CreateKeyspaceIfNotExists(empty): got %v, want ErrKeyEmpty", err)
	}
}

// TestKeyspaceRootCoWPropagatesAcrossCommit promotes Inv-B: Create +
// Commit advances meta.KeyspaceRoot and numKeyspaces; re-Open observes
// the new state.
func TestKeyspaceRootCoWPropagatesAcrossCommit(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)

	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if db.Meta().KeyspaceRoot != 0 {
		t.Fatalf("initial KeyspaceRoot = %d, want 0", db.Meta().KeyspaceRoot)
	}
	if db.Meta().NumKeyspaces != 0 {
		t.Fatalf("initial NumKeyspaces = %d, want 0", db.Meta().NumKeyspaces)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.CreateKeyspace("a"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if _, err := tx.CreateKeyspace("b"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	meta := db.Meta()
	if meta.KeyspaceRoot == 0 {
		t.Error("meta.KeyspaceRoot still 0 after Create + Commit")
	}
	if meta.NumKeyspaces != 2 {
		t.Errorf("meta.NumKeyspaces = %d, want 2", meta.NumKeyspaces)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-Open: meta should round-trip the keyspace state.
	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	if db2.Meta().KeyspaceRoot != meta.KeyspaceRoot {
		t.Errorf("re-Open KeyspaceRoot = %d, want %d (persisted)",
			db2.Meta().KeyspaceRoot, meta.KeyspaceRoot)
	}
	if db2.Meta().NumKeyspaces != 2 {
		t.Errorf("re-Open NumKeyspaces = %d, want 2", db2.Meta().NumKeyspaces)
	}
	tx2, err := db2.Begin(ctx)
	if err != nil {
		t.Fatalf("re-Open Begin: %v", err)
	}
	defer tx2.Rollback()
	if _, err := tx2.OpenKeyspace("a"); err != nil {
		t.Errorf("OpenKeyspace(a) after re-Open: %v", err)
	}
	if _, err := tx2.OpenKeyspace("b"); err != nil {
		t.Errorf("OpenKeyspace(b) after re-Open: %v", err)
	}
	if _, err := tx2.OpenKeyspace("c"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenKeyspace(c) after re-Open: got %v, want ErrNotFound", err)
	}
}

// TestListKeyspacesReturnsSortedNames promotes Inv-C: numKeyspaces
// equals the visible-name count; the list is in sorted byte order.
func TestListKeyspacesReturnsSortedNames(t *testing.T) {
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

	// Insert in reverse alphabetical order; List must return sorted.
	for _, n := range []string{"zebra", "delta", "alpha", "mango"} {
		if _, err := tx.CreateKeyspace(n); err != nil {
			t.Fatalf("CreateKeyspace(%s): %v", n, err)
		}
	}
	names, err := tx.ListKeyspaces()
	if err != nil {
		t.Fatalf("ListKeyspaces: %v", err)
	}
	want := []string{"alpha", "delta", "mango", "zebra"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("ListKeyspaces = %v, want %v", names, want)
	}
	if tx.numKeyspaces != uint64(len(want)) {
		t.Errorf("tx.numKeyspaces = %d, want %d", tx.numKeyspaces, len(want))
	}
}

func TestListKeyspacesEmpty(t *testing.T) {
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

	names, err := tx.ListKeyspaces()
	if err != nil {
		t.Fatalf("ListKeyspaces on empty: %v", err)
	}
	if names != nil {
		t.Errorf("ListKeyspaces empty = %v, want nil", names)
	}
}

// TestListKeyspacesFiltersKindIndexInternal promotes Inv-D. Forges a
// Kind=2 descriptor into the keyspace B+tree via the codec then
// asserts ListKeyspaces excludes it.
func TestListKeyspacesFiltersKindIndexInternal(t *testing.T) {
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

	// User-visible keyspace.
	if _, err := tx.CreateKeyspace("users"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// Engine-internal index keyspace forged at the codec level —
	// CreateKeyspace cannot produce Kind=2 (Kind is set internally
	// to KeyspaceKindKeyspace), so we write the descriptor directly
	// via the same internal storeDescriptor path with a hand-built
	// Kind=2 descriptor.
	forged := page.KeyspaceDescriptor{Kind: page.KeyspaceKindIndexInternal}
	if err := tx.storeDescriptor("__idx_internal", forged); err != nil {
		t.Fatalf("storeDescriptor Kind=2: %v", err)
	}
	tx.numKeyspaces++ // mirror CreateKeyspace's bookkeeping

	names, err := tx.ListKeyspaces()
	if err != nil {
		t.Fatalf("ListKeyspaces: %v", err)
	}
	want := []string{"users"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("ListKeyspaces = %v, want %v (Kind=2 should be filtered)", names, want)
	}
}

// TestOpenKeyspaceRejectsForgedKindMismatch promotes the API-level
// inheritance of keyspaces.md invariant #3 (ErrKeyspaceKindMismatch
// on Kind=1 stored when OpenKeyspace called).
func TestOpenKeyspaceRejectsForgedKindMismatch(t *testing.T) {
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

	// Forge a Kind=1 (SetKeyspace) descriptor — chunk 6 lands the
	// CreateSetKeyspace API, but the codec accepts Kind=1 today.
	forged := page.KeyspaceDescriptor{Kind: page.KeyspaceKindSetKeyspace}
	if err := tx.storeDescriptor("sks", forged); err != nil {
		t.Fatalf("storeDescriptor Kind=1: %v", err)
	}
	tx.numKeyspaces++

	if _, err := tx.OpenKeyspace("sks"); !errors.Is(err, ErrKeyspaceKindMismatch) {
		t.Errorf("OpenKeyspace(Kind=1): got %v, want ErrKeyspaceKindMismatch", err)
	}
}

// TestOpenKeyspaceRejectsForgedKindReserved promotes invariant #4
// (ErrKeyspaceReserved on Kind=2).
func TestOpenKeyspaceRejectsForgedKindReserved(t *testing.T) {
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

	forged := page.KeyspaceDescriptor{Kind: page.KeyspaceKindIndexInternal}
	if err := tx.storeDescriptor("__idx", forged); err != nil {
		t.Fatalf("storeDescriptor Kind=2: %v", err)
	}
	tx.numKeyspaces++

	if _, err := tx.OpenKeyspace("__idx"); !errors.Is(err, ErrKeyspaceReserved) {
		t.Errorf("OpenKeyspace(Kind=2): got %v, want ErrKeyspaceReserved", err)
	}
}

// TestOpenKeyspaceRejectsForgedUnknownKind promotes invariant #2 at
// the API level: a Kind byte forged out of {0,1,2} on disk results in
// OpenKeyspace returning a wrapped ErrCorrupted (via
// ValidateKeyspaceDescriptor → ErrCorrupted route).
func TestOpenKeyspaceRejectsForgedUnknownKind(t *testing.T) {
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

	// Write a valid Kind=0 descriptor first so the keyspace B+tree
	// exists, then forge a Kind=3 entry by encoding directly with a
	// raw buffer.
	if _, err := tx.CreateKeyspace("anchor"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	buf := make([]byte, page.KeyspaceDescriptorSize)
	page.EncodeKeyspaceDescriptor(buf, page.KeyspaceDescriptor{Kind: page.KeyspaceKindKeyspace})
	buf[16] = 3 // forge Kind byte to an unknown value
	cfg := tx.pgr.Config()
	newRoot, err := btree.Put(btreeWriter{tx.pgr}, cfg, tx.keyspaceRoot, []byte("bad"), buf)
	if err != nil {
		t.Fatalf("btree.Put forged: %v", err)
	}
	tx.keyspaceRoot = newRoot
	tx.numKeyspaces++

	_, err = tx.OpenKeyspace("bad")
	if err == nil || !errors.Is(err, ErrCorrupted) {
		t.Errorf("OpenKeyspace(forged Kind=3): got %v, want wrapped ErrCorrupted", err)
	}
}

// TestOpenKeyspaceRejectsForgedFixedValueSizeOnKind0 promotes the
// API-level inheritance of invariant #5 (FixedValueSize != 0 on
// Kind=0 stored).
func TestOpenKeyspaceRejectsForgedFixedValueSizeOnKind0(t *testing.T) {
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

	if _, err := tx.CreateKeyspace("anchor"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	buf := make([]byte, page.KeyspaceDescriptorSize)
	page.EncodeKeyspaceDescriptor(buf, page.KeyspaceDescriptor{
		Kind:           page.KeyspaceKindKeyspace,
		FixedValueSize: 8, // illegal on Kind=0
	})
	cfg := tx.pgr.Config()
	newRoot, err := btree.Put(btreeWriter{tx.pgr}, cfg, tx.keyspaceRoot, []byte("bad"), buf)
	if err != nil {
		t.Fatalf("btree.Put forged: %v", err)
	}
	tx.keyspaceRoot = newRoot
	tx.numKeyspaces++

	_, err = tx.OpenKeyspace("bad")
	if err == nil || !errors.Is(err, ErrCorrupted) {
		t.Errorf("OpenKeyspace(forged FVS=8 on Kind=0): got %v, want wrapped ErrCorrupted", err)
	}
}

func TestKeyspaceMethodsOnClosedTxReturnErrTxClosed(t *testing.T) {
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
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := tx.OpenKeyspace("x"); !errors.Is(err, ErrTxClosed) {
		t.Errorf("OpenKeyspace after Rollback: got %v, want ErrTxClosed", err)
	}
	if _, err := tx.CreateKeyspace("x"); !errors.Is(err, ErrTxClosed) {
		t.Errorf("CreateKeyspace after Rollback: got %v, want ErrTxClosed", err)
	}
	if _, err := tx.CreateKeyspaceIfNotExists("x"); !errors.Is(err, ErrTxClosed) {
		t.Errorf("CreateKeyspaceIfNotExists after Rollback: got %v, want ErrTxClosed", err)
	}
	if _, err := tx.ListKeyspaces(); !errors.Is(err, ErrTxClosed) {
		t.Errorf("ListKeyspaces after Rollback: got %v, want ErrTxClosed", err)
	}
}
