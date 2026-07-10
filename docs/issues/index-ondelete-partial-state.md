# Index onDelete interleaves extraction with mutation: panic leaves committed partial state

Lands: 17

## Findings

**[M] Extractor panic mid-`onDelete` leaves committed-visible partial
index state (lost index entries) plus an unresolved shallow
savepoint.** `index_engine.go:132-156`: unlike `onReplace` (all
extraction completes in `buildReplacePlans` before any mutation),
`onDelete` runs extract→delete per index in lex order. The typed layer
deliberately panics from inside extractors (`typed_index.go:120-148`)
and user extractors can panic. A panic while extracting for index "b"
lands after index "a"'s entries were deleted; the caller-side
`restoreIndexes`/`RestoreSavepoint` (`keyspace.go:831-832`) never run.
A recovering caller (`defer recover()` around `ks.Delete`) then commits:
index "a" permanently misses the row's entries while the row exists —
false-negative lookups forever (only `Check(CheckIndexes)` flags it).
The leaked `BeginShallowSavepoint` also violates the pager's
"all savepoints resolved before Commit" assumption
(`internal/pager/commit.go:248-249`).

**[L] Cached handle `coverValue` flag not reconciled after a same-tx
`Rebuild` that changes the covering shape.** `index_rebuild.go:499-502`,
`index.go:442-453`, `typed_index.go:167-169`,
`keyspace_core.go:371-390`: reconcile re-points pinned state but never
recomputes `coverValue`; a stale cover-value handle then surfaces
false `ErrCorrupted: "covering: zero columns"` from a healthy database.

**[L] `schemaHash` doc grammar contradicts the code and spec.**
`index_types.go:125-130`: the grammar block shows `Name ||` un-prefixed
while the body (`:155`) and indexing.md:259-264 prescribe
`uvarint(len(Name)) || Name`. A reimplementation from the doc never
matches on-disk hashes.

## Fix direction

Restructure `onDelete` to extract-all-then-mutate (the `onReplace`
shape), making a panic escape before any index mutation — plus
panic-path savepoint resolution if any mutation phase can still panic.
Recompute `coverValue` in handle reconciliation; fix the doc grammar
block. Regression: recovering-caller panic test asserting
`Check(CheckIndexes)` clean post-commit.

## Provenance

2026-07-10 defect audit; indexing reviewer.
`TestTypedIndexEncodeErrorPanics` asserts only that the panic
propagates; nothing pins post-recovery/commit state.
