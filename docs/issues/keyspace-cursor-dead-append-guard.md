# `Keyspace.Cursor` appends unconditionally; `SetKeyspace.Cursor` guards `if !ks.dead`

**Lands:** condition — proactive burn-down. Low severity (bounded by tx
lifetime, no correctness/data impact). Pull when next touching cursor
lifecycle.

## Problem

`SetKeyspace.Cursor` (set_cursor.go) guards its open-cursor
registration:

```go
if !ks.dead {
    ks.openSetCursors = append(ks.openSetCursors, c)
}
```

with a godoc explaining the guard prevents unbounded growth: a
pathological `for { sks.Cursor() }` after `DeleteKeyspace` would
otherwise grow `openSetCursors` without bound, and
`markSetCursorsStale` walks every entry on every sibling mutation.

`Keyspace.Cursor` (keyspace.go) has **no such guard** — it appends to
`openCursors` unconditionally and carries no rationale comment:

```go
c := &Cursor{inner: ks.newRootCursor(), tx: ks.tx, ks: ks}
ks.openCursors = append(ks.openCursors, c)
return c
```

## Demonstrated fault

Reachable, in-spec, within one write tx:

```go
ks, _ := tx.CreateKeyspace("x")
tx.DeleteKeyspace("x")          // ks.dead = true
for i := 0; i < N; i++ {
    ks.Cursor()                 // each appends to openCursors
}
```

`openCursors` grows to N entries on a dead handle and the references are
held for the transaction's lifetime. Bounded by tx lifetime (the slice
is freed at tx end) with no correctness or data-integrity impact —
purely a per-tx memory/CPU amplification on a pathological path.
Severity: low. The `Cursor()` returned on a dead handle is already
useless (requireOpen / Err surface ErrKeyspaceClosed), so not
registering it loses nothing.

## Resolution

Mirror the SetKeyspace guard on `Keyspace.Cursor`:

```go
if !ks.dead {
    ks.openCursors = append(ks.openCursors, c)
}
```

with the same rationale comment. Add a regression test asserting
`len(ks.openCursors)` stays bounded across repeated `Cursor()` calls on
a DeleteKeyspace'd handle (mirroring the SetKeyspace coverage if it
exists). Behavioral change (a deliberate bug fix), so land it as its own
change set with the test — not folded into a pure refactor.

## Notes

Surfaced by the fresh-eyes adversarial review of the keyspaceCore
cursor-factory extraction: the `newRootCursor` cut preserved the
asymmetry rather than silently changing it. Pre-existing — the
unconditional append predates that refactor.
