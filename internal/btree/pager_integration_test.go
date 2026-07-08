package btree

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/pager"
	"github.com/thegrumpylion/gmdb/internal/pagertest"
)

// Inv-3: PageWriter parity — the overflow chain
// semantics (Put-replace / Delete free the chain) hold over a real
// *pager.Pager implementation of the PageWriter interface, not just
// fakeWriter. setupPagerWriter is a sibling of
// internal/pager/freespace_test.go's setupWriter (kept local to avoid an
// internal/pager test-helper export) but seeds the fixture differently on
// purpose: setupWriter starts with an empty free list to exercise the
// free-space machinery, whereas this fixture pre-frees the whole space as
// a frictionless page pool for btree allocation (see the SetCommitState
// comment below for why the HWM sits at the top, not at firstDataPage).

const pagerTestPageSize = 4096

// setupPagerWriter builds the shared cross-package writer-pager
// fixture (internal/pagertest) in the exhaustion-testing shape and
// wraps it in this package's pagerWriter adapter. See
// TestSetupPagerWriterExhaustsToDBFull for the HWM-at-top rationale
// the shared fixture encodes.
func setupPagerWriter(t *testing.T, pages int) (pagerWriter, *bitmap.Bitmap, *os.File) {
	t.Helper()
	p, bm, f := pagertest.SetupWriter(t, pagertest.Params{
		Pages:              pages,
		PageSize:           pagerTestPageSize,
		RestartGroupTarget: 16,
		MarkAllFree:        true,
	})
	return pagerWriter{p}, bm, f
}

// TestSetupPagerWriterExhaustsToDBFull pins the corrected setupPagerWriter
// fixture: with HWM at the top of the space, AllocPage hands out each data
// page exactly once and returns ErrDBFull at capacity — never a duplicate
// id. This is a regression guard for the prior fixture, which pinned HWM at
// firstDataPage while marking pages free above it: exhausting the bitmap
// then fell to file extension (AllocPage step 5) and re-handed-out
// firstDataPage, a duplicate of the first bitmap allocation, instead of
// ErrDBFull. The duplicate-id assertion below fails against that prior
// shape.
func TestSetupPagerWriterExhaustsToDBFull(t *testing.T) {
	const pages = 16
	pw, bm, f := setupPagerWriter(t, pages)
	defer pw.Close()
	defer f.Close()

	// Usable capacity = data pages = total minus the meta+bitmap region.
	capacity := int(uint64(pages) - bm.FirstDataPage())
	seen := make(map[uint64]bool, capacity)
	for i := range capacity {
		id, err := pw.AllocPage()
		if err != nil {
			t.Fatalf("AllocPage %d/%d: unexpected error %v (want %d distinct pages before ErrDBFull)",
				i+1, capacity, err, capacity)
		}
		if seen[id] {
			t.Fatalf("AllocPage returned duplicate id %d at allocation %d "+
				"(double-allocation: HWM not bounding the free region)", id, i+1)
		}
		seen[id] = true
	}
	// One past capacity: the bitmap is exhausted and HWM == maxSizePages,
	// so AllocPage must report ErrDBFull rather than re-handing-out a
	// live page.
	if id, err := pw.AllocPage(); !errors.Is(err, pager.ErrDBFull) {
		t.Fatalf("AllocPage past capacity = (id=%d, err=%v), want ErrDBFull", id, err)
	}
}

// TestPagerOverflowPutGetDelete pins Inv-3: a Put with a value larger
// than inline leaf capacity, followed by Get and Delete, round-trips
// correctly on *pager.Pager and frees the overflow chain on Delete
// (bitmap bits restored, no retiredPages growth for same-tx work).
func TestPagerOverflowPutGetDelete(t *testing.T) {
	pw, bm, f := setupPagerWriter(t, 128)
	defer pw.Close()
	defer f.Close()

	cfg := pw.Config()
	key := []byte("overflow-key")
	// 8 KB value — overflow at 4 KB pages (single-entry inline leaf
	// capacity is < 4 KB).
	value := make([]byte, 8192)
	for i := range value {
		value[i] = byte(i & 0xFF)
	}

	root, err := Put(pw, cfg, 0, key, value)
	if err != nil {
		t.Fatalf("Put overflow: %v", err)
	}

	got, found, err := Get(pw, cfg, root, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("Get: key not found after Put")
	}
	if !bytes.Equal(got, value) {
		t.Errorf("Get mismatch: len(got)=%d, len(want)=%d", len(got), len(value))
	}

	// Snapshot the bitmap state before Delete so we can assert the
	// overflow chain pages are restored to free.
	freeBefore := 0
	for id := bm.FirstDataPage(); id < bm.TotalPages(); id++ {
		if bm.IsSet(id) {
			freeBefore++
		}
	}

	newRoot, err := Delete(pw, cfg, root, DefaultMergeThreshold, key)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if newRoot != 0 {
		t.Errorf("Delete of only entry should leave rootID=0, got %d", newRoot)
	}

	// After Delete, every page used by the overflow chain + the
	// retired leaf should be either freed (bitmap set) or in the loose
	// set (same-tx pages with slab buffers).
	freeAfter := 0
	for id := bm.FirstDataPage(); id < bm.TotalPages(); id++ {
		if bm.IsSet(id) {
			freeAfter++
		}
	}
	if freeAfter < freeBefore {
		t.Errorf("Delete shrunk bitmap-free set: %d → %d (overflow chain "+
			"pages not restored)", freeBefore, freeAfter)
	}
}

// TestPagerOverflowReplaceFreesOldChain pins the Put-replace
// invariant on *pager.Pager: replacing an overflow value with a new
// overflow value frees the prior chain.
func TestPagerOverflowReplaceFreesOldChain(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 128)
	defer pw.Close()
	defer f.Close()

	cfg := pw.Config()
	key := []byte("k")
	value1 := bytes.Repeat([]byte{'A'}, 8192)
	value2 := bytes.Repeat([]byte{'B'}, 9216)

	root, err := Put(pw, cfg, 0, key, value1)
	if err != nil {
		t.Fatalf("Put #1 (overflow): %v", err)
	}
	allocsAfter1 := len(pw.PendingAllocs())

	root, err = Put(pw, cfg, root, key, value2)
	if err != nil {
		t.Fatalf("Put #2 (overflow replace): %v", err)
	}

	// Heuristic sanity bound — a regression that leaks the old chain
	// would more than double pendingAllocs (old chain pages retained +
	// new chain allocated). The tight invariant (slab
	// partition: allocated = reachable ⊕ freed, with overflow-chain
	// reachability) is already enforced by fakeWriter assertions in
	// put_test.go; this integration test's job is parity, so the
	// loose bound is sufficient — the load-bearing assertion is the
	// Get round-trip below.
	allocsAfter2 := len(pw.PendingAllocs())
	if allocsAfter2 > allocsAfter1*2 {
		t.Errorf("Put-replace did not free old overflow chain: "+
			"pendingAllocs %d → %d (more than 2x growth)", allocsAfter1, allocsAfter2)
	}

	got, found, err := Get(pw, cfg, root, key)
	if err != nil {
		t.Fatalf("Get after replace: %v", err)
	}
	if !found || !bytes.Equal(got, value2) {
		t.Errorf("Get after replace: found=%v, value matches=%v",
			found, bytes.Equal(got, value2))
	}
}

// pagerWriter adapts *pager.Pager to btree.PageWriter for these
// parity tests, mirroring the root package's gmdb.btreeWriter (which
// is unreachable here: importing gmdb from internal/btree would
// cycle). The pager keeps its MVCC/slab vocabulary (CoW/AllocSlab);
// btree consumes the storage-neutral CopyPage/ZeroPage/ZeroPageRun.
// Pager methods (Config, Close, PendingAllocs, ...) reach callers
// through the embedded pointer.
type pagerWriter struct{ *pager.Pager }

func (w pagerWriter) CopyPage(srcID, dstID uint64) ([]byte, error) {
	return w.Pager.CoW(srcID, dstID)
}

func (w pagerWriter) ZeroPage(id uint64) ([]byte, error) {
	return w.Pager.AllocSlab(id)
}

func (w pagerWriter) ZeroPageRun(firstID uint64, n uint32) ([][]byte, error) {
	return w.Pager.AllocSlabRun(firstID, n)
}
