# Cross-Process Coordination

Processes sharing a gmdb file coordinate via a separate **lock
file** (`<dbname>.lock`) mmap'd as shared memory. This spec defines
the lock-file layout, the write-lock protocol (intra-process queue +
cross-process flock), the reader table and its stale-detection
rules, and the heartbeat goroutine that drives cross-PID-namespace
liveness.

Scope:
- Lock file layout (header + reader slots) using `structs.HostLayout`.
- Lock file lifecycle (creation, validation, deletion).
- Write lock: flock goroutine, writer acquisition flow, stale
  writer recovery.
- Reader table: scan + CAS acquisition, atomic-ordered release,
  stale-reader detection with `HintEpoch` orphan anchor.
- Process start time as PID-reuse discriminator.
- PID namespace awareness.
- Heartbeat goroutine.
- Atomic-operations convention for shared memory.
- Writer's page reclamation entry point.

Out of scope (covered elsewhere):
- Lock acquisition ordering across all internal locks — see
  `lock-ordering.md`.
- Lagging-reader callback — see `lock-ordering.md §Lagging Reader
  Handling`.
- Reader-slot release coupling to leak detection — see
  `leak-detection.md`.

## Invariants

Invariant: kind=clause-explicit;
  property=The lock file's `UUID` matches the data file's `UUID`;
    mismatch causes the opener to treat the lock file as stale,
    delete it, and recreate. `MaxReaders` is read from the lock
    file header at Open and is the immutable per-database
    coordination capacity for the life of the lock file;
  from=this spec §Lock File Lifecycle;
  violation=Pairing a lock file from one database with a different
    data file lets readers/writers from two unrelated databases
    coordinate on the same shared memory — torn snapshots, missed
    writer-PID changes, and cross-database leakage.

Invariant: kind=clause-explicit;
  property=A reader slot is acquired by CAS on `TxnID` from `0` to
    the current meta's TxnID; immediately after a successful CAS,
    the acquirer writes `Heartbeat`, then `HintEpoch = 0`, then
    `PIDNamespace`, then `ProcessStartTime`, then `PID` — in that
    order. `PID = 0` is the discriminator for "mid-acquire" vs
    "owned";
  from=this spec §Reader Table (slot acquire);
  violation=Out-of-order field stores let a stale-reader scan run
    `kill(pid, 0)` against a PID the prior owner left behind,
    misclassifying the new acquirer as stale and clearing its
    slot mid-transaction (snapshot loss).

Invariant: kind=clause-explicit;
  property=A reader slot is released by atomic stores in the order:
    `PID = 0`, `Heartbeat = 0`, `HintEpoch = 0`, `TxnID = 0`.
    `TxnID = 0` is always the last store — the slot is observably
    free only after every other field has been cleared;
  from=this spec §Reader Table (slot release);
  violation=Releasing `TxnID` before `PID` lets a writer scan
    between the next acquirer's CAS and its `PID` store see the
    *prior* owner's PID — running `kill()` against an exited
    process, misclassifying the slot as stale, and clearing the
    fresh acquirer.

Invariant: kind=clause-explicit;
  property=Stale-reader detection that observes `TxnID != 0 AND
    PID == 0 AND Heartbeat == 0` (the "stuck mid-acquire" state)
    uses `HintEpoch` as the cross-process orphan anchor: the first
    observer CAS-stores its monotonic clock into `HintEpoch`;
    subsequent observers in *any* process compare against the
    stored epoch. The slot is cleared only once `now - HintEpoch >
    StaleTimeout`;
  from=this spec §Reader Table (stale detection, PID == 0 path);
  violation=Without the shared epoch, short-lived writer processes
    each observe the orphan once, record a local epoch, and exit
    before `StaleTimeout` elapses — the slot is permanently
    pinned, blocking all future reclamation.

Invariant: kind=clause-explicit;
  property=When a writer clears a stale slot, it performs the SAME
    four atomic stores in the SAME order as slot release:
    `PID = 0`, `Heartbeat = 0`, `HintEpoch = 0`, `TxnID = 0` —
    the dead occupant's identity never survives the clear, and the
    slot is observably free only after every other field is clean;
  from=this spec §Reader Table (stale detection — clear ordering);
  violation=A clear that leaves the dead occupant's PID/Heartbeat
    behind makes the next acquirer's CAS→publish window observably
    `TxnID = fresh, PID = dead, Heartbeat = stale`; the scan
    classifies by `PID != 0` (same-namespace `IsAlive` failure or
    cross-namespace stale heartbeat) and immediately evicts the
    LIVE acquirer — snapshot leaves the table, reclamation bound
    advances past it (use-after-reclaim), and its own later
    release zeroes a slot a third reader may have won. Reversing
    only HintEpoch/TxnID instead leaves the narrower window where
    a fresh acquirer inherits the prior epoch (already aged out)
    and a genuinely-crashed acquirer is re-cleared faster than
    `StaleTimeout`, violating the per-occupant timer invariant.
    (Enforced by `ClearStaleReaderSlot`; pinned by
    `TestClearedSlotDoesNotEvictMidPublishAcquirer` in
    `internal/lock/reader_test.go`.)

Invariant: kind=clause-explicit;
  property=`HintEpoch` lives in the shared-memory lock file (not
    process-local memory), so the orphan-detection timer survives
    writer-process turnover;
  from=this spec §Reader Table (why HintEpoch is shared);
  violation=Process-local epoch + short-lived writers ⇒
    permanently-pinned slot (Round-2 finding M1, see source
    commit history). The shared-memory placement is the only
    encoding that survives turnover.

Invariant: kind=clause-explicit;
  property=Every stale-detection age comparison (`now - Heartbeat`
    and `now - HintEpoch`, in the reader-table scan AND the
    writer-recovery check) treats a stamp in the FUTURE
    (`stamp > now`) as fresh/live, never stale — i.e. clears only
    when `stamp <= now AND now - stamp > StaleTimeout`. The
    monotonic-clock stamps are unsigned, so a naive `now - stamp >
    StaleTimeout` underflows to ~2^64 (> any timeout) for a
    future stamp and clears a live owner;
  from=this spec §Reader Table (stale detection) and §Stale Writer
    Recovery (shared future-stamp guard);
  violation=A mid-publish reader stores `Heartbeat = nowMonotonic()`
    (step 4a) before its `PID` (step 4e); a writer scanning with an
    earlier `now` than that reader's clock read — reachable with no
    happens-before between the two reads, and routine on
    darwin/freebsd where `CLOCK_MONOTONIC` origins are per-process
    (§Process Start Time) — sees `Heartbeat > now`, underflows, and
    clears the live acquirer mid-publish. Its snapshot `TxnID`
    leaves the table, the reclamation bound advances past it, and
    RPL reclamation frees pages the reader is about to read:
    use-after-reclaim / torn snapshot. The HB-first/PID-last acquire
    ordering exists precisely to make mid-publish safe; the underflow
    defeats it. Backward clock skew (NTP step-back, manual set) is a
    second trigger.

Invariant: kind=clause-explicit;
  property=Only the flock goroutine ever calls `flock()`; at most
    one goroutine in the process is attempting `flock` at any
    moment. Writers communicate with the flock goroutine via a
    request channel and a per-request response channel;
  from=this spec §Write Lock (flock goroutine);
  violation=Multiple goroutines calling `flock(LOCK_EX)` block
    indefinitely in the kernel — `Close()` and `ctx` cancellation
    cannot dequeue them, producing the goroutine-accumulation
    pathology this design avoids.

Invariant: kind=clause-explicit;
  property=The flock goroutine uses `flock(LOCK_EX | LOCK_NB)` in
    a retry loop with `select` on the stop channel, the request's
    context, and a tick channel — never blocking `flock(LOCK_EX)`;
  from=this spec §Write Lock (flock goroutine non-blocking
    acquisition);
  violation=A blocking flock would let another process hold the
    lock indefinitely while this process's `Close()` or
    cancellation cannot unwind the goroutine; the design's
    "cooperatively cancellable" property breaks.

Invariant: kind=clause-explicit;
  property=The flock goroutine clears `WriterPID`,
    `WriterStartTime`, and `WriterPIDNamespace` (all to 0) BEFORE
    issuing `flock(LOCK_UN)`. Equivalently, while this process
    holds `LOCK_EX`, `WriterPID != 0` implies the publication is
    live and ours, and `WriterPID == 0` implies we are mid-clear
    or pre-publish — but `LOCK_EX` is never released with stale
    `WriterPID` set;
  from=this spec §Write Lock step 4 (release path);
  violation=A peer process that acquires `flock(LOCK_EX)`
    immediately after our `LOCK_UN` reads our stale `WriterPID`
    and runs stale-writer-recovery against what it concludes is a
    crashed writer — but the writer those fields named is *us*,
    which has cleanly finished and released. Recovery clears
    state mid-tx for a newly-granted live writer, or worse,
    treats the peer's fresh ownership as a continuation of the
    "stale" state and skips initialisation.

Invariant: kind=clause-explicit;
  property=When `Close()` runs while the flock goroutine holds
    `LOCK_EX`, the goroutine clears the writer-header fields and
    issues `flock(LOCK_UN)` before exiting; `Close()` blocks
    until the goroutine has exited (i.e., `doneCh` closed). This
    is the application of the clear-before-unlock invariant to
    the stopCh-during-hold path;
  from=this spec §Write Lock step 4 (stopCh branch);
  violation=`Close()` returning with the kernel-side `LOCK_EX`
    still held (released only when the process exits or the fd
    is finally closed by GC) lets peer processes block
    indefinitely on `flock(LOCK_EX|LOCK_NB)` retries for a writer
    this process has already torn down. If the application keeps
    the data-file open after `db.Close()` (e.g., for `Check`
    or `CopyTo`), the held flock is permanent for the lifetime
    of the data-file fd in the worst case.

Invariant: kind=clause-explicit;
  property=`WriterHeartbeat` is published under `LOCK_EX`
    synchronously with the publish-identity step (cross-process.md
    §Write Lock step 3). A peer that observes `WriterPID != 0`
    therefore always also sees a non-zero, recent `WriterHeartbeat`
    from the same writer;
  from=this spec §Write Lock step 3 and §Heartbeat Goroutine
    (initial-publish-on-grant);
  violation=Without the synchronous initial store, a different-
    namespace peer scanning between grant and the heartbeat
    goroutine's first tick observes `WriterPID != 0` +
    `WriterHeartbeat == 0`; "now - 0 > StaleTimeout" trivially
    holds, so the freshly-granted writer is mis-classified as
    crashed and stale-writer recovery races a live commit.

Invariant: kind=clause-explicit;
  property=A process writes `WriterHeartbeat` only while it holds
    `LOCK_EX` on the lock file. Concurrent peers (in other
    processes) and the same process's heartbeat goroutine when
    `LOCK_EX` is not held by this process MUST NOT write
    `WriterHeartbeat`;
  from=this spec §Heartbeat Goroutine (writer-only updates);
  violation=A non-holder "freshness-refresh" (e.g., a hypothetical
    maintenance pass that touches the writer header) corrupts the
    holder's own `WriterHeartbeat` cadence and confuses cross-
    namespace stale-detection: a peer reads a recent heartbeat
    that does NOT belong to the actual current writer.

Invariant: kind=clause-explicit;
  property=Stale writer recovery only proceeds when the writer is
    confirmed dead via same-namespace `kill(pid, 0)` returning
    `ESRCH` (plus `WriterStartTime` mismatch on PID reuse) or
    cross-namespace `now - WriterHeartbeat > StaleTimeout`. A live
    writer is never displaced;
  from=this spec §Stale Writer Recovery;
  violation=Evicting a live writer races two concurrent commits
    into the same data file — guaranteed corruption, the very
    failure mode the single-writer model exists to prevent.

Invariant: kind=clause-explicit;
  property=Shared-memory reads/writes in the lock file use
    function-based `sync/atomic` operations on naturally 8-byte-
    aligned `uint64` fields; typed atomics are not used for these
    fields because the memory is a raw region in `MAP_SHARED`
    mmap visible across processes;
  from=this spec §Atomic Operations Convention;
  violation=Mixing typed atomics with shared memory across
    processes is outside Go's memory model — torn reads/writes
    or compiler reordering can produce reader-slot states the
    detection logic doesn't anticipate.

Invariant: kind=clause-explicit;
  property=Every process that mmaps a lock file does so with
    size = 80 + (48 × LockFileHeader.MaxReaders), where
    MaxReaders is the value of the lock file's MaxReaders field
    at the moment the mmap is established. The mmap size is
    established once at Open and is never resized; MaxReaders is
    immutable for the life of the lock file (re-derivable from
    the on-disk header by any opener);
  from=this spec §Lock File Layout (size formula) and §Lock File
    Lifecycle (MaxReaders is NOT in the data file — it is a
    runtime coordination property read from the lock-file
    header);
  violation=A mmap smaller than the header dictates makes
    high-index reader slots invisible to writer scans
    (false-stale clears against slots the scanner cannot see;
    legitimately-alive readers missed; reader-slot exhaustion
    surfacing prematurely). A mmap larger than the file's
    on-disk size SIGBUSes the process on first access to the
    over-mapped region, defeating the cooperatively-cancellable
    design.

Invariant: kind=entailed;
  property=Writer-grant freshness — a writer holding the
    cross-process write grant builds its transaction on the current
    on-disk active meta, with the allocation bitmap, RPL chain, and
    cached file size all consistent with it. No clause states this
    directly; it is the assumption the serialized single-writer model
    rests on (the grant-holder's base state IS the latest commit);
  from=entailed: §Write Lock serializes writers but says nothing about
    refreshing the per-process view the previous holder advanced;
  violation=Peer A commits TxnID=6 and releases the grant; B acquires
    it with a cached TxnID=5 view, builds its tree on the TxnID=5 root
    (A's commit silently lost), writes its meta over the slot holding
    A's TxnID=6 (two metas claiming TxnID=6), and allocates from a
    stale bitmap handing out pages A's committed tree references (page
    aliasing). Enforced by the mandatory re-sync in §Writer
    acquisition flow step 3, and by the cross-handle writer-interleave
    test.

Invariant: kind=clause-explicit;
  property=Reader snapshot pinning — a read transaction's snapshot
    tree is protected from RPL reclamation for the transaction's
    whole lifetime: the reader-slot TxnID it publishes is
    restabilized against the latest meta AFTER the slot is visible
    (§Reader Table, slot acquire step 8), so the interval in which
    the snapshot was chosen but the slot invisible cannot admit a
    writer that reclaims the snapshot's pages;
  from=this spec §Reader Table (slot acquire, snapshot
    restabilization);
  violation=Without the post-publish re-read, a reader descheduled
    between its meta read and its slot CAS while a writer commits
    twice and reclaims the first tree's RPL segment traverses
    reclaimed-and-reused pages — wrong values or ErrCorrupted on a
    perfectly healthy database. (Pinned by
    `TestBeginReadRestabilizesSnapshotAfterRacingCommits`.)

Invariant: kind=entailed;
  property=Reader snapshot currency — a read transaction snapshots the
    latest committed meta visible at `BeginRead`, so it observes every
    commit (peer-process included) that completed happens-before its
    begin;
  from=entailed: the MVCC visibility contract assumes a reader sees the
    latest commit, which no single clause restates for the
    cross-process case;
  violation=Peer A commits `k1` (TxnID=6); a reader opened on handle B
    afterward snapshots B's cached TxnID=5 and never observes `k1`
    though A's commit completed before the reader began. (A visibility
    defect, not corruption: the stale-low TxnID floors the writer's
    reclamation bound conservatively.) Enforced by the latest-committed
    snapshot selection in §Reader Table and the cross-handle
    reader-sees-peer-commit test.

## Lock File Layout

```
Lock File
+----------------------------------------------+
| Header (80 bytes)                            |
| Magic              | uint64                  |
| MaxReaders         | uint32                  |
| Padding            | 4 bytes                 |
| UUID               | [16]byte                |  must match data file's UUID
| WriterPID          | uint64                  |
| WriterStartTime    | uint64                  |
| WriterPIDNamespace | uint64                  |
| WriterHeartbeat    | uint64                  |
| LastMaintenanceTime| uint64                  |
+----------------------------------------------+
| Reader Table                                 |
| +-------+-----+-----+------+-------+-------+ |
| | TxnID | PID | PST | PIDN | HB    | HEpoch| | Slot 0 (48 bytes)
| | u64   | u64 | u64 | u64  | u64   | u64   | |
| +-------+-----+-----+------+-------+-------+ |
| | ...                                       | | up to MaxReaders slots
| +-------+-----+-----+------+-------+-------+ |
+----------------------------------------------+
```

PST = Process Start Time. PIDN = PID Namespace. HB = Heartbeat.
HEpoch = HintEpoch (cross-process orphan-detection anchor for
slots stuck mid-acquire; see Stale Reader Detection).

The lock-file structures use Go structs with `structs.HostLayout`
(Go 1.24+), which guarantees the host platform's C ABI layout.
This allows safely overlaying Go structs on the mmap'd shared
memory region without manual byte-offset arithmetic.

```go
type LockFileHeader struct {
    _                   structs.HostLayout
    Magic               uint64
    MaxReaders          uint32
    _                   [4]byte
    UUID                [16]byte
    WriterPID           uint64
    WriterStartTime     uint64
    WriterPIDNamespace  uint64
    WriterHeartbeat     uint64
    LastMaintenanceTime uint64
}

type ReaderSlot struct {
    _                structs.HostLayout
    TxnID            uint64
    PID              uint64
    ProcessStartTime uint64
    PIDNamespace     uint64
    Heartbeat        uint64
    HintEpoch        uint64 // monotonic clock; first observer of PID==0+Heartbeat==0 sets this
}
```

The `HostLayout` marker applies only to the lock file's shared-
memory structures. Data file page formats remain raw byte layouts
with explicit endian-aware encode/decode functions for portability.

**Cross-platform portability of the lock file is not a goal.** The
lock file is ephemeral (deleted when all processes exit) and its
layout deliberately follows the host platform's C ABI. A lock file
written by a little-endian process is not readable by a big-endian
process; mounting the database on a different architecture
requires deleting any stale lock file (the next opener does this
automatically when the UUID does not match the data file's). The
data file itself is fully portable (little-endian, explicit
encode/decode).

### Header fields (80 bytes)

- **Magic**: identifies the file as a gmdb lock file.
- **MaxReaders**: number of reader slots, set at lock-file creation
  via `Options.MaxReaders` (default 4096). Immutable.
- **UUID**: copied from the data file's meta at creation. On
  `Open()` the lock file's UUID is compared to the data file's
  UUID; mismatch ⇒ stale lock file ⇒ deleted and recreated.
- **WriterPID**: PID of the current write-lock holder (0 = no
  writer). `uint64` for forward safety (Linux `pid_max` can reach
  2²²).
- **WriterStartTime**: process start time of the writer for PID-
  reuse detection.
- **WriterPIDNamespace**: PID namespace inode of the writer
  (Linux), 0 on other platforms.
- **WriterHeartbeat**: `CLOCK_BOOTTIME` nanos (Linux) /
  `CLOCK_MONOTONIC` elsewhere, updated periodically by the flock
  goroutine (the lock holder) while the write lock is held.
- **LastMaintenanceTime**: updated after a maintenance pass
  completes (see `background-maintenance.md`).
- **DataGeneration** (atomic): counts data-file replacements
  (Compact's rename-over). See §Data-file generation below.

### Reader slot (48 bytes)

- **TxnID** (atomic): snapshot TxnID held by this reader. `0` =
  free.
- **PID** (atomic): owning process. `uint64` for alignment +
  forward safety.
- **ProcessStartTime** (atomic): owning process's start time when
  the slot was acquired.
- **PIDNamespace** (atomic): PID namespace inode of owner.
- **Heartbeat** (atomic): monotonic clock, updated periodically
  (~1 s) by owning process's heartbeat goroutine.
- **HintEpoch** (atomic): cross-process orphan-detection anchor.
  Zero during normal operation. The first writer-scan that
  observes the slot in the "stuck mid-acquire" state
  (`TxnID != 0 AND PID == 0 AND Heartbeat == 0`) CAS-stores its
  current monotonic clock here; subsequent scans by *any* process
  compare against this stored epoch, so the orphan timer survives
  writer-process turnover. Cleared back to 0 by slot release and
  by successful acquire's field-write phase.

Total size: `80 + (48 × MaxReaders)`. Default 4096 readers:
`80 + 196608 = 196688` bytes (~192 KB).

`MaxReaders` is bounded `[1, 65536]`. The lower bound is one slot
(degenerate but legal); the upper bound caps the mmap at
`80 + 48 × 65536 ≈ 3 MiB`, so a corrupted or maliciously-crafted
header value cannot demand a petabyte-scale mmap. A header
`MaxReaders` value outside this range is treated as `ErrCorrupted`
by `Open`.

The lock file is mmap'd `MAP_SHARED` by all processes for the
reader table. The write lock is a separate concern via `flock()`.

## Lock File Lifecycle

Ephemeral. The first process to open the database creates the
lock file, writes the header
(`Magic`, `MaxReaders`, `WriterPID = 0`, `WriterStartTime = 0`),
and initializes all slots to zero. Subsequent processes validate
`Magic`, read `MaxReaders`, mmap at the corresponding size. If
deleted (e.g., after all processes exit), the next opener
recreates it. `MaxReaders` is NOT in the data file — it is a
runtime coordination property.

On open, if the lock file exists, the opener checks `WriterPID`.
Non-zero: determine whether the writer is still alive via
`kill(pid, 0)` + `WriterStartTime` comparison (see Process Start
Time). Dead or recycled: writer crashed; see Stale Writer
Recovery.

### Data-file generation

`Compact()` replaces the data file by renaming a rebuilt inode over
`db.path`. The lock file is deliberately NOT renamed (the UUID and
reader table survive), so a peer process's open handle keeps its fd
and mmap on the old, now-unlinked inode — and, without a guard, its
next write grant acquires normally and commits to the dead file:
writes permanently invisible to every other process, reads frozen
pre-Compact forever.

The `DataGeneration` header field closes this. Protocol:

1. Every handle caches the field's value at `Open()`, then
   verifies its data fd still names the path's inode (fstat vs
   path stat) — an Open racing a peer Compact would otherwise
   cache the post-bump value while mapping the replaced inode,
   defeating every later check; a mismatch retries the Open
   against the current inode. The rename happens-before the
   bump, so a same-inode stat proves any unobserved bump belongs
   to a later compact, which the per-operation checks catch.
2. `Compact()`, holding the write grant, bumps the field atomically
   IMMEDIATELY after the rename (and updates its own cache — the
   compacting handle stays valid). The directory fsync follows; its
   failure poisons the compacting handle but the bump stands — the
   live directory entry already changed, so peers must converge
   regardless.
3. After every write-grant acquisition, and after every reader-slot
   publish, the handle compares the field against its cache. A
   mismatch means a peer replaced the inode this handle still maps:
   the handle POISONS itself (grant released / slot freed first) and
   the operation returns `ErrPoisoned` — Close + re-Open converges on
   the new inode.

Invariant: kind=clause-explicit;
  property=No transaction begins against a data-file inode that a
    completed peer `Compact()` has replaced: the generation check
    runs after the write-grant acquisition (writers) and after the
    reader-slot publish (readers), both strictly before any page
    access;
  from=this spec §Data-file generation;
  violation=A peer handle that misses the replacement commits to the
    unlinked inode — the write succeeds locally, is invisible to
    every other process and every future opener, and the two files
    diverge silently (the api-surface.md §Compact all-or-nothing
    contract broken across processes).
    (Enforced by the generation checks in Begin/BeginRead/
    Checkpoint/Compact plus the Open-time inode verification;
    pinned per entry point by the peer-handle tests in
    compact_generation_test.go.)

### Creator-side flock protocol

The creator opens the file with `O_CREATE|O_EXCL`, then takes
`flock(LOCK_EX)` and holds it across `ftruncate(2)` +
`pwrite(header)` + `fsync(2)`; `LOCK_EX` is released only after
the header has been published. Adopters take `flock(LOCK_SH)`
immediately after open and validate the header under that lock.

Because `open()` and `flock()` are separate POSIX syscalls, an
adopter can land between the creator's `O_CREATE|O_EXCL` and
its first `flock(LOCK_EX)` and observe `size < HeaderSize` or
`Magic == 0` (`ftruncate` zero-fills the extended region, so the
post-truncate / pre-write header reads as all-zero); this is
mid-init, not corruption. Adopters retry with bounded
exponential backoff (10 attempts capped at 256 ms each,
≈ 800 ms total); budget exhaustion surfaces `ErrCorrupted` (a
creator that crashed inside the open→flock window or external
tampering left a zero-Magic file at this path).

A failed creator (crash mid-init, or `initLockFile` error from
syscall) unlinks the in-progress file before exit so subsequent
adopters do not waste the retry budget on a known-stuck file.

## Write Lock

Two layers:

- **Intra-process**: a writer queue managed by a single **flock
  goroutine** on the `DB` struct. Writers submit via a channel and
  receive the lock grant via a per-request response channel.
  Prevents two same-process goroutines from concurrent writes
  while supporting context-aware cancellation with zero goroutine
  accumulation.
- **Cross-process**: `flock(LOCK_EX)` on the lock file, acquired
  and released by the flock goroutine.

### Flock goroutine

A single persistent goroutine (started at Open, stopped at Close)
solely owns flock acquisition/release. At most one goroutine is
ever attempting `flock()`. The goroutine never blocks
indefinitely in the kernel — it uses `flock(LOCK_EX | LOCK_NB)`
(non-blocking) in a retry loop with a `select` on the stop
channel, so `Close()` can always unwind the goroutine even when
another process holds the write lock for an extended period.

```
db.writerCh chan writerRequest
db.stopCh   chan struct{}       // closed by Close()
db.lockTry  *time.Ticker        // retry interval, default 50ms

type writerRequest struct {
    ctx     context.Context
    ctxDone <-chan struct{}     // equivalent to req.ctx.Done()
    result  chan<- error        // nil = lock granted; non-nil = cancelled/error
}
```

Loop:

1. `select` on `db.writerCh` (next request), `db.stopCh` (Close
   signal), and (when flock is held) the writer's release channel.
   On `db.stopCh`: release flock if held, exit.
2. On a new `writerRequest`:
   a. Check `req.ctx` — if already cancelled, send
      `context.Cause(req.ctx)` on `req.result` and loop.
   b. Enter the **non-blocking acquisition loop**:
      - `flock(db.lockFd, LOCK_EX | LOCK_NB)`.
      - On success: proceed to step 3.
      - On `EWOULDBLOCK`: `select` on
        (`db.lockTry.C`, `req.ctxDone`, `db.stopCh`):
          - tick → retry the `flock` syscall.
          - `req.ctxDone` → caller cancelled; send
            `context.Cause(req.ctx)` and loop to step 1 (slot is
            free for the next request).
          - `db.stopCh` → release flock if held, exit.
      - On any other error: send the error to `req.result` and
        loop.
3. Store `WriterPID`, `WriterStartTime`, `WriterPIDNamespace`,
   and `WriterHeartbeat = nowMonotonic()` in the lock-file header.
   `WriterHeartbeat` is published synchronously here — under
   `LOCK_EX` — so any peer scanning between grant and the flock
   goroutine's first refresh tick (step 4) observes a non-zero,
   recent heartbeat alongside the non-zero `WriterPID` (see §Stale
   Writer Recovery case 2 for why a zero `WriterHeartbeat` at
   that instant would false-stale the fresh writer).
   Relative store order across these four is not load-bearing —
   peers inspect them jointly under §Stale Writer Recovery,
   unlike the reader-slot acquire sequence whose order *is*
   load-bearing — see §Reader Table (slot acquire) for the
   asymmetry. Send `nil` on `req.result` — writer holds the
   lock.
4. **Hold loop.** `select` on (writer's release channel,
   `db.stopCh`, a `HeartbeatInterval` ticker).
   - Ticker: re-store `WriterHeartbeat = nowMonotonic()` and loop.
     The flock goroutine holds `LOCK_EX` across the whole loop, so
     every refresh lands under `LOCK_EX` *by construction* — this is
     the sole `WriterHeartbeat`-refresh site, keeping a long-held
     writer fresh against cross-namespace stale-detection without a
     cross-goroutine "holding" flag that could straddle the unlock.
   - Release: clear `WriterPID` / `WriterStartTime` /
     `WriterPIDNamespace` *before* `flock(LOCK_UN)` (see the
     clear-before-unlock invariant), then loop to step 1.
   - `db.stopCh`: clear writer header fields *before*
     `flock(LOCK_UN)` (same invariant), then exit.

The non-blocking + ticker pattern is a small CPU/syscall cost
(`Options.LockRetryInterval`, default 50 ms; one extra syscall per
tick while contention persists) in exchange for bounded `Close()`
latency and bounded cancellation latency.

### Writer acquisition flow

`Begin(ctx, writable=true)`:

1. Send `writerRequest{ctx, result}` to `db.writerCh`.
2. `select` on `result` and `ctx.Done()`:
   - `result` ← `nil`: lock granted. Proceed.
   - `result` ← non-nil error: lock not granted.
   - `ctx.Done()` first: writer gives up. The flock goroutine
     will detect cancellation when it processes the request and
     skip or release. Return `context.Cause(ctx)`.
3. **Re-sync in-memory state from disk (mandatory).** A writer's
   active meta, allocation bitmap, RPL chain, and cached file size
   are loaded at `Open` and otherwise mutated only by *this* handle's
   own commits. While we waited for the grant, a **peer** process may
   have acquired it, committed, and released — leaving every one of
   those stale. Before building the transaction, re-read both meta
   pages and, if the on-disk state advanced, rebuild bitmap + RPL +
   fileSize + commit-state from disk (`Pager.Resync`). The mmap is
   reused unchanged: `MaxSize` and `PageSize` are immutable for the
   file's life, so the reservation always covers the current file
   (a peer can only grow it up to `MaxSize`). Skipping this step is a
   guaranteed lost-update + page-aliasing corruption for serialized
   cross-process writers (the writer builds on a stale root, writes
   its meta over the slot holding the peer's newer commit, and
   allocates pages the peer's committed tree references) — see the
   Writer-grant freshness invariant.

   **Selection and projections.** The re-sync adopts the
   highest-valid-TxnID meta (`page.ActiveMeta`) — the one selection
   rule shared with recovery and readers (`durability.md §One
   selection, two projections`) — and uses its LIVE projection: the
   peer cleanly committed and released the flock, so its latest
   commit — even an unfsynced `SyncLazy` one — is complete and
   page-cache-visible; rolling back to the durable epoch would
   silently lose it (and contradict the latest-committed snapshot
   reads observe — see Reader Table). The reclamation lower bound
   comes from the same meta's `AnchoredDurableTxnID` — the newest
   epoch assertion the disk is guaranteed to record, so never ahead
   of any state a crash could make recovery adopt (`durability.md
   §Anchoring`, `free-space.md §RPL Reclamation`) — optionally
   tightened by the writer's own completed-fsync knowledge; no
   separate flag scan exists.

`Checkpoint()` performs the same re-sync after acquiring the grant,
for the same reason: it bumps the active meta's durable sub-record in
place, so it must bump the *current* on-disk meta in its *current*
slot, never an older cached meta over a peer's newer one.

`Commit()` and `Rollback()` signal the flock goroutine to
release.

### Why this design

A goroutine-per-attempt model would suffer under rapid context
cancellation: each cancelled attempt leaves a goroutine blocked
in `flock` until it acquires-and-releases. Under pathological
cancel patterns this accumulates hundreds of goroutines.

A single flock goroutine eliminates that. Cancelled writers
simply dequeue — they never touch flock. Fixed cost (one
goroutine per DB handle, ~8 KB stack + one `time.Ticker`) for
the lifetime of the database handle.

The non-blocking `flock` + retry pattern means the goroutine is
always cooperatively cancellable: `Close()` and per-writer `ctx`
cancellation are both honoured within at most one retry interval,
even when another process holds the lock indefinitely. The cost
is one wasted syscall per tick of contention.

The DB holds a dedicated fd for the write lock (`db.lockFd`),
separate from the fd for the reader-table mmap. Used exclusively
for `flock()` / `funlock()`.

**Cancellation latency under pathological queue patterns.** When
a writer's `ctx` cancels while the request sits in `db.writerCh`
behind a still-pending predecessor, the cancelled writer's caller
returns immediately with `context.Cause(ctx)`. The request itself
remains in the channel and is processed in turn by the flock
goroutine, which discards already-cancelled requests via the
step-2a check *before* attempting `flock`. Under sustained
high-cancellation load this produces no extra flock syscalls —
cancelled requests cost only a channel receive and a `select`.

### Stale writer recovery

If a process crashes holding the write lock, `WriterPID` remains
non-zero and the `flock()` is automatically released by the
kernel (fd close on process exit). On `Open()` or write-lock
acquisition with `WriterPID` non-zero, the process determines
whether the writer is alive using the same namespace-aware logic
as reader stale detection:

1. **Same PID namespace** (`WriterPIDNamespace` == checker's,
   both non-zero):
   a. `kill(pid, 0)` — `ESRCH` ⇒ dead.
   b. If alive, compare `WriterStartTime` — different ⇒ PID
      recycled.
2. **Different PID namespace** (or either 0): check
   `WriterHeartbeat`. `now - WriterHeartbeat > StaleTimeout` ⇒
   dead.

If dead, recovery:

1. Read both meta pages, select the valid one (highest TxnID +
   valid checksum). The crashed writer's partial commit is
   invisible — CoW guarantees the previous meta points to a
   consistent tree.
2. Scan the reader table for slots matching the dead writer by
   `(PID, PIDNamespace, ProcessStartTime)` — all three must
   agree with the corresponding header fields — and clear them.
   Matching only on `(PID, PIDNamespace)` would wipe a live
   reader's slot if the OS recycled the dead writer's PID to
   another in-namespace process that subsequently opened a read
   transaction (snapshot loss for that reader). The
   ProcessStartTime term distinguishes PID lifetimes per the
   same logic the reader-stale-detection same-namespace path
   uses (§Reader Table case 1).
3. Clear `WriterPID`, `WriterStartTime`, `WriterPIDNamespace`,
   `WriterHeartbeat`.

No special rollback logic for tree consistency — CoW guarantees
the previous meta points to a fully consistent tree.

Bitmap modifications are deferred in memory (`tx.pendingAllocs` /
`tx.pendingFrees`) and only written to disk via pwrite at commit
time. If the writer crashes before commit, no bitmap modifications
reach disk — the on-disk bitmap is fully consistent with the
previous meta. No leaked pages. The slab buffers were anonymous
mmap pages that are released to the OS on process exit — no
on-disk artifacts.

## Reader Table

Slot allocation uses a simple scan with atomic CAS — no free
stack or other auxiliary data structure. The reader table is a
flat array of 48-byte slots in the lock file's shared mmap. All
operations use atomic memory ops visible across processes.

**Snapshot selection.** A read transaction (`BeginRead`) snapshots
the **latest committed** on-disk meta — both meta pages are re-read
and the highest-valid-TxnID one is chosen (`page.ActiveMeta`) — NOT
the handle's cached `currentMeta` (which a peer's commit leaves
stale). The reader uses the selected meta's LIVE projection
(a reader wants visibility of the newest commit, not the durable
epoch's rollback target; the two differ only for an unfsynced
`SyncLazy` commit, which a reader correctly observes —
`durability.md §One selection, two projections`). The read is lock-free — readers
hold no write grant — so a writer may be mid-commit on the inactive
slot; a torn slot fails its checksum and the valid one is selected,
and because a commit writes (and, per `SyncMode`, fsyncs) data pages
before the meta, the selected meta's tree pages are always present.
This is what lets a reader in one process observe a commit that
completed (happens-before its `BeginRead`) in another — see the
Reader snapshot currency invariant.

### Slot acquire (`Begin` read transaction)

The acquire sequence is structured so that a crash at *any* point
after the CAS leaves the slot in a state the stale-detector can
reclaim. Heartbeat is written first (so a crash mid-acquire still
gives the slot a "recent liveness" anchor that will eventually go
stale); PID is written last (so the detector's PID-based fast
path is only used once the full identity has been populated).

1. Start scanning from the **slot hint** (`coord.readerSlotHint`, an
   `atomic.Uint32` on the Coord struct) rather than slot 0.
2. Scan forward (with wraparound) for `TxnID == 0` (free).
3. Atomically CAS the `TxnID` field from `0` to the current meta
   page's TxnID. CAS failure ⇒ continue scanning.
4. Immediately after a successful CAS, in this exact order:
   a. Store `Heartbeat = nowMonotonic()` (atomic).
   b. Store `HintEpoch = 0` (atomic, clears any prior
      orphan-anchor left over from a stale-cleared slot).
   c. Store `PIDNamespace = db.pidNamespace` (atomic).
   d. Store `ProcessStartTime = db.processStartTime` (atomic).
   e. Store `PID = currentPID` (atomic).
5. Register the slot index with the heartbeat goroutine's active
   list.
6. Update `coord.readerSlotHint`.
7. If all slots occupied (full wraparound), return
   `ErrReadersFull`.
8. **Snapshot restabilization.** The meta whose TxnID seeded step 3
   was read BEFORE the slot became visible, so a writer whose
   bound-computation scan completed inside that window may already
   have reclaimed that snapshot tree's pages. After the slot is
   published: re-read the latest meta; if its TxnID differs from the
   pinned one, raise the slot's `TxnID` to the new value (an owner-
   only, monotonic overwrite — a concurrent scan reading the old
   value computes a lower, strictly conservative, bound) and repeat
   until one re-read returns the pinned TxnID unchanged. The
   transaction proceeds on the stabilized meta.

   Why a stable re-read is sufficient: pages of tree `T` are only
   reclaimed through RPL segments with `TxnID t > T`, and
   reclamation runs strictly after the meta carrying `t` was
   written. If the post-publish re-read still returns `T`, no such
   meta existed at re-read time — nothing of tree `T` had been
   reclaimed — and from that instant every writer's bound scan sees
   this slot, flooring the bound at `T`.

The hint is process-local, updated with a relaxed atomic store —
no cross-process coordination. Under steady-state load, the hint
points to a recently-freed slot and the scan completes in 1–2
iterations. Worst case wraps to O(MaxReaders).

The CAS on `TxnID` is the serialization point. 48-byte slots ×
4096 = 192 KB — fits in L2 cache, sequential scan with hardware
prefetching.

### Slot release (`Commit` / `Rollback` read transaction)

In order:

1. `PID = 0` (atomic store). Prevents the next stale-reader scan
   from inspecting this process's (about-to-be-stale) PID after
   release.
2. `Heartbeat = 0` (atomic store). Resets the heartbeat-based
   liveness marker so a subsequent acquirer is in a clean state.
   *Race note.* The heartbeat goroutine may concurrently store
   a fresh value to `Heartbeat` for this slot if it has not yet
   observed the corresponding `activeSlotsMu`-protected
   Begin/Commit list update. The race is benign — both stores
   are valid `uint64` values and step 4 (`TxnID = 0`) lands
   shortly after, putting the slot in the unambiguously-free
   state. The active-slot list removal happens *before* this
   step 2 store, so the heartbeat goroutine's next tick will not
   target this slot.
3. `HintEpoch = 0` (atomic store). Clears any orphan-detection
   anchor.
4. `TxnID = 0` (atomic store). Final release — slot is now free.

No CAS — only the slot owner writes its own slot. Step 1 before
step 4 closes the prior-owner-PID race: a writer scanning between
the next acquirer's CAS and its PID store sees `PID == 0` and
falls through to the heartbeat path rather than running `kill()`
against the previous (now-exited) owner's PID.

### Stale-reader detection

During the writer's reader-table scan (to find min active TxnID),
if a slot has non-zero `TxnID`, classify it by inspecting `PID`
and `Heartbeat`:

**0. `PID == 0` path (slot is mid-acquire, mid-release, or
orphaned):**

a. **Fresh heartbeat** (`Heartbeat != 0 AND now - Heartbeat <=
   StaleTimeout`): a live owner is mid-acquire/release. **Skip.**

b. **Stale heartbeat** (`Heartbeat != 0 AND now - Heartbeat >
   StaleTimeout`): the acquirer made it past step 4a (heartbeat
   store) but crashed before step 4e (PID store), and the
   heartbeat has now aged out. **Orphan: clear `TxnID = 0`.**

c. **Zero heartbeat** (`Heartbeat == 0`): the acquirer crashed
   *before* step 4a, leaving the slot with `TxnID != 0,
   PID == 0, Heartbeat == 0`. There is no per-slot age signal
   yet; use `HintEpoch` as the cross-process orphan anchor:

   - If `HintEpoch == 0`: this is the first observation. CAS
     `HintEpoch` from 0 to `now`. **Skip clearing this round AND
     include `TxnID` in the oldest-min computation**: the acquirer
     may be a live mid-publish reader about to stamp PID;
     advancing the reclamation bound past its TxnID would let the
     writer reclaim pages the reader will snapshot. The next scan
     (from any process) compares against the stored epoch.
   - If `HintEpoch != 0 AND now - HintEpoch > StaleTimeout`:
     confirmed orphan. **Clear `TxnID = 0`** (and `HintEpoch`
     via the post-clear cleanup below).
   - Otherwise (`HintEpoch != 0`, epoch set but not yet aged out):
     **skip clearing AND include `TxnID` in the oldest-min
     computation** — same safety rationale as the first-observer
     case.

**1. `PID != 0`, same PID namespace** (slot's `PIDNamespace` ==
checker's, both non-zero):

a. `kill(pid, 0)` — `ESRCH` ⇒ stale.
b. If alive, compare `ProcessStartTime` — different ⇒ PID
   recycled, stale.
c. Match ⇒ alive and holding the slot legitimately.

**2. `PID != 0`, different PID namespace** (or either namespace
inode is 0):

a. `now - Heartbeat > StaleTimeout` ⇒ stale, clear `TxnID = 0`.
b. Fresh ⇒ not stale.

**3.** If neither PID nor heartbeat can determine liveness, fall
back to PID-only liveness (legacy path).

### Clearing a stale slot

When the writer clears a stale slot, it stores in the SAME exact
order as slot release:

1. `PID = 0` (atomic). The dead occupant's identity must not
   survive the clear. If it did, the next acquirer's CAS→publish
   window would be observably `TxnID = fresh, PID = dead
   occupant, Heartbeat = stale` — and the stale-detection scan
   classifies by `PID != 0`: same-namespace `IsAlive(deadPID)`
   fails (or the cross-namespace heartbeat is stale), so the scan
   immediately evicts the LIVE acquirer. Its snapshot leaves the
   table, the reclamation bound advances past it (the
   use-after-reclaim failure mode), and its own later release
   zeroes a slot a third reader may since have won.
2. `Heartbeat = 0` (atomic). Same hazard for the `PID == 0`
   sub-cases: a leftover stale heartbeat puts the mid-publish
   acquirer in case (b) — immediate clear — instead of case (c),
   the epoch-anchored StaleTimeout-bounded path that mid-publish
   protection relies on.
3. `HintEpoch = 0` (atomic). Resets the orphan-detection anchor
   *while the slot is still observably non-free*, so no acquirer
   can race into the slot and inherit a stale epoch.
4. `TxnID = 0` (atomic). Final release — slot is now free. Only
   after this store can an acquirer CAS the slot, so acquirers
   never observe a partial clear; concurrent scans are excluded
   by flock(LOCK_EX).

`PST`/`PIDN` are left as-is, exactly like the release path — the
classification consults them only when `PID != 0`, which steps
1–4 guarantee is false until the next acquirer publishes its own
identity.

The HintEpoch-before-TxnID ordering is load-bearing: reversed, a
window exists between `TxnID = 0` and `HintEpoch = 0` during
which a fresh acquirer can CAS-win `TxnID` and crash before step
4a (heartbeat store). A subsequent stale-detection scan would
then see `TxnID != 0, PID == 0, Heartbeat == 0,
HintEpoch = <stale value from prior cycle>` and immediately
re-clear the slot via case (c)'s timer (already aged out),
evicting the (genuinely dead) new acquirer faster than
StaleTimeout — benign for that slot but violating the
per-occupant timer invariant. Zeroing HintEpoch first closes
the window.

### Read-only fleets never reap

Every stale-slot clear path runs from a WRITER-side context (the
stale scan is part of writer acquisition and maintenance, both of
which hold `flock(LOCK_EX)`), and a read-only handle's coordinator
refuses `AcquireWriter` while background maintenance is not started
for read-only handles at all. Consequence — a documented deployment
bound: in a fleet consisting EXCLUSIVELY of read-only handles, a
crashed reader's slot is never reaped, and its stale `TxnID` pins
the reclamation bound until any writable handle opens the database
(its first grant acquisition scans and clears). Deployments that
run long-lived read-only fleets over a database that also grows
should include at least one writable opener (or periodic writable
maintenance opens); pure-RO fleets over a static database are
unaffected (nothing reclaims, so nothing is pinned). Granting
read-only handles a narrow reap-only `LOCK_EX` path was considered
and rejected: it would break the invariant that every bitmap/table
mutation is writer-serialized, for a corner that a single writable
open resolves.

### Why HintEpoch lives in shared memory

A process-local epoch would not survive writer-process turnover:
short-lived writers (cron jobs, batch scripts) each observe the
orphaned slot once, record their own local epoch, and exit
before the StaleTimeout elapses, leaving the slot permanently
pinned. The shared-memory `HintEpoch` accumulates observation
time across all writer processes — the first observer sets it;
any later writer in any process clears the slot once `now -
HintEpoch > StaleTimeout`.

The PID namespace check prevents cross-namespace failure modes
(false dead when containers don't share PIDs; false alive when
distinct processes happen to share a PID).

### Go goroutine model

Multiple slots may share the same PID (same process running
multiple read transactions). This is correct:

- **Slot allocation.** CAS on TxnID serializes claims across
  goroutines and external processes.
- **Stale detection.** `kill(pid, 0)` checks process liveness,
  not thread liveness. If a process crashes, all its slots are
  stale.
- **Oldest-reader scan.** Writer finds min TxnID across all
  occupied slots. Multiple slots from one process with different
  TxnIDs are handled naturally.

A single Go process running N concurrent read transactions
consumes N reader slots. Set `MaxReaders` high enough for the
expected total across all processes.

## Process Start Time

Both reader slots and writer header store **start time** alongside
PID. Monotonically-increasing value that changes when a PID is
recycled — unique `(PID, StartTime)` per process lifetime.

At `Open()`, the process reads its own start time once and caches
it on the DB struct (`db.processStartTime uint64`). Stored in
reader slots on `Begin()` and in `WriterStartTime` on write-lock
acquisition.

During stale detection, the writer reads the current start time
for a given PID via `processStartTime(pid int) (uint64, error)`.
If the PID is alive but the current start time differs from the
stored value, the PID was recycled.

| Platform | Source | Value | Notes |
|----------|--------|-------|-------|
| Linux | `/proc/[pid]/stat` field 22 | Clock ticks since boot (uint64) | No privileges. Pure Go: `os.ReadFile` + parse. |
| macOS | `sysctl KERN_PROC_PID` → `kinfo_proc.kp_proc.p_starttime` | timeval packed as `sec*1e6+usec` | Same-user processes. Pure Go via `syscall.Sysctl`. |
| FreeBSD | `sysctl KERN_PROC_PID` → `kinfo_proc.ki_start` | timeval packed | Same as macOS interface. |

All pure Go. `processStartTime` per-platform via build tags.

If `processStartTime` fails, falls back to heartbeat (if
available) or PID-only liveness.

**Resolution caveat.** `ProcessStartTime` is *non-decreasing*, not
strictly unique. Linux `/proc/[pid]/stat` field 22 reports clock
ticks since boot (typically 100 Hz = 10 ms resolution); two
processes spawned within the same tick share a start time. macOS
/ FreeBSD sysctl encodes `sec*1e6 + usec` which is microsecond-
resolution but still collision-prone under heavy fork bursts. In
particular, container PID 1 typically has a start time very near
zero relative to the container's boot. The protocol's
correctness does **not** rely on uniqueness of
`(PID, StartTime)` — it relies on the *combination* of
(same-namespace PID liveness, start-time match, fresh
heartbeat) and the heartbeat path being available as a fallback.
A same-namespace `(PID, StartTime)` collision between distinct
process lifetimes is benign because either (a) the prior holder
is dead and the heartbeat is stale (caught by the heartbeat
goroutine if the new holder rebooted heartbeat tracking, or by
the zero-heartbeat orphan rule in stale detection) or (b) the
prior holder is alive and legitimately holds the slot.

## PID Namespace Awareness

PID-based liveness operates within the caller's PID namespace.
When multiple containers share a database file via volume mount,
each container has its own PID namespace — a PID in one refers
to a different (or nonexistent) process in another. Two failure
modes:

- **False dead.** Container A holds slot at PID 42; container B
  has no PID 42; `kill(42, 0)` from B returns `ESRCH` — B clears
  the slot, removing snapshot protection for A's active reader.
- **False alive.** Container A crashes with PID 42 in a slot;
  container B also has a PID 42; `kill(42, 0)` from B succeeds —
  slot never reclaimed.

Each slot and the writer header store the process's **PID
namespace inode** alongside the PID. On Linux, read from
`/proc/self/ns/pid` via `readlink` at Open and cached on the DB
struct. On non-Linux, 0. If the `readlink` fails (no `/proc`
mounted, hardened sandbox), the DB caches 0 and logs the failure
via `slog.Logger` — this forces every cross-process stale check
involving this process to fall through to the heartbeat path,
which is safe but slower than PID-based.

The writer compares its own PID namespace to the slot's. Match ⇒
PID + StartTime fast path is safe. Differ ⇒ use heartbeat. A
process with `PIDNamespace == 0` (Linux without `/proc`, or
non-Linux) is treated as "different namespace" for the purposes
of stale detection when the peer has a non-zero namespace inode
— the asymmetry routes both directions through heartbeat, which
is the correct conservative behavior.

## Heartbeat Goroutine

The DB struct maintains a **heartbeat goroutine** (started at
Open, stopped at Close) that periodically refreshes the
`Heartbeat` field on every reader slot this process holds.
`WriterHeartbeat` is *not* refreshed here — it is refreshed by the
flock goroutine inside its §Write Lock step-4 hold loop, the only
context in which this process holds `LOCK_EX` (see **Writer-only
updates** below).

Ticks every ~1 s. Writes current monotonic clock
(`CLOCK_BOOTTIME` on Linux, `CLOCK_MONOTONIC` on other
platforms) to each active slot. The DB maintains an in-process
**active-slot list** — a `[]uint32` of slot indices protected
by `db.activeSlotsMu` (a `sync.Mutex`). `Begin()` appends under
the mutex; `Commit()`/`Rollback()` removes under the mutex; the
heartbeat goroutine takes a brief snapshot of the list under
the mutex each tick and issues the atomic stores outside the
lock to keep tick cost bounded.

**Writer-only updates.** `WriterHeartbeat` is refreshed only by
the flock goroutine, inside its §Write Lock step-4 hold loop, at
the same `HeartbeatInterval` cadence. The flock goroutine holds
`LOCK_EX` continuously from the step-3 publish to the step-4
clear+unlock, so every refresh lands under `LOCK_EX` *by
construction*: this goroutine is the sole writer of
`WriterHeartbeat` and writes it only inside the hold window. There
is no in-process "holding" flag a separate goroutine could read
stale and so stomp the field after this process's `LOCK_UN`. The
general heartbeat goroutine never writes `WriterHeartbeat` — it
refreshes only reader-slot Heartbeats, every tick, regardless of
write-lock state. Were a non-holder to write `WriterHeartbeat` it
would stomp the value of whichever process actually holds the lock
and corrupt cross-namespace stale-detection (§Stale Writer
Recovery case 2); confining the write to the lock-holding
goroutine's hold window makes that illegal state unrepresentable.

**Initial publish on grant.** §Write Lock step 3 stores
`WriterHeartbeat = nowMonotonic()` synchronously under `LOCK_EX`,
*before* sending `nil` on the writer's result channel. The flock
goroutine's step-4 hold loop then refreshes that value each tick
while the lock is held; a peer observing `WriterPID != 0`
therefore always also sees a non-zero, recent `WriterHeartbeat`
from the same writer, even before the first refresh tick fires.

`CLOCK_BOOTTIME` on Linux because it is monotonic, survives
suspend/resume, and is shared across all containers on the
same host (kernel-wide, not per-PID-namespace).
`CLOCK_MONOTONIC` on macOS / FreeBSD does not survive suspend;
on a laptop that resumes after a long sleep, the heartbeat
clock jumps forward by less than wall-time elapsed, so
`StaleTimeout`'s 10-second default is safe — false-stale
detection requires a heartbeat older than 10 s of *monotonic*
time, which a suspended process cannot accumulate.

`StaleTimeout` (default 10 s) controls how long a heartbeat
must be stale before the slot is reclaimed. Must be
significantly larger than the heartbeat interval (1 s) for
scheduling jitter.

**Shutdown coordination.** `Close()` sets `db.closed = true`
(atomic, see `leak-detection.md`), closes the heartbeat
goroutine's stop channel, and **waits** for the goroutine to
acknowledge via a done channel before unmapping the lock file.
The heartbeat goroutine checks the stop channel before each
tick and exits promptly. Without the wait, a final tick could
race with the munmap and SIGSEGV. The wait is bounded by the
tick interval (~1 s) since the goroutine sleeps in a `select`
that includes the stop channel.

Fixed-cost resource: one goroutine per DB handle, one atomic
store per active slot per second, plus one `uint64` slice copy
per tick — the snapshot-and-release pattern (`slots :=
append(nil, activeSlots...)` under `activeSlotsMu`, atomic
stores issued outside the lock) trades a small per-tick
allocation for a bounded critical section that doesn't stall on
slow atomic-store latency. No syscalls.

## Atomic Operations Convention

- **In-process fields** (DB/Coord/Tx struct fields like
  `coord.readerSlotHint`) use Go's **typed atomics**
  (`atomic.Uint64`, `atomic.Uint32`, `atomic.Int64`).
- **Shared-memory fields** (reader-table fields, header writer
  fields in the mmap'd lock file) use **function-based
  atomics** (`atomic.LoadUint64`, `atomic.StoreUint64`,
  `atomic.CompareAndSwapUint64`) on `unsafe.Pointer`-derived
  addresses. Typed atomics cannot be used here because the
  memory is a raw region in `MAP_SHARED` mmap visible across
  processes.
  The single 32-bit shared-memory field
  (`LockFileHeader.MaxReaders`) uses `atomic.LoadUint32` /
  `atomic.StoreUint32` by the same convention. Its alignment
  is guaranteed by `structs.HostLayout` placing it at offset 8
  (4-byte aligned, sufficient for the architecture's u32
  single-copy atomicity).

**Memory-model caveat.** Go's memory model formally describes
synchronization only on memory the Go runtime owns. Cross-
process shared memory via `MAP_SHARED` is outside that model —
the protocol's correctness rests on (a) Go's `sync/atomic`
functions emitting hardware atomic instructions
(`LOCK CMPXCHG` / aligned `MOV` on amd64; `LDAR` / `STLR` /
`LDXR`-`STXR` on arm64) and (b) the underlying hardware
guaranteeing single-copy atomicity for naturally-aligned
64-bit loads and stores. Both gmdb-supported architectures
(amd64, arm64) satisfy this.

All shared-memory fields are 8-byte aligned at runtime by
composition: each field is a `uint64` whose natural C ABI
alignment is 8 bytes (enforced inside the struct by
`structs.HostLayout`'s "no Go-internal reordering" guarantee),
and the struct's base address is page-aligned because it
lives inside a `MAP_SHARED` mapping (≥ 4096-byte alignment).
`HostLayout` itself controls field layout, not struct-pointer
alignment — the page-aligned mmap base is what makes the
struct-start address naturally aligned. Ports to architectures
with weaker single-copy atomicity guarantees would require
revisiting this section.

## Writer's Page Reclamation

Before reclaiming retired pages, the writer scans the reader
table to find the minimum active TxnID. Any RPL entries with
`TxnID < min_active` are safe to reclaim — their bits are set
in the bitmap. See `free-space.md §RPL Reclamation` for the
full reclamation-bound rule, which combines this scan with the
anchored epoch (`AnchoredDurableTxnID`).
