# Sync-mode surface: drop the phantom SyncUnsafe (4→3), decide rename, note the larger recovery-model direction

**Lands:** proactive — queued for after the cross-process hardening chunk
(do not fold into it). Surfaced during a sync-mode discussion while fixing
the cross-process writer/reader stale-state corruption
(`cross-process.md §Writer acquisition flow`).

**Severity:** [M] (doc-vs-code drift: a documented mode contract the code does
not implement; plus a surface simplification)

**Governing spec:** `docs/specs/durability.md` §Durability Modes; `options.go`.

## Problem (concrete, near-term)

`SyncUnsafe` is a **phantom mode**: behaviourally identical to `SyncLazy` in
the current implementation. Both land in the same commit arm
(`tx.go:337` `case SyncLazy, SyncUnsafe:` → `pager.SyncNone`, checkpoint flag
cleared), and there is **no other `SyncUnsafe`-specific code path** (verified:
`grep -rn SyncUnsafe` shows only the option doc, the `AllowSyncUnsafe` validation
gate, the error sentinel, and doc comments — never a divergent branch).

So `SyncUnsafe`'s documented "skip both syncs, **no safety net**, risk of
**corruption** on crash" is unbacked by code: it skips the same fsyncs as
`SyncLazy` and clears the same `MetaFlagCheckpoint`, so recovery's
checkpoint-preferring selector falls back to the last checkpoint **identically**
— it is exactly as crash-consistent as `SyncLazy`, which `durability.md`
documents as *never corrupting*. The LMDB mode it is named after corrupts via
out-of-order writable-mmap flushes; gmdb's pwrite-based commit path never had
that exposure, so the mode never acquired teeth. Net: 4 modes that are really
3, plus an `AllowSyncUnsafe` opt-in gate (and a scary warning) guarding nothing.

## Fix (near-term)

1. **Drop `SyncUnsafe`** (and `AllowSyncUnsafe`, `errSyncUnsafeRequiresOptIn`,
   the `durability.md` §SyncUnsafe Warning). Clean break — pre-v1, per-process
   option, not persisted, no installed base. Leaves three modes:
   `SyncDurable` / `SyncDataOnly` / `SyncLazy`.
   - If a genuinely-faster-but-unsafe mode is wanted later, add it when it
     actually does something distinct (e.g. skipping the intra-commit
     data-before-meta ordering), not as a no-op alias.

2. **Rename decision (open).** The remaining 3 map onto sibling project
   `grove`'s 3 at the contract level:
   `SyncDurable`≡`SyncFull`, `SyncDataOnly`≡`SyncSkipMeta`,
   `SyncLazy`≈`SyncManual` (gmdb `Checkpoint()` ≈ grove `Sync()`).
   - *Keep gmdb names* (lean): they name the guarantee (Durable/DataOnly/Lazy)
     vs grove's mechanism (Full/SkipMeta/Manual); preserves gmdb's own
     identity; avoids dragging `Checkpoint()`→`Sync()`.
   - *Rename to match grove*: cross-project symmetry for the
     gmdb↔grove backport loop.
   Decide; do not silently converge.

## Larger adjacent direction (NOT this issue — noted so it isn't lost)

The discussion also surfaced that gmdb's per-commit `MetaFlagCheckpoint` +
**checkpoint-preferring recovery** is the root of three recurring complexities:
the live-handoff-vs-recovery meta-selection asymmetry (handled in
`Pager.Resync` — see the cross-process work), the `lastCheckpointTxnID`
reclamation-bound machinery, and the genesis-rollback gotcha. `grove` avoids
all three with a single **highest-valid-epoch** recovery rule (enabled by
deferring the meta *write* until durable) — but grove is single-process
(exclusive `flock` for the handle's life), so its in-memory
`pendingReclaim`/`pendingMeta` deferral does not port directly to gmdb's
multi-process model: a peer needs an **on-disk durability marker** to bound
reclaim safely (today that is the checkpoint flag).

A future gmdb redesign *could* replace checkpoint-preferring with
highest-epoch recovery + an explicit on-disk `durableEpoch` meta field
(removing the asymmetry and reshaping/retiring `lastCheckpointTxnID`). This is
a substantial Spec-first redesign of the commit/recovery/RPL path with real
multi-process design questions (cross-process visibility under deferred meta;
peer-visible durable-epoch). **It is explicitly a separate, larger effort** —
not part of dropping `SyncUnsafe`. Per the project workflow this is the kind of
thing to take when gmdb reaches a "good enough" state and the focus rotates to
grove for backporting (and looping back). gmdb keeps its own direction; grove
is the other end of the loop, not a convergence target.

## Verification

- Drop: `grep -rn SyncUnsafe` returns nothing in production code; the mode enum
  has three values; `durability.md` documents three modes; existing tests that
  used `SyncUnsafe`/`AllowSyncUnsafe` are migrated to `SyncLazy` or removed.
- Rename (if chosen): mechanical; update `options.go`, `durability.md`, all
  call sites + tests.
