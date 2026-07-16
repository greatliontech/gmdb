package pager

import (
	"bytes"
	"errors"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// Overflow-run pager surfaces (checksums.md §Overflow-Run Digest):
// run-page provenance for the commit footer exemption, and the
// PageRun contiguous-image accessor.

// TestRunPageProvenance: AllocSlabRun records every page of the run as
// footer-exempt; a later single-page install at the same id (AllocSlab
// or CoW — a run freed in-tx and its id re-allocated as a node page)
// supersedes the run provenance so the node page is footer-stamped at
// commit; ReleaseAll clears the set at the tx boundary.
func TestRunPageProvenance(t *testing.T) {
	p, _, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	first, err := p.AllocContiguous(3)
	if err != nil {
		t.Fatalf("AllocContiguous: %v", err)
	}
	if _, err := p.AllocSlabRun(first, 3); err != nil {
		t.Fatalf("AllocSlabRun: %v", err)
	}
	for i := range uint64(3) {
		if _, ok := p.runPages[first+i]; !ok {
			t.Errorf("run page %d not recorded footer-exempt", first+i)
		}
	}

	// Node-page reuse supersedes run provenance.
	if _, err := p.AllocSlab(first); err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	if _, ok := p.runPages[first]; ok {
		t.Errorf("AllocSlab did not clear run provenance at %d — the node page would commit without a footer", first)
	}
	if _, err := p.CoW(first, first+1); err != nil {
		t.Fatalf("CoW: %v", err)
	}
	if _, ok := p.runPages[first+1]; ok {
		t.Errorf("CoW did not clear run provenance at %d", first+1)
	}
	if _, ok := p.runPages[first+2]; !ok {
		t.Errorf("untouched run page %d lost its provenance", first+2)
	}

	p.ReleaseAll()
	if len(p.runPages) != 0 {
		t.Errorf("ReleaseAll left %d stale run-provenance entries", len(p.runPages))
	}
}

// TestPageRunDirtyAssembly: a run written in the current tx (slab
// buffers) comes back from PageRun as a contiguous image byte-identical
// to the committed on-disk form, digest-verifiable, with the extent
// intact.
func TestPageRunDirtyAssembly(t *testing.T) {
	p, _, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()
	cfg := p.Config()

	value := bytes.Repeat([]byte{0x6B}, page.OverflowFirstPageCapacity(cfg)+300)
	runLen := page.OverflowRunLength(cfg, uint64(len(value)))
	first, err := p.AllocContiguous(runLen)
	if err != nil {
		t.Fatalf("AllocContiguous: %v", err)
	}
	pages, err := p.AllocSlabRun(first, runLen)
	if err != nil {
		t.Fatalf("AllocSlabRun: %v", err)
	}
	if err := page.EncodeOverflowRun(pages, cfg, value); err != nil {
		t.Fatalf("EncodeOverflowRun: %v", err)
	}

	run, err := p.PageRun(first)
	if err != nil {
		t.Fatalf("PageRun on dirty run: %v", err)
	}
	if len(run) != int(runLen)*int(cfg.PageSize) {
		t.Fatalf("run image %d bytes, want %d", len(run), int(runLen)*int(cfg.PageSize))
	}
	var want []byte
	for _, b := range pages {
		want = append(want, b...)
	}
	if !bytes.Equal(run, want) {
		t.Fatal("dirty-run image differs from the slab buffers' concatenation")
	}
	if !page.VerifyOverflowRun(run, cfg) {
		t.Fatal("dirty-run image fails whole-run digest verification")
	}
	ext := page.OverflowRunExtent(run, cfg)
	if !bytes.Equal(ext[:len(value)], value) {
		t.Fatal("extent mismatch")
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

// TestRunProvenanceSurvivesShallowRestore (regression): a shallow
// savepoint window that frees a pre-window run, loose-pops one of its
// ids into a NODE page, and then restores must revive the run's
// footer-exemption provenance alongside the run buffer the loose-pop
// replay re-attaches — otherwise commitStep1 stamps a footer into a
// live overflow-run page (8 bytes of extent destroyed + digest
// mismatch on every later Get).
func TestRunProvenanceSurvivesShallowRestore(t *testing.T) {
	p, _, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()
	cfg := p.Config()

	value := bytes.Repeat([]byte{0x42}, page.OverflowFirstPageCapacity(cfg)+100) // 2-page run
	runLen := page.OverflowRunLength(cfg, uint64(len(value)))
	first, err := p.AllocContiguous(runLen)
	if err != nil {
		t.Fatalf("AllocContiguous: %v", err)
	}
	pages, err := p.AllocSlabRun(first, runLen)
	if err != nil {
		t.Fatalf("AllocSlabRun: %v", err)
	}
	if err := page.EncodeOverflowRun(pages, cfg, value); err != nil {
		t.Fatalf("encode: %v", err)
	}

	sp := p.BeginShallowSavepoint()
	if err := p.FreeRun(first, runLen); err != nil {
		t.Fatalf("FreeRun: %v", err)
	}
	// Loose-pop one of the freed run ids and install a node page there.
	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if id < first || id >= first+uint64(runLen) {
		t.Fatalf("allocator did not loose-pop a run id (got %d, run [%d,+%d))", id, first, runLen)
	}
	if _, err := p.AllocSlab(id); err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	p.RestoreSavepoint(sp)

	// Post-restore the run is live again and its buffer back in dirty;
	// its provenance must be back too.
	if _, ok := p.dirty[id]; !ok {
		t.Fatalf("restore did not re-attach the run buffer at %d", id)
	}
	if _, isRun := p.runPages[id]; !isRun {
		t.Errorf("runPages[%d] lost across shallow restore — commitStep1 would stamp a footer into a live overflow-run page", id)
	}
}

// TestNodePageDoesNotInheritRunProvenanceAcrossRestore (regression,
// mirror image): a pre-window NODE page freed in-window and
// loose-popped into a 1-page overflow run must NOT keep the run
// provenance after restore re-attaches the node buffer — otherwise
// the node page commits with no footer and every later read fails
// ErrBadPageChecksum on healthy data.
func TestNodePageDoesNotInheritRunProvenanceAcrossRestore(t *testing.T) {
	p, _, f := setupWriter(t, 32)
	defer p.Close()
	defer f.Close()

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if _, err := p.AllocSlab(id); err != nil {
		t.Fatalf("AllocSlab node: %v", err)
	}

	sp := p.BeginShallowSavepoint()
	if err := p.FreePage(id); err != nil {
		t.Fatalf("FreePage: %v", err)
	}
	rid, err := p.AllocContiguous(1)
	if err != nil {
		t.Fatalf("AllocContiguous: %v", err)
	}
	if rid != id {
		t.Fatalf("allocator did not loose-pop the node id (got %d, want %d)", rid, id)
	}
	if _, err := p.AllocSlabRun(rid, 1); err != nil {
		t.Fatalf("AllocSlabRun: %v", err)
	}
	p.RestoreSavepoint(sp)

	if _, ok := p.dirty[id]; !ok {
		t.Fatalf("restore did not re-attach the node buffer at %d", id)
	}
	if _, isRun := p.runPages[id]; isRun {
		t.Errorf("stale runPages[%d] after shallow restore — commitStep1 would skip the footer on a live node page", id)
	}
}
