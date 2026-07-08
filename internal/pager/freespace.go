package pager

import (
	"fmt"
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
//  4. Lagging-reader callback.
//  5. File extension — if no free pages anywhere, advance HighWaterMark
//     by one (up to maxSizePages; ErrDBFull at the cap). When the file
//     must grow, ensureFileCovers extends it by a GrowStep-aligned
//     increment (file-format.md §File Growth); the extra pages sit above
//     HighWaterMark — bitmap-clear, not marked free — and are handed out
//     by subsequent HWM advances without another ftruncate.
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

	// 1. Loose pages — suspended while a NESTED-kind child savepoint
	// is active. A loose page is a same-tx page that was CoW'd then
	// freed; reusing it detaches its slab buffer (see below) and lets
	// a subsequent CoW install fresh content. If a NESTED descendant
	// is in progress, that freed page may be one the child freed but
	// an ancestor's tree still references (the ancestor's saved
	// root), so reusing it would overwrite a buffer the ancestor
	// holds — violating the transactions.md §Invariants "child never
	// mutates a parent's slab buffer in place" guarantee (pager-slab.md §Slab Lifecycle Across Nested Transactions).
	// While savepointDepth > 0 the allocator skips straight to the
	// bitmap; freed pages remain loose (not reusable) until every
	// nested child resolves and the stack empties.
	//
	// SHALLOW savepoints (BeginShallowSavepoint, used by per-row
	// indexed maintenance) do NOT increment savepointDepth so loose-
	// pop stays enabled — internal-helper atomicity does not require
	// suspension because there is no concurrent parent tree pinning a
	// stale root. Every loose-pop event during a shallow savepoint
	// window is recorded in each active shallow savepoint's
	// loosePopLog so RestoreSavepoint can re-attach the detached
	// buffer (savepoint.go).
	for id := range p.loosePages {
		if p.savepointDepth > 0 {
			break
		}
		delete(p.loosePages, id)
		// loosePages[id] WAS present (we just iterated it) — record
		// the delete in the savepoint undo log when any savepoint is
		// open so an outstanding Restore can re-add the entry.
		p.recordSavepointUndo(fieldLoosePages, id, true)
		// Bookkeeping: the id was initially allocated (bitmap.Clear +
		// pendingAllocs add), then FreePage moved it to loosePages.
		// Re-allocating it now:
		//
		//   - Detach the existing slab buffer at p.dirty[id] into
		//     p.detachedBufs. Required by the
		//     loose-page reuse contract: the buffer in p.dirty[id]
		//     holds STALE content from when id was previously CoW'd
		//     before being freed. If we left it in p.dirty, the next
		//     pw.CoW(srcID, id) would hit CoW's idempotent re-CoW
		//     shortcut and return the stale buffer instead of
		//     refreshing with srcID's content (btree workloads
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
		//     error) takes the pendingAllocs branch
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
			// Record the loose-pop in every active SHALLOW savepoint
			// so a subsequent RestoreSavepoint can re-attach buf to
			// p.dirty[id]. (No-op when no shallow savepoint is active.)
			// The dirty-delete here is NOT recorded in the
			// savepointUndoLog — loosePopLog owns the buffer-pointer
			// pairing and the dirty re-attach happens in the
			// loose-pop replay phase of Restore. Filtered by kind ==
			// SavepointShallow because Nested savepoints suspend
			// loose-pop via savepointDepth > 0 (checked above), so
			// they can never observe this branch.
			//
			// wasPreWindow per Savepoint: was dirty[id] in p.dirty at
			// this savepoint's Begin time, or did the in-window add it
			// (via CoW / AllocSlab / AllocSlabRun)? Scan sp's window
			// slice of savepointUndoLog for a prior (fieldDirty, id,
			// false) entry — present iff dirty[id] was added inside
			// this window. Critical for Restore correctness: an in-
			// window add followed by a loose-pop within the same
			// window means buf is in-window content; replay must drop
			// it, not re-install it (re-installing would leak the
			// in-window buffer into the parent tx's dirty map post-
			// Restore — pager-slab.md byte-slice-ownership violation). See loosePopEntry godoc.
			for _, sp := range p.activeSavepoints {
				if sp.kind != SavepointShallow {
					continue
				}
				wasPreWindow := true
				for i := sp.undoLogPos; i < len(p.savepointUndoLog); i++ {
					e := p.savepointUndoLog[i]
					if e.field == fieldDirty && e.key == id && !e.wasPresent {
						wasPreWindow = false
						break
					}
				}
				sp.loosePopLog = append(sp.loosePopLog, loosePopEntry{
					id: id, buf: buf, wasPreWindow: wasPreWindow,
				})
			}
		}
		_, wasInPendingAllocs := p.pendingAllocs[id]
		p.pendingAllocs[id] = struct{}{}
		if !wasInPendingAllocs {
			p.recordSavepointUndo(fieldPendingAllocs, id, false)
		}
		// Defensive symmetry with the bitmap-path branch below. The
		// commit pipeline is the only writer of pendingFrees and runs
		// strictly after all tx-body AllocPage calls, so this delete
		// is unreachable today — kept for uniformity with the
		// FreePage contract (allocation removes a pending-free
		// scheduling) and to make a future commit-step rearrangement
		// safe by construction.
		_, wasInPendingFrees := p.pendingFrees[id]
		delete(p.pendingFrees, id)
		if wasInPendingFrees {
			p.recordSavepointUndo(fieldPendingFrees, id, true)
		}
		return id, nil
	}

	// 2. Allocation bitmap.
	if id, ok := p.bitmap.FindFirst(); ok {
		p.bitmap.Clear(id)
		p.bitmap.SetHint(id)
		_, wasInPendingAllocs := p.pendingAllocs[id]
		p.pendingAllocs[id] = struct{}{}
		if !wasInPendingAllocs {
			p.recordSavepointUndo(fieldPendingAllocs, id, false)
		}
		// If id was in pendingFrees from earlier in this tx (a loose
		// page that was already scheduled to be marked free), undo:
		// the bitmap bit goes from set→clear (commit-time) but the
		// page is now in active use again.
		_, wasInPendingFrees := p.pendingFrees[id]
		delete(p.pendingFrees, id)
		if wasInPendingFrees {
			p.recordSavepointUndo(fieldPendingFrees, id, true)
		}
		return id, nil
	}

	// 3. RPL reclamation. Walk tail→head, set bitmap bits for entries
	// in segments whose TxnID < reclamationBound, then retry the
	// bitmap.
	if p.reclaimRPL() > 0 {
		if id, ok := p.bitmap.FindFirst(); ok {
			p.bitmap.Clear(id)
			p.bitmap.SetHint(id)
			_, wasInPendingAllocs := p.pendingAllocs[id]
			p.pendingAllocs[id] = struct{}{}
			if !wasInPendingAllocs {
				p.recordSavepointUndo(fieldPendingAllocs, id, false)
			}
			_, wasInPendingFrees := p.pendingFrees[id]
			delete(p.pendingFrees, id)
			if wasInPendingFrees {
				p.recordSavepointUndo(fieldPendingFrees, id, true)
			}
			return id, nil
		}
	}

	// 4. Lagging-reader callback.
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
						_, wasInPendingAllocs := p.pendingAllocs[id]
						p.pendingAllocs[id] = struct{}{}
						if !wasInPendingAllocs {
							p.recordSavepointUndo(fieldPendingAllocs, id, false)
						}
						_, wasInPendingFrees := p.pendingFrees[id]
						delete(p.pendingFrees, id)
						if wasInPendingFrees {
							p.recordSavepointUndo(fieldPendingFrees, id, true)
						}
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
	// File-extension ids are necessarily fresh (id == prior HWM, which
	// was an unallocated page), so pendingAllocs[id] cannot have been
	// previously present. Record unconditionally — the only path that
	// might disagree is the ftruncate-error rollback below, which
	// undoes both the delete and the undo entry by the truncate-with-
	// erase pattern (the trailing undo entry's wasPresent=false
	// matches the rollback's delete(pendingAllocs, id)).
	p.pendingAllocs[id] = struct{}{}
	p.recordSavepointUndo(fieldPendingAllocs, id, false)
	if err := p.ensureFileCovers(p.highWaterMark); err != nil {
		// Roll back the HWM bump on truncate failure. The
		// savepointUndoLog entry just appended for id is also
		// reverted by trimming the log back one position, so a
		// later Restore does not undo a never-completed allocation.
		p.highWaterMark--
		delete(p.pendingAllocs, id)
		if n := len(p.savepointUndoLog); n > 0 && len(p.activeSavepoints) > 0 {
			last := p.savepointUndoLog[n-1]
			if last.field == fieldPendingAllocs && last.key == id && !last.wasPresent {
				p.savepointUndoLog = p.savepointUndoLog[:n-1]
			}
		}
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
	if int64(pages)*int64(p.cfg.PageSize) <= p.fileSize {
		return nil
	}
	// Grow in GrowStep-aligned increments (file-format.md §File Growth):
	// newSize = min(alignUp(pages, GrowStep), MaxSize). `pages` is the new
	// HighWaterMark (= prior HWM + the one page just claimed). The pages
	// between `pages` and the aligned end are file-backed but above
	// HighWaterMark, so later AllocPage calls advance HWM into them with NO
	// further ftruncate — one syscall per GrowStep pages, giving the OS a
	// large contiguous extent to read ahead. A zero GrowStep (unconfigured
	// pager / raw-NewWriter test) falls back to growing by the exact need.
	targetPages := alignUp(pages, p.growStepPages)
	if p.maxSizePages != 0 {
		targetPages = min(targetPages, p.maxSizePages)
	}
	targetPages = max(targetPages, pages) // never under-cover the allocated page
	target := int64(targetPages) * int64(p.cfg.PageSize)
	if err := p.fops.Truncate(target); err != nil {
		return fmt.Errorf("pager: ftruncate to %d: %w", target, err)
	}
	p.fileSize = target
	return nil
}

// alignUp rounds n up to the next multiple of step; step==0 returns n
// unchanged (a zero GrowStep means "no alignment — grow/shrink by exact
// need"). n is bounded by MaxSize pages, so n+step-1 cannot overflow uint64.
func alignUp(n, step uint64) uint64 {
	if step == 0 {
		return n
	}
	return ((n + step - 1) / step) * step
}

// FreePage marks id for retirement. The disposition depends on whether
// id was allocated within this transaction:
//
//   - In p.dirty (CoW'd or AllocSlab'd this tx): becomes a loose
//     page. Reusable via AllocPage; if not reused, its bitmap bit is set
//     at commit (bypassing the RPL because no other process holds a
//     snapshot referencing a same-tx page).
//   - In pendingAllocs but not yet in p.dirty (allocated this tx but
//     never CoW'd / AllocSlab'd — the overflow rollback path
//     reaches here when AllocContiguous succeeds but AllocSlabRun fails
//     mid-run): bitmap bit is restored to free, pendingAllocs entry is
//     dropped. No retiredPages entry: no prior-tx reader holds a
//     snapshot referencing this page.
//   - Not in p.dirty and not in pendingAllocs (mmap-backed, from a
//     prior tx): joins retiredPages. Appended to the RPL at commit so
//     the page survives in MVCC until the reclamation bound advances
//     past this tx's TxnID.
//
// Errors: ErrReadOnly on a read-only pager; ErrTxTooLarge when
// retiring a prior-tx page would push the commit-time RPL reserve
// past the slab budget (see the admission check on the retire branch).
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
		_, wasInLoose := p.loosePages[id]
		p.loosePages[id] = struct{}{}
		if !wasInLoose {
			p.tc.LoosePages++ // TxStats: distinct pages that went loose this tx
			p.recordSavepointUndo(fieldLoosePages, id, false)
		}
		_, wasInPendingAllocs := p.pendingAllocs[id]
		delete(p.pendingAllocs, id)
		if wasInPendingAllocs {
			p.recordSavepointUndo(fieldPendingAllocs, id, true)
		}
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
		// justAllocated => wasPresent=true at delete time.
		p.recordSavepointUndo(fieldPendingAllocs, id, true)
		return nil
	}
	// Prior-tx page: retire via RPL. retiredPages length is captured
	// at savepoint Begin and truncated on Restore, so the append needs
	// no per-entry undo log.
	//
	// Admission check mirroring CoW's: each retire grows the
	// commit-time RPL segment projection (rplReserveBytes), and a
	// retire-heavy operation (FreeSubtree) can grow it without any
	// intervening CoW admission. Reject the retire that would make the
	// transaction unable to afford its own commit — the caller rolls
	// the operation back per the write-helper error contract. Skipped
	// in the commit phase: the descriptor flush's own retires are
	// covered by the external reserve's RPL slack
	// (Tx.recalcFlushReserve).
	capPerSeg := RPLEntriesPerSegment(p.cfg)
	if !p.inCommit && capPerSeg > 0 && len(p.retiredPages)%capPerSeg == 0 {
		// This retire opens a new segment: projected reserve grows by
		// one page.
		if p.dirtyBytes+p.rplReserveBytes()+p.externalReserve+int(p.cfg.PageSize) > p.maxBytes {
			return ErrTxTooLarge
		}
	}
	p.retiredPages = append(p.retiredPages, id)
	return nil
}

// FreeLeakedPage frees a leaked page — one the caller has determined is
// allocated in the bitmap yet unreachable from every committed tree and
// absent from the RPL — by marking its bitmap bit free directly,
// bypassing the RPL.
//
// This differs from FreePage's prior-tx path (which retires through the
// RPL so a reader pinning an older snapshot keeps the page until it
// drains). A genuinely leaked page is referenced by NO snapshot, so it
// needs no reader-pinned deferral — but that is only true under verified
// exclusive access (no concurrent readers or writers). The caller (the
// exclusive Repair path; api-surface.md §Check, CopyTo, Compact) is responsible for establishing
// that precondition; this method does not and cannot check it.
//
// Atomicity: the freed bit is dirtied in the in-memory bitmap ONLY. It
// reaches disk solely through the next Commit's atomic meta-swap (step-1
// bitmap pwrite, then meta publication), so a crash mid-repair leaves
// either the pre-repair bitmap or the fully-repaired one — never a
// partial free. There is deliberately no incremental-flush path.
func (p *Pager) FreeLeakedPage(id uint64) error {
	if p.readOnly {
		return ErrReadOnly
	}
	if p.bitmap == nil {
		return ErrFreespaceUnconfigured
	}
	// A leaked page is ALLOCATED (bit clear); IsSet reports true for a
	// FREE page. An already-free id means the caller's leak set is stale
	// — refuse rather than double-count NumFreePages.
	if p.bitmap.IsSet(id) {
		return fmt.Errorf("pager: FreeLeakedPage %d: page is already free", id)
	}
	p.bitmap.Set(id) // mark free + dirty
	return nil
}

// reclaimRPL walks the in-memory RPL segment list from tail (oldest) and
// reclaims whole segments whose TxnID < reclamationBound. For each
// reclaimed segment:
//
//  1. Read the segment page via the shared per-segment convention
//     (readRPLSegment: checksum-footer verification before decode —
//     checksums.md covers RPL segments like every data page and this
//     walker reads via pageRaw, which does NOT verify), then bound
//     every listed PageID against the bitmap's allocatable range (a
//     decodable segment with a bit-flipped or forged entry would
//     otherwise free a live tree page in-range, or panic in Bitmap.Set
//     out-of-range — a crash on corrupt input, against integrity.md's
//     error-not-crash contract). Any of the three failing quarantines
//     the segment (below). In-range WRONG ids on a checksums-off
//     database are inherently undetectable here — that is the trade
//     the PageChecksum option states.
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
		var decoded RPLSegment
		// The segment page id itself is bounded FIRST — before pageRaw
		// (which panics past the mmap reservation) and before the Set
		// at the end (which panics outside the allocatable range). A
		// wild id can enter the chain via a corrupt/forged meta whose
		// geometry shifted firstDataPage under the persisted chain.
		ok := seg.PageID >= p.bitmap.FirstDataPage() && seg.PageID < p.bitmap.TotalPages()
		if ok {
			decoded, _, ok = readRPLSegment(p.pageRaw, p.cfg, seg.PageID)
		}
		if ok {
			// Whole-segment validation BEFORE any Set — quarantine
			// must be atomic (free-space.md: whole-segment
			// consumption), not free a prefix then bail.
			for _, id := range decoded.PageIDs {
				if id < p.bitmap.FirstDataPage() || id >= p.bitmap.TotalPages() {
					ok = false
					break
				}
			}
		}
		if !ok {
			// Corrupt RPL segment. We cannot decode its page IDs to
			// reclaim them, nor safely free the segment page itself,
			// so those pages leak — but the leak is bounded to THIS
			// segment and recoverable later via Check()/Repair's
			// structural walk. Quarantine it: record + surface the
			// corruption, pop it from the chain, and CONTINUE so newer
			// eligible segments still reclaim (each segment's
			// reclaimability is independent of older ones). Halting the
			// whole queue here would strand every newer segment's free
			// space behind one torn segment (free-space.md §RPL
			// Reclamation).
			p.rplCorruptCount++
			if p.rplCorruptCb != nil {
				p.rplCorruptCb(seg.PageID)
			}
			p.trimRPLChainTail(1)
			continue
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
		// Pop from the in-memory list. copy-trim preserves trailing
		// slice capacity so the next appendRPL reuses the same
		// backing array; `s = s[1:]` would shrink cap by 1 and force
		// earlier reallocation as the chain re-grows.
		p.trimRPLChainTail(1)
	}
	if count > 0 {
		// Bias the next allocation toward the most-recently-reclaimed
		// region per LIFO locality.
		p.bitmap.SetHint(lastReclaimed)
	}
	p.tc.ReclaimedPages += uint64(count) // TxStats: RPL pages reclaimed this tx
	return count
}

// trimRPLChainTail removes the n oldest entries (chain tail per
// SetRPLChain: tail = index 0 = oldest TxnID, head = last = newest)
// from the in-memory chain. Copies surviving entries down and
// reslices so the next append reuses the full backing array;
// `p.rplSegments = p.rplSegments[n:]` would shrink cap by n and force
// earlier reallocation as the chain re-grows on subsequent commits.
func (p *Pager) trimRPLChainTail(n int) {
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
			_, wasInPendingFrees := p.pendingFrees[tail]
			delete(p.pendingFrees, tail)
			if wasInPendingFrees {
				p.recordSavepointUndo(fieldPendingFrees, tail, true)
			}
			p.highWaterMark--
			continue
		}
		if _, loose := p.loosePages[tail]; loose {
			delete(p.loosePages, tail)
			p.recordSavepointUndo(fieldLoosePages, tail, true)
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
// extension). Implements the PageWriter contract used by the
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
// Loose pages are not consulted: overflow runs require
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

	// Instrument the contiguous-allocation failure rate (the incremental-
	// compaction trigger, background-maintenance.md §Incremental
	// Compaction). Every n>1 call is an attempt.
	p.contigAttempts.Add(1)

	// 1. Bitmap contiguous-run search.
	if firstID, ok := p.bitmap.FindContiguous(int(n)); ok {
		p.reserveBitmapRun(firstID, n)
		return firstID, nil
	}
	// First bitmap scan found no contiguous run. If total free pages still
	// cover the request, the failure is fragmentation (not fullness) — the
	// signal incremental compaction exists to relieve. Counted on the first
	// scan regardless of whether a later tier (RPL reclaim / lagging-reader
	// wait / file extension) satisfies the request.
	if p.bitmap.NumFree() >= uint64(n) {
		p.contigFragFails.Add(1)
	}

	// 2. RPL reclamation + retry. Reclamation may set bits free in a
	// region that contains a contiguous run.
	if p.reclaimRPL() > 0 {
		if firstID, ok := p.bitmap.FindContiguous(int(n)); ok {
			p.reserveBitmapRun(firstID, n)
			return firstID, nil
		}
	}

	// 3. Lagging-reader callback (free-space.md
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
	// File-extension ids are necessarily fresh, so each wasPresent is
	// false. Append n undo entries unconditionally — the rollback
	// branch below trims them when ensureFileCovers fails.
	undoBaseLen := len(p.savepointUndoLog)
	for id := firstID; id < newHWM; id++ {
		p.pendingAllocs[id] = struct{}{}
		p.recordSavepointUndo(fieldPendingAllocs, id, false)
	}
	p.highWaterMark = newHWM
	if err := p.ensureFileCovers(p.highWaterMark); err != nil {
		// Roll back the HWM bump + pendingAllocs entries on truncate
		// failure. Atomicity per the run-allocation atomicity contract. Trim
		// the savepointUndoLog entries appended for this run so a
		// later Restore does not undo a never-completed allocation.
		for id := firstID; id < newHWM; id++ {
			delete(p.pendingAllocs, id)
		}
		if len(p.savepointUndoLog) > undoBaseLen {
			p.savepointUndoLog = p.savepointUndoLog[:undoBaseLen]
		}
		p.highWaterMark = firstID
		return 0, err
	}
	return firstID, nil
}

// ConsumeContiguousAllocStats atomically reads and resets the contiguous-
// allocation counters accumulated since the last call: attempts =
// multi-page (n>1) AllocContiguous calls; fragFails = those whose first
// bitmap scan found no contiguous run despite sufficient total free pages
// (fragmentation, not fullness). The background-maintenance compaction
// trigger consumes these once per pass so the failure rate reflects recent
// allocation behaviour and converges after compaction reduces fragmentation
// (background-maintenance.md §Incremental Compaction).
//
// The two Swaps are not jointly atomic; a concurrent increment landing
// between them shifts the next window's ratio by at most one sample —
// acceptable for a scheduling heuristic.
func (p *Pager) ConsumeContiguousAllocStats() (attempts, fragFails uint64) {
	return p.contigAttempts.Swap(0), p.contigFragFails.Swap(0)
}

// reserveBitmapRun reserves [firstID, firstID+n) on the bitmap path:
// clears the bitmap bits, records pendingAllocs, drops any pendingFrees
// entries that were scheduled (a loose-page free that hadn't been
// committed). Sets the LIFO hint just past the run for locality.
func (p *Pager) reserveBitmapRun(firstID uint64, n uint32) {
	end := firstID + uint64(n)
	for id := firstID; id < end; id++ {
		p.bitmap.Clear(id)
		_, wasInPendingAllocs := p.pendingAllocs[id]
		p.pendingAllocs[id] = struct{}{}
		if !wasInPendingAllocs {
			p.recordSavepointUndo(fieldPendingAllocs, id, false)
		}
		_, wasInPendingFrees := p.pendingFrees[id]
		delete(p.pendingFrees, id)
		if wasInPendingFrees {
			p.recordSavepointUndo(fieldPendingFrees, id, true)
		}
	}
	p.bitmap.SetHint(end - 1)
}

// FreeRun retires a contiguous run of n pages starting at firstID. Each
// ID in [firstID, firstID+n) is dispositioned by FreePage's per-ID
// rules: same-tx p.dirty page → loose; same-tx allocated-but-never-
// written → bitmap-bit restored; prior-tx → retiredPages.
//
// The overflow rollback path calls FreeRun after AllocContiguous
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
