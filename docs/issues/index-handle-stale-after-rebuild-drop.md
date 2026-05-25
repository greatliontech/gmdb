# `*Index` iterators / cursors not invalidated by same-tx RebuildIndex / DropIndex

**Lands:** when concurrent-iteration safety hardening lands
(likely chunk 11 Check + repair, or earlier if a Check pass
surfaces stale-page reads from this gap). Originally tracked
for chunk 7.10 alongside SetKeyspace RebuildIndex / DropIndex;
chunk-7.10 redeferred per Round-1 H-1 triage because the full
markIndexHandlesStale wiring (Keyspace + SetKeyspace + Index
handle + iterator-side cursor tracking) is its own substantial
work bundle, not in scope of chunk 7.10's plan-doc roster.

## Problem

Chunk-7.8's `Tx.RebuildIndex` mutates `pinnedIndex.root` /
`.count` / `.schemaHash` / `.decl` in place and FreeSubtrees the
old data tree. Chunk-7.8's `Tx.DropIndex` deletes the pinned
entry and FreeSubtrees the data tree. Neither operation
invalidates outstanding `*Index` query iterators or the
underlying `btree.Cursor` instances they hold.

A user sequence:

```go
idx, _ := ks.Index("by_color")
for k, v := range idx.Lookup([]byte{0x42}) {
    if needsRebuild(k) {
        tx.RebuildIndex("ks", newDecl)  // FreeSubtree(old root) + p.root=new
    }
    use(k, v)
}
```

leaves the iterator's `btree.Cursor` referencing the OLD index
tree's pages. Same-tx `AllocPage` calls during RebuildIndex's
build phase may have reallocated those pages as new index
content. The subsequent `cursor.Next()` reads stale or
reallocated bytes — wrong-key reads or, worst case, a panic if
the page layout decodes unexpectedly.

This is the same defect class as `*Cursor` invalidation on
sibling mutations (chunk-5.6 markCursorsStale), but for index
iterators. Chunk-7.6's atomic Put/Delete updates `pinnedIndex.root`
in place too, so the underlying issue is pre-existing — chunk
7.8's bulk-free amplification just makes the reachability much
higher (a chunk-7.6 Put may CoW some leaf pages; a chunk-7.8
RebuildIndex FreeSubtree's a whole tree).

## Acceptance

Mirror the chunk-5.6 `*Keyspace.openCursors` + `markCursorsStale`
pattern for `*Index`:

1. `*Keyspace` / `*SetKeyspace` tracks an `openIndexHandles
   []*Index` slice (per-keyspace).
2. `Keyspace.Index(name)` / `SetKeyspace.Index(name)` appends
   the returned `*Index` to the slice.
3. Each `*Index` carries an `openCursors` slice of the
   `btree.Cursor` instances it has handed out via iteratePrefix /
   Range / LookupKeys.
4. `Tx.RebuildIndex` and `Tx.DropIndex` walk the cached
   keyspace's `openIndexHandles`, find handles for the affected
   index, and invoke `MarkStale` on each handle's cursors. The
   handle itself transitions to a "stale" state — subsequent
   Lookup / Range / Prefix / Get calls return ErrCursorStale (or
   a new ErrIndexHandleStale sentinel — user choice).

The atomic-Put/Delete path in chunk 7.6 should mark its own
keyspace's `*Index` handles stale on every successful row
mutation (similar to `markCursorsStale`). This closes the
pre-existing gap chunk 7.6 left open.

Land alongside chunk 7.10's SetKeyspace RebuildIndex /
DropIndex so the invalidation contract is uniform across both
kinds.

When this issue closes, the load-bearing rationale moves inline
into the `*Index` godoc + the `markIndexHandlesStale` helper
godoc, and this file is deleted per the no-cite invariant.

## Notes

Surfaced by the chunk-7.8 Round-1 adversarial review (M-1). The
chunk-7.8 close-out gate-3 check at chunk 7.10 will fold this
back into the implementation plan.

Adjacent: chunk-5.6 `markCursorsStale` (the structural template);
chunk-6.7 `markSetCursorsStale` (the Kind=1 analog already
existing for outer cursors on SetKeyspaces).
