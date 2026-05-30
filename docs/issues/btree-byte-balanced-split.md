# B+tree split/redistribute partitions by entry count, not byte size

**Lands:** proactive — demonstrated correctness defect (reachable in-spec
`Put` returns spurious `ErrKeyTooLarge`; reachable in-spec `Delete`
returns spurious `ErrCorrupted`).

**Severity:** [H]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw findings
0, 1, 2, 3, 11, 19 — six finders converged on one root cause.

**Governing spec:** `docs/specs/page-formats.md:549-558` (Leaf Split —
mandates the 50%-of-data-bytes boundary via `FindSplitGroup` /
`FindSplitIndex`); `docs/specs/range-delete.md:83-112` (post-mutation
fill-floor invariant).

## Problem

Every B+tree split and merge→redistribute chooses its boundary by entry
**count** (`mid := len(entries)/2`), not by **byte size**. The
spec-mandated byte-balanced splitter (`FindSplitGroup` /
`FindSplitIndex`, biased to ~50% of data bytes such that each half is
guaranteed to fit) is **unimplemented**. Affected sites:

- `internal/btree/put.go:293` — leaf split (`Put` / `PutReportExisting` /
  `InsertIfAbsent`).
- `internal/btree/entry_ops.go:191` — `PutEntry` split.
- `internal/btree/delete.go:1104` — leaf merge→redistribute.
- `internal/btree/delete.go:1252` — branch merge→redistribute.
- `internal/btree/put.go:603` — branch split (`ascendWithSplit`).

### Reachable faults (demonstrated)

1. **Spurious `ErrKeyTooLarge` on a valid `Put`** (findings 0, 1).
   Reproduced end-to-end through the **public API** (`Open → Update →
   CreateKeyspace →` 60 small `Put`s + large `Put`s; fails at the 3rd
   large `Put`). A completely valid `Put` — small key (4 bytes), ~1300-byte
   inline value, far below the "no practical upper limit" of
   `limits.md §Maximum Value Size` — returns `btree: key too large for
   overflow-reference leaf entry`. The count midpoint lands so that one
   half cannot fit, yet a valid byte-balanced split point exists. The
   trigger (mostly-small values with an occasional large inline value —
   e.g. a directory/metadata keyspace) is ordinary, not adversarial.
   `limits.md §Invariants` pins `ErrKeyTooLarge` to *keys* exceeding the
   branch budget; returning it for a valid key is a contract violation.

2. **Spurious `ErrCorrupted` on a valid `Delete`** (finding 2). The
   delete-side count-split (`delete.go:1104`) returns the internal error
   mapped to `gmdb.ErrCorrupted`, making the engine declare a healthy
   database corrupt and abort (fail-stop / integrity-alarm handling).
   Reachable via `Delete` / `DeleteRange` boundary rebalance at any
   in-spec `MergeThreshold` (e.g. the max 50) on size-skewed leaves.

3. **Fill-floor invariant violated** (finding 11). Count-balanced
   redistribute can leave a non-root page far below `MergeThreshold`,
   silently breaking the `range-delete.md` fill-floor invariant that
   compaction pacing, leak-detection utilization heuristics, and split
   fairness all reason against. No data loss, but the structural
   guarantee does not hold, and the "subsequent mutations heal it"
   rationale becomes an unenforced load-bearing assumption.

4. **Branch path shares the flaw, latent** (finding 19). Branch
   split/redistribute (`put.go:603`, `delete.go:1252`) have the same
   count-balance defect; lower likelihood than leaves due to separator
   prefix-truncation, but the same structural bug under adversarial
   separator-size skew (long, low-prefix-sharing keys).

### Note — a probe for this exact bug was deleted without a fix

A developer probe (`internal/btree/zzprobe_test.go` /
`zzprobe2_test.go`) encoding this precise concern existed in the tree and
was **deleted without a fix landing** — the count-split lines are
unchanged. The audit re-derived and re-reproduced it from scratch through
the public API.

## Fix

Implement the spec's byte-balanced split point once and route **all five
sites** through it:

- `FindSplitIndex` (uncompressed) / `FindSplitGroup` (compressed):
  accumulate encoded entry bytes, choose the boundary closest to 50% of
  leaf capacity such that **each half is guaranteed to fit**; only return
  `ErrKeyTooLarge` when a single entry genuinely cannot fit even alone.
- Extend the same boundary selection to branch cells
  (`BranchEncodedSize`-aware).
- Stop classifying a count-split overflow on the delete path as
  `ErrCorrupted` — it is an algorithm defect, not input corruption.
- Remove/repair the `put.go:279-287` and `delete.go:1091-1100` comments
  that mis-justify count balance as sufficient, and the inaccurate
  inductive-maintenance claim in the `delete_test.go` comment.

## Verification

- Regression test: size-skewed inline entries (small entries interleaved
  with several near-half-page inline values) on **both** the Put-split
  and the Delete-redistribute paths.
- Fill-floor test strengthened with mixed key lengths and mixed inline
  value sizes (each redistribute half `>= MergeThreshold` by construction).
- Adversarial branch test: long, low-prefix-sharing keys, to pin
  branch-split reachability before deciding final branch-path scope.
- Confirm `BulkLoad`'s bottom-up builder uses the same size-aware
  boundary (it builds by fill, so likely already correct — cover it).
