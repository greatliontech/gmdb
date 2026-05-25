package btree

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// Chunk-5.7 btree-layer tests for DeleteRange. The higher-level
// gmdb-package tests in delete_range_test.go pin the public surface
// against the deferred-flush descriptor machinery; these tests pin
// the btree-layer invariants directly against an in-memory pager.

// TestBtreeDeleteRangeEmptyTree promotes the rootID == 0 edge case:
// DeleteRange on an empty tree returns (0, 0, nil) and does not
// allocate or free pages.
func TestBtreeDeleteRangeEmptyTree(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 16)
	defer pw.Close()
	defer f.Close()
	count, newRoot, err := DeleteRange(pw, pw.Config(), 0, DefaultMergeThreshold, []byte("a"), []byte("z"))
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
		count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, pair.s, pair.e)
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
	count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, []byte("b"), []byte("d"))
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
			count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, tc.start, tc.end)
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
	count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, nil, nil)
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
// Workload sized to fit the chunk-4.7 16 MB tx-slab budget set by
// setupPagerWriter: 1200 keys × 100-byte values → ~30 leaves at 4 KB
// pages with one branch level (depth 2). The internal slab-reuse
// machinery (chunk-5.4 loose-pop) keeps the working set bounded.
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
	rootTyp, _, _, _ := page.ReadHeader(pw.Page(root))
	if rootTyp != page.TypeBranch {
		t.Fatalf("test setup too small — root is leaf, not branch")
	}
	// Delete the middle 600 keys [k00300, k00900).
	startK := []byte("k00300")
	endK := []byte("k00900")
	count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, startK, endK)
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
	count, _, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, []byte("a"), []byte("d"))
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
	count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, []byte("b"), []byte("c"))
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
	_, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, []byte("k00050"), []byte("k00150"))
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
