# Typed layer: incoherent `Keyspace`/`KS` naming + duplicated Open/Create

**Lands:** condition — the rename is breaking (exported types), so it
must land before the first tagged release while `development: true`;
the internal duplication cleanup can ride along.

## Problem

**Naming.** The typed layer uses two spellings for the same concept
tier with no documented convention:

- Spelled-out = stateless declaration/factory: `TypedKeyspace`
  (`typed.go:43`), `TypedSetKeyspace` (`typed_set.go:24`),
  `NewTypedKeyspace`, `NewTypedSetKeyspace`.
- Abbreviated = tx-scoped opened handle: `TypedKS` (`typed.go:138`),
  `TypedSetKS` (`typed_set.go:106`).

So `NewTypedKeyspace(...).Open(tx)` returns a `*TypedKS` — a reader
cannot predict that from the names. The convention (Decl-vs-Handle, the
prepared-statement/execution split) is *correct in design* but
*undocumented and non-obvious*, and the byte layer has no analogous
split (`Keyspace` / `SetKeyspace` only), making the typed layer the
lone offender. The `Index` family has the same latent overload, tracked
in `index-noun-overload.md`.

**Duplication.** Within the typed layer, `translateIndexes`, the `wrap`
helper, and the four `Open` / `OpenReadOnly` / `Create` /
`CreateIfNotExists` methods are structurally identical between
`TypedKeyspace` (`typed.go:61-134`) and `TypedSetKeyspace`
(`typed_set.go:39-103`), differing only in the byte-layer call target.

## Resolution

- Rename to a self-evident pair. Options: `TypedKeyspaceDecl` (factory)
  + `TypedKeyspace` (handle); or keep `TypedKeyspace` as the factory and
  name the handle `TypedKeyspaceHandle`. Apply the same to the Set and
  Index typed types so the whole typed layer reads consistently.
- Factor the identical `translateIndexes` / `Open` / `Create` quartet
  into a shared generic helper parameterized by the byte-layer target.

## Notes

Surfaced during the 2026-05-30 architecture/factoring audit (public API
surface + composition passes). The typed handles are otherwise
confirmed thin zero-cost wrappers (encode → delegate → decode, no
re-implemented btree logic).
