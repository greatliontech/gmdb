package pager

import (
	"strings"
	"testing"
)

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
// (the per-row index-maintenance correctness + cost contract from
// transactions.md §Write-helper error contract). A child Loose-pop
// within the shallow window is allowed; the existing nested-
// savepoint Inv-N1 mechanic (suspension while savepointDepth > 0)
// stays unchanged.
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

// TestSavepointOutOfOrderPanics pins the strict-LIFO guard on
// savepoint resolution (transactions.md §Nested Transactions:
// "out-of-order Restore or Discard panics rather than silently
// corrupt state"). The guard is kind-agnostic — it lives in
// RestoreSavepoint / ReleaseSavepoint and fires before any
// kind-specific work. NESTED is used here because two SHALLOWs can
// no longer coexist (TestShallowSavepointPanicsOnNestedShallow
// pins the single-active SHALLOW rule); the LIFO contract under
// either kind goes through the same shared check.
//
// Without the guard, Restoring the outer first would silently no-op
// the pop, leave the inner dangling in activeSavepoints with a
// stale bitmap snapshot in openSnapshots, and surface as a delayed
// bitmap-snapshot panic at the inner's eventual resolution.
// Pre-emptive panic matches bitmap.go's openSnapshots LIFO
// behaviour.
func TestSavepointOutOfOrderPanics(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 16)

	outer := p.BeginSavepoint()
	inner := p.BeginSavepoint()
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

// TestShallowSavepointBeginCostConstantInTxState pins the
// transactions.md §Nested Transactions cost contract: BeginShallowSavepoint
// must run in O(1) time and allocations regardless of cumulative tx
// state at Begin time — proportional to per-window mutations only
// (which is 0 at the moment of Begin).
//
// The pre-fix implementation cloned pendingAllocs / pendingFrees /
// loosePages and built a dirtyKeys set per call (4 map operations each
// proportional to current tx state). For an OLTP workload that opens
// many shallow savepoints in one tx (the canonical per-row indexed
// maintenance loop fires one per Put), the per-Begin cost summed to
// O(N²·depth) clone work across N indexed Puts — a clause-explicit
// violation of "Cost is proportional to pages modified since the
// outermost open savepoint, plus O(bitmap-pages currently dirty) for
// the bitmap-dirty-set clone, not total database size."
//
// The post-fix design captures only an undo-log marker (undoLogPos)
// plus the bitmap.Snapshot marker, both O(1) per Begin. Per-mutation
// undo entries appended during the window absorb the work; Restore
// replays only the per-window entries. Total cost across N savepoints
// becomes O(per-window mutations) = O(N), not O(N²).
//
// Test demonstrates the fault by measuring per-Begin allocation count:
// pre-fix grows with prior tx state (maps.Clone of 100-entry maps);
// post-fix stays at the small constant set (Savepoint struct + bitmap
// Snapshot's small clones).
func TestShallowSavepointBeginCostConstantInTxState(t *testing.T) {
	p, bm, f := setupWriter(t, 16384)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 1024)

	// Accumulate cumulative tx state so the pre-fix map clones would
	// have meaningful size. 100 alloc+CoW cycles populate
	// pendingAllocs and dirty with 100 entries each.
	for i := range 100 {
		id, err := p.AllocPage()
		if err != nil {
			t.Fatalf("AllocPage %d: %v", i, err)
		}
		if _, err := p.AllocSlab(id); err != nil {
			t.Fatalf("AllocSlab %d: %v", i, err)
		}
	}

	allocs := testing.AllocsPerRun(20, func() {
		sp := p.BeginShallowSavepoint()
		p.ReleaseSavepoint(sp)
	})

	// Post-fix per Begin+Release cycle:
	//   - Savepoint struct: 1 alloc.
	//   - slices.Clone(rplSegments): 0 (chain empty at startup).
	//   - bitmap.Snapshot: Snapshot struct + maps.Clone(b.dirty,
	//     small constant ≤ 2048 entries): 2 allocs.
	//   - activeSavepoints append: amortised 0 after the first grow.
	// Total: ~3-4 allocations per cycle.
	//
	// Pre-fix added 4 cloned maps (pendingAllocs/Frees/loosePages +
	// dirtyKeys), each maps.Clone or make() = ~2 allocs each = ~8
	// extra. Pre-fix total: ~11-12 per cycle.
	//
	// Threshold 6 is generous post-fix (4 actual + 2 slack) and
	// detects the pre-fix shape cleanly.
	if allocs > 6 {
		t.Errorf("BeginShallowSavepoint+Release allocates %.1f per cycle with 100-op prior tx state; want ≤ 6 (the per-Begin O(1) cost invariant — pendingAllocs/Frees/loosePages/dirtyKeys map clones would push this to ~12+)", allocs)
	}
}

// TestShallowSavepointLoosePopReCoWRestore pins the Restore ordering
// for the worst interleave the unified per-pager undo-log design must
// handle: pre-window dirty[id] = bufA; in window FreePage(id) →
// loose; AllocPage loose-pop detaches bufA; CoW(srcID, id) installs
// bufB; Restore.
//
// Pre-fix the clone-based restore handled this by capturing dirtyKeys
// at Begin and dropping any post-Begin additions to p.dirty at
// Restore. Post-fix the savepointUndoLog records (fieldDirty, id, false)
// for the second CoW; Restore replays this BEFORE the loosePopLog
// replay, deleting bufB and leaving dirty[id] absent — exactly the
// pre-loose-pop-replay state — so the loose-pop replay can cleanly
// re-install bufA without double-Put on the same id.
//
// Without the "savepointUndoLog first, then loosePopLog" order in
// RestoreSavepoint (savepoint.go), this test fails: dirty[id] ends
// up absent (the savepointUndoLog replay pool-Puts the just-re-
// attached bufA after loose-pop's replay), or holds bufB (if the
// fieldDirty entry never fires), or double-Puts bufA (corrupting the
// pool).
func TestShallowSavepointLoosePopReCoWRestore(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 32)

	// Pre-window: allocate id and CoW (dirty[id] = bufA marker 0xAA).
	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	bufA, err := p.AllocSlab(id)
	if err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	bufA[0] = 0xAA

	// Open shallow window.
	sp := p.BeginShallowSavepoint()

	// In window step 1: FreePage(id) — same-tx path → loose.
	if err := p.FreePage(id); err != nil {
		t.Fatalf("FreePage in window: %v", err)
	}
	if _, ok := p.loosePages[id]; !ok {
		t.Fatalf("page %d not in loosePages after in-window FreePage (test precondition)", id)
	}

	// In window step 2: AllocPage loose-pops id (detaches bufA).
	popped, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage in window: %v", err)
	}
	if popped != id {
		t.Fatalf("AllocPage = %d, want loose-pop %d (test precondition)", popped, id)
	}
	if _, ok := p.dirty[id]; ok {
		t.Fatalf("dirty[%d] still present after loose-pop (test precondition)", id)
	}

	// In window step 3: re-CoW the same id (installs bufB).
	bufB, err := p.AllocSlab(id)
	if err != nil {
		t.Fatalf("AllocSlab post-loose-pop: %v", err)
	}
	bufB[0] = 0xBB

	// Restore.
	p.RestoreSavepoint(sp)

	// Pre-window state must be recovered: dirty[id] holds bufA
	// (0xAA marker); id is back in pendingAllocs (from the pre-
	// window AllocPage); loosePages does not hold id.
	got, ok := p.dirty[id]
	if !ok {
		t.Fatalf("dirty[%d] missing after Restore (loose-pop replay regressed)", id)
	}
	if (*got)[0] != 0xAA {
		t.Errorf("dirty[%d][0] = %#x after Restore, want 0xAA (pre-window bufA marker)", id, (*got)[0])
	}
	if _, ok := p.pendingAllocs[id]; !ok {
		t.Errorf("page %d not in pendingAllocs after Restore", id)
	}
	if _, ok := p.loosePages[id]; ok {
		t.Errorf("page %d in loosePages after Restore (in-window FreePage not reverted)", id)
	}
}

// TestSavepointUndoLogTruncatesOnLastRelease verifies the bitmap-
// undo-log-style truncation: log entries persist while any savepoint
// is open, but a Release that empties the activeSavepoints stack
// truncates the log to 0 so per-tx memory does not accumulate
// across savepoint windows.
//
// Mirrors bitmap.Discard's "if openSnapshots becomes empty, truncate
// undoLog" pattern from 0893be5 — and is required for the same
// reason: without it, indexed-OLTP workloads opening N shallow
// savepoints would leak each window's mutation log into the next
// window's log indexing.
func TestSavepointUndoLogTruncatesOnLastRelease(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 16)

	if got := len(p.savepointUndoLog); got != 0 {
		t.Fatalf("log non-empty at fixture init: %d", got)
	}

	sp := p.BeginShallowSavepoint()

	// In-window mutations populate the log.
	for range 5 {
		if _, err := p.AllocPage(); err != nil {
			t.Fatalf("AllocPage: %v", err)
		}
	}

	if got := len(p.savepointUndoLog); got == 0 {
		t.Fatalf("log empty after in-window mutations; expected entries")
	}

	p.ReleaseSavepoint(sp)

	if got := len(p.savepointUndoLog); got != 0 {
		t.Errorf("savepointUndoLog len = %d after last Release; want 0 (truncate-on-empty contract)", got)
	}
}

// TestShallowSavepointInWindowAllocLoosePopRestoreDoesNotLeak pins
// the loose-pop replay's wasPreWindow branching: a Shallow window
// that ALLOCATES a fresh page, installs a slab buffer (CoW or
// AllocSlab), then frees that page (same-tx → loose), then re-
// allocates the same id via loose-pop, then Restores — the in-
// window-installed buffer MUST NOT survive into the post-Restore
// dirty map. Pre-window dirty[id] was absent; post-Restore dirty[id]
// must be absent too (Inv-N2: post-Restore state matches pre-Begin).
//
// Without the wasPreWindow flag in loosePopEntry (i.e., the loose-
// pop replay unconditionally re-attaches entry.buf to dirty[id]),
// the in-window AllocSlab buffer leaks into post-Restore dirty —
// breaking Inv-N2 and the dirtyBytes accounting (pre-window
// dirtyBytes was 0; post-Restore dirty[id] holds PageSize bytes of
// in-window content, yet dirtyBytes is restored to 0 from
// sp.dirtyBytes). A subsequent ReleaseAll iterates dirty and pool-
// Puts the buffer — but a same-tx caller that observes dirty mid-
// tx (commit step 1's DirtyIDs iteration) would see and pwrite an
// in-window-only page.
//
// The wasPreWindow flag distinguishes this case from the pre-
// existing test TestShallowSavepointRestoreReversesLoosePop, which
// covers the inverse: pre-window dirty[id] = bufA, in-window
// loose-pop detaches bufA, in-window CoW installs bufB. There the
// flag is true and the replay re-attaches bufA correctly.
func TestShallowSavepointInWindowAllocLoosePopRestoreDoesNotLeak(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 32)

	preDirty := len(p.dirty)
	preDirtyBytes := p.dirtyBytes
	prePendingAllocs := len(p.pendingAllocs)
	preLoose := len(p.loosePages)

	sp := p.BeginShallowSavepoint()

	// In window: AllocPage (bitmap-hit) + AllocSlab — pre-window
	// dirty[id] was absent; in-window install adds it.
	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage in window: %v", err)
	}
	if _, err := p.AllocSlab(id); err != nil {
		t.Fatalf("AllocSlab in window: %v", err)
	}

	// In window: FreePage(id) → same-tx loose.
	if err := p.FreePage(id); err != nil {
		t.Fatalf("FreePage: %v", err)
	}
	if _, ok := p.loosePages[id]; !ok {
		t.Fatalf("page %d not loose (test precondition)", id)
	}

	// In window: AllocPage loose-pops id — detaches the in-window
	// buffer to detachedBufs; loosePopLog appends (id, bufA,
	// wasPreWindow=false).
	popped, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage loose-pop: %v", err)
	}
	if popped != id {
		t.Fatalf("AllocPage = %d, want loose-pop %d", popped, id)
	}
	if _, ok := p.dirty[id]; ok {
		t.Fatalf("dirty[%d] still present after loose-pop (test precondition)", id)
	}

	// Restore.
	p.RestoreSavepoint(sp)

	// Pre-window state must be recovered exactly. The in-window
	// buffer that was detached by the loose-pop MUST NOT be re-
	// installed (wasPreWindow=false). dirtyBytes must match pre-
	// window (0).
	if got, ok := p.dirty[id]; ok {
		t.Errorf("dirty[%d] present after Restore = %p; want absent (pre-window state had no dirty for this id — in-window-allocated buffer must not leak via loose-pop replay)", id, got)
	}
	if got := len(p.dirty); got != preDirty {
		t.Errorf("len(p.dirty) = %d after Restore, want %d (pre-window)", got, preDirty)
	}
	if got := p.dirtyBytes; got != preDirtyBytes {
		t.Errorf("dirtyBytes = %d after Restore, want %d (pre-window)", got, preDirtyBytes)
	}
	if got := len(p.pendingAllocs); got != prePendingAllocs {
		t.Errorf("len(p.pendingAllocs) = %d after Restore, want %d (pre-window)", got, prePendingAllocs)
	}
	if got := len(p.loosePages); got != preLoose {
		t.Errorf("len(p.loosePages) = %d after Restore, want %d (pre-window)", got, preLoose)
	}
}

// TestSavepointRestoreOuterRevertsInnerReleasedWork pins the per-pager
// log shared-by-outer semantic: when an inner savepoint Releases
// (commits) work that the outer later Restores (rolls back), the
// outer's Restore MUST undo the inner's committed work because that
// work became part of the outer's post-Begin state.
//
// The unified per-pager savepointUndoLog naturally encodes this: the
// inner's log entries (between inner.undoLogPos and inner-end) stay
// in the log past inner Release because outer remains active. When
// outer Restores, it replays log[outer.undoLogPos:end-after-inner-
// release] in reverse — which includes the inner-window entries.
//
// Pre-fix this worked because outer's captured clone of pendingAllocs
// at Begin time was wholesale-restored, naturally including pre-inner
// state. Post-fix the equivalent guarantee comes from the log
// retention rule in ReleaseSavepoint (truncate only when activeSavepoints
// becomes empty).
//
// Uses NESTED savepoints because two SHALLOWs can no longer coexist
// (TestShallowSavepointPanicsOnNestedShallow pins the single-active
// SHALLOW rule). The savepointUndoLog substrate is shared across
// kinds — per-Savepoint marker, per-pager log, replay on Restore —
// so the invariant tested here applies equally to either kind. Under
// NESTED, AllocPage takes the bitmap.FindFirst branch (loose-pop
// suspended via savepointDepth > 0) which records the same
// (fieldPendingAllocs, id, false) undo entries the SHALLOW path
// would have under loose-pop-disabled state.
func TestSavepointRestoreOuterRevertsInnerReleasedWork(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 32)

	preAllocs := len(p.pendingAllocs)
	preDirty := len(p.dirty)

	outer := p.BeginSavepoint()

	// Outer-window does some work.
	outerID, err := p.AllocPage()
	if err != nil {
		t.Fatalf("outer AllocPage: %v", err)
	}
	if _, err := p.AllocSlab(outerID); err != nil {
		t.Fatalf("outer AllocSlab: %v", err)
	}

	inner := p.BeginSavepoint()

	// Inner-window does more work.
	innerID, err := p.AllocPage()
	if err != nil {
		t.Fatalf("inner AllocPage: %v", err)
	}
	if _, err := p.AllocSlab(innerID); err != nil {
		t.Fatalf("inner AllocSlab: %v", err)
	}

	// Inner commits — its mutations stay in the parent's pager state
	// pending the outer's resolution.
	p.ReleaseSavepoint(inner)

	if _, ok := p.pendingAllocs[innerID]; !ok {
		t.Fatalf("inner page %d dropped from pendingAllocs by inner Release (committed work lost)", innerID)
	}

	// Outer rolls back — must revert BOTH outer's and inner-committed
	// work, returning to pre-outer state.
	p.RestoreSavepoint(outer)

	if _, ok := p.pendingAllocs[innerID]; ok {
		t.Errorf("inner page %d still in pendingAllocs after outer Restore (inner-released work not reverted)", innerID)
	}
	if _, ok := p.pendingAllocs[outerID]; ok {
		t.Errorf("outer page %d still in pendingAllocs after outer Restore", outerID)
	}
	if got := len(p.pendingAllocs); got != preAllocs {
		t.Errorf("len(pendingAllocs) = %d after outer Restore, want %d", got, preAllocs)
	}
	if got := len(p.dirty); got != preDirty {
		t.Errorf("len(dirty) = %d after outer Restore, want %d", got, preDirty)
	}
}

// TestShallowSavepointPanicsOnNestedShallow pins the single-active
// SHALLOW invariant (transactions.md §Nested Transactions /
// §Write-helper error contract): at most one SHALLOW savepoint may be
// active on the pager at any moment. BeginShallowSavepoint panics if
// another shallow savepoint is already unresolved on
// p.activeSavepoints.
//
// Why it's a panic, not an error. Two simultaneously-active SHALLOW
// savepoints both record the SAME loose-popped *[]byte pointer in
// their loosePopLogs — freespace.go's loose-pop branch walks every
// active SHALLOW savepoint and appends a loosePopEntry with the buf
// pointer it just detached, with no per-savepoint cloning of the
// underlying []byte (the buf IS the original slab buffer being
// transferred). Both entries get wasPreWindow=true because the
// pre-window scan of sp.undoLogPos..end finds no (fieldDirty, id,
// false) entry for either. On Restore (inner-first, LIFO):
//
//   - Inner Restore step-4: dirty[id] is absent (loose-pop deleted
//     it); cur,ok := p.dirty[id]; !ok → no pool-Put; p.dirty[id] =
//     entry.buf re-attaches the original buffer.
//   - Outer Restore step-4: dirty[id] = buf (the same pointer the
//     inner just re-installed); cur,ok := p.dirty[id]; ok →
//     p.bufPool.Put(buf); p.dirty[id] = entry.buf RE-INSTALLS the
//     same pointer.
//
// End state: buf is simultaneously in bufPool's free list AND in
// p.dirty[id]. A subsequent bufPool.Get() returns buf to a new
// caller, who writes to it, silently corrupting the page content at
// p.dirty[id]. This is a use-after-free in user-visible page data,
// fired by a programming-discipline violation at the API surface;
// panic is the conventional response (same posture as bitmap.go's
// openSnapshots LIFO discipline and RestoreSavepoint's out-of-order
// guard — see TestNestedSavepointOutOfOrderPanics).
//
// SHALLOW-inside-NESTED is the legitimate cross-kind combination
// (allowed): NESTED suspends loose-pop via savepointDepth > 0, so
// no loose-pop events fire inside the SHALLOW window and no alias
// can form. Pinned by TestShallowSavepointDoesNotSuspendNestedInv.
func TestShallowSavepointPanicsOnNestedShallow(t *testing.T) {
	p, _, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()

	outer := p.BeginShallowSavepoint()
	if outer == nil {
		t.Fatalf("BeginShallowSavepoint on writable pager returned nil")
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("BeginShallowSavepoint with another shallow already active did not panic")
		} else if msg, ok := r.(string); !ok || !strings.Contains(msg, "shallow savepoint already active") {
			// Pin the panic identity: future maintenance could move the
			// panic to a different site (e.g. captureSavepointState
			// itself) and a bare r != nil check would still pass while
			// no longer asserting THIS guard fired. The message text is
			// the contract surface — it appears in user-facing stack
			// traces — and is grep-anchored by the spec amend
			// (transactions.md §Write-helper error contract).
			t.Errorf("panic = %v, want message containing %q", r, "shallow savepoint already active")
		}
		// Clean up the outer so the test ends in a consistent state.
		// The panic fires BEFORE captureSavepointState / append, so
		// activeSavepoints is unchanged; outer remains the top entry.
		// A second defer swallows any unexpected panic from Release
		// so it surfaces as a test failure, not a goroutine kill.
		defer func() {
			if rr := recover(); rr != nil {
				t.Errorf("ReleaseSavepoint(outer) panicked: %v", rr)
			}
		}()
		p.ReleaseSavepoint(outer)
	}()

	_ = p.BeginShallowSavepoint()
}

// TestNestedInsideShallowSavepointAllowed pins the cross-kind
// allowance documented in transactions.md §Nested Transactions §Why
// this is cheap and BeginShallowSavepoint godoc: a NESTED savepoint
// may open inside an active SHALLOW savepoint without panic. The
// safety argument: the NESTED's Begin establishes loose-pop
// suspension (savepointDepth 0 → 1) for its window, so no new
// loose-pop event fires during the NESTED window — the outer
// SHALLOW's loosePopLog cannot grow inside the NESTED window, and
// the NESTED's resolution leaves the SHALLOW's existing
// loosePopLog entries (if any) intact.
//
// Asserts:
//   - Both Begin calls succeed (no panic from the single-active
//     SHALLOW guard; NESTED is a different kind so the guard skips).
//   - During the NESTED window, an AllocPage takes the bitmap branch
//     (loose-pop suspended); the result is NOT a previously freed
//     loose page.
//   - The outer SHALLOW resolves cleanly after the inner NESTED
//     resolves (no LIFO violation, no leftover loosePopLog alias).
func TestNestedInsideShallowSavepointAllowed(t *testing.T) {
	p, bm, f := setupWriter(t, 64)
	defer p.Close()
	defer f.Close()
	first := bm.FirstDataPage()
	markFree(bm, first, 32)

	// Pre-stage a loose page so we can verify loose-pop is suspended
	// under the NESTED window. Without the SHALLOW open, this loose
	// page would be the next AllocPage result.
	loose, err := p.AllocPage()
	if err != nil {
		t.Fatalf("loose AllocPage: %v", err)
	}
	if _, err := p.AllocSlab(loose); err != nil {
		t.Fatalf("loose AllocSlab: %v", err)
	}
	if err := p.FreePage(loose); err != nil {
		t.Fatalf("loose FreePage: %v", err)
	}

	shallow := p.BeginShallowSavepoint()
	if shallow == nil {
		t.Fatalf("BeginShallowSavepoint on writable pager returned nil")
	}

	// Open NESTED inside the SHALLOW window. This must NOT panic —
	// the single-active SHALLOW guard checks kind == SavepointShallow
	// and skips NESTED entries.
	nested := p.BeginSavepoint()
	if nested == nil {
		t.Fatalf("BeginSavepoint on writable pager returned nil")
	}
	if got := p.SavepointDepth(); got != 1 {
		t.Errorf("SavepointDepth = %d under NESTED-inside-SHALLOW, want 1", got)
	}

	// Loose-pop must be suspended inside the NESTED window
	// regardless of the outer SHALLOW. If suspension fails, AllocPage
	// returns the previously-loose page, aliasing the outer
	// SHALLOW's would-be loosePopLog.
	got, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage inside NESTED-inside-SHALLOW: %v", err)
	}
	if got == loose {
		t.Errorf("AllocPage = %d = loose-popped under nested savepoint (loose-pop not suspended)", loose)
	}
	if _, err := p.AllocSlab(got); err != nil {
		t.Fatalf("AllocSlab inside NESTED-inside-SHALLOW: %v", err)
	}

	// The outer SHALLOW's loosePopLog must remain empty — no
	// loose-pop fired inside the NESTED window.
	if n := len(shallow.loosePopLog); n != 0 {
		t.Errorf("shallow.loosePopLog len = %d after NESTED window, want 0", n)
	}

	// Inner NESTED commits (Release). Its mutations stay in the
	// pager. The outer SHALLOW must still be resolvable (LIFO holds).
	p.ReleaseSavepoint(nested)
	if got := p.SavepointDepth(); got != 0 {
		t.Errorf("SavepointDepth = %d after NESTED release, want 0", got)
	}

	// Outer SHALLOW commits. Must not panic; mutations persist.
	p.ReleaseSavepoint(shallow)
}
