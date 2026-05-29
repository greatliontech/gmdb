# `rplSegments` clone in `captureSavepointState` is bounded but
unenforced

**Lands:** profiling-driven — when the in-memory RPL chain length is
shown to materially affect per-`BeginSavepoint` cost in a real
workload, or when `transactions.md`'s "chain length is bounded by
in-memory RPL state" claim is contradicted by a measured workload.

## Problem

`Pager.captureSavepointState` (`internal/pager/savepoint.go`) clones
the in-memory RPL chain via `slices.Clone(p.rplSegments)` at every
`Begin{Savepoint,ShallowSavepoint}` call. `RestoreSavepoint` reverts
via wholesale `p.rplSegments = sp.rplSegments`.

The chain length is bounded by current in-memory RPL state, not by
`MaxSize`. In healthy steady-state OLTP it is small (10–100 segments
typical). The transactions.md §Why this is cheap amend acknowledges
this:

> Per-tx-body mid-tx mutations to `rplSegments` (only `reclaimRPL`'s
> head trim) are not undo-logged; the savepoint clones the chain
> slice at capture instead. The chain length is bounded by in-memory
> RPL state — independent of `MaxSize` and of per-tx mutation count
> — so the clone cost is itself bounded by a small constant in
> practice.

The "in practice" is the gap. A pathological workload — high write
rate with a stuck reclamation bound (a lagging-reader pinning every
RPL segment indefinitely) — could grow the in-memory chain to
thousands of segments. Per `RPLSegmentRef` is ~24 bytes; 10,000
segments × N per-row `BeginShallowSavepoint` calls = 240 MB of
cumulative clone work per tx. The cost clause's "not total database
size" guarantee still holds (chain length is independent of `MaxSize`),
but the "bounded by small constant" claim has no enforcing test or
assertion.

## Acceptance

One of:

1. **Extend the savepoint-undo-log substrate to rplSegments.** Mid-tx
   `reclaimRPL` head-trim becomes a per-event log entry (the popped
   `RPLSegmentRef`); `RestoreSavepoint` prepends back. Begin becomes
   O(1) for rplSegments too. Mirrors the existing pendingAllocs/
   Frees/loosePages/dirty pattern.
2. **Add a runtime assertion or stats counter** that records the
   per-savepoint rplSegments clone size and surfaces it via
   `Pager.Stats` (or `BackgroundMaintenance`). The 0893be5
   bitmap-undo-log resolution adopted a similar profile-driven
   trigger for the bitmap.
3. **Spec amendment**: tighten the "bounded by small constant"
   claim into a concrete numerical bound (e.g., "RPL chain length
   exceeds N segments only when reclamation has been blocked for
   M segments' worth of tx commits; the chain naturally drains as
   the reclamation bound advances") and document the workload
   profile under which the bound holds.

## Notes

Filed at the close-out of `shallow-savepoint-clone-cost.md` by the
adversarial-loop reviewer (L-2). Not a correctness defect — the
clone is functionally correct; only the cost-clause enforcement is
incomplete. Resolution sequenced after a workload measurement shows
the chain grows past the small-constant assumption.
