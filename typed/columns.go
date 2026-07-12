package typed

import (
	"encoding/binary"
	"errors"
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

// AnySingleColumn is the sealed erasure for SINGLE-VALUED columns
// — implemented only by *Column. It types the positions where a
// multi-valued column is structurally illegal (covering
// declarations: a multi-valued covering slot has no single
// enc(get(k, v)) payload and no From surface), making
// "MultiColumn in Covering" unrepresentable rather than a runtime
// rejection (typed-columns.md §Covering projections).
type AnySingleColumn[K, V any] interface {
	AnyColumn[K, V]
	// encodeOne returns the single encoded value for a row.
	// PANICS on encode failure, per the tier's convention.
	encodeOne(indexName string, k K, v V) []byte
}

func (c *Column[K, V, C]) columnName() string {
	return synthesizeColumnName(columnNamePrefix, c.name, c.enc.ID())
}
func (c *Column[K, V, C]) encoderIdentity() (string, string) { return c.name, c.enc.ID() }
func (c *Column[K, V, C]) encodeOne(indexName string, k K, v V) []byte {
	b, err := c.enc.AppendEncode(nil, c.get(k, v))
	if err != nil {
		panic(fmt.Errorf("gmdb: column index %q covering column %q: encode: %w", indexName, c.name, err))
	}
	return b
}

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

	// Covering pins single-valued columns to be carried in each
	// index entry's value — the typed covering projection surface
	// (typed-columns.md §Covering projections): reads satisfiable
	// from key + covering columns never touch the row keyspace,
	// and Column.From decodes the projected slots.
	Covering []AnySingleColumn[K, V]

	// CoverValue makes this a full-row covering index: each entry
	// stores encode(V) as the single covering column under the
	// shared gmdb/cover-value/ sentinel, so the typed read path
	// recognizes the shape and reads of V skip the row
	// back-lookup. Mutually exclusive with Covering (full-row
	// already covers every projection): declaring both is
	// rejected with ErrInvalidOptions.
	CoverValue bool
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

// coveringDeclared reports the declaration's covering state —
// probed by the SetKeyspace factories, which reject covering
// declarations outright: a set index's covering payload has no
// read path (the byte layer never serves covering for set
// indexes; the compound PK already carries the member value), so
// declaring one would pay write amplification for nothing
// (typed-columns.md §Covering projections).
func (ci *ColumnIndex[K, V]) coveringDeclared() (string, bool) {
	return ci.name, ci.opts.CoverValue || len(ci.opts.Covering) > 0
}

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
	if ci.opts.CoverValue && len(ci.opts.Covering) > 0 {
		return nil, fmt.Errorf("gmdb: column index %q declares both CoverValue and Covering (full-row covering already covers every projection): %w",
			ci.name, gmdb.ErrInvalidOptions)
	}
	if ci.opts.CoverValue {
		valID := valEnc.ID()
		if valID == "" {
			return nil, fmt.Errorf("gmdb: column index %q value encoder (CoverValue): %w", ci.name, gmdb.ErrIndexEncoderIDEmpty)
		}
		decl.Covering = []gmdb.IndexCoveringColumn{{Name: indexing.CoverValueColumn(valID)}}
	}
	coverSeen := make(map[string]struct{}, len(ci.opts.Covering))
	for i, col := range ci.opts.Covering {
		if col == nil {
			return nil, fmt.Errorf("gmdb: column index %q covering column %d is nil: %w", ci.name, i, gmdb.ErrInvalidOptions)
		}
		userName, encID := col.encoderIdentity()
		if encID == "" {
			return nil, fmt.Errorf("gmdb: column index %q covering column %q: %w", ci.name, userName, gmdb.ErrIndexEncoderIDEmpty)
		}
		if err := validateEncoderIDNamespace(fmt.Sprintf("column index %q covering column %q", ci.name, userName), encID); err != nil {
			return nil, err
		}
		name := col.columnName()
		if _, dup := coverSeen[name]; dup {
			return nil, fmt.Errorf("gmdb: column index %q declares covering column %q twice: %w", ci.name, userName, gmdb.ErrInvalidOptions)
		}
		coverSeen[name] = struct{}{}
		decl.Covering = append(decl.Covering, gmdb.IndexCoveringColumn{Name: name})
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
	covering := ci.opts.Covering
	coverValue := ci.opts.CoverValue
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
		// Covering payload is per-ROW (computed once, shared by
		// every product entry): the projection columns'
		// enc(get(k, v)), or encode(V) verbatim under CoverValue
		// (copied so the entry does not alias the caller's buffer).
		var cover [][]byte
		if coverValue {
			cb := make([]byte, len(valueBytes))
			copy(cb, valueBytes)
			cover = [][]byte{cb}
		} else if len(covering) > 0 {
			cover = make([][]byte, len(covering))
			for i, cc := range covering {
				cover[i] = cc.encodeOne(name, k, v)
			}
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
				entries = append(entries, gmdb.IndexEntry{Cols: e, Cover: cover})
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

// ErrColumnAbsent reports a Column.From call against a projection
// that does not carry that column's slot — never a zero value
// (typed-columns.md §Covering projections).
var ErrColumnAbsent = errors.New("gmdb/typed: column not carried by this projection")

// Projection is an opaque row produced by a covering-index read:
// it carries the column slots the serving plan resolved, keyed by
// synthesized column name. Columns are decoded individually via
// Column.From. Construction is package-internal; the query
// package — the producing surface (query-builder.md
// §Covering-aware execution) — reaches it through the internal
// representation seam the two packages share.
type Projection struct {
	slots map[string][]byte
}

// newProjection builds a projection from parallel synthesized
// column names and their raw slot bytes (the decoded covering
// tuple, in declaration order).
func newProjection(names []string, vals [][]byte) Projection {
	slots := make(map[string][]byte, len(names))
	for i, n := range names {
		if i < len(vals) {
			slots[n] = vals[i]
		}
	}
	return Projection{slots: slots}
}

// From decodes this column's slot out of a projection row.
// Requesting a column the projection does not carry returns
// ErrColumnAbsent. From is the read-side inverse of the covering
// write (Inv-TC5): the returned value equals get(k, v) evaluated
// on the row's current value.
func (c *Column[K, V, C]) From(p Projection) (C, error) {
	b, ok := p.slots[c.columnName()]
	if !ok {
		var zero C
		return zero, fmt.Errorf("column %q: %w", c.name, ErrColumnAbsent)
	}
	return c.enc.Decode(b)
}
