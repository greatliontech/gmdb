# Statistics surface (DBStats / TxStats / KeyspaceStats) spec'd but entirely unimplemented

**Lands:** proactive — spec'd capability gap, currently an untracked
silent downscope.

**Severity:** [M]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw finding 9.

**Governing spec:** `docs/specs/api-surface.md:1303-1378 §Statistics`;
plan note `docs/plans/v0-implementation.md:2081`.

## Problem

The entire observability/monitoring surface promised by the master API
spec is missing, with **no tracked deferral** — a silent downscope. None
of `DBStats` / `TxStats` / `KeyspaceStats` (and `db.Stats()` /
`Tx.Stats()` / `Keyspace.Stats()` / `SetKeyspace.Stats()`) exist. So
operators cannot introspect: free/retired page counts, active readers,
per-tx CoW/split/merge/index counters, or per-keyspace depth/page-type
breakdown. The plan's own roadmap (chunk 11 "final wiring") asserts a
delivery that did not happen.

## Fix

Implement the `§Statistics` surface (the types + the four `Stats()`
methods), **or** file a concrete `Lands:` deferral per gap and surface to
the user. Either way, correct the plan's chunk-11 "final wiring" claim,
which currently asserts delivery that did not happen.
