# Byte-level composable iterators Keyspace/SetKeyspace.All/Range/Prefix unbuilt; typed layer reimplements

**Lands:** proactive — already documented-deferred in the plan; needs a
concrete trigger or a build.

**Severity:** [L]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw finding 15.

**Governing spec:** `docs/specs/api-surface.md:1144-1151` (signatures);
plan `docs/plans/v0-implementation.md:1153` (deferral) and `:1498`
(claimed delegation).

## Problem

The flagship public iteration surface for byte keyspaces —
`Keyspace.All/Range/Prefix` and `SetKeyspace.All/Range/Prefix` — is
unbuilt; callers must drop to the stateful `Cursor` for range/prefix
scans. It is documented-deferred (so not a *silent* gap), but the plan
also claims the typed layer **delegates** to these byte iterators
(`:1498`) while in fact the typed layer **reimplements** the iteration
logic — a plan-vs-code inconsistency.

## Fix

Build the byte-level `All/Range/Prefix` (the typed layer then delegates as
the plan describes), **or** update the plan to reflect that the typed
iterators are standalone and give the byte iterators a concrete `Lands:`.
Low because it is tracked as deferred — but the plan's delegation claim
should be corrected either way.
