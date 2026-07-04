# Covering-index bytes go stale when an update changes the value but not the index key; rebuild vs maintenance also disagree on duplicate collapse

**Lands:** audit-burndown-2026-07 chunk 17.

**Severity:** [H] — wrong query results from every covering lookup
after a value-only update (failing reproducer on HEAD), and
Check(CheckIndexes) flags an ordinary Put-update workload as corrupt.
Companion [L]: rebuild keeps the first duplicate entry
(`index_rebuild.go:215-222`) while maintenance keeps the last
(`index_maintain.go:130-137`, `index_setkeyspace.go:196-205`) —
becomes load-bearing once the value-diff fix lands; align tie-breaks
in the same change set.

**Source:** 2026-07-04 full-codebase audit (indexing/typed auditor).

**Governing spec:** `docs/specs/indexing.md` atomic-maintenance
invariant (row + index always agree);
`docs/specs/typed-keyspaces.md` §Covering ("identical results, fewer
reads").

## Problem

`buildReplacePlans` (`index_engine.go:153-186`) diffs old/new entry
sets by encoded index key only (indexEntryKey = Cols + PK; Cover is
not part of the key). A Put whose extracted index key is unchanged but
whose covering bytes change lands in neither dels nor ins; the stored
covering blob (indexEntryValue, `index_maintain.go:55-75`) is never
rewritten. Typed CoverValue: Put(1,"alice"), Put(1,"anna") with same
IK → covering Lookup returns (1,"alice") while Get(1) returns "anna".
Check computes expected values via indexEntryValue (`check.go:874,
958-966`) and reports these as value mismatches.

## Fix direction

Diff by (key, encoded value): an entry present in both sets whose
indexEntryValue differs must be rewritten (delete+insert or in-place
Put; the unique probe must treat the overwrite as benign). Align
rebuild/maintenance duplicate tie-break (last-wins) in the same
change set. Regression: adapt the auditor's failing reproducers
(typed CoverValue + byte-API covering update).
