# Branch pages lack within-page prefix compression → born-underfull trees on deep-shared-prefix keys

**Lands:** proactive — architectural quality defect (deep-shared-prefix +
large-value workloads build trees with most branches below `MergeThreshold`
at `Put` time; correctness holds, fill/fanout does not).

**Severity:** [M] (quality/architectural; the [H] *availability* facet —
the delete-rebalance non-termination it triggered — is already fixed, see
"Relationship to the termination fix" below).

**Source:** 2026-05-31, discovered (with a tree-dump instrument) while
fixing the branch delete-rebalance non-termination. The same root cause
underlies the byte-balanced-split branch work and the rebalance OOM.

**Governing spec:** `docs/specs/page-formats.md` §Prefix-Truncated Branch
Keys + §Branch Page; `docs/specs/overview.md` (the "maximizes fan-out"
claim); `docs/specs/range-delete.md` §Invariants (fill-floor "where
reachable" — already amended to record this regime).

## Problem

Branch pages store **full separators with NO within-page prefix
compression**. Per `page-formats.md` §Prefix-Truncated Branch Keys this is
deliberate: branches truncate *across* tree levels (a separator is the
shortest string distinguishing left from right subtree), and the format
*assumes separators are therefore short*. `EncodeBranch` copies each whole
key; `BranchEncodedSize` sums full key lengths (additive — confirmed by
`page.BranchCellCost`).

That assumption is **false for keys that share deep prefixes**. When
adjacent leaf-boundary keys share a long prefix, `ShortestSeparator` must
include the whole shared prefix to distinguish them, so the separator
approaches the `limits.md` §Maximum Key Size bound (~`(PageSize-40)/2`). A
4 KB branch then holds only **~2** such separators → **fanout 2, ~35%
fill** — below any `MergeThreshold` ≥ 36%.

### Evidence (tree dump, `Put`-only, before any delete)

72 keys in 6 clusters (each cluster shares a ~1400-byte prefix; values
1300 B so leaves hold ~2 entries), `MergeThreshold 50`:

```
BRANCH cells=2 (fanout=3) fill=2850 (70%)               ← only top branches healthy
  BRANCH cells=1 (fanout=2) fill=1433 (35%) <<BELOW-MT   ← ~70% of branches: one
    BRANCH cells=1 (fanout=2) fill=1433 (35%) <<BELOW-MT     1405-byte separator → 35%
      LEAF entries=2 fill=4038 (99%)                     ← leaves packed
```

~70% of branches are below `MergeThreshold` **at birth**. The
`range-delete.md` §Invariants claim that "a `Put`-only build maintains the
floor (splits balance ~50%)" is false here (now corrected in that spec with
the reachability qualifier).

## Fix (architectural — design Spec-first before code)

Add **within-branch prefix compression** so a branch stores the common
prefix of its separators once + per-cell suffixes (like leaf prefix
compression, but at the branch level). The separators that trigger this
*share the cluster prefix* (same-cluster leaf boundaries), so compression
collapses them, raising fanout to many-per-page → branches land ≥ 50% →
the fill-floor becomes reachable from `Put` and the unreachable-`Delete`
regime shrinks to only genuinely pathological all-distinct-deep-prefix
data.

This is a **branch-page format change** (pre-v1 → clean break is on the
table; no installed base). Spec-first on the new branch layout BEFORE code:

1. Design the compressed branch page format (header, shared-prefix region,
   per-cell suffix + child-ptr + directory). Decide search machinery
   (binary search must still work over suffixes against the shared prefix).
2. Amend `page-formats.md` §Branch Page + §Prefix-Truncated Branch Keys
   (today they assume short, uncompressed separators and claim
   "higher fan-out → shallower trees" unconditionally —
   `page-formats.md:222`, `overview.md:60`; both need the deep-prefix
   caveat + the compression mechanism).
3. Update `BranchEncodedSize` / `BranchCellCost` / `findBranchSplitIndex`
   (the byte-balanced branch splitter assumes additive per-cell cost — with
   compression the cost is no longer additive, mirroring why the leaf
   splitter measures fill through a real builder). This is the same
   non-additive-sizing consideration documented in `findBranchSplitIndex`.
4. Migration: pre-v1, no installed base — regenerate, no dual-read.

### Ruled out: prefix-aware leaf split-point selection

Choosing the leaf split boundary to minimize the resulting separator length
does **not** help this workload: within a cluster *all* keys share the full
prefix, so *every* boundary yields a ~prefix-length separator regardless of
where the leaf splits. Only within-branch compression addresses it.
(Investigated 2026-05-31.)

## Relationship to the termination fix (resolved)

The born-underfull tree made the branch delete-rebalance machinery
**non-terminate / OOM** (a redistribute lifting a large separator left both
halves below MT; the cousin cascade chased the relocating deficit forever).
That [H] availability defect is **fixed**: `mergeOrRedistributeBranches`
now **declines** a redistribute that cannot clear the floor for both halves;
the rebalance accepts the below-MT page and terminates (the
`range-delete.md` §Invariants "where reachable" + termination clause).
Regression: `internal/btree/rebalance_unreachable_floor_test.go`. This
compression work is the *root* fix that makes that unreachable regime rare;
it is **not** required for correctness/termination (those hold now), only
for fill/fanout quality.

## Verification (when built)

- The tree-dump evidence above, re-run after compression: branches at the
  leaf-adjacent level should be ≥ 50% (fanout many, not 2).
- `findBranchSplitIndex` + bulk-load branch builder updated for
  non-additive sizing; existing byte-balanced-split tests still green.
- Round-trip + balance + fill-floor at `MergeThreshold 50` on the
  deep-shared-prefix workload now *reachable* (the rebalance test can assert
  the floor where it previously could not).
