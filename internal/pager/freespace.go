package pager

import (
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// AllocPage allocates a single page following the priority order in
// free-space.md §Page Allocation Priority:
//
//  1. Loose pages — same-tx pages that were CoW'd then freed, immediately
//     reusable without touching the bitmap or RPL.
//  2. Allocation bitmap — scan from the LIFO hint for any free page.
//  3. RPL reclamation — if the bitmap has no free pages, walk the RPL
//     tail→head and free entries with TxnID < reclamationBound, then
//     retry the bitmap.
//  4. Lagging-reader callback — chunk 2 territory; not implemented in
//     chunk 1.
//  5. File extension — if no free pages anywhere, advance HighWaterMark
//     up to maxSizePages, marking newly-mapped pages free in the bitmap
//     (the one being allocated stays clear).
//
// The returned page id has its bitmap bit cleared (or, for loose pages,
// is removed from the loose set) and is recorded in pendingAllocs.
//
// Loose-page reuse contract: the slab buffer at p.dirty[id] is left as-is
// — its previous content is not erased — to honour the byte-slice
// ownership invariant from pager-slab.md (a `[]byte` returned to the user
// earlier from this id must remain valid). The caller is responsible for
// overwriting the buffer via CoW / AllocSlab / Mutate before exposing any
// new `[]byte` from the reused id. Re-use does not double the slab
// budget: idempotent CoW/AllocSlab returns the existing buffer.
func (p *Pager) AllocPage() (uint64, error) {
	if p.readOnly {
		return 0, ErrReadOnly
	}
	if p.bitmap == nil {
		return 0, ErrFreespaceUnconfigured
	}

	// 1. Loose pages.
	for id := range p.loosePages {
		delete(p.loosePages, id)
		// Bookkeeping: the id was initially allocated (bitmap.Clear +
		// pendingAllocs add), then FreePage moved it to loosePages.
		// Re-allocating it now:
		//
		//   - Detach the existing slab buffer at p.dirty[id] into
		//     p.detachedBufs. Required by the chunk-5.4 fix to the
		//     loose-page reuse contract: the buffer in p.dirty[id]
		//     holds STALE content from when id was previously CoW'd
		//     before being freed. If we left it in p.dirty, the next
		//     pw.CoW(srcID, id) would hit CoW's idempotent re-CoW
		//     shortcut and return the stale buffer instead of
		//     refreshing with srcID's content (chunk-4 btree workloads
		//     that CoW a leaf, free it, then alloc+CoW it again to a
		//     different src silently lose data). Detach lets the
		//     subsequent CoW / AllocSlab take the fresh-allocation
		//     path. The detached buffer stays alive in p.detachedBufs
		//     for the byte-slice ownership invariant (any borrowed
		//     []byte the original caller holds remains valid through
		//     tx close); pool-Put'd at ReleaseAll / AbortTx alongside
		//     p.dirty's buffers.
		//
		//   - Add to pendingAllocs so a subsequent FreePage on this
		//     id (without intermediate CoW/AllocSlab to install a new
		//     slab buffer — e.g., the rollback path after a budget
		//     error) takes the chunk-5.2 pendingAllocs branch
		//     (bitmap.Set, drop pendingAllocs) rather than the
		//     prior-tx retiredPages branch. delete(pendingFrees) is
		//     defensive — loose-popped ids should not be in
		//     pendingFrees, but the mirror to the bitmap step keeps
		//     the surface uniform.
		//
		// dirtyBytes is unchanged: the detached buffer is still
		// "held by the transaction" per pager-slab.md, so it
		// continues to count toward MaxTxBufferBytes. A subsequent
		// CoW that installs a fresh buffer adds another PageSize to
		// dirtyBytes — correct, both buffers are live.
		if buf, ok := p.dirty[id]; ok {
			p.detachedBufs = append(p.detachedBufs, buf)
			delete(p.dirty, id)
		}
		p.pendingAllocs[id] = struct{}{}
		// Defensive symmetry with the bitmap-path branch below. The
		// commit pipeline is the only writer of pendingFrees and runs
		// strictly after all tx-body AllocPage calls, so this delete
		// is unreachable today — kept for uniformity with the chunk-
		// 5.2 FreePage contract (allocation removes a pending-free
		// scheduling) and to make a future commit-step rearrangement
		// safe by construction.
		delete(p.pendingFrees, id)
		return id, nil
	}

	// 2. Allocation bitmap.
	if id, ok := p.bitmap.FindFirst(); ok {
		p.bitmap.Clear(id)
		p.bitmap.SetHint(id)
		p.pendingAllocs[id] = struct{}{}
		// If id was in pendingFrees from earlier in this tx (a loose
		// page that was already scheduled to be marked free), undo:
		// the bitmap bit goes from set→clear (commit-time) but the
		// page is now in active use again.
		delete(p.pendingFrees, id)
		return id, nil
	}

	// 3. RPL reclamation. Walk tail→head, set bitmap bits for entries
	// in segments whose TxnID < reclamationBound, then retry the
	// bitmap.
	if p.reclaimRPL() > 0 {
		if id, ok := p.bitmap.FindFirst(); ok {
			p.bitmap.Clear(id)
			p.bitmap.SetHint(id)
			p.pendingAllocs[id] = struct{}{}
			delete(p.pendingFrees, id)
			return id, nil
		}
	}

	// 4. Lagging-reader callback per chunk-5.5 wiring.
	// Reclamation just returned 0 entries — either the RPL was
	// empty (no callback to make: nothing to reclaim regardless of
	// readers) or the bound is blocking advance. Distinguish via
	// the RPL chain: non-empty + still-blocked ⇒ reader is the
	// factor; invoke the callback.
	if p.laggingReader != nil && len(p.rplSegments) > 0 {
		info := p.buildLaggingReaderInfo()
		switch p.laggingReader(info) {
		case LaggingReaderAbort:
			return 0, ErrDBFull
		case LaggingReaderWait:
			if p.refreshReclamationBound != nil {
				p.reclamationBound = p.refreshReclamationBound()
				if p.reclaimRPL() > 0 {
					if id, ok := p.bitmap.FindFirst(); ok {
						p.bitmap.Clear(id)
						p.bitmap.SetHint(id)
						p.pendingAllocs[id] = struct{}{}
						delete(p.pendingFrees, id)
						return id, nil
					}
				}
			}
			// Fall through to file extension if Wait + refresh + retry
			// did not free a page (matches the spec's "at most once
			// per AllocPage" invariant — no second callback).
		}
	}

	// 5. File extension. Advance HighWaterMark by one page. Cap at
	// maxSizePages; ErrDBFull when reached. ftruncate is issued if
	// the new HWM exceeds the current file size — this is allowed
	// outside step-0 assembly per pager-slab.md (step 0's no-syscall
	// rule applies to the commit assembly phase, not the tx body).
	if p.highWaterMark >= p.maxSizePages {
		return 0, ErrDBFull
	}
	id := p.highWaterMark
	p.highWaterMark++
	p.pendingAllocs[id] = struct{}{}
	if err := p.ensureFileCovers(p.highWaterMark); err != nil {
		// Roll back the HWM bump on truncate failure.
		p.highWaterMark--
		delete(p.pendingAllocs, id)
		return 0, err
	}
	return id, nil
}

// ensureFileCovers ftruncates the file up to at least pages * PageSize
// bytes if the current file size is smaller. No-op when the file is
// already large enough. The mmap reservation is separate and is sized
// to MaxSize at Open; ftruncate within the reservation does not require
// remap on Linux/macOS/FreeBSD (the existing VMA covers the new region).
func (p *Pager) ensureFileCovers(pages uint64) error {
	need := int64(pages) * int64(p.cfg.PageSize)
	if need <= p.fileSize {
		return nil
	}
	if err := p.file.Truncate(need); err != nil {
		return fmt.Errorf("pager: ftruncate to %d: %w", need, err)
	}
	p.fileSize = need
	return nil
}

// FreePage marks id for retirement. The disposition depends on whether
// id was allocated within this transaction:
//
//   - In p.dirty (CoW'd or AllocSlab'd this tx): becomes a loose
//     page. Reusable via AllocPage; if not reused, its bitmap bit is set
//     at commit (bypassing the RPL because no other process holds a
//     snapshot referencing a same-tx page).
//   - In pendingAllocs but not yet in p.dirty (allocated this tx but
//     never CoW'd / AllocSlab'd — the chunk-4.7 overflow rollback path
//     reaches here when AllocContiguous succeeds but AllocSlabRun fails
//     mid-run): bitmap bit is restored to free, pendingAllocs entry is
//     dropped. No retiredPages entry: no prior-tx reader holds a
//     snapshot referencing this page.
//   - Not in p.dirty and not in pendingAllocs (mmap-backed, from a
//     prior tx): joins retiredPages. Appended to the RPL at commit so
//     the page survives in MVCC until the reclamation bound advances
//     past this tx's TxnID.
//
// Errors: ErrReadOnly on a read-only pager.
func (p *Pager) FreePage(id uint64) error {
	if p.readOnly {
		return ErrReadOnly
	}
	if p.bitmap == nil {
		return ErrFreespaceUnconfigured
	}
	if _, sameTx := p.dirty[id]; sameTx {
		// Same-tx page becomes loose. Its slab buffer stays in p.dirty
		// (byte-slice ownership invariant). pendingAllocs entry is
		// cleared because the page no longer needs its bitmap bit
		// cleared at commit — it was never published as in-use.
		p.loosePages[id] = struct{}{}
		delete(p.pendingAllocs, id)
		return nil
	}
	if _, justAllocated := p.pendingAllocs[id]; justAllocated {
		// Allocated this tx via AllocPage / AllocContiguous but never
		// CoW'd / AllocSlab'd. Restore the bitmap bit (no slab buffer
		// to retain) and drop the pendingAllocs entry — no prior-tx
		// snapshot references this page, so it does not enter the
		// RPL. Reachable today via AllocContiguous + AllocSlabRun
		// rollback when AllocSlabRun fails on the budget check after
		// AllocContiguous already cleared the bitmap bits.
		p.bitmap.Set(id)
		delete(p.pendingAllocs, id)
		return nil
	}
	// Prior-tx page: retire via RPL.
	p.retiredPages = append(p.retiredPages, id)
	return nil
}

// reclaimRPL walks the in-memory RPL segment list from tail (oldest) and
// reclaims whole segments whose TxnID < reclamationBound. For each
// reclaimed segment:
//
//  1. Decode the on-disk segment page (mmap-backed — segments from
//     previous tx's are immutable and footer-verified at chunk-1.8
//     read time).
//  2. Set bitmap bits for every PageID entry in the segment.
//  3. Set the bitmap bit for the segment page itself.
//  4. Pop the segment from the in-memory list and update the LIFO hint
//     to point near the last reclaimed page.
//
// Returns the number of entries (including segment pages) that became
// newly free. Whole-segment consumption is mandatory per the
// free-space.md invariant.
func (p *Pager) reclaimRPL() int {
	count := 0
	var lastReclaimed uint64
	for len(p.rplSegments) > 0 && p.rplSegments[0].TxnID < p.reclamationBound {
		seg := p.rplSegments[0]
		buf := p.Page(seg.PageID)
		decoded, ok := page.DecodeRPLSegment(buf, p.cfg)
		if !ok {
			// Corrupt RPL segment — surface via the integrity-check
			// path in chunk 11. Halt reclamation here to avoid feeding
			// garbage page IDs to the bitmap.
			break
		}
		for _, id := range decoded.PageIDs {
			p.bitmap.Set(id)
			count++
			lastReclaimed = id
		}
		// The segment page itself is now reclaimable.
		p.bitmap.Set(seg.PageID)
		count++
		lastReclaimed = seg.PageID
		// Pop from the in-memory list (copy-trim to free the head
		// slot for GC rather than a head-retaining reslice).
		p.trimRPLChainHead(1)
	}
	if count > 0 {
		// Bias the next allocation toward the most-recently-reclaimed
		// region per LIFO locality.
		p.bitmap.SetHint(lastReclaimed)
	}
	return count
}

// trimRPLChainHead removes the consumed head entries from the in-memory
// chain so the backing array can be GC'd as the chain shrinks. Cheaper
// than `p.rplSegments = p.rplSegments[1:]` which retains the backing
// array.
func (p *Pager) trimRPLChainHead(n int) {
	if n <= 0 || n > len(p.rplSegments) {
		return
	}
	copy(p.rplSegments, p.rplSegments[n:])
	p.rplSegments = p.rplSegments[:len(p.rplSegments)-n]
}

// TailRefund clears bitmap bits and decrements highWaterMark for tail
// pages currently free in the bitmap or in the loose set. Iterates until
// the page below highWaterMark is neither free in the bitmap nor loose.
//
// Per free-space.md §Tail Page Refund: pages held by an active reader
// cannot be tail-refunded because their bitmap bits are not set (they're
// still in the RPL pending reclamation). Tail refund only touches pages
// already known free.
//
// Errors: ErrReadOnly on a read-only pager; ErrFreespaceUnconfigured if
// the bitmap has not been attached.
func (p *Pager) TailRefund() error {
	if p.readOnly {
		return ErrReadOnly
	}
	if p.bitmap == nil {
		return ErrFreespaceUnconfigured
	}
	first := p.bitmap.FirstDataPage()
	for p.highWaterMark > first {
		tail := p.highWaterMark - 1
		if p.bitmap.IsSet(tail) {
			p.bitmap.Clear(tail)
			// Bit was set (free); we cleared it. The page is also
			// outside HWM after this, so the bitmap bit being clear
			// matches "page outside the file" — no pendingFrees
			// update needed (the bit transitions free→clear before
			// HWM transitions to exclude the page).
			delete(p.pendingFrees, tail)
			p.highWaterMark--
			continue
		}
		if _, loose := p.loosePages[tail]; loose {
			delete(p.loosePages, tail)
			// The loose page's slab buffer stays alive in p.dirty
			// (byte-slice ownership). It will be released at
			// Commit / Rollback via ReleaseAll.
			p.highWaterMark--
			continue
		}
		break
	}
	return nil
}

// ResetFreespace clears the tx-scoped freespace bookkeeping (pendingAllocs,
// pendingFrees, retiredPages, loosePages). Called by rollback and after
// commit-step-0 finalisation; does not touch the bitmap or RPL chain (
// those updates are applied at commit step 1 via pwrite + meta swap, and
// rollback simply leaves the on-disk bitmap untouched).
func (p *Pager) ResetFreespace() {
	if p.readOnly {
		return
	}
	clear(p.pendingAllocs)
	clear(p.pendingFrees)
	clear(p.loosePages)
	p.retiredPages = p.retiredPages[:0]
}

// buildLaggingReaderInfo constructs the info struct for the
// LaggingReader callback. The pager has access to TxnID (the
// reclamationBound is the youngest TxnID that the RPL is waiting on
// — a proxy for "the blocking reader's TxnID") and currentTxnID;
// PID and HeldPages are zero today (no coord access at this layer,
// no cheap RPL-by-TxnID count). The DB-layer wrapper around
// Options.LaggingReader can enrich the info before forwarding to
// the user callback.
func (p *Pager) buildLaggingReaderInfo() LaggingReaderInfo {
	lag := uint64(0)
	if p.currentTxnID > p.reclamationBound {
		lag = p.currentTxnID - p.reclamationBound
	}
	return LaggingReaderInfo{
		PID:       0,
		TxnID:     p.reclamationBound,
		Lag:       lag,
		HeldPages: 0,
	}
}

// AllocContiguous returns the first page ID of a contiguous run of n
// pages allocated atomically per free-space.md §Page Allocation Priority
// (the n>1 path: bitmap.FindContiguous → RPL reclamation + retry → file
// extension). Implements the chunk-4.7 PageWriter contract used by the
// overflow-chain Put path (internal/btree.overflow).
//
// Atomicity: on success, all n pages [firstID, firstID+n) are reserved
// (bitmap bits cleared on the bitmap-hit branch; pendingAllocs entries
// added on every branch) in a single transition. On error, no run is
// reserved — pendingAllocs and the bitmap allocation set are unchanged.
// Concurrent reclamation and LIFO-hint side effects that happened
// during the call (reclaimRPL's bitmap.Set, SetHint) are retained,
// matching AllocPage's contract: they are speculative free-pool
// accounting, not allocation reservations.
//
// Loose pages are not consulted: chunk-4.7 overflow runs require
// contiguous addressing (followers have no header and must be
// addressable as firstID+i), and the loose-page set holds individually
// freed pages with no contiguity guarantee. n==1 delegates to AllocPage
// so the loose-page priority + LIFO hint behaviour is preserved.
func (p *Pager) AllocContiguous(n uint32) (uint64, error) {
	if p.readOnly {
		return 0, ErrReadOnly
	}
	if p.bitmap == nil {
		return 0, ErrFreespaceUnconfigured
	}
	if n == 0 {
		return 0, fmt.Errorf("pager: AllocContiguous: n must be > 0")
	}
	if n == 1 {
		return p.AllocPage()
	}

	// 1. Bitmap contiguous-run search.
	if firstID, ok := p.bitmap.FindContiguous(int(n)); ok {
		p.reserveBitmapRun(firstID, n)
		return firstID, nil
	}

	// 2. RPL reclamation + retry. Reclamation may set bits free in a
	// region that contains a contiguous run.
	if p.reclaimRPL() > 0 {
		if firstID, ok := p.bitmap.FindContiguous(int(n)); ok {
			p.reserveBitmapRun(firstID, n)
			return firstID, nil
		}
	}

	// 3. Lagging-reader callback per chunk-5.5 wiring (free-space.md
	// step 4 for the n>1 path). Same shape as AllocPage: invoke at
	// most once when RPL is non-empty AND reclamation is blocked.
	if p.laggingReader != nil && len(p.rplSegments) > 0 {
		info := p.buildLaggingReaderInfo()
		switch p.laggingReader(info) {
		case LaggingReaderAbort:
			return 0, ErrDBFull
		case LaggingReaderWait:
			if p.refreshReclamationBound != nil {
				p.reclamationBound = p.refreshReclamationBound()
				if p.reclaimRPL() > 0 {
					if firstID, ok := p.bitmap.FindContiguous(int(n)); ok {
						p.reserveBitmapRun(firstID, n)
						return firstID, nil
					}
				}
			}
		}
	}

	// 4. File extension. Advance HighWaterMark by n. Pages past the
	// prior HWM had no bitmap bit set (bits at and above totalPages
	// are forced clear; the same is conceptually true for pages
	// between HWM and totalPages until first used). They join
	// pendingAllocs without a bitmap.Clear call. One ensureFileCovers
	// covers the whole run.
	if p.highWaterMark+uint64(n) > p.maxSizePages {
		return 0, ErrDBFull
	}
	firstID := p.highWaterMark
	newHWM := p.highWaterMark + uint64(n)
	for id := firstID; id < newHWM; id++ {
		p.pendingAllocs[id] = struct{}{}
	}
	p.highWaterMark = newHWM
	if err := p.ensureFileCovers(p.highWaterMark); err != nil {
		// Roll back the HWM bump + pendingAllocs entries on truncate
		// failure. Atomicity per the chunk-5.2 Inv-1 contract.
		for id := firstID; id < newHWM; id++ {
			delete(p.pendingAllocs, id)
		}
		p.highWaterMark = firstID
		return 0, err
	}
	return firstID, nil
}

// reserveBitmapRun reserves [firstID, firstID+n) on the bitmap path:
// clears the bitmap bits, records pendingAllocs, drops any pendingFrees
// entries that were scheduled (a loose-page free that hadn't been
// committed). Sets the LIFO hint just past the run for locality.
func (p *Pager) reserveBitmapRun(firstID uint64, n uint32) {
	end := firstID + uint64(n)
	for id := firstID; id < end; id++ {
		p.bitmap.Clear(id)
		p.pendingAllocs[id] = struct{}{}
		delete(p.pendingFrees, id)
	}
	p.bitmap.SetHint(end - 1)
}

// FreeRun retires a contiguous run of n pages starting at firstID. Each
// ID in [firstID, firstID+n) is dispositioned by FreePage's per-ID
// rules: same-tx p.dirty page → loose; same-tx allocated-but-never-
// written → bitmap-bit restored; prior-tx → retiredPages.
//
// The chunk-4.7 rollback path calls FreeRun after AllocContiguous
// succeeds but AllocSlabRun fails — every page in the run is in
// pendingAllocs but not in p.dirty, so FreeRun restores all n bitmap
// bits and drops the pendingAllocs entries. No retiredPages growth.
func (p *Pager) FreeRun(firstID uint64, n uint32) error {
	if p.readOnly {
		return ErrReadOnly
	}
	if p.bitmap == nil {
		return ErrFreespaceUnconfigured
	}
	if n == 0 {
		return fmt.Errorf("pager: FreeRun: n must be > 0")
	}
	end := firstID + uint64(n)
	for id := firstID; id < end; id++ {
		if err := p.FreePage(id); err != nil {
			return err
		}
	}
	return nil
}
