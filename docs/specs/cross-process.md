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
  property=When a writer clears a stale slot, it stores
    `HintEpoch = 0` *before* `TxnID = 0`. The slot is observably
    non-free during the `HintEpoch` reset, preventing a fresh
    acquirer from inheriting a stale epoch;
  from=this spec §Reader Table (stale detection — clear ordering);
  violation=Reversed order leaves a window where a fresh acquirer
    can CAS-win `TxnID` and then crash before its `Heartbeat`
    store, inheriting the prior epoch (already aged out) — the
    next scan immediately re-clears the slot, evicting a genuinely
    in-progress acquirer faster than `StaleTimeout` and violating
    the per-occupant timer invariant.

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
    size = 72 + (48 × LockFileHeader.MaxReaders), where
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

## Lock File Layout

```
Lock File
+----------------------------------------------+
| Header (72 bytes)                            |
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

### Header fields (72 bytes)

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
  `CLOCK_MONOTONIC` elsewhere, updated periodically by the
  heartbeat goroutine while the write lock is held.
- **LastMaintenanceTime**: updated after a maintenance pass
  completes (see `background-maintenance.md`).

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

Total size: `72 + (48 × MaxReaders)`. Default 4096 readers:
`72 + 196608 = 196680` bytes (~192 KB).

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
3. Store `WriterPID`, `WriterStartTime`, `WriterPIDNamespace` in
   the lock-file header; send `nil` on `req.result` — writer
   holds the lock.
4. `select` on (writer's release channel, `db.stopCh`).
   - Release: clear `WriterPID` / `WriterStartTime` /
     `WriterPIDNamespace`, `flock(LOCK_UN)`, loop to step 1.
   - `db.stopCh`: clear writer header fields, `flock(LOCK_UN)`,
     exit.

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
2. Scan the reader table for slots with the dead writer's PID
   (in the same PID namespace) and clear them.
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

### Slot acquire (`Begin` read transaction)

The acquire sequence is structured so that a crash at *any* point
after the CAS leaves the slot in a state the stale-detector can
reclaim. Heartbeat is written first (so a crash mid-acquire still
gives the slot a "recent liveness" anchor that will eventually go
stale); PID is written last (so the detector's PID-based fast
path is only used once the full identity has been populated).

1. Start scanning from the **slot hint** (`db.readerSlotHint`, an
   `atomic.Uint32` on the DB struct) rather than slot 0.
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
6. Update `db.readerSlotHint`.
7. If all slots occupied (full wraparound), return
   `ErrReadersFull`.

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
     `HintEpoch` from 0 to `now`. **Skip** this round; the next
     scan (from any process) compares against the stored epoch.
   - If `HintEpoch != 0 AND now - HintEpoch > StaleTimeout`:
     confirmed orphan. **Clear `TxnID = 0`** (and `HintEpoch`
     via the post-clear cleanup below).
   - Otherwise: **skip**.

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

When the writer clears a stale slot, it stores in this exact
order:

1. `HintEpoch = 0` (atomic). Resets the orphan-detection anchor
   *while the slot is still observably non-free*, so no acquirer
   can race into the slot and inherit a stale epoch.
2. `TxnID = 0` (atomic). Final release — slot is now free.

The slot's PID/PST/PIDN/Heartbeat are left as-is and will be
overwritten by the next acquirer per the acquire ordering above.

The HintEpoch-first ordering is load-bearing: reversed, a window
exists between `TxnID = 0` and `HintEpoch = 0` during which a
fresh acquirer can CAS-win `TxnID` and crash before step 4a
(heartbeat store). A subsequent stale-detection scan would then
see `TxnID != 0, PID == 0, Heartbeat == 0,
HintEpoch = <stale value from prior cycle>` and immediately
re-clear the slot via case (c)'s timer (already aged out),
evicting the (genuinely dead) new acquirer faster than
StaleTimeout — benign for that slot but violating the
per-occupant timer invariant. Zeroing HintEpoch first closes
the window.

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
Open, stopped at Close) that periodically updates the
`Heartbeat` field on all reader slots and the writer header
held by this process.

Ticks every ~1 s. Writes current monotonic clock
(`CLOCK_BOOTTIME` on Linux, `CLOCK_MONOTONIC` on other
platforms) to each active slot. The DB maintains an in-process
**active-slot list** — a `[]uint32` of slot indices protected
by `db.activeSlotsMu` (a `sync.Mutex`). `Begin()` appends under
the mutex; `Commit()`/`Rollback()` removes under the mutex; the
heartbeat goroutine takes a brief snapshot of the list under
the mutex each tick and issues the atomic stores outside the
lock to keep tick cost bounded.

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
store per active slot per second. No syscalls, no allocations.

## Atomic Operations Convention

- **In-process fields** (DB/Tx struct fields like
  `db.readerSlotHint`) use Go's **typed atomics**
  (`atomic.Uint64`, `atomic.Uint32`, `atomic.Int64`).
- **Shared-memory fields** (reader-table fields, header writer
  fields in the mmap'd lock file) use **function-based
  atomics** (`atomic.LoadUint64`, `atomic.StoreUint64`,
  `atomic.CompareAndSwapUint64`) on `unsafe.Pointer`-derived
  addresses. Typed atomics cannot be used here because the
  memory is a raw region in `MAP_SHARED` mmap visible across
  processes.

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
last-checkpoint TxnID.
