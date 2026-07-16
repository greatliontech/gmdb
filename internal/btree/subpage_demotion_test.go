package btree

// Demotion + per-key bulk-free tests.
//
// Demotion: DemoteNestedTreeIfFits inspects a nested-tree root and,
// when the root is a single leaf whose values fit as a subpage,
// returns (subpageBytes, true, nil) and FreePage's the leaf.
//
// Per-key bulk-free: covered by FreeSubtree directly — extending
// FreeSubtree to recurse into nested-tree cells closes the
// bulk-retire inheritance gap. The "per-key" semantics live at the SetKeyspace
// surface (caller looks up the cell, calls FreeSubtree(NestedRoot),
// then removes the cell from the parent leaf). At this layer the
// test pins (a) the extended count semantic and (b) every nested
// page is retired.

import (
	"bytes"
	"sort"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// --- DemoteNestedTreeIfFits ---

// promoteThenDemote helper: starts from a small subpage, promotes to
// a single-leaf nested tree (via PromoteSubpageToNestedTree),
// deletes one value to trigger the demotion check.
func buildNestedTreeFromSubpage(t *testing.T, pw PageWriter, cfg page.Config, values [][]byte, newValue []byte) uint64 {
	t.Helper()
	subpage, err := page.EncodeSubpage(values, 0)
	if err != nil {
		t.Fatalf("EncodeSubpage: %v", err)
	}
	root, _, err := PromoteSubpageToNestedTree(pw, cfg, subpage, 0, newValue)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	return root
}

func TestDemoteNestedTreeSingleLeafFits(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 128)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()

	// Build a small nested tree by promoting a 5-value subpage with
	// one new value → 6 values in a single leaf. Then call demote
	// and verify it succeeds (6 small values pack as a subpage).
	values := [][]byte{
		[]byte("alpha"), []byte("beta"), []byte("delta"),
		[]byte("echo"), []byte("foxtrot"),
	}
	newValue := []byte("charlie")
	root := buildNestedTreeFromSubpage(t, pw, cfg, values, newValue)

	subpage, demoted, err := DemoteNestedTreeIfFits(pw, cfg, 0, root, page.SubpagePromotionThreshold(cfg))
	if err != nil {
		t.Fatalf("Demote: %v", err)
	}
	if !demoted {
		t.Fatalf("demoted=false, want true (6 small values must fit as subpage)")
	}
	// Verify the returned subpage contains all 6 values in sorted order.
	r := page.NewSubpageReader(subpage, 0)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate returned subpage: %v", err)
	}
	if r.Count() != 6 {
		t.Errorf("subpage Count=%d, want 6", r.Count())
	}
	want := append([][]byte{}, values...)
	want = append(want, newValue)
	sort.Slice(want, func(i, j int) bool { return bytes.Compare(want[i], want[j]) < 0 })
	for i := range want {
		got := r.ValueAt(i)
		if !bytes.Equal(got, want[i]) {
			t.Errorf("subpage[%d]=%q, want %q", i, got, want[i])
		}
	}
}

func TestDemoteNestedTreeFixedSize(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 128)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()

	const fvs uint16 = 4
	values := [][]byte{
		{0, 0, 0, 1}, {0, 0, 0, 2}, {0, 0, 0, 3},
	}
	subpage, _ := page.EncodeSubpage(values, fvs)
	root, _, err := PromoteSubpageToNestedTree(pw, cfg, subpage, fvs, []byte{0, 0, 0, 4})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	sp, demoted, err := DemoteNestedTreeIfFits(pw, cfg, fvs, root, page.SubpagePromotionThreshold(cfg))
	if err != nil || !demoted {
		t.Fatalf("Demote: demoted=%v err=%v", demoted, err)
	}
	r := page.NewSubpageReader(sp, fvs)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate fixed-size demoted subpage: %v", err)
	}
	if r.Count() != 4 {
		t.Errorf("Count=%d, want 4", r.Count())
	}
	if r.FixedValueSize() != fvs {
		t.Errorf("FixedValueSize=%d, want %d", r.FixedValueSize(), fvs)
	}
}

func TestDemoteNestedTreeRootFreed(t *testing.T) {
	// Pin: on demoted=true, the nested-root leaf page is FreePage'd
	// so the caller's bookkeeping correctly retires it.
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)

	values := [][]byte{[]byte("a"), []byte("b"), []byte("c")}
	subpage, _ := page.EncodeSubpage(values, 0)
	root, _, err := PromoteSubpageToNestedTree(fake, cfg, subpage, 0, []byte("d"))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	freedBefore := len(fake.freed)

	_, demoted, err := DemoteNestedTreeIfFits(fake, cfg, 0, root, page.SubpagePromotionThreshold(cfg))
	if err != nil || !demoted {
		t.Fatalf("Demote: demoted=%v err=%v", demoted, err)
	}
	if _, freed := fake.freed[root]; !freed {
		t.Errorf("root=%d not in freed set post-demote; freed=%v", root, fake.freed)
	}
	if len(fake.freed) != freedBefore+1 {
		t.Errorf("freed set grew by %d, want 1 (only the nested-root leaf)", len(fake.freed)-freedBefore)
	}
}

func TestDemoteNestedTreeMultiLeafReturnsFalse(t *testing.T) {
	// A multi-leaf nested tree (root is a branch) is not a demotion
	// candidate. Returns (nil, false, nil) without freeing anything.
	pw, _, f := setupPagerWriter(t, 256)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()

	// Build a nested tree large enough that several splits occur,
	// producing a branch root. ~300 entries × ~100-byte keys each
	// fills several leaves and forces a branch. Build by inserting
	// all values via Put on root=0 (avoids the subpage→promotion
	// path which only produces single-leaf trees in practice).
	var root uint64 = 0
	for i := range 300 {
		v := make([]byte, 100)
		v[0] = byte(i / 256)
		v[1] = byte(i % 256)
		// Fill remaining bytes with index-derived pattern so each
		// key is unique and prefix-compression is minimal.
		for j := 2; j < len(v); j++ {
			v[j] = byte(j ^ i)
		}
		newRoot, err := Put(pw, cfg, root, v, nil)
		if err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		root = newRoot
	}
	// Verify the tree has a branch root.
	buf, _ := pw.Page(root)
	typ, _, _, _ := page.ReadHeader(buf)
	if !page.IsBranchType(typ) {
		t.Fatalf("root type=%d, want branch (test premise: multi-leaf tree)", typ)
	}

	sp, demoted, err := DemoteNestedTreeIfFits(pw, cfg, 0, root, page.SubpagePromotionThreshold(cfg))
	if err != nil {
		t.Fatalf("Demote: %v", err)
	}
	if demoted {
		t.Errorf("demoted=true on multi-leaf tree; want false")
	}
	if sp != nil {
		t.Errorf("subpage bytes returned on no-demote; want nil")
	}
}

func TestDemoteNestedTreeSingleLeafTooLargeReturnsFalse(t *testing.T) {
	// A single-leaf nested tree whose content exceeds the subpage
	// threshold is not demoted. Returns (nil, false, nil); the
	// nested-root leaf is NOT freed.
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)

	// Build a single leaf with values that fit in one leaf but,
	// when encoded as a subpage, exceed the threshold. The leaf has
	// ~4 KB usable space; subpage threshold is ~2 KB. So we want
	// ~3 KB of subpage content: ~150 entries × 20 bytes (2-byte
	// ValueLen + 18 body) = 3000 bytes.
	threshold := page.SubpagePromotionThreshold(cfg)
	target := threshold + 200 // overshoot the threshold

	values := make([][]byte, 0, 200)
	totalEncoded := 0
	for i := 0; totalEncoded < target; i++ {
		v := make([]byte, 18)
		v[0] = byte(i / 256)
		v[1] = byte(i % 256)
		values = append(values, v)
		totalEncoded += 2 + len(v)
	}

	// Build the nested tree by Putting each value as a leaf entry.
	// To avoid triggering a leaf split, set RestartGroupTarget=16
	// and use values small enough that 150+ entries fit in 4 KB
	// (each leaf cell = 7 + key = 25 bytes; 150 × 25 = 3750 bytes;
	// fits below 4080-byte usable space).
	var root uint64 = 0
	for _, v := range values {
		nr, err := Put(fake, cfg, root, v, nil)
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		root = nr
	}
	// Confirm the tree is a single leaf (test premise).
	buf, _ := fake.Page(root)
	typ, _, _, _ := page.ReadHeader(buf)
	if !page.IsLeafType(typ) {
		t.Fatalf("tree is not single-leaf (type=%d); test fixture invalid — try fewer entries", typ)
	}

	freedBefore := len(fake.freed)
	sp, demoted, err := DemoteNestedTreeIfFits(fake, cfg, 0, root, page.SubpagePromotionThreshold(cfg))
	if err != nil {
		t.Fatalf("Demote: %v", err)
	}
	if demoted {
		t.Errorf("demoted=true for single-leaf-too-large; want false")
	}
	if sp != nil {
		t.Errorf("subpage bytes returned on no-demote; want nil")
	}
	if len(fake.freed) != freedBefore {
		t.Errorf("freed set grew by %d on no-demote; want 0", len(fake.freed)-freedBefore)
	}
}

func TestDemoteNestedTreeRejectsZeroRoot(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)
	_, _, err := DemoteNestedTreeIfFits(fake, cfg, 0, 0, page.SubpagePromotionThreshold(cfg))
	if err == nil {
		t.Fatalf("Demote with rootID=0 did not error")
	}
}

// --- FreeSubtree extension: SetKeyspace cells ---

func TestFreeSubtreeSubpageCellCount(t *testing.T) {
	// A Kind=1 data leaf with subpage cells: FreeSubtree returns
	// count = sum of subpage.Count (per-key values), NOT the count
	// of top-level leaf entries (which would be Kind=0 semantic).
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)

	// Build a Kind=1 leaf with 3 subpage cells: 2 + 3 + 4 = 9 values.
	leafID, err := fake.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	leafBuf, _ := fake.ZeroPage(leafID)
	b := page.NewLeafBuilder(leafBuf, cfg)
	sp2, _ := page.EncodeSubpage([][]byte{[]byte("a"), []byte("b")}, 0)
	sp3, _ := page.EncodeSubpage([][]byte{[]byte("a"), []byte("b"), []byte("c")}, 0)
	sp4, _ := page.EncodeSubpage([][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d")}, 0)
	b.AddSubpage([]byte("k1"), sp2)
	b.AddSubpage([]byte("k2"), sp3)
	b.AddSubpage([]byte("k3"), sp4)
	b.Finish()

	count, err := FreeSubtree(fake, cfg, leafID)
	if err != nil {
		t.Fatalf("FreeSubtree: %v", err)
	}
	if count != 9 {
		t.Errorf("count=%d, want 9 (sum of subpage value counts: 2+3+4)", count)
	}
	if _, freed := fake.freed[leafID]; !freed {
		t.Errorf("leafID=%d not freed", leafID)
	}
}

func TestFreeSubtreeNestedTreeCellRecursesAndCounts(t *testing.T) {
	// A Kind=1 data leaf with a nested-tree cell: FreeSubtree
	// recursively retires the nested tree and adds the nested
	// leaf's count to the parent count. Pins the closure of the
	// bulk-retire inheritance gap (SetKeyspace nested-tree pages
	// reachable from a Kind=1 leaf must be freed when the parent
	// keyspace is bulk-freed).
	pw, _, f := setupPagerWriter(t, 128)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()

	// Build a nested tree (single leaf) via promotion of a 4-value
	// subpage + new value = 5 values in the nested tree.
	values := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d")}
	subpage, _ := page.EncodeSubpage(values, 0)
	nestedRoot, nestedCount, err := PromoteSubpageToNestedTree(pw, cfg, subpage, 0, []byte("e"))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if nestedCount != 5 {
		t.Fatalf("nestedCount=%d, want 5", nestedCount)
	}

	// Build a parent Kind=1 leaf containing the nested-tree cell.
	parentID, err := pw.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage parent: %v", err)
	}
	parentBuf, _ := pw.ZeroPage(parentID)
	b := page.NewLeafBuilder(parentBuf, cfg)
	if !b.AddNestedTreeRef([]byte("topic"), nestedRoot, nestedCount) {
		t.Fatalf("AddNestedTreeRef: false")
	}
	b.Finish()

	// Free the parent subtree.
	count, err := FreeSubtree(pw, cfg, parentID)
	if err != nil {
		t.Fatalf("FreeSubtree parent: %v", err)
	}
	// Expected count: 5 values from the nested tree (no plain
	// entries; the only cell is a nested-tree ref).
	if count != 5 {
		t.Errorf("count=%d, want 5 (nested tree value count)", count)
	}
}

func TestFreeSubtreeMixedKind1Cells(t *testing.T) {
	// A Kind=1 leaf with a mix of subpage + nested-tree cells.
	// FreeSubtree sums their counts correctly.
	pw, _, f := setupPagerWriter(t, 128)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()

	// Build a nested tree (5 values).
	subpage, _ := page.EncodeSubpage([][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d")}, 0)
	nestedRoot, nestedCount, _ := PromoteSubpageToNestedTree(pw, cfg, subpage, 0, []byte("e"))

	// Parent leaf with subpage(2 values) + nested-tree-ref(5 values) + subpage(3 values).
	parentID, _ := pw.AllocPage()
	parentBuf, _ := pw.ZeroPage(parentID)
	b := page.NewLeafBuilder(parentBuf, cfg)
	sp2, _ := page.EncodeSubpage([][]byte{[]byte("x"), []byte("y")}, 0)
	sp3, _ := page.EncodeSubpage([][]byte{[]byte("p"), []byte("q"), []byte("r")}, 0)
	b.AddSubpage([]byte("aaa"), sp2)
	b.AddNestedTreeRef([]byte("mid-topic"), nestedRoot, nestedCount)
	b.AddSubpage([]byte("zzz"), sp3)
	b.Finish()

	count, err := FreeSubtree(pw, cfg, parentID)
	if err != nil {
		t.Fatalf("FreeSubtree: %v", err)
	}
	// 2 + 5 + 3 = 10 values total.
	if count != 10 {
		t.Errorf("count=%d, want 10 (2 + 5 + 3)", count)
	}
}

func TestFreeSubtreeRejectsNestedTreeNullRoot(t *testing.T) {
	// Defensive: a NestedTree cell with NestedRoot=0 is structural
	// corruption (a valid nested tree has at least one leaf, hence
	// non-zero root). FreeSubtree surfaces ErrCorrupted.
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)

	leafID, _ := fake.AllocPage()
	leafBuf, _ := fake.ZeroPage(leafID)
	b := page.NewLeafBuilder(leafBuf, cfg)
	b.AddNestedTreeRef([]byte("k"), 0, 5) // bad: root=0
	b.Finish()

	_, err := FreeSubtree(fake, cfg, leafID)
	if err == nil {
		t.Fatalf("FreeSubtree did not error on NestedRoot=0")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("NestedRoot=0")) {
		t.Errorf("err=%v, want 'NestedRoot=0' substring", err)
	}
}

func TestFreeSubtreeSubpageCellCountFixedSize(t *testing.T) {
	// Regression pin: FreeSubtree's subpage value-count works
	// regardless of fixedValueSize because the count is read from
	// the subpage's 2-byte Count header, which is independent of
	// the variable/fixed mode. Pins this for a fvs=4 keyspace so
	// a future refactor that incorrectly threads fixedValueSize
	// through the FreeSubtree path tripps a test.
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)
	const fvs uint16 = 4

	leafID, _ := fake.AllocPage()
	leafBuf, _ := fake.ZeroPage(leafID)
	b := page.NewLeafBuilder(leafBuf, cfg)
	sp1, _ := page.EncodeSubpage([][]byte{
		{0, 0, 0, 1}, {0, 0, 0, 2},
	}, fvs)
	sp2, _ := page.EncodeSubpage([][]byte{
		{0, 0, 0, 3}, {0, 0, 0, 4}, {0, 0, 0, 5},
	}, fvs)
	b.AddSubpage([]byte("k1"), sp1)
	b.AddSubpage([]byte("k2"), sp2)
	b.Finish()

	count, err := FreeSubtree(fake, cfg, leafID)
	if err != nil {
		t.Fatalf("FreeSubtree: %v", err)
	}
	if count != 5 {
		t.Errorf("count=%d, want 5 (2 + 3 fixed-size subpage values)", count)
	}
}

func TestFreeSubtreeKind0Unchanged(t *testing.T) {
	// Regression: for Kind=0 trees (no MultiValue cells), the
	// extended FreeSubtree returns the same count as the original
	// implementation (= number of leaf entries).
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)

	var root uint64 = 0
	for i := range 30 {
		k := []byte{byte(i)}
		nr, err := Put(fake, cfg, root, k, []byte("v"))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		root = nr
	}
	count, err := FreeSubtree(fake, cfg, root)
	if err != nil {
		t.Fatalf("FreeSubtree: %v", err)
	}
	if count != 30 {
		t.Errorf("Kind=0 count=%d, want 30 (one per leaf entry)", count)
	}
}
