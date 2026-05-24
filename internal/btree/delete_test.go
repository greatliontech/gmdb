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
	typ, _, _, _ := page.ReadHeader(pw.Page(root))
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
	typ, _, _, _ = page.ReadHeader(pw.Page(root))
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
		typ, _, _, _ := page.ReadHeader(pw.Page(id))
		switch {
		case page.IsLeafType(typ):
			leafDepths[depth]++
		case typ == page.TypeBranch:
			lm, cells := page.DecodeBranch(pw.Page(id), cfg)
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
		buf := pw.Page(id)
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
