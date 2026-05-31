# B+tree split/redistribute partitions by entry count, not byte size

**Lands:** proactive — demonstrated correctness defect (reachable in-spec
`Put` returns spurious `ErrKeyTooLarge`; reachable in-spec `Delete`
returns spurious `ErrCorrupted`).

**Severity:** [H]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw findings
0, 1, 2, 3, 11, 19 — six finders converged on one root cause.

**Governing spec:** `docs/specs/page-formats.md` §Leaf Split (mandates
the ~50%-of-data-bytes boundary) + §Prefix-Truncated Branch Keys (the
`BranchEncodedSize` analog for branch cells); `docs/specs/range-delete.md`
§Invariants (post-mutation fill-floor). (Section refs, not line numbers —
the §Insert and Delete / §Leaf Split region shifted ~15 lines when the
splice-layer close-out promoted DECISION 1's format rationale into
§Compressed Leaf.)

## Resume (2026-05-31) — leaf paths FIXED; branch paths (finding 19) remain

**Current state (audited at this date):**

- **Leaf split + redistribute: byte-balanced — DONE.** All three leaf sites
  route through `findLeafSplitIndex` (`internal/btree/split.go:127`) — the
  entry-precise, fit-guaranteeing byte-balance (measures fill through a real
  `LeafBuilder`, promotes the largest inline value to overflow if no two-page
  partition fits): `put.go:383` (Put leaf split), `entry_ops.go:188` (PutEntry
  split), `delete.go:1149` (leaf merge→redistribute). Faults 1 (spurious
  `ErrKeyTooLarge`) and 2 (spurious `ErrCorrupted`) are fixed for leaves; the
  delete path uses the feasible byte-balanced boundary, not a count midpoint.
- **Branch split + redistribute: STILL count-balanced — finding 19, the open
  work.** `put.go:724` (`mid := len(newCells) / 2`, the branch split in the
  `ascendWithSplit` path) and `delete.go:1292` (`mid := len(combined) / 2`, the
  branch merge→redistribute) partition branch CELLS by count, not by
  `page.BranchEncodedSize`. `delete.go` already uses `BranchEncodedSize` for the
  fit-CHECKS around 1259-1306 — the splitter just doesn't use it for the boundary
  CHOICE. Latent under adversarial separator-size skew (long, low-prefix-sharing
  keys → large branch cells): a count midpoint can overflow a half (spurious
  error) or leave a half below `MergeThreshold` (fault 3, fill-floor violation).
- **NOT this issue — do not confuse:** the splice layer's `page.FindSplitGroup` /
  `page.FindUCSplitIndex` (`leaf_split.go`) are NO-DECODE leaf-split boundary
  pickers (fit guaranteed there by the subset property of an append-split); they
  are NOT the fit-guaranteeing byte-balance this issue is about (that is
  `findLeafSplitIndex`). Don't route branch splits through them.

**To resolve PROPERLY AND FULLY (full refactor OK):**

1. Implement a byte-balanced BRANCH split point — `findBranchSplitIndex`,
   `BranchEncodedSize`-aware, fit-guaranteeing (each half ≤ `ContentEnd`) AND
   fill-floor-respecting (each redistribute half ≥ `MergeThreshold` where
   reachable) — mirroring `findLeafSplitIndex`. If a clean shared abstraction
   emerges (parameterize the finder by an encoded-size measurer over a generic
   item slice), unify the leaf + branch finders — refactor if it cuts duplication
   without contorting either path; otherwise keep them parallel.
2. Route `put.go:724` (branch split) + `delete.go:1292` (branch redistribute)
   through it; promote the correct separator (the boundary cell's key).
3. Confirm `BulkLoad`'s bottom-up branch builder is size-aware (the Fix section
   below suspects it already is — cover it with a test, don't assume).
4. Tests (Root-cause: anchor each to a DEMONSTRATED fault): adversarial branch
   split AND redistribute with long, low-prefix-sharing keys (large separator
   cells) where the count midpoint overflows a half but a byte-balanced boundary
   fits; a branch fill-floor test (each redistribute half ≥ `MergeThreshold`).

**Read first (Spec-first + Root-cause):** this issue IN FULL; `page-formats.md`
§Leaf Split + §Prefix-Truncated Branch Keys; `range-delete.md` §Invariants
(fill-floor); `split.go` (`findLeafSplitIndex` — the leaf template to mirror);
`put.go` ~660-730 (`ascendWithSplit` / branch split, `splitBranch` if present);
`delete.go` ~1250-1310 (branch redistribute + the existing `BranchEncodedSize`
fit-checks); `branch.go` (`BranchEncodedSize`, `BranchCell`, the branch builder /
`ShortestSeparator`). State the `Diagnosis:` for each branch fault before the cut.

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
