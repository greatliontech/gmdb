// Package qrep is the query-representation seam shared by
// gmdb/typed (which constructs terms and order keys from column
// declarations) and gmdb/query (which plans and executes over
// them). It is deliberately non-generic: the typed tier erases its
// K/V parameters into closures at construction, so the planner and
// executor never instantiate over row types
// (query-builder.md §Terms).
package qrep

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

// RowOps is the executor's type-erased row codec over one typed
// keyspace handle: index leaves yield raw PK / value bytes, and
// the query package — which cannot reach the handle's unexported
// encoders — decodes rows through these closures. Values are
// boxed exactly as Term.Eval's arguments (the typed tier asserts
// back to its K / V).
type RowOps struct {
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
