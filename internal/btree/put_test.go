package btree

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// fakeWriter implements PageWriter for unit tests. Pages are
// stored in a map keyed by id; allocations hand out monotonically-
// increasing ids. CoW copies the source bytes into a fresh
// allocation. FreePage marks the id reusable (loose-page semantics
// — actually freed at the end of a logical commit; for tests we
// don't release).
type fakeWriter struct {
	t        *testing.T
	pageSize uint32
	pages    map[uint64][]byte
	nextID   uint64
	freed    map[uint64]struct{}
}

func newFakeWriter(t *testing.T, pageSize uint32) *fakeWriter {
	t.Helper()
	return &fakeWriter{
		t:        t,
		pageSize: pageSize,
		pages:    make(map[uint64][]byte),
		nextID:   1,
		freed:    make(map[uint64]struct{}),
	}
}

func (f *fakeWriter) Page(id uint64) []byte {
	if buf, ok := f.pages[id]; ok {
		return buf
	}
	panic(fmt.Sprintf("fakeWriter: page %d not allocated", id))
}

func (f *fakeWriter) AllocPage() (uint64, error) {
	id := f.nextID
	f.nextID++
	return id, nil
}

func (f *fakeWriter) CoW(srcID, dstID uint64) ([]byte, error) {
	src, ok := f.pages[srcID]
	if !ok {
		return nil, fmt.Errorf("fakeWriter.CoW: src %d not allocated", srcID)
	}
	dst := make([]byte, f.pageSize)
	copy(dst, src)
	f.pages[dstID] = dst
	return dst, nil
}

func (f *fakeWriter) AllocSlab(id uint64) ([]byte, error) {
	buf := make([]byte, f.pageSize)
	f.pages[id] = buf
	return buf, nil
}

func (f *fakeWriter) FreePage(id uint64) error {
	// Pin the no-double-free invariant: a single Delete (or Put)
	// must not retire the same page twice — that masks reclamation
	// bugs (a freed page later read or freed a second time) under
	// the production *pager.Pager. Surface as t.Errorf so the test
	// run fails loudly rather than silently coalescing the double
	// FreePage into one set membership.
	if _, alreadyFreed := f.freed[id]; alreadyFreed {
		f.t.Errorf("fakeWriter.FreePage: double-free of page %d", id)
	}
	f.freed[id] = struct{}{}
	return nil
}

// AllocContiguous returns a run of n consecutive ids. Mirrors the
// pager's bitmap contiguous-run search semantics (free-space.md
// §Contiguous-run search) at the test layer: ids are monotonically
// increasing across the writer's lifetime, never recycled within
// the same tx — matching production except that production may
// reuse loose pages.
func (f *fakeWriter) AllocContiguous(n uint32) (uint64, error) {
	if n == 0 {
		return 0, fmt.Errorf("fakeWriter.AllocContiguous: n=0 invalid")
	}
	first := f.nextID
	f.nextID += uint64(n)
	return first, nil
}

// AllocSlabRun returns fresh zero-filled slab buffers for each id
// in [firstID, firstID+n). Mirrors AllocSlab semantics page-by-
// page; the caller (writeOverflowChain) writes via
// page.EncodeOverflowRun.
func (f *fakeWriter) AllocSlabRun(firstID uint64, n uint32) ([][]byte, error) {
	if n == 0 {
		return nil, fmt.Errorf("fakeWriter.AllocSlabRun: n=0 invalid")
	}
	pages := make([][]byte, n)
	for i := range n {
		buf := make([]byte, f.pageSize)
		f.pages[firstID+uint64(i)] = buf
		pages[i] = buf
	}
	return pages, nil
}

// FreeRun retires n consecutive pages starting at firstID. Each
// id is checked against the no-double-free invariant (same as
// FreePage), so a btree bug that retires an overflow chain twice
// surfaces as a test failure.
func (f *fakeWriter) FreeRun(firstID uint64, n uint32) error {
	if n == 0 {
		return fmt.Errorf("fakeWriter.FreeRun: n=0 invalid")
	}
	for i := range n {
		id := firstID + uint64(i)
		if _, alreadyFreed := f.freed[id]; alreadyFreed {
			f.t.Errorf("fakeWriter.FreeRun: double-free of page %d (in run %d..%d)", id, firstID, firstID+uint64(n)-1)
		}
		f.freed[id] = struct{}{}
	}
	return nil
}

func TestPutEmptyTreeCreatesGenesisLeaf(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root, err := Put(pw, cfg, 0, []byte("k"), []byte("v"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if root == 0 {
		t.Fatal("root id 0 after Put on empty tree")
	}
	v, found, err := Get(pw, cfg, root, []byte("k"))
	if err != nil || !found {
		t.Errorf("Get after Put: found=%v err=%v", found, err)
	}
	if !bytes.Equal(v, []byte("v")) {
		t.Errorf("Get value = %q, want v", v)
	}
}

func TestPutMultipleSingleLeafGetAllBack(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	want := map[string]string{
		"alpha": "A", "beta": "B", "gamma": "G", "delta": "D", "epsilon": "E",
	}
	for k, v := range want {
		newRoot, err := Put(pw, cfg, root, []byte(k), []byte(v))
		if err != nil {
			t.Fatalf("Put(%q): %v", k, err)
		}
		root = newRoot
	}
	for k, v := range want {
		got, found, err := Get(pw, cfg, root, []byte(k))
		if err != nil || !found {
			t.Errorf("Get(%q): found=%v err=%v", k, found, err)
		}
		if !bytes.Equal(got, []byte(v)) {
			t.Errorf("Get(%q): got=%q want=%q", k, got, v)
		}
	}
}

// TestPutDeleteGetUncompressedLeafVariant exercises the uncompressed
// leaf variant (cfg.RestartGroupTarget = 1) end-to-end through Put,
// Get, and Delete. The chunk-4.6γ btree port is variant-agnostic
// past LeafReader / LeafBuilder; this pins that the uncompressed
// dispatch is actually wired through the mutation paths and not
// accidentally compressed-only.
func TestPutDeleteGetUncompressedLeafVariant(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 1}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)

	// Enough entries to force at least one split, exercising the
	// uncompressed dispatch through ascendWithSplit as well as the
	// single-leaf path.
	const N = 60
	want := make(map[string]string, N)
	for i := range N {
		key := fmt.Appendf(nil, "uc-%05d", i)
		val := bytes.Repeat([]byte{byte('a' + i%26)}, 100)
		nr, err := Put(pw, cfg, root, key, val)
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
		root = nr
		want[string(key)] = string(val)
	}

	// Pin: every Put produced an uncompressed-typed leaf. Walk the
	// live tree and check the type byte on every leaf. Also assert
	// the test actually exercised the split path — a future leaf-
	// density change that fits everything in one leaf would silently
	// downgrade this test's coverage from "split + merge variant
	// pin" to "single-leaf variant pin" without failing.
	walkLeavesUC(t, pw, cfg, root)
	if leafCount := countLeaves(t, pw, cfg, root); leafCount < 2 {
		t.Fatalf("test no longer exercises split path: %d leaf(s) after %d Puts; expected ≥ 2", leafCount, N)
	}

	// Round-trip every key.
	for k, v := range want {
		got, found, err := Get(pw, cfg, root, []byte(k))
		if err != nil || !found {
			t.Errorf("Get(%q): found=%v err=%v", k, found, err)
			continue
		}
		if !bytes.Equal(got, []byte(v)) {
			t.Errorf("Get(%q): value mismatch", k)
		}
	}

	// Delete half, exercising the merge/redistribute dispatch under
	// the uncompressed variant.
	for i := 0; i < N; i += 2 {
		key := fmt.Appendf(nil, "uc-%05d", i)
		nr, err := Delete(pw, cfg, root, DefaultMergeThreshold, key)
		if err != nil {
			t.Fatalf("Delete(%d): %v", i, err)
		}
		root = nr
		delete(want, string(key))
	}

	// Re-verify type byte after delete-driven merges.
	walkLeavesUC(t, pw, cfg, root)

	// Surviving keys still retrievable.
	for k, v := range want {
		got, found, err := Get(pw, cfg, root, []byte(k))
		if err != nil || !found {
			t.Errorf("post-delete Get(%q): found=%v err=%v", k, found, err)
			continue
		}
		if !bytes.Equal(got, []byte(v)) {
			t.Errorf("post-delete Get(%q): value mismatch", k)
		}
	}
}

// countLeaves returns the number of leaf pages reachable from root.
// Used by variant-pin tests to assert the split path actually fired
// — a coverage guard against future leaf-density changes silently
// degrading the test.
func countLeaves(t *testing.T, pw *fakeWriter, cfg page.Config, root uint64) int {
	t.Helper()
	if root == 0 {
		return 0
	}
	n := 0
	var walk func(id uint64)
	walk = func(id uint64) {
		buf := pw.Page(id)
		typ, _, _, _ := page.ReadHeader(buf)
		switch {
		case page.IsLeafType(typ):
			n++
		case typ == page.TypeBranch:
			lm, cells := page.DecodeBranch(buf, cfg)
			walk(lm)
			for _, c := range cells {
				walk(c.Child)
			}
		}
	}
	walk(root)
	return n
}

// walkLeavesUC asserts every leaf reachable from root has
// page.TypeLeafUncompressed. Independent of the existing
// walkLeavesXxx helpers so the uncompressed-variant test can pin
// the variant choice flowed all the way through Put / merge /
// redistribute.
func walkLeavesUC(t *testing.T, pw *fakeWriter, cfg page.Config, root uint64) {
	t.Helper()
	if root == 0 {
		return
	}
	var walk func(id uint64)
	walk = func(id uint64) {
		buf := pw.Page(id)
		typ, _, _, _ := page.ReadHeader(buf)
		switch typ {
		case page.TypeLeafUncompressed:
			return
		case page.TypeLeaf:
			t.Errorf("page %d encoded as compressed (TypeLeaf) under RestartGroupTarget=1; want TypeLeafUncompressed", id)
		case page.TypeBranch:
			lm, cells := page.DecodeBranch(buf, cfg)
			walk(lm)
			for _, c := range cells {
				walk(c.Child)
			}
		default:
			t.Errorf("page %d unexpected type %d", id, typ)
		}
	}
	walk(root)
}

func TestPutUpdatesExistingKey(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	root, err := Put(pw, cfg, root, []byte("k"), []byte("v1"))
	if err != nil {
		t.Fatalf("Put initial: %v", err)
	}
	root, err = Put(pw, cfg, root, []byte("k"), []byte("v2"))
	if err != nil {
		t.Fatalf("Put update: %v", err)
	}
	got, found, err := Get(pw, cfg, root, []byte("k"))
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if !bytes.Equal(got, []byte("v2")) {
		t.Errorf("Get after update: got=%q want=v2", got)
	}
}

func TestPutForcesLeafSplitAndRootGrows(t *testing.T) {
	// Insert enough keys that they don't fit in a single 4KB leaf.
	// Each entry: 8B key + 1024B value ≈ ~1040B with overhead.
	// 4 entries ≈ 4160B > one page → triggers split → root becomes
	// branch.
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	const N = 8
	for i := range N {
		key := fmt.Appendf(nil, "key-%04d", i)
		val := bytes.Repeat([]byte{byte('a' + i)}, 1024)
		newRoot, err := Put(pw, cfg, root, key, val)
		if err != nil {
			t.Fatalf("Put(%q): %v", key, err)
		}
		root = newRoot
	}
	// Root should now be a branch (depth > 0).
	typ, _, _, _ := page.ReadHeader(pw.Page(root))
	if typ != page.TypeBranch {
		t.Errorf("root type = %d after %d puts, want TypeBranch=%d", typ, N, page.TypeBranch)
	}
	// All keys still retrievable.
	for i := range N {
		key := fmt.Appendf(nil, "key-%04d", i)
		want := bytes.Repeat([]byte{byte('a' + i)}, 1024)
		got, found, err := Get(pw, cfg, root, key)
		if err != nil || !found {
			t.Errorf("Get(%q): found=%v err=%v", key, found, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Get(%q): value mismatch (len %d vs %d)", key, len(got), len(want))
		}
	}
}

func TestPutManyKeysAllRetrievable(t *testing.T) {
	// 500 small keys forces several rounds of leaf splits (and at
	// least one root growth from single-leaf → branch). Pin that
	// every key remains retrievable after the cascade — the
	// functional contract is "Put-then-Get round-trips every key
	// regardless of split topology."
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	const N = 500
	for i := range N {
		key := fmt.Appendf(nil, "k-%05d", i)
		val := fmt.Appendf(nil, "v-%05d-%s", i, bytes.Repeat([]byte{'x'}, 50))
		newRoot, err := Put(pw, cfg, root, key, val)
		if err != nil {
			t.Fatalf("Put(%q): %v at i=%d", key, err, i)
		}
		root = newRoot
	}
	// Sanity: root grew from leaf to branch (depth >= 1).
	depth := treeDepth(t, pw, root)
	if depth < 1 {
		t.Errorf("tree depth after %d puts = %d, want >= 1 (single-leaf-to-branch transition)", N, depth)
	}
	t.Logf("tree depth after %d puts: %d", N, depth)
	for i := range N {
		key := fmt.Appendf(nil, "k-%05d", i)
		want := fmt.Appendf(nil, "v-%05d-%s", i, bytes.Repeat([]byte{'x'}, 50))
		got, found, err := Get(pw, cfg, root, key)
		if err != nil || !found {
			t.Errorf("Get(%q) at i=%d: found=%v err=%v", key, i, found, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Get(%q): value mismatch", key)
		}
	}
}

func TestPutForcesMultiLevelTreeAndBranchSplit(t *testing.T) {
	// Force the tree past depth-1 by inserting keys with large
	// values so each leaf holds few entries. With value=512B,
	// each leaf fits ~7 entries; 200 entries → ~30 leaves; a
	// branch with 30 cells of 8-byte keys fits one page, so we
	// still wouldn't split branches. Bump value to 1024B and
	// add many distinct large keys to force a deeper tree.
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	// Use longer keys (60B) so each branch cell takes more bytes
	// — forces branch splits sooner. ~50 cells per branch.
	const N = 400
	keyPrefix := bytes.Repeat([]byte("k"), 50)
	for i := range N {
		key := fmt.Appendf(nil, "%s-%05d", keyPrefix, i)
		val := bytes.Repeat([]byte{byte('a' + i%26)}, 512)
		newRoot, err := Put(pw, cfg, root, key, val)
		if err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
		root = newRoot
	}
	depth := treeDepth(t, pw, root)
	if depth < 2 {
		t.Errorf("tree depth after %d large-key puts = %d, want >= 2", N, depth)
	}
	t.Logf("tree depth after %d large-key puts: %d", N, depth)
	for i := range N {
		key := fmt.Appendf(nil, "%s-%05d", keyPrefix, i)
		want := bytes.Repeat([]byte{byte('a' + i%26)}, 512)
		got, found, err := Get(pw, cfg, root, key)
		if err != nil || !found {
			t.Errorf("Get(%d): found=%v err=%v", i, found, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Get(%d): value mismatch", i)
		}
	}
}

func treeDepth(t *testing.T, pw *fakeWriter, root uint64) int {
	t.Helper()
	if root == 0 {
		return 0
	}
	depth := 0
	cur := root
	for {
		typ, _, _, _ := page.ReadHeader(pw.Page(cur))
		if page.IsLeafType(typ) {
			return depth
		}
		if typ != page.TypeBranch {
			t.Fatalf("treeDepth: page %d type %d", cur, typ)
		}
		depth++
		cur = page.BranchLeftmostChild(pw.Page(cur))
	}
}

func TestPutContentsInvariantUnderInsertOrder(t *testing.T) {
	// Final tree CONTENTS (set of key→value mappings) must be the
	// same regardless of insertion order — pins that splits don't
	// lose entries under various interleavings. Tree STRUCTURE is
	// order-dependent (splits depend on insertion sequence); this
	// test verifies content equivalence only.
	cfg := page.Config{PageSize: 4096}
	const N = 100
	orderings := [][]int{
		// Forward: 0, 1, 2, ...
		makeSeq(N, false),
		// Reverse: N-1, N-2, ...
		makeSeq(N, true),
		// Interleaved: 0, N-1, 1, N-2, ...
		makeInterleaved(N),
	}
	results := make([]uint64, len(orderings))
	writers := make([]*fakeWriter, len(orderings))
	for o, order := range orderings {
		pw := newFakeWriter(t, 4096)
		writers[o] = pw
		root := uint64(0)
		for _, i := range order {
			key := fmt.Appendf(nil, "k-%04d", i)
			val := fmt.Appendf(nil, "v-%04d", i)
			newRoot, err := Put(pw, cfg, root, key, val)
			if err != nil {
				t.Fatalf("ordering %d Put(%d): %v", o, i, err)
			}
			root = newRoot
		}
		results[o] = root
	}
	// Verify each tree contains all N keys with correct values.
	for o, root := range results {
		for i := range N {
			key := fmt.Appendf(nil, "k-%04d", i)
			want := fmt.Appendf(nil, "v-%04d", i)
			got, found, err := Get(writers[o], cfg, root, key)
			if err != nil || !found {
				t.Errorf("ordering %d Get(%q): found=%v err=%v", o, key, found, err)
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("ordering %d Get(%q): got=%q want=%q", o, key, got, want)
			}
		}
	}
}

func TestPutPromotesLargeValueToOverflow(t *testing.T) {
	// Chunk-4.7 contract: a value exceeding inline single-entry
	// leaf capacity is automatically promoted to an overflow
	// chain (limits.md §Maximum Value Size). Round-trip via Get
	// must return the assembled value. Replaces the chunk-4.4
	// "reject oversize value" test — values no longer have a
	// hard upper bound short of disk space.
	cfg := page.Config{PageSize: 4096}
	big := bytes.Repeat([]byte{'x'}, 8192)

	// (a) Empty-tree path: putEmpty promotes the genesis entry.
	pw := newFakeWriter(t, 4096)
	root, err := Put(pw, cfg, 0, []byte("k"), big)
	if err != nil {
		t.Fatalf("Put oversize on empty tree: %v", err)
	}
	got, found, err := Get(pw, cfg, root, []byte("k"))
	if err != nil || !found {
		t.Fatalf("Get after oversize Put: found=%v err=%v", found, err)
	}
	if !bytes.Equal(got, big) {
		t.Errorf("Get returned wrong bytes (len=%d vs %d)", len(got), len(big))
	}

	// (b) Existing-tree path: a small entry already lives in the
	// leaf, then an oversize value is inserted at a new key. The
	// new entry goes overflow; the existing inline entry stays
	// in place.
	pw2 := newFakeWriter(t, 4096)
	root, err = Put(pw2, cfg, 0, []byte("seed"), []byte("v"))
	if err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	root, err = Put(pw2, cfg, root, []byte("big"), big)
	if err != nil {
		t.Fatalf("Put oversize on non-empty tree: %v", err)
	}
	got, found, err = Get(pw2, cfg, root, []byte("big"))
	if err != nil || !found || !bytes.Equal(got, big) {
		t.Errorf("Get(big) after overflow Put: found=%v err=%v len=%d", found, err, len(got))
	}
	// The pre-existing small entry survives.
	got, found, err = Get(pw2, cfg, root, []byte("seed"))
	if err != nil || !found || !bytes.Equal(got, []byte("v")) {
		t.Errorf("Get(seed) after overflow Put: found=%v err=%v val=%q", found, err, got)
	}
}

func TestPutOverflowReplaceFreesOldChain(t *testing.T) {
	// Spec-tier invariant (chunk-4.7): a same-key Put-replace
	// where the displaced entry was overflow must free the prior
	// chain in the same write tx. The failure class is "chain
	// orphan after replace": the leaf's new entry references a
	// freshly-allocated chain while the prior chain stays
	// bitmap-allocated but unreachable from any live leaf entry.
	//
	// Pinned via the slab-partition invariant — checkSlabPartition
	// (chunk-4.7 extension) walks overflow chains as reachable;
	// a freed chain stays out of `reachable`, an orphan would
	// trip the "allocated but neither reachable nor freed" arm.
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)

	// (a) Insert with a large value (→ overflow).
	big1 := bytes.Repeat([]byte{'a'}, 8000)
	root, err := Put(pw, cfg, 0, []byte("k"), big1)
	if err != nil {
		t.Fatalf("Put #1: %v", err)
	}
	// Confirm we did produce an overflow chain.
	pagesAfter1 := len(pw.pages)
	if pagesAfter1 < 3 { // 1 leaf + ≥2 overflow pages for 8 KB at 4 KB pages
		t.Fatalf("Put #1: only %d pages allocated; expected ≥3 (leaf + chain)", pagesAfter1)
	}

	// (b) Replace with another large value (→ different chain).
	big2 := bytes.Repeat([]byte{'b'}, 8000)
	root, err = Put(pw, cfg, root, []byte("k"), big2)
	if err != nil {
		t.Fatalf("Put #2 (replace): %v", err)
	}
	// Get must return the new value.
	got, found, err := Get(pw, cfg, root, []byte("k"))
	if err != nil || !found {
		t.Fatalf("Get after replace: found=%v err=%v", found, err)
	}
	if !bytes.Equal(got, big2) {
		t.Errorf("Get after replace returned old value (len=%d)", len(got))
	}

	// (c) Slab-partition invariant: the old overflow chain pages
	// are now in `pw.freed`; the new chain pages are reachable
	// from the live leaf. Nothing is both / neither.
	checkSlabPartition(t, pw, cfg, root)

	// (d) Replace overflow → inline. The chain is freed.
	root, err = Put(pw, cfg, root, []byte("k"), []byte("small"))
	if err != nil {
		t.Fatalf("Put #3 (overflow → inline): %v", err)
	}
	got, found, err = Get(pw, cfg, root, []byte("k"))
	if err != nil || !found || !bytes.Equal(got, []byte("small")) {
		t.Errorf("Get after overflow→inline replace: %q found=%v err=%v", got, found, err)
	}
	checkSlabPartition(t, pw, cfg, root)

	// (e) Replace inline → overflow. Symmetric: no chain to free
	// on the old side, new chain allocated.
	root, err = Put(pw, cfg, root, []byte("k"), big1)
	if err != nil {
		t.Fatalf("Put #4 (inline → overflow): %v", err)
	}
	got, found, err = Get(pw, cfg, root, []byte("k"))
	if err != nil || !found || !bytes.Equal(got, big1) {
		t.Errorf("Get after inline→overflow replace: found=%v err=%v len=%d", found, err, len(got))
	}
	checkSlabPartition(t, pw, cfg, root)
}

func TestPutRejectsOversizeKey(t *testing.T) {
	// Chunk-4.7 contract: ErrKeyTooLarge fires only on keys too
	// large for the overflow-reference leaf entry (a small fixed
	// header per limits.md §Maximum Key Size). At 4 KB pages the
	// overflow-reference entry overhead is 19 bytes plus the
	// key; a key > ~4076 bytes can't fit a single-entry leaf.
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	// Key larger than any single-entry leaf can hold even with
	// overflow value reference.
	bigKey := bytes.Repeat([]byte{'k'}, 5000)
	_, err := Put(pw, cfg, 0, bigKey, []byte("v"))
	if !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("Put oversize key on empty tree: err = %v, want ErrKeyTooLarge", err)
	}
}

func TestPutFreesEveryRetiredPageAfterSplits(t *testing.T) {
	// Spec-tier invariant for CoW correctness (transactions.md
	// §Write Transaction step 3): pages CoW'd out of the live
	// tree must be retired. A leaked old page (not freed but no
	// longer referenced) is a slab/disk leak that surfaces only
	// later as RPL bloat or bitmap drift.
	//
	// Walk the live tree after a burst of puts; compare reachable
	// page IDs against pw.freed. The two sets must partition the
	// allocated-id space: every allocated id is either reachable
	// (in the tree) or freed (retired).
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	const N = 200
	for i := range N {
		key := fmt.Appendf(nil, "k-%05d", i)
		val := bytes.Repeat([]byte{byte('a' + i%26)}, 200)
		newRoot, err := Put(pw, cfg, root, key, val)
		if err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		root = newRoot
	}
	reachable := make(map[uint64]struct{})
	collectReachable(t, pw, cfg, root, reachable)
	// Every allocated id is in pw.pages. Check partition: an id
	// in pw.pages is either reachable or freed (never both,
	// never neither).
	for id := range pw.pages {
		_, isReachable := reachable[id]
		_, isFreed := pw.freed[id]
		if isReachable && isFreed {
			t.Errorf("page %d is both reachable from root and freed (double-state)", id)
		}
		if !isReachable && !isFreed {
			t.Errorf("page %d is allocated but neither reachable nor freed (slab leak)", id)
		}
	}
}

func collectReachable(t *testing.T, pw *fakeWriter, cfg page.Config, id uint64, out map[uint64]struct{}) {
	t.Helper()
	if id == 0 {
		return
	}
	if _, seen := out[id]; seen {
		t.Errorf("page %d reachable from two paths (DAG instead of tree)", id)
		return
	}
	out[id] = struct{}{}
	typ, _, _, _ := page.ReadHeader(pw.Page(id))
	if page.IsLeafType(typ) {
		// Walk overflow chains owned by any overflow leaf entries:
		// each chain page is reachable via the leaf entry's
		// OverflowPage + i for i in [0, OverflowRunLength). The
		// slab-partition invariant (chunk-4.7 extension) requires
		// overflow chains to be reachable from a live leaf entry
		// — orphan chains are the chunk-4.7 chain-orphan failure
		// class (chain bitmap-allocated but no leaf entry
		// references it; pinned by
		// TestPutOverflowReplaceFreesOldChain +
		// TestDeleteOverflowEntryFreesChain).
		r := page.NewLeafReader(pw.Page(id), cfg)
		it := r.IterForReuse(nil, nil, nil)
		for {
			e, ok := it.Next()
			if !ok {
				break
			}
			if !e.IsOverflow() {
				continue
			}
			runLen := page.OverflowRunLength(cfg, e.TotalLen)
			for i := range runLen {
				chainID := e.OverflowPage + uint64(i)
				if _, seen := out[chainID]; seen {
					t.Errorf("overflow chain page %d reachable from two leaf entries", chainID)
					continue
				}
				out[chainID] = struct{}{}
			}
		}
		return
	}
	if typ != page.TypeBranch {
		t.Errorf("collectReachable: page %d type %d unexpected", id, typ)
		return
	}
	lm, cells := page.DecodeBranch(pw.Page(id), cfg)
	collectReachable(t, pw, cfg, lm, out)
	for _, c := range cells {
		collectReachable(t, pw, cfg, c.Child, out)
	}
}

// makeSeq returns [0..N) or [N-1..0] depending on reverse.
func makeSeq(n int, reverse bool) []int {
	out := make([]int, n)
	if reverse {
		for i := range n {
			out[i] = n - 1 - i
		}
	} else {
		for i := range n {
			out[i] = i
		}
	}
	return out
}

// makeInterleaved returns 0, N-1, 1, N-2, ...
func makeInterleaved(n int) []int {
	out := make([]int, 0, n)
	lo, hi := 0, n-1
	for lo <= hi {
		out = append(out, lo)
		if hi != lo {
			out = append(out, hi)
		}
		lo++
		hi--
	}
	return out
}
