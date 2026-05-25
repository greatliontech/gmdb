# SetKeyspace indexing: snapshot redundancy + zero-column edge

**Lands:** profiling-driven (items A + B are perf-only; remaining
in scope at chunk 7.10's redefer).

**Status:** Chunk 7.10 folded item C (zero-column IndexDecls now
rejected at `validateIndexDecls`); items A + B remain open under
the profiling-driven trigger above.

## Problems

### A) Double snapshot in indexed SetKeyspace.Put / DeleteValue

`SetKeyspace.Put` (set_keyspace.go) snapshots `ks.indexes` at the
top of the indexed-path block, then calls
`applyIndexMaintenanceOnAddValue` which ALSO snapshots internally
(via the wrapper at `index_setkeyspace.go:applyIndexMaintenanceOnAddValue`).
On failure, both restore the same pre-call state — idempotent but
wasteful. Same shape in `SetKeyspace.DeleteValue`.

Same shape exists for chunk-7.6 Keyspace.Put / Delete /
Cursor.Delete vs chunk-7.6's `applyIndexMaintenanceOnPut/OnDelete`
wrappers. Pre-existing-adjacent for the SetKeyspace mirror.

### B) Per-value snapshot in indexed bulk-key Delete

`SetKeyspace.Delete` on an indexed keyspace calls
`applyIndexMaintenanceOnBulkKeyDelete` which loops over set
members calling `applyIndexMaintenanceOnRemoveValue` (the
wrapper). Each loop iteration snapshots `ks.indexes` afresh,
even though the outer Delete already snapshotted at top. For an
N-member set, N snapshot allocations are performed but only the
outer-snapshot's restore is load-bearing.

Fix shape: call the inner `applyIndexMaintenanceOnRemoveValueInner`
from the bulk loop (skipping the per-call snapshot/restore) and
rely on the top-level snapshot.

### C) Zero-column IndexDecls rejected at decode time

`extractSetKeyspaceCompoundPKFromIndexKey`
(`index_setkeyspace.go:extractSetKeyspaceCompoundPKFromIndexKey`)
walks the index key counting REAL `0x00 0x00` terminators. For a
zero-column IndexDecl (Cols: nil), the very first terminator is
both the column-tuple terminator AND the PK terminator — the
function returns `errCompoundPKMalformed` with "extra terminator
before PK."

`validateIndexDecls` (`index_types.go`) does NOT reject zero-
column decls. The chunk-7.7 Keyspace non-unique decoder
(`index.go:extractPKAndValue`) also rejects len(cols)==0 with
ErrCorrupted. Both are silent corruption paths if a future
caller constructs a zero-column IndexDecl.

Fix shape: add `len(decl.Columns) == 0 → ErrInvalidOptions` to
`validateIndexDecls`, OR document that zero-column indexes are
unsupported and add a guard at IndexDecl construction.

## Acceptance

Both A) and B) are perf-only — no correctness defect. C) is
defense-in-depth against a future misuse.

When the issue closes, the load-bearing rationale moves inline
into the relevant helper godoc and this file is deleted per the
no-cite invariant.

## Notes

Surfaced by chunk-7.9 Round-1 adversarial review (L-1, L-2, M-3).
A is symmetric with the chunk-7.6 Keyspace pattern (pre-existing
adjacent). B is unique to chunk-7.9's bulk-key delete. C is a
pre-existing gap that chunk-7.9 noticed via the new SetKeyspace
decoder path.
