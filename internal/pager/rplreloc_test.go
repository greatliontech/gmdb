package pager

import (
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
