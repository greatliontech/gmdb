package btree

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// skewedBranchKeys builds keys that force branch pages to carry a
// size-skewed mix of separators — the end-to-end analogue of the white-box
// skewedBranchCells (split_test.go). Keys within a cluster share a long
// (~prefixLen-byte) prefix, so consecutive same-cluster leaves are divided
// by a LARGE separator (ShortestSeparator returns the deep common prefix +
// 1 byte); cluster transitions diverge at byte 0, yielding a 1-byte
// separator. A large inline value keeps each leaf to a few entries, so a
// cluster spans several leaves (several large separators in a row). The
// resulting branches mix large and tiny separators, so an entry-count
// midpoint can cluster large separators on one half — the exact shape the
// byte-balanced branch splitter must handle (page-formats.md
// §Prefix-Truncated Branch Keys).
func skewedBranchKeys(clusters, per, prefixLen int) [][]byte {
	var keys [][]byte
	for c := range clusters {
		prefix := append([]byte{byte('A' + c)}, bytes.Repeat([]byte("p"), prefixLen)...)
		for j := range per {
			k := append(append([]byte(nil), prefix...), fmt.Appendf(nil, "%04d", j)...)
			keys = append(keys, k)
		}
	}
	return keys
}

// TestPutSizeSkewedBranchSplitNoSpuriousError drives real multi-level
// branch splits with size-skewed separators (~1400-byte cluster prefixes).
// Every key is well below the limits.md §Maximum Key Size bound, so no Put
// may return a spurious error: the count-midpoint branch split could
// cluster more than a page of large separators on one half (the finding-19
// fault), but the byte-balanced findBranchSplitIndex must place each half
// within a page. Asserts the tree reached depth >= 2 (branch splits
// actually occurred), every value reads back, and the structural invariants
// hold. (Put-only, so the delete-side rebalance machinery is not exercised
// here — see TestDeleteSizeSkewedBranchRedistributePreservesFillFloor.)
func TestPutSizeSkewedBranchSplitNoSpuriousError(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	keys := skewedBranchKeys(6, 12, 1400)
	val := bytes.Repeat([]byte("v"), 1300) // large value → few entries/leaf → many leaves

	for i, k := range keys {
		nr, err := Put(pw, cfg, root, k, val)
		if err != nil {
			t.Fatalf("Put #%d (key len %d): %v", i, len(k), err)
		}
		root = nr
	}

	if d := treeDepth(t, pw, root); d < 2 {
		t.Fatalf("tree depth = %d, want >= 2 (no branch split exercised — reshape the workload)", d)
	}
	for i, k := range keys {
		got, found, err := Get(pw, cfg, root, k)
		if err != nil || !found {
			t.Fatalf("Get #%d: found=%v err=%v", i, found, err)
		}
		if !bytes.Equal(got, val) {
			t.Errorf("Get #%d: value mismatch", i)
		}
	}
	checkBalance(t, pw, cfg, root)
	checkUnderflowInvariant(t, pw, cfg, root, DefaultMergeThreshold)
	checkSlabPartition(t, pw, cfg, root)
}

// TestDeleteSizeSkewedBranchRedistributePreservesFillFloor exercises the
// delete-side byte-balanced branch redistribute over a multi-cluster,
// size-skewed tree (~700-byte within-cluster separators, 1-byte cluster
// seams). It builds a depth-3 tree, then deletes ~80% of keys at
// DefaultMergeThreshold, cascading branch-level merges and redistributes.
//
// The guarantee asserted (after every delete) is the post-compression form of
// the range-delete.md §Invariants fill-floor: the redistribute leaves no
// branch below the LOGICAL floor that COULD have been raised above it by a
// merge or redistribute with an adjacent sibling (checkReachableFloor). The
// STRICT "no branch below floor" no longer holds under within-page prefix
// truncation: a cluster-SEAM branch (a large within-cluster separator plus a
// tiny cross-cluster one) whose neighbours are dense same-cluster branches is
// genuinely un-healable — absorbing more cells would un-compress across the
// cluster boundary and overflow a physical page. That is the "where
// reachable" exception. The finder must (a) keep every redistribute half
// within a physical page, (b) balance on LOGICAL content so it never strands
// a reachable half below the floor, and (c) not return a spurious
// ErrCorrupted. (The compressed-vs-logical balance distinction is the
// finding-19 successor: balancing the redistribute on compressed bytes piles
// cheap same-cluster cells on one half and falsely trips the decline.)
func TestDeleteSizeSkewedBranchRedistributePreservesFillFloor(t *testing.T) {
	const threshold = DefaultMergeThreshold
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	keys := skewedBranchKeys(5, 16, 700)
	val := bytes.Repeat([]byte("v"), 900)

	for i, k := range keys {
		nr, err := Put(pw, cfg, root, k, val)
		if err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
		root = nr
	}
	if d := treeDepth(t, pw, root); d < 2 {
		t.Fatalf("setup: tree depth = %d, want >= 2", d)
	}

	// Delete ~80% in shuffled order; each delete may trigger leaf and
	// branch merge/redistribute. The fill-floor must hold after every one.
	rng := rand.New(rand.NewPCG(13, 17))
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	deleted := make(map[int]bool)
	for _, i := range order[:len(order)*8/10] {
		nr, err := Delete(pw, cfg, root, threshold, keys[i])
		if err != nil {
			t.Fatalf("Delete #%d: %v", i, err)
		}
		root = nr
		deleted[i] = true
		checkReachableFloor(t, pw, cfg, root, threshold)
		checkBalance(t, pw, cfg, root)
	}

	for i, k := range keys {
		got, found, err := Get(pw, cfg, root, k)
		if err != nil {
			t.Errorf("Get #%d: err %v", i, err)
			continue
		}
		if want := !deleted[i]; found != want {
			t.Errorf("Get #%d: found=%v want=%v", i, found, want)
		} else if found && !bytes.Equal(got, val) {
			t.Errorf("Get #%d: value mismatch", i)
		}
	}
	checkSlabPartition(t, pw, cfg, root)
}
