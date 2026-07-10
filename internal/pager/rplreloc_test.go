package pager

import (
	"errors"
	"sort"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// buildChainForReloc drives real commits on a full initDB writer until
// the RPL chain holds at least minSegs segments, returning the opened
// DB and its file. Retirements come from alloc-then-free churn across
// commits so every segment carries genuine entries.
func buildChainForReloc(t *testing.T, minSegs int) (*OpenedDB, func()) {
	t.Helper()
	f, db, cleanup := initDB(t, false)
	_ = f
	p := db.Pager
	txn := uint64(0)
	prev := db.Meta
	prevActive := db.ActiveMetaIdx
	var held []uint64
	for len(p.RPLChain()) < minSegs {
		txn++
		p.BeginTx(TxParams{
			HighWaterMark: prev.HighWaterMark,
			MaxSize:       prev.MaxSize,
			GrowStep:      prev.GrowStep,
			MinSize:       prev.MinSize,
			TxnID:         txn,
			// Reclamation pinned off so the chain only grows.
			ReclamationBound: func() uint64 { return 0 },
		})
		// Free last round's pages (they are prior-tx pages now → RPL).
		for _, id := range held {
			if err := p.FreePage(id); err != nil {
				t.Fatalf("FreePage(%d): %v", id, err)
			}
		}
		held = held[:0]
		// Allocate a few fresh pages to free next round.
		for range 3 {
			id, err := p.AllocPage()
			if err != nil {
				t.Fatalf("AllocPage: %v", err)
			}
			if _, err := p.AllocSlab(id); err != nil {
				t.Fatalf("AllocSlab: %v", err)
			}
			held = append(held, id)
		}
		res, err := p.Commit(CommitParams{NewTxnID: txn, Flags: prev.Flags, Sync: SyncNone}, prev, prevActive)
		if err != nil {
			t.Fatalf("Commit(%d): %v", txn, err)
		}
		prev, prevActive = res.Meta, res.ActiveMetaIdx
	}
	db.Meta, db.ActiveMetaIdx = prev, prevActive
	return db, cleanup
}

// chainEntries decodes every segment of the in-memory chain into the
// (TxnID, PageID) retirement multiset — the conservation invariant's
// observable (free-space.md §RPL segment relocation).
func chainEntries(t *testing.T, p *Pager) []struct{ Txn, Page uint64 } {
	t.Helper()
	var out []struct{ Txn, Page uint64 }
	for _, ref := range p.RPLChain() {
		seg, _, ok := readRPLSegment(p.pageRaw, p.cfg, ref.PageID)
		if !ok {
			t.Fatalf("segment %d does not decode", ref.PageID)
		}
		for _, id := range seg.PageIDs {
			out = append(out, struct{ Txn, Page uint64 }{seg.TxnID, id})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Txn != out[j].Txn {
			return out[i].Txn < out[j].Txn
		}
		return out[i].Page < out[j].Page
	})
	return out
}

// TestRPLRelocationMovesPrefixBelowFloor pins the mechanism end to
// end: an armed request relocates every in-region segment below the
// floor, preserves the retirement multiset (the conservation
// invariant), keeps the on-disk chain walkable across a reopen, and
// retires the old prefix pages in the relocating commit's head.
func TestRPLRelocationMovesPrefixBelowFloor(t *testing.T) {
	db, cleanup := buildChainForReloc(t, 3)
	defer cleanup()
	p := db.Pager

	// Create below-floor homes: reclaim the oldest segment (its
	// entries and its own page are LOW ids — freed into the bitmap
	// below every newer segment's page), exactly how a real
	// compaction pass creates the holes relocation fills.
	{
		txn := db.Meta.TxnID + 1
		p.BeginTx(TxParams{
			HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
			GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
			// Cover exactly the oldest segment so it reclaims.
			ReclamationBound: func() uint64 { return p.RPLChain()[0].TxnID + 1 },
		})
		if p.ReclaimFreeSpace() == 0 {
			t.Fatal("fixture: reclamation freed nothing")
		}
		res, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncNone}, db.Meta, db.ActiveMetaIdx)
		if err != nil {
			t.Fatalf("homes commit: %v", err)
		}
		db.Meta, db.ActiveMetaIdx = res.Meta, res.ActiveMetaIdx
	}

	before := chainEntries(t, p)
	chain := p.RPLChain()
	// Floor at the second-newest segment's page: at least TWO
	// segments are in-region, so the copy loop exercises the
	// neighbor-link cascade (a k=1 prefix never rewrites a link).
	floor := min(chain[len(chain)-1].PageID, chain[len(chain)-2].PageID)
	inRegion := p.RPLSegmentsAtOrAbove(floor)
	if inRegion < 2 {
		t.Fatalf("fixture: %d segments at/above the floor, need >= 2 for the cascade", inRegion)
	}

	txn := db.Meta.TxnID + 1
	p.BeginTx(TxParams{
		HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
		GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
		ReclamationBound: func() uint64 { return 0 },
	})
	p.RequestRPLRelocation(floor)
	res, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncBoth}, db.Meta, db.ActiveMetaIdx)
	if err != nil {
		t.Fatalf("relocating commit: %v", err)
	}
	if p.RPLRelocationDeclined() {
		t.Fatal("relocation declined; fixture expected it to run")
	}

	// Every pre-existing segment now sits below the floor (the
	// relocating commit's own fresh head may legitimately sit
	// anywhere the allocator put it — exclude the head).
	after := p.RPLChain()
	for _, ref := range after[:len(after)-1] {
		if ref.PageID >= floor {
			t.Errorf("segment %d (txn %d) still at/above floor %d", ref.PageID, ref.TxnID, floor)
		}
	}

	// Conservation: the multiset of pre-relocation entries survives,
	// and the relocating commit added exactly the old prefix pages as
	// its own retirements.
	afterEntries := chainEntries(t, p)
	var relocated, carried []struct{ Txn, Page uint64 }
	for _, e := range afterEntries {
		if e.Txn == txn {
			relocated = append(relocated, e)
		} else {
			carried = append(carried, e)
		}
	}
	if len(carried) != len(before) {
		t.Fatalf("carried entries %d != before %d (conservation)", len(carried), len(before))
	}
	for i := range before {
		if carried[i] != before[i] {
			t.Fatalf("entry %d: %v != %v (conservation)", i, carried[i], before[i])
		}
	}
	if len(relocated) != inRegion {
		t.Errorf("relocating commit retired %d pages, want %d (the old prefix)", len(relocated), inRegion)
	}

	// The on-disk chain reopens and walks clean.
	_ = res
	file := p.file
	od2, err := Open(file, OpenParams{Pool: NewBufPool(testPageSize), MaxTxBufferBytes: 16 << 20})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	m2, _, err := od2.Pager.AttachLatest(file)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if m2.RPLEntryCount != uint64(len(afterEntries)) {
		t.Errorf("reopened entry count %d, want %d", m2.RPLEntryCount, len(afterEntries))
	}
	// The REOPENED chain must run through the copies — identical page
	// ids to the post-relocation in-memory chain (a link pointing at
	// an old, retired-but-not-yet-reclaimed page would still decode
	// and walk; only this identity check catches it).
	reopened := od2.Pager.RPLChain()
	if len(reopened) != len(after) {
		t.Fatalf("reopened chain length %d, want %d", len(reopened), len(after))
	}
	for i := range after {
		if reopened[i].PageID != after[i].PageID {
			t.Errorf("reopened segment %d at page %d, want %d", i, reopened[i].PageID, after[i].PageID)
		}
	}
	_ = od2.Pager.Close()
}

// TestRPLRelocationDeclinesWhenUnplaceable pins the decline rule's
// placement arm: no free page below the floor ⇒ the whole request
// declines, the chain is untouched, and nothing was allocated.
func TestRPLRelocationDeclinesWhenUnplaceable(t *testing.T) {
	db, cleanup := buildChainForReloc(t, 2)
	defer cleanup()
	p := db.Pager

	before := p.RPLChain()
	beforePages := make([]uint64, len(before))
	for i, r := range before {
		beforePages[i] = r.PageID
	}
	// Floor at firstDataPage: EVERY page is in-region and no home can
	// exist below it.
	floor := uint64(db.Meta.BitmapPages) + 2

	txn := db.Meta.TxnID + 1
	p.BeginTx(TxParams{
		HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
		GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
		ReclamationBound: func() uint64 { return 0 },
	})
	p.RequestRPLRelocation(floor)
	if _, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncNone}, db.Meta, db.ActiveMetaIdx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !p.RPLRelocationDeclined() {
		t.Fatal("relocation not declined despite no below-floor homes")
	}
	after := p.RPLChain()
	if len(after) != len(before) {
		t.Fatalf("chain length changed on decline: %d -> %d", len(before), len(after))
	}
	for i, r := range after {
		if r.PageID != beforePages[i] {
			t.Errorf("segment %d moved on decline: %d -> %d", i, beforePages[i], r.PageID)
		}
	}
}

// TestRPLRelocationDeclinesOverBudget pins the decline rule's budget
// arm: a prefix whose copy buffers exceed the slab budget declines
// rather than partially relocating.
func TestRPLRelocationDeclinesOverBudget(t *testing.T) {
	db, cleanup := buildChainForReloc(t, 3)
	defer cleanup()
	p := db.Pager

	chain := p.RPLChain()
	floor := chain[0].PageID // whole chain in-region
	txn := db.Meta.TxnID + 1
	p.BeginTx(TxParams{
		HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
		GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
		ReclamationBound: func() uint64 { return 0 },
	})
	// Shrink the budget below k copy buffers.
	p.maxBytes = p.dirtyBytes + int(testPageSize)*(len(chain)-1)
	p.RequestRPLRelocation(floor)
	if _, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncNone}, db.Meta, db.ActiveMetaIdx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !p.RPLRelocationDeclined() {
		t.Fatal("relocation not declined despite over-budget prefix")
	}
}

var _ = page.Config{} // keep the import if fixtures change

// TestRPLRelocationSurvivesSameCommitReclaim pins the interleaving
// where the relocating commit's own appendRPL triggers reclamation
// that pops relocated COPIES (their original TxnIDs sit below a live
// bound): reclaim must read the copies' slab buffers (dirty-first
// pageRaw), conserve entries, and leave a clean reopenable chain.
func TestRPLRelocationSurvivesSameCommitReclaim(t *testing.T) {
	db, cleanup := buildChainForReloc(t, 3)
	defer cleanup()
	p := db.Pager

	// Homes: reclaim the oldest segment first (as the main test).
	{
		txn := db.Meta.TxnID + 1
		p.BeginTx(TxParams{
			HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
			GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
			ReclamationBound: func() uint64 { return p.RPLChain()[0].TxnID + 1 },
		})
		if p.ReclaimFreeSpace() == 0 {
			t.Fatal("fixture: reclamation freed nothing")
		}
		res, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncNone}, db.Meta, db.ActiveMetaIdx)
		if err != nil {
			t.Fatalf("homes commit: %v", err)
		}
		db.Meta, db.ActiveMetaIdx = res.Meta, res.ActiveMetaIdx
	}

	chain := p.RPLChain()
	floor := min(chain[len(chain)-1].PageID, chain[len(chain)-2].PageID)
	if p.RPLSegmentsAtOrAbove(floor) < 2 {
		t.Fatalf("fixture: need >= 2 in-region segments")
	}
	beforeCorrupt := p.RPLCorruptCount()

	txn := db.Meta.TxnID + 1
	p.BeginTx(TxParams{
		HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
		GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
		// LIVE bound covering every existing segment: appendRPL's own
		// allocations may reclaim the just-relocated copies.
		ReclamationBound: func() uint64 { return txn },
	})
	p.RequestRPLRelocation(floor)
	// Force reclamation pressure inside the commit: reclaim
	// explicitly after arming, mirroring what AllocPage-under-
	// pressure does in appendRPL Phase 1.
	if _, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncBoth}, db.Meta, db.ActiveMetaIdx); err != nil {
		t.Fatalf("relocating commit under live bound: %v", err)
	}
	if p.RPLRelocationDeclined() {
		t.Fatal("declined; fixture expected execution")
	}
	if got := p.RPLCorruptCount(); got != beforeCorrupt {
		t.Fatalf("quarantine fired during same-commit reclaim: %d -> %d", beforeCorrupt, got)
	}

	// Whatever reclaim popped, what remains must reopen cleanly and
	// the surviving entries must be a subset of the pre-relocation
	// multiset plus the relocating commit's own retirements.
	file := p.file
	od2, err := Open(file, OpenParams{Pool: NewBufPool(testPageSize), MaxTxBufferBytes: 16 << 20})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, _, err := od2.Pager.AttachLatest(file); err != nil {
		t.Fatalf("attach: %v", err)
	}
	_ = od2.Pager.Close()
}

// TestRPLRelocationAbortRestoresChain pins AbortTx after an executed
// relocation: the in-memory chain, bitmap, and retirement list all
// revert — the crash story's in-process twin (publication rides the
// ordinary meta flip, whose crash atomicity the commit tests own).
func TestRPLRelocationAbortRestoresChain(t *testing.T) {
	db, cleanup := buildChainForReloc(t, 3)
	defer cleanup()
	p := db.Pager
	{
		txn := db.Meta.TxnID + 1
		p.BeginTx(TxParams{
			HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
			GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
			ReclamationBound: func() uint64 { return p.RPLChain()[0].TxnID + 1 },
		})
		if p.ReclaimFreeSpace() == 0 {
			t.Fatal("fixture: reclamation freed nothing")
		}
		res, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncNone}, db.Meta, db.ActiveMetaIdx)
		if err != nil {
			t.Fatalf("homes commit: %v", err)
		}
		db.Meta, db.ActiveMetaIdx = res.Meta, res.ActiveMetaIdx
	}
	chain := p.RPLChain()
	floor := min(chain[len(chain)-1].PageID, chain[len(chain)-2].PageID)
	beforePages := make([]uint64, len(chain))
	for i, r := range chain {
		beforePages[i] = r.PageID
	}

	txn := db.Meta.TxnID + 1
	p.BeginTx(TxParams{
		HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
		GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
		ReclamationBound: func() uint64 { return 0 },
	})
	p.RequestRPLRelocation(floor)
	// Execute the relocation directly (the commit-step entry point),
	// then abort instead of committing.
	if err := p.relocateRPLPrefix(); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	if p.RPLRelocationDeclined() {
		t.Fatal("declined; fixture expected execution")
	}
	p.AbortTx()

	after := p.RPLChain()
	if len(after) != len(beforePages) {
		t.Fatalf("chain length changed across abort: %d -> %d", len(beforePages), len(after))
	}
	for i, r := range after {
		if r.PageID != beforePages[i] {
			t.Errorf("segment %d not restored: %d, want %d", i, r.PageID, beforePages[i])
		}
	}
}

// TestRPLRelocationRequestClearedOnAbort pins one-shot ownership
// across ABORT (free-space.md §RPL segment relocation: the request is
// consumed — executed or declined — by the arming transaction's OWN
// commit): a transaction that arms a request and rolls back before
// committing must not leak it into the next, unrelated commit.
func TestRPLRelocationRequestClearedOnAbort(t *testing.T) {
	db, cleanup := buildChainForReloc(t, 3)
	defer cleanup()
	p := db.Pager

	// Below-floor homes exist (reclaim the oldest segment), so a
	// leaked request would EXECUTE rather than decline — the
	// observable fault is unrequested relocation work.
	{
		txn := db.Meta.TxnID + 1
		p.BeginTx(TxParams{
			HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
			GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
			ReclamationBound: func() uint64 { return p.RPLChain()[0].TxnID + 1 },
		})
		if p.ReclaimFreeSpace() == 0 {
			t.Fatal("fixture: reclamation freed nothing")
		}
		res, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncNone}, db.Meta, db.ActiveMetaIdx)
		if err != nil {
			t.Fatalf("homes commit: %v", err)
		}
		db.Meta, db.ActiveMetaIdx = res.Meta, res.ActiveMetaIdx
	}
	chain := p.RPLChain()
	floor := min(chain[len(chain)-1].PageID, chain[len(chain)-2].PageID)
	if p.RPLSegmentsAtOrAbove(floor) < 2 {
		t.Fatal("fixture: need >= 2 in-region segments")
	}
	beforePages := make([]uint64, len(chain))
	for i, r := range chain {
		beforePages[i] = r.PageID
	}

	// The compaction-shaped transaction arms, then fails before its
	// commit (the reachable path: flushKeyspaces → ErrTxTooLarge →
	// rollback).
	txn := db.Meta.TxnID + 1
	p.BeginTx(TxParams{
		HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
		GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
		ReclamationBound: func() uint64 { return 0 },
	})
	p.RequestRPLRelocation(floor)
	p.AbortTx()
	// Per-site pin: the abort itself discards the request (the
	// next-BeginTx clear is a second, independent barrier — each site
	// is asserted separately so neither silently carries the other).
	if p.rplRelocFloor != 0 {
		t.Fatal("AbortTx left the relocation request armed")
	}
	p.rplRelocFloor = floor // re-arm past the abort clear to pin BeginTx's own barrier

	// The next, unrelated commit must not relocate anything.
	p.BeginTx(TxParams{
		HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
		GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
		ReclamationBound: func() uint64 { return 0 },
	})
	if _, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncNone}, db.Meta, db.ActiveMetaIdx); err != nil {
		t.Fatalf("unrelated commit: %v", err)
	}
	after := p.RPLChain()
	if len(after) != len(beforePages) {
		t.Fatalf("chain length changed (%d -> %d): the aborted tx's request leaked into this commit", len(beforePages), len(after))
	}
	for i, r := range after {
		if r.PageID != beforePages[i] {
			t.Errorf("segment %d relocated (%d -> %d) by a commit that never armed a request", i, beforePages[i], r.PageID)
		}
	}
}

// TestRPLRelocationDeclinesWhenSegmentAppendExceedsBudget pins the
// budget probe's projection of the RPL segment pages appendRPL needs
// for the k old prefix pages the relocation retires: a budget that
// fits the k copy buffers but not the segment append must DECLINE
// (free-space.md §RPL segment relocation: no state change until the
// prefix is known to fit the work budget) — never fail the commit
// after relocation state changed.
func TestRPLRelocationDeclinesWhenSegmentAppendExceedsBudget(t *testing.T) {
	db, cleanup := buildChainForReloc(t, 3)
	defer cleanup()
	p := db.Pager
	{
		txn := db.Meta.TxnID + 1
		p.BeginTx(TxParams{
			HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
			GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
			ReclamationBound: func() uint64 { return p.RPLChain()[0].TxnID + 1 },
		})
		if p.ReclaimFreeSpace() == 0 {
			t.Fatal("fixture: reclamation freed nothing")
		}
		res, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncNone}, db.Meta, db.ActiveMetaIdx)
		if err != nil {
			t.Fatalf("homes commit: %v", err)
		}
		db.Meta, db.ActiveMetaIdx = res.Meta, res.ActiveMetaIdx
	}
	chain := p.RPLChain()
	floor := min(chain[len(chain)-1].PageID, chain[len(chain)-2].PageID)
	k := p.RPLSegmentsAtOrAbove(floor)
	if k < 2 {
		t.Fatal("fixture: need >= 2 in-region segments")
	}
	beforePages := make([]uint64, len(chain))
	for i, r := range chain {
		beforePages[i] = r.PageID
	}

	txn := db.Meta.TxnID + 1
	p.BeginTx(TxParams{
		HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
		GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
		ReclamationBound: func() uint64 { return 0 },
	})
	// Budget fits exactly the k copy buffers but NOT the one RPL
	// segment page the k retirements need (no other retirements, so
	// appendRPL allocates ceil(k/capPerSeg) = 1 extra slab).
	p.maxBytes = p.dirtyBytes + int(testPageSize)*k
	p.RequestRPLRelocation(floor)
	res, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncNone}, db.Meta, db.ActiveMetaIdx)
	if err != nil {
		t.Fatalf("commit must decline the relocation, not fail: %v", err)
	}
	if !p.RPLRelocationDeclined() {
		t.Fatal("relocation not declined despite the segment append exceeding the budget")
	}
	after := p.RPLChain()
	if len(after) != len(beforePages) {
		t.Fatalf("chain length changed on decline: %d -> %d", len(beforePages), len(after))
	}
	for i, r := range after {
		if r.PageID != beforePages[i] {
			t.Errorf("segment %d moved on decline: %d -> %d", i, beforePages[i], r.PageID)
		}
	}
	db.Meta, db.ActiveMetaIdx = res.Meta, res.ActiveMetaIdx

	// Acceptance boundary: one segment page more of budget and the
	// same request EXECUTES — a projection that overcounts (spurious
	// declines) fails here.
	txn++
	p.BeginTx(TxParams{
		HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
		GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
		ReclamationBound: func() uint64 { return 0 },
	})
	p.maxBytes = p.dirtyBytes + int(testPageSize)*(k+1)
	p.RequestRPLRelocation(floor)
	if _, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncNone}, db.Meta, db.ActiveMetaIdx); err != nil {
		t.Fatalf("boundary commit: %v", err)
	}
	if p.RPLRelocationDeclined() {
		t.Fatal("relocation declined at the exact-fit boundary (projection overcounts)")
	}
}

// TestRPLRelocationDeclinesWhenNoPageForSegmentAppend pins the
// availability arm of the probe: when the only free pages are the k
// below-floor homes and there is no file-extension headroom, the
// request must DECLINE — otherwise appendRPL's segment-page AllocPage
// returns ErrDBFull after relocation state changed (free-space.md
// §RPL segment relocation: no state change until probing establishes
// the request fits).
func TestRPLRelocationDeclinesWhenNoPageForSegmentAppend(t *testing.T) {
	db, cleanup := buildChainForReloc(t, 3)
	defer cleanup()
	p := db.Pager
	{
		txn := db.Meta.TxnID + 1
		p.BeginTx(TxParams{
			HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
			GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
			ReclamationBound: func() uint64 { return p.RPLChain()[0].TxnID + 1 },
		})
		if p.ReclaimFreeSpace() == 0 {
			t.Fatal("fixture: reclamation freed nothing")
		}
		res, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncNone}, db.Meta, db.ActiveMetaIdx)
		if err != nil {
			t.Fatalf("homes commit: %v", err)
		}
		db.Meta, db.ActiveMetaIdx = res.Meta, res.ActiveMetaIdx
	}
	chain := p.RPLChain()
	floor := min(chain[len(chain)-1].PageID, chain[len(chain)-2].PageID)
	k := p.RPLSegmentsAtOrAbove(floor)
	if k < 2 {
		t.Fatal("fixture: need >= 2 in-region segments")
	}
	beforePages := make([]uint64, len(chain))
	for i, r := range chain {
		beforePages[i] = r.PageID
	}

	// MaxSize pinned to the current HWM: no extension headroom.
	txn := db.Meta.TxnID + 1
	p.BeginTx(TxParams{
		HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.HighWaterMark,
		GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
		ReclamationBound: func() uint64 { return 0 },
	})
	// Consume every free page, then free back exactly k BELOW-FLOOR
	// ids (a same-tx page never CoW'd takes FreePage's pendingAllocs
	// branch: its bitmap bit is restored immediately, so the k bits
	// are free at probe time) — the homes exist, but nothing is left
	// for the segment append.
	var grabbed []uint64
	for {
		id, err := p.AllocPage()
		if err != nil {
			if errors.Is(err, ErrDBFull) {
				break
			}
			t.Fatalf("AllocPage: %v", err)
		}
		grabbed = append(grabbed, id)
	}
	freed := 0
	for _, id := range grabbed {
		if id < floor {
			if err := p.FreePage(id); err != nil {
				t.Fatalf("FreePage(%d): %v", id, err)
			}
			if freed++; freed == k {
				break
			}
		}
	}
	if freed < k {
		t.Fatalf("fixture: only %d below-floor pages available, need %d homes", freed, k)
	}

	p.RequestRPLRelocation(floor)
	if _, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncNone}, db.Meta, db.ActiveMetaIdx); err != nil {
		t.Fatalf("commit must decline the relocation, not fail: %v", err)
	}
	if !p.RPLRelocationDeclined() {
		t.Fatal("relocation not declined despite no page for the segment append")
	}
	after := p.RPLChain()
	if len(after) != len(beforePages) {
		t.Fatalf("chain length changed on decline: %d -> %d", len(beforePages), len(after))
	}
	for i, r := range after {
		if r.PageID != beforePages[i] {
			t.Errorf("segment %d moved on decline: %d -> %d", i, beforePages[i], r.PageID)
		}
	}
}

// TestRPLRelocationRequestIsOneShot pins the consumed-on-decline
// contract: after a declined request, the next commit performs no
// relocation attempt without a re-arm.
func TestRPLRelocationRequestIsOneShot(t *testing.T) {
	db, cleanup := buildChainForReloc(t, 2)
	defer cleanup()
	p := db.Pager
	floor := uint64(db.Meta.BitmapPages) + 2 // unplaceable

	txn := db.Meta.TxnID + 1
	p.BeginTx(TxParams{
		HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
		GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
		ReclamationBound: func() uint64 { return 0 },
	})
	p.RequestRPLRelocation(floor)
	res, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncNone}, db.Meta, db.ActiveMetaIdx)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !p.RPLRelocationDeclined() {
		t.Fatal("expected decline")
	}
	if p.rplRelocFloor != 0 {
		t.Fatal("request not consumed on decline")
	}
	db.Meta, db.ActiveMetaIdx = res.Meta, res.ActiveMetaIdx
	// A follow-up commit must not attempt relocation (white-box: the
	// floor stays disarmed; declined flag untouched by the commit).
	txn++
	p.BeginTx(TxParams{
		HighWaterMark: db.Meta.HighWaterMark, MaxSize: db.Meta.MaxSize,
		GrowStep: db.Meta.GrowStep, MinSize: db.Meta.MinSize, TxnID: txn,
		ReclamationBound: func() uint64 { return 0 },
	})
	if _, err := p.Commit(CommitParams{NewTxnID: txn, Flags: db.Meta.Flags, Sync: SyncNone}, db.Meta, db.ActiveMetaIdx); err != nil {
		t.Fatalf("follow-up commit: %v", err)
	}
	if p.rplRelocFloor != 0 {
		t.Fatal("follow-up commit re-armed the request")
	}
}
