package gmdb

import (
	"fmt"
	"iter"
)

// Typed index layer (typed-keyspaces.md §Typed Indexes). A TypedIndex
// declares a type-safe secondary index on a TypedKeyspace[K,V] with an
// extracted index-key type IK; it lowers to a byte-layer *IndexDecl via
// the sealed AnyTypedIndex.indexDecl, threading the keyspace's K/V
// encoders so the extractor closure can decode the stored (key,value)
// before running the user's typed Extract and encoding each IK.
//
// Schema-hash drift (Inv-T7): the synthesized IndexColumn's Name is set
// to IKEnc.ID(). Since the byte schema-hash hashes column names (which
// are pure fingerprint inputs, never read at decode), swapping IKEnc for
// one with a different ID changes the column name and therefore the
// stored fingerprint — surfacing as ErrIndexFingerprintMismatch at Open.

// Typed covering indexes (a covering-value extractor + the byte-layer
// covering-return path) land in a later sub-chunk; TypedIndex has no
// Covering field until that wiring is in place end-to-end, so callers
// never declare covering that silently stores or returns nothing.

// TypedIndex declares a typed index on a TypedKeyspace[K,V] with index-
// key type IK.
//
// Extract produces zero or more IK values per row; an empty/nil slice
// skips the row (partial index). IKEnc encodes each IK to lex-safe
// bytes and MUST have a stable non-empty ID() (Inv-T2) — an empty ID is
// rejected at Open with ErrIndexEncoderIDEmpty.
//
// IKEnc MUST be able to encode every value Extract produces: the
// byte-layer index extractor is infallible (it cannot return an error),
// so an IKEnc.AppendEncode failure during maintenance PANICS with a
// descriptive error rather than silently dropping an index entry (which
// would diverge the index from the rows). For all canonical encoders
// except an out-of-range BENanosEncoder value this never fires; use
// infallible index-key encoders, or ensure Extract never yields an
// unrepresentable value.
type TypedIndex[K, V, IK any] struct {
	Name    string
	IKEnc   Encoder[IK]
	Unique  bool
	Version string
	Extract func(K, V) []IK
}

// Compile-time proof that *TypedIndex implements the sealed
// AnyTypedIndex — the only legal implementer (the unexported indexDecl
// method seals the interface to this package).
var _ AnyTypedIndex[int, int] = (*TypedIndex[int, int, int])(nil)

// indexDecl lowers the typed index to a byte *IndexDecl, threading the
// owning keyspace's encoders into the extractor closure. Implements
// AnyTypedIndex[K,V] (sealed). Returns ErrIndexEncoderIDEmpty if IKEnc's
// ID() is empty.
func (t *TypedIndex[K, V, IK]) indexDecl(keyEnc Encoder[K], valEnc Encoder[V]) (*IndexDecl, error) {
	ikID := t.IKEnc.ID()
	if ikID == "" {
		return nil, fmt.Errorf("gmdb: typed index %q index-key encoder: %w", t.Name, ErrIndexEncoderIDEmpty)
	}
	extract := t.makeExtractor(keyEnc, valEnc)
	return &IndexDecl{
		Name: t.Name,
		// One opaque column for the IK; its Name = IKEnc.ID() folds the
		// encoder identity into the schema-hash fingerprint (Inv-T7).
		Columns: []IndexColumn{{Name: ikID}},
		Unique:  t.Unique,
		Version: t.Version,
		Extract: extract,
	}, nil
}

// makeExtractor builds the byte-layer IndexExtractor closure: decode the
// stored (key,value) into (K,V), run the typed Extract, encode each IK.
// Decode/encode failures panic (see the TypedIndex godoc): the byte
// extractor contract is total, and silently dropping an entry would
// diverge the index from the rows.
func (t *TypedIndex[K, V, IK]) makeExtractor(keyEnc Encoder[K], valEnc Encoder[V]) IndexExtractor {
	return func(keyBytes, valueBytes []byte) []IndexEntry {
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
		entries := make([]IndexEntry, 0, len(iks))
		for _, ik := range iks {
			ikb, err := t.IKEnc.AppendEncode(nil, ik)
			if err != nil {
				panic(fmt.Errorf("gmdb: typed index %q: encode index key: %w", t.Name, err))
			}
			entries = append(entries, IndexEntry{Cols: [][]byte{ikb}})
		}
		return entries
	}
}

// Index returns a typed query handle for the named index on this typed
// keyspace. The returned *TypedIndexHandle carries the keyspace's K/V
// encoders (type-erased); bind it to a specific IK type with
// NewTypedIndexQuery. Returns ErrIndexNotFound if no such index.
func (t *TypedKS[K, V]) Index(name string) (*TypedIndexHandle, error) {
	idx, err := t.ks.Index(name)
	if err != nil {
		return nil, err
	}
	return &TypedIndexHandle{idx: idx, keyEnc: t.keyEnc, valEnc: t.valEnc}, nil
}

// Index returns a typed query handle for the named index on this typed
// set keyspace. For a SetKeyspace index the query yields (setKey,
// setValue) pairs, so bind K = the set-key type and V = the set-value
// type in NewTypedIndexQuery.
func (t *TypedSetKS[K, V]) Index(name string) (*TypedIndexHandle, error) {
	idx, err := t.sks.Index(name)
	if err != nil {
		return nil, err
	}
	return &TypedIndexHandle{idx: idx, keyEnc: t.keyEnc, valEnc: t.valEnc}, nil
}

// TypedIndexHandle is a type-erased handle to an opened index on a typed
// keyspace. It carries the byte *Index plus the keyspace's K/V encoders
// (as any); NewTypedIndexQuery re-introduces the static K/V/IK types.
type TypedIndexHandle struct {
	idx    *Index
	keyEnc any // Encoder[K] of the owning keyspace
	valEnc any // Encoder[V]
}

// TypedIndexQuery is a statically-typed query over an index whose
// index-key type is IK and whose owning keyspace is keyed/valued by
// K/V. Construct via NewTypedIndexQuery. Like the byte *Index, a query
// handle is not safe for concurrent iteration; Err() is per-handle.
type TypedIndexQuery[K, V, IK any] struct {
	idx     *Index
	keyEnc  Encoder[K]
	valEnc  Encoder[V]
	ikEnc   Encoder[IK]
	bindErr error // permanent: encoder-type mismatch at construction (query inert)
	err     error // per-sequence: IK encode / result decode / iteration error
}

// NewTypedIndexQuery binds a TypedIndexHandle to concrete K/V/IK types
// with the supplied index-key encoder. If the handle's keyspace
// encoders do not match the requested K/V types (the caller passed the
// wrong type parameters), the returned query is permanently inert:
// every method yields nothing and Err() reports the mismatch.
func NewTypedIndexQuery[K, V, IK any](h *TypedIndexHandle, ikEnc Encoder[IK]) *TypedIndexQuery[K, V, IK] {
	q := &TypedIndexQuery[K, V, IK]{idx: h.idx, ikEnc: ikEnc}
	ke, ok := h.keyEnc.(Encoder[K])
	if !ok {
		q.bindErr = fmt.Errorf("gmdb: NewTypedIndexQuery: keyspace key encoder type does not match K: %w", ErrInvalidOptions)
		return q
	}
	ve, ok := h.valEnc.(Encoder[V])
	if !ok {
		q.bindErr = fmt.Errorf("gmdb: NewTypedIndexQuery: keyspace value encoder type does not match V: %w", ErrInvalidOptions)
		return q
	}
	q.keyEnc = ke
	q.valEnc = ve
	return q
}

// encodeIK encodes a single IK value to the one-column tuple the byte
// index API expects. Records an encode error on the query.
func (q *TypedIndexQuery[K, V, IK]) encodeIK(ik IK) ([]byte, bool) {
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
func (q *TypedIndexQuery[K, V, IK]) decodePair(pkb, vb []byte) (K, V, bool) {
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
func (q *TypedIndexQuery[K, V, IK]) Lookup(ik IK) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if q.bindErr != nil {
			return
		}
		q.err = nil
		ikb, ok := q.encodeIK(ik)
		if !ok {
			return
		}
		for pkb, vb := range q.idx.Lookup(ikb) {
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
func (q *TypedIndexQuery[K, V, IK]) LookupKeys(ik IK) iter.Seq[K] {
	return func(yield func(K) bool) {
		if q.bindErr != nil {
			return
		}
		q.err = nil
		ikb, ok := q.encodeIK(ik)
		if !ok {
			return
		}
		for pkb := range q.idx.LookupKeys(ikb) {
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
func (q *TypedIndexQuery[K, V, IK]) Range(start, end *IK) iter.Seq2[K, V] {
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
func (q *TypedIndexQuery[K, V, IK]) boundTuple(p *IK) ([][]byte, bool) {
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
func (q *TypedIndexQuery[K, V, IK]) Prefix(prefix IK) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		if q.bindErr != nil {
			return
		}
		q.err = nil
		pb, ok := q.encodeIK(prefix)
		if !ok {
			return
		}
		for pkb, vb := range q.idx.Prefix(pb) {
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
// ik, or ErrNotFound. Returns ErrIndexNotUnique on a non-unique index.
func (q *TypedIndexQuery[K, V, IK]) Get(ik IK) (K, V, error) {
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
// NewTypedIndexQuery) takes precedence and is never reset; otherwise the
// per-sequence error (IK encode, result decode, or underlying index
// iteration), which is reset at the start of each Lookup / LookupKeys /
// Range / Prefix / Get.
func (q *TypedIndexQuery[K, V, IK]) Err() error {
	if q.bindErr != nil {
		return q.bindErr
	}
	return q.err
}
