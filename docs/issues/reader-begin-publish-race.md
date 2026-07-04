# Reader-begin TOCTOU: snapshot meta is read before the reader slot is published; reclamation can free the snapshot's pages in the gap

**Lands:** audit-burndown-2026-07 chunk 7.

**Severity:** [H] — reader traverses reclaimed-and-reused pages: wrong
values or ErrCorrupted. Found independently by two auditors.

**Source:** 2026-07-04 full-codebase audit (durability + concurrency
auditors, converging).

**Governing spec:** `docs/specs/cross-process.md` §Slot acquire —
which specifies the same read-then-CAS order and never closes the
window (its "snapshot currency" invariant covers only the stale-low,
conservative direction); `docs/specs/transactions.md` snapshot
immutability; `docs/specs/free-space.md` §Why the bound is sufficient
(its "oldestReader ≤ N" premise holds only once the slot is visible).
Spec-amend required alongside the fix.

## Problem

`read_tx.go:281` (`pager.ReadLatestMeta` → TxnID N) runs before
`read_tx.go:305` (`coord.AcquireReader(ctx, snapTxnID)`), with no
post-publish re-validation. A preemption between the two (GC pause,
scheduler, cross-process) lets a writer commit N+1 (retiring tree-N
pages into an RPL segment) and N+2, then Begin with bound =
min(oldestReader=∞, lastCheckpoint) and reclaim the segment — all
before the reader's CAS lands with TxnID N. All mid-publish
protections (HintEpoch, HB-first) start after the CAS; this window is
strictly before it. Under SyncDurable two commits are ms-scale.

## Fix direction

LMDB pattern: publish/reserve the slot first, then read the latest
meta; if TxnID advanced, restore the slot's TxnID to the new value and
re-read until stable; build the pager from the re-read meta. Amend
cross-process.md §Slot acquire to the reserve-then-read protocol.
Composes with chunk 6 (slot identity ordering). Regression: hook
between meta read and slot CAS; drive two commits + reclamation in the
gap; assert the reader either sees the new meta or its pages survive.
