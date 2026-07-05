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
    → inner order;
  from=this spec §Lock Ordering;
  violation=Out-of-order acquisition introduces a cycle and
    deadlock — the exact failure mode this global order exists to
    prevent.

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
Outer  →  write grant (Coord.AcquireWriter: request queue to the
          flock goroutine → cross-process flock(LOCK_EX);
          serializes writers across and within processes)
       →  db.mu (handle state: currentMeta / pgr / coord swaps,
          poison + generation checks — held briefly, never across
          I/O or a grant acquisition)
       →  db.batch.mu (batch-coordinator queue state — held
          briefly; never acquired with db.mu held or vice versa
          today, ordered here for future composition)
       →  Coord.activeSlotsMu (in-process reader-slot registry —
          for heartbeat and drain coordination)
Inner  →  reader-table slot CAS (no mutex — atomic CAS/store per
          cross-process.md §Atomic Operations Convention)
```

Transaction-local state (the open-keyspace map, cursors, pinned
indexes, the pager slab) is single-goroutine by the transaction
contract and takes no lock at all; it is not an ordering
participant.

### Notes

- A read transaction only ever touches the reader-table CAS path,
  a brief db.mu acquisition (meta snapshot + generation check),
  and activeSlotsMu (slot registration) on
  Begin/Commit/Rollback. It does not take the write grant.
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
- The commit path acquires db.mu (to publish currentMeta) while
  HOLDING the write grant — grant-then-db.mu is the canonical
  outer→inner pair; acquiring the grant while holding db.mu is a
  violation.
- The pager slab map and the bitmap are NOT lock-ordering
  participants: they are mutated only by the sole writer (or the
  maintenance goroutine, both under the intra-process write grant +
  cross-process `flock(LOCK_EX)`), so the write grant serializes
  them without a dedicated mutex. Internal read snapshots taken by
  write-flow operations (e.g., the read-snapshot side of
  `Compact()`'s copy phase) acquire `activeSlotsMu` in the normal
  outer→inner order under the write grant.
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
