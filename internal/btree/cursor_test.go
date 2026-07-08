package btree

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// buildTree puts every (key, value) in pairs into a fresh tree
// against the given cfg and returns the rootID + pw. Helper for
// cursor tests so each test reads as the navigation matrix rather
// than the setup boilerplate.
func buildTree(t *testing.T, cfg page.Config, pairs [][2]string) (uint64, *fakeWriter) {
	t.Helper()
	pw := newFakeWriter(t, cfg.PageSize)
	root := uint64(0)
	for _, p := range pairs {
		nr, err := Put(pw, cfg, root, []byte(p[0]), []byte(p[1]))
		if err != nil {
			t.Fatalf("Put(%q): %v", p[0], err)
		}
		root = nr
	}
	return root, pw
}

func TestCursorUnpositionedStateMachine(t *testing.T) {
	// transactions.md §Cursor State Machine spec-tier invariant
	// (kind=clause-explicit): Unpositioned cursors return (nil,nil)
	// from Current and ErrCursorUnpositioned from Err — distinct
	// from End-of-iteration (Err == nil).
	cfg := page.Config{PageSize: 4096}
	root, pw := buildTree(t, cfg, [][2]string{{"a", "A"}, {"b", "B"}})
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	if k, v := c.Current(); k != nil || v != nil {
		t.Errorf("Unpositioned Current = (%q, %q); want (nil, nil)", k, v)
	}
	if err := c.Err(); !errors.Is(err, ErrCursorUnpositioned) {
		t.Errorf("Unpositioned Err = %v; want ErrCursorUnpositioned", err)
	}

	// After First — Positioned; Err() is nil.
	k, _ := c.First()
	if c.state != csPositioned {
		t.Errorf("post-First state = %v; want csPositioned", c.state)
	}
	if err := c.Err(); err != nil {
		t.Errorf("post-First Err = %v; want nil", err)
	}
	if !bytes.Equal(k, []byte("a")) {
		t.Errorf("First key = %q; want %q", k, "a")
	}

	// Advance past the end — End-of-iteration; Err() still nil.
	c.Next() // → b
	if k, _ := c.Next(); k != nil {
		t.Errorf("post-end Next key = %q; want nil", k)
	}
	if c.state != csEndOfIteration {
		t.Errorf("post-end state = %v; want csEndOfIteration", c.state)
	}
	if err := c.Err(); err != nil {
		t.Errorf("End-of-iteration Err = %v; want nil (distinct from Unpositioned)", err)
	}
}

func TestCursorFirstLastSingleLeaf(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	root, pw := buildTree(t, cfg, [][2]string{
		{"alpha", "A"}, {"beta", "B"}, {"gamma", "G"}, {"delta", "D"},
	})
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	k, v := c.First()
	if !bytes.Equal(k, []byte("alpha")) || !bytes.Equal(v, []byte("A")) {
		t.Errorf("First = (%q, %q); want (alpha, A)", k, v)
	}
	k, v = c.Last()
	if !bytes.Equal(k, []byte("gamma")) || !bytes.Equal(v, []byte("G")) {
		t.Errorf("Last = (%q, %q); want (gamma, G)", k, v)
	}
}

func TestCursorForwardWalkSingleLeaf(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	want := [][2]string{
		{"alpha", "A"}, {"beta", "B"}, {"delta", "D"}, {"gamma", "G"},
	}
	root, pw := buildTree(t, cfg, want)
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	got := drain(c)
	if !slices.Equal(got, want) {
		t.Errorf("forward walk = %v; want %v", got, want)
	}
}

func TestCursorBackwardWalkSingleLeaf(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pairs := [][2]string{
		{"alpha", "A"}, {"beta", "B"}, {"delta", "D"}, {"gamma", "G"},
	}
	root, pw := buildTree(t, cfg, pairs)
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	got := drainBackward(c)
	want := reverse(pairs)
	if !slices.Equal(got, want) {
		t.Errorf("backward walk = %v; want %v", got, want)
	}
}

func TestCursorForwardWalkMultiLeaf(t *testing.T) {
	// Force a multi-leaf tree via large values, then walk start-to-
	// end and pin that every key surfaces exactly once in sorted
	// order — exercising the leaf-transition code in
	// advanceToNextLeaf.
	cfg := page.Config{PageSize: 4096}
	const N = 200
	pairs := make([][2]string, N)
	for i := range N {
		pairs[i] = [2]string{fmt.Sprintf("k-%05d", i), fmt.Sprintf("v-%05d-%s", i, bytes.Repeat([]byte{'x'}, 60))}
	}
	root, pw := buildTree(t, cfg, pairs)
	if treeDepth(t, pw, root) < 1 {
		t.Fatalf("setup: tree depth = 0; need at least 1 for multi-leaf walk")
	}
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	got := drain(c)
	if len(got) != N {
		t.Fatalf("walk yielded %d entries; want %d", len(got), N)
	}
	for i, p := range got {
		if p != pairs[i] {
			t.Errorf("walk[%d] = %v; want %v", i, p, pairs[i])
		}
	}
}

func TestCursorBackwardWalkMultiLeaf(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	const N = 200
	pairs := make([][2]string, N)
	for i := range N {
		pairs[i] = [2]string{fmt.Sprintf("k-%05d", i), fmt.Sprintf("v-%05d-%s", i, bytes.Repeat([]byte{'x'}, 60))}
	}
	root, pw := buildTree(t, cfg, pairs)
	if treeDepth(t, pw, root) < 1 {
		t.Fatalf("setup: tree depth = 0; need at least 1 for multi-leaf backward walk")
	}
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	got := drainBackward(c)
	want := reverse(pairs)
	if len(got) != N {
		t.Fatalf("backward walk yielded %d entries; want %d", len(got), N)
	}
	for i, p := range got {
		if p != want[i] {
			t.Errorf("backward walk[%d] = %v; want %v", i, p, want[i])
		}
	}
}

func TestCursorRestartGroupBoundaryStress(t *testing.T) {
	// Compressed leaves at small RestartGroupTarget force many
	// restart-group boundaries. Pin that forward + backward walk
	// produce the same set in opposite order — exercises the
	// LeafIter's group-boundary loadBufferedGroup transitions.
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 3}
	const N = 50
	pairs := make([][2]string, N)
	for i := range N {
		pairs[i] = [2]string{fmt.Sprintf("prefix-key-%04d", i), fmt.Sprintf("v%d", i)}
	}
	root, pw := buildTree(t, cfg, pairs)
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	fwd := drain(c)
	bwd := drainBackward(c)
	if len(fwd) != N || len(bwd) != N {
		t.Fatalf("fwd=%d bwd=%d; want both %d", len(fwd), len(bwd), N)
	}
	for i := range fwd {
		if fwd[i] != bwd[N-1-i] {
			t.Errorf("at i=%d: fwd=%v bwd-mirror=%v", i, fwd[i], bwd[N-1-i])
		}
	}
}

func TestCursorSeekExact(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pairs := [][2]string{
		{"a", "A"}, {"c", "C"}, {"e", "E"}, {"g", "G"}, {"i", "I"},
	}
	root, pw := buildTree(t, cfg, pairs)
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	// Hits.
	for _, p := range pairs {
		k, v := c.Seek([]byte(p[0]))
		if !bytes.Equal(k, []byte(p[0])) || !bytes.Equal(v, []byte(p[1])) {
			t.Errorf("Seek(%q) = (%q, %q); want (%q, %q)", p[0], k, v, p[0], p[1])
		}
	}
	// Misses (between, before-all, after-all).
	for _, miss := range []string{"b", "d", "f", "h", "j", "", "z"} {
		k, v := c.Seek([]byte(miss))
		if k != nil || v != nil {
			t.Errorf("Seek miss %q = (%q, %q); want (nil, nil)", miss, k, v)
		}
		if c.state != csEndOfIteration {
			t.Errorf("Seek miss %q state = %v; want csEndOfIteration", miss, c.state)
		}
		if err := c.Err(); err != nil {
			t.Errorf("Seek miss %q Err = %v; want nil", miss, err)
		}
	}
}

func TestCursorSeekGESuccessor(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pairs := [][2]string{
		{"a", "A"}, {"c", "C"}, {"e", "E"}, {"g", "G"}, {"i", "I"},
	}
	root, pw := buildTree(t, cfg, pairs)
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	// Exact match.
	k, v := c.SeekGE([]byte("c"))
	if !bytes.Equal(k, []byte("c")) || !bytes.Equal(v, []byte("C")) {
		t.Errorf("SeekGE(c) = (%q, %q); want (c, C)", k, v)
	}
	// Between: SeekGE(b) → c; SeekGE(d) → e; etc.
	for _, c2 := range []struct{ target, wantKey, wantVal string }{
		{"", "a", "A"},
		{"b", "c", "C"},
		{"d", "e", "E"},
		{"f", "g", "G"},
		{"h", "i", "I"},
	} {
		k, v := c.SeekGE([]byte(c2.target))
		if !bytes.Equal(k, []byte(c2.wantKey)) || !bytes.Equal(v, []byte(c2.wantVal)) {
			t.Errorf("SeekGE(%q) = (%q, %q); want (%q, %q)", c2.target, k, v, c2.wantKey, c2.wantVal)
		}
	}
	// Past-end.
	if k, v := c.SeekGE([]byte("z")); k != nil || v != nil {
		t.Errorf("SeekGE(z) = (%q, %q); want (nil, nil)", k, v)
	}
	if c.state != csEndOfIteration {
		t.Errorf("SeekGE past-end state = %v; want csEndOfIteration", c.state)
	}
}

func TestCursorSeekGEMultiLeafResumes(t *testing.T) {
	// SeekGE positions at a successor mid-tree; Next/Prev from there
	// stream the remaining entries correctly across leaf boundaries.
	cfg := page.Config{PageSize: 4096}
	const N = 200
	pairs := make([][2]string, N)
	for i := range N {
		pairs[i] = [2]string{fmt.Sprintf("k-%05d", i), fmt.Sprintf("v-%05d-%s", i, bytes.Repeat([]byte{'x'}, 60))}
	}
	root, pw := buildTree(t, cfg, pairs)
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	// SeekGE into the middle; expect to stream the tail.
	target := []byte("k-00100")
	k, _ := c.SeekGE(target)
	if !bytes.Equal(k, target) {
		t.Fatalf("SeekGE(%q) = %q; want exact", target, k)
	}
	got := drainFrom(c)
	if len(got) != N-100 {
		t.Errorf("post-SeekGE walk yielded %d entries; want %d", len(got), N-100)
	}
	if !bytes.Equal([]byte(got[0][0]), target) {
		t.Errorf("first post-SeekGE entry key = %q; want %q", got[0][0], target)
	}
}

func TestCursorCursorDeleteAdvancesToSuccessor(t *testing.T) {
	// Spec-tier invariant (kind=clause-explicit): Cursor.Delete on
	// a Positioned cursor advances to the post-delete successor.
	cfg := page.Config{PageSize: 4096}
	pairs := [][2]string{
		{"a", "A"}, {"b", "B"}, {"c", "C"}, {"d", "D"}, {"e", "E"},
	}
	root, pw := buildTree(t, cfg, pairs)
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	// Position at b.
	c.Seek([]byte("b"))
	if k, _ := c.Current(); !bytes.Equal(k, []byte("b")) {
		t.Fatalf("setup: Seek(b) Current = %q; want b", k)
	}
	if err := c.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Cursor must now be Positioned at c.
	if c.state != csPositioned {
		t.Errorf("post-Delete state = %v; want csPositioned", c.state)
	}
	if k, v := c.Current(); !bytes.Equal(k, []byte("c")) || !bytes.Equal(v, []byte("C")) {
		t.Errorf("post-Delete Current = (%q, %q); want (c, C)", k, v)
	}
	// Next/Prev consistent thereafter.
	if k, _ := c.Next(); !bytes.Equal(k, []byte("d")) {
		t.Errorf("post-Delete-then-Next = %q; want d", k)
	}
	// Verify b is gone.
	_, found, err := Get(pw, cfg, c.RootID(), []byte("b"))
	if err != nil || found {
		t.Errorf("Get(b) post-Delete: found=%v err=%v; want (false, nil)", found, err)
	}
}

func TestCursorDeleteAtLastEntryTransitionsToEnd(t *testing.T) {
	// Spec-tier invariant: deleting the last entry transitions the
	// cursor to End-of-iteration (no successor).
	cfg := page.Config{PageSize: 4096}
	pairs := [][2]string{{"a", "A"}, {"b", "B"}, {"c", "C"}}
	root, pw := buildTree(t, cfg, pairs)
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	c.Last() // → c
	if err := c.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if c.state != csEndOfIteration {
		t.Errorf("post-Delete-of-last state = %v; want csEndOfIteration", c.state)
	}
	if k, v := c.Current(); k != nil || v != nil {
		t.Errorf("End-of-iteration Current = (%q, %q); want (nil, nil)", k, v)
	}
	if err := c.Err(); err != nil {
		t.Errorf("End-of-iteration Err = %v; want nil", err)
	}
}

func TestCursorDeleteOnUnpositionedReturnsErr(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	root, pw := buildTree(t, cfg, [][2]string{{"a", "A"}})
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	err := c.Delete()
	if !errors.Is(err, ErrCursorUnpositioned) {
		t.Errorf("Delete on Unpositioned: err = %v; want ErrCursorUnpositioned", err)
	}
}

func TestCursorDeleteOnReadOnlyCursorReturnsErr(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	root, pw := buildTree(t, cfg, [][2]string{{"a", "A"}, {"b", "B"}})
	c := NewReadCursor(pw, cfg, root)
	c.First()
	err := c.Delete()
	if !errors.Is(err, ErrReadOnly) {
		t.Errorf("Delete on read-only cursor: err = %v; want ErrReadOnly", err)
	}
}

func TestCursorDeleteCowMergeCascadeTolerance(t *testing.T) {
	// Spec-tier invariant (kind=entailed): Cursor.Delete tolerates
	// CopyPage + merge cascade — pre-delete leaf may be freed
	// mid-operation, yet post-Delete Next/Prev/Current return
	// structurally-correct entries.
	//
	// Force a multi-leaf tree at MergeThreshold=50 so a single
	// delete fires a merge; then delete via the cursor and walk
	// forward to confirm every surviving key is reachable in
	// order.
	cfg := page.Config{PageSize: 4096}
	const N = 12
	pairs := make([][2]string, N)
	for i := range N {
		pairs[i] = [2]string{
			fmt.Sprintf("k-%03d", i),
			string(bytes.Repeat([]byte{byte('a' + i)}, 500)),
		}
	}
	root, pw := buildTree(t, cfg, pairs)
	if treeDepth(t, pw, root) < 1 {
		t.Fatalf("setup: tree depth = 0; need ≥ 1 for cascade test")
	}
	c := NewCursor(pw, cfg, root, 50)

	c.Seek([]byte("k-005"))
	if err := c.Delete(); err != nil {
		t.Fatalf("Delete(k-005): %v", err)
	}
	// Walk forward from the post-delete position; collect.
	got := drainFrom(c)
	for i, p := range got {
		// Surviving suffix should be k-006..k-011, in order.
		wantKey := fmt.Sprintf("k-%03d", 6+i)
		if p[0] != wantKey {
			t.Errorf("post-Delete walk[%d] key = %q; want %q", i, p[0], wantKey)
		}
	}
	// Verify k-005 is absent and the rest survive.
	for i := range N {
		key := fmt.Appendf(nil, "k-%03d", i)
		_, found, err := Get(pw, cfg, c.RootID(), key)
		want := i != 5
		if err != nil {
			t.Errorf("Get(%q): err %v", key, err)
			continue
		}
		if found != want {
			t.Errorf("Get(%q): found=%v want=%v", key, found, want)
		}
	}
}

func TestCursorOverflowEntryNavigation(t *testing.T) {
	// Chunk-4.7 contract (replaces the chunk-4.3 sentinel test):
	// the cursor eagerly assembles overflow-entry values on
	// adoptEntry, returning the assembled bytes via Current /
	// Next / Prev. Pin: a leaf with mixed inline + overflow
	// entries walks correctly and the overflow entry's value
	// matches the chain bytes.
	cfg := page.Config{PageSize: 4096}
	pr := newFakeReader(t, 4096)

	// Set up an overflow chain rooted at page 42 with a known
	// 6000-byte value (≥ 4 KB single-page capacity → 2-page run).
	const totalLen = 6000
	value := make([]byte, totalLen)
	for i := range totalLen {
		value[i] = byte(i % 251)
	}
	runLen := page.OverflowRunLength(cfg, totalLen)
	if runLen != 2 {
		t.Fatalf("setup: runLen = %d; want 2", runLen)
	}
	pages := make([][]byte, runLen)
	for i := range runLen {
		pages[i] = make([]byte, cfg.PageSize)
	}
	if err := page.EncodeOverflowRun(pages, cfg, value); err != nil {
		t.Fatalf("setup: EncodeOverflowRun: %v", err)
	}
	for i := range runLen {
		pr.put(42+uint64(i), pages[i])
	}

	pr.put(1, makeLeaf(t, cfg, []page.LeafEntry{
		{Key: []byte("a"), Value: []byte("A")},
		{
			Key:          []byte("big"),
			Flags:        page.CellFlagOverflow,
			OverflowPage: 42,
			TotalLen:     totalLen,
		},
		{Key: []byte("z"), Value: []byte("Z")},
	}))
	c := NewReadCursor(pr, cfg, 1)
	got := drain(c)
	if len(got) != 3 {
		t.Fatalf("overflow-mixed walk yielded %d entries; want 3", len(got))
	}
	if got[0][0] != "a" || got[2][0] != "z" {
		t.Errorf("overflow-mixed walk = %v; want first=a, last=z", got)
	}
	if got[1][0] != "big" {
		t.Errorf("overflow entry key = %q; want big", got[1][0])
	}
	// Cursor now assembles the overflow value into a heap slice;
	// the assembled bytes must match the original.
	if got[1][1] != string(value) {
		t.Errorf("overflow entry value mismatch (got len=%d, want len=%d)", len(got[1][1]), len(value))
	}
}

func TestCursorSeekGEExactMatchOverflowAssemblesValue(t *testing.T) {
	// Regression for chunk-4.7 round-1 H1: SeekGE's exact-match
	// path previously did `c.curValue = entry.Value` directly,
	// bypassing valueFor — so an exact match on an overflow
	// entry returned (key, nil) instead of (key, assembled).
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	big := bytes.Repeat([]byte{'q'}, 8000)
	root, err := Put(pw, cfg, 0, []byte("k"), big)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	k, v := c.SeekGE([]byte("k"))
	if !bytes.Equal(k, []byte("k")) {
		t.Errorf("SeekGE key = %q; want k", k)
	}
	if !bytes.Equal(v, big) {
		t.Errorf("SeekGE overflow value mismatch (got len=%d, want len=%d)", len(v), len(big))
	}

	// Symmetric sanity check on Seek.
	k, v = c.Seek([]byte("k"))
	if !bytes.Equal(k, []byte("k")) || !bytes.Equal(v, big) {
		t.Errorf("Seek (exact) overflow value mismatch")
	}
}

func TestCursorEmptyTreeAllOpsReturnEnd(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	c := NewCursor(pw, cfg, 0, DefaultMergeThreshold)

	if k, _ := c.First(); k != nil {
		t.Errorf("First on empty tree = %q; want nil", k)
	}
	if c.state != csEndOfIteration {
		t.Errorf("post-First-on-empty state = %v; want csEndOfIteration", c.state)
	}
	if k, _ := c.Last(); k != nil {
		t.Errorf("Last on empty tree = %q; want nil", k)
	}
	if k, _ := c.Seek([]byte("k")); k != nil {
		t.Errorf("Seek on empty tree = %q; want nil", k)
	}
	if k, _ := c.SeekGE([]byte("k")); k != nil {
		t.Errorf("SeekGE on empty tree = %q; want nil", k)
	}
}

func TestCursorMarkStaleSurfacesErrCursorStaleOnNonPositioningOps(t *testing.T) {
	// Scaffolding for chunk-5+ external-mutation invalidation. The
	// cursor's non-positioning methods (Next / Prev / Current /
	// Delete) detect c.gen != c.posGen and surface ErrCursorStale;
	// positioning methods (First / Last / Seek / SeekGE) reset
	// posGen and recover. At chunk-4.6δ no internal caller bumps
	// gen externally; pinning the behavior here ensures the
	// chunk-5 keyspace integration finds the contract uniform
	// across Next / Prev / Current / Delete.
	cfg := page.Config{PageSize: 4096}
	root, pw := buildTree(t, cfg, [][2]string{{"a", "A"}, {"b", "B"}, {"c", "C"}})
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)

	c.Seek([]byte("b"))
	if c.state != csPositioned {
		t.Fatalf("setup: Seek(b) state = %v; want csPositioned", c.state)
	}
	c.MarkStale()

	// Current surfaces ErrCursorStale via Err() and returns
	// (nil, nil).
	if k, v := c.Current(); k != nil || v != nil {
		t.Errorf("Current on stale cursor = (%q, %q); want (nil, nil)", k, v)
	}
	if !errors.Is(c.Err(), ErrCursorStale) {
		t.Errorf("Err on stale cursor = %v; want ErrCursorStale", c.Err())
	}

	// Next likewise.
	if k, _ := c.Next(); k != nil {
		t.Errorf("Next on stale cursor returned non-nil key %q", k)
	}
	if !errors.Is(c.Err(), ErrCursorStale) {
		t.Errorf("Err after Next on stale = %v; want ErrCursorStale", c.Err())
	}

	// Delete surfaces ErrCursorStale directly (not via Err) —
	// symmetric with the other non-positioning ops per the
	// internal-cursor Delete contract.
	if err := c.Delete(); !errors.Is(err, ErrCursorStale) {
		t.Errorf("Delete on stale cursor = %v; want ErrCursorStale", err)
	}

	// Re-positioning recovers: Seek resets posGen = gen.
	c.Seek([]byte("c"))
	if c.state != csPositioned {
		t.Errorf("post-stale-recovery Seek state = %v; want csPositioned", c.state)
	}
	if err := c.Err(); err != nil {
		t.Errorf("post-stale-recovery Err = %v; want nil", err)
	}
}

func TestCursorPrevFromUnpositionedActsAsLast(t *testing.T) {
	// State-machine: from Unpositioned, Prev behaves like Last.
	cfg := page.Config{PageSize: 4096}
	root, pw := buildTree(t, cfg, [][2]string{{"a", "A"}, {"b", "B"}, {"c", "C"}})
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)
	k, v := c.Prev()
	if !bytes.Equal(k, []byte("c")) || !bytes.Equal(v, []byte("C")) {
		t.Errorf("Prev from Unpositioned = (%q, %q); want (c, C)", k, v)
	}
}

// drain walks the cursor forward from First and returns every
// (key, value) it visits.
func drain(c *Cursor) [][2]string {
	out := make([][2]string, 0, 32)
	for k, v := c.First(); k != nil; k, v = c.Next() {
		out = append(out, [2]string{string(bytes.Clone(k)), string(bytes.Clone(v))})
	}
	return out
}

// drainBackward walks the cursor backward from Last.
func drainBackward(c *Cursor) [][2]string {
	out := make([][2]string, 0, 32)
	for k, v := c.Last(); k != nil; k, v = c.Prev() {
		out = append(out, [2]string{string(bytes.Clone(k)), string(bytes.Clone(v))})
	}
	return out
}

// drainFrom walks the cursor forward from its current position
// (inclusive of Current).
func drainFrom(c *Cursor) [][2]string {
	out := make([][2]string, 0, 32)
	k, v := c.Current()
	for ; k != nil; k, v = c.Next() {
		out = append(out, [2]string{string(bytes.Clone(k)), string(bytes.Clone(v))})
	}
	return out
}

// reverse returns a reversed copy of pairs.
func reverse(pairs [][2]string) [][2]string {
	out := make([][2]string, len(pairs))
	for i, p := range pairs {
		out[len(pairs)-1-i] = p
	}
	return out
}

// TestCursorDescentRejectsCorruptLeaf pins the descent skeleton's leaf
// validation: a structurally corrupt leaf surfaces ErrCorrupted via
// Cursor.Err instead of iterating garbage bytes.
func TestCursorDescentRejectsCorruptLeaf(t *testing.T) {
	pw := newFakeWriter(t, 4096)
	cfg := page.Config{PageSize: 4096}

	// Root branch → child 5 claims to be a leaf but its cell directory
	// is garbage.
	leafBuf, err := pw.ZeroPage(5)
	if err != nil {
		t.Fatalf("ZeroPage(5): %v", err)
	}
	page.WriteHeader(leafBuf, page.TypeLeaf, 3, 0)
	for i := page.HeaderSize; i < len(leafBuf); i++ {
		leafBuf[i] = 0xFF
	}
	rootBuf, err := pw.ZeroPage(4)
	if err != nil {
		t.Fatalf("ZeroPage(4): %v", err)
	}
	if err := page.EncodeBranch(rootBuf, cfg, 5, nil); err != nil {
		t.Fatalf("EncodeBranch: %v", err)
	}

	c := NewCursor(pw, cfg, 4, DefaultMergeThreshold)
	if k, _ := c.First(); k != nil {
		t.Fatalf("First on corrupt leaf returned key %q", k)
	}
	if err := c.Err(); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Err = %v, want ErrCorrupted", err)
	}
}

// TestCursorLastOnEmptyBranch pins the rightmost-descent policy on a
// zero-cell branch (leftmost child only): the descent must follow the
// leftmost pointer — the only child — not index cells[-1].
func TestCursorLastOnEmptyBranch(t *testing.T) {
	pw := newFakeWriter(t, 4096)
	cfg := page.Config{PageSize: 4096}

	leafBuf, err := pw.ZeroPage(5)
	if err != nil {
		t.Fatalf("ZeroPage(5): %v", err)
	}
	lb := page.NewLeafBuilder(leafBuf, cfg)
	if !lb.AddEntry(page.LeafEntry{Key: []byte("a"), Value: []byte("1")}) ||
		!lb.AddEntry(page.LeafEntry{Key: []byte("b"), Value: []byte("2")}) {
		t.Fatal("AddEntry overflow")
	}
	lb.Finish()

	rootBuf, err := pw.ZeroPage(4)
	if err != nil {
		t.Fatalf("ZeroPage(4): %v", err)
	}
	if err := page.EncodeBranch(rootBuf, cfg, 5, nil); err != nil {
		t.Fatalf("EncodeBranch: %v", err)
	}

	c := NewCursor(pw, cfg, 4, DefaultMergeThreshold)
	k, v := c.Last()
	if err := c.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if string(k) != "b" || string(v) != "2" {
		t.Fatalf("Last = (%q,%q), want (b,2)", k, v)
	}
}
