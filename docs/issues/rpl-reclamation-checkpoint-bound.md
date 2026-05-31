# RPL reclamation bound uses prevMeta.TxnID, not lastCheckpointTxnID — CONFIRMED [H] (regression test)

**Lands:** proactive — **dispute RESOLVED (2026-05-31) by the decisive test:
CONFIRMED [H] corruption.** The fix is to bound reclamation on the last
checkpoint TxnID per the spec (see Resolution).

**Severity:** **[H]** — confirmed. The decisive regression test
(`TestRPLSyncLazyReopenCorruptionMinimal`, package `gmdb`, currently
`t.Skip`ped pending the fix) reproduces an UNOPENABLE database (Open fails:
`rebuild RPL chain: RPL segment at page N malformed`) from standard public
API under SyncLazy. The two arguments below are kept for the record; the
**refutation was incomplete** (see Resolution).

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

## Resolution (settled 2026-05-31) — CONFIRMED [H]

The decisive regression test went **RED** → finding 4 holds → **[H]
correctness bug**. `TestRPLSyncLazyReopenCorruptionMinimal` (package `gmdb`)
uses only the public API — SyncLazy `Put`/`Delete` churn + periodic
`Checkpoint` + `Close`/reopen, no compaction — and reproduces (18/20;
reliable under `-race`/`-count`) an **unopenable** database:
`Open` fails with `rebuild RPL chain: RPL segment at page N malformed`.

**Root cause (confirmed empirically):** the bound `min(oldestReaderTxnID,
prevMeta.TxnID)` reclaims RPL segments that a **still-recoverable meta's RPL
chain** references. On reopen, recovery selects that meta and walks its
`RPLHeadPage → … → RPLTailPage` chain into a reclaimed-and-reused segment
page → `ErrCorrupted`. Setting `bound = 0` (reclaim nothing) eliminates it;
`SyncDurable` (every commit a checkpoint, so `prevMeta.TxnID ==
lastCheckpointTxnID`) does not reproduce.

**Why the refutation was wrong:** it reasoned only about a recovered
*checkpoint's data tree* (overwritten at C+2 before its pages are freed). The
corruption is in the recovered meta's **RPL chain pointers**, not its data
tree, and the recovered meta can be a non-checkpoint step-3 fallback — neither
covered by the double-buffer argument.

**Fix — DEEPER than the spec's bound; `free-space.md`'s bound is itself
insufficient (proven 2026-05-31).** Implementing the spec's exact bound
`min(oldestReaderTxnID, lastCheckpointTxnID)` — tracked correctly (verified by
instrumentation: `bound` drops to the last checkpoint, e.g. `bound=8` while
`prevMeta.TxnID=10`) — STILL corrupts **30/30**. Refined root cause: a
recoverable checkpoint K's RPL chain references segments tagged **below** K
(its older tail, retired by commits `< K`). A post-checkpoint SyncLazy commit
reclaims with `bound = K`, freeing segments tagged `< K` — exactly K's tail —
while K is still on-disk and recovery-preferred, so reopening to K walks its
chain into a freed-and-reused segment page. So `lastCheckpointTxnID` does NOT
prevent it: **the spec's bound is defective.**

The correct invariant: reclamation must not free a segment **page** that any
on-disk (recoverable) meta's RPL chain still references. Candidate designs (a
decision to settle, surfaced to the user):
  - (a) bound by the OLDEST segment TxnID any recoverable meta's chain
    references (much lower than `lastCheckpointTxnID`);
  - (b) defer freeing a segment page until the meta that chains it is
    overwritten (track per-slot chain ownership);
  - (c) make recovery tolerant — `rebuildRPLChain` stops at the first
    un-decodable segment (the reclaimed-tail case) instead of erroring,
    treating `RPLTailPage` as advisory.
Amend `free-space.md §RPL Reclamation` to the chosen design. Verify by
un-skipping `TestRPLSyncLazyReopenCorruptionMinimal` (green under
`-race -count`). Also fix the stale `db.go` reclamation-bound comment.
