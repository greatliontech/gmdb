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

Invariant: kind=clause-explicit;
  property=A write transaction with an unresolved child (or any
    descendant) is **frozen**: data ops, `Commit`, `Rollback`, and
    a second `BeginChild` all return `ErrChildActive` until the
    child resolves. Equivalently, the pager savepoint stack is empty
    when the top-level transaction commits;
  from=this spec §Nested Transactions (Parent freeze);
  violation=Committing the parent while a child holds an open
    savepoint publishes the child's *tentative* page allocations —
    pages the child might still roll back — so a meta swap could
    reference pages the bitmap will later mark free, corrupting the
    on-disk tree. (The strongest counterexample: every per-page CoW
    invariant still holds, yet the published tree references a page
    a not-yet-resolved child allocated and would have freed.)

Invariant: kind=entailed;
  property=A `Cursor.Delete()` positions the cursor ON the
    **post-delete successor** — the entry that followed the deleted
    entry — regardless of mid-iteration CoW or rebalance triggered
    by the delete. The canonical drain-loop pattern is
    `for k, _ := c.SeekGE(start); k != nil; k, _ = c.Current() {
    c.Delete() }` — `Current()` reads the post-delete successor
    in-place each iteration. `Next()` would advance PAST the
    successor and skip alternating entries;
  from=entailed: cursor state machine (this spec) + cursor stack
    tolerance (`page-formats.md` §Cursor Iteration);
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
   latency; higher → higher throughput. The zero `MaxBatchDelay`
   takes the 10 ms default; for minimal coalescing set
   `MaxBatchSize = 1` (each call runs in its own transaction).
3. The coordinator opens a write transaction via `db.Begin` on a
   **coordinator-lifetime context** (a cancellable context derived
   from `context.Background()`, cancelled only by `DB.Close`) — a
   single caller's context never aborts the shared batch transaction,
   yet `Close` can unblock a pending write-lock acquire. Caller
   contexts are checked separately (step 4).
4. Each collected closure runs in its own **child transaction** (see
   Nested Transactions). Before executing, the caller's `ctx` is
   checked — if cancelled, the closure is skipped and the caller
   receives `context.Cause(ctx)`.
5. If a closure returns an error or **panics**, its child is
   **rolled back** (a panic is recovered and surfaced to the caller
   as `ErrBatchClosurePanic` wrapping the panic value). The parent
   transaction is unaffected; other closures' children remain
   intact. The failing caller receives the error. (A closure that
   leaves a nested `BeginChild` unresolved is treated the same way —
   its child is force-resolved and the caller receives
   `ErrChildActive` — so one misbehaving closure cannot freeze the
   batch.)
6. If a closure succeeds, its child is **committed** (merged into
   the parent). The caller will receive `nil` when the parent
   commits.
7. After all closures have run, the parent commits. All callers
   whose closures succeeded receive `nil`. If the parent commit
   fails, every caller whose closure succeeded receives the commit
   error.

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
`DB.Close()` is called: Close cancels the coordinator-lifetime
context (rejecting new calls and unblocking any pending write-lock
acquire) and joins the goroutine before tearing down the pager and
lock coordinator. A call that was not yet accepted, or one rejected
because Close has begun, receives `ErrClosed`. (Cancel-and-join,
rather than closing `db.batchCh`, avoids a send-on-closed-channel
panic from a caller still blocked on the unbuffered send.)

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

**Parent freeze (LMDB model).** While a child — or any of its
descendants — is open and unresolved, the parent and every ancestor
are **frozen**: every operation on a frozen transaction (data ops,
`Commit`, `Rollback`, and a second `BeginChild`) returns
`ErrChildActive` until the child commits or rolls back. This
prevents the parent and child from racing on the shared
copy-on-write pager state, and guarantees the savepoint stack is
empty when the top-level transaction commits. A frozen cursor
surfaces `ErrChildActive` transiently (the freeze lifts when the
child resolves — it is not a terminal cursor error).

**Child begin** — capture a pager savepoint of the parent's
tx-scoped state so the child can be rolled back independently:

- Capture the allocation bitmap snapshot, `HighWaterMark`, and RPL
  chain.
- Capture `pendingAllocs`, `pendingFrees`, `loosePages`, the
  `retiredPages` length, and the slab `dirty` page-ID set (so
  rollback can release exactly the buffers the child added).
- Inherit the parent's keyspace state — root page IDs, counts, and
  the in-memory keyspace handles (the descriptor mutations the
  deferred-flush model keeps on the handles, not yet on disk) — by
  clone, so the child can mutate and roll them back without touching
  the parent's handles.
- The child does not get its own slab; it shares the parent's pager
  but never modifies a page already reachable from an ancestor's
  tree in place. The mechanism: while a savepoint is active the
  allocator **suspends loose-page reuse**, so a freed page an
  ancestor still references can never be handed back out and
  overwritten. Freed pages remain loose (not reusable) until every
  child resolves and the savepoint stack empties.

**Child does work.** CoW always allocates a fresh page ID and a
fresh slab buffer. If the page being CoW'd is already in the
parent's `dirty` set, the child copies the parent's buffer into a
new buffer at a new page ID. The child never mutates a parent's
slab buffer in place.

**Child commit.** Release (discard) the savepoint. The child's
modifications (slab buffers, pending sets, retired list, root
updates) remain in the parent's pager, to be published at the
top-level Commit. The child's keyspace descriptor state is merged
back into the parent by name: a parent handle for the same name is
updated in place (so a caller still holding it observes the
committed child work); a keyspace the child opened or created
installs a fresh parent-owned handle; a keyspace the child deleted
invalidates the parent's handle.

**Child rollback.** Restore the pager savepoint:

- Release child's slab buffers (those added since child begin) back
  to the pool; remove from the parent pager's `dirty` map.
- Restore the bitmap, `HighWaterMark`, RPL chain, `pendingAllocs`,
  `pendingFrees`, `loosePages`, and `retiredPages` to the captured
  state.
- The parent's keyspace handles and roots were never touched — the
  child's clones are simply dropped.
- The child's CoW'd page IDs are returned to the allocator (they
  were pending allocations and never reached disk).
- Done. No buffer-content restoration needed — every page the child
  touched lives at a fresh page ID, with a fresh slab buffer;
  dropping the buffer drops the modification.

**Handle lifetime.** Keyspace / SetKeyspace / Cursor handles opened
on a child are valid only for the child's lifetime — every child
handle returns `ErrTxClosed` once the child commits or rolls back.
After a child commits, a caller continues through a handle opened on
the parent (re-opening by name if the parent never had it open).

**Nesting depth.** Children can create their own children
(arbitrary nesting). Each level captures its own savepoint.
Rollback at any level restores to that level's savepoint. Cost is
proportional to pages modified since the outermost open savepoint,
plus O(bitmap-pages currently dirty) for the bitmap-dirty-set
clone, plus O(`rplSegments` chain length) for the chain clone
(workload-dependent at cross-tx granularity, see §Why this is
cheap), not total database size.

### Why this is cheap

A page CoW'd by a child lives in a fresh slab buffer at a fresh
page ID. Nothing the parent holds was overwritten. Rolling back
means releasing the buffer (back to a `sync.Pool`) and clearing
the page ID from bookkeeping sets — no buffer-content restoration,
no parent-state reconstruction. The slab analogue of the fresh-
mmap-position CoW model.

The allocation bitmap is the one piece of cross-level state whose
naive snapshot would scale with `MaxSize`. Its rollback substrate
is a three-method, strict-LIFO lifecycle: **Snapshot** opens a
marker at the current bitmap state; subsequent `Set`/`Clear`
operations append undo entries to a shared per-bitmap log while
at least one Snapshot is open; **Restore** replays the log in
reverse from the marker (reverting bit flips) and reinstalls
captured scalars (`hint`, `numFree`) plus the dirty-set clone;
**Discard** releases a marker without replaying (child commit /
top-level Commit success). Markers must be released in LIFO
order — the parent-freeze rule (§Nested Transactions) and the BeginTx/Commit
pairing already guarantee this; an out-of-order Restore or Discard
panics rather than silently corrupt state. Memory per open marker
is `O(bit flips since the marker)` + `O(bitmap-pages dirty at
marker capture)`, both bounded by mutation count and bitmap
geometry, not by `MaxSize`.

The pager's tx-scoped pending sets — `pendingAllocs`, `pendingFrees`,
`loosePages`, and additions to the slab's `dirty` map — use the same
substrate, distinct only in that their log is per-pager (not per-
bitmap). At `BeginSavepoint`/`BeginShallowSavepoint` the savepoint
records `undoLogPos = len(savepointUndoLog)` (an `int`), then every
state-changing mutation of those four fields while at least one
savepoint is open appends an `(field, key, wasPresent)` entry.
`RestoreSavepoint` replays `log[sp.undoLogPos:end]` in reverse —
for the three set-valued fields, setting `map[key] = wasPresent`
(delete to remove, struct{} to add); for `dirty` adds, deleting
`dirty[key]` and pool-`Put`'ing the buffer. `ReleaseSavepoint`
pops the savepoint and, when the active stack becomes empty,
truncates the log to length 0 (mirroring `bitmap.Discard`'s
no-open-Snapshot truncation). Per-Savepoint memory is therefore
`O(state-changing mutations observed during this savepoint's
window)`, never `O(cumulative tx state at Begin time)` — which is
what makes per-row `BeginShallowSavepoint` (one per
`Keyspace.Put`/`Delete`/`Cursor.Delete`, `SetKeyspace.Put`/
`Delete`/`DeleteValue` — indexed or not — plus one per un-indexed
`DeleteRange` walk) safe to invoke N times in a single tx:
total clone work across the tx is `O(N)`, not `O(N²)`.

The slab's `dirty` map specifically is tracked via append-only
log entries on `CoW`/`AllocSlab`/`AllocSlabRun` (the idempotent
shortcut on `dirty[id]` already-present skips both the install
and the log append). Loose-pop's `dirty[id]` detach is recorded
in a separate per-Savepoint `loosePopLog` (with the original
buffer pointer) because the (key, wasPresent) shape of the shared
log cannot carry the buffer reference; the loose-pop replay
branches on `wasPreWindow` (captured at loose-pop time by scanning
sp's window slice of `savepointUndoLog` for a prior
`(fieldDirty, id, false)` entry) to decide whether to re-attach
the original buffer (pre-window dirty) or pool-`Put` it (in-
window-installed buffer that was loose-popped within the same
window — re-attaching would leak it post-Restore).

The `loosePopLog`'s record of one (id, original-buffer) entry
per loose-pop event carries an implicit **single-owner contract**
on the detached `*[]byte`: `RestoreSavepoint`'s step-4 acts on
each entry as if it were uniquely owned (unconditional
`pool.Put(cur)` of the present `dirty[id]` followed by re-install
of `entry.buf`, with no refcount). The loose-pop branch
(`freespace.go`) appends the SAME `buf` pointer to every active
SHALLOW savepoint's `loosePopLog`, so the single-owner contract
holds only when at most one SHALLOW is active at the time of any
loose-pop event. To make the contract structural (illegal-state-
unrepresentable rather than caller-discipline), `Pager.
BeginShallowSavepoint` **panics if another SHALLOW savepoint is
already unresolved on the pager** — at most one SHALLOW is
active per pager at any moment. The per-op callers — every
keyspace-layer row mutation, indexed or not, plus the un-indexed
`DeleteRange` walk (the only legitimate callers) — each
open-and-resolve exactly one SHALLOW per call and do not nest,
so the rule is free in practice. SHALLOW-inside-NESTED and NESTED-inside-
SHALLOW remain allowed: NESTED suspends loose-pop via
`savepointDepth > 0`, so no loose-pop fires inside the nested
window and no alias can form regardless of which kind is at the
top of the stack.

Per-tx-body mid-tx mutations to `rplSegments` (only `reclaimRPL`'s
tail trim, which monotonically shrinks the chain — `appendRPL`'s
commit-time append runs after every savepoint has resolved, never
inside a window) are not undo-logged; the savepoint clones the chain
slice at capture instead. The clone is O(chain length), independent
of `MaxSize` and of per-tx mutation count, but **workload-dependent
at cross-tx granularity**: each commit that retires pages appends one
or more segments, and `reclaimRPL` can only drain tail segments whose
`TxnID < reclamationBound`, so a lagging reader pinning an old
`TxnID` blocks reclamation across writer commits and lets the chain
accumulate proportionally to retired-pages-pending-reclamation until
the reader releases. Under healthy OLTP the chain stays in the
10s–100s of segments; under stuck reclamation it grows until the
reader releases. The structural ceiling (chain entries ≤
`MaxSize`/`PageSize`, since each segment occupies one on-disk page
below `HighWaterMark`) is the only workload-independent bound; there
is no tighter constant-bound on the per-savepoint clone.

### Interaction with write batching

Each `Batch()` closure runs in a child transaction. If a closure
fails, its child is rolled back — sibling closures' children are
unaffected. Closures execute exactly once and do not need to be
idempotent.

## Write-helper error contract

A **write-helper** is an internal API on `*Tx` (or on a `*Keyspace` /
`*SetKeyspace` rooted in a `*Tx`) that mutates more than one page
inside a single user-visible operation — the DDL surface
(`CreateKeyspace` with indexes, `TxIndexes.Rebuild`, `TxIndexes.Drop`,
`tx.DeleteKeyspace`'s three-subtree retirement), the per-row
mutation path (`Keyspace.Put`/`Delete`, `Cursor.Delete`,
`SetKeyspace.Put`/`Delete`/`DeleteValue` — whose btree row op
retires prior-tx pages before its last fallible step even with no
indexes declared), the un-indexed `DeleteRange` walk, and any
future helper following the same shape.

**Rest-of-tx-continues contract.** A per-op error returned by such a
helper does NOT auto-rollback the tx — `Tx.Commit` is a separate
caller decision, and `Tx.Rollback` is recovery-of-last-resort. After
a helper returns an error the caller may legitimately `Commit` to
publish the work done before the failure (a use case the atomicity
comment in `writeNewIndexRegistry`'s body — chunk-7.3 — explicitly
opts into).

**Atomicity guarantee.** Every write-helper is **all-or-nothing in the
bitmap** for every in-spec return (named errors, including caller-
canceled context): at the end of a returning call — success or error
— the set of pages that the bitmap considers allocated, the set the
meta's trees reach, and the set the RPL holds for reclamation together
satisfy free-space.md's bitmap-consistency invariant (every reachable
page has its bit clear; every RPL page has its bit clear; every other
page below `HighWaterMark` has its bit set). A helper that returns an
error leaves the bitmap exactly as it was at entry, modulo whatever
work the helper successfully completed *before* the failure point
that the caller is then free to keep at `Commit` time — bookkeeping
state, never partially-freed or partially-allocated pages. A panic
from internal corruption (e.g. a guarded-walk assertion in btree)
is out-of-spec and unrecoverable; the panic propagates through the
defer (which sees `retErr == nil` and takes the `ReleaseSavepoint`
branch, merging the savepoint's pager state into the parent tx) —
only `Tx.Rollback()` (or process exit) reverts that merged state via
the whole-tx `AbortTx` snapshot.

**Scope.** This contract governs helpers whose implementation builds
on the pager's per-tx incremental allocator/RPL substrate (every site
enumerated above). `Keyspace.BulkLoad` and `SetKeyspace.BulkLoad` are
*outside* this contract: they bypass the slab via `pwriteAlloc` for
durability-after-meta-swap semantics and carry the distinct atomicity
model documented in `bulkload.md §Atomicity` ("bounded leakage
reclaimed by background maintenance"). A future helper whose
implementation also bypasses the slab is similarly out of scope and
must declare its own atomicity model.

**Implementation.** Internal write-helpers achieve all-or-nothing
through one of two pager substrates, chosen per the helper's calling
shape:

- **Nested savepoint** (`Pager.BeginSavepoint` / `RestoreSavepoint(on
  error)` / `ReleaseSavepoint(on success)`) for one-shot DDL helpers
  (`writeNewIndexRegistry`, `TxIndexes.Rebuild`, `TxIndexes.Drop`,
  `tx.DeleteKeyspace`'s retirement). Nested kind suspends loose-page
  reuse for the duration — acceptable here because each helper runs
  at most once per tx and the suspension window does not multiply.

- **Shallow savepoint** (`Pager.BeginShallowSavepoint`) for every
  per-row mutation — `Keyspace.Put`/`Delete`, `Cursor.Delete`,
  `SetKeyspace.Put`/`Delete`/`DeleteValue`, indexed or not (the
  row btree op itself retires prior-tx pages before its last
  fallible step; free-space.md's bitmap-consistency invariant
  needs the rollback regardless of index maintenance) — and for
  the un-indexed `DeleteRange` walk. Shallow preserves
  loose-pop across the savepoint window so an N-row indexed-Put
  workload stays bounded in file growth (the nested kind's
  loose-pop suspension would multiply to O(N·depth) and exhaust
  `MaxSize` for moderate batches). **At most one SHALLOW
  savepoint may be active on the pager at any moment**:
  `BeginShallowSavepoint` panics if another SHALLOW is already
  unresolved, because two simultaneously-active SHALLOWs would
  alias the same loose-popped `*[]byte` across their
  `loosePopLog`s and corrupt the pool/`dirty` invariant on
  Restore (see §Nested Transactions §Why this is cheap for the
  alias mechanism and the single-owner contract). The callers
  listed above each open-and-resolve exactly one SHALLOW per
  call and do not nest, so the rule is free in practice.
  SHALLOW-inside-NESTED and NESTED-inside-SHALLOW remain
  allowed. See §Nested Transactions for the substrate semantics.

A savepoint owns *pager* state (bitmap, `pendingAllocs`/`Frees`,
`loosePages`, `dirtyKeys`, `retiredPages`, slab buffers added during
the window). It does NOT own *caller* state — descriptor fields the
helper mutates (`KeyspaceDescriptor.IndexRegistryRoot`, in particular,
which `registryPut` / `registryDelete` advance in place), pinned-index
entries on the cached handle, or the handle's flush state. A helper
that mutates such caller fields **and has any error-returning step
following the mutation within the savepoint window** must capture
the pre-call value explicitly and restore it on the error path beside
the `RestoreSavepoint`. Caller-field mutations that strictly follow
the last error-returning step in the window — DeleteKeyspace's
in-memory invalidation block (cache eviction, dead-marking,
`pendingDeletes`), RebuildIndex's `propagateNotCachedDescChange` +
`syncRebuildToCachedPinned`, DropIndex's `delete(cachedKS.indexes,
…)` — need no restore: they cannot be reached on a `retErr != nil`
exit, so the defer's `RestoreSavepoint` branch cannot observe them
half-applied. Missing the restore where it IS required re-opens the
atomicity gap the savepoint was added to close: the bitmap rolls
back, the descriptor stays at the would-be-published value, and a
later op following the descriptor reads from pages the bitmap now
considers free → `ReachableButFree` / `ReachableInRPL` corruption on
the next `Check()`.

A future contributor adding a fallible step BELOW the existing
caller-field mutations must restructure the helper (explicit
`ReleaseSavepoint` before the new step + a fresh savepoint if more
atomic work follows) or extend the explicit capture-and-restore to
the field whose mutation now precedes a fallible step.

**Failure-injection seam.** Write-helpers expose a test-only
`atomic.Pointer[func(...) error]` hook (one per helper, in the
helper's source file) so regression tests can deterministically
exercise the partial-failure path without depending on fragile
`MaxTxBufferBytes` calibration. Each helper has a regression test
that asserts `db.Check()` reports no `BitmapLeak` /
`ReachableButFree` / `ReachableInRPL` / `FreeAndPending` after a
mid-helper error followed by `Tx.Commit`.

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
  delete; the canonical iteration pattern is:
  ```go
  for k, _ := c.SeekGE(start); k != nil && bytes.Compare(k, end) < 0; k, _ = c.Current() {
      c.Delete()
  }
  ```
  `Current()` reads the post-delete successor in-place — `Next()`
  would advance PAST the successor and skip alternating entries
  (since `Delete()` already advanced the cursor). (Chunk-7.10
  spec clarification.)
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
  key-removal case — the same guarantee as `Cursor.Delete()`. The
  canonical drain pattern uses `Current()` after `Delete()`, NOT
  `Next()`: `Delete()` already advanced the cursor to the
  post-delete successor (next-value-in-key or first-of-next-key),
  and `Next()` would advance PAST it (chunk-7.10 spec
  clarification).
- Same error set as `Cursor.Delete()`.

### Cursor invalidation by `DeleteKeyspace`

Calling `tx.DeleteKeyspace(name)` invalidates every cursor and
Index handle previously opened on that keyspace within the same
transaction. Subsequent use of an invalidated cursor or Index
returns `ErrKeyspaceClosed`. The caller is responsible for not
retaining handles past a `DeleteKeyspace` call.
