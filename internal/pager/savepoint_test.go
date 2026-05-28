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

// TestShallowSavepointPreservesLoosePop verifies that a SHALLOW
// savepoint does NOT suspend loose-pop the way nested savepoints do
// (the writenewindexregistry-partial-leak per-row case's correctness
// + cost contract). A child Loose-pop within the shallow window is
// allowed; the existing nested-savepoint Inv-N1 mechanic (suspension
// while savepointDepth > 0) stays unchanged.
func TestShallowSavepointPreservesLoosePop(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 32)

	// Seed: alloc + CoW + free → loose page with a known buffer.
	loose, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if _, err := p.AllocSlab(loose); err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	if err := p.FreePage(loose); err != nil {
		t.Fatalf("FreePage: %v", err)
	}
	if _, ok := p.loosePages[loose]; !ok {
		t.Fatalf("page %d not loose (test precondition)", loose)
	}

	sp := p.BeginShallowSavepoint()
	// Loose-pop MUST occur during the shallow window — that's the
	// across-Put bounded-file-growth property the shallow kind
	// exists to preserve.
	got, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage in shallow window: %v", err)
	}
	if got != loose {
		t.Fatalf("AllocPage in shallow window = %d, want loose-pop %d (loose-pop wrongly suspended)", got, loose)
	}
	if _, ok := p.loosePages[loose]; ok {
		t.Errorf("loose page %d still in loosePages after pop", loose)
	}
	p.ReleaseSavepoint(sp)
}

// TestShallowSavepointRestoreReversesLoosePop is the regression test
// for the loose-pop-replay branch in RestoreSavepoint (shallow kind).
// Stash the original-buffer marker pre-savepoint; loose-pop the page
// inside the shallow window; CoW a "corrupted" marker; Restore the
// savepoint; verify p.dirty[id] holds the ORIGINAL buffer again.
//
// Without the replay (i.e., commenting out `p.dirty[entry.id] =
// entry.buf` in savepoint.go) the assertion fails — dirty[loose]
// holds the corrupted buffer, the original is dropped from
// detachedBufs without re-attach, and a future Page(loose) call
// reads the post-Restore-stale CoW content (a buffer-leak from the
// pool's perspective, and a byte-slice ownership invariant
// violation if anyone retained a []byte into the original).
func TestShallowSavepointRestoreReversesLoosePop(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 32)

	loose, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	origBuf, err := p.AllocSlab(loose)
	if err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	origBuf[0] = 0xAA // original-buffer marker
	if err := p.FreePage(loose); err != nil {
		t.Fatalf("FreePage: %v", err)
	}

	sp := p.BeginShallowSavepoint()
	popped, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage in shallow window: %v", err)
	}
	if popped != loose {
		t.Fatalf("AllocPage = %d, want loose-pop %d", popped, loose)
	}
	// The loose-pop detached origBuf to detachedBufs. CoW installs a
	// fresh post-pop buffer.
	postPopBuf, err := p.AllocSlab(loose)
	if err != nil {
		t.Fatalf("AllocSlab post-pop: %v", err)
	}
	postPopBuf[0] = 0xCC // corrupted marker

	p.RestoreSavepoint(sp)

	// Replay must have re-attached origBuf to dirty[loose] and
	// pool-Put'd postPopBuf — observe via the marker byte.
	got, ok := p.dirty[loose]
	if !ok {
		t.Fatalf("page %d missing from dirty after shallow restore", loose)
	}
	if (*got)[0] != 0xAA {
		t.Errorf("post-restore dirty[%d][0] = %#x, want 0xAA (original) — loose-pop replay regressed", loose, (*got)[0])
	}
	// Loose-pages restoration: page is back in loosePages, not in
	// pendingAllocs.
	if _, ok := p.loosePages[loose]; !ok {
		t.Errorf("page %d not in loosePages after shallow restore", loose)
	}
	if _, ok := p.pendingAllocs[loose]; ok {
		t.Errorf("page %d still in pendingAllocs after shallow restore", loose)
	}
}

// TestShallowSavepointReleaseKeepsLoosePop verifies the
// commit-success path: when a shallow savepoint Release's, the
// loose-pop stays committed (page remains allocated, dirty[id] has
// the post-pop buffer). The detached original buffer stays alive
// in detachedBufs for tx-end pool-Put — same as a loose-pop with
// no savepoint wrapping.
func TestShallowSavepointReleaseKeepsLoosePop(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 32)

	loose, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if _, err := p.AllocSlab(loose); err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	if err := p.FreePage(loose); err != nil {
		t.Fatalf("FreePage: %v", err)
	}

	sp := p.BeginShallowSavepoint()
	popped, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if popped != loose {
		t.Fatalf("AllocPage = %d, want loose-pop %d", popped, loose)
	}
	postPopBuf, err := p.AllocSlab(loose)
	if err != nil {
		t.Fatalf("AllocSlab post-pop: %v", err)
	}
	postPopBuf[0] = 0xCC

	p.ReleaseSavepoint(sp)

	// Release commits the loose-pop: page is allocated, dirty[id]
	// has the post-pop buffer, NOT in loosePages anymore.
	if _, ok := p.loosePages[loose]; ok {
		t.Errorf("page %d still in loosePages after shallow release (should be allocated)", loose)
	}
	if _, ok := p.pendingAllocs[loose]; !ok {
		t.Errorf("page %d not in pendingAllocs after shallow release", loose)
	}
	got, ok := p.dirty[loose]
	if !ok {
		t.Fatalf("page %d missing from dirty after shallow release", loose)
	}
	if (*got)[0] != 0xCC {
		t.Errorf("post-release dirty[%d][0] = %#x, want 0xCC (post-pop committed)", loose, (*got)[0])
	}
}

// TestShallowSavepointOutOfOrderPanics pins the strict-LIFO guard on
// shallow savepoint resolution (transactions.md §Nested Transactions:
// "out-of-order Restore or Discard panics rather than silently
// corrupt state"). Without the guard, Restoring the outer first
// would silently no-op the pop, leave the inner dangling in
// activeShallowSavepoints, and surface as a delayed bitmap-snapshot
// panic at the inner's eventual resolution. Pre-emptive panic
// matches bitmap.go's openSnapshots LIFO behaviour.
func TestShallowSavepointOutOfOrderPanics(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 16)

	outer := p.BeginShallowSavepoint()
	inner := p.BeginShallowSavepoint()
	_ = inner

	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("RestoreSavepoint of outer-while-inner-open did not panic")
		}
		// Drain remaining state — Close cleans up.
	}()
	p.RestoreSavepoint(outer)
}

// TestShallowSavepointDoesNotSuspendNestedInv ensures shallow and
// nested kinds coexist without breaking Inv-N1: with a NESTED
// savepoint already active (savepointDepth > 0), a shallow savepoint
// inside it must NOT re-enable loose-pop. The nested kind's loose-
// pop suspension wins regardless of the shallow's relaxed mechanic.
func TestShallowSavepointDoesNotSuspendNestedInv(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 32)

	loose, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if _, err := p.AllocSlab(loose); err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	if err := p.FreePage(loose); err != nil {
		t.Fatalf("FreePage: %v", err)
	}

	nested := p.BeginSavepoint()
	shallow := p.BeginShallowSavepoint()
	got, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage under nested+shallow: %v", err)
	}
	if got == loose {
		t.Errorf("AllocPage = %d, loose-popped while nested savepoint active (Inv-N1 violation)", loose)
	}
	if len(shallow.loosePopLog) != 0 {
		t.Errorf("shallow.loosePopLog len = %d, want 0 (loose-pop should have been suspended)", len(shallow.loosePopLog))
	}
	p.ReleaseSavepoint(shallow)
	p.ReleaseSavepoint(nested)
}
