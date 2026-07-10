# Leaf rebalance: missing fill-floor decline breaks loop termination

Lands: 3

## Findings

**[H] Infinite loop: leaf redistribute has no fill-floor decline, and
both rebalance loops assume it does.**
`internal/btree/delete.go:1155` (`mergeOrRedistributeLeaves`) declines
only on `parentFits` (`delete.go:1255`), unlike the branch variant
(`delete.go:1455`) which requires both halves to clear MergeThreshold.
`rebalanceChildAtPos` resets `triedLeft/triedRight` after every
redistribute on the explicit assumption the decline guard exists
(`delete.go:853`, comment 836-838); `rebalanceSurvivors` rewinds on a
below-MT redistribute output with the same claimed termination argument
(`range_delete.go:826-837`). Failure (in-spec, default MT=25, 4 KB
pages): sibling leaves — one ~90%-full single inline value next to a
~26% leaf of small entries; `Delete` of a small key → underflow → merge
overflows → greedy split reproduces the identical skewed partition →
redistribute proceeds (no floor decline) → the loop never terminates,
allocating/freeing 3-4 pages per iteration (unbounded pending growth →
OOM). Same non-termination via `DeleteRange` through
`rebalanceSurvivors`. When the tracked side is the big half, the sub-MT
half is instead silently stranded below a reachable floor.

**[M] `patchBranchAfterChildDelete` drops an in-flight
`deepUnderflowChildIn` on case-C redistribute and decline outcomes.**
`internal/btree/delete.go:620` gates the cousin heal on
`isMerge && !leftIsLeaf`; the forced case-C pairing (`:696-699`)
resolving as a branch redistribute (`:582-601`) or a healthy-fill
decline (`:749-761`) leaves `deepUnderflowChildOut` zero — the sub-MT
descendant is never healed nor propagated while cousins exist. Silent
fill-floor invariant drift (range-delete.md §Invariants type (1));
requires a ≥3-level cascade to reach.

## Fix direction

Add the both-halves-clear-the-floor decline to the leaf redistribute
(mirroring the branch variant) — this is the contract the loop
termination arguments already cite; propagate/heal the deep-underflow
child on the non-merge outcomes. Spec-amend rider: range-delete.md
§Invariants states the decline contract for branch redistributes only;
extend to leaf pairs (surfaced in the audit spec-amend list).
Regression tests: size-skewed sibling-leaf fixture (must not hang —
run under a timeout), deep-cascade heal coverage.

## Provenance

2026-07-10 defect audit; btree reviewer. Existing decline tests are
branch-level/parentFits-only; no size-skewed leaf fixture exists (a
triggering test would hang on HEAD).
