# `SetKeyspace.Put` — return `(added bool, err error)`?

**Lands:** chunk 6 (SetKeyspace storage + API) — the call to
implement `SetKeyspace.Put` is in that chunk's surface.

## Problem

`docs/specs/api-surface.md` declares:

```go
func (ks *SetKeyspace) Put(key, value []byte) error
```

The earlier set-keyspace companion doc proposed extending this
to `Put(key, value []byte) (added bool, err error)` so callers
can tell whether the set actually grew, without paying a
`HasValue` probe first.

Arguments for adding the bool:

- The B+tree already does the membership check during insert (to
  find the insertion point and detect the duplicate-no-op case).
  The information is free; surfacing it is a no-cost API
  enhancement.
- Use cases that need "did this caller cause the set to grow"
  — pub/sub broadcasts, ref-counted indexes, idempotent retries
  — currently require a separate `HasValue` round-trip plus
  retry logic to handle the in-between race window. The bool
  collapses that pattern.

Arguments against:

- Breaks symmetry with `Keyspace.Put`, which returns `error`
  only. The API gets a typed variant by accident.
- The same caller can read the value back via cursor or
  `HasValue` after `Put` and observe identical information; the
  bool is a convenience, not a capability.
- `TypedSetKS.Put` would have to mirror the bool through its
  wrapper, propagating the asymmetry into the generic layer.

## Options

1. **Add the bool** — `Put(key, value []byte) (added bool, err
   error)`. Mirror in `TypedSetKS.Put`.
2. **Keep error-only** — preserve symmetry with `Keyspace.Put`;
   callers needing the signal use `HasValue` + `Put`. (This is
   what `docs/specs/api-surface.md` currently documents.)
3. **Sibling method** — `Put` stays error-only, add
   `PutNew(key, value) (added bool, err error)` as a named
   variant. Avoids changing the simple signature while making the
   bool available to callers who need it.

## Acceptance criteria

A decision is recorded in `docs/specs/api-surface.md §Keyspace
API` (and `docs/specs/typed-keyspaces.md` if a typed mirror is
needed). When this issue closes, the load-bearing rationale
moves inline into those specs and this file is deleted per the
no-cite invariant in `~/.claude/CLAUDE.md`.

## Notes

Same shape as the chunk-5.1 Delete-on-miss decision (pinned at
`api-surface.md §Invariants`; see `git log --all -- docs/issues/
keyspace-delete-missing-key.md` for the chunk-5.1 lock-in
context). Purely API-surface; storage is unaffected. The
chunk-6 implementer should pick a decision in a brief `6.1`
(planning) sub-chunk and surface it to the user before lock-in.
