package pager

import (
	"bytes"
	"errors"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// Overflow-run pager surfaces (checksums.md §Overflow-Run Digest):
// runs are NEVER slab-resident — every run page is written directly
// (WriteDirectRaw) and read back through the mmap, same-tx included.
// The commit footer pass therefore cannot touch a run page (it only
// iterates the slab), which is what makes the old footer-exemption
// provenance unnecessary: the illegal state (a footer stamped into a
// run page) is unrepresentable.

// writeDirectRun allocates and direct-writes an overflow run carrying
// value, returning its head id — the production write shape
// (page.WriteOverflowRun over WriteDirectRaw, head last).
func writeDirectRun(t *testing.T, p *Pager, value []byte) uint64 {
	t.Helper()
	cfg := p.Config()
	runLen := page.OverflowRunLength(cfg, uint64(len(value)))
	first, err := p.AllocContiguous(runLen)
	if err != nil {
		t.Fatalf("AllocContiguous: %v", err)
	}
	if err := page.WriteOverflowRun(cfg, value, func(idx uint32, buf []byte) error {
		return p.WriteDirectRaw(first+uint64(idx), buf)
	}); err != nil {
		t.Fatalf("WriteOverflowRun: %v", err)
	}
	return first
}

// TestPageRunDirectWriteSameTxReadBack: a run written in the CURRENT
// tx reads back through PageRun via the mmap (unified page cache),
// digest-verified, extent intact — no slab assembly exists anymore.
func TestPageRunDirectWriteSameTxReadBack(t *testing.T) {
	p, _, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()
	cfg := p.Config()

	value := bytes.Repeat([]byte{0x6B}, page.OverflowFirstPageCapacity(cfg)+300)
	first := writeDirectRun(t, p, value)

	run, err := p.PageRun(first)
	if err != nil {
		t.Fatalf("PageRun on same-tx direct run: %v", err)
	}
	runLen := page.OverflowRunLength(cfg, uint64(len(value)))
	if len(run) != int(runLen)*int(cfg.PageSize) {
		t.Fatalf("run image %d bytes, want %d", len(run), int(runLen)*int(cfg.PageSize))
	}
	if !page.VerifyOverflowRun(run, cfg) {
		t.Fatal("same-tx direct run fails whole-run digest verification")
	}
	ext := page.OverflowRunExtent(run, cfg)
	if !bytes.Equal(ext[:len(value)], value) {
		t.Fatal("extent mismatch")
	}
}

// TestPageRunRejectsDirtyHead: a slab buffer at a run-head id means
// the reference is stale or forged (runs are never slab-resident) —
// ErrCorrupted, never an image decoded from the MMAP bytes behind
// the slab buffer. The load-bearing case: a run freed in-tx whose
// head id is re-allocated as a NODE page — the mmap still holds the
// pwritten (now-stale) run head, so without the dirty-head guard
// PageRun would resurrect the freed run's content for an id that is
// now a node page.
func TestPageRunRejectsDirtyHead(t *testing.T) {
	p, _, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()
	cfg := p.Config()

	// A direct-written run: its head bytes are in the file (and the
	// unified page cache) immediately.
	value := bytes.Repeat([]byte{0x33}, page.OverflowFirstPageCapacity(cfg)+50) // 2-page run
	head := writeDirectRun(t, p, value)
	// Free it and re-allocate the head id as a node page.
	if err := p.FreeRun(head, 2); err != nil {
		t.Fatalf("FreeRun: %v", err)
	}
	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if id != head {
		// Allocation order is bitmap-first-fit; the just-freed head is
		// the lowest free page in this fixture.
		t.Fatalf("allocator returned %d, fixture expects the freed head %d", id, head)
	}
	if _, err := p.CoW(2, id); err != nil { // any src; installs a dirty NODE buffer at the old head id
		t.Fatalf("CoW: %v", err)
	}
	if _, err := p.PageRun(id); !errors.Is(err, ErrCorrupted) {
		t.Errorf("PageRun on a dirty head = %v, want ErrCorrupted (stale freed run must not resurrect)", err)
	}
	if _, err := p.PageRunRaw(id); !errors.Is(err, ErrCorrupted) {
		t.Errorf("PageRunRaw on a dirty head = %v, want ErrCorrupted", err)
	}
}

// TestPageRunRejectsNonOverflowHead: PageRun refuses a head whose type
// byte is not TypeOverflow (run pages are reachable only through
// PageRun; everything else is a node page) and a head beyond the
// file-resident extent — ErrCorrupted, never a slice past the mapping.
func TestPageRunRejectsNonOverflowHead(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	// A committed (file-backed, zeroed) page: type byte 0 != TypeOverflow.
	if _, err := p.PageRun(bm.FirstDataPage()); !errors.Is(err, ErrCorrupted) {
		t.Errorf("PageRun on a non-overflow page = %v, want ErrCorrupted", err)
	}
	if _, err := p.PageRun(1 << 40); !errors.Is(err, ErrCorrupted) {
		t.Errorf("PageRun on an out-of-range head = %v, want ErrCorrupted", err)
	}
}

// TestFreeRunDirectRunRestoresBitmap: freeing a same-tx direct-written
// run returns every page to the bitmap (the allocated-but-never-
// slab-written branch) — no loose entries (nothing to resurrect), no
// retiredPages growth (no prior-tx reader can reference the run).
func TestFreeRunDirectRunRestoresBitmap(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()
	cfg := p.Config()

	value := bytes.Repeat([]byte{0x11}, page.OverflowFirstPageCapacity(cfg)+100) // 2-page run
	first := writeDirectRun(t, p, value)
	runLen := page.OverflowRunLength(cfg, uint64(len(value)))

	retiredBefore := len(p.RetiredPages())
	if err := p.FreeRun(first, runLen); err != nil {
		t.Fatalf("FreeRun: %v", err)
	}
	for i := range uint64(runLen) {
		if !bm.IsSet(first + i) {
			t.Errorf("run page %d not free in bitmap after FreeRun", first+i)
		}
		if _, loose := p.LoosePages()[first+i]; loose {
			t.Errorf("run page %d went loose — direct pages have no slab buffer to resurrect", first+i)
		}
	}
	if got := len(p.RetiredPages()); got != retiredBefore {
		t.Errorf("FreeRun of a same-tx run grew retiredPages: %d → %d", retiredBefore, got)
	}
}

// TestFreedDirectRunNotReallocatableInsideSavepointWindow (regression):
// freeing a same-tx direct-written run inside a savepoint window must
// NOT make its pages re-allocatable within that window — a later
// direct write over them would destroy content a RestoreSavepoint can
// resurrect the tree's reference to. In-spec reachability: the btree
// put replace-split path frees the displaced value chain and then
// direct-writes a long separator's key extent before the ascend, which
// can still fail (the op's shallow savepoint then restores).
func TestFreedDirectRunNotReallocatableInsideSavepointWindow(t *testing.T) {
	p, _, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()
	cfg := p.Config()

	valueA := bytes.Repeat([]byte{0xA1}, page.OverflowFirstPageCapacity(cfg)+50) // 2-page run
	head := writeDirectRun(t, p, valueA)

	sp := p.BeginShallowSavepoint()
	if err := p.FreeRun(head, 2); err != nil {
		t.Fatalf("FreeRun: %v", err)
	}
	// A same-window run allocation must not land on the freed pages.
	valueB := bytes.Repeat([]byte{0xB2}, page.OverflowFirstPageCapacity(cfg)+50)
	newHead := writeDirectRun(t, p, valueB)
	if newHead == head || (newHead < head+2 && newHead+2 > head) {
		t.Fatalf("same-window allocation landed on the freed run [%d,+2): got [%d,+2)", head, newHead)
	}
	p.RestoreSavepoint(sp)

	// Post-restore the original run is referenced again; its bytes
	// must be intact.
	run, err := p.PageRun(head)
	if err != nil {
		t.Fatalf("PageRun on restored run: %v", err)
	}
	ext := page.OverflowRunExtent(run, cfg)
	if !bytes.Equal(ext[:len(valueA)], valueA) {
		t.Fatal("restored run's content was destroyed by a same-window re-allocation")
	}
}

// TestDeferredSameTxFreesApplyAtWindowClose: the deferral is scoped to
// the window — once every savepoint resolves (release), the freed
// pages return to the bitmap and are re-allocatable.
func TestDeferredSameTxFreesApplyAtWindowClose(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()
	cfg := p.Config()

	value := bytes.Repeat([]byte{0xC3}, page.OverflowFirstPageCapacity(cfg)+50)
	head := writeDirectRun(t, p, value)

	sp := p.BeginShallowSavepoint()
	if err := p.FreeRun(head, 2); err != nil {
		t.Fatalf("FreeRun: %v", err)
	}
	for i := range uint64(2) {
		if bm.IsSet(head + i) {
			t.Errorf("page %d free in bitmap inside the window — re-allocatable while restorable", head+i)
		}
	}
	p.ReleaseSavepoint(sp)
	for i := range uint64(2) {
		if !bm.IsSet(head + i) {
			t.Errorf("page %d not freed after the window closed", head+i)
		}
	}
}

// TestDeferredFreesDroppedOnRestore (mutation-hardening): deferred
// frees recorded inside a restored window must be DROPPED — the
// restore reverts the frees, the pages are allocated again with live
// content, and a later window's release must not apply the stale
// deferrals (that would free — and hand to re-allocation — pages the
// tree references).
func TestDeferredFreesDroppedOnRestore(t *testing.T) {
	p, bm, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()
	cfg := p.Config()

	value := bytes.Repeat([]byte{0xD4}, page.OverflowFirstPageCapacity(cfg)+50) // 2-page run
	head := writeDirectRun(t, p, value)

	sp := p.BeginShallowSavepoint()
	if err := p.FreeRun(head, 2); err != nil {
		t.Fatalf("FreeRun: %v", err)
	}
	p.RestoreSavepoint(sp) // free undone; run allocated + referenced again

	// A later, unrelated window resolving must not resurrect the
	// dropped deferrals.
	sp2 := p.BeginShallowSavepoint()
	p.ReleaseSavepoint(sp2)
	for i := range uint64(2) {
		if bm.IsSet(head + i) {
			t.Errorf("page %d freed by a stale deferred entry after restore — live content became re-allocatable", head+i)
		}
	}
	run, err := p.PageRun(head)
	if err != nil {
		t.Fatalf("PageRun after restore: %v", err)
	}
	ext := page.OverflowRunExtent(run, cfg)
	if !bytes.Equal(ext[:len(value)], value) {
		t.Fatal("restored run content wrong")
	}
}
