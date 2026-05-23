# Commit-publication-phase failure leaves in-memory meta cache stale

**Lands:** chunk 11 (Check + integrity recovery semantics).

## Problem

`internal/pager/commit.go` Commit runs steps 0–4. A failure at step 3
(meta pwrite) or step 4 (meta fdatasync) calls `AbortTx` which
restores the pager's in-memory bitmap / HighWaterMark / RPL chain to
the pre-tx snapshot. The on-disk active meta, however, may already
point at the *new* tree if step 3 partially completed before
returning the error. The root-package `db.currentMeta` is only
updated on a successful return from `pager.Commit`, so after a
publication-phase failure:

- On-disk active meta: NEW (TxnID = prev+1) — points at the just-
  committed tree.
- In-memory `db.currentMeta`: OLD — still says prev.

The next in-process `Begin` computes `newTxnID = OLD.TxnID + 1 =
prev+1`, then writes a different meta payload to the *other* slot
with that same TxnID. Two slots now carry equal non-zero TxnIDs with
different content — a direct violation of the entailed
strict-increase invariant in `file-layout.md §Invariants`.

A cross-process Open is unaffected (it re-reads both metas from
disk); the bug is purely in the single-process re-Begin path after
a step-3/4 failure.

## Acceptance

Either:

1. **Poison the DB handle on publication-phase failure.** A flag on
   `*DB` is set; subsequent `Begin` / `Update` return
   `ErrPoisoned` directing the caller to `Close` + `Open`. The
   re-Open does dual-meta selection from disk and picks the
   correct active.
2. **Re-read `db.currentMeta` from disk inside the error branch of
   `Tx.Commit`.** The next `Begin` sees the truth on disk and
   computes `newTxnID` correctly.

Option 2 is smaller and matches the integrity-check repair surface
chunk 11 will introduce.

Regression test: simulate a step-3 failure (inject a `WriteAt`
error via a wrapper `*os.File`); assert that re-reading the DB's
active meta matches what `Open` would have read; assert that the
next `Begin` writes its meta to the alternating slot with TxnID =
on-disk active + 1 (not OLD + 1).

When this issue closes, the load-bearing rationale moves inline into
the chunk-11 commit-failure recovery code; this file is deleted per
the no-cite invariant in `~/.claude/CLAUDE.md §Issue triage`.

## Notes

Classified as **newly-exposed** by the chunk-1 round-2 reviewer (not
a regression introduced by round-1 fixes — the bug pre-existed; the
deeper round-2 audit found it after the strict-increase invariant was
recorded in `file-layout.md`, which made the violation legible).
