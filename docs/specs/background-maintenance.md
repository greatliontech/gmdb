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
  property=A page identified as leaked in the bitmap-leak
    detection phase's read snapshot cannot become un-leaked
    by the time the write transaction runs: the leaked-page
    set is a function of the snapshot's tree, and that tree
    is immutable for the duration of the read transaction;
  from=this spec §Bitmap Leak Reclamation;
  violation=Reclaiming a "leaked" page that has since become
    referenced by the active tree hands the page to two
    consumers — the same allocator/aliasing bug
    `free-space.md` invariants forbid.

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
  property=`CheckWarning` produced by the scrubber identifies
    the affected page ID and is logged via `slog.Logger`;
    no scrub-detected corruption is silently dropped;
  from=entailed: §Checksum Scrubbing + `api-surface.md
    §Check, CopyTo, Compact`;
  violation=A silently-dropped warning leaves the operator
    unaware of bitrot until it surfaces as
    `ErrBadPageChecksum` on a user read — the proactive
    intent of the scrubber breaks.

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

1. Open a write transaction.
2. For each leaked page, set its bitmap bit.
3. Commit.

**Safety.** A leaked page is permanently stuck — its bitmap
bit is clear so no future transaction can allocate it, and no
tree references it. A page identified as leaked in the read
snapshot cannot become un-leaked by the time the write
transaction runs.

**Trigger.** Every maintenance pass. Additionally, if `Open()`
recovered from an unclean prior shutdown — signalled by accepting
a non-checkpoint-flagged meta (`pager.Open`'s `NoCheckpoint`, the
available recovery signal) — the first maintenance pass runs
immediately rather than waiting for the interval, to reclaim any
crash-leaked pages promptly. (This is an approximation of "a
fallback meta was selected": a clean reopen can also see
`NoCheckpoint` for a never-checkpointed SyncLazy database — a
harmless extra first pass — and some crashes that left a
checkpoint-flagged meta durable do not trip it, in which case
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

Each pass verifies `ScrubBatchSize` pages (default 4096) in a
read transaction, advancing through the file sequentially
across passes. A `ScrubCursor` on the DB tracks the next page
ID to verify, wrapping at `HighWaterMark`. A full scrub cycle
covers the database over
`ceil(HighWaterMark / ScrubBatchSize)` passes.

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
failure rate — the fraction of multi-page `pageAlloc(n)` calls
(`n > 1`) that fail to find a contiguous run on the first
bitmap scan despite sufficient total free pages. When this
rate exceeds `CompactionThreshold` (default 0.5), the
maintenance goroutine schedules compaction work.

**Mechanism.** Each pass opens a write transaction and
relocates up to `CompactionBatchSize` pages (default 1024):

1. Identify fragmented regions — pages that interrupt
   potential contiguous runs.
2. For each, CoW it to a new location (allocated from a
   region with more free neighbours).
3. The old page goes to the RPL and is reclaimed in a future
   txn.
4. Commit.

Over multiple passes, scattered pages consolidate and
contiguous free runs emerge. Converges when the failure rate
drops below the threshold.

**Cost per pass.** Each moved leaf forces a CoW cascade up the
tree (every ancestor branch needs CoW + new child pointer),
so worst-case I/O is `CompactionBatchSize × (1 + depth) ×
PageSize` plus `CompactionBatchSize × (1 + depth)` RPL entries
for the retired originals. At 1024 pages, depth 5, 4 KB
pages: ~24 MB of pwrite I/O per pass, ~6 K RPL entries (~12
segment pages at 508 entries/segment). Size
`CompactionBatchSize` against `MaxTxBufferBytes` accordingly
— the slab must hold the whole cascade plus assembly buffers
in step 0 of the commit. Bounded and amortised across the
maintenance interval.

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

    // CompactionThreshold triggers incremental compaction when the
    // contiguous-allocation failure rate exceeds this fraction.
    // Range: 0.0 (disabled) to 1.0. Default: 0.5.
    CompactionThreshold float64

    // CompactionBatchSize is the number of pages relocated per write
    // transaction during incremental compaction.
    // Default: 1024.
    CompactionBatchSize int
}
```

Maintenance is a fixed-cost resource: one goroutine per DB
handle. Same lifecycle pattern as the flock and heartbeat
goroutines. The explicit tools (`Check`, `CheckWithOptions`,
`Compact`, `CopyTo`) remain available for on-demand use.
