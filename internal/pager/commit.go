package pager

import (
	"fmt"
	"slices"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// SyncPolicy selects which fdatasync calls fire during commit. The
// promotion of durability.md §Durability Modes; mapped 1:1
// onto the root package's gmdb.SyncMode but kept narrow here so the
// pager doesn't import the root.
//
//   - SyncBoth: fdatasync at step 2 AND step 4. Maps from
//     SyncDurable. Sets MetaFlagCheckpoint on the new meta.
//   - SyncDataOnly: fdatasync at step 2; skip step 4. Maps from
//     SyncDataOnly. Sets MetaFlagCheckpoint (data is durable).
//   - SyncNone: skip both. Maps from SyncLazy.
//     Caller decides MetaFlagCheckpoint via the Flags field
//     (SyncLazy clears it).
type SyncPolicy uint8

const (
	SyncBoth     SyncPolicy = iota // step 2 + step 4 fdatasync. SyncDurable.
	SyncDataOnly                   // step 2 fdatasync only. SyncDataOnly.
	SyncNone                       // no fdatasync. SyncLazy.
)

// CommitParams supplies the meta-level updates a commit publishes.
//   - NewTxnID: TxnID for the new meta page. Per the file-layout.md
//     strict-increase invariant, this must be strictly greater than
//     prev.TxnID (or, for the genesis commit on a TxnID==0 file, equal
//     to 1).
//   - KeyspaceRoot / NumKeyspaces: root-state snapshot for the new
//     meta. Supplied by the caller's current root state.
//   - Flags: meta-page Flags for the new meta. The caller composes
//     MetaFlagPageChecksum (from prev) and MetaFlagCheckpoint (per
//     SyncMode policy). The pager does NOT auto-set Checkpoint based
//     on Sync — that decision is the caller's, because durability.md
//     ties Checkpoint to "data-pages on stable storage" which is the
//     step-2 fsync (SyncBoth, SyncDataOnly) but not step-4 (meta
//     fsync). Caller composes per the SyncMode table in
//     durability.md.
//   - Sync: which fdatasync calls fire (see SyncPolicy doc).
type CommitParams struct {
	NewTxnID     uint64
	KeyspaceRoot uint64
	NumKeyspaces uint64
	Flags        uint32
	Sync         SyncPolicy
	// SetFileFormat, when non-nil, overrides the mutable file-format meta
	// fields (MinSize / GrowStep / ShrinkThreshold, in pages) on the new
	// meta — Tx.SetFileFormat (api-surface.md §File Format, file-format.md).
	// nil ⇒ the new meta carries the previous values unchanged. MaxSize and
	// BitmapPages are immutable and are never overridden.
	SetFileFormat *MetaFileFormat
}

// MetaFileFormat carries a Tx.SetFileFormat override of the mutable
// file-format meta fields (in pages) into Commit.
type MetaFileFormat struct {
	MinSize         uint64
	GrowStep        uint64
	ShrinkThreshold uint64
}

// CommitResult bundles the post-commit meta state for the caller.
type CommitResult struct {
	Meta          page.Meta
	ActiveMetaIdx int
}

// Commit runs the four-step commit protocol from pager-slab.md.
//
// Inputs: cp (new meta-level state); prev (the active meta as of tx
// begin — UUID, MinSize/MaxSize/GrowStep/ShrinkThreshold, BitmapPages
// flow through unchanged); prevActive (the active meta index before
// this commit; Commit writes the new meta to the OTHER slot).
//
// On success returns the new meta + new active index. On any error the
// on-disk meta is untouched (the step-3 meta pwrite is the only
// publication point) AND the pager's in-memory state is rolled back
// to the snapshot taken by BeginTx: bitmap bits, HighWaterMark, and
// RPL chain are restored; slab buffers are released; tx-scoped
// bookkeeping is reset. Data-page pwrites that succeeded before the
// failure become bounded crash-equivalent leakage that background
// maintenance reclaims.
//
// Callers must call BeginTx before each commit attempt so the snapshot
// exists. Without a snapshot, AbortTx is a no-op on the bitmap and
// in-memory divergence may persist until the next Open.
func (p *Pager) Commit(cp CommitParams, prev page.Meta, prevActive int) (CommitResult, error) {
	if p.readOnly {
		return CommitResult{}, ErrReadOnly
	}
	if p.bitmap == nil {
		return CommitResult{}, ErrFreespaceUnconfigured
	}
	if cp.NewTxnID <= prev.TxnID {
		return CommitResult{}, fmt.Errorf("pager: NewTxnID %d must be strictly greater than prev TxnID %d", cp.NewTxnID, prev.TxnID)
	}

	// Step 0 — assembly. May call ftruncate up via the allocator's
	// file-extension path, which is reader-observability-safe (the
	// active meta still says HWM=prev.HighWaterMark, so no reader
	// accesses the newly-mapped region). On failure: full rollback
	// via AbortTx — bitmap, HWM, RPL chain restored from snapshot.
	if err := p.commitStep0(); err != nil {
		p.AbortTx()
		return CommitResult{}, err
	}

	// Compose the new meta payload now (step 0 finalised HWM, RPL chain,
	// bitmap state).
	newMeta := p.buildNewMeta(cp, prev)
	metaBuf := make([]byte, p.cfg.PageSize)
	page.EncodeMeta(metaBuf, &newMeta)

	newActive := 1 - prevActive

	// Step 1 — pwrite data + RPL + bitmap pages.
	nWritten, err := p.commitStep1()
	if err != nil {
		p.AbortTx()
		return CommitResult{}, fmt.Errorf("pager: step 1: %w", err)
	}

	// Step 2 — fdatasync per SyncPolicy. SyncBoth + SyncDataOnly
	// fsync; SyncNone skips. Per durability.md §Durability Modes
	// table, skipping step 2 in SyncLazy means data pages may not
	// reach disk before the meta does — recovery's checkpoint-
	// preferring meta selector compensates: a SyncLazy commit
	// writes its meta with MetaFlagCheckpoint CLEAR, so recovery
	// falls back to the last checkpoint-flagged meta whose data
	// IS durable.
	if cp.Sync != SyncNone {
		if err := fdatasync(p.file); err != nil {
			p.AbortTx()
			return CommitResult{}, fmt.Errorf("pager: step 2 fdatasync: %w", err)
		}
	}

	// Step 3 — pwrite the new meta to its slot. From this point on a
	// crash leaves the new meta visible on Open; we are past the
	// publication point of the commit protocol. On a step-3 / step-4
	// failure we still call AbortTx to clean up in-memory state, but
	// the on-disk meta may already point at the new tree — the next
	// Open uses ActiveMeta selection (highest valid TxnID) which
	// correctly picks whichever meta has the higher TxnID, including
	// the new one if step 3 partially completed.
	off := int64(newActive) * int64(p.cfg.PageSize)
	if _, err := p.file.WriteAt(metaBuf, off); err != nil {
		p.AbortTx()
		return CommitResult{}, fmt.Errorf("pager: step 3 write meta %d: %w", newActive, err)
	}

	// Test injection point — see commitStep4HookForTest doc on Pager.
	// Fires after step 3's pwrite has placed the new meta on disk so
	// the test observes the same step-3-success / step-4-fail state
	// the publication-phase failure mode produces in production.
	if p.commitStep4HookForTest != nil {
		if err := p.commitStep4HookForTest(); err != nil {
			p.AbortTx()
			return CommitResult{}, fmt.Errorf("pager: step 4 fdatasync meta (test-injected): %w", err)
		}
	}

	// Step 4 — fdatasync (atomic commit point) per SyncPolicy.
	// SyncBoth fsyncs; SyncDataOnly + SyncNone skip — but the
	// MetaFlagCheckpoint composition in cp.Flags reflects that
	// fact (caller has cleared the flag on SyncLazy;
	// SyncDataOnly KEEPS the flag because data IS durable per
	// step 2). Recovery's checkpoint-preferring selector reads
	// the flag.
	if cp.Sync == SyncBoth {
		if err := fdatasync(p.file); err != nil {
			p.AbortTx()
			return CommitResult{}, fmt.Errorf("pager: step 4 fdatasync meta: %w", err)
		}
	}

	// Success path: shrink, then clean up without restoring snapshot.
	if err := p.maybeShrink(prev.ShrinkThreshold); err != nil {
		// Non-fatal. Bounded trailing slack reclaimable next commit.
		_ = err
	}

	// TxStats: data/RPL/bitmap pages pwritten in step 1, plus the one
	// meta page pwritten in step 3 (api-surface.md §Statistics
	// TxStats.WrittenPages). Recorded on the success path so a
	// post-commit Stats() (before the next BeginTx) sees the total.
	p.setWrittenPages(uint64(nWritten) + 1)

	p.discardTxSnapshot()
	p.ReleaseAll()
	p.bitmap.ClearDirty()
	clear(p.pendingAllocs)
	clear(p.pendingFrees)
	clear(p.loosePages)
	p.retiredPages = p.retiredPages[:0]
	p.currentTxnID = 0

	return CommitResult{Meta: newMeta, ActiveMetaIdx: newActive}, nil
}

// commitStep0 performs the no-syscall assembly phase: tail refund,
// loose→pendingFrees migration, RPL segment allocation+encoding, and
// the corresponding bitmap-bit updates. Any failure inside this method
// is reversible — no writes have left the process.
func (p *Pager) commitStep0() error {
	// (a) Tail refund — drop trailing free pages from HWM.
	if err := p.TailRefund(); err != nil {
		return fmt.Errorf("step 0 tail refund: %w", err)
	}

	// (b) Move remaining loose pages to pendingFrees. Same-tx pages
	// cannot be referenced by any reader (no reader saw a meta
	// referencing them), so they bypass the RPL and go straight to
	// "free in bitmap" at commit.
	for id := range p.loosePages {
		p.bitmap.Set(id)
		p.pendingFrees[id] = struct{}{}
		delete(p.loosePages, id)
	}

	// (c) Allocate RPL segment pages for the retired-pages list and
	// fill them with (TxnID, sorted PageIDs). Each segment goes into
	// p.dirty via AllocSlab so step 1 pwrites it.
	if len(p.retiredPages) > 0 {
		if err := p.appendRPL(); err != nil {
			return fmt.Errorf("step 0 RPL append: %w", err)
		}
	}

	// (d) Bitmap-page changes are already reflected in the bitmap's
	// dirty set (Set/Clear were called inline as work happened in
	// AllocPage, FreePage, TailRefund, reclaimRPL, and the loose→pendingFrees
	// loop above). No further work needed.

	// (e) The new meta payload is constructed by the caller-facing
	// Commit() after step 0 returns; building it here would leak
	// commit-protocol parameters into a private helper.

	return nil
}

// appendRPL allocates one or more RPL segment pages to hold the tx's
// retired pages, fills them per the spec layout, inserts them into
// p.dirty, and appends new entries to the in-memory chain so the head
// pointer reflects the newest segment.
//
// Allocation and encoding are split into two phases to close the
// self-reference race the round-1 review caught: phase 1 reserves all
// segment page IDs (allocator may trigger RPL reclamation, which
// drains oldest segments from the chain tail per the SetRPLChain
// convention and in the full-drain limit empties the chain, after
// which headPageID() returns 0); phase 2 captures `prevHead` from
// the post-reservation chain state, then encodes segments with
// `OlderSegment` links that never point to a page reclaimed in
// phase 1.
func (p *Pager) appendRPL() error {
	capPerSeg := page.RPLEntriesPerSegment(p.cfg)
	if capPerSeg <= 0 {
		return fmt.Errorf("pager: RPL segment capacity is %d", capPerSeg)
	}
	if p.currentTxnID == 0 {
		return fmt.Errorf("pager: currentTxnID not seeded before RPL append")
	}

	// Sort pageIDs ascending per free-space.md §RPL append.
	ids := slices.Clone(p.retiredPages)
	slices.Sort(ids)

	numSegs := (len(ids) + capPerSeg - 1) / capPerSeg

	// Phase 1: reserve all segment page IDs. Each AllocPage may
	// trigger reclaimRPL, which pops segments from p.rplSegments.
	// After this loop completes, the chain state is stable for
	// phase 2.
	segPageIDs := make([]uint64, numSegs)
	for i := range numSegs {
		id, err := p.AllocPage()
		if err != nil {
			return fmt.Errorf("pager: alloc RPL segment page: %w", err)
		}
		segPageIDs[i] = id
	}

	// Phase 2: snapshot prevHead AFTER reservation so reclamation has
	// settled. Encode and install buffers.
	prevHead := p.headPageID()
	newSegs := make([]RPLSegmentRef, numSegs)
	for i := range numSegs {
		start := i * capPerSeg
		end := min(start+capPerSeg, len(ids))
		segIDs := ids[start:end]
		segPageID := segPageIDs[i]
		buf, err := p.AllocSlab(segPageID)
		if err != nil {
			return fmt.Errorf("pager: alloc RPL segment buffer: %w", err)
		}
		var older uint64
		if i == 0 {
			older = prevHead
		} else {
			older = segPageIDs[i-1]
		}
		// Defense in depth: a self-reference is structurally invalid
		// (Open's chain walk would loop), and the only way to reach
		// it here is a bug in reservation. Refuse rather than encode.
		if older == segPageID {
			return fmt.Errorf("pager: RPL segment self-reference at page %d: %w", segPageID, ErrCorrupted)
		}
		page.EncodeRPLSegment(buf, p.cfg, p.currentTxnID, older, segIDs)
		newSegs[i] = RPLSegmentRef{
			PageID: segPageID,
			TxnID:  p.currentTxnID,
			Count:  uint32(len(segIDs)),
		}
	}

	// Append newSegs to the in-memory chain (tail-first ordering).
	p.rplSegments = append(p.rplSegments, newSegs...)
	p.retiredPages = p.retiredPages[:0]
	return nil
}

// headPageID returns the current RPL head (newest segment's page id),
// or 0 if the chain is empty.
func (p *Pager) headPageID() uint64 {
	if len(p.rplSegments) == 0 {
		return 0
	}
	return p.rplSegments[len(p.rplSegments)-1].PageID
}

// commitStep1 pwrites the dirty data/RPL pages from p.dirty and the
// modified bitmap pages from the bitmap struct. Computes the xxhash64
// footer for each page that needs one (data + RPL — bitmap pages carry
// no checksum per checksums.md §Storage).
func (p *Pager) commitStep1() (int, error) {
	pageSize := int(p.cfg.PageSize)
	written := 0
	for id, buf := range p.dirty {
		if p.cfg.PageChecksum {
			page.WritePageFooter(*buf, p.cfg.PageSize)
		}
		off := int64(id) * int64(pageSize)
		if _, err := p.file.WriteAt(*buf, off); err != nil {
			return written, fmt.Errorf("pwrite page %d: %w", id, err)
		}
		written++
	}
	for _, idx := range p.bitmap.DirtyPages() {
		off := int64(2+uint64(idx)) * int64(pageSize)
		if _, err := p.file.WriteAt(p.bitmap.PageBytes(idx), off); err != nil {
			return written, fmt.Errorf("pwrite bitmap page %d: %w", idx, err)
		}
		written++
	}
	return written, nil
}

// buildNewMeta composes the new meta payload from CommitParams + the
// previous meta's persistent file-format fields + the pager's freshly-
// updated state.
func (p *Pager) buildNewMeta(cp CommitParams, prev page.Meta) page.Meta {
	headID := p.headPageID()
	var tailID uint64
	if len(p.rplSegments) > 0 {
		tailID = p.rplSegments[0].PageID
	}
	var entryCount uint64
	for _, s := range p.rplSegments {
		entryCount += uint64(s.Count)
	}
	// Mutable file-format fields carry forward from prev unless Tx.SetFileFormat
	// overrode them this commit. MaxSize/BitmapPages are immutable (changing
	// MaxSize would shift the bitmap region and every data-page offset).
	minSize, growStep, shrinkThreshold := prev.MinSize, prev.GrowStep, prev.ShrinkThreshold
	if cp.SetFileFormat != nil {
		minSize = cp.SetFileFormat.MinSize
		growStep = cp.SetFileFormat.GrowStep
		shrinkThreshold = cp.SetFileFormat.ShrinkThreshold
	}
	return page.Meta{
		Magic:           page.Magic,
		Version:         page.FormatVersion,
		PageSize:        p.cfg.PageSize,
		Flags:           cp.Flags,
		BitmapPages:     prev.BitmapPages,
		UUID:            prev.UUID,
		MinSize:         minSize,
		MaxSize:         prev.MaxSize,
		GrowStep:        growStep,
		ShrinkThreshold: shrinkThreshold,
		HighWaterMark:   p.highWaterMark,
		RPLHeadPage:     headID,
		RPLTailPage:     tailID,
		RPLEntryCount:   entryCount,
		NumFreePages:    p.bitmap.NumFree(),
		KeyspaceRoot:    cp.KeyspaceRoot,
		NumKeyspaces:    cp.NumKeyspaces,
		TxnID:           cp.NewTxnID,
	}
}

// SetCurrentTxnID seeds the in-progress TxnID. Required before Commit
// when the tx has retired any pages — the RPL segment encoder uses this
// to stamp the per-segment TxnID. Idempotent.
func (p *Pager) SetCurrentTxnID(txnID uint64) { p.currentTxnID = txnID }

// maybeShrink truncates the file toward a GrowStep-aligned size at or above
// HighWaterMark, floored at MinSize, when the trailing slack exceeds
// shrinkThreshold pages (file-format.md §File Shrinkage). It never truncates
// below MinSize (the user's pre-allocated minimum is never discarded — a
// clause-explicit invariant) nor below HighWaterMark. Called at the end of
// Commit after the meta swap. Non-fatal on failure.
func (p *Pager) maybeShrink(shrinkThreshold uint64) error {
	if shrinkThreshold == 0 {
		return nil
	}
	// newSize = max(alignUp(HighWaterMark, GrowStep), MinSize), never above
	// the current size. A zero GrowStep aligns to HighWaterMark itself; a
	// zero MinSize imposes no floor (raw-NewWriter fallback).
	targetPages := max(alignUp(p.highWaterMark, p.growStepPages), p.minSizePages)
	target := int64(targetPages) * int64(p.cfg.PageSize)
	if target >= p.fileSize {
		return nil // target at/above current size — nothing to shrink
	}
	if p.fileSize-target < int64(shrinkThreshold)*int64(p.cfg.PageSize) {
		return nil // trailing slack below threshold — avoid ftruncate thrash
	}
	if err := p.file.Truncate(target); err != nil {
		return err
	}
	p.fileSize = target
	return nil
}
