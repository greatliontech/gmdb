# Overflow promotion threshold allows inline values too large to guarantee a two-page split

**Lands:** proactive — surfaced while fixing `btree-byte-balanced-split`;
a distinct root cause (promotion threshold, not split-point selection).

**Severity:** [M] (reachable rejection of valid data; narrow — requires
near-full-page inline values)

**Source:** 2026-05-30 — derived during the byte-balanced split fix
(`internal/btree/split.go`). Not one of the 27 audit findings; it is the
genuine residual the split fix exposes once the *spurious* count-split
failures are removed.

**Governing spec:** `docs/specs/limits.md §Maximum Value Size`;
`docs/specs/page-formats.md §Leaf Split`; `internal/btree/overflow.go:49-63`
(`needsOverflow`).

## Problem

`needsOverflow` (overflow.go:60) promotes a value to an overflow chain
only when its inline encoding *cannot fit an otherwise-empty page*
(strict-fit). So an inline value may be up to ~a full page. Two such
near-full inline entries cannot coexist in one leaf, and — crucially —
when a near-full inline entry is inserted **between** existing entries
that must stay adjacent to it, no contiguous two-page split can fit both
halves. The result is a reachable `ErrKeyTooLarge` on a *valid* `Put`,
even though every key is small and the value is within
`limits.md`'s "no practical upper limit."

This is **independent of** the split-point algorithm: the byte-balanced
splitter (`findLeafSplitIndex`, `btree-byte-balanced-split`) now returns
a feasible split whenever one exists and reports `ok=false` *only* in
this genuinely-unsplittable case — so the split fix makes the
genuine-vs-spurious boundary exact, and this is the genuine remainder.

`findLeafSplitIndex`'s `ok=false` path (and the unit test
`TestFindLeafSplitIndexNoFeasibleSplit`) pin where this bites: a leaf
whose entries have no contiguous two-partition that fits.

The threshold's own godoc already anticipates this: "profiling can
introduce a lower threshold later if dense-large-value workloads benefit
from earlier promotion."

## Fix

Lower the inline/overflow promotion threshold so any entry that survives
inline is at most ~50% of a page's usable bytes (e.g. promote to overflow
when `inlineSize > UsableSpace/2`). Then any two inline entries co-fit a
page and every leaf admits a byte-balanced two-page split — the
unsplittable case disappears. This is format-affecting (density tables in
`page-formats.md §Leaf Density`, the `NeedsOverflow`/`OverflowRefFitsLeaf`
BulkLoad parity in overflow.go, and on-disk inline-vs-overflow choices for
existing data) and must be evaluated against `limits.md` and the
clean-break policy — hence a tracked follow-up, not folded into the split
fix.

**Alternative:** keep the strict-fit threshold and document the
near-full-inline limitation explicitly in `limits.md` (a single value
within ~one entry of a full page may be unstorable alongside neighbors).
Lower-cost but leaves valid data reachably rejected.

## Verification

Add an integration test: into a keyspace region of small keys, `Put` a
value sized just below the strict-fit threshold whose key sorts between
existing entries, and assert it succeeds (after the threshold fix) — the
red case is the current `ErrKeyTooLarge`.
