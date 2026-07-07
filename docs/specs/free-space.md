# Free Space Management

Free space is tracked by two on-disk structures with separated
concerns: **what is free** (the allocation bitmap) and **when it
became free** (the retired page log, RPL). Decoupling avoids the
self-referential allocation problem found in freelist-B+tree designs
where modifying the freelist during commit could itself allocate or
free pages.

Scope:
- Allocation bitmap: storage, two-level summary, bit operations.
- Retired page log (RPL): segment page format, append at commit,
  reclamation contract (including the durable-epoch bound), in-memory
  segment list.
- LIFO allocation hint, loose pages, allocation priority, tail-page
  refund, and the commit-time free-space update step.

Depends on / interacts with:
- `file-layout.md` for the bitmap region location and meta-page
  pointers (`RPLHeadPage`, `RPLTailPage`, `NumFreePages`,
  `HighWaterMark`).
- `pager-slab.md` step 0 of commit, which calls into this spec for
  bitmap-page assembly and RPL append.
- `cross-process.md` for the oldest-reader scan that bounds RPL
  reclamation.
- `durability.md` for the durable-epoch bound rationale.
- `background-maintenance.md` for bitmap-leak reclamation and the
  recomputation of `NumFreePages`.

## Invariants

Invariant: kind=clause-explicit;
  property=A page is **safe to allocate** iff its bit is set in the
    allocation bitmap and the page is not in the meta-page range
    (0, 1) or the bitmap region (`2..2+BitmapPages-1`). The meta and
    bitmap pages have permanently clear bits;
  from=this spec §Allocation Bitmap;
  violation=Allocating a meta or bitmap page corrupts the file
    structure on first pwrite; allocating a page whose bit is clear
    (still referenced by some snapshot) hands the same page to two
    consumers — silent data corruption.

Invariant: kind=clause-explicit;
  property=A page whose bit is **clear** is either (a) in use by the
    active meta's tree, (b) retired and pending RPL reclamation, or
    (c) a meta page or bitmap page. A page whose bit is **set** is
    free and unreferenced by any meta the engine can recover to;
  from=this spec §Allocation Bitmap + §RPL Reclamation;
  violation=Setting a bit for a page that some still-active reader
    references lets the writer reallocate it under the reader,
    corrupting the reader's snapshot mid-traversal.

Invariant: kind=clause-explicit;
  property=No page is reclaimed (bit set, RPL entry removed) before
    the **reclamation bound**: `min(oldestActiveReaderTxnID,
    anchoredEpoch)` — the anchored epoch being the newest
    fsync-covered `DurableTxnID` assertion (`durability.md
    §Anchoring`; never ahead of any epoch a crash could make
    recovery adopt). Only RPL entries whose `TxnID` is
    strictly less than the bound move to the bitmap;
  from=this spec §RPL Reclamation;
  violation=Premature reclamation of a page held by an active
    reader's snapshot lets the writer rewrite it — the reader
    observes a half-written page; or, after crash recovery adopts
    the durable sub-record, the bitmap is inconsistent with the
    recovered tree, surfacing as wrong tree traversal results.

Invariant: kind=entailed;
  property=The on-disk bitmap, after a successful commit in
    `SyncDurable` or `SyncDataOnly`, is consistent with the active
    meta's reachable tree set: every page reachable from the active
    meta has bit clear; every page in the RPL has bit clear; all
    other pages below `HighWaterMark` have bit set;
  from=entailed: tree + RPL + bitmap commit ordering
    (`pager-slab.md` + this spec);
  violation=A bitmap that contradicts the tree set lets the
    allocator hand out an in-use page (false free) or refuses to
    allocate a genuinely free one (false used → file growth +
    spurious `ErrDBFull`). The load-bearing reachable case is a row
    mutation failing AFTER the btree retired prior-tx pages but
    before its last fallible allocation, followed by Commit (the
    rest-of-tx-continues contract): the commit would publish
    still-referenced pages to the RPL (reclamation later hands live
    tree pages to the allocator) and durably leak the mutation's
    orphaned allocations. Every keyspace-layer row mutation
    therefore runs under a shallow pager savepoint restored on the
    op's error path, indexed or not.
    (Enforced by the unconditional `BeginShallowSavepoint` in
    `Keyspace.Put`/`Delete`, `Cursor.Delete`,
    `SetKeyspace.Put`/`Delete`/`DeleteValue`, and
    `deleteRangeUnindexed`; pinned by
    `TestFailedOpsLeavePagerOpStateUnchanged` and
    `TestCommitAfterFailedOpsSurvivesReclamation` in
    `op_failure_rollback_test.go`.)

Invariant: kind=entailed;
  property=`NumFreePages` in the meta equals the bit-count of
    free-pages in the on-disk bitmap *as of that meta's commit*. In
    `SyncLazy`, post-checkpoint commits may leave the on-disk
    bitmap with set bits not yet reflected in `NumFreePages`;
    `Check()` recomputes from the bitmap and reports any
    discrepancy as `CheckWarning`;
  from=entailed: meta atomicity + SyncLazy partial-flush note;
  violation=Trusting `NumFreePages` without bitmap cross-check
    after a `SyncLazy` crash gives the user wrong free-space
    accounting in `DBStats` and miscalibrates background
    compaction's failure-rate threshold.

Invariant: kind=clause-explicit;
  property=RPL reclamation consumes **whole segments** — clean
    boundary since each segment header carries a single `TxnID`
    shared by every entry in the segment. Partial-segment
    modification is forbidden;
  from=this spec §RPL Reclamation;
  violation=Partial-segment writes break the "segments are
    immutable after publish" guarantee — concurrent readers can
    observe a segment whose entries no longer match its `TxnID`,
    misclassifying still-pinned pages as reclaimable.

Invariant: kind=entailed;
  property=Every walk of the persisted RPL chain (the in-memory
    rebuild at Open, `Check`'s RPL validator) traverses exactly the
    live segments `RPLHeadPage … RPLTailPage` and terminates *at*
    `RPLTailPage` — never following the tail segment's `OlderSegment`,
    which dangles at a reclaimed page once any tail segment has been
    reclaimed. `OlderSegment == 0` is not the terminator;
  from=entailed: §RPL Reclamation advances `RPLTailPage` and leaves the
    new tail's `OlderSegment` unrewritten, but no clause states the
    walk terminator is `RPLTailPage` rather than `OlderSegment == 0`;
  violation=After any tail segment is reclaimed, the new tail's
    `OlderSegment` points at a freed page; once that page is reused
    (any bitmap-exhausting workload — delete-heavy churn, or background
    compaction), an `OlderSegment == 0`-terminated walk reads it as a
    segment → `ErrCorrupted` at Open (the database becomes UNOPENABLE)
    / `RPLSegmentMalformed` at Check, with every other invariant intact.

Invariant: kind=clause-explicit;
  property=A **loose** page (CoW'd then freed within the same
    transaction) is reusable within that transaction only and
    bypasses the RPL: at commit it goes directly to
    `tx.pendingFrees` because no other process can hold a snapshot
    pinning it;
  from=this spec §Loose Pages + §Commit-Time Free Space Update;
  violation=Routing a loose page through the RPL costs a segment
    entry for a page that needed none, and (worse) a loose page
    placed in `tx.retiredPages` could survive into a snapshot the
    same transaction's commit publishes — temporary allocation
    aliasing.

Invariant: kind=clause-explicit;
  property=Tail-page refund (lowering `HighWaterMark` past tail
    pages with bits set in the bitmap) clears those bits *before*
    the meta-page pwrite that advertises the new `HighWaterMark`;
    pages held by any active reader cannot be tail-refunded because
    their bits are not set (they remain in the RPL until the
    reclamation bound advances);
  from=this spec §Tail Page Refund;
  violation=A tail refund that "frees" a still-referenced page,
    then `ftruncate`s past it, SIGBUSes the reader on next access
    (the page is no longer mapped) — defeats reader isolation.

## Allocation Bitmap

A flat bitfield — one bit per page in the database. **Set bit** =
free and safe to allocate. **Clear bit** = in use OR retired but
not yet reclaimable.

Stored in a contiguous region starting at page 2. Number of bitmap
pages fixed at creation (see `file-layout.md`):

```
BitmapPages  = ceil(MaxSize / PageSize / BitsPerPage)
BitsPerPage  = PageSize * 8
```

| MaxSize | PageSize | Total Pages | BitmapPages | Bitmap Size |
|---------|----------|-------------|-------------|-------------|
| 1 GB | 4 KB | 262 144 | 8 | 32 KB |
| 64 GB | 4 KB | 16 777 216 | 512 | 2 MB |
| 256 GB | 4 KB | 67 108 864 | 2 048 | 8 MB |
| 1 TB | 4 KB | 268 435 456 | 8 192 | 32 MB |

Bitmap pages are never marked free in the bitmap (bits permanently
clear). Same for meta pages. Data pages start at `2 + BitmapPages`.

### Bitmap storage

The bitmap is stored in the data file and accessed via the mmap
(read path) and pwrite (write path). Bitmap modifications are
deferred in memory: `tx.pendingAllocs` tracks pages allocated during
the transaction (bits to clear at commit); `tx.pendingFrees` tracks
pages freed (bits to set at commit). At commit, modified bitmap
pages are pwritten before the meta page. Bitmap pages on disk are
only ever modified via the ordered pwrite + fdatasync of
`pager-slab.md §Commit Write Ordering` — never via the mmap (which
is read-only in all processes).

Bitmap pages do not use the standard page header. The entire page
is usable as bitmap data. The page type is identified by position
in the file (pages 2 through `2 + BitmapPages - 1`).

### Two-level summary

The bitmap uses a two-level structure for fast allocation searches:

- **Level 0 (detail).** One bit per page in the database, across
  bitmap pages 2 through `2 + BitmapPages - 1`.
- **Level 1 (summary).** In-memory `[]uint64`, one bit per `uint64`
  word of the detail level. Set if the corresponding 64-page word
  has any set bits. Size: `ceil(TotalPages / 64 / 64)` `uint64`
  words. Rebuilt from detail at Open and maintained incrementally.

At 4 KB pages with 256 GB `MaxSize` (67 M pages): detail is ~1 M
`uint64` words (8 MB); summary is ~16 K `uint64` words (128 KB in
memory). The summary lets allocation scans skip 64-page regions in
one read.

For contiguous-run searches (overflow allocation), the writer scans
summary words for regions with free pages, then scans detail words
within using `math/bits.TrailingZeros64` / `LeadingZeros64`. A
single `uint64` word covers 64 pages — runs `N < 64` fit within one
word; larger runs span word boundaries via a carry-forward scan.

### Bitmap operations

- **Set bit (free a page).** Load uint64 word, OR in the bit, write
  back. Update summary if word transitioned `0 → non-zero`. O(1).
- **Clear bit (allocate a page).** Load word, AND out bit, write
  back. Update summary if word transitioned `non-zero → 0`. O(1).
- **Find first free (single-page alloc).** Scan summary from the
  LIFO hint for a non-zero word, then scan detail words within.
  Clear and return. O(1) amortized with hint; O(TotalPages/64)
  worst case.
- **Find N contiguous free.** Scan detail words for runs of
  consecutive set bits. `math/bits.TrailingZeros64` on the
  complement finds run length from LSB. Across word boundaries,
  track trailing run of one word + leading run of next. O(scanned
  words).
- **Count free pages.** `math/bits.OnesCount64` (hardware `popcnt`)
  across all detail words. Cached in `NumFreePages` in the meta
  page.

## Retired Page Log (RPL)

The RPL tracks which pages were freed by which transaction — needed
for MVCC safety: a page freed by transaction `T` cannot move into
the allocation bitmap until no active reader holds a snapshot `≤ T`.

Append-only singly-linked list of segment pages. Each segment stores
a single `TxnID` plus an array of `PageID`s. New segments are
appended at commit time; existing segments are never modified.
Storing the TxnID once per segment doubles capacity.

```
RPL Segment Page
+--------------------------+
| Page Header (8 bytes)    |  Count = N (number of PageID entries)
+--------------------------+
| TxnID          | uint64  |  transaction that retired these pages
| OlderSegment   | uint64  |  page ID of the next older segment (0 only on the original tail; see below)
+--------------------------+
| PageID 0       | uint64  |
| PageID 1       | uint64  |
| ...                      |
+--------------------------+
```

The page-header `Count` field carries the entry count for RPL segments
(per `file-layout.md §Page Header`: "Count ... entry count for RPL
segment"). No separate count field exists; the encoder writes `Count`,
the decoder reads it.

Segment capacity at 4 KB: 8 (header) + 8 (TxnID) + 8 (link) = 24
bytes overhead. Remaining `4096 - 24 = 4072` / 8 = **509 entries per
segment** (508 with checksums enabled, due to the 8-byte xxhash64
footer: `4096 - 24 - 8 = 4064` / 8 = 508).

Meta stores `RPLHeadPage` (newest) and `RPLTailPage` (oldest).
Segments are singly linked head → tail via `OlderSegment`. The
tail-toward-head direction is maintained as an in-memory segment
list rebuilt at Open.

**`RPLTailPage` is the authoritative tail boundary, NOT `OlderSegment
== 0`.** Reclamation drains whole segments from the tail and advances
`RPLTailPage`, but does *not* rewrite the surviving new tail's on-disk
`OlderSegment` (that would mean CoW-ing an immutable prior-snapshot
segment page on every reclaim). So once any tail segment has been
reclaimed, the new tail's `OlderSegment` points at a now-reclaimed —
and possibly reused — page; it is stale and MUST NOT be followed. Every
consumer that walks the chain (the in-memory rebuild at Open, `Check`'s
RPL validator) walks `RPLHeadPage → … → RPLTailPage` and stops *at*
`RPLTailPage`, never following the tail's `OlderSegment`. `OlderSegment
== 0` holds only for a chain whose original tail has never been
reclaimed; it is not a safe walk terminator.

**Recovery to the durable epoch: the sub-record's RPL pointers may be
stale.** `RPLTailPage` is the authoritative boundary only for the
*live* meta. Reclamation advances the live meta's `RPLTailPage` and
frees the drained segment pages, but the durable sub-record adopted by
crash recovery names the RPL as of the durable epoch — reclamation
since then (bound = `min(oldestReader, anchoredEpoch)`, anchored <=
durable, so segments
retired by transactions *before* the epoch are eligible) may have
freed and reused segment pages the sub-record's `RPLHeadPage → … →
OlderSegment` path runs into. The recovery rebuild therefore walks the
head forward and stops at the **first reclaimed segment** — a segment
page that is free in the bitmap (set bit), or a non-owned page that
fails its checksum footer or no longer decodes as a segment
(reclaimed-then-reused) — in addition to stopping at `RPLTailPage`.
This is safe and consistent: the reclamation bound guarantees the
recovered *tree* pages are intact (the durable epoch's tree never
references pages retired before it), and a reclaimed segment's listed
data pages are already free in the bitmap, so truncating the in-memory
chain at the reclaimed boundary yields an RPL consistent with the
bitmap.

**Head classification requires the persisted head TxnID.** The head
segment is exempt from the reclaimed-boundary treatment — a
failing head is a hard error, not a stale tail — exactly when it
cannot have been legitimately reclaimed: `RPLHeadTxnID >=
DurableTxnID` of the projection being walked (the bound never exceeds
the durable epoch, and reclamation frees only segments strictly below
the bound). This covers the durable projection's own head
(`DurableRPLHeadTxnID == DurableTxnID` — the epoch's commit appended
it) and a live projection's post-epoch head (`RPLHeadTxnID >
DurableTxnID` — appended by an unfsynced later commit, unreclaimable)
uniformly. A **carried-forward** head (`RPLHeadTxnID <
DurableTxnID` — the meta's commit retired nothing and re-pointed at an
older segment) is reclaimable like any non-head segment and gets the
same truncate-at-reclaimed treatment. The classification MUST come from the
meta's persisted `RPLHeadTxnID`, never from decoding the head page:
a reclaimed-and-reused head page's bytes are arbitrary, so its decoded
TxnID cannot be trusted, and a torn head cannot be decoded at all.
Both on-disk chain walkers apply all of this — the in-memory rebuild
at Open and `Check`'s RPL validator — since either may run on a
recovered state in the window between a truncating reopen and the next
commit (which is the first write to persist a corrected `RPLTailPage`).
Without the reclaimed-boundary rule, recovery to the durable epoch
under `SyncLazy` fails at Open with a malformed-RPL-segment error;
without the ownership condition on the head exemption, a
carried-forward head that was legitimately reclaimed and reused makes
the database permanently unopenable (hard error at Open on a healthy
file).
Lands: chunk 8 of docs/plans/architecture-consolidation.md (the
ownership condition and `RPLHeadTxnID`; the reclaimed-boundary walk is
already enforced by the shared chain walker).

Accepted trade: a genuine bitrot of a *live* (bitmap-clear) non-head segment
also fails to decode and is treated as this boundary, so it degrades to a
bounded page leak — the orphaned older segments surface as a `BitmapLeak` in
`Check` — rather than a hard `ErrCorrupted`. Distinguishing reclaimed-reuse
from live bitrot without a per-segment discriminator is not possible, and a
bounded, detectable leak is strictly safer than refusing to open an otherwise
intact database.

**Runtime reclamation (`reclaimRPL`).** When the writer drains eligible
segments back to the bitmap and a segment fails to decode (bitrot, a torn
write), reclamation **quarantines** it rather than halting: the corrupt
segment is popped from the in-memory chain and reclamation continues with
the newer eligible segments behind it. Each segment's reclaimability is
independent (a segment retired at TxnID `T` is reclaimable once the bound
passes `T`, regardless of older segments), so skipping one torn segment
does not strand the free space of every newer one. The quarantined
segment's listed pages and its own segment page cannot be reclaimed
(undecodable / unsafe to free), so they leak — bounded to that one
segment, and recoverable by a `Check()`/`Repair` structural walk (they
surface as `BitmapLeak`). The corruption is **observable without an
explicit `Check()`**: `DBStats.RPLCorruptSegments` counts quarantined
segments and the DB logs a warning (naming the segment page) on each. A
growing file or an `ErrDBFull` while `RPLCorruptSegments > 0` is a
corruption symptom, not genuine capacity exhaustion.

### RPL append (at commit time)

When a write transaction commits with retired pages:

1. Allocate one or more new segment pages from the bitmap (or via
   file extension). Each commit creates new segment pages —
   existing segments are never modified (they belong to previous
   snapshots).
2. Fill segments with the current TxnID in the header and PageID
   entries sorted by page ID. If the retired list exceeds one
   segment's `EntriesPerSegment` capacity, allocate additional
   segments linked via `OlderSegment`.
3. Set the new head's `OlderSegment` to the old `RPLHeadPage`.
4. Update `RPLHeadPage` (and `RPLTailPage` if RPL was empty).
5. Append the new segment page IDs to the in-memory segment list.

A transaction retiring N pages needs at most
`ceil(N / EntriesPerSegment)` segment allocations. Bounded,
non-recursive.

### RPL reclamation

At the start of a write transaction (or lazily on first
`pageAlloc()`), the writer reclaims RPL entries safe to reuse:

1. Compute the **reclamation bound**:
   `min(oldestActiveReaderTxnID, anchoredEpoch)` — the anchored
   epoch being the newest fsync-covered `DurableTxnID` assertion
   (`durability.md §Anchoring`): the active meta's
   `AnchoredDurableTxnID`, or the writer's own newer in-process
   anchoring knowledge, or — on a freshly-recovered handle — the
   recovered meta's `DurableTxnID` itself (disk reads are anchored
   by definition). In `SyncDurable`, every commit anchors itself
   (bound keeps pace). In `SyncDataOnly`, anchoring trails by one
   commit. In `SyncLazy`, the anchored epoch advances only at
   fsync events, restricting reclamation.
2. Walk the in-memory segment list from **tail** (oldest first).
3. For each segment with `TxnID < reclamationBound`: set bitmap
   bits for all PageIDs in the segment.
4. When a segment is fully reclaimed, free the segment page itself,
   remove it from the in-memory list, advance `RPLTailPage` to the new
   oldest segment. The new tail's on-disk `OlderSegment` is left as-is
   (it now dangles at the reclaimed page) — `RPLTailPage` is the
   authoritative boundary, so the dangling link is never followed.
5. Update `RPLEntryCount` and `NumFreePages`.

Reclamation is oldest-first so the RPL shrinks from the tail.
Empty segments are immediately freed. Whole-segment consumption is
mandatory (Invariants).

**Why the bound is sufficient.** Suppose the bound's epoch term is
`C` (an ANCHORED epoch: its assertion is on stable storage, so no
crash can make recovery adopt an epoch older than `C` —
`durability.md §Anchoring`). Reclamation at any later TxnID `T > C`
uses
`bound = min(oldestReader, C) <= C`, so it can only set bitmap bits
for pages freed by transactions with `TxnID < C`. Those pages were
freed *before* the durable epoch, so the epoch's tree (the durable
sub-record's) does not reference them. If a crash forces recovery to
adopt a sub-record at or after `C` (anchoring guarantees never
before it), the on-disk bitmap may show those pages
as free (reclaimed between `C` and the crash) or as not-yet-reclaimed
(still in the RPL at `C`) — either is consistent with `C`'s tree,
because `C`'s tree never referenced them. Pages freed *at or after*
`C` (`TxnID >= C`) are excluded by the bound, so the bitmap never
gains spurious free-bits for pages the recovered tree might still
reference.

**Cross-process derivation of `C`.** The anchored epoch is
self-describing on disk: a writer acquiring the grant after a peer's
commits reads `AnchoredDurableTxnID` from the meta it just re-synced
to (monotone per `durability.md §Invariants`, so the active meta's
value is the newest persisted one), with no flag scan to reconcile.
The live writer may hold newer in-process anchoring knowledge (its own
completed step-4/Checkpoint fsyncs), which is always at least as
conservative on disk — an assertion it anchored is on stable storage,
so the sufficiency argument holds either way.

**SyncLazy and partial bitmap flush.** In `SyncLazy` the bitmap
pwrites for transactions past the durable epoch happen
without `fdatasync`. The OS may flush some pwrites and not others.
On recovery to the durable epoch `C` the on-disk bitmap can be in a
partial state: some pages freed by transactions in `(C, crash]` may
have their bits set on disk; others may not. The argument above
guarantees the *tree* is safe, but `NumFreePages` (last written at
`C`) may disagree with the actual on-disk bit count. Background
maintenance's bitmap-leak reclamation pass walks the tree from
`C`'s roots, identifies pages allocated-but-unreferenced
(partial-flush leaks), and either sets their bits free or
recomputes `NumFreePages` from the current bitmap. `Check()`
recomputes the free count from the bitmap bits via
`math/bits.OnesCount64` and reports any discrepancy as
`CheckWarning`.

### Reclamation-bound derivation points

Scanning the reader table is O(MaxReaders), so the writer derives
the bound `min(oldestActiveReaderTxnID, anchoredEpoch)`
(`durability.md §Anchoring`) at
fixed points rather than per allocation: once at write-transaction
start (seeding the transaction's bound), and re-derived on the two
paths that want a fresher value — eager reclamation at the start of
an incremental-compaction transaction, and the lagging-reader Wait
retry. Every derivation runs under the write grant (the reader-table
scan's LOCK_EX precondition).

Any re-derived value is safe by construction: it is computed from
the live reader table, so it never exceeds the true current bound;
reclamation frees only segments strictly below the bound; and
already-reclaimed segments have left the in-memory segment list, so
a transiently lower re-derivation (a reader publishing its slot
just after taking its meta snapshot can briefly lower the scan)
only delays reclamation — it can neither un-reclaim nor
over-reclaim.

### RPL in-memory segment list

On-disk RPL is singly linked head → tail. Reclamation walks tail →
head. To avoid full-chain traversal, the writer maintains an
in-memory `[]uint64` of segment page IDs ordered tail (index 0) to
head (last).

Rebuilt at `Open()` by walking the on-disk chain `RPLHeadPage → … →
RPLTailPage` (following `OlderSegment`, stopping *at* `RPLTailPage` —
never following the tail's possibly-dangling link) then reversing.
O(RPL segments) — typically tens to low hundreds. A chain that reaches
`OlderSegment == 0` before hitting `RPLTailPage`, or exceeds the
`RPLEntryCount` bound, is corrupt meta and aborts the Open.

Maintained incrementally:

- **Append** (commit with retired pages): new segment IDs appended.
- **Reclaim** (tail consumption): consumed segment IDs removed
  from the tail (slice index 0).

Stored on the `DB` struct; access guarded by the write lock.

## LIFO Allocation Locality

The bitmap doesn't inherently provide LIFO. The writer maintains a
**LIFO hint** (`tx.allocHint`) — the page ID of the last page
reclaimed during the most recent reclamation pass. `pageAlloc()`
begins its scan at this hint. Reclamation walks RPL oldest → newest,
so the last entries processed are most-recently-freed pages — the
hint naturally points to recently-freed regions.

For workloads with steady write/free/reuse cycles, this keeps the
active page set small and concentrated.

## Loose Pages

Pages CoW'd and then freed within the **same write transaction**.
Common during rebalancing: a merge CoWs a node then frees one of
two originals.

Tracked in a hash map (`tx.loosePages map[uint64]struct{}`) for
O(1) insert/lookup/delete. The hash map is required because tail-
page refund does up to `n` membership lookups by page ID against
`n` loose pages — O(n²) with a slice vs. O(n) with a map.

Loose pages are immediately reusable within the same transaction:

- `pageAlloc()` checks `tx.loosePages` first (single-page allocs).
- Loose pages reused via `pageAlloc()` never touch the bitmap or
  RPL.
- At commit time, any loose pages still in the map are added to
  `tx.pendingFrees` (bypass RPL — same-tx pages cannot be
  referenced by any reader).

## Page Allocation Priority

`pageAlloc(n)` allocates `n` contiguous pages in priority order:

1. **Loose pages** (n=1 only): pop from `tx.loosePages`. O(1).
2. **Allocation bitmap**: scan for free page (n=1) or contiguous
   run (n>1) starting at the LIFO hint.
3. **RPL reclamation**: if the bitmap is exhausted, reclaim entries
   with `TxnID < bound`.
4. **Lagging-reader check**: if reclamation is blocked by a
   long-lived reader, invoke the `LaggingReader` callback (see
   `lock-ordering.md`).
5. **File extension**: if no free pages, grow per `file-format.md`
   and advance `HighWaterMark`.

## Tail Page Refund

After reclamation or at commit time, the writer checks whether any
tail pages (`HighWaterMark - 1`, `HighWaterMark - 2`, …) are free
in the bitmap. If so, the corresponding bits are cleared and
`HighWaterMark` is decremented. Reclaims file space and enables
file shrinkage at commit. Iterates until no more tail pages are
free.

**Safety with concurrent readers.** Tail refund only decrements
`HighWaterMark` for pages free in the bitmap. Pages held by an
active reader remain in the RPL until the reclamation bound allows
their reclamation; tail refund cannot remove pages a reader
references. File shrinkage via `ftruncate()` only truncates pages
beyond `HighWaterMark`.

## Freeing Pages

When a CoW replaces an old page with a new copy:

- If the old page was **CoW'd in this transaction** (already a CoW
  copy from earlier in this txn), it becomes a **loose page** —
  added to `tx.loosePages`.
- If the old page was **from a previous transaction** (an immutable
  page accessible via mmap), its page ID is added to
  `tx.retiredPages` — appended to the RPL at commit time (TxnID
  stored once per RPL segment).

Retired pages are NOT immediately marked free. They enter the RPL
and move to the bitmap only when reclamation deems them safe (no
active reader holds their snapshot).

## Commit-Time Free Space Update

The free-space side of commit happens entirely inside step 0 of
`pager-slab.md §Commit Write Ordering`. Work:

1. Tail-page refund: check the bitmap for tail free pages,
   decrement `HighWaterMark`.
2. Move remaining loose pages into `tx.pendingFrees` (bypass RPL).
3. Append all `tx.retiredPages` to the RPL by allocating new
   segment pages and appending to the in-memory segment list. The
   newly allocated segment pages enter `p.dirty` and are flushed
   by step 1 of commit alongside data and bitmap pages.
4. Update `NumFreePages`, `RPLHeadPage`, `RPLTailPage`,
   `RPLEntryCount` in the new meta-page buffer.

Sub-step 3 may allocate RPL segments from the bitmap — bounded,
non-recursive. A transaction retiring N pages needs
`ceil(N / EntriesPerSegment)` allocations.

If the bitmap has no free pages and file extension would exceed
`MaxSize`, RPL segment allocation fails and commit returns
`ErrTxTooLarge` or `ErrDBFull` from step 0 — no pwrite has happened,
so rollback is clean.
