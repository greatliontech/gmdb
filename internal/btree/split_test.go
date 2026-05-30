package btree

import (
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// splitTestEntry builds an inline leaf entry with a 2-byte sorted key and
// an inline value of valLen bytes. Value bytes are deterministic so the
// encoded size is stable.
func splitTestEntry(i, valLen int) page.LeafEntry {
	val := make([]byte, valLen)
	for j := range val {
		val[j] = byte(j)
	}
	return page.LeafEntry{Key: []byte{byte(i >> 8), byte(i)}, Value: val}
}

func splitTestBuilder() (*page.LeafBuilder, []byte, page.Config) {
	cfg := page.Config{PageSize: 4096}
	scratch := make([]byte, 4096)
	return page.NewLeafBuilder(scratch, cfg), scratch, cfg
}

// TestFindLeafSplitIndexFeasibleWhenCountMidpointFails is the core
// regression for the byte-balanced split (btree-byte-balanced-split,
// page-formats.md §Leaf Split). A size-skewed leaf whose entry-count
// midpoint places more than a page of bytes on one half — while a
// feasible byte-balanced boundary exists — must split successfully. The
// prior `len(entries)/2` split returned a spurious ErrKeyTooLarge on
// exactly this shape.
func TestFindLeafSplitIndexFeasibleWhenCountMidpointFails(t *testing.T) {
	b, scratch, cfg := splitTestBuilder()
	// Values ≈ 29% / 29% / 34% / 68% of a 4 KB page. Count midpoint
	// (idx 2) → right half [1400,2800] overflows a page; feasible at
	// idx 3 → left [1200,1200,1400], right [2800].
	entries := []page.LeafEntry{
		splitTestEntry(0, 1200),
		splitTestEntry(1, 1200),
		splitTestEntry(2, 1400),
		splitTestEntry(3, 2800),
	}

	// Precondition: the entry-count midpoint (what the old code chose) is
	// genuinely infeasible, so this test exercises the new path.
	countMid := len(entries) / 2
	if leafEntriesFit(b, scratch, cfg, entries[countMid:]) {
		t.Fatalf("precondition: count-midpoint right half unexpectedly fits — reshape value sizes")
	}

	mid, ok := findLeafSplitIndex(b, scratch, cfg, entries)
	if !ok {
		t.Fatalf("findLeafSplitIndex ok=false for a splittable size-skewed leaf")
	}
	if mid < 1 || mid > len(entries)-1 {
		t.Fatalf("mid=%d out of range [1,%d]", mid, len(entries)-1)
	}
	if !leafEntriesFit(b, scratch, cfg, entries[:mid]) {
		t.Errorf("left half entries[:%d] does not fit a page", mid)
	}
	if !leafEntriesFit(b, scratch, cfg, entries[mid:]) {
		t.Errorf("right half entries[%d:] does not fit a page", mid)
	}
}

// TestFindLeafSplitIndexRedistributeAlwaysFeasible pins the redistribute
// invariant: entries that already fit in two pages (the merge→overflow
// redistribute input always arrives from two valid sibling pages) must
// always yield a feasible split, regardless of size skew. A range of
// mixed small and large values summing to ~1.5 pages stands in for that
// combined list.
func TestFindLeafSplitIndexRedistributeAlwaysFeasible(t *testing.T) {
	b, scratch, cfg := splitTestBuilder()
	sizes := []int{40, 1300, 40, 40, 1300, 40, 40, 1300, 40, 40}
	entries := make([]page.LeafEntry, len(sizes))
	for i, s := range sizes {
		entries[i] = splitTestEntry(i, s)
	}
	mid, ok := findLeafSplitIndex(b, scratch, cfg, entries)
	if !ok {
		t.Fatalf("findLeafSplitIndex ok=false for a two-page-feasible entry set")
	}
	if !leafEntriesFit(b, scratch, cfg, entries[:mid]) || !leafEntriesFit(b, scratch, cfg, entries[mid:]) {
		t.Errorf("split at %d produced an oversize half", mid)
	}
}

// TestFindLeafSplitIndexDeterministic pins the determinism invariant
// (page-formats.md §Leaf Split): the split is a pure function of the
// inputs, so repeated calls return the same boundary.
func TestFindLeafSplitIndexDeterministic(t *testing.T) {
	b, scratch, cfg := splitTestBuilder()
	entries := []page.LeafEntry{
		splitTestEntry(0, 1200),
		splitTestEntry(1, 1200),
		splitTestEntry(2, 1400),
		splitTestEntry(3, 2800),
	}
	m1, ok1 := findLeafSplitIndex(b, scratch, cfg, entries)
	m2, ok2 := findLeafSplitIndex(b, scratch, cfg, entries)
	if m1 != m2 || ok1 != ok2 {
		t.Errorf("non-deterministic split: (%d,%v) vs (%d,%v)", m1, ok1, m2, ok2)
	}
}

// TestFindLeafSplitIndexNoFeasibleSplit exercises the genuine-unstorable
// branch: three near-2/3-page inline entries have no contiguous
// two-partition that fits (every adjacent pair overflows a page), so the
// function reports ok=false rather than fabricating an oversize half.
func TestFindLeafSplitIndexNoFeasibleSplit(t *testing.T) {
	b, scratch, cfg := splitTestBuilder()
	entries := []page.LeafEntry{
		splitTestEntry(0, 2450),
		splitTestEntry(1, 2450),
		splitTestEntry(2, 2450),
	}
	if _, ok := findLeafSplitIndex(b, scratch, cfg, entries); ok {
		t.Errorf("findLeafSplitIndex ok=true for an entry set with no feasible two-page split")
	}
}
