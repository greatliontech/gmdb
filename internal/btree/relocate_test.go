package btree

import (
	"bytes"
	"fmt"
	"slices"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// buildTreeForRelocate builds a multi-level B+tree of n keys (100-byte
// values force splits) and returns the root + the expected contents.
func buildTreeForRelocate(t *testing.T, pw *fakeWriter, cfg page.Config, n int) (uint64, map[string]string) {
	t.Helper()
	root := uint64(0)
	want := make(map[string]string, n)
	for i := range n {
		key := fmt.Appendf(nil, "k-%05d", i)
		// 100-byte values stay below the overflow-promotion threshold, so
		// the reachable page set is tree pages only (no overflow chains).
		// TestRelocatePagesFullRoundTrip's moved == len(before) relies on
		// this; overflow survival is covered separately.
		val := bytes.Repeat([]byte{byte('a' + i%26)}, 100)
		nr, err := Put(pw, cfg, root, key, val)
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
		root = nr
		want[string(key)] = string(val)
	}
	return root, want
}

func collectKVForRelocate(t *testing.T, pw PageWriter, cfg page.Config, root uint64) map[string]string {
	t.Helper()
	got := make(map[string]string)
	if err := WalkKV(pw, cfg, root, ^uint64(0), func(k, v []byte) error {
		got[string(k)] = string(v)
		return nil
	}); err != nil {
		t.Fatalf("WalkKV: %v", err)
	}
	return got
}

func assertSameKV(t *testing.T, want, got map[string]string) {
	t.Helper()
	if len(want) != len(got) {
		t.Errorf("kv count: got %d, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q: got %q, want %q", k, got[k], v)
		}
	}
}

// TestRelocatePagesFullRoundTrip relocates EVERY page (predicate always
// true): the result is a structurally fresh tree with identical contents,
// a new root, every old page retired, and no new page reusing an old id.
func TestRelocatePagesFullRoundTrip(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root, want := buildTreeForRelocate(t, pw, cfg, 200)

	before := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, root, before)
	if len(before) < 3 {
		t.Fatalf("tree too small (%d pages) to exercise multi-level relocation", len(before))
	}

	newRoot, moved, err := RelocatePages(pw, cfg, root, func(uint64) bool { return true }, 1<<30)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	if newRoot == root {
		t.Errorf("root id unchanged (%d) after full relocation", newRoot)
	}
	if moved != len(before) {
		t.Errorf("moved=%d, want %d (every page eligible)", moved, len(before))
	}
	assertSameKV(t, want, collectKVForRelocate(t, pw, cfg, newRoot))

	for id := range before {
		if _, freed := pw.freed[id]; !freed {
			t.Errorf("old page %d not retired after full relocation", id)
		}
	}
	after := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, newRoot, after)
	if len(after) != len(before) {
		t.Errorf("page count changed: %d → %d (structure not preserved)", len(before), len(after))
	}
	for id := range after {
		if _, old := before[id]; old {
			t.Errorf("new tree still references old page id %d", id)
		}
		if _, freed := pw.freed[id]; freed {
			t.Errorf("new tree references a retired page id %d", id)
		}
	}
}

// TestRelocatePagesPreservesOverflowLeaf: relocating a leaf that owns an
// overflow chain leaves the chain — which this primitive deliberately does
// NOT relocate — reachable, and the large value intact. Pins the doc
// claim that a leaf's overflow refs survive a verbatim leaf relocation.
func TestRelocatePagesPreservesOverflowLeaf(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	bigKey := []byte("big")
	bigVal := bytes.Repeat([]byte{0x5A}, 6000) // > page size ⇒ overflow chain
	if !NeedsOverflow(cfg, bigKey, bigVal) {
		t.Fatalf("test precondition: value of %d bytes does not overflow", len(bigVal))
	}
	root, err := Put(pw, cfg, 0, bigKey, bigVal)
	if err != nil {
		t.Fatalf("Put big: %v", err)
	}
	for i := range 40 { // grow a real multi-level tree around the overflow leaf
		nr, err := Put(pw, cfg, root, fmt.Appendf(nil, "s-%03d", i), []byte("v"))
		if err != nil {
			t.Fatalf("Put small %d: %v", i, err)
		}
		root = nr
	}

	newRoot, moved, err := RelocatePages(pw, cfg, root, func(uint64) bool { return true }, 1<<30)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	if moved == 0 {
		t.Fatal("nothing relocated")
	}
	// The relocated leaf's overflow ref still points at the (un-relocated)
	// chain, so the large value round-trips.
	got, found, err := Get(pw, cfg, newRoot, bigKey)
	if err != nil || !found || !bytes.Equal(got, bigVal) {
		t.Errorf("big value after relocation: found=%v err=%v len(got)=%d want %d", found, err, len(got), len(bigVal))
	}
}

// TestRelocatePagesBudgetBound: a small maxMoves caps eligible relocations
// while leaving the tree fully consistent (partial relocation round-trips,
// and some original pages survive un-relocated).
func TestRelocatePagesBudgetBound(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root, want := buildTreeForRelocate(t, pw, cfg, 200)
	before := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, root, before)

	const maxMoves = 3
	newRoot, moved, err := RelocatePages(pw, cfg, root, func(uint64) bool { return true }, maxMoves)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	if moved > maxMoves {
		t.Errorf("moved=%d exceeds maxMoves=%d", moved, maxMoves)
	}
	if moved == 0 {
		t.Error("moved=0; expected partial relocation")
	}
	assertSameKV(t, want, collectKVForRelocate(t, pw, cfg, newRoot))

	after := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, newRoot, after)
	survivors := 0
	for id := range after {
		if _, old := before[id]; old {
			survivors++
		}
	}
	if survivors == 0 {
		t.Errorf("expected un-relocated original pages to survive with maxMoves=%d", maxMoves)
	}
}

// TestRelocatePagesTargeted: a predicate matching only the upper half of
// the page-id space relocates exactly those pages (ancestor pointer-fix
// CoWs are mandatory and uncounted), preserves contents, retires the
// targeted originals, and leaves no targeted old id in the new tree.
func TestRelocatePagesTargeted(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root, want := buildTreeForRelocate(t, pw, cfg, 200)
	before := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, root, before)

	ids := make([]uint64, 0, len(before))
	for id := range before {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	threshold := ids[len(ids)/2]
	eligible := 0
	for _, id := range ids {
		if id > threshold {
			eligible++
		}
	}
	if eligible == 0 {
		t.Fatalf("no eligible pages above threshold %d (tree too small)", threshold)
	}

	newRoot, moved, err := RelocatePages(pw, cfg, root, func(id uint64) bool { return id > threshold }, 1<<30)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	if moved != eligible {
		t.Errorf("moved=%d, want %d (pages with id > %d)", moved, eligible, threshold)
	}
	assertSameKV(t, want, collectKVForRelocate(t, pw, cfg, newRoot))

	for id := range before {
		_, freed := pw.freed[id]
		if id > threshold && !freed {
			t.Errorf("targeted page %d not retired", id)
		}
	}
	after := map[uint64]struct{}{}
	collectReachable(t, pw, cfg, newRoot, after)
	for id := range after {
		if _, old := before[id]; old && id > threshold {
			t.Errorf("targeted old page %d still referenced by the new tree", id)
		}
	}
}

// TestRelocatePagesSingleLeaf: a one-page (root leaf) tree relocates to a
// fresh id with contents intact and the old leaf retired.
func TestRelocatePagesSingleLeaf(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root, err := Put(pw, cfg, 0, []byte("k"), []byte("v"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	newRoot, moved, err := RelocatePages(pw, cfg, root, func(uint64) bool { return true }, 10)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	if moved != 1 || newRoot == root {
		t.Errorf("single-leaf: moved=%d newRoot=%d (root=%d), want moved=1 and a new root", moved, newRoot, root)
	}
	got, found, err := Get(pw, cfg, newRoot, []byte("k"))
	if err != nil || !found || !bytes.Equal(got, []byte("v")) {
		t.Errorf("Get after relocate: got=%q found=%v err=%v", got, found, err)
	}
	if _, freed := pw.freed[root]; !freed {
		t.Errorf("old root leaf %d not retired", root)
	}
}

// TestRelocatePagesEmpty: an empty tree (root 0) is a no-op.
func TestRelocatePagesEmpty(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	nr, moved, err := RelocatePages(pw, cfg, 0, func(uint64) bool { return true }, 10)
	if err != nil || nr != 0 || moved != 0 {
		t.Errorf("empty tree: nr=%d moved=%d err=%v, want 0,0,nil", nr, moved, err)
	}
}
