package btree

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// unreachableFloorKeys builds keys with deep (~1400-byte) shared prefixes
// within each of 6 clusters. Within-page branch prefix truncation
// (page-formats.md §Branch Page) packs same-cluster separators densely — two
// of them are ~70% logical fill, above MergeThreshold 50 — so the tree is no
// longer born uniformly underfull. What stays below the (logical) floor at
// this maximum threshold is the residual unreachable set (range-delete.md
// §Invariants "where reachable"): a branch reduced to a SINGLE near-max
// separator (~35%), and cluster-SEAM branches (a large within-cluster
// separator plus a tiny cross-cluster one) that cannot absorb more cells
// without un-compressing across the cluster boundary and overflowing a page.
// 6 clusters × 12 keys + large (1300-byte) values → a depth->=3 tree carrying
// those unreachable branches, which exercises the delete-rebalance decline +
// termination machinery.
func unreachableFloorKeys() [][]byte {
	var keys [][]byte
	for c := range 6 {
		prefix := append([]byte{byte('A' + c)}, bytes.Repeat([]byte("p"), 1400)...)
		for j := range 12 {
			keys = append(keys, append(append([]byte(nil), prefix...), fmt.Appendf(nil, "%04d", j)...))
		}
	}
	return keys
}

func buildUnreachableFloorTree(t *testing.T) (*fakeWriter, page.Config, uint64, [][]byte) {
	t.Helper()
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	keys := unreachableFloorKeys()
	val := bytes.Repeat([]byte("v"), 1300)
	for i, k := range keys {
		nr, err := Put(pw, cfg, root, k, val)
		if err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
		root = nr
	}
	if d := treeDepth(t, pw, root); d < 3 {
		t.Fatalf("setup: depth=%d, want >=3 (need the near-fanout-2 regime)", d)
	}
	return pw, cfg, root, keys
}

// TestDeleteUnreachableFloorTerminates is the regression for the
// range-delete.md §Invariants fill-floor "where reachable" + termination
// clause (single-key Delete path). The
// delete-rebalance machinery must TERMINATE in the unreachable-floor regime,
// accepting the below-MT pages, rather than chase the relocating deficit and
// allocate forever. Before the fix this OOM'd (and double-freed pages on the
// pre-byte-balance HEAD). Asserts: the delete pass completes (no hang/OOM),
// no page double-free (fakeWriter detects; checkSlabPartition verifies the
// reachable/freed partition), the B+tree stays balanced (checkBalance), and
// every surviving key reads back.
//
// It deliberately does NOT assert the fill-floor (checkUnderflowInvariant):
// the residual single-near-max-separator and cluster-seam branches sit below
// the floor where it is genuinely unreachable (range-delete.md §Invariants).
// The architectural root fix — within-page branch prefix truncation — has
// landed (page-formats.md §Branch Page); it raises fan-out for deep-shared-
// prefix keys but cannot make those residual cases reachable, so this test
// pins termination + integrity rather than the strict floor.
func TestDeleteUnreachableFloorTerminates(t *testing.T) {
	pw, cfg, root, keys := buildUnreachableFloorTree(t)

	rng := rand.New(rand.NewPCG(13, 17))
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	deleted := make(map[int]bool)
	for _, i := range order[:len(order)*8/10] {
		nr, err := Delete(pw, cfg, root, MaxMergeThreshold, keys[i])
		if err != nil {
			t.Fatalf("Delete #%d: %v", i, err)
		}
		root = nr
		deleted[i] = true
		checkBalance(t, pw, cfg, root)
	}

	for i, k := range keys {
		_, found, err := Get(pw, cfg, root, k)
		if err != nil {
			t.Fatalf("Get #%d: %v", i, err)
		}
		if want := !deleted[i]; found != want {
			t.Errorf("Get #%d: found=%v want=%v", i, found, want)
		}
	}
	checkSlabPartition(t, pw, cfg, root)
}

// TestDeleteRangeUnreachableFloorTerminates is the DeleteRange-path regression
// for the same defect: a range delete spanning whole clusters drives the
// rebalanceSurvivors branch merge/redistribute into the unreachable-floor
// regime, where the redistribute must decline (no churn) rather than relocate
// the deficit and loop. Asserts termination, balance, page accounting, and
// that exactly the in-range keys are gone and the rest survive.
func TestDeleteRangeUnreachableFloorTerminates(t *testing.T) {
	pw, cfg, root, keys := buildUnreachableFloorTree(t)

	// Delete clusters B, C, D entirely: [first key of 'B', first key of 'E').
	clusterStart := func(c byte) []byte {
		return append(append([]byte{c}, bytes.Repeat([]byte("p"), 1400)...), fmt.Appendf(nil, "%04d", 0)...)
	}
	start, end := clusterStart('B'), clusterStart('E')

	_, newRoot, err := DeleteRange(pw, cfg, root, MaxMergeThreshold, start, end, plainCellFreeForTest)
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	root = newRoot

	checkBalance(t, pw, cfg, root)
	for _, k := range keys {
		_, found, err := Get(pw, cfg, root, k)
		if err != nil {
			t.Fatalf("Get %q: %v", k[:1], err)
		}
		inRange := bytes.Compare(k, start) >= 0 && bytes.Compare(k, end) < 0
		if found == inRange {
			t.Errorf("Get key cluster %q: found=%v, inRange=%v (should differ)", k[:1], found, inRange)
		}
	}
	checkSlabPartition(t, pw, cfg, root)
}
