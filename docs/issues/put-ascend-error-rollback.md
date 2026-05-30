# Put store path does not roll back allocations when the branch ascend fails

**Lands:** proactive — pre-existing intra-tx page-reuse gap; negligible
(no durability/correctness impact), surfaced during the on-demand
promotion review.

**Severity:** [L]

**Source:** 2026-05-30 adversarial review of on-demand overflow promotion.
Classified `class=adjacent` — the base commit (`fc279ba`) has the
identical shape; on-demand promotion only *widens* the leaked set (it adds
the promoted overflow chains).

## Problem

In `internal/btree/put.go`'s insert store loop, the two terminal commit
paths call `ascendNoSplit` / `ascendWithSplit` to rewrite the branch
spine, and on error return the error **without** rolling back the pages
allocated for this Put:

- `put.go:290` (single-leaf commit): `nr, e := ascendNoSplit(...)`; on
  `e != nil` returns `0, false, e` without freeing `leftID`, the new
  value's overflow chain (`rollbackNewChain`), or any promoted chains.
- `put.go:379` (split commit): `nr, e := ascendWithSplit(...)`; on
  `e != nil` returns without freeing `leftID`, `rightID`, or the chains.

By that point the old leaf (`leafID`) and the displaced chain are already
freed, so the tree is mid-mutation. The "leaked" pages are loose pages for
**intra-transaction reuse** (per the `pager`/`AllocPage` contract), not a
durability leak: a write-tx `Rollback` discards the in-memory bitmap
wholesale, and an ascend failure (a CoW/alloc error deep in the spine)
aborts the tx anyway. So the only observable effect is missed intra-tx
page reuse on an essentially-unreachable failure path.

## Fix

Give the two ascend-error returns the same rollback the other failure
paths use (`_ = pw.FreePage(leftID)` [+ `rightID` on the split path] and
`rollbackNewChain()`), or factor a single deferred rollback for the whole
store loop. Address it uniformly (both ascend call sites), not just the
promotion delta. Add a test that injects an ascend error and asserts the
intra-tx loose-page set is restored.
