package pager

import "fmt"

// RPL chain-prefix relocation (free-space.md §RPL segment relocation):
// the incremental-compaction pass asks the commit pipeline to move RPL
// segment pages out of an evacuation region. Segments are immutable
// and newest→oldest linked, so moving one re-points its newer
// neighbor, cascading to the head; the pipeline is the one safe
// cascade site. The requesting transaction's commit copies the full
// prefix from the deepest in-region segment to the head into
// below-floor pages, retires the old prefix pages in its own head
// segment, and publishes through the ordinary meta flip.

// RequestRPLRelocation arms a one-shot relocation request for the
// NEXT commit on this pager: relocate every RPL segment page at or
// above floor. The caller is the compaction pass's transaction (the
// request is consumed — executed or declined — by that transaction's
// own commit). floor must be a data-region page id BELOW the
// high-water mark — homes are claimed strictly below floor, and a
// floor above the HWM could claim homes past it, which Check's
// hwm-bounded RPL walk rejects (the compaction pass's evacuationFloor
// satisfies this by construction). 0 disarms.
func (p *Pager) RequestRPLRelocation(floor uint64) {
	p.rplRelocFloor = floor
	p.rplRelocDeclined = false
}

// RPLRelocationDeclined reports whether the last armed relocation
// request was declined (unplaceable copy or over-budget prefix —
// free-space.md §RPL segment relocation's decline rule). Read by the
// compaction pass after commit to report the region unsatisfiable
// for this pass. Reset by the next RequestRPLRelocation.
func (p *Pager) RPLRelocationDeclined() bool { return p.rplRelocDeclined }

// RPLSegmentsAtOrAbove reports how many RPL segment pages of the
// in-memory chain sit at or above floor — the compaction pass's
// signal that a region is pinned by RPL pages and a relocation
// request is worth arming.
func (p *Pager) RPLSegmentsAtOrAbove(floor uint64) int {
	n := 0
	for _, seg := range p.rplSegments {
		if seg.PageID >= floor {
			n++
		}
	}
	return n
}

// relocateRPLPrefix executes an armed relocation request. Called by
// commitStep0 BEFORE appendRPL so the old prefix pages join this
// transaction's retirement list and ride its head segment.
//
// Probe-first (the spec's decline rule): the prefix bound, the slab
// budget, and a below-floor home for every copy are all established
// read-only before any state changes; a failed probe declines the
// whole request and leaves the transaction untouched. Single-threaded
// commit assembly under the write grant makes probe-then-claim
// race-free.
func (p *Pager) relocateRPLPrefix() error {
	floor := p.rplRelocFloor
	if floor == 0 {
		return nil
	}
	p.rplRelocFloor = 0 // one-shot: consumed regardless of outcome

	// Deepest in-region segment. p.rplSegments is tail-first; the
	// prefix to copy is rplSegments[j:] for the smallest j whose page
	// sits at/above the floor.
	j := -1
	for i, seg := range p.rplSegments {
		if seg.PageID >= floor {
			j = i
			break
		}
	}
	if j < 0 {
		return nil // nothing in-region: trivially satisfied
	}
	k := len(p.rplSegments) - j

	// Probe 1 — slab budget: k copy buffers on top of current usage.
	// Commit assembly runs with the raw cap (inCommit), so probe
	// against maxBytes directly; a mid-copy AllocSlab failure would
	// violate probe-first.
	if p.dirtyBytes+k*int(p.cfg.PageSize) > p.maxBytes {
		p.rplRelocDeclined = true
		return nil
	}

	// Probe 2 — a below-floor home for every copy, read-only:
	// advance `from` past each probed id so the k ids are distinct.
	homes := make([]uint64, 0, k)
	from := uint64(0)
	for range k {
		id, ok := p.bitmap.FindFirstBelowFrom(from, floor)
		if !ok {
			p.rplRelocDeclined = true
			return nil
		}
		homes = append(homes, id)
		from = id + 1
	}

	// Probe 3 — every prefix segment still decodes, read-only. A
	// bitrot segment DECLINES (the quarantine machinery owns bitrot;
	// erroring here would fail every later compaction pass's commit
	// against the same region — free-space.md's decline rule names
	// this case).
	segs := make([]RPLSegment, k)
	for m := 0; m < k; m++ {
		seg, _, ok := readRPLSegment(p.pageRaw, p.cfg, p.rplSegments[j+m].PageID)
		if !ok {
			p.rplRelocDeclined = true
			return nil
		}
		segs[m] = seg
	}

	// Probes passed — claim and copy, deepest first. The deepest
	// copy keeps its on-disk OlderSegment verbatim (it points at an
	// untouched older segment, or carries the stale tail link);
	// every newer copy points at its neighbor's copy.
	prevNewID := uint64(0)
	for m := 0; m < k; m++ {
		ref := p.rplSegments[j+m]
		oldID := ref.PageID
		newID := homes[m]

		// Claim newID with AllocPage's bitmap-path bookkeeping.
		p.bitmap.Clear(newID)
		p.bitmap.SetHint(newID)
		if _, was := p.pendingAllocs[newID]; !was {
			p.pendingAllocs[newID] = struct{}{}
			p.recordSavepointUndo(fieldPendingAllocs, newID, false)
		}
		if _, was := p.pendingFrees[newID]; was {
			delete(p.pendingFrees, newID)
			p.recordSavepointUndo(fieldPendingFrees, newID, true)
		}

		// Re-encode the probed decode into the copy's slab buffer
		// (the canonical encoder is deterministic, so this is the
		// byte-for-byte copy modulo the OlderSegment rewrite). A
		// below-floor home returned by the probe cannot hold a stale
		// dirty buffer here: commit step (b2) Discarded every buffer
		// whose page is in pendingFrees BEFORE step (c) ran, so
		// AllocSlab installs fresh — the ordering this claim block
		// depends on.
		seg := segs[m]
		older := seg.OlderSegment
		if m > 0 {
			older = prevNewID
		}
		buf, err := p.AllocSlab(newID)
		if err != nil {
			return fmt.Errorf("pager: RPL relocation: alloc copy buffer: %w", err)
		}
		EncodeRPLSegment(buf, p.cfg, seg.TxnID, older, seg.PageIDs)

		// Retire the old page in THIS transaction (it rides the head
		// segment appendRPL builds next — free-space.md's
		// retire-in-head invariant), and re-home the in-memory ref.
		p.retiredPages = append(p.retiredPages, oldID)
		p.rplSegments[j+m].PageID = newID
		prevNewID = newID
	}
	return nil
}
