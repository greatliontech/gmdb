# Indexed SetKeyspace.BulkLoad leaks internal btree/bulkload error sentinels instead of the public ones

**Lands:** audit-burndown-2026-07 chunk 20.

**Severity:** [M] — `errors.Is(err, gmdb.ErrKeyTooLarge)` is false on
the indexed-set path (demonstrated: oversize key on an indexed
SetKeyspace yields the internal `btree: key too large…`); breaks the
documented sentinel (`errors.go:131-138`). Companion [L]: a key's
*first* value bypasses the promotion threshold
(`bulkload.go:686-694`), so an oversize first value reaches the
builder and returns internal `errBulkEntryTooLarge` — the guard's
"not a reachable in-spec input" claim (`bulkload.go:13-21`) is false
for variable-size sets; the same input via Put maps to ErrKeyTooLarge.

**Source:** 2026-07-04 full-codebase audit (bulkload/maintenance
auditor).

**Governing spec:** `docs/specs/bulkload.md` (error contract);
`docs/specs/limits.md`.

## Problem

`SetKeyspace.bulkLoadIndexed` returns `bulkLoadStream` / `sb.flush` /
`sb.top.finish` errors without `mapBtreeErr`
(`bulkload_indexed.go:791-800`), unlike the un-indexed path
(`bulkload.go:531-539`); the `setBulk.flush` comment
(`bulkload.go:721-724`) promises the public sentinel.
`error_keytoolarge_test.go` pins 4 of the 5 paths — exactly this one
is missing.

## Fix direction

Wrap the three returns in mapBtreeErr; route the oversize-first-value
case to the public sentinel (or gate it at entry like Put does);
correct the false unreachability comment. Extend the sentinel test to
all paths including oversize-first-value.
