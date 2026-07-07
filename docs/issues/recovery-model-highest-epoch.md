# Recovery model: replace checkpoint-preferring with highest-valid-epoch

**Lands:** chunks 8–9 of `docs/plans/architecture-consolidation.md`.
The Spec-first design this issue called for landed at chunk 7 (user
selected the **durable sub-record** model: every meta carries its
recovery target — `durability.md §Checkpoints and the durable
sub-record`, `file-layout.md §Meta Page`). The multi-process question
below is settled: grove's deferred-meta-write does not port because
peers need eagerly-pwritten metas for live visibility, so the
durability marker rides IN each meta instead of gating its write.
This file resolves when the implementation lands (chunks 8–9);
originally spun out of `sync-mode-surface-consolidation`.

**Severity:** design direction (not a defect) — a simplification that
retires three recurring complexity sources, gated on a multi-process
design.

**Governing spec:** `docs/specs/durability.md`, `file-layout.md`
(meta `Flags`), `free-space.md` §RPL Reclamation, `cross-process.md`.

## Problem

gmdb's per-commit `MetaFlagCheckpoint` + **checkpoint-preferring
recovery** is the root of three recurring complexities:

1. The live-handoff-vs-recovery meta-selection **asymmetry** (handled
   in `Pager.Resync` — see the cross-process work).
2. The `lastCheckpointTxnID` reclamation-bound machinery
   (`free-space.md` §RPL Reclamation).
3. The genesis-rollback gotcha.

Sibling project `grove` avoids all three with a single
**highest-valid-epoch** recovery rule, enabled by deferring the meta
*write* until durable.

## Why it doesn't port directly

`grove` is single-process (exclusive `flock` for the handle's life), so
its in-memory `pendingReclaim` / `pendingMeta` deferral does not port to
gmdb's multi-process model: a peer needs an **on-disk durability
marker** to bound reclaim safely. Today that marker is the checkpoint
flag.

## Direction (not a committed plan)

A future gmdb redesign *could* replace checkpoint-preferring recovery
with highest-epoch recovery + an explicit on-disk `durableEpoch` meta
field — removing the asymmetry and reshaping/retiring
`lastCheckpointTxnID`. This is a substantial Spec-first redesign with
real multi-process design questions:

- cross-process visibility under deferred meta;
- a peer-visible durable-epoch marker.

gmdb keeps its own direction; `grove` is the other end of the
backport loop, not a convergence target.

## Notes

Recorded verbatim-in-spirit from the 2026-05-30 sync-mode discussion so
the direction survives the closure of `sync-mode-surface-consolidation`.
No code change is in scope until this is taken as its own Spec-first
effort.
