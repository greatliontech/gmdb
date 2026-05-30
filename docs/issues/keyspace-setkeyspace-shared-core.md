# `Keyspace` / `SetKeyspace` duplicate infrastructure — no shared core

**Lands:** condition — proactive burn-down; pull when next touching the
keyspace data path. Internal refactor, no external blocker, not
breaking.

## Problem

`Keyspace` (`keyspace.go:60`) and `SetKeyspace` (`set_keyspace.go:50`)
are built as two fully-independent parallel hierarchies: neither embeds
the other and there is no shared base struct (`grep
keyspaceCore|baseKeyspace` → empty). The two structs share 7 of 8
fields (`tx, name, desc, state, dead, openIndexHandles, indexes,
readOnly`), differing only in `openCursors []*Cursor` vs
`openSetCursors []*SetCursor`. The result is large-scale verbatim
duplication of *infrastructure* (not data-path logic):

**Byte-identical helper methods** (diffed — only the receiver token
differs):

- `markIndexHandlesStale` + `markIndexHandleStaleByName` +
  `markIndexHandleDead` — `keyspace.go:1121` vs `set_keyspace.go:185`
  (~60 lines). These operate solely on `openIndexHandles`, a field both
  carry — zero reason for two copies.
- `builderCfg` — `keyspace.go:587` vs `set_keyspace.go:117`.
- `markDirty` — `keyspace.go:848` vs `set_keyspace.go:128`.
- `descriptor` — `keyspace.go:860` vs `set_keyspace.go:139`.

**Duplicated boilerplate around legitimately-different cores:**

- The write-guard preamble (`requireOpen(true)` + `dead` + `readOnly` +
  empty-key check) appears ~12× across the two files with no extracted
  helper.
- The cursor-construction block (`if tx.writable { btree.NewCursor }
  else { btree.NewReadCursor }` + the `if !dead { append }`
  registration) is duplicated 4×: `keyspace.go:1055`,
  `keyspace.go:1028`, `set_cursor.go:130`, `set_cursor.go:115`.
- The `DeleteRange` and `BulkLoad` preambles + un-indexed walker tails
  are near-verbatim — `keyspace.go:914` vs `set_keyspace.go:1291`
  (the set-side code self-identifies: "Mirrors Keyspace.DeleteRange").

The data-path cores (`Put` single-value vs set-member-add, the indexed
fallbacks) legitimately differ and should stay separate.

## Resolution

Introduce an embedded `keyspaceCore` carrying the 7 shared fields plus
the byte-identical helpers (`builderCfg` / `markDirty` / `descriptor` /
`markIndexHandle*`), and extract a `newKSCursor(...)` factory and a
write-guard preamble helper. This removes ~120–150 duplicated lines.

The repo already proves the pattern: `Index` (`index.go:47`) is a
single struct with a nil-discriminated union (`ks *Keyspace; sks
*SetKeyspace`) that dispatches internally — the same composition the
keyspace types should adopt.

(Upstream design question for the regroup, not this issue: whether set
keyspaces warrant a fully parallel *type tree* — `SetKeyspace` /
`SetCursor` / `TypedSetKeyspace` / `TypedSetKS` / `TypedSetCursor` — or
should be a mode of the single-value types. That decision dwarfs this
mechanical DRY pass.)

## Notes

Surfaced during the 2026-05-30 architecture/factoring audit
(root-package composition pass). Identical-helper claim verified by
`diff` after receiver-token normalization.
