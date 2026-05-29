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
//   Inv-IHS2 (post-drop handle dead): after tx.DropIndex(ks, name),
//   every previously-handed-out *Index for (ks, name) rejects
//   subsequent Lookup/Range/Prefix/Get/Stats/LookupKeys with
//   ErrIndexNotFound. Violation: cached idx.Stats() returns the stale
//   pre-drop count; cached idx.Lookup walks freed root pages.

// TestIndexHandleStatsAfterDropReturnsErrIndexNotFound is the
// deterministic Inv-IHS2 regression. On HEAD, a cached *Index handle
// retains idx.pinned even after tx.DropIndex removes the entry from
// ks.indexes, so idx.Stats() returns IndexStats{Count: prev_count}, nil
// — the user sees the index as still populated when it is gone. With
// the fix, the dead-handle check at Stats() entry returns
// ErrIndexNotFound.
func TestIndexHandleStatsAfterDropReturnsErrIndexNotFound(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx, true)
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
	if err := tx.DropIndex("items", "by_color"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	stats, err := idx.Stats()
	if !errors.Is(err, ErrIndexNotFound) {
		t.Errorf("Stats after Drop: err = %v, want ErrIndexNotFound", err)
	}
	if stats.Count != 0 {
		t.Errorf("Stats after Drop: Count = %d, want 0 (handle is dead)", stats.Count)
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
	tx, _ := db.Begin(ctx, true)
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
	if err := tx.DropIndex("items", "by_color"); err != nil {
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
	tx, _ := db.Begin(ctx, true)
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
	if err := tx.DropIndex("items", "by_color"); err != nil {
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
	tx, _ := db.Begin(ctx, true)
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
			if err := tx.RebuildIndex("items", newDecl); err != nil {
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
	tx, _ := db.Begin(ctx, true)
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
	tx, _ := db.Begin(ctx, true)
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
	tx, _ := db.Begin(ctx, true)
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
			if err := tx.DropIndex("items", "by_color"); err != nil {
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
	tx, _ := db.Begin(ctx, true)
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
	if err := tx.RebuildIndex("items", newDecl); err != nil {
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
	tx, _ := db.Begin(ctx, true)
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
	tx, _ := db.Begin(ctx, true)
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
	if err := tx.DropIndex("members", "by_color"); err != nil {
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
	tx, _ := db.Begin(ctx, true)
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
	tx, _ := db.Begin(ctx, true)
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
