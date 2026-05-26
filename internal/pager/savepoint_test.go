package pager

import "testing"

// markFree marks pages [first, first+n) free in the bitmap so AllocPage
// has somewhere to allocate from.
func markFree(bm interface{ Set(uint64) }, first uint64, n int) {
	for i := range n {
		bm.Set(first + uint64(i))
	}
}

// TestSavepointReleaseKeepsAllocations: a child commit (ReleaseSavepoint)
// leaves the child's allocations in the parent's pager state.
func TestSavepointReleaseKeepsAllocations(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 32)

	sp := p.BeginSavepoint()
	if p.SavepointDepth() != 1 {
		t.Fatalf("depth = %d, want 1", p.SavepointDepth())
	}
	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if _, err := p.AllocSlab(id); err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	p.ReleaseSavepoint(sp)

	if p.SavepointDepth() != 0 {
		t.Fatalf("depth = %d after release, want 0", p.SavepointDepth())
	}
	if _, ok := p.pendingAllocs[id]; !ok {
		t.Errorf("page %d dropped from pendingAllocs after child commit", id)
	}
	if _, ok := p.dirty[id]; !ok {
		t.Errorf("page %d slab buffer dropped after child commit", id)
	}
}

// TestSavepointRestoreRevertsState (Inv-N2): after RestoreSavepoint the
// pager's tx-scoped freespace state equals its state at BeginSavepoint.
func TestSavepointRestoreRevertsState(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 32)

	// Parent does some work before the savepoint.
	pid, err := p.AllocPage()
	if err != nil {
		t.Fatalf("parent AllocPage: %v", err)
	}
	if _, err := p.AllocSlab(pid); err != nil {
		t.Fatalf("parent AllocSlab: %v", err)
	}

	wantNumFree := bm.NumFree()
	wantHWM := p.HighWaterMark()
	wantDirtyBytes := p.dirtyBytes
	wantAllocs := len(p.pendingAllocs)
	wantDirty := len(p.dirty)

	sp := p.BeginSavepoint()

	// Child allocates and CoWs several pages.
	for i := range 5 {
		cid, err := p.AllocPage()
		if err != nil {
			t.Fatalf("child AllocPage %d: %v", i, err)
		}
		if _, err := p.AllocSlab(cid); err != nil {
			t.Fatalf("child AllocSlab %d: %v", i, err)
		}
	}
	if len(p.pendingAllocs) == wantAllocs {
		t.Fatal("child did not change pendingAllocs (test precondition)")
	}

	p.RestoreSavepoint(sp)

	if got := bm.NumFree(); got != wantNumFree {
		t.Errorf("NumFree = %d after restore, want %d", got, wantNumFree)
	}
	if got := p.HighWaterMark(); got != wantHWM {
		t.Errorf("HWM = %d after restore, want %d", got, wantHWM)
	}
	if got := p.dirtyBytes; got != wantDirtyBytes {
		t.Errorf("dirtyBytes = %d after restore, want %d", got, wantDirtyBytes)
	}
	if got := len(p.pendingAllocs); got != wantAllocs {
		t.Errorf("len(pendingAllocs) = %d after restore, want %d", got, wantAllocs)
	}
	if got := len(p.dirty); got != wantDirty {
		t.Errorf("len(dirty) = %d after restore, want %d", got, wantDirty)
	}
	// Parent's page must survive the child rollback.
	if _, ok := p.dirty[pid]; !ok {
		t.Errorf("parent page %d dropped by child rollback", pid)
	}
	if p.SavepointDepth() != 0 {
		t.Errorf("depth = %d after restore, want 0", p.SavepointDepth())
	}
}

// TestSavepointSuspendsLoosePop (Inv-N1): while a savepoint is active,
// AllocPage does not reuse a loose page — it allocates from the bitmap —
// so a page an ancestor's tree references is never handed out and
// overwritten. After the savepoint resolves, loose-pop resumes.
func TestSavepointSuspendsLoosePop(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 32)

	// Create a loose page: alloc, CoW (installs buffer + dirty), free.
	loose, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	buf, err := p.AllocSlab(loose)
	if err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	buf[0] = 0xAB // ancestor marker
	if err := p.FreePage(loose); err != nil {
		t.Fatalf("FreePage: %v", err)
	}
	if _, ok := p.loosePages[loose]; !ok {
		t.Fatalf("page %d not loose (test precondition)", loose)
	}

	sp := p.BeginSavepoint()
	// Child allocates several pages; none may be the loose page.
	for i := range 5 {
		cid, err := p.AllocPage()
		if err != nil {
			t.Fatalf("child AllocPage %d: %v", i, err)
		}
		if cid == loose {
			t.Fatalf("child reused loose page %d while savepoint active (Inv-N1 violation)", loose)
		}
	}
	if _, ok := p.loosePages[loose]; !ok {
		t.Errorf("loose page %d consumed while savepoint active", loose)
	}
	// Ancestor's buffer untouched.
	if got := (*p.dirty[loose])[0]; got != 0xAB {
		t.Errorf("ancestor buffer byte = %#x, want 0xAB (overwritten by child)", got)
	}

	// Child commits — loose-pop resumes for the parent.
	p.ReleaseSavepoint(sp)
	reused, err := p.AllocPage()
	if err != nil {
		t.Fatalf("post-release AllocPage: %v", err)
	}
	if reused != loose {
		t.Errorf("post-release AllocPage = %d, want loose reuse %d", reused, loose)
	}
}

// TestSavepointRestorePreservesFreedAncestorPage (Inv-N1 / Inv-N2): a
// child that frees a page the ancestor's tree still references (a parent
// same-tx page) must, on rollback, return that page to the pending-alloc
// set with its slab buffer intact — never leave it loose or freed.
func TestSavepointRestorePreservesFreedAncestorPage(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 32)

	// Parent allocates page P and writes a marker.
	pageP, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	buf, err := p.AllocSlab(pageP)
	if err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	buf[0] = 0xCD

	sp := p.BeginSavepoint()
	// Child frees P (parent's page) and allocates elsewhere.
	if err := p.FreePage(pageP); err != nil {
		t.Fatalf("child FreePage: %v", err)
	}
	for i := range 3 {
		if _, err := p.AllocPage(); err != nil {
			t.Fatalf("child AllocPage %d: %v", i, err)
		}
	}
	p.RestoreSavepoint(sp)

	if _, ok := p.pendingAllocs[pageP]; !ok {
		t.Errorf("page %d not restored to pendingAllocs", pageP)
	}
	if _, ok := p.loosePages[pageP]; ok {
		t.Errorf("page %d still loose after rollback", pageP)
	}
	b, ok := p.dirty[pageP]
	if !ok {
		t.Fatalf("page %d slab buffer dropped by rollback", pageP)
	}
	if (*b)[0] != 0xCD {
		t.Errorf("page %d buffer byte = %#x, want 0xCD", pageP, (*b)[0])
	}
}

// TestSavepointRetiredPagesTruncate (Inv-N2): a child freeing a prior-tx
// page appends to retiredPages; rollback truncates it back.
func TestSavepointRetiredPagesTruncate(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 32)

	wantRetired := len(p.retiredPages)
	sp := p.BeginSavepoint()
	// A prior-tx page: not in dirty, not in pendingAllocs → retire path.
	priorTxPage := first + 20
	if err := p.FreePage(priorTxPage); err != nil {
		t.Fatalf("FreePage: %v", err)
	}
	if len(p.retiredPages) != wantRetired+1 {
		t.Fatalf("retiredPages len = %d, want %d", len(p.retiredPages), wantRetired+1)
	}
	p.RestoreSavepoint(sp)
	if len(p.retiredPages) != wantRetired {
		t.Errorf("retiredPages len = %d after rollback, want %d", len(p.retiredPages), wantRetired)
	}
}

// TestSavepointNesting: savepoints nest LIFO; depth tracks correctly and
// an inner rollback does not disturb the outer level's work.
func TestSavepointNesting(t *testing.T) {
	p, bm, f := setupWriter(t, 128)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 64)

	outer := p.BeginSavepoint()
	outerID, err := p.AllocPage()
	if err != nil {
		t.Fatalf("outer AllocPage: %v", err)
	}
	if _, err := p.AllocSlab(outerID); err != nil {
		t.Fatalf("outer AllocSlab: %v", err)
	}

	inner := p.BeginSavepoint()
	if p.SavepointDepth() != 2 {
		t.Fatalf("depth = %d, want 2", p.SavepointDepth())
	}
	innerID, err := p.AllocPage()
	if err != nil {
		t.Fatalf("inner AllocPage: %v", err)
	}
	if _, err := p.AllocSlab(innerID); err != nil {
		t.Fatalf("inner AllocSlab: %v", err)
	}
	p.RestoreSavepoint(inner) // inner rollback

	if p.SavepointDepth() != 1 {
		t.Fatalf("depth = %d after inner rollback, want 1", p.SavepointDepth())
	}
	if _, ok := p.dirty[innerID]; ok {
		t.Errorf("inner page %d survived inner rollback", innerID)
	}
	if _, ok := p.dirty[outerID]; !ok {
		t.Errorf("outer page %d dropped by inner rollback", outerID)
	}

	p.ReleaseSavepoint(outer) // outer commit
	if p.SavepointDepth() != 0 {
		t.Fatalf("depth = %d after outer commit, want 0", p.SavepointDepth())
	}
	if _, ok := p.dirty[outerID]; !ok {
		t.Errorf("outer page %d dropped by outer commit", outerID)
	}
}
