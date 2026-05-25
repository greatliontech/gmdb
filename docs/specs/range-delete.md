# Range Delete

`Keyspace.DeleteRange(start, end)` deletes all keys in `[start, end)`
in a single operation. On un-indexed keyspaces this is significantly
more efficient than cursor iteration because it retires entire
subtrees without visiting individual leaves. On indexed keyspaces it
falls back to a per-row walk — see Bulk Operations below and
`indexing.md §Bulk Operations on Indexed Keyspaces`.

Scope:
- Three-phase range-delete algorithm on un-indexed keyspaces.
- Complexity comparison vs. cursor loop.
- Set-keyspace bulk-free interaction.
- Cursor-based delete loop for callers that need finer control.

Depends on / interacts with:
- `page-formats.md` for branch and leaf structure.
- `free-space.md` for the retire-pages-to-RPL hook used at the end
  of each phase.
- `set-keyspace.md` for nested-tree bulk free.
- `indexing.md` for the per-row-walk fallback contract on indexed
  keyspaces.
- `transactions.md` for cursor stability across CoW + rebalance.

## Invariants

Invariant: kind=clause-explicit;
  property=`DeleteRange(start, end)` deletes every key `k` with
    `start <= k < end` from the keyspace and zero keys outside that
    interval; `end` itself is never deleted. Passing
    `(nil, nil)` deletes every key in the keyspace. `nil` is the
    open-boundary sentinel: `nil` start = "from the beginning";
    `nil` end = "through the last key". A non-nil zero-length
    boundary (`[]byte{}`) is rejected with `ErrKeyEmpty` per
    api-surface.md §Invariants empty-key clause — distinct from
    the open-boundary `nil`;
  from=this spec §Algorithm + API contract + api-surface.md
    §Keyspace API DeleteRange;
  violation=Off-by-one in the boundary-leaf cleanup deletes `end`
    (inclusive bug) or leaves the first key `>= start` alive
    (exclusive bug) — silent data loss or silent retention of data
    the caller said to delete; OR coalescing `[]byte{}` with `nil`
    silently accepts an invalid boundary the spec rejects.

Invariant: kind=entailed;
  property=After `DeleteRange` commits, every overflow run referenced
    by any deleted entry is retired (its page IDs appear in
    `tx.retiredPages`). No overflow page survives a delete of its
    only referencing leaf entry;
  from=entailed: tree-integrity + free-space accounting
    (`page-formats.md` + `free-space.md`);
  violation=Orphan overflow runs become permanent bitmap leakage
    that background maintenance cannot reclaim until the
    leak-reclamation pass identifies them — for large value
    workloads this is unbounded leakage in practice.

Invariant: kind=entailed;
  property=A successful `DeleteRange` leaves the keyspace's B+tree
    well-formed: branch separators still satisfy
    `max(left) < S <= min(right)`, no branch has fewer than the
    minimum children except via root collapse, and the keyspace
    descriptor's `Root` points to the new root (or `0` for an
    emptied keyspace);
  from=entailed: tree invariants of `page-formats.md` + this spec
    §Phase 3 root collapse;
  violation=A separator that no longer satisfies the ordering
    invariant after boundary rebalance routes subsequent Get/Cursor
    operations to the wrong subtree; a forgotten root-collapse leaves
    an empty branch with one child, breaking the depth-derivation
    convention.

Invariant: kind=entailed;
  property=On an indexed keyspace, `DeleteRange` deletes every
    secondary-index entry that the engine's extractor would produce
    for any deleted row — exactly the same set of index entries a
    per-row `Delete()` loop would remove;
  from=entailed: per-row walk fallback (this spec + `indexing.md`);
  violation=A subtree-retirement shortcut on indexed keyspaces drops
    a leaf's index-bearing entries without removing the index rows
    — the index returns stale primary keys that point at deleted
    rows.

## Algorithm (un-indexed keyspaces)

Three phases.

### Phase 1 — Find boundary paths

Descend the B+tree twice to find the left and right boundary paths.
A path is a stack of `(pageID, index)` pairs from root to leaf.

### Phase 2 — Identify and retire interior subtrees

Walk up from the two boundary paths to find their lowest common
ancestor (LCA). At each level between LCA and leaves:

- **Interior children** (between left and right boundaries) lie
  entirely within the range — their entire subtrees are retired
  without visiting individual leaves.
- **Boundary children** are partially within the range and must be
  descended.

Retiring a subtree walks the branch pages recursively. For each page
encountered, add its page ID to `tx.retiredPages`. For leaf pages,
accumulate the entry count for the return value. For overflow pages
referenced by leaf cells, retire the entire overflow run. The walk
visits every page in the subtree exactly once — `O(pages in
subtree)`.

### Phase 3 — Clean up boundary leaves and rebalance

- In the left boundary leaf: delete entries from `start` through end
  of leaf.
- In the right boundary leaf: delete entries from start through last
  key before `end`.
- If both boundaries are in the same leaf, delete entries between
  them.
- Retire any overflow pages referenced by deleted entries.
- Walk up from boundary leaves to LCA, removing the retired interior
  child pointers from each branch (CoW each branch).
- Rebalance: check fill ratios on modified branches and leaves. Merge
  or redistribute per `MergeThreshold` (see Options in
  `api-surface.md`).

**Root collapse.** If rebalance reduces the keyspace's root to a
single child (a branch with one child pointer and no separators),
retire the root and promote the surviving child to the new root —
update the keyspace descriptor's `Root` field. If `DeleteRange`
emptied the keyspace entirely, retire the root and set `Root = 0`
(empty keyspace). The descriptor update is part of the same write
transaction and propagates up through the keyspace B+tree via CoW.

## Complexity

| Operation | Naive (cursor loop) | Range delete |
|-----------|---------------------|--------------|
| Delete N keys spanning P pages | O(N × depth) | O(P + depth²) |
| CoW'd pages | O(N × depth) | O(depth²) |
| Retired pages | N leaf cells + splits | P pages (bulk) + boundary cleanup |

Worked example: for 1M keys across 10K leaves at depth 4, a naive
loop CoWs ~4M pages; range delete walks ~10K pages and CoWs ~16 on
boundary paths.

## Set Keyspace Bulk Free

Deleting a key in a SetKeyspace whose values are in a nested B+tree
frees the nested tree via the same subtree retirement: read root +
count from the cell, walk the nested tree recursively retiring every
page, remove the cell. `O(pages in nested tree)`, not `O(values)`.
See `set-keyspace.md §Bulk Free`.

If the SetKeyspace has indexes declared, this falls back to a
per-member walk — same reasoning as `DeleteRange` on indexed
keyspaces.

## Indexed-keyspace fallback

`DeleteRange` on an indexed keyspace does NOT use the O(pages)
subtree-retirement fast path. The engine cannot retire a subtree
without knowing the prior-index-keys for every row in it (the
extractor output depends on the row's value, which the subtree-
retirement walk does not visit).

Implementation: the engine iterates the range with a cursor, calling
`Delete()` for each row. Cost is `O(entries × (indexes +
extractor))`. The cursor must remain stable across the CoW +
rebalance triggered by per-row deletes — `Cursor.Delete()` advances
the cursor to the post-delete successor in-place, so the canonical
drain pattern reads it via `Cursor.Current()` (NOT `Cursor.Next()`,
which would skip the successor). See `transactions.md §Cursor State
Machine` for the full pattern, and `§Cursor-Based Range Delete`
below for the worked example.

This is the same cost a SQL engine pays for `DELETE … WHERE … IN
range` with secondary indexes. Predictable and correct.

**Partial-progress on error (chunk-7.10 spec amendment).** Unlike
non-indexed `Keyspace.DeleteRange` — whose underlying
`btree.DeleteRange` is atomic and returns `(0, err)` with no
visible state change on failure — the indexed-keyspace cursor-
walk is per-row atomic, not per-call atomic. On a per-row failure
at iteration `i`, iterations `0..i-1` have already completed: each
of those rows is removed from the parent keyspace AND each of
their index entries has been cleared via the chunk-7.6 atomic
`Cursor.Delete` maintenance. `DeleteRange` returns
`(deleted_so_far, err)` so the caller sees the real scope of
state change. Each successful per-row delete satisfies the
chunk-5.1 keyed-removal invariants and the chunk-7.1 atomic-Put/
Delete invariant individually; the in-memory + on-disk state is
consistent-but-partial. The only safe recovery is `Tx.Rollback()`
(which restores via the pager bitmap snapshot per
`pager-slab.md`). This matches the chunk-6.8
`SetKeyspace.DeleteRange` partial-progress contract.

Callers needing the O(pages) fast path on indexed data can:

- Drop the indexes before the bulk operation, run `DeleteRange`,
  then rebuild the indexes (`tx.RebuildIndex`).
- Or use `DeleteKeyspace` to drop the whole keyspace (which also
  drops its indexes — the engine cleans up internal index keyspaces
  and the per-keyspace index registry).

## Cursor-Based Range Delete

For callers needing finer control:

```go
c := ks.Cursor()
for k, _ := c.SeekGE(start); k != nil && bytes.Compare(k, end) < 0; k, _ = c.Current() {
    c.Delete()
}
```

Note the use of `c.Current()` (not `c.Next()`) inside the loop:
`Cursor.Delete()` already advances the cursor to the post-delete
successor per `transactions.md §Cursor.Delete() post-delete state`,
so `Current()` reads it directly. `Next()` would step PAST the
successor and skip alternating entries. (Chunk-7.10 spec
amendment — earlier revisions of this example used `Next()`.)

One-at-a-time path. `DeleteRange` should be preferred for contiguous
unconditional deletes.
