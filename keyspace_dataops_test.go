package gmdb

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// Chunk-5.5 tests promote four data-op invariants:
//
//   Inv-A:  Put-then-Get round-trips the value.
//   Inv-B:  descriptor CoW → meta.KeyspaceRoot across commit;
//           reopen finds the data.
//   Inv-C:  descriptor.Count tracks leaf-entry count.
//   Inv-D:  Sibling cursor MarkStale'd by Put/Delete (curKey/curValue
//           nilled; subsequent ops return ErrCursorStale).
//
// Plus the API-level Delete-on-miss invariant (Kind=0 portion).

func TestKeyspacePutGetRoundTrip(t *testing.T) {
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

	ks, err := tx.CreateKeyspace("ks")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("hello"), []byte("world")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := ks.Get([]byte("hello"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, []byte("world")) {
		t.Errorf("Get = %q, want %q", got, "world")
	}
}

func TestKeyspaceGetMissingReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()

	ks, _ := tx.CreateKeyspace("ks")
	if _, err := ks.Get([]byte("missing")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing: got %v, want ErrNotFound", err)
	}
}

// TestKeyspaceDeleteMissingReturnsErrNotFound promotes the chunk-5.1
// Delete-on-miss invariant at the Keyspace.Delete surface.
func TestKeyspaceDeleteMissingReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	if err := ks.Delete([]byte("missing")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing: got %v, want ErrNotFound", err)
	}
}

func TestKeyspaceEmptyKeyReturnsErrKeyEmpty(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	for _, k := range [][]byte{nil, {}} {
		if _, err := ks.Get(k); !errors.Is(err, ErrKeyEmpty) {
			t.Errorf("Get(empty): got %v", err)
		}
		if err := ks.Put(k, []byte("v")); !errors.Is(err, ErrKeyEmpty) {
			t.Errorf("Put(empty): got %v", err)
		}
		if err := ks.Delete(k); !errors.Is(err, ErrKeyEmpty) {
			t.Errorf("Delete(empty): got %v", err)
		}
	}
}

// Inv-C: descriptor.Count tracks leaf-entry count.
func TestKeyspaceCountTracksLeafEntries(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")

	if ks.desc.Count != 0 {
		t.Fatalf("initial Count = %d, want 0", ks.desc.Count)
	}
	for i := 0; i < 5; i++ {
		_ = ks.Put([]byte{byte('a' + i)}, []byte("v"))
	}
	if ks.desc.Count != 5 {
		t.Errorf("Count after 5 Puts = %d, want 5", ks.desc.Count)
	}
	// Put-replace does not bump Count.
	_ = ks.Put([]byte("a"), []byte("v2"))
	if ks.desc.Count != 5 {
		t.Errorf("Count after replace = %d, want 5", ks.desc.Count)
	}
	// Delete decrements.
	_ = ks.Delete([]byte("c"))
	if ks.desc.Count != 4 {
		t.Errorf("Count after Delete = %d, want 4", ks.desc.Count)
	}
}

// Inv-B: CoW propagation across commit + reopen.
func TestKeyspacePutCommitReopenRetrieves(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("ks")
	if err := ks.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	_ = db.Close()

	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	tx2, _ := db2.Begin(ctx)
	defer tx2.Rollback()
	ks2, err := tx2.OpenKeyspace("ks")
	if err != nil {
		t.Fatalf("OpenKeyspace after reopen: %v", err)
	}
	got, err := ks2.Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if !bytes.Equal(got, []byte("v")) {
		t.Errorf("Get after reopen = %q, want %q", got, "v")
	}
}

// Inv-D: sibling cursor MarkStale'd by Put/Delete.
func TestCursorMarkStaleAfterSiblingPut(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	_ = ks.Put([]byte("a"), []byte("1"))
	_ = ks.Put([]byte("b"), []byte("2"))

	c := ks.Cursor()
	k, _ := c.First()
	if !bytes.Equal(k, []byte("a")) {
		t.Fatalf("First() = %q, want %q", k, "a")
	}

	// Sibling Put — should MarkStale c.
	if err := ks.Put([]byte("c"), []byte("3")); err != nil {
		t.Fatalf("sibling Put: %v", err)
	}
	// Next on the stale cursor must surface ErrCursorStale.
	k, _ = c.Next()
	if k != nil {
		t.Errorf("Next on stale cursor = %q, want nil", k)
	}
	if !errors.Is(c.Err(), ErrCursorStale) {
		t.Errorf("Err on stale cursor = %v, want ErrCursorStale", c.Err())
	}
	// Re-position recovers — and observes the sibling-Put'd "c".
	// Chunk-5.7 adjacent-fix: prior to the cursor.SetRootID +
	// markCursorsStale-refresh pair, the re-position would have
	// descended from the now-retired old root and silently read
	// stale (or corrupted post-loose-pop) data.
	k, _ = c.First()
	if !bytes.Equal(k, []byte("a")) {
		t.Errorf("Re-First after sibling Put = %q, want a", k)
	}
	if err := c.Err(); err != nil {
		t.Errorf("Err after re-position = %v, want nil", err)
	}
	// Walk to confirm the Put'd "c" is visible.
	saw := map[string]struct{}{}
	for k != nil {
		saw[string(k)] = struct{}{}
		k, _ = c.Next()
	}
	for _, want := range []string{"a", "b", "c"} {
		if _, ok := saw[want]; !ok {
			t.Errorf("cursor walk missing %q after sibling-Put + re-position", want)
		}
	}
}

// SetKeyspaceConfig: Inv-E.
func TestSetKeyspaceConfigUpdatesRestartGroupTarget(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	if ks.desc.RestartGroupTarget != 0 {
		t.Fatalf("initial RestartGroupTarget = %d, want 0", ks.desc.RestartGroupTarget)
	}
	if err := tx.SetKeyspaceConfig("ks", KeyspaceConfig{RestartGroupTarget: 32}); err != nil {
		t.Fatalf("SetKeyspaceConfig: %v", err)
	}
	// Lookup via the in-memory-aware path — the chunk-5.6 deferred-
	// flush refactor moves descriptor persistence to Commit time, so
	// loadDescriptor (disk-only) returns the pre-config state until
	// the flush walk runs. lookupDescriptor consults openKeyspaces
	// and dirtyDescriptors first per the chunk-5.6 contract.
	desc, found, err := tx.lookupDescriptor("ks")
	if err != nil || !found {
		t.Fatalf("lookupDescriptor: found=%v err=%v", found, err)
	}
	if desc.RestartGroupTarget != 32 {
		t.Errorf("descriptor RestartGroupTarget = %d, want 32", desc.RestartGroupTarget)
	}
	// Cache also reflects.
	if ks.desc.RestartGroupTarget != 32 {
		t.Errorf("cached handle RestartGroupTarget = %d, want 32", ks.desc.RestartGroupTarget)
	}
}

func TestSetKeyspaceConfigZeroIsNoOp(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	_, _ = tx.CreateKeyspace("ks")
	desc, _, _ := tx.loadDescriptor("ks")
	rootBefore := tx.keyspaceRoot
	_ = desc

	if err := tx.SetKeyspaceConfig("ks", KeyspaceConfig{}); err != nil {
		t.Fatalf("SetKeyspaceConfig zero: %v", err)
	}
	if tx.keyspaceRoot != rootBefore {
		t.Errorf("zero-config SetKeyspaceConfig CoW'd the keyspace B+tree")
	}
}

func TestSetKeyspaceConfigOversizeReturnsErrInvalidOptions(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	_, _ = tx.CreateKeyspace("ks")
	if err := tx.SetKeyspaceConfig("ks", KeyspaceConfig{RestartGroupTarget: 256}); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("RestartGroupTarget=256: got %v, want ErrInvalidOptions", err)
	}
}

func TestSetKeyspaceConfigMissingReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	if err := tx.SetKeyspaceConfig("missing", KeyspaceConfig{RestartGroupTarget: 32}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing keyspace: got %v, want ErrNotFound", err)
	}
}

// Cursor walk: round-trip via cursor.
func TestCursorWalkRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	for _, k := range []string{"c", "a", "b"} {
		_ = ks.Put([]byte(k), []byte("v-"+k))
	}
	c := ks.Cursor()
	var seen []string
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		seen = append(seen, string(k))
	}
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("cursor walk = %v, want %v", seen, want)
	}
}

func TestCursorDeleteAdvancesToSuccessor(t *testing.T) {
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
	c.First()
	k, _ := c.Current()
	if !bytes.Equal(k, []byte("a")) {
		t.Fatalf("Current = %q, want a", k)
	}
	if err := c.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Post-delete: cursor at successor "b".
	k, _ = c.Current()
	if !bytes.Equal(k, []byte("b")) {
		t.Errorf("post-Delete Current = %q, want b", k)
	}
	// Descriptor Count decremented.
	if ks.desc.Count != 2 {
		t.Errorf("Count after cursor Delete = %d, want 2", ks.desc.Count)
	}
}

// Options.Logger plumb-through smoke test: a non-nil logger captures
// the cleanup warning.
func TestOptionsLoggerCapturesLeakWarning(t *testing.T) {
	// We can't easily intercept the cleanup since it fires on GC,
	// but we can verify Options.Logger is captured at Open and
	// available via the cleanup-info plumbing. The semantic
	// pinning lives in the existing leak-detection tests; this
	// test just confirms the field is set.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 128,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if db.logger == nil {
		t.Error("db.logger is nil; Open did not install discard fallback")
	}
}

// TestCursorAfterRollbackReturnsErrTxClosed pins H2 fix: Cursor
// methods refuse to read after the parent tx has closed.
func TestCursorAfterRollbackReturnsErrTxClosed(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("ks")
	_ = ks.Put([]byte("a"), []byte("v"))
	c := ks.Cursor()
	c.First()

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if k, _ := c.Next(); k != nil {
		t.Errorf("Next on closed tx returned key %q, want nil", k)
	}
	if err := c.Err(); !errors.Is(err, ErrTxClosed) {
		t.Errorf("Err on closed tx = %v, want ErrTxClosed", err)
	}
	// Mutating ops also refuse.
	if err := c.Delete(); !errors.Is(err, ErrTxClosed) {
		t.Errorf("Delete on closed tx = %v, want ErrTxClosed", err)
	}
}

// TestKeyspacePutHonorsPerKeyspaceRestartGroupTarget pins H3 fix:
// SetKeyspaceConfig's RestartGroupTarget value reaches the leaf
// builder. Verify by observing the leaf-page Type byte: target=1
// produces TypeLeafUncompressed; target>=2 produces TypeLeaf.
func TestKeyspacePutHonorsPerKeyspaceRestartGroupTarget(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")

	// Default RGT=0 (engine default = 16 = compressed leaf).
	_ = ks.Put([]byte("a"), []byte("v"))
	leafBuf := tx.pgr.PageRaw(ks.desc.Root)
	if !page.IsLeafType(leafBuf[0]) {
		t.Fatalf("root is not a leaf: type=%d", leafBuf[0])
	}
	if leafBuf[0] != page.TypeLeaf {
		t.Errorf("default RGT: leaf type = %d, want %d (TypeLeaf compressed)",
			leafBuf[0], page.TypeLeaf)
	}

	// Set RGT=1 → next leaf should be uncompressed.
	if err := tx.SetKeyspaceConfig("ks", KeyspaceConfig{RestartGroupTarget: 1}); err != nil {
		t.Fatalf("SetKeyspaceConfig: %v", err)
	}
	// Write enough to force a split or rewrite — Put on an existing
	// leaf CoWs it and re-encodes with the new builder cfg.
	_ = ks.Put([]byte("b"), []byte("v"))
	leafBuf = tx.pgr.PageRaw(ks.desc.Root)
	if leafBuf[0] != page.TypeLeafUncompressed {
		t.Errorf("after SetKeyspaceConfig(RGT=1): leaf type = %d, "+
			"want %d (TypeLeafUncompressed) — per-keyspace RGT not "+
			"reaching the builder", leafBuf[0], page.TypeLeafUncompressed)
	}
}
