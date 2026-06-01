# Durability Modes

Three sync modes (`SyncDurable`, `SyncDataOnly`, `SyncLazy`). The
mode controls which `fdatasync()` calls fire during commit and how
recovery selects the active meta page after a crash.

Scope:
- `Options.SyncMode` semantics.
- Checkpoint flag on the meta page and `DB.Checkpoint()` mechanics.
- Recovery rules (which meta is selected).
- Cross-process `SyncMode` interleaving.

Depends on / interacts with:
- `pager-slab.md` for commit step 2 / step 4 fdatasync placement.
- `file-layout.md` for the meta page `Flags` field (bit 1 =
  Checkpoint).
- `free-space.md` for the checkpoint-bound used by RPL
  reclamation in `SyncLazy`.
- `api-surface.md` for `SyncMode` constants and `Checkpoint`.

## Invariants

Invariant: kind=clause-explicit;
  property=`SyncDurable` issues `fdatasync` at both commit
    step 2 (data + RPL + bitmap) and commit step 4 (meta). After
    `SyncDurable`'s commit returns successfully, the commit is
    durable end-to-end;
  from=this spec §Durability Modes table + `pager-slab.md`;
  violation=A `SyncDurable` commit that returns success without
    durable meta-fsync violates the user-facing ACID contract —
    an ack'd transaction can be lost on crash, the worst-case
    surprise.

Invariant: kind=clause-explicit;
  property=Recovery prefers the highest-`TxnID` valid meta whose
    `Checkpoint` flag is set; non-checkpoint metas are never
    preferred over checkpoint ones regardless of `TxnID`;
  from=this spec §Recovery;
  violation=Selecting a non-checkpoint meta when a checkpoint
    meta is available rolls forward to a tree whose data pages
    may not be durable — readers can traverse into pages the
    OS never flushed, surfacing as `ErrBadPageChecksum` or
    wrong values.

Invariant: kind=entailed;
  property=`DB.Checkpoint()` makes prior `SyncLazy` commits
    durable: after Checkpoint returns success, every commit
    whose `TxnID <= active meta TxnID at Checkpoint time` is on
    stable storage, and the meta carries the checkpoint flag
    set;
  from=entailed: §Checkpoints mechanics (fdatasync at step 2 +
    flag-set + fdatasync at step 4);
  violation=A "successful" `Checkpoint` that fails to fdatasync
    prior pwrites lets recovery accept the checkpoint meta and
    traverse into not-yet-flushed pages — silent corruption.

Invariant: kind=entailed;
  property=Multiple processes attached to the same database may
    use different `SyncMode`s; recovery composes correctly
    because the on-disk checkpoint flag reflects the committer's
    mode, and recovery selects the highest-`TxnID`
    checkpoint-flagged meta regardless of which process wrote
    it;
  from=entailed: §Cross-process SyncMode interleaving;
  violation=An assumption that all processes share a `SyncMode`
    fails under mixed deployments (e.g., a `SyncLazy` build of
    one binary alongside a `SyncDurable` build of another) —
    correctness must hold across the composition.

## Durability Modes

Three modes, configurable via `Options.SyncMode`. The mode
controls which `fdatasync()` calls are performed during commit.
All modes preserve **database integrity** (the file is always
structurally valid).

| Mode | Data Sync | Meta Sync | On Crash | Performance |
|------|-----------|-----------|----------|-------------|
| `SyncDurable` (default) | `fdatasync()` | `fdatasync()` | No data loss. Full ACID. | Slowest |
| `SyncDataOnly` | `fdatasync()` | skip | Last committed transaction may be lost. DB is consistent — falls back to previous meta page. | ~2× faster |
| `SyncLazy` | skip | skip | Rolls back to the last **checkpoint**. DB is always consistent — no corruption. | Much faster |

## Checkpoints

In `SyncLazy` mode, commits pwrite bitmap, data, and meta but
skip all `fdatasync()` calls. The OS page cache holds the
writes; order is not guaranteed.

A **checkpoint** is a commit whose data pages have been
confirmed on stable storage. Checkpoints occur when:

- `DB.Checkpoint()` is called explicitly (`fdatasync` of the
  data file).
- A commit happens in `SyncDurable` or `SyncDataOnly` mode (these
  sync data pages as part of their normal commit path).

Each meta page carries a **checkpoint flag** (`Flags` bit 1, see
`file-layout.md`). Set when `fdatasync()` completes. In
`SyncLazy`, commits write meta with the flag **clear**.
`DB.Checkpoint()` re-writes meta with the flag **set**.

### `Checkpoint()` mechanics

1. Acquire the write lock via the flock goroutine — same path as
   `Begin(writable=true)`, respecting the supplied `ctx`. This
   serialises Checkpoint against any concurrent write transaction
   and any concurrent `Compact()` in the queue; concurrent reads
   are unaffected. Returns `context.Cause(ctx)` if cancelled
   before the lock is acquired.
2. `fdatasync(fd)` to flush all data, RPL, bitmap, and meta
   pages pwritten by prior `SyncLazy` commits that are sitting
   in the OS page cache. (The data mmap is `PROT_READ` and the
   writer never writes through it, so there are no mmap dirty
   pages from gmdb; the fdatasync's job is purely to flush
   pwritten page-cache contents.)
3. Read the currently active meta page; toggle its checkpoint
   flag on; recompute the xxhash64 checksum over the full meta
   payload (the flag change shifts the hash); `pwrite()` it
   back to the same slot. The TxnID is unchanged — Checkpoint
   records that the already-committed state is durable, not a
   new transaction.
4. `fdatasync(fd)` again so the flag set itself reaches stable
   storage.
5. Release the write lock.

Steps 2 and 4 are both required: step 2 makes prior lazy commits
durable; step 4 makes the flag-set durable so recovery can trust
it. The single-meta-slot pwrite in step 3 is atomic because it
stays within one page (an unaligned tear cannot affect a single
contiguous sub-page region, and the xxhash64 checksum catches
any partial write — recovery falls back to the other slot).

## Recovery

On recovery (Open after crash):

1. Read both meta pages. Discard any with invalid xxhash64
   checksum.
2. Of the valid metas, select the one with the highest `TxnID`
   whose checkpoint flag is **set**.
3. If neither meta has the checkpoint flag set (the user never
   called `Checkpoint()` and never used `SyncDurable` /
   `SyncDataOnly`), select the higher-`TxnID` valid meta. Data
   integrity depends on whether the OS flushed pages in the
   right order — not guaranteed. `Open()` logs a warning via
   `slog.Logger`.
4. Non-checkpoint metas are never preferred over checkpoint
   ones, regardless of `TxnID`.

Recovery does not attempt to validate a non-checkpoint meta's
tree. Accepting a partially-durable tree would risk surfacing
`ErrCorrupted` on later reads when traversals reach unflushed
pages. The checkpoint's tree is guaranteed intact because CoW
never modifies existing pages.

## Cross-process SyncMode interleaving

`SyncMode` is a per-process `Options` setting, not stored on
disk. Different processes attached to the same database may run
with different SyncModes. The on-disk checkpoint flag reflects
whichever mode the *committer* used: a commit by a `SyncDurable`
process sets the flag; a commit by a `SyncLazy` process clears
it. Recovery selects the highest-`TxnID` **checkpoint-flagged**
meta, so interleaving `SyncLazy` and `SyncDurable` writers
across processes works correctly — a crash rolls back to the
most recent `SyncDurable`-or-`Checkpoint`-set meta, possibly
losing intervening `SyncLazy` commits from any process. This is
the same trade-off as `SyncLazy` within a single process; the
multi-process composition is consistent with that.

**Live visibility vs. crash recovery — distinct selections.** The
checkpoint-preferring selection above governs *crash recovery* (a
fresh `Open`). It does **not** govern *live* operation: a writer
re-syncing on a grant handoff, and a reader beginning a transaction,
both select the **latest committed** meta (`cross-process.md §Writer
acquisition flow` / §Reader Table), so an uncheckpointed `SyncLazy`
commit IS visible to other live handles and is built upon by the next
writer — it is not silently rolled back during normal operation. The
asymmetry is intentional and consistent with the `SyncLazy` contract:
such a commit is *visible-while-live but not crash-durable*. The grant
serializes writers, so a live handoff is never a torn read; only a
real crash invokes the recovery rollback.
