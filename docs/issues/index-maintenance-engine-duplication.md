# Index-maintenance diff/probe/insert engine duplicated per keyspace kind

**Lands:** condition — proactive burn-down; pull when next touching
index maintenance. Internal refactor, no external blocker, not
breaking.

## Problem

The engine that maintains secondary indexes on a mutation —
compute the index-key diff, run the unique-violation probe, insert/
delete index entries — is implemented twice, receiver-coupled to the
two keyspace kinds rather than factored into a kind-agnostic helper:

- `*Keyspace`: `applyIndexMaintenanceOnPut` / `…OnDelete`
  (`index_maintain.go:197` / `:348`).
- `*SetKeyspace`: `applyIndexMaintenanceOnAddValue` / `…OnRemoveValue`
  / `…OnBulkKeyDelete` (`index_setkeyspace.go:221` / `:368` / `:313`).

The two `apply*` families share the `perIndex` plan struct, the
`sortedIndexNames` ordering, the per-index `opIdx` insert loop with its
test-hook, and — most starkly — the **verbatim unique-probe loop**
(`index_maintain.go:246-268` vs `index_setkeyspace.go:254-271`,
identical but for the error-string). Each family also redeclares its
own local `perIndex` type.

The genuine difference is only the *unit*: a `Keyspace` maintains
indexes keyed by `(row key → row value)`; a `SetKeyspace` maintains
them per `(set key, member)` pair. That difference is a small
projection at the edges, not a reason to fork the whole engine.

## Resolution

Extract a kind-agnostic maintenance engine that takes the index plan +
a `(prior, next)` extraction pair and owns the diff / unique-probe /
insert-delete loop. The two keyspace kinds supply only the projection
from their mutation to `(prior, next)` index entries. Collapses the
duplicated probe/insert loop and the two `perIndex` redeclarations.

Composes with `keyspace-setkeyspace-shared-core.md` but is a distinct
change set — that issue is about the keyspace *struct* infrastructure;
this is about the index-maintenance *algorithm* living in two places.

## Notes

Surfaced during the 2026-05-30 architecture/factoring audit
(root-package composition pass, sub-agent deep dive). Verbatim
probe-loop claim verified at the cited line ranges.
