# Indexed bulkload bypasses online-path gates: oversize keys persist, covering values rejected

Lands: 18

## Findings

**[H] Bulk index build accepts index keys the online path rejects
(missing split-safety gate).** `bulkload_indexed.go:554` and `:592`
feed index entries to the bulk builder with no
`KeyFitsBranchSeparators` gate; every online insert enforces the
limits.md two-separators-per-branch bound (`internal/btree/put.go:187`);
the row path gates via `bulkLeafEntry` (`bulkload.go:798`). Only
backstop is the empty-leaf guard (~leaf capacity, far above the spec
bound). Failure: extractor produces a 3000-byte column at PageSize
4096; `Put` correctly fails ErrKeyTooLarge, `BulkLoad` succeeds. On the
committed database: (a) any later update of that row fails forever
(maintenance re-inserts via the gated path); (b)
`CopyTo(compact=true)`/`Compact()` fail permanently
(`rebuildRegistry → rebuildKVTree → bulkLeafEntry` rejects the key) —
the database can never be compacted; (c) `RebuildIndex` fails the same
way. Direct violation of bulkload.md §API ("an extractor-produced index
key … surface ErrKeyTooLarge, exactly as the same input would through
Put").

**[M] Bulk index build never overflow-promotes index values: BulkLoad
rejects data Put stores.** Same sites: online maintenance stores via
`btree.Put` (`index_engine.go:291,309`), which overflow-promotes; the
bulk build passes a plain inline LeafEntry and surfaces the empty-leaf
guard as a misleading ErrKeyTooLarge. A covering index over a 5000-byte
row value at PageSize 4096 works per-Put and aborts under BulkLoad —
the recommended migration path fails on data the per-op path accepts.

**[L] Bulk-built index trees inherit the keyspace's
`RestartGroupTarget` instead of the base config.**
`bulkload_indexed.go:714/742, 767/802` pass `ks.builderCfg()`; online
maintenance builds index trees with the base `tx.pgr.Config()`
(`index_engine.go:331,348`), the convention copyCompact documents
(`copy.go:359-365`). Reads work (type-dispatched) but contradicts
bulkload.md's "encoded exactly as the per-Put maintenance path".

## Fix direction

Route bulk index entries through the same gate + overflow-promotion
shape as `bulkLeafEntry` (or share it), and build index trees with the
base config. Spec-amend rider: bulkload.md's sentinel paragraph covers
oversize keys but not extractor-produced index *values*; state the
overflow-promotion parity (surfaced in the audit spec-amend list).
Regression: oversize index key rejected at the spec bound; covering
large-value indexed bulkload round-trips; bulk-vs-online index tree
config parity.

## Provenance

2026-07-10 defect audit; bulkload reviewer. Existing large-key tests
hit the empty-leaf guard, not the spec bound; no covering/large-value
indexed-bulkload cases.
