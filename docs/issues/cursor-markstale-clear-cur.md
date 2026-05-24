# `Cursor.MarkStale` does not clear `curKey` / `curValue`

Lands: chunk 5.5 — folded by the chunk-5.1 triage gate when the
keyspace integration first wires `MarkStale` call-sites.

## Symptom

`internal/btree.Cursor.MarkStale` bumps `c.gen` but leaves `c.curKey`
and `c.curValue` aliasing leaf-page-buffer (for restart entries and
uncompressed leaves) or iter `keyBuf` (for compressed deltas) slices.
At chunk-4.6δ this is observationally inert — the cursor's gen-check
in `Current` / `Next` / `Prev` / `Delete` short-circuits BEFORE
reading the cur* slots, returning `(nil, nil)` + `ErrCursorStale`.

At chunk-5 wiring, `MarkStale` will be called by a sibling mutator
(e.g. `keyspace.Delete` from outside this cursor, or a sibling
`Cursor.Delete` on the same keyspace) AFTER it has actually freed or
CoW'd the leaf pages the cursor's `curKey` / `curValue` alias. A
caller that ignores the stale signal (e.g. reads `Current` after
MarkStale via a future code path that skips the gen check, or
accesses `c.curKey` directly through some debug accessor) reads
garbage.

## Mechanism (reachable in-spec path at chunk-5)

1. Cursor A positions at entry K in leaf L. `A.curKey` / `A.curValue`
   alias L's page buffer.
2. Cursor B (sibling on the same keyspace) deletes a different entry
   that triggers a merge with L → L's page is CoW'd then `FreePage`d.
3. Keyspace.Delete calls `A.MarkStale()` (chunk-5 wiring).
4. Within the same write tx the freed leaf L stays addressable
   (loose-page semantics), but at tx commit L's slab buffer is
   released to the pool. After commit, A's curKey / curValue point
   at recycled memory.

The cursor's contract on a stale cursor IS "you can't read me" — the
gen check enforces it on the standard read path. But the cur* slots
still hold the dangling references, and any future code path that
bypasses the gen check (e.g. a profiling hook, a debug API, or a
chunk-5 introspection accessor) reads garbage.

## Resolution

`MarkStale` should nil out `c.curKey` and `c.curValue` so the cursor
holds no references to potentially-freed buffers:

```go
func (c *Cursor) MarkStale() {
    c.gen++
    c.curKey, c.curValue = nil, nil
}
```

Trade-off: a caller that reads `Current()` between MarkStale and a
re-positioning op would have observed the (gen-check-shortcircuited)
`(nil, nil)` already; clearing the slots changes the
`gen-check-bypass` failure mode from "garbage bytes" to "nil
dereference," which is the right direction.

The chunk-5 issue is to (a) decide when `MarkStale` is called by the
keyspace wiring, (b) decide what to do about the dangling `iter`
that may also reference freed slab pages (likely: nil out via
`c.iter = page.LeafIter{}` or call `c.resetPath()`), and (c) verify
no debug / introspection surface reads cur* on a stale cursor.

## Regression test (to land with the chunk-5 wiring)

```go
func TestCursorMarkStaleClearsCurSlots(t *testing.T) {
    // After MarkStale, curKey/curValue must be nil so a caller
    // that bypasses the gen check sees nil rather than dangling.
    cfg := page.Config{PageSize: 4096}
    root, pw := buildTree(t, cfg, [][2]string{{"a", "A"}, {"b", "B"}})
    c := NewCursor(pw, cfg, root, DefaultMergeThreshold)
    c.Seek([]byte("a"))
    c.MarkStale()
    // Expose cur* via test-internal access or via a sentinel read.
    if c.curKey != nil || c.curValue != nil {
        t.Errorf("post-MarkStale cur* = (%q, %q); want (nil, nil)",
            c.curKey, c.curValue)
    }
}
```

## Detection signature

If chunk-5 wiring forgets `MarkStale` clear, the symptom is a
stale-cursor read returning garbage bytes after a sibling cursor's
Delete triggers tx-commit slab release. Asan / race detector won't
catch — slab pool reuse is well-defined Go memory.
