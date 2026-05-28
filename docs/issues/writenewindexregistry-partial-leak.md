# `writeNewIndexRegistry` partial-write leaks pages on Commit-after-error

**Status:** chunk-7.5 (`writeNewIndexRegistry`) **closed** by commit
`c1effd2`; chunk-7.6 / 7.9 per-row indexed maintenance **closed**
this session (`Pager.BeginShallowSavepoint` substrate). Remaining:
the 3 cold-path DDL siblings — `Tx.RebuildIndex`, `Tx.DropIndex`,
`retireIndexRegistry`.

**Lands:** the 3 DDL siblings remain. Apply the c1effd2
`BeginSavepoint` / `RestoreSavepoint(on error)` /
`ReleaseSavepoint(on success)` pattern at each cold-path DDL site
(nested savepoint kind is fine for these — they're one-shot per DDL
op, not per-row, so loose-pop suspension is not a concern). Each
needs a failure-injection regression test using the
`atomic.Pointer[func()]` seam idiom.

## Problem

Chunk-7.3's `writeNewIndexRegistry` (`index_open.go`) calls
`registryPut` once per declared `IndexDecl`. Each call allocates
pages via the pager and threads the new registry-tree root through
`desc.IndexRegistryRoot`. If iteration `k` succeeds and iteration
`k+1` fails (e.g. pager `ErrTxTooLarge` on a tight slab budget +
many decls), the chunk-7.5 callers (`CreateKeyspace`,
`CreateKeyspaceIfNotExists`, `CreateSetKeyspace`,
`CreateSetKeyspaceIfNotExists`) roll back the in-memory cache:

```go
delete(tx.openKeyspaces, handle)
tx.numKeyspaces--
if pendingDelete { tx.pendingDeletes[name] = struct{}{} }
return nil, err
```

The `k` pages allocated by iterations 0..k-1 are reachable through
the no-longer-cached *Keyspace's `desc.IndexRegistryRoot`. With no
cached handle, no flush walk visits those pages — they become loose
pages.

- On `Tx.Rollback()`: the pager's `AbortTx` reclaims everything via
  the bitmap snapshot. Clean.
- On `Tx.Commit()` (the "rest-of-tx-continues" contract acknowledged
  by the caller's error handling — the caller catches `err`,
  continues other work, then commits): the loose pages have no
  referrer in any committed tree → orphaned for the lifetime of the
  file until checkpoint / GC reclaims them.

The chunk-7.3 godoc on the rollback path explicitly opts into
"rest-of-tx-continues" semantics. Whether that's the right contract
across the engine (vs. "all-or-nothing per write helper") is a
design question outside chunk-7.5 scope.

## Repro

Tight `MaxTxBufferBytes`, many `IndexDecl`s, force the
second-or-later `registryPut` to fail. Compare `tx.Stats().FreePages`
pre-Begin vs post-Commit: should be unchanged on the
all-or-nothing contract; will differ by `k * pages-per-registry-leaf`
under current code.

## Acceptance

Either:

1. **Make `writeNewIndexRegistry` atomic.** Build the full sub-tree
   in memory (or against a savepoint), install the final root in
   `desc.IndexRegistryRoot` via a single descriptor mutation; on
   error, free the in-flight pages explicitly. Increases
   implementation complexity but holds the "all in-flight
   allocations land in committed tree OR returned to free space"
   spec invariant uniformly.

2. **Canonicalize the "rest-of-tx-continues" contract spec-side.**
   Add a clause to `transactions.md` defining when a per-op error
   may leave loose pages reachable only via uncommitted in-memory
   state, and what `Tx.Commit` after such an error is expected to
   do (silently commit with leaks reclaimable via maintenance? fail
   loudly?). Then chunk-11 `Check()` audits + chunk-12 background
   maintenance reclaims.

The decision is upstream of chunk-7; chunk-7.5 takes the existing
"rest-of-tx-continues" contract as given. This issue tracks the
revisit.

When this issue closes, the load-bearing rationale moves inline
into `transactions.md` (the spec'd contract) or the
`writeNewIndexRegistry` implementation (the atomic variant) and
this file is deleted per the no-cite invariant.

## Notes

Surfaced by the chunk-7.5 Round-1 adversarial review (M-3). The
chunk-7.3 H-1 fix (descriptorOwner interface) is unrelated — that
fix closed the silent-data-loss on Clean-keyspace mutations. This
issue is specifically about leak-on-error in the bulk-registry-
write path that chunk-7.5 introduced.

Adjacent: any future write helper that loops over multiple
btree.Put calls inside one Tx will have the same shape (chunk-7.8
RebuildIndex's cursor-walk-then-write loop is the natural next
case).

## Chunk-7.6 / 7.9 extension: per-row index maintenance — RESOLVED

The per-row case is closed by the `Pager.BeginShallowSavepoint`
substrate. Resolution shape:

1. `Pager.BeginShallowSavepoint` (internal/pager/savepoint.go) is a
   new savepoint kind that, unlike nested savepoints, does NOT
   suspend loose-page reuse — so wrapping each indexed Put/Delete
   per-row preserves the across-Put loose-page recycling the file
   needs to stay bounded under bulk indexed workloads.
2. The 6 caller sites — Keyspace.Put / Delete / Cursor.Delete and
   SetKeyspace.Put / Delete (bulk-key) / DeleteValue — bracket the
   `applyIndexMaintenanceOn*` call AND the subsequent row btree
   mutation in `BeginShallowSavepoint` / `ReleaseSavepoint(success)`
   / `RestoreSavepoint(error)` pairs.
3. Each loose-pop AllocPage performs during a shallow savepoint
   window appends a `(id, original-buffer)` entry to the
   savepoint's loose-pop log; `RestoreSavepoint` replays the log to
   re-attach the original buffer to `p.dirty[id]`, faithfully
   reversing the detach.

Regression coverage:

- `internal/pager/savepoint_test.go`:
  `TestShallowSavepointPreservesLoosePop`,
  `TestShallowSavepointRestoreReversesLoosePop`,
  `TestShallowSavepointReleaseKeepsLoosePop`,
  `TestShallowSavepointDoesNotSuspendNestedInv`,
  `TestShallowSavepointOutOfOrderPanics`.
- gmdb-level: `TestApplyIndexMaintenanceAtomicOn{Keyspace,SetKeyspace}-
  {Put,Delete,CursorDelete,DeleteValue,BulkKeyDelete}` in
  `index_maintain_test.go` / `index_setkeyspace_test.go`.

Cost trade-off: the residual per-savepoint clone of
`pendingAllocs`/`Frees`/`loosePages`/`dirtyKeys` is O(this-tx-state-
at-Begin), N²-in-#mutations across N per-row savepoints in one tx.
Benign for OLTP-N (≤ 100); profiling-driven follow-up tracked as
`shallow-savepoint-clone-cost.md`.

The chunk-7.6 H-2 in-memory `pinnedIndex` snapshot/restore stays —
it covers the `flushIndexRegistry` registry-consistency layer; the
new shallow savepoint covers the on-disk page-allocation layer.
Both layers are required.

## Chunk-7.8 extension: DeleteKeyspace three-subtree retirement +
RebuildIndex / DropIndex mid-flight failures

Chunk-7.8 adds three new partial-failure shapes under the same
rest-of-tx-continues contract:

1. `Keyspace.DeleteKeyspace` calls `btree.FreeSubtree(desc.Root)`
   for the data tree, then `retireIndexRegistry` which walks the
   registry sub-tree freeing each entry's Root, then frees the
   registry root itself. A failure inside `retireIndexRegistry`
   (e.g. `decodeRegistryEntry` on a corrupted entry) leaves the
   data subtree already freed but the registry partially freed.
   `Tx.Rollback` recovers via the bitmap snapshot; Commit-after-
   error orphans the partial state.

2. `Tx.RebuildIndex` builds the new index data tree via N
   `btree.Put` calls. A failure mid-build leaks the partial new
   tree. Publish-then-retire ordering (chunk-7.8 H-2 fix) ensures
   the registry never points at freed pages, so Commit-after-error
   leaks the partial new tree (no corruption).

3. `Tx.DropIndex` does `registryDelete` then `FreeSubtree(root)`.
   A failure between the two leaks the data tree but keeps the
   registry consistent.

All three shapes follow the same canonicalization path this issue
tracks. When the rest-of-tx-continues contract is replaced by an
all-or-nothing per-helper guarantee, all three benefit
automatically.

## Chunk-11.4 triage update (redefer, not resolved)

Chunk 11 makes the *consequence* recoverable without fixing the
*root*: `Check()` reports the orphaned pages as `BitmapLeak`
(11.2) and `CheckWithOptions(Repair)` reclaims them under
exclusive access (11.4). So a partial-failure leak is now both
detectable and offline-reclaimable. The root — the
rest-of-tx-continues contract that lets `Tx.Commit` keep the
orphaned pages in the first place — is unchanged. Redeferred to
the original trigger (error-contract canonicalization /
loose-pages-on-error policy); the Repair path is the interim
recovery, not the fix.
