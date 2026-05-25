package btree

import (
	"bytes"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// --- GetEntry ---

func TestGetEntryEmptyTree(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)
	e, found, err := GetEntry(fake, cfg, 0, []byte("k"))
	if err != nil || found {
		t.Errorf("GetEntry(empty)=(_, %v, %v); want (_, false, nil)", found, err)
	}
	_ = e
}

func TestGetEntryPlainHit(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)
	root, err := Put(fake, cfg, 0, []byte("apple"), []byte("A"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	e, found, err := GetEntry(fake, cfg, root, []byte("apple"))
	if err != nil || !found {
		t.Fatalf("GetEntry: found=%v err=%v", found, err)
	}
	if e.Flags != 0 {
		t.Errorf("Flags=0x%x, want 0", e.Flags)
	}
	if !bytes.Equal(e.Value, []byte("A")) {
		t.Errorf("Value=%q, want A", e.Value)
	}
}

func TestGetEntrySubpageHit(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)

	// Build a tree with a subpage cell via PutEntry.
	sp, _ := page.EncodeSubpage([][]byte{[]byte("v1"), []byte("v2")}, 0)
	root, _, err := PutEntry(fake, cfg, 0, page.LeafEntry{
		Flags: page.CellFlagMultiValue,
		Key:   []byte("topic"),
		Value: sp,
	})
	if err != nil {
		t.Fatalf("PutEntry subpage: %v", err)
	}

	e, found, err := GetEntry(fake, cfg, root, []byte("topic"))
	if err != nil || !found {
		t.Fatalf("GetEntry: found=%v err=%v", found, err)
	}
	if !e.IsSubpage() {
		t.Errorf("IsSubpage=false; Flags=0x%x", e.Flags)
	}
	if !bytes.Equal(e.Value, sp) {
		t.Errorf("subpage bytes round-trip mismatch")
	}
}

func TestGetEntryNestedTreeHit(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)

	root, _, err := PutEntry(fake, cfg, 0, page.LeafEntry{
		Flags:       page.CellFlagMultiValue | page.CellFlagNestedTree,
		Key:         []byte("topic"),
		NestedRoot:  42,
		NestedCount: 1000,
	})
	if err != nil {
		t.Fatalf("PutEntry nested-tree: %v", err)
	}

	e, found, err := GetEntry(fake, cfg, root, []byte("topic"))
	if err != nil || !found {
		t.Fatalf("GetEntry: found=%v err=%v", found, err)
	}
	if !e.IsNestedTree() {
		t.Errorf("IsNestedTree=false; Flags=0x%x", e.Flags)
	}
	if e.NestedRoot != 42 || e.NestedCount != 1000 {
		t.Errorf("(NestedRoot, NestedCount)=(%d, %d), want (42, 1000)", e.NestedRoot, e.NestedCount)
	}
}

func TestGetEntryMiss(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)
	root, _ := Put(fake, cfg, 0, []byte("apple"), []byte("A"))
	_, found, err := GetEntry(fake, cfg, root, []byte("banana"))
	if err != nil || found {
		t.Errorf("GetEntry(miss): found=%v err=%v; want (false, nil)", found, err)
	}
}

// --- PutEntry ---

func TestPutEntryInsertSubpage(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)

	sp, _ := page.EncodeSubpage([][]byte{[]byte("x")}, 0)
	root, displaced, err := PutEntry(fake, cfg, 0, page.LeafEntry{
		Flags: page.CellFlagMultiValue,
		Key:   []byte("k"),
		Value: sp,
	})
	if err != nil {
		t.Fatalf("PutEntry: %v", err)
	}
	if displaced.Flags != 0 || displaced.Key != nil {
		t.Errorf("displaced should be zero on insert; got %+v", displaced)
	}
	// Confirm via GetEntry.
	e, found, _ := GetEntry(fake, cfg, root, []byte("k"))
	if !found || !e.IsSubpage() {
		t.Errorf("post-insert GetEntry: found=%v IsSubpage=%v", found, e.IsSubpage())
	}
}

func TestPutEntryReplaceSubpageToNestedTree(t *testing.T) {
	// Simulates the chunk-6.6 SetKeyspace.Put promotion path:
	// the cell was a subpage; after promotion the cell becomes a
	// nested-tree-ref. PutEntry replaces the cell and returns
	// the displaced subpage entry to the caller.
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)

	sp, _ := page.EncodeSubpage([][]byte{[]byte("a"), []byte("b")}, 0)
	root, _, err := PutEntry(fake, cfg, 0, page.LeafEntry{
		Flags: page.CellFlagMultiValue,
		Key:   []byte("k"),
		Value: sp,
	})
	if err != nil {
		t.Fatalf("PutEntry initial: %v", err)
	}

	// Replace with nested-tree-ref.
	root2, displaced, err := PutEntry(fake, cfg, root, page.LeafEntry{
		Flags:       page.CellFlagMultiValue | page.CellFlagNestedTree,
		Key:         []byte("k"),
		NestedRoot:  77,
		NestedCount: 5,
	})
	if err != nil {
		t.Fatalf("PutEntry replace: %v", err)
	}
	// Verify displaced is the subpage entry.
	if !displaced.IsSubpage() {
		t.Errorf("displaced should be subpage; Flags=0x%x", displaced.Flags)
	}
	if !bytes.Equal(displaced.Value, sp) {
		t.Errorf("displaced subpage bytes mismatch")
	}
	// Verify the new cell is the nested-tree-ref.
	e, found, _ := GetEntry(fake, cfg, root2, []byte("k"))
	if !found || !e.IsNestedTree() {
		t.Errorf("post-replace GetEntry: found=%v IsNestedTree=%v", found, e.IsNestedTree())
	}
	if e.NestedRoot != 77 || e.NestedCount != 5 {
		t.Errorf("nested-ref (root,count)=(%d,%d), want (77,5)", e.NestedRoot, e.NestedCount)
	}
}

func TestPutEntryEmptyTreeNestedTreeRef(t *testing.T) {
	// PutEntry on rootID=0 with a nested-tree-ref allocates a
	// single-leaf root containing just that cell.
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)

	root, displaced, err := PutEntry(fake, cfg, 0, page.LeafEntry{
		Flags:       page.CellFlagMultiValue | page.CellFlagNestedTree,
		Key:         []byte("k"),
		NestedRoot:  100,
		NestedCount: 50,
	})
	if err != nil {
		t.Fatalf("PutEntry on empty tree: %v", err)
	}
	if displaced.Key != nil {
		t.Errorf("displaced should be zero on empty-tree insert")
	}
	e, found, _ := GetEntry(fake, cfg, root, []byte("k"))
	if !found || !e.IsNestedTree() {
		t.Errorf("post-genesis GetEntry: found=%v IsNestedTree=%v", found, e.IsNestedTree())
	}
}

func TestPutEntryDoesNotFreeDisplacedTrailerPages(t *testing.T) {
	// Pin: PutEntry returns the displaced entry but does NOT free
	// any resources owned by the displaced cell (overflow chain,
	// nested-tree pages). The caller's responsibility — different
	// cell types have different cleanup paths.
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)

	// Insert a nested-tree-ref pointing at a fake NestedRoot=42.
	// (We don't actually allocate page 42 — PutEntry doesn't read
	// the displaced NestedRoot, so this is fine for the test.)
	root, _, err := PutEntry(fake, cfg, 0, page.LeafEntry{
		Flags:       page.CellFlagMultiValue | page.CellFlagNestedTree,
		Key:         []byte("k"),
		NestedRoot:  42,
		NestedCount: 99,
	})
	if err != nil {
		t.Fatalf("PutEntry: %v", err)
	}
	freedCountBefore := len(fake.freed)

	// Replace with a different nested-tree-ref. Displaced cell
	// reports NestedRoot=42 + NestedCount=99 — but page 42 must
	// NOT appear in freed (PutEntry does not free trailer pages).
	_, displaced, err := PutEntry(fake, cfg, root, page.LeafEntry{
		Flags:       page.CellFlagMultiValue | page.CellFlagNestedTree,
		Key:         []byte("k"),
		NestedRoot:  43,
		NestedCount: 100,
	})
	if err != nil {
		t.Fatalf("PutEntry replace: %v", err)
	}
	if displaced.NestedRoot != 42 || displaced.NestedCount != 99 {
		t.Errorf("displaced (root,count)=(%d,%d), want (42,99)", displaced.NestedRoot, displaced.NestedCount)
	}
	if _, leaked := fake.freed[42]; leaked {
		t.Errorf("page 42 was FreePage'd by PutEntry; caller-owned (NestedRoot trailer should not be auto-freed)")
	}
	// Sanity: PutEntry did free its own CoW'd leaf, so the freed
	// set MUST have grown (by exactly 1 — the prior root).
	if len(fake.freed) <= freedCountBefore {
		t.Errorf("freed did not grow; PutEntry should have FreePage'd the prior root")
	}
}
