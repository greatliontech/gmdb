# IndexStats shape diverges from spec, and TreeDepth is hardwired to 0

**Lands:** proactive — a published public struct that diverges from spec
and returns a constant for one field.

**Severity:** [L]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw findings 16
and 23; also the completeness pass.

**Governing spec:** `docs/specs/api-surface.md:1380-1388`.

## Problem

Two coupled gaps in the same public struct (`index_types.go`,
`index.go:88-129`):

1. **Shape divergence** (finding 16). The implemented
   `IndexStats{Count uint64; TreeDepth int}` is a much smaller, different
   shape than the spec's `Depth / BranchPages / LeafPages / Entries /
   Unique / Covering / SizeBytes`. `Index.Stats()` exposes neither the
   page-type breakdown, size, nor the Unique/Covering descriptors. A
   consumer coding to the spec'd struct would not compile against the
   implemented one.
2. **TreeDepth always 0** (finding 23). `IndexStats.TreeDepth` is
   hardwired to 0 — persisted in the public struct but never computed, so
   any consumer reading it for index-health diagnostics silently gets 0
   for every index. The depth is trivially available: `btree.Walk`'s
   visit callback already receives `depth int` (used by `check.go`'s
   `walkTree`).

## Fix

Reconcile the implemented `IndexStats` with the spec'd shape (or amend the
spec to the chosen shape via the spec-amend channel), and populate the
page-walk fields — compute `Depth`/`BranchPages`/`LeafPages`/`Entries`/
`SizeBytes` via a single `btree.Walk` (or reuse the chunk-7.7 Lookup tree
walk that already traverses the index tree), and fill `Unique`/`Covering`
from the index descriptor. Drop no field silently: any field that stays
must be computed, not left constant-0.
