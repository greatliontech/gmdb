# Copy-on-Write Transaction Model

Lifecycle and semantics of read and write transactions, including
write batching, nested transactions, and the cursor state machine
that lets cursor-driven operations remain stable across mid-tx
CoW + rebalance.

Scope:
- Write transaction phases (begin → CoW → commit) at the
  bookkeeping level.
- Read transaction phases (slot acquire → snapshot read → release).
- Write batching via `DB.Batch()`.
- Nested transactions (`Tx.BeginChild`).
- Cursor state machine and post-delete state.

Out of scope (covered elsewhere):
- Slab buffer management and commit pwrite ordering — see
  `pager-slab.md`.
- Reader-slot mechanics (acquire/release, stale detection) — see
  `cross-process.md`.
- Leak detection via `runtime.AddCleanup` — see
  `leak-detection.md`.
- Lock ordering — see `lock-ordering.md`.
- `Batch()`, `Update()`, `View()`, `BeginChild` Go-level signatures
  — see `api-surface.md`.

## Invariants

Invariant: kind=clause-explicit;
  property=A read transaction's snapshot is identified by the
    `TxnID` recorded in its reader slot at `Begin`; every page
    reachable from that snapshot's meta is immutable for the
    duration of the read transaction (CoW ensures the writer never
    overwrites it in place);
  from=this spec §Read Transaction;
  violation=A reader observing in-place mutation reads a torn page
    mid-traversal — wrong keys, wrong child pointers, or a checksum
    mismatch.

Invariant: kind=clause-explicit;
  property=A read transaction holds its snapshot's TxnID until it
    releases the reader slot via `Commit()`, `Rollback()`, or
    `runtime.AddCleanup`. While the slot is held, RPL reclamation
    cannot advance past `min(TxnID across active readers,
    lastCheckpointTxnID)`;
  from=this spec §Read Transaction + `free-space.md §RPL Reclamation`;
  violation=Early slot release lets the writer reclaim pages a
    reader still references; late release blocks reclamation and
    inflates the file unboundedly (long-lived-snapshot
    pathology — see Lagging-Reader Contract below).

Invariant: kind=clause-explicit;
  property=A `Batch()` closure is invoked **exactly once** — there
    is no rollback-and-retry loop. Each closure runs in its own
    child transaction; closure errors roll the child back without
    affecting the parent or sibling closures;
  from=this spec §Write Batching;
  violation=Multiple invocations of a closure with non-idempotent
    side effects (logging, channel sends, gRPC calls) duplicate
    those side effects, breaking caller assumptions inherited from
    "the function ran".

Invariant: kind=clause-explicit;
  property=A child transaction's CoW always allocates a fresh page
    ID and a fresh slab buffer; it never mutates a parent's slab
    buffer in place. On child commit the child's entries merge into
    the parent's pager state; on child rollback the child's buffers
    are released and its page IDs returned to the allocator —
    without restoring any parent buffer content;
  from=this spec §Nested Transactions;
  violation=Mutating a parent buffer in place couples child rollback
    to buffer-content restoration; the design's "rollback discards
    bookkeeping, never restores prior buffer state" simplification
    breaks and child rollback can leave the parent's pager in an
    inconsistent CoW state.

Invariant: kind=entailed;
  property=A `Cursor.Delete()` followed by `Cursor.Next()` resumes
    iteration at the **post-delete successor** — the entry that
    followed the deleted entry — regardless of mid-iteration CoW or
    rebalance triggered by the delete;
  from=entailed: cursor state machine (this spec) + cursor stack
    tolerance (`page-formats.md` §Cursor Key Reconstruction);
  violation=Cursor desync after delete causes the delete-range loop
    pattern to either skip entries (silent retention) or revisit
    entries (re-delete attempts on already-deleted rows).

Invariant: kind=clause-explicit;
  property=A `Cursor` returned by `Cursor.Delete()` followed by
    `Current()` returns `(nil, nil)` with `Err() == nil` when the
    delete advanced past the end — distinguishing
    end-of-iteration from unpositioned (`Err() ==
    ErrCursorUnpositioned`);
  from=this spec §Cursor State Machine;
  violation=Confusing end-of-iteration with unpositioned breaks the
    standard "for k, v := c.Next(); k != nil; k, v = c.Next()"
    drain loop — callers either re-enter at `First` (re-deleting
    rows) or panic on Current.

## Write Transaction

1. Writer submits a request to the flock goroutine's writer queue
   and waits for the lock grant, respecting `ctx` cancellation (see
   `cross-process.md §Write Lock`). Returns `context.Cause(ctx)` if
   cancelled while waiting.
2. Writer reads the active meta page to get current roots, TxnID,
   and file-format fields.
3. For each modification:
   - Traverse the B+tree from root to leaf via `pager.Page(id)`.
   - On modify: the pager CoWs to a fresh page ID + fresh slab
     buffer (`pager-slab.md`).
   - Allocate new pages via `pageAlloc()` (`free-space.md`).
   - Old pages from previous transactions go to `tx.retiredPages`;
     pages CoW'd then freed in this transaction become loose pages.
   - For indexed keyspaces, the engine invokes the index extractor
     on old + new values and applies the index delta in the same
     write — see `indexing.md`.
4. Run Commit Write Ordering (`pager-slab.md §Commit Write
   Ordering`).
5. If OS file size exceeds `HighWaterMark` by more than
   `ShrinkThreshold`, `ftruncate()`. After the commit point — a
   crash before truncation leaves the file larger than necessary
   but consistent.
6. Release all slab buffers back to the pool; clear the pager's
   `p.dirty` map and the transaction's `tx.pendingAllocs`,
   `tx.pendingFrees`, `tx.cowPages`, `tx.loosePages`,
   `tx.retiredPages`.
7. Signal the flock goroutine to release the lock.

## Read Transaction

1. Reader checks `ctx` — returns `context.Cause(ctx)` if already
   cancelled.
2. Reader acquires a slot in the reader table via scan + CAS and
   records the current TxnID from the active meta page. If no slots
   are available and the context has a deadline, the reader retries
   with short backoff until a slot becomes free or the context
   expires. With no deadline, returns `ErrReadersFull` immediately.
   Use `context.WithTimeout` to control the wait window.
3. Reader traverses the B+tree using page pointers from that meta
   page via the read-only pager. Because of CoW, all pages
   referenced by this TxnID are immutable — the writer will never
   modify them in place.
4. When done, the reader clears its slot in the reader table.

Readers never need locks on the data file. They never block
writers. Writers never block readers. The only contention point is
reader-table slot acquisition (atomic CAS). The context governs the
retry window for slot acquisition but is not stored on the
transaction.

### Lagging-Reader Contract

A read transaction's snapshot pins every page in the snapshot
against RPL reclamation. Daemons that keep a single read transaction
open across many client operations cause unbounded file growth —
pages freed by intervening write transactions accumulate in the RPL
and cannot be reclaimed.

The correct pattern in a service (MCP server, request handler, RPC)
is a **short read transaction per request**, not per session. The
`LaggingReader` callback (see `lock-ordering.md §Lagging Reader
Handling`) exists as a last-resort signal, not a substitute for
short-lived snapshots.

## Write Batching

`DB.Batch()` amortizes write-transaction commit costs across
multiple concurrent in-process callers.

```
db.batchCh chan batchCall

type batchCall struct {
    fn     func(tx *Tx) error
    ctx    context.Context
    result chan<- error
}
```

1. `db.Batch(ctx, fn)` sends the closure, context, and a result
   channel to `db.batchCh`. The caller blocks on the result channel.
2. A coordinator goroutine reads from `db.batchCh`, collecting
   calls until either `Options.MaxBatchSize` calls have accumulated
   (default 1000) or `Options.MaxBatchDelay` has elapsed since the
   first call in the batch (default 10 ms). Lower delay → lower
   latency; higher → higher throughput. Set 0 to fire as soon as
   the coordinator runs.
3. The coordinator opens a write transaction via `db.Begin(ctx,
   true)` using `context.Background()` — caller contexts are
   checked separately.
4. Each collected closure runs in its own **child transaction** (see
   Nested Transactions). Before executing, the caller's `ctx` is
   checked — if cancelled, the closure is skipped and the caller
   receives `context.Cause(ctx)`.
5. If a closure returns an error, its child is **rolled back**. The
   parent transaction is unaffected; other closures' children
   remain intact. The failing caller receives the error.
6. If a closure succeeds, its child is **committed** (merged into
   the parent). The caller will receive `nil` when the parent
   commits.
7. After all closures have run, the parent commits. All callers
   whose closures succeeded receive `nil`. If commit fails, all
   callers in the batch receive the commit error.

Each closure is invoked **exactly once** — there is no rollback-
and-retry loop. This guarantee is about invocation count, NOT about
the atomicity of the closure's external side effects against the
database write. External side effects (logging, metrics, channel
sends, gRPC calls) run *inside* the closure and are unconditional;
the parent batch commit can still fail afterward (e.g., ENOSPC at
fdatasync), in which case the caller receives the commit error
while the side effect has already taken place. Closures whose side
effects must be atomic with the write should defer the side effect
until after `Batch()` returns nil:

```go
err := db.Batch(ctx, func(tx *Tx) error {
    // database write only — no external side effects here
    return ks.Put(key, value)
})
if err != nil { return err }
// safe to notify now: this caller's write is durable
notifyChan <- key
```

**Cross-closure side-effect ordering.** Closures within a single
batch run sequentially in implementation-defined order during the
parent transaction. In-closure side effects from closures A and B
fire in whatever order the coordinator dispatched them; the
deferred-notification pattern guarantees per-caller *durability*
but does NOT guarantee any ordering relative to *other* callers'
in-closure side effects. If a downstream observer must see caller
A's notification before caller B's notification, the callers must
coordinate that ordering themselves — `Batch` does not provide it.

**Cross-process group commit is not provided.** Each process has
its own batch coordinator. Cross-process write coalescing would
require shipping closures or redo records between processes —
complexity not warranted by the target workloads. Cross-process
writers serialize via the flock; each individual commit is short
enough (microseconds to low milliseconds in cheap-commit modes)
that queuing is not a bottleneck for the target N-daemon profiles.

### Coordinator lifecycle

Started lazily on the first `Batch()` call. Stopped when
`DB.Close()` is called: `db.batchCh` is closed, the coordinator
drains pending calls (returning `ErrTxClosed` to each), then exits.

## Nested Transactions

A write transaction can create child transactions independently
committed (merged into parent) or rolled back (discarded) without
affecting the parent. Children never write to disk; only the
top-level parent commits.

```go
child, err := tx.BeginChild()
if err != nil { ... }

if err := riskyOperation(child); err != nil {
    child.Rollback()  // undo child's work; parent unchanged
} else {
    child.Commit()    // merge into parent
}
```

**Child begin** — snapshot the parent's state:

- Snapshot `tx.pendingAllocs` length (or copy).
- Snapshot `tx.pendingFrees` length.
- Snapshot `tx.cowPages` (CoW'd page IDs).
- Snapshot `tx.loosePages`.
- Snapshot `tx.retiredPages` length.
- Snapshot keyspace root page IDs and counts.
- Snapshot the slab `dirty` map (which page IDs the parent has
  dirtied) — for rollback comparison, not for state restoration.
  The child does not get its own slab; it shares the parent's pager
  but never modifies a page already in the parent's `dirty` set in
  place.

**Child does work.** CoW always allocates a fresh page ID and a
fresh slab buffer. If the page being CoW'd is already in the
parent's `dirty` set, the child copies the parent's buffer into a
new buffer at a new page ID. The child never mutates a parent's
slab buffer in place.

**Child commit.** Discard the saved snapshots. The child's
modifications (slab buffers, pending sets, retired list, root
updates) remain in the parent's pager. No-op beyond freeing the
snapshot memory.

**Child rollback.**

- Release child's slab buffers (those added since child begin) back
  to the pool; remove from the parent pager's `dirty` map.
- Restore `pendingAllocs`, `pendingFrees`, `cowPages`,
  `loosePages`, `retiredPages` from snapshots.
- Restore keyspace roots to their pre-child state.
- The child's CoW'd page IDs are returned to the allocator (they
  were pending allocations and never reached disk).
- Done. No buffer-content restoration needed — every page the child
  touched lives at a fresh page ID, with a fresh slab buffer;
  dropping the buffer drops the modification.

**Nesting depth.** Children can create their own children
(arbitrary nesting). Each level snapshots its current state.
Rollback at any level restores to that level's snapshot. Cost is
proportional to pages modified at that level, not total database
size.

### Why this is cheap

A page CoW'd by a child lives in a fresh slab buffer at a fresh
page ID. Nothing the parent holds was overwritten. Rolling back
means releasing the buffer (back to a `sync.Pool`) and clearing
the page ID from bookkeeping sets — no buffer-content restoration,
no parent-state reconstruction. The slab analogue of the fresh-
mmap-position CoW model.

### Interaction with write batching

Each `Batch()` closure runs in a child transaction. If a closure
fails, its child is rolled back — sibling closures' children are
unaffected. Closures execute exactly once and do not need to be
idempotent.

## Cursor State Machine

Every cursor (`Cursor`, `SetCursor`, `TypedCursor`) is at any
moment in exactly one of three states:

| State | Meaning | Behavior |
|-------|---------|----------|
| Unpositioned | Cursor was created but never moved, or was Reset | `Current()` returns `(nil, nil)` and `Err()` returns `ErrCursorUnpositioned`. `Next`/`Prev` from this state behaves like `First`/`Last`. |
| Positioned | Cursor refers to an existing entry | `Current()` returns `(key, value)`. `Next`/`Prev`/`Seek*` move; `Delete()` removes the entry and transitions per the rules below. |
| End-of-iteration | Last `Next`/`Prev`/`Seek*`/`Delete()` advanced past the end | `Current()` returns `(nil, nil)` (no error). `Err()` returns nil (normal end). The next `Next`/`Prev` returns `(nil, nil)` again. `First`/`Last`/`Seek*` re-positions. |

Distinguishing end-of-iteration from unpositioned: end-of-iteration's
`Err()` is nil; unpositioned-state `Err()` is
`ErrCursorUnpositioned`.

### `Cursor.Delete()` post-delete state (single-value Keyspace cursor)

- Cursor must be Positioned. Otherwise returns
  `ErrCursorUnpositioned`.
- After successful delete, the cursor advances to the entry that
  followed the deleted entry. If no such entry exists, the cursor
  transitions to End-of-iteration (subsequent `Next` returns
  `(nil, nil)`, `Err()` is nil).
- The cursor stack tolerates CoW + rebalance triggered by the
  delete: `Next()` after `Delete()` is the supported pattern and
  always resumes correctly at the post-delete successor.
- Possible errors: `ErrReadOnly` (cursor on a read-only txn or
  read-only keyspace), `ErrCursorUnpositioned`, `ErrTxClosed`.

### `SetCursor.Delete()` post-delete state

- Cursor must be Positioned on a `(key, value)` pair.
- Deletes the current value from the current key's set.
- If the deleted value was not the last value for the key,
  advances to the next value for the same key.
- If the deleted value was the last value for the key, the key
  itself is removed (empty sets never exist) and the cursor
  advances to the first value of the next key. If there is no
  next key, transitions to End-of-iteration.
- The cursor stack tolerates CoW + rebalance triggered by the
  key-removal case — the same guarantee as `Cursor.Delete()` — so
  `Next()` after `Delete()` always resumes correctly at the
  post-delete successor, including across leaf splits and merges
  caused by the parent-keyspace key removal.
- Same error set as `Cursor.Delete()`.

### Cursor invalidation by `DeleteKeyspace`

Calling `tx.DeleteKeyspace(name)` invalidates every cursor and
Index handle previously opened on that keyspace within the same
transaction. Subsequent use of an invalidated cursor or Index
returns `ErrKeyspaceClosed`. The caller is responsible for not
retaining handles past a `DeleteKeyspace` call.
