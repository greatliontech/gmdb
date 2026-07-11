# Typed Columns

Per-field typed column declarations over typed keyspace rows, and
the `ColumnIndex` declaration that compiles them into a
multi-column byte-oriented `IndexDecl`. This is the typed form of
the byte API's multi-column contract: where `TypedIndex[K, V, IK]`
collapses a composite index key into one opaque column (see
`typed-keyspaces.md §Limitation`), a `ColumnIndex` declares one
byte `IndexColumn` per field. Partial-prefix queries, per-column
planning, and typed covering projections compose naturally on top.

Columns are also the declaration surface the query builder plans
against (`query-builder.md`): because a column carries its own
`Encoder[C]`, a literal in a query term can be encoded into index
key bytes by the same encoder the extractor used — the property
that makes index pushdown sound.

Scope:
- `Column[K, V, C]` and `MultiColumn[K, V, C]` declarations.
- `ColumnIndex[K, V]`: options, `Where` predicate, covering
  columns, `AnyTypedIndex` integration.
- Compilation to the byte `IndexDecl`: extractor synthesis,
  Cartesian expansion, empty-slice gating.
- Synthesized column-name grammar (schema-hash folding).
- Typed covering projections and per-column projection decode.
- Relationship to `TypedIndex[K, V, IK]`.

Depends on / interacts with:
- `typed-keyspaces.md` for `Encoder[T]`, the key-ordering
  constraint, encoder-ID immutability, and the sealed
  `AnyTypedIndex` interface this tier implements.
- `indexing.md` for the byte `IndexDecl` contract this tier
  compiles to: drift guard, unique semantics, partial-index
  semantics, covering storage, duplicate collapse.
- `page-formats.md §NUL-escape encoding` for the column
  concatenation this tier inherits unchanged.
- `query-builder.md` consumes this tier; nothing here depends
  on it.

## Invariants

All invariants in this section are spec-tier; each carries
`Lands: when code able to violate it is first written.`

Invariant: kind=clause-explicit;
  property=Inv-TC1: every `Column` / `MultiColumn` encoder MUST
    produce byte sequences whose lex order matches the column
    type's intended order — the same constraint
    `typed-keyspaces.md` places on `Encoder[K]`, applied per
    column;
  from=this spec §Column declaration +
    `typed-keyspaces.md §Key ordering constraint`;
  violation=A non-lex-preserving column encoder makes every
    range term on that column (`Lt`/`Gte`/`Between`) return the
    wrong row set from an index seek while a full scan with
    Go-value comparison would return the right one — silent
    plan-dependent wrong results.

Invariant: kind=clause-explicit;
  property=Inv-TC2: the synthesized byte `IndexColumn.Name` for
    a column is injective over (declaration form, user column
    name, encoder ID), and the KEY-column name domain is
    disjoint from the synthesized-name domain of every OTHER
    TYPED decl form (`TypedIndex`'s encoder-ID-derived synthesis
    included; the raw byte API's caller-chosen names are an open
    namespace outside this guarantee). The one sanctioned shared
    name is the full-row covering sentinel
    `gmdb/cover-value/<valEncID>` — identical across typed forms
    BY DESIGN, so the typed read path recognizes either. Two
    structurally different typed declarations never synthesize
    the same key-column name;
  from=this spec §Synthesized column-name grammar;
  violation=A `Column` and a `MultiColumn` with the same user
    name and encoder ID (or a `TypedIndex` whose synthesized
    name collides with a `ColumnIndex` column's) would hash
    identically; opening a keyspace whose stored index was built
    under one form with a decl of the other form passes the
    drift guard, then the extractor's expansion semantics
    diverge from the stored entries — false-negative lookups the
    fingerprint was designed to prevent.

Invariant: kind=entailed;
  property=Inv-TC3: a column accessor (`get`) is a pure function
    of `(K, V)` — deterministic, no hidden state. Changing an
    accessor's logic so it produces different output for the
    same row requires bumping the owning `ColumnIndex.Version`;
  from=entailed: `indexing.md §Drift Guard` — the `Version` tag
    exists because the engine cannot inspect extractor logic;
    the synthesized extractor's logic IS the accessor set, so
    the accessor inherits the extractor's determinism contract;
  violation=A time- or state-dependent accessor produces
    different entries on rebuild than on live maintenance —
    `Check(CheckIndexes)` reports drift on an index that was
    never structurally changed, and the rebuilt index disagrees
    with the live-maintained one.

Invariant: kind=clause-explicit;
  property=Inv-TC4: the synthesized extractor's output for a row
    equals: if `Where` is non-nil and `Where(k, v)` is false,
    the empty slice; otherwise the Cartesian product — as a
    MULTISET, in element order, with no tier-side dedup — of
    per-column encoded value sequences, where a `Column`
    contributes the one-element sequence `enc(get(k, v))` and a
    `MultiColumn` contributes `enc(e)` per element `e` of
    `get(k, v)` in order (empty element slice ⇒ empty product ⇒
    no entries). Duplicate encoded keys in the expansion are
    emitted verbatim; what happens to them is exclusively the
    engine's contract — `ErrIndexUniqueViolation` via the
    candidate-set rule on a unique index, last-wins collapse on
    a non-unique one (`indexing.md`). Covering bytes are
    computed per entry from the covering columns the same way;
  from=this spec §Compilation to IndexDecl;
  violation=Any divergence between this rule and the emitted
    entries breaks plan/scan equivalence
    (`query-builder.md Inv-QB1`) — a row the rule says is
    indexed under key X but the extractor emitted under key Y is
    unreachable via the index and reachable via scan. The
    no-dedup clause is load-bearing on its own: a tier that
    pre-dedups the expansion silently converts a unique-index
    candidate-set violation (`["go","go"]`, or two elements a
    folding encoder maps to equal bytes) into a successful
    write, masking the engine's uniqueness contract.

Invariant: kind=clause-explicit;
  property=Inv-TC5: a covering column's stored bytes decode via
    the same `Encoder[C]` that produced them, and
    `Column.From(row)` on a projection sourced from a covering
    index returns a value equal to `get(k, v)` evaluated on the
    row's current value;
  from=this spec §Covering projections +
    `indexing.md §Covering Indexes` (update rewrites covering);
  violation=Put(k, v1) then Put(k, v2): a projection served from
    covering bytes that were not rewritten (or decode under a
    different encoder) yields v1's field while `Get(k)` yields
    v2's — the covering staleness failure `indexing.md` pins,
    surfaced through the typed projection instead of the byte
    surface.

## Column declaration

```go
// Column declares one named, typed, order-preserving projection
// of a row. C's encoder must satisfy Inv-TC1 (lex order).
//
// The name is a semantic anchor with the same contract as
// IndexColumn.Name (indexing.md): renaming forces a rebuild via
// the schema hash; reusing a name for changed semantics requires
// a Version bump on every ColumnIndex that uses the column.
func NewColumn[K, V, C any](
    name string,
    enc Encoder[C],
    get func(K, V) C,
) *Column[K, V, C]

// MultiColumn declares a column with zero or more values per
// row (e.g. one entry per element of a slice field). An empty
// or nil returned slice contributes no entries for the row
// (partial-index semantics at element granularity).
func NewMultiColumn[K, V, C any](
    name string,
    enc Encoder[C],
    get func(K, V) []C,
) *MultiColumn[K, V, C]
```

Columns are stateless declarations, built once outside any
transaction, following the declaration/handle convention of
`typed-keyspaces.md §Naming convention`. A column may be shared
by any number of `ColumnIndex` declarations over the same
`(K, V)`.

Both column forms are type-erased for aggregation via a sealed
interface (`AnyColumn[K, V]`), sealed for the same reason
`AnyTypedIndex` is: the compilation path relies on every column
having been constructed through `NewColumn` / `NewMultiColumn`,
which pins encoder-ID folding and the name grammar. A second
sealed erasure, `AnySingleColumn[K, V]`, is implemented ONLY by
`*Column` — it types the positions where a multi-valued column
is structurally illegal (covering declarations), so "MultiColumn
in Covering" is unrepresentable rather than a runtime rejection
(a multi-valued covering slot has no single `enc(get(k, v))`
payload and no `From` surface).

## ColumnIndex

```go
type ColumnIndexOpts[K, V any] struct {
    Unique     bool
    Version    string                   // bump on accessor/Where logic change
    Where      func(K, V) bool          // nil ⇒ index every row
    Covering   []AnySingleColumn[K, V]  // typed covering projection; single-valued only
    CoverValue bool                     // full-row covering (see §Covering projections)
}

// NewColumnIndex declares an index over ordered columns.
// The declaration implements AnyTypedIndex[K, V] and is passed
// to TypedKeyspace Open / Create exactly like a TypedIndex.
func NewColumnIndex[K, V any](
    name string,
    columns []AnyColumn[K, V],
    opts ColumnIndexOpts[K, V],
) *ColumnIndex[K, V]
```

`Where` is the row-level partial-index predicate: false gates the
entire row (empty entry slice). Element-level filtering is the
accessor's job — a `MultiColumn` accessor returns the elements
that should be indexed. There is no separate predicate primitive,
matching `indexing.md §Partial Indexes` ("the extractor is the
predicate"; here, `Where` plus the accessors are the extractor).

`Unique` composes with `MultiColumn` at element granularity:
every expanded entry key must be unique, so a unique index over a
multi-column enforces global element uniqueness. Two entries with
the same key produced by one row's expansion are rejected via the
candidate-set rule of `indexing.md §Unique Indexes`.

Duplicate expanded entries on a NON-unique index collapse
last-wins per `indexing.md §Covering Indexes` (duplicate
collapse) — a `MultiColumn` returning `["go", "go"]` indexes the
row once under `"go"`.

## Compilation to IndexDecl

A `ColumnIndex` compiles to exactly one byte `IndexDecl`:

- `Name` = the ColumnIndex name, verbatim.
- `Columns[i].Name` = the synthesized name of `columns[i]` (see
  grammar below). `Covering[i].Name` likewise.
- `Unique`, `Version` = verbatim.
- `Extract` = the synthesized extractor implementing Inv-TC4.

The synthesized extractor panics on an `AppendEncode` error, the
same convention the typed layer uses for encode failures; the
engine's panic-atomicity contract
(`indexing.md §Write Path: Atomic Index Maintenance`, Panic
atomicity) guarantees no partial index state escapes.

### Synthesized column-name grammar

```
name := form-prefix || uvarint(len(userName)) || userName
             || uvarint(len(encoderID))   || encoderID

form-prefix := "gmdb/col/"        (Column)
             | "gmdb/multicol/"   (MultiColumn)
```

The result is used as the byte `IndexColumn.Name` string. The
grammar is injective (Inv-TC2): uvarint length prefixes make the
(userName, encoderID) pair unambiguous, and the form prefix
keeps a `Column` and a `MultiColumn` with identical name and
encoder distinct in the schema hash — their expansion semantics
differ, so their fingerprints must too (neither prefix is a
prefix of the other, or of `gmdb/cover-value/`). Encoder-ID
folding gives the same guarantee the typed layer's synthesis
gives `TypedIndex`: swapping a column's encoder surfaces as
`ErrIndexFingerprintMismatch` at open, never as silently misread
entries. Empty encoder IDs are rejected with
`ErrIndexEncoderIDEmpty` at open, as for `IKEnc`.

Cross-form disjointness (the second half of Inv-TC2) follows the
reserved-namespace pattern the typed layer already uses for its
full-row-covering sentinel (`gmdb/cover-value/`, the printable
engine-namespace prefix the typed read path recognizes):
`gmdb/col/` and `gmdb/multicol/` are reserved column-namespace
prefixes. `TypedIndex`'s synthesized key-column name is its raw
IK-encoder ID, so disjointness is enforced where the two domains
could meet: an encoder ID beginning with a reserved
column-namespace prefix (`gmdb/col/`, `gmdb/multicol/`,
`gmdb/cover-value/`) is rejected at open with
`ErrIndexEncoderIDReserved` — the rejection is what makes the
engine namespace non-mintable (the `<pkg>/<type>` ID convention
alone is only recommended). Existing `TypedIndex` fingerprints
are unaffected: the
typed synthesis itself does not change, and no canonical encoder
ID lies in the reserved namespace.

The raw byte API remains an open namespace: a byte `IndexDecl`
may name columns anything, including reserved-prefix strings —
the same accepted, documented residual as the existing
cover-value sentinel. The raw tier owns its namespace; Inv-TC2's
disjointness guarantee is between the typed forms.

## Covering projections

`Covering` columns store `enc(get(k, v))` per entry as the byte
covering tuple, in declaration order, per
`indexing.md §Covering Indexes`. This gives arbitrary covering
projections the typed return surface that
`typed-keyspaces.md §Covering` documents as absent from the
`TypedIndex` tier: the consumer decodes per column, not per row.

`CoverValue: true` is the full-row alternative: the entry stores
`encode(V)` as the single covering column with the value
encoder's ID folded into the fingerprint — the same mechanics
AND the same `gmdb/cover-value/` sentinel as
`TypedIndex.CoverValue` (`typed-keyspaces.md §Covering`), so the
typed read path recognizes the shape from either decl form and
reads of `V` through the index skip the row back-lookup.
`CoverValue` and a non-empty `Covering` are mutually exclusive
(full-row covering already covers every projection); declaring
both is rejected at open with `ErrInvalidOptions`.

```go
// Projection is an opaque row produced by a covering-index read
// (query-builder.md §Covering-aware execution is the producing
// surface). It carries the column slots the serving plan
// resolved; columns are decoded individually via Column.From.
type Projection struct { /* unexported */ }

// From decodes this column's slot out of a projection row.
// Requesting a column the projection does not carry returns
// ErrColumnAbsent — never a zero value.
func (c *Column[K, V, C]) From(row Projection) (C, error)
```

`Projection` and `From` live in the typed package (`gmdb/typed`)
with the column declarations (the query package returns them;
the sealed decl types decode them). `From` is the read-side inverse of the
covering write (Inv-TC5). Which reads are served from covering
bytes versus row back-lookup is the planner's contract
(`query-builder.md Inv-QB3`); this tier only guarantees the
bytes round-trip.

## Relationship to TypedIndex

`ColumnIndex` and `TypedIndex` coexist as peers implementing the
same sealed `AnyTypedIndex`:

- `TypedIndex[K, V, IK]` remains the opaque-IK escape hatch: one
  encoder controls the entire key encoding, `Extract` may return
  multiple `IK`s with arbitrary logic. Maximum control, no
  per-column structure — and therefore invisible to the query
  builder's planner.
- `ColumnIndex` trades that control for structure: per-column
  encoders, partial-prefix capability, typed covering
  projections, and planner visibility.

The `typed-keyspaces.md §Limitation` workaround ("use the
byte-oriented IndexDecl, declaring each sub-field as a separate
IndexColumn") is superseded by `ColumnIndex` for typed callers;
the byte API remains for byte-oriented callers. A keyspace may
declare indexes of both forms simultaneously; Inv-TC2's
cross-form name-domain disjointness is what keeps a form swap
under an unchanged index name from passing the drift guard.
