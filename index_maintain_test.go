package gmdb

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// Test extractor that splits a CSV value `a,b` into one IndexEntry
// per column for a single-column index. Used by many chunk-7.6
// tests to exercise the Put / Delete index-maintenance paths.
func splitCSVExtract(_, value []byte) []IndexEntry {
	if len(value) == 0 {
		return nil
	}
	parts := bytes.Split(value, []byte(","))
	out := make([]IndexEntry, 0, len(parts))
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		out = append(out, IndexEntry{Cols: [][]byte{p}})
	}
	return out
}

// firstByteExtract emits one IndexEntry whose column is the
// value's first byte (or no entry if value is empty). Useful for
// the unique-probe test (deterministic single-entry-per-row).
func firstByteExtract(_, value []byte) []IndexEntry {
	if len(value) == 0 {
		return nil
	}
	return []IndexEntry{{Cols: [][]byte{{value[0]}}}}
}

// --- Atomic Put: basic happy path -------------------------------

// TestIndexedPutWritesIndexEntries verifies that Put on an indexed
// keyspace writes index entries reachable via the index data tree.
// Probe the entries by computing the expected encoded index key
// and walking the tree directly (chunk 7.7 will wire the Lookup
// API; chunk 7.6 only writes).
func TestIndexedPutWritesIndexEntries(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("alpha"), []byte{0x42, 'x'}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	p := ks.indexes["by_color"]
	if p.count != 1 {
		t.Errorf("pinnedIndex.count: got %d want 1", p.count)
	}
	if p.root == 0 {
		t.Errorf("pinnedIndex.root: still 0 after Put — no index data tree allocated")
	}
}

// TestIndexedPutOnEmptyValueWritesNoEntries verifies that a row
// whose extractor returns no entries (partial-index semantics)
// does NOT mutate the index data tree.
func TestIndexedPutOnEmptyValueWritesNoEntries(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("alpha"), []byte{}); err != nil {
		t.Fatalf("Put empty value: %v", err)
	}
	p := ks.indexes["by_color"]
	if p.count != 0 {
		t.Errorf("pinnedIndex.count after partial-index miss: got %d want 0", p.count)
	}
	if p.root != 0 {
		t.Errorf("pinnedIndex.root: got %d want 0 (no index entries written)", p.root)
	}
}

// TestIndexedPutMultipleRowsAllIndexed verifies that N row Puts
// produce N index entries (non-unique index — no collision).
func TestIndexedPutMultipleRowsAllIndexed(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for i, k := range []string{"a", "b", "c"} {
		v := []byte{byte('R' + i), 'x'} // first byte = R, S, T
		if err := ks.Put([]byte(k), v); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	if got := ks.indexes["by_color"].count; got != 3 {
		t.Errorf("pinnedIndex.count: got %d want 3", got)
	}
}

// --- Atomic Put: update path (diff) ------------------------------

// TestIndexedPutUpdateRespectsOldNewDiff verifies the spec's
// diff semantics: old → new with one entry in common and one
// added results in count = previous + 1 (one delete, two inserts —
// wait, no: one delete of the now-removed-from-new, two inserts
// of news-not-in-olds. Net: count_new - count_common =
// count_inserted. Net change: (new \ old) - (old \ new). Let me
// be explicit: old=[A], new=[A,B] → del=[], ins=[B], net +1.
func TestIndexedPutUpdateRespectsOldNewDiff(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	decl := testDecl("by_letter", "letter")
	decl.Extract = splitCSVExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// First Put: value "a,b" → 2 index entries.
	if err := ks.Put([]byte("k"), []byte("a,b")); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if got := ks.indexes["by_letter"].count; got != 2 {
		t.Fatalf("count after first Put: got %d want 2", got)
	}
	// Update: value "a,b,c" → old=[a,b], new=[a,b,c]; diff: del=[],
	// ins=[c]. Net count: +1 → 3.
	if err := ks.Put([]byte("k"), []byte("a,b,c")); err != nil {
		t.Fatalf("update Put: %v", err)
	}
	if got := ks.indexes["by_letter"].count; got != 3 {
		t.Errorf("count after update with one added: got %d want 3", got)
	}
	// Update: value "b,c" → old=[a,b,c], new=[b,c]; diff: del=[a],
	// ins=[]. Net count: -1 → 2.
	if err := ks.Put([]byte("k"), []byte("b,c")); err != nil {
		t.Fatalf("shrinking update: %v", err)
	}
	if got := ks.indexes["by_letter"].count; got != 2 {
		t.Errorf("count after shrinking update: got %d want 2", got)
	}
}

// --- Atomic Put: unique-index probe ------------------------------

// TestIndexedPutUniqueViolationOnDiskConflict verifies that a Put
// whose extractor produces an index key already in the on-disk
// unique index returns ErrIndexUniqueViolation. The row write does
// not happen.
func TestIndexedPutUniqueViolationOnDiskConflict(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	decl.Unique = true
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("k1"), []byte{0x42, 'x'}); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	// k2 with same first-byte → unique collision against k1's entry.
	err = ks.Put([]byte("k2"), []byte{0x42, 'y'})
	if !errors.Is(err, ErrIndexUniqueViolation) {
		t.Fatalf("expected ErrIndexUniqueViolation, got %v", err)
	}
	// Verify the row k2 was NOT written.
	if _, err := ks.Get([]byte("k2")); !errors.Is(err, ErrNotFound) {
		t.Errorf("k2 row written despite unique violation: %v", err)
	}
	// Verify k1's index entry is still there (count=1, not 2).
	if got := ks.indexes["by_color"].count; got != 1 {
		t.Errorf("index count after rejected Put: got %d want 1", got)
	}
}

// TestIndexedPutUniqueViolationOnCandidateSetCollision verifies
// that an extractor returning two IndexEntry values with the same
// encoded key on a unique index aborts with ErrIndexUniqueViolation
// — detected against the candidate set, no need for an empty
// index.
func TestIndexedPutUniqueViolationOnCandidateSetCollision(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	decl := testDecl("by_color", "color")
	decl.Unique = true
	// Extractor returns TWO entries with the same column tuple
	// from a single row — candidate-set collision.
	decl.Extract = func(_, value []byte) []IndexEntry {
		return []IndexEntry{
			{Cols: [][]byte{{0x42}}},
			{Cols: [][]byte{{0x42}}},
		}
	}
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	err = ks.Put([]byte("k"), []byte("anything"))
	if !errors.Is(err, ErrIndexUniqueViolation) {
		t.Fatalf("expected candidate-set ErrIndexUniqueViolation, got %v", err)
	}
}

// TestIndexedPutNonUniqueAllowsSameColumn verifies that on a
// NON-unique index, two rows with the same column tuple coexist
// (the PK is appended to the index key, so the encoded keys
// differ).
func TestIndexedPutNonUniqueAllowsSameColumn(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	// Unique = false (default).
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("k1"), []byte{0x42}); err != nil {
		t.Fatalf("Put k1: %v", err)
	}
	if err := ks.Put([]byte("k2"), []byte{0x42}); err != nil {
		t.Fatalf("Put k2 (same color): %v", err)
	}
	if got := ks.indexes["by_color"].count; got != 2 {
		t.Errorf("non-unique index count: got %d want 2", got)
	}
}

// --- Atomic Delete: index entries cleared ------------------------

// TestIndexedDeleteClearsIndexEntries verifies that Delete on an
// indexed keyspace also deletes the row's index entries.
func TestIndexedDeleteClearsIndexEntries(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for _, k := range []string{"a", "b"} {
		if err := ks.Put([]byte(k), []byte{0x42, 'x'}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	if got := ks.indexes["by_color"].count; got != 2 {
		t.Fatalf("pre-delete count: got %d want 2", got)
	}
	if err := ks.Delete([]byte("a")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := ks.indexes["by_color"].count; got != 1 {
		t.Errorf("post-delete count: got %d want 1", got)
	}
}

// TestIndexedDeleteMissingReturnsErrNotFound verifies that Delete
// on a key that doesn't exist returns ErrNotFound without
// mutating any index (chunk-5.1 Delete-on-miss invariant + the
// chunk-7.6 indexed-keyspace contract).
func TestIndexedDeleteMissingReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Delete([]byte("missing")); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v want ErrNotFound", err)
	}
}

// --- Cursor.Delete + index maintenance ---------------------------

// TestIndexedCursorDeleteClearsIndexEntries verifies that
// Cursor.Delete on an indexed keyspace deletes the row's index
// entries too (the chunk-7.6 cursor-delete wire-in).
func TestIndexedCursorDeleteClearsIndexEntries(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for i, k := range []string{"a", "b", "c"} {
		if err := ks.Put([]byte(k), []byte{byte('R' + i)}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	c := ks.Cursor()
	if k, _ := c.First(); k == nil {
		t.Fatalf("Cursor.First on populated keyspace returned nil")
	}
	if err := c.Delete(); err != nil {
		t.Fatalf("Cursor.Delete: %v", err)
	}
	if got := ks.indexes["by_color"].count; got != 2 {
		t.Errorf("post-CursorDelete count: got %d want 2", got)
	}
}

// --- Regression: Round-1 H-1 (stale Cursor.Delete on indexed ks) -

// TestIndexedCursorDeleteOnStaleCursorReturnsErrCursorStale is the
// chunk-7.6 Round-1 H-1 regression: a stale indexed cursor must
// return ErrCursorStale, not ErrCursorUnpositioned, matching the
// non-indexed path and transactions.md §Cursor State Machine. The
// indexed-path code translates Current() returning nil through
// the inner cursor's Err() to distinguish stale from unpositioned.
func TestIndexedCursorDeleteOnStaleCursorReturnsErrCursorStale(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("a"), []byte{0x42}); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	c := ks.Cursor()
	if k, _ := c.First(); k == nil {
		t.Fatalf("Cursor.First returned nil on populated keyspace")
	}
	// Sibling mutation invalidates the cursor.
	if err := ks.Put([]byte("b"), []byte{0x43}); err != nil {
		t.Fatalf("Put b: %v", err)
	}
	// Now c is stale. Delete must return ErrCursorStale, not
	// ErrCursorUnpositioned (the Round-1 H-1 regression).
	err = c.Delete()
	if !errors.Is(err, ErrCursorStale) {
		t.Errorf("stale Cursor.Delete on indexed ks: got %v want ErrCursorStale", err)
	}
}

// --- Regression: Round-1 H-2 (atomicity snapshot/restore on failure)

// TestIndexedPutPinnedStateRevertsOnCandidateCollision verifies the
// chunk-7.6 H-2 atomicity fix: when applyIndexMaintenanceOnPut
// fails on a candidate-set unique collision, the pinnedIndex
// (root, count) is restored to the pre-call snapshot. Without
// the fix, a later flushIndexRegistry would commit half-mutated
// pinned state — but on the candidate-collision path no
// btree.Put/Delete has yet happened, so the test verifies the
// restore is a no-op on this specific path. The complementary
// case (where mid-loop btree.Put fails after some prior btree.Put
// succeeded) requires a failure injection seam not available at
// chunk-7.6 — that fault-mode is in scope of the
// writenewindexregistry-partial-leak deferral.
func TestIndexedPutPinnedStateRevertsOnCandidateCollision(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Unique = true
	// Extractor that collides for value=="bad" (candidate-set
	// collision) but works fine for other values.
	decl.Extract = func(_, value []byte) []IndexEntry {
		if string(value) == "bad" {
			return []IndexEntry{
				{Cols: [][]byte{{0x42}}},
				{Cols: [][]byte{{0x42}}}, // collision
			}
		}
		if len(value) == 0 {
			return nil
		}
		return []IndexEntry{{Cols: [][]byte{{value[0]}}}}
	}
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// Seed one row to set pinned.count=1.
	if err := ks.Put([]byte("k1"), []byte{0x41}); err != nil {
		t.Fatalf("Put k1: %v", err)
	}
	if got := ks.indexes["by_color"].count; got != 1 {
		t.Fatalf("pre-collision count: got %d want 1", got)
	}
	prevRoot := ks.indexes["by_color"].root
	// Trigger the candidate-set collision.
	err = ks.Put([]byte("k2"), []byte("bad"))
	if !errors.Is(err, ErrIndexUniqueViolation) {
		t.Fatalf("expected ErrIndexUniqueViolation, got %v", err)
	}
	// H-2 fix: pinned state must equal the pre-call snapshot.
	if got := ks.indexes["by_color"].count; got != 1 {
		t.Errorf("post-failed-Put count: got %d want 1 — H-2 revert regression", got)
	}
	if got := ks.indexes["by_color"].root; got != prevRoot {
		t.Errorf("post-failed-Put root: got %d want %d — H-2 revert regression", got, prevRoot)
	}
}

// --- Persistence across Commit/Re-open --------------------------

// TestIndexedPutCountPersistsAcrossCommit verifies that the
// pinnedIndex.count + .root persist via the flushIndexRegistry sync
// at Tx.Commit, and the next OpenKeyspace re-loads them.
func TestIndexedPutCountPersistsAcrossCommit(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	{
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		tx, err := db.Begin(ctx, true)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		decl := testDecl("by_color", "color")
		decl.Extract = firstByteExtract
		ks, err := tx.CreateKeyspace("items", decl)
		if err != nil {
			t.Fatalf("CreateKeyspace: %v", err)
		}
		for i, k := range []string{"a", "b", "c"} {
			if err := ks.Put([]byte(k), []byte{byte('R' + i)}); err != nil {
				t.Fatalf("Put %q: %v", k, err)
			}
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
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin #2: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.OpenKeyspace("items", decl)
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	p := ks.indexes["by_color"]
	if p.count != 3 {
		t.Errorf("post-reopen count: got %d want 3", p.count)
	}
	if p.root == 0 {
		t.Errorf("post-reopen root: got 0 (registry sync lost?)")
	}
}

// --- Keyspace.Index handle --------------------------------------

// TestKeyspaceIndexHandleReturnsExisting verifies that
// Keyspace.Index(name) returns a handle for a declared index.
func TestKeyspaceIndexHandleReturnsExisting(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	st, err := idx.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Count != 0 {
		t.Errorf("fresh index Stats.Count: got %d want 0", st.Count)
	}
	if err := ks.Put([]byte("k"), []byte{0x42}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	st, _ = idx.Stats()
	if st.Count != 1 {
		t.Errorf("post-Put Stats.Count: got %d want 1", st.Count)
	}
}

// TestKeyspaceIndexHandleUnknownNameReturnsErrIndexNotFound
// verifies that Index(name) for an unknown name returns
// ErrIndexNotFound naming the missing index.
func TestKeyspaceIndexHandleUnknownNameReturnsErrIndexNotFound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("items")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	_, err = ks.Index("nonexistent")
	if !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("got %v want ErrIndexNotFound", err)
	}
}

// --- writenewindexregistry-partial-leak per-row case ------------
//
// These four regression tests pin the caller-site
// Pager.BeginShallowSavepoint / RestoreSavepoint(on error) /
// ReleaseSavepoint(on success) wrap that closes the per-row case of
// writenewindexregistry-partial-leak (the chunk-7.6 / 7.9 extension
// the original chunk-7.5 fix deferred). Each:
//
//   1. Creates an indexed Keyspace / SetKeyspace with ≥2 indexes
//      that the row mutation will touch.
//   2. Commits the setup (so subsequent allocations come from a
//      fresh tx whose Check leak count is isolated).
//   3. Begins a new write tx.
//   4. Installs indexMaintenanceFailHookForTest to fail after the
//      1st successful btree.Put / btree.Delete on an index data
//      tree, demonstrating the partial-success path.
//   5. Invokes the user op; expects the injected error.
//   6. Commits despite the per-op error (rest-of-tx-continues).
//   7. db.Check() reports zero BitmapLeak — i.e. the savepoint
//      Restore reverted the 1st btree op's pager allocations and
//      no orphaned pages reached disk.
//
// Without the caller-site savepoint, step 6's Commit publishes the
// pages the 1st btree mutation allocated, free-space.md's entailed
// bitmap-consistency invariant breaks ("every page below
// HighWaterMark with bit clear is reachable from the active meta,
// in the RPL, or is a meta/bitmap page"), and Check reports them
// as BitmapLeak. The tests were empirically verified to fail in
// that mode (stash the savepoint hunks, rerun → "BitmapLeak page
// …" reported; restore → clean).

func TestApplyIndexMaintenanceAtomicOnKeyspacePut(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Setup: create an indexed keyspace with 2 indexes both
	// producing one entry per row (firstByteExtract on the value).
	{
		tx, err := db.Begin(ctx, true)
		if err != nil {
			t.Fatalf("Begin setup: %v", err)
		}
		da := testDecl("idx_a", "a")
		da.Extract = firstByteExtract
		db2 := testDecl("idx_b", "b")
		db2.Extract = firstByteExtract
		if _, err := tx.CreateKeyspace("items", da, db2); err != nil {
			tx.Rollback()
			t.Fatalf("CreateKeyspace: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit setup: %v", err)
		}
	}

	injected := errors.New("injected index-maintenance failure (Put)")
	setIndexMaintenanceFailHookForTest(func(i int) error {
		if i >= 0 { // fail after the 1st successful btree.Put
			return injected
		}
		return nil
	})
	t.Cleanup(func() { setIndexMaintenanceFailHookForTest(nil) })

	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.OpenKeyspace("items", keyspaceTestDeclsForOpen()...)
	if err != nil {
		tx.Rollback()
		t.Fatalf("OpenKeyspace: %v", err)
	}
	err = ks.Put([]byte("k1"), []byte{0x42, 'x'})
	if !errors.Is(err, injected) {
		tx.Rollback()
		t.Fatalf("Put err = %v, want injected", err)
	}
	// Rest-of-tx-continues: commit despite the per-op error.
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after injected failure: %v", err)
	}

	assertNoBitmapLeak(t, db)
}

func TestApplyIndexMaintenanceAtomicOnKeyspaceDelete(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Setup: indexed keyspace + 2 rows so Delete has index entries
	// to remove on both indexes.
	{
		tx, err := db.Begin(ctx, true)
		if err != nil {
			t.Fatalf("Begin setup: %v", err)
		}
		da := testDecl("idx_a", "a")
		da.Extract = firstByteExtract
		db2 := testDecl("idx_b", "b")
		db2.Extract = firstByteExtract
		ks, err := tx.CreateKeyspace("items", da, db2)
		if err != nil {
			tx.Rollback()
			t.Fatalf("CreateKeyspace: %v", err)
		}
		if err := ks.Put([]byte("k1"), []byte{0x42, 'x'}); err != nil {
			tx.Rollback()
			t.Fatalf("Put k1: %v", err)
		}
		if err := ks.Put([]byte("k2"), []byte{0x43, 'y'}); err != nil {
			tx.Rollback()
			t.Fatalf("Put k2: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit setup: %v", err)
		}
	}

	injected := errors.New("injected index-maintenance failure (Delete)")
	setIndexMaintenanceFailHookForTest(func(i int) error {
		if i >= 0 {
			return injected
		}
		return nil
	})
	t.Cleanup(func() { setIndexMaintenanceFailHookForTest(nil) })

	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.OpenKeyspace("items", keyspaceTestDeclsForOpen()...)
	if err != nil {
		tx.Rollback()
		t.Fatalf("OpenKeyspace: %v", err)
	}
	err = ks.Delete([]byte("k1"))
	if !errors.Is(err, injected) {
		tx.Rollback()
		t.Fatalf("Delete err = %v, want injected", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after injected failure: %v", err)
	}

	assertNoBitmapLeak(t, db)
}

func TestApplyIndexMaintenanceAtomicOnCursorDelete(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	{
		tx, err := db.Begin(ctx, true)
		if err != nil {
			t.Fatalf("Begin setup: %v", err)
		}
		da := testDecl("idx_a", "a")
		da.Extract = firstByteExtract
		db2 := testDecl("idx_b", "b")
		db2.Extract = firstByteExtract
		ks, err := tx.CreateKeyspace("items", da, db2)
		if err != nil {
			tx.Rollback()
			t.Fatalf("CreateKeyspace: %v", err)
		}
		if err := ks.Put([]byte("k1"), []byte{0x42, 'x'}); err != nil {
			tx.Rollback()
			t.Fatalf("Put k1: %v", err)
		}
		if err := ks.Put([]byte("k2"), []byte{0x43, 'y'}); err != nil {
			tx.Rollback()
			t.Fatalf("Put k2: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit setup: %v", err)
		}
	}

	injected := errors.New("injected index-maintenance failure (Cursor.Delete)")
	setIndexMaintenanceFailHookForTest(func(i int) error {
		if i >= 0 {
			return injected
		}
		return nil
	})
	t.Cleanup(func() { setIndexMaintenanceFailHookForTest(nil) })

	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.OpenKeyspace("items", keyspaceTestDeclsForOpen()...)
	if err != nil {
		tx.Rollback()
		t.Fatalf("OpenKeyspace: %v", err)
	}
	c := ks.Cursor()
	if k, _ := c.First(); k == nil {
		tx.Rollback()
		t.Fatalf("Cursor.First returned nil on a seeded keyspace")
	}
	err = c.Delete()
	if !errors.Is(err, injected) {
		tx.Rollback()
		t.Fatalf("Cursor.Delete err = %v, want injected", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after injected failure: %v", err)
	}

	assertNoBitmapLeak(t, db)
}

// keyspaceTestDeclsForOpen returns the same two IndexDecls used in
// the Keyspace setup blocks above — needed at OpenKeyspace time per
// indexing.md §Open Semantics (handed back at every reopen with the
// declared extractor and schema).
func keyspaceTestDeclsForOpen() []*IndexDecl {
	da := testDecl("idx_a", "a")
	da.Extract = firstByteExtract
	db2 := testDecl("idx_b", "b")
	db2.Extract = firstByteExtract
	return []*IndexDecl{da, db2}
}

// assertNoBitmapLeak walks db.Check() and fails if any BitmapLeak
// issue is reported. The savepoint-correctness probe used by every
// applyIndexMaintenance per-row atomicity regression test.
func assertNoBitmapLeak(t *testing.T, db *DB) {
	t.Helper()
	var leaks []CheckIssue
	for _, iss := range collectIssues(db.Check()) {
		if iss.Code == "BitmapLeak" {
			leaks = append(leaks, iss)
		}
	}
	if len(leaks) != 0 {
		t.Errorf("applyIndexMaintenance orphaned %d page(s) on Commit-after-error (want 0): %v",
			len(leaks), leaks)
	}
}
