# Shallow savepoint clones tx-cumulative maps at every Begin

**Lands:** profiling-driven — when an indexed-OLTP workload's per-Put
`BeginShallowSavepoint` cost becomes a measurable fraction of CPU /
allocation pressure, OR when `transactions.md §Nested Transactions`'s
cost clause is amended to distinguish "this-savepoint-window" from
"this-tx-cumulative-at-Begin", OR opportunistically.

## Problem

`Pager.BeginShallowSavepoint` (`internal/pager/savepoint.go`) clones
`pendingAllocs`, `pendingFrees`, `loosePages`, and builds a `dirtyKeys`
set in `captureSavepointState`. Each clone is `O(this-tx-cumulative-
state-at-Begin)` — the size of the running tx's mutation set at the
moment the savepoint is taken, not the size of the work this savepoint
will perform.

For per-row indexed maintenance (Keyspace.Put / Delete / Cursor.Delete;
SetKeyspace.Put / Delete / DeleteValue) the wrap fires once per row. A
transaction with N indexed mutations therefore pays:

```
sum_{k=1..N} O(state_at_Put_k) ≈ O(N²·depth)
```

map-entry clones total across the tx. For OLTP-scale N (≤ 100) the
constant is in the few-tens-of-thousands of map ops and not material;
for bulk-style N (10⁴+) it is.

The 0893be5 chunk closed the most egregious O(MaxSize) cost in
`Bitmap.Snapshot` (now an O(window-flips) undo-log marker). The
remaining fields (`pendingAllocs`, `pendingFrees`, `loosePages`,
`dirty`) are still clone-based. Per-row use of `BeginShallowSavepoint`
shifts the residual clone cost from "once per nested-tx" (the
historical use case) to "once per indexed Put" — same per-Begin cost,
N× more Begins per tx.

## `transactions.md` cost-clause interpretation

§Nested Transactions states: *"Cost is proportional to pages modified
since the outermost open savepoint, plus O(bitmap-pages currently
dirty) for the bitmap-dirty-set clone, not total database size."*

The clause speaks to "pages modified since the outermost open
savepoint" — which for a per-row shallow savepoint is the per-row
window (small). But the clone fields (pendingAllocs/Frees/loosePages)
are size-bounded by parent-tx mutations, not the per-row delta. The
clause is technically satisfied (`O(this-tx-state) ≤ O(this-tx-
mutations) ≤ ≪ O(MaxSize)`), but the "proportional to pages modified
at THAT level" intent is weaker for per-row shallow use than the
clause's wording suggests.

## Resolution candidates

1. **Extend the undo-log pattern.** Convert `pendingAllocs` / Frees /
   `loosePages` to per-op undo logs (mirror Bitmap.undoLog from
   0893be5). `BeginShallowSavepoint` records only a marker; mutations
   append undo entries; `RestoreSavepoint` replays. Per-Begin becomes
   O(1), per-Restore O(this-window-mutations). Cost stays bounded by
   per-savepoint work even for many savepoints per tx. Larger pager-
   internal change.
2. **Profile-driven trigger.** Add a counter to `Pager` that
   accumulates Shallow-savepoint Begin-clone work per tx; surface via
   `DBStats` or `BackgroundMaintenance` signal. Resolve only when the
   counter shows meaningful overhead in a real workload.
3. **Spec amendment.** Acknowledge the per-row shallow-savepoint
   relaxation in `transactions.md` (the cost clause's "this level"
   means "this savepoint level for nested kind; this-tx-cumulative for
   shallow kind"). Lower-priority polish; doesn't change behavior.

## Notes

Surfaced by the writenewindexregistry-partial-leak per-row resolution
session's Round 2 adversarial review (L-3). The user explicitly chose
"Build shallow-savepoint now" knowing the clone-cost trade-off; this
issue tracks the next-level resolution path (undo-log conversion).

The 6 per-row callers of `BeginShallowSavepoint` are leak-free and
correct under the current clone-based shape. This issue is a perf /
spec-fidelity follow-up, NOT a correctness defect.
