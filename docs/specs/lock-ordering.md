# Lock Ordering and Lagging Reader Handling

A global lock-acquisition order shared by every internal code path
in gmdb. Code that acquires multiple locks must do so in the order
below; the maintenance, batch, and flock goroutines all respect the
same order.

This spec also defines the **lagging-reader callback** because its
invocation rules interact directly with the lock-ordering
constraints on the writer's allocation path.

Depends on / interacts with:
- `cross-process.md` for the flock goroutine and reader-table CAS.
- `transactions.md` for batch and child-tx coordination.
- `free-space.md` for the allocator's lagging-reader hook.

## Invariants

Invariant: kind=clause-explicit;
  property=Every internal code path that acquires more than one of
    the locks listed below acquires them in the documented outer
    → inner order. A write transaction must not initiate an
    internal read snapshot (which acquires `activeSlotsMu`) while
    holding `pager.mu`;
  from=this spec §Lock Ordering;
  violation=Out-of-order acquisition introduces a cycle and
    deadlock — the exact failure mode this global order exists to
    prevent. Internal read snapshots taken by write-flow
    operations (e.g., the read-snapshot side of `Compact()`'s copy
    phase) must therefore be initiated *before* the writer's
    pager work begins, or *after* it completes.

Invariant: kind=clause-explicit;
  property=Cleanup callbacks (`runtime.AddCleanup`) run on GC
    background goroutines and acquire none of these locks; they
    only do non-blocking operations (atomic check of
    `db.closed`, atomic store on reader slot, non-blocking
    channel send to flock goroutine);
  from=this spec §Lock Ordering notes + `leak-detection.md`;
  violation=A cleanup that acquires a mutex can deadlock against
    a goroutine holding that mutex while waiting on GC — the
    safety net becomes a deadlock source.

Invariant: kind=clause-explicit;
  property=The `LaggingReader` callback is invoked at most once
    per `pageAlloc()` call;
  from=this spec §Lagging Reader Handling;
  violation=Repeated invocation inside one alloc call produces a
    busy loop (callback → retry alloc → callback → …) that
    pegs CPU when the reader cannot be drained.

## Lock Ordering

gmdb maintains several mutex/lock primitives. To prevent deadlock,
they are acquired in the following strict order. Code that
violates this order is a bug.

```
Outer  →  flock goroutine queue (db.writerCh)
       →  cross-process flock(LOCK_EX) on lock file fd
       →  intra-process write lock (held implicitly by write txn)
       →  per-keyspace open registry (db.keyspaceRegistry.mu)
       →  active-slot list (db.activeSlotsMu — for heartbeat coord)
       →  pager mutex (per write txn, db.pager.mu — for slab map updates)
       →  reader-table slot CAS (no mutex — atomic CAS only)
Inner  →  bitmap mutex (in-process, db.bitmap.mu — for two-level summary)
```

### Notes

- A read transaction only ever touches the reader-table CAS path
  and the active-slot list mutex (briefly, on
  Begin/Commit/Rollback). It does not enter any of the
  writer-side locks above.
- The flock goroutine never calls into the application;
  application goroutines never call into the flock goroutine
  except by sending on `db.writerCh`. This breaks any potential
  cycle through application code.
- The maintenance goroutine acquires the same locks as a writer
  when performing reclamation or compaction; it must respect this
  order.
- The heartbeat goroutine only acquires `activeSlotsMu` (briefly,
  to snapshot the slot list) and issues atomic stores to
  shared-memory reader-slot fields outside the mutex. It does not
  enter any writer-side lock.
- A write transaction must NOT open an internal read snapshot
  (which would acquire `activeSlotsMu`) while holding `pager.mu`,
  because that would invert the documented order. Internal read
  snapshots taken by write-flow operations (e.g., the
  read-snapshot side of `Compact()`'s copy phase) must be
  initiated *before* the writer's pager work begins, or after it
  completes.
- Cleanup callbacks (`runtime.AddCleanup`) run on GC background
  goroutines and only do non-blocking operations — they do not
  acquire any of the above locks.
- The snapshot/reader-table mmap and the data-file mmap are
  separate mappings; no lock is required to access either, only
  the atomic conventions of `cross-process.md §Atomic Operations
  Convention`.

## Lagging Reader Handling

A single long-lived reader prevents all RPL reclamation for
transactions newer than its snapshot, causing unbounded file
growth.

The application can register a `LaggingReader` callback via
`Options` that is invoked when a reader is blocking allocation.
Invoked from `pageAlloc()` when:

1. The bitmap has no suitable free pages.
2. The RPL has no more reclaimable entries.
3. A reader in the reader table is blocking reclamation.

The callback receives `LaggingReaderInfo` and returns an action:

- `LaggingReaderWait` causes `pageAlloc()` to refresh the reader
  table and retry.
- `LaggingReaderAbort` causes `pageAlloc()` to return
  `ErrDBFull`.

Invoked at most once per `pageAlloc()` call to avoid busy loops.
The application can log warnings, send alerts, or take
corrective action (e.g., killing a stuck process identified by
PID).

**The callback is a safety net, not a substitute for short read
transactions.** Services should structure read access as "one
read transaction per request/operation," not per session — see
`transactions.md §Read Transaction` and §Lagging-Reader Contract.

### `LaggingReaderInfo` / `LaggingReaderAction`

The Go-level shape lives in `api-surface.md §Types and Options`.
