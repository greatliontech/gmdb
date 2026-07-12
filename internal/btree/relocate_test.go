package btree

import (
	"bytes"
	"fmt"
	"slices"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// buildTreeForRelocate builds a multi-level B+tree of n keys (100-byte
// values force splits) and returns the root + the expected contents.
func buildTreeForRelocate(t *testing.T, pw *fakeWriter, cfg page.Config, n int) (uint64, map[string]string) {
	t.Helper()
	root := uint64(0)
	want := make(map[string]string, n)
	for i := range n {
		key := fmt.Appendf(nil, "k-%05d", i)
		// 100-byte values stay below the overflow-promotion threshold, so
		// the reachable page set is tree pages only (no overflow chains).
		// TestRelocatePagesFullRoundTrip's moved == len(before) relies on
		// this; overflow survival is covered separately.
		val := bytes.Repeat([]byte{byte('a' + i%26)}, 100)
		nr, err := Put(pw, cfg, root, key, val)
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
		root = nr
		want[string(key)] = string(val)
	}
	return root, want
}

func collectKVForRelocate(t *testing.T, pw PageWriter, cfg page.Config, root uint64) map[string]string {
	t.Helper()
	got := make(map[string]string)
	if err := WalkKV(pw, cfg, root, ^uint64(0), func(k, v []byte) error {
		got[string(k)] = string(v)
		return nil
	}); err != nil {
		t.Fatalf("WalkKV: %v", err)
	}
	return got
}

func assertSameKV(t *testing.T, want, got map[string]string) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("kv count: got %d, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q: got %q, want %q", k, got[k], v)
		}
	}
}

// TestRelocatePagesFullRoundTrip relocates EVERY page (predicate always
// true): the result is a structurally fresh tree with identical contents,
// a new root, every old page retired, and no new page reusing an old id.
func TestRelocatePagesFullRoundTrip(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root, want := buildTreeForRelocate(t, pw, cfg, 200)

	before := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, root, before)
	if len(before) < 3 {
		t.Fatalf("tree too small (%d pages) to exercise multi-level relocation", len(before))
	}

	newRoot, moved, err := RelocatePages(pw, cfg, root, func(uint64) bool { return true }, 1<<30)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	if newRoot == root {
		t.Errorf("root id unchanged (%d) after full relocation", newRoot)
	}
	if moved != len(before) {
		t.Errorf("moved=%d, want %d (every page eligible)", moved, len(before))
	}
	assertSameKV(t, want, collectKVForRelocate(t, pw, cfg, newRoot))

	for id := range before {
		if _, freed := pw.freed[id]; !freed {
			t.Errorf("old page %d not retired after full relocation", id)
		}
	}
	after := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, newRoot, after)
	if len(after) != len(before) {
		t.Errorf("page count changed: %d → %d (structure not preserved)", len(before), len(after))
	}
	for id := range after {
		if _, old := before[id]; old {
			t.Errorf("new tree still references old page id %d", id)
		}
		if _, freed := pw.freed[id]; freed {
			t.Errorf("new tree references a retired page id %d", id)
		}
	}
}

// TestRelocatePagesPreservesOverflowLeaf: relocating a leaf that owns an
// overflow chain leaves the chain — which this primitive deliberately does
// NOT relocate — reachable, and the large value intact. Pins the doc
// claim that a leaf's overflow refs survive a verbatim leaf relocation.
func TestRelocatePagesPreservesOverflowLeaf(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	bigKey := []byte("big")
	bigVal := bytes.Repeat([]byte{0x5A}, 6000) // > page size ⇒ overflow chain
	if !NeedsOverflow(cfg, bigKey, bigVal) {
		t.Fatalf("test precondition: value of %d bytes does not overflow", len(bigVal))
	}
	root, err := Put(pw, cfg, 0, bigKey, bigVal)
	if err != nil {
		t.Fatalf("Put big: %v", err)
	}
	for i := range 40 { // grow a real multi-level tree around the overflow leaf
		nr, err := Put(pw, cfg, root, fmt.Appendf(nil, "s-%03d", i), []byte("v"))
		if err != nil {
			t.Fatalf("Put small %d: %v", i, err)
		}
		root = nr
	}

	// Relocate every tree page EXCEPT the overflow chain (exclude its first
	// page from the predicate). The leaf owning the chain is thus relocated
	// *verbatim*, and its unchanged overflow ref must still resolve the
	// un-relocated chain — the verbatim-leaf-keeps-its-ref path.
	chainFirst := map[uint64]struct{}{}
	if err := WalkLeafEntries(pw, cfg, root, ^uint64(0), func(e page.LeafEntry) error {
		if e.IsOverflow() {
			chainFirst[e.OverflowPage] = struct{}{}
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkLeafEntries: %v", err)
	}
	newRoot, moved, err := RelocatePages(pw, cfg, root, func(id uint64) bool {
		_, isChain := chainFirst[id]
		return !isChain
	}, 1<<30)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	if moved == 0 {
		t.Fatal("nothing relocated")
	}
	// The chain was NOT relocated (excluded by the predicate)...
	for cp := range chainFirst {
		if _, freed := pw.freed[cp]; freed {
			t.Errorf("chain first page %d was relocated despite the predicate excluding it", cp)
		}
	}
	// ...yet the verbatim-relocated leaf's ref still resolves it.
	got, found, err := Get(pw, cfg, newRoot, bigKey)
	if err != nil || !found || !bytes.Equal(got, bigVal) {
		t.Errorf("big value after relocation: found=%v err=%v len(got)=%d want %d", found, err, len(got), len(bigVal))
	}
}

// TestRelocatePagesBudgetBound: a small maxMoves caps eligible relocations
// while leaving the tree fully consistent (partial relocation round-trips,
// and some original pages survive un-relocated).
func TestRelocatePagesBudgetBound(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root, want := buildTreeForRelocate(t, pw, cfg, 200)
	before := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, root, before)

	const maxMoves = 3
	newRoot, moved, err := RelocatePages(pw, cfg, root, func(uint64) bool { return true }, maxMoves)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	if moved > maxMoves {
		t.Errorf("moved=%d exceeds maxMoves=%d", moved, maxMoves)
	}
	if moved == 0 {
		t.Error("moved=0; expected partial relocation")
	}
	assertSameKV(t, want, collectKVForRelocate(t, pw, cfg, newRoot))

	after := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, newRoot, after)
	survivors := 0
	for id := range after {
		if _, old := before[id]; old {
			survivors++
		}
	}
	if survivors == 0 {
		t.Errorf("expected un-relocated original pages to survive with maxMoves=%d", maxMoves)
	}
}

// TestRelocatePagesTargeted: a predicate matching only the upper half of
// the page-id space relocates exactly those pages (ancestor pointer-fix
// CoWs are mandatory and uncounted), preserves contents, retires the
// targeted originals, and leaves no targeted old id in the new tree.
func TestRelocatePagesTargeted(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root, want := buildTreeForRelocate(t, pw, cfg, 200)
	before := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, root, before)

	ids := make([]uint64, 0, len(before))
	for id := range before {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	threshold := ids[len(ids)/2]
	eligible := 0
	for _, id := range ids {
		if id > threshold {
			eligible++
		}
	}
	if eligible == 0 {
		t.Fatalf("no eligible pages above threshold %d (tree too small)", threshold)
	}

	newRoot, moved, err := RelocatePages(pw, cfg, root, func(id uint64) bool { return id > threshold }, 1<<30)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	if moved != eligible {
		t.Errorf("moved=%d, want %d (pages with id > %d)", moved, eligible, threshold)
	}
	assertSameKV(t, want, collectKVForRelocate(t, pw, cfg, newRoot))

	for id := range before {
		_, freed := pw.freed[id]
		if id > threshold && !freed {
			t.Errorf("targeted page %d not retired", id)
		}
	}
	after := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, newRoot, after)
	for id := range after {
		if _, old := before[id]; old && id > threshold {
			t.Errorf("targeted old page %d still referenced by the new tree", id)
		}
	}
}

// TestRelocatePagesRelocatesOverflowChains: a predicate matching only the
// overflow-chain first pages relocates each chain to a fresh run, rewrites
// the owning leaf entry (re-encoding the leaf), retires the old run, and
// preserves every value. Tree-node ids are excluded by the predicate, so
// the only counted moves are chain pages (the forced leaf re-encodes +
// ancestor fixes are uncounted overhead).
func TestRelocatePagesRelocatesOverflowChains(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	want := map[string]string{}
	// Overflow values (6000 B > page) interleaved with small keys so the
	// tree has branches too.
	const nOvf = 12
	for i := range nOvf {
		key := fmt.Appendf(nil, "ovf-%03d", i)
		val := bytes.Repeat([]byte{byte('A' + i%26)}, 6000)
		nr, err := Put(pw, cfg, root, key, val)
		if err != nil {
			t.Fatalf("Put overflow %d: %v", i, err)
		}
		root = nr
		want[string(key)] = string(val)
		nr, err = Put(pw, cfg, root, fmt.Appendf(nil, "small-%03d", i), []byte("v"))
		if err != nil {
			t.Fatalf("Put small %d: %v", i, err)
		}
		root = nr
		want[fmt.Sprintf("small-%03d", i)] = "v"
	}

	// First pages of every overflow chain, and the expected moved count
	// (sum of run lengths).
	chainFirst := map[uint64]struct{}{}
	wantMoved := 0
	if err := WalkLeafEntries(pw, cfg, root, ^uint64(0), func(e page.LeafEntry) error {
		if e.IsOverflow() {
			chainFirst[e.OverflowPage] = struct{}{}
			wantMoved += int(page.OverflowRunLength(cfg, e.TotalLen))
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkLeafEntries: %v", err)
	}
	if len(chainFirst) != nOvf {
		t.Fatalf("built %d overflow chains, want %d", len(chainFirst), nOvf)
	}

	newRoot, moved, err := RelocatePages(pw, cfg, root, func(id uint64) bool {
		_, isChain := chainFirst[id]
		return isChain
	}, 1<<30)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	if moved != wantMoved {
		t.Errorf("moved=%d, want %d (sum of chain run lengths)", moved, wantMoved)
	}
	// Every value round-trips (overflow refs rewritten correctly).
	assertSameKV(t, want, collectKVForRelocate(t, pw, cfg, newRoot))
	// Old chain first pages retired; the new tree references none of them.
	for cp := range chainFirst {
		if _, freed := pw.freed[cp]; !freed {
			t.Errorf("old chain first page %d not retired", cp)
		}
	}
	if err := WalkLeafEntries(pw, cfg, newRoot, ^uint64(0), func(e page.LeafEntry) error {
		if e.IsOverflow() {
			if _, old := chainFirst[e.OverflowPage]; old {
				t.Errorf("new tree still references old chain first page %d", e.OverflowPage)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkLeafEntries(new): %v", err)
	}
}

// TestRelocatePagesRelocatesNestedTree: a parent tree holding a nested-tree
// reference cell (SetKeyspace member set promoted to its own B+tree) has the
// whole nested subtree relocated when the predicate targets the nested pages.
// Pins the 12.5b-2b entailed invariant: after relocation (a) the parent's
// NestedRoot is rewritten to the relocated nested root and every member stays
// reachable (referential integrity across the nesting boundary), and (b) the
// cached NestedCount is preserved (set-keyspace.md E1) — the strong case where
// structure is fully intact yet a dropped count would silently break Count.
func TestRelocatePagesRelocatesNestedTree(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)

	// A multi-level nested B+tree of members (100-byte values force splits,
	// so the nested tree has branches + leaves — exercising the recursive
	// branch pointer-fix within the nested subtree).
	const nMembers = 200
	nestedRoot := uint64(0)
	wantMembers := make(map[string]string, nMembers)
	for i := range nMembers {
		key := fmt.Appendf(nil, "m-%05d", i)
		val := bytes.Repeat([]byte{byte('a' + i%26)}, 100)
		nr, err := Put(pw, cfg, nestedRoot, key, val)
		if err != nil {
			t.Fatalf("Put member %d: %v", i, err)
		}
		nestedRoot = nr
		wantMembers[string(key)] = string(val)
	}

	// Parent tree: the nested-tree reference cell plus two inline keys
	// flanking it, so the parent-leaf re-encode (forced by the NestedRoot
	// rewrite) must also faithfully carry the inline cells.
	parentRoot, _, err := PutEntry(pw, cfg, 0, page.LeafEntry{
		Flags:       page.CellFlagMultiValue | page.CellFlagNestedTree,
		Key:         []byte("mmm-set"),
		NestedRoot:  nestedRoot,
		NestedCount: nMembers,
	})
	if err != nil {
		t.Fatalf("PutEntry nested-tree: %v", err)
	}
	for _, k := range []string{"aaa", "zzz"} {
		nr, err := Put(pw, cfg, parentRoot, []byte(k), []byte("inline-"+k))
		if err != nil {
			t.Fatalf("Put inline %q: %v", k, err)
		}
		parentRoot = nr
	}

	// Snapshot the nested subtree's page set + contents.
	nestedBefore := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, nestedRoot, nestedBefore)
	if len(nestedBefore) < 3 {
		t.Fatalf("nested tree too small (%d pages) to exercise multi-level relocation", len(nestedBefore))
	}

	// Relocate ONLY the nested subtree pages. The parent leaf is not
	// eligible, so the only counted moves are nested pages; the parent leaf
	// re-encode + ancestor fixes are uncounted overhead.
	newParentRoot, moved, err := RelocatePages(pw, cfg, parentRoot, func(id uint64) bool {
		_, isNested := nestedBefore[id]
		return isNested
	}, 1<<30)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	if moved != len(nestedBefore) {
		t.Errorf("moved=%d, want %d (every nested page eligible)", moved, len(nestedBefore))
	}

	// Read the rewritten nested-tree cell from the new parent.
	var newNestedRoot, gotCount uint64
	found := false
	if err := WalkLeafEntries(pw, cfg, newParentRoot, ^uint64(0), func(e page.LeafEntry) error {
		if e.IsNestedTree() && bytes.Equal(e.Key, []byte("mmm-set")) {
			newNestedRoot, gotCount, found = e.NestedRoot, e.NestedCount, true
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkLeafEntries(new parent): %v", err)
	}
	if !found {
		t.Fatal("nested-tree cell missing from relocated parent")
	}
	// (a) ref rewritten to a fresh id; (b) count preserved.
	if newNestedRoot == nestedRoot {
		t.Errorf("NestedRoot unchanged (%d) despite the nested root being relocated", newNestedRoot)
	}
	if _, old := nestedBefore[newNestedRoot]; old {
		t.Errorf("new NestedRoot %d reuses an old nested page id", newNestedRoot)
	}
	if gotCount != nMembers {
		t.Errorf("NestedCount=%d after relocation, want %d (E1 broken)", gotCount, nMembers)
	}

	// Every member round-trips via the relocated nested tree.
	assertSameKV(t, wantMembers, collectKVForRelocate(t, pw, cfg, newNestedRoot))

	// Old nested pages retired; the relocated nested tree references none.
	for id := range nestedBefore {
		if _, freed := pw.freed[id]; !freed {
			t.Errorf("old nested page %d not retired", id)
		}
	}
	nestedAfter := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, newNestedRoot, nestedAfter)
	if len(nestedAfter) != len(nestedBefore) {
		t.Errorf("nested page count changed: %d → %d (structure not preserved)", len(nestedBefore), len(nestedAfter))
	}
	for id := range nestedAfter {
		if _, old := nestedBefore[id]; old {
			t.Errorf("relocated nested tree still references old page id %d", id)
		}
	}

	// The flanking inline keys survived the parent-leaf re-encode.
	for _, k := range []string{"aaa", "zzz"} {
		got, ok, err := Get(pw, cfg, newParentRoot, []byte(k))
		if err != nil || !ok || string(got) != "inline-"+k {
			t.Errorf("inline %q after relocation: got=%q ok=%v err=%v", k, got, ok, err)
		}
	}
}

// TestRelocatePagesNestedTreeBudgetBound: a small maxMoves truncates a
// nested-subtree relocation partway through. Pins that a budget-truncated
// partial nested relocation is still fully consistent — every member stays
// reachable via the (rewritten) NestedRoot, NestedCount is preserved, the
// move count respects the budget, and some original nested pages survive
// un-relocated (genuine partial progress). The bottom-up cascade that makes
// this hold is the same one TestRelocatePagesBudgetBound pins at the top
// level; this asserts it across the nesting boundary directly.
func TestRelocatePagesNestedTreeBudgetBound(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)

	const nMembers = 200
	nestedRoot := uint64(0)
	wantMembers := make(map[string]string, nMembers)
	for i := range nMembers {
		key := fmt.Appendf(nil, "m-%05d", i)
		val := bytes.Repeat([]byte{byte('a' + i%26)}, 100)
		nr, err := Put(pw, cfg, nestedRoot, key, val)
		if err != nil {
			t.Fatalf("Put member %d: %v", i, err)
		}
		nestedRoot = nr
		wantMembers[string(key)] = string(val)
	}
	parentRoot, _, err := PutEntry(pw, cfg, 0, page.LeafEntry{
		Flags:       page.CellFlagMultiValue | page.CellFlagNestedTree,
		Key:         []byte("mmm-set"),
		NestedRoot:  nestedRoot,
		NestedCount: nMembers,
	})
	if err != nil {
		t.Fatalf("PutEntry nested-tree: %v", err)
	}

	nestedBefore := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, nestedRoot, nestedBefore)
	if len(nestedBefore) < 4 {
		t.Fatalf("nested tree too small (%d pages) to exercise a partial relocation", len(nestedBefore))
	}

	const maxMoves = 3 // < len(nestedBefore): budget truncates mid-subtree.
	newParentRoot, moved, err := RelocatePages(pw, cfg, parentRoot, func(id uint64) bool {
		_, isNested := nestedBefore[id]
		return isNested
	}, maxMoves)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	if moved > maxMoves {
		t.Errorf("moved=%d exceeds maxMoves=%d", moved, maxMoves)
	}
	if moved == 0 {
		t.Error("moved=0; expected partial relocation")
	}

	// Some nested page moved ⇒ the nested root id changed ⇒ the parent ref
	// was rewritten. Read the new root + count.
	var newNestedRoot, gotCount uint64
	found := false
	if err := WalkLeafEntries(pw, cfg, newParentRoot, ^uint64(0), func(e page.LeafEntry) error {
		if e.IsNestedTree() && bytes.Equal(e.Key, []byte("mmm-set")) {
			newNestedRoot, gotCount, found = e.NestedRoot, e.NestedCount, true
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkLeafEntries: %v", err)
	}
	if !found {
		t.Fatal("nested-tree cell missing from parent after partial relocation")
	}
	if gotCount != nMembers {
		t.Errorf("NestedCount=%d after partial relocation, want %d", gotCount, nMembers)
	}

	// Every member round-trips through the partially-relocated nested tree.
	assertSameKV(t, wantMembers, collectKVForRelocate(t, pw, cfg, newNestedRoot))

	// Genuine partial progress: some original nested pages survive un-relocated.
	nestedAfter := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, newNestedRoot, nestedAfter)
	survivors := 0
	for id := range nestedAfter {
		if _, old := nestedBefore[id]; old {
			survivors++
		}
	}
	if survivors == 0 {
		t.Errorf("expected un-relocated original nested pages to survive with maxMoves=%d", maxMoves)
	}
}

// TestRelocateLeafReEncodePreservesAllCellTypes: a single leaf carrying one
// of every cell variant — inline, overflow ref, subpage, nested-tree ref —
// is re-encoded when its overflow chain is relocated (predicate targets only
// the chain). Pins that the AddEntry-dispatch re-encode round-trips ALL cell
// types verbatim (not just the overflow ref it rewrote): inline value,
// subpage bytes, and the untouched nested-tree ref (NestedRoot + NestedCount)
// must all survive.
func TestRelocateLeafReEncodePreservesAllCellTypes(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)

	// A small nested tree for the nested-tree ref cell to point at (left
	// un-relocated by the predicate, so its root id must stay constant).
	nestedRoot := uint64(0)
	for i := range 5 {
		nr, err := Put(pw, cfg, nestedRoot, fmt.Appendf(nil, "nm-%02d", i), []byte("x"))
		if err != nil {
			t.Fatalf("Put nested member %d: %v", i, err)
		}
		nestedRoot = nr
	}
	const nestedCount = 5

	// One leaf, four cell variants (keys ordered so they share a single leaf).
	root := uint64(0)
	root, err := Put(pw, cfg, root, []byte("a-inline"), []byte("inline-value"))
	if err != nil {
		t.Fatalf("Put inline: %v", err)
	}
	bigVal := bytes.Repeat([]byte{0x5A}, 6000) // > page ⇒ overflow chain
	if !NeedsOverflow(cfg, []byte("b-overflow"), bigVal) {
		t.Fatalf("test precondition: value does not overflow")
	}
	root, err = Put(pw, cfg, root, []byte("b-overflow"), bigVal)
	if err != nil {
		t.Fatalf("Put overflow: %v", err)
	}
	sp, _ := page.EncodeSubpage([][]byte{[]byte("s1"), []byte("s2")}, 0)
	root, _, err = PutEntry(pw, cfg, root, page.LeafEntry{
		Flags: page.CellFlagMultiValue,
		Key:   []byte("c-subpage"),
		Value: sp,
	})
	if err != nil {
		t.Fatalf("PutEntry subpage: %v", err)
	}
	root, _, err = PutEntry(pw, cfg, root, page.LeafEntry{
		Flags:       page.CellFlagMultiValue | page.CellFlagNestedTree,
		Key:         []byte("d-nested"),
		NestedRoot:  nestedRoot,
		NestedCount: nestedCount,
	})
	if err != nil {
		t.Fatalf("PutEntry nested: %v", err)
	}

	// Find the overflow chain's first page; relocate ONLY it. The owning
	// leaf is thus re-encoded (refsRewritten) without being itself eligible.
	chainFirst := map[uint64]struct{}{}
	if err := WalkLeafEntries(pw, cfg, root, ^uint64(0), func(e page.LeafEntry) error {
		if e.IsOverflow() {
			chainFirst[e.OverflowPage] = struct{}{}
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkLeafEntries: %v", err)
	}
	if len(chainFirst) != 1 {
		t.Fatalf("expected exactly one overflow chain, got %d", len(chainFirst))
	}
	newRoot, moved, err := RelocatePages(pw, cfg, root, func(id uint64) bool {
		_, isChain := chainFirst[id]
		return isChain
	}, 1<<30)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	if moved == 0 {
		t.Fatal("overflow chain not relocated")
	}

	// Inline value intact.
	if got, ok, err := Get(pw, cfg, newRoot, []byte("a-inline")); err != nil || !ok || string(got) != "inline-value" {
		t.Errorf("inline after re-encode: got=%q ok=%v err=%v", got, ok, err)
	}
	// Overflow value intact (ref rewritten to the relocated chain).
	if got, ok, err := Get(pw, cfg, newRoot, []byte("b-overflow")); err != nil || !ok || !bytes.Equal(got, bigVal) {
		t.Errorf("overflow after re-encode: ok=%v err=%v len(got)=%d want %d", ok, err, len(got), len(bigVal))
	}
	// Subpage + nested-tree cells round-trip verbatim through the re-encode.
	se, ok, err := GetEntry(pw, cfg, newRoot, []byte("c-subpage"))
	if err != nil || !ok || !se.IsSubpage() || !bytes.Equal(se.Value, sp) {
		t.Errorf("subpage after re-encode: ok=%v err=%v IsSubpage=%v bytesEqual=%v", ok, err, se.IsSubpage(), bytes.Equal(se.Value, sp))
	}
	ne, ok, err := GetEntry(pw, cfg, newRoot, []byte("d-nested"))
	if err != nil || !ok || !ne.IsNestedTree() {
		t.Fatalf("nested cell after re-encode: ok=%v err=%v IsNestedTree=%v", ok, err, ne.IsNestedTree())
	}
	if ne.NestedRoot != nestedRoot {
		t.Errorf("NestedRoot changed (%d → %d) though the nested subtree was not relocated", nestedRoot, ne.NestedRoot)
	}
	if ne.NestedCount != nestedCount {
		t.Errorf("NestedCount=%d after re-encode, want %d", ne.NestedCount, nestedCount)
	}
}

// TestRelocatePagesSingleLeaf: a one-page (root leaf) tree relocates to a
// fresh id with contents intact and the old leaf retired.
func TestRelocatePagesSingleLeaf(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root, err := Put(pw, cfg, 0, []byte("k"), []byte("v"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	newRoot, moved, err := RelocatePages(pw, cfg, root, func(uint64) bool { return true }, 10)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	if moved != 1 || newRoot == root {
		t.Errorf("single-leaf: moved=%d newRoot=%d (root=%d), want moved=1 and a new root", moved, newRoot, root)
	}
	got, found, err := Get(pw, cfg, newRoot, []byte("k"))
	if err != nil || !found || !bytes.Equal(got, []byte("v")) {
		t.Errorf("Get after relocate: got=%q found=%v err=%v", got, found, err)
	}
	if _, freed := pw.freed[root]; !freed {
		t.Errorf("old root leaf %d not retired", root)
	}
}

// TestRelocatePagesEmpty: an empty tree (root 0) is a no-op.
func TestRelocatePagesEmpty(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	nr, moved, err := RelocatePages(pw, cfg, 0, func(uint64) bool { return true }, 10)
	if err != nil || nr != 0 || moved != 0 {
		t.Errorf("empty tree: nr=%d moved=%d err=%v, want 0,0,nil", nr, moved, err)
	}
}
