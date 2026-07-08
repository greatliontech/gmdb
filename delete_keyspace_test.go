package gmdb

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// These tests promote four invariants over DeleteKeyspace:
//
//   Inv-A (clause-explicit, api-surface.md §Keyspace API
//          DeleteKeyspace): post-Delete OpenKeyspace returns
//          ErrNotFound (same tx and post-commit re-Open).
//   Inv-B (entailed, "bulk subtree retirement"): every page reachable
//          from desc.Root pre-Delete enters loosePages or
//          retiredPages.
//   Inv-C (entailed, file-layout.md §Meta Page): tx.numKeyspaces
//          decrements; meta.NumKeyspaces / meta.KeyspaceRoot reflect
//          the post-Delete state across commit + re-Open.
//   Inv-D (clause-explicit, api-surface.md §Keyspace API
//          DeleteKeyspace): every *Keyspace / *Cursor previously
//          opened on name returns ErrKeyspaceClosed; CreateKeyspace
//          re-creating the same name does NOT reactivate the old
//          handle.
//
// Plus the partial-mutation drift class:
// the deferred-flush design removes the per-op
// storeDescriptor window, so a tx that Put-then-Rolls-back leaves no
// orphan data pages on disk. See Tx.Commit godoc for the contract;
// TestDeferredFlushClosesDescriptorDrift below pins the symptom-class.

func TestDeleteKeyspaceEmptyNameReturnsErrKeyEmpty(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	if err := tx.DeleteKeyspace(""); !errors.Is(err, ErrKeyEmpty) {
		t.Errorf("DeleteKeyspace(\"\"): got %v, want ErrKeyEmpty", err)
	}
}

func TestDeleteKeyspaceMissingReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	if err := tx.DeleteKeyspace("absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteKeyspace(absent): got %v, want ErrNotFound", err)
	}
}

// TestDeleteKeyspaceRejectsKindIndexInternal forges a Kind=2 descriptor
// into the keyspace B+tree (the user API cannot produce Kind=2) and
// asserts DeleteKeyspace refuses it with ErrKeyspaceReserved per the
// keyspaces.md invariant #4 application to DeleteKeyspace.
func TestDeleteKeyspaceRejectsKindIndexInternal(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	forged := keyspaceDescriptor{Kind: keyspaceKindIndexInternal}
	if err := tx.storeDescriptor("__idx", forged); err != nil {
		t.Fatalf("storeDescriptor Kind=2: %v", err)
	}
	tx.numKeyspaces++
	if err := tx.DeleteKeyspace("__idx"); !errors.Is(err, ErrKeyspaceReserved) {
		t.Errorf("DeleteKeyspace(Kind=2): got %v, want ErrKeyspaceReserved", err)
	}
}

// TestDeleteKeyspaceSameTxOpenReturnsNotFound promotes Inv-A (same-tx
// portion): OpenKeyspace after DeleteKeyspace within the same tx
// returns ErrNotFound.
func TestDeleteKeyspaceSameTxOpenReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	if _, err := tx.CreateKeyspace("ks"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.DeleteKeyspace("ks"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	if _, err := tx.OpenKeyspace("ks"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenKeyspace post-Delete: got %v, want ErrNotFound", err)
	}
}

// TestDeleteKeyspaceAcrossCommitReturnsNotFound promotes Inv-A
// (post-commit portion) and Inv-C: a fresh Open + Begin observes
// numKeyspaces decremented and the name absent.
func TestDeleteKeyspaceAcrossCommitReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)

	db, _ := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	tx, _ := db.Begin(ctx)
	if _, err := tx.CreateKeyspace("a"); err != nil {
		t.Fatalf("CreateKeyspace a: %v", err)
	}
	if _, err := tx.CreateKeyspace("b"); err != nil {
		t.Fatalf("CreateKeyspace b: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit #1: %v", err)
	}

	tx2, _ := db.Begin(ctx)
	if err := tx2.DeleteKeyspace("a"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	if tx2.numKeyspaces != 1 {
		t.Errorf("tx2.numKeyspaces = %d, want 1", tx2.numKeyspaces)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit #2: %v", err)
	}

	meta := db.Meta()
	if meta.NumKeyspaces != 1 {
		t.Errorf("post-commit NumKeyspaces = %d, want 1", meta.NumKeyspaces)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, _ := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db2.Close()
	if got := db2.Meta().NumKeyspaces; got != 1 {
		t.Errorf("re-Open NumKeyspaces = %d, want 1", got)
	}
	tx3, _ := db2.Begin(ctx)
	defer tx3.Rollback()
	if _, err := tx3.OpenKeyspace("a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenKeyspace(a) post-Delete-Commit-Reopen: got %v, want ErrNotFound", err)
	}
	if _, err := tx3.OpenKeyspace("b"); err != nil {
		t.Errorf("OpenKeyspace(b) survived Delete: %v", err)
	}
	names, _ := tx3.ListKeyspaces()
	if len(names) != 1 || names[0] != "b" {
		t.Errorf("ListKeyspaces post-Delete = %v, want [b]", names)
	}
}

// TestDeleteKeyspaceBulkFreesDataSubtree promotes Inv-B: every page
// reachable from desc.Root pre-Delete enters retiredPages (prior-tx
// pages — the Put committed earlier) or loosePages (same-tx pages —
// the keyspace-B+tree CoW path may produce some). Counts:
//
//	N data pages alive pre-Delete = unique page IDs reachable from
//	desc.Root via a recursive walk. Post-Delete: every one of those
//	IDs is in tx.pgr.RetiredPages() ∪ tx.pgr.LoosePages().
func TestDeleteKeyspaceBulkFreesDataSubtree(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)

	db, _ := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()

	// Commit a keyspace with enough data to materialise a multi-page
	// B+tree, so the bulk-free walk has branches AND leaves to
	// retire (not just one root leaf).
	tx, _ := db.Begin(ctx)
	ks, err := tx.CreateKeyspace("ks")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// 1000 keys with 200-byte values forces ≥ 50 leaves at 4 KB
	// page size — guarantees at least one branch level.
	val := bytes.Repeat([]byte{0x42}, 200)
	for i := range 1000 {
		key := []byte{byte(i >> 8), byte(i)}
		if err := ks.Put(key, val); err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Enumerate the persisted data subtree's pages BEFORE Delete.
	tx2, _ := db.Begin(ctx)
	desc, found, err := tx2.loadDescriptor("ks")
	if err != nil || !found {
		t.Fatalf("loadDescriptor: found=%v err=%v", found, err)
	}
	cfg := tx2.pgr.Config()
	reachable := collectReachablePages(t, tx2.pgr, cfg, desc.Root)
	if len(reachable) < 2 {
		t.Fatalf("data subtree too small to test bulk-free; reachable=%d", len(reachable))
	}

	if err := tx2.DeleteKeyspace("ks"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}

	// Every reachable id must now be in retiredPages or loosePages.
	retired := tx2.pgr.RetiredPages()
	loose := tx2.pgr.LoosePages()
	retiredSet := make(map[uint64]struct{}, len(retired))
	for _, id := range retired {
		retiredSet[id] = struct{}{}
	}
	for id := range reachable {
		_, inRetired := retiredSet[id]
		_, inLoose := loose[id]
		if !inRetired && !inLoose {
			t.Errorf("page %d reachable pre-Delete but not retired/loose post-Delete", id)
		}
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit DeleteKeyspace: %v", err)
	}
}

// collectReachablePages walks the tree rooted at rootID returning the
// set of every reachable page ID (branches + leaves + overflow chains).
// Mirrors btree.FreeSubtree's traversal so the test's reachability
// definition matches the implementation's retire-set.
func collectReachablePages(t *testing.T, pr btree.PageReader, cfg page.Config, rootID uint64) map[uint64]struct{} {
	t.Helper()
	out := make(map[uint64]struct{})
	if rootID == 0 {
		return out
	}
	var walk func(id uint64, depth int)
	walk = func(id uint64, depth int) {
		if depth > btree.MaxTreeDepth {
			t.Fatalf("collectReachablePages: depth exceeded MaxTreeDepth at %d", id)
		}
		if _, dup := out[id]; dup {
			t.Fatalf("collectReachablePages: page %d visited twice (cycle?)", id)
		}
		out[id] = struct{}{}
		buf, _ := pr.Page(id)
		typ, _, count, _ := page.ReadHeader(buf)
		switch {
		case typ == page.TypeBranch:
			// Descent-index range: BranchChildAt(0) = leftmost,
			// BranchChildAt(i ∈ [1, count]) = cell i-1's child.
			for i := uint16(0); i <= count; i++ {
				walk(page.BranchChildAt(buf, cfg, i), depth+1)
			}
		case page.IsLeafType(typ):
			r := page.NewLeafReader(buf, cfg)
			if err := r.Validate(); err != nil {
				t.Fatalf("collectReachablePages: leaf %d: %v", id, err)
			}
			it := r.IterForReuse(nil, nil, nil)
			for {
				e, ok := it.Next()
				if !ok {
					break
				}
				if e.IsOverflow() {
					runLen := page.OverflowRunLength(cfg, e.TotalLen)
					for k := range runLen {
						pid := e.OverflowPage + uint64(k)
						if _, dup := out[pid]; dup {
							t.Fatalf("collectReachablePages: overflow page %d visited twice", pid)
						}
						out[pid] = struct{}{}
					}
				}
			}
		default:
			t.Fatalf("collectReachablePages: page %d unexpected type %d", id, typ)
		}
	}
	walk(rootID, 0)
	return out
}

// TestDeleteKeyspaceInvalidatesHandle promotes Inv-D (handle-
// invalidation portion): every *Keyspace method on a deleted handle
// returns ErrKeyspaceClosed.
func TestDeleteKeyspaceInvalidatesHandle(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()

	ks, _ := tx.CreateKeyspace("ks")
	if err := ks.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := tx.DeleteKeyspace("ks"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}

	if _, err := ks.Get([]byte("k")); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("Get on dead handle: got %v, want ErrKeyspaceClosed", err)
	}
	if err := ks.Put([]byte("k"), []byte("v")); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("Put on dead handle: got %v, want ErrKeyspaceClosed", err)
	}
	if err := ks.Delete([]byte("k")); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("Delete on dead handle: got %v, want ErrKeyspaceClosed", err)
	}
}

// TestDeleteKeyspaceInvalidatesCursor promotes Inv-D for *Cursor
// handles obtained pre-Delete.
func TestDeleteKeyspaceInvalidatesCursor(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()

	ks, _ := tx.CreateKeyspace("ks")
	_ = ks.Put([]byte("a"), []byte("1"))
	_ = ks.Put([]byte("b"), []byte("2"))
	c := ks.Cursor()
	if k, _ := c.First(); !bytes.Equal(k, []byte("a")) {
		t.Fatalf("First before Delete = %q, want a", k)
	}

	if err := tx.DeleteKeyspace("ks"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	if k, _ := c.Next(); k != nil {
		t.Errorf("Next on dead-keyspace cursor returned (%q, _), want nil", k)
	}
	if err := c.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("Cursor.Err on dead keyspace: got %v, want ErrKeyspaceClosed", err)
	}
	if err := c.Delete(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("Cursor.Delete on dead keyspace: got %v, want ErrKeyspaceClosed", err)
	}
}

// TestKeyspaceCursorOnDeadHandleDoesNotRegister asserts Cursor() on a
// DeleteKeyspace'd handle does NOT register the cursor in openCursors —
// a pathological `for { ks.Cursor() }` after Delete must not grow the
// slice unbounded (mirrors SetKeyspace.Cursor's `if !ks.dead` guard).
func TestKeyspaceCursorOnDeadHandleDoesNotRegister(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()

	ks, _ := tx.CreateKeyspace("ks")
	_ = ks.Put([]byte("a"), []byte("1"))

	// A live handle registers its cursors (the guard must not break the
	// normal path).
	_ = ks.Cursor()
	if got := len(ks.openCursors); got != 1 {
		t.Fatalf("live-handle Cursor() registered count = %d, want 1", got)
	}

	if err := tx.DeleteKeyspace("ks"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}

	// After Delete the handle is dead; repeated Cursor() must not grow
	// openCursors.
	before := len(ks.openCursors)
	for range 100 {
		_ = ks.Cursor()
	}
	if got := len(ks.openCursors); got != before {
		t.Errorf("openCursors grew on dead handle: %d -> %d after 100 Cursor() calls, want stable at %d", before, got, before)
	}

	// A cursor from a dead handle is inert.
	c := ks.Cursor()
	if k, _ := c.First(); k != nil {
		t.Errorf("First on dead-handle cursor = %q, want nil", k)
	}
	if err := c.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("dead-handle cursor Err = %v, want ErrKeyspaceClosed", err)
	}
}

// TestDeleteKeyspaceRecreateLeavesOldHandleDead promotes Inv-D
// (permanent-invalidation portion): a same-tx CreateKeyspace re-
// creating the deleted name returns a fresh *Keyspace; the old
// handle stays dead.
func TestDeleteKeyspaceRecreateLeavesOldHandleDead(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()

	ks1, _ := tx.CreateKeyspace("ks")
	_ = ks1.Put([]byte("k"), []byte("v1"))
	if err := tx.DeleteKeyspace("ks"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	ks2, err := tx.CreateKeyspace("ks")
	if err != nil {
		t.Fatalf("CreateKeyspace re-create: %v", err)
	}
	if ks2 == ks1 {
		t.Errorf("CreateKeyspace re-create returned the same *Keyspace pointer as the dead handle")
	}
	if _, err := ks1.Get([]byte("k")); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("old handle Get after re-create: got %v, want ErrKeyspaceClosed", err)
	}
	// New handle is live.
	if err := ks2.Put([]byte("k"), []byte("v2")); err != nil {
		t.Errorf("new handle Put: %v", err)
	}
	got, err := ks2.Get([]byte("k"))
	if err != nil || !bytes.Equal(got, []byte("v2")) {
		t.Errorf("new handle Get = %q, err=%v, want v2", got, err)
	}
}

// TestDeleteKeyspaceThenCreateSameTxFlushesAsOverwrite verifies the
// "Delete + Create same tx = single btree.Put at flush" optimisation
// in the deferred-flush walk: after Commit, the on-disk descriptor
// is the NEW one (Kind=0, fresh Root), not absent.
func TestDeleteKeyspaceThenCreateSameTxFlushesAsOverwrite(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)

	db, _ := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()

	tx, _ := db.Begin(ctx)
	ks1, _ := tx.CreateKeyspace("ks")
	_ = ks1.Put([]byte("old"), []byte("data"))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit #1: %v", err)
	}

	tx2, _ := db.Begin(ctx)
	if err := tx2.DeleteKeyspace("ks"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	ks2, _ := tx2.CreateKeyspace("ks")
	if err := ks2.Put([]byte("new"), []byte("data")); err != nil {
		t.Fatalf("new Put: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit #2: %v", err)
	}

	tx3, _ := db.Begin(ctx)
	defer tx3.Rollback()
	ks3, err := tx3.OpenKeyspace("ks")
	if err != nil {
		t.Fatalf("OpenKeyspace post-recreate: %v", err)
	}
	if _, err := ks3.Get([]byte("old")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(old) survived Delete: %v", err)
	}
	got, err := ks3.Get([]byte("new"))
	if err != nil || !bytes.Equal(got, []byte("data")) {
		t.Errorf("Get(new) = %q, err=%v, want data", got, err)
	}
}

// TestCreateThenDeleteSameTxLeavesNoTrace promotes the deferred-flush
// short-circuit: a name Created and Deleted in the same tx never
// touches the on-disk keyspace B+tree (no btree.Put + no btree.Delete
// at flush). pendingDeletes stays empty for the name.
func TestCreateThenDeleteSameTxLeavesNoTrace(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)

	db, _ := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()

	tx, _ := db.Begin(ctx)
	rootBefore := tx.keyspaceRoot
	if _, err := tx.CreateKeyspace("ephemeral"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.DeleteKeyspace("ephemeral"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	if _, marked := tx.pendingDeletes["ephemeral"]; marked {
		t.Errorf("pendingDeletes carries Created+Deleted name (no on-disk entry to delete)")
	}
	if tx.numKeyspaces != 0 {
		t.Errorf("numKeyspaces = %d, want 0", tx.numKeyspaces)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := db.Meta().NumKeyspaces; got != 0 {
		t.Errorf("post-commit NumKeyspaces = %d, want 0", got)
	}
	if got := db.Meta().KeyspaceRoot; got != rootBefore {
		t.Errorf("post-commit KeyspaceRoot = %d, want %d (unchanged — no flush ops)", got, rootBefore)
	}
}

// TestDeleteKeyspaceListExcludesDeleted verifies ListKeyspaces in the
// same tx filters out names in pendingDeletes.
func TestDeleteKeyspaceListExcludesDeleted(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)

	db, _ := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()

	tx, _ := db.Begin(ctx)
	for _, n := range []string{"a", "b", "c"} {
		if _, err := tx.CreateKeyspace(n); err != nil {
			t.Fatalf("CreateKeyspace(%s): %v", n, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	tx2, _ := db.Begin(ctx)
	defer tx2.Rollback()
	if err := tx2.DeleteKeyspace("b"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	names, err := tx2.ListKeyspaces()
	if err != nil {
		t.Fatalf("ListKeyspaces: %v", err)
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "c" {
		t.Errorf("ListKeyspaces post-Delete = %v, want [a c]", names)
	}
}

// TestDeleteKeyspaceMissingAfterDeleteReturnsErrNotFound verifies a
// second DeleteKeyspace on the same name in the same tx returns
// ErrNotFound (the first Delete already removed it).
func TestDeleteKeyspaceMissingAfterDeleteReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	if _, err := tx.CreateKeyspace("ks"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.DeleteKeyspace("ks"); err != nil {
		t.Fatalf("DeleteKeyspace #1: %v", err)
	}
	if err := tx.DeleteKeyspace("ks"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteKeyspace #2 (already deleted): got %v, want ErrNotFound", err)
	}
}

// TestDeleteKeyspaceAfterSetKeyspaceConfigClearsDirtyDescriptor
// promotes the Q5 adversarial path: SetKeyspaceConfig on an uncached
// name lands in dirtyDescriptors; a subsequent DeleteKeyspace must
// remove the dirtyDescriptors entry, add the name to pendingDeletes,
// bulk-free the data subtree, and Commit must result in the name
// being absent on disk (no orphan descriptor with the
// SetKeyspaceConfig-changed RestartGroupTarget).
func TestDeleteKeyspaceAfterSetKeyspaceConfigClearsDirtyDescriptor(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)

	db, _ := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()

	// Pre-commit a keyspace so SetKeyspaceConfig has an on-disk
	// descriptor to land against.
	tx, _ := db.Begin(ctx)
	if _, err := tx.CreateKeyspace("ks"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit #1: %v", err)
	}

	tx2, _ := db.Begin(ctx)
	// SetKeyspaceConfig without OpenKeyspace lands in dirtyDescriptors.
	if err := tx2.SetKeyspaceConfig("ks", KeyspaceConfig{RestartGroupTarget: 32}); err != nil {
		t.Fatalf("SetKeyspaceConfig: %v", err)
	}
	if _, ok := tx2.dirtyDescriptors["ks"]; !ok {
		t.Fatalf("expected ks in dirtyDescriptors post-SetKeyspaceConfig")
	}
	if err := tx2.DeleteKeyspace("ks"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	if _, ok := tx2.dirtyDescriptors["ks"]; ok {
		t.Errorf("dirtyDescriptors still has ks post-DeleteKeyspace")
	}
	if _, ok := tx2.pendingDeletes["ks"]; !ok {
		t.Errorf("pendingDeletes missing ks post-DeleteKeyspace (the on-disk descriptor must be removed at flush)")
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit #2: %v", err)
	}

	tx3, _ := db.Begin(ctx)
	defer tx3.Rollback()
	if _, err := tx3.OpenKeyspace("ks"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenKeyspace(ks) post-flush: got %v, want ErrNotFound", err)
	}
}

// TestCursorErrReturnsKeyspaceClosedOnDeadHandle promotes Inv-D for
// the standalone Cursor.Err() path — calling Err() on a dead cursor
// without any intervening nav op must surface ErrKeyspaceClosed
// (api-surface.md §Keyspace API DeleteKeyspace: every method on a
// dead handle reports ErrKeyspaceClosed).
func TestCursorErrReturnsKeyspaceClosedOnDeadHandle(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()

	ks, _ := tx.CreateKeyspace("ks")
	_ = ks.Put([]byte("a"), []byte("1"))
	c := ks.Cursor()
	if k, _ := c.First(); !bytes.Equal(k, []byte("a")) {
		t.Fatalf("First = %q, want a", k)
	}
	if err := c.Err(); err != nil {
		t.Fatalf("Err pre-Delete: %v", err)
	}
	if err := tx.DeleteKeyspace("ks"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	// No intervening nav op — Err() must observe ErrKeyspaceClosed
	// directly from c.ks.dead, not via a latched closeErr.
	if err := c.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("Cursor.Err() on dead handle (no nav op): got %v, want ErrKeyspaceClosed", err)
	}
}

// TestDeferredFlushClosesDescriptorDrift pins the descriptor-drift
// closure: under the old per-op storeDescriptor scheme, a Put that
// mutated the data B+tree before a failing storeDescriptor could
// orphan pages on commit. Under deferred flush, no per-op
// storeDescriptor exists (see Tx.Commit
// godoc for the deferred-flush contract); the test verifies the
// symptom-class is gone by exercising Put-then-Rollback and
// asserting no on-disk drift.
func TestDeferredFlushClosesDescriptorDrift(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)

	db, _ := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()

	tx, _ := db.Begin(ctx)
	if _, err := tx.CreateKeyspace("ks"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit #1: %v", err)
	}
	rootBefore := db.Meta().KeyspaceRoot

	tx2, _ := db.Begin(ctx)
	ks, _ := tx2.OpenKeyspace("ks")
	if err := ks.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// Post-rollback meta.KeyspaceRoot must be unchanged — no on-disk
	// state was written because the deferred-flush walk never ran.
	if got := db.Meta().KeyspaceRoot; got != rootBefore {
		t.Errorf("Rollback drift: KeyspaceRoot = %d, want %d", got, rootBefore)
	}

	tx3, _ := db.Begin(ctx)
	defer tx3.Rollback()
	ks3, err := tx3.OpenKeyspace("ks")
	if err != nil {
		t.Fatalf("OpenKeyspace after rollback: %v", err)
	}
	if _, err := ks3.Get([]byte("k")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get post-rollback: got err=%v, want ErrNotFound (no orphan write)", err)
	}
}

// TestDeleteKeyspaceAtomicOnPartialRetirement pins the DDL atomicity
// contract for the three-subtree retirement (transactions.md §Write-
// helper error contract). A failure mid-walk of retireIndexRegistry —
// after step-1 FreeSubtree(desc.Root) has already returned the
// keyspace's data tree pages to the loose pool, and after one index
// data tree has been freed — leaves the bitmap with the keyspace's
// data tree partially-freed yet the cached *Keyspace's descriptor
// still pointing at desc.Root. Without the savepoint wrap around the
// whole retirement, Tx.Commit (the rest-of-tx-continues path)
// publishes that bitmap, and a future tx that re-allocates the freed
// pages overwrites still-referenced data — corruption, not just a
// leak. Check() must report zero BitmapLeak after Commit AND the
// keyspace must still read correctly (no overwrites since the
// savepoint reverted everything).
func TestDeleteKeyspaceAtomicOnPartialRetirement(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Setup tx: create a keyspace with TWO indexes so the registry
	// walk has multiple iterations; populate it so step-1 and the
	// per-index FreeSubtree have real subtrees to retire.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	idx1 := testDecl("by_color", "color")
	idx1.Extract = firstByteExtract
	idx2 := testDecl("by_size", "size")
	idx2.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", idx1, idx2)
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

	// Failure-injection: fail after the first registry entry's
	// per-index FreeSubtree (i==0) — step 1's data-subtree free has
	// already run, plus one index's data tree, but the other index
	// and the registry-root free have not.
	injected := errors.New("injected retire failure")
	setRetireIndexRegistryFailHookForTest(func(i int) error {
		if i == 0 {
			return injected
		}
		return nil
	})
	t.Cleanup(func() { setRetireIndexRegistryFailHookForTest(nil) })

	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin 2: %v", err)
	}
	if err := tx.DeleteKeyspace("items"); !errors.Is(err, injected) {
		tx.Rollback()
		t.Fatalf("DeleteKeyspace err = %v, want injected failure", err)
	}
	// Rest-of-tx-continues: commit despite the per-op error.
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after injected failure: %v", err)
	}

	assertNoBitmapCorruption(t, db, "DeleteKeyspace")

	// Stronger check: the keyspace must still read correctly after a
	// re-open — the savepoint reverted every FreeSubtree, so the
	// data tree is intact. (Without the wrap, the bitmap shows the
	// pages free; even if Check shows zero leak this tx, a future
	// re-allocation of those pages would corrupt the still-live
	// data tree.)
	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin 3: %v", err)
	}
	defer tx.Rollback()
	ks2, err := tx.OpenKeyspace("items", idx1, idx2)
	if err != nil {
		t.Fatalf("OpenKeyspace after failed delete: %v", err)
	}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		v, err := ks2.Get([]byte(k))
		if err != nil {
			t.Errorf("Get %q after failed delete: %v", k, err)
		}
		if len(v) != 2 || v[0] != 0x42 || v[1] != 'x' {
			t.Errorf("Get %q: got %x, want {0x42, 'x'}", k, v)
		}
	}
}
