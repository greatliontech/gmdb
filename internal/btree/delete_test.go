package btree

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
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
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	// Long keys (60B) and large values (512B) to force a depth-2 or
	// deeper tree (same shape as TestPutForcesMultiLevelTreeAndBranchSplit).
	const N = 400
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
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	const N = 400
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
	// Chunk-4.7 contract: Delete of an overflow-flagged leaf entry
	// frees the associated chain in the same write tx. Pinned via
	// the slab-partition invariant (chunk-4.7 extension walks
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
	// Chunk-4.7 contract via merge/redistribute: an overflow leaf
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
	// Chunk-4.7 contract via the cursor: Cursor.Delete delegates
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
		switch typ {
		case page.TypeLeaf, page.TypeLeafUncompressed:
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
		case page.TypeBranch:
			lm, cells := page.DecodeBranch(buf, cfg)
			if !isRoot {
				size := page.BranchEncodedSize(cfg, cells)
				if size*100 < int(threshold)*cfg.ContentEnd() {
					t.Errorf("non-root branch %d underflowed: size=%d (%.1f%% of %d), threshold=%d%%",
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

// TestMergeBranchesForgedSiblingNoPanic (M-1, btree-branch-page-validation):
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
// >= MT and the merged result is >= MT; redistribute can only
// happen when combined > ContentEnd, which means combined > 2*MT
// for any MT <= 50, so per-half count split is >= MT). What
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
		buf, err := pw.AllocSlab(id)
		if err != nil {
			t.Fatalf("AllocSlab(%d): %v", id, err)
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
		buf, err := pw.AllocSlab(id)
		if err != nil {
			t.Fatalf("AllocSlab(%d): %v", id, err)
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

	_, _, _, _, _, err := mergeOrRedistributeBranches(pw, cfg, 1, 2, []byte("sep"))
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("mergeOrRedistributeBranches with forged sibling = %v, want ErrCorrupted (no panic)", err)
	}
}
