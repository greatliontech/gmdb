# `Keyspace.Delete` semantics on missing key — `ErrNotFound` or no-op?

**Lands:** chunk 5 (Keyspace API + DeleteRange) — the call to
implement `Keyspace.Delete` is in that chunk's surface.

## Problem

`docs/specs/api-surface.md` declares:

```go
func (ks *Keyspace) Delete(key []byte) error
```

without specifying behaviour when the key does not exist. The
error inventory in the same spec lists `ErrNotFound`, and
`Cursor.Delete` already returns errors for missing preconditions
(`ErrCursorUnpositioned`), so a missing key on `Keyspace.Delete`
could plausibly return `ErrNotFound` or be a silent no-op.

Two precedents:

- **BoltDB.** `Delete` is a silent no-op on missing keys.
- **LMDB.** Returns `MDB_NOTFOUND`.

The earlier set-keyspace companion doc proposed:

- `Keyspace.Delete` — silent no-op (caller is saying "make this
  key not exist"; if it already doesn't, that's success).
- `SetKeyspace.DeleteValue(key, value)` — `ErrNotFound` (caller
  is asking to remove a specific element from a set; absence is
  meaningful information).
- `SetKeyspace.Delete(key)` — same `ErrNotFound` treatment as
  `Keyspace.Delete`? Not stated.

This needs a single decision applied uniformly across the API
surface before any implementer locks it in.

## Options

1. **No-op everywhere** — `Keyspace.Delete`, `SetKeyspace.Delete`,
   `SetKeyspace.DeleteValue` all silently return `nil` for missing
   keys. Simpler. Matches BoltDB. Loses the "did anything change"
   signal.
2. **`ErrNotFound` everywhere** — strict. Matches LMDB. Forces
   callers to handle the "already gone" path explicitly even when
   they don't care.
3. **Asymmetric** (the set-keyspace.md leaning):
   `Keyspace.Delete` no-op, `SetKeyspace.DeleteValue`
   `ErrNotFound`. Argues by semantics of "set the key to absent"
   vs "remove a specific member". Open question: what about
   `SetKeyspace.Delete(key)` — the whole-key delete?

## Acceptance criteria

A decision is recorded in `docs/specs/api-surface.md §Keyspace
API` (and `§Keyspace lifecycle` errors, if applicable) covering
every Delete-variant: `Keyspace.Delete`, `SetKeyspace.Delete`,
`SetKeyspace.DeleteValue`, `Cursor.Delete`, `SetCursor.Delete`,
`TypedKS.Delete`, `TypedSetKS.Delete`, `TypedSetKS.DeleteValue`,
`TypedCursor.Delete`, `TypedSetCursor.Delete`. The decision is
either uniform (all no-op or all `ErrNotFound`) or has a single
explicit rule that places each in a category with a one-sentence
rationale.

When this issue closes, the load-bearing rationale moves inline
into `api-surface.md` (and any sibling spec it touches) and this
file is deleted per the no-cite invariant in
`~/.claude/CLAUDE.md`.

## Notes

This is purely an API-surface decision; storage and tree
mechanics are unaffected. The chunk-5 implementer should pick a
decision in a brief `5.1` (planning) sub-chunk and surface it to
the user before lock-in.
