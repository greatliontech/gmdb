# The RPL chain rebuild's head exemption is unsound for carried-forward heads — crash-mid-commit recovery can permanently fail to open

**Lands:** condition — with the recovery-model redesign
(`recovery-model-highest-epoch`), or when the RPL chain-walk
head-vs-non-head convention is next revisited.

**Severity:** [M] — a database that crashes at the wrong moment
becomes unopenable (hard error at Open), no data corruption. The
window is the ordinary crash-mid-commit shape in every sync mode: the
recovered meta is the LATEST durable one (its successor crashed
before publishing) with a carried-forward head that the crashed
successor's reclamation had already drained and reused.

**Source:** 2026-07-05 adversarial review of the
pager-rpl-footer-verification change set (chunk 4), finding H1
(adjacent — reachable on the pre-existing decode path; the chunk's
checksum path inherits the same convention).

**Governing spec:** `docs/specs/free-space.md` §RPL (recovery to a
non-latest meta) — the clause "the recovery target's own newest
segment is never reclaimed" is the unsound premise.

## Problem

`rebuildRPLChain` treats a head-segment failure (decode or checksum)
as a hard `ErrCorrupted`/`ErrBadPageChecksum` at Open, on the premise
that the recovered meta's head is that meta's own newest segment and
therefore never legitimately reclaimed. But `buildNewMeta` carries
`RPLHeadPage` forward across commits that retire nothing (`appendRPL`
is skipped when retiredPages is empty — empty commits,
format-flag-only commits), so a checkpoint meta's head can have
`TxnID < meta.TxnID` and sit BELOW a later reclamation bound.
Sequence: segment S (Txn 90) → no-retire commits → checkpoint A
(Txn 100, head=S) → later tx reclaims the whole chain including S and
reuses S's page → crash before the next meta publish → recovery
selects A → the head exemption forces reading S → Open permanently
fails (checksum error if torn, ErrCorrupted if cleanly rewritten as a
non-segment page). The same premise appears in the Check RPL walker's
comments (`check.go` walkRPL).

## Fix direction

The head can only be exempted when `head.TxnID == meta.TxnID` (the
meta's own commit appended it); a carried-forward head (older TxnID)
must get the same reclaimed-stale-tail treatment as non-heads —
requires persisting or deriving the head's TxnID trustworthily before
reading the page, or dropping the exemption entirely and accepting
that a genuinely-corrupt head truncates to an empty chain (bounded
leak, recoverable via Check/Repair) instead of failing Open. Interacts
with the `recovery-model-highest-epoch` redesign, which retires the
non-latest-meta recovery shape that makes this reachable.
