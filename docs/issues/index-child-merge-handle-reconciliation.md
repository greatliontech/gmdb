# Child-transaction commit strands the parent's IndexHandles on the pre-child index tree

**Lands:** audit-burndown-2026-07 chunk 18.

**Severity:** [H] — silently stale results with Err()==nil (failing
reproducer: 1 row via the parent's pre-child handle vs 201 via a
fresh one); after a child Drop/Rebuild the parent handle descends a
FreeSubtree'd root — freed-page reads. Same mechanism in the
SetKeyspace mirror (mergeSetKeyspaceHandles) and for Stats().

**Source:** 2026-07-04 full-codebase audit (indexing/typed auditor).

**Governing spec:** `docs/specs/indexing.md` §Handle Invalidation; the
BeginChild contract stated at `nested.go:24-26` ("the parent's handles
reflect the committed child work").

## Problem

BeginChild clones pinned indexes into fresh pinnedIndex objects
(`nested.go:167-177, 208-218`; openIndexHandles deliberately not
carried). On child commit, `mergeKeyspaceHandles`
(`nested.go:242-277`) swaps `pks.indexes = cks.indexes` wholesale, but
every *IndexHandle the parent handed out still points (`index.go:49`)
at the old pinnedIndex with the pre-child root/count; the merge's
markCursorsStale → markIndexHandlesStale (`keyspace_core.go:254-265`)
refreshes cursor roots from that stale root. Child Drop ran
markIndexHandleDead only on the child clone
(`index_rebuild.go:542-547`), so the parent handle is never marked
dead (Inv-IHS2 violation).

## Fix direction

The merge reconciles `pks.openIndexHandles` against the incoming map:
re-point each handle's pinned at the same-name entry (mark dead if
absent), then stale-mark cursors; mirror in mergeSetKeyspaceHandles.
Regression: adapt the auditor's reproducer (parent handle after child
commit sees child rows; after child Drop, handle reports dead, not
freed-page reads).
