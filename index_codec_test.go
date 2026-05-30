package gmdb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// --- Codec roundtrip tests ----------------------------------------

// TestRegistryEntryRoundtripFull verifies a fully-populated entry
// encodes + decodes byte-identical. Covers SchemaHash, Unique=true,
// Root, Count, UserVersion, multiple Columns, multiple Covering.
func TestRegistryEntryRoundtripFull(t *testing.T) {
	want := &indexRegistryEntry{
		SchemaHash:  0xdeadbeefcafebabe,
		Unique:      true,
		Root:        42,
		Count:       1234567,
		UserVersion: "v1.2.3",
		Columns:     []string{"owner", "repo", "branch"},
		Covering:    []string{"size", "mtime"},
	}
	encoded, err := encodeRegistryEntry(want)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeRegistryEntry(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SchemaHash != want.SchemaHash {
		t.Errorf("SchemaHash: got %x want %x", got.SchemaHash, want.SchemaHash)
	}
	if got.Unique != want.Unique {
		t.Errorf("Unique: got %v want %v", got.Unique, want.Unique)
	}
	if got.Root != want.Root {
		t.Errorf("Root: got %d want %d", got.Root, want.Root)
	}
	if got.Count != want.Count {
		t.Errorf("Count: got %d want %d", got.Count, want.Count)
	}
	if got.UserVersion != want.UserVersion {
		t.Errorf("UserVersion: got %q want %q", got.UserVersion, want.UserVersion)
	}
	if !stringSlicesEqual(got.Columns, want.Columns) {
		t.Errorf("Columns: got %v want %v", got.Columns, want.Columns)
	}
	if !stringSlicesEqual(got.Covering, want.Covering) {
		t.Errorf("Covering: got %v want %v", got.Covering, want.Covering)
	}
}

// TestRegistryEntryRoundtripMinimal verifies the minimal entry:
// no UserVersion, no Columns, no Covering, Unique=false, zero
// SchemaHash/Root/Count. The encoded form is exactly the
// fixed-prefix (32 B) + three uint16 zero counters = 38 B.
func TestRegistryEntryRoundtripMinimal(t *testing.T) {
	want := &indexRegistryEntry{}
	encoded, err := encodeRegistryEntry(want)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) != indexRegistryEntryFixedPrefixSize+2+2+2 {
		t.Fatalf("minimal encoded length: got %d want %d",
			len(encoded), indexRegistryEntryFixedPrefixSize+6)
	}
	got, err := decodeRegistryEntry(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SchemaHash != 0 || got.Unique || got.Root != 0 || got.Count != 0 {
		t.Errorf("minimal decoded fixed fields wrong: %+v", got)
	}
	if got.UserVersion != "" {
		t.Errorf("UserVersion: got %q want empty", got.UserVersion)
	}
	if len(got.Columns) != 0 {
		t.Errorf("Columns: got %v want empty", got.Columns)
	}
	if len(got.Covering) != 0 {
		t.Errorf("Covering: got %v want empty", got.Covering)
	}
}

// TestRegistryEntryFixedFieldsAtSpecOffsets verifies the binary
// layout matches indexing.md §Storage Layout offsets exactly:
// SchemaHash at 0, Unique at 8, Padding at 9..15, Root at 16,
// Count at 24. Critical for cross-implementation compatibility.
func TestRegistryEntryFixedFieldsAtSpecOffsets(t *testing.T) {
	e := &indexRegistryEntry{
		SchemaHash: 0x1122334455667788,
		Unique:     true,
		Root:       0xAABBCCDDEEFF1122,
		Count:      0x99AABBCCDDEEFF00,
	}
	enc, err := encodeRegistryEntry(e)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := binary.LittleEndian.Uint64(enc[0:]); got != e.SchemaHash {
		t.Errorf("SchemaHash@0: got %x want %x", got, e.SchemaHash)
	}
	if enc[8] != 1 {
		t.Errorf("Unique@8: got %d want 1", enc[8])
	}
	for i := 9; i < 16; i++ {
		if enc[i] != 0 {
			t.Errorf("Padding@%d: got %d want 0", i, enc[i])
		}
	}
	if got := binary.LittleEndian.Uint64(enc[16:]); got != e.Root {
		t.Errorf("Root@16: got %x want %x", got, e.Root)
	}
	if got := binary.LittleEndian.Uint64(enc[24:]); got != e.Count {
		t.Errorf("Count@24: got %x want %x", got, e.Count)
	}
}

// TestRegistryEntryRejectsOversizedUserVersion verifies the encoder
// rejects a UserVersion exceeding uint16. Necessary because the
// on-disk format uses uint16 length prefixes — silent truncation
// would corrupt the encoded entry.
func TestRegistryEntryRejectsOversizedUserVersion(t *testing.T) {
	e := &indexRegistryEntry{UserVersion: strings.Repeat("x", 70000)}
	_, err := encodeRegistryEntry(e)
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("expected ErrInvalidOptions for oversized UserVersion, got %v", err)
	}
}

// TestRegistryEntryRejectsOversizedColumnName verifies the encoder
// rejects a column name exceeding uint16.
func TestRegistryEntryRejectsOversizedColumnName(t *testing.T) {
	e := &indexRegistryEntry{Columns: []string{strings.Repeat("x", 70000)}}
	_, err := encodeRegistryEntry(e)
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("expected ErrInvalidOptions for oversized column name, got %v", err)
	}
}

// TestDecodeRegistryEntryRejectsTruncatedPrefix verifies the
// decoder rejects a value shorter than the fixed prefix.
func TestDecodeRegistryEntryRejectsTruncatedPrefix(t *testing.T) {
	_, err := decodeRegistryEntry(make([]byte, 16))
	if !errors.Is(err, errRegistryEntryShort) {
		t.Fatalf("expected truncated-prefix error, got %v", err)
	}
}

// TestDecodeRegistryEntryRejectsTruncatedColumnList verifies that
// an entry promising 3 columns but cut off mid-name returns a
// short-entry error.
func TestDecodeRegistryEntryRejectsTruncatedColumnList(t *testing.T) {
	e := &indexRegistryEntry{Columns: []string{"owner", "repo", "branch"}}
	enc, err := encodeRegistryEntry(e)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Truncate to half — past the column-count field, mid-list.
	truncated := enc[:len(enc)/2]
	_, err = decodeRegistryEntry(truncated)
	if !errors.Is(err, errRegistryEntryShort) {
		t.Fatalf("expected truncated-list error, got %v", err)
	}
}

// TestDecodeRegistryEntryRejectsTrailingBytes verifies that extra
// bytes after the last covering name cause a decode error — the
// entry's length is implicit (it ends at the last byte of the
// last covering name), so any trailing bytes signal a malformed
// on-disk record.
func TestDecodeRegistryEntryRejectsTrailingBytes(t *testing.T) {
	e := &indexRegistryEntry{Columns: []string{"owner"}}
	enc, err := encodeRegistryEntry(e)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	enc = append(enc, 0x99) // trailing junk
	_, err = decodeRegistryEntry(enc)
	if !errors.Is(err, errRegistryEntryShort) {
		t.Fatalf("expected trailing-bytes error, got %v", err)
	}
}

// TestRegistryEntryRoundtripEmptyStrings verifies that empty
// strings in Columns/Covering (zero-length names) roundtrip — the
// encoding is (NameLen=0)(no Name bytes) and the spec doesn't
// forbid empty column names (validateIndexDecls rejects empty
// IndexDecl.Name but column.Name == "" is currently legal at the
// codec layer; the 7.5 wiring may add stricter validation).
func TestRegistryEntryRoundtripEmptyStrings(t *testing.T) {
	want := &indexRegistryEntry{Columns: []string{""}}
	enc, err := encodeRegistryEntry(want)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeRegistryEntry(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Columns) != 1 || got.Columns[0] != "" {
		t.Errorf("empty-column roundtrip failed: got %v", got.Columns)
	}
}

// --- Registry CRUD tests via tx helpers ---------------------------

func makeTestEntry(name string, root, count uint64) *indexRegistryEntry {
	return &indexRegistryEntry{
		SchemaHash:  uint64(len(name)) * 0x100,
		Unique:      false,
		Root:        root,
		Count:       count,
		UserVersion: "v1",
		Columns:     []string{name + "_col"},
	}
}

// TestRegistryGetEmpty verifies registryGet on a keyspace with no
// indexes returns ErrIndexNotFound (desc.IndexRegistryRoot == 0
// → no registry sub-tree → no entries).
func TestRegistryGetEmpty(t *testing.T) {
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
	ks, err := tx.CreateKeyspace("users")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if ks.desc.IndexRegistryRoot != 0 {
		t.Fatalf("freshly-created keyspace IndexRegistryRoot = %d, want 0", ks.desc.IndexRegistryRoot)
	}
	_, err = tx.registryGet(ks, "by_owner")
	if !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("registryGet on empty registry: got %v want ErrIndexNotFound", err)
	}
}

// TestRegistryPutThenGet verifies a Put → Get returns the same
// entry, and that desc.IndexRegistryRoot is non-zero after the
// first Put (allocation on first index, per chunk-7.1 entailed
// invariant on empty-registry canonical-at-zero).
func TestRegistryPutThenGet(t *testing.T) {
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
	ks, err := tx.CreateKeyspace("users")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	entry := makeTestEntry("by_owner", 99, 42)
	if err := tx.registryPut(ks, "by_owner", entry); err != nil {
		t.Fatalf("registryPut: %v", err)
	}
	if ks.desc.IndexRegistryRoot == 0 {
		t.Fatalf("desc.IndexRegistryRoot still 0 after first Put — allocation didn't happen")
	}
	got, err := tx.registryGet(ks, "by_owner")
	if err != nil {
		t.Fatalf("registryGet: %v", err)
	}
	if got.SchemaHash != entry.SchemaHash || got.Root != entry.Root || got.Count != entry.Count {
		t.Errorf("registryGet returned wrong entry: got %+v want %+v", got, entry)
	}
	if got.UserVersion != entry.UserVersion {
		t.Errorf("UserVersion: got %q want %q", got.UserVersion, entry.UserVersion)
	}
}

// TestRegistryPutReplace verifies a second Put with the same name
// replaces the existing entry (B+tree Put semantic). Verifies
// that the in-place replace is observable via Get.
func TestRegistryPutReplace(t *testing.T) {
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
	ks, err := tx.CreateKeyspace("users")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.registryPut(ks, "by_owner", makeTestEntry("by_owner", 1, 0)); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := tx.registryPut(ks, "by_owner", makeTestEntry("by_owner", 999, 50)); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	got, err := tx.registryGet(ks, "by_owner")
	if err != nil {
		t.Fatalf("registryGet: %v", err)
	}
	if got.Root != 999 || got.Count != 50 {
		t.Errorf("replace not observable: got Root=%d Count=%d want 999/50", got.Root, got.Count)
	}
}

// TestRegistryDeleteLastResetsRootToZero verifies the chunk-7.1
// indexing.md entailed invariant: DropIndex removing the last
// declared index resets desc.IndexRegistryRoot to 0. The btree
// layer guarantees Delete returns newRoot=0 on empty-tree shrink;
// registryDelete propagates that to desc.IndexRegistryRoot.
func TestRegistryDeleteLastResetsRootToZero(t *testing.T) {
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
	ks, err := tx.CreateKeyspace("users")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.registryPut(ks, "by_owner", makeTestEntry("by_owner", 1, 0)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ks.desc.IndexRegistryRoot == 0 {
		t.Fatalf("registry root still 0 after Put")
	}
	if err := tx.registryDelete(ks, "by_owner"); err != nil {
		t.Fatalf("registryDelete: %v", err)
	}
	if ks.desc.IndexRegistryRoot != 0 {
		t.Fatalf("registry root after deleting last entry: got %d want 0 — entailed-invariant violation",
			ks.desc.IndexRegistryRoot)
	}
	// Verify subsequent Get returns ErrIndexNotFound (empty registry).
	_, err = tx.registryGet(ks, "by_owner")
	if !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("after delete-last: got %v want ErrIndexNotFound", err)
	}
}

// TestRegistryDeleteMissingReturnsErrIndexNotFound verifies that
// deleting a non-existent name returns ErrIndexNotFound. Matches
// the chunk-7.1 api-surface.md Tx.DropIndex godoc sentinel.
func TestRegistryDeleteMissingReturnsErrIndexNotFound(t *testing.T) {
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
	ks, err := tx.CreateKeyspace("users")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// Empty registry path.
	err = tx.registryDelete(ks, "missing")
	if !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("delete on empty registry: got %v want ErrIndexNotFound", err)
	}
	// Populated registry with a different name.
	if err := tx.registryPut(ks, "by_owner", makeTestEntry("by_owner", 1, 0)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	err = tx.registryDelete(ks, "missing")
	if !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("delete missing name in populated registry: got %v want ErrIndexNotFound", err)
	}
	// Verify the unrelated entry is still there.
	if _, err := tx.registryGet(ks, "by_owner"); err != nil {
		t.Fatalf("by_owner gone after failed delete of missing name: %v", err)
	}
}

// TestRegistryListLexOrder verifies registryList returns names in
// lex order across multiple Puts.
func TestRegistryListLexOrder(t *testing.T) {
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
	ks, err := tx.CreateKeyspace("users")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// Insert out-of-order to verify lex ordering on read.
	for _, name := range []string{"by_repo", "by_owner", "by_active", "by_size"} {
		if err := tx.registryPut(ks, name, makeTestEntry(name, 1, 0)); err != nil {
			t.Fatalf("Put %q: %v", name, err)
		}
	}
	names, err := tx.registryList(ks)
	if err != nil {
		t.Fatalf("registryList: %v", err)
	}
	want := []string{"by_active", "by_owner", "by_repo", "by_size"}
	if !stringSlicesEqual(names, want) {
		t.Errorf("registryList: got %v want %v", names, want)
	}
}

// TestRegistryListEmpty verifies registryList on a keyspace with no
// indexes returns nil (no entries, no error).
func TestRegistryListEmpty(t *testing.T) {
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
	ks, err := tx.CreateKeyspace("users")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	names, err := tx.registryList(ks)
	if err != nil {
		t.Fatalf("registryList empty: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("registryList empty: got %v want nil", names)
	}
}

// TestRegistryPersistsAcrossCommit verifies the registry sub-tree
// + its root in the parent descriptor persist across a Commit +
// re-Open. Confirms the dirty-flush integration: marking the
// *Keyspace state Dirty (here via the chunk-5.6 keyspaceStateCreated
// from CreateKeyspace; registryPut doesn't need to re-mark) carries
// the updated IndexRegistryRoot to disk via flushKeyspaces.
func TestRegistryPersistsAcrossCommit(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	entry := makeTestEntry("by_owner", 99, 42)

	// Tx 1: create keyspace + register one index, commit.
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		ks, err := tx.CreateKeyspace("users")
		if err != nil {
			t.Fatalf("CreateKeyspace: %v", err)
		}
		if err := tx.registryPut(ks, "by_owner", entry); err != nil {
			t.Fatalf("registryPut: %v", err)
		}
		// CreateKeyspace set state=Created; that survives — the
		// flushKeyspaces walk will persist the descriptor with the
		// updated IndexRegistryRoot.
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		_ = db.Close()
	}

	// Tx 2: re-open, verify the registry entry survives.
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
		// Same backdoor as TestRegistryPutOnReopenedKeyspacePersists:
		// registryPut wrote with ad-hoc SchemaHash; use the
		// read-only path to skip chunk-7.5 fingerprint validation.
		ks, err := tx.OpenKeyspaceReadOnly("users")
		if err != nil {
			t.Fatalf("OpenKeyspaceReadOnly: %v", err)
		}
		if ks.desc.IndexRegistryRoot == 0 {
			t.Fatalf("IndexRegistryRoot lost across Commit/Open")
		}
		got, err := tx.registryGet(ks, "by_owner")
		if err != nil {
			t.Fatalf("registryGet after re-open: %v", err)
		}
		if got.SchemaHash != entry.SchemaHash || got.Root != entry.Root || got.Count != entry.Count {
			t.Errorf("registry entry drifted across Commit: got %+v want %+v", got, entry)
		}
	}
}

// TestRegistryPutOnReopenedKeyspacePersists verifies the chunk-7.3
// H-1 fix: registryPut on a keyspace opened in a SECOND tx (state
// is Clean, not Created) still transitions the owner's flush
// state and persists the registry mutation. Before the H-1 fix,
// registryPut mutated desc.IndexRegistryRoot in place but never
// touched ks.state — flushKeyspaces' state-driven walk skipped
// the Clean keyspace at Commit, the descriptor was never re-
// written, and the registry-tree pages became orphans.
//
// Three-tx sequence: (1) Create keyspace + Commit (no registry).
// (2) OpenKeyspace + registryPut + Commit. (3) OpenKeyspace +
// registryGet — must observe the Tx 2 Put.
func TestRegistryPutOnReopenedKeyspacePersists(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	entry := makeTestEntry("by_owner", 99, 42)

	// Tx 1: create keyspace + Commit (no indexes).
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open #1: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin #1: %v", err)
		}
		ks, err := tx.CreateKeyspace("users")
		if err != nil {
			t.Fatalf("CreateKeyspace: %v", err)
		}
		if ks.desc.IndexRegistryRoot != 0 {
			t.Fatalf("fresh keyspace IndexRegistryRoot != 0")
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit #1: %v", err)
		}
		_ = db.Close()
	}

	// Tx 2: re-open keyspace (state=Clean) + registryPut + Commit.
	// This is the path that exposed the H-1 silent data loss.
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open #2: %v", err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin #2: %v", err)
		}
		ks, err := tx.OpenKeyspace("users")
		if err != nil {
			t.Fatalf("OpenKeyspace: %v", err)
		}
		if ks.state != keyspaceStateClean {
			t.Fatalf("OpenKeyspace returned state %d, want Clean (the H-1 trigger condition)", ks.state)
		}
		if err := tx.registryPut(ks, "by_owner", entry); err != nil {
			t.Fatalf("registryPut: %v", err)
		}
		// H-1 fix: registryPut MUST have transitioned state.
		if ks.state == keyspaceStateClean {
			t.Fatalf("registryPut on Clean keyspace did not transition state — H-1 regression")
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit #2: %v", err)
		}
		_ = db.Close()
	}

	// Tx 3: verify the Tx 2 Put survived Commit + re-Open.
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open #3: %v", err)
		}
		defer db.Close()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin #3: %v", err)
		}
		defer tx.Rollback()
		// Use OpenKeyspaceReadOnly to bypass chunk-7.5's open-time
		// IndexDecl validation. The test backdoor-wrote a registry
		// entry via registryPut with ad-hoc SchemaHash that doesn't
		// match any real IndexDecl; the public OpenKeyspace path
		// would correctly reject this with ErrIndexExtractorRequired.
		// We only need read-only access to verify persistence.
		ks, err := tx.OpenKeyspaceReadOnly("users")
		if err != nil {
			t.Fatalf("OpenKeyspaceReadOnly #3: %v", err)
		}
		if ks.desc.IndexRegistryRoot == 0 {
			t.Fatalf("Tx 2 registryPut lost across Commit — H-1 regression")
		}
		got, err := tx.registryGet(ks, "by_owner")
		if err != nil {
			t.Fatalf("registryGet Tx 3: %v", err)
		}
		if got.SchemaHash != entry.SchemaHash || got.Root != entry.Root || got.Count != entry.Count {
			t.Errorf("registry entry drifted across Tx 2 Commit: got %+v want %+v", got, entry)
		}
	}
}

// TestRegistryPutEmptyNameReturnsErrKeyEmpty verifies the chunk-7.3
// M-2 fix: registryPut("", ...) returns ErrKeyEmpty at the helper
// boundary. Defense-in-depth against internal callers that bypass
// chunk-7.2 validateIndexDecls.
func TestRegistryPutEmptyNameReturnsErrKeyEmpty(t *testing.T) {
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
	ks, err := tx.CreateKeyspace("users")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	err = tx.registryPut(ks, "", makeTestEntry("x", 1, 0))
	if !errors.Is(err, ErrKeyEmpty) {
		t.Errorf("registryPut empty name: got %v want ErrKeyEmpty", err)
	}
	err = tx.registryDelete(ks, "")
	if !errors.Is(err, ErrKeyEmpty) {
		t.Errorf("registryDelete empty name: got %v want ErrKeyEmpty", err)
	}
	_, err = tx.registryGet(ks, "")
	if !errors.Is(err, ErrKeyEmpty) {
		t.Errorf("registryGet empty name: got %v want ErrKeyEmpty", err)
	}
}

// TestRegistryEncodeDecodeBytesEqual verifies a sanity check
// against bytes-equality on a roundtrip (encode → decode → re-
// encode → compare). Catches any decoder field-skip / re-encoder
// reordering bug.
func TestRegistryEncodeDecodeBytesEqual(t *testing.T) {
	want := &indexRegistryEntry{
		SchemaHash:  0x1234,
		Unique:      true,
		Root:        77,
		Count:       8,
		UserVersion: "abc",
		Columns:     []string{"a", "bb", "ccc"},
		Covering:    []string{"d"},
	}
	first, err := encodeRegistryEntry(want)
	if err != nil {
		t.Fatalf("encode 1: %v", err)
	}
	got, err := decodeRegistryEntry(first)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	second, err := encodeRegistryEntry(got)
	if err != nil {
		t.Fatalf("encode 2: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("encode-decode-encode bytes differ:\n  first=%x\n second=%x", first, second)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDecodeRegistryEntryRejectsForgedColumnCount (Inv-RV4): a forged
// ColumnCount on a truncated on-disk registry entry is rejected as
// errRegistryEntryShort without panicking or pre-allocating a
// count-sized slice — decodeRegistryEntry bounds colCount*2 against the
// remaining bytes before make([]string, colCount).
func TestDecodeRegistryEntryRejectsForgedColumnCount(t *testing.T) {
	data, err := encodeRegistryEntry(&indexRegistryEntry{
		SchemaHash: 1, Root: 2, Count: 3, Columns: []string{"a"},
	})
	if err != nil {
		t.Fatalf("encodeRegistryEntry: %v", err)
	}
	// ColumnCount is the u16 at offset 34 (32-byte fixed prefix + 2-byte
	// empty UserVersionLen). Forge it to 0xFFFF; the entry stays the same
	// (now-truncated) length.
	binary.LittleEndian.PutUint16(data[34:], 0xFFFF)
	if _, err := decodeRegistryEntry(data); !errors.Is(err, errRegistryEntryShort) {
		t.Fatalf("decodeRegistryEntry(forged ColumnCount) = %v, want errRegistryEntryShort", err)
	}
}

// TestDecodeRegistryEntryRejectsForgedCoveringCount (Inv-RV4): the
// symmetric pre-check for CoveringCount — a forged CoveringCount on a
// truncated entry is rejected before make([]string, covCount).
func TestDecodeRegistryEntryRejectsForgedCoveringCount(t *testing.T) {
	data, err := encodeRegistryEntry(&indexRegistryEntry{
		SchemaHash: 1, Root: 2, Count: 3, Columns: []string{"a"},
	})
	if err != nil {
		t.Fatalf("encodeRegistryEntry: %v", err)
	}
	// CoveringCount is the u16 immediately after the single 1-byte column
	// "a": 32 prefix + 2 uvLen + 2 colCount + 2 nameLen + 1 name = offset 39.
	binary.LittleEndian.PutUint16(data[39:], 0xFFFF)
	if _, err := decodeRegistryEntry(data); !errors.Is(err, errRegistryEntryShort) {
		t.Fatalf("decodeRegistryEntry(forged CoveringCount) = %v, want errRegistryEntryShort", err)
	}
}
