package typed

import (
	"fmt"
	"iter"

	"github.com/thegrumpylion/gmdb"
	"github.com/thegrumpylion/gmdb/internal/indexing"
)

// The full-row cover-value sentinel contract (prefix, column
// synthesis, decl recognition) lives in internal/indexing beside
// the decl type: the engine's covering-return path recognizes it,
// this tier writes it — one source for the persisted format.

// Typed index layer (typed-keyspaces.md §Typed Indexes). A Index
// declares a type-safe secondary index on a Keyspace[K,V] with an
// extracted index-key type IK; it lowers to a byte-layer *gmdb.IndexDecl via
// the sealed AnyIndex.indexDecl, threading the keyspace's K/V
// encoders so the extractor closure can decode the stored (key,value)
// before running the user's typed Extract and encoding each IK.
//
// Schema-hash drift (typed-keyspaces.md §Invariants): the synthesized gmdb.IndexColumn's Name is set
// to IKEnc.ID(). Since the byte schema-hash hashes column names (which
// are pure fingerprint inputs, never read at decode), swapping IKEnc for
// one with a different ID changes the column name and therefore the
// stored fingerprint — surfacing as gmdb.ErrIndexFingerprintMismatch at Open.

// Index declares a typed index on a Keyspace[K,V] with index-
// key type IK.
//
// Extract produces zero or more IK values per row; an empty/nil slice
// skips the row (partial index). IKEnc encodes each IK to lex-safe
// bytes and MUST have a stable non-empty ID() (typed-keyspaces.md §Invariants) — an empty ID is
// rejected at Open with gmdb.ErrIndexEncoderIDEmpty.
//
// IKEnc MUST be able to encode every value Extract produces: the
// byte-layer index extractor is infallible (it cannot return an error),
// so an IKEnc.AppendEncode failure during maintenance PANICS with a
// descriptive error rather than silently dropping an index entry (which
// would diverge the index from the rows). For all canonical encoders
// except an out-of-range TimeEncoder value this never fires; use
// infallible index-key encoders, or ensure Extract never yields an
// unrepresentable value.
//
// CoverValue makes this a full-row covering index: the encoded row value
// is stored in each index entry, so IndexQuery Lookup/Range/Prefix/
// Get return V directly from the index without a back-lookup against the
// row keyspace (a read optimization — identical results, fewer reads).
// The keyspace's value-encoder ID is folded into the schema-hash
// fingerprint, so swapping the value encoder triggers
// gmdb.ErrIndexFingerprintMismatch; an empty value-encoder ID is rejected
// with gmdb.ErrIndexEncoderIDEmpty. CoverValue has effect only on a
// Keyspace (Keyspace-backed) index — a SetKeyspace index's value is
// already carried in its compound PK, so there is no back-lookup to skip.
type Index[K, V, IK any] struct {
	Name       string
	IKEnc      Encoder[IK]
	Unique     bool
	Version    string
	Extract    func(K, V) []IK
	CoverValue bool
}

// Compile-time proof that *Index implements the sealed
// AnyIndex — the only legal implementer (the unexported indexDecl
// method seals the interface to this package).
var _ AnyIndex[int, int] = (*Index[int, int, int])(nil)

// indexDecl lowers the typed index to a byte *gmdb.IndexDecl, threading the
// owning keyspace's encoders into the extractor closure. Implements
// AnyIndex[K,V] (sealed). Returns gmdb.ErrIndexEncoderIDEmpty if IKEnc's
// ID() is empty.
func (t *Index[K, V, IK]) indexDecl(keyEnc Encoder[K], valEnc Encoder[V]) (*gmdb.IndexDecl, error) {
	ikID := t.IKEnc.ID()
	if ikID == "" {
		return nil, fmt.Errorf("gmdb: typed index %q index-key encoder: %w", t.Name, gmdb.ErrIndexEncoderIDEmpty)
	}
	decl := &gmdb.IndexDecl{
		Name: t.Name,
		// One opaque column for the IK; its Name = IKEnc.ID() folds the
		// encoder identity into the schema-hash fingerprint (typed-keyspaces.md §Invariants).
		Columns: []gmdb.IndexColumn{{Name: ikID}},
		Unique:  t.Unique,
		Version: t.Version,
	}
	if t.CoverValue {
		valID := valEnc.ID()
		if valID == "" {
			return nil, fmt.Errorf("gmdb: typed index %q value encoder (CoverValue): %w", t.Name, gmdb.ErrIndexEncoderIDEmpty)
		}
		// One covering column carrying the full encoded value; its name
		// folds the value-encoder identity into the fingerprint.
		decl.Covering = []gmdb.IndexCoveringColumn{{Name: indexing.CoverValueColumn(valID)}}
	}
	decl.Extract = t.makeExtractor(keyEnc, valEnc)
	return decl, nil
}

// makeExtractor builds the byte-layer gmdb.IndexExtractor closure: decode the
// stored (key,value) into (K,V), run the typed Extract, encode each IK.
// Decode/encode failures panic (see the Index godoc): the byte
// extractor contract is total, and silently dropping an entry would
// diverge the index from the rows.
func (t *Index[K, V, IK]) makeExtractor(keyEnc Encoder[K], valEnc Encoder[V]) gmdb.IndexExtractor {
	coverValue := t.CoverValue
	return func(keyBytes, valueBytes []byte) []gmdb.IndexEntry {
		k, err := keyEnc.Decode(keyBytes)
		if err != nil {
			panic(fmt.Errorf("gmdb: typed index %q: decode key: %w", t.Name, err))
		}
		v, err := valEnc.Decode(valueBytes)
		if err != nil {
			panic(fmt.Errorf("gmdb: typed index %q: decode value: %w", t.Name, err))
		}
		iks := t.Extract(k, v)
		if len(iks) == 0 {
			return nil
		}
		// Full-row covering: each entry carries the stored value bytes
		// (which ARE encode(V)) as its single covering column, so Lookup
		// can return V without a back-lookup. Copied so the gmdb.IndexEntry
		// does not alias the caller's value buffer.
		var cover [][]byte
		if coverValue {
			cb := make([]byte, len(valueBytes))
			copy(cb, valueBytes)
			cover = [][]byte{cb}
		}
		entries := make([]gmdb.IndexEntry, 0, len(iks))
		for _, ik := range iks {
			ikb, err := t.IKEnc.AppendEncode(nil, ik)
			if err != nil {
				panic(fmt.Errorf("gmdb: typed index %q: encode index key: %w", t.Name, err))
			}
			entries = append(entries, gmdb.IndexEntry{Cols: [][]byte{ikb}, Cover: cover})
		}
		return entries
	}
}

// Index returns a typed query handle for the named index on this typed
// keyspace. The returned *IndexHandle carries the keyspace's K/V
// encoders (type-erased); bind it to a specific IK type with
// NewIndexQuery. Returns gmdb.ErrIndexNotFound if no such index.
func (t *KeyspaceHandle[K, V]) Index(name string) (*IndexHandle, error) {
	idx, err := t.ks.Index(name)
	if err != nil {
		return nil, err
	}
	// Enable the byte-layer covering-return for a full-row-covering typed
	// index, so Lookup/Range/Prefix/Get return V from the index entry
	// without a back-lookup.
	if indexing.IsCoverValueDecl(idx.Decl()) {
		idx.EnableCoverValueReturn()
	}
	return &IndexHandle{idx: idx, keyEnc: t.keyEnc, valEnc: t.valEnc}, nil
}

// Index returns a typed query handle for the named index on this typed
// set keyspace. For a SetKeyspace index the query yields (setKey,
// setValue) pairs, so bind K = the set-key type and V = the set-value
// type in NewIndexQuery.
func (t *SetKeyspaceHandle[K, V]) Index(name string) (*IndexHandle, error) {
	idx, err := t.sks.Index(name)
	if err != nil {
		return nil, err
	}
	return &IndexHandle{idx: idx, keyEnc: t.keyEnc, valEnc: t.valEnc}, nil
}

// IndexHandle is a type-erased handle to an opened index on a typed
// keyspace. It carries the byte *gmdb.IndexHandle plus the keyspace's K/V encoders
// (as any); NewIndexQuery re-introduces the static K/V/IK types.
type IndexHandle struct {
	idx    *gmdb.IndexHandle
	keyEnc any // Encoder[K] of the owning keyspace
	valEnc any // Encoder[V]
}

// IndexQuery is a statically-typed query over an index whose
// index-key type is IK and whose owning keyspace is keyed/valued by
// K/V. Construct via NewIndexQuery. Like the byte *gmdb.IndexHandle, a query
// handle is not safe for concurrent iteration; Err() is per-handle.
type IndexQuery[K, V, IK any] struct {
	idx     *gmdb.IndexHandle
	keyEnc  Encoder[K]
	valEnc  Encoder[V]
	ikEnc   Encoder[IK]
	bindErr error // permanent: encoder-type mismatch at construction (query inert)
	err     error // per-sequence: IK encode / result decode / iteration error
}

// NewIndexQuery binds a IndexHandle to concrete K/V/IK types
// with the supplied index-key encoder. If the handle's keyspace
// encoders do not match the requested K/V types (the caller passed the
// wrong type parameters), the returned query is permanently inert:
// every method yields nothing and Err() reports the mismatch.
func NewIndexQuery[K, V, IK any](h *IndexHandle, ikEnc Encoder[IK]) *IndexQuery[K, V, IK] {
	q := &IndexQuery[K, V, IK]{idx: h.idx, ikEnc: ikEnc}
	ke, ok := h.keyEnc.(Encoder[K])
	if !ok {
		q.bindErr = fmt.Errorf("gmdb: NewIndexQuery: keyspace key encoder type does not match K: %w", gmdb.ErrInvalidOptions)
		return q
	}
	ve, ok := h.valEnc.(Encoder[V])
	if !ok {
		q.bindErr = fmt.Errorf("gmdb: NewIndexQuery: keyspace value encoder type does not match V: %w", gmdb.ErrInvalidOptions)
		return q
	}
	q.keyEnc = ke
	q.valEnc = ve
	return q
}

// encodeIK encodes a single IK value to the one-column tuple the byte
// index API expects. Records an encode error on the query.
func (q *IndexQuery[K, V, IK]) encodeIK(ik IK) ([]byte, bool) {
	b, err := q.ikEnc.AppendEncode(nil, ik)
	if err != nil {
		q.err = err
		return nil, false
	}
	return b, true
}

// decodePair decodes a byte (pk, value) result into (K, V), recording
// the first decode error. For a SetKeyspace index the byte layer yields
// (setKey, setValue), decoded by the same K/V encoders.
func (q *IndexQuery[K, V, IK]) decodePair(pkb, vb []byte) (K, V, bool) {
	var zk K
	var zv V
	k, err := q.keyEnc.Decode(pkb)
	if err != nil {
		q.err = err
		return zk, zv, false
	}
	v, err := q.valEnc.Decode(vb)
	if err != nil {
		q.err = err
		return zk, zv, false
	}
	return k, v, true
}

// Lookup yields (K, V) pairs whose index key equals ik (exact match on
// the single IK column). For a unique index yields at most one pair.
func (q *IndexQuery[K, V, IK]) Lookup(ik IK) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if q.bindErr != nil {
			return
		}
		q.err = nil
		ikb, ok := q.encodeIK(ik)
		if !ok {
			return
		}
		for pkb, vb := range q.idx.Lookup([][]byte{ikb}) {
			k, v, ok := q.decodePair(pkb, vb)
			if !ok || !yield(k, v) {
				return
			}
		}
		if e := q.idx.Err(); e != nil {
			q.err = e
		}
	}
}

// LookupKeys yields matching keys (K) without value back-lookup. On a
// SetKeyspace index this surfaces an error via Err() (the byte layer
// has no single-key form for the compound (setKey,setValue) pair — use
// Lookup).
func (q *IndexQuery[K, V, IK]) LookupKeys(ik IK) iter.Seq[K] {
	return func(yield func(K) bool) {
		if q.bindErr != nil {
			return
		}
		q.err = nil
		ikb, ok := q.encodeIK(ik)
		if !ok {
			return
		}
		for pkb := range q.idx.LookupKeys([][]byte{ikb}) {
			k, err := q.keyEnc.Decode(pkb)
			if err != nil {
				q.err = err
				return
			}
			if !yield(k) {
				return
			}
		}
		if e := q.idx.Err(); e != nil {
			q.err = e
		}
	}
}

// Range yields (K, V) for index keys in [*start, *end) (nil = open
// boundary). Comparison is on the full IK value (the typed layer treats
// IK as one opaque column — see typed-keyspaces.md §Limitation:
// partial-prefix queries).
func (q *IndexQuery[K, V, IK]) Range(start, end *IK) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if q.bindErr != nil {
			return
		}
		q.err = nil
		startTuple, ok := q.boundTuple(start)
		if !ok {
			return
		}
		endTuple, ok := q.boundTuple(end)
		if !ok {
			return
		}
		for pkb, vb := range q.idx.Range(startTuple, endTuple) {
			k, v, ok := q.decodePair(pkb, vb)
			if !ok || !yield(k, v) {
				return
			}
		}
		if e := q.idx.Err(); e != nil {
			q.err = e
		}
	}
}

// boundTuple encodes an optional IK range boundary into the [][]byte
// column tuple the byte index Range expects: nil pointer → nil tuple
// (open). Records an encode error on the query.
func (q *IndexQuery[K, V, IK]) boundTuple(p *IK) ([][]byte, bool) {
	if p == nil {
		return nil, true
	}
	b, err := q.ikEnc.AppendEncode(nil, *p)
	if err != nil {
		q.err = err
		return nil, false
	}
	return [][]byte{b}, true
}

// Prefix yields (K, V) whose index key has the encoded prefix as a byte
// prefix. For a fixed-width IK encoder this is an exact match; see the
// composite-IK limitation in the spec.
func (q *IndexQuery[K, V, IK]) Prefix(prefix IK) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if q.bindErr != nil {
			return
		}
		q.err = nil
		pb, ok := q.encodeIK(prefix)
		if !ok {
			return
		}
		for pkb, vb := range q.idx.Prefix([][]byte{pb}) {
			k, v, ok := q.decodePair(pkb, vb)
			if !ok || !yield(k, v) {
				return
			}
		}
		if e := q.idx.Err(); e != nil {
			q.err = e
		}
	}
}

// Get is the unique-index shorthand: returns the single (K, V) matching
// ik, or gmdb.ErrNotFound. Returns gmdb.ErrIndexNotUnique on a non-unique index.
func (q *IndexQuery[K, V, IK]) Get(ik IK) (K, V, error) {
	var zk K
	var zv V
	if q.bindErr != nil {
		return zk, zv, q.bindErr
	}
	q.err = nil
	ikb, err := q.ikEnc.AppendEncode(nil, ik)
	if err != nil {
		return zk, zv, err
	}
	pkb, vb, err := q.idx.Get(ikb)
	if err != nil {
		return zk, zv, err
	}
	k, err := q.keyEnc.Decode(pkb)
	if err != nil {
		return zk, zv, err
	}
	v, err := q.valEnc.Decode(vb)
	if err != nil {
		return zk, zv, err
	}
	return k, v, nil
}

// Err returns the first error encountered during the last query. A
// permanent binding error (encoder-type mismatch from
// NewIndexQuery) takes precedence and is never reset; otherwise the
// per-sequence error (IK encode, result decode, or underlying index
// iteration), which is reset at the start of each Lookup / LookupKeys /
// Range / Prefix / Get.
func (q *IndexQuery[K, V, IK]) Err() error {
	if q.bindErr != nil {
		return q.bindErr
	}
	return q.err
}
