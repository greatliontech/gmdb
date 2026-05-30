# `Tx` is a god-object spanning unrelated responsibilities

**Lands:** condition — proactive burn-down. No external blocker: the
breaking public-surface slice already landed (the raw pager-primitive
cluster was withdrawn from the public API — see api-surface.md §ReadTx
and `export_test.go`). The remaining decomposition is internal and can
land any time.

## Problem

`Tx` (`tx.go:24`) carries **21 struct fields** and **16 exported
methods** (down from 22 after the raw pager-primitive cluster was moved
out of the public surface). The remaining method surface still spans
responsibilities independent of the transaction-lifecycle concern a
`Tx` should own:

1. **Keyspace DDL** — `OpenKeyspace`, `OpenKeyspaceReadOnly`,
   `CreateKeyspace`, `CreateKeyspaceIfNotExists`, `OpenSetKeyspace`,
   `OpenSetKeyspaceReadOnly`, `CreateSetKeyspace`,
   `CreateSetKeyspaceIfNotExists`, `DeleteKeyspace`, `ListKeyspaces`,
   `SetKeyspaceConfig` (~11 methods).
2. **Index DDL** — `RebuildIndex`, `DropIndex`.
3. **Transaction lifecycle** — `Commit`, `Rollback`, `BeginChild`,
   `SetFileFormat`.

The index-DDL cluster is bolted onto the transaction object rather than
owned by it.

(The original audit listed a fourth cluster — raw pager passthrough:
`AllocPage` / `FreePage` / `Page` / `CoW` / `AllocSlab` / `Mutate`.
That cluster has since been removed from the public surface: the five
live methods moved to `export_test.go` as test-only and `Mutate` was
deleted as dead. That work closed the highest-severity slice of this
issue.)

## Resolution

- Cluster (2) Index DDL (`RebuildIndex` / `DropIndex`) operates on a
  named keyspace's index registry — it reads more naturally as a small
  index-admin accessor (`tx.Indexes()` returning an unexported helper,
  or methods on the opened `*Keyspace`) than as top-level `Tx` methods.
- Keep clusters (1) and (3) on `Tx` — they are the transaction's
  genuine surface — but reconsider whether the `Create*IfNotExists` ×
  `{Keyspace, SetKeyspace}` matrix (4 variants) needs to be 4 distinct
  top-level methods or can collapse behind an options arg.

This is a surface-shaping change, not a behavioral one; the underlying
implementations stay put and are re-homed behind narrower handles.

## Notes

Surfaced during the 2026-05-30 architecture/factoring audit
(root-package composition pass). Method/field counts verified by grep;
the 22→16 exported-method reduction reflects the landed raw-pager
removal.
