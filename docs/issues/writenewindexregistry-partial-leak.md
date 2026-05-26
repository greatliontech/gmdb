# `writeNewIndexRegistry` partial-write leaks pages on Commit-after-error

**Lands:** when the engine's "rest-of-tx-continues after a per-op
error" contract is canonicalized (chunk 11 free-space recovery, or
whenever the loose-pages-on-error policy is reconsidered for write
helpers that fail mid-loop). Or earlier if benchmarking shows the
leakage is material at scale.

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

## Chunk-7.6 extension: per-row index maintenance

Chunk-7.6's `applyIndexMaintenanceOnPut` /
`applyIndexMaintenanceOnDelete` are per-row analogues. Each loops
over declared indexes calling `btree.Put` / `btree.Delete` on the
index data trees. The chunk-7.6 H-2 fix snapshots `pinnedIndex`
(root, count) at the start and reverts on any failure — so the
in-memory pinned state at flushIndexRegistry time always reflects
either pre-call or post-success state, never partial. This keeps
the registry consistent.

The on-disk **pages** allocated by the partially-successful loop
iterations are still loose under the same rest-of-tx-continues
contract this issue tracks: on `Tx.Rollback` the pager's AbortTx
reclaims; on `Tx.Commit` they orphan. Chunk-7.6 inherits the leak
shape but does NOT add a new drift mode (the H-2 revert closes the
shape-2 silent-drift path the chunk-7.6 Round-1 reviewer surfaced).

When this issue resolves with the atomic variant, the chunk-7.6
helpers benefit identically — the snapshot/revert layer can drop
once the underlying contract guarantees all-or-nothing.

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
