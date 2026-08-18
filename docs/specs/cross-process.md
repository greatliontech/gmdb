# Cross-Process Coordination

Processes sharing a gmdb file coordinate via a separate **lock
file** (`<dbname>.lock`) mmap'd as shared memory. This spec defines
the lock-file layout, the write-lock protocol (intra-process queue +
cross-process flock), the reader table and its per-slot kernel
locks (held-lock liveness: a reader is alive exactly while its
slot lock is held), and the last-writer heartbeat that feeds the
recovery-commit gate.

Scope:
- Lock file layout (header + reader slots) using `structs.HostLayout`.
- Lock file lifecycle (creation, validation, deletion).
- Write lock: flock goroutine, writer acquisition flow, stale
  writer recovery.
- Reader table: per-slot kernel locks, lock-then-store
  acquisition, probe-and-clear stale reclamation.
- Process start time as PID-reuse discriminator (writer records).
- PID namespace awareness (writer records).
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
  property=A reader slot's fields are mutated only while the
    mutator holds the slot's lock (§Reader Table, slot locks):
    acquisition takes the lock through the handle's hold
    description before its first store, release zeroes `TxnID`
    before dropping the lock, and a stale clear zeroes `TxnID`
    only inside a successful probe acquisition. There is no other
    writer of a slot;
  from=this spec §Reader Table (slot locks);
  violation=A store outside a held slot lock reintroduces the
    verdict/act gap the lock exists to close: a clearer racing a
    live acquirer evicts it mid-publish — its snapshot leaves the
    table, the reclamation bound advances past it, and RPL
    reclamation frees pages it is reading.

Invariant: kind=clause-explicit;
  property=In-process reader-slot acquisition is serialized by the
    handle's acquisition mutex, because two try-locks through one
    open file description do not conflict with each other; probes
    (stale clears) always go through a description distinct from
    every hold description;
  from=this spec §Reader Table (slot locks, same-description
    caveat);
  violation=Two same-handle Begins racing the same free slot both
    "acquire" it through the shared description and both publish —
    two snapshots on one slot, the second silently overwriting the
    first's pin; a probe through a hold description reads this
    process's own live reader as dead and clears it.

Invariant: kind=clause-explicit;
  property=A STAMPED heartbeat is never the literal value 0 — the
    coordination clock is floored at 1 ns, so
    `WriterHeartbeat == 0` always means "unstamped/cleared", never
    "stamped at the boot instant". Enforced by the Coord clock
    funnel; enforced by `TestCoordClockNeverStampsSentinel` and,
    end-to-end at boot-instant time, by the DST coordination suite.
    (Reader slots carry no heartbeat: their liveness is the held
    slot lock);
  from=this spec (the sentinel reading of `WriterHeartbeat == 0`
    in the writer records and the recovery-commit gate);
  violation=CLOCK_BOOTTIME legitimately reads 0 at the boot
    instant — unreachable for userspace on real kernels but exact
    under a virtualized boot clock. A writer record stamped 0
    reads instantly stale, evicting a live writer.

Invariant: kind=clause-explicit;
  property=A stale reader slot is cleared only by
    probe-and-clear: the clearer try-locks the slot through a
    probe description, and the verdict (acquired ⇒ the owner is
    gone) and the clear (`TxnID = 0` under the held probe) are one
    act. Held or undecided probes never clear;
  from=this spec §Reader Table (stale-slot reclamation);
  violation=A clear decided by anything but the held slot lock
    (an age window, an identity read) can fire against a live
    owner — a frozen reader's snapshot is evicted and RPL
    reclamation frees pages it resumes into (wrong values or
    ErrCorrupted on a healthy database), the exact failure class
    the lock-based protocol exists to delete.

Invariant: kind=clause-explicit;
  property=Every remaining heartbeat age comparison
    (`now - WriterHeartbeat` in the recovery-commit gate's
    last-writer classification) treats a stamp in the FUTURE
    (`stamp > now`) as fresh/live, never stale — i.e. classifies
    stale only when `stamp <= now AND now - stamp >
    StaleTimeout`. The monotonic-clock stamps are unsigned, so a
    naive `now - stamp > StaleTimeout` underflows to ~2^64 (> any
    timeout) for a future stamp and evicts a live owner. (Reader
    slots carry no age comparisons at all — their liveness is the
    held slot lock);
  from=this spec §Stale Writer Recovery and the recovery-commit
    gate (durability.md);
  violation=Nothing orders the stamper's clock read against the
    classifier's (both kernel-wide and boot-relative; skew bounded
    by scheduling, not clock domain): a record stamped after the
    classifier sampled `now` reads as future, underflows, and a
    live writer's record is classified dead. Backward clock skew
    (NTP step-back, manual set) is a second trigger.

Invariant: kind=clause-explicit;
  property=Only the flock goroutine ever calls `flock()` on the
    database's lock-file descriptor; at most one goroutine in the
    process is attempting `flock` on that descriptor at any moment
    (other packages' locks on other files — `oslock.md` — are
    outside this clause). Writers communicate with the flock
    goroutine via a request channel and a per-request response
    channel;
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
  from=this spec §Write Lock step 3 and §Writer Heartbeat
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
    processes) and the same process's last-writer refresher when
    `LOCK_EX` is not held by this process MUST NOT write
    `WriterHeartbeat`;
  from=this spec §Writer Heartbeat (writer-only updates);
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
    size = HeaderSize + (SlotSize × LockFileHeader.MaxReaders)
    (144 + 56 × MaxReaders at the current layout), where
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
| Header (144 bytes)                           |
| Magic              | uint64                  |
| MaxReaders         | uint32                  |
| TakeoverSeq        | uint32                  |  dead-author takeover counter
| UUID               | [16]byte                |  must match data file's UUID
| WriterPID          | uint64                  |
| WriterStartTime    | uint64                  |
| WriterPIDNamespace | uint64                  |
| WriterHeartbeat    | uint64                  |
| LastMaintenanceTime| uint64                  |
| LastWriterPID      | uint64                  |
| LastWriterStartTime| uint64                  |
| LastWriterPIDNS    | uint64                  |
| LastWriterHeartbeat| uint64                  |
| DataGeneration     | uint64                  |
| BootID             | [16]byte                |  boot-epoch discriminator
| ReadersDirNonce    | uint32 (trailing)       |  lock-file incarnation id
| ShrinkSeq          | uint64                  |  file-shrink seqlock
| RedirtyCoveredSeq  | uint32 (+4 pad)         |  covered-through takeover sequence
+----------------------------------------------+
| Reader Table                                 |
| +-------+-----+-----+------+-------+-------+ |
| | TxnID | PID | Reserved (5 x u64, retired fields) | Slot 0 (56 bytes)
| | u64   | u64 | u64 | u64  | u64| u64    | u64 | |
| +-------+-----+-----+------+-------+-------+ |
| | ...                                       | | up to MaxReaders slots
| +-------+-----+-----+------+-------+-------+ |
+----------------------------------------------+
| Notification Region (520 bytes)              |
| Global version word   | uint64               |  word 0: every commit
| Keyspace version words| [64]uint64           |  words 1..64: by name hash
+----------------------------------------------+
```

PST = Process Start Time. PIDN = PID Namespace (writer records
only). Reader-slot bytes past TxnID and the diagnostic PID are
RESERVED — the retired heartbeat-era fields (see §Reader slot).

TakeoverSeq counts torn-unpublished-write events: grant
acquisitions that observed a non-zero WriterPID — a holder that
died without its clear-before-unlock (definitionally dead under
the acquirer's LOCK_EX; no liveness classifier runs) — bumped
under the acquisition's LOCK_EX before stale-writer recovery
clears the header, plus publication-phase commit failures, where
the poisoning author bumps under the grant it still holds. Each
writable handle caches the value its bitmap + RPL state was last
rebuilt at and forces a full re-sync rebuild on mismatch
(`free-space.md §Grant-handoff tear detection`). It
occupies former header padding, so HeaderSize is unchanged —
layout GROWTH (the header, or any region that changes the total
file size, like the notification region) remains gated on shipping
with a data-format break (see the stale-recreate arm's safety
invariant).

The lock-file structures use Go structs with `structs.HostLayout`
(Go 1.24+), which guarantees the host platform's C ABI layout.
This allows safely overlaying Go structs on the mmap'd shared
memory region without manual byte-offset arithmetic.

```go
type LockFileHeader struct {
    _                   structs.HostLayout
    Magic               uint64
    MaxReaders          uint32
    TakeoverSeq         uint32
    UUID                [16]byte
    WriterPID           uint64
    WriterStartTime     uint64
    WriterPIDNamespace  uint64
    WriterHeartbeat     uint64
    LastMaintenanceTime uint64
    LastWriterPID          uint64
    LastWriterStartTime    uint64
    LastWriterPIDNamespace uint64
    LastWriterHeartbeat    uint64
    DataGeneration      uint64
    BootID              [16]byte
    // … ShrinkSeq, RedirtyCoveredSeq, ReadersDirNonce (see format.go)
    ShrinkSeq           uint64
    RedirtyCoveredSeq   uint32
    _                   [4]byte
}

type ReaderSlot struct {
    _                structs.HostLayout
    TxnID            uint64
    PID              uint64
    Reserved1 uint64 // retired: ProcessStartTime
    Reserved2 uint64 // retired: PIDNamespace
    Reserved3 uint64 // retired: Heartbeat
    Reserved4 uint64 // retired: HintEpoch
    Reserved5 uint64 // retired: Gen
}
```

The `HostLayout` marker applies only to the lock file's shared-
memory structures. Data file page formats remain raw byte layouts
with explicit endian-aware encode/decode functions for portability.

**Cross-platform portability of the lock file is not a goal.** The
lock file is transient coordination state — it PERSISTS on disk
(no code path deletes it in normal operation; persistence is
harmless because cross-boot state is invalidated by the boot
epoch below — or, on zero-boot-id platforms, accepted as the
zero-epoch residual documented under BootID — and a recreated
database stale-classifies it by
UUID) — and its layout deliberately follows the host platform's
C ABI. A lock file written by a little-endian process is not
readable by a big-endian process; mounting the database on a
different architecture requires deleting any stale lock file
manually. The data file itself is fully portable (little-endian,
explicit encode/decode).

### Header fields (144 bytes)

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
- **ReadersDirNonce**: the lock-file incarnation nonce — random,
  stamped at creation, immutable, boot-epoch-reset-surviving. The
  per-slot lock-FILE backend derives its directory name from it
  (§Reader Table, slot locks), so two incarnations can never share
  slot files.
- **BootID**: the boot-epoch discriminator (Linux
  `/proc/sys/kernel/random/boot_id`), stamped at creation and by
  every cross-boot adoption reset. Heartbeats use a boot-relative
  clock and process start times are ticks-since-boot, so every
  stamp and identity in this file is meaningful ONLY within the
  boot that wrote it: after a reboot, pre-boot heartbeats read as
  huge FUTURE values (honoured as fresh forever by the future-stamp
  guard) and PID/starttime identities can collide with new-boot
  processes — both legs would bypass the recovery-commit gate and
  pin reclamation. An adopter whose current boot differs RESETS the
  volatile coordination state under a non-blocking
  `flock(LOCK_EX)` conversion (contention ⇒ back off and re-adopt;
  the winner already stamped): the writer block,
  `LastMaintenanceTime`, the LastWriter record, `ShrinkSeq`,
  `TakeoverSeq` with its covered-through mark `RedirtyCoveredSeq`
  (zeroed together so the mark never leads the sequence it gates
  on), and
  every reader slot (including the reserved fields) are zeroed — no process from
  the stamped boot can exist, so nothing live is evicted — then the
  current boot id is stamped LAST (a crash mid-reset repeats the
  idempotent reset). `DataGeneration` and `ReadersDirNonce` SURVIVE
  (they identify inode replacements and the lock-file incarnation,
  not boot-relative time). The reset fires ONLY when
  both the stamped and the current boot id are KNOWN (non-zero) and
  differ: a zero on either side (unreadable `/proc` in a chroot or
  mount namespace) DISABLES cross-boot invalidation — resetting on
  an unknown epoch could zero a LIVE same-boot peer's slots
  (use-after-reclaim), strictly worse than the cross-boot staleness
  it would fix. Zero-epoch environments therefore keep the
  pre-boot-epoch semantics (future-stamp guard only) as a
  documented residual. The future-stamp guard's trust is thereby
  scoped to SAME-BOOT stamps; within one boot it remains
  load-bearing for mid-publish clock skew. A header CREATED by a
  zero-epoch process keeps `BootID = 0` for the file's lifetime —
  known-epoch adopters never upgrade the stamp (stamping without
  resetting would bless unknown-epoch state; stamping with a reset
  re-opens the live-peer eviction), so invalidation stays disabled
  for that file, across real reboots too, until it is
  stale-cycled (UUID or layout change). (Pinned by `TestAdoptForeignBootEpochResetsCoordinationState`
  and `TestAdoptSameBootPreservesCoordinationState`.)
- **RedirtyCoveredSeq**: the TakeoverSeq value through which the
  dropped-writeback recovery rewrite has run AND a covering
  fdatasync has completed (durability.md §Anchoring). Read and
  written only under the write grant, where TakeoverSeq is stable:
  a recovery-lineage attach redirties the attached extent only when
  this trails TakeoverSeq, then barriers and stores the value it
  read. Every poison/death bump reopens the gate; a healthy
  database keeps the two equal, so ordinary writable Opens pay a
  header compare. Zeroed by the boot-epoch reset together with
  TakeoverSeq, so the mark never leads the sequence it gates on;
  a differently-sized pre-field lock file never coexists — the
  format-version gate partitions such binaries off the data file
  before the size arm could stale-cycle a live peer's lock file.
- **ShrinkSeq**: the file-shrink seqlock — see `file-format.md`
  §File Shrinkage for the protocol (writer brackets its
  scan→truncate span odd/even under the write grant; readers
  bracket their file-size read and re-read on overlap; stale-writer
  recovery re-evens a counter left odd by a writer crash).
- **LastWriterPID / LastWriterStartTime / LastWriterPIDNamespace /
  LastWriterHeartbeat** (atomic): the persisted identity of the most
  recent write-grant holder. Written at every grant acquisition
  under LOCK_EX and — unlike the Writer* block — NOT cleared at
  release; the author handle's last-writer refresher goroutine
  (§Writer Heartbeat) refreshes `LastWriterHeartbeat` for the
  handle's LIFETIME while the record still names its process. Only the last writer's process can hold
  unfsynced live commits in the shared page cache, so this record is
  the recovery-commit gate's author-liveness signal (`durability.md
  §Recovery` step 5): recovery must not roll the database back while
  the author lives, even if it is idle (no grant, no reader slots).
  Classification mirrors the reader-slot rules (same-namespace
  kill(0) + start-time; cross-namespace heartbeat staleness).

### Reader slot (56 bytes)

- **TxnID** (atomic): snapshot TxnID held by this reader. `0` =
  free. Mutated only under the held slot lock (§Reader Table,
  slot locks) — the lock, not any field, is the slot's liveness.
- **PID** (atomic): owning process — DIAGNOSTIC only, never
  consulted for liveness. `uint64` for alignment + forward
  safety.
- **Reserved** (5 × uint64): the retired heartbeat-era fields
  (start time, namespace, heartbeat, generation, orphan epoch).
  Zeroed on acquire, never read. The 56-byte slot size is
  retained so the table geometry and the size formula are
  untouched by the liveness rework; `Magic` itself is bumped
  regardless (the heartbeat-era value survives as `MagicV1`),
  because a heartbeat-era peer sharing the file would evict
  lock-era readers (no heartbeats to observe) — mixed-format
  peers must not coordinate. A `MagicV1` file is handled by the
  stale-FORMAT arm of §Lock File Lifecycle's stale detection.

Total size: `144 + (56 × MaxReaders) + 520`. Default 4096 readers:
`144 + 229376 + 520 = 230040` bytes (~225 KB).

`MaxReaders` is bounded `[1, 65536]`. The lower bound is one slot
(degenerate but legal); the upper bound caps the mmap at
`144 + 56 × 65536 + 520 ≈ 3.5 MiB`, so a corrupted or
maliciously-crafted header value cannot demand a petabyte-scale
mmap. A header `MaxReaders` value outside this range is treated as
`ErrCorrupted` by `Open`.

The lock file is mmap'd `MAP_SHARED` by all processes for the
reader table and the notification region. The write lock is a
separate concern via `flock()`.

### Notification region

A fixed array of 65 `uint64` version words after the reader table
(8-byte aligned: both the header size and the slot size are
multiples of 8). The words carry opaque, mutually comparable commit
versions — the substrate of the public change-notification API
(`api-surface.md §Change Notification`): a waiter remembers a
version `from` and blocks until a word exceeds it.

- **Word 0 — global version.** Incremented (atomic `+1`) by every
  commit's notification publish. CAS-max seeded from the adopted
  meta's `TxnID` by every `Open`, so versions continue ascending
  across a lock-file recreation on a database that has never been
  compacted. It is NOT the meta `TxnID`: `Compact` resets the
  `TxnID`, and a version word must never regress within the file's
  lifetime — waiters compare `value > from`, so a regression would
  hide every later commit from an existing waiter (the monotonicity
  invariant; the CAS-max seed and the grant-serialized publishes
  together enforce it).
- **Words 1..64 — keyspace versions.** A commit that touched a
  keyspace — data writes, creation, configuration change, deletion
  — stamps the keyspace's word, `1 + XXH3-64(name) mod 64`, with
  the just-incremented GLOBAL version. Stamping the global value
  (rather than counting per word) keeps every word comparable with
  every observed version: `word > from` ⇔ a commit newer than
  `from` touched a keyspace in that hash class. Name collisions
  only widen a wake to unrelated waiters — spurious wakeups are
  part of the wait contract.

**Publish ordering.** The publish runs under the still-held write
grant (serializing the stamps), strictly AFTER the commit's meta
publication: a woken waiter immediately reads the database and must
observe the commit — its wake is consumed either way. Commits whose
error is classified visible or durability-unknown
(`durability.md §Commit Outcome Classification`) publish too: peers
can see them, so waiters must not sleep through them. Unclassified
commit failures do not publish — delivery is best-effort there, and
the next successful commit's stamp covers the gap.

**Waiting.** On Linux, waiters block in a shared `futex(2)`
(`FUTEX_WAIT` without `FUTEX_PRIVATE_FLAG`) on the low-order 32
bits of the word — a publish moves the global word by exactly 1,
and any same-low-half movement (an Open-time CAS-max seed jump, or
a keyspace stamp landing ≥2^32 commits after the word's previous
value) costs a waiter at most one sleep slice, never a lost wake —
with a bounded sleep slice for liveness re-checks; publishes and
context cancellation issue `FUTEX_WAKE`. Elsewhere, waiters poll the word with an adaptive
backoff (sub-millisecond floor, single-digit-millisecond cap).
Spurious wakeups (collisions, cancellation wakes on shared words)
are absorbed by re-checking `value > from` before returning.

**No notification region** exists on the read-only lock-free
fallback (no lock file; see `mmap-strategy.md §Read-Only`): the
waits degrade to polling the data file's committed meta `TxnID`,
and keyspace-scoped waits degrade to global waits — conforming,
since both only add spurious wakes.

## Lock File Lifecycle

Transient coordination state (persists on disk; see the
portability note in §Lock File Layout — persistence is harmless).
The first process to open the database creates the lock file, writes the header
(`Magic`, `MaxReaders`, `WriterPID = 0`, `WriterStartTime = 0`),
and initializes all slots to zero. Subsequent processes validate
`Magic`, read `MaxReaders`, mmap at the corresponding size. If
deleted (e.g., after all processes exit), the next opener
recreates it. `MaxReaders` is NOT in the data file — it is a
runtime coordination property.

**Stale detection and identity-guarded removal.** Three validated
states classify an existing lock file as STALE and route to
delete-and-recreate: a `UUID` that does not match the data file's
(a database recreated at the same path); a plausible header
(valid `Magic`, in-range `MaxReaders`) whose file size disagrees
with the current layout — a lock file written by a binary with a
different header layout; and a header carrying the heartbeat-era
magic (`MagicV1`) — a stale-FORMAT file, never adopted, because a
heartbeat-era peer sharing the table would evict lock-era readers
(they publish no heartbeats). The size arm is sound only because a
layout change ships with a data-format-incompatible change: an
old-binary peer can never be LIVE on the same data file (it cannot
even open it). Growing the header WITHOUT a data-format break
would make that arm reachable with a live old-binary peer — grow
the header only alongside a data-format break, or replace the arm
first. The `MagicV1` arm has no such structural guarantee (the
data format did not break at the liveness rework): its soundness
rests on not running mixed-format binaries against one data file
concurrently. The identity guard below still refuses removal
while a live heartbeat-era WRITER holds its flock; an idle or
read-only heartbeat-era handle holds no kernel lock and is
undetectable by construction.

Every by-name unlink of the lock file is IDENTITY-GUARDED. The
stale-removal path holds `flock(LOCK_EX)` on the fd it VALIDATED
(non-blocking — contention means another remover or a live legacy
coordinator; skip and retry with backoff) and re-verifies, under
that lock, that the name still points at the validated inode
(fstat vs path stat); a re-bound or already-gone name skips the
removal, and a stat failure other than not-exist is surfaced,
never treated as a skip. The creator's failed-creation cleanup is
SameFile-guarded only (no flock, no retry): it removes the name
solely when it still points at the inode its own O_CREATE|O_EXCL
made, and its guard errors are best-effort (the cleanup runs in a
failure path that already has an error to surface). An unguarded
`unlink(name)` races a concurrent opener that already removed the
stale file and created a fresh one: the unlink takes out the FRESH file and its creator
coordinates on an unlinked inode — two lock files, two
simultaneous writers, meta overwrite. After adopt or create, and
before coordinating, every opener re-verifies its fd still names
the path's inode (defence against an unguarded remover — an old
binary or external tampering); a mismatch retries against the
current binding.
(Removal guard pinned by `TestOpenStaleRemovalSkipsRecreatedLockFile`,
`TestOpenStaleRemovalSkipsWhenNameAlreadyGone`,
`TestOpenStaleRemovalSkipsUnderLiveFlockHolder`, and
`TestOpenSizeMismatchRoutesThroughGuardedRemoval`; the identity
verify by `TestVerifyPathIdentity`.)

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
≈ 800 ms total).

Budget exhaustion on a partially-initialised file is the
CRASHED-CREATOR staleness class — a power loss between the
create and a completed init leaves exactly this state, and the
crash forecloses the polite unlink below. It is recovered like
every other staleness class (the lock file is transient
coordination state), under a guard strictly stronger than the
stale-removal guard, because a zero header is momentarily
indistinguishable from a live creator's open→flock window:
(a) the SAME inode must have been pinned partially-initialised
for a minimum observation window (500 ms) with NO other
lifecycle arm running since the pin — far longer than any
legitimate init, and a fresh creator's file is a different
inode; (b) `flock(LOCK_EX|LOCK_NB)` must succeed — a live
mid-init creator holds `LOCK_EX`; (c) the name↔inode binding
and (d) the still-unpublished header are re-verified UNDER the
lock. The file is then removed and the lifecycle re-runs once.
A recovery abort that indicates a LIVE peer advanced the file —
contended guard, replaced inode, re-bound name, published
header — ALSO re-runs the lifecycle (the name is serviceable;
reporting corruption would be false); only a second exhaustion,
an unstable or too-young pin, or a genuine recovery I/O error
surfaces `ErrCorrupted`. For the crashed-creator class the
removal cannot orphan a live coordinator — no peer can adopt a
never-finalised file (adoption requires a published header).
External tampering that zeroes a FINALISED file's Magic is
treated as stale by the same recovery; live adopters of the
pre-tamper file are then orphaned — the availability choice
already made for UUID-zeroing tampering, under this spec's
existing out-of-model caveat for tampering. The pin identity is
dev+inode+mtime+size — dev+inode alone is insufficient, since
filesystems hand a freed inode number straight back to the next
create (empirically the common case on ext4), while a
never-progressing stuck file keeps mtime and size bit-identical.
Accepted residual: a recreate forging an identical
dev+inode+mtime+size tuple inside the creator's
microseconds-wide open→flock window — realistic only within one
filesystem timestamp tick (nanoseconds on ext4/XFS/APFS; the
~15 ms update granularity on NTFS widens it); the flock,
published-header, and identity re-checks confine even that to a
retried open, never a split brain.

A failed creator whose failure path still runs (an
`initLockFile` syscall error, any non-crash exit) unlinks the
in-progress file before returning, so subsequent adopters do
not waste the retry budget on a known-stuck file; only a hard
crash leaves the torn file for the recovery above.

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
   fileSize + commit-state from disk (`Pager.Resync`). An unchanged
   TxnID skips the rebuild — EXCEPT when the header's takeover
   sequence differs from the value cached at this handle's last full
   rebuild, which forces it: a holder that died mid-reclamation — or whose
   publication-phase commit failed — leaves torn bitmap writes with
   no TxnID advance, and every surviving handle's chain must be
   rebuilt over them
   (`free-space.md §Grant-handoff tear detection`). The mmap is
   reused unchanged: `MaxSize` and `PageSize` are immutable for the
   file's life, so the reservation always covers the current file
   (a peer can only grow it up to `MaxSize`). Skipping this step is a
   guaranteed lost-update + page-aliasing corruption for serialized
   cross-process writers (the writer builds on a stale root, writes
   its meta over the slot holding the peer's newer commit, and
   allocates pages the peer's committed tree references) — see the
   Writer-grant freshness invariant.

   **Selection and projections.** The re-sync adopts the
   highest-valid-TxnID meta (`pager.ActiveMeta`) — the one selection
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
whether the writer is alive using namespace-aware identity
classification (readers need none — their liveness is the held
slot lock):

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
2. Clear `WriterPID`, `WriterStartTime`, `WriterPIDNamespace`,
   `WriterHeartbeat`. (The dead writer's own reader slots need no
   identity-matched scan: the kernel released their slot locks
   with the process, so they are ordinary stale slots for the
   probe-based reap — §Reader Table, stale-slot reclamation.)

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

Slot allocation uses a simple scan guarded by per-slot kernel
locks — no free stack or other auxiliary data structure. The
reader table is a flat array of 56-byte slots in the lock file's
shared mmap; slot CONTENT uses atomic memory ops visible across
processes, and slot OWNERSHIP is a held advisory lock the kernel
releases at process death (SIGKILL included). Liveness is never
read from memory: a slot is alive exactly while its lock is held,
judged by try-acquisition — the verdict and any consequent claim
or clear are one act, so no verdict can go stale between judging
and acting. No heartbeat, no identity, no timer ever decides a
reader's death.

**Snapshot selection.** A read transaction (`BeginRead`) snapshots
the **latest committed** on-disk meta — both meta pages are re-read
and the highest-valid-TxnID one is chosen (`pager.ActiveMeta`) — NOT
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

### Slot locks

Each slot `i` has one kernel advisory lock — the slot's liveness
authority:

- **Linux**: an open-file-description (OFD) byte-range write lock
  over the slot's own 56 bytes of the lock file
  (`fcntl(F_OFD_SETLK)` on `[readerTableOffset + 56·i, 56)`).
  Each handle keeps two descriptions of the lock file: the HOLD
  description, on which its own readers' ranges are held, and a
  PROBE description for judge-only try-locks. OFD range locks and
  the writer's `flock` are independent lock systems on every
  supported local filesystem, so they coexist on one file without
  interaction; a network filesystem that emulates one over the
  other could collide, and is already excluded by the shared
  mmap the protocol requires.
- **macOS / FreeBSD**: OFD range locks do not exist and POSIX
  range locks are per-process (any close of any descriptor drops
  them — unsound for multi-handle processes), so each slot's lock
  is `flock` on a per-slot lock FILE
  (`<lock>.readers-<nonce>/<i>`), opened per acquisition. The
  directory and EVERY slot file are created eagerly by the
  lock-file creator, under its `LOCK_EX`, before the header
  publishes — the open path never creates, so a vanished entry
  fails CLOSED for every handle (see the protection boundary
  below). Creation cost: `MaxReaders` empty files plus two
  directory fsyncs, once per incarnation — 4096 files at the
  default, and as many persistent inodes per database for the
  incarnation's lifetime (a sizing consideration on fixed-inode
  filesystems hosting many small databases; dynamic-inode
  filesystems like APFS/ZFS are unaffected). `<nonce>` is the
  header's incarnation nonce
  (`ReadersDirNonce`): stamped random at lock-file creation,
  immutable, boot-epoch-reset-surviving, read by every adopter
  from the mapped header. Scoping the directory to the lock-file
  INCARNATION makes cross-incarnation aliasing unrepresentable —
  a recreated lock file (UUID mismatch, stale format) stamps a
  fresh nonce and derives a fresh directory, however the
  filesystem reuses inodes — so a prior incarnation's surviving
  holders cannot wedge the fresh reader table by holding locks
  on same-named slot files. A superseded directory is removed by
  the lifecycle itself: the guarded stale removal deletes the
  outgoing incarnation's directory under the same `LOCK_EX` and
  identity proof as the lock-file unlink, so litter never
  accumulates. That is the ONLY sanctioned removal — externally
  deleting a readers directory (or a slot file) while any process
  has the database open is outside the protection boundary,
  exactly as unlinking the live lock file is: a recreated
  same-named entry is a fresh inode, surviving holders' locks
  ride the unlinked one, and mutual exclusion silently splits.
  The acquisition and probe paths therefore fail CLOSED on a
  vanished directory or slot file (undecided error) rather than
  recreating either — nothing ever recreates a slot file inside
  a live incarnation. The bound consumer is the one path that
  cannot fail closed: nonzero residue on an unprobeable slot
  keeps pinning `OldestReaderTxnID` (conservative — never an
  eviction), so the reap SURFACES its undecided-probe count and
  background maintenance logs it every pass — a halted
  reclamation bound is observable, never silent.
  The creator's eager population also sweeps leftover readers
  directories (orphans of crashed inits, whose unpublished nonce
  nothing else can name, and residue of crashed removals):
  anything present at CREATE time is provably not the live
  incarnation. Descriptor
  budget: one open descriptor per ACTIVE
  read transaction — proportional to actual concurrency, never
  to `MaxReaders` — documented as this tier's cost.
- **Windows**: unchanged — the lock mmap shim is unsupported, so
  a writable open fails and a read-only open falls back to
  lock-free operation (see PLATFORM SUPPORT).

**Descriptions outlive Close while slots are outstanding.** A
read transaction may legally outlive `Close()` (leak-detection.md):
its slot lock must stay HELD until that transaction releases, or
peers would reclaim the slot under a live post-Close reader — so
the hold description is refcounted by outstanding slots and closed
only when the last releases (the per-slot-file tier gets this for
free: each acquisition owns its descriptor). This is strictly
stronger than the heartbeat era, which AGED OUT a post-Close
reader's slot cross-namespace — a documented snapshot loss now
unrepresentable.

**Same-description caveat.** Two try-locks through one open file
description do not conflict, on any platform. Consequences, both
load-bearing: in-process acquisition is serialized by the
handle's acquisition mutex (two same-handle Begins racing one
free slot would otherwise both "acquire" it), and probes always
run through a description distinct from every hold description
(a probe through a hold description would read this process's
own live reader as dead). The per-slot-file tier gets the second
property for free (each acquisition opens its own description);
the range tier keeps a dedicated probe description per handle.

### Slot acquire (`Begin` read transaction)

Under the handle's acquisition mutex:

1. Scan from the **slot hint** (`coord.readerSlotHint`) with
   wraparound for `TxnID == 0` (likely free).
2. Try-lock the candidate slot's lock through the hold
   description. Busy ⇒ another owner got there first (or a
   concurrent prober is mid-clear) — continue scanning. Acquired
   ⇒ the slot is OWNED: the held lock is the ownership token, and
   there is no CAS, no generation, no publish ordering, and no
   ownership verify — nothing else can write the slot while the
   lock is held. Any other try-lock error is UNDECIDED — the
   slot is skipped (never stolen on an error), and the outcome
   class survives to step 5.
3. Store `TxnID = ` the snapshot meta's TxnID (plain atomic
   store — any residue belonged to a dead owner, whose lock the
   kernel released; the held lock excludes every other writer of
   the slot). Store `PID` (diagnostic). Zero the reserved fields.
4. Register the slot in the handle's active list (Close-time
   cleanup), update the hint.
5. If a full wraparound finds no free slot, a SECOND wraparound
   try-locks EVERY slot: an acquired nonzero one is a dead
   owner's residue — take it (store TxnID over it), the inline
   form of stale reclamation — and an acquired zero one closes
   the pass-boundary race (a peer releasing between the passes
   leaves a slot the first pass skipped as occupied and a
   nonzero-only second pass would skip as free). `ErrReadersFull`
   is returned only when every slot's lock is HELD; if any
   try-lock was UNDECIDED and no slot was won, the acquisition
   surfaces that error as a distinct failure — never
   `ErrReadersFull`, which asserts a table full of live readers.
6. **Snapshot restabilization** — unchanged from the heartbeat
   era, because its proof never depended on how liveness is
   judged: the meta whose TxnID seeded step 3 was read BEFORE the
   slot became visible, so after publishing, re-read the latest
   meta; if its TxnID differs, raise the slot's `TxnID` (an
   owner-only overwrite, trivially exclusive under the held lock)
   and repeat until one re-read returns the pinned TxnID
   unchanged.

   Why a stable re-read is sufficient: pages of tree `T` are only
   reclaimed through RPL segments with `TxnID t > T`, and
   reclamation runs strictly after the meta carrying `t` was
   written. If the post-publish re-read still returns `T`, no such
   meta existed at re-read time — nothing of tree `T` had been
   reclaimed — and from that instant every writer's bound scan
   sees this slot, flooring the bound at `T`. The argument is
   independent of how scanners treat unpublished slots, so the
   window between taking the slot lock and the `TxnID` store
   needs no scanner-side rule at all.

The hint is process-local, updated with a relaxed atomic store —
no cross-process coordination. Under steady-state load the scan
completes in 1–2 iterations; worst case wraps to O(MaxReaders)
with one try-lock syscall per candidate actually contended.

### Slot release (`Commit` / `Rollback` read transaction)

In order: store `TxnID = 0`, store `PID = 0`, then release the
slot lock (unlock the range, or close the per-slot descriptor).
Zero-before-release mirrors acquire's lock-before-store: the slot
is observably free before it is claimable, so no prober ever
holds a slot whose fields still claim a snapshot. Only the owner
releases — enforced by possession of the held lock, not by
discipline.

### Stale-slot reclamation

A slot is stale exactly when `TxnID != 0` and its lock is
acquirable. The verdict and the clear are ONE act — probe-and-
clear: try-lock the slot through a probe description; acquired ⇒
the owner's process is gone (kernel-proven) ⇒ store `TxnID = 0`
under the held probe ⇒ release. A held probe means a live owner;
an undecided probe (open failure and the like) is never a stale
verdict. There is no guarded-clear observation tuple, no
namespace classification, no age window, and no orphan epoch:
the eviction races those mechanisms managed are unrepresentable,
because clearing requires holding the very lock a live owner
holds. A frozen reader — the heartbeat era's documented residual
gap — is simply live: it holds its lock, pins its snapshot, and
resumes safely.

Where reclamation runs: the background maintenance pass
(`ReapStaleReaderSlots`), the acquire path's second wraparound
(inline, on table pressure), and a writer whose reclamation
bound is pinned by a suspicious slot may run the reap before
recomputing. The reap needs NO write grant: cross-handle clearers
serialize on the slot lock itself, and same-handle clearers — who
share ONE probe description, which cannot conflict with itself —
serialize on a per-handle mutex. Read-only handles may reap too —
the heartbeat era's "read-only fleets never reap" deployment
bound is DELETED (a crashed reader's slot is reclaimable by any
peer, writable or not).

**Bound scans stay pure memory reads.** `OldestReaderTxnID`
computes the minimum over nonzero slots with atomic loads only —
no per-slot syscalls on the write path. A stale slot
conservatively pins the bound until a reap clears it; a
mid-acquire slot needs no special floor (see the restabilization
argument above).

### Go goroutine model

Multiple slots may share the same PID (one process running
multiple read transactions) — each read transaction owns one
slot through the handle's hold description (Linux) or its own
per-slot descriptor (macOS/FreeBSD). Slot allocation across
goroutines of one handle is serialized by the acquisition mutex;
across handles and processes, by the kernel's lock conflict. A
single Go process running N concurrent read transactions
consumes N reader slots. Set `MaxReaders` high enough for the
expected total across all processes.

## Process Start Time

The WRITER header stores **start time** alongside PID (reader
slots retired theirs — reader liveness is the held slot lock).
Monotonically-increasing value that changes when a PID is
recycled — unique `(PID, StartTime)` per process lifetime.

At `Open()`, the process reads its own start time once and caches
it on the DB struct (`db.processStartTime uint64`). Stored in
`WriterStartTime` on write-lock acquisition.

During stale-writer classification, the checker reads the current
start time for a given PID via `processStartTime(pid int)
(uint64, error)`. If the PID is alive but the current start time
differs from the stored value, the PID was recycled.

| Platform | Source | Value | Notes |
|----------|--------|-------|-------|
| Linux | `/proc/[pid]/stat` field 22 | Clock ticks since boot (uint64) | No privileges. Pure Go: `os.ReadFile` + parse. |
| macOS | `sysctl KERN_PROC_PID` → `kinfo_proc.kp_proc.p_starttime` | timeval packed as `sec*1e6+usec` | Same-user processes. PORT DESIGN — not shipped. |
| FreeBSD | `sysctl KERN_PROC_PID` → `kinfo_proc.ki_start` | timeval packed | Same as macOS interface. PORT DESIGN — not shipped. |
| Windows | `GetProcessTimes` creation time | FILETIME (100 ns units since 1601) | Same-user processes without SeDebugPrivilege need `PROCESS_QUERY_LIMITED_INFORMATION`. PORT DESIGN — not shipped. |

Only the Linux row is implemented; the macOS/FreeBSD/Windows rows
are the settled design for those platforms (see PLATFORM SUPPORT
under §Writer Heartbeat — the non-Linux helpers ship as error
stubs and liveness there is heartbeat-only).

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
process lifetimes is benign for the one remaining consumer (the
writer-record classification): the flock gate, not the identity,
is what admits a recoverer — the identity only labels the record.

## PID Namespace Awareness

PID-based classification operates within the caller's PID
namespace; a PID in one container refers to a different (or
nonexistent) process in another. Reader slots retired their
namespace field with the rest of identity-based liveness — a
held lock conflicts across every namespace of a host, so
cross-namespace readers need no classification at all. The one
remaining consumer is the WRITER header: `WriterPIDNamespace` is
read from `/proc/self/ns/pid` via `readlink` at Open (Linux;
0 elsewhere or on failure, logged via `slog.Logger`) and lets
the recovery-commit gate's last-writer classification route
same-namespace records through the `(PID, StartTime)` fast path
and everything else through the heartbeat window — conservative
in both directions.

## Writer Heartbeat

There is no reader heartbeat: reader liveness is the held slot
lock, released by the kernel at process death, and the per-slot
refresh — one store per active slot per second, with the
tick/active-list race notes that came with it — is deleted with
it. The in-process **active-slot list** (`[]uint32` under
`db.activeSlotsMu`) survives for one narrower job: Close-time
cleanup of this handle's own slots.

The heartbeat GOROUTINE survives, narrowed to one job: while the
LastWriter record still names this process, it refreshes
`LastWriterHeartbeat` each tick — the recovery-commit gate's
author-liveness signal (durability.md §Recovery step 5), a
bounded residual of clock-based judgment that the flock gate
itself never depends on. A handle that never commits never
refreshes anything.

**Writer-only updates.** `WriterHeartbeat` is refreshed only by
the flock goroutine, inside its §Write Lock step-4 hold loop, at
the same `HeartbeatInterval` cadence. The flock goroutine holds
`LOCK_EX` continuously from the step-3 publish to the step-4
clear+unlock, so every refresh lands under `LOCK_EX` *by
construction*: this goroutine is the sole writer of
`WriterHeartbeat` and writes it only inside the hold window. There
is no in-process "holding" flag a separate goroutine could read
stale and so stomp the field after this process's `LOCK_UN`. The
last-writer refresher never writes `WriterHeartbeat` — it
refreshes only `LastWriterHeartbeat`, and only while the record
names this process. Were a non-holder to write `WriterHeartbeat` it
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

PLATFORM SUPPORT: the lock file — and with it the cross-process
coordination this spec defines — is implemented on the unix
family (Linux, macOS, FreeBSD). Outside that family (windows) the
lock mmap shim returns an unsupported-platform error, so a
WRITABLE open fails outright and a read-only open falls back to
lock-free operation. The non-Linux family members run with the
degradations this spec defines: adaptive-poll notification waits
(no shared futex), zero boot id (cross-boot invalidation
disabled), per-slot lock FILES instead of OFD ranges for reader
liveness (§Reader Table, slot locks — same verdicts, a
descriptor per active reader), and writer-record classification
falling to the heartbeat window (`ProcessStartTime` ships as an
error stub until the sysctl-based designs in §Process Start Time
are implemented). `CLOCK_MONOTONIC` on macOS / FreeBSD is
kernel-wide and boot-relative but does not survive suspend; on a
laptop that resumes after a long sleep, the heartbeat clock jumps
forward by less than wall-time elapsed, so `StaleTimeout`'s
10-second default is safe — false-stale detection requires a
heartbeat older than 10 s of *monotonic* time, which a suspended
process cannot accumulate.

WINDOWS PORT DESIGN — the settled contract for a windows port; it
describes no shipped behavior until that port lands:

- **flock**: whole-file advisory semantics emulated with a one-byte
  `LockFileEx`/`UnlockFileEx` range at offset 2^63−1 — beyond any
  byte the lock file ever contains, because windows byte-range locks
  are MANDATORY against `ReadFile`/`WriteFile` (mapped-view access
  is not checked) and must never intersect the real read/write
  paths. Shared/exclusive map to the `LOCKFILE_EXCLUSIVE_LOCK` flag;
  non-blocking to `LOCKFILE_FAIL_IMMEDIATELY`. The shared→exclusive
  CONVERSION is unlock-then-try-lock — exactly `flock(2)`'s
  documented non-atomic conversion, which every conversion caller
  already tolerates (contention ⇒ back off / retry; the §Lock File
  Lifecycle validation re-runs under the new lock). Blocking
  acquisitions poll the non-blocking variant (windows has no EINTR;
  the blocking windows are brief creator-init races).
- **monotonic clock**: `QueryUnbiasedInterruptTime` — kernel-wide,
  boot-relative, excludes suspend; the `StaleTimeout` suspend
  analysis above applies unchanged.
- **process start time**: `GetProcessTimes` creation time
  (§Process Start Time).
- **boot id**: no clean source → zero; the zero-epoch residual
  under §Lock File Layout BootID applies.
- **futex**: the adaptive-poll fallback (`WaitOnAddress` is
  within-process only, so polling is the correct degradation).
- **fdatasync**: the full-strength fsync fallback
  (`FlushFileBuffers`; durability.md §Platform sync primitives).
- **stale lock-file removal**: kernel-gated like every windows
  replace/delete of a mapped file — removal fails while any process
  maps the stale file, surfacing as a clean, retryable Open error.
  Sound: whatever still maps the stale file — a dead boot's
  leftovers, a foreign/replaced database's peers, or an
  older-layout process on this database — is coordination state
  this Open must replace either way, and a later Open retries once
  the mappers are gone.

`StaleTimeout` (default 10 s) survives only for the writer-record
classifications above; no reader path consults it. Must remain
significantly larger than the writer heartbeat interval (1 s) for
scheduling jitter.

**Shutdown coordination.** `Close()` stores the shared close
gate's closed flag (atomic, see `leak-detection.md`), closes the
last-writer refresher's stop channel and **waits** for it to
acknowledge before releasing this handle's remaining reader-slot
locks (the active-slot list) and unmapping the lock file —
without the wait, a final refresh tick could race the munmap and
SIGSEGV. The wait is bounded by the tick interval (~1 s).

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
