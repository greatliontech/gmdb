// Package qrep is the query-representation seam shared by
// gmdb/typed (which constructs terms and order keys from column
// declarations) and gmdb/query (which plans and executes over
// them). It is deliberately non-generic: the typed tier erases its
// K/V parameters into closures at construction, so the planner and
// executor never instantiate over row types
// (query-builder.md §Terms).
package qrep

import "bytes"

// Kind discriminates a term's comparison shape.
type Kind uint8

const (
	// KindEq matches column == literal (Lo).
	KindEq Kind = iota
	// KindLt / KindLte / KindGt / KindGte compare against Lo.
	KindLt
	KindLte
	KindGt
	KindGte
	// KindBetween matches Lo <= column < Hi.
	KindBetween
	// KindHasPrefix matches encoded-byte prefixes of Lo
	// (query-builder.md §Terms: defined purely at the byte level).
	KindHasPrefix
	// KindContains matches multi-columns with ANY element == Lo.
	KindContains
	// KindContainsRange matches multi-columns with ANY element in
	// [Lo, Hi).
	KindContainsRange
	// KindOr is a disjunction of conjunction groups (Disjuncts);
	// the scalar fields are unused.
	KindOr
)

// Term is one predicate leaf (or an Or of conjunctions) in the
// builder's structured representation. Literals are pre-encoded at
// construction via the column's own encoder — the property that
// makes pushdown and residual evaluation agree (Inv-QB2).
type Term struct {
	Kind Kind

	// ColumnName is the column's synthesized byte name — the same
	// string that appears in a lowered IndexDecl's Columns, which
	// is what pushdown matching compares.
	ColumnName string
	// Multi reports a multi-valued column (Contains kinds).
	Multi bool

	// Lo and Hi are the encoded literal bound(s); Hi only for the
	// range-shaped kinds.
	Lo, Hi []byte

	// Err carries a literal-encode failure from construction; a
	// query using the term fails at iteration start with it
	// (query-builder.md §Terms).
	Err error

	// Eval evaluates the term residually against a DECODED row,
	// type-erased (the typed tier asserts back to its K/V). It
	// implements Inv-QB2: encode the row's column value(s) with
	// the same encoder and compare bytes.
	Eval func(k, v any) (bool, error)

	// Disjuncts holds KindOr's conjunction groups.
	Disjuncts [][]Term
}

// OrderKey is one ordering key: a column plus direction.
type OrderKey struct {
	ColumnName string
	Desc       bool
	// EncodeRow returns the row's encoded column value for
	// materialized ordering (type-erased, as Eval).
	EncodeRow func(k, v any) ([]byte, error)
}

// IndexCol is one key column of a planner-eligible index: the
// synthesized byte column name plus whether the column is
// multi-valued (a MultiColumn — one index entry per element, which
// is what forces distinct-by-PK dedup on partial consumption).
type IndexCol struct {
	Name  string
	Multi bool
}

// IndexInfo is the planner's view of one ColumnIndex declared on a
// typed keyspace handle, distilled at open/create time from the
// same declaration the byte lowering consumes. It exists because
// the lowered byte IndexDecl cannot answer planner questions: a
// Where predicate folds invisibly into the extractor closure, and
// the multi/scalar form of each column is only recoverable from
// the typed declaration (query-builder.md §Planning rules).
type IndexInfo struct {
	Name    string
	KeyCols []IndexCol
	// Covering holds the synthesized covering column names (empty
	// under CoverValue).
	Covering   []string
	CoverValue bool
	Unique     bool
	// Partial reports a non-nil Where: rule 7 — never
	// planner-eligible (the entry set excludes rows the query may
	// want).
	Partial bool
}

// EvalScalar implements Inv-QB2's byte-level comparison for the
// scalar term kinds — the SAME semantics an index seek realizes,
// so pushdown, row-residual, and entry-slot evaluation agree by
// construction. enc is the encoded column value under comparison;
// lo/hi are the term's encoded literals. Returns false for kinds
// it does not handle (Contains kinds are remapped by callers; Or
// never reaches scalar evaluation) — callers that must fail loud
// on an unhandled kind check HandledScalarKind first.
func EvalScalar(kind Kind, enc, lo, hi []byte) bool {
	switch kind {
	case KindEq:
		return bytes.Equal(enc, lo)
	case KindLt:
		return bytes.Compare(enc, lo) < 0
	case KindLte:
		return bytes.Compare(enc, lo) <= 0
	case KindGt:
		return bytes.Compare(enc, lo) > 0
	case KindGte:
		return bytes.Compare(enc, lo) >= 0
	case KindBetween:
		return bytes.Compare(enc, lo) >= 0 && bytes.Compare(enc, hi) < 0
	case KindHasPrefix:
		return bytes.HasPrefix(enc, lo)
	}
	return false
}

// HandledScalarKind reports whether EvalScalar implements kind.
func HandledScalarKind(kind Kind) bool {
	switch kind {
	case KindEq, KindLt, KindLte, KindGt, KindGte, KindBetween, KindHasPrefix:
		return true
	}
	return false
}

// SelectCol is one selected projection column, erased for the
// query executor: the synthesized byte column name (what index
// key/covering slots are matched by) plus the row-side encoder —
// route-3 projections (query-builder.md §Covering-aware execution)
// compute slots from the materialized row with it.
type SelectCol struct {
	Name string
	// EncodeRow returns enc(get(k, v)) for the column (type-erased,
	// as Term.Eval).
	EncodeRow func(k, v any) ([]byte, error)
}

// ProjectionSlots builds the typed Projection value from parallel
// synthesized column names and slot bytes; the typed tier
// registers its constructor here so the query package — the
// producing surface — can build Projection values without the
// typed package exporting the constructor (typed-columns.md
// §Covering projections).
var ProjectionSlots func(names []string, vals [][]byte) any

// RowOps is the executor's type-erased row codec over one typed
// keyspace handle: index leaves yield raw PK / value bytes, and
// the query package — which cannot reach the handle's unexported
// encoders — decodes rows through these closures. Values are
// boxed exactly as Term.Eval's arguments (the typed tier asserts
// back to its K / V).
type RowOps struct {
	// ValEncID is the handle's value-encoder identity — the
	// executor verifies a live full-row-covering sentinel embeds
	// THIS ID before decoding entry bytes as V (a same-tx Rebuild
	// can install a foreign encoder's sentinel; decoding its bytes
	// with this codec would be silently wrong).
	ValEncID string
	// DecodeKey decodes a primary-key byte slice to K.
	DecodeKey func(pk []byte) (any, error)
	// DecodeVal decodes stored row bytes to V.
	DecodeVal func(vb []byte) (any, error)
	// FetchRow back-looks-up pk's row and decodes it. found=false
	// reports a vanished row — the caller skips the entry, matching
	// the byte Lookup contract's silent-skip of index/row
	// inconsistencies (indexing.md §Lookup API).
	FetchRow func(pk []byte) (val any, found bool, err error)
}
