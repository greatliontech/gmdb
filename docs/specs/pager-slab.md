# Pager and Slab Architecture

This spec defines how gmdb resolves pages for reads and writes and how
write-side modifications are staged. The data file is memory-mapped
read-only by every process; all modifications happen in process-local
**slab buffers** owned by a per-transaction **pager**, and reach disk
only through the commit write-ordering protocol defined below.

Scope:
- Page resolution (read path and write path) and CoW via the slab.
- Slab budget and `ErrTxTooLarge`.
- Commit write ordering — the partitioning of commit into a
  pure-buffer assembly phase and a pwrite phase.
- Slab lifecycle across nested transactions.

Out of scope (covered elsewhere):
- mmap protections and `madvise` policy — see `mmap-strategy.md`.
- Page-checksum compute/verify — see `checksums.md`.
- Free space (bitmap + RPL) bookkeeping — see `free-space.md`;
  this spec consumes the commit-time hooks defined there.
- The `[]byte` ownership contract from a caller's perspective — see
  `api-surface.md §Byte Slice Ownership`; the lifetime guarantee is
  derived from the slab invariant below.

## Invariants

Invariant: kind=clause-explicit;
  property=The data file mapping is `MAP_SHARED | PROT_READ` in every
    process (including the writer), with `mprotect(PROT_READ)` applied
    after Open;
  from=this spec §Page Resolution + `mmap-strategy.md`;
  violation=A writable mapping admits stray-pointer or `unsafe` writes
    that silently corrupt on-disk state and bypass the commit protocol,
    so the next reader observes a tree the writer never agreed to
    publish.

Invariant: kind=clause-explicit;
  property=Every write-side page modification goes through the pager:
    fresh page ID + fresh slab buffer (or re-mutation of an existing
    same-tx slab buffer). The writer never writes through the mmap;
    on-disk state changes only via the commit-protocol pwrite path;
  from=this spec §CoW via the Slab + §Commit Write Ordering;
  violation=A direct mmap mutation reaches the unified page cache out
    of order with the commit protocol, producing a meta that references
    a tree whose pages did not pass step 1/step 2 — readers can observe
    a partially-published commit (no atomic-swap guarantee).

Invariant: kind=clause-explicit;
  property=Within a write transaction, a slab buffer is not returned to
    the buffer pool until `Commit()` or `Rollback()`. Buffers that
    become loose mid-transaction stay alive until tx close, and the
    `[]byte` borrowed from them remains valid for the full transaction;
  from=this spec §Slab Budget and `ErrTxTooLarge`;
  violation=A loose-page buffer recycled mid-tx leaves a dangling
    `[]byte` in caller hands — the API's "valid until tx close"
    contract breaks for own-write reads, and a subsequent read returns
    zero-filled bytes (pool clear on release) or another page's content
    (pool reuse).

Invariant: kind=clause-explicit;
  property=`MaxTxBufferBytes` bounds the sum of all page-sized SLAB
    buffers held by the transaction: live (routed via `dirty[id]`),
    loose, and commit-step-0 RPL segment assembly buffers.
    `ErrTxTooLarge` fires on the first CoW or commit-step-0
    allocation that would exceed the bound. Two commit-time writes
    are deliberately OUTSIDE the budget because they are not
    slab-allocated: modified bitmap pages pwrite directly from the
    in-memory bitmap's own storage (bounded by `BitmapPages`, a
    file-geometry constant), and the meta page is one page-sized
    scratch allocation per commit;
  from=this spec §Slab Budget and `ErrTxTooLarge`;
  violation=Unbounded slab growth (loose buffers excluded, or RPL
    assembly excluded) lets a transaction OOM the process despite
    nominal compliance with `MaxTxBufferBytes`, breaking the cost model
    callers use to size the budget.

Invariant: kind=entailed;
  property=Commit step 0 (pre-pwrite assembly) issues no syscall that
    publishes content reachable from any active or recoverable meta.
    `pwrite` on any page id is forbidden until step 1; `fdatasync` is
    forbidden until step 2; `ftruncate` (both directions) is permitted
    because it changes file size only, not the bytes of pages an
    active-meta reader can observe (pages above HighWaterMark are
    out of bounds for readers per `file-layout.md` invariants, and
    POSIX ftruncate-up fills new pages with zeros independent of
    content). Step 0 failure rolls back to in-memory bookkeeping
    only; the on-disk content of every page in the active meta's
    tree is byte-identical to its pre-step-0 state. The file may
    carry bounded trailing slack the next commit's tail refund or
    `maybeShrink` reclaims;
  from=entailed: the meta-swap at step 3 is the sole publication
    point (clause-explicit, §Commit Write Ordering). Any syscall
    that does not modify reader-observable bytes is reversible by
    construction — readers cannot tell it happened. The bounding of
    reversibility to "reader-observable bytes" is the *narrowest*
    invariant the spec's atomic-commit guarantee depends on; any
    stronger statement (e.g. "no syscalls at all") would be an
    over-approximation, and `free-space.md §Commit-Time Free Space
    Update` sub-step 3 already explicitly contemplates allocator
    activity (including bitmap reclamation and, transitively, file
    extension) inside step 0;
  violation=A pwrite or fdatasync hidden inside step 0 publishes
    half-written commit state — the next Open can select a meta
    whose data/bitmap pages are partially pwritten, surfacing as
    silent tree corruption. Note: an ftruncate-up inside step 0 is
    *not* a violation because no reader of the active meta accesses
    pages above HighWaterMark; ftruncate-down is permitted only as
    far as HighWaterMark for the same reason.

Invariant: kind=entailed;
  property=A crash between step 2 (data/RPL/bitmap fdatasync) and step
    4 (meta fdatasync) leaves the on-disk tree consistent with the
    *previous* meta. Pages pwritten by the partial commit are
    unreferenced and become bounded leakage reclaimed by background
    maintenance;
  from=entailed: "atomic commit point" property of the meta swap, plus
    CoW (no in-place overwrites of pages reachable from any meta);
  violation=The crashed transaction's pages are reachable from the
    new meta after recovery, but data/bitmap state is partially-
    pwritten — readers traversing the new tree dereference a page
    whose contents were never durable, surfacing as wrong values, bad
    checksums, or `ErrCorrupted`.

Invariant: kind=entailed;
  property=Re-modifying a page already CoW'd in this transaction
    mutates the existing slab buffer in place — no second buffer is
    allocated and no second page ID is consumed for the same logical
    write;
  from=entailed: `MaxTxBufferBytes` cost model in §Slab Budget assumes
    one buffer per *unique* CoW destination;
  violation=A re-CoW that re-allocates pushes the slab budget into
    multiplicative behaviour over operation count, and consumes a
    second page ID that must be reclaimed via the RPL even though no
    new content is on disk — wasted free-space pressure and
    misleading `ErrTxTooLarge` triggers.

## Roles

A single `Pager` type per transaction handles both reads and writes.
Read transactions get a read-only pager that resolves pages from the
mmap. Write transactions get a writable pager that additionally owns
the dirty-page slab.

```
type Pager struct {
    mmap        []byte               // read-only view of the data file
    pageSize    int
    dirty       map[uint64]*[]byte   // page ID → slab buffer (write txn only)
    dirtyBytes  int                  // current slab usage in bytes
    maxBytes    int                  // Options.MaxTxBufferBytes
    bufPool     *sync.Pool           // page-sized scratch buffers
    readOnly    bool
}
```

## Page Resolution

`pager.Page(id) []byte` returns a borrowed byte slice for the page at
`id`:

```
if buf, ok := p.dirty[id]; ok {
    return *buf                                             // own dirty page
}
return p.mmap[id*p.pageSize : (id+1)*p.pageSize]            // mmap
```

One branch, two cases. No layered buffer cache, no eviction policy.
The OS page cache handles everything except the writer's own in-flight
changes.

Reads through the mmap are file-cache-backed; the kernel handles
eviction under memory pressure. There is no application-level page
buffer and no page-count limit. CoW'd pages from a committed write
transaction become visible through every process's mmap after the
commit's data-page pwrites complete (Linux/macOS unified page cache);
readers serialize their snapshot via the meta page's `TxnID`.

## CoW via the Slab

When the writer modifies a page from a prior transaction:

1. Allocate a fresh page ID via `pageAlloc()` (see `free-space.md`).
2. Acquire a page-sized buffer from `bufPool`.
3. Copy current page content (from `pager.Page(oldID)` — mmap or a
   same-tx dirty buffer) into the slab buffer.
4. Insert the buffer into `p.dirty[newID]`.
5. `dirtyBytes += pageSize`. If `dirtyBytes > maxBytes`, return
   `ErrTxTooLarge` — the caller must roll back.
6. Track the old page ID for retirement (`tx.retiredPages` if from a
   prior tx, `tx.loosePages` if same-tx CoW that has just been
   superseded).
7. Track the new page ID in `tx.pendingAllocs` and `tx.cowPages`.
8. Mutate the slab buffer in place.

A re-modification of a page already in `p.dirty` mutates the existing
buffer in place — no second buffer is allocated. `tx.cowPages` is the
discriminator.

## Slab Budget and `ErrTxTooLarge`

`Options.MaxTxBufferBytes` (default 256 MiB) bounds the slab. The
budget covers every page-sized buffer the transaction has allocated:

- Live buffers (`dirty[id]` routes here).
- Loose buffers (CoW'd then freed mid-tx; retained to honour the
  byte-slice ownership invariant above).
- Commit-time assembly buffers — RPL segment pages and modified
  bitmap pages allocated in step 0 of commit.

`ErrTxTooLarge` fires on the first CoW (during the transaction body)
or step-0 allocation (during commit) that would push `dirtyBytes`
over `maxBytes`. The commit-time variant is detected before any
pwrite — rollback is clean (no on-disk side effects).

Buffers are **not** returned to the pool when a page becomes loose
within the transaction. They are returned only at `Commit()` or
`Rollback()`. This preserves the byte-slice ownership contract: a
`[]byte` returned by `Keyspace.Get` or a cursor read that points into
a slab buffer remains valid for the full transaction even if the
underlying page is CoW'd, rebalanced, or freed mid-transaction. The
cost is bounded by `MaxTxBufferBytes` (loose buffers count against
the same budget as live ones).

The buffer pool is shared process-wide via `sync.Pool`. Returning a
buffer clears it (zero-fill) and makes it available for reuse.
Cross-transaction reuse keeps allocator pressure low for steady write
workloads; cross-process slab usage is not visible from any one DB
handle (each process holds its own pool).

**Cost-model note.** A transaction that CoWs the same logical page
multiple times still pays one buffer (the re-modify invariant above)
— but a transaction that CoWs different pages at different tree
levels accumulates one buffer per unique destination. The 256 MiB
default sizes against `unique CoW destinations × pageSize`, not
operation count. Typical worst case is `pages-touched × depth × (1 +
indexes)`. Bulk operations have a dedicated escape hatch — see
`bulkload.md`.

## Commit Write Ordering

Commit is partitioned into a **pre-publish assembly phase** (no
syscall publishes reader-observable bytes; `ftruncate` is permitted
because file-size changes are not reader-observable) followed by a
**pwrite phase** whose failures are bounded to crash-equivalent
leakage.

### Step 0 — Pre-publish assembly (no publishing syscalls)

- Tail page refund: check the bitmap for tail free pages, decrement
  `HighWaterMark`.
- Move remaining loose pages into `tx.pendingFrees` (they bypass the
  RPL because no reader can reference a same-tx page).
- Drop freed pages' slab buffers from the write set: every `p.dirty`
  entry whose page this commit marks free (`tx.pendingFrees`) or that
  the tail refund moved past the new `HighWaterMark` is discarded,
  not pwritten — such a page is referenced by no meta, no tree, and
  no RPL entry, so writing its bytes is pure write amplification.
  The buffers must survive in `p.dirty` until this step (an open
  shallow savepoint may resurrect the operation that freed them;
  step 0 runs with every savepoint resolved).
- Allocate RPL segment pages for `tx.retiredPages` and fill them with
  per-segment TxnID + sorted PageID entries. Insert into `p.dirty`
  (counts against `MaxTxBufferBytes`). Append the new segment page
  IDs to the in-memory RPL segment list.
- For each modified bitmap page (derived from `tx.pendingAllocs ∪
  tx.pendingFrees`), read the current bitmap page from the mmap, apply
  pending bit changes into a freshly allocated slab buffer, and insert
  the buffer into `p.dirty` keyed by its bitmap page ID. Counts
  against `MaxTxBufferBytes`.
- Construct the new meta page payload (new roots, new TxnID, updated
  `HighWaterMark`, updated RPL pointers and counters, recomputed
  xxhash64 checksum) into a fresh buffer held on the transaction (not
  in `p.dirty` — the meta page lives at a fixed slot and is pwritten
  in step 3).

Any step-0 failure (`ErrTxTooLarge`, `ErrDBFull`, RPL capacity
exhausted) is fully reversible *with respect to reader-observable
state*: rollback releases buffers, drops `tx.pendingAllocs` (re-
availing any RPL-segment page IDs to the next transaction's
allocator), reverts the in-memory RPL segment list appends, and
restores the in-memory bitmap + HighWaterMark from the snapshot
taken at tx begin. Any `ftruncate`-up that happened during step 0
(file extension to back newly-allocated pages) leaves trailing
slack the next commit's tail refund or `maybeShrink` reclaims —
this is not a reader-visible side effect, so the reversibility
contract still holds.

### Step 1 — Data + RPL + bitmap pwrite

For each `(pageID, buf)` in `p.dirty` (now containing data pages, RPL
segment pages, and modified bitmap pages): compute the xxhash64
footer (if `PageChecksum`), then `pwrite(fd, *buf, pageID *
pageSize)`. Order within step 1 is unspecified; implementations may
coalesce contiguous runs via `pwritev2` on Linux.

A partial-success pwrite (some pages reach the page cache, others
fail mid-step) is crash-equivalent: meta is untouched, the previous
meta is selected on next Open, and the partially-written pages are
unreferenced bounded leakage.

### Step 2 — fdatasync (data/RPL/bitmap)

Skipped in `SyncLazy` and below. After step 2 returns, data, RPL,
and bitmap pages are durable.

### Step 3 — Meta pwrite

`pwrite` the meta-page buffer constructed in step 0 to the inactive
meta slot.

### Step 4 — fdatasync (meta) — atomic commit point

Skipped in `SyncDataOnly` and below. After step 4 returns, the
commit is durable end-to-end.

A crash between step 2 and step 4 leaves the previous meta active;
the new data/RPL/bitmap pages are unreferenced free space until the
next commit (or background maintenance) reclaims them — bounded
leakage, tree integrity preserved.

### Commit failure cleanup

A pwrite error during step 1 or step 3 returns the error to the
caller; the transaction must be rolled back. Rollback releases every
slab buffer (live, loose, assembled RPL segments) back to the pool;
the bitmap restores from its transaction snapshot and the meta
scratch buffer is garbage-collected (neither is pool-managed). The on-disk meta is untouched, so the
database remains consistent with the previous meta; any
partially-pwritten data/RPL/bitmap pages are unreferenced and become
bounded crash leakage reclaimed by background maintenance.

Typical commit: tens to low hundreds of data-page pwrites + 0–N RPL
segment pwrites + 2–5 bitmap pwrites (all step 1) + 1 meta pwrite
(step 3), with two fdatasyncs (step 2 + step 4).

## Slab Lifecycle Across Nested Transactions

Child transactions never modify a parent's slab buffer in place.
Every child CoW allocates a fresh page ID and a fresh slab buffer
(copied from the parent's current view of that page). On child
commit, the child's `(pageID, buf)` entries are merged into the
parent's `p.dirty`, and the child's allocations join the parent's
`tx.pendingAllocs`. On child rollback, the child's buffers are
released back to the pool and the child's page IDs are returned to
the allocator (cleared from `tx.pendingAllocs`).

This preserves the "rollback discards bookkeeping, never restores
prior buffer state" simplicity at the slab layer — the analogue of a
fresh-mmap-position CoW. See `transactions.md §Nested Transactions`
for the full bookkeeping snapshot/restore contract.

## Portability Rationale

A single commit path on Linux, macOS, and FreeBSD: pwrite +
fdatasync. No platform-conditional code in the commit hot path. No
`msync(MS_SYNC)` is needed on any supported OS because the writer
never writes through the mmap; Linux and macOS both use a unified
page cache for `MAP_SHARED` + `pwrite` on the same file, so a
subsequent read through the read-only mapping observes the pwritten
contents.
