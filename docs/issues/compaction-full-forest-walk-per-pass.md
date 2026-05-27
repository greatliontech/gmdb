# Incremental compaction re-walks the whole forest every pass

**Lands:** profiling-driven — when a large database shows incremental
compaction's per-pass read cost is material, or opportunistically.

## Problem

`maintCompact` → `compactionPass` → `compactForest` walks **every**
B+tree in the forest each pass (`btree.RelocatePages` on every keyspace
data tree, index registry, and index data tree). `RelocatePages` reads
every page of each tree to evaluate `shouldRelocate(id)` and recurse, so
a pass is O(total live pages) in reads even though it relocates at most
`CompactionBatchSize` pages. For a large database this re-reads the
whole forest every maintenance interval to find the few high-watermark
pages worth moving.

## Why deferred (design decision at 12.5b-3b)

The 12.5b-3 plan sketched a "resumable keyspace cursor" for fairness.
On analysis it was dropped: with high-watermark evacuation the budget
flows to wherever the high pages are regardless of keyspace order (a
keyspace with no above-floor pages costs only a read-walk, consumes no
budget), and a relocated page receives a low id so it stops matching the
floor — so there is no starvation/fairness problem to solve, and a
keyspace cursor would not even map cleanly to page-id locality (high
pages are not keyspace-ordered). Correctness and convergence hold
without it; the only cost is the redundant per-pass read-walk, which is
an efficiency concern, not a correctness one, and is bounded + amortised
across the (default 5-minute) maintenance interval.

## Related: compaction self-signals the fragmentation trigger

`relocateOverflowChain` issues `AllocContiguous(runLen>1)` per relocated
overflow chain, which bumps the same `contigAttempts` / `contigFragFails`
counters the trigger reads. `maintCompact` consumes the stats at pass
*start*, so this pass's relocation allocs land in the *next* pass's
window; while the forest is still fragmented those allocs can miss a
contiguous run and inflate the rate, keeping compaction triggered for an
extra pass or two. Self-limiting (once eager reclaim consolidates,
`FindContiguous` succeeds and the rate falls) and arguably desirable
(more compaction is wanted while fragmentation persists), so not a
correctness defect — but the trigger metric conflates user-driven and
maintenance-driven fragmentation. A fix would exclude maintenance-internal
`AllocContiguous` from the counters (e.g. a per-tx "don't count" flag).
Surfaced by the 12.5b-3b review (L-2).

## Resolution options

1. **Id-range pruning in the walk:** skip a subtree whose pages are all
   below the evacuation floor — needs per-branch min/max child-id
   tracking the page format does not carry today.
2. **Bitmap-driven targeting:** scan the allocation bitmap for the
   high allocated pages directly, then map them to owning trees —
   closer to the rejected "fragmentation-region" strategy.
3. **Leave as-is** if profiling shows the per-pass walk never dominates
   — close as obsolete.

Surfaced at chunk 12.5b-3b (cursor omitted vs the plan sketch; tracked
per "no silent downscoping").
