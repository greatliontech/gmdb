# `Tx.SetKeyspaceConfig` missing-name behavior unspecified

**Lands:** chunk 5.5 — `SetKeyspaceConfig` lands there per the
chunk-5.1 plan; the missing-name decision needs to be recorded
before the implementation commits.

## Problem

`docs/specs/api-surface.md §Database and Transaction API`
declares:

```go
func (tx *Tx) SetKeyspaceConfig(name string, cfg KeyspaceConfig) error
```

Neither inline godoc nor any §Invariants clause documents what
happens when `name` does not resolve to an existing user
keyspace. By the chunk-5.1 Delete-on-miss invariant this should
plausibly be `ErrNotFound` (consistent with
`Tx.DeleteKeyspace(name)`), but the invariant's framing was
narrowed in chunk 5.1 to **keyspace-content keyed removal** —
explicitly out of scope for configuration mutation. So the
behavior is left undefined.

## Acceptance

Decide the missing-name sentinel for `Tx.SetKeyspaceConfig`:
`ErrNotFound` (consistent with `Tx.DeleteKeyspace`) or a
distinct namespace sentinel. Record the decision in
`api-surface.md` (inline godoc on the signature; if it
generalises across more surfaces, an §Invariants block). The
chunk-5.5 implementer locks the decision in a brief planning
step and surfaces it to the user before lock-in.

When this issue closes, the load-bearing rationale moves inline
into `api-surface.md` and this file is deleted per the no-cite
invariant.

## Notes

Surfaced by the chunk-5.1 adversarial review (Round 1 M-1
spillover) when the Delete-on-miss invariant was narrowed to
keyspace-content removal to avoid coupling unrelated namespaces.
The narrowing was correct; the gap it surfaced is real and
filed here rather than coded around. Originally filed as a
compound `Tx.SetKeyspaceConfig + Tx.RebuildIndex` issue; split
in chunk-5.1 Round 2 because the two surfaces land in different
chunks (5.5 vs 7) and each disposition is independent. The
sibling `tx-rebuildindex-missing-name-behavior.md` carries the
RebuildIndex half.
