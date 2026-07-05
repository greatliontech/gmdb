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
  property=A failed or aborted `BulkLoad` never publishes a
    partial tree: `desc.Root`, `desc.Count`, and the index roots
    advance only after the *entire* load succeeds, so the meta
    the engine can recover to always references either the
    complete new state or the pre-`BulkLoad` state — never a
    partial one. Disposition of the already-pwritten pages then
    depends on how the transaction ends: (a) a clean in-process
    rollback (`Tx.Rollback` → `AbortTx`) restores the bitmap
    snapshot, reverting every bulk-written page to free with NO
    leak — immediately reusable, no maintenance pass needed;
    (b) a commit-after-error (the caller ignores the `BulkLoad`
    error and commits anyway, per the rest-of-tx-continues
    contract) commits those allocated-but-unreferenced pages as
    bounded leakage reclaimed by background maintenance's
    bitmap-leak pass; (c) a crash before commit never recorded
    the allocations on the on-disk bitmap (pwritten only at
    commit), so the pages reopen as free — only any physical
    file extension persists, reclaimed by `Compact()` / shrink;
  from=entailed: §Atomicity + `background-maintenance.md`;
  violation=Advancing any reachable meta (a root field) before
    the whole load succeeds lets a crash or rollback expose the
    partial tree as active state — the next opener sees a
    partial tree (corruption).

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

Invariant: kind=clause-explicit;
  property=The indexed-`BulkLoad` merger holds at most
    `maxMergeFanIn` spilled-run files open concurrently and uses
    at most `O(maxMergeFanIn × 64 KiB)` of bufio read-buffer
    memory at the merge phase, regardless of how many sort runs
    were spilled (`#runs = inputBytes / budget`, where `budget =
    MaxTxBufferBytes / #indexes`). The merge heap's per-slot
    record key+value bytes (one `make([]byte, n)` per
    `readRunField` call, lifetime = one merge step) are not
    bounded by `maxMergeFanIn × 64 KiB`; their footprint is a
    pre-existing property of the merger inherited by the
    cascade and bounded by `O(maxMergeFanIn × max-record-size)`.
    When `#runs > maxMergeFanIn`, the merger cascades through
    one or more intermediate merge passes (each merging up to
    `maxMergeFanIn` runs into a single larger run) until the
    final fan-in fits the cap. This bound extends the
    `MaxTxBufferBytes`-scoped read-buffer footprint from phase-1
    sorter accumulation to phase-2 merge, independent of input
    size;
  from=§Interaction with Indexes "Merge fan-in cap";
  violation=Without the cap, a multi-gigabyte input under a
    small `MaxTxBufferBytes` (the gitfs SQLite → gmdb migration
    the §Interaction with Indexes "Leakage scale warning"
    describes; e.g. 16 MiB `MaxTxBufferBytes` ÷ 5 indexes ⇒ 3.2
    MiB per-index budget against 16 GB of index entries ⇒ ~5000
    spilled runs per index) lets the merger open all runs
    simultaneously. The open-file count exceeds the per-process
    FD limit (`EMFILE`; default 256 on macOS, 1024 on most
    Linux distros), failing the operation; and even when FDs
    hold, the 64 KiB `bufio.Reader` per open run consumes
    `O(#runs × 64 KiB)` of read-buffer memory — at 4000 runs,
    256 MiB of additional memory on top of the user-configured
    `MaxTxBufferBytes`, effectively doubling the in-tx memory
    footprint relative to what `MaxTxBufferBytes` promises.

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

Error sentinels: BulkLoad surfaces the same public sentinels as the
per-op paths on EVERY variant — indexed and non-indexed, Keyspace
and SetKeyspace: an oversize key (row key, set key, or an
extractor-produced index key on the indexed variants) and an
oversize set value that cannot fit a leaf even alone (including a
key's FIRST value, which by design bypasses the promotion threshold
to match Put's genesis shape) surface `ErrKeyTooLarge`, exactly as
the same input would through `Put`. Internal builder sentinels never
escape the BulkLoad boundary. (Pinned per path by
TestErrKeyTooLargeSentinel.)

## Algorithm

Standard bottom-up B+tree construction:

1. Allocate a fresh leaf page from `pageAlloc()`. Fill with input
   entries — encoded per the keyspace's `RestartGroupTarget`
   (prefix-compressed `TypeLeaf` for target `≥ 2`, uncompressed
   `TypeLeafUncompressed` for target `= 1`; see `page-formats.md
   §Leaf Page`) — until the page is full or the input is
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
safe. A clean in-process rollback (`Tx.Rollback`) restores the
bitmap snapshot, so every bulk-written page reverts to free and
is immediately reusable with no leak. Only a commit-after-error
or a crash leaves the pages as bounded leakage in the bitmap,
reclaimed by the next maintenance pass exactly like any other
crash leakage (see the §Invariants bounded-leakage invariant for
the full disposition).

This bypass keeps memory usage flat regardless of input size and
makes `BulkLoad` the recommended path for inputs that would
otherwise exceed `MaxTxBufferBytes`.

## Interaction with Indexes

For an indexed keyspace, the engine runs the extractor on every
row and accumulates index entries per index. Each index's
entries are **re-sorted** to lex order (the extractor may produce
entries in arbitrary order even if rows are sorted by primary
key) and bulk-loaded into a fresh index data tree using the same
algorithm. The sort is external (chunked sort with disk-spill if
needed). All indexes' sorters accumulate concurrently during the
single pass over the input, so the budget is the **aggregate**:
each index's in-memory chunk is bounded by `MaxTxBufferBytes /
#indexes`, keeping the combined in-memory sort footprint bounded
by `MaxTxBufferBytes` (not `MaxTxBufferBytes` per index, which
would let N indexes peak at N× the budget and defeat the memory
contract).

When the sort fits in memory the indexes load in a single
in-memory pass. When it does not, spill chunks are written to a
per-DB scratch directory (configurable via `Options.ScratchDir`,
default `os.TempDir`) and merge-sorted into the bulk builder.
Scratch files are best-effort deleted on success and failure; an
unremovable scratch file (e.g. `ScratchDir` on a vanishing
tmpfs) is logged via `slog.Logger` and does not fail the
operation. A spill *write* failure (`ENOSPC` on `ScratchDir`)
aborts the `BulkLoad` with the underlying I/O error wrapped; no
rebuilt index entries are committed.

**Merge fan-in cap.** The k-way merger opens every spilled run
simultaneously to read sorted heads from each, so unbounded
`#runs` would let merge-time file descriptors and read-buffer
memory grow with the workload — exceeding the per-process FD
limit and, separately, exceeding the `MaxTxBufferBytes`-scoped
memory contract because the 64 KiB `bufio.Reader` per open run
is allocated outside the phase-1 sorter accumulator. The merger
therefore caps simultaneous fan-in at `maxMergeFanIn = 128`:
when `#runs > maxMergeFanIn`, runs are first cascaded through
one or more intermediate merge passes (each merging up to
`maxMergeFanIn` runs into a single larger run) until the final
fan-in fits the cap. Cascade intermediates are written to
`ScratchDir` with the same spill-file format and are removed as
soon as the next cascade pass consumes them. The pre-level
runs are NOT removed pre-emptively — only after the next level
fully writes — so a write failure mid-pass never destroys data
that hasn't been safely re-encoded into the next level.

The cascade adds `O(log_fanin(#runs))` extra scratch read+write
passes but bounds merge-phase resource usage at
`O(maxMergeFanIn)` simultaneously-open files and
`O(maxMergeFanIn × 64 KiB) ≈ 8 MiB` bufio read-buffer memory
regardless of `#runs`. The 128-run cap stays comfortably under
the typical per-process FD limit (256 on macOS default, 1024+
on most Linux distros) while keeping the read-buffer budget
small relative to the default 256 MiB `MaxTxBufferBytes`.
Per-cascade-pass FD ceiling is `maxMergeFanIn + 1` (the +1 for
the in-flight cascade-output file). The
`maxMergeFanIn`-bounded final merger preserves the
`MaxTxBufferBytes`-scoped memory contract end-to-end: phase-1
sorter accumulator (`≤ MaxTxBufferBytes / #indexes`) and
phase-2 merge buffer (`≤ maxMergeFanIn × 64 KiB`) are both
independent of input size.

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
(no UB, no tree corruption), not magnitude — and the magnitude
matters *only* if the caller **commits the transaction after the
`BulkLoad` returned an error** (the rest-of-tx-continues path).
The normal recovery — `Tx.Rollback()` on the error — restores
the bitmap snapshot and reclaims every bulk-written page
immediately, with no leak (§Invariants disposition (a)). Callers
should always roll back a failed `BulkLoad`.

When a caller *does* commit-after-error, the consequence is
material: for a spilling-sort `BulkLoad` that aborted on a late
index unique violation, the row corpus is *already on disk* as
allocated-but-unreferenced pages — committed leakage is
`O(input size)`, potentially gigabytes for a large migration
(e.g. gitfs SQLite → gmdb import). Background maintenance's
bitmap-leak reclamation does reclaim it, but only on its next
scheduled pass; in the meantime the leaked pages are in-use in
the on-disk bitmap with no meta referencing them, so subsequent
write transactions cannot reuse the space until the reclamation
pass clears them. A caller that committed-after-error and cannot
wait for a pass should trigger
`CheckWithOptions(&CheckOptions{Repair: true})`.

A two-pass (validate-then-load) mode that guarantees "no pwrite
before violation detection" even for spilling sorts is
straightforward to add later as an option; the current single-
pass merge-output detection above is the shipped behaviour.

## Atomicity

`BulkLoad` is a transactional operation. It runs inside a write
transaction and only takes effect on commit. Either the entire
load (keyspace data + all index data + count updates) commits
atomically, or none of it does.

The implementation enforces this with a stronger *in-process*
guarantee than commit-time atomicity alone: it builds the row
tree and every index data tree to completion first, and only
then — in a single publish step reached exclusively on full
success — advances `desc.Root`, `desc.Count`, and the in-memory
index roots. A unique violation or I/O error during any index
build returns before that step, so the descriptor and index
roots are never even transiently advanced to a partial state.
Combined with the bitmap snapshot, a clean `Tx.Rollback()` after
an error reclaims all bulk-written pages with no leak; only a
commit-after-error or a crash leaves bounded leakage, reclaimed
by background maintenance (see the §Invariants disposition).
