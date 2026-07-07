# Online compaction does not relocate RPL segment pages

**Lands:** condition — when fragmentation profiling shows RPL segment
pages materially block contiguous-run consolidation (an evacuation region
stays pinned by an RPL page across many maintenance passes), or when RPL
relocation is folded into the commit pipeline.

## Problem

Online incremental compaction (v0 chunk 12.5b) relocates B+tree nodes
(`btree.RelocatePages`, 12.5b-1) and overflow chains (12.5b-2), but **not
RPL segment pages**. An RPL segment page sitting in an evacuation region
cannot be moved, so that region cannot be fully cleared into a contiguous
free run while the segment lives there.

## Why deferred (user-approved at v0 12.5b-2)

RPL segment pages are owned and managed by the **commit pipeline**:
`commitStep0`→`appendRPL` allocates them via `AllocPage`, links them via
each segment's `OlderSegment` pointer, records the head in
`meta.RPLHeadPage`, and `reclaimRPL` retires them tail-first as readers
advance. Relocating a segment out-of-band would have to rewrite its
referent (the meta head, or the newer segment's `OlderSegment` — which
cascades through the chain) and the pager's in-memory `rplSegments`
slice, racing the machinery that reassembles the RPL at every commit.
That is a high corruption-risk surface for low value:

- RPL segment pages are **transient** — they drain via `reclaimRPL` as
  the reclamation bound advances, so an RPL page in a target region
  clears on its own.
- **New** RPL segments (including those compaction itself creates by
  retiring relocated originals) are allocated fresh each commit, so a
  consolidating allocator already self-places them low.

So the v0 12.5b-3 orchestration treats RPL segment pages as **immovable**:
its evacuation predicate excludes them. A region pinned by an RPL page is
a self-resolving gap, not a permanent one.

## Resolution options

1. **Fold RPL relocation into the commit pipeline**, where the RPL is
   already rebuilt — the only place it can be mutated without racing
   `appendRPL`/`reclaimRPL`. Likely the correct home if relocation is ever
   warranted.
2. **Leave as-is** (orchestration skips RPL-pinned pages) if profiling
   shows RPL pages never materially block consolidation — close as
   obsolete.

Surfaced during v0 chunk-12.5b-2 implementation (the commit-pipeline
entanglement was discovered reading `appendRPL`); deferral approved by
the user.
