# SetKeyspace.Put on nested-tree cells pays a redundant membership probe

**Lands:** opportunistic — when `btree.Put` gets a "report existing"
variant, OR when chunk-6 close-out audits the chunk-6.1 "no-cost API
enhancement" claim.

## Problem

Chunk-6.1 user-locked the `SetKeyspace.Put(key, value) (added bool,
err error)` signature with the rationale that "the membership probe
is already paid by the insert path … so surfacing the bool is a
no-cost API enhancement that collapses the Put + HasValue retry
pattern without a TOCTOU window."

The current implementation pays the probe TWICE on the nested-tree
cell path:

- `set_keyspace.go:putIntoNestedTree` (chunk 6.6) calls
  `btree.Has(NestedRoot, value)` to check for an existing value,
  then calls `btree.Put(NestedRoot, value, nil)` — each a full
  tree descent.

For subpage cells the probe is genuinely no-cost: `SubpageReader.
Insert` returns the `(newBuf, added)` tuple in one O(N)-or-O(log N)
scan.

The chunk-5 `Keyspace.Put` has the same shape
(`keyspace.go:498-505`) — Has-then-Put pays two descents — so this
is a project-wide pattern, not a SetKeyspace-specific regression.
The chunk-6.1 "no-cost API enhancement" wording was aspirational
for the cell path the codec already paid; the nested-tree path
needs a corresponding btree-layer change.

## Proposed remediation

Add a `btree.PutReportExisting(pw, cfg, rootID, key, value) (newRoot
uint64, existed bool, err error)` variant that performs the
insert/replace in a single descent and reports whether the key
existed. `Put` becomes a thin wrapper that discards `existed`.

`SetKeyspace.putIntoNestedTree` then becomes:

```go
newRoot, existed, err := btree.PutReportExisting(pw, cfg, e.NestedRoot, value, nil)
if err != nil { ... }
if existed {
    return false, nil  // no NestedCount change, no parent-cell rewrite
}
// ... update parent cell as today
```

Same shape fits `Keyspace.Put` for the chunk-5 redundant probe.

## Acceptance criteria

A decision is recorded that either:

1. Accepts the redundant probe as a known cost (chunk-6.1's claim is
   amended to acknowledge nested-tree-cell Put is O(2·descent)), OR
2. Adds the btree.PutReportExisting variant and rewires both
   Keyspace.Put and SetKeyspace.putIntoNestedTree.

## Notes

Surfaced by the chunk-6.6 Round-1 adversarial review (M-2). Not a
defect today — the redundant probe is correct, just slower. Filed
so chunk-6.1's load-bearing rationale doesn't quietly rot if/when a
performance-sensitive caller benchmarks SetKeyspace.Put on a
heavily-nested-tree workload.
