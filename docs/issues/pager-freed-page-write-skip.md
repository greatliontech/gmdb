# Freed / tail-refunded pages' slab buffers are still pwritten at commit

**Lands:** audit-burndown-2026-07 chunk 5.

**Severity:** [L] — pure write amplification; no correctness impact
(offsets are within fileSize; content in free pages is harmless).

**Source:** 2026-07-04 full-codebase audit (durability + btree/pager
auditors, independently).

**Governing spec:** `docs/specs/pager-slab.md` (commit pipeline).

## Problem

`commitStep0` moves loose pages to `pendingFrees` but leaves their
slab buffers in `p.dirty` (`internal/pager/commit.go:221-225`), so
`commitStep1` pwrites pages the same commit marks free
(`commit.go:345-354`). `FreePage` likewise leaves loose buffers dirty,
and `TailRefund` (`internal/pager/freespace.go:544-551`) never
Discards them — rebalance-heavy transactions pwrite every intermediate
page, including pages beyond the new HWM.

## Fix direction

Drop freed/refunded pages from `p.dirty` (Discard) in commitStep0 /
FreePage / TailRefund, taking care not to disturb savepoint-restore
invariants (chunk 3). Pin with a commit-write-set assertion test.
