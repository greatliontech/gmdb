# `Tx.RebuildIndex` missing-name behavior unspecified

**Lands:** chunk 7 — `Tx.RebuildIndex` lands as part of chunk-7
indexing (the recovery path after
`ErrIndexFingerprintMismatch`); the missing-name decision needs
to be recorded before the implementation commits.

## Problem

`docs/specs/api-surface.md §Database and Transaction API`
declares:

```go
func (tx *Tx) RebuildIndex(keyspace string, decl *IndexDecl) error
```

Neither inline godoc nor any §Invariants clause documents what
happens when `keyspace` does not resolve to an existing user
keyspace, or when `decl.Name` does not match an existing
registry entry on the keyspace. The chunk-5.1 Delete-on-miss
invariant was narrowed to **keyspace-content keyed removal** —
explicitly out of scope for index maintenance — so the behavior
is left undefined.

Two distinct missing cases:

1. `keyspace` does not exist. Likely `ErrNotFound` (consistent
   with `Tx.DeleteKeyspace`), but worth confirming the sentinel
   matches the rest of the keyspace-management surface.
2. `keyspace` exists but `decl.Name` does not match any entry
   in the keyspace's index registry. Likely `ErrIndexNotFound`
   (consistent with `Tx.DropIndex`), since the namespace is
   index-level.

## Acceptance

Decide both sentinels and record them in `api-surface.md`
(inline godoc on the signature; if it generalises, an
§Invariants block). The chunk-7 implementer locks the decision
in a brief planning step and surfaces it to the user before
lock-in.

When this issue closes, the load-bearing rationale moves inline
into `api-surface.md` and this file is deleted per the no-cite
invariant.

## Notes

Surfaced by the chunk-5.1 adversarial review (Round 1 M-1
spillover) when the Delete-on-miss invariant was narrowed to
keyspace-content removal. Originally filed as a compound
`Tx.SetKeyspaceConfig + Tx.RebuildIndex` issue; split in
chunk-5.1 Round 2 because the two surfaces land in different
chunks (5.5 vs 7). The sibling
`tx-setkeyspaceconfig-missing-name-behavior.md` carries the
SetKeyspaceConfig half.
