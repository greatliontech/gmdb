# `SetKeyspace.DeleteRange` per-key Delete loop pays O(K log N) descents

**Lands:** opportunistic — when profiling shows `DeleteRange`-heavy
workloads on Kind=1 keyspaces are bottlenecked by the per-key
descent cost, OR when a chunk-7 indexed-DeleteRange variant is
written that needs the same bulk-walker shape.

## Problem

Chunk-6.8's `SetKeyspace.DeleteRange` uses a snapshot-then-Delete
strategy: walk the parent tree via a read cursor to materialize
the key list in `[start, end)`, then call `SetKeyspace.Delete(k)`
for each key. Each `Delete` runs a full descent + CoW + parent-
cell removal — O(log N) per key.

For a range covering K keys in a parent tree of size N, the total
cost is O(K log N), versus the chunk-5.7 `btree.DeleteRange`
three-phase algorithm's O(K + log N) (one tree walk, in-place
leaf rebuilds where possible).

The chunk-5.7 algorithm is not directly reusable because it
does not free nested-tree subtrees per cell — for a SetKeyspace
with nested-tree-promoted keys in the range, naive use would
leak pages.

## Proposed remediation

Adapt the chunk-5.7 three-phase algorithm with a per-cell
free callback. Walk parent leaves in [start, end); for each
entry to be removed:

- Subpage cell: no extra work (the inline subpage bytes go
  away with the leaf entry).
- Nested-tree cell: call `FreeSubtree(NestedRoot)` BEFORE the
  entry is removed.
- Plain cell (overflow): existing `freeOverflowChainIfPresent`.

The callback shape:

```go
// PerCellFreeFn is called for each entry to be removed by
// btree.DeleteRange's phase-2 walker BEFORE the entry is
// deleted. Returns the value count to add to the rows-deleted
// accumulator.
type PerCellFreeFn func(pw PageWriter, cfg Config, e page.LeafEntry) (uint64, error)
```

`Keyspace.DeleteRange` passes a no-op callback (Kind=0 entries
contribute 1 each to the count, no per-cell freeing beyond what
chunk-5.7 already does). `SetKeyspace.DeleteRange` passes a
callback that handles subpage / nested-tree counts + bulk-free.

## Acceptance

When this lands, the v1 snapshot-then-Delete shape in
`set_keyspace.go:DeleteRange` is replaced by a single call to
`btree.DeleteRange(... withCellCallback)`. The behavior should
be byte-identical (same desc.Count delta, same on-disk state) —
verifiable via the existing chunk-6.8 tests.

## Notes

Surfaced by chunk-6.8 implementation. Not a defect today — the
v1 implementation is correct, just slower for large ranges. The
plan note "Reuses chunk-5.7 three-phase DeleteRange machinery
where possible" is partially satisfied by v1 (same general
approach, walks the parent tree once for the snapshot) and fully
addressed by this follow-up.
