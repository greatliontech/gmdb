# Read-only keyspace opens never load declared indexes — spec'd index lookups on RO handles are unreachable

**Lands:** audit-burndown-2026-07 chunk 19.

**Severity:** [M] — spec'd capability missing on every surface
(failing reproducer: ks.Index(name) → ErrIndexNotFound); the
backup/inspector use-case indexing.md cites is unreachable.

**Source:** 2026-07-04 full-codebase audit (indexing/typed auditor).

**Governing spec:** `docs/specs/indexing.md` §Open Semantics — stated
twice: "OpenKeyspaceReadOnly(name) … Index lookups still work — they
read stored index entries directly"; the OpenKeyspaceReadOnly godoc
(`keyspace.go:151-157`) repeats the claim.

## Problem

Neither `Tx.OpenKeyspaceReadOnly` (`keyspace.go:161-192`) nor
`OpenSetKeyspaceReadOnly` (`set_keyspace.go:158-192`) populates
`ks.indexes` from the on-disk registry; `ks.Index(name)`
(`index.go:238-242`) returns ErrIndexNotFound for every declared
index. `ReadTx.OpenKeyspaceReadOnly` (`read_tx_keyspace.go:58`)
delegates to the same path. No test pins RO index lookup.

## Fix direction

RO open loads registry entries and synthesizes extractor-less pinned
decls (Columns/Unique/Covering from indexRegistryEntry); the existing
write guard already prevents maintenance needing Extract. Regression:
adapt the auditor's reproducer across Tx and ReadTx surfaces,
Keyspace and SetKeyspace.
