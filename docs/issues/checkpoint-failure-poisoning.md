# Checkpoint publication-phase failures do not poison the handle — torn active-slot write or failed fsync leaves a live handle that can lose or falsely certify committed data

**Lands:** audit-burndown-2026-07 chunk 9.

**Severity:** [H] — needs an I/O error then continued use; the commit
path already treats exactly this class as poison-worthy
(`tx.go:387-390`).

**Source:** 2026-07-04 full-codebase audit (durability auditor).

**Governing spec:** `docs/specs/durability.md` — currently silent on
Checkpoint error-path semantics (spec-amend required; user granted
blanket fix authority 2026-07-04).

## Problem

`checkpoint.go:113-146` — step-2 `file.Sync()`, step-3 `WriteAt` to
the **active** slot, step-4 `file.Sync()` all just return the error;
the handle stays live. Consequences:

1. *Torn active meta + split brain.* SyncLazy commit N acked;
   Checkpoint step-3 WriteAt partially fails → the only on-disk copy
   of meta N is torn; a peer's Resync
   (`internal/pager/init.go:447-477`) selects the other slot (N−1),
   builds on it, commits its own TxnID N — committed N silently lost;
   the first process's next Begin sees on-disk TxnID == its cached N
   (`init.go:470`), keeps its stale bitmap/RPL, commits N+1 over the
   peer's tree — page aliasing.
2. *Retry falsely certifies durability (fsyncgate).* Step-2 fsync
   returns EIO (kernel marks pages clean, error consumed); a retried
   Checkpoint's fsync succeeds trivially; MetaFlagCheckpoint is
   stamped on a meta whose data pages never reached disk — the exact
   violation durability.md itself describes.

## Fix direction

Poison the handle (`db.poisoned.Store(true)`, forcing Close+reopen) on
any step-2/3/4 failure, matching Commit's publication contract. Amend
durability.md to state Checkpoint failure semantics. Regression:
inject a step-3 short write / step-2 error (same technique as
`SetCommitStep4HookForTest`); assert subsequent Begin/Checkpoint
returns the poisoned error.
