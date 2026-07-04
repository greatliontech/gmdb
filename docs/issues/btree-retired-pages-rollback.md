# Failed un-indexed write op retires still-referenced pages; next commit publishes them to the RPL → reclamation frees live tree pages

**Lands:** audit-burndown-2026-07 chunk 3.

**Severity:** [H] — silent corruption of committed data on an in-spec
path (ErrDBFull/ErrTxTooLarge mid-op, then commit — the engine's
documented rest-of-tx-continues contract).

**Source:** 2026-07-04 full-codebase audit (btree/pager auditor).
Sibling of commit c6f441b, which rolled back only the loose-page half.

**Governing spec:** `docs/specs/free-space.md` (entailed bitmap
consistency: a bitmap-free page is referenced by no committed tree).

## Problem

Every btree mutation frees old (prior-tx) pages before the last
fallible step: Put frees the old leaf at `internal/btree/put.go:256,
310, 374, 461` (+ displaced overflow chains at 369/455) before the
branch ascend, which can fail (ErrDBFull, ErrTxTooLarge); ascendNoSplit
(`put.go:645`) frees old branches level-by-level. Delete likewise at
`internal/btree/delete.go:209-217, 280-287, 1153-1158, 1226-1231,
1312-1317, 124`. `Pager.FreePage` routes prior-tx pages to
`retiredPages` (`internal/pager/freespace.go:390`); nothing un-retires
on the error return. Only savepoint restore truncates `retiredPages`
(`internal/pager/savepoint.go:493-495`), and the keyspace layer opens
a savepoint only when indexed (`keyspace.go:619-622, 697-709,
1186-1210`; the comment at 616-618 claiming the un-indexed branch
"needs neither layer" is wrong for retiredPages). Commit appends
retiredPages to the RPL (`internal/pager/commit.go:230-234`) while the
committed meta's tree still references them; reclamation frees and
reuses them → wrong values / ErrCorrupted on committed data. Secondary:
intermediate ascend branch CoWs leak durably (BitmapLeak).

## Fix direction

Cover un-indexed Keyspace.Put/Delete, Cursor.Delete, and SetKeyspace
analogues with the shallow savepoint the indexed path already uses, or
truncate retiredPages (and roll back the op's allocations) on the
btree error return. Regression at pager level: 1 free page, HWM at
max; Put fails in ascend; assert retiredPages does not contain the old
leaf (or the savepoint restored it); commit; assert reclaimRPL never
frees a page reachable from the committed root.
