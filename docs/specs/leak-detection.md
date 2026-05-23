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
  property=A `Tx` cleanup observing `*db.closed == true` returns
    without touching the reader-table mmap or the flock goroutine
    — it logs and exits;
  from=this spec §Cleanup Behavior;
  violation=A cleanup that runs after `Close()` has begun unmapping
    SIGSEGVs the GC goroutine (touching unmapped memory), bringing
    the process down — defeating the safety net entirely.

Invariant: kind=clause-explicit;
  property=`db.closed` is a `*atomic.Bool` allocated on the heap and
    shared by pointer between the `DB` struct, every
    `txCleanupInfo`, and the `dbCleanupInfo` itself;
  from=this spec §Cleanup Behavior step 0;
  violation=An inline `closed` field on `DB` is captured by Tx
    cleanups as `&db.closed`; if `DB` is GC'd before a leaked Tx,
    the captured pointer dangles and the Tx cleanup reads garbage,
    bypassing the close guard.

Invariant: kind=clause-explicit;
  property=`Close()` sets `*db.closed = true` (release-store)
    *before* it begins unmapping or stopping the flock goroutine;
    the heartbeat and flock goroutines exit (with done-channel
    confirmation) *before* `Close()` unmaps the lock file or data
    file;
  from=this spec §`Close()` Ordering;
  violation=Unmapping before goroutines exit allows a final
    heartbeat tick to write to unmapped memory; releasing
    goroutines after `db.closed = true` is observable lets Tx
    cleanups race the unmap and SIGSEGV.

Invariant: kind=clause-explicit;
  property=A `Commit()` or `Rollback()` on a `Tx` (or `Close()` on
    a `DB`) cancels its `runtime.AddCleanup` callback before
    releasing the resource;
  from=this spec §Normal Close;
  violation=A cleanup that fires after a normal close re-releases
    the resource (e.g., re-clears a reader slot the next acquirer
    has taken over), introducing slot aliasing and snapshot
    corruption.

Invariant: kind=clause-explicit;
  property=Cleanup callbacks run on a GC background goroutine and
    perform only non-blocking operations: atomic check of
    `db.closed`, atomic store on reader slot, non-blocking channel
    send to flock goroutine, `sync.Mutex.Unlock` of a lock the
    leaked owner held (wait-free; not a contended acquisition),
    and non-blocking diagnostic logging via the configured `slog`
    handler. No mutex *acquisition* (no `Lock`/`RLock`/spin), no
    blocking syscall (other than the slog handler's bounded
    diagnostic write), no panic;
  from=this spec §Cleanup Behavior;
  violation=A blocking cleanup stalls all subsequent GC cleanups,
    backing up the whole process; a panicking cleanup aborts the
    program — the safety net becomes a single point of failure.
    `sync.Mutex.Unlock` and the standard slog handlers are
    explicitly admitted because (a) `Unlock` is wait-free and is
    the only way a leak cleanup can release the resource it owns,
    and (b) the §Cleanup Behavior contract itself requires writing
    a warning, so a strict no-syscall reading would be
    self-contradicting.

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

0. **Check `db.closed` first.** `db.closed` is a `*atomic.Bool`
   allocated on the heap and shared by pointer between the `DB`
   struct, every `txCleanupInfo`, and the `dbCleanupInfo` itself.
   The pointer is captured into each cleanup at
   `runtime.AddCleanup` time. Allocating the flag separately (not
   as an inline field of `DB`) is required because
   `runtime.AddCleanup` provides no ordering guarantee between a
   `DB` cleanup and the `Tx` cleanups that depend on observing the
   close state — if `DB` is collected first, an inline-on-`DB`
   flag would become a dangling pointer. With the shared-heap flag,
   the underlying `atomic.Bool` lives until the last referencing
   cleanup releases its capture. `Close()` sets
   `*db.closed = true` (release-store) *before* it begins
   unmapping. If a Tx cleanup observes `*db.closed == true`, it
   logs the warning and returns immediately — it does NOT touch
   the reader-table mmap (already unmapped or about to be) or
   signal the flock goroutine (already stopped).
1. **Log a warning** via the `*slog.Logger` on the `DB` struct
   (read txn / write txn, TxnID, Begin stack).
2. **Release the reader slot** by storing `TxnID = 0` (atomic) in
   the reader table.
3. **Release the write lock** (if writable): non-blocking signal
   to the flock goroutine (channel send with `default:` branch —
   if the channel is closed because `Close()` raced past the
   guard at step 0, the cleanup logs and returns).

Cleanup runs on a GC background goroutine — must not block or
panic. Permitted operations: atomic loads/stores, non-blocking
channel sends, `sync.Mutex.Unlock` of a lock the leaked owner
held (wait-free), and the configured `slog` handler's
diagnostic write (a bounded syscall, not a blocking operation).
Forbidden: mutex `Lock`/`RLock`/spin, blocking I/O, panic.

### Limitations

- **Non-deterministic timing.** GC-collection-dependent. A leaked
  transaction may hold its slot for an extended period.
- **Cross-process.** Cleanup only runs in the creating process.
  Other processes' leaks are reclaimed via PID-based stale
  detection (see `cross-process.md §Reader Table`).
- **Debug, not control flow.** Applications must not rely on
  cleanup. Safety net only.

## Database Handle Leak Detection

`runtime.AddCleanup` applied to the `DB` struct too. A leaked `DB`
holds open file descriptors, mmap regions, and the flock goroutine
— process-scoped resources outliving any individual transaction.

Same pattern: cleanup logs a warning with the Open stack trace,
stops the flock goroutine, munmaps data + lock files, closes file
descriptors. `Close()` cancels the cleanup.

## `Close()` Ordering

To make per-Tx leak cleanups safe against an early `Close()` (see
Cleanup Behavior step 0 above), `Close()` runs in this order:

1. Store `*db.closed = true` (release-store on the shared
   `*atomic.Bool`) — visible to any subsequent Tx cleanup
   invocation regardless of `runtime.AddCleanup` ordering between
   the DB and its Txs.
2. Stop the heartbeat goroutine via its stop channel and wait for
   its done channel (bounded by the tick interval).
3. Stop the flock goroutine: close `db.stopCh` (the goroutine's
   `select` honors it within at most
   `Options.LockRetryInterval`). Wait for the goroutine's done
   channel to signal exit; on exit it has released any held flock
   and cleared its writer header fields. **After** the done
   signal, `Close()`'s own goroutine closes `db.writerCh` and
   ranges over it, sending `ErrTxClosed` on each pending
   `writerRequest.result` channel — the flock goroutine is no
   longer reading from the channel at this point, so `Close()` is
   the sole drainer.
4. Stop the batch coordinator (if started): close `db.batchCh`,
   drain pending calls with `ErrTxClosed`, wait for exit.
5. Stop the maintenance goroutine (if running): stop channel +
   wait.
6. Munmap the data file and lock file.
7. Close all file descriptors.

Any Tx cleanup invoked between steps 1 and 6 sees
`db.closed = true` and exits without touching the soon-to-be-
unmapped memory. After step 6 the mmap is gone but the cleanup's
guard at step 0 prevents the SEGV. Any Tx cleanup invoked *after*
step 7 is fine — the guard still prevents access.

`Close()` is **not** safe to call concurrently with active write
or batch transactions in the same process. Active *read*
transactions hold reader slots that `Close()` will leave occupied;
they continue to operate against the now-unmapped lock file ⇒
undefined behavior. Callers must ensure all transactions in the
process are committed or rolled back before calling `Close()` —
see also `Compact()` for the related drain pattern in
`api-surface.md`.
