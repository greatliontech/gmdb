# Nested SHALLOW savepoints double-reference loose-popped buf

**Lands:** condition — when nested SHALLOW savepoints become an in-spec
production primitive, OR when a panic-on-nested-shallow guard lands in
`Pager.BeginShallowSavepoint`. Today's 6 per-row callers
(`Keyspace.Put`/`Delete`, `Cursor.Delete`, `SetKeyspace.Put`/`Delete`/
`DeleteValue`) each open-and-resolve one shallow savepoint per call;
nested shallows are reachable only via test code.

## Problem

`AllocPage`'s loose-pop branch (`internal/pager/freespace.go`
loose-pop section) appends the SAME `buf` pointer to every active
SHALLOW savepoint's `loosePopLog`. `RestoreSavepoint`'s loose-pop
replay (`internal/pager/savepoint.go`, step 4) when
`wasPreWindow=true` does:

```go
if cur, ok := p.dirty[entry.id]; ok {
    p.bufPool.Put(cur)
}
p.dirty[entry.id] = entry.buf
```

For nested SHALLOW (outer A, inner B; loose-pop fires in B; both
get wasPreWindow=true because dirty[id] was pre-A-and-B):

1. Inner B Restore: step-4 sees `dirty[id]` absent (detached), no
   pool-Put, installs `dirty[id] = buf` (B's loose-pop entry).
2. Outer A Restore (later): step-4 sees `cur = buf` in dirty[id]
   (B's re-install). pool-Put(buf) → buf enters the pool's free
   list. Then `dirty[id] = entry.buf` (= same buf pointer).

End state: `buf` is in both `bufPool`'s free list AND in `dirty[id]`.
A subsequent `bufPool.Get()` returns `buf`; the new caller writes
to it, silently corrupting `dirty[id]`'s content.

## Reachability

- **Production**: unreachable. Every per-row indexed maintenance
  helper opens exactly one shallow savepoint, completes its row
  work, and resolves it. There is no call site where a shallow
  savepoint window contains another shallow savepoint.
- **Tests**: structurally reachable via `BeginShallowSavepoint`
  inside another shallow window. The existing
  `TestShallowSavepointOutOfOrderPanics` opens nested shallows but
  exercises the LIFO panic path, not loose-pop + Restore-both. No
  current test triggers the bug.
- **Pre-existence**: the unconditional `pool-Put cur; install
  entry.buf` pattern existed pre-savepoint-undo-log work (commit
  `15f9b70`'s shallow-savepoint introduction). The H-2 fix in the
  shallow-savepoint-clone-cost close-out added the
  `wasPreWindow=false` branch but left the `wasPreWindow=true`
  pattern unchanged. Surfaced by Round-2 adversarial review of
  that close-out (M-1 finding).

## Resolution candidates

1. **Panic-on-nested-shallow guard.** Add a check in
   `BeginShallowSavepoint` that panics (or returns an error) if
   another shallow savepoint is already on `p.activeSavepoints`.
   Matches the 6 production callers' actual usage; reflects the
   spec's framing of shallow as a per-row internal-helper
   primitive. Cheapest fix; closes the bug by making the buggy
   code unreachable from any caller. Spec amend: state explicitly
   that BeginShallowSavepoint is single-active per tx.
2. **Per-buffer reference count or pool-Put deduplication.**
   Track which loosePopLog entries across active savepoints share
   the same buf pointer; only the outermost's Restore actually
   pool-Puts. More complex; matches the M-1 fix sketch in the
   Round-2 review.
3. **Re-design loosePopLog so inner Release migrates the entry
   to outer's log.** When inner B Releases, walk B's loosePopLog
   and re-append each (id, buf, wasPreWindow=…) to outer A's
   loosePopLog (or merge: drop duplicates by id). Inner's
   Restore drops B's entries normally.

User-decided. Option 1 (panic guard + spec amend) is the
narrowest correct fix per the spec-framing reading; options 2 / 3
keep nested shallows in-spec but add nontrivial bookkeeping.

## Notes

Surfaced by the Round-2 adversarial review of
`shallow-savepoint-clone-cost`'s close-out. Diff arbiter: cause-line
is pre-existing (`15f9b70` baseline); classified adjacent — filed-
and-proceed per chunk-close adjacent-issue contract. Not a
correctness defect in production today; a latent foot-gun the next
time nested-shallow becomes reachable.
