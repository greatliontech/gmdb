# Background Maintenance

The `DB` struct runs a **maintenance goroutine** (started at
`Open()`, stopped at `Close()`) that performs periodic housekeeping
to prevent issues from accumulating. Goal: avoid reaching a state
that requires offline intervention.

Scope:
- Cross-process maintenance coordination via the lock-file
  `LastMaintenanceTime`.
- Four maintenance tasks: bitmap leak reclamation, stale reader
  slot cleanup, checksum scrubbing, incremental compaction.
- `MaintenanceOptions`.

Depends on / interacts with:
- `cross-process.md` for the lock-file header field used to
  coordinate across processes.
- `free-space.md` for bitmap-leak semantics.
- `checksums.md` for the scrubbing target.
- `pager-slab.md` for the commit budget the compaction batch
  must fit inside.
- `api-surface.md` for `CheckWithOptions(Repair)`,
  `Compact()`, and `CopyTo(compact=true)` as the explicit
  on-demand alternatives.

## Invariants

Invariant: kind=clause-explicit;
  property=Across processes sharing a database, at most one
    maintenance pass runs per `MaintenanceOptions.Interval`.
    The lock-file header's `LastMaintenanceTime` is the
    coordination anchor;
  from=this spec §Coordination;
  violation=Concurrent passes from multiple processes either
    duplicate work (wasted I/O) or race on the same write txn
    (one process's pass discards another's) — efficiency loss
    in the best case, correctness loss in the worst.

Invariant: kind=clause-explicit;
  property=The reclamation write transaction frees a detected
    leaked set ONLY when no commit — this process's or a
    peer's, including a peer's own maintenance pass — landed
    after the detection snapshot was taken: it compares the
    current meta TxnID AND the writer-pager identity (a
    same-process Compact rebuilds the file densely and resets
    TxnID, so the TxnID term alone could spuriously pass)
    against the snapshot's — under the write grant, so no
    further commit can intervene — and discards the whole set
    on any change. The snapshot's tree alone
    does NOT make the set stable: detection classifies against
    the LIVE bitmap (which has no MVCC), so a page that was a
    free hole at snapshot time and was allocated by an
    intervening commit classifies as leaked;
  from=this spec §Bitmap Leak Reclamation (reclamation phase,
    snapshot-currency guard);
  violation=Reclaiming a "leaked" page that an intervening
    commit allocated into the live tree hands the page to two
    consumers — the same allocator/aliasing bug
    `free-space.md` invariants forbid (demonstrated: a commit
    landing inside the detection window made freshly-allocated
    live pages classify as leaked; reclamation freed them and
    Check reported ReachableButFree).
    (Enforced by the TxnID + pager-identity guard in maintReclaimLeaks;
    pinned by TestMaintenanceReclaimDiscardsStaleDetection.)

Invariant: kind=entailed;
  property=Leak reclamation — background maintenance and
    `CheckWithOptions(Repair)` alike — frees nothing when the
    detection walk's RPL chain walk stopped at a footer/decode
    boundary rather than reaching the authoritative tail or a
    reclaimed boundary. A reclaimed boundary is safe when every
    live handle's in-memory chain agrees with the on-disk
    projection — self-authored (the ordinary single-writer case:
    the image derives from the chain), or rebuilt by a walk over
    the current image (crash recovery at Open, re-sync after
    a TxnID advance, or the takeover-sequence-forced re-sync —
    the one case where a peer's torn, never-published
    reclamation could otherwise leave a surviving handle's
    chain predating the tear without a TxnID advance to
    trigger the rebuild;
    `free-space.md §Grant-handoff tear detection`).
    A footer/decode boundary is ambiguous — the segment
    may have bitrotted AFTER the live writer built its in-memory
    chain, in which case the still-pending segments behind it are
    invisible to the walk and their entries misclassify as
    leaked;
  from=entailed: §Bitmap Leak Reclamation Safety says "absent
    from the RPL" but no clause states which RPL — the walkable
    on-disk projection and the live writer's in-memory chain can
    diverge at exactly a corrupt segment;
  violation=Bitrot one byte of a middle RPL segment: the
    detection walk truncates there, an older INTACT segment's
    entries classify as leaked and get freed, a user transaction
    re-allocates one into the live tree, and the live writer's
    own reclamation later consumes the intact in-memory segment
    and frees the page under its new owner — double allocation,
    silent corruption (demonstrated pre-fix: maintenance freed 30
    pages behind the boundary).
    (Enforced by the rplBoundary gate in maintReclaimLeaks and
    checkRepair; pinned by
    TestMaintenanceReclaimSkipsBehindRPLBoundary and
    TestRepairSkipsBehindRPLBoundary.)

Invariant: kind=clause-explicit;
  property=The checksum scrubber only reports corruption; it
    does not repair. Repair is the explicit `CheckWithOptions
    (Repair)` or `CopyTo(compact=true)` path;
  from=this spec §Checksum Scrubbing;
  violation=Auto-repair in a background pass risks rewriting a
    page with content the scrubber cannot validate against —
    silent escalation from a detected corruption to an
    altered file the operator can no longer triage.

Invariant: kind=clause-explicit;
  property=Incremental compaction's per-pass page-budget
    (`CompactionBatchSize` × `(1 + depth)` × `PageSize` of
    slab usage) is bounded by `MaxTxBufferBytes`. A pass
    that would exceed the budget either reduces
    `CompactionBatchSize` for that pass or aborts and
    re-schedules — it never produces `ErrTxTooLarge` from
    the maintenance code path;
  from=this spec §Incremental Compaction (Cost per pass);
  violation=A maintenance pass that returns `ErrTxTooLarge`
    surfaces as a user-visible error log (and rollback +
    retry storm) when the *user's* writes have not changed
    — the maintenance budget must be a property the
    goroutine respects, not a user-facing trigger.

Invariant: kind=entailed;
  property=When an incremental-compaction pass commits, every
    relocated tree root is re-pointed at its holder — a moved
    keyspace data or index-registry root in that keyspace's
    descriptor, a moved index data-tree root in its registry
    entry, and a moved keyspace descriptor tree in
    `meta.KeyspaceRoot` — so the whole forest stays reachable.
    The descriptor tree is relocated last, after the per-keyspace
    descriptors are staged, so the staged re-puts land on the
    relocated tree;
  from=entailed: §Incremental Compaction step 2 says pages are
    "re-pointed" but no clause states the cascade must be
    complete across all four root-holder kinds;
  violation=A relocated root whose holder keeps the stale id is
    a dangling root: the subtree's old pages are retired to the
    RPL yet still referenced, so a later reclamation hands a
    live-referenced page back to the allocator — aliasing /
    data loss — while every other invariant still holds.

Invariant: kind=entailed;
  property=`CheckWarning` produced by the scrubber identifies
    the affected page ID and is logged via `slog.Logger`;
    no scrub-detected corruption is silently dropped;
  from=entailed: §Checksum Scrubbing + `api-surface.md
    §Check, CopyTo, Compact`;
  violation=A silently-dropped warning leaves the operator
    unaware of bitrot until it surfaces as
    `ErrBadPageChecksum` on a user read — the proactive
    intent of the scrubber breaks.

Invariant: kind=entailed;
  property=The scrubber footer-verifies only pages it can
    prove carry a footer: allocated pages (the snapshot
    bitmap's bit is clear) in `[firstData, HighWaterMark)`.
    The meta/bitmap region (`< firstData`) carries no
    xxhash64 footer (`checksums.md §Storage`), and a free or
    never-written page holds no valid footer — neither is
    verified;
  from=entailed: §Checksum Scrubbing says "verify data pages
    sequentially" but does not state which pages carry a
    footer; the gate is the assumption that makes that
    sequential scan coherent;
  violation=On any non-full database — free pages within
    `[firstData, HighWaterMark)`, i.e. the common case — an
    ungated sequential verify emits a spurious
    `BadPageChecksum` `CheckWarning` for every free page,
    flooding the log and burying real bitrot. The
    proactive-detection intent inverts into noise.

## Coordination

Multiple processes sharing the same database coordinate via a
`LastMaintenanceTime` field in the lock-file header (see
`cross-process.md §Lock File Layout`) — a `uint64` monotonic
clock value (`CLOCK_BOOTTIME` on Linux). Before a pass, the
goroutine **atomically claims** the interval: if no pass has run
within `MaintenanceOptions.Interval`, a single CAS stamps
`LastMaintenanceTime` to the current time and the claimer runs;
otherwise it skips. The claim is at pass *start* (not completion):
a check-then-run-then-stamp-after design has a TOCTOU window where
two processes both observe a stale timestamp, both pass the check,
and both run — violating "one pass per interval". The atomic
claim-at-start closes that window, so the CAS winner is the sole
runner for the interval. (Consequence: a pass that runs longer than
`Interval` measures the next interval from its start, becoming
re-claimable as soon as it finishes — benign.)

On Linux `CLOCK_BOOTTIME` is kernel-wide, so all processes sharing
the file see one clock origin. On platforms with a per-process
monotonic clock (darwin/freebsd) the claim degrades to best-effort
(an occasional redundant pass) but never a double-reclaim — the CAS
serialises any single instant and reclamation is leak-safe.

## Tasks

Four tasks per pass.

### 1. Bitmap Leak Reclamation

Reclaims pages allocated in the bitmap but unreferenced by any
tree structure — "leaked" pages caused by crashes between
bitmap pwrite and meta pwrite, or by slab-flush partial writes
interrupted by ENOSPC.

**Detection phase** (read transaction, non-blocking):

1. Open a read transaction.
2. Walk the full tree (all keyspaces incl. internal index
   keyspaces, per-keyspace index registries, RPL segments,
   overflow pages) to build the set of all referenced page
   IDs.
3. Scan the bitmap. Any page with its bit clear (allocated)
   that is not in the referenced set and is not meta /
   bitmap / RPL is leaked.
4. Close the read transaction.

**Reclamation phase** (write transaction):

1. Open a write transaction (holds the write grant; its re-sync
   observes the latest committed meta).
2. Snapshot-currency guard: if the current meta TxnID differs
   from the detection snapshot's, OR the writer-pager identity
   changed (a same-process Compact rebuilt the file and reset
   TxnID), activity landed during or after detection — discard
   the whole set (the next pass recomputes). This is also what
   makes overlapping cross-process maintenance passes safe: a
   peer's reclamation commit advances the TxnID.
3. For each leaked page, set its bitmap bit.
4. Commit.

**Safety.** A genuinely leaked page is permanently stuck — its
bitmap bit is clear so no future transaction can allocate it,
and no tree references it. That argument covers only pages
already leaked at snapshot time; because detection classifies
against the LIVE bitmap, the snapshot-currency guard (step 2)
is what makes the set trustworthy — see the reclamation
invariant above. It also presumes the detection walk saw the
WHOLE RPL: the walk must have reached the authoritative tail or
a reclaimed boundary (the latter safe only under the
walker-agreement precondition the invariant above qualifies).
A footer/decode truncation means segments
behind the boundary — possibly still pending in the live
writer's in-memory chain — were invisible, so their entries
misclassify as leaked; reclamation then frees nothing (the set
becomes reclaimable after the writer's own reclamation
quarantines the corrupt segment and the chain no longer claims
it). `CheckWithOptions(Repair)` applies the same gate: its
exclusive access serialises writers but cannot make a truncated
walk complete.

**Trigger.** Every maintenance pass. Additionally, if `Open()`
recovered to a durable epoch older than the selected meta's own
commit — `DurableTxnID < TxnID`, the available recovery signal for
"unfsynced `SyncLazy` commits were rolled back" — the first
maintenance pass runs immediately rather than waiting for the
interval, to reclaim any crash-leaked pages promptly. (This is an
approximation of "state was rolled back": a FAILED or killed Close
trips it too — a clean Close checkpoints, making the final meta
self-durable (`durability.md §Clean shutdown`) — a harmless extra
first pass; and a crash that lost only the final meta pwrite
recovers to a self-durable meta and does not trip it, in which case
reclamation waits for the regular interval.)

### 2. Stale Reader Slot Cleanup

Proactively scans the reader table and clears slots owned by
dead processes. Same namespace-aware logic as the writer's
stale-reader scan (see `cross-process.md §Reader Table`):
same-namespace uses PID + StartTime, cross-namespace uses
heartbeat timeout.

No write *transaction* is needed — clearing a slot is a single
atomic store (`TxnID = 0`) on the shared mmap, independent of the
data file. But the scan still **acquires the write lock
(`flock(LOCK_EX)`)** for its duration, the same exclusivity the
writer's RPL-reclamation scan relies on. This is not an
optimisation that can be dropped: the *decision* to clear must be
serialised against every other clearer (a peer process's
RPL-reclamation scan, stale-writer recovery). Two unsynchronised
clearers could race the orphan-anchor CAS / clear stores and evict
a slot a live reader just acquired (mid-publish, before its `PID`
store), after which the writer's RPL reclamation advances its bound
past that reader's snapshot and frees pages it is still reading. A
lock-free scan is therefore unsafe by construction.

**Why this matters.** The writer already clears stale slots
during RPL reclamation, but only when it needs free pages.
If no writer is active for an extended period, stale slots
from crashed containers sit indefinitely, blocking RPL
reclamation for the next writer. Proactive cleanup keeps the
reader table clean.

### 3. Checksum Scrubbing

When `PageChecksum` is enabled, the maintenance goroutine
performs a background read-only scan that verifies xxhash64
footers on data pages proactively — before they are accessed
by a user transaction. Catches silent bitrot early.

Each pass scans up to `ScrubBatchSize` page IDs (default 4096)
in a read transaction, advancing through the data region
sequentially across passes. A `ScrubCursor` on the DB tracks
the next page ID, wrapping at `HighWaterMark`. A full scrub
cycle covers the data region over
`ceil((HighWaterMark - firstData) / ScrubBatchSize)` passes.

The scan footer-verifies only **allocated** pages (the
snapshot bitmap's bit is clear) in `[firstData,
HighWaterMark)` — data and RPL segment pages, both of which
carry a footer (`pager-slab.md §Commit`). Free page IDs in
that window are advanced over but not verified — a free page
holds no valid footer. The meta/bitmap region (`< firstData`)
is excluded entirely: those pages carry no xxhash64 footer
(`checksums.md §Storage`).

The scrubber is **best-effort and report-only**, not a
substitute for `Check`. The allocated/free gate uses a bitmap
snapshot copied once at pass start (consistent for the whole
pass); page content is read live through the read
transaction's mmap. Pages allocated in this reader's snapshot
are stable — reachable ones are pinned
by the read transaction's slot; leaked ones are absent from
the RPL, so neither is reclaimed under the reader. But a page
a *newer* concurrent writer allocates in a hole below the
snapshot's `HighWaterMark` is not pinned, so the scan can
momentarily observe its in-flight write as a torn page. A
mismatch is therefore **re-verified once** (a transient torn
read clears on re-read) before a warning is emitted. Genuine
bitrot — or a page allocated via the low-level `Tx.AllocPage`
and committed without ever being written, which carries no
footer — persists across the re-read and is reported
truthfully. `Check` / `CheckWithOptions(Repair)` remains the
authority for confirming and repairing.

Detected corruption is logged via `slog.Logger` as
`CheckWarning` with the affected page ID. The scrubber does
not repair — only reports. Repair: `CheckWithOptions(Repair)`
or `CopyTo(compact=true)`.

Skipped when `PageChecksum` is not enabled.

### 4. Incremental Compaction

Defragments the database by relocating pages in batches to
restore contiguous free runs for overflow allocation. Online
alternative to `Compact()`.

**Trigger.** The allocator tracks the contiguous-allocation
failure rate — the fraction of multi-page `AllocContiguous(n)`
calls (`n > 1`) whose first bitmap scan finds no contiguous run
despite sufficient total free pages (fragmentation, not
fullness). The counters are consumed (read-and-reset) once per
maintenance pass, so the rate reflects the most recent
interval's allocations and falls as compaction relieves
fragmentation. When the rate **exceeds** `CompactionThreshold`
the maintenance goroutine schedules compaction work.

`CompactionThreshold` is a rate in `[0,1]`: `0` is most
aggressive (compact on any fragmentation), `1` is least
(effectively never). Default `0.5`. The zero value defaults to
`0.5` — `0.0` is not a magic "disabled" value (it would be the
*most* aggressive setting). To disable compaction specifically
while keeping the other three tasks running, set
`DisableCompaction`; to disable all maintenance, set `Disable`.
A pass with no multi-page allocations since the last (no
signal) does not trigger.

The counters include *both* user-driven and maintenance-internal
allocations — `relocateOverflowChain`'s `AllocContiguous(runLen)`
during compaction bumps the same `contigAttempts` /
`contigFragFails`. This is intentional rather than a metric
defect: a fragmented forest typically needs more than one pass
to consolidate, and the maintenance-internal allocs landing in
the next pass's window keep the rate elevated until consolidation
succeeds — self-limiting, since once eager reclaim has
consolidated, `FindContiguous` succeeds and the rate falls below
the threshold. A per-tx "don't count" flag to exclude
maintenance-internal allocs would suppress that self-driving
behaviour for no correctness gain.

**Mechanism.** Each pass opens a write transaction, **eagerly
reclaims the RPL** (refreshes the reclamation bound and returns every
now-reader-safe retired page to the bitmap *before* relocating, rather
than waiting for the lazy on-allocation reclaim), and relocates up to
`CompactionBatchSize` pages (default 1024) by **high-watermark
evacuation**:

1. Choose an evacuation floor by EXACT feasibility scan over the
   allocation bitmap: the lowest floor whose band
   `[floor, HighWaterMark)` holds at most `CompactionBatchSize`
   allocated pages while the free capacity strictly below the
   floor covers the band twice over (CoW-cascade headroom) plus a
   reserve for the RPL chain-prefix relocation's homes and the
   commit's head-segment append. Every hole the pass may consume
   then lies strictly below every page it relocates, so every
   relocation is strictly DOWNWARD — the property the convergence
   argument below rests on. (A density ESTIMATE is not
   sufficient: a budget large relative to the region collapses an
   estimated floor to the first data page, allocation targets and
   sources interleave, and passes shuffle the same pages between
   hole sets forever.) No feasible floor ⇒ the pass declines
   relocation (see the shrink paragraph for what still commits).
   Walk every B+tree in the forest: the
   keyspace descriptor tree, each keyspace's data tree, its
   index registry sub-tree and index data trees, and any
   set-keyspace nested trees. RPL segment pages are **never
   relocated out-of-band** — they are owned by the commit pipeline
   (allocated, chained, and reclaimed there), and mutating the
   chain outside it would race that machinery. Instead, when the
   walk finds RPL segment pages at or above the floor, the pass
   REQUESTS an in-pipeline chain-prefix relocation, which its own
   commit executes as part of step 1 (`free-space.md §RPL segment
   relocation`): the full prefix from the deepest in-region segment
   to the head, with every copy placed below the floor or the whole
   request declined-and-reported for the pass (the decline and
   placement rules live in that section).
2. Relocate each allocated page at or above the floor: it is
   CoW'd to a fresh id, which the consolidating allocator draws
   from the LOWEST free hole strictly below the floor; every
   owning parent, descriptor, and `KeyspaceRoot` is re-pointed so
   the relocated subtree stays reachable (the same bottom-up CoW
   cascade a normal write uses). The allocator has NO fallback
   tier: it never extends the file (extension would place live
   pages at the file top, re-creating the band being drained) and
   never takes an at-or-above-floor hole (the refill pathology).
   Exhausting the below-floor capacity mid-pass aborts and rolls
   the pass back; the driver lands the reclaim/bound-advance
   commit described in the shrink paragraph AT MOST ONCE per
   maintenance invocation and retries, and every further
   exhaustion halves the batch — the earlier relocations then
   fit. The once-per-invocation cap is the termination bound: the
   reclamation bound moves at most once per commit and a
   reader-pinned bound does not move at all, so an unconditional
   retry would spin empty commits against frozen state; capped,
   the driver runs at most 1 + log2(batch) passes per invocation.
   (Convergence pinned by TestCompactionConvergesToQuiescence and
   TestAllocBelowFloor/TestEvacuationFloorExact; termination by
   TestRunCompactionTerminatesOnFrozenBound.)
3. The old page goes to the RPL and is reclaimed in a future
   txn.
4. Commit.

Over multiple passes the top band drains into a contiguous free
run, the live set consolidates toward low ids, and the file
shrinks. Converges because a relocated page receives a low id and
so stops matching the evacuation floor — successive passes make
monotone progress until the failure rate drops below the
threshold.

**Shrink is lazy but monotone (MVCC).** A relocated-from page goes
to the RPL (a concurrent reader may still hold the pre-relocation
snapshot), so it cannot be freed — and the tail cannot refund past
it — until the reclamation bound advances beyond this pass's commit,
i.e. one pass later. The eager reclaim at each pass start drains the
*previous* pass's relocated-from pages, so the trailing band frees up
and `HighWaterMark` falls steadily across passes. A pass NEVER
extends the file. The state that once forced an extension — large
frees not yet reader-safe (the reclamation bound has not advanced
past them, e.g. compacting in the same instant as a bulk delete with
no intervening commit), so nothing is reclaimable to relocate into —
is handled by a RECLAIM/BOUND-ADVANCE COMMIT instead: a pass that
declines relocation still commits whenever its eager reclaim freed
pages (landing the tail refund a rollback would discard) or the RPL
is non-empty (the commit advances the reclamation bound so the NEXT
pass's reclaim frees the last pass's retirees — the bound covers
only strictly-older transactions). One tiny commit per declined
pass, self-limiting once the RPL drains; suppressing it would freeze
the bound and stall a quiescent database at a permanent
HighWaterMark plateau. Online compaction therefore shrinks amortised
over passes, not instantly; `Compact()` remains the instant-shrink
path (it rebuilds into a fresh, RPL-less file).

**Cost per pass.** Two cost dimensions, separately bounded.

*Write cost.* Each moved leaf forces a CoW cascade up the tree
(every ancestor branch needs CoW + new child pointer), so
worst-case pwrite I/O is `CompactionBatchSize × (1 + depth) ×
PageSize` plus `CompactionBatchSize × (1 + depth)` RPL entries
for the retired originals. At 1024 pages, depth 5, 4 KB pages:
~24 MB of pwrite I/O per pass, ~6 K RPL entries (~12 segment
pages at 508 entries/segment). Size `CompactionBatchSize`
against `MaxTxBufferBytes` accordingly — the slab must hold the
whole cascade plus assembly buffers in step 0 of the commit.
Bounded by `CompactionBatchSize` and depth, independent of total
database size.

*Read cost.* Step 1's high-watermark scan walks every B+tree in
the forest to evaluate the floor predicate on each node (the
page format carries no per-branch min/max child-id metadata that
would let a subtree be pruned without descending, and on B+tree
nodes `shouldRelocate(id)` runs only after the page is read —
see `internal/btree/relocate.go`'s `relocateNode`; overflow
chains are gated the other way, predicate-then-relocate, with no
walk-time read of their pages). Cost is O(live B+tree node
pages), **workload-dependent**: a forest with more data has more
live nodes. Reads go through the mmap, so OS
page-fault latency dominates for cold pages; warm pages are
near-free. The structural ceiling (`MaxSize`/`PageSize`, a
fully-allocated database) is the only workload-independent
bound; there is no tighter constant-bound on the per-pass walk
because the page format does not support id-range pruning, and
bitmap-driven targeting would change the high-watermark strategy
itself.

Both dimensions are amortised across the maintenance interval.

## Options

```go
type MaintenanceOptions struct {
    // Disable disables the background maintenance goroutine.
    // Default: false (maintenance enabled).
    Disable bool

    // Interval is the minimum time between maintenance runs.
    // Coordinated across processes via the lock file.
    // Default: 5m.
    Interval time.Duration

    // ScrubBatchSize is the number of pages to verify per checksum
    // scrubbing pass. Only meaningful when PageChecksum is enabled.
    // Default: 4096.
    ScrubBatchSize int

    // CompactionThreshold is the contiguous-allocation failure rate
    // above which incremental compaction triggers. Range [0,1]: 0 is
    // most aggressive, 1 is least (effectively never). The zero value
    // defaults to 0.5 (0.0 is NOT a "disabled" sentinel — that would be
    // the most aggressive setting). Default: 0.5.
    CompactionThreshold float64

    // CompactionBatchSize is the number of pages relocated per write
    // transaction during incremental compaction.
    // Default: 1024.
    CompactionBatchSize int

    // DisableCompaction turns off incremental compaction (Task 4) only,
    // leaving leak reclamation, stale-reader cleanup, and checksum
    // scrubbing running. Compaction is the one task that rewrites live
    // data and amplifies writes, so disabling it specifically (vs all
    // maintenance via Disable) is a distinct, supported control.
    // Default: false (compaction enabled).
    DisableCompaction bool
}
```

Maintenance is a fixed-cost resource: one goroutine per DB
handle. Same lifecycle pattern as the flock and heartbeat
goroutines. The explicit tools (`Check`, `CheckWithOptions`,
`Compact`, `CopyTo`) remain available for on-demand use.
