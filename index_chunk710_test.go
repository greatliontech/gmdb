package gmdb

import (
	"context"
	"errors"
	"sort"
	"testing"
)

// --- Chunk 7.10: SetKeyspace.DeleteKeyspace three-subtree retirement

// TestDeleteSetKeyspaceWithIndexesRetiresAllSubtrees verifies that
// DeleteKeyspace on a SetKeyspace with declared indexes
// successfully retires the three sub-trees (data, per-index
// Kind=2, registry) — chunk-7.8's kind-agnostic retirement is
// asserted to work for Kind=1.
func TestDeleteSetKeyspaceWithIndexesRetiresAllSubtrees(t *testing.T) {
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
	for _, p := range [][2]string{{"u1", "alpha"}, {"u2", "bee"}} {
		if _, err := sks.Put([]byte(p[0]), []byte(p[1])); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if sks.desc.IndexRegistryRoot == 0 {
		t.Fatalf("IndexRegistryRoot 0 after Create with index")
	}
	if err := tx.DeleteKeyspace("subs"); err != nil {
		t.Fatalf("DeleteKeyspace SetKeyspace with indexes: %v", err)
	}
	names, err := tx.ListKeyspaces()
	if err != nil {
		t.Fatalf("ListKeyspaces: %v", err)
	}
	for _, n := range names {
		if n == "subs" {
			t.Errorf("DeleteKeyspace did not remove %q from ListKeyspaces", n)
		}
	}
}

// --- Chunk 7.10: SetKeyspace RebuildIndex with same-tx Puts ------

// TestRebuildIndexOnSetKeyspaceSeesSameTxPuts verifies that the
// chunk-7.10 SetKeyspace rebuild's SetCursor walk sees same-tx
// Put rows (matches indexing.md §Rebuild mechanics dirty-state
// contract).
func TestRebuildIndexOnSetKeyspaceSeesSameTxPuts(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	v1 := testDecl("by_topic", "topic")
	v1.Extract = func(_, _ []byte) []IndexEntry { return nil }
	sks, err := tx.CreateSetKeyspace("subs", nil, v1)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	for _, p := range [][2]string{{"u1", "alpha"}, {"u2", "beta"}, {"u3", "carrot"}} {
		if _, err := sks.Put([]byte(p[0]), []byte(p[1])); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if sks.indexes["by_topic"].count != 0 {
		t.Fatalf("pre-rebuild count: got %d want 0 (v1 emits nothing)", sks.indexes["by_topic"].count)
	}
	// Rebuild with extractor that DOES emit entries.
	v2 := testDecl("by_topic", "topic")
	v2.Extract = setKeyspaceFirstByteExtract
	v2.Version = "v2"
	if err := tx.Indexes().Rebuild("subs", v2); err != nil {
		t.Fatalf("RebuildIndex SetKeyspace: %v", err)
	}
	if got := sks.indexes["by_topic"].count; got != 3 {
		t.Errorf("post-rebuild count: got %d want 3 (same-tx Puts must be visible)", got)
	}
}

// TestRebuildIndexOnSetKeyspaceUniqueViolation verifies that the
// chunk-7.10 SetKeyspace rebuild aborts cleanly on a unique
// violation produced by the new extractor.
func TestRebuildIndexOnSetKeyspaceUniqueViolation(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	v1 := testDecl("by_topic", "topic")
	v1.Unique = true
	v1.Extract = setKeyspaceFirstByteExtract
	sks, err := tx.CreateSetKeyspace("subs", nil, v1)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	// Two members with distinct first-bytes (no v1 conflict).
	if _, err := sks.Put([]byte("u1"), []byte("alpha")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := sks.Put([]byte("u2"), []byte("bee")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	prevRoot := sks.indexes["by_topic"].root
	// v2 emits a constant key across all members → collision.
	v2 := testDecl("by_topic", "topic")
	v2.Unique = true
	v2.Extract = func(_, _ []byte) []IndexEntry {
		return []IndexEntry{{Cols: [][]byte{{0xFF}}}}
	}
	err = tx.Indexes().Rebuild("subs", v2)
	if !errors.Is(err, ErrIndexUniqueViolation) {
		t.Fatalf("expected ErrIndexUniqueViolation, got %v", err)
	}
	if sks.indexes["by_topic"].root != prevRoot {
		t.Errorf("pinned root mutated on failed rebuild: %d != %d", sks.indexes["by_topic"].root, prevRoot)
	}
}

// --- Chunk 7.10: indexed Keyspace.DeleteRange fallback -----------

// TestKeyspaceIndexedDeleteRangeClearsIndexEntries verifies the
// chunk-7.10 Keyspace.DeleteRange indexed fallback: each row's
// index entries are deleted alongside the row.
func TestKeyspaceIndexedDeleteRangeClearsIndexEntries(t *testing.T) {
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
	for i, k := range []string{"a", "b", "c", "d"} {
		if err := ks.Put([]byte(k), []byte{byte('R' + i)}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if ks.indexes["by_color"].count != 4 {
		t.Fatalf("pre-DeleteRange count: got %d want 4", ks.indexes["by_color"].count)
	}
	// Delete b..d (exclusive on d) → deletes b, c. Two rows + two
	// index entries should vanish.
	deleted, err := ks.DeleteRange([]byte("b"), []byte("d"))
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if deleted != 2 {
		t.Errorf("DeleteRange count: got %d want 2", deleted)
	}
	if ks.indexes["by_color"].count != 2 {
		t.Errorf("post-DeleteRange index count: got %d want 2", ks.indexes["by_color"].count)
	}
	// Verify a and d remain.
	if _, err := ks.Get([]byte("a")); err != nil {
		t.Errorf("a missing post-DeleteRange: %v", err)
	}
	if _, err := ks.Get([]byte("d")); err != nil {
		t.Errorf("d missing post-DeleteRange: %v", err)
	}
}

// TestKeyspaceIndexedDeleteRangeOpenBounds verifies that open
// bounds (nil start / nil end) work on indexed keyspaces.
func TestKeyspaceIndexedDeleteRangeOpenBounds(t *testing.T) {
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
			t.Fatalf("Put: %v", err)
		}
	}
	deleted, err := ks.DeleteRange(nil, nil)
	if err != nil {
		t.Fatalf("DeleteRange(nil, nil): %v", err)
	}
	if deleted != 3 {
		t.Errorf("DeleteRange(nil, nil) count: got %d want 3", deleted)
	}
	if ks.indexes["by_color"].count != 0 {
		t.Errorf("post-DeleteRange-all index count: got %d want 0", ks.indexes["by_color"].count)
	}
}

// --- Chunk 7.10: indexed SetKeyspace.DeleteRange fallback --------

// TestSetKeyspaceIndexedDeleteRangeClearsIndexEntries verifies
// that SetKeyspace.DeleteRange on an indexed keyspace transparently
// uses the chunk-7.9 per-(setKey, setValue) bulk-key delete walk
// (inherited via the chunk-6.8 per-key Delete loop).
func TestSetKeyspaceIndexedDeleteRangeClearsIndexEntries(t *testing.T) {
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
	// Per-user-key set of (key, value) pairs.
	pairs := []struct{ k, v string }{
		{"u1", "alpha"}, {"u1", "apple"}, // u1 has 2 values
		{"u2", "bee"},    // u2 has 1
		{"u3", "carrot"}, // u3 has 1
	}
	for _, p := range pairs {
		if _, err := sks.Put([]byte(p.k), []byte(p.v)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if sks.indexes["by_topic"].count != 4 {
		t.Fatalf("pre-DeleteRange count: got %d want 4", sks.indexes["by_topic"].count)
	}
	// Delete u1 and u2 (key range ["u1", "u3")), leaving u3.
	deleted, err := sks.DeleteRange([]byte("u1"), []byte("u3"))
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	// Returns count of VALUES deleted, not keys.
	if deleted != 3 {
		t.Errorf("DeleteRange returned %d want 3 (values: 2+1+0=3)", deleted)
	}
	if sks.indexes["by_topic"].count != 1 {
		t.Errorf("post-DeleteRange index count: got %d want 1 (u3's carrot remains)",
			sks.indexes["by_topic"].count)
	}
	// Verify u3 still in the SetKeyspace.
	idx, _ := sks.Index("by_topic")
	var found []string
	for sk, sv := range idx.Lookup([]byte{'c'}) {
		found = append(found, string(sk)+"/"+string(sv))
	}
	sort.Strings(found)
	if len(found) != 1 || found[0] != "u3/carrot" {
		t.Errorf("Lookup post-DeleteRange: got %v want [u3/carrot]", found)
	}
}
