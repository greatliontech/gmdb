# LaggingReader bound-refresh uses prevMeta.TxnID instead of lastCheckpointTxnID — reclaims past the checkpoint under SyncLazy

**Lands:** audit-burndown-2026-07 chunk 8.

**Severity:** [H] — deterministic corruption after crash recovery
under SyncLazy; no races needed.

**Source:** 2026-07-04 full-codebase audit (concurrency auditor).

**Governing spec:** `docs/specs/free-space.md` §Why the bound is
sufficient (the bound MUST include the lastCheckpointTxnID term).

## Problem

`db.go:767-769`:
`return min(coord.OldestReaderTxnID(), prevMeta.TxnID)` — while the
Begin-time bound at `db.go:699-710` documents "the bound MUST use
lastCheckpointTxnID, NOT prevMeta.TxnID". The refresh is consumed at
`internal/pager/freespace.go:227, 666` and directly assigned to
`p.reclamationBound`, which `reclaimRPL` compares against.

Failure: SyncLazy DB; Checkpoint at C=10; lazy commits 11–20 retiring
pages referenced by checkpoint-10's tree; reader pinned at 15; tx 21
hits alloc pressure → LaggingReaderWait → refresh sets bound =
min(15, 20) = 15 > 10 → segments 11..14 reclaimed and reused → crash
→ recovery selects checkpoint meta 10 → tree references overwritten
pages. SyncDurable/DataOnly unaffected (checkpoint == prev), which is
why existing lagging-reader tests miss it.

## Fix direction

Include the lastCheckpointTxnID term in the refresh closure, exactly
as the Begin-time bound does. Regression: the sequence above +
`Check()` after reopen (fails on HEAD, passes with fix).
