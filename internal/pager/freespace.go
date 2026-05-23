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
		// No bookkeeping update needed here. Trace the lifecycle:
		// the id was initially allocated (bitmap.Clear + pendingAllocs
		// add), then FreePage removed it from pendingAllocs and put
		// it in loosePages. Reusing here just hands the id back —
		// the bitmap bit is still clear (correct on-disk state for
		// "allocated"), and the id has no pendingAllocs entry. That
		// matches the desired commit state: the id is in active use
		// but its on-disk bit doesn't need to flip (it was already
		// clear from the initial alloc). The slab buffer remains in
		// p.dirty per the byte-slice ownership invariant.
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

	// 4. Lagging-reader callback — not in chunk 1.

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
//   - In p.dirty (CoW'd or freshly-allocated this tx): becomes a loose
//     page. Reusable via AllocPage; if not reused, its bitmap bit is set
//     at commit (bypassing the RPL because no other process holds a
//     snapshot referencing a same-tx page).
//   - Not in p.dirty (mmap-backed, from a prior tx): joins retiredPages.
//     Appended to the RPL at commit so the page survives in MVCC until
//     the reclamation bound advances past this tx's TxnID.
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
