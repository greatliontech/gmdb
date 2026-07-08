package btree

import (
	"errors"
	"fmt"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// TestWalkVisitsEveryReachablePageOnce builds a multi-level tree and
// verifies Walk visits every reachable page exactly once, that the root
// is a branch (so the test actually exercises branch descent), and that
// every visited id is an allocated page.
func TestWalkVisitsEveryReachablePageOnce(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pairs := make([][2]string, 0, 500)
	for i := range 500 {
		pairs = append(pairs, [2]string{fmt.Sprintf("key%05d", i), fmt.Sprintf("val%05d", i)})
	}
	root, pw := buildTree(t, cfg, pairs)
	hwm := pw.nextID

	// Root must be a branch for this test to mean anything.
	rootBuf, _ := pw.Page(root)
	if typ, _, _, _ := page.ReadHeader(rootBuf); typ != page.TypeBranch {
		t.Fatalf("root type = %d, want branch (need a multi-level tree)", typ)
	}

	seen := make(map[uint64]int)
	var branches, leaves int
	err := Walk(pw, cfg, root, hwm, func(id uint64, kind PageKind, depth int) error {
		seen[id]++
		switch kind {
		case PageKindBranch:
			branches++
		case PageKindLeaf:
			leaves++
		}
		if _, ok := pw.pages[id]; !ok {
			t.Errorf("Walk visited unallocated page %d", id)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("page %d visited %d times, want 1", id, n)
		}
	}
	if branches < 1 || leaves < 1 {
		t.Errorf("branches=%d leaves=%d, want >=1 of each", branches, leaves)
	}
}

// TestWalkSingleLeaf: a tiny tree (root is a leaf) visits exactly one page.
func TestWalkSingleLeaf(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	root, pw := buildTree(t, cfg, [][2]string{{"a", "A"}, {"b", "B"}})
	hwm := pw.nextID
	var n int
	err := Walk(pw, cfg, root, hwm, func(id uint64, kind PageKind, depth int) error {
		n++
		if kind != PageKindLeaf {
			t.Errorf("kind = %d, want leaf", kind)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if n != 1 {
		t.Errorf("visited %d pages, want 1", n)
	}
}

// TestWalkRejectsOutOfRangeChild (Inv-C1): a forged branch child pointer
// >= HighWaterMark is rejected as ErrCorrupted, never SIGBUS/panic.
func TestWalkRejectsOutOfRangeChild(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pairs := make([][2]string, 0, 500)
	for i := range 500 {
		pairs = append(pairs, [2]string{fmt.Sprintf("key%05d", i), fmt.Sprintf("val%05d", i)})
	}
	root, pw := buildTree(t, cfg, pairs)
	hwm := pw.nextID
	// Forge the root branch's leftmost child to point past HWM.
	rootBuf, _ := pw.Page(root)
	page.SetBranchLeftmostChild(rootBuf, hwm+9999)

	err := Walk(pw, cfg, root, hwm, func(uint64, PageKind, int) error { return nil })
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Walk on forged out-of-range child = %v, want ErrCorrupted", err)
	}
}

// TestWalkRejectsForgedBranchDirectory (Inv-C1): a branch whose cell
// directory points outside the page is rejected by ValidateBranch
// (inside Walk) as ErrCorrupted — no panic from BranchCellAt.
func TestWalkRejectsForgedBranchDirectory(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pairs := make([][2]string, 0, 500)
	for i := range 500 {
		pairs = append(pairs, [2]string{fmt.Sprintf("key%05d", i), fmt.Sprintf("val%05d", i)})
	}
	root, pw := buildTree(t, cfg, pairs)
	hwm := pw.nextID
	buf, _ := pw.Page(root)
	if typ, _, n, _ := page.ReadHeader(buf); typ != page.TypeBranch || n == 0 {
		t.Fatalf("root not a non-empty branch")
	}
	// Corrupt the first cell-directory entry's offset to 0xFFFF (way
	// past ContentEnd) — ValidateBranch must reject without panicking.
	buf[16] = 0xFF
	buf[17] = 0xFF

	err := Walk(pw, cfg, root, hwm, func(uint64, PageKind, int) error { return nil })
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Walk on forged branch directory = %v, want ErrCorrupted", err)
	}
}

// TestWalkKVEnumeratesOverflowAndPlain: WalkKV yields every
// (key,value) in ascending order, assembling overflow values correctly.
func TestWalkKVEnumeratesOverflowAndPlain(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	// Mix small values and one large value that forces an overflow chain.
	big := make([]byte, 9000)
	for i := range big {
		big[i] = byte('A' + i%26)
	}
	pairs := [][2]string{
		{"aaa", "1"},
		{"bbb", string(big)},
		{"ccc", "3"},
	}
	root, pw := buildTree(t, cfg, pairs)
	hwm := pw.nextID

	got := map[string]string{}
	var order []string
	err := WalkKV(pw, cfg, root, hwm, func(k, v []byte) error {
		got[string(k)] = string(v)
		order = append(order, string(k))
		return nil
	})
	if err != nil {
		t.Fatalf("WalkKV: %v", err)
	}
	for _, p := range pairs {
		if got[p[0]] != p[1] {
			t.Errorf("key %q = %d bytes, want %d", p[0], len(got[p[0]]), len(p[1]))
		}
	}
	if len(order) != 3 || order[0] != "aaa" || order[1] != "bbb" || order[2] != "ccc" {
		t.Errorf("order = %v, want [aaa bbb ccc]", order)
	}
}

// TestWalkKVEmptyAndForged: empty tree is a no-op; a forged out-of-range
// child is rejected (no panic).
func TestWalkKVEmptyAndForged(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	if err := WalkKV(nil, cfg, 0, 100, func([]byte, []byte) error { return nil }); err != nil {
		t.Errorf("WalkKV(root=0) = %v, want nil", err)
	}
	pairs := make([][2]string, 0, 500)
	for i := range 500 {
		pairs = append(pairs, [2]string{fmt.Sprintf("k%05d", i), "v"})
	}
	root, pw := buildTree(t, cfg, pairs)
	hwm := pw.nextID
	rootBuf, _ := pw.Page(root)
	page.SetBranchLeftmostChild(rootBuf, hwm+5)
	err := WalkKV(pw, cfg, root, hwm, func([]byte, []byte) error { return nil })
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("WalkKV forged child = %v, want ErrCorrupted", err)
	}
}

// TestWalkVisitorErrorPropagates: a visitor error aborts the walk and is
// returned verbatim.
func TestWalkVisitorErrorPropagates(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	root, pw := buildTree(t, cfg, [][2]string{{"a", "A"}})
	hwm := pw.nextID
	sentinel := errors.New("stop")
	err := Walk(pw, cfg, root, hwm, func(uint64, PageKind, int) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("Walk = %v, want sentinel", err)
	}
}
