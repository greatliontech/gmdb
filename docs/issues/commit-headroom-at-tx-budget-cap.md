# A transaction driven to the MaxTxBufferBytes cap cannot commit — Commit's own allocations trip ErrTxTooLarge

**Lands:** condition — when the slab-budget accounting or the commit
pipeline's allocation model is next revisited (needs a design
decision: reserve commit headroom in the per-op budget check, or
exempt commit-pipeline allocations from the cap).

**Severity:** [M] — availability/contract, no corruption: a caller
whose ops fail with ErrTxTooLarge (the engine's rest-of-tx-continues
contract invites committing the partial work) can find Tx.Commit
itself failing with ErrTxTooLarge; the successfully-applied work in
that tx is then only recoverable by Rollback, i.e. lost.

**Source:** discovered 2026-07-05 while building the
audit-burndown-2026-07 chunk-3 regression fixture (Commit at the cap
failed deterministically; the tests now keep an explicit
commit-headroom reserve to work around it).

**Governing spec:** `docs/specs/pager-slab.md` (budget clause; note
its step-0 accounting drift is already tracked in
api-and-doc-drift-sweep item 4); `docs/specs/transactions.md`
(rest-of-tx-continues).

## Problem

Commit-pipeline allocations (RPL segment pages via AllocPage/
AllocSlab) count against the same MaxTxBufferBytes budget the ops
consumed. A tx that hit the cap mid-op keeps dirtyBytes at ~cap after
the per-op rollback, so commitStep0/1's own allocations exceed the
budget and Commit returns ErrTxTooLarge. Reproduce: fill a tx until
Put returns ErrTxTooLarge, then Commit.

## Fix direction

Either (a) per-op admission reserves a small fixed commit headroom
(ops fail once dirtyBytes > cap − reserve, sized to the commit
pipeline's worst-case segment count), or (b) commit-pipeline
allocations are exempted from the cap (bounded: O(retired/segment
capacity) pages). (a) keeps the cap a true memory bound; size the
reserve from the RPL segment fan-out math. Regression: fill to
ErrTxTooLarge, Commit must succeed.
