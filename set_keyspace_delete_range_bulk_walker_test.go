package gmdb

import (
	"context"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// Regression tests for the per-key-loop → walker migration of
// SetKeyspace.DeleteRange. The un-indexed path now dispatches to
// btree.DeleteRange (the atomic three-phase walker) with
// setKeyspaceCellFree as the per-cell-free callback; the indexed
// path keeps the per-row dispatch via deleteRangePerKey.
//
// Each test below targets one neuter-able line in
// set_keyspace.go's DeleteRange machinery — the higher-level
// "count is correct" tests (TestSetKeyspaceDeleteRangeMixedCellTypes,
// TestSetKeyspaceDeleteRangeCommitReopen, TestSetKeyspaceIndexed
// DeleteRangeClearsIndexEntries) cover the success-path output;
// these pin the structural correctness of the cell-type-aware
// retire (no bitmap leak) via assertNoBitmapCorruption (the same
// four-code check the DDL atomicity tests use).

// TestSetKeyspaceDeleteRangeUnindexedNoLeakWithNestedTreeAtBoundary
// pins the setKeyspaceCellFree nested-tree branch in set_keyspace.go:
// without FreeSubtree(e.NestedRoot) the walker's boundary-leaf
// cleanup leaks the nested B+tree's pages. The parent tree is a
// single leaf — under any partial-range delete, that one leaf IS
// the boundary leaf — so [k1, k3) drives k1 and k2 through
// deleteRangeFromLeaf's Phase 3 cleanup, which invokes
// setKeyspaceCellFree per entry. k3 stays (out of range), so the
// leaf survives as a partial-rebuild rather than a whole-leaf
// retire — exercising the partial-path branch of
// deleteRangeFromLeaf (the path that pages-CoW the leaf and frees
// the original, not the whole-emptied FreePage path).
func TestSetKeyspaceDeleteRangeUnindexedNoLeakWithNestedTreeAtBoundary(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, _ := db.Begin(ctx)
	sks, err := tx.CreateSetKeyspace("k", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	// k1: 3 values (subpage).
	for _, v := range []string{"a", "b", "c"} {
		if _, err := sks.Put([]byte("k1"), []byte(v)); err != nil {
			t.Fatalf("Put k1/%s: %v", v, err)
		}
	}
	// k2: 200 values (nested tree — at 30-byte values, 200*30 = 6000 B
	// exceeds the 50% promotion threshold for a 4 KB page, so this
	// allocates a nested B+tree).
	for i := range 200 {
		v := make([]byte, 30)
		v[0] = byte(i / 256)
		v[1] = byte(i % 256)
		if _, err := sks.Put([]byte("k2"), v); err != nil {
			t.Fatalf("Put k2/#%d: %v", i, err)
		}
	}
	// k3: 2 values (subpage) — OUT of range below, keeps the leaf at
	// boundary so k1 + k2 go through setKeyspaceCellFree (the
	// boundary path), not FreeSubtree's interior path.
	for _, v := range []string{"x", "y"} {
		if _, err := sks.Put([]byte("k3"), []byte(v)); err != nil {
			t.Fatalf("Put k3/%s: %v", v, err)
		}
	}
	// Sanity pre-DeleteRange.
	if sks.desc.Count != 205 {
		t.Fatalf("pre-DeleteRange desc.Count=%d, want 205", sks.desc.Count)
	}
	// Delete [k1, k3) — covers k1 (subpage, 3 values) + k2 (nested,
	// 200 values). k3 stays. The parent leaf is the boundary leaf.
	n, err := sks.DeleteRange([]byte("k1"), []byte("k3"))
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if n != 203 {
		t.Errorf("DeleteRange count=%d, want 203 (3 + 200)", n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Atomicity contract: no bitmap leak / reachable-but-free /
	// reachable-in-RPL / free-and-pending — the walker + setKeyspaceCellFree
	// must retire every page from k2's nested tree as well as k1's
	// (subpage cells have no extra pages to retire — inline).
	assertNoBitmapCorruption(t, db, "SetKeyspace.DeleteRange unindexed (nested-tree at boundary)")
}

// TestSetKeyspaceDeleteRangeUnindexedNoLeakInteriorSubtreeRetire
// pins the interior-subtree retire path through FreeSubtree (the
// three-phase walker's Phase 2). The deleted range spans the WHOLE
// parent tree, so the un-indexed walker's Phase 2 retires every
// subtree via FreeSubtree (which itself handles SetKeyspace cell
// types — subpage / nested-tree — via freeSubtreeAt). Pins that
// the un-indexed dispatch correctly routes through btree.DeleteRange
// (a neuter that early-returned (0, nil) without calling
// btree.DeleteRange would skip every retire and leak everything).
func TestSetKeyspaceDeleteRangeUnindexedNoLeakInteriorSubtreeRetire(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, _ := db.Begin(ctx)
	sks, err := tx.CreateSetKeyspace("k", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	// Enough keys at sufficient cell size to force a multi-leaf
	// parent tree (cellCount of the root branch >= 2 so the walker's
	// Phase 2 actually fires for interior children at descent
	// positions strictly between leftIdx and rightIdx). At ~60-byte
	// keys + ~40-byte subpages, each cell is ~110 bytes; ~30 cells
	// fit per 4 KB leaf, so 500 keys span ~17 leaves → branch root
	// with ~16 cells.
	for k := range 500 {
		key := make([]byte, 60)
		key[0], key[1], key[2] = byte('k'), byte(k/256), byte(k%256)
		for v := range 5 {
			val := []byte{byte(v), byte(v), byte(v)}
			if _, err := sks.Put(key, val); err != nil {
				t.Fatalf("Put k=%v v=%v: %v", key, val, err)
			}
		}
	}
	// One key with a nested tree (200 values * 30 bytes); key chosen
	// to sort late ("zz_nested") so it lands in the right-boundary
	// branch path, ensuring FreeSubtree exercises the nested-tree
	// recursive retire at an interior position too.
	for i := range 200 {
		v := make([]byte, 30)
		v[0] = byte(i)
		if _, err := sks.Put([]byte("zz_nested"), v); err != nil {
			t.Fatalf("Put nested #%d: %v", i, err)
		}
	}
	// Probe: confirm root is a branch with cellCount >= 2 so Phase 2
	// FreeSubtree at strictly-interior positions actually fires under
	// the full-range delete below (without this, leftIdx==rightIdx
	// or the single-child case would bypass Phase 2 entirely and the
	// test would pass for the wrong reason).
	if buf, perr := tx.pgr.Page(sks.desc.Root); perr == nil {
		typ, _, cellCount, _ := page.ReadHeader(buf)
		if typ != page.TypeBranch {
			t.Fatalf("workload too small: root is type=%d (want TypeBranch=%d) — "+
				"Phase 2 FreeSubtree will not fire", typ, page.TypeBranch)
		}
		if cellCount < 2 {
			t.Fatalf("workload too small: root branch cellCount=%d (want >=2) — "+
				"no interior children between leftIdx+1 and rightIdx", cellCount)
		}
	} else {
		t.Fatalf("probe Page(root=%d): %v", sks.desc.Root, perr)
	}
	preCount := sks.desc.Count
	// Full-range delete: every key in the SetKeyspace. The walker
	// recurses through the parent branch + FreeSubtrees every
	// interior child + invokes setKeyspaceCellFree on boundary
	// leaves' cells.
	n, err := sks.DeleteRange(nil, nil)
	if err != nil {
		t.Fatalf("DeleteRange(nil, nil): %v", err)
	}
	if n != preCount {
		t.Errorf("DeleteRange count=%d, want %d (full keyspace)", n, preCount)
	}
	if sks.desc.Count != 0 {
		t.Errorf("post-DeleteRange desc.Count=%d, want 0", sks.desc.Count)
	}
	if sks.desc.Root != 0 {
		t.Errorf("post-DeleteRange desc.Root=%d, want 0 (root collapse)", sks.desc.Root)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	assertNoBitmapCorruption(t, db, "SetKeyspace.DeleteRange unindexed (interior subtree retire)")
}

// TestSetKeyspaceDeleteRangeIndexedDispatchPreservesPerRowMaintenance
// pins the indexed-keyspace fallback dispatch in
// SetKeyspace.DeleteRange: when len(ks.indexes) > 0, deleteRangePerKey
// runs (per-row Delete + applyIndexMaintenanceOnBulkKeyDelete),
// not the walker (which would bypass index maintenance and leave
// stale index entries pointing at retired rows). The check is
// the index count post-DeleteRange — TestSetKeyspaceIndexed
// DeleteRangeClearsIndexEntries (index_setkeyspace_lifecycle_test.go) covers
// the index-count side; this test pins the no-leak side under the
// same dispatch.
func TestSetKeyspaceDeleteRangeIndexedDispatchPreservesPerRowMaintenance(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, _ := db.Begin(ctx)
	decl := testDecl("by_topic", "topic")
	decl.Extract = setKeyspaceFirstByteExtract
	sks, err := tx.CreateSetKeyspace("subs", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	pairs := []struct{ k, v string }{
		{"u1", "alpha"}, {"u1", "apple"}, // u1: 2 values
		{"u2", "bee"},    // u2: 1 value
		{"u3", "carrot"}, // u3: 1 value (kept)
	}
	for _, p := range pairs {
		if _, err := sks.Put([]byte(p.k), []byte(p.v)); err != nil {
			t.Fatalf("Put %s/%s: %v", p.k, p.v, err)
		}
	}
	// Pre-DeleteRange: index has 4 entries (one per Put).
	if sks.indexes["by_topic"].count != 4 {
		t.Fatalf("pre-DeleteRange index count=%d, want 4", sks.indexes["by_topic"].count)
	}
	// Delete [u1, u3): u1 + u2. u3 stays.
	deleted, err := sks.DeleteRange([]byte("u1"), []byte("u3"))
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if deleted != 3 {
		t.Errorf("DeleteRange count=%d, want 3 (2+1 values)", deleted)
	}
	// Pin the indexed-dispatch invariant: index entry count must drop
	// to 1 (only u3/carrot remains). A neuter routing the indexed
	// call through the walker bypasses applyIndexMaintenanceOnBulkKey
	// Delete; index entries for u1/u2 stay reachable → count stays 4.
	if sks.indexes["by_topic"].count != 1 {
		t.Errorf("post-DeleteRange index count=%d, want 1 (u3 only) — "+
			"likely the indexed dispatch line was bypassed and the "+
			"walker ran in its place, leaving u1/u2 index entries stale",
			sks.indexes["by_topic"].count)
	}
	// Verify index is per-row maintained: only u3/carrot survives.
	idx, _ := sks.Index("by_topic")
	var found []string
	for sk, sv := range idx.Lookup([][]byte{[]byte{'c'}}) {
		found = append(found, string(sk)+"/"+string(sv))
	}
	sort.Strings(found)
	if len(found) != 1 || found[0] != "u3/carrot" {
		t.Errorf("Lookup post-DeleteRange got %v want [u3/carrot]", found)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	assertNoBitmapCorruption(t, db, "SetKeyspace.DeleteRange indexed (per-row maintenance)")
}

// TestSetKeyspaceDeleteRangeUnindexedDispatchesToWalker pins the
// set-keyspace.md §Invariants entailed dispatch-direction clause:
// un-indexed SetKeyspace.DeleteRange MUST route through
// btree.DeleteRange (the atomic three-phase walker), not through the
// per-row deleteRangePerKey path. Verified via the btree-level
// SetDeleteRangeCalledHookForTest instrumentation: the hook fires
// at btree.DeleteRange entry. If a future refactor silently routes
// un-indexed traffic to deleteRangePerKey, the hook never fires
// and this test fails — preventing the atomicity contract from
// silently weakening from atomic to per-row (the failure mode
// named by the spec §Invariants violation= clause).
func TestSetKeyspaceDeleteRangeUnindexedDispatchesToWalker(t *testing.T) {
	var called atomic.Bool
	hook := func() { called.Store(true) }
	prev := btree.SetDeleteRangeCalledHookForTest(&hook)
	defer btree.SetDeleteRangeCalledHookForTest(prev)

	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	sks, err := tx.CreateSetKeyspace("unindexed", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	for _, k := range []string{"a", "b", "c", "d"} {
		if _, err := sks.Put([]byte(k), []byte("v")); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	n, err := sks.DeleteRange([]byte("b"), []byte("d"))
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	// Pin count too — the hook fires before rootID/empty-range
	// short-circuit, so asserting only !called.Load() would pass
	// for the wrong reason on a future refactor that calls
	// btree.DeleteRange with a no-op range.
	if n != 2 {
		t.Errorf("DeleteRange count=%d, want 2 (b + c)", n)
	}
	if !called.Load() {
		t.Fatalf("un-indexed SetKeyspace.DeleteRange did not invoke btree.DeleteRange — " +
			"likely the dispatch was silently routed through deleteRangePerKey, " +
			"weakening the atomicity contract from atomic to per-row")
	}
}

// TestSetKeyspaceDeleteRangeIndexedDoesNotDispatchToWalker is the
// negative counterpart of the test above: indexed
// SetKeyspace.DeleteRange MUST NOT route through btree.DeleteRange
// (the walker bypasses the per-(setKey, setValue) index
// maintenance and leaves stale index entries). The hook MUST NOT
// fire under the indexed dispatch.
func TestSetKeyspaceDeleteRangeIndexedDoesNotDispatchToWalker(t *testing.T) {
	var called atomic.Bool
	hook := func() { called.Store(true) }
	prev := btree.SetDeleteRangeCalledHookForTest(&hook)
	defer btree.SetDeleteRangeCalledHookForTest(prev)

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
	sks, err := tx.CreateSetKeyspace("indexed", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	for _, p := range []struct{ k, v string }{
		{"a", "alpha"}, {"b", "bravo"}, {"c", "charlie"}, {"d", "delta"},
	} {
		if _, err := sks.Put([]byte(p.k), []byte(p.v)); err != nil {
			t.Fatalf("Put %s/%s: %v", p.k, p.v, err)
		}
	}
	n, err := sks.DeleteRange([]byte("b"), []byte("d"))
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if n != 2 {
		t.Errorf("DeleteRange count=%d, want 2 (b + c)", n)
	}
	if called.Load() {
		t.Fatalf("indexed SetKeyspace.DeleteRange invoked btree.DeleteRange — " +
			"the walker bypasses per-(setKey,setValue) index maintenance, " +
			"which would leave stale index entries pointing at retired rows")
	}
}
