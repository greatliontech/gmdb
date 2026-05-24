# Put same-key replace of an overflow entry orphans the overflow chain

Lands: 4.7

## Symptom

`btree.Put(pw, cfg, root, K, V_inline)` where `K` already exists as an
overflow-flagged leaf entry (`Flags & CellFlagOverflow != 0`,
`OverflowPage = P`, `TotalLen = L`) silently drops the
`OverflowPage` / `TotalLen` fields when `insertOrReplaceLeaf`
overwrites the slot:

```go
// internal/btree/put.go (insertOrReplaceLeaf replace branch)
entries[mid] = page.LeafEntry{Key: key, Value: value}
```

The chain rooted at `P` stays allocated in the bitmap but is no longer
reachable from any keyspace — the freespace allocator never reclaims
it.

## Mechanism (reachable in-spec path)

1. Chunk 4.7 lands overflow-value support: `Put` with `len(value) >`
   leaf capacity calls `pager.AllocOverflow` and writes an overflow
   chain, then inserts an overflow-flagged leaf entry pointing at the
   chain's first page.
2. A later `Put` with the same key and a small inline value reaches
   `insertOrReplaceLeaf`, finds the existing key, and overwrites the
   slot with `page.LeafEntry{Key, Value}` — `Flags / OverflowPage /
   TotalLen` are zero in the new struct.
3. The CoW + rebuild emits a clean inline entry. The pre-existing
   overflow chain pages are now unreferenced but still
   reachable-bit-set in the bitmap.

## Pre-existing fault, surfaced by γ

The same bug existed in the chunk-4.4 `insertOrReplace` over
`page.EncodedEntry`:

```go
// pre-γ
entries[mid] = page.EncodedEntry{Key: key, Value: value}
```

The chunk-4.6γ rename to `LeafEntry` preserved the bug verbatim.
`class=pre-existing adjacent` per the loop's diff-arbiter rule.

## Resolution

The right place to fix is chunk 4.7's overflow-write path, because
the same code that allocates a new chain must also retire the
previous chain on same-key replace. Two viable shapes:

1. **Replace-detection in Put.** Before `insertOrReplaceLeaf`,
   `SearchLeaf` for `key`; if found and the existing entry is
   `IsOverflow()`, retire its chain via `pager.FreeOverflowRun(P)`
   before the slot rewrite.
2. **Replace returns the old entry.** `insertOrReplaceLeaf` returns
   the displaced `LeafEntry` (zero-valued on insert). Caller
   inspects `displaced.IsOverflow()` and frees the chain.

Choice tracks chunk 4.7's broader overflow lifecycle design.

## Regression test (to land with the fix)

```go
func TestPutSameKeyOverflowReplaceFreesChain(t *testing.T) {
    // Put a value > leaf capacity → creates overflow chain.
    // Put a small value under the same key.
    // Assert every overflow chain page is freed.
}
```

Place under `internal/btree/put_test.go` after the chunk-4.7 overflow
test fixtures land.

## Detection signature

Bitmap drift between "allocated" and "reachable from any keyspace
root" after a Put-replace-overflow sequence — the regression catches
itself in `checkSlabPartition` if extended over an overflow workload.
