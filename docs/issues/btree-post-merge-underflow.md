# Post-merge underflow flag forcibly cleared regardless of fill ratio

**Lands:** when an enforced invariant test for range-delete.md
invariant #3 ("no leaf or branch except the root has fill <
MergeThreshold") is added — likely chunk 5.8 hardening or whenever
a stricter post-merge invariant is encoded.

## Problem

`internal/btree/delete.go` `patchBranchAfterChildDelete` case-C and
`internal/btree/range_delete.go` `rebalanceSurvivors` both implement
the same pattern: after a `mergeOrRedistribute*` call, the merged
slot's `underflow` is unconditionally cleared and the next-level
branch-underflow propagation uses the encoded branch size as the
sole signal. When two siblings each at fill ~10% merge into a
single page at ~20%, the resulting page is still below
`MergeThreshold` (default 25%) — but the post-merge code reports
`underflow=false` and the propagation stops.

For DeleteRange (chunk 5.7) this means the rebuilt branch can hold
a leaf or branch with fill < MergeThreshold without triggering a
second-pass rebalance. Same shape exists in chunk-4
`mergeOrRedistribute*`-following code paths used by single-key
`Delete`.

## Mechanism (cited reachable path)

Build a leaf-pair siblings A, B each at ~10% fill (pad keys to
force this). Call DeleteRange that empties C_3 (an interior child)
and leaves A=newLeftID, B=newRightID intact-but-tiny.
`rebalanceSurvivors` picks (A, B) as the boundary pair, merges them
→ AB at ~20%. `survivors[leftJ].underflow = false`. The parent's
`branchUnderflow(newCells, mt)` check considers the encoded branch
size, which is healthy because it has multiple children (just AB +
others). The merged AB sits at 20% < 25% default threshold for the
rest of the tx; subsequent reads still work (correctness preserved)
but the page-fill invariant is silently violated.

## Class

`class=adjacent` per the chunk-5.7 diff arbiter — cause-line is in
chunk-4's `mergeOrRedistribute*` contract (the helper claims the
merged page is "healthy" without checking fill against the
threshold). Chunk 5.7's `rebalanceSurvivors` inherits the pattern;
not introduced by this change set. Surfaced as M-1 in the chunk-5.7
Round 1 adversarial review.

## Fix sketch

Two paths:

1. **Tighten the merge contract.** `mergeOrRedistributeLeaves` /
   `mergeOrRedistributeBranches` return the post-merge underflow
   state alongside the merged page id. Callers propagate it
   instead of assuming `false`. ~10 LOC.

2. **Relax invariant #3.** Accept that a merge of two below-MT
   siblings can leave the merged page below MT; the next mutation
   that touches it gets another chance to rebalance. Document the
   eventual-convergence semantic.

Per CLAUDE.md the user decides; defaults `(1)` if pressed.

## Notes

No demonstrated visible defect — the tree remains well-formed in
the strict sense (branch separators valid, no degenerate non-root
branches). The page-fill invariant is the only thing that drifts,
and the existing `Delete`/`DeleteRange` callers eventually
re-balance via further mutations.
