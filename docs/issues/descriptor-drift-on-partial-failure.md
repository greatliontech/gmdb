# Descriptor drift on storeDescriptor failure mid-Put/Delete/Cursor.Delete

**Lands:** chunk that adds tx-poison semantics (a focused
recovery/correctness chunk — likely 5.6 alongside DeleteKeyspace, or
the broader Tx.SetFileFormat-style recovery work). Conditioned on
"the engine gains a tx-poisoning mechanism for partial-mutation
failures."

## Problem

`Keyspace.Put` / `Keyspace.Delete` / `Cursor.Delete` (chunk 5.5) mutate
the keyspace's own data B+tree FIRST, then write the updated descriptor
back into the keyspace-B+tree via `tx.storeDescriptor`. The two writes
are not atomic at the API layer:

```go
// keyspace.go Put (sketch)
newRoot, err := btree.Put(ks.tx.pgr, cfg, ks.desc.Root, key, value)
if err != nil { return err }
ks.desc.Root = newRoot
if !existed { ks.desc.Count++ }
if err := ks.tx.storeDescriptor(ks.name.Value(), ks.desc); err != nil {
    return err   // ← in-memory ks.desc is advanced; the keyspace
                  //   B+tree still references the OLD descriptor.
}
```

If `storeDescriptor` fails (e.g. `ErrTxTooLarge` when the keyspace-
B+tree CoW trips the slab budget), the data B+tree carries the
mutation but the keyspace-B+tree's descriptor for this keyspace still
points at the OLD root with OLD count.

## Failure mode (concrete)

A user near the slab budget calls `ks.Put("k","v")`. `btree.Put`
succeeds — N pages CoW'd into the slab. The subsequent
`storeDescriptor` allocs one more page (the keyspace-B+tree CoW) and
trips ErrTxTooLarge.

- `ks.desc.Root` is advanced in memory, `ks.desc.Count++`.
- The persisted keyspace-B+tree's descriptor for "ks" still has the
  prior Root and Count.
- The just-CoW'd data-tree pages are in `pendingAllocs` (allocated
  this tx) but unreachable from the persisted descriptor.

The natural Go pattern is that errors don't auto-rollback; the user
chooses Commit vs Rollback. If the user catches the error and calls
`tx.Commit()`:

- pager.Commit writes `tx.keyspaceRoot` (the OLD keyspace-B+tree's
  root) to the new meta.
- The just-CoW'd data-tree pages get their bitmap bits cleared at
  commit (they were in pendingAllocs).
- Net result: those pages are **allocated on disk but unreachable
  from any descriptor** — orphan/leak that bitmap-tracked reachability
  invariants can't recover.

`Cursor.Delete` has the same shape: `c.inner.Delete` succeeds and
bumps the data-tree root + self-re-Seeks; `storeDescriptor` then
fails; the cursor is positioned in a tree the descriptor doesn't
reference. On commit, orphans.

## Mechanism (reachable in-spec path)

1. Tx with `MaxTxBufferBytes` set tight relative to the workload.
2. `ks.Put` triggers `btree.Put` that allocates N CoW destinations
   (leaf + ancestors).
3. `storeDescriptor` needs the (N+1)st alloc; slab budget exceeded.
4. User catches the returned error, decides to Commit (perhaps to
   preserve other unrelated work in the same tx — a legitimate
   choice).
5. Commit succeeds; meta advances; orphans on disk.

## Diagnosis

Anchor: cited reachable in-spec path above. Test pinning the failure
mode: synthesize a tx with `MaxTxBufferBytes` sized so a single Put's
last alloc trips budget; commit; verify either (a) the post-commit
on-disk state matches a known-good shape or (b) the path is poisoned
and Commit refuses.

Proximate: `keyspace.go` Put / Delete / Cursor.Delete update `ks.desc`
before persisting, with no rollback on `storeDescriptor` failure.

Root: missing tx-poison semantics — partial-mutation failures should
mark the tx as not-Commitable, mirroring the chunk-1 commit-pipeline
poison machinery that already exists for commit-publication failures.

## Fix options (none small)

1. **Tx-poison on partial-mutation failure.** Set `tx.poisoned` (or
   a similar flag) when `storeDescriptor` fails after a successful
   data-tree mutation. Commit returns `ErrPoisoned`. Rollback works
   normally. Requires API contract amend ("Put/Delete on slab
   exhaustion poison the tx") + plumb-through into Commit's gate.

2. **Compensating rollback in keyspace ops.** On `storeDescriptor`
   failure, re-issue `btree.Delete`/`btree.Put` to undo the data-tree
   mutation. Complex (the un-do op itself can fail; recursive
   failure handling).

3. **Pre-allocate descriptor write first.** Reserve the descriptor's
   CoW page before the data-tree mutation. Hard to size correctly
   because the keyspace-B+tree's depth determines how many pages
   the descriptor write CoWs.

4. **Atomic-commit-time descriptor flush.** Commit walks
   `tx.openKeyspaces` and writes each `ks.desc` unconditionally before
   pager.Commit. Failure during this walk poisons. Removes per-op
   `storeDescriptor` calls — descriptor lives only in memory until
   Commit. Smallest API surface change but reshapes the
   storeDescriptor lifecycle.

Recommend evaluating option 1 (tx-poison) as the cleanest; option 4
is the smallest-fix alternative.

## Notes

Surfaced by the chunk-5.5 Round 1 adversarial review (H1). The
chunk-5.4 `CreateKeyspace` path has the same shape — the bug is
latent there too but reaches it only via `storeDescriptor`, not
a preceding data-tree mutation that needs unwinding. So chunk-5.4's
exposure is one-write-fails-cleanly (storeDescriptor returns the
error before any other state changed); chunk-5.5 introduces the
two-write-no-atomicity shape.

`class=introduced` by the chunk-5.5 diff arbiter — the
two-write-with-drift pattern lives in chunk-5.5's new
`Keyspace.Put`/`Delete`/`Cursor.Delete`. The chunk-5.4
`CreateKeyspace` shape was single-write-no-drift.
