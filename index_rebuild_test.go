package gmdb

import (
	"context"
	"errors"
	"testing"
)

// --- Tx.RebuildIndex --------------------------------------------

// TestRebuildIndexBasicReplacesExtractor verifies the chunk-7.1
// RebuildIndex contract: re-running an extractor over existing rows
// produces fresh index entries; the registry entry's SchemaHash +
// Version reflect the new decl.
func TestRebuildIndexBasicReplacesExtractor(t *testing.T) {
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

	v1 := testDecl("by_color", "color")
	v1.Extract = firstByteExtract
	v1.Version = "v1"
	ks, err := tx.CreateKeyspace("items", v1)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if err := ks.Put([]byte(k), []byte{0x42, 'x'}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	beforeCount := ks.indexes["by_color"].count
	if beforeCount != 3 {
		t.Fatalf("pre-rebuild count: got %d want 3", beforeCount)
	}

	// Rebuild with a new Version (same schema, just a fresh decl).
	v2 := testDecl("by_color", "color")
	v2.Extract = firstByteExtract
	v2.Version = "v2"
	if err := tx.Indexes().Rebuild("items", v2); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	if got := ks.indexes["by_color"].count; got != 3 {
		t.Errorf("post-rebuild count: got %d want 3", got)
	}
	// SchemaHash unchanged (same schema), Version updated.
	if ks.indexes["by_color"].decl.Version != "v2" {
		t.Errorf("decl.Version not updated: %q", ks.indexes["by_color"].decl.Version)
	}
	// Verify Lookup finds the rebuilt entries.
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	n := 0
	for range idx.Lookup([]byte{0x42}) {
		n++
	}
	if idx.Err() != nil {
		t.Errorf("idx.Err: %v", idx.Err())
	}
	if n != 3 {
		t.Errorf("Lookup post-rebuild: got %d entries want 3", n)
	}
}

// TestRebuildIndexNilDeclReturnsErrInvalidOptions verifies nil decl
// rejection.
func TestRebuildIndexNilDeclReturnsErrInvalidOptions(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	if _, err := tx.CreateKeyspace("items"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	err = tx.Indexes().Rebuild("items", nil)
	if !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("nil decl: got %v want ErrInvalidOptions", err)
	}
}

// TestRebuildIndexNilExtractReturnsErrIndexExtractorRequired
// verifies the chunk-7.1 spec contract: decl.Extract MUST be
// non-nil per indexing.md §Rebuild.
func TestRebuildIndexNilExtractReturnsErrIndexExtractorRequired(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	if _, err := tx.CreateKeyspace("items"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	noExtract := &IndexDecl{
		Name:    "by_color",
		Columns: []IndexColumn{{Name: "color"}},
		Version: "v1",
		// Extract: nil
	}
	err = tx.Indexes().Rebuild("items", noExtract)
	if !errors.Is(err, ErrIndexExtractorRequired) {
		t.Errorf("nil Extract: got %v want ErrIndexExtractorRequired", err)
	}
}

// TestRebuildIndexMissingKeyspaceReturnsErrNotFound verifies the
// chunk-7.1 user-locked decision: keyspace-management dimension →
// ErrNotFound.
func TestRebuildIndexMissingKeyspaceReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	err = tx.Indexes().Rebuild("nonexistent", decl)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing keyspace: got %v want ErrNotFound", err)
	}
}

// TestRebuildIndexMissingIndexNameReturnsErrIndexNotFound verifies
// the chunk-7.1 user-locked decision: index-management dimension →
// ErrIndexNotFound.
func TestRebuildIndexMissingIndexNameReturnsErrIndexNotFound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	if _, err := tx.CreateKeyspace("items"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	decl := testDecl("by_nothing", "x")
	decl.Extract = firstByteExtract
	err = tx.Indexes().Rebuild("items", decl)
	if !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("missing index: got %v want ErrIndexNotFound", err)
	}
}

// TestRebuildIndexUniqueViolationFailsCleanly verifies that when
// the new extractor produces duplicate keys for a unique index,
// the rebuild aborts with ErrIndexUniqueViolation. Per indexing.md
// §Rebuild mechanics: the existing registry entry is unchanged on
// failure.
func TestRebuildIndexUniqueViolationFailsCleanly(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	v1 := testDecl("by_color", "color")
	v1.Unique = true
	v1.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", v1)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// Two rows with DIFFERENT colors (no conflict under v1).
	if err := ks.Put([]byte("a"), []byte{0x42}); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if err := ks.Put([]byte("b"), []byte{0x43}); err != nil {
		t.Fatalf("Put b: %v", err)
	}
	prevRoot := ks.indexes["by_color"].root
	// Rebuild with a buggy extractor that outputs the same column
	// for both rows — should conflict.
	v2 := testDecl("by_color", "color")
	v2.Unique = true
	v2.Extract = func(_, _ []byte) []IndexEntry {
		return []IndexEntry{{Cols: [][]byte{{0xFF}}}}
	}
	err = tx.Indexes().Rebuild("items", v2)
	if !errors.Is(err, ErrIndexUniqueViolation) {
		t.Fatalf("expected ErrIndexUniqueViolation, got %v", err)
	}
	// Existing registry entry unchanged: pinned still points at
	// the old root + old Version.
	if ks.indexes["by_color"].root != prevRoot {
		t.Errorf("registry mutated on failed rebuild: pinned.root changed from %d", prevRoot)
	}
}

// TestRebuildIndexEmptyKeyspaceProducesEmptyIndex verifies the
// fast-path for a keyspace with no rows: rebuild produces an
// empty index data tree (Root=0, Count=0).
func TestRebuildIndexEmptyKeyspaceProducesEmptyIndex(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	v2 := testDecl("by_color", "color")
	v2.Extract = firstByteExtract
	v2.Version = "v2"
	if err := tx.Indexes().Rebuild("items", v2); err != nil {
		t.Fatalf("RebuildIndex on empty: %v", err)
	}
	p := ks.indexes["by_color"]
	if p.count != 0 || p.root != 0 {
		t.Errorf("post-rebuild empty: count=%d root=%d want 0/0", p.count, p.root)
	}
	if p.decl.Version != "v2" {
		t.Errorf("Version not updated: %q", p.decl.Version)
	}
}

// --- Tx.DropIndex -----------------------------------------------

// TestDropIndexRemovesEntry verifies the chunk-7.8 DropIndex
// happy path: registry entry removed, pinned set updated.
func TestDropIndexRemovesEntry(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("k"), []byte{0x42}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := tx.Indexes().Drop("items", "by_color"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	if _, ok := ks.indexes["by_color"]; ok {
		t.Errorf("pinned set still has by_color after DropIndex")
	}
	// Verify the index is gone from the on-disk registry.
	_, err = tx.registryGet(ks, "by_color")
	if !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("registryGet after drop: got %v want ErrIndexNotFound", err)
	}
}

// TestDropIndexLastResetsIndexRegistryRoot verifies the chunk-7.1
// indexing.md entailed invariant on empty-registry canonical-at-
// zero: DropIndex of the last declared index resets
// desc.IndexRegistryRoot to 0.
func TestDropIndexLastResetsIndexRegistryRoot(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if ks.desc.IndexRegistryRoot == 0 {
		t.Fatalf("IndexRegistryRoot 0 after CreateKeyspace with index")
	}
	if err := tx.Indexes().Drop("items", "by_color"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	if ks.desc.IndexRegistryRoot != 0 {
		t.Errorf("IndexRegistryRoot after last DropIndex: got %d want 0 (entailed-invariant violation)",
			ks.desc.IndexRegistryRoot)
	}
}

// TestDropIndexMissingKeyspaceReturnsErrNotFound.
func TestDropIndexMissingKeyspaceReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	err = tx.Indexes().Drop("nonexistent", "by_color")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing keyspace: got %v want ErrNotFound", err)
	}
}

// TestDropIndexMissingIndexNameReturnsErrIndexNotFound.
func TestDropIndexMissingIndexNameReturnsErrIndexNotFound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	if _, err := tx.CreateKeyspace("items"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	err = tx.Indexes().Drop("items", "by_nothing")
	if !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("missing index: got %v want ErrIndexNotFound", err)
	}
}

// --- Chunk 7.10: SetKeyspace Rebuild/Drop -------------------------
//
// These tests previously verified the chunk-7.8 H-1 gate that
// rejected SetKeyspace Rebuild/Drop with ErrInvalidOptions. The
// gate was removed at chunk-7.10; the tests now assert the
// positive behavior (Rebuild/Drop succeed on SetKeyspace).

// TestRebuildIndexOnSetKeyspaceSucceeds verifies the chunk-7.10
// SetKeyspace RebuildIndex path: per-(setKey, setValue) extractor
// invocation via SetCursor; compound-PK index entry encoding.
func TestRebuildIndexOnSetKeyspaceSucceeds(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	v1 := testDecl("by_topic", "topic")
	v1.Extract = setKeyspaceFirstByteExtract
	v1.Version = "v1"
	sks, err := tx.CreateSetKeyspace("subs", nil, v1)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if _, err := sks.Put([]byte("u1"), []byte("alpha")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Rebuild with a fresh version.
	v2 := testDecl("by_topic", "topic")
	v2.Extract = setKeyspaceFirstByteExtract
	v2.Version = "v2"
	if err := tx.Indexes().Rebuild("subs", v2); err != nil {
		t.Fatalf("RebuildIndex SetKeyspace: %v", err)
	}
	if sks.indexes["by_topic"].count != 1 {
		t.Errorf("post-rebuild count: got %d want 1", sks.indexes["by_topic"].count)
	}
	if sks.indexes["by_topic"].decl.Version != "v2" {
		t.Errorf("decl.Version not updated: %q", sks.indexes["by_topic"].decl.Version)
	}
}

// TestDropIndexOnSetKeyspaceSucceeds verifies the chunk-7.10
// SetKeyspace DropIndex path inherits the chunk-7.8 generic logic.
func TestDropIndexOnSetKeyspaceSucceeds(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_topic", "topic")
	decl.Extract = setKeyspaceFirstByteExtract
	sks, err := tx.CreateSetKeyspace("subs", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if _, err := sks.Put([]byte("u1"), []byte("alpha")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := tx.Indexes().Drop("subs", "by_topic"); err != nil {
		t.Fatalf("DropIndex SetKeyspace: %v", err)
	}
	if _, ok := sks.indexes["by_topic"]; ok {
		t.Errorf("pinned set still has by_topic after DropIndex")
	}
	if sks.desc.IndexRegistryRoot != 0 {
		t.Errorf("IndexRegistryRoot after last DropIndex: got %d want 0",
			sks.desc.IndexRegistryRoot)
	}
}

// --- L-2: rebuild visibility against same-tx Puts ----------------

// TestRebuildIndexSeesSameTxPuts verifies the chunk-7.8 spec
// contract from indexing.md §Rebuild mechanics: "The internal
// cursor sees the current write transaction's dirty state — rows
// Put earlier in the same transaction are included in the rebuilt
// index."
func TestRebuildIndexSeesSameTxPuts(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	v1 := testDecl("by_color", "color")
	// Extract returns NOTHING under v1 (so the index is empty
	// after the initial Puts).
	v1.Extract = func(_, _ []byte) []IndexEntry { return nil }
	ks, err := tx.CreateKeyspace("items", v1)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if err := ks.Put([]byte(k), []byte{0x42}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	if ks.indexes["by_color"].count != 0 {
		t.Fatalf("pre-rebuild count: got %d want 0 (v1 emits nothing)", ks.indexes["by_color"].count)
	}
	// Now rebuild with a SECOND extractor that DOES emit entries
	// — the rebuild must see the 3 same-tx Put rows.
	v2 := testDecl("by_color", "color")
	v2.Extract = firstByteExtract
	v2.Version = "v2"
	if err := tx.Indexes().Rebuild("items", v2); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	if got := ks.indexes["by_color"].count; got != 3 {
		t.Errorf("post-rebuild count: got %d want 3 (same-tx Puts must be visible)", got)
	}
}

// --- L-3: DropIndex same-tx visibility ---------------------------

// TestDropIndexThenReopenWithSameDeclErrIndexUnknown verifies
// indexing.md §Removing an Index: "Future OpenKeyspace calls must
// omit the corresponding IndexDecl." After DropIndex within the
// same tx, the keyspace must reject re-opening with the dropped
// decl. The check goes via a fresh same-tx OpenKeyspace which
// hits the cache; the cache no longer has the dropped index in
// its pinned set, so reopening with the dropped decl produces
// ErrKeyspaceAlreadyOpen (the cached state's index-set differs
// from the supplied set).
func TestDropIndexThenReopenWithSameDeclErrIndexUnknown(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, _ := db.Begin(ctx)
		decl := testDecl("by_color", "color")
		decl.Extract = firstByteExtract
		if _, err := tx.CreateKeyspace("items", decl); err != nil {
			t.Fatalf("CreateKeyspace: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		_ = db.Close()
	}
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open #2: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	if _, err := tx.OpenKeyspace("items", decl); err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	if err := tx.Indexes().Drop("items", "by_color"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	// Subsequent OpenKeyspace with the now-dropped decl: the
	// cache hit produces ErrKeyspaceAlreadyOpen because the
	// cached *Keyspace's indexes (now empty post-drop) doesn't
	// match the supplied {decl}.
	if _, err := tx.OpenKeyspace("items", decl); !errors.Is(err, ErrKeyspaceAlreadyOpen) {
		t.Errorf("post-drop re-open with dropped decl: got %v want ErrKeyspaceAlreadyOpen", err)
	}
}

// --- L-4: not-cached path coverage --------------------------------

// TestRebuildIndexNotCachedPathPersists verifies the chunk-7.8
// not-cached RebuildIndex path: a fresh tx that has NEVER
// OpenKeyspace'd "items" can still RebuildIndex it (the spec's
// recovery-after-FingerprintMismatch loop pattern, where
// OpenKeyspace fails BEFORE caching). The rebuild writes the
// updated descriptor to tx.dirtyDescriptors via descAdapterValue;
// flushKeyspaces persists it at Commit.
func TestRebuildIndexNotCachedPathPersists(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, _ := db.Begin(ctx)
		v1 := testDecl("by_color", "color")
		v1.Extract = firstByteExtract
		v1.Version = "v1"
		ks, err := tx.CreateKeyspace("items", v1)
		if err != nil {
			t.Fatalf("CreateKeyspace: %v", err)
		}
		if err := ks.Put([]byte("a"), []byte{0x42}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		_ = db.Close()
	}
	// Tx 2: NEVER OpenKeyspace; just RebuildIndex.
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open #2: %v", err)
		}
		tx, _ := db.Begin(ctx)
		v2 := testDecl("by_color", "color")
		v2.Extract = firstByteExtract
		v2.Version = "v2"
		if err := tx.Indexes().Rebuild("items", v2); err != nil {
			t.Fatalf("RebuildIndex not-cached: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit #2: %v", err)
		}
		_ = db.Close()
	}
	// Tx 3: OpenKeyspace with v2 — must succeed (registry now
	// has v2's Version + schema-hash).
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open #3: %v", err)
		}
		defer db.Close()
		tx, _ := db.Begin(ctx)
		defer tx.Rollback()
		v2 := testDecl("by_color", "color")
		v2.Extract = firstByteExtract
		v2.Version = "v2"
		ks, err := tx.OpenKeyspace("items", v2)
		if err != nil {
			t.Fatalf("OpenKeyspace post-rebuild: %v", err)
		}
		if ks.indexes["by_color"].count != 1 {
			t.Errorf("post-rebuild count: got %d want 1", ks.indexes["by_color"].count)
		}
	}
}

// TestDropIndexNotCachedPathPersists mirrors the L-4 not-cached
// path for DropIndex.
func TestDropIndexNotCachedPathPersists(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, _ := db.Begin(ctx)
		decl := testDecl("by_color", "color")
		decl.Extract = firstByteExtract
		if _, err := tx.CreateKeyspace("items", decl); err != nil {
			t.Fatalf("CreateKeyspace: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		_ = db.Close()
	}
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open #2: %v", err)
		}
		tx, _ := db.Begin(ctx)
		if err := tx.Indexes().Drop("items", "by_color"); err != nil {
			t.Fatalf("DropIndex not-cached: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit #2: %v", err)
		}
		_ = db.Close()
	}
	// Tx 3: OpenKeyspace without the dropped decl — must succeed.
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open #3: %v", err)
		}
		defer db.Close()
		tx, _ := db.Begin(ctx)
		defer tx.Rollback()
		if _, err := tx.OpenKeyspace("items"); err != nil {
			t.Fatalf("OpenKeyspace post-drop (no decl): %v", err)
		}
	}
}

// --- DeleteKeyspace three-subtree retirement --------------------

// TestDeleteKeyspaceWithIndexesRetiresAllSubtrees verifies the
// chunk-7.8 three-subtree retirement: DeleteKeyspace on an
// indexed keyspace successfully retires (1) data subtree,
// (2) per-index Kind=2 data trees, (3) registry sub-tree, all
// in one atomic CoW tx.
func TestDeleteKeyspaceWithIndexesRetiresAllSubtrees(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()

	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if err := ks.Put([]byte(k), []byte{0x42}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	if ks.desc.IndexRegistryRoot == 0 {
		t.Fatalf("IndexRegistryRoot 0 after CreateKeyspace with index")
	}
	// DeleteKeyspace previously failed with ErrCorrupted under
	// chunk-5.6's defensive gate; chunk-7.8 must succeed.
	if err := tx.DeleteKeyspace("items"); err != nil {
		t.Fatalf("DeleteKeyspace with indexes: %v (chunk-5.6 defensive gate not replaced)", err)
	}
	// The keyspace must be invisible to subsequent ListKeyspaces.
	names, err := tx.ListKeyspaces()
	if err != nil {
		t.Fatalf("ListKeyspaces: %v", err)
	}
	for _, n := range names {
		if n == "items" {
			t.Errorf("DeleteKeyspace did not remove %q from ListKeyspaces", n)
		}
	}
}

// TestDeleteKeyspacePersistsAcrossCommit verifies that the
// three-subtree retirement survives Commit + re-Open.
func TestDeleteKeyspacePersistsAcrossCommit(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, _ := db.Begin(ctx)
		decl := testDecl("by_color", "color")
		decl.Extract = firstByteExtract
		ks, err := tx.CreateKeyspace("items", decl)
		if err != nil {
			t.Fatalf("CreateKeyspace: %v", err)
		}
		if err := ks.Put([]byte("a"), []byte{0x42}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit #1: %v", err)
		}
		_ = db.Close()
	}
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open #2: %v", err)
		}
		tx, _ := db.Begin(ctx)
		if err := tx.DeleteKeyspace("items"); err != nil {
			t.Fatalf("DeleteKeyspace #2: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit #2: %v", err)
		}
		_ = db.Close()
	}
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open #3: %v", err)
		}
		defer db.Close()
		tx, _ := db.Begin(ctx)
		defer tx.Rollback()
		names, err := tx.ListKeyspaces()
		if err != nil {
			t.Fatalf("ListKeyspaces post-delete: %v", err)
		}
		for _, n := range names {
			if n == "items" {
				t.Errorf("DeleteKeyspace did not survive Commit: %q still in ListKeyspaces", n)
			}
		}
	}
}

// assertNoBitmapCorruption fails the test if db.Check() reports any
// issue that signals page-level inconsistency between the committed
// reachable set and the bitmap — BitmapLeak (allocated-but-unreferenced),
// ReachableButFree (referenced-but-marked-free), ReachableInRPL
// (referenced-but-pending-reclamation), or FreeAndPending (free AND in
// RPL: future double-allocation hazard). Different partial-failure
// shapes (cached vs not-cached descriptor path) produce different
// corruption codes — all four indicate the write-helper's atomicity
// contract has been violated.
func assertNoBitmapCorruption(t *testing.T, db *DB, site string) {
	t.Helper()
	bad := map[string]bool{
		"BitmapLeak":       true,
		"ReachableButFree": true,
		"ReachableInRPL":   true,
		"FreeAndPending":   true,
	}
	var problems []CheckIssue
	for _, iss := range collectIssues(db.Check()) {
		if bad[iss.Code] {
			problems = append(problems, iss)
		}
	}
	if len(problems) != 0 {
		t.Errorf("%s violated atomicity contract — %d Check issue(s): %v",
			site, len(problems), problems)
	}
}

// TestRebuildIndexAtomicOnPartialFailure pins the chunk-7.8 DDL
// write-helper atomicity contract (transactions.md §Write-helper error
// contract): a mid-rebuild failure must not orphan any pages on
// Tx.Commit (the rest-of-tx-continues path). The failure is injected
// after the publish-then-retire registryPut succeeded (the registry
// now points at the freshly-built newRoot) but before the OLD index
// data tree is freed — the H-2 ordering's worst-case window, where
// without the savepoint wrap newRoot's pages would be orphaned (the
// restored descriptor must NOT keep pointing at them) and the OLD
// tree would remain partially live. Check() must report no bitmap-
// to-tree inconsistency after Commit.
func TestRebuildIndexAtomicOnPartialFailure(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// First tx: create + populate so the rebuild has a real OLD tree.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	v1 := testDecl("by_color", "color")
	v1.Extract = firstByteExtract
	v1.Version = "v1"
	ks, err := tx.CreateKeyspace("items", v1)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		if err := ks.Put([]byte(k), []byte{0x42, 'x'}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit setup: %v", err)
	}

	// Second tx: inject the failure during RebuildIndex.
	injected := errors.New("injected rebuild failure")
	setRebuildIndexFailHookForTest(func() error { return injected })
	t.Cleanup(func() { setRebuildIndexFailHookForTest(nil) })

	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin 2: %v", err)
	}
	// Cache the keyspace handle so registryPut mutates ks.desc
	// directly (uniform cached-path: ks.markDirty + flushKeyspaces
	// publishes the new descriptor at Commit, surfacing the orphans
	// as BitmapLeak under neuter).
	v1Open := testDecl("by_color", "color")
	v1Open.Extract = firstByteExtract
	v1Open.Version = "v1"
	if _, err := tx.OpenKeyspace("items", v1Open); err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	v2 := testDecl("by_color", "color")
	v2.Extract = firstByteExtract
	v2.Version = "v2"
	if err := tx.Indexes().Rebuild("items", v2); !errors.Is(err, injected) {
		tx.Rollback()
		t.Fatalf("RebuildIndex err = %v, want injected failure", err)
	}
	// Rest-of-tx-continues: commit despite the per-op error.
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after injected failure: %v", err)
	}

	assertNoBitmapCorruption(t, db, "RebuildIndex")
}

// TestDropIndexAtomicOnPartialFailure pins the DDL atomicity contract
// for Tx.DropIndex: a failure between the publish-then-retire
// registryDelete (which advances the registry off the entry) and the
// FreeSubtree of the OLD data tree must not orphan the data tree on
// Tx.Commit. Without the savepoint wrap, registryDelete already
// updated desc.IndexRegistryRoot — the data tree pages are still
// allocated yet unreferenced. Check() must report zero BitmapLeak.
func TestDropIndexAtomicOnPartialFailure(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Setup tx: create + populate so DropIndex has a non-trivial data
	// tree to retire.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		if err := ks.Put([]byte(k), []byte{0x42, 'x'}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit setup: %v", err)
	}

	injected := errors.New("injected drop failure")
	setDropIndexFailHookForTest(func() error { return injected })
	t.Cleanup(func() { setDropIndexFailHookForTest(nil) })

	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin 2: %v", err)
	}
	// Not-cached path (no OpenKeyspace in this tx): on error,
	// propagateNotCachedDescChange does NOT run, so the on-disk
	// descriptor stays pointing at the OLD IndexRegistryRoot — which
	// registryDelete has already mutated in the bitmap (the registry
	// shrank to empty, freeing the old leaf into retired/RPL).
	// Under neuter the bitmap and the descriptor disagree →
	// ReachableInRPL on the old registry leaf. The cached-path
	// alternative is masked by flushKeyspaces's flushIndexRegistry
	// rebuild, which re-writes the registry from ks.indexes (still
	// pinned) so the leak vanishes from the committed state — a
	// faulty test, not a safe outcome.
	if err := tx.Indexes().Drop("items", "by_color"); !errors.Is(err, injected) {
		tx.Rollback()
		t.Fatalf("DropIndex err = %v, want injected failure", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after injected failure: %v", err)
	}

	assertNoBitmapCorruption(t, db, "DropIndex")
}
