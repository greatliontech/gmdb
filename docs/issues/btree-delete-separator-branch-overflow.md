# Delete-path separator growth can overflow a full parent branch — no split fallback on the delete path

**Lands:** audit-burndown-2026-07 chunk 2.

**Severity:** [M] — a valid in-spec Delete fails with an encode-size
error; by that point old sibling pages are already retired, feeding
the btree-retired-pages-rollback corruption path (chunk 3).

**Source:** 2026-07-04 full-codebase audit (btree/pager auditor).

**Governing spec:** `docs/specs/page-formats.md` (branch capacity),
`docs/specs/range-delete.md` (separator invariant).

## Problem

Leaf/branch redistribute recomputes the boundary separator
(`internal/btree/delete.go:1232, 1344`) and installs it in the parent
(`delete.go:592, 866`); the new separator can be much longer than the
old (boundary keys sharing a long prefix ⇒ ShortestSeparator =
prefix+1 bytes). The parent is re-encoded at `delete.go:684` with no
branch-split fallback (unlike Put's ascendWithSplit): near-full parent
+ separator growth ⇒ EncodeBranch size error on a valid Delete. The
old sibling leaves are already retired inside
`mergeOrRedistributeLeaves` (`delete.go:1226-1231`) and nothing rolls
back `mergedID`/`newLeft/RightID` at the failure return.

## Fix direction

Give the delete-path parent patch a split (or pre-flight capacity
check choosing merge-direction/redistribute-mid to bound separator
growth). Must compose with chunk 3's rollback so no path retires pages
before the last fallible step. Regression: full parent with short
separator; craft siblings so redistribute's mid lands between two
long-shared-prefix keys; delete to underflow; assert success.
