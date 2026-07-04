# SetCursor materializes the entire value set per key position — unbounded, undocumented memory

**Lands:** audit-burndown-2026-07 chunk 21.

**Severity:** [L] — no wrong results; unbounded allocation on the
postings-list/adjacency workloads `set-keyspace.md:28-33` targets
(millions of members per key), and SetCursor.CountValues is O(set)
via the copy vs the O(1) SetKeyspace.CountValues the spec advertises.

**Source:** 2026-07-04 full-codebase audit (bulkload/maintenance
auditor).

**Governing spec:** `docs/specs/set-keyspace.md` — no clause bounds or
documents cursor memory behavior (decide: fix streaming, or spec the
bound; user granted blanket authority 2026-07-04, prefer the
streaming fix if proportionate).

## Problem

Positioning on a nested-tree key copies every member into memory
(`set_cursor.go:650-695`, materializeAtOuter); `SetCursor.CountValues`
(`set_cursor.go:470-481`) rides that copy.

## Fix direction

Stream nested-tree members through the cursor position (iterate the
nested tree lazily, as SetKeyspace.Values does) and take CountValues
from the O(1) nested count; if streaming is disproportionate for the
cursor model, document the materialization bound in set-keyspace.md
and api-surface.md instead. Regression/bench: cursor position on a
large set must not allocate O(set) (or the documented bound stands).
