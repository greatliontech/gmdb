# Byte-API covering indexes store covering bytes but `Lookup`/`Get` never return them

**Lands:** when byte-API covering-return is wired for arbitrary covering
projections, OR when `indexing.md §Covering Indexes` is scoped to match
the implemented shape (typed full-row covering only) — whichever the
maintainer chooses (see Resolution options).

## Problem

`indexing.md §Covering Indexes` states unconditionally:

> When `Covering` is non-empty, the index entry value carries the
> covering columns ... `Lookup` returns covering bytes directly,
> skipping the back-lookup to the row keyspace.

The byte layer **stores** covering bytes when an `IndexDecl` declares
`Covering` (`index_maintain.go` `indexEntryValue`), but the read path
(`index.go` `extractPKAndValue`) only **returns** them when
`idx.coverValue` is set — and that flag is enabled **only** by the typed
layer (`TypedKS.Index`) for an index it recognizes as full-row covering
(covering column named `gmdb/cover-value/<valEnc.ID()>`). A byte-API
caller declaring `IndexDecl{Covering: [...]}` with arbitrary projection
columns gets the covering blob stored but `Lookup`/`Get` still
back-look-up and return the **whole row value**, not the covering
projection.

This is the original chunk-7.7 deferral (`index.go` comment: "chunk 7.7
always back-lookups for Lookup's value return"). Chunk 9.6b **narrowed**
the gap — typed full-row covering (`TypedIndex.CoverValue`) now returns
covering — but left the byte-API projection-covering promise unmet.

## Severity / scope

Pre-existing (adjacent to chunk 9.6b, not introduced by it — the
back-lookup-always cause-line reproduces on the chunk-7.7 base). Not a
correctness *defect* in the stored data (the covering bytes are stored
correctly and consistently); it is an unfulfilled API/spec promise: a
byte-API caller expecting `Lookup` to skip the back-lookup and return
projections instead gets the row value via back-lookup. Results are
correct for the common (no-covering / typed-full-row) cases.

## Resolution options (maintainer decides — spec-amend candidate)

1. **Scope the spec to match the code (smaller).** Amend
   `indexing.md §Covering Indexes` to state that covering-**return** is
   currently the typed full-row optimization (`TypedIndex.CoverValue`);
   byte-API `Covering` columns are *stored* (queryable later / via
   `Check`) but `Lookup`/`Get` back-look-up the row value. This closes
   the gap by documentation and matches the shipped behavior.
2. **Wire byte-API covering-return (larger).** Make `extractPKAndValue`
   return the covering tuple (decoded projection columns) for any
   covering index, with a defined byte-level return contract (the caller
   decodes the NUL-escape tuple). This changes byte `Lookup`'s returned
   "value" semantics for covering indexes (currently the row value) and
   must not break existing byte covering tests
   (`TestIndexedPutWritesCoveringBytes`), which assert the stored bytes,
   not the Lookup return.

## Notes

Surfaced by the chunk-9.6b Round-1 adversarial review (M-1 +
spec-amend candidate). The typed-layer covering-return is fully wired
and tested (chunk 9.6b); this issue is strictly about the byte-API
projection-covering path. `typed-keyspaces.md §Covering` cross-
references this issue so its "use the byte-oriented IndexDecl for
projection covering" note does not over-promise.
