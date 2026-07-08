package gmdb

import (
	"iter"
)

// Typed keyspace layer (typed-keyspaces.md §Typed Keyspace). A
// zero-cost abstraction over the byte-oriented Keyspace: every method
// encodes its K / V arguments through the keyspace's Encoder[K] /
// Encoder[V] and delegates to the corresponding byte-layer method,
// decoding results on the way out. The key encoder MUST produce
// lexicographically ordered output (typed-keyspaces.md §Invariants) so range / prefix / cursor
// order matches the intended key order.

// AnyTypedIndex is the type-erased interface satisfied by every
// TypedIndex[K, V, IK] (typed-keyspaces.md §Typed Indexes). It exists
// so one Open / Create call can declare indexes with heterogeneous IK
// types in a single variadic argument.
//
// The interface is intentionally SEALED — indexDecl is unexported, so
// only types in this package implement it (in practice only
// *TypedIndex[K, V, IK]). The
// engine relies on every supplied *IndexDecl having been constructed
// through the typed-index path, which guarantees encoder-ID
// consistency, deterministic schema-hash, and well-formed extractor
// wiring; a user-supplied implementation could bypass these. Library
// code that needs to decorate a typed index composes at the Extract
// func level, not at this interface.
//
// indexDecl receives the owning keyspace's key / value encoders so the
// concrete TypedIndex can build the byte-layer extractor closure
// (decode (key,value) → (K,V), run the typed Extract, encode each IK)
// and validate encoder IDs; it returns ErrIndexEncoderIDEmpty for an
// empty IKEnc / covering encoder ID.
type AnyTypedIndex[K, V any] interface {
	indexDecl(keyEnc Encoder[K], valEnc Encoder[V]) (*IndexDecl, error)
}

// TypedKeyspace wraps a single-value Keyspace with type-safe encoding.
// It is a stateless descriptor (name + encoders); Open / Create return
// a transaction-scoped TypedKeyspaceHandle.
type TypedKeyspace[K, V any] struct {
	name   string
	keyEnc Encoder[K]
	valEnc Encoder[V]
}

// NewTypedKeyspace creates a typed keyspace descriptor. keyEnc MUST
// produce lexicographically ordered output for the desired key order
// (typed-keyspaces.md §Invariants; for uint64 keys big-endian, for strings the natural byte
// representation — see the canonical encoders).
func NewTypedKeyspace[K, V any](name string, keyEnc Encoder[K], valEnc Encoder[V]) *TypedKeyspace[K, V] {
	return &TypedKeyspace[K, V]{name: name, keyEnc: keyEnc, valEnc: valEnc}
}

// buildTypedIndexDecls lowers typed index declarations to byte-layer
// *IndexDecl, threading the keyspace's encoders so each TypedIndex can
// build its extractor closure and validate encoder IDs. A nil/empty
// slice yields a nil decl slice (indexless keyspace). Shared by the
// Keyspace and SetKeyspace typed factories.
func buildTypedIndexDecls[K, V any](keyEnc Encoder[K], valEnc Encoder[V], indexes []AnyTypedIndex[K, V]) ([]*IndexDecl, error) {
	if len(indexes) == 0 {
		return nil, nil
	}
	decls := make([]*IndexDecl, 0, len(indexes))
	for _, idx := range indexes {
		d, err := idx.indexDecl(keyEnc, valEnc)
		if err != nil {
			return nil, err
		}
		decls = append(decls, d)
	}
	return decls, nil
}

// openTypedHandle translates the typed index declarations, invokes the
// byte-layer open/create call with them, and wraps the resulting byte
// handle. Shared by the TypedKeyspace and TypedSetKeyspace
// Open / Create / CreateIfNotExists methods — the only per-call
// difference is the byte target (byteOpen) and the handle wrap.
func openTypedHandle[K, V, BK, H any](
	keyEnc Encoder[K], valEnc Encoder[V],
	indexes []AnyTypedIndex[K, V],
	byteOpen func(decls []*IndexDecl) (BK, error),
	wrap func(BK) H,
) (H, error) {
	decls, err := buildTypedIndexDecls(keyEnc, valEnc, indexes)
	if err != nil {
		var zero H
		return zero, err
	}
	bk, err := byteOpen(decls)
	if err != nil {
		var zero H
		return zero, err
	}
	return wrap(bk), nil
}

// Open opens the keyspace for read+write within tx, declaring the
// supplied typed indexes. Delegates to Tx.OpenKeyspace; index drift,
// missing/extra indexes, and encoder-ID errors surface from there
// (indexing.md §Open Semantics + ErrIndexEncoderIDEmpty).
func (tks *TypedKeyspace[K, V]) Open(tx *Tx, indexes ...AnyTypedIndex[K, V]) (*TypedKeyspaceHandle[K, V], error) {
	return openTypedHandle(tks.keyEnc, tks.valEnc, indexes,
		func(decls []*IndexDecl) (*Keyspace, error) { return tx.OpenKeyspace(tks.name, decls...) },
		tks.wrap)
}

// OpenReadOnly opens the keyspace for reads only (no index decls; index
// lookups still work against stored entries). Mutations on the returned
// handle return ErrReadOnly.
func (tks *TypedKeyspace[K, V]) OpenReadOnly(tx *Tx) (*TypedKeyspaceHandle[K, V], error) {
	ks, err := tx.OpenKeyspaceReadOnly(tks.name)
	if err != nil {
		return nil, err
	}
	return tks.wrap(ks), nil
}

// Create creates the keyspace (error if it already exists) with the
// supplied typed indexes and returns a write handle.
func (tks *TypedKeyspace[K, V]) Create(tx *Tx, indexes ...AnyTypedIndex[K, V]) (*TypedKeyspaceHandle[K, V], error) {
	return openTypedHandle(tks.keyEnc, tks.valEnc, indexes,
		func(decls []*IndexDecl) (*Keyspace, error) { return tx.CreateKeyspace(tks.name, decls...) },
		tks.wrap)
}

// CreateIfNotExists creates the keyspace if absent, else opens the
// existing one (which must match the supplied index set per the
// byte-layer re-open rules).
func (tks *TypedKeyspace[K, V]) CreateIfNotExists(tx *Tx, indexes ...AnyTypedIndex[K, V]) (*TypedKeyspaceHandle[K, V], error) {
	return openTypedHandle(tks.keyEnc, tks.valEnc, indexes,
		func(decls []*IndexDecl) (*Keyspace, error) { return tx.CreateKeyspaceIfNotExists(tks.name, decls...) },
		tks.wrap)
}

func (tks *TypedKeyspace[K, V]) wrap(ks *Keyspace) *TypedKeyspaceHandle[K, V] {
	return &TypedKeyspaceHandle[K, V]{ks: ks, keyEnc: tks.keyEnc, valEnc: tks.valEnc}
}

// TypedKeyspaceHandle is a handle to an opened typed keyspace within a transaction.
// Valid for the lifetime of the owning transaction.
type TypedKeyspaceHandle[K, V any] struct {
	ks     *Keyspace
	keyEnc Encoder[K]
	valEnc Encoder[V]
}

// Get returns the value for key, or the zero V and ErrNotFound if the
// key is absent. Encoder Decode errors (malformed stored bytes) and
// ErrKeyEmpty (a key that encodes to empty bytes) propagate from the
// byte layer / encoder.
func (t *TypedKeyspaceHandle[K, V]) Get(key K) (V, error) {
	var zero V
	kb, err := t.keyEnc.AppendEncode(nil, key)
	if err != nil {
		return zero, err
	}
	vb, err := t.ks.Get(kb)
	if err != nil {
		return zero, err
	}
	return t.valEnc.Decode(vb)
}

// Put inserts or replaces (key, value). A value that encodes to empty
// bytes is stored as empty (the byte layer's nil-value-as-empty rule);
// a key that encodes to empty bytes returns ErrKeyEmpty.
func (t *TypedKeyspaceHandle[K, V]) Put(key K, value V) error {
	kb, err := t.keyEnc.AppendEncode(nil, key)
	if err != nil {
		return err
	}
	vb, err := t.valEnc.AppendEncode(nil, value)
	if err != nil {
		return err
	}
	return t.ks.Put(kb, vb)
}

// Delete removes key, returning ErrNotFound if it does not exist
// (api-surface.md §Invariants — keyed-removal returns ErrNotFound on
// miss).
func (t *TypedKeyspaceHandle[K, V]) Delete(key K) error {
	kb, err := t.keyEnc.AppendEncode(nil, key)
	if err != nil {
		return err
	}
	return t.ks.Delete(kb)
}

// DeleteRange deletes every key k with *start <= k < *end, returning the
// number deleted. A nil pointer is an open boundary (nil start = from
// the beginning, nil end = through the last key, (nil,nil) = all). A
// non-nil boundary that encodes to empty bytes is rejected with
// ErrKeyEmpty rather than silently collapsing to an open boundary.
// Returns (0, nil) for an empty range.
func (t *TypedKeyspaceHandle[K, V]) DeleteRange(start, end *K) (uint64, error) {
	sb, err := encodeBound(t.keyEnc, start)
	if err != nil {
		return 0, err
	}
	eb, err := encodeBound(t.keyEnc, end)
	if err != nil {
		return 0, err
	}
	return t.ks.DeleteRange(sb, eb)
}

// encodeBound encodes an optional typed range boundary for Range /
// DeleteRange. A nil pointer is the open-boundary sentinel (returns nil
// bytes, which the byte layer reads as "open"). A non-nil pointer is
// encoded; if the encoding is empty it is returned as a NON-nil empty
// slice so the byte layer does not misread a real boundary as open —
// for DeleteRange the byte layer then rejects the degenerate empty-key
// boundary with ErrKeyEmpty; for Range the cursor treats an empty lower
// bound as "from the beginning" and an empty exclusive upper bound as
// the empty range (both correct: an empty key cannot exist anyway).
func encodeBound[T any](enc Encoder[T], p *T) ([]byte, error) {
	if p == nil {
		return nil, nil
	}
	b, err := enc.AppendEncode(nil, *p)
	if err != nil {
		return nil, err
	}
	if b == nil {
		b = []byte{}
	}
	return b, nil
}

// Cursor returns a typed cursor over the keyspace. Navigation methods
// return (K, V, ok); ok is false at end-of-iteration, when unpositioned,
// or after a decode/encode error — Err() distinguishes those states
// (transactions.md §Cursor State Machine, mirrored for the typed layer).
func (t *TypedKeyspaceHandle[K, V]) Cursor() *TypedCursor[K, V] {
	return &TypedCursor[K, V]{bc: t.ks.Cursor(), keyEnc: t.keyEnc, valEnc: t.valEnc}
}

// All yields every (key, value) in ascending key order. Range yields
// pairs with *start <= key < *end (nil pointer = open boundary). Prefix
// yields pairs whose encoded key has the encoded prefix as a byte
// prefix — for a fixed-width key encoder (e.g. uint64) this is an exact
// match; for variable-width encoders (string/bytes) it is a true
// prefix.
//
// These convenience iterators are best-effort: a cursor I/O error or a
// decode error ENDS the sequence (it yields nothing further). Callers
// that must observe such errors should iterate with Cursor() and check
// Err(); the bare iter.Seq2 has no error channel by design (matching
// the spec's typed-iterator surface).
func (t *TypedKeyspaceHandle[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		for kb, vb := range t.ks.All() {
			k, v, ok := decodeKV(t.keyEnc, t.valEnc, kb, vb)
			if !ok || !yield(k, v) {
				return
			}
		}
	}
}

func (t *TypedKeyspaceHandle[K, V]) Range(start, end *K) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		sb, err := encodeBound(t.keyEnc, start)
		if err != nil {
			return
		}
		eb, err := encodeBound(t.keyEnc, end)
		if err != nil {
			return
		}
		for kb, vb := range t.ks.Range(sb, eb) {
			k, v, ok := decodeKV(t.keyEnc, t.valEnc, kb, vb)
			if !ok || !yield(k, v) {
				return
			}
		}
	}
}

func (t *TypedKeyspaceHandle[K, V]) Prefix(prefix K) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		pb, err := t.keyEnc.AppendEncode(nil, prefix)
		if err != nil {
			return
		}
		for kb, vb := range t.ks.Prefix(pb) {
			k, v, ok := decodeKV(t.keyEnc, t.valEnc, kb, vb)
			if !ok || !yield(k, v) {
				return
			}
		}
	}
}

// decodeKV decodes a borrowed (key, value) byte pair into owned (K, V).
// ok is false if either decode fails (the convenience iterators end on
// !ok). The Decode contract forbids retaining the borrowed src, so the
// returned K / V are independent of kb / vb.
func decodeKV[K, V any](keyEnc Encoder[K], valEnc Encoder[V], kb, vb []byte) (K, V, bool) {
	var zk K
	var zv V
	k, err := keyEnc.Decode(kb)
	if err != nil {
		return zk, zv, false
	}
	v, err := valEnc.Decode(vb)
	if err != nil {
		return zk, zv, false
	}
	return k, v, true
}

// TypedCursor is a type-safe cursor over a TypedKeyspaceHandle. Mirrors the byte
// Cursor (transactions.md §Cursor State Machine) with K / V
// decoding. A decode or encode error is sticky and surfaces via Err().
type TypedCursor[K, V any] struct {
	bc     *Cursor
	keyEnc Encoder[K]
	valEnc Encoder[V]
	err    error
}

// decode lowers a byte navigation result to (K, V, ok), recording the
// first decode error on the cursor. A nil key (end / unpositioned)
// yields ok=false with no error.
func (c *TypedCursor[K, V]) decode(kb, vb []byte) (K, V, bool) {
	var zk K
	var zv V
	if kb == nil {
		return zk, zv, false
	}
	k, err := c.keyEnc.Decode(kb)
	if err != nil {
		if c.err == nil {
			c.err = err
		}
		return zk, zv, false
	}
	v, err := c.valEnc.Decode(vb)
	if err != nil {
		if c.err == nil {
			c.err = err
		}
		return zk, zv, false
	}
	return k, v, true
}

func (c *TypedCursor[K, V]) First() (K, V, bool)   { return c.decode(c.bc.First()) }
func (c *TypedCursor[K, V]) Last() (K, V, bool)    { return c.decode(c.bc.Last()) }
func (c *TypedCursor[K, V]) Next() (K, V, bool)    { return c.decode(c.bc.Next()) }
func (c *TypedCursor[K, V]) Prev() (K, V, bool)    { return c.decode(c.bc.Prev()) }
func (c *TypedCursor[K, V]) Current() (K, V, bool) { return c.decode(c.bc.Current()) }

// Seek positions at target if present; on a miss the cursor goes to
// end-of-iteration (matching the byte Cursor.Seek — use SeekGE for the
// first key >= target). SeekGE positions at the first key >= target. An
// encode error on target is recorded (Err()) and returns ok=false.
func (c *TypedCursor[K, V]) Seek(target K) (K, V, bool) {
	tb, err := c.keyEnc.AppendEncode(nil, target)
	if err != nil {
		if c.err == nil {
			c.err = err
		}
		var zk K
		var zv V
		return zk, zv, false
	}
	return c.decode(c.bc.Seek(tb))
}

func (c *TypedCursor[K, V]) SeekGE(target K) (K, V, bool) {
	tb, err := c.keyEnc.AppendEncode(nil, target)
	if err != nil {
		if c.err == nil {
			c.err = err
		}
		var zk K
		var zv V
		return zk, zv, false
	}
	return c.decode(c.bc.SeekGE(tb))
}

// Delete removes the current entry (same semantics as Cursor.Delete).
func (c *TypedCursor[K, V]) Delete() error { return c.bc.Delete() }

// Err returns the first error encountered: a sticky decode/encode error
// from the typed layer takes precedence, else the byte cursor's error.
func (c *TypedCursor[K, V]) Err() error {
	if c.err != nil {
		return c.err
	}
	return c.bc.Err()
}
