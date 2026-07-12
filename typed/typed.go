package typed

import (
	"errors"
	"iter"

	"github.com/greatliontech/gmdb"
	"github.com/greatliontech/gmdb/internal/qrep"
)

// Typed keyspace layer (typed-keyspaces.md §Typed Keyspace). A
// zero-cost abstraction over the byte-oriented Keyspace: every method
// encodes its K / V arguments through the keyspace's Encoder[K] /
// Encoder[V] and delegates to the corresponding byte-layer method,
// decoding results on the way out. The key encoder MUST produce
// lexicographically ordered output (typed-keyspaces.md §Invariants) so range / prefix / cursor
// order matches the intended key order.

// AnyIndex is the type-erased interface satisfied by every
// Index[K, V, IK] (typed-keyspaces.md §Typed Indexes). It exists
// so one Open / Create call can declare indexes with heterogeneous IK
// types in a single variadic argument.
//
// The interface is intentionally SEALED — indexDecl is unexported, so
// only types in this package implement it (in practice
// *Index[K, V, IK] and *ColumnIndex[K, V]). The
// engine relies on every supplied *gmdb.IndexDecl having been constructed
// through the typed-index path, which guarantees encoder-ID
// consistency, deterministic schema-hash, and well-formed extractor
// wiring; a user-supplied implementation could bypass these. Library
// code that needs to decorate a typed index composes at the Extract
// func level, not at this interface.
//
// indexDecl receives the owning keyspace's key / value encoders so the
// concrete Index can build the byte-layer extractor closure
// (decode (key,value) → (K,V), run the typed Extract, encode each IK)
// and validate encoder IDs; it returns gmdb.ErrIndexEncoderIDEmpty for an
// empty IKEnc / covering encoder ID.
type AnyIndex[K, V any] interface {
	indexDecl(keyEnc Encoder[K], valEnc Encoder[V]) (*gmdb.IndexDecl, error)
}

// Keyspace wraps a single-value gmdb.Keyspace with type-safe encoding.
// It is a stateless descriptor (name + encoders); Open / Create return
// a transaction-scoped KeyspaceHandle.
type Keyspace[K, V any] struct {
	name   string
	keyEnc Encoder[K]
	valEnc Encoder[V]
}

// NewKeyspace creates a typed keyspace descriptor. keyEnc MUST
// produce lexicographically ordered output for the desired key order
// (typed-keyspaces.md §Invariants; for uint64 keys big-endian, for strings the natural byte
// representation — see the canonical encoders).
func NewKeyspace[K, V any](name string, keyEnc Encoder[K], valEnc Encoder[V]) *Keyspace[K, V] {
	return &Keyspace[K, V]{name: name, keyEnc: keyEnc, valEnc: valEnc}
}

// plannerCandidate is the optional interface a typed index
// declaration implements to advertise itself to the query planner.
// Only *ColumnIndex does: an opaque Index[K, V, IK] has no
// per-column structure to plan over (query-builder.md §Planning
// rules — rule 2 enumerates ColumnIndexes only).
type plannerCandidate interface {
	plannerInfo() qrep.IndexInfo
}

// buildIndexDecls lowers typed index declarations to byte-layer
// *gmdb.IndexDecl, threading the keyspace's encoders so each Index can
// build its extractor closure and validate encoder IDs. A nil/empty
// slice yields a nil decl slice (indexless keyspace). Shared by the
// Keyspace and SetKeyspace typed factories. The second result is
// the planner's distilled view of the ColumnIndex declarations
// (qrep.IndexInfo) — derived here, from the same declaration
// values the lowering consumes, so the two views cannot diverge.
func buildIndexDecls[K, V any](keyEnc Encoder[K], valEnc Encoder[V], indexes []AnyIndex[K, V]) ([]*gmdb.IndexDecl, []qrep.IndexInfo, error) {
	if len(indexes) == 0 {
		return nil, nil, nil
	}
	decls := make([]*gmdb.IndexDecl, 0, len(indexes))
	var infos []qrep.IndexInfo
	for _, idx := range indexes {
		d, err := idx.indexDecl(keyEnc, valEnc)
		if err != nil {
			return nil, nil, err
		}
		decls = append(decls, d)
		if pc, ok := idx.(plannerCandidate); ok {
			infos = append(infos, pc.plannerInfo())
		}
	}
	return decls, infos, nil
}

// openTypedHandle translates the typed index declarations, invokes the
// byte-layer open/create call with them, and wraps the resulting byte
// handle. Shared by the Keyspace and SetKeyspace
// Open / Create / CreateIfNotExists methods — the only per-call
// difference is the byte target (byteOpen) and the handle wrap.
func openTypedHandle[K, V, BK, H any](
	keyEnc Encoder[K], valEnc Encoder[V],
	indexes []AnyIndex[K, V],
	byteOpen func(decls []*gmdb.IndexDecl) (BK, error),
	wrap func(BK, []qrep.IndexInfo) H,
) (H, error) {
	decls, infos, err := buildIndexDecls(keyEnc, valEnc, indexes)
	if err != nil {
		var zero H
		return zero, err
	}
	bk, err := byteOpen(decls)
	if err != nil {
		var zero H
		return zero, err
	}
	return wrap(bk, infos), nil
}

// Open opens the keyspace for read+write within tx, declaring the
// supplied typed indexes. Delegates to gmdb.Tx.OpenKeyspace; index drift,
// missing/extra indexes, and encoder-ID errors surface from there
// (indexing.md §Open Semantics + gmdb.ErrIndexEncoderIDEmpty).
func (tks *Keyspace[K, V]) Open(tx *gmdb.Tx, indexes ...AnyIndex[K, V]) (*KeyspaceHandle[K, V], error) {
	return openTypedHandle(tks.keyEnc, tks.valEnc, indexes,
		func(decls []*gmdb.IndexDecl) (*gmdb.Keyspace, error) { return tx.OpenKeyspace(tks.name, decls...) },
		tks.wrap)
}

// ReadOpener is the read-only keyspace-open surface the typed tier
// requires of a transaction: both *gmdb.Tx and *gmdb.ReadTx satisfy it,
// so one OpenReadOnly entry point serves write transactions and
// snapshot read transactions (db.View / db.BeginRead). Over a ReadTx
// the returned handle observes the snapshot's consistent, immutable
// view — the engine's single-writer + N-snapshot-readers path — and is
// bound to that ReadTx's lifetime: once it closes, error-returning
// operations return gmdb.ErrTxClosed and iterator construction panics
// per the byte tier's construction-panic rule (api-surface.md §Range
// Iterators). A ReadTx serves one goroutine; open one per goroutine
// for concurrent reads.
type ReadOpener interface {
	OpenKeyspaceReadOnly(name string) (*gmdb.Keyspace, error)
	OpenSetKeyspaceReadOnly(name string) (*gmdb.SetKeyspace, error)
}

// OpenReadOnly opens the keyspace for reads only (no index decls; index
// lookups still work against stored entries). src is either a write
// transaction (*gmdb.Tx) or a snapshot read transaction (*gmdb.ReadTx);
// the handle is bound to src's lifetime. Mutations on the returned
// handle return gmdb.ErrReadOnly. With no declarations supplied the
// handle carries no planner index metadata — queries over it plan as
// full scans (results identical per Inv-QB1; plan choice is cost-only).
func (tks *Keyspace[K, V]) OpenReadOnly(src ReadOpener) (*KeyspaceHandle[K, V], error) {
	ks, err := src.OpenKeyspaceReadOnly(tks.name)
	if err != nil {
		return nil, err
	}
	return tks.wrap(ks, nil), nil
}

// Create creates the keyspace (error if it already exists) with the
// supplied typed indexes and returns a write handle.
func (tks *Keyspace[K, V]) Create(tx *gmdb.Tx, indexes ...AnyIndex[K, V]) (*KeyspaceHandle[K, V], error) {
	return openTypedHandle(tks.keyEnc, tks.valEnc, indexes,
		func(decls []*gmdb.IndexDecl) (*gmdb.Keyspace, error) { return tx.CreateKeyspace(tks.name, decls...) },
		tks.wrap)
}

// CreateIfNotExists creates the keyspace if absent, else opens the
// existing one (which must match the supplied index set per the
// byte-layer re-open rules).
func (tks *Keyspace[K, V]) CreateIfNotExists(tx *gmdb.Tx, indexes ...AnyIndex[K, V]) (*KeyspaceHandle[K, V], error) {
	return openTypedHandle(tks.keyEnc, tks.valEnc, indexes,
		func(decls []*gmdb.IndexDecl) (*gmdb.Keyspace, error) {
			return tx.CreateKeyspaceIfNotExists(tks.name, decls...)
		},
		tks.wrap)
}

func (tks *Keyspace[K, V]) wrap(ks *gmdb.Keyspace, infos []qrep.IndexInfo) *KeyspaceHandle[K, V] {
	return &KeyspaceHandle[K, V]{ks: ks, keyEnc: tks.keyEnc, valEnc: tks.valEnc, idxInfo: infos}
}

// KeyspaceHandle is a handle to an opened typed keyspace within a transaction.
// Valid for the lifetime of the owning transaction.
type KeyspaceHandle[K, V any] struct {
	ks     *gmdb.Keyspace
	keyEnc Encoder[K]
	valEnc Encoder[V]
	// idxInfo is the planner's distilled view of the ColumnIndex
	// declarations this handle was opened with (nil for
	// OpenReadOnly and decl-less opens).
	idxInfo []qrep.IndexInfo
}

// Get returns the value for key, or the zero V and gmdb.ErrNotFound if the
// key is absent. Encoder Decode errors (malformed stored bytes) and
// gmdb.ErrKeyEmpty (a key that encodes to empty bytes) propagate from the
// byte layer / encoder.
func (t *KeyspaceHandle[K, V]) Get(key K) (V, error) {
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
// a key that encodes to empty bytes returns gmdb.ErrKeyEmpty.
func (t *KeyspaceHandle[K, V]) Put(key K, value V) error {
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

// Delete removes key, returning gmdb.ErrNotFound if it does not exist
// (api-surface.md §Invariants — keyed-removal returns gmdb.ErrNotFound on
// miss).
func (t *KeyspaceHandle[K, V]) Delete(key K) error {
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
// gmdb.ErrKeyEmpty rather than silently collapsing to an open boundary.
// Returns (0, nil) for an empty range.
func (t *KeyspaceHandle[K, V]) DeleteRange(start, end *K) (uint64, error) {
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
// boundary with gmdb.ErrKeyEmpty; for Range the cursor treats an empty lower
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
func (t *KeyspaceHandle[K, V]) Cursor() *Cursor[K, V] {
	return &Cursor[K, V]{bc: t.ks.Cursor(), keyEnc: t.keyEnc, valEnc: t.valEnc}
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
func (t *KeyspaceHandle[K, V]) All() iter.Seq2[K, V] {
	seq := t.ks.All() // eager: the construction guard fires HERE, not at loop start
	return func(yield func(K, V) bool) {
		for kb, vb := range seq {
			k, v, ok := decodeKV(t.keyEnc, t.valEnc, kb, vb)
			if !ok || !yield(k, v) {
				return
			}
		}
	}
}

func (t *KeyspaceHandle[K, V]) Range(start, end *K) iter.Seq2[K, V] {
	// Bounds encode and the raw seq construct EAGERLY, so the
	// construction guard fires here and a post-construction state
	// change ends the sequence silently — uniform with the raw
	// iterators and typed All. An encode failure keeps its existing
	// contract: a silently empty sequence.
	sb, serr := encodeBound(t.keyEnc, start)
	eb, eerr := encodeBound(t.keyEnc, end)
	if serr != nil || eerr != nil {
		t.ks.GuardIterConstruction()
		return func(yield func(K, V) bool) {}
	}
	seq := t.ks.Range(sb, eb)
	return func(yield func(K, V) bool) {
		for kb, vb := range seq {
			k, v, ok := decodeKV(t.keyEnc, t.valEnc, kb, vb)
			if !ok || !yield(k, v) {
				return
			}
		}
	}
}

func (t *KeyspaceHandle[K, V]) Prefix(prefix K) iter.Seq2[K, V] {
	// Eager construction — see Range.
	pb, perr := t.keyEnc.AppendEncode(nil, prefix)
	if perr != nil {
		t.ks.GuardIterConstruction()
		return func(yield func(K, V) bool) {}
	}
	seq := t.ks.Prefix(pb)
	return func(yield func(K, V) bool) {
		for kb, vb := range seq {
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

// Cursor is a type-safe cursor over a KeyspaceHandle. Mirrors the byte
// Cursor (transactions.md §Cursor State Machine) with K / V
// decoding. A decode or encode error is sticky and surfaces via Err().
type Cursor[K, V any] struct {
	bc     *gmdb.Cursor
	keyEnc Encoder[K]
	valEnc Encoder[V]
	err    error
}

// decode lowers a byte navigation result to (K, V, ok), recording the
// first decode error on the cursor. A nil key (end / unpositioned)
// yields ok=false with no error.
func (c *Cursor[K, V]) decode(kb, vb []byte) (K, V, bool) {
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

func (c *Cursor[K, V]) First() (K, V, bool)   { return c.decode(c.bc.First()) }
func (c *Cursor[K, V]) Last() (K, V, bool)    { return c.decode(c.bc.Last()) }
func (c *Cursor[K, V]) Next() (K, V, bool)    { return c.decode(c.bc.Next()) }
func (c *Cursor[K, V]) Prev() (K, V, bool)    { return c.decode(c.bc.Prev()) }
func (c *Cursor[K, V]) Current() (K, V, bool) { return c.decode(c.bc.Current()) }

// Seek positions at target if present; on a miss the cursor goes to
// end-of-iteration (matching the byte Cursor.Seek — use SeekGE for the
// first key >= target). SeekGE positions at the first key >= target. An
// encode error on target is recorded (Err()) and returns ok=false.
func (c *Cursor[K, V]) Seek(target K) (K, V, bool) {
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

func (c *Cursor[K, V]) SeekGE(target K) (K, V, bool) {
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

// Delete removes the current entry (same semantics as gmdb.Cursor.Delete).
func (c *Cursor[K, V]) Delete() error { return c.bc.Delete() }

// Close releases the cursor before the transaction ends (same
// semantics as gmdb.Cursor.Close: unregisters from staleness
// tracking; subsequent operations surface gmdb.ErrCursorClosed;
// terminal, idempotent, optional).
func (c *Cursor[K, V]) Close() { c.bc.Close() }

// ByteIndex returns the byte-oriented handle for an index declared
// on this keyspace — the typed→byte bridge the query executor's
// plan leaves iterate (typed.IndexQuery is IK-opaque and cannot
// serve per-column entry bytes; query-builder.md §Byte-surface
// requirements). Each call returns a FRESH *gmdb.IndexHandle,
// exactly like gmdb.Keyspace.Index — the per-handle Err state
// makes handle sharing between concurrently-draining iterators
// mutually clobbering, so the executor obtains one per plan leaf
// per execution. Returns gmdb.ErrIndexNotFound for an unknown
// name.
func (t *KeyspaceHandle[K, V]) ByteIndex(name string) (*gmdb.IndexHandle, error) {
	return t.ks.Index(name)
}

// InternalIndexInfo exposes the planner's distilled view of this
// handle's ColumnIndex declarations through the shared internal
// seam (query-builder.md §Planning rules). The returned types live
// in an internal package: callers outside this module cannot name
// or construct them, so the representation carries no
// compatibility promise. Treat as read-only. Nil for handles
// opened without declarations (OpenReadOnly) — the planner then
// falls back to a full scan.
func (t *KeyspaceHandle[K, V]) InternalIndexInfo() []qrep.IndexInfo { return t.idxInfo }

// InternalRowOps exposes the handle's type-erased row codec
// through the shared internal seam — the query executor decodes
// index-leaf PK / value bytes and back-looks-up rows with it (it
// cannot reach the handle's encoders). Same internal-type
// non-promise as InternalIndexInfo. FetchRow reports found=false
// on a vanished row, mirroring the byte Lookup contract's
// silent-skip (indexing.md §Lookup API).
func (t *KeyspaceHandle[K, V]) InternalRowOps() qrep.RowOps {
	return qrep.RowOps{
		ValEncID:  t.valEnc.ID(),
		DecodeKey: func(pk []byte) (any, error) { return t.keyEnc.Decode(pk) },
		EncodeKey: func(k any) ([]byte, error) { return t.keyEnc.AppendEncode(nil, k.(K)) },
		DecodeVal: func(vb []byte) (any, error) { return t.valEnc.Decode(vb) },
		FetchRow: func(pk []byte) (any, bool, error) {
			vb, err := t.ks.Get(pk)
			if err != nil {
				if errors.Is(err, gmdb.ErrNotFound) {
					return nil, false, nil
				}
				return nil, false, err
			}
			v, err := t.valEnc.Decode(vb)
			if err != nil {
				return nil, false, err
			}
			return v, true, nil
		},
	}
}

// Err returns the first error encountered: a sticky decode/encode error
// from the typed layer takes precedence, else the byte cursor's error.
func (c *Cursor[K, V]) Err() error {
	if c.err != nil {
		return c.err
	}
	return c.bc.Err()
}
