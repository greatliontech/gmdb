# Byte-API reverse iteration on index handles

Lands: 9

Spec-amend rider, not a defect: the clause text below merges into
`indexing.md §Lookup API` and `api-surface.md §Index Lookup API`
with the implementing change set (amendments to as-built specs
ride their implementation chunks).

## Motivation

`Lookup` / `LookupKeys` / `Range` / `Prefix` iterate ascending
only; row cursors have `Prev` but index iteration surfaces do
not expose direction. The query builder's streaming
`OrderBy(col.Desc())` plans
(`query-builder.md §Result semantics`) consume this surface;
without it every descending order materializes.

## Clause text to merge

A direction option rather than method doubling:

```go
func (idx *IndexHandle) Range(start, end [][]byte,
    opts ...IterOption) iter.Seq2[[]byte, []byte]
// gmdb.Reverse() IterOption — yields the same entry SET in
// exactly reversed order. Accepted by Lookup / LookupKeys /
// Range / Prefix (LookupKeys included so a descending
// keys-only plan needs neither covering decode nor
// back-lookup).
```

- Reverse yields exactly the reverse of the forward sequence
  over the same snapshot (same-tx dirty state included) — the
  set equality is the invariant that makes reverse plans
  interchangeable with materialized-descending plans.
- Handle-invalidation contract unchanged: a reverse iterator is
  a cursor walk and stales identically (Inv-IHS1..5 apply as
  written).
- Underlying requirement: descending B+tree cursor traversal
  (`Prev`) already exists on row cursors; the amendment is
  surface, not engine capability.

## Provenance

Query-builder design (`typed-columns.md`, `query-builder.md`);
amendment accepted 2026-07-11.
