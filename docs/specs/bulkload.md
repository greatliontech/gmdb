# BulkLoad

`BulkLoad` constructs a keyspace's B+tree bottom-up from a sorted
input stream, bypassing the per-key insert path entirely. Targets
two concrete scenarios:

- **gitfs**: one-shot migration of SQLite tables into gmdb at first
  open.
- **notes**: initial import of a corpus from filesystem dumps.

Scope:
- API contract (Keyspace and SetKeyspace variants).
- Bottom-up algorithm with per-level in-progress pages.
- Slab bypass (streaming pwrite to fresh page IDs).
- Interaction with indexes (sort + spill).
- Unique-violation detection at the merge output, including the
  bounded-leakage rule for mid-load aborts.
- Atomicity.

Depends on / interacts with:
- `pager-slab.md` for the slab-bypass rationale and the
  bounded-leakage property of pwrites to fresh page IDs not yet
  referenced by any meta.
- `indexing.md` for the rebuild and uniqueness contracts.
- `background-maintenance.md` for bitmap-leak reclamation that
  cleans up after a mid-load abort.
- `api-surface.md` for the Go-level signatures and the
  `ScratchDir` option.

## Invariants

Invariant: kind=clause-explicit;
  property=`BulkLoad` requires the target keyspace to be empty
    (`Count == 0`); otherwise it returns `ErrBulkLoadNonEmpty`
    without writing anything;
  from=this spec §API;
  violation=Bulk-loading into a non-empty keyspace mixes
    bottom-up-constructed leaves with pre-existing top-down ones
    — duplicate keys, broken prefix-truncated separators, and
    incoherent count.

Invariant: kind=clause-explicit;
  property=`BulkLoad` input MUST be in strictly ascending lex
    key order. A non-ascending key returns
    `ErrBulkLoadOutOfOrder` and aborts the load;
  from=this spec §API;
  violation=Out-of-order input produces a tree whose leaves are
    not lex-sorted — every subsequent `Get` and range query
    silently returns wrong results.

Invariant: kind=clause-explicit;
  property=Bulk-loaded pages are pwritten to fresh page IDs as
    they are constructed. The new keyspace root is published
    only on `tx.Commit()` via the meta swap; until then the
    new pages are unreferenced by any meta the engine can
    recover to;
  from=this spec §Slab Bypass + §Atomicity;
  violation=Referencing the new pages from any reachable meta
    before commit lets a crash or rollback expose the partial
    tree as the active state — corruption.

Invariant: kind=entailed;
  property=A mid-`BulkLoad` crash or unique-violation abort
    leaves pwritten pages as bounded leakage: they are
    unreferenced (no meta points to them) and are reclaimed by
    background maintenance's bitmap-leak reclamation pass. The
    on-disk state is consistent with the pre-`BulkLoad` meta;
  from=entailed: §Atomicity + `background-maintenance.md`;
  violation=A mid-load abort that leaves pwritten pages
    *reachable* breaks the "no partial writes visible" property
    — the next opener sees a partial tree.

Invariant: kind=entailed;
  property=For an indexed keyspace, the engine runs the extractor
    on every row and writes index entries to fresh internal
    index keyspaces using the same bottom-up algorithm.
    Unique-index violations are detected at the merge-sort
    output and abort the entire `BulkLoad` with
    `ErrIndexUniqueViolation` — no row or index pages become
    reachable;
  from=entailed: §Interaction with Indexes;
  violation=Allowing a partial commit (some indexes populated,
    others rolled back) breaks the atomicity contract user
    code relies on for migrations.

## API

```go
// BulkLoad replaces the contents of an empty keyspace with the
// sorted key-value stream produced by yield. Input MUST be in
// strictly ascending lex key order; a non-ascending key returns
// ErrBulkLoadOutOfOrder.
//
// The keyspace must be empty (Count == 0); otherwise returns
// ErrBulkLoadNonEmpty. Use ks.DeleteRange(nil, nil) to clear first
// if necessary.
//
// For indexed keyspaces, BulkLoad runs the index extractor on every
// row and bulk-loads each index in parallel using the same bottom-up
// algorithm. Indexes are written to fresh index keyspace roots; the
// existing index keyspace data is retired at commit. Unique-index
// violations abort the BulkLoad with ErrIndexUniqueViolation; nothing
// is written to disk before the abort because all bulk-loaded pages
// are at fresh page IDs invisible until the meta swap.
//
// For SetKeyspaces, input is a stream of (key, value) pairs in
// (key, value) lex order; duplicate (key, value) pairs are silently
// deduplicated.
//
// BulkLoad bypasses the per-txn slab budget: pages are pwritten
// directly to fresh page IDs as they are constructed, not buffered.
// Memory usage is O(depth × pageSize), independent of input size.
//
// Returns the number of input pairs written.
func (ks *Keyspace) BulkLoad(yield func(yield func(key, value []byte) bool)) (uint64, error)
func (ks *SetKeyspace) BulkLoad(yield func(yield func(key, value []byte) bool)) (uint64, error)
```

## Algorithm

Standard bottom-up B+tree construction:

1. Allocate a fresh leaf page from `pageAlloc()`. Fill with input
   entries (prefix-compressed with the keyspace's
   `RestartGroupTarget`) until the page is full or the input is
   exhausted.
2. When a leaf page is full, pwrite it directly to its allocated
   page ID (slab bypass — see below), free the page-sized
   scratch buffer back to the buffer pool, and start a new leaf
   page.
3. Each completed leaf contributes one
   `(separator, pageID)` pair to the in-progress branch page at
   the level above.
4. Recurse: when a branch page is full, write it directly and
   start a new branch at that level.
5. When input is exhausted, finalise all in-progress branches up
   to the root.
6. Set the keyspace descriptor's `Root` to the final root page ID.
   Increment `Count` by the input count.
7. At commit, the keyspace's old pages (the empty tree's root or
   a prior population) are retired and the meta page is swapped.

For each level of the tree there is exactly one in-progress page
at a time. Memory is `O(depth × pageSize)` — for a depth-5 tree
at 4 KB pages, 20 KB.

## Slab Bypass

Bulk-loaded pages are written to disk as they are completed, not
held in the slab. The pwrite goes to a fresh page ID — invisible
until the meta swap commits the new tree, so the partial write is
safe (a crash before commit leaves the pages as unreferenced
"leaked" pages in the bitmap, reclaimed by the next maintenance
pass exactly like any other crash leakage).

This bypass keeps memory usage flat regardless of input size and
makes `BulkLoad` the recommended path for inputs that would
otherwise exceed `MaxTxBufferBytes`.

## Interaction with Indexes

For an indexed keyspace, the engine runs the extractor on every
row and accumulates index entries per index. Each index's
entries are **re-sorted** to lex order (the extractor may produce
entries in arbitrary order even if rows are sorted by primary
key) and bulk-loaded into a fresh index keyspace using the same
algorithm. The sort is external (chunked sort with disk-spill if
needed; chunk size bounded by `MaxTxBufferBytes`).

When the sort fits in memory the indexes load in a single
in-memory pass. When it does not, spill chunks are written to a
per-DB scratch directory (configurable via `Options.ScratchDir`,
default `os.TempDir`) and merge-sorted in the final pass. Scratch
files are best-effort deleted on success and failure; an
unremovable scratch file (e.g. `ScratchDir` on a vanishing
tmpfs) is logged via `slog.Logger` and does not fail the
operation. A spill *write* failure (`ENOSPC` on `ScratchDir`)
aborts the `BulkLoad` with the underlying I/O error wrapped; no
rebuilt index entries are committed.

**Unique-violation detection happens at the merge output** — the
external sort's final merge pass yields entries in sorted order,
and the bulk-loader observes the first adjacent-duplicate pair
as it consumes the stream. Detection therefore happens *during*
the index pwrite phase, not before it:

- For in-memory sorts (the row count fits in
  `MaxTxBufferBytes`), the sort completes before any index-page
  pwrite, so the first duplicate is found *before* the index
  pwrite phase starts — the abort is fully reversible at the
  index layer.
- For spilling sorts, the merge output is consumed interleaved
  with index-page pwrites; the first duplicate may be found
  after some index pages have already been pwritten.

Either way, when `ErrIndexUniqueViolation` fires, `BulkLoad`
returns naming the index and offending key. Any pages already
pwritten to disk — row pages, index pages, RPL segments — are at
fresh page IDs **unreferenced by the un-swapped meta**. They
become bounded leakage reclaimed by the next bitmap-leak
reclamation pass (see `background-maintenance.md`), identical
in mechanism to any other mid-commit crash leakage. The
transaction's caller observes a clean error and can roll back;
the on-disk state is consistent with the pre-`BulkLoad` meta.

**Leakage scale warning.** "Bounded" here refers to crash-safety
(no UB, no tree corruption), not magnitude. For a spilling-sort
`BulkLoad` that aborts on a late index unique violation, the
row corpus is *already on disk* as unreferenced pages — leakage
is `O(input size)`, potentially gigabytes for a large migration
(e.g. gitfs SQLite → gmdb import). Background maintenance's
bitmap-leak reclamation does reclaim it, but only on its next
scheduled pass; in the meantime the leaked pages are invisible
to the allocator (bits clear in the on-disk bitmap until the
reclamation pass sets them), so subsequent write transactions
cannot reuse the space. Callers performing large `BulkLoad`s
that may fail should trigger
`CheckWithOptions(&CheckOptions{Repair: true})` or wait for a
maintenance pass before retrying.

A two-pass (validate-then-load) mode that guarantees "no pwrite
before violation detection" even for spilling sorts is
straightforward to add later as an option; the current single-
pass merge-output detection above is the shipped behaviour.

## Atomicity

`BulkLoad` is a transactional operation. It runs inside a write
transaction and only takes effect on commit. Either the entire
load (keyspace data + all index data + count updates) commits
atomically, or none of it does. Mid-`BulkLoad` crash leaks
pages exactly as a mid-commit crash does — bounded leakage
reclaimed by background maintenance.
