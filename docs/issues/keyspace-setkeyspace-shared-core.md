# `Keyspace` / `SetKeyspace` duplicate infrastructure — shared core

**Lands:** condition — proactive burn-down; pull when next touching the
keyspace data path. Internal refactor, no external blocker, not
breaking.

## Landed: `keyspaceCore` embed (the byte-identical helpers)

`keyspace_core.go` now defines a `keyspaceCore` struct carrying the 8
fields both handles shared (`tx, name, desc, state, dead,
openIndexHandles, indexes, readOnly`); `Keyspace` and `SetKeyspace`
each embed it and keep only their own cursor slice (`openCursors` /
`openSetCursors`). The 7 byte-identical helpers — `Name`, `builderCfg`,
`markDirty`, `descriptor`, and the `markIndexHandlesStale` /
`markIndexHandleStaleByName` / `markIndexHandleDead` trio — moved to the
core (promoted to both embedders). Net −315 duplicated lines across the
four files. Behavior-preserving: full `-race` suite green + fresh-eyes
adversarial review confirmed byte-identical method bodies, unchanged
public API, and `descriptorOwner` still satisfied via promotion.

## Remaining

The shared *struct + helpers* are done; what's left is the data-path
*boilerplate* duplication around legitimately-different cores:

- The write-guard preamble (`requireOpen(true)` + `dead` + `readOnly` +
  empty-key check) appears ~12× across the two files with no extracted
  helper.
- The cursor-construction block (`if tx.writable { btree.NewCursor }
  else { btree.NewReadCursor }` + the `if !dead { append }`
  registration) is duplicated 4×: `Keyspace.Cursor` /
  `newInternalCursor` (keyspace.go), `SetKeyspace.Cursor` /
  `newInternalSetCursor` (set_cursor.go).
- The `DeleteRange` / `BulkLoad` preambles + un-indexed walker tails are
  near-verbatim between `keyspace.go` and `set_keyspace.go` (the
  set-side code self-identifies: "Mirrors Keyspace.DeleteRange").

The data-path cores (`Put` single-value vs set-member-add, the indexed
fallbacks) legitimately differ and stay separate.

## Resolution (remaining)

Extract a `newKSCursor(...)` factory to kill the 4× cursor-construction
duplication, a shared write-guard preamble helper, and lift the common
`DeleteRange` / `BulkLoad` preamble + un-indexed-walker tail into shared
funcs parameterized by the per-cell free callback.

(Upstream design question, not this issue: whether set keyspaces warrant
a fully parallel *type tree* — `SetKeyspace` / `SetCursor` /
`TypedSetKeyspace` / `TypedSetKS` / `TypedSetCursor` — or should be a
mode of the single-value types. That decision dwarfs this mechanical
DRY pass.)

## Notes

Surfaced during the 2026-05-30 architecture/factoring audit
(root-package composition pass). The `keyspaceCore` embed landed as a
behavior-preserving cut; this issue stays open for the remaining
cursor-factory / preamble extraction.
