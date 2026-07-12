package btree

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// Btree-layer tests for DeleteRange. The higher-level
// gmdb-package tests in delete_range_test.go pin the public surface
// against the deferred-flush descriptor machinery; these tests pin
// the btree-layer invariants directly against an in-memory pager.

// plainCellFreeForTest is the per-cell free callback DeleteRange
// expects: retire the overflow chain when CellFlagOverflow is set
// and contribute 1 per entry to the values count. Matches the
// shape Keyspace.DeleteRange uses in production (plain key→value
// cells, no subpage or nested-tree). All btree-layer tests pass
// this — the SetKeyspace shape (subpage / nested-tree handling)
// is covered by the higher-level gmdb-package tests.
func plainCellFreeForTest(pw PageWriter, cfg page.Config, e page.LeafEntry) (uint64, error) {
	if err := freeOverflowChainIfPresent(pw, cfg, e); err != nil {
		return 0, err
	}
	return 1, nil
}

// TestBtreeDeleteRangeEmptyTree promotes the rootID == 0 edge case:
// DeleteRange on an empty tree returns (0, 0, nil) and does not
// allocate or free pages.
func TestBtreeDeleteRangeEmptyTree(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 16)
	defer pw.Close()
	defer f.Close()
	count, newRoot, err := DeleteRange(pw, pw.Config(), 0, DefaultMergeThreshold, []byte("a"), []byte("z"), plainCellFreeForTest)
	if err != nil {
		t.Fatalf("DeleteRange(empty): %v", err)
	}
	if count != 0 || newRoot != 0 {
		t.Errorf("DeleteRange(empty) = (%d, %d), want (0, 0)", count, newRoot)
	}
}

// TestBtreeDeleteRangeEmptyRange promotes the start >= end no-op:
// DeleteRange with start == end (or start > end) returns (0, rootID,
// nil) — the tree is unchanged and the original rootID round-trips.
func TestBtreeDeleteRangeEmptyRange(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 32)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()
	root, err := Put(pw, cfg, 0, []byte("k"), []byte("v"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, pair := range []struct{ s, e []byte }{
		{[]byte("a"), []byte("a")},
		{[]byte("z"), []byte("a")},
	} {
		count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, pair.s, pair.e, plainCellFreeForTest)
		if err != nil {
			t.Fatalf("DeleteRange(%q, %q): %v", pair.s, pair.e, err)
		}
		if count != 0 {
			t.Errorf("DeleteRange(%q, %q) count = %d, want 0", pair.s, pair.e, count)
		}
		if newRoot != root {
			t.Errorf("DeleteRange(%q, %q) rotated root: got %d, want %d", pair.s, pair.e, newRoot, root)
		}
	}
}

// TestBtreeDeleteRangeBoundaryInclusion promotes range-delete.md
// invariant #1 (clause-explicit): start is inclusive, end is
// exclusive.
func TestBtreeDeleteRangeBoundaryInclusion(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 64)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()
	keys := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d"), []byte("e")}
	root := uint64(0)
	var err error
	for _, k := range keys {
		root, err = Put(pw, cfg, root, k, []byte("v"))
		if err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	// Delete [b, d): expect b and c gone, a/d/e present.
	count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, []byte("b"), []byte("d"), plainCellFreeForTest)
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if count != 2 {
		t.Errorf("DeleteRange count = %d, want 2", count)
	}
	for _, tc := range []struct {
		k    []byte
		want bool
	}{
		{[]byte("a"), true},
		{[]byte("b"), false},
		{[]byte("c"), false},
		{[]byte("d"), true},
		{[]byte("e"), true},
	} {
		ok, err := Has(pw, cfg, newRoot, tc.k)
		if err != nil {
			t.Fatalf("Has(%q): %v", tc.k, err)
		}
		if ok != tc.want {
			t.Errorf("Has(%q) = %v, want %v", tc.k, ok, tc.want)
		}
	}
}

// TestBtreeDeleteRangeOpenBoundaries promotes range-delete.md
// invariant #1 open-boundary clause: nil start = open-left, nil end =
// open-right, (nil, nil) deletes everything.
func TestBtreeDeleteRangeOpenBoundaries(t *testing.T) {
	cases := []struct {
		name        string
		start, end  []byte
		wantDeleted []string
		wantKept    []string
	}{
		{"open-left", nil, []byte("c"), []string{"a", "b"}, []string{"c", "d", "e"}},
		{"open-right", []byte("c"), nil, []string{"c", "d", "e"}, []string{"a", "b"}},
		{"both-open", nil, nil, []string{"a", "b", "c", "d", "e"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pw, _, f := setupPagerWriter(t, 64)
			defer pw.Close()
			defer f.Close()
			cfg := pw.Config()
			root := uint64(0)
			for _, k := range []string{"a", "b", "c", "d", "e"} {
				var err error
				root, err = Put(pw, cfg, root, []byte(k), []byte("v"))
				if err != nil {
					t.Fatalf("Put %q: %v", k, err)
				}
			}
			count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, tc.start, tc.end, plainCellFreeForTest)
			if err != nil {
				t.Fatalf("DeleteRange: %v", err)
			}
			if int(count) != len(tc.wantDeleted) {
				t.Errorf("count = %d, want %d", count, len(tc.wantDeleted))
			}
			for _, k := range tc.wantDeleted {
				ok, err := Has(pw, cfg, newRoot, []byte(k))
				if err != nil {
					t.Fatalf("Has(%q): %v", k, err)
				}
				if ok {
					t.Errorf("key %q present post-delete; should be deleted", k)
				}
			}
			for _, k := range tc.wantKept {
				ok, err := Has(pw, cfg, newRoot, []byte(k))
				if err != nil {
					t.Fatalf("Has(%q): %v", k, err)
				}
				if !ok {
					t.Errorf("key %q missing post-delete; should be kept", k)
				}
			}
		})
	}
}

// TestBtreeDeleteRangeAllKeys promotes range-delete.md invariant #3
// root-collapse to 0: DeleteRange(nil, nil) on a non-empty tree
// returns newRoot=0 (empty tree).
func TestBtreeDeleteRangeAllKeys(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 64)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()
	root := uint64(0)
	for i := range 50 {
		var err error
		key := []byte(fmt.Sprintf("k%04d", i))
		root, err = Put(pw, cfg, root, key, []byte("v"))
		if err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
	}
	count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, nil, nil, plainCellFreeForTest)
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if count != 50 {
		t.Errorf("count = %d, want 50", count)
	}
	if newRoot != 0 {
		t.Errorf("newRoot = %d, want 0 (root collapse to empty tree)", newRoot)
	}
}

// TestBtreeDeleteRangeMultiLevelInteriorRetire builds a tree with a
// multi-level branch, deletes an interior range, and asserts every
// page in the interior subtrees enters loosePages/retiredPages while
// the boundary leaves remain well-formed.
//
// Workload sized to fit the 16 MB tx-slab budget set by
// setupPagerWriter: 1200 keys × 100-byte values → ~30 leaves at 4 KB
// pages with one branch level (depth 2). The internal slab-reuse
// machinery (the loose-pop pool) keeps the working set bounded.
func TestBtreeDeleteRangeMultiLevelInteriorRetire(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 1024)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()
	val := bytes.Repeat([]byte{0x42}, 100)
	root := uint64(0)
	var err error
	for i := range 1200 {
		key := []byte(fmt.Sprintf("k%05d", i))
		root, err = Put(pw, cfg, root, key, val)
		if err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
	}
	// Sanity: tree should have at least one branch level.
	rootBuf, _ := pw.Page(root)
	rootTyp, _, _, _ := page.ReadHeader(rootBuf)
	if rootTyp != page.TypeBranch {
		t.Fatalf("test setup too small — root is leaf, not branch")
	}
	// Delete the middle 600 keys [k00300, k00900).
	startK := []byte("k00300")
	endK := []byte("k00900")
	count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, startK, endK, plainCellFreeForTest)
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if count != 600 {
		t.Errorf("count = %d, want 600", count)
	}
	for i := range 300 {
		ok, err := Has(pw, cfg, newRoot, []byte(fmt.Sprintf("k%05d", i)))
		if err != nil {
			t.Fatalf("Has #%d: %v", i, err)
		}
		if !ok {
			t.Errorf("pre-range key %d missing", i)
			break
		}
	}
	for i := 900; i < 1200; i++ {
		ok, err := Has(pw, cfg, newRoot, []byte(fmt.Sprintf("k%05d", i)))
		if err != nil {
			t.Fatalf("Has #%d: %v", i, err)
		}
		if !ok {
			t.Errorf("post-range key %d missing", i)
			break
		}
	}
	// Deleted keys absent.
	for i := 300; i < 900; i += 50 {
		ok, err := Has(pw, cfg, newRoot, []byte(fmt.Sprintf("k%05d", i)))
		if err != nil {
			t.Fatalf("Has #%d: %v", i, err)
		}
		if ok {
			t.Errorf("in-range key %d still present", i)
			break
		}
	}
	// Boundary-adjacent keys reachable via Get (separator-correctness
	// check — a botched rebalance would route here to the wrong
	// subtree).
	if _, found, err := Get(pw, cfg, newRoot, []byte("k00299")); err != nil || !found {
		t.Errorf("Get(k00299) post-DeleteRange: found=%v err=%v", found, err)
	}
	if _, found, err := Get(pw, cfg, newRoot, []byte("k00900")); err != nil || !found {
		t.Errorf("Get(k00900) post-DeleteRange: found=%v err=%v", found, err)
	}
}

// TestBtreeDeleteRangeOverflowChainRetire promotes range-delete.md
// invariant #2: every overflow run referenced by a deleted entry is
// retired (no orphan overflow chains).
func TestBtreeDeleteRangeOverflowChainRetire(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 64)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()
	root := uint64(0)
	var err error
	// 3 keys, each with a 16 KB value → each promotes to overflow.
	bigVal := bytes.Repeat([]byte{0xAB}, 16384)
	for _, k := range []string{"a", "b", "c"} {
		root, err = Put(pw, cfg, root, []byte(k), bigVal)
		if err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	reachable := collectSubtreePages(t, pw, cfg, root)
	if len(reachable) < 13 {
		// 1 leaf + 3 × ~4-page overflow chains = ≥13 pages.
		t.Fatalf("expected ≥13 reachable pages (leaf + 3 overflow chains), got %d", len(reachable))
	}
	count, _, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, []byte("a"), []byte("d"), plainCellFreeForTest)
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	// Every reachable page must be retired/loose.
	retired := pw.RetiredPages()
	loose := pw.LoosePages()
	retiredSet := make(map[uint64]struct{}, len(retired))
	for _, id := range retired {
		retiredSet[id] = struct{}{}
	}
	for id := range reachable {
		_, inRetired := retiredSet[id]
		_, inLoose := loose[id]
		if !inRetired && !inLoose {
			t.Errorf("page %d not retired/loose post-DeleteRange (overflow leak?)", id)
		}
	}
}

// TestBtreeDeleteRangeMissOnRange asserts a range that contains no
// keys returns count=0 and the root unchanged. Validates the
// "deletedCount == 0" short-circuit path through deleteRangeFromBranch.
func TestBtreeDeleteRangeMissOnRange(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 16)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()
	root := uint64(0)
	var err error
	for _, k := range []string{"a", "c", "e"} {
		root, err = Put(pw, cfg, root, []byte(k), []byte("v"))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Range (a, c) — exclusive of both — touches the gap between
	// existing keys. Wait: spec is [start, end). [b, c) → no hit
	// because b doesn't exist. count should be 0.
	count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, []byte("b"), []byte("c"), plainCellFreeForTest)
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
	if newRoot != root {
		t.Errorf("newRoot = %d, want %d (unchanged on no-hit)", newRoot, root)
	}
	// All keys still present.
	for _, k := range []string{"a", "c", "e"} {
		ok, err := Has(pw, cfg, newRoot, []byte(k))
		if err != nil {
			t.Fatalf("Has(%q): %v", k, err)
		}
		if !ok {
			t.Errorf("key %q missing after no-op DeleteRange", k)
		}
	}
}

// TestBtreeDeleteRangeWellFormedAfter promotes range-delete.md
// invariant #3: post-DeleteRange tree is well-formed (separators
// satisfy max(left) < S <= min(right)) — verified via complete
// per-key cursor walk in sorted order.
func TestBtreeDeleteRangeWellFormedAfter(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 256)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()
	root := uint64(0)
	var err error
	// 200 keys with 200-byte values → likely 2-level branch.
	val := bytes.Repeat([]byte{0x42}, 200)
	for i := range 200 {
		key := []byte(fmt.Sprintf("k%05d", i))
		root, err = Put(pw, cfg, root, key, val)
		if err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
	}
	// Delete a slice from the middle.
	_, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, []byte("k00050"), []byte("k00150"), plainCellFreeForTest)
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	// Walk the tree in order; every key < k00050 or >= k00150 must
	// appear once, in sorted order.
	c := NewReadCursor(pw, cfg, newRoot)
	var seen []string
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		seen = append(seen, string(k))
	}
	if err := c.Err(); err != nil {
		t.Fatalf("Cursor.Err: %v", err)
	}
	expected := []string{}
	for i := range 50 {
		expected = append(expected, fmt.Sprintf("k%05d", i))
	}
	for i := 150; i < 200; i++ {
		expected = append(expected, fmt.Sprintf("k%05d", i))
	}
	if len(seen) != len(expected) {
		t.Fatalf("cursor walk returned %d keys, want %d", len(seen), len(expected))
	}
	for i, k := range expected {
		if seen[i] != k {
			t.Errorf("cursor[%d] = %q, want %q", i, seen[i], k)
			break
		}
	}
}

// TestBtreeDeleteRangePreservesFillFloor pins the range-delete.md
// fill-floor invariant: after a successful DeleteRange, every non-root
// page reachable from the new root has fill >= MergeThreshold% of
// ContentEnd.
//
// Reproduction shape (cousin-cascade case):
//
//	ROOT(branch) -> [P(branch), Q(branch), R(branch)]
//	P -> [A(leaf), B(leaf), C(leaf), D, E, F]  // 6 leaves, leftIdx=0..rightIdx=cellCount range
//	Q -> 6 leaves untouched
//	R -> 6 leaves untouched
//
// DeleteRange targets P's entire key range minus the first key of A
// and the last key of F — both boundary leaves come back with a
// single entry (below MT), and B..E are retired. In rebalanceSurvivors
// the two surviving boundary leaves merge into one below-MT page; P
// degenerates to that single child and cascades upward into ROOT,
// where mergeOrRedistributeBranches absorbs P into Q. The merged
// result M ends up as a non-root child of ROOT (because R remains as
// a sibling, so ROOT does NOT root-collapse) with the below-MT merged
// leaf as M.leftmost — violating the fill-floor for a non-root page.
//
// Built by hand at PageSize=4096 (the minimum, per page.Config) with
// a non-default MergeThreshold=10 so each leaf needs only 408 B of
// content and each non-root branch needs only ~4 cells with padded
// separator keys; sequential Puts at default PageSize give a similar
// shape but with too much fill jitter to reliably trigger the
// 2-survivor case at the intermediate-branch level.
func TestBtreeDeleteRangePreservesFillFloor(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)

	// Custom MergeThreshold = 10 so the math stays tractable for a
	// hand-built shape: leaves need >= ~408 B fill; branches need
	// >= ~408 B encoded size. Default 25 would force 5x more
	// children per branch — too tedious to hand-build, and obscures
	// the actual scenario.
	const mt uint8 = 10

	// Page-id allocation plan; drive nextID forward by hand so the
	// build is deterministic, then AllocPage during DeleteRange
	// picks up from there.
	//
	//   1     = ROOT
	//   2,3,4 = P, Q, R intermediate branches
	//   5..10 = P's leaves A..F
	//   11..16 = Q's leaves
	//   17..22 = R's leaves
	const (
		rootID = 1
		pID    = 2
		qID    = 3
		rID    = 4
		aID    = 5
		bID    = 6
		cID    = 7
		dID    = 8
		eID    = 9
		fID    = 10
	)
	pw.nextID = 23

	// Each entry: key 100B padded + value 50B = ~160B encoded.
	// 3 entries per leaf -> ~480B fill -> above MT (~408B). The key
	// padding (100B) also pads branch separators so each cell costs
	// ~111B -> 4 cells per branch reaches MT.
	padKey := func(s string) []byte {
		out := make([]byte, 100)
		copy(out, []byte(s))
		return out
	}
	val := bytes.Repeat([]byte{0xAB}, 50)

	buildLeaf := func(id uint64, keys []string) {
		t.Helper()
		buf, err := pw.ZeroPage(id)
		if err != nil {
			t.Fatalf("ZeroPage(%d): %v", id, err)
		}
		b := page.NewLeafBuilder(buf, cfg)
		for _, k := range keys {
			if !b.AddEntry(page.LeafEntry{Key: padKey(k), Value: val}) {
				t.Fatalf("leaf %d: AddEntry(%q) overflow", id, k)
			}
		}
		b.Finish()
	}

	// P's leaves: 6 leaves × 3 entries each.
	buildLeaf(aID, []string{"a01", "a02", "a03"})
	buildLeaf(bID, []string{"b01", "b02", "b03"})
	buildLeaf(cID, []string{"c01", "c02", "c03"})
	buildLeaf(dID, []string{"d01", "d02", "d03"})
	buildLeaf(eID, []string{"e01", "e02", "e03"})
	buildLeaf(fID, []string{"f01", "f02", "f03"})

	// Q and R: 6 leaves each, kept well-filled so they satisfy the
	// invariant initially.
	for q := range 6 {
		buildLeaf(uint64(11+q), []string{
			fmt.Sprintf("q%c01", 'a'+q),
			fmt.Sprintf("q%c02", 'a'+q),
			fmt.Sprintf("q%c03", 'a'+q),
		})
		buildLeaf(uint64(17+q), []string{
			fmt.Sprintf("r%c01", 'a'+q),
			fmt.Sprintf("r%c02", 'a'+q),
			fmt.Sprintf("r%c03", 'a'+q),
		})
	}

	buildBranch := func(id uint64, leftmost uint64, cells []page.BranchCell) {
		t.Helper()
		buf, err := pw.ZeroPage(id)
		if err != nil {
			t.Fatalf("ZeroPage(%d): %v", id, err)
		}
		if err := page.EncodeBranch(buf, cfg, leftmost, cells); err != nil {
			t.Fatalf("EncodeBranch(%d): %v", id, err)
		}
	}

	// P: leftmost=A, cells=[(b01,B),(c01,C),(d01,D),(e01,E),(f01,F)]
	buildBranch(pID, aID, []page.BranchCell{
		{Key: padKey("b01"), Child: bID},
		{Key: padKey("c01"), Child: cID},
		{Key: padKey("d01"), Child: dID},
		{Key: padKey("e01"), Child: eID},
		{Key: padKey("f01"), Child: fID},
	})
	// Q: leftmost=L11 (qa group), cells lift the (qb..qf) leaves.
	buildBranch(qID, 11, []page.BranchCell{
		{Key: padKey("qb01"), Child: 12},
		{Key: padKey("qc01"), Child: 13},
		{Key: padKey("qd01"), Child: 14},
		{Key: padKey("qe01"), Child: 15},
		{Key: padKey("qf01"), Child: 16},
	})
	// R: leftmost=L17, cells for ra..rf.
	buildBranch(rID, 17, []page.BranchCell{
		{Key: padKey("rb01"), Child: 18},
		{Key: padKey("rc01"), Child: 19},
		{Key: padKey("rd01"), Child: 20},
		{Key: padKey("re01"), Child: 21},
		{Key: padKey("rf01"), Child: 22},
	})
	// ROOT: leftmost=P, cells=[(qa01,Q),(ra01,R)]. ROOT itself sits
	// far below MT (only 2 cells) but root is exempt.
	buildBranch(rootID, pID, []page.BranchCell{
		{Key: padKey("qa01"), Child: qID},
		{Key: padKey("ra01"), Child: rID},
	})

	// Sanity: hand-built tree satisfies the floor pre-DeleteRange.
	// If the build is too sparse, the post-state assertion below
	// would mask a setup bug as a real failure.
	checkUnderflowInvariant(t, pw, cfg, rootID, mt)

	// DeleteRange("a02"-padded, "f03"-padded) — within P's full key
	// extent (leftIdx=0 since the start routes to leaf A,
	// rightIdx=cellCount=5 since the end routes to leaf F). Phase-2
	// retires B..E; phase-3 rebuilds A to keep [a01] only and F to
	// keep [f03] only. Both boundary survivors come back below MT;
	// rebalanceSurvivors merges them; the merged page is itself
	// below MT. P degenerates, cascades into ROOT via
	// mergeOrRedistributeBranches(P, Q), produces a merged branch M
	// with the sub-MT leaf as M.leftmost. R survives so ROOT does
	// NOT root-collapse — M is a non-root branch of ROOT, and its
	// leftmost is a non-root sub-MT leaf.
	count, newRoot, err := DeleteRange(pw, cfg, rootID, mt, padKey("a02"), padKey("f03"), plainCellFreeForTest)
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	// Sanity on the row count: P had 18 entries (6 leaves × 3); we
	// kept a01 and f03 → deleted 16.
	if count != 16 {
		t.Errorf("count = %d, want 16", count)
	}

	// FILL-FLOOR INVARIANT — the heart of the test. On HEAD this
	// fires because the merged AF leaf sits below MT as a non-root
	// child of the cascade-merged M branch.
	checkUnderflowInvariant(t, pw, cfg, newRoot, mt)
}

// TestBtreeDeleteRangePreservesFillFloorUseLeftCousin mirrors the
// fill-floor regression test on the OTHER side: instead of targeting
// the leftmost intermediate branch (P, descentIdx=0, case-C uses the
// useLeft=false geometry), it targets the rightmost (R, descentIdx=2,
// case-C uses useLeft=true). The cousin step's `mergeOrRedistribute-
// Branches(Q, R_degenerate, sep_QR)` lands the deep child at a
// NON-leftmost position of the merge result (specifically at
// mergedID.cells[len(Q.cells)].Child) — the path the original
// leftmost-only spine walk missed.
// Pins the all-children scan + useLeft=true case-C cousin
// propagation.
func TestBtreeDeleteRangePreservesFillFloorUseLeftCousin(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)

	const mt uint8 = 10

	pw.nextID = 23

	padKey := func(s string) []byte {
		out := make([]byte, 100)
		copy(out, []byte(s))
		return out
	}
	val := bytes.Repeat([]byte{0xAB}, 50)

	buildLeaf := func(id uint64, keys []string) {
		t.Helper()
		buf, err := pw.ZeroPage(id)
		if err != nil {
			t.Fatalf("ZeroPage(%d): %v", id, err)
		}
		b := page.NewLeafBuilder(buf, cfg)
		for _, k := range keys {
			if !b.AddEntry(page.LeafEntry{Key: padKey(k), Value: val}) {
				t.Fatalf("leaf %d: AddEntry(%q) overflow", id, k)
			}
		}
		b.Finish()
	}

	// Same shape as TestBtreeDeleteRangePreservesFillFloor (3 inter-
	// mediate branches × 6 leaves × 3 entries) but with the cousin
	// cascade target = R (descentIdx=2 at ROOT level).
	for q := range 6 {
		buildLeaf(uint64(5+q), []string{
			fmt.Sprintf("p%c01", 'a'+q),
			fmt.Sprintf("p%c02", 'a'+q),
			fmt.Sprintf("p%c03", 'a'+q),
		})
		buildLeaf(uint64(11+q), []string{
			fmt.Sprintf("q%c01", 'a'+q),
			fmt.Sprintf("q%c02", 'a'+q),
			fmt.Sprintf("q%c03", 'a'+q),
		})
	}
	// R's leaves: keys "ra01..rf03" — the cousin-cascade target.
	for q := range 6 {
		buildLeaf(uint64(17+q), []string{
			fmt.Sprintf("r%c01", 'a'+q),
			fmt.Sprintf("r%c02", 'a'+q),
			fmt.Sprintf("r%c03", 'a'+q),
		})
	}

	buildBranch := func(id uint64, leftmost uint64, cells []page.BranchCell) {
		t.Helper()
		buf, err := pw.ZeroPage(id)
		if err != nil {
			t.Fatalf("ZeroPage(%d): %v", id, err)
		}
		if err := page.EncodeBranch(buf, cfg, leftmost, cells); err != nil {
			t.Fatalf("EncodeBranch(%d): %v", id, err)
		}
	}

	buildBranch(2, 5, []page.BranchCell{
		{Key: padKey("pb01"), Child: 6},
		{Key: padKey("pc01"), Child: 7},
		{Key: padKey("pd01"), Child: 8},
		{Key: padKey("pe01"), Child: 9},
		{Key: padKey("pf01"), Child: 10},
	})
	buildBranch(3, 11, []page.BranchCell{
		{Key: padKey("qb01"), Child: 12},
		{Key: padKey("qc01"), Child: 13},
		{Key: padKey("qd01"), Child: 14},
		{Key: padKey("qe01"), Child: 15},
		{Key: padKey("qf01"), Child: 16},
	})
	buildBranch(4, 17, []page.BranchCell{
		{Key: padKey("rb01"), Child: 18},
		{Key: padKey("rc01"), Child: 19},
		{Key: padKey("rd01"), Child: 20},
		{Key: padKey("re01"), Child: 21},
		{Key: padKey("rf01"), Child: 22},
	})
	buildBranch(1, 2, []page.BranchCell{
		{Key: padKey("qa01"), Child: 3},
		{Key: padKey("ra01"), Child: 4},
	})

	checkUnderflowInvariant(t, pw, cfg, 1, mt)

	// DeleteRange("ra02", "rf03") — within R's full key extent.
	// ROOT-level: leftIdx = BranchSearch("ra02") = 2 (descend into R);
	// rightIdx = BranchSearch("rf03") = 2 (also descend into R); single-
	// child case at ROOT. Recurse into R.
	// Inside R: leftIdx=0, rightIdx=5 → multi-child path, identical
	// shape to the original test but mirrored to R. R returns
	// `deepUnderflowChild = merged_AF_leaf`. ROOT's case-C runs with
	// descentIdx=2 → useLeft=true, siblingPos=1=Q. mergeOrRedistribute-
	// Branches(Q, R_degenerate) lands the deep leaf at the LAST cell of
	// mergedID — exactly the non-leftmost geometry the all-children
	// scan was added to catch.
	count, newRoot, err := DeleteRange(pw, cfg, 1, mt, padKey("ra02"), padKey("rf03"), plainCellFreeForTest)
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if count != 16 {
		t.Errorf("count = %d, want 16", count)
	}

	checkUnderflowInvariant(t, pw, cfg, newRoot, mt)
}
