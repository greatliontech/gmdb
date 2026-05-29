# `*Index` iter methods don't observe `tx.DeleteKeyspace` invalidation

**Lands:** opportunistically — the resolution is a single-line guard
at every `*Index` entry method (`if idx.ks.dead || idx.sks.dead { … }`
returning `ErrKeyspaceClosed`), plus a 4–6 test additions. Mirror the
existing `Stats`/`Lookup`/`LookupKeys`/`Range`/`Prefix`/`Get` dead-
handle guards added by the index-handle-stale-after-rebuild-drop fix
(`docs/specs/indexing.md §Handle Invalidation`).

## Problem

`docs/specs/transactions.md §Cursor invalidation by DeleteKeyspace`
specifies:

> Calling `tx.DeleteKeyspace(name)` invalidates every cursor and
> Index handle previously opened on that keyspace within the same
> transaction. Subsequent use of an invalidated cursor or Index
> returns `ErrKeyspaceClosed`.

The implementation enforces this on row cursors (`Cursor`,
`SetCursor`) via `ks.dead` / `sks.dead` checks at every method's
entry. The `*Index` surface does not: `Lookup` / `LookupKeys` /
`Range` / `Prefix` / `Get` / `Stats` all check `idx.dead` (set by
`Tx.DropIndex` per `Inv-IHS2`) but NOT `idx.ks.dead` (set by
`Tx.DeleteKeyspace`).

After `tx.DeleteKeyspace(ks.Name())`:

- `retireIndexRegistry` walks every index entry's `Root` and
  `FreeSubtree`s the data tree (`index_rebuild.go:retireIndexRegistry`,
  via `Keyspace.DeleteKeyspace`).
- A cached `*Index` handle still carries `idx.pinned` and
  `idx.pinned.root` is the now-freed root.
- A subsequent `idx.Lookup(...)` opens a cursor on freed pages —
  same failure mode as `Inv-IHS1`'s mid-iter RebuildIndex case:
  wrong-key reads or layout-decode panic.

This is a **pre-existing adjacent gap** to the index-handle-stale-
after-rebuild-drop fix (the close-out commit). The two are the
same fault class (in-flight `*Index` reads freed pages) but
different trigger cause-lines, so the close-out fix was correctly
scoped to its demonstrated faults and this case was filed-and-
proceed per the chunk-close adjacent-issue contract.

## Demonstrated fault

```go
db, _ := Open(ctx, path, Options{...})
tx, _ := db.Begin(ctx, true)
decl := &IndexDecl{Name: "by_color", Columns: []IndexColumn{{Name: "color"}},
    Extract: firstByteExtract}
ks, _ := tx.CreateKeyspace("items", decl)
ks.Put([]byte("a"), []byte{0x42})
idx, _ := ks.Index("by_color")
tx.DeleteKeyspace("items")           // ks.dead = true; index data tree freed
stats, err := idx.Stats()             // currently: returns stale Count, nil err
                                       // spec: should return ErrKeyspaceClosed
```

## Acceptance

1. Add `idx.ks.dead` / `idx.sks.dead` checks at every `*Index`
   entry method (mirroring the existing `idx.dead` checks added by
   the index-handle-stale-after-rebuild-drop fix). Return
   `ErrKeyspaceClosed` (per the existing spec) — distinct sentinel
   from `ErrIndexNotFound` (which means "the index doesn't exist on
   this keyspace") and `ErrCursorStale` (which means "in-flight
   iter cursor was MarkStaled by a mutation").

2. Wire `ks.markIndexHandlesStale()` (or a dedicated mark-all-dead
   variant) into `Keyspace.DeleteKeyspace` and `SetKeyspace.Delete`
   `Keyspace` so any in-flight iter cursor is also `MarkStale`'d.
   The existing markCursorsStale path on row cursors should already
   stale row cursors via `Keyspace.dead` checks; verify the index-
   handle path is symmetric.

3. Add regression tests mirroring
   `TestIndexHandleStatsAfterDropReturnsErrIndexNotFound` et al.
   in `index_handle_stale_test.go`, asserting `ErrKeyspaceClosed`
   on `Stats` / `Lookup` / `LookupKeys` / `Range` / `Prefix` / `Get`
   after `Tx.DeleteKeyspace`.

## Notes

Surfaced by the index-handle-stale-after-rebuild-drop close-out
review (Round 1 — adjacent gap flagged in the change set
description, deferred per the escalation rule because it is a
different cause-line from the three demonstrated faults the fix
addresses). Pre-existing on HEAD before this commit; not
introduced. Severity M (spec violation with reachable panic /
wrong-yield mode, but only via a non-standard usage pattern —
caching an `*Index` handle across a DeleteKeyspace of its parent).
