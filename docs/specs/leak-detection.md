# Transaction and DB Handle Leak Detection

A transaction or `DB` handle garbage-collected without explicit
close is a resource leak: the reader slot, write lock, mmap, fds,
and flock goroutine outlive their nominal scope. gmdb uses
`runtime.AddCleanup` (Go 1.24+) to detect both classes of leak and
release their resources without crashing the GC goroutine.

Scope:
- Transaction leak detection: setup, cleanup behavior, race against
  `Close()`.
- DB handle leak detection.
- `Close()` ordering required to make per-Tx cleanups safe.
- Limitations (non-deterministic timing, cross-process leaks not
  covered, debug-only role).

Depends on / interacts with:
- `cross-process.md` for reader-slot release semantics and the
  flock-goroutine stop channel.
- `transactions.md` for `Commit()` / `Rollback()` cancelling the
  cleanup in the normal path.

## Invariants

Invariant: kind=clause-explicit;
  property=A WRITE-`Tx` cleanup observing the gate closed
    (`EnterCleanup` returning false) returns without touching the
    flock goroutine — it logs and
    exits. A READ-`Tx` slot release (normal close or leak cleanup)
    runs unconditionally, but touches the reader-table mmap only
    through the lifetime reference its `BeginRead` took on the
    lock-file mapping (§`Close()` Ordering step 8): the mapping
    cannot unmap before the release completes;
  from=this spec §Cleanup Behavior + §`Close()` Ordering step 8;
  violation=A reader-slot release without the reference races
    `Close()`'s munmap and SIGSEGVs the GC goroutine (or the
    caller), bringing the process down; a SKIPPED release strands
    the slot occupied under a live PID — unreapable by stale
    detection — pinning RPL reclamation for the process lifetime.

Invariant: kind=clause-explicit;
  property=`db.closeGate` is a heap-allocated composite gate
    (`internal/closegate.Gate`: the `closed` flag plus the
    `txInflight` drain counter) shared by pointer between the `DB`
    struct, every `txCleanupInfo`, and the `dbCleanupInfo` itself;
  from=this spec §Cleanup Behavior step 0;
  violation=An inline gate on `DB` is captured by Tx cleanups as
    `&db.closeGate`; if `DB` is GC'd before a leaked Tx, the
    captured pointer dangles and the Tx cleanup reads garbage,
    bypassing the close guard.

Invariant: kind=clause-explicit;
  property=`Close()` stores the gate's closed flag (release-store
    via `closeGate.CompareAndSwapClosed`) *before* it begins
    unmapping or stopping the flock goroutine;
    the heartbeat and flock goroutines exit (with done-channel
    confirmation) *before* `Close()` unmaps the lock file or data
    file;
  from=this spec §`Close()` Ordering;
  violation=Unmapping before goroutines exit allows a final
    heartbeat tick to write to unmapped memory; releasing
    goroutines after the closed store is observable lets Tx
    cleanups race the unmap and SIGSEGV.

Invariant: kind=clause-explicit;
  property=`Close()` drains the in-flight windows registered on the
    heap-shared atomic refcount (`gate.txInflight` — incremented at
    entry, decremented at exit regardless of the skip-vs-work
    branch) BEFORE unmapping the lock file. Two participant classes
    hold windows: leaked-Tx cleanups, and `BeginRead`'s acquire
    sequence (transactions.md §Read Transaction, Close-vs-BeginRead
    invariant). A cleanup that
    observed the gate open (and therefore proceeded into
    the resource-touching path) MUST complete before `Close`
    advances to unmap. The drain pairs with the gate's closed
    release-store to close the read-tx-leak race that the original
    closed-only gate left open: a leaked-ReadTx cleanup that
    passed the gate could otherwise race `Close`'s subsequent
    `lockFile.Close` and SIGSEGV writing to the unmapped reader
    slot;
  from=this spec §`Close()` Ordering + the read-tx leaked-handle
    slot-release path (the demonstrated fault — write-tx cleanup
    doesn't touch the lock-file mmap, so chunks 1–2 didn't
    surface the race);
  violation=Without the refcount drain, a leaked-ReadTx cleanup
    can SIGSEGV writing to `ReleaseReaderSlot`'s atomic stores
    against `f.slots` after `File.Close` has nilled the overlay
    slice. The closed-only gate is necessary but insufficient:
    closed-store + cleanup-load + cleanup-work + Close-unmap is
    a four-step race where the cleanup-work can land after the
    Close-unmap.

Invariant: kind=clause-explicit;
  property=A `Commit()` or `Rollback()` on a `Tx` cancels its
    `runtime.AddCleanup` callback (via `.Stop()`) BEFORE releasing
    the resource. A `Close()` on a `DB` achieves the same property
    via a different pattern: it stores the shared `closeGate`'s
    closed flag BEFORE the resource drain and `.Stop()`s the
    DB-level cleanup at the end; the cleanup callback itself
    consults the same shared gate and short-circuits when it
    observes it closed. Both patterns prevent a cleanup from re-
    releasing a resource the normal-close path is already
    releasing;
  from=this spec §Normal Close (Tx path) + §`Close()` Ordering
    (DB path);
  violation=A cleanup that fires after a normal close re-releases
    the resource (e.g., re-clears a reader slot the next acquirer
    has taken over), introducing slot aliasing and snapshot
    corruption.

Invariant: kind=clause-explicit;
  property=**Tx cleanup callbacks** run on a GC background
    goroutine and perform only BOUNDED, non-blocking operations:
    atomic gate check (`closeGate.EnterCleanup`), atomic store on
    reader slot, non-
    blocking channel send to flock goroutine, `sync.Mutex.Unlock`
    of a lock the leaked owner held (wait-free; not a contended
    acquisition), non-blocking diagnostic logging via the
    configured `slog` handler, and — on a leaked READ-Tx whose
    mapping reference is the LAST drop (reachable only when the DB
    handle was closed or collected first) — the final lock-file
    munmap + fd close, two bounded syscalls. No mutex *acquisition*
    (no `Lock`/`RLock`/spin), no other syscalls beyond the
    enumerated bounded ones, no unbounded blocking work, no panic.
    The DB cleanup callback is a separate concern (see next
    invariant);
  from=this spec §Cleanup Behavior;
  violation=An unbounded-blocking cleanup stalls all subsequent GC
    cleanups, backing up the whole process; a panicking cleanup
    aborts the program — the safety net becomes a single point of
    failure. Conversely, FORBIDDING the last-drop munmap would
    force the leaked-ReadTx cleanup to strand the mapping (and, if
    it also skipped the release, the slot) forever.

Invariant: kind=clause-explicit;
  property=The **DB cleanup callback** may perform a bounded
    blocking drain — specifically the same goroutine-stop +
    munmap + fd-close sequence as `Close()` — because exactly one
    DB cleanup fires per `*DB` over the process lifetime, so it
    cannot back up other cleanups in the way a per-`Tx` cleanup
    could. The drain duration is bounded by
    `Options.LockRetryInterval` (for the flock goroutine) +
    `Options.HeartbeatInterval` (for the heartbeat goroutine).
    Close + DB-cleanup racing is a caller-error pattern (a real
    `*DB` leak requires a still-reachable handle the GC can't
    reclaim mid-Close); the spec does not require synchronous
    completion between them — Close may return while a racing
    DB-cleanup is mid-drain;
  from=this spec §Database Handle Leak Detection;
  violation=A "no blocking" reading of the cleanup-callback rules
    would forbid the only safe way to release per-`*DB`
    resources (the heartbeat goroutine MUST be stopped before
    the lock-file mmap is released, else SIGSEGV). The Tx-cleanup
    non-blocking rule exists because Tx leaks are high-frequency;
    DB leaks are low-frequency and one-shot.

## Transaction Leak Detection

When `Begin()` creates a `Tx`, a cleanup is registered:

```go
tx := &Tx{...}
tx.cleanup = runtime.AddCleanup(tx, func(info txCleanupInfo) {
    // 1. Log warning with the stack trace captured at Begin() time.
    // 2. Release the reader slot (or signal flock goroutine to release write lock).
}, txCleanupInfo{
    slotIndex:  tx.readerSlot,
    writable:   tx.writable,
    beginStack: captureStack(),
    db:         tx.db,
})
```

`txCleanupInfo` is a separate struct — `AddCleanup` requires that
the cleanup function not reference the object being cleaned up (no
resurrection). The struct contains only what is needed to release
resources and log a diagnostic.

`captureStack()` calls `runtime.Callers()` at `Begin()` time to
record the call stack — included in the warning so the user can
identify where the leaked transaction was opened.

### Normal close

`Commit()` or `Rollback()` cancels the cleanup:

```go
func (tx *Tx) Commit() error {
    tx.cleanup.Stop()
    // ... normal commit logic ...
}
```

In the non-leak case, `AddCleanup` at `Begin()` + `Stop()` at close
are both cheap, allocation-free operations.

### Cleanup behavior

When the GC collects a leaked `Tx`:

0. **Check the close gate first.** `db.closeGate` is a
   heap-allocated `internal/closegate.Gate` — the `closed` flag
   plus the `txInflight` drain counter — shared by pointer between
   the `DB` struct, every `txCleanupInfo`, and the `dbCleanupInfo`
   itself. The pointer is captured into each cleanup at
   `runtime.AddCleanup` time. Allocating the gate separately (not
   as an inline field of `DB`) is required because
   `runtime.AddCleanup` provides no ordering guarantee between a
   `DB` cleanup and the `Tx` cleanups that depend on observing the
   close state — if `DB` is collected first, an inline-on-`DB`
   gate would become a dangling pointer. With the shared heap
   gate, it lives until the last referencing cleanup releases its
   capture. `Close()` stores closed=true (release-store via
   `CompareAndSwapClosed`) *before* it begins unmapping. A WRITE-Tx cleanup that observes closed ==
   true (via `EnterCleanup`) logs the warning and returns immediately — it does not
   signal the flock goroutine (already stopped). A READ-Tx
   cleanup releases its reader slot REGARDLESS of the close
   state: the leaked ReadTx's lifetime reference on the lock-file
   mapping (§`Close()` Ordering step 8) keeps the reader table
   mapped until the release completes, and skipping would strand
   the slot occupied under a live PID.
1. **Log a warning** via the `*slog.Logger` on the `DB` struct
   (read txn / write txn, TxnID, Begin stack).
2. **Release the reader slot** by storing `TxnID = 0` (atomic) in
   the reader table, then drop the mapping reference.
3. **Release the write lock** (if writable): non-blocking signal
   to the flock goroutine (channel send with `default:` branch —
   if the channel is closed because `Close()` raced past the
   guard at step 0, the cleanup logs and returns).

Cleanup runs on a GC background goroutine — must not block or
panic. Permitted operations: atomic loads/stores, non-blocking
channel sends, `sync.Mutex.Unlock` of a lock the leaked owner
held (wait-free), and the configured `slog` handler's
diagnostic write (a bounded syscall, not a blocking operation),
and — on a leaked READ-Tx whose reference is the LAST drop on the
lock-file mapping — the final munmap + fd close (two bounded
syscalls; reachable only when the DB handle was closed or
collected first). Forbidden: mutex `Lock`/`RLock`/spin, unbounded
blocking I/O, panic.

### Limitations

- **Non-deterministic timing.** GC-collection-dependent. A leaked
  transaction may hold its slot for an extended period.
- **Cross-process.** Cleanup only runs in the creating process.
  Other processes' leaks are reclaimed via PID-based stale
  detection (see `cross-process.md §Reader Table`).
- **Debug, not control flow.** Applications must not rely on
  cleanup. Safety net only.

## Database Handle Leak Detection

**Reachability contract.** The leak cleanup can fire only if a
dropped handle becomes GC-unreachable, so nothing long-lived may
hold the `*DB` strongly: the maintenance loop and the batch
coordinator hold it through a WEAK pointer, taking a strong
reference only for the duration of one pass/batch (a mid-pass handle
is reachable by definition, so the cleanup never races a pass; a
collected handle makes the daemon exit at its next tick — the batch
coordinator carries a liveness ticker for the abandoned-while-idle
case). Callbacks stored on the long-lived writer pager must not
capture `*db` either — the cleanup's own info holds the pager
strongly, so a captured handle would be reachable through
runtime → cleanup-info → pager → callback. Public API calls keep
the handle reachable for their FULL duration — where the
compiler's liveness analysis would end it before a blocking wait
(`Batch`'s post-acceptance result wait), an explicit
`runtime.KeepAlive` pins it, so a handle is never collected under
an in-flight call. USER-supplied callbacks retained by the engine
(`Options.LaggingReader`, `Options.Logger`) extend reachability to
whatever they capture: a handle captured there never becomes
leak-detectable — a documented caller responsibility. (Pinned by
`TestLeakedDBHandleCleanupFires`,
`TestLeakedDBHandleCleanupFiresAfterWriteTx`, and
`TestBatchInFlightPinsHandle`.)

`runtime.AddCleanup` applied to the `DB` struct too. A leaked `DB`
holds open file descriptors, mmap regions, and the flock goroutine
— process-scoped resources outliving any individual transaction.

The cleanup logs a warning with the Open stack trace, stops the
flock + heartbeat goroutines, munmaps data + lock files, and
closes file descriptors. The Close-vs-cleanup coordination uses
the shared-gate pattern (invariant 4): the cleanup calls
`SwapClosed(true)` on the shared `closeGate` — if the prior value
was true (i.e., `Close()` already stored it via
`CompareAndSwapClosed(false, true)`), the cleanup exits silently
with no drain. Otherwise the
cleanup wins the gate, logs, and drains the resources itself.
`Close()` additionally calls `db.cleanup.Stop()` at the end of
its drain, but the gate is the load-bearing safety against
double-drain — Stop is a courtesy that prevents the cleanup
from firing at all on the common path.

The Close-vs-cleanup race itself is unreachable under normal use:
for `Close()` to be invoked, `*DB` must be reachable from the
calling goroutine, but for the cleanup to fire, GC must have
determined `*DB` unreachable. The gate exists as a defense-in-
depth against `runtime.AddCleanup` ordering pathologies, not as
a contended fast path.

## `Close()` Ordering

To make per-Tx leak cleanups safe against an early `Close()` (see
Cleanup Behavior step 0 above), `Close()` runs in this order:

1. Win the close CAS (`closeGate.CompareAndSwapClosed(false,
   true)` — a release-store visible to any subsequent Tx cleanup
   regardless of `runtime.AddCleanup` ordering between the DB and
   its Txs). A second `Close()` returns immediately.
2. Stop the batch coordinator (if started): cancel its context
   (new submissions refused), wait for the coordinator goroutine
   to exit — its in-flight write transaction unwinds and releases
   the write grant first.
3. Stop the maintenance goroutine (if running): stop channel +
   wait, so no maintenance pass touches a torn-down mmap.
4. Drain the in-flight cleanup windows by spinning on
   `gate.txInflight == 0` (`closeGate.BeginClose`). A cleanup that
   has passed the release-store gate but not yet finished its
   resource-touching work (e.g., a leaked-ReadTx
   mid-`ReleaseReader`) must complete before any unmap, as must a
   `BeginRead` mid-acquire (its restabilization loop bails one
   iteration after Close begins; only the readers-full retry path
   can extend the drain, up to that caller's ctx deadline). The
   spin is bounded by those windows — microseconds in the common
   case.
5. Run the shutdown checkpoint (`durability.md` §Clean shutdown)
   while the pager and file are still alive — after the drain, no
   new Begin can start and every in-flight write completed, so the
   bump covers all acknowledged commits.
6. Capture and nil the resource pointers (coord, lock file, pager,
   data file, directory root) under `db.mu`, so a concurrent
   `Begin` sees a consistent pre-close or post-nil view.
7. `Coord.Close`: blocks until both the flock goroutine and the
   heartbeat goroutine have exited. The flock goroutine's stop
   path releases any held flock, clears the writer-header fields,
   and fails pending `AcquireWriter` requests — writer-queue
   drainage lives inside the Coord, not in `Close()` itself.
8. Drop the handle's lifetime reference on the lock-file mapping,
   then munmap the data file and close the data-file descriptors
   (the lock-file munmap and its fd close happen at the LAST
   mapping drop, which an open read transaction can defer). The
   lock-file mapping is REFERENCE-COUNTED: the handle holds one
   reference from Open, and every open read transaction holds one
   from `BeginRead`; the munmap happens at the LAST drop. A read
   transaction still open here therefore keeps the reader table
   mapped — its slot stays bound-pinning and releasable — until
   its own close (or its leak cleanup) releases the slot and drops
   the reference.

A WRITE-Tx cleanup invoked between steps 1 and 8 sees the gate
closed and skips the flock signal. A READ-Tx cleanup
releases its slot at ANY time — before, during, or after these
steps — protected not by the closed flag but by the leaked
transaction's own mapping reference (step 8): the lock-file mmap
cannot be gone while the release runs. BeginRead windows and
cleanups that entered the gate before step 1's store complete
fully before teardown, because step 4 spins on the in-flight
refcount until they decrement.

`Close()` is **not** safe to call concurrently with active write
or batch transactions in the same process; callers must complete
those first — see also `Compact()` for the related drain pattern
in `api-surface.md`. Active *read* transactions, by contrast, are
DEFINED across a concurrent `Close()`, via the reference-counted
lock-file mapping (step 8): `Close()` never unpins a live
snapshot. An open ReadTx's reader slot stays occupied — visible
to every peer's reclamation-bound scan — until the transaction's
own `Commit`/`Rollback` (or leak cleanup) releases it, so
operations already in flight and borrowed key/value slices remain
backed by protected pages for the transaction's whole lifetime.
NEW operations on the ReadTx after `Close()` begins fail with
`ErrClosed`; the release path itself works at any time (it goes
through the transaction's own mapping reference and its
BeginRead-captured Coord, not through the nil'd `*DB` pointers),
and each slot is freed exactly once (the `held` CAS). The cost of
the guarantee: a ReadTx held open indefinitely past `Close()`
keeps the lock-file mapping and its snapshot's pages pinned —
exactly as it would with the DB still open. Heartbeat residual:
after `Close()` stops the heartbeat goroutine, a cross-namespace
peer ages the slot out after its longer window
(`cross-process.md` §Stale-reader detection); same-namespace
peers classify by PID liveness and keep it pinned — unless the
occupant's start time is unreadable to the scanner (restricted
/proc), whose fallback is the same frozen heartbeat and the
SHORT window.
(Pinned by `TestCloseReleasesOpenReaderSlots`.)
