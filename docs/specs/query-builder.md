# Query Builder

A typed query surface over keyspaces with `ColumnIndex`
declarations (`typed-columns.md`): structured predicates, index
selection, ordered and ranked result composition, and typed
projections. Planning and execution live in their own package
(`gmdb/query`), consuming exported gmdb and `gmdb/typed`
surfaces — keyspace handles, index handles, cursors — plus one
shared internal representation package (the predicate and
projection seam; term and projection internals stay unexported
in the public surface). The inert
declaration-tier value types shared with the column
declarations — `Term`, `OrderKey`, `Projection` — live in the
typed package (`gmdb/typed`; their constructors are `Column`
methods — definable only in the receiver type's package — plus
the free `Or` constructor, which builds `Term` values and shares
the home). The ENGINE contract —
transactions, keyspaces, index maintenance — does not change;
what this spec requires of the byte tier is exactly
§Byte-surface requirements.

Design position: the builder's structured predicate
representation is the stable seam for any future textual query
front-end — a front-end compiles into terms; nothing below the
term layer knows the front-end exists. Ranked retrieval (full
text, vector) enters as a leaf node kind (`§Ranked sources`)
composed by the same ordering rules as everything else.

Scope:
- Terms and the predicate representation (sargable tier,
  disjunction, opaque residual tier).
- Query surface: `Where`, `Or`, `Filter`, `Select`, `OrderBy`,
  `Limit`, result iteration, `Err`, `Explain`.
- Plan node taxonomy and the ordering property.
- Planning rules (normative): index selection, coverage scoring,
  residual splitting, union/intersection.
- Residual evaluation via encoded-byte comparison.
- Covering-aware execution and typed projections.
- Multi-valued dedup, determinism, materialization bounds.
- Ranked-source leaf contract (reserved for FTS / vector).
- The plan/scan equivalence testing contract.

Depends on / interacts with:
- `typed-columns.md` for column declarations, encoders, covering
  projections, and `Column.From`.
- `indexing.md` for the index handle query surface, covering
  return contract, and handle invalidation (the builder's
  iterators inherit `ErrCursorStale` / `ErrTxClosed` semantics
  unchanged).
- `typed-keyspaces.md` for `typed.KeyspaceHandle` iteration used
  by scan plans.
- `transactions.md` — a query executes within one transaction
  and sees its dirty state, exactly as the surfaces it composes.

## Invariants

Invariants without an enforcement pointer are spec-tier and carry
`Lands: when code able to violate it is first written.` Landed
slices are annotated per invariant; an invariant whose surface is
only partially built names the enforced slice.

Invariant: kind=clause-explicit;
  property=Inv-QB1 (plan/scan equivalence): for every keyspace
    state, declaration set, and query, the result sequence
    produced by the chosen plan equals the reference semantics —
    full scan of the keyspace, residual evaluation of every
    term and filter, distinct-by-PK, ordering by the `OrderBy`
    keys with the Inv-QB5 tie-break (when an `OrderBy` is
    present), offset/limit applied last. The comparison is
    ordering-sensitive exactly as far as the query orders:
    under an `OrderBy`, the result SEQUENCE equals the
    reference; without one, the result SET equals the reference
    matched set (order is plan-defined, §Result semantics);
    with `Limit`/`Offset` but no `OrderBy`, the result is a
    subset of the matched set with cardinality
    max(0, min(limit, matched − offset)), where an unset
    `Limit` reads as ∞. Plan choice is never observable in
    results, only in cost;
  from=this spec §Planning rules;
  violation=Any planner bug that consumes a term it did not
    fully push (e.g. treating a range term as an EQ prefix)
    returns rows a scan would exclude, or excludes rows a scan
    would return — silently, since no error surface fires.
    Enforced by `TestQueryPlanScanEquivalence` (set, ordered, and
    limit/offset cardinality regimes; the ordered arm compares
    SEQUENCES against an independent reference sort) +
    `TestQueryLimitOffsetCardinality` +
    `TestQueryRangeBoundAnchors` + the cross-plan ordered anchors
    in `TestQueryOrderByStreams` /
    `TestQueryOrderByEqualKeyLimitBoundary`.

Invariant: kind=clause-explicit;
  property=Inv-QB2 (encoded comparison): sargable terms evaluate
    — whether pushed to an index seek or evaluated residually —
    by comparing ENCODED bytes produced by the column's own
    encoder, never by comparing Go values;
  from=this spec §Residual evaluation;
  violation=A column type whose Go comparison diverges from its
    encoding's lex order (float NaN ordering, a custom encoder
    with case folding) returns different rows depending on
    whether the planner chose an index — plan-dependent results,
    the exact failure Inv-QB1 forbids.
    Enforced by `TestQueryEncodedComparisonAnchor` +
    `TestQueryNaNSafeFloatAnchor`.

Invariant: kind=clause-explicit;
  property=Inv-QB3 (covering interpretation): the executor
    interprets index-entry value bytes strictly per the LIVE
    declaration's covering shape — covering tuple
    (`DecodeCoveringTuple`) when covering columns are declared,
    typed full-row when the CoverValue sentinel is present, row
    back-lookup bytes otherwise — mirroring
    `indexing.md §Byte-API return contract`;
  from=this spec §Covering-aware execution;
  violation=Decoding a covering tuple as row bytes (or the
    reverse) yields garbage values or a decode error on every
    covered read — the covering feature becomes unusable through
    the builder despite being correct at the byte layer.
    Enforced by `TestQueryIndexExecFollowsLiveCoveringShape` +
    `TestQueryIndexExecFallsBackOnLiveTupleChange` (live-decl
    routing) and the index-only/cover-value route anchors in
    `TestQueryRowsIndexOnlyFromCovering` /
    `TestQueryRowsIndexOnlyFromKeyColumns` /
    `TestQueryAllCoverValueRoute` /
    `TestQueryRowsCoveringFreshAfterUpdate`.

Invariant: kind=clause-explicit;
  property=Inv-QB4 (distinct-by-PK): a query's result sequence
    yields each primary key at most once, regardless of plan
    shape — multi-column entry expansion, union branches
    matching the same row, or ranked sources are all deduped;
  from=this spec §Result semantics;
  violation=A range term over a `MultiColumn` (one index entry
    per element) yields the same row once per matching element;
    a scan yields it once — plan-dependent duplication.
    Enforced (landed slices: index-leaf expansion and Or-branch
    overlap) by `TestQueryMultiColumnRangeDedup` +
    `TestQueryUnionOverlapDedup` (both Union arms) + the duplicate
    check inside `TestQueryPlanScanEquivalence`; ranked-source
    dedup lands with its node.

Invariant: kind=clause-explicit;
  property=Inv-QB5 (determinism): for a fixed keyspace state
    and query, the result sequence is identical across
    EXECUTIONS — the planner is deterministic and every node
    iterates deterministically. Ordered output (an `OrderBy`,
    or `ByScore`) additionally breaks ties by PK in the
    DIRECTION of the final ordering key — ascending PK under an
    ascending final key, descending under a descending one —
    making the ordered sequence identical across PLAN choices
    too. Without an `OrderBy`, order is plan-defined: stable
    for the query, not canonical across plans;
  from=this spec §Result semantics;
  violation=Two rows with equal sort keys returned in
    plan-dependent order make Inv-QB1's ordered comparison
    untestable by sequence and make `Limit` non-deterministic
    at the boundary. The direction clause is load-bearing: an
    ascending-always rule is unsatisfiable by streaming reverse
    iteration (equal-key runs arrive PK-descending on a
    non-unique index), forcing either an equal-key-run buffer no
    Inv-QB6 node sanctions or plan-dependent sequences.
    Enforced by the repeat check inside
    `TestQueryPlanScanEquivalence` (no-OrderBy regime) and, for
    the directional tie-break, `TestQueryOrderByStreams`
    (descending equal-key runs) +
    `TestQueryOrderByEqualKeyLimitBoundary` (equal-key Limit cuts
    identical across executions and plan choices) + the harness's
    ordered-sequence arm.

Invariant: kind=clause-explicit;
  property=Inv-QB6 (bounded materialization, explicit failure):
    every buffering node (`Sort`, `TopK` — whose heap holds
    O(limit+offset) rows — hash dedup, `Intersect` build side)
    counts its buffer against the query's materialization budget
    when one is set, failing with `ErrQueryMaterializeLimit` —
    never silently truncating, sampling, or capping results;
  from=this spec §Materialization budget;
  violation=A capped-but-successful sort returns a correct-
    looking prefix of the wrong result set — a silent coverage
    cap indistinguishable from a correct small result.
    Enforced by `TestQueryMaterializeBudget` (Sort, TopK, hash
    dedup, and Intersect build all trip
    `ErrQueryMaterializeLimit`; pure streams are unaffected;
    a sufficient budget changes nothing).

Invariant: kind=clause-explicit;
  property=Inv-QB7 (opaque filters see whole rows): a `Filter`
    func is invoked only with the row's decoded `(K, V)` as
    materialized from the row keyspace or a full-row covering
    entry — never with a zero value, a partial projection, or a
    value decoded from a projection covering tuple;
  from=this spec §Residual evaluation;
  violation=An index-only plan that "optimizes" by calling the
    filter with a partially-populated V evaluates user logic on
    fields that silently read as zero — wrong inclusion or
    exclusion with no error.
    Enforced by `TestQueryFilterForcesWholeRows` (a filter query
    is never index-only and observes fields outside the index).

## Terms

Term constructors are methods on column declarations. Each
produces a structured node `(column, op, encoded literal)`;
literals are encoded at term construction via the column's
encoder. A `Term` carries any encode error; a query using such a
term fails at iteration start — before any row work — with that
error via `Err()`.

```go
// Column[K, V, C]
func (c *Column[K, V, C]) Eq(v C) Term[K, V]
func (c *Column[K, V, C]) Lt(v C) Term[K, V]
func (c *Column[K, V, C]) Lte(v C) Term[K, V]
func (c *Column[K, V, C]) Gt(v C) Term[K, V]
func (c *Column[K, V, C]) Gte(v C) Term[K, V]
func (c *Column[K, V, C]) Between(lo, hi C) Term[K, V] // [lo, hi)

// HasPrefix matches rows whose ENCODED column value has the
// term's encoded literal as a byte prefix — defined purely at
// the byte level, so pushdown and residual evaluation agree
// (Inv-QB2) for every encoder. Byte-prefix coincides with the
// natural "string starts with" semantic only for identity-like
// encoders (the canonical gmdb/string and gmdb/bytes); for any
// other encoder the byte-level meaning is what you get.
func (c *Column[K, V, C]) HasPrefix(v C) Term[K, V]

// MultiColumn[K, V, C]: matches rows where ANY element matches.
func (m *MultiColumn[K, V, C]) Contains(v C) Term[K, V]
func (m *MultiColumn[K, V, C]) ContainsRange(lo, hi C) Term[K, V]

// Disjunction: each group is a conjunction of terms; Or matches
// rows matching any group.
func Or[K, V any](groups ...[]Term[K, V]) Term[K, V]
```

The predicate representation is therefore: a top-level
conjunction of terms, where a term is a leaf or an `Or` of
conjunctions. Deeper nesting is legal and always evaluable
(residually, per Inv-QB2); how much of it is pushed down is the
planner's affair (§Planning rules).

## Query surface

```go
func New[K, V any](ks *typed.KeyspaceHandle[K, V]) *Query[K, V]

func (q *Query[K, V]) Where(terms ...Term[K, V]) *Query[K, V]   // ANDed
func (q *Query[K, V]) Filter(f func(K, V) bool) *Query[K, V]    // opaque residual
// Select takes SINGLE-VALUED columns (the same sealed erasure as
// Covering declarations): a multi-valued column has no single
// projection slot and no From surface, so MultiColumn-in-Select
// is unrepresentable rather than a runtime rejection.
func (q *Query[K, V]) Select(cols ...AnySingleColumn[K, V]) *Query[K, V]
func (q *Query[K, V]) OrderBy(keys ...OrderKey[K, V]) *Query[K, V]
func (q *Query[K, V]) Limit(n int) *Query[K, V]
func (q *Query[K, V]) Offset(n int) *Query[K, V]
func (q *Query[K, V]) WithMaterializeLimit(bytes int) *Query[K, V]

func (q *Query[K, V]) All() iter.Seq2[K, V]        // whole rows
func (q *Query[K, V]) Keys() iter.Seq[K]           // PKs only
// Rows serves the Select projection. Calling it on a query with
// no Select fails at iteration start via Err — there is nothing
// to project.
func (q *Query[K, V]) Rows() iter.Seq2[K, Projection]
func (q *Query[K, V]) Count() (uint64, error)
func (q *Query[K, V]) Err() error
func (q *Query[K, V]) Explain() Plan

// OrderKey constructors on columns:
func (c *Column[K, V, C]) Asc() OrderKey[K, V]
func (c *Column[K, V, C]) Desc() OrderKey[K, V]
```

Queries are values bound to a transaction-scoped handle; they
inherit the handle-lifetime contract of the surfaces they compose
(`indexing.md §Handle Invalidation`). `Err()` reports the first
error of the last iteration, matching the house iterator
convention. `Explain()` returns the chosen plan as a value
without executing — the surface plan-pinning tests and operators
use.

## Plan nodes and the ordering property

Every plan node declares an ordering:

```
Ordering := ByColumns(index, prefixLen, dir)  // index lex order
          | ByPK(dir)                          // primary-key order
          | ByKeys(orderKeys)                  // materialized OrderBy order
          | ByScore                            // ranked; always descending
          | Unordered                          // deterministic, not canonical

dir := asc | desc
```

`Sort` and `TopK` emit `ByKeys` of the requested `OrderBy` keys
(each key carries its direction; ties per Inv-QB5).

Leaf orderings are ascending by default; the index leaves flip
to `desc` under reverse iteration (§Byte-surface requirements).
`Scan` is ascending-only — a descending order with no index
route materializes.

Node taxonomy:

- Leaves: `Scan` (row keyspace; ByPK(asc)), `IndexSeek` (every
  declared column consumed by an EQ term — byte `Lookup`/`Get`;
  ordering ByPK(asc), because within one fixed column-key group
  non-unique entries sort by their escaped-PK suffix and a
  unique seek yields at most one row), `IndexPrefix` (a strict
  leading-EQ prefix, no trailing bound — byte `Prefix`; ordering
  ByColumns(index, prefixLen, asc)), `IndexRange` (leading-EQ
  prefix + one trailing bound — byte `Range` partial-tuple
  prefix-bounds, §Byte-surface requirements; ordering
  ByColumns(index, prefixLen, asc)), `RankedSource` (ByScore;
  §Ranked sources).
- Combiners: `Union` (Or branches) and `Intersect` (two indexed
  conjuncts; hash join on PK sets). `Union` dedups by PK:
  STREAMING merge-dedup is sound only when every input's
  ordering is `ByPK` (duplicates then meet at the merge point);
  any other input ordering — including two branches sharing
  `ByColumns` of the same index, where one row can surface under
  two different column keys — takes hash dedup, which counts
  against the materialization budget. Combiner output orderings:
  the merge arm emits `ByPK(dir)` (all inputs share one
  direction); the hash arm emits `Unordered` — branches drain in
  plan order, so the sequence is deterministic without being
  canonical; `Intersect` preserves its probe input's ordering
  (the build side materializes into the budget).
- Transforms: `ResidualFilter`, `Project`, `TopK` (OrderBy +
  Limit; bounded heap holding O(limit + offset) rows), `Sort`
  (OrderBy without Limit; materializing), `Limit`/`Offset`.

Composition is driven purely by the ordering property, never by
node kind: a requested `OrderBy` satisfied by the input's
declared ordering — columns AND direction — streams; otherwise
the planner inserts `TopK` (when a limit bounds it) or `Sort`. This is the seam ranked
sources plug into: a BM25 or vector top-k leaf is just a node
whose ordering is `ByScore`.

Handle discipline: the executor obtains a FRESH byte
`*IndexHandle` per plan leaf (via the `ByteIndex` bridge,
§Byte-surface requirements), so no two concurrently-draining
iterators ever share a handle — per-handle `Err` state makes
overlapping iterators on one handle mutually clobbering
(`indexing.md §Lookup API`), and combiner nodes drain their
inputs interleaved by construction.

## Planning rules

Normative; the planner is rule-based and deterministic, not
cost-based.

1. Flatten the top-level conjunction. Partition leaves into
   sargable terms (on declared columns) and everything else
   (opaque filters; nested disjunctions the rules below do not
   push).
2. For each `ColumnIndex` on the keyspace: match the longest
   prefix of its column sequence satisfiable by EQ terms
   (`Eq` / `Contains`), optionally extended by ONE range-shaped
   term (`Lt`/`Lte`/`Gt`/`Gte`/`Between`/`HasPrefix`/
   `ContainsRange`) on the next column. The consumed shape maps
   to exactly one leaf: all columns EQ ⇒ `IndexSeek`; a shorter
   EQ prefix with no trailing bound ⇒ `IndexPrefix`; an EQ
   prefix (possibly empty) + trailing bound ⇒ `IndexRange`.
   An index containing a `MultiColumn` is a sound access path
   only when EVERY multi column's element existence is entailed
   by the query: the column is consumed by the leaf (`Contains`
   in the EQ prefix, or `ContainsRange` as the trailing bound),
   or a TOP-LEVEL `Contains`/`ContainsRange` term on it
   evaluates residually. A row whose multi accessor returns an
   empty slice has NO entries in such an index
   (`typed-columns.md` Inv-TC4 — an empty column sequence
   empties the Cartesian product); under entailment the omitted
   rows are rows the query rejects anyway, so nothing is lost.
   An `Or`-nested `Contains` does NOT entail existence — a
   disjunct may be false for a row another group matches. A
   match leaving any multi column unentailed excludes rows the
   query may want — rule 7's exclusion class, at element
   granularity — and is skipped during candidate enumeration.
   An entailed-but-unconsumed multi column keeps one entry per
   element, so such leaves dedup by PK (Inv-QB4).
3. Score candidates: (a) most terms consumed; tie-break (b)
   covering — an index whose key + covering columns satisfy
   every column the query touches (terms, Select, OrderBy) wins;
   then (c) unique over non-unique; then (d) index name, for
   determinism.
4. A top-level `Or` plans each group independently by rules 1–3
   and joins with `Union`. A group that cannot be pushed at all
   degrades the whole `Or` to residual evaluation over a wider
   plan (never a partial union — that would violate Inv-QB1).
5. Two conjuncts each consumable only by DIFFERENT indexes may
   plan as `Intersect`; the planner does so only when neither
   index alone consumes both and both seeks are EQ-shaped.
6. Unconsumed sargable terms and all opaque filters become
   `ResidualFilter` nodes. No index fits ⇒ `Scan`.
7. `Where`-partial indexes (a `ColumnIndex` with a non-nil
   `Where`) are NOT planner-eligible: rule 2 skips them during
   candidate enumeration, unconditionally. A partial index's
   entry set excludes rows the query may want, and no sound,
   implementable eligibility test exists (predicate entailment
   is undecidable over opaque funcs, and Go func values are not
   comparable — the position `indexing.md §Open Semantics`
   itself records). Partial indexes remain fully queryable
   through the typed and byte index-handle surfaces directly;
   any future explicit opt-in surface requires its own clause
   in this spec, with its non-equivalence to scan semantics
   stated — it is not an implementation liberty.

## Residual evaluation

A residual sargable term evaluates by encoding the row's column
value (accessor + encoder, exactly as the extractor would) and
comparing bytes against the term's encoded literal (Inv-QB2).
`Contains` terms evaluate over the element set the accessor
returns. Because both the index path and the residual path derive
from the same encoders and accessors, plan/scan equivalence holds
by construction rather than by parallel implementations.

Residual terms on columns carried by the chosen index's key or
covering tuple MAY evaluate against the entry bytes directly
without materializing `V` (a pure read optimization — same
results by Inv-QB2). Opaque `Filter` funcs always force whole-row
materialization (Inv-QB7).

## Covering-aware execution

Value acquisition per plan, in order of preference:

1. `Select` satisfiable from the chosen index's key columns +
   covering columns ⇒ index-only: `Rows()` serves projections
   decoded from entry bytes; the row keyspace is never read.
2. `All()` on a CoverValue index ⇒ values decode from the entry's
   full-row covering bytes; no back-lookup.
3. Otherwise ⇒ back-lookup via the index `Lookup` contract, or
   direct scan.

Interpretation of entry value bytes in every case follows the
live declaration (Inv-QB3). `Projection` rows are decoded lazily
per column via `Column.From` (`typed-columns.md §Covering
projections`); requesting a column the plan does not carry is an
error at `From`, not a zero value.

## Result semantics

- Distinct-by-PK (Inv-QB4). Streaming merge-dedup only over
  all-`ByPK` inputs (§Plan nodes); hash dedup otherwise (counts
  against the materialization budget).
- All ordered output tie-breaks by PK in the final ordering
  key's direction (Inv-QB5).
- Without an `OrderBy`, order is plan-defined — the chosen
  plan's natural stream: deterministic for the query, not
  canonical across plans (Inv-QB5; Inv-QB1 compares sets there).
- `OrderBy` with no pushable streaming order materializes via
  `TopK` (with `Limit`) or `Sort`. `Desc` streams when the
  chosen index provides the ordering (reverse iteration —
  §Byte-surface requirements) and materializes otherwise.
- `Offset`/`Limit` apply to the final deduped, ordered sequence.
- `Count()` returns the cardinality of the query's OWN result —
  exactly `len(All())`, so `Offset`/`Limit` apply: max(0,
  min(limit, matched − offset)), unset `Limit` = ∞ — via the
  cheapest plan that can count it (index-only where possible),
  without materializing values.

## Materialization budget

`WithMaterializeLimit(bytes)` bounds the total bytes buffered by
buffering nodes for one query execution — `Sort`, `TopK` (its
heap holds O(limit + offset) rows and is NOT exempt), hash
dedup, and the `Intersect` build side; one budget spans ALL of an
execution's buffering nodes. The accounting basis is each node's
retained INDEXABLE bytes: encoded sort-key bytes plus PK bytes
per buffered row for `Sort`/`TopK`, PK bytes per entry for hash
sets — the decoded row values a sort node also holds are not
re-encoded just to be measured (accounting must not cost more
than what it accounts). Zero (the default) means unbounded — the
caller owns the tradeoff, as with `Sort` in any embedded engine.
When a budget is set, exceeding it fails the
iteration with `ErrQueryMaterializeLimit` (Inv-QB6). Pure
streams (index-order iteration, streaming merge-dedup) buffer
nothing and are unaffected — the budget exists precisely to
make "this query should never need to buffer" checkable.

## Ranked sources

Lands: with the first `RankedSource` implementation (full-text
or vector search) — this section reserves contract; nothing in
it is implementable standalone.

`RankedSource` is the reserved leaf kind for ranked retrieval:
a source yielding `(pk, score)` in descending score order with
descending-PK tie-break (Inv-QB5: the final — only — ordering
key is descending), each PK at most once. Full-text and vector
search
land as implementations of this contract in their own specs;
this spec owns only the composition rules:

- A `RankedSource` has ordering `ByScore`; combining a ranked
  ordering with `OrderBy` on columns, or fusing multiple ranked
  sources, is specified by the owning feature spec (e.g.
  reciprocal-rank fusion), not here.
- `ResidualFilter` over a ranked source narrows the stream
  order-preservingly. Completeness protocols for filtered ranked
  retrieval (over-fetch and widen) are the owning feature's
  contract.

## Byte-surface requirements

The `IndexSeek` / `IndexPrefix` / `IndexRange` leaves need no
new byte surface: `Lookup`, `Prefix`, and `Range`'s documented
partial-tuple prefix-bounds (`api-surface.md §Index Lookup
API`) already express every consumed shape. Trailing-bound
intervals are constructed at the VALUE level — an EQ-prefix
group `(X)` is closed from above by the value-level successor
`X || 0x00` as the bound column — so the builder never touches
the NUL-escape encoding.

Reverse iteration (streaming `Desc`) is the `Reverse()`
`IterOption` — its normative clause lives in `indexing.md
§Lookup API` / `api-surface.md §Index Lookup API`.

The executor additionally requires a typed→byte bridge:

```go
// ByteIndex returns the byte-oriented handle for an index
// declared on this keyspace — the surface the plan leaves
// iterate (typed.IndexQuery is IK-opaque and cannot serve
// per-column entry bytes).
func (t *KeyspaceHandle[K, V]) ByteIndex(name string) (*gmdb.IndexHandle, error)
```

Its normative clause lives in `typed-keyspaces.md §Typed
Indexes` / `api-surface.md §Index Lookup API`. The general
fallback rule stands regardless: any term the planner cannot
push is evaluated residually (Inv-QB2), never approximated.

## Testing contract

The anchor is a property test of Inv-QB1: a generator produces
schemas (columns, single/multi, unique/partial/covering mixes),
row corpora, and queries (terms over declared and undeclared
columns, Or nesting, filters, orderings, limits); execution
compares the planned results against the reference
scan-and-evaluate semantics — exact SEQUENCE match under an
`OrderBy` (Inv-QB5's directional tie-break makes it valid), SET
match plus a repeat-execution determinism check without one, the
subset-plus-cardinality rule for `Limit`/`Offset` with no
`OrderBy`, and `Count() == len(reference result)` under the same
regime (Inv-QB1). Every node kind with a landed implementation
and every planning rule — including rule 7's partial-index
exclusion, whose grammar arm generates `Where`-partial indexes
and asserts they are never chosen while results stay correct —
must be reachable from the generator grammar; extending the term
surface or node taxonomy extends the grammar in the same change.
Named anchor cases pin: covering staleness after update
(Inv-TC5/Inv-QB3), multi-column range dedup (Inv-QB4), Or-branch
overlap dedup, budget exhaustion (Inv-QB6), and equal-key `Limit`
boundaries (Inv-QB5).
