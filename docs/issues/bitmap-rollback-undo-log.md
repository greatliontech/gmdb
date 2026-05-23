# `Bitmap.Snapshot` allocates O(detail-size) per write tx

**Lands:** when profiling shows `BeginTx` allocation pressure is
material on a real workload.

## Problem

`internal/bitmap/bitmap.go Snapshot()` clones the entire detail
`[]byte` + summary `[]uint64` + dirty map at every `Pager.BeginTx()`.
For `MaxSize = 256 GB / PageSize = 4 KB`: 8 MB detail + 128 KB
summary cloned per write transaction. Per-tx allocation cost is
linear in MaxSize, not in tx mutation count.

Chunk-1 round-2 review classified this as **L (newly-exposed,
performance)** — correct on the rollback path but heavy on the
common-path `BeginTx`.

## Options

1. **Undo log.** Replace the full clone with a `[]bitmapMutation`
   captured by `Set`/`Clear`/`SetHint`/dirty mutations during the
   tx. `Restore` replays the log in reverse to undo each mutation.
   Memory cost: O(mutations) typically ≤ a few KB per tx.
   Implementation cost: every mutation site records a small entry;
   `AbortTx` walks the log; `Commit` discards the log.
2. **Copy-on-write detail level.** Snapshot retains the original
   detail slice; mutations clone the affected page (4 KB granule)
   on first write. `Restore` reinstalls the original detail. Cost:
   per-tx O(modified-bitmap-pages × PageSize) memory; restoration
   is a single slice swap.
3. **Status quo (current).** Whole-bitmap clone at BeginTx; whole-
   bitmap restore at AbortTx. Simplest; bounded; only a problem
   for very large databases with high tx rate.

Option 1 is the most space-efficient but adds branching to the
mutation hot path. Option 2 is a hybrid that costs little for small
mutations and degrades gracefully. Option 3 is what shipped in
chunk 1 and is fine until profiling proves otherwise.

## Acceptance

Profile a representative workload (gitfs-style metadata writes;
notes-store document writes) and measure `BeginTx` allocation
pressure. If material, implement option 1 or 2; the
`Bitmap.Snapshot()` / `Restore()` API surface stays the same so the
pager doesn't change. If immaterial, close the issue as "kept
status quo: profiling showed N% / Y MB / Z ns per BeginTx, within
budget."

When this issue closes, the load-bearing rationale moves inline
into `internal/bitmap/bitmap.go` (next to `Snapshot`/`Restore`);
this file is deleted per the no-cite invariant in
`~/.claude/CLAUDE.md §Issue triage`.

## Notes

Surfaced by chunk-1 round-2 internal/pager review. The fix is a
pure implementation choice — the spec does not prescribe a
rollback mechanism, only the result.
