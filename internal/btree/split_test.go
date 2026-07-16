package btree

import (
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
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
// regression for the byte-balanced split (page-formats.md §Leaf Split).
// A size-skewed leaf whose entry-count
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

	mid, ok := findLeafSplitIndex(b, scratch, cfg, entries, false)
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
	mid, ok := findLeafSplitIndex(b, scratch, cfg, entries, false)
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
	for _, appendRightmost := range []bool{false, true} {
		m1, ok1 := findLeafSplitIndex(b, scratch, cfg, entries, appendRightmost)
		m2, ok2 := findLeafSplitIndex(b, scratch, cfg, entries, appendRightmost)
		if m1 != m2 || ok1 != ok2 {
			t.Errorf("non-deterministic split (appendRightmost=%v): (%d,%v) vs (%d,%v)", appendRightmost, m1, ok1, m2, ok2)
		}
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
	if _, ok := findLeafSplitIndex(b, scratch, cfg, entries, false); ok {
		t.Errorf("findLeafSplitIndex ok=true for an entry set with no feasible two-page split")
	}
}

// branchTestCell builds a branch cell with a key of keyLen bytes whose
// first two bytes are (lead, idx). Callers pick `lead` so the cells sort:
// a lower lead sorts before a higher one regardless of length. Child is a
// distinct non-zero page ID derived from the key bytes so the finder's
// boundary choice is observable via the lifted cell.
func branchTestCell(lead byte, idx int, keyLen int) page.BranchCell {
	key := make([]byte, keyLen)
	key[0] = lead
	if keyLen > 1 {
		key[1] = byte(idx)
	}
	return page.BranchCell{Key: key, Child: uint64(1000 + int(lead)*100 + idx)}
}

// skewedBranchCells returns a size-skewed cell set reachable as `newCells`
// in ascendWithSplit / `combined` in mergeOrRedistributeBranches: three
// ~1400-byte separators (lead 0x10) followed by three tiny ones (lead
// 0x90). At PageSize 4096 (ContentEnd 4096) the cell costs are 1412 and
// 14 bytes (len(key)+12); a valid ≤1-page branch holds 2 bigs + 3 smalls
// (16 + 2·1412 + 3·14 = 2882 ≤ 4096), so inserting a third big separator
// is an in-spec mutation. The entry-count midpoint (idx 3) then clusters
// all three bigs on the left half (16 + 3·1412 = 4252 > 4096), the exact
// count-midpoint branch-split fault (page-formats.md §Separator
// Branch Keys).
func skewedBranchCells() []page.BranchCell {
	return []page.BranchCell{
		branchTestCell(0x10, 0, 1400),
		branchTestCell(0x10, 1, 1400),
		branchTestCell(0x10, 2, 1400),
		branchTestCell(0x90, 0, 2),
		branchTestCell(0x90, 1, 2),
		branchTestCell(0x90, 2, 2),
	}
}

// branchHalvesFit reports whether both halves of a lift split at mid
// (left=cells[:mid], right=cells[mid+1:], cells[mid] lifted) encode within
// one page — the property findBranchSplitIndex must guarantee.
func branchHalvesFit(cfg page.Config, cells []page.BranchCell, mid int) bool {
	ce := cfg.ContentEnd()
	return page.BranchEncodedSize(cfg, cells[:mid]) <= ce &&
		page.BranchEncodedSize(cfg, cells[mid+1:]) <= ce
}

// TestBranchCountSplitFaultDemonstration is the demonstrated-fault anchor
// for the branch count-midpoint split fault (page-formats.md
// §Separator Computation). It proves,
// against page.BranchEncodedSize alone, that the entry-COUNT midpoint
// (`mid := len(cells)/2`, what put.go/delete.go chose) places more than one
// page of cells on a half for a reachable cell set, so the count split
// overflows where a byte-balanced split would fit. Runnable on HEAD before
// the fix lands; findBranchSplitIndex must avoid exactly this.
func TestBranchCountSplitFaultDemonstration(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	cells := skewedBranchCells()
	ce := cfg.ContentEnd()

	countMid := len(cells) / 2
	left := page.BranchEncodedSize(cfg, cells[:countMid])
	if left <= ce {
		t.Fatalf("precondition: count-midpoint left half (%d cells) is %d bytes, expected > ContentEnd %d — reshape", countMid, left, ce)
	}
	t.Logf("FAULT: count-midpoint left half [:%d] = %d bytes > ContentEnd %d (the count split overflows a page)", countMid, left, ce)

	// A byte-balanced boundary exists: lift cell 1, left=[:1], right=[2:].
	if !branchHalvesFit(cfg, cells, 1) {
		t.Fatalf("expected a feasible byte-balanced split at mid=1")
	}
}

// TestFindBranchSplitIndexFeasibleWhenCountMidpointFails is the branch
// regression mirroring TestFindLeafSplitIndexFeasibleWhenCountMidpointFails:
// on the size-skewed cell set whose count midpoint overflows a half,
// findBranchSplitIndex must return a feasible byte-balanced boundary (both
// halves fit one page).
func TestFindBranchSplitIndexFeasibleWhenCountMidpointFails(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	cells := skewedBranchCells()

	// Precondition: the count midpoint (the old choice) is genuinely
	// infeasible, so this exercises the byte-balanced path.
	if branchHalvesFit(cfg, cells, len(cells)/2) {
		t.Fatalf("precondition: count-midpoint split unexpectedly fits — reshape cell sizes")
	}

	mid, ok := findBranchSplitIndex(cfg, cells)
	if !ok {
		t.Fatalf("findBranchSplitIndex ok=false for a splittable size-skewed branch")
	}
	if mid < 1 || mid > len(cells)-1 {
		t.Fatalf("mid=%d out of range [1,%d]", mid, len(cells)-1)
	}
	if !branchHalvesFit(cfg, cells, mid) {
		t.Errorf("split at mid=%d produced an oversize half", mid)
	}
}

// TestFindBranchSplitIndexRedistributeAlwaysFeasible pins the
// redistribute-path invariant: cells arriving from two valid sibling
// branches (one underflowing, so the combined content is below two pages)
// always yield a feasible split regardless of size skew. A mix of long and
// short separators summing to ~1.5 pages stands in for that combined list.
func TestFindBranchSplitIndexRedistributeAlwaysFeasible(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	// Sorted: a block of long (lead 0x10) then short (lead 0x90) cells,
	// interleaving lengths to skew the byte distribution. ~1.5 pages total.
	cells := []page.BranchCell{
		branchTestCell(0x10, 0, 1400),
		branchTestCell(0x10, 1, 40),
		branchTestCell(0x10, 2, 1400),
		branchTestCell(0x10, 3, 40),
		branchTestCell(0x90, 0, 1400),
		branchTestCell(0x90, 1, 40),
		branchTestCell(0x90, 2, 40),
	}
	mid, ok := findBranchSplitIndex(cfg, cells)
	if !ok {
		t.Fatalf("findBranchSplitIndex ok=false for a two-page-feasible cell set")
	}
	if !branchHalvesFit(cfg, cells, mid) {
		t.Errorf("split at mid=%d produced an oversize half", mid)
	}
	// Neither half is empty (redistribute must yield two real siblings).
	if mid < 1 || mid > len(cells)-1 {
		t.Fatalf("mid=%d out of range", mid)
	}
	if mid == len(cells)-1 {
		t.Errorf("redistribute split lifted the last cell, leaving an empty right branch")
	}
}

// TestFindBranchSplitIndexBalancedHalvesClearFloor pins the fill-floor
// benefit (range-delete.md §Invariants) at the boundary-choice level: where
// a balanced split is reachable, byte-balancing keeps BOTH halves above
// MergeThreshold. Nine uniform ~600-byte cells (combined ~1.35 pages) split
// 4 / lift / 4 → each half ~60% of a page, clear of the 50% maximum
// threshold. (A count midpoint would land the same boundary here; the point
// is that the byte-balanced finder does not undershoot the floor when it is
// reachable — the redistribute-path guarantee.)
func TestFindBranchSplitIndexBalancedHalvesClearFloor(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	cells := make([]page.BranchCell, 9)
	for i := range cells {
		cells[i] = branchTestCell(0x10, i, 600)
	}
	mid, ok := findBranchSplitIndex(cfg, cells)
	if !ok {
		t.Fatalf("findBranchSplitIndex ok=false for a balanced-feasible set")
	}
	ce := cfg.ContentEnd()
	floor := int(MaxMergeThreshold) * ce / 100 // 50% of ContentEnd
	leftSize := page.BranchEncodedSize(cfg, cells[:mid])
	rightSize := page.BranchEncodedSize(cfg, cells[mid+1:])
	if leftSize < floor || rightSize < floor {
		t.Errorf("byte-balanced halves undershoot the fill-floor: left=%d right=%d floor=%d (mid=%d)",
			leftSize, rightSize, floor, mid)
	}
}

// TestFindBranchSplitIndexDeterministic pins the determinism invariant
// (page-formats.md §Leaf Split): the boundary is a pure function of the
// inputs, so repeated calls return the same mid.
func TestFindBranchSplitIndexDeterministic(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	cells := skewedBranchCells()
	m1, ok1 := findBranchSplitIndex(cfg, cells)
	m2, ok2 := findBranchSplitIndex(cfg, cells)
	if m1 != m2 || ok1 != ok2 {
		t.Errorf("non-deterministic split: (%d,%v) vs (%d,%v)", m1, ok1, m2, ok2)
	}
}

// TestFindBranchSplitIndexLowerIndexTiebreak pins the tiebreak: when two
// adjacent boundaries are equidistant from the byte-balance point, the
// lower-index boundary wins. A symmetric, uniform-size cell set with an
// even cell count places the balance point between two boundaries.
func TestFindBranchSplitIndexLowerIndexTiebreak(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	// Four uniform cells: lifting cell 1 (left=[c0], right=[c2,c3]) vs cell 2
	// (left=[c0,c1], right=[c3]) are mirror images — equal imbalance. The
	// lower index (1) must win.
	cells := []page.BranchCell{
		branchTestCell(0x10, 0, 200),
		branchTestCell(0x10, 1, 200),
		branchTestCell(0x10, 2, 200),
		branchTestCell(0x10, 3, 200),
	}
	mid, ok := findBranchSplitIndex(cfg, cells)
	if !ok {
		t.Fatalf("findBranchSplitIndex ok=false for a trivially feasible set")
	}
	if mid != 1 {
		t.Errorf("tiebreak: got mid=%d, want the lower-index boundary 1", mid)
	}
}

// TestFindBranchSplitIndexNoFeasibleSplit exercises the genuine-unstorable
// branch: six ~1400-byte separators (~2.07 pages) have no contiguous
// two-partition that fits — every boundary leaves three big cells on a side
// — so the function reports ok=false rather than fabricating an oversize
// half. (Not reachable as a real split input, which is bounded below two
// pages; it exercises the ok=false defense-in-depth path the callers map to
// ErrKeyTooLarge / ErrCorrupted.)
func TestFindBranchSplitIndexNoFeasibleSplit(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	cells := make([]page.BranchCell, 6)
	for i := range cells {
		cells[i] = branchTestCell(0x10, i, 1400)
	}
	if _, ok := findBranchSplitIndex(cfg, cells); ok {
		t.Errorf("findBranchSplitIndex ok=true for a cell set with no feasible two-page split")
	}
}

// TestFindBranchSplitIndexTwoCells covers the split-path edge (n=2): two
// near-max separators that overflow one page split with the boundary cell
// lifted, leaving a one-cell left and an empty right — valid and reachable
// only from a Put-driven branch split, never from redistribute (guarded by
// len(combined) >= 3).
func TestFindBranchSplitIndexTwoCells(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	cells := []page.BranchCell{
		branchTestCell(0x10, 0, 2000),
		branchTestCell(0x10, 1, 2000),
	}
	mid, ok := findBranchSplitIndex(cfg, cells)
	if !ok {
		t.Fatalf("findBranchSplitIndex ok=false for two single-page-fitting cells")
	}
	if mid != 1 {
		t.Errorf("n=2 split: got mid=%d, want 1", mid)
	}
	if !branchHalvesFit(cfg, cells, mid) {
		t.Errorf("n=2 split at mid=%d produced an oversize half", mid)
	}
}
