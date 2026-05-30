# RPL reclamation bound uses prevMeta.TxnID, not lastCheckpointTxnID — DISPUTED (confirmed vs. refuted)

**Lands:** proactive — but **resolve the dispute first** via the named
regression test below; the test's verdict decides whether this is a High
corruption bug or a Low code-clarity fix.

**Severity:** [H] if the corruption is reachable / **[L]** if not —
**DISPUTED.** See both arguments below.

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`). One finder
chain **confirmed** this as a High bug (raw finding 4); a *separate*
finder chain raised the same claim and its verifier **refuted** it (raw
`refutedTitles[1]`) with a detailed double-buffering timeline argument.
The two chains reached opposite verdicts — this is filed to resolve, not
to assert.

**Governing spec:** `docs/specs/free-space.md:53-63` (clause-explicit
invariant: "No page is reclaimed before the reclamation bound:
`min(oldestActiveReaderTxnID, lastCheckpointTxnID)`") and `:303-307`
(algorithm); `docs/specs/durability.md §Recovery`.

## The agreed facts

`db.go:585` computes `bound := min(coord.OldestReaderTxnID(), prevMeta.TxnID)`.
No `lastCheckpointTxnID` field exists; it appears only in comments
(`db.go:574/579/583`, `read_tx.go:268`, `internal/lock/reader.go:160`,
`internal/pager/pager.go:365`). The spec literally requires
`min(oldestActiveReaderTxnID, lastCheckpointTxnID)`. Consumed at
`internal/pager/freespace.go:421` with a **strict** `seg.TxnID < bound`.
The `db.go:577-584` comment is stale/misleading (it claims `SyncMode` is
unwired and the `SyncLazy` split "arrives in 3.5", but `tx.go:330-340`
wires it). **Both chains agree the comment is wrong and a named field
would clarify.**

## The CONFIRMED argument (finding 4 — High, corruption)

SyncLazy DB: commit checkpoint C (`TxnID=C`, `MetaFlagCheckpoint` set).
Subsequent SyncLazy commits C+1..T retire old page versions into the RPL
(checkpoint flag clear). A later writer or background compaction
(`compaction.go:321` → `ReclaimFreeSpace`) computes
`bound = min(MaxUint64, T) = T`, and `reclaimRPL` frees all RPL pages with
`TxnID < T` — including pages retired at C+1..T-1 that **C's tree may
still reference**. Those pages get reallocated and overwritten
(un-fsynced). Crash before the next checkpoint → `durability.md §Recovery`
rolls the meta back to checkpoint C, but C's tree now points at pages
holding post-C garbage → silent data corruption / `ErrBadPageChecksum`.
Reachable via normal background compaction under SyncLazy.

## The REFUTING argument (refuted chain — not reachable)

The 2-slot meta is strictly double-buffered: only 2 slots,
`newActive := 1 - prevActive` (`commit.go:106`); `Checkpoint`
re-flags the active slot **in place** without advancing `TxnID`. Decisive
timeline: a surviving on-disk checkpoint meta-C is **overwritten at commit
C+2** (the 2nd commit after C lands on C's slot). So whenever meta-C is
still recoverable, the latest committed `TxnID <= C+1`, hence
`prevMeta.TxnID <= C+1`, hence `bound <= C+1`. RPL tags a page superseded
by commit T with `TxnID=T`, and such a page is referenced only by trees
with `TxnID <= T-1`. With `bound <= C+1` and **strict** `< bound`,
`reclaimRPL` frees only pages tagged `<= C`, referenced only by trees
`<= C-1` — **never tree-C**. To free a tag-(C+1) page tree-C references
you'd need `bound >= C+2`, but by then meta-C is already overwritten and
unrecoverable. So `bound = prevMeta.TxnID` yields identical correct
behavior to the spec's `min(reader, lastCheckpoint)` in every reachable
state; the divergence is code-clarity only (missing named field + the
stale `db.go:578` comment). Single exclusive writer makes
`prevMeta.TxnID == latest on-disk meta TxnID`; readers only lower the
bound. `TestRecoveryPrefersCheckpointMeta` is consistent.

## Resolution

1. **Write the decisive test** (finding 4's proposed regression):
   checkpoint at C, do post-checkpoint SyncLazy commits with churn that
   triggers `reclaimRPL`, simulate crash, re-Open at C, verify the tree is
   intact. Run with `MetaFlagCheckpoint` semantics exactly as production.
   - Test **stays green** → the refutation holds; downgrade to **[L]**:
     introduce a named `lastCheckpointTxnID` field (set on
     SyncDurable/SyncDataOnly commits and `Checkpoint`, unchanged on
     SyncLazy/SyncUnsafe) for clarity, and fix the stale `db.go:577-584`
     comment.
   - Test **goes red** → finding 4 holds; the field is a **correctness
     fix**, not clarity, and `bound` must use it.
2. Regardless of verdict, fix the stale `db.go:577-584` comment (both
   chains agree it is wrong).
