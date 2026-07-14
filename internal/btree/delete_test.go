package btree

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// fakeWriter and helpers live in put_test.go; reused here.

func TestDeleteOnEmptyTreeReturnsNotFound(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root, err := Delete(pw, cfg, 0, DefaultMergeThreshold, []byte("k"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete on empty tree: err = %v, want ErrNotFound", err)
	}
	if root != 0 {
		t.Errorf("Delete on empty tree: root = %d, want 0", root)
	}
}

func TestDeleteMissingKeyReturnsNotFoundUnchanged(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root, err := Put(pw, cfg, 0, []byte("a"), []byte("A"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	pagesBefore := len(pw.pages)
	freedBefore := len(pw.freed)
	got, err := Delete(pw, cfg, root, DefaultMergeThreshold, []byte("z"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing: err = %v, want ErrNotFound", err)
	}
	if got != root {
		t.Errorf("Delete missing: returned root = %d, want unchanged %d", got, root)
	}
	if len(pw.pages) != pagesBefore || len(pw.freed) != freedBefore {
		t.Errorf("Delete missing key allocated/freed pages: pages %d→%d freed %d→%d",
			pagesBefore, len(pw.pages), freedBefore, len(pw.freed))
	}
}

func TestDeleteSingleLeafEntryEmptiesTree(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root, err := Put(pw, cfg, 0, []byte("k"), []byte("v"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	newRoot, err := Delete(pw, cfg, root, DefaultMergeThreshold, []byte("k"))
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if newRoot != 0 {
		t.Errorf("Delete last entry: root = %d, want 0 (empty tree)", newRoot)
	}
	if _, ok := pw.freed[root]; !ok {
		t.Errorf("Delete last entry: old root %d not freed", root)
	}
}

func TestDeleteFromMultiEntryLeaf(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	pairs := []struct{ k, v string }{
		{"alpha", "A"}, {"beta", "B"}, {"gamma", "G"}, {"delta", "D"},
	}
	for _, p := range pairs {
		nr, err := Put(pw, cfg, root, []byte(p.k), []byte(p.v))
		if err != nil {
			t.Fatalf("Put(%q): %v", p.k, err)
		}
		root = nr
	}
	// Delete one entry.
	newRoot, err := Delete(pw, cfg, root, DefaultMergeThreshold, []byte("beta"))
	if err != nil {
		t.Fatalf("Delete(beta): %v", err)
	}
	// Missing key surfaces correctly.
	if _, found, err := Get(pw, cfg, newRoot, []byte("beta")); err != nil || found {
		t.Errorf("Get(beta) after delete: found=%v err=%v; want (false, nil)", found, err)
	}
	// Others still present with original values.
	for _, p := range pairs {
		if p.k == "beta" {
			continue
		}
		v, found, err := Get(pw, cfg, newRoot, []byte(p.k))
		if err != nil || !found {
			t.Errorf("Get(%q) after delete: found=%v err=%v", p.k, found, err)
			continue
		}
		if !bytes.Equal(v, []byte(p.v)) {
			t.Errorf("Get(%q) after delete: value=%q want=%q", p.k, v, p.v)
		}
	}
}

func TestDeleteCausesRootCollapseAfterLeafMerge(t *testing.T) {
	// Put enough keys to force a root branch with two leaves, then
	// delete keys until merge fires, collapsing the root branch back
	// to a single leaf. Threshold 50% guarantees merge fires on a
	// 4 KB page once one leaf drops below ~2 KB.
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	const valSize = 1024
	const N = 6 // ~6 KB worth of entries → split → 2 leaves + branch root
	for i := range N {
		key := fmt.Appendf(nil, "k-%02d", i)
		val := bytes.Repeat([]byte{byte('a' + i)}, valSize)
		nr, err := Put(pw, cfg, root, key, val)
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
		root = nr
	}
	// Confirm initial topology: root is a branch.
	rootBuf, _ := pw.Page(root)
	typ, _, _, _ := page.ReadHeader(rootBuf)
	if typ != page.TypeBranch {
		t.Fatalf("setup: root type = %d, want TypeBranch", typ)
	}

	// Delete enough keys to trigger merge + root collapse. With
	// threshold 50%, removing 3 of 6 brings each leaf well below
	// threshold, forcing merge to fire and the root branch to
	// collapse to a single leaf.
	keysToDelete := []string{"k-00", "k-02", "k-04"}
	for _, k := range keysToDelete {
		nr, err := Delete(pw, cfg, root, 50, []byte(k))
		if err != nil {
			t.Fatalf("Delete(%q): %v", k, err)
		}
		root = nr
	}

	// Root should now be a single leaf (collapse fired).
	rootBuf, _ = pw.Page(root)
	typ, _, _, _ = page.ReadHeader(rootBuf)
	if !page.IsLeafType(typ) {
		t.Errorf("after merge+collapse: root type = %d, want leaf variant (root branch should have collapsed)", typ)
	}

	// Surviving keys retrievable.
	survivors := []string{"k-01", "k-03", "k-05"}
	for _, k := range survivors {
		_, found, err := Get(pw, cfg, root, []byte(k))
		if err != nil || !found {
			t.Errorf("Get(%q) after collapse: found=%v err=%v", k, found, err)
		}
	}
	// Deleted keys missing.
	for _, k := range keysToDelete {
		_, found, err := Get(pw, cfg, root, []byte(k))
		if err != nil || found {
			t.Errorf("Get(%q) after collapse: found=%v err=%v; want missing", k, found, err)
		}
	}
}

func TestDeleteRedistributesWhenSiblingFull(t *testing.T) {
	// Force a topology where one leaf can spare entries to a sibling
	// that would otherwise underflow. Use a large key/value ratio so
	// the per-leaf entry count is small and predictable, and a high
	// threshold (50%) so a single delete triggers underflow.
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	// 12 keys with 500B values → ~6 KB / page. Splits into 2 leaves
	// of 3 entries each at minimum.
	const N = 12
	for i := range N {
		key := fmt.Appendf(nil, "k-%03d", i)
		val := bytes.Repeat([]byte{byte('a' + i%26)}, 500)
		nr, err := Put(pw, cfg, root, key, val)
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
		root = nr
	}
	// Track depth and reachable pages before delete.
	depthBefore := treeDepth(t, pw, root)

	// Delete one entry. With threshold 50% on a heavily-loaded tree,
	// some delete will likely trigger redistribute or merge; check
	// that all OTHER keys remain after a sequence of deletes.
	keysDeleted := map[string]bool{}
	for _, i := range []int{1, 3, 5, 7, 9, 11} {
		key := fmt.Appendf(nil, "k-%03d", i)
		nr, err := Delete(pw, cfg, root, 50, key)
		if err != nil {
			t.Fatalf("Delete(%d): %v", i, err)
		}
		root = nr
		keysDeleted[string(key)] = true
	}

	// Walk: every surviving key still retrievable.
	for i := range N {
		key := fmt.Appendf(nil, "k-%03d", i)
		_, found, err := Get(pw, cfg, root, key)
		want := !keysDeleted[string(key)]
		if err != nil {
			t.Errorf("Get(%q): err %v", key, err)
			continue
		}
		if found != want {
			t.Errorf("Get(%q): found = %v, want %v", key, found, want)
		}
	}
	depthAfter := treeDepth(t, pw, root)
	if depthAfter > depthBefore {
		t.Errorf("depth grew on delete: %d → %d", depthBefore, depthAfter)
	}
}

func TestDeleteCascadesBranchMergeAndRootCollapse(t *testing.T) {
	// Build a multi-level tree, then delete enough to cascade a
	// merge from the leaf level up through one or more branch levels.
	// Pin: (a) all leaves at the same depth post-delete; (b) every
	// non-root page above threshold; (c) every key retrievable.
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16} // depth calibration: N=1500 → ~375 leaves at target 16
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	// Long shared-prefix keys (~57B) and large values (512B) to force a
	// depth-2+ tree (same shape as TestPutForcesMultiLevelTreeAndBranchSplit).
	// N=1500: within-page branch prefix truncation packs ~260 shared-prefix
	// separators per branch, so a multi-level branch structure needs ~375
	// leaves' worth of keys (the prior 400 fit one compressed root branch).
	const N = 1500
	keyPrefix := bytes.Repeat([]byte("k"), 50)
	keys := make([][]byte, N)
	for i := range N {
		keys[i] = fmt.Appendf(nil, "%s-%05d", keyPrefix, i)
		val := bytes.Repeat([]byte{byte('a' + i%26)}, 512)
		nr, err := Put(pw, cfg, root, keys[i], val)
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
		root = nr
	}
	initialDepth := treeDepth(t, pw, root)
	if initialDepth < 2 {
		t.Fatalf("setup: depth = %d, want ≥ 2 for cascade test", initialDepth)
	}

	// Delete roughly half the keys; with default threshold (25%),
	// some merges will fire and at least one branch-level cascade
	// should occur. The test doesn't require a SPECIFIC topology
	// change — it just pins the invariants.
	rng := rand.New(rand.NewPCG(1, 2))
	deleteOrder := make([]int, N)
	for i := range N {
		deleteOrder[i] = i
	}
	rng.Shuffle(N, func(i, j int) { deleteOrder[i], deleteOrder[j] = deleteOrder[j], deleteOrder[i] })

	deleted := make(map[int]bool)
	for _, i := range deleteOrder[:N/2] {
		nr, err := Delete(pw, cfg, root, DefaultMergeThreshold, keys[i])
		if err != nil {
			t.Fatalf("Delete(%d): %v", i, err)
		}
		root = nr
		deleted[i] = true
		// Per-delete depth-uniformity check — every leaf at same depth.
		checkBalance(t, pw, cfg, root)
	}

	// Every remaining key retrievable; every deleted key missing.
	for i := range N {
		_, found, err := Get(pw, cfg, root, keys[i])
		if err != nil {
			t.Errorf("Get(%d): err %v", i, err)
			continue
		}
		want := !deleted[i]
		if found != want {
			t.Errorf("Get(%d): found=%v want=%v", i, found, want)
		}
	}

	// Underflow invariant: every non-root page ≥ threshold.
	checkUnderflowInvariant(t, pw, cfg, root, DefaultMergeThreshold)
	// Slab-leak invariant: every allocated page reachable or freed.
	checkSlabPartition(t, pw, cfg, root)
}

func TestDeleteForcesBranchMergeAndRedistribute(t *testing.T) {
	// Specifically exercise the branch-level merge/redistribute path
	// (mergeOrRedistributeBranches). Build a depth-2+ tree, then
	// delete enough that branches at depth 1 fall below threshold
	// and merge with each other — invariants checked per delete.
	// N=1500: with within-page branch prefix truncation, ~260 shared-prefix
	// separators fit one branch, so a multi-level tree needs ~375 leaves.
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16} // depth calibration: N=1500 → ~375 leaves at target 16
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	const N = 1500
	keyPrefix := bytes.Repeat([]byte("k"), 50)
	keys := make([][]byte, N)
	for i := range N {
		keys[i] = fmt.Appendf(nil, "%s-%05d", keyPrefix, i)
		val := bytes.Repeat([]byte{byte('a' + i%26)}, 512)
		nr, err := Put(pw, cfg, root, keys[i], val)
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
		root = nr
	}
	if treeDepth(t, pw, root) < 2 {
		t.Fatalf("setup: depth < 2; can't exercise branch merge")
	}

	// Delete 90% of keys. With default threshold, this guarantees
	// branch-level cascades all the way to the root.
	rng := rand.New(rand.NewPCG(7, 11))
	order := make([]int, N)
	for i := range N {
		order[i] = i
	}
	rng.Shuffle(N, func(i, j int) { order[i], order[j] = order[j], order[i] })

	deleted := make(map[int]bool)
	for _, i := range order[:int(N*9/10)] {
		nr, err := Delete(pw, cfg, root, DefaultMergeThreshold, keys[i])
		if err != nil {
			t.Fatalf("Delete(%d): %v", i, err)
		}
		root = nr
		deleted[i] = true
		checkBalance(t, pw, cfg, root)
	}

	// Surviving keys retrievable; deleted keys missing.
	for i := range N {
		_, found, err := Get(pw, cfg, root, keys[i])
		if err != nil {
			t.Errorf("Get(%d): err %v", i, err)
			continue
		}
		want := !deleted[i]
		if found != want {
			t.Errorf("Get(%d): found=%v want=%v", i, found, want)
		}
	}
	checkUnderflowInvariant(t, pw, cfg, root, DefaultMergeThreshold)
	checkSlabPartition(t, pw, cfg, root)
}

func TestDeleteRejectsBadMergeThreshold(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	// rootID=0 ensures the threshold validation fires before any
	// pw.Page resolution — the validation must reject independently
	// of tree state.
	for _, bad := range []uint8{0, 51, 100, 255} {
		_, err := Delete(pw, cfg, 0, bad, []byte("k"))
		if err == nil {
			t.Errorf("Delete with mergeThreshold=%d: expected error", bad)
		}
		if errors.Is(err, ErrNotFound) {
			t.Errorf("Delete with mergeThreshold=%d returned ErrNotFound; the threshold check should fire first", bad)
		}
	}
}

func TestDeleteThenPutRoundTrips(t *testing.T) {
	// Stress: alternating insert/delete cycles. After many rounds,
	// the surviving set is correct. Pins absence of subtle aliasing
	// or off-by-one in the merge/split interleaving.
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	const N = 100
	want := make(map[string]string)
	rng := rand.New(rand.NewPCG(42, 1337))
	for range N {
		key := fmt.Appendf(nil, "k-%05d", rng.IntN(50))
		switch rng.IntN(3) {
		case 0, 1:
			val := fmt.Appendf(nil, "v-%d", rng.Int())
			nr, err := Put(pw, cfg, root, key, val)
			if err != nil {
				t.Fatalf("Put(%q): %v", key, err)
			}
			root = nr
			want[string(key)] = string(val)
		case 2:
			nr, err := Delete(pw, cfg, root, DefaultMergeThreshold, key)
			if err != nil && !errors.Is(err, ErrNotFound) {
				t.Fatalf("Delete(%q): %v", key, err)
			}
			if err == nil {
				root = nr
				delete(want, string(key))
			}
		}
	}
	for k, v := range want {
		got, found, err := Get(pw, cfg, root, []byte(k))
		if err != nil || !found {
			t.Errorf("Get(%q): found=%v err=%v", k, found, err)
			continue
		}
		if !bytes.Equal(got, []byte(v)) {
			t.Errorf("Get(%q): got=%q want=%q", k, got, v)
		}
	}
	// Slab partition: every allocated page reachable-or-freed.
	checkSlabPartition(t, pw, cfg, root)
}

func TestDeleteFreesEveryRetiredPage(t *testing.T) {
	// The slab-partition invariant restated for delete-heavy
	// workloads. Mirrors TestPutFreesEveryRetiredPageAfterSplits.
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	const N = 200
	for i := range N {
		key := fmt.Appendf(nil, "k-%05d", i)
		val := bytes.Repeat([]byte{byte('a' + i%26)}, 200)
		nr, err := Put(pw, cfg, root, key, val)
		if err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		root = nr
	}
	// Delete every other key.
	for i := 0; i < N; i += 2 {
		key := fmt.Appendf(nil, "k-%05d", i)
		nr, err := Delete(pw, cfg, root, DefaultMergeThreshold, key)
		if err != nil {
			t.Fatalf("Delete %d: %v", i, err)
		}
		root = nr
	}
	checkSlabPartition(t, pw, cfg, root)
}

func TestDeleteOverflowEntryFreesChain(t *testing.T) {
	// Contract: Delete of an overflow-flagged leaf entry
	// frees the associated chain in the same write tx. Pinned via
	// the slab-partition invariant (checkSlabPartition walks
	// overflow chains as reachable). Both shapes covered:
	//   (a) Delete that empties the leaf (newID=0 path) — the
	//       chain must still be freed.
	//   (b) Delete that leaves the leaf non-empty (rebuild path).
	cfg := page.Config{PageSize: 4096}
	big := bytes.Repeat([]byte{'x'}, 8000)

	// (a) Single-entry overflow tree → delete empties it.
	pw := newFakeWriter(t, 4096)
	root, err := Put(pw, cfg, 0, []byte("k"), big)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	newRoot, err := Delete(pw, cfg, root, DefaultMergeThreshold, []byte("k"))
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if newRoot != 0 {
		t.Errorf("Delete last entry: root = %d, want 0 (empty tree)", newRoot)
	}
	// Slab partition with rootID=0: every page is freed (no
	// reachable set). The orphan-chain failure mode would surface
	// as "allocated but neither reachable nor freed."
	checkSlabPartition(t, pw, cfg, newRoot)

	// (b) Multi-entry leaf with one overflow entry → delete that
	// entry; the leaf survives, the chain is freed.
	pw2 := newFakeWriter(t, 4096)
	root, err = Put(pw2, cfg, 0, []byte("inline"), []byte("v"))
	if err != nil {
		t.Fatalf("seed Put inline: %v", err)
	}
	root, err = Put(pw2, cfg, root, []byte("ovfl"), big)
	if err != nil {
		t.Fatalf("Put overflow: %v", err)
	}
	root, err = Delete(pw2, cfg, root, DefaultMergeThreshold, []byte("ovfl"))
	if err != nil {
		t.Fatalf("Delete overflow: %v", err)
	}
	// Inline entry survives.
	got, found, err := Get(pw2, cfg, root, []byte("inline"))
	if err != nil || !found || !bytes.Equal(got, []byte("v")) {
		t.Errorf("Get(inline) after delete-ovfl: %q found=%v err=%v", got, found, err)
	}
	// Overflow entry gone.
	_, found, err = Get(pw2, cfg, root, []byte("ovfl"))
	if err != nil || found {
		t.Errorf("Get(ovfl) after delete: found=%v err=%v; want (false, nil)", found, err)
	}
	checkSlabPartition(t, pw2, cfg, root)
}

func TestMergeRedistributePreservesOverflowEntries(t *testing.T) {
	// Contract via merge/redistribute: an overflow leaf
	// entry moved between leaves during a merge or redistribute
	// keeps its OverflowPage / TotalLen — the chain is NOT
	// re-allocated, it just changes which leaf entry references
	// it. The slab-partition invariant survives the move.
	//
	// Build a tree with several overflow entries that forces a
	// multi-leaf topology; delete enough inline-padding entries
	// to trigger a merge; verify (a) overflow values still
	// round-trip and (b) the slab partition holds.
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)

	// Three overflow entries with distinct payloads + inline
	// padding to force multi-leaf topology.
	overflowValues := map[string][]byte{
		"k-01-ovfl": bytes.Repeat([]byte{'a'}, 6000),
		"k-05-ovfl": bytes.Repeat([]byte{'b'}, 6000),
		"k-09-ovfl": bytes.Repeat([]byte{'c'}, 6000),
	}
	for k, v := range overflowValues {
		nr, err := Put(pw, cfg, root, []byte(k), v)
		if err != nil {
			t.Fatalf("Put(%q): %v", k, err)
		}
		root = nr
	}
	// Inline padding entries between the overflow keys — large
	// enough that the entries can't fit in a single 4 KB leaf.
	// 1 KB values × 8 entries = 8 KB, forcing ≥2 leaves.
	inlineKeys := []string{"k-02", "k-03", "k-04", "k-06", "k-07", "k-08", "k-10", "k-11"}
	for _, k := range inlineKeys {
		val := bytes.Repeat([]byte{'i'}, 1000)
		nr, err := Put(pw, cfg, root, []byte(k), val)
		if err != nil {
			t.Fatalf("Put(%q): %v", k, err)
		}
		root = nr
	}

	if treeDepth(t, pw, root) < 1 {
		t.Fatalf("setup: tree depth < 1; need multi-leaf topology")
	}

	// Delete every inline-padding key with threshold=50 to force
	// merges; overflow entries stay alive and move between
	// leaves as siblings merge.
	for _, k := range inlineKeys {
		nr, err := Delete(pw, cfg, root, 50, []byte(k))
		if err != nil {
			t.Fatalf("Delete(%q): %v", k, err)
		}
		root = nr
	}

	// Overflow values still retrievable post-merge.
	for k, want := range overflowValues {
		got, found, err := Get(pw, cfg, root, []byte(k))
		if err != nil || !found {
			t.Errorf("Get(%q) post-merge: found=%v err=%v", k, found, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Get(%q) post-merge: value mismatch (got len=%d, want len=%d)", k, len(got), len(want))
		}
	}
	// Slab partition holds — no chain orphans across the merge.
	checkSlabPartition(t, pw, cfg, root)
}

func TestCursorDeleteOverflowEntryFreesChain(t *testing.T) {
	// Contract via the cursor: Cursor.Delete delegates
	// to btree.Delete; an overflow entry's chain must be freed
	// the same way as a direct btree.Delete.
	cfg := page.Config{PageSize: 4096}
	big := bytes.Repeat([]byte{'x'}, 8000)
	pw := newFakeWriter(t, 4096)
	root, err := Put(pw, cfg, 0, []byte("a"), []byte("A"))
	if err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	root, err = Put(pw, cfg, root, []byte("big"), big)
	if err != nil {
		t.Fatalf("Put overflow: %v", err)
	}
	root, err = Put(pw, cfg, root, []byte("z"), []byte("Z"))
	if err != nil {
		t.Fatalf("Put trailing: %v", err)
	}

	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)
	c.Seek([]byte("big"))
	if err := c.Delete(); err != nil {
		t.Fatalf("Cursor.Delete on overflow entry: %v", err)
	}
	root = c.RootID()
	// Successor advanced to "z".
	if k, _ := c.Current(); !bytes.Equal(k, []byte("z")) {
		t.Errorf("post-Cursor.Delete current key = %q; want z", k)
	}
	checkSlabPartition(t, pw, cfg, root)
}

// checkBalance walks the tree and asserts every leaf is at the same
// depth from the root. A B+tree must be perfectly height-balanced;
// any path-length disagreement indicates the delete cascade dropped a
// level on one side.
func checkBalance(t *testing.T, pw *fakeWriter, cfg page.Config, root uint64) {
	t.Helper()
	if root == 0 {
		return
	}
	leafDepths := make(map[int]int)
	var walk func(id uint64, depth int)
	walk = func(id uint64, depth int) {
		buf, _ := pw.Page(id)
		typ, _, _, _ := page.ReadHeader(buf)
		switch {
		case page.IsLeafType(typ):
			leafDepths[depth]++
		case typ == page.TypeBranch:
			lm, cells := page.DecodeBranch(buf, cfg)
			walk(lm, depth+1)
			for _, c := range cells {
				walk(c.Child, depth+1)
			}
		default:
			t.Errorf("checkBalance: page %d unexpected type %d", id, typ)
		}
	}
	walk(root, 0)
	if len(leafDepths) > 1 {
		t.Errorf("tree imbalanced: leaves at multiple depths %v (B+tree balance broken)", leafDepths)
	}
}

// checkUnderflowInvariant walks the tree and reports any non-root
// page whose encoded fill is below threshold. The root is exempt
// (it can be any size).
func checkUnderflowInvariant(t *testing.T, pw *fakeWriter, cfg page.Config, root uint64, threshold uint8) {
	t.Helper()
	if root == 0 {
		return
	}
	var walk func(id uint64, isRoot bool)
	walk = func(id uint64, isRoot bool) {
		buf, _ := pw.Page(id)
		typ, _, _, _ := page.ReadHeader(buf)
		switch {
		case page.IsLeafType(typ):
			r := page.NewLeafReader(buf, cfg)
			if err := r.Validate(); err != nil {
				t.Errorf("checkUnderflowInvariant: validate leaf %d: %v", id, err)
				return
			}
			if isRoot {
				return
			}
			size := cfg.ContentEnd() - r.FreeSpace()
			if size*100 < int(threshold)*cfg.ContentEnd() {
				t.Errorf("non-root leaf %d underflowed: size=%d (%.1f%% of %d), threshold=%d%%",
					id, size, float64(size)*100/float64(cfg.ContentEnd()), cfg.ContentEnd(), threshold)
			}
		case typ == page.TypeBranch:
			lm, cells := page.DecodeBranch(buf, cfg)
			if !isRoot {
				// Fill-floor is measured on LOGICAL (uncompressed) content,
				// not compressed bytes (range-delete.md §Invariants): within-
				// page prefix truncation shrinks a dense same-cluster branch's
				// bytes without reducing its fanout, so the floor would
				// spuriously fire on a maximally-dense page if measured on
				// compressed size.
				size := page.BranchLogicalSize(cells)
				if size*100 < int(threshold)*cfg.ContentEnd() {
					t.Errorf("non-root branch %d underflowed: logical size=%d (%.1f%% of %d), threshold=%d%%",
						id, size, float64(size)*100/float64(cfg.ContentEnd()), cfg.ContentEnd(), threshold)
				}
			}
			walk(lm, false)
			for _, c := range cells {
				walk(c.Child, false)
			}
		default:
			t.Errorf("checkUnderflowInvariant: page %d type %d unexpected", id, typ)
		}
	}
	walk(root, true)
}

// checkReachableFloor asserts the rebalance left no below-floor branch that
// COULD have been raised above the fill-floor by merging or redistributing
// with an adjacent same-parent sibling. The fill-floor is LOGICAL
// (range-delete.md §Invariants); within-page prefix truncation
// (page-formats.md §Branch Page) makes some branches genuinely un-healable —
// a cluster-SEAM branch (a large within-cluster separator plus a tiny
// cross-cluster one) whose neighbours are dense same-cluster branches cannot
// absorb more cells without un-compressing across the cluster boundary and
// overflowing a physical page. Those are the "where reachable" exception and
// are allowed below the floor. A below-floor branch that IS healable is a
// rebalance defect (e.g. balancing a redistribute on compressed instead of
// logical size piles the cheap same-cluster cells on one half and strands the
// other below the floor). This is the precise post-compression statement of
// the floor guarantee — strictly stronger than "no branch below floor", which
// no longer holds for multi-cluster (seam-bearing) workloads.
//
// Scope: it checks healing against an adjacent SAME-PARENT sibling only, not
// the cousin-cascade (a below-floor child healed by being merged into a
// sibling-rich branch one level up, range-delete.md §Phase 3). That makes it
// conservatively UNDER-strict — a cousin-only-healable branch is reported as
// stuck, never the reverse — so it cannot raise a spurious failure; it does
// catch the same-parent compressed-vs-logical balance defect it is here for.
func checkReachableFloor(t *testing.T, pw *fakeWriter, cfg page.Config, root uint64, mt uint8) {
	t.Helper()
	if root == 0 {
		return
	}
	ce := cfg.ContentEnd()
	below := func(n int) bool { return n*100 < int(mt)*ce }

	type binfo struct {
		cells    []page.BranchCell
		leftmost uint64
		children []uint64 // leftmost + each cell's child, in descent order
	}
	branches := map[uint64]*binfo{}
	parentOf := map[uint64]uint64{}
	idxOf := map[uint64]int{}
	var walk func(id uint64)
	walk = func(id uint64) {
		buf, _ := pw.Page(id)
		if typ, _, _, _ := page.ReadHeader(buf); typ != page.TypeBranch {
			return
		}
		lm, cells := page.DecodeBranch(buf, cfg)
		children := []uint64{lm}
		for _, c := range cells {
			children = append(children, c.Child)
		}
		branches[id] = &binfo{cells: cells, leftmost: lm, children: children}
		for i, ch := range children {
			parentOf[ch] = id
			idxOf[ch] = i
			walk(ch)
		}
	}
	walk(root)

	for id, bi := range branches {
		if id == root || !below(page.BranchLogicalSize(bi.cells)) {
			continue
		}
		pi := branches[parentOf[id]]
		idx := idxOf[id]
		// Replicate mergeOrRedistributeBranches against an adjacent sibling:
		// combine cells (with the parent separator between them), then either
		// full-merge (fits one page) or redistribute via findBranchSplitIndex
		// (both halves fit AND both clear the logical floor).
		heal := func(sibIdx int) bool {
			if sibIdx < 0 || sibIdx >= len(pi.children) {
				return false
			}
			sib, ok := branches[pi.children[sibIdx]]
			if !ok {
				return false
			}
			lo := min(idx, sibIdx)
			left, right := bi, sib
			if idx > sibIdx {
				left, right = sib, bi
			}
			combined := append([]page.BranchCell{}, left.cells...)
			combined = append(combined, page.BranchCell{Key: pi.cells[lo].Key, Child: right.leftmost})
			combined = append(combined, right.cells...)
			if page.BranchEncodedSize(cfg, combined) <= ce {
				return true // a full merge would heal it
			}
			if m, ok := findBranchSplitIndex(cfg, combined); ok {
				return !below(page.BranchLogicalSize(combined[:m])) && !below(page.BranchLogicalSize(combined[m+1:]))
			}
			return false
		}
		if heal(idx-1) || heal(idx+1) {
			ls := page.BranchLogicalSize(bi.cells)
			t.Errorf("reachable-floor violation: below-floor branch %d (logical %d, %.1f%% < %d%%) is healable via an adjacent sibling — rebalance stranded a reachable branch below the floor",
				id, ls, float64(ls)*100/float64(ce), mt)
		}
	}
}

// checkSlabPartition asserts every allocated page is either reachable
// from root or freed. Mirrors collectReachable from put_test.go;
// duplicated rather than imported to keep test files independent.
func checkSlabPartition(t *testing.T, pw *fakeWriter, cfg page.Config, root uint64) {
	t.Helper()
	reachable := make(map[uint64]struct{})
	collectReachable(t, pw, cfg, root, reachable)
	for id := range pw.pages {
		_, isReachable := reachable[id]
		_, isFreed := pw.freed[id]
		if isReachable && isFreed {
			t.Errorf("page %d both reachable and freed", id)
		}
		if !isReachable && !isFreed {
			t.Errorf("page %d allocated but neither reachable nor freed", id)
		}
	}
}

// TestMergeBranchesForgedSiblingNoPanic:
// mergeOrRedistributeBranches reads BOTH siblings fresh — the merge sibling
// is the adjacent slot, NOT on the descent path that was validated on the
// way down. A forged sibling-branch directory must therefore surface as
// ErrCorrupted via validateBranchPage, not a BranchCellAt out-of-bounds
// panic. This mirrors the sibling-leaf validation in
// mergeOrRedistributeLeaves (forged sibling leaf already yields
// ErrCorrupted; this closes the same gap for branches).
// TestDeleteSingleKeyPreservesFillFloor pins range-delete.md
// §Invariants fill-floor clause's "the same floor holds for
// single-key `Delete`" sub-clause. The cousin-cascade case is
// **structurally unreachable** from a valid pre-state for
// single-key Delete (the inductive maintenance argument: each
// merge has one input >= MT — the untouched sibling — so combined
// >= MT and the merged result is >= MT). When a merge instead
// overflows and redistributes, the byte-balanced split
// (findLeafSplitIndex) lands each half near 50%; any half still
// below MT is healed by rebalanceSurvivors' fill re-check — the
// floor is enforced by that re-check, not by the split being
// inherently >= MT. What
// SHOULD be true even so: every successful single-key Delete
// leaves every non-root page >= MergeThreshold% of ContentEnd.
//
// This is a per-Delete progression smoke test: a hand-built
// depth-2 tree where every non-root page is just above MT,
// every key is deleted one at a time in a deterministic order,
// and the floor is asserted after each delete. A regression
// that broke the inductive maintenance — including any future
// merge/redistribute change that allowed an under-floor
// non-root page — fires here.
func TestDeleteSingleKeyPreservesFillFloor(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)

	// Same hand-built shape as the DeleteRange test but with the
	// invariant pinned at mt=10 across a sequence of single-key
	// Deletes. ROOT -> [P, Q, R]; each intermediate -> 6 leaves;
	// each leaf -> 3 entries.
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

	leafKeys := map[uint64][]string{
		5:  {"a01", "a02", "a03"},
		6:  {"b01", "b02", "b03"},
		7:  {"c01", "c02", "c03"},
		8:  {"d01", "d02", "d03"},
		9:  {"e01", "e02", "e03"},
		10: {"f01", "f02", "f03"},
	}
	for id, keys := range leafKeys {
		buildLeaf(id, keys)
	}
	for q := range 6 {
		qid := uint64(11 + q)
		qkeys := []string{
			fmt.Sprintf("q%c01", 'a'+q),
			fmt.Sprintf("q%c02", 'a'+q),
			fmt.Sprintf("q%c03", 'a'+q),
		}
		buildLeaf(qid, qkeys)
		leafKeys[qid] = qkeys
		rid := uint64(17 + q)
		rkeys := []string{
			fmt.Sprintf("r%c01", 'a'+q),
			fmt.Sprintf("r%c02", 'a'+q),
			fmt.Sprintf("r%c03", 'a'+q),
		}
		buildLeaf(rid, rkeys)
		leafKeys[rid] = rkeys
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
		{Key: padKey("b01"), Child: 6},
		{Key: padKey("c01"), Child: 7},
		{Key: padKey("d01"), Child: 8},
		{Key: padKey("e01"), Child: 9},
		{Key: padKey("f01"), Child: 10},
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

	// Collect every key and delete in a deterministic order. After
	// each delete, the floor must hold.
	var allKeys []string
	for _, ks := range leafKeys {
		allKeys = append(allKeys, ks...)
	}
	// Deterministic order: lexical.
	for i := 0; i < len(allKeys); i++ {
		for j := i + 1; j < len(allKeys); j++ {
			if allKeys[i] > allKeys[j] {
				allKeys[i], allKeys[j] = allKeys[j], allKeys[i]
			}
		}
	}

	root := uint64(1)
	for _, k := range allKeys {
		nr, err := Delete(pw, cfg, root, mt, padKey(k))
		if err != nil {
			t.Fatalf("Delete(%q): %v", k, err)
		}
		root = nr
		checkUnderflowInvariant(t, pw, cfg, root, mt)
		if root == 0 {
			break
		}
	}
}

func TestMergeBranchesForgedSiblingNoPanic(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	// Left: a well-formed branch (two children).
	left := make([]byte, 4096)
	if err := page.EncodeBranch(left, cfg, 100, []page.BranchCell{{Key: []byte("m"), Child: 101}}); err != nil {
		t.Fatalf("EncodeBranch(left): %v", err)
	}
	pw.pages[1] = left
	// Right (the sibling): well-formed, then forge the first cell-directory
	// entry offset to 0xFFFF (past content end) — BranchCellAt would panic.
	right := make([]byte, 4096)
	if err := page.EncodeBranch(right, cfg, 200, []page.BranchCell{{Key: []byte("t"), Child: 201}}); err != nil {
		t.Fatalf("EncodeBranch(right): %v", err)
	}
	right[16], right[17] = 0xFF, 0xFF
	pw.pages[2] = right

	_, _, _, _, _, err := mergeOrRedistributeBranches(pw, cfg, DefaultMergeThreshold, 1, 2, page.BranchCell{Key: []byte("sep")},
		func(page.BranchCell) bool { return true })
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("mergeOrRedistributeBranches with forged sibling = %v, want ErrCorrupted (no panic)", err)
	}
}

// buildLeafPageForTest encodes entries into a freshly-zeroed page at
// id, failing the test if any entry does not fit.
func buildLeafPageForTest(t *testing.T, pw *fakeWriter, cfg page.Config, id uint64, entries []page.LeafEntry) []byte {
	t.Helper()
	buf, err := pw.ZeroPage(id)
	if err != nil {
		t.Fatalf("ZeroPage(%d): %v", id, err)
	}
	b := page.NewLeafBuilder(buf, cfg)
	for _, e := range entries {
		if !b.AddEntry(e) {
			t.Fatalf("fixture leaf %d: entry %q does not fit", id, e.Key)
		}
	}
	b.Finish()
	return buf
}

// declineFixture builds the parent-capacity-decline topology shared by
// the Delete and DeleteRange regression tests below:
//
//	root branch (near-full, ~zero slack)
//	├─ leftmost: L0 — short "aNNN" keys, a hair above the fill floor
//	├─ cells[0] = {"b", L1} — 1-byte boundary separator
//	│    L1 — 300-byte-shared-prefix keys, near-full
//	└─ cells[1..] = filler separators + tiny filler leaves
//
// Deleting from L0 underflows it; the pair cannot merge (combined
// exceeds one page); any redistribute boundary lands inside L1's
// prefix cluster, recomputing a ~300-byte separator the near-full
// parent cannot fit — the redistribute must DECLINE.
type declineFixture struct {
	rootID     uint64
	l0Keys     [][]byte
	l1Keys     [][]byte
	fillerKeys [][]byte
	cellCount  int
	sep0       []byte
}

func buildDeclineFixture(t *testing.T, pw *fakeWriter, cfg page.Config) declineFixture {
	t.Helper()

	l0Entries := make([]page.LeafEntry, 8)
	l0Keys := make([][]byte, len(l0Entries))
	for i := range l0Entries {
		l0Keys[i] = fmt.Appendf(nil, "a%03d", i)
		l0Entries[i] = page.LeafEntry{Key: l0Keys[i], Value: bytes.Repeat([]byte{'v'}, 130)}
	}
	pfx := bytes.Repeat([]byte("p"), 300)
	l1Entries := make([]page.LeafEntry, 10)
	l1Keys := make([][]byte, len(l1Entries))
	for i := range l1Entries {
		l1Keys[i] = fmt.Appendf(nil, "%s-%03d", pfx, i)
		l1Entries[i] = page.LeafEntry{Key: l1Keys[i], Value: bytes.Repeat([]byte{'w'}, 330)}
	}

	l0ID, _ := pw.AllocPage()
	l0Buf := buildLeafPageForTest(t, pw, cfg, l0ID, l0Entries)
	l1ID, _ := pw.AllocPage()
	buildLeafPageForTest(t, pw, cfg, l1ID, l1Entries)

	// Fixture property 1: L0 is above the fill floor now, below it
	// with its first entry gone (scratch measurement).
	if leafUnderflow(l0Buf, cfg, DefaultMergeThreshold) {
		t.Fatalf("fixture: L0 already below the fill floor")
	}
	scratch := make([]byte, cfg.PageSize)
	sb := page.NewLeafBuilder(scratch, cfg)
	for _, e := range l0Entries[1:] {
		sb.AddEntry(e)
	}
	sb.Finish()
	if !leafUnderflow(scratch, cfg, DefaultMergeThreshold) {
		t.Fatalf("fixture: L0 minus one entry not below the fill floor")
	}

	// Fixture property 2: the pair cannot merge into one page.
	scratch2 := make([]byte, cfg.PageSize)
	mb := page.NewLeafBuilder(scratch2, cfg)
	mergeFits := true
	for _, e := range append(append([]page.LeafEntry{}, l0Entries...), l1Entries...) {
		if !mb.AddEntry(e) {
			mergeFits = false
			break
		}
	}
	if mergeFits {
		t.Fatalf("fixture: L0+L1 merge fits one page; redistribute never runs")
	}

	// Parent: 1-byte boundary separator, then filler cells until the
	// next 150-byte filler no longer fits — leaving slack far below
	// the ~300-byte separator growth a redistribute would need.
	sep0 := []byte("b")
	cells := []page.BranchCell{{Key: sep0, Child: l1ID}}
	var fillerKeys [][]byte
	for i := 0; ; i++ {
		fk := fmt.Appendf(nil, "q%03d-%s", i, bytes.Repeat([]byte{'f'}, 145))
		cand := append(append([]page.BranchCell{}, cells...), page.BranchCell{Key: fk})
		if page.BranchEncodedSize(cfg, cand) > cfg.ContentEnd() {
			break
		}
		childID, _ := pw.AllocPage()
		childKey := append(bytes.Clone(fk), 'z')
		buildLeafPageForTest(t, pw, cfg, childID, []page.LeafEntry{{Key: childKey, Value: []byte("f")}})
		fillerKeys = append(fillerKeys, childKey)
		cells = append(cells, page.BranchCell{Key: fk, Child: childID})
	}

	// Fixture property 3: a prefix-cluster separator does NOT fit the
	// parent (this is the guard the fix adds; assert the topology
	// actually exercises it).
	probe := append(bytes.Clone(pfx), []byte("-0")...)
	if parentFitsSeparator(cfg, cells, 0, sizingSeparatorCell(cfg, probe)) {
		t.Fatalf("fixture: parent has room for a %d-byte separator; slack too large", len(probe))
	}

	rootID, _ := pw.AllocPage()
	rootBuf, err := pw.ZeroPage(rootID)
	if err != nil {
		t.Fatalf("ZeroPage(root): %v", err)
	}
	if err := page.EncodeBranch(rootBuf, cfg, l0ID, cells); err != nil {
		t.Fatalf("fixture: encode parent: %v", err)
	}
	return declineFixture{
		rootID:     rootID,
		l0Keys:     l0Keys,
		l1Keys:     l1Keys,
		fillerKeys: fillerKeys,
		cellCount:  len(cells),
		sep0:       sep0,
	}
}

// verifyDeclineOutcome asserts the rebalance DECLINED (parent shape
// unchanged) and every surviving key is still readable.
func verifyDeclineOutcome(t *testing.T, pw *fakeWriter, cfg page.Config, fx declineFixture, newRoot uint64, deletedKeys [][]byte) {
	t.Helper()
	rootBuf, err := pw.Page(newRoot)
	if err != nil {
		t.Fatalf("Page(newRoot): %v", err)
	}
	if typ, _, _, _ := page.ReadHeader(rootBuf); typ != page.TypeBranch {
		t.Fatalf("newRoot type = %d, want branch", typ)
	}
	_, cells := page.DecodeBranch(rootBuf, cfg)
	if len(cells) != fx.cellCount {
		t.Errorf("parent cell count = %d, want %d (decline must not merge/split)", len(cells), fx.cellCount)
	}
	if !bytes.Equal(cells[0].Key, fx.sep0) {
		t.Errorf("boundary separator = %q, want %q (decline must not replace it)", cells[0].Key, fx.sep0)
	}
	deleted := make(map[string]bool, len(deletedKeys))
	for _, k := range deletedKeys {
		deleted[string(k)] = true
	}
	for _, group := range [][][]byte{fx.l0Keys, fx.l1Keys, fx.fillerKeys} {
		for _, k := range group {
			_, found, err := Get(pw, cfg, newRoot, k)
			if err != nil {
				t.Fatalf("Get(%q): %v", k, err)
			}
			if found == deleted[string(k)] {
				t.Errorf("Get(%q): found=%v, want %v", k, found, !deleted[string(k)])
			}
		}
	}
	checkSlabPartition(t, pw, cfg, newRoot)
}

// TestDeleteDeclinesRedistributeWhenParentCannotFitSeparator pins the
// parent-capacity decline (range-delete.md §Invariants): a valid
// Delete whose rebalance would recompute a boundary separator the
// near-full parent cannot physically encode must succeed by declining
// the redistribute (accepting the below-floor leaf), not fail with an
// encode error after freeing the sibling pages.
func TestDeleteDeclinesRedistributeWhenParentCannotFitSeparator(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	pw := newFakeWriter(t, 4096)
	fx := buildDeclineFixture(t, pw, cfg)

	newRoot, err := Delete(pw, cfg, fx.rootID, DefaultMergeThreshold, fx.l0Keys[0])
	if err != nil {
		t.Fatalf("Delete: %v (want success via redistribute decline)", err)
	}
	verifyDeclineOutcome(t, pw, cfg, fx, newRoot, fx.l0Keys[:1])
}

// TestDeleteRangeDeclinesRedistributeWhenParentCannotFitSeparator is
// the DeleteRange analogue. The range SPANS the L0/L1 boundary so both
// leaves become partially-deleted boundary survivors and the healing
// runs through rebalanceSurvivors — whose parent-fit candidate is
// built from the survivor boundary keys. (A range inside one leaf
// routes through the single-Delete rebalance machinery instead and
// leaves the survivors-path closure unexercised — a mutation test
// caught exactly that in an earlier version of this test.)
func TestDeleteRangeDeclinesRedistributeWhenParentCannotFitSeparator(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	pw := newFakeWriter(t, 4096)
	fx := buildDeclineFixture(t, pw, cfg)

	// [l0Keys[6], l1Keys[1]) deletes L0's last two keys (underflowing
	// it) and L1's first key. Fixture property: the post-delete pair
	// still cannot merge into one page, so the survivor healing must
	// take the redistribute plan and hit the parent-fit decline.
	scratch := make([]byte, cfg.PageSize)
	sb := page.NewLeafBuilder(scratch, cfg)
	survivorsMerge := true
	for _, k := range append(append([][]byte{}, fx.l0Keys[:6]...), fx.l1Keys[1:]...) {
		v, found, err := Get(pw, cfg, fx.rootID, k)
		if err != nil || !found {
			t.Fatalf("fixture: Get(%q) pre-delete: found=%v err=%v", k, found, err)
		}
		if !sb.AddEntry(page.LeafEntry{Key: k, Value: v}) {
			survivorsMerge = false
			break
		}
	}
	if survivorsMerge {
		t.Fatalf("fixture: post-delete pair merges into one page; redistribute never runs")
	}

	count, newRoot, err := DeleteRange(pw, cfg, fx.rootID, DefaultMergeThreshold, fx.l0Keys[6], fx.l1Keys[1], plainCellFreeForTest)
	if err != nil {
		t.Fatalf("DeleteRange: %v (want success via redistribute decline)", err)
	}
	if count != 3 {
		t.Fatalf("DeleteRange count = %d, want 3", count)
	}
	deleted := [][]byte{fx.l0Keys[6], fx.l0Keys[7], fx.l1Keys[0]}
	verifyDeclineOutcome(t, pw, cfg, fx, newRoot, deleted)
}

// TestMergeOrRedistributeBranchesParentFitDecline pins the branch
// helper's parent-capacity decline term directly: a redistribute whose
// lifted separator the parent cannot encode returns the all-zero
// decline with nothing allocated or freed; the same topology with a
// permissive parentFits redistributes normally.
func TestMergeOrRedistributeBranchesParentFitDecline(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}

	build := func(pw *fakeWriter, id uint64, leftmost uint64, cells []page.BranchCell) {
		t.Helper()
		buf, err := pw.ZeroPage(id)
		if err != nil {
			t.Fatalf("ZeroPage(%d): %v", id, err)
		}
		if err := page.EncodeBranch(buf, cfg, leftmost, cells); err != nil {
			t.Fatalf("EncodeBranch(%d): %v", id, err)
		}
	}
	mkCells := func(prefix byte, n int, firstChild uint64) []page.BranchCell {
		cells := make([]page.BranchCell, n)
		for i := range cells {
			// ~310-byte keys with no shared prefix across cells keeps
			// physical == logical size; 7 cells/side → combined > one
			// page (forces redistribute) with both halves above the
			// fill floor (so only parentFits can decline).
			cells[i] = page.BranchCell{
				Key:   fmt.Appendf(nil, "%c%02d-%s", prefix, i, bytes.Repeat([]byte{prefix}, 305)),
				Child: firstChild + uint64(i),
			}
		}
		return cells
	}

	run := func(fits bool) (bool, uint64, uint64, uint64, page.BranchCell, *fakeWriter) {
		pw := newFakeWriter(t, 4096)
		leftID, _ := pw.AllocPage()
		build(pw, leftID, 100, mkCells('c', 7, 101))
		rightID, _ := pw.AllocPage()
		build(pw, rightID, 200, mkCells('s', 7, 201))
		isMerge, mergedID, newLeftID, newRightID, newSep, err := mergeOrRedistributeBranches(
			pw, cfg, DefaultMergeThreshold, leftID, rightID, page.BranchCell{Key: []byte("m")},
			func(page.BranchCell) bool { return fits })
		if err != nil {
			t.Fatalf("mergeOrRedistributeBranches(fits=%v): %v", fits, err)
		}
		if isMerge {
			t.Fatalf("fixture: pair merged; need redistribute-sized inputs")
		}
		return isMerge, mergedID, newLeftID, newRightID, newSep, pw
	}

	// Permissive parent: redistribute proceeds (also proves the
	// fixture reaches the redistribute plan, so the decline below is
	// attributable to parentFits alone).
	_, _, nl, nr, sep, _ := run(true)
	if nl == 0 || nr == 0 || len(sep.Key) == 0 {
		t.Fatalf("fits=true: got (%d, %d, %q), want a performed redistribute", nl, nr, sep.Key)
	}

	// Unfit parent: decline — all-zero, nothing allocated or freed.
	_, mergedID, nl, nr, sep, pw := run(false)
	if mergedID != 0 || nl != 0 || nr != 0 || sep.Key != nil || sep.KeyExtPage != 0 {
		t.Errorf("fits=false: got (%d, %d, %d, %q), want all-zero decline", mergedID, nl, nr, sep.Key)
	}
	if len(pw.freed) != 0 {
		t.Errorf("fits=false: %d pages freed on decline, want 0", len(pw.freed))
	}
	if len(pw.pages) != 2 {
		t.Errorf("fits=false: %d pages exist after decline, want the 2 inputs", len(pw.pages))
	}
}

// TestMergePairRejectsMixedSiblingTypes pins the shared pair
// dispatch's same-level guard: all children of a branch live at the
// same depth, so a leaf paired with a branch is structural corruption
// and must surface ErrCorrupted — never dispatch into a helper that
// would misread the other page's layout.
func TestMergePairRejectsMixedSiblingTypes(t *testing.T) {
	pw := newFakeWriter(t, 4096)
	cfg := page.Config{PageSize: 4096}

	leafBuf, err := pw.ZeroPage(5)
	if err != nil {
		t.Fatalf("ZeroPage(5): %v", err)
	}
	lb := page.NewLeafBuilder(leafBuf, cfg)
	if !lb.AddEntry(page.LeafEntry{Key: []byte("a"), Value: []byte("v")}) {
		t.Fatal("AddEntry overflow")
	}
	lb.Finish()

	branchBuf, err := pw.ZeroPage(6)
	if err != nil {
		t.Fatalf("ZeroPage(6): %v", err)
	}
	if err := page.EncodeBranch(branchBuf, cfg, 100, []page.BranchCell{{Key: []byte("m"), Child: 101}}); err != nil {
		t.Fatalf("EncodeBranch: %v", err)
	}

	_, err = mergeOrRedistributePair(pw, cfg, 30, 5, 6, page.BranchCell{Key: []byte("m")}, func(page.BranchCell) bool { return true })
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("mixed-type pair: err = %v, want ErrCorrupted", err)
	}
}
