package btree

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// The deep-underflow (cousin-cascade) signal must be handled on EVERY
// case-C pair outcome — merge, redistribute, and decline — and the
// redistribute outcome must locate which output actually received the
// deep child (the count-balanced split can land the right input's
// leftmost in the LEFT output). These drive patchBranchAfterChildDelete
// and rebalanceSurvivors directly with hand-built topologies, since the
// outcomes need fat-separator branch pairs a keyed workload cannot
// deterministically produce.

// healFixture allocates a page id and installs buf.
func installPage(pw *fakeWriter, buf []byte) uint64 {
	id := pw.nextID
	pw.nextID++
	pw.pages[id] = buf
	return id
}

func fatKeyN(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

func fatKey(b byte) []byte { return fatKeyN(b, 1200) }

func smallLeaf(t *testing.T, cfg page.Config, prefix byte, n, vlen int) []byte {
	t.Helper()
	var es []page.LeafEntry
	for i := range n {
		es = append(es, page.LeafEntry{
			Key:   fmt.Appendf(nil, "%c-%03d", prefix, i),
			Value: bytes.Repeat([]byte("v"), vlen),
		})
	}
	return makeLeaf(t, cfg, es)
}

// A case-C DECLINE with a deep in flight must thread the deep's
// wrapper upward (deepUnderflowChildOut = the recursed child) and
// force parent underflow — previously the signal was silently dropped
// when the wrapper's own encoded fill was healthy.
func TestPatchBranchDeclineThreadsDeepWrapper(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false}
	pw := newFakeWriter(t, 4096)
	pw.nextID = 1

	// Leaves (healthy except D, the sub-MT deep).
	c0 := installPage(pw, smallLeaf(t, cfg, 'a', 12, 100))
	c1 := installPage(pw, smallLeaf(t, cfg, 'c', 12, 100))
	l0 := installPage(pw, smallLeaf(t, cfg, 'e', 12, 100))
	m0 := installPage(pw, smallLeaf(t, cfg, 'g', 12, 100))
	d := installPage(pw, smallLeaf(t, cfg, 'i', 1, 8)) // deep: far below floor

	// S: [c0, fat(b)·1700, c1]. W: [l0, fat(f)·1700, m0, fat(h)·1700, D]
	// — healthy logical fill, carries D as a direct child. Pair
	// separator in P is the SHORT "d".
	//
	// Case-C pair (S, W): combined = [fat(b), "d"→l0, fat(f), fat(h)]
	// ≈ 5.1 KB → merge fails. The balance-optimal lift is fat(f)
	// (lifting "d" leaves a 1700-vs-3400 logical imbalance), and P —
	// stuffed with a 2400-byte filler separator — cannot swap "d" for
	// a 1700-byte lift: parentFits fails → DECLINE.
	s := installPage(pw, makeBranch(t, cfg, c0, []page.BranchCell{{Key: fatKeyN('b', 1700), Child: c1}}))
	w := installPage(pw, makeBranch(t, cfg, l0, []page.BranchCell{
		{Key: fatKeyN('f', 1700), Child: m0},
		{Key: fatKeyN('h', 1700), Child: d},
	}))
	x0 := installPage(pw, smallLeaf(t, cfg, 'A', 12, 100))
	xMid := installPage(pw, makeLeaf(t, cfg, func() []page.LeafEntry {
		var es []page.LeafEntry
		for i := range 12 {
			es = append(es, page.LeafEntry{Key: fmt.Appendf(nil, "_z-%03d", i), Value: bytes.Repeat([]byte("v"), 100)})
		}
		return es
	}()))
	// Two in-spec filler separators (limits.md caps keys ≈ 2028) fill P
	// to the same ~2.4 KB the old single out-of-spec filler did.
	p := installPage(pw, makeBranch(t, cfg, x0, []page.BranchCell{
		{Key: bytes.Repeat([]byte{0x5F}, 1300), Child: xMid}, // '_' < '`'
		{Key: bytes.Repeat([]byte{0x60}, 1100), Child: s},    // '`' < 'a'
		{Key: []byte("d"), Child: w},
	}))

	newID, underflow, deepOut, err := patchBranchAfterChildDelete(
		pw, cfg, DefaultMergeThreshold, p, 3 /* descent = W's position */, w,
		true /* childUnderflow (semantic) */, d /* deepUnderflowChildIn */)
	if err != nil {
		t.Fatalf("patchBranchAfterChildDelete: %v", err)
	}
	if deepOut != w {
		t.Fatalf("deepUnderflowChildOut = %d, want the wrapper %d (signal must not be dropped on decline)", deepOut, w)
	}
	if !underflow {
		t.Fatal("parent underflow not forced while a deep signal is in flight")
	}
	// Decline changed nothing structurally: W still P's child at
	// position 3, D still W's direct child.
	buf, _ := pw.Page(newID)
	if got := page.BranchChildAt(buf, cfg, 3); got != w {
		t.Fatalf("child 3 = %d, want unchanged wrapper %d", got, w)
	}
	wbuf, _ := pw.Page(w)
	if got := page.BranchChildAt(wbuf, cfg, 2); got != d {
		t.Fatalf("wrapper child 2 = %d, want deep %d", got, d)
	}
}

// A case-C REDISTRIBUTE with a deep in flight must heal the deep
// inside whichever output received it. The deep (the right pair
// member's leftmost) is merged with its new adjacent leaf sibling;
// afterwards no leaf under the returned branch is below the floor.
// Previously the redistribute outcome skipped the cousin pass
// entirely, leaving the sub-MT deep in place.
func TestPatchBranchRedistributeHealsDeep(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false}
	pw := newFakeWriter(t, 4096)
	pw.nextID = 1

	c0 := installPage(pw, smallLeaf(t, cfg, 'a', 12, 100))
	c1 := installPage(pw, smallLeaf(t, cfg, 'c', 12, 100))
	c2 := installPage(pw, smallLeaf(t, cfg, 'e', 12, 100))
	c3 := installPage(pw, smallLeaf(t, cfg, 'g', 12, 100))
	d := installPage(pw, smallLeaf(t, cfg, 'i', 1, 8)) // deep

	// S: [c0, fat(b), c1, fat(d), c2, fat(f), c3] — three fat cells.
	s := installPage(pw, makeBranch(t, cfg, c0, []page.BranchCell{
		{Key: fatKey('b'), Child: c1},
		{Key: fatKey('d'), Child: c2},
		{Key: fatKey('f'), Child: c3},
	}))
	// W: degenerate wrapper — 0 cells, leftmost = D.
	w := installPage(pw, makeBranch(t, cfg, d, nil))
	// P: [S, fat(h), W]. Pair (S, W) combined = 4 fat cells > one page
	// → merge fails → redistribute (all-fat halves clear the logical
	// floor; P has one cell so parentFits holds).
	p := installPage(pw, makeBranch(t, cfg, s, []page.BranchCell{{Key: fatKey('h'), Child: w}}))

	newID, _, deepOut, err := patchBranchAfterChildDelete(
		pw, cfg, DefaultMergeThreshold, p, 1 /* descent = W */, w,
		true, d)
	if err != nil {
		t.Fatalf("patchBranchAfterChildDelete: %v", err)
	}
	if deepOut != 0 {
		t.Fatalf("deepUnderflowChildOut = %d, want 0 (deep healed in place)", deepOut)
	}
	// No leaf under the result may be below the floor, and every key
	// (incl. D's) must remain reachable in order.
	assertNoLeafBelowFloor(t, pw, cfg, newID)
	assertKeyPresent(t, pw, cfg, newID, []byte("i-000"))
}

// rebalanceSurvivors' redistribute outcome must SCAN for the output
// holding the deep: with a left-heavy combined set the right input's
// leftmost lands in the LEFT output, where the old per-side assumption
// called cousinRebalanceBranch on the wrong page and failed a valid
// delete with ErrCorrupted.
func TestRebalanceSurvivorsDeepHolderScan(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false}
	pw := newFakeWriter(t, 4096)
	pw.nextID = 1

	la := installPage(pw, smallLeaf(t, cfg, 'a', 12, 100))
	lb := installPage(pw, smallLeaf(t, cfg, 'c', 12, 100))
	d := installPage(pw, smallLeaf(t, cfg, 'e', 1, 8)) // deep
	rc := installPage(pw, smallLeaf(t, cfg, 'g', 12, 100))
	rd := installPage(pw, smallLeaf(t, cfg, 'i', 12, 100))
	re := installPage(pw, smallLeaf(t, cfg, 'k', 12, 100))

	// LB: [la, fat(b), lb] — one fat cell. RB: [D, fat(f), rc, fat(h),
	// rd, fat(j), re] — leftmost is the deep, three fat cells.
	lbr := installPage(pw, makeBranch(t, cfg, la, []page.BranchCell{{Key: fatKey('b'), Child: lb}}))
	rbr := installPage(pw, makeBranch(t, cfg, d, []page.BranchCell{
		{Key: fatKey('f'), Child: rc},
		{Key: fatKey('h'), Child: rd},
		{Key: fatKey('j'), Child: re},
	}))

	// Combined = [fat(b), sepX(fat d)→D, fat(f), fat(h), fat(j)]: five
	// fat cells; the balanced lift puts D's cell in the LEFT output.
	survivors := []slot{
		{origIdx: 0, child: lbr},
		{origIdx: 1, child: rbr, underflow: true, deepUnderflow: d},
	}
	origCellKeys := [][]byte{fatKey('d')}
	if err := rebalanceSurvivors(pw, cfg, DefaultMergeThreshold, origCellKeys, &survivors); err != nil {
		t.Fatalf("rebalanceSurvivors: %v", err)
	}
	// Fixture guard: this test pins the holder SCAN, so the deep must
	// have landed in the LEFT output (where the old per-side
	// assumption looked in the right one and failed). If the split
	// heuristic drifts and the deep lands right, the test would pass
	// under the assumption too — fail loudly instead. After the heal
	// the deep merged into a sibling, so we assert indirectly: the old
	// assumption's failure mode (ErrCorrupted above) plus the deep's
	// key surviving under the LEFT slot.
	if !keyPresent(t, pw, cfg, survivors[0].child, []byte("e-000")) {
		t.Fatal("fixture drifted: deep no longer lands in the left output — the holder scan is unpinned")
	}
	for _, sl := range survivors {
		assertNoLeafBelowFloor(t, pw, cfg, sl.child)
	}
	// D's key survived the heal.
	found := false
	for _, sl := range survivors {
		if keyPresent(t, pw, cfg, sl.child, []byte("e-000")) {
			found = true
		}
	}
	if !found {
		t.Fatal("deep leaf's key lost during heal")
	}
}

// assertNoLeafBelowFloor walks the subtree and fails on any non-root
// leaf strictly below MergeThreshold.
func assertNoLeafBelowFloor(t *testing.T, pw PageReader, cfg page.Config, root uint64) {
	t.Helper()
	if err := Walk(pw, cfg, root, ^uint64(0), func(id uint64, kind PageKind, depth int) error {
		if kind != PageKindLeaf || id == root {
			return nil
		}
		buf, err := pw.Page(id)
		if err != nil {
			return err
		}
		if leafUnderflow(buf, cfg, DefaultMergeThreshold) {
			return fmt.Errorf("leaf %d below floor after heal", id)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func keyPresent(t *testing.T, pw PageReader, cfg page.Config, root uint64, key []byte) bool {
	t.Helper()
	_, ok, err := Get(pw, cfg, root, key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	return ok
}

func assertKeyPresent(t *testing.T, pw PageReader, cfg page.Config, root uint64, key []byte) {
	t.Helper()
	if !keyPresent(t, pw, cfg, root, key) {
		t.Fatalf("key %q not found after heal", key)
	}
}

// The deep's threaded wrapper id must be LIVE in the returned
// topology: after a case-C decline records the wrapper, the post-
// decline re-rebalance loop can merge that wrapper into the OTHER
// sibling (freeing it). The reconciliation must detect the stale id,
// find the merge product that now holds the deep, and heal there —
// previously the freed id was threaded upward, failing the next
// level's cousin pass on a valid delete.
func TestPatchBranchDeclineThenMergeReconcilesDeep(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false}
	pw := newFakeWriter(t, 4096)
	pw.nextID = 1

	x0 := installPage(pw, smallLeaf(t, cfg, 'A', 12, 100))
	c0 := installPage(pw, smallLeaf(t, cfg, 'a', 12, 100))
	c1 := installPage(pw, makeLeaf(t, cfg, []page.LeafEntry{{Key: []byte("bz-000"), Value: bytes.Repeat([]byte("v"), 100)}, {Key: []byte("bz-001"), Value: bytes.Repeat([]byte("v"), 100)}, {Key: []byte("bz-002"), Value: bytes.Repeat([]byte("v"), 800)}, {Key: []byte("bz-003"), Value: bytes.Repeat([]byte("v"), 100)}}))
	c2 := installPage(pw, makeLeaf(t, cfg, []page.LeafEntry{{Key: []byte("cz-000"), Value: bytes.Repeat([]byte("v"), 800)}, {Key: []byte("cz-001"), Value: bytes.Repeat([]byte("v"), 300)}}))
	d := installPage(pw, smallLeaf(t, cfg, 'f', 1, 8)) // deep
	s0 := installPage(pw, smallLeaf(t, cfg, 'h', 12, 100))
	s1x := installPage(pw, makeLeaf(t, cfg, []page.LeafEntry{{Key: []byte("iz-000"), Value: bytes.Repeat([]byte("v"), 500)}, {Key: []byte("iz-001"), Value: bytes.Repeat([]byte("v"), 600)}}))

	// S1: two 1500-byte fat cells. Pair (S1, W): combined =
	// [1500(b), 1500(c), 1400(e)→D] > one page → merge fails; the
	// balance-optimal lift is the 1500(c) boundary — and P, stuffed
	// with two in-spec fillers totalling 2.6 KB, cannot swap its
	// 1400(e) separator for a 1500 lift: parentFits fails → DECLINE. The loop then merges the
	// below-floor wrapper W into S2 (combined ≈ 1.5 KB fits), freeing
	// W while the merge result is healthy — the stale-thread window.
	s1b := installPage(pw, makeBranch(t, cfg, c0, []page.BranchCell{
		{Key: fatKeyN('b', 1500), Child: c1},
		{Key: fatKeyN('c', 1500), Child: c2},
	}))
	w := installPage(pw, makeBranch(t, cfg, d, nil))
	s2b := installPage(pw, makeBranch(t, cfg, s0, []page.BranchCell{{Key: fatKeyN('i', 1500), Child: s1x}}))
	xMid2 := installPage(pw, makeLeaf(t, cfg, func() []page.LeafEntry {
		var es []page.LeafEntry
		for i := range 12 {
			es = append(es, page.LeafEntry{Key: fmt.Appendf(nil, "_z-%03d", i), Value: bytes.Repeat([]byte("v"), 100)})
		}
		return es
	}()))
	// Two in-spec fillers replace the old single out-of-spec 2600-byte
	// separator; P stays too full to swap its 1400(e) for a 1500 lift.
	p := installPage(pw, makeBranch(t, cfg, x0, []page.BranchCell{
		{Key: bytes.Repeat([]byte{0x5F}, 1300), Child: xMid2}, // '_' < '`'
		{Key: bytes.Repeat([]byte{0x60}, 1300), Child: s1b},   // '`' < 'a'
		{Key: fatKeyN('e', 1400), Child: w},
		{Key: []byte("g"), Child: s2b},
	}))

	newID, _, deepOut, err := patchBranchAfterChildDelete(
		pw, cfg, DefaultMergeThreshold, p, 3 /* descent = W */, w,
		true, d)
	if err != nil {
		t.Fatalf("patchBranchAfterChildDelete: %v", err)
	}
	if deepOut != 0 {
		if _, freed := pw.freed[deepOut]; freed {
			t.Fatalf("deepUnderflowChildOut = %d is a FREED page (stale wrapper threaded)", deepOut)
		}
		t.Fatalf("deepUnderflowChildOut = %d, want 0 (deep healed after wrapper merge)", deepOut)
	}
	assertNoLeafBelowFloor(t, pw, cfg, newID)
	assertKeyPresent(t, pw, cfg, newID, []byte("f-000"))
}

// Two deeps on one redistributed pair: healing the first can merge the
// second deep into a sibling (both can land adjacent in one output).
// The second scan's miss is then legitimate (already healed by
// absorption) — previously it errored `deep underflow child not
// found`, failing a valid range delete.
func TestRebalanceSurvivorsTwoDeepsAbsorbed(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false}
	pw := newFakeWriter(t, 4096)
	pw.nextID = 1

	la := installPage(pw, smallLeaf(t, cfg, 'a', 12, 100))
	// Near-full single-huge-value leaf: its pairing with dL must
	// merge-overflow and floor-decline, pushing dL to merge with dR.
	lbLeaf := installPage(pw, makeLeaf(t, cfg, []page.LeafEntry{
		{Key: []byte("c-huge"), Value: bytes.Repeat([]byte("H"), 3900)},
	}))
	dL := installPage(pw, makeLeaf(t, cfg, []page.LeafEntry{
		{Key: []byte("e-000"), Value: bytes.Repeat([]byte("v"), 300)},
	}))
	dR := installPage(pw, makeLeaf(t, cfg, []page.LeafEntry{
		{Key: []byte("g-000"), Value: bytes.Repeat([]byte("v"), 300)},
	}))
	x1 := installPage(pw, smallLeaf(t, cfg, 'i', 12, 100))
	x2 := installPage(pw, smallLeaf(t, cfg, 'k', 12, 100))
	x3 := installPage(pw, smallLeaf(t, cfg, 'm', 12, 100))

	lbr := installPage(pw, makeBranch(t, cfg, la, []page.BranchCell{
		{Key: fatKeyN('b', 600), Child: lbLeaf},
		{Key: fatKeyN('d', 600), Child: dL},
	}))
	rbr := installPage(pw, makeBranch(t, cfg, dR, []page.BranchCell{
		{Key: fatKeyN('h', 1200), Child: x1},
		{Key: fatKeyN('j', 1200), Child: x2},
		{Key: fatKeyN('l', 1200), Child: x3},
	}))

	// Both slots carry deeps with forced underflow — the shape every
	// producer emits (a deep always forces its wrapper's underflow).
	survivors := []slot{
		{origIdx: 0, child: lbr, underflow: true, deepUnderflow: dL},
		{origIdx: 1, child: rbr, underflow: true, deepUnderflow: dR},
	}
	origCellKeys := [][]byte{fatKeyN('f', 600)}
	if err := rebalanceSurvivors(pw, cfg, DefaultMergeThreshold, origCellKeys, &survivors); err != nil {
		t.Fatalf("rebalanceSurvivors: %v", err)
	}
	// Absorption guard: the scenario is only pinned if the first heal
	// really consumed the second deep (dR freed by merging into dL's
	// heal) — otherwise the per-side assumption would also pass.
	if _, freed := pw.freed[dR]; !freed {
		t.Fatal("fixture drifted: dR was not absorbed by the first heal — the tolerance gate is unpinned")
	}
	// Both deeps' keys survive under the healed slots.
	for _, key := range []string{"e-000", "g-000"} {
		found := false
		for _, sl := range survivors {
			if keyPresent(t, pw, cfg, sl.child, []byte(key)) {
				found = true
			}
		}
		if !found {
			t.Fatalf("key %q lost during two-deep heal", key)
		}
	}
}

// The stale-thread rescue must survive a PARTIAL heal: the
// redistribute arm's cousin pass can merge the original deep away
// (freed) yet exit with a residual wrapper, and the loop can then
// merge that wrapper away too. Chasing ids fails (both are dead);
// the reconciliation instead rescans the final topology for any
// below-floor grandchild and heals/threads there — never an error.
// Previously: `ErrCorrupted: deep underflow child N unreachable
// after re-rebalance` on a valid delete.
func TestPatchBranchPartialHealStaleThreadRescued(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false}
	pw := newFakeWriter(t, 4096)
	pw.nextID = 1

	x0 := installPage(pw, smallLeaf(t, cfg, 'A', 12, 100))
	xs := installPage(pw, smallLeaf(t, cfg, 'b', 12, 100))
	x1 := installPage(pw, makeLeaf(t, cfg, []page.LeafEntry{{Key: []byte("cz-000"), Value: bytes.Repeat([]byte("v"), 200)}, {Key: []byte("cz-001"), Value: bytes.Repeat([]byte("v"), 900)}}))
	x2huge := installPage(pw, makeLeaf(t, cfg, []page.LeafEntry{{Key: []byte("ez-000"), Value: bytes.Repeat([]byte("H"), 3900)}}))
	tt := installPage(pw, makeLeaf(t, cfg, []page.LeafEntry{{Key: []byte("h-000"), Value: bytes.Repeat([]byte("v"), 300)}}))
	d := installPage(pw, makeLeaf(t, cfg, []page.LeafEntry{{Key: []byte("iz-000"), Value: bytes.Repeat([]byte("v"), 300)}}))
	f1 := installPage(pw, makeLeaf(t, cfg, []page.LeafEntry{{Key: []byte("l-000"), Value: bytes.Repeat([]byte("H"), 3900)}}))

	s := installPage(pw, makeBranch(t, cfg, xs, []page.BranchCell{
		{Key: fatKeyN('c', 1550), Child: x1},
		{Key: fatKeyN('e', 1550), Child: x2huge},
	}))
	w := installPage(pw, makeBranch(t, cfg, tt, []page.BranchCell{
		{Key: fatKeyN('i', 1050), Child: d},
		{Key: []byte("k"), Child: f1},
	}))
	p := installPage(pw, makeBranch(t, cfg, x0, []page.BranchCell{
		{Key: []byte("a"), Child: s},
		{Key: []byte("g"), Child: w},
	}))

	newID, _, deepOut, err := patchBranchAfterChildDelete(
		pw, cfg, DefaultMergeThreshold, p, 2 /* descent = W */, w,
		true, d)
	if err != nil {
		t.Fatalf("patchBranchAfterChildDelete: %v (stale thread must be rescued, not an error)", err)
	}
	if deepOut != 0 {
		if _, freed := pw.freed[deepOut]; freed {
			t.Fatalf("deepUnderflowChildOut = %d is a FREED page", deepOut)
		}
	}
	// Staleness guards: this test pins the rescue only while the flow
	// really frees both the original deep (partial heal) and its
	// wrapper (loop merge) — fail loudly if the topology drifts.
	if _, freed := pw.freed[d]; !freed {
		t.Fatal("fixture drifted: original deep no longer consumed by the partial heal")
	}
	if _, freed := pw.freed[w]; !freed {
		t.Fatal("fixture drifted: wrapper no longer merged away by the loop")
	}
	// Every key survives, including the twice-merged deep's.
	for _, k := range []string{"h-000", "iz-000", "l-000", "ez-000", "cz-000"} {
		assertKeyPresent(t, pw, cfg, newID, []byte(k))
	}
}
