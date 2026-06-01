package gmdb

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// --- *Index handle invalidation regression tests for
// docs/specs/indexing.md §Handle Invalidation. These pin the two
// invariants stated in the spec section:
//
//   Inv-IHS1 (cursor-on-stale-tree): a *btree.Cursor opened by an
//   *Index iter is MarkStale'd before any same-tx code path completes
//   that frees or replaces the index data tree pages it walks.
//   Violation: mid-iter RebuildIndex/DropIndex/atomic Put yields wrong-
//   key reads or layout-decode panics from freed/reallocated pages.
//
//   Inv-IHS2 (post-drop handle dead): after tx.Indexes().Drop(ks, name),
//   every previously-handed-out *Index for (ks, name) rejects
//   subsequent Lookup/Range/Prefix/Get/Stats/LookupKeys with
//   ErrIndexNotFound. Violation: cached idx.Stats() returns the stale
//   pre-drop count; cached idx.Lookup walks freed root pages.

// TestIndexHandleStatsAfterDropReturnsErrIndexNotFound is the
// deterministic Inv-IHS2 regression. On HEAD, a cached *Index handle
// retains idx.pinned even after tx.DropIndex removes the entry from
// ks.indexes, so idx.Stats() returns IndexStats{Entries: prev_count}, nil
// — the user sees the index as still populated when it is gone. With
// the fix, the dead-handle check at Stats() entry returns
// ErrIndexNotFound.
func TestIndexHandleStatsAfterDropReturnsErrIndexNotFound(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
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
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := tx.Indexes().Drop("items", "by_color"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	stats, err := idx.Stats()
	if !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("Stats after Drop: err = %v, want ErrIndexNotFound", err)
	}
	if stats.Entries != 0 {
		t.Errorf("Stats after Drop: Count = %d, want 0 (handle is dead)", stats.Entries)
	}
}

// TestIndexHandleLookupAfterDropReturnsErrIndexNotFound asserts the
// iter surfaces report ErrIndexNotFound on a dead handle. The unique
// surface uses btree.Get (no cursor) and is exercised via Get; the
// non-unique surface uses the iteratePrefix cursor path. Both routes
// must respect the dead flag.
func TestIndexHandleLookupAfterDropReturnsErrIndexNotFound(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for _, k := range []string{"a", "b"} {
		if err := ks.Put([]byte(k), []byte{0x42}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := tx.Indexes().Drop("items", "by_color"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	// Lookup (non-unique iter path): yield nothing, idx.Err() = ErrIndexNotFound.
	yielded := 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
	}
	if yielded != 0 {
		t.Errorf("Lookup after Drop yielded %d entries, want 0", yielded)
	}
	if !errors.Is(idx.Err(), ErrIndexNotFound) {
		t.Errorf("Lookup after Drop: idx.Err() = %v, want ErrIndexNotFound", idx.Err())
	}
	// Range path: same shape.
	yielded = 0
	for range idx.Range(nil, nil) {
		yielded++
	}
	if yielded != 0 {
		t.Errorf("Range after Drop yielded %d entries, want 0", yielded)
	}
	if !errors.Is(idx.Err(), ErrIndexNotFound) {
		t.Errorf("Range after Drop: idx.Err() = %v, want ErrIndexNotFound", idx.Err())
	}
	// Prefix path: same shape.
	yielded = 0
	for range idx.Prefix() {
		yielded++
	}
	if yielded != 0 {
		t.Errorf("Prefix after Drop yielded %d entries, want 0", yielded)
	}
	if !errors.Is(idx.Err(), ErrIndexNotFound) {
		t.Errorf("Prefix after Drop: idx.Err() = %v, want ErrIndexNotFound", idx.Err())
	}
	// LookupKeys path.
	yielded = 0
	for range idx.LookupKeys([]byte{0x42}) {
		yielded++
	}
	if yielded != 0 {
		t.Errorf("LookupKeys after Drop yielded %d entries, want 0", yielded)
	}
	if !errors.Is(idx.Err(), ErrIndexNotFound) {
		t.Errorf("LookupKeys after Drop: idx.Err() = %v, want ErrIndexNotFound", idx.Err())
	}
}

// TestIndexHandleGetAfterDropReturnsErrIndexNotFound exercises the
// unique-Get path (no cursor; btree.Get on idx.pinned.root). Without
// the dead check, Get descends into the freed root → undefined.
func TestIndexHandleGetAfterDropReturnsErrIndexNotFound(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Unique = true
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("a"), []byte{0x42}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := tx.Indexes().Drop("items", "by_color"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	_, _, err = idx.Get([]byte{0x42})
	if !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("Get after Drop: err = %v, want ErrIndexNotFound", err)
	}
}

// TestIndexHandleInFlightRebuildSurfacesCursorStale exercises Inv-IHS1
// for tx.RebuildIndex. The user iterates a non-unique index; mid-iter
// calls tx.RebuildIndex which FreeSubtree's the old data tree. The
// closure's next c.Next() must surface gmdb.ErrCursorStale; on HEAD
// the cursor walks the freed pages and either yields stale entries or
// panics.
func TestIndexHandleInFlightRebuildSurfacesCursorStale(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// Two rows with the same first byte → non-unique Lookup yields two
	// entries; mutation triggers between the yields.
	for _, k := range []string{"a", "b"} {
		if err := ks.Put([]byte(k), []byte{0x42}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	yielded := 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
		if yielded == 1 {
			// Mid-iter rebuild: FreeSubtree's the old data tree and
			// publishes the new root via syncRebuildToCachedPinned.
			newDecl := testDecl("by_color", "color")
			newDecl.Extract = firstByteExtract
			if err := tx.Indexes().Rebuild("items", newDecl); err != nil {
				t.Fatalf("RebuildIndex: %v", err)
			}
		}
	}
	if yielded != 1 {
		t.Errorf("yielded = %d, want 1 (iter must terminate after the in-flight rebuild)", yielded)
	}
	if !errors.Is(idx.Err(), ErrCursorStale) {
		t.Errorf("idx.Err() = %v, want ErrCursorStale", idx.Err())
	}
}

// TestIndexHandleInFlightSiblingPutSurfacesCursorStale exercises Inv-
// IHS1 for the chunk-7.6 atomic Put path. applyIndexMaintenanceOnPut
// runs btree.Put on the index trees → CoWs pages reachable from the
// iter's cursor → the cursor's leaf-page refs become stale.
func TestIndexHandleInFlightSiblingPutSurfacesCursorStale(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for _, k := range []string{"a", "b"} {
		if err := ks.Put([]byte(k), []byte{0x42}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	yielded := 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
		if yielded == 1 {
			// Sibling Put on parent ks → atomic index maintenance →
			// index tree pages CoW'd → cursor's stack is stale.
			if err := ks.Put([]byte("c"), []byte{0x42}); err != nil {
				t.Fatalf("sibling Put: %v", err)
			}
		}
	}
	if yielded != 1 {
		t.Errorf("yielded = %d, want 1 (iter must terminate after the in-flight Put)", yielded)
	}
	if !errors.Is(idx.Err(), ErrCursorStale) {
		t.Errorf("idx.Err() = %v, want ErrCursorStale", idx.Err())
	}
}

// TestIndexHandleInFlightSiblingDeleteSurfacesCursorStale: atomic
// Delete path (applyIndexMaintenanceOnDelete) on the parent ks.
func TestIndexHandleInFlightSiblingDeleteSurfacesCursorStale(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
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
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	yielded := 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
		if yielded == 1 {
			if err := ks.Delete([]byte("c")); err != nil {
				t.Fatalf("sibling Delete: %v", err)
			}
		}
	}
	if yielded != 1 {
		t.Errorf("yielded = %d, want 1", yielded)
	}
	if !errors.Is(idx.Err(), ErrCursorStale) {
		t.Errorf("idx.Err() = %v, want ErrCursorStale", idx.Err())
	}
}

// TestIndexHandleInFlightDropSurfacesCursorStaleAndDead: a mid-iter
// DropIndex stales the in-flight cursor (ErrCursorStale) AND marks the
// handle dead (next call returns ErrIndexNotFound). Two assertions in
// one test — they are the same change set's behavior.
func TestIndexHandleInFlightDropSurfacesCursorStaleAndDead(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for _, k := range []string{"a", "b"} {
		if err := ks.Put([]byte(k), []byte{0x42}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	yielded := 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
		if yielded == 1 {
			if err := tx.Indexes().Drop("items", "by_color"); err != nil {
				t.Fatalf("DropIndex: %v", err)
			}
		}
	}
	if yielded != 1 {
		t.Errorf("yielded = %d, want 1 (mid-iter Drop stales the cursor)", yielded)
	}
	if !errors.Is(idx.Err(), ErrCursorStale) {
		t.Errorf("after mid-iter Drop: idx.Err() = %v, want ErrCursorStale", idx.Err())
	}
	// Second iter call after the in-flight Drop: handle is dead → next
	// iter sets idx.Err() = ErrIndexNotFound and yields nothing.
	yielded = 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
	}
	if yielded != 0 {
		t.Errorf("second iter after Drop yielded %d, want 0", yielded)
	}
	if !errors.Is(idx.Err(), ErrIndexNotFound) {
		t.Errorf("second iter after Drop: idx.Err() = %v, want ErrIndexNotFound", idx.Err())
	}
}

// TestIndexHandleAfterRebuildRePositionWorks: the canonical recovery
// pattern. After an in-flight rebuild stales the cursor, a fresh
// idx.Lookup re-opens a cursor on the new pinned.root and sees the
// rebuilt entries.
func TestIndexHandleAfterRebuildRePositionWorks(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
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
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	// Run rebuild before any iter.
	newDecl := testDecl("by_color", "color")
	newDecl.Extract = firstByteExtract
	if err := tx.Indexes().Rebuild("items", newDecl); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	// Fresh iter on the cached handle: descends from the NEW root.
	seen := map[string]struct{}{}
	for k := range idx.LookupKeys([]byte{0x42}) {
		seen[string(k)] = struct{}{}
	}
	if err := idx.Err(); err != nil {
		t.Errorf("post-rebuild iter: idx.Err() = %v, want nil", err)
	}
	for _, want := range []string{"a", "b", "c"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("post-rebuild LookupKeys missing %q", want)
		}
	}
}

// TestIndexHandleInFlightCursorDeleteSurfacesCursorStale exercises
// Inv-IHS1 for the chunk-7.6 indexed Cursor.Delete path on a
// Keyspace. Cursor.Delete on an indexed keyspace runs
// applyIndexMaintenanceOnDelete → mutates index trees; the
// open-coded markIndexHandlesStale call in keyspace.go's
// Cursor.Delete must stale every in-flight *Index iter cursor.
func TestIndexHandleInFlightCursorDeleteSurfacesCursorStale(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
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
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	yielded := 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
		if yielded == 1 {
			// Position a row cursor on "c" and Cursor.Delete it.
			rc := ks.Cursor()
			if k, _ := rc.SeekGE([]byte("c")); !bytes.Equal(k, []byte("c")) {
				t.Fatalf("SeekGE c: got %q", k)
			}
			if err := rc.Delete(); err != nil {
				t.Fatalf("Cursor.Delete: %v", err)
			}
		}
	}
	if yielded != 1 {
		t.Errorf("yielded = %d, want 1", yielded)
	}
	if !errors.Is(idx.Err(), ErrCursorStale) {
		t.Errorf("idx.Err() = %v, want ErrCursorStale", idx.Err())
	}
}

// --- SetKeyspace mirror ---------------------------------------------

// TestSetKeyspaceIndexHandleStatsAfterDropReturnsErrIndexNotFound:
// Inv-IHS2 on a SetKeyspace-anchored *Index.
func TestSetKeyspaceIndexHandleStatsAfterDropReturnsErrIndexNotFound(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	sks, err := tx.CreateSetKeyspace("members", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if _, err := sks.Put([]byte("a"), []byte{0x42}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := sks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := tx.Indexes().Drop("members", "by_color"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	if _, err := idx.Stats(); !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("SetKeyspace Stats after Drop: err = %v, want ErrIndexNotFound", err)
	}
}

// TestSetKeyspaceIndexHandleInFlightSetCursorDeleteSurfacesCursorStale
// exercises Inv-IHS1 on the SetCursor.Delete path: SetCursor.Delete
// delegates to SetKeyspace.DeleteValue which runs
// applyIndexMaintenanceOnRemoveValue → mutates index trees;
// markSetCursorsStale's consolidation calls markIndexHandlesStale,
// staling the in-flight *Index iter cursor.
func TestSetKeyspaceIndexHandleInFlightSetCursorDeleteSurfacesCursorStale(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	sks, err := tx.CreateSetKeyspace("members", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	for _, pair := range [][2]string{{"a", "X"}, {"b", "Y"}, {"c", "Z"}} {
		val := []byte{0x42, pair[1][0]}
		if _, err := sks.Put([]byte(pair[0]), val); err != nil {
			t.Fatalf("Put %q.%q: %v", pair[0], pair[1], err)
		}
	}
	idx, err := sks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	yielded := 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
		if yielded == 1 {
			sc := sks.Cursor()
			if k, v := sc.SeekGE([]byte("c")); !bytes.Equal(k, []byte("c")) || v == nil {
				t.Fatalf("SeekGE c: got (%q, %v)", k, v)
			}
			if err := sc.Delete(); err != nil {
				t.Fatalf("SetCursor.Delete: %v", err)
			}
		}
	}
	if yielded != 1 {
		t.Errorf("yielded = %d, want 1", yielded)
	}
	if !errors.Is(idx.Err(), ErrCursorStale) {
		t.Errorf("idx.Err() = %v, want ErrCursorStale", idx.Err())
	}
}

// TestSetKeyspaceIndexHandleInFlightSiblingPutSurfacesCursorStale:
// SetKeyspace.Put runs applyIndexMaintenanceOnAddValue → mutates the
// index tree → mid-iter cursor must surface ErrCursorStale.
func TestSetKeyspaceIndexHandleInFlightSiblingPutSurfacesCursorStale(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	sks, err := tx.CreateSetKeyspace("members", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	for _, pair := range [][2]string{{"a", "X"}, {"b", "X"}} {
		val := []byte{0x42, pair[1][0]}
		if _, err := sks.Put([]byte(pair[0]), val); err != nil {
			t.Fatalf("Put %q.%q: %v", pair[0], pair[1], err)
		}
	}
	idx, err := sks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	yielded := 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
		if yielded == 1 {
			if _, err := sks.Put([]byte("c"), []byte{0x42, 'Z'}); err != nil {
				t.Fatalf("sibling Put: %v", err)
			}
		}
	}
	if yielded != 1 {
		t.Errorf("yielded = %d, want 1", yielded)
	}
	if !errors.Is(idx.Err(), ErrCursorStale) {
		t.Errorf("idx.Err() = %v, want ErrCursorStale", idx.Err())
	}
}

// --- *Index handle invalidation by tx.DeleteKeyspace --------------------
//
// These pin Inv-IHS3 (post-DeleteKeyspace handle closed — indexing.md
// §Handle Invalidation): once tx.DeleteKeyspace succeeds on the parent
// keyspace, every subsequent call on every previously-handed-out
// *Index for that keyspace returns ErrKeyspaceClosed, and any in-
// flight *btree.Cursor opened by an idx iter closure is MarkStale'd
// before the FreeSubtree returns. The mid-iter case translates
// btree.ErrCursorStale to ErrKeyspaceClosed (not ErrCursorStale)
// at the public boundary — the "re-position to recover" semantic of
// ErrCursorStale does not apply when the parent keyspace is gone
// (mirroring row Cursor.Err's dead-check-wins ordering).

// TestIndexHandleBareErrAfterDeleteKeyspaceReturnsErrKeyspaceClosed
// pins the bare-Err() path: a user who opens an *Index, calls
// tx.DeleteKeyspace, then probes idx.Err() WITHOUT an intervening
// iter call must see ErrKeyspaceClosed. The entry-method guards on
// Lookup/Stats/etc. set idx.err on the iter path, but a user
// polling Err() directly never goes through those — so Err()
// itself probes keyspaceDead/idx.dead before returning the sticky
// idx.err (mirroring Cursor.Err's keyspace.go:1483-1489 ordering).
// Without this Err()-side guard, the bare path returns nil and the
// transactions.md §Cursor invalidation by DeleteKeyspace clause
// ("subsequent use ... returns ErrKeyspaceClosed") is silently
// violated.
func TestIndexHandleBareErrAfterDeleteKeyspaceReturnsErrKeyspaceClosed(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("a"), []byte{0x42}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := tx.DeleteKeyspace("items"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	// Bare Err() — no iter call between Index() and Err().
	if err := idx.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("bare Err() after DeleteKeyspace: %v, want ErrKeyspaceClosed", err)
	}
}

// TestStatsPreservesInFlightStaleSignal pins Round-3 H-1's
// regression boundary: Stats must NOT clear idx.err. A user who
// observes a mid-iter sibling-mutation sentinel via idx.Err()
// must continue observing it across an unrelated Stats() call —
// the Inv-IHS1 sticky-cause contract from chunk-7.6 / 5.6 says
// the iter cause survives until a fresh iter call resets it. A
// prior Round-2 cut reset idx.err at Stats entry to close Round-1
// L-1; that overshot the fix (Stats's keyspaceDead-first ordering
// already closes L-1 without the reset) and silently destroyed
// the stale signal on every Stats call. This test pins the fix.
func TestStatsPreservesInFlightStaleSignal(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for _, k := range []string{"a", "b"} {
		if err := ks.Put([]byte(k), []byte{0x42}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	// Mid-iter sibling Put → cursor MarkStale'd → idx.err =
	// ErrCursorStale via mapCursorErr.
	yielded := 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
		if yielded == 1 {
			if err := ks.Put([]byte("c"), []byte{0x42}); err != nil {
				t.Fatalf("sibling Put: %v", err)
			}
		}
	}
	if !errors.Is(idx.Err(), ErrCursorStale) {
		t.Fatalf("pre-Stats idx.Err() = %v, want ErrCursorStale (setup)", idx.Err())
	}
	// Unrelated Stats call on a live keyspace. Inv-IHS1's contract:
	// the stale signal survives. Round-2 H-1 violated this.
	stats, statsErr := idx.Stats()
	if statsErr != nil {
		t.Errorf("Stats on live ks: err = %v, want nil", statsErr)
	}
	if stats.Entries != 3 {
		t.Errorf("Stats.Count = %d, want 3 (a,b + the sibling Put 'c')", stats.Entries)
	}
	if !errors.Is(idx.Err(), ErrCursorStale) {
		t.Errorf("post-Stats idx.Err() = %v, want ErrCursorStale (Stats must not destroy the sticky stale signal)", idx.Err())
	}
}

// TestErrSymmetricWithStatsAfterDeleteKeyspace pins Round-3 M-1:
// the (bad-cols Lookup → DeleteKeyspace → bare Err) sequence must
// report ErrKeyspaceClosed on idx.Err() — symmetric with what
// idx.Stats() reports for the same state. Round-2's idx.err-first
// ordering would have returned the sticky ErrInvalidOptions wrap
// from the bad-cols Lookup, contradicting Stats. Round-3's
// keyspaceDead-first ordering closes the asymmetry on the
// Inv-IHS3 side while preserving Inv-IHS2's mid-iter Drop
// ErrCursorStale contract (verified by the pre-existing
// TestIndexHandleInFlightDropSurfacesCursorStaleAndDead which
// continues to pass).
func TestErrSymmetricWithStatsAfterDeleteKeyspace(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("a"), []byte{0x42}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	// Bad-cols Lookup (0 supplied, 1 declared) sets idx.err =
	// ErrInvalidOptions wrap.
	for range idx.Lookup() {
	}
	if !errors.Is(idx.Err(), ErrInvalidOptions) {
		t.Fatalf("setup: idx.Err() = %v, want ErrInvalidOptions wrap", idx.Err())
	}
	// DeleteKeyspace.
	if err := tx.DeleteKeyspace("items"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	// Stats reports the broader truth: ErrKeyspaceClosed.
	if _, err := idx.Stats(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("Stats after bad Lookup + DeleteKeyspace: err = %v, want ErrKeyspaceClosed", err)
	}
	// Err must report the same broader truth (Round-3 M-1 fix:
	// keyspaceDead-first ordering).
	if err := idx.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("Err after bad Lookup + DeleteKeyspace: %v, want ErrKeyspaceClosed (symmetric with Stats)", err)
	}
}

// TestIndexHandleDropThenDeleteReportsErrKeyspaceClosed pins the
// "broader truth wins" ordering of the entry-method guards: when
// both Inv-IHS2 (idx.dead, set by tx.DropIndex) and Inv-IHS3
// (idx.ks.dead, set by tx.DeleteKeyspace) hold for the same
// handle, every entry method reports ErrKeyspaceClosed (the
// broader sentinel) — NOT ErrIndexNotFound (the narrower one).
// indexing.md §Handle Invalidation explicitly records this:
//
//	"a handle whose index was dropped AND whose keyspace was then
//	 deleted in the same tx reports `ErrKeyspaceClosed` (the
//	 broader truth) rather than `ErrIndexNotFound`."
//
// Without this test, a future refactor swapping the entry-method
// guard order ("idx.dead first" looks like a micro-optimization
// because it's a field read vs. a method+pointer-check) would
// silently violate the spec while still passing every other
// existing test — none of which exercise both Drop + DeleteKeyspace
// on the same handle. Pinned across Stats / Err / Lookup / Get
// to defend the ordering invariant on all entry surfaces.
func TestIndexHandleDropThenDeleteReportsErrKeyspaceClosed(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("a"), []byte{0x42}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	// Drop first → idx.dead = true.
	if err := tx.Indexes().Drop("items", "by_color"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	// DeleteKeyspace second → ks.dead = true. Both dead-flags now hold.
	if err := tx.DeleteKeyspace("items"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	if _, err := idx.Stats(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("Stats: %v, want ErrKeyspaceClosed (broader truth wins over ErrIndexNotFound)", err)
	}
	if err := idx.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("Err: %v, want ErrKeyspaceClosed", err)
	}
	yielded := 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
	}
	if yielded != 0 {
		t.Errorf("Lookup yielded %d, want 0", yielded)
	}
	if !errors.Is(idx.Err(), ErrKeyspaceClosed) {
		t.Errorf("Lookup idx.Err(): %v, want ErrKeyspaceClosed", idx.Err())
	}
	if _, _, err := idx.Get([]byte{0x42}); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("Get: %v, want ErrKeyspaceClosed", err)
	}
}

// TestIndexHandleStatsAfterDeleteKeyspaceReturnsErrKeyspaceClosed is
// the deterministic Inv-IHS3 regression on the Stats path (no cursor,
// no back-lookup). On HEAD, Stats returns IndexStats{Entries: pre-delete
// count}, nil because the cached idx.pinned.count is read without
// consulting idx.ks.dead. With the fix, the ks.dead-check-wins guard
// at Stats entry returns ErrKeyspaceClosed.
func TestIndexHandleStatsAfterDeleteKeyspaceReturnsErrKeyspaceClosed(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
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
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := tx.DeleteKeyspace("items"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	stats, err := idx.Stats()
	if !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("Stats after DeleteKeyspace: err = %v, want ErrKeyspaceClosed", err)
	}
	if stats.Entries != 0 {
		t.Errorf("Stats after DeleteKeyspace: Count = %d, want 0 (handle is closed)", stats.Entries)
	}
}

// TestIndexHandleLookupAfterDeleteKeyspaceReturnsErrKeyspaceClosed
// exercises every cached-handle iter path post-DeleteKeyspace. The
// row Keyspace path is canonical: without the entry-time ks.dead
// guard, the closure descends from idx.pinned.root which
// retireIndexRegistry just FreeSubtree'd — yields the pre-delete
// entries (the freed pages are still readable until a subsequent
// allocation re-issues them), with idx.Err() = nil. The fix's
// entry-time guard returns ErrKeyspaceClosed before any descent.
func TestIndexHandleLookupAfterDeleteKeyspaceReturnsErrKeyspaceClosed(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for _, k := range []string{"a", "b"} {
		if err := ks.Put([]byte(k), []byte{0x42}); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := tx.DeleteKeyspace("items"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	// Lookup (non-unique iter path).
	yielded := 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
	}
	if yielded != 0 {
		t.Errorf("Lookup after DeleteKeyspace yielded %d entries, want 0", yielded)
	}
	if !errors.Is(idx.Err(), ErrKeyspaceClosed) {
		t.Errorf("Lookup after DeleteKeyspace: idx.Err() = %v, want ErrKeyspaceClosed", idx.Err())
	}
	// Range.
	yielded = 0
	for range idx.Range(nil, nil) {
		yielded++
	}
	if yielded != 0 {
		t.Errorf("Range after DeleteKeyspace yielded %d entries, want 0", yielded)
	}
	if !errors.Is(idx.Err(), ErrKeyspaceClosed) {
		t.Errorf("Range after DeleteKeyspace: idx.Err() = %v, want ErrKeyspaceClosed", idx.Err())
	}
	// Prefix.
	yielded = 0
	for range idx.Prefix() {
		yielded++
	}
	if yielded != 0 {
		t.Errorf("Prefix after DeleteKeyspace yielded %d entries, want 0", yielded)
	}
	if !errors.Is(idx.Err(), ErrKeyspaceClosed) {
		t.Errorf("Prefix after DeleteKeyspace: idx.Err() = %v, want ErrKeyspaceClosed", idx.Err())
	}
	// LookupKeys.
	yielded = 0
	for range idx.LookupKeys([]byte{0x42}) {
		yielded++
	}
	if yielded != 0 {
		t.Errorf("LookupKeys after DeleteKeyspace yielded %d entries, want 0", yielded)
	}
	if !errors.Is(idx.Err(), ErrKeyspaceClosed) {
		t.Errorf("LookupKeys after DeleteKeyspace: idx.Err() = %v, want ErrKeyspaceClosed", idx.Err())
	}
}

// TestIndexHandleGetAfterDeleteKeyspaceReturnsErrKeyspaceClosed
// exercises the unique-Get path (btree.Get on idx.pinned.root with no
// cursor — the closest analogue of Stats since it short-circuits before
// any extractPKAndValue back-lookup when the entry doesn't exist).
// Without the entry-time ks.dead guard, btree.Get descends the freed
// root → either yields stale data or panics.
func TestIndexHandleGetAfterDeleteKeyspaceReturnsErrKeyspaceClosed(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Unique = true
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("a"), []byte{0x42}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := tx.DeleteKeyspace("items"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	_, _, err = idx.Get([]byte{0x42})
	if !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("Get after DeleteKeyspace: err = %v, want ErrKeyspaceClosed", err)
	}
}

// TestIndexHandleInFlightDeleteKeyspaceSurfacesErrKeyspaceClosed
// exercises Inv-IHS3's in-flight branch: a mid-iter tx.DeleteKeyspace
// must MarkStale every in-flight *btree.Cursor opened by an idx iter
// closure before the FreeSubtree returns, AND the closure's err
// translation must report ErrKeyspaceClosed (not ErrCursorStale) when
// the parent ks is dead at translation time — matching row Cursor.Err's
// dead-check-wins ordering. On HEAD, Tx.DeleteKeyspace does NOT walk
// openIndexHandles to MarkStale, so the next c.Next() walks the just-
// FreeSubtree'd pages with no MarkStale signal.
func TestIndexHandleInFlightDeleteKeyspaceSurfacesErrKeyspaceClosed(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
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
	idx, err := ks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	yielded := 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
		if yielded == 1 {
			if err := tx.DeleteKeyspace("items"); err != nil {
				t.Fatalf("mid-iter DeleteKeyspace: %v", err)
			}
		}
	}
	if yielded != 1 {
		t.Errorf("yielded = %d, want 1 (mid-iter DeleteKeyspace stales the cursor)", yielded)
	}
	if !errors.Is(idx.Err(), ErrKeyspaceClosed) {
		t.Errorf("after mid-iter DeleteKeyspace: idx.Err() = %v, want ErrKeyspaceClosed", idx.Err())
	}
	// A subsequent iter call hits the entry-time ks.dead guard.
	yielded = 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
	}
	if yielded != 0 {
		t.Errorf("second iter after DeleteKeyspace yielded %d, want 0", yielded)
	}
	if !errors.Is(idx.Err(), ErrKeyspaceClosed) {
		t.Errorf("second iter after DeleteKeyspace: idx.Err() = %v, want ErrKeyspaceClosed", idx.Err())
	}
}

// --- SetKeyspace mirror -------------------------------------------------

// TestSetKeyspaceIndexHandleStatsAfterDeleteKeyspaceReturnsErrKeyspaceClosed:
// Inv-IHS3 on a SetKeyspace-anchored *Index, Stats path. Stats reads
// idx.pinned.count without any keyspace probe, so the entry-time
// sks.dead guard is the only mechanism — no accidental enforcement
// via extractSetKeyspacePKAndValue's HasValue back-lookup.
func TestSetKeyspaceIndexHandleStatsAfterDeleteKeyspaceReturnsErrKeyspaceClosed(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	sks, err := tx.CreateSetKeyspace("members", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if _, err := sks.Put([]byte("a"), []byte{0x42}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := sks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := tx.DeleteKeyspace("members"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	stats, err := idx.Stats()
	if !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("SetKeyspace Stats after DeleteKeyspace: err = %v, want ErrKeyspaceClosed", err)
	}
	if stats.Entries != 0 {
		t.Errorf("SetKeyspace Stats after DeleteKeyspace: Count = %d, want 0", stats.Entries)
	}
}

// TestSetKeyspaceIndexHandleLookupAfterDeleteKeyspaceReturnsErrKeyspaceClosed:
// Inv-IHS3 on the SetKeyspace iter path. Note: on HEAD this path
// surfaces ErrKeyspaceClosed accidentally via the back-lookup
// (extractSetKeyspacePKAndValue → sks.HasValue → sks.dead → err), so
// this test pins the entry-time guard's behavior rather than uniquely
// detecting the regression. Removing the entry-time guard AND
// HasValue's sks.dead check would surface this test as the wrong-yield
// shape; today the entry-time guard catches it cleanly without
// descending into any freed-page read.
func TestSetKeyspaceIndexHandleLookupAfterDeleteKeyspaceReturnsErrKeyspaceClosed(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	sks, err := tx.CreateSetKeyspace("members", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	for _, pair := range [][2]string{{"a", "X"}, {"b", "Y"}} {
		val := []byte{0x42, pair[1][0]}
		if _, err := sks.Put([]byte(pair[0]), val); err != nil {
			t.Fatalf("Put %q.%q: %v", pair[0], pair[1], err)
		}
	}
	idx, err := sks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if err := tx.DeleteKeyspace("members"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	yielded := 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
	}
	if yielded != 0 {
		t.Errorf("SetKeyspace Lookup after DeleteKeyspace yielded %d entries, want 0", yielded)
	}
	if !errors.Is(idx.Err(), ErrKeyspaceClosed) {
		t.Errorf("SetKeyspace Lookup after DeleteKeyspace: idx.Err() = %v, want ErrKeyspaceClosed", idx.Err())
	}
}

// TestSetKeyspaceIndexHandleInFlightDeleteKeyspaceSurfacesErrKeyspaceClosed:
// Inv-IHS3 in-flight branch, SetKeyspace. Mirrors the row-Keyspace
// mid-iter test. On HEAD, Tx.DeleteKeyspace's SetKeyspace branch does
// NOT walk openIndexHandles → in-flight cursor is not MarkStale'd
// → next c.Next() walks FreeSubtree'd pages.
func TestSetKeyspaceIndexHandleInFlightDeleteKeyspaceSurfacesErrKeyspaceClosed(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	decl := testDecl("by_color", "color")
	decl.Extract = firstByteExtract
	sks, err := tx.CreateSetKeyspace("members", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	for _, pair := range [][2]string{{"a", "X"}, {"b", "Y"}, {"c", "Z"}} {
		val := []byte{0x42, pair[1][0]}
		if _, err := sks.Put([]byte(pair[0]), val); err != nil {
			t.Fatalf("Put %q.%q: %v", pair[0], pair[1], err)
		}
	}
	idx, err := sks.Index("by_color")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	yielded := 0
	for range idx.Lookup([]byte{0x42}) {
		yielded++
		if yielded == 1 {
			if err := tx.DeleteKeyspace("members"); err != nil {
				t.Fatalf("mid-iter DeleteKeyspace: %v", err)
			}
		}
	}
	if yielded != 1 {
		t.Errorf("yielded = %d, want 1 (mid-iter DeleteKeyspace stales the cursor)", yielded)
	}
	if !errors.Is(idx.Err(), ErrKeyspaceClosed) {
		t.Errorf("after mid-iter SetKeyspace DeleteKeyspace: idx.Err() = %v, want ErrKeyspaceClosed", idx.Err())
	}
}
