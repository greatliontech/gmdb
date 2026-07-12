package btree

// Promotion tests. PromoteSubpageToNestedTree is exercised
// against a real *pager.Pager fixture so the 4-step algorithm —
// alloc nested-root leaf + copy subpage entries + insert new value —
// runs over the same PageWriter surface the SetKeyspace
// surface uses.

import (
	"bytes"
	"errors"
	"sort"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// collectNestedTreeValues walks every leaf in the nested tree rooted
// at rootID and returns the (key, value) pairs in sorted key order.
// For nested trees the cells are inline empty-value entries (where
// the "keys" are the subpage's original values), so the returned
// values are all empty.
func collectNestedTreeValues(t *testing.T, pw PageWriter, cfg page.Config, rootID uint64) [][]byte {
	t.Helper()
	if rootID == 0 {
		return nil
	}
	var out [][]byte
	var walk func(pageID uint64)
	walk = func(pageID uint64) {
		buf, _ := pw.Page(pageID)
		typ, _, _, _ := page.ReadHeader(buf)
		switch {
		case page.IsLeafType(typ):
			r := page.NewLeafReader(buf, cfg)
			if err := r.Validate(); err != nil {
				t.Fatalf("Validate leaf %d: %v", pageID, err)
			}
			it := r.IterForReuse(nil, nil, nil)
			for e, ok := it.Next(); ok; e, ok = it.Next() {
				out = append(out, append([]byte(nil), e.Key...))
			}
		case typ == page.TypeBranch:
			_, _, cellCount, _ := page.ReadHeader(buf)
			children := make([]uint64, 0, int(cellCount)+1)
			for i := uint16(0); i <= cellCount; i++ {
				c := page.BranchChildAt(buf, cfg, i)
				if c == 0 {
					t.Fatalf("null child %d in branch %d", i, pageID)
				}
				children = append(children, c)
			}
			for _, c := range children {
				walk(c)
			}
		default:
			t.Fatalf("unexpected type %d on page %d", typ, pageID)
		}
	}
	walk(rootID)
	return out
}

func TestPromoteSubpageBasicVariableSize(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 128)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()

	// Seed: a small subpage with a few values.
	values := [][]byte{
		[]byte("apple"), []byte("banana"), []byte("cherry"),
		[]byte("date"), []byte("elderberry"),
	}
	subpage, err := page.EncodeSubpage(values, 0)
	if err != nil {
		t.Fatalf("EncodeSubpage: %v", err)
	}

	// Promote with a new value that sorts in the middle.
	newValue := []byte("clementine")
	root, count, err := PromoteSubpageToNestedTree(pw, cfg, subpage, 0, newValue)
	if err != nil {
		t.Fatalf("PromoteSubpageToNestedTree: %v", err)
	}
	if root == 0 {
		t.Fatalf("rootID = 0, want non-zero")
	}
	if count != uint64(len(values))+1 {
		t.Errorf("count = %d, want %d", count, len(values)+1)
	}

	// Walk the nested tree and verify every value is present in
	// sorted order, with no duplicates and no garbage.
	got := collectNestedTreeValues(t, pw, cfg, root)
	want := append([][]byte{}, values...)
	want = append(want, newValue)
	sort.Slice(want, func(i, j int) bool { return bytes.Compare(want[i], want[j]) < 0 })
	if len(got) != len(want) {
		t.Fatalf("nested-tree count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("entry %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPromoteSubpageFixedSize(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 128)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()

	const fvs uint16 = 8
	values := make([][]byte, 0, 20)
	for i := range 20 {
		v := make([]byte, fvs)
		v[0] = byte(i)
		// Distinct bytes so sort-order holds.
		values = append(values, v)
	}
	subpage, err := page.EncodeSubpage(values, fvs)
	if err != nil {
		t.Fatalf("EncodeSubpage fixed: %v", err)
	}
	newValue := make([]byte, fvs)
	newValue[0] = 0xFF // sorts after everything
	root, count, err := PromoteSubpageToNestedTree(pw, cfg, subpage, fvs, newValue)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if count != uint64(len(values))+1 {
		t.Errorf("count=%d, want %d", count, len(values)+1)
	}
	got := collectNestedTreeValues(t, pw, cfg, root)
	if len(got) != len(values)+1 {
		t.Errorf("nested-tree count=%d, want %d", len(got), len(values)+1)
	}
	// Last entry should be newValue (it sorts after the others).
	if !bytes.Equal(got[len(got)-1], newValue) {
		t.Errorf("last entry = %x, want %x", got[len(got)-1], newValue)
	}
}

func TestPromoteSubpageNewValueBeforeAll(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 128)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()

	values := [][]byte{[]byte("banana"), []byte("cherry"), []byte("date")}
	subpage, _ := page.EncodeSubpage(values, 0)
	newValue := []byte("apple") // sorts before everything
	root, count, err := PromoteSubpageToNestedTree(pw, cfg, subpage, 0, newValue)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if count != 4 {
		t.Errorf("count=%d, want 4", count)
	}
	got := collectNestedTreeValues(t, pw, cfg, root)
	if len(got) != 4 || !bytes.Equal(got[0], newValue) {
		t.Errorf("first=%q got=%v, want apple+sorted", got[0], got)
	}
}

func TestPromoteSubpageRejectsDuplicate(t *testing.T) {
	// Caller contract: the SetKeyspace surface short-circuits Put on
	// a duplicate via SubpageReader.Search before invoking promotion.
	// PromoteSubpageToNestedTree defends-in-depth by rejecting the
	// case rather than producing a tree with a duplicate value.
	pw, _, f := setupPagerWriter(t, 128)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()

	values := [][]byte{[]byte("alpha"), []byte("beta"), []byte("gamma")}
	subpage, _ := page.EncodeSubpage(values, 0)
	_, _, err := PromoteSubpageToNestedTree(pw, cfg, subpage, 0, []byte("beta"))
	if err == nil {
		t.Fatalf("Promote on duplicate did not error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("already present")) {
		t.Errorf("err=%v, want 'already present' substring", err)
	}
}

func TestPromoteSubpageRejectsMalformedSubpage(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 128)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()

	// Hand-craft a malformed subpage: Count=2, DataSize=0 (header
	// inconsistent). Validate at the codec layer rejects.
	bad := make([]byte, page.SubpageHeaderSize)
	bad[0] = 2 // Count=2
	bad[1] = 0
	bad[2] = 0 // DataSize=0
	bad[3] = 0
	_, _, err := PromoteSubpageToNestedTree(pw, cfg, bad, 0, []byte("x"))
	if err == nil {
		t.Fatalf("Promote on malformed subpage did not error")
	}
}

func TestPromoteSubpageNearThreshold(t *testing.T) {
	// A near-threshold subpage (many values, each small) should
	// promote successfully into a single-leaf nested tree (no split
	// triggered by the Put of newValue). The nested-tree leaf is
	// fresh and has full capacity, so subpage_size + new_entry fits
	// comfortably below the leaf's free space.
	pw, _, f := setupPagerWriter(t, 128)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()

	// 80 entries × 16 bytes each = ~1440 bytes subpage content
	// (close to 50% of 4 KB leaf usable). Below the threshold by
	// construction.
	values := make([][]byte, 0, 80)
	for i := range 80 {
		v := make([]byte, 16)
		v[0] = byte(i / 256)
		v[1] = byte(i)
		values = append(values, v)
	}
	subpage, err := page.EncodeSubpage(values, 0)
	if err != nil {
		t.Fatalf("EncodeSubpage: %v", err)
	}
	if subpageSz, threshold := len(subpage), page.SubpagePromotionThreshold(cfg); subpageSz > threshold {
		// Pin the test premise: this case is "near-but-below
		// threshold." A future encoder change that pushes the
		// subpage past threshold turns this into a different test
		// (over-threshold promotion) — fail loudly rather than
		// silently re-purpose the test.
		t.Fatalf("subpage size %d > threshold %d — test fixture must satisfy near-but-below; adjust the value-count or value-size",
			subpageSz, threshold)
	}

	newValue := make([]byte, 16)
	newValue[0] = 0xFE
	root, count, err := PromoteSubpageToNestedTree(pw, cfg, subpage, 0, newValue)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if count != 81 {
		t.Errorf("count=%d, want 81", count)
	}
	got := collectNestedTreeValues(t, pw, cfg, root)
	if len(got) != 81 {
		t.Errorf("nested-tree count=%d, want 81", len(got))
	}

	// Verify the new value is present (search via Get).
	v, found, err := Get(pw, cfg, root, newValue)
	if err != nil {
		t.Fatalf("Get newValue: %v", err)
	}
	if !found {
		t.Errorf("newValue not found in promoted tree")
	}
	if len(v) != 0 {
		t.Errorf("nested-tree value len=%d, want 0 (empty)", len(v))
	}

	// Verify a value from the original subpage is also present.
	v, found, err = Get(pw, cfg, root, values[40])
	if err != nil {
		t.Fatalf("Get values[40]: %v", err)
	}
	if !found {
		t.Errorf("values[40]=%x not found", values[40])
	}
	if len(v) != 0 {
		t.Errorf("len(v)=%d, want 0", len(v))
	}
}

func TestPromoteSubpageMatchesEncoderOutput(t *testing.T) {
	// Round-trip pin: the cell the caller would write via
	// LeafBuilder.AddNestedTreeRef(key, root, count) decodes back
	// with matching (NestedRoot, NestedCount) fields. This is the
	// SetKeyspace caller contract — PromoteSubpageToNestedTree
	// returns the (root, count) the cell builder needs.
	pw, _, f := setupPagerWriter(t, 128)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()

	values := [][]byte{[]byte("a"), []byte("b"), []byte("c")}
	subpage, _ := page.EncodeSubpage(values, 0)
	root, count, err := PromoteSubpageToNestedTree(pw, cfg, subpage, 0, []byte("d"))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	// Build a parent leaf with the nested-tree-ref cell.
	parentBuf := make([]byte, cfg.PageSize)
	b := page.NewLeafBuilder(parentBuf, cfg)
	parentKey := []byte("topic")
	if !b.AddNestedTreeRef(parentKey, root, count) {
		t.Fatalf("AddNestedTreeRef returned false")
	}
	b.Finish()
	r := page.NewLeafReader(parentBuf, cfg)
	if err := r.Validate(); err != nil {
		t.Fatalf("parent leaf Validate: %v", err)
	}
	got, _ := r.EntryAt(0, nil)
	if !got.IsNestedTree() {
		t.Fatalf("cell flags wrong: 0x%x", got.Flags)
	}
	if got.NestedRoot != root {
		t.Errorf("NestedRoot=%d, want %d", got.NestedRoot, root)
	}
	if got.NestedCount != count {
		t.Errorf("NestedCount=%d, want %d", got.NestedCount, count)
	}
	if string(got.Key) != string(parentKey) {
		t.Errorf("Key=%q, want %q", got.Key, parentKey)
	}
}

// --- Atomicity: pin entailed invariant E3 (promotion atomicity) ---
//
// E3 requires that on any error path PromoteSubpageToNestedTree
// leaves the caller in a recoverable state: the function returns
// (0, 0, err) and every page allocated during the call is retired
// (FreePage'd) before return so the caller's tx-abort path retires
// no orphan pages. The next two tests inject failures at each
// reachable failure boundary and assert (a) the function returns
// (0, 0, err) and (b) every page newLeafID promotion allocated
// appears in the fakeWriter's `freed` set.

// failingFakeWriter wraps fakeWriter and fails the Nth call to a
// chosen method. Used to drive promotion's failure-injection tests
// against deterministic call counts (newLeafID == 1 by fakeWriter's
// monotonic allocation).
type failingFakeWriter struct {
	*fakeWriter
	allocSlabCallsToFail uint64 // 0 = never fail
	allocSlabCalls       uint64
	allocPageCallsToFail uint64
	allocPageCalls       uint64
}

func (f *failingFakeWriter) AllocPage() (uint64, error) {
	f.allocPageCalls++
	if f.allocPageCallsToFail != 0 && f.allocPageCalls == f.allocPageCallsToFail {
		return 0, errors.New("injected: AllocPage")
	}
	return f.fakeWriter.AllocPage()
}

func (f *failingFakeWriter) ZeroPage(id uint64) ([]byte, error) {
	f.allocSlabCalls++
	if f.allocSlabCallsToFail != 0 && f.allocSlabCalls == f.allocSlabCallsToFail {
		return nil, errors.New("injected: ZeroPage")
	}
	return f.fakeWriter.ZeroPage(id)
}

func TestPromoteSubpageAtomicityAllocSlabFailure(t *testing.T) {
	// Inject: ZeroPage fails on its first call (which is the
	// step-1+2 nested-root leaf slab). AllocPage(call=1) succeeds
	// returning newLeafID=1; then ZeroPage(call=1) fails. The
	// function must return (0, 0, err) AND FreePage(1) before
	// returning so the caller's bookkeeping observes no leaked
	// page. (Per the subpage-cell contract + E3, the post-call state
	// is "as-if-no-promotion-happened" within the tx.)
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)
	pw := &failingFakeWriter{
		fakeWriter:           fake,
		allocSlabCallsToFail: 1,
	}

	subpage, _ := page.EncodeSubpage([][]byte{[]byte("a"), []byte("b")}, 0)
	root, count, err := PromoteSubpageToNestedTree(pw, cfg, subpage, 0, []byte("c"))
	if err == nil {
		t.Fatalf("Promote did not error on injected ZeroPage failure")
	}
	if root != 0 || count != 0 {
		t.Errorf("on error want (0,0,err); got (%d,%d,%v)", root, count, err)
	}
	// newLeafID was 1 (first AllocPage); E3 requires it be retired.
	if _, freed := fake.freed[1]; !freed {
		t.Errorf("newLeafID=1 not retired post-failure; freed=%v (E3 atomicity violation)", fake.freed)
	}
	// Tighten: the freed set must be exactly {1} — a divergent
	// allocation pattern (e.g. alloc'd id=2 and freed only id=1)
	// would silently pass a presence-only check.
	if len(fake.freed) != 1 {
		t.Errorf("freed set size %d, want 1 (only newLeafID); freed=%v", len(fake.freed), fake.freed)
	}
}

func TestPromoteSubpageAtomicityAllocPageFailure(t *testing.T) {
	// AllocPage fails on the very first call. Trivially safe — no
	// allocations to clean up — but pin the (0,0,err) return shape
	// so a future refactor that accidentally returns a non-zero
	// rootID on early failure trips the test.
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)
	pw := &failingFakeWriter{
		fakeWriter:           fake,
		allocPageCallsToFail: 1,
	}

	subpage, _ := page.EncodeSubpage([][]byte{[]byte("a"), []byte("b")}, 0)
	root, count, err := PromoteSubpageToNestedTree(pw, cfg, subpage, 0, []byte("c"))
	if err == nil {
		t.Fatalf("Promote did not error on injected AllocPage failure")
	}
	if root != 0 || count != 0 {
		t.Errorf("on error want (0,0,err); got (%d,%d,%v)", root, count, err)
	}
	if len(fake.freed) != 0 {
		t.Errorf("no allocations should have happened; freed=%v", fake.freed)
	}
}

func TestPromoteSubpageAtomicityPutFailure(t *testing.T) {
	// Inject: Put internally calls AllocPage for the CopyPage leaf. So
	// the 2nd AllocPage call is Put's first allocation. Failing it
	// triggers Put's rollback (which doesn't free newLeafID — that
	// belongs to the promotion's caller).
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}
	fake := newFakeWriter(t, cfg.PageSize)
	pw := &failingFakeWriter{
		fakeWriter:           fake,
		allocPageCallsToFail: 2,
	}

	subpage, _ := page.EncodeSubpage([][]byte{[]byte("a"), []byte("b")}, 0)
	root, count, err := PromoteSubpageToNestedTree(pw, cfg, subpage, 0, []byte("c"))
	if err == nil {
		t.Fatalf("Promote did not error on injected Put-time AllocPage failure")
	}
	if root != 0 || count != 0 {
		t.Errorf("on error want (0,0,err); got (%d,%d,%v)", root, count, err)
	}
	if _, freed := fake.freed[1]; !freed {
		t.Errorf("newLeafID=1 not retired post-Put-failure; freed=%v", fake.freed)
	}
}
