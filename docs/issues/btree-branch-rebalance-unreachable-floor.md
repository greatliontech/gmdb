# Branch delete-rebalance loops/double-frees when the fill-floor is unreachable

**Lands:** proactive — demonstrated correctness + liveness defect (reachable
in-spec `Delete` double-frees pages on HEAD; with byte-balanced branch
redistribute the same input fails to terminate / OOMs).

**Severity:** [H]

**Source:** 2026-05-31, discovered while implementing the branch half of
`btree-byte-balanced-split` (finding 19). An adversarial deep-tree delete
workload exposed it; it is **not** caused by the byte-balanced split fix
(it reproduces on HEAD `be63f47` with the prior count-midpoint
redistribute — see below), but that fix changes its *symptom*.

**Governing spec:** `docs/specs/range-delete.md` §Invariants (the fill-floor
clause and its "where reachable" / "eventual-convergence" qualifier) +
§Algorithm Phase 3 Rebalance (`rebalanceSurvivors`, `rebalanceChildAtPos`,
`cousinRebalanceBranch`).

## Problem

When a keyspace holds keys with **large separators** — long keys that share
deep common prefixes, so `ShortestSeparator` returns a long byte string,
up to the `limits.md §Maximum Key Size` bound (~`(PageSize-40)/2`) — branch
pages reach a near-fanout-2 shape: a branch holding a single ~1400-byte
separator (4 KB page) is only ~35% full, **below** a high `MergeThreshold`
(e.g. the maximum, 50). In this regime the fill-floor is **genuinely
unreachable**: merging two such branches plus the parent separator overflows
one page (3 × ~1400 > 4 KB), and a redistribute of N large cells cannot make
both halves ≥ 50% (each half holds 1–2 large cells = 17–35%). The spec
acknowledges this with the fill-floor's "where reachable" + soft
eventual-convergence qualifier.

The delete-side rebalance machinery does not treat the unreachable floor as a
**termination condition**. On a deep tree (depth ≥ 3) built from such keys, a
heavy delete pass drives the post-merge re-rebalance loop / cousin cascade
(`rebalanceChildAtPos`, `cousinRebalanceBranch`) to keep attempting to heal a
permanently-below-MT page:

- **On HEAD** (count-midpoint redistribute, `be63f47`): the loop **double-
  frees pages** — observed as repeated `fakeWriter.FreePage: double-free of
  page 353 / 139` under the test harness. In the production allocator a
  double-free corrupts the free-space bitmap (a page is returned to the free
  pool twice → later handed out twice → aliasing / data loss).
- **With byte-balanced branch redistribute** (`findBranchSplitIndex`): the
  loop instead **allocates without making progress and never terminates**,
  ballooning memory until the process is OOM-killed (~13 s on the repro). The
  redistribute halves themselves are valid (each fits a page — the splitter's
  post-hoc `BranchEncodedSize ≤ ContentEnd` checks never fire); the non-
  termination is in the surrounding rebalance loop, not the splitter.

Both symptoms are the same underlying defect: the rebalance loop assumes the
floor is always reachable and has no bounded-progress / give-up path for the
case the spec marks "where reachable."

### Reachable fault (demonstrated)

In-spec input: keys ≈ 1400 bytes (well under the ~2024-byte max-key bound)
sharing deep prefixes, large values, `MergeThreshold = 50` (a valid
`Options` value, `MaxMergeThreshold`), heavy deletion of a depth-≥3 tree.

```go
// internal/btree, fakeWriter harness. Reproduces on HEAD as a double-free;
// with the byte-balanced branch redistribute, as an OOM/non-termination.
cfg := page.Config{PageSize: 4096}
pw := newFakeWriter(t, 4096)
root := uint64(0)
var keys [][]byte
for c := range 6 { // 6 clusters × 12 keys, ~1400-byte shared prefix → depth-4 tree
    prefix := append([]byte{byte('A' + c)}, bytes.Repeat([]byte("p"), 1400)...)
    for j := range 12 {
        keys = append(keys, append(append([]byte(nil), prefix...), fmt.Appendf(nil, "%04d", j)...))
    }
}
val := bytes.Repeat([]byte("v"), 1300)
for _, k := range keys { root, _ = Put(pw, cfg, root, k, val) } // depth == 4
// Delete ~80% in shuffled order at the max threshold:
//   Delete(pw, cfg, root, MaxMergeThreshold, key)
// → HEAD: repeated FreePage double-free; byte-balanced: OOM-killed.
```

The leaf-level rebalance may share the flaw (the same loop services leaf
underflow); this issue was demonstrated at the branch level and the leaf
case is unconfirmed.

## Scope / relation to btree-byte-balanced-split

This is a **separate root cause** from the split-boundary choice. The
byte-balanced-split fix is about *which* boundary a single split/redistribute
picks (each-half-fits + fill-floor *where reachable*); it is correct and
verified in the floor-reachable regime
(`TestDeleteSizeSkewedBranchRedistributePreservesFillFloor`,
`DefaultMergeThreshold`). This issue is about the *rebalance loop's*
termination/page-management when the floor is **un**reachable — `delete.go`
`rebalanceChildAtPos` / `cousinRebalanceBranch` / the post-merge re-rebalance
loop, not `findBranchSplitIndex`. Fixing it must not be conflated with the
boundary choice.

## Fix (sketch — to be designed)

1. Root-cause which loop fails to terminate (instrument
   `rebalanceChildAtPos` / `cousinRebalanceBranch` progress detection on the
   repro; identify the cycle or the unbounded-alloc path).
2. Make "floor unreachable for this page" an explicit, bounded **give-up**
   outcome: accept a below-MT page (the spec's "where reachable" exception)
   rather than looping — and never re-free a page already freed in the
   cascade (the HEAD double-free).
3. Decide whether the fill-floor clause needs a spec amendment to state the
   unreachable-floor give-up precisely (it currently leans on the soft
   "eventual-convergence" wording).

## Verification

- Promote the repro above into a regression test (must terminate; no
  double-free; surviving keys intact) at `MergeThreshold = 50`.
- Re-enable the `MaxMergeThreshold` + large-separator variant of
  `TestDeleteSizeSkewedBranchRedistributePreservesFillFloor` (currently it
  uses `DefaultMergeThreshold` to stay in the floor-reachable regime).
- Cover the leaf-level rebalance under the same regime to confirm/deny the
  shared flaw.
