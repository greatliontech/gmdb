package typed

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/thegrumpylion/gmdb"
	"github.com/thegrumpylion/gmdb/internal/indexing"
)

// Per-field typed column declarations and the ColumnIndex that
// compiles them into a multi-column byte gmdb.IndexDecl
// (typed-columns.md). Where Index[K, V, IK] collapses a composite
// index key into one opaque column, a ColumnIndex declares one
// byte IndexColumn per field — partial-prefix queries and
// per-column planning compose on top, because the declaration is
// invertible for literals: the planner holds the same Encoder[C]
// the extractor uses.

// Reserved column-namespace prefixes (typed-columns.md
// §Synthesized column-name grammar). The synthesized names of the
// two column forms live behind printable engine-namespace
// prefixes, following the cover-value sentinel pattern; encoder
// IDs are barred from the reserved namespace so `Index`'s raw
// IK-encoder-ID synthesis can never collide with a ColumnIndex
// column's name (Inv-TC2 cross-form disjointness).
const (
	columnNamePrefix      = "gmdb/col/"
	multiColumnNamePrefix = "gmdb/multicol/"
)

// validateEncoderIDNamespace rejects encoder IDs inside the
// reserved column namespace (typed-keyspaces.md §Encoder
// interface): the engine namespace is not mintable by callers —
// the rejection is what makes the Inv-TC2 disjointness real
// rather than conventional.
func validateEncoderIDNamespace(where, id string) error {
	for _, p := range []string{columnNamePrefix, multiColumnNamePrefix, indexing.CoverValuePrefix} {
		if strings.HasPrefix(id, p) {
			return fmt.Errorf("gmdb: %s: encoder ID %q is inside the reserved column namespace %q: %w",
				where, id, p, gmdb.ErrIndexEncoderIDReserved)
		}
	}
	return nil
}

// synthesizeColumnName builds the byte gmdb.IndexColumn.Name for a
// column per typed-columns.md §Synthesized column-name grammar:
//
//	name := form-prefix || uvarint(len(userName)) || userName
//	             || uvarint(len(encoderID))   || encoderID
//
// The uvarint length prefixes make the (userName, encoderID) pair
// injective; the form prefix keeps the two column forms — whose
// expansion semantics differ — fingerprint-distinct (Inv-TC2).
func synthesizeColumnName(formPrefix, userName, encoderID string) string {
	var b strings.Builder
	var buf [binary.MaxVarintLen64]byte
	b.WriteString(formPrefix)
	n := binary.PutUvarint(buf[:], uint64(len(userName)))
	b.Write(buf[:n])
	b.WriteString(userName)
	n = binary.PutUvarint(buf[:], uint64(len(encoderID)))
	b.Write(buf[:n])
	b.WriteString(encoderID)
	return b.String()
}

// Column declares one named, typed, order-preserving projection
// of a row: C's encoder must produce byte sequences whose lex
// order matches the column type's intended order (Inv-TC1 — the
// same constraint typed-keyspaces.md places on Encoder[K]).
//
// The name is a semantic anchor with the same contract as
// gmdb.IndexColumn.Name: renaming forces a rebuild via the schema
// hash; reusing a name for changed semantics requires a Version
// bump on every ColumnIndex that uses the column. The accessor
// MUST be a pure function of (K, V) (Inv-TC3) — a change to its
// logic is extractor-logic drift the engine cannot see, so bump
// Version.
type Column[K, V, C any] struct {
	name string
	enc  Encoder[C]
	get  func(K, V) C
}

// NewColumn builds a Column declaration. Columns are stateless,
// built once outside any transaction, and may be shared by any
// number of ColumnIndex declarations over the same (K, V).
func NewColumn[K, V, C any](name string, enc Encoder[C], get func(K, V) C) *Column[K, V, C] {
	return &Column[K, V, C]{name: name, enc: enc, get: get}
}

// MultiColumn declares a column with zero or more values per row
// (e.g. one entry per element of a slice field). An empty or nil
// returned slice contributes no entries for the row
// (partial-index semantics at element granularity). Same Inv-TC1
// / Inv-TC3 contracts as Column.
type MultiColumn[K, V, C any] struct {
	name string
	enc  Encoder[C]
	get  func(K, V) []C
}

// NewMultiColumn builds a MultiColumn declaration.
func NewMultiColumn[K, V, C any](name string, enc Encoder[C], get func(K, V) []C) *MultiColumn[K, V, C] {
	return &MultiColumn[K, V, C]{name: name, enc: enc, get: get}
}

// AnyColumn is the type-erased interface satisfied by every
// Column and MultiColumn over (K, V). It exists so one
// ColumnIndex can declare columns with heterogeneous C types.
//
// The interface is SEALED (unexported methods) for the same
// reason AnyIndex is: the compilation path relies on every column
// having been constructed through NewColumn / NewMultiColumn,
// which pins encoder-ID folding and the synthesized-name grammar.
type AnyColumn[K, V any] interface {
	// columnName returns the synthesized byte IndexColumn.Name.
	columnName() string
	// encoderIdentity returns (userName, encoderID) for
	// validation and diagnostics.
	encoderIdentity() (string, string)
	// encodeAll returns the column's encoded value sequence for a
	// row — a singleton for Column, one element per accessor
	// output for MultiColumn (order preserved, no dedup;
	// Inv-TC4). PANICS on an encode failure, the typed tier's
	// established convention (the byte extractor is infallible;
	// the engine's panic-atomicity contract contains it).
	encodeAll(indexName string, k K, v V) [][]byte
}

func (c *Column[K, V, C]) columnName() string {
	return synthesizeColumnName(columnNamePrefix, c.name, c.enc.ID())
}
func (c *Column[K, V, C]) encoderIdentity() (string, string) { return c.name, c.enc.ID() }
func (c *Column[K, V, C]) encodeAll(indexName string, k K, v V) [][]byte {
	b, err := c.enc.AppendEncode(nil, c.get(k, v))
	if err != nil {
		panic(fmt.Errorf("gmdb: column index %q column %q: encode: %w", indexName, c.name, err))
	}
	return [][]byte{b}
}

func (m *MultiColumn[K, V, C]) columnName() string {
	return synthesizeColumnName(multiColumnNamePrefix, m.name, m.enc.ID())
}
func (m *MultiColumn[K, V, C]) encoderIdentity() (string, string) { return m.name, m.enc.ID() }
func (m *MultiColumn[K, V, C]) encodeAll(indexName string, k K, v V) [][]byte {
	vals := m.get(k, v)
	if len(vals) == 0 {
		return nil
	}
	out := make([][]byte, 0, len(vals))
	for _, e := range vals {
		b, err := m.enc.AppendEncode(nil, e)
		if err != nil {
			panic(fmt.Errorf("gmdb: column index %q column %q: encode element: %w", indexName, m.name, err))
		}
		out = append(out, b)
	}
	return out
}

// ColumnIndexOpts configures a ColumnIndex declaration.
type ColumnIndexOpts[K, V any] struct {
	// Unique rejects duplicate expanded entry keys — for a
	// MultiColumn at ELEMENT granularity: every expanded entry
	// key must be globally unique, and intra-row duplicates are
	// candidate-set collisions (indexing.md §Unique Indexes).
	Unique bool
	// Version is the extractor-logic drift tag: bump after any
	// accessor or Where change that alters output for the same
	// row (indexing.md §Drift Guard).
	Version string
	// Where is the row-level partial-index predicate: false gates
	// the entire row (no entries). Element-level filtering is the
	// accessor's job. nil ⇒ index every row.
	Where func(K, V) bool
}

// ColumnIndex declares an index over ordered per-field columns,
// compiling to exactly one multi-column byte gmdb.IndexDecl
// (typed-columns.md §Compilation to IndexDecl). It implements the
// sealed AnyIndex and is passed to Keyspace Open / Create exactly
// like an Index.
type ColumnIndex[K, V any] struct {
	name    string
	columns []AnyColumn[K, V]
	opts    ColumnIndexOpts[K, V]
}

// NewColumnIndex builds a ColumnIndex declaration.
func NewColumnIndex[K, V any](name string, columns []AnyColumn[K, V], opts ColumnIndexOpts[K, V]) *ColumnIndex[K, V] {
	return &ColumnIndex[K, V]{name: name, columns: columns, opts: opts}
}

// Compile-time proof that *ColumnIndex implements the sealed
// AnyIndex — the second legal implementer beside *Index.
var _ AnyIndex[int, int] = (*ColumnIndex[int, int])(nil)

// indexDecl lowers the column index to a byte *gmdb.IndexDecl:
// synthesized column names (encoder IDs folded into the schema
// hash), the multiset Cartesian extractor (Inv-TC4), composite
// kind. Implements AnyIndex (sealed).
func (ci *ColumnIndex[K, V]) indexDecl(keyEnc Encoder[K], valEnc Encoder[V]) (*gmdb.IndexDecl, error) {
	if len(ci.columns) == 0 {
		return nil, fmt.Errorf("gmdb: column index %q declares zero columns: %w", ci.name, gmdb.ErrInvalidOptions)
	}
	decl := &gmdb.IndexDecl{
		Name:    ci.name,
		Unique:  ci.opts.Unique,
		Version: ci.opts.Version,
	}
	for i, col := range ci.columns {
		if col == nil {
			return nil, fmt.Errorf("gmdb: column index %q column %d is nil: %w", ci.name, i, gmdb.ErrInvalidOptions)
		}
		userName, encID := col.encoderIdentity()
		if encID == "" {
			return nil, fmt.Errorf("gmdb: column index %q column %q: %w", ci.name, userName, gmdb.ErrIndexEncoderIDEmpty)
		}
		if err := validateEncoderIDNamespace(fmt.Sprintf("column index %q column %q", ci.name, userName), encID); err != nil {
			return nil, err
		}
		decl.Columns = append(decl.Columns, gmdb.IndexColumn{Name: col.columnName()})
	}
	// Duplicate column declarations in one index are caller bugs:
	// two identically-synthesized names would hash consistently but
	// make per-column positions ambiguous (typed-columns.md
	// §ColumnIndex).
	seen := make(map[string]struct{}, len(decl.Columns))
	for i, dc := range decl.Columns {
		if _, dup := seen[dc.Name]; dup {
			userName, _ := ci.columns[i].encoderIdentity()
			return nil, fmt.Errorf("gmdb: column index %q declares column %q twice: %w", ci.name, userName, gmdb.ErrInvalidOptions)
		}
		seen[dc.Name] = struct{}{}
	}
	decl.Extract = ci.makeExtractor(keyEnc, valEnc)
	return decl, nil
}

// makeExtractor builds the byte extractor implementing Inv-TC4:
// Where gates the whole row; otherwise the output is the
// Cartesian product of the per-column encoded value sequences —
// as a MULTISET, in element order, rightmost column varying
// fastest, with NO tier-side dedup (duplicate keys are
// exclusively the engine's contract: candidate-set violation on a
// unique index, last-wins collapse on a non-unique one).
func (ci *ColumnIndex[K, V]) makeExtractor(keyEnc Encoder[K], valEnc Encoder[V]) gmdb.IndexExtractor {
	cols := ci.columns
	where := ci.opts.Where
	name := ci.name
	return func(keyBytes, valueBytes []byte) []gmdb.IndexEntry {
		k, err := keyEnc.Decode(keyBytes)
		if err != nil {
			panic(fmt.Errorf("gmdb: column index %q: decode key: %w", name, err))
		}
		v, err := valEnc.Decode(valueBytes)
		if err != nil {
			panic(fmt.Errorf("gmdb: column index %q: decode value: %w", name, err))
		}
		if where != nil && !where(k, v) {
			return nil
		}
		perCol := make([][][]byte, len(cols))
		total := 1
		for i, col := range cols {
			perCol[i] = col.encodeAll(name, k, v)
			total *= len(perCol[i])
			if total == 0 {
				return nil // an empty column sequence empties the product
			}
		}
		entries := make([]gmdb.IndexEntry, 0, total)
		tuple := make([][]byte, len(cols))
		var expand func(depth int)
		expand = func(depth int) {
			if depth == len(cols) {
				e := make([][]byte, len(cols))
				copy(e, tuple)
				entries = append(entries, gmdb.IndexEntry{Cols: e})
				return
			}
			for _, b := range perCol[depth] {
				tuple[depth] = b
				expand(depth + 1)
			}
		}
		expand(0)
		return entries
	}
}
