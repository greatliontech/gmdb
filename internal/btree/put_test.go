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
	pageSize uint32
	pages    map[uint64][]byte
	nextID   uint64
	freed    map[uint64]struct{}
}

func newFakeWriter(t *testing.T, pageSize uint32) *fakeWriter {
	t.Helper()
	return &fakeWriter{
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
	f.freed[id] = struct{}{}
	return nil
}

func TestPutEmptyTreeCreatesGenesisLeaf(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root, err := Put(pw, cfg, 0, 16, []byte("k"), []byte("v"))
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
		newRoot, err := Put(pw, cfg, root, 16, []byte(k), []byte(v))
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

func TestPutUpdatesExistingKey(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	root, err := Put(pw, cfg, root, 16, []byte("k"), []byte("v1"))
	if err != nil {
		t.Fatalf("Put initial: %v", err)
	}
	root, err = Put(pw, cfg, root, 16, []byte("k"), []byte("v2"))
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
		newRoot, err := Put(pw, cfg, root, 16, key, val)
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
		newRoot, err := Put(pw, cfg, root, 16, key, val)
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
		newRoot, err := Put(pw, cfg, root, 16, key, val)
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
		if typ == page.TypeLeaf {
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
			newRoot, err := Put(pw, cfg, root, 16, key, val)
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

func TestPutRejectsTooBigSingleEntry(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	big := bytes.Repeat([]byte{'x'}, 8192)
	// (a) Empty-tree path: goes through putEmpty.
	pw := newFakeWriter(t, 4096)
	_, err := Put(pw, cfg, 0, 16, []byte("k"), big)
	if !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("Put oversize on empty tree: err = %v, want ErrKeyTooLarge", err)
	}
	// (b) Existing-tree path: pre-populate with a small entry,
	// then insert an oversize value. Goes through Put → CoW leaf
	// → insertOrReplace → fit-ahead fail → split → len(enc) < 2
	// guard fires (the oversize entry can't be split off into
	// its own half because the other half would be empty).
	pw2 := newFakeWriter(t, 4096)
	root, err := Put(pw2, cfg, 0, 16, []byte("seed"), []byte("v"))
	if err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	_, err = Put(pw2, cfg, root, 16, []byte("big"), big)
	if !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("Put oversize on non-empty tree: err = %v, want ErrKeyTooLarge", err)
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
		newRoot, err := Put(pw, cfg, root, 16, key, val)
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
	if typ == page.TypeLeaf {
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
