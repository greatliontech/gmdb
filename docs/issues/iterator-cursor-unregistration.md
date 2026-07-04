# Convenience iterators leak cursor registrations for the tx lifetime

**Lands:** audit-burndown-2026-07 chunk 16.

**Severity:** [M] — O(iterations) memory growth and O(iterations)
per-mutation stale-walk cost in long write transactions (batch job
alternating `for range ks.All()` with Puts degrades quadratically).

**Source:** 2026-07-04 full-codebase audit (indexing/typed auditor).

**Governing spec:** `docs/specs/api-surface.md` (iterators);
engine-internal precedent treats this class as real:
`keyspace.go:839-845` (deleteRangeIndexed uses newInternalCursor
precisely to avoid unbounded openCursors growth) and
`index.go:290-314` (IndexHandle.registerCursor pairs with a deferred
unregister).

## Problem

`Keyspace.All/Range/Prefix` (`iterators.go:25, 40, 61`; SetKeyspace
analogues) call `ks.Cursor()`, which appends to `ks.openCursors`
(`keyspace.go:899-911`) with no unregistration; every subsequent
Put/Delete walks the grown slice in markCursorsStale.

## Fix direction

Unregister on iterator-closure exit (including early break), mirroring
the IndexHandle pattern. Regression: assert len(openCursors) is
constant across N completed iterations.
