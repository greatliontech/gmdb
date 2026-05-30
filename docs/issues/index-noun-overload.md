# `Index` noun is overloaded: query handle vs the `Index*` declaration family

**Lands:** condition — before the first tagged release (renaming the
exported handle type is breaking).

## Problem

`Index` is used for two different roles:

- The **live query handle** returned by `ks.Index(name) (*Index,
  error)` — a tx-scoped cursor-backed object with `Lookup` / `Range` /
  `Prefix` / `Get` / `Err` / `Stats`.
- The **prefix of the declaration family**: `IndexDecl`, `IndexColumn`,
  `IndexEntry`, `IndexExtractor`, `IndexStats`, `IndexFingerprintError`
  (plus `CoveringColumn`, which is a peer of `IndexColumn` but breaks
  the prefix).

A reader who sees `Index` cannot tell "query handle" from "the Index
family of value types." The typed side already got this right —
`TypedIndexHandle` / `TypedIndexQuery` — so the byte-layer handle is
the inconsistent one.

## Resolution

Rename the byte-layer handle to `IndexHandle` or `IndexQuery` (match
whichever the typed side settles on per
`typed-layer-naming-and-duplication.md`), freeing the bare `Index`
noun to read as the family prefix. Optionally rename `CoveringColumn`
→ `IndexCoveringColumn` for prefix consistency, or accept it as a
documented peer.

## Notes

Surfaced during the 2026-05-30 architecture/factoring audit (public API
surface pass). Pairs with `typed-layer-naming-and-duplication.md` —
settle the handle-naming convention once and apply it to both layers.
