# SetKeyspace.BulkLoad of an oversize set key surfaces the internal errBulkEntryTooLarge, not ErrKeyTooLarge

**Lands:** proactive — adjacent defect found while wiring the public
`ErrKeyTooLarge` sentinel.

**Severity:** [L]

**Source:** discovered while wiring the public `ErrKeyTooLarge` sentinel (this session) —
a `SetKeyspace.BulkLoad` test case with a >page set key surfaced a
different error than the Keyspace path.

**Governing spec / code:** `bulkload.go:13-19` (`errBulkEntryTooLarge`
+ its "never a reachable in-spec input" comment); `api-surface.md`
§Sentinel errors (`ErrKeyTooLarge`).

## Problem

The `Keyspace` bulk path pre-checks each key via `bulkLeafEntry`, which
returns `btree.ErrKeyTooLarge` for a key too large even for an
overflow-reference leaf entry; `mapBtreeErr` now translates that to the
public `gmdb.ErrKeyTooLarge`. The `SetKeyspace` bulk path (the `setBulk`
bottom-up builder) does **not** pre-check the outer set key — an oversize
set key reaches the builder, which fails its single-entry-fits-empty-leaf
guard with `errBulkEntryTooLarge`:

    gmdb: bulkload entry too large for an empty leaf page (key 8000 bytes)

Two problems:

1. `errBulkEntryTooLarge`'s own godoc claims it is "a defensive guard
   against a caller bug, **never a reachable in-spec input**." That is
   false: an oversize set key reaches it (the SetKeyspace layer bounds
   *value* size at the declaration layer, but not the set *key*).
2. The condition is the documented `ErrKeyTooLarge` case, but a caller
   cannot detect it via `errors.Is(err, gmdb.ErrKeyTooLarge)` on the
   `SetKeyspace.BulkLoad` path — the same detectability gap
   the `ErrKeyTooLarge` sentinel wiring fixed for the Keyspace / Put
   paths, on a different path with a different (internal) error.

A regression test was deliberately NOT added for this case while landing
the `ErrKeyTooLarge` sentinel wiring (it is out of that work's `btree.ErrKeyTooLarge`
scope); see `error_keytoolarge_test.go`'s NOTE.

## Fix

Pre-check the set key in the `SetKeyspace` bulk path (mirroring
`bulkLeafEntry`) and return `btree.ErrKeyTooLarge` — translated to
`gmdb.ErrKeyTooLarge` at the `BulkLoad` boundary — **or** make
`errBulkEntryTooLarge`'s key-too-large sub-case wrap `ErrKeyTooLarge`.
Either way correct the "never reachable" comment, and add a
`SetKeyspace.BulkLoad` oversize-key regression asserting
`errors.Is(err, gmdb.ErrKeyTooLarge)`.
