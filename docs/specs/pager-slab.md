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
    same-tx slab buffer), or a DIRECT pwrite to a page allocated in
    this transaction and referenced by no active or recoverable meta
    (overflow-run pages, the bulk-load builder, and the spill pass's
    write-out of slab pages — the `WriteDirect` contract,
    `bulkload.md §Slab Bypass`). The writer never writes
    through the mmap; pages reachable from any committed meta change
    only via the commit-protocol pwrite path;
  from=this spec §CoW via the Slab + §Commit Write Ordering;
  violation=A direct mmap mutation — or a direct pwrite to a page an
    active meta can reach — reaches the unified page cache out
    of order with the commit protocol, producing a meta that references
    a tree whose pages did not pass step 1/step 2 — readers can observe
    a partially-published commit (no atomic-swap guarantee).

Invariant: kind=clause-explicit;
  property=Within a write transaction, a slab buffer is never
    POOL-RECYCLED before `Commit()` or `Rollback()`: the pool's
    clear-on-release and reuse would corrupt a borrowed `[]byte`. A
    buffer may be DROPPED mid-transaction (the spill pass writes it
    out first when its content is still referenced; loose and
    detached buffers drop outright once no savepoint can resurrect
    their content) — a dropped buffer survives through the garbage
    collector for exactly as long as any borrowed `[]byte` aliases
    it, so the API's "valid until tx close" contract holds either
    way;
  from=this spec §Slab Budget and `ErrTxTooLarge`;
  violation=A loose-page buffer POOL-recycled mid-tx leaves a
    dangling `[]byte` in caller hands — a subsequent read returns
    zero-filled bytes (pool clear on release) or another page's
    content (pool reuse).

Invariant: kind=clause-explicit;
  property=`MaxTxBufferBytes` is the SPILL THRESHOLD, not a
    transaction-size cap: at every operation boundary the slab is
    brought back under `MaxTxBufferBytes − commitReserve` — live
    pages (in `pendingAllocs`) AND loose pages are pwritten to their
    own file locations and their buffers dropped (a loose page's
    bitmap bit stays clear until commit, so the id is stable and a
    savepoint restore that resurrects the free reads the content
    back through the mmap — which is what keeps child-transaction
    churn bounded); with NO savepoint open, buffers held only for a
    restorability nothing can still exercise (loose pages' buffers,
    loose-pop detached buffers) are dropped outright without the
    pwrite. Between boundaries the slab may exceed the threshold by
    one operation's footprint. Deliberately OUTSIDE the accounting
    because they are never slab-allocated: overflow-run pages
    (pwritten directly as encoded, O(2 pages) working memory — see
    §Slab Budget), modified bitmap pages (pwritten from the
    in-memory bitmap's own storage, bounded by `BitmapPages`, a
    file-geometry constant), and the meta page (one page-sized
    scratch allocation per commit). User-borrowed `[]byte` slices
    alias dropped buffers and stay valid through tx close (garbage
    collection — the byte-slice ownership invariant), so
    steady-state memory is bounded by the threshold plus what the
    caller itself retains;
  from=this spec §Slab Budget and `ErrTxTooLarge`;
  violation=A boundary that fails to spill (or an accounting drift
    that hides held buffers) lets a long transaction OOM the process
    despite nominal compliance with `MaxTxBufferBytes`, breaking the
    cost model callers use to size the threshold — the failure mode
    the old hard-admission budget prevented and the spill must too.

Invariant: kind=clause-explicit;
  property=`Commit` never fails the budget: the commit phase's own
    slab allocations — the descriptor flush's CoW pages and step-0's
    RPL segment pages, the pages that CANNOT spill — are covered by
    `commitReserve`, an exact projection maintained live (RPL:
    `ceil(retired / entriesPerSegment)`, pager-internal; flush: one
    CoW page per tree-path level per pending flush write, with slack
    for the flush's own retires — transaction layer). The reserve
    alone is bounded by `MaxTxBufferBytes`: an OBLIGATION event
    (dirtying a keyspace, a registry DDL) whose projection would
    exceed the bound rejects with `ErrTxTooLarge` and unwinds, and a
    retire that opens an RPL segment the reserve cannot afford
    rejects the same way — the two remaining `ErrTxTooLarge`
    surfaces (live dirtyBytes is charged by NEITHER: data pages
    spill at boundaries, and freed pages' buffers drop at step 0). A
    pre-commit spill brings the slab under the threshold before the
    commit phase draws from the reserved space.
    INV-COMMIT-HEADROOM: enforced by
    `TestCommitSucceedsAfterTxTooLarge` (over-threshold fill still
    commits), `TestCommitNeedsOnlyReservedHeadroom`,
    `TestRetireBudgetGuard`, and
    `TestRebuildRejectionUnwindsObligation`;
  from=this spec §Slab Budget and `ErrTxTooLarge` +
    `transactions.md` write-helper error contract (the
    rest-of-tx-continues shape is incoherent if the engine's own
    commit can exceed what its reserve guarantees);
  violation=A reserve that under-projects (or an obligation admitted
    past the bound) makes `Commit` itself fail `ErrTxTooLarge` — the
    applied work is recoverable only by `Rollback`, i.e. lost, while
    every other budget clause still holds.

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
  from=entailed: the `MaxTxBufferBytes` cost model in §Slab Budget
    prices one buffer per unique CoW destination; within a single
    operation the tree layer re-mutates the destination it already
    owns, and this invariant keeps that re-mutation free;
  violation=A re-CoW that re-allocates pushes the slab budget into
    multiplicative behaviour over operation count, and consumes a
    second page ID that must be reclaimed via the RPL even though no
    new content is on disk — wasted free-space pressure and
    misleading `ErrTxTooLarge` triggers.

## Roles

A single pager per transaction handles both reads and writes. Read
transactions get a read-only pager that resolves pages from the mmap.
Write transactions get a writable pager that additionally owns the
dirty-page slab: the write set (`p.dirty`, page ID → slab buffer)
and its running byte count (`dirtyBytes`), bounded by
`Options.MaxTxBufferBytes`. Those two names are used throughout
this spec for the write set and its budget charge.

## Page Resolution

Page resolution returns a borrowed byte slice: the transaction's own
dirty slab buffer when the page was written this transaction,
otherwise a slice of the mmap. One branch, two cases. No layered
buffer cache, no eviction policy. The OS page cache handles
everything except the writer's own in-flight changes.

Reads through the mmap are file-cache-backed; the kernel handles
eviction under memory pressure. There is no application-level page
buffer and no page-count limit. CoW'd pages from a committed write
transaction become visible through every process's mmap after the
commit's data-page pwrites complete (Linux/macOS unified page cache);
readers serialize their snapshot via the meta page's `TxnID`.

## CoW via the Slab

When the writer modifies a page from a prior transaction:

1. Allocate a fresh page ID via `pageAlloc()` (see `free-space.md`).
2. Acquire a page-sized buffer from the buffer pool.
3. Copy the current page content (resolved as any read — mmap or a
   same-tx dirty buffer) into the slab buffer.
4. Register the buffer as the new page ID's dirty entry.
5. Charge one page against the slab budget; over
   `Options.MaxTxBufferBytes`, return `ErrTxTooLarge` — the caller
   must roll back.
6. Track the old page ID for retirement (RPL-bound if from a prior
   tx; the loose pool if it was a same-tx CoW just superseded).
7. Track the new page ID as pending-allocated and CoW'd-this-tx.
8. Mutate the slab buffer in place.

A re-modification of a page already dirty this transaction mutates
the existing buffer in place — no second buffer is allocated; the
CoW'd-this-tx tracking is the discriminator.

## Slab Budget and `ErrTxTooLarge`

`Options.MaxTxBufferBytes` (default 256 MiB) is the slab's SPILL
THRESHOLD: transactions of any size commit; the threshold bounds
steady-state memory. The accounting covers every page-sized buffer
the transaction holds:

- Live buffers (`dirty[id]` routes here).
- Loose buffers (CoW'd then freed mid-tx; held for the
  shallow-savepoint resurrection of the free and for byte-slice
  ownership).
- Commit-time assembly buffers — the RPL segment pages allocated in
  step 0 of commit. (Modified bitmap pages are NOT slab-allocated:
  step 1 pwrites them directly from the in-memory bitmap's own
  storage, outside the budget per the Invariants above.)

**The spill pass.** At every operation boundary — a savepoint
resolution that leaves no shallow window open, and once more before
the commit phase — a slab over `MaxTxBufferBytes − commitReserve`
is brought back under it:

- Live pages (in `pendingAllocs`) and loose pages are pwritten to
  their own file locations — footer-stamped exactly like commit's
  step 1 — and their buffers dropped. Spilling loose pages is
  load-bearing under an open nested window, where they can neither
  drop (a restore could resurrect the free) nor loose-pop
  (suspended): without it a child transaction's churn accumulates
  unbounded loose buffers. The pages read back through the mmap
  (unified page cache) and are unreferenced by any recoverable meta
  until the meta swap, so a crash mid-transaction leaves the
  died-holding-grant image (`free-space.md` grant-handoff tear
  detection and leak reclamation cover it) and a rollback's bitmap
  restore orphans them as free-page garbage. A later re-modification
  frees the spilled page and CoWs to a fresh id like any committed
  page; an in-window free of a spilled page rides the deferred-frees
  quarantine (`free-space.md`, restorable-content invariant).
- With no savepoint open, loose pages' buffers and loose-pop
  detached buffers are dropped OUTRIGHT (no pwrite): they exist for
  a savepoint resurrection that can no longer happen, and a loose
  page's content is read by nothing else.
- Between boundaries the slab may exceed the threshold by one
  operation's footprint; an operation mid-flight is never spilled
  (its writable buffers are in active use).

The spill is best-effort on I/O error — the pass stops, memory
relief degrades, and the error resurfaces loudly at commit.
`TxStats.SpilledPages` counts spilled pages for threshold tuning.

**Overflow runs never enter the slab.** Every run page — online
chain writes, key extents, run relocation, bulk load — is pwritten
directly at its allocation-fresh id as the run is encoded
(followers first, head last; `checksums.md §Overflow-Run Digest`),
in O(2 × PageSize) working memory regardless of value size, and
reads back through the mmap (unified page cache) same-tx included.
Large values therefore do not charge `MaxTxBufferBytes` at all.
Until the meta swap the run's pages are unreferenced by any
recoverable meta — a crash or rollback leaves them as harmless
free-page garbage (the in-memory bitmap snapshot restores; the
on-disk bitmap was never pwritten pre-commit), exactly the
`WriteDirect` bulk-load contract (`bulkload.md §Slab Bypass`),
which this generalizes.

**`ErrTxTooLarge` fires on exactly two surfaces** — both about the
commit RESERVE, the slab the commit phase itself must allocate and
therefore cannot spill:

1. A retire that opens an RPL segment the reserve cannot afford
   (`reserve + PageSize > MaxTxBufferBytes` at the segment
   boundary) — a retire-heavy operation (a huge `DeleteRange`, a
   giant compaction pass) whose retired-page log alone outgrows the
   threshold.
2. An obligation event — dirtying a keyspace, a registry DDL —
   whose flush projection would push the reserve past the
   threshold; the event unwinds (INV-COMMIT-HEADROOM above).

Ordinary data writes NEVER fail the budget: past the threshold they
spill. During the commit phase allocations are checked against the
raw bound as a backstop.

**Commit reserve.** The pager continuously reserves the exact slab
cost of the commit sequence — its own RPL segment projection plus
the transaction layer's descriptor-flush projection — and the
commit phase draws from that reserved space (a pre-commit spill
freed everything else). Every flush write is a same-size upsert
(descriptors are fixed-width; registry entries change only
fixed-width fields after open; descriptor inserts and deletes
happen eagerly inside `CreateKeyspace*` / `DeleteKeyspace`), so it
can neither split nor merge and costs exactly one CoW page per
tree-path level — which is what makes the projection exact rather
than an estimate.

Buffers are **never pool-recycled** when a page becomes loose
within the transaction — only at `Commit()` or `Rollback()` (or
dropped to the garbage collector by the spill pass, which preserves
borrowed slices — see the Invariants). This preserves the
byte-slice ownership contract: a `[]byte` returned by
`Keyspace.Get` or a cursor read that points into a slab buffer
remains valid for the full transaction even if the underlying page
is CoW'd, rebalanced, freed, spilled, or dropped mid-transaction.

The buffer pool is shared process-wide. Returning a buffer clears
it (zero-fill) and makes it available for reuse.
Cross-transaction reuse keeps allocator pressure low for steady write
workloads; cross-process slab usage is not visible from any one DB
handle (each process holds its own pool).

**Cost-model note.** Within one operation, re-modifying a page the
operation already CoW'd pays nothing further (the re-modify
invariant above). Across operations, each tree-level modification
allocates a fresh destination page whose superseded same-tx
predecessor goes loose; the spill pass drops loose buffers (and
spills live ones) at boundaries, so steady-state memory tracks the
THRESHOLD rather than the transaction's cumulative
`operations × depth × (1 + indexes)` footprint — the transaction
trades pwrite volume for memory past the threshold. Bulk operations
still have their dedicated bottom-up path — see `bulkload.md`.

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
- Bitmap-bit changes need no assembly: they were applied to the
  in-memory bitmap inline as work happened (AllocPage / FreePage /
  tail refund / reclamation), and step 1 pwrites each modified
  bitmap page directly from the bitmap's own storage — no slab
  buffer, outside `MaxTxBufferBytes` (bounded by `BitmapPages`, a
  file-geometry constant).
- The new meta page payload is NOT built here: it is composed after
  step 2, because its `AnchoredDurableTxnID` may name the
  step-2-anchored assertion (that fsync has completed) but never this
  commit's own step-4, which has not run yet — durability.md
  §Anchoring's no-forward-promise, the governing tier. Step 0
  finalises everything ELSE the payload reads (HighWaterMark, RPL
  chain, bitmap state — steps 1-2 change none of them); the anchored
  epoch is the one input only step 2 settles.

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
segment pages, and modified bitmap pages): compute the XXH3-64
footer (if `PageChecksum` — every slab page is a node/RPL page and
takes a footer; overflow-run pages are never slab-resident, so the
footer pass cannot reach one: they were pwritten directly at write
time with the head-resident whole-run digest, `checksums.md
§Overflow-Run Digest`), then `pwrite(fd, *buf, pageID * pageSize)`.
Order within step 1 is unspecified; implementations may coalesce
contiguous runs via `pwritev2` on Linux.

A partial-success pwrite (some pages reach the page cache, others
fail mid-step) is crash-equivalent: meta is untouched, the previous
meta is selected on next Open, and the partially-written pages are
unreferenced bounded leakage.

### Step 2 — fdatasync (data/RPL/bitmap)

Skipped in `SyncLazy` and below. After step 2 returns, data, RPL,
and bitmap pages are durable.

### Step 3 — Meta pwrite

Compose the new meta payload (new roots, new TxnID, updated
`HighWaterMark`, updated RPL pointers and counters, the anchored
epoch as of the completed step 2, recomputed XXH3-64 checksum) and
`pwrite` it to the inactive meta slot. Composed here — after step 2,
never in step 0 — per durability.md §Anchoring's no-forward-promise
(see the step-0 bullet).

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
