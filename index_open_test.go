package gmdb

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/descriptor"
	"github.com/thegrumpylion/gmdb/internal/indexing"
)

// Convenience IndexDecl factory for tests.
func testDecl(name string, columns ...string) *IndexDecl {
	cols := make([]IndexColumn, len(columns))
	for i, c := range columns {
		cols[i] = IndexColumn{Name: c}
	}
	return &IndexDecl{
		Name:    name,
		Columns: cols,
		Version: "v1",
		Extract: func(_, _ []byte) []IndexEntry { return nil },
	}
}

// --- CreateKeyspace with indexes ----------------------------------

// TestCreateKeyspaceWithIndexAllocatesRegistry verifies that
// CreateKeyspace("ks", decl) allocates the registry sub-tree and
// the descriptor's IndexRegistryRoot becomes non-zero before
// commit. The pinned-index map is populated on the *Keyspace.
func TestCreateKeyspaceWithIndexAllocatesRegistry(t *testing.T) {
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

	ks, err := tx.CreateKeyspace("users", testDecl("by_owner", "owner"))
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if ks.desc.IndexRegistryRoot == 0 {
		t.Fatalf("IndexRegistryRoot still 0 after CreateKeyspace with index")
	}
	if len(ks.indexes) != 1 {
		t.Fatalf("ks.indexes len=%d, want 1", len(ks.indexes))
	}
	p, ok := ks.indexes["by_owner"]
	if !ok {
		t.Fatalf("ks.indexes['by_owner'] missing")
	}
	if p.schemaHash == 0 {
		t.Errorf("pinnedIndex.schemaHash unset")
	}
}

// TestCreateKeyspaceWithMultipleIndexesAllRegistered verifies that
// multiple IndexDecls each get a registry entry.
func TestCreateKeyspaceWithMultipleIndexesAllRegistered(t *testing.T) {
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

	ks, err := tx.CreateKeyspace("users",
		testDecl("by_owner", "owner"),
		testDecl("by_repo", "repo"),
	)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	names, err := tx.registryList(ks)
	if err != nil {
		t.Fatalf("registryList: %v", err)
	}
	if len(names) != 2 || names[0] != "by_owner" || names[1] != "by_repo" {
		t.Errorf("registry names: got %v want [by_owner by_repo]", names)
	}
}

// TestCreateKeyspaceWithDuplicateIndexNameReturnsErrIndexExists
// verifies the validateIndexDecls integration: two decls
// with the same Name → ErrIndexExists.
func TestCreateKeyspaceWithDuplicateIndexNameReturnsErrIndexExists(t *testing.T) {
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

	_, err = tx.CreateKeyspace("users",
		testDecl("by_x", "a"),
		testDecl("by_x", "b"),
	)
	if !errors.Is(err, ErrIndexExists) {
		t.Errorf("duplicate index name: got %v want ErrIndexExists", err)
	}
}

// --- OpenKeyspace validation against stored registry -------------

// TestOpenKeyspaceMissingDeclReturnsErrIndexExtractorRequired
// verifies that opening a keyspace with declared indexes but no
// matching IndexDecls returns ErrIndexExtractorRequired naming
// the registry's index.
func TestOpenKeyspaceMissingDeclReturnsErrIndexExtractorRequired(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateKeyspace("users", testDecl("by_owner", "owner")); err != nil {
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
		defer db.Close()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin #2: %v", err)
		}
		defer tx.Rollback()
		_, err = tx.OpenKeyspace("users")
		if !errors.Is(err, ErrIndexExtractorRequired) {
			t.Errorf("open without decls: got %v want ErrIndexExtractorRequired", err)
		}
		if err != nil && !strings.Contains(err.Error(), "by_owner") {
			t.Errorf("error must name the registered index: %v", err)
		}
	}
}

// TestOpenKeyspaceExtraDeclReturnsErrIndexUnknown verifies that
// supplying an IndexDecl whose Name has no matching registry
// entry returns ErrIndexUnknown.
func TestOpenKeyspaceExtraDeclReturnsErrIndexUnknown(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateKeyspace("users"); err != nil { // no indexes
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
		defer db.Close()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin #2: %v", err)
		}
		defer tx.Rollback()
		_, err = tx.OpenKeyspace("users", testDecl("by_extra", "x"))
		if !errors.Is(err, ErrIndexUnknown) {
			t.Errorf("open with extra decl: got %v want ErrIndexUnknown", err)
		}
		if err != nil && !strings.Contains(err.Error(), "by_extra") {
			t.Errorf("error must name the offending decl: %v", err)
		}
	}
}

// TestOpenKeyspaceMatchingDeclSucceeds verifies the happy path:
// supplying a matching IndexDecl on a keyspace with declared
// indexes returns the cached handle without error.
func TestOpenKeyspaceMatchingDeclSucceeds(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateKeyspace("users", testDecl("by_owner", "owner")); err != nil {
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
		defer db.Close()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin #2: %v", err)
		}
		defer tx.Rollback()
		ks, err := tx.OpenKeyspace("users", testDecl("by_owner", "owner"))
		if err != nil {
			t.Fatalf("OpenKeyspace matching: %v", err)
		}
		p, ok := ks.indexes["by_owner"]
		if !ok {
			t.Fatalf("pinnedIndex missing after Open")
		}
		// root/count come from on-disk registry entry.
		if p.root != 0 || p.count != 0 {
			t.Errorf("expected freshly-created index root=0 count=0; got root=%d count=%d", p.root, p.count)
		}
	}
}

// TestOpenKeyspaceSchemaHashMismatchReturnsFingerprintError verifies
// that opening with a structurally-different IndexDecl (e.g. an
// extra column) on the same name returns IndexFingerprintError
// with Field="schema-hash".
func TestOpenKeyspaceSchemaHashMismatchReturnsFingerprintError(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateKeyspace("users", testDecl("by_owner", "owner")); err != nil {
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
		defer db.Close()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin #2: %v", err)
		}
		defer tx.Rollback()
		// Different shape: extra column "repo" → different schema-hash.
		_, err = tx.OpenKeyspace("users", testDecl("by_owner", "owner", "repo"))
		if !errors.Is(err, ErrIndexFingerprintMismatch) {
			t.Fatalf("schema-hash drift: got %v want ErrIndexFingerprintMismatch", err)
		}
		var fp *IndexFingerprintError
		if !errors.As(err, &fp) {
			t.Fatalf("expected *IndexFingerprintError wrap, got %T", err)
		}
		if fp.Field != "schema-hash" {
			t.Errorf("Field: got %q want schema-hash", fp.Field)
		}
		if fp.IndexName != "by_owner" {
			t.Errorf("IndexName: got %q want by_owner", fp.IndexName)
		}
		if fp.Keyspace != "users" {
			t.Errorf("Keyspace: got %q want users", fp.Keyspace)
		}
		if fp.StoredHash == fp.SuppliedHash {
			t.Errorf("Stored == Supplied hash: %x", fp.StoredHash)
		}
	}
}

// TestOpenKeyspaceVersionMismatchReturnsFingerprintError verifies
// the same-schema-hash + different-Version path returns
// IndexFingerprintError with Field="version".
func TestOpenKeyspaceVersionMismatchReturnsFingerprintError(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		decl := testDecl("by_owner", "owner")
		decl.Version = "v1"
		if _, err := tx.CreateKeyspace("users", decl); err != nil {
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
		defer db.Close()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin #2: %v", err)
		}
		defer tx.Rollback()
		drifted := testDecl("by_owner", "owner")
		drifted.Version = "v2" // structural inputs same; only Version differs
		_, err = tx.OpenKeyspace("users", drifted)
		if !errors.Is(err, ErrIndexFingerprintMismatch) {
			t.Fatalf("version drift: got %v want ErrIndexFingerprintMismatch", err)
		}
		var fp *IndexFingerprintError
		if !errors.As(err, &fp) {
			t.Fatalf("expected *IndexFingerprintError wrap, got %T", err)
		}
		if fp.Field != "version" {
			t.Errorf("Field: got %q want version", fp.Field)
		}
		if fp.StoredVersion != "v1" || fp.SuppliedVersion != "v2" {
			t.Errorf("versions: got stored=%q supplied=%q want v1/v2", fp.StoredVersion, fp.SuppliedVersion)
		}
	}
}

// TestOpenKeyspaceMissingDeclsAgainstMultiIndexRegistry verifies
// that supplying zero decls against a registry with multiple
// indexes returns ErrIndexExtractorRequired (the missing-decl
// path fires before any fingerprint check, since there's nothing
// to fingerprint). Validation order in
// validatePinnedAgainstRegistry: extras (none here, supplied is
// empty), missing (this fires), fingerprint (unreachable here).
func TestOpenKeyspaceMissingDeclsAgainstMultiIndexRegistry(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateKeyspace("users",
			testDecl("by_owner", "owner"),
			testDecl("by_repo", "repo"),
		); err != nil {
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
		defer db.Close()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin #2: %v", err)
		}
		defer tx.Rollback()
		_, err = tx.OpenKeyspace("users") // zero decls
		if !errors.Is(err, ErrIndexExtractorRequired) {
			t.Errorf("missing decls: got %v want ErrIndexExtractorRequired", err)
		}
	}
}

// --- Same-tx re-open idempotence ---------------------------------

// TestOpenKeyspaceSameTxReopenSameDeclsCached verifies that two
// OpenKeyspace calls in the same tx with structurally identical
// IndexDecls return the SAME *Keyspace pointer.
func TestOpenKeyspaceSameTxReopenSameDeclsCached(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateKeyspace("users", testDecl("by_owner", "owner")); err != nil {
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
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin #2: %v", err)
	}
	defer tx.Rollback()
	ks1, err := tx.OpenKeyspace("users", testDecl("by_owner", "owner"))
	if err != nil {
		t.Fatalf("OpenKeyspace #1: %v", err)
	}
	ks2, err := tx.OpenKeyspace("users", testDecl("by_owner", "owner"))
	if err != nil {
		t.Fatalf("OpenKeyspace #2: %v", err)
	}
	if ks1 != ks2 {
		t.Errorf("same-tx re-open returned different handles: %p vs %p", ks1, ks2)
	}
}

// TestOpenKeyspaceSameTxReopenDifferentVersionReturnsErrAlreadyOpen
// verifies that a same-tx re-open with a different Version (one
// hashable input differs) returns ErrKeyspaceAlreadyOpen.
func TestOpenKeyspaceSameTxReopenDifferentVersionReturnsErrAlreadyOpen(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateKeyspace("users", testDecl("by_owner", "owner")); err != nil {
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
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin #2: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.OpenKeyspace("users", testDecl("by_owner", "owner")); err != nil {
		t.Fatalf("OpenKeyspace #1: %v", err)
	}
	conflict := testDecl("by_owner", "owner")
	conflict.Version = "v2"
	_, err = tx.OpenKeyspace("users", conflict)
	if !errors.Is(err, ErrKeyspaceAlreadyOpen) {
		t.Errorf("conflicting Version re-open: got %v want ErrKeyspaceAlreadyOpen", err)
	}
}

// TestOpenKeyspaceSameTxReopenDifferentExtractFirstWins verifies
// the first-Extract-wins rule: two OpenKeyspace calls
// with structurally identical IndexDecls but different Extract
// functions yield the SAME *Keyspace handle (Extract from the
// first call wins).
func TestOpenKeyspaceSameTxReopenDifferentExtractFirstWins(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateKeyspace("users", testDecl("by_owner", "owner")); err != nil {
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
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin #2: %v", err)
	}
	defer tx.Rollback()

	firstExtractCalled := false
	first := testDecl("by_owner", "owner")
	first.Extract = func(_, _ []byte) []IndexEntry {
		firstExtractCalled = true
		return nil
	}
	secondExtractCalled := false
	second := testDecl("by_owner", "owner")
	second.Extract = func(_, _ []byte) []IndexEntry {
		secondExtractCalled = true
		return nil
	}

	ks1, err := tx.OpenKeyspace("users", first)
	if err != nil {
		t.Fatalf("OpenKeyspace #1: %v", err)
	}
	ks2, err := tx.OpenKeyspace("users", second)
	if err != nil {
		t.Fatalf("OpenKeyspace #2 (different Extract): %v", err)
	}
	if ks1 != ks2 {
		t.Fatalf("first-Extract-wins: different handles returned %p vs %p", ks1, ks2)
	}
	// Invoke the pinned Extract to verify it's the FIRST call's.
	if p := ks1.indexes["by_owner"]; p != nil {
		_ = p.decl.Extract(nil, nil)
	}
	if !firstExtractCalled || secondExtractCalled {
		t.Errorf("first-Extract-wins violated: firstCalled=%v secondCalled=%v",
			firstExtractCalled, secondExtractCalled)
	}
}

// TestMixingOpenKeyspaceAndOpenKeyspaceReadOnlyReturnsErrAlreadyOpen
// verifies the mixed-read+write same-tx-open rejection.
func TestMixingOpenKeyspaceAndOpenKeyspaceReadOnlyReturnsErrAlreadyOpen(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateKeyspace("users"); err != nil {
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

	t.Run("read-then-write", func(t *testing.T) {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer tx.Rollback()
		if _, err := tx.OpenKeyspaceReadOnly("users"); err != nil {
			t.Fatalf("OpenKeyspaceReadOnly: %v", err)
		}
		_, err = tx.OpenKeyspace("users")
		if !errors.Is(err, ErrKeyspaceAlreadyOpen) {
			t.Errorf("readOnly then writable: got %v want ErrKeyspaceAlreadyOpen", err)
		}
	})

	t.Run("write-then-read", func(t *testing.T) {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer tx.Rollback()
		if _, err := tx.OpenKeyspace("users"); err != nil {
			t.Fatalf("OpenKeyspace: %v", err)
		}
		_, err = tx.OpenKeyspaceReadOnly("users")
		if !errors.Is(err, ErrKeyspaceAlreadyOpen) {
			t.Errorf("writable then readOnly: got %v want ErrKeyspaceAlreadyOpen", err)
		}
	})
}

// --- SetKeyspace mirror -----------------------------------------

// TestCreateSetKeyspaceWithIndexesAllocatesRegistry verifies the
// SetKeyspace-side mirror of CreateKeyspace-with-indexes.
func TestCreateSetKeyspaceWithIndexesAllocatesRegistry(t *testing.T) {
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
	sks, err := tx.CreateSetKeyspace("subs", nil, testDecl("by_topic", "topic"))
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if sks.desc.IndexRegistryRoot == 0 {
		t.Fatalf("IndexRegistryRoot still 0 after CreateSetKeyspace with index")
	}
	if len(sks.indexes) != 1 {
		t.Errorf("sks.indexes len=%d, want 1", len(sks.indexes))
	}
}

// TestOpenSetKeyspaceMissingDeclReturnsErrIndexExtractorRequired
// mirrors the Keyspace test for the SetKeyspace surface.
func TestOpenSetKeyspaceMissingDeclReturnsErrIndexExtractorRequired(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateSetKeyspace("subs", nil, testDecl("by_topic", "topic")); err != nil {
			t.Fatalf("CreateSetKeyspace: %v", err)
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
		defer db.Close()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin #2: %v", err)
		}
		defer tx.Rollback()
		_, err = tx.OpenSetKeyspace("subs")
		if !errors.Is(err, ErrIndexExtractorRequired) {
			t.Errorf("open without decls: got %v want ErrIndexExtractorRequired", err)
		}
	}
}

// --- Kind=2 reachability via real registry creation -------------

// --- Rollback-state-preservation regression tests ------------------

// TestCreateKeyspaceWriteFailureRestoresPendingDelete is a
// regression test: a Tx that DeleteKeyspace's an existing
// keyspace, then CreateKeyspace's the same name with a decl whose
// registry write fails, must leave tx.pendingDeletes[name] intact
// so the original on-disk descriptor still gets removed at Commit
// (rather than silently surviving because the create cleared the
// pending-delete eagerly).
//
// Failure injection: an IndexDecl with a Version string exceeding
// uint16 length triggers ErrInvalidOptions from encodeRegistryEntry
// inside writeNewIndexRegistry — deterministic, no slab tuning.
func TestCreateKeyspaceWriteFailureRestoresPendingDelete(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateKeyspace("users"); err != nil {
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
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin #2: %v", err)
	}
	defer tx.Rollback()
	if err := tx.DeleteKeyspace("users"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	if _, ok := tx.pendingDeletes["users"]; !ok {
		t.Fatalf("after DeleteKeyspace: pendingDeletes['users'] missing")
	}
	// Try CreateKeyspace with a decl whose encoded form is rejected.
	badDecl := testDecl("by_owner", "owner")
	badDecl.Version = strings.Repeat("v", 70000) // > uint16 → encode fails
	_, err = tx.CreateKeyspace("users", badDecl)
	if err == nil {
		t.Fatalf("CreateKeyspace expected to fail (oversized Version), got nil")
	}
	// pendingDeletes['users'] must still be present so
	// the original on-disk descriptor gets removed at Commit.
	if _, ok := tx.pendingDeletes["users"]; !ok {
		t.Fatalf("after failed CreateKeyspace: pendingDeletes['users'] dropped — pending-delete regression")
	}
	// numKeyspaces decrement-then-increment-then-rollback math must
	// also be clean; the DeleteKeyspace ↓ + Create ↑ + Rollback ↓
	// sequence leaves numKeyspaces at original - 1.
}

// TestOpenKeyspaceFingerprintFailurePreservesDirtyDescriptor is a
// regression test: a Tx that SetKeyspaceConfig's a not-
// yet-opened name (creates a dirtyDescriptors entry), then attempts
// OpenKeyspace with a drifted IndexDecl, must leave the dirtyDescriptor
// entry intact so the SetKeyspaceConfig mutation still flushes at
// Commit.
func TestOpenKeyspaceFingerprintFailurePreservesDirtyDescriptor(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateKeyspace("users", testDecl("by_owner", "owner")); err != nil {
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
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin #2: %v", err)
	}
	defer tx.Rollback()

	if err := tx.SetKeyspaceConfig("users", KeyspaceConfig{RestartGroupTarget: 32}); err != nil {
		t.Fatalf("SetKeyspaceConfig: %v", err)
	}
	if _, ok := tx.dirtyDescriptors["users"]; !ok {
		t.Fatalf("after SetKeyspaceConfig: dirtyDescriptors['users'] missing")
	}

	// OpenKeyspace with a drifted decl (extra column) → fingerprint
	// mismatch. The validation failure must NOT silently drop the
	// dirtyDescriptors entry.
	_, err = tx.OpenKeyspace("users", testDecl("by_owner", "owner", "repo"))
	if !errors.Is(err, ErrIndexFingerprintMismatch) {
		t.Fatalf("expected fingerprint mismatch, got %v", err)
	}
	if _, ok := tx.dirtyDescriptors["users"]; !ok {
		t.Fatalf("after failed OpenKeyspace: dirtyDescriptors['users'] dropped — dirty-descriptor regression")
	}
}

// TestCreateSetKeyspaceWriteFailureRestoresPendingDelete is the
// SetKeyspace mirror of TestCreateKeyspaceWriteFailureRestoresPendingDelete
// (the same pending-delete regression on the set_keyspace.go path).
func TestCreateSetKeyspaceWriteFailureRestoresPendingDelete(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateSetKeyspace("subs", nil); err != nil {
			t.Fatalf("CreateSetKeyspace: %v", err)
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
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin #2: %v", err)
	}
	defer tx.Rollback()
	if err := tx.DeleteKeyspace("subs"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	if _, ok := tx.pendingDeletes["subs"]; !ok {
		t.Fatalf("after DeleteKeyspace: pendingDeletes['subs'] missing")
	}
	badDecl := testDecl("by_topic", "topic")
	badDecl.Version = strings.Repeat("v", 70000)
	_, err = tx.CreateSetKeyspace("subs", nil, badDecl)
	if err == nil {
		t.Fatalf("CreateSetKeyspace expected to fail (oversized Version), got nil")
	}
	if _, ok := tx.pendingDeletes["subs"]; !ok {
		t.Fatalf("after failed CreateSetKeyspace: pendingDeletes['subs'] dropped — SetKeyspace pending-delete regression")
	}
}

// TestOpenSetKeyspaceFingerprintFailurePreservesDirtyDescriptor is
// the SetKeyspace mirror of
// TestOpenKeyspaceFingerprintFailurePreservesDirtyDescriptor (the
// same dirty-descriptor regression on the set_keyspace.go path).
func TestOpenSetKeyspaceFingerprintFailurePreservesDirtyDescriptor(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateSetKeyspace("subs", nil, testDecl("by_topic", "topic")); err != nil {
			t.Fatalf("CreateSetKeyspace: %v", err)
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
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin #2: %v", err)
	}
	defer tx.Rollback()

	if err := tx.SetKeyspaceConfig("subs", KeyspaceConfig{RestartGroupTarget: 32}); err != nil {
		t.Fatalf("SetKeyspaceConfig: %v", err)
	}
	if _, ok := tx.dirtyDescriptors["subs"]; !ok {
		t.Fatalf("after SetKeyspaceConfig: dirtyDescriptors['subs'] missing")
	}
	_, err = tx.OpenSetKeyspace("subs", testDecl("by_topic", "topic", "user"))
	if !errors.Is(err, ErrIndexFingerprintMismatch) {
		t.Fatalf("expected fingerprint mismatch, got %v", err)
	}
	if _, ok := tx.dirtyDescriptors["subs"]; !ok {
		t.Fatalf("after failed OpenSetKeyspace: dirtyDescriptors['subs'] dropped — SetKeyspace dirty-descriptor regression")
	}
}

// --- Read-only handle rejection ----------------------------------

// TestOpenKeyspaceReadOnlyHandleRejectsPut verifies that
// a *Keyspace handle returned from OpenKeyspaceReadOnly
// rejects all mutating operations with ErrReadOnly, even on a
// writable Tx. Per api-surface.md §Keyspace API +
// indexing.md §Open Semantics.
func TestOpenKeyspaceReadOnlyHandleRejectsPut(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateKeyspace("users"); err != nil {
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
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.OpenKeyspaceReadOnly("users")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly: %v", err)
	}
	if err := ks.Put([]byte("k"), []byte("v")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Put on readonly handle: got %v want ErrReadOnly", err)
	}
	if err := ks.Delete([]byte("k")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Delete on readonly handle: got %v want ErrReadOnly", err)
	}
	if _, err := ks.DeleteRange([]byte("a"), []byte("z")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("DeleteRange on readonly handle: got %v want ErrReadOnly", err)
	}
	c := ks.Cursor()
	if err := c.Delete(); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Cursor.Delete on readonly handle: got %v want ErrReadOnly", err)
	}
}

// TestOpenSetKeyspaceReadOnlyHandleRejectsPut mirrors
// TestOpenKeyspaceReadOnlyHandleRejectsPut for
// the SetKeyspace surface.
func TestOpenSetKeyspaceReadOnlyHandleRejectsPut(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.CreateSetKeyspace("subs", nil); err != nil {
			t.Fatalf("CreateSetKeyspace: %v", err)
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
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	sks, err := tx.OpenSetKeyspaceReadOnly("subs")
	if err != nil {
		t.Fatalf("OpenSetKeyspaceReadOnly: %v", err)
	}
	if _, err := sks.Put([]byte("k"), []byte("v")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Put on readonly handle: got %v want ErrReadOnly", err)
	}
	if err := sks.Delete([]byte("k")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Delete on readonly handle: got %v want ErrReadOnly", err)
	}
	if err := sks.DeleteValue([]byte("k"), []byte("v")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("DeleteValue on readonly handle: got %v want ErrReadOnly", err)
	}
	if _, err := sks.DeleteRange([]byte("a"), []byte("z")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("DeleteRange on readonly handle: got %v want ErrReadOnly", err)
	}
	c := sks.Cursor()
	if err := c.Delete(); !errors.Is(err, ErrReadOnly) {
		t.Errorf("SetCursor.Delete on readonly handle: got %v want ErrReadOnly", err)
	}
}

// TestCreateKeyspaceWithIndexDoesNotPolluteListKeyspaces verifies
// the indexing.md entailed invariant on Kind=2 one-parent-
// reachability uniqueness: indexes are stored in the parent's
// registry sub-tree (a child B+tree), NOT as Kind=2 entries in the
// top-level keyspace B+tree. ListKeyspaces shows only the parent
// keyspace, not any per-index internal names.
func TestCreateKeyspaceWithIndexDoesNotPolluteListKeyspaces(t *testing.T) {
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
	if _, err := tx.CreateKeyspace("users",
		testDecl("by_owner", "owner"),
		testDecl("by_repo", "repo"),
	); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	names, err := tx.ListKeyspaces()
	if err != nil {
		t.Fatalf("ListKeyspaces: %v", err)
	}
	if len(names) != 1 || names[0] != "users" {
		t.Errorf("ListKeyspaces: got %v want [users]", names)
	}
}

// TestWriteNewIndexRegistryAtomicOnPartialFailure pins the write-helper
// atomicity contract: a mid-loop failure in writeNewIndexRegistry must
// not orphan the registry-tree pages allocated by the iterations that
// already succeeded. The savepoint wrap frees them and restores the
// descriptor's registry root; without it, Tx.Commit (the
// rest-of-tx-continues path) would commit those pages as a BitmapLeak.
// A deterministic failure is injected after the 2nd registryPut via the
// test hook; Check() must then report zero BitmapLeak pages.
func TestWriteNewIndexRegistryAtomicOnPartialFailure(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	injected := errors.New("injected registry-write failure")
	setWriteRegistryFailHookForTest(func(i int) error {
		if i >= 1 { // iterations 0 and 1 succeed, then fail
			return injected
		}
		return nil
	})
	t.Cleanup(func() { setWriteRegistryFailHookForTest(nil) })

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_, err = tx.CreateKeyspace("ks",
		testDecl("idx_a", "a"),
		testDecl("idx_b", "b"),
		testDecl("idx_c", "c"),
	)
	if !errors.Is(err, injected) {
		tx.Rollback()
		t.Fatalf("CreateKeyspace err = %v, want injected failure", err)
	}
	// Rest-of-tx-continues: commit despite the per-op error (the path
	// that would orphan the partial allocation without the savepoint).
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after injected failure: %v", err)
	}

	var leaks []CheckIssue
	for _, iss := range collectIssues(db.Check()) {
		if iss.Code == "BitmapLeak" {
			leaks = append(leaks, iss)
		}
	}
	if len(leaks) != 0 {
		t.Errorf("writeNewIndexRegistry orphaned %d page(s) on partial failure (want 0): %v", len(leaks), leaks)
	}
}

// TestKind2DescriptorsHaveDistinctIndexRegistryRoots enforces the
// "exactly one parent" half of the indexing.md entailed
// invariant: every engine-internal Kind=2 keyspace descriptor is
// reachable via exactly one user keyspace's index-registry sub-tree —
// never via two distinct parents. Per-keyspace IndexRegistryRoot
// allocation makes this structurally true (each CreateKeyspace /
// CreateSetKeyspace allocates its own registry sub-tree root), so the
// uniqueness of non-zero IndexRegistryRoot page IDs across all
// top-level descriptors is the observable proxy: two parents sharing
// a root would mean a Kind=2 descriptor reachable via both.
//
// This fixture exercises the Create path (CreateKeyspace /
// CreateSetKeyspace with indexes); it pins root uniqueness at
// allocation. A bad descriptor copy in a later refactor that mutates
// IndexRegistryRoot copies (DeleteKeyspace's three-subtree retirement,
// RebuildIndex/DropIndex's registry-root rewrites) would violate the
// same proxy — caught here once a fixture exercises those ops, and
// across all on-disk descriptors by the Check(CheckIndexes)
// full walk.
//
// The companion TestCreateKeyspaceWithIndexDoesNotPolluteListKeyspaces
// + the forge test TestListKeyspacesFiltersKindIndexInternal cover the
// other half ("never via the top-level keyspace B+tree"). This walks
// the persisted keyspace B+tree (not the cached handles) so a
// descriptor mutated on disk but not in-cache is still caught.
func TestKind2DescriptorsHaveDistinctIndexRegistryRoots(t *testing.T) {
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
	// Three indexed parents of both kinds + one index-free keyspace
	// (IndexRegistryRoot == 0, must not register as a "duplicate").
	if _, err := tx.CreateKeyspace("users", testDecl("by_owner", "owner"), testDecl("by_repo", "repo")); err != nil {
		t.Fatalf("CreateKeyspace users: %v", err)
	}
	if _, err := tx.CreateKeyspace("repos", testDecl("by_org", "org")); err != nil {
		t.Fatalf("CreateKeyspace repos: %v", err)
	}
	if _, err := tx.CreateSetKeyspace("subs", nil, testDecl("by_tag", "tag")); err != nil {
		t.Fatalf("CreateSetKeyspace subs: %v", err)
	}
	if _, err := tx.CreateKeyspace("plain"); err != nil {
		t.Fatalf("CreateKeyspace plain: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Walk the persisted top-level keyspace B+tree, collecting every
	// descriptor's IndexRegistryRoot. The top-level tree holds only the
	// Kind=0/1 parents (Kind=2 internals live in the registry sub-trees),
	// so this is exactly the set of "parents" whose roots must be unique.
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer rtx.Rollback()
	cfg := rtx.pgr.Config()
	meta := rtx.meta

	seen := map[uint64]string{} // IndexRegistryRoot -> first keyspace seen
	var indexed, total int
	walkErr := btree.WalkKV(rtx.pgr, cfg, meta.KeyspaceRoot, meta.HighWaterMark, func(k, v []byte) error {
		total++
		name := string(k)
		d := descriptor.Decode(v)
		if d.IndexRegistryRoot == 0 {
			return nil
		}
		indexed++
		if prev, dup := seen[d.IndexRegistryRoot]; dup {
			t.Errorf("keyspaces %q and %q share IndexRegistryRoot %d — Kind=2 one-parent-reachability violated",
				prev, name, d.IndexRegistryRoot)
		}
		seen[d.IndexRegistryRoot] = name
		return nil
	})
	if walkErr != nil {
		t.Fatalf("WalkKV: %v", walkErr)
	}
	// Guard against a vacuous pass: the three indexed parents must each
	// have contributed a distinct non-zero root, and the walk must have
	// seen all four top-level keyspaces (no Kind=2 pollution).
	if indexed != 3 {
		t.Errorf("indexed parents with non-zero IndexRegistryRoot = %d, want 3", indexed)
	}
	if total != 4 {
		t.Errorf("top-level descriptors walked = %d, want 4 (Kind=2 internals must not appear here)", total)
	}
	if len(seen) != 3 {
		t.Errorf("distinct non-zero IndexRegistryRoots = %d, want 3", len(seen))
	}
}

// An IndexDecl carrying a kind this engine version does not
// implement is rejected before any work, at every decl entry point
// (indexing.md §Overview): OpenKeyspace / CreateKeyspace via decl
// validation, and Rebuild via its own gate.
func TestIndexKindUnknownRejectedAtOpenAndRebuild(t *testing.T) {
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

	extract := func(_, _ []byte) []IndexEntry { return nil }
	bad := &IndexDecl{Name: "i", Columns: []IndexColumn{{Name: "c"}},
		Kind: IndexKind(9), Extract: extract}
	if _, err := tx.CreateKeyspace("ks", bad); !errors.Is(err, ErrIndexKindUnknown) {
		t.Fatalf("CreateKeyspace(kind=9) = %v, want ErrIndexKindUnknown", err)
	}

	good := &IndexDecl{Name: "i", Columns: []IndexColumn{{Name: "c"}}, Extract: extract}
	if _, err := tx.CreateKeyspace("ks", good); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.Indexes().Rebuild("ks", bad); !errors.Is(err, ErrIndexKindUnknown) {
		t.Fatalf("Rebuild(kind=9) = %v, want ErrIndexKindUnknown", err)
	}
}

// A STORED registry entry whose Kind this engine version does not
// implement is rejected at open — on the write path BEFORE the
// fingerprint compare (a mismatch's documented recovery would
// silently convert the index to composite), and on the read-only
// path outright (indexing.md §Open Semantics + §Storage Layout).
func TestIndexKindUnknownStoredEntryRejectedAtOpen(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	extract := func(_, _ []byte) []IndexEntry { return nil }
	decl := &IndexDecl{Name: "i", Columns: []IndexColumn{{Name: "c"}}, Extract: extract}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.CreateKeyspace("ks", decl); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Forge the stored entry's Kind via the registry surface (the
	// codec accepts any non-composite kind with an empty payload).
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin(forge): %v", err)
	}
	owner, _, _, _, err := tx.resolveKeyspaceForIndexOp("ks")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	entry, err := tx.registryGet(owner, "i")
	if err != nil {
		t.Fatalf("registryGet: %v", err)
	}
	entry.Kind = indexing.Kind(9)
	if err := tx.registryPut(owner, "i", entry); err != nil {
		t.Fatalf("registryPut: %v", err)
	}
	if err := tx.propagateNotCachedDescChange("ks", owner); err != nil {
		t.Fatalf("propagate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(forge): %v", err)
	}

	// Confirm the forgery persisted (a pinned-state flush at commit
	// would silently rewrite it from the composite decl).
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin(verify-forge): %v", err)
	}
	owner, _, _, _, err = tx.resolveKeyspaceForIndexOp("ks")
	if err != nil {
		t.Fatalf("resolve(verify-forge): %v", err)
	}
	if e2, err := tx.registryGet(owner, "i"); err != nil {
		t.Fatalf("registryGet(verify-forge): %v", err)
	} else if e2.Kind != indexing.Kind(9) {
		t.Fatalf("forged kind did not persist: got %d want 9", e2.Kind)
	}
	tx.Rollback()

	// Write-path open: ErrIndexKindUnknown, NOT a fingerprint error.
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin(open): %v", err)
	}
	_, err = tx.OpenKeyspace("ks", decl)
	if !errors.Is(err, ErrIndexKindUnknown) {
		t.Fatalf("OpenKeyspace over forged kind = %v, want ErrIndexKindUnknown", err)
	}
	var fpErr *IndexFingerprintError
	if errors.As(err, &fpErr) {
		t.Fatalf("OpenKeyspace surfaced a fingerprint error (%v) — the kind gate must win", err)
	}
	tx.Rollback()

	// Read-only open: same sentinel.
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin(ro): %v", err)
	}
	defer tx.Rollback()
	_, err = tx.OpenKeyspaceReadOnly("ks")
	if !errors.Is(err, ErrIndexKindUnknown) {
		t.Fatalf("OpenKeyspaceReadOnly over forged kind = %v, want ErrIndexKindUnknown", err)
	}

	// Rebuild and Drop load the stored entry outside the open
	// gates — both must reject the foreign kind too (Rebuild would
	// silently convert it; Drop cannot see past Root). Same write
	// tx: the single-writer lock is still held above.
	if err := tx.Indexes().Rebuild("ks", decl); !errors.Is(err, ErrIndexKindUnknown) {
		t.Fatalf("Rebuild over forged kind = %v, want ErrIndexKindUnknown", err)
	}
	if err := tx.Indexes().Drop("ks", "i"); !errors.Is(err, ErrIndexKindUnknown) {
		t.Fatalf("Drop over forged kind = %v, want ErrIndexKindUnknown", err)
	}

	// The integrity walk reports the same state rather than
	// silently passing what open rejects.
	found := false
	for iss := range db.Check() {
		if iss.Code == "RegistryEntryKindUnknown" {
			found = true
		}
	}
	if !found {
		t.Fatal("Check emitted no RegistryEntryKindUnknown issue for the forged entry")
	}
}

// The strict integrity walk asserts the registry entry's padding
// bytes are zero (indexing.md §Storage Layout) — the decoder is
// deliberately tolerant, so the walk is the enforcement point. A
// nonzero padding byte written today would be reinterpreted by a
// future layout revision's reader.
func TestCheckReportsRegistryEntryPaddingNonzero(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	extract := func(_, _ []byte) []IndexEntry { return nil }
	decl := &IndexDecl{Name: "i", Columns: []IndexColumn{{Name: "c"}}, Extract: extract}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.CreateKeyspace("ks", decl); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Forge a nonzero padding byte: encode the stored entry (the
	// codec always zeroes padding), flip offset 10, and write the
	// raw bytes into the registry tree, mirroring registryPut.
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin(forge): %v", err)
	}
	owner, _, _, _, err := tx.resolveKeyspaceForIndexOp("ks")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	entry, err := tx.registryGet(owner, "i")
	if err != nil {
		t.Fatalf("registryGet: %v", err)
	}
	encoded, err := indexing.EncodeRegistryEntry(entry)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	encoded[10] = 0xAB
	desc := owner.descriptor()
	newRoot, err := btree.Put(btreeWriter{tx.pgr}, tx.pgr.Config(), desc.IndexRegistryRoot, []byte("i"), encoded)
	if err != nil {
		t.Fatalf("raw registry put: %v", err)
	}
	desc.IndexRegistryRoot = newRoot
	owner.markDirty()
	if err := tx.propagateNotCachedDescChange("ks", owner); err != nil {
		t.Fatalf("propagate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(forge): %v", err)
	}

	found := false
	for iss := range db.Check() {
		if iss.Code == "RegistryEntryPaddingNonzero" {
			found = true
		}
	}
	if !found {
		t.Fatal("Check emitted no RegistryEntryPaddingNonzero issue for the forged padding byte")
	}
}
