# Index write path has no background-maintenance or epoch-invalidation seam

Lands: when an index kind requiring asynchronous maintenance
(vector ANN split/merge, full-text corpus-stat recompute) is
designed

## Finding

The index write path is strictly synchronous per row
(`applyIndexMaintenanceOn*`), and handle invalidation marks
in-flight cursors at each same-transaction mutation — the model
assumes all index-tree mutation happens inside the write
operation that caused it. Two structural gaps for future kinds:

- Nothing can record or schedule a deferred maintenance
  obligation (a pending partition split, a stale stats head):
  there is no persisted "work owed" state and no background
  hook on the index layer (the maintenance daemon serves the
  pager, not indexes).
- Cursor invalidation is per-operation `MarkStale`; mutation
  from a background pass has no channel to invalidate readers —
  an epoch/generation check would be needed instead.

Additionally, `Check(CheckIndexes)` verifies an index by
re-running the extractor and comparing byte-equal entry sets —
sound only for kinds whose entries are a pure function of the
row. A centroid-dependent kind (vector) yields permanent false
drift under this model; verification for such kinds needs a
kind-specific checker.

## Provenance

2026-07-11 architecture audit (index-kind assumptions
inventory). The registry/decl format half of the same audit is
handled by `index-kind-format-groundwork` (plan chunk 7); this
issue is the write-path/invalidation/verification half,
deliberately out of that chunk's scope.
