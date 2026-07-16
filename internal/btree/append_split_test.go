package btree

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// Append-aware split policy (page-formats.md §Leaf Split, append-aware
// policy): a split triggered by an append onto the tree's RIGHTMOST
// leaf is lopsided — existing content packs the left half nearly full —
// while every other split keeps the byte-balanced boundary. These pins
// hold for both compressed layouts and the uncompressed variant.

// leafCapacityOracle measures a packed leaf's entry capacity for the
// given key shape through the real builder — the absolute yardstick
// the fill assertions compare against (a relative-to-max floor is
// self-referential: under a uniformly balanced policy every fill
// halves, max included, and the assertion goes vacuous).
func leafCapacityOracle(cfg page.Config, key func(i int) []byte, val []byte) int {
	buf := make([]byte, cfg.PageSize)
	b := page.NewLeafBuilder(buf, cfg)
	n := 0
	for b.AddInline(key(n), val) {
		n++
	}
	return n
}

// leafFillStats walks the tree's leaves, returning per-leaf entry
// counts in key order.
func leafFillStats(t *testing.T, pr PageReader, cfg page.Config, root uint64) []int {
	t.Helper()
	var counts []int
	err := Walk(pr, cfg, root, ^uint64(0)>>1, func(id uint64, kind PageKind, _ int) error {
		if kind == PageKindLeaf {
			buf, err := pr.Page(id)
			if err != nil {
				return err
			}
			counts = append(counts, page.NewLeafReader(buf, cfg).Count())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return counts
}

// TestPutAscendingPacksLeavesFull: N ascending Puts leave every leaf
// except the rightmost near its capacity — the fill a BulkLoad-style
// packed build achieves — instead of the ~50% a balanced split
// strands. The pin compares against the observed maximum fill: every
// non-rightmost leaf holds at least 60% of the fullest leaf's entries
// (the lopsided group split moves one trailing group right, so "near
// full" is full-minus-one-group, comfortably above 60%; a balanced
// split would leave every left half at ~50% and fail).
func TestPutAscendingPacksLeavesFull(t *testing.T) {
	for _, layout := range []struct {
		name string
		cfg  page.Config
	}{
		{"seg", page.Config{PageSize: 4096}},
		{"ivl", page.Config{PageSize: 4096, LeafLayout: page.LeafLayoutInterleaved}},
		{"uc", page.Config{PageSize: 4096, RestartGroupTarget: 1}},
	} {
		t.Run(layout.name, func(t *testing.T) {
			cfg := layout.cfg
			pw := newFakeWriter(t, 4096)
			root := uint64(0)
			const N = 3000
			for i := range N {
				key := fmt.Appendf(nil, "seq-%08d", i)
				nr, err := Put(pw, cfg, root, key, bytes.Repeat([]byte{'v'}, 40))
				if err != nil {
					t.Fatalf("Put(%d): %v", i, err)
				}
				root = nr
			}
			// Order + reachability stay intact under the lopsided policy.
			if _, _, err := ValidateOrder(pw, cfg, root, ^uint64(0), 0,
				func(kind OrderViolationKind, pageID uint64, msg string) bool {
					t.Errorf("order violation %v at %d: %s", kind, pageID, msg)
					return true
				}); err != nil {
				t.Fatalf("ValidateOrder after ascending puts: %v", err)
			}
			counts := leafFillStats(t, pw, cfg, root)
			if len(counts) < 4 {
				t.Fatalf("fixture too small: %d leaves", len(counts))
			}
			// Absolute floor: 70% of a packed leaf's capacity for this
			// key shape (the lopsided carve moves one trailing group
			// right, so left halves sit at full-minus-one-group; a
			// balanced split leaves every left half near 50% and fails).
			capacity := leafCapacityOracle(cfg, func(i int) []byte { return fmt.Appendf(nil, "seq-%08d", i) }, bytes.Repeat([]byte{'v'}, 40))
			floor := capacity * 70 / 100
			for i, c := range counts[:len(counts)-1] { // rightmost may be partial
				if c < floor {
					t.Errorf("leaf %d/%d holds %d entries < %d (70%% of packed capacity %d) — ascending inserts stranded a half-full page",
						i, len(counts), c, floor, capacity)
				}
			}
			// Every key still readable.
			for i := 0; i < N; i += 97 {
				key := fmt.Appendf(nil, "seq-%08d", i)
				if _, found, err := Get(pw, cfg, root, key); err != nil || !found {
					t.Fatalf("Get(%d): found=%v err=%v", i, found, err)
				}
			}
		})
	}
}

// TestPutMidTreeAppendStaysBalanced: an append onto a leaf that is NOT
// the tree's rightmost (its key range is bounded above by a parent
// separator) keeps the byte-balanced split — the lopsided policy is
// gated on the rightmost path, because a mid-tree lopsided split
// strands a nearly-empty right page in a range that may never see
// another ascending run.
func TestPutMidTreeAppendStaysBalanced(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	// Two clusters: "a…" then "z…" (both built by ascending appends, so
	// they pack). The boundary leaf — first key "a…", holding the a-tail
	// — also absorbs the first z-keys; delete those so its LAST key is
	// the a-maximum. Appends of new a-keys then hit an append-position
	// insert on a leaf that is NOT the tree's rightmost (the z-leaves
	// bound it above).
	const N = 400
	for i := range N {
		nr, err := Put(pw, cfg, root, fmt.Appendf(nil, "a-%06d0", i), bytes.Repeat([]byte{'v'}, 40))
		if err != nil {
			t.Fatalf("Put a(%d): %v", i, err)
		}
		root = nr
	}
	for i := range 150 {
		nr, err := Put(pw, cfg, root, fmt.Appendf(nil, "z-%06d", i), bytes.Repeat([]byte{'v'}, 40))
		if err != nil {
			t.Fatalf("Put z(%d): %v", i, err)
		}
		root = nr
	}
	// Find the boundary leaf's z-members and delete them (few enough to
	// stay above the merge threshold, so the leaf survives a-pure).
	var boundaryZ [][]byte
	if err := Walk(pw, cfg, root, ^uint64(0)>>1, func(id uint64, kind PageKind, _ int) error {
		if kind != PageKindLeaf {
			return nil
		}
		buf, err := pw.Page(id)
		if err != nil {
			return err
		}
		r := page.NewLeafReader(buf, cfg)
		first, _ := r.EntryAt(0, nil)
		if first.Key[0] != 'a' {
			return nil
		}
		it := r.IterForReuse(nil, nil, nil)
		for {
			e, ok := it.Next()
			if !ok {
				return nil
			}
			if e.Key[0] == 'z' {
				boundaryZ = append(boundaryZ, bytes.Clone(e.Key))
			}
		}
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, k := range boundaryZ {
		nr, err := Delete(pw, cfg, root, 25, k)
		if err != nil {
			t.Fatalf("Delete(%q): %v", k, err)
		}
		root = nr
	}

	// Fill the boundary leaf until it splits: every insert appends past
	// its last key (the a-maximum) while z-leaves make it non-rightmost.
	before := len(leafFillStats(t, pw, cfg, root))
	i := 0
	for len(leafFillStats(t, pw, cfg, root)) == before {
		nr, err := Put(pw, cfg, root, fmt.Appendf(nil, "a-%06d0", N+i), bytes.Repeat([]byte{'v'}, 40))
		if err != nil {
			t.Fatalf("Put fill(%d): %v", i, err)
		}
		root = nr
		i++
		if i > 400 {
			t.Fatal("no split after 400 fills")
		}
	}
	counts := leafFillStats(t, pw, cfg, root)
	// The split halves each hold ~50% of a packed leaf under the
	// balanced policy; a lopsided split here would leave the right at
	// one trailing group (~10%). Assert against the absolute capacity
	// oracle: no leaf except the tree's last holds < 30% of a packed
	// leaf (everything else was packed by the ascending phases).
	capacity := leafCapacityOracle(cfg, func(i int) []byte { return fmt.Appendf(nil, "a-%06d0", i) }, bytes.Repeat([]byte{'v'}, 40))
	for i, c := range counts[:len(counts)-1] {
		if c < capacity*30/100 {
			t.Errorf("leaf %d/%d holds %d entries < 30%% of packed capacity %d — a mid-tree append split went lopsided",
				i, len(counts), c, capacity)
		}
	}
}

// TestFindLeafSplitIndexAppendRightmost pins the decode-split
// (slow-path) lopsided boundary: with appendRightmost the boundary is
// the MAXIMAL fitting left prefix — the left half packs as full as the
// encoder allows — while the same input without the flag picks a
// balanced interior boundary strictly to its left.
func TestFindLeafSplitIndexAppendRightmost(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	entries := make([]page.LeafEntry, 40)
	for i := range entries {
		entries[i] = page.LeafEntry{
			Key:   fmt.Appendf(nil, "key-%06d", i),
			Value: bytes.Repeat([]byte{'v'}, 120),
		}
	}
	scratch := make([]byte, cfg.PageSize)
	b := page.NewLeafBuilder(scratch, cfg)

	// The maximal fitting prefix, measured with the same encoder.
	wantMid := 0
	b.Reset(scratch, cfg)
	for i := range entries {
		if !b.AddEntry(entries[i]) {
			break
		}
		wantMid = i + 1
	}
	if wantMid >= len(entries) {
		t.Fatal("fixture does not overflow one page")
	}

	mid, ok := findLeafSplitIndex(b, scratch, cfg, entries, true)
	if !ok || mid != wantMid {
		t.Errorf("appendRightmost: mid=%d ok=%v, want %d/true (maximal fitting left prefix)", mid, ok, wantMid)
	}
	balMid, ok := findLeafSplitIndex(b, scratch, cfg, entries, false)
	if !ok || balMid >= mid || balMid < 2 {
		t.Errorf("balanced: mid=%d ok=%v, want an interior boundary strictly left of the lopsided %d", balMid, ok, mid)
	}
}

// TestPutAscendingPacksBranchesFull: the lopsided policy applies at
// branch levels too — an ascending workload deep enough for multiple
// branch splits leaves every non-spine branch near the maximum
// observed fanout (a balanced branch split would strand every left
// branch at ~50%).
func TestPutAscendingPacksBranchesFull(t *testing.T) {
	// Plain branch layout + a long shared key prefix: separators
	// minimize past the prefix, so each carries ~200 stored bytes and
	// branch fanout stays small enough that 3000 ascending inserts
	// force several branch splits. (The segregated branch would store
	// the prefix once and defeat the fixture; the policy under test is
	// layout-independent.)
	cfg := page.Config{PageSize: 4096, BranchLayout: page.BranchLayoutPlain}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	const N = 3000
	prefix := bytes.Repeat([]byte{'p'}, 200)
	for i := range N {
		key := fmt.Appendf(nil, "%s%08d", prefix, i)
		nr, err := Put(pw, cfg, root, key, bytes.Repeat([]byte{'v'}, 40))
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
		root = nr
	}
	if _, _, err := ValidateOrder(pw, cfg, root, ^uint64(0), 0,
		func(kind OrderViolationKind, pageID uint64, msg string) bool {
			t.Errorf("order violation %v at %d: %s", kind, pageID, msg)
			return true
		}); err != nil {
		t.Fatalf("ValidateOrder: %v", err)
	}
	var fanouts []int
	err := Walk(pw, cfg, root, ^uint64(0)>>1, func(id uint64, kind PageKind, depth int) error {
		if kind == PageKindBranch && depth > 0 { // exclude the root (legitimately partial)
			buf, err := pw.Page(id)
			if err != nil {
				return err
			}
			fanouts = append(fanouts, int(page.BranchCellCount(buf)))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(fanouts) < 3 {
		t.Fatalf("fixture built only %d non-root branches; cannot assess branch fill", len(fanouts))
	}
	// Absolute capacity oracle: pack this fixture's own separator
	// shape (prefix + divergent digits, as the splits minimize them)
	// into one branch via the real encoder. A relative-to-max floor
	// would go vacuous when a balanced policy halves every fanout,
	// max included.
	// (prefix declared by the fixture above)
	oracleBuf := make([]byte, cfg.PageSize)
	capacity := 0
	var cells []page.BranchCell
	for {
		cells = append(cells, page.BranchCell{Key: fmt.Appendf(nil, "%s%08d", prefix, capacity), Child: uint64(capacity + 1)})
		if page.EncodeBranch(oracleBuf, cfg, 1, cells) != nil {
			break
		}
		capacity++
	}
	maxFan := 0
	low := 0
	for _, f := range fanouts {
		maxFan = max(maxFan, f)
		if f < capacity*60/100 {
			low++
		}
	}
	if maxFan < capacity*85/100 {
		t.Errorf("fullest non-root branch holds %d cells < 85%% of packed capacity %d — lopsided branch splits are not packing", maxFan, capacity)
	}
	// The rightmost spine (one branch per level) is legitimately
	// partial; everything else must be near-full.
	if low > 2 {
		t.Errorf("%d of %d non-root branches hold <60%% of packed capacity %d (fanouts=%v) — ascending inserts stranded half-full branches",
			low, len(fanouts), capacity, fanouts)
	}
}

// TestPutEntryAscendingPacksLeavesFull: the caller-managed entry path
// (PutEntry — set-keyspace nested trees, index data trees) applies the
// same append-aware lopsided policy as Put: time-ordered index keys
// are exactly the ascending shape that would otherwise strand every
// leaf at ~50%.
func TestPutEntryAscendingPacksLeavesFull(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	const N = 3000
	val := bytes.Repeat([]byte{'v'}, 40)
	for i := range N {
		e := page.LeafEntry{Key: fmt.Appendf(nil, "seq-%08d", i), Value: val}
		nr, _, err := PutEntry(pw, cfg, root, e)
		if err != nil {
			t.Fatalf("PutEntry(%d): %v", i, err)
		}
		root = nr
	}
	counts := leafFillStats(t, pw, cfg, root)
	if len(counts) < 4 {
		t.Fatalf("fixture too small: %d leaves", len(counts))
	}
	capacity := leafCapacityOracle(cfg, func(i int) []byte { return fmt.Appendf(nil, "seq-%08d", i) }, val)
	floor := capacity * 70 / 100
	for i, c := range counts[:len(counts)-1] {
		if c < floor {
			t.Errorf("leaf %d/%d holds %d entries < %d (70%% of packed capacity %d) — PutEntry ascending inserts stranded a half-full page",
				i, len(counts), c, floor, capacity)
		}
	}
}
