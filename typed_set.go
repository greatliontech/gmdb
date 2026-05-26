package gmdb

import (
	"bytes"
	"iter"
)

// Typed set keyspace layer (typed-keyspaces.md §Typed Set Keyspace).
// Mirrors the typed Keyspace wrapper for the byte SetKeyspace: every
// method encodes K / V through the keyspace's encoders and delegates to
// the corresponding byte-layer set operation.
//
// The TypedSetCursor exposes member-level navigation — each position is
// a distinct (key, value) set member, and First/Next iterate every
// member in (key, value) lex order. The byte SetCursor's value-level
// intra-key navigation (FirstValue / NextValue / …) is intentionally
// not mirrored: its nil-return end sentinel is ambiguous with an
// empty-bytes set value, whereas member-level navigation keys its end
// sentinel on the (never-empty) key, and reaches every member anyway.

// TypedSetKeyspace wraps a SetKeyspace with type-safe encoding. Stateless
// descriptor (name + encoders + creation options); Open / Create return
// a transaction-scoped TypedSetKS handle.
type TypedSetKeyspace[K, V any] struct {
	name   string
	keyEnc Encoder[K]
	valEnc Encoder[V]
	opts   *SetKeyspaceOptions
}

// NewTypedSetKeyspace creates a typed set keyspace descriptor. keyEnc
// MUST produce lexicographically ordered output (Inv-T1). opts carries
// the create-time SetKeyspace options (e.g. FixedValueSize); it is
// consulted only by Create / CreateIfNotExists.
func NewTypedSetKeyspace[K, V any](name string, keyEnc Encoder[K], valEnc Encoder[V], opts *SetKeyspaceOptions) *TypedSetKeyspace[K, V] {
	return &TypedSetKeyspace[K, V]{name: name, keyEnc: keyEnc, valEnc: valEnc, opts: opts}
}

func (tsk *TypedSetKeyspace[K, V]) translateIndexes(indexes []AnyTypedIndex[K, V]) ([]*IndexDecl, error) {
	if len(indexes) == 0 {
		return nil, nil
	}
	decls := make([]*IndexDecl, 0, len(indexes))
	for _, idx := range indexes {
		d, err := idx.indexDecl(tsk.keyEnc, tsk.valEnc)
		if err != nil {
			return nil, err
		}
		decls = append(decls, d)
	}
	return decls, nil
}

func (tsk *TypedSetKeyspace[K, V]) wrap(sks *SetKeyspace) *TypedSetKS[K, V] {
	return &TypedSetKS[K, V]{sks: sks, keyEnc: tsk.keyEnc, valEnc: tsk.valEnc}
}

// Open opens the set keyspace for read+write within tx with the supplied
// typed indexes. OpenReadOnly opens for reads only (no index decls).
// Create / CreateIfNotExists create with the descriptor's options.
func (tsk *TypedSetKeyspace[K, V]) Open(tx *Tx, indexes ...AnyTypedIndex[K, V]) (*TypedSetKS[K, V], error) {
	decls, err := tsk.translateIndexes(indexes)
	if err != nil {
		return nil, err
	}
	sks, err := tx.OpenSetKeyspace(tsk.name, decls...)
	if err != nil {
		return nil, err
	}
	return tsk.wrap(sks), nil
}

func (tsk *TypedSetKeyspace[K, V]) OpenReadOnly(tx *Tx) (*TypedSetKS[K, V], error) {
	sks, err := tx.OpenSetKeyspaceReadOnly(tsk.name)
	if err != nil {
		return nil, err
	}
	return tsk.wrap(sks), nil
}

func (tsk *TypedSetKeyspace[K, V]) Create(tx *Tx, indexes ...AnyTypedIndex[K, V]) (*TypedSetKS[K, V], error) {
	decls, err := tsk.translateIndexes(indexes)
	if err != nil {
		return nil, err
	}
	sks, err := tx.CreateSetKeyspace(tsk.name, tsk.opts, decls...)
	if err != nil {
		return nil, err
	}
	return tsk.wrap(sks), nil
}

func (tsk *TypedSetKeyspace[K, V]) CreateIfNotExists(tx *Tx, indexes ...AnyTypedIndex[K, V]) (*TypedSetKS[K, V], error) {
	decls, err := tsk.translateIndexes(indexes)
	if err != nil {
		return nil, err
	}
	sks, err := tx.CreateSetKeyspaceIfNotExists(tsk.name, tsk.opts, decls...)
	if err != nil {
		return nil, err
	}
	return tsk.wrap(sks), nil
}

// TypedSetKS is a handle to an opened typed set keyspace within a tx.
type TypedSetKS[K, V any] struct {
	sks    *SetKeyspace
	keyEnc Encoder[K]
	valEnc Encoder[V]
}

// Has reports whether key has any members.
func (t *TypedSetKS[K, V]) Has(key K) (bool, error) {
	kb, err := t.keyEnc.AppendEncode(nil, key)
	if err != nil {
		return false, err
	}
	return t.sks.Has(kb)
}

// HasValue reports whether (key, value) is a member.
func (t *TypedSetKS[K, V]) HasValue(key K, value V) (bool, error) {
	kb, err := t.keyEnc.AppendEncode(nil, key)
	if err != nil {
		return false, err
	}
	vb, err := t.valEnc.AppendEncode(nil, value)
	if err != nil {
		return false, err
	}
	return t.sks.HasValue(kb, vb)
}

// Put inserts value into key's sorted set. added is false iff (key,
// value) was already present (mirrors SetKeyspace.Put — the membership
// probe is already paid by the insert path).
func (t *TypedSetKS[K, V]) Put(key K, value V) (added bool, err error) {
	kb, err := t.keyEnc.AppendEncode(nil, key)
	if err != nil {
		return false, err
	}
	vb, err := t.valEnc.AppendEncode(nil, value)
	if err != nil {
		return false, err
	}
	return t.sks.Put(kb, vb)
}

// Delete removes key and all its members, returning ErrNotFound if key
// has no members.
func (t *TypedSetKS[K, V]) Delete(key K) error {
	kb, err := t.keyEnc.AppendEncode(nil, key)
	if err != nil {
		return err
	}
	return t.sks.Delete(kb)
}

// DeleteValue removes one (key, value) member, returning ErrNotFound if
// the pair does not exist.
func (t *TypedSetKS[K, V]) DeleteValue(key K, value V) error {
	kb, err := t.keyEnc.AppendEncode(nil, key)
	if err != nil {
		return err
	}
	vb, err := t.valEnc.AppendEncode(nil, value)
	if err != nil {
		return err
	}
	return t.sks.DeleteValue(kb, vb)
}

// CountValues returns the number of members under key (0 if absent).
func (t *TypedSetKS[K, V]) CountValues(key K) (uint64, error) {
	kb, err := t.keyEnc.AppendEncode(nil, key)
	if err != nil {
		return 0, err
	}
	return t.sks.CountValues(kb)
}

// DeleteRange deletes every key (and all its members) with *start <= key
// < *end. nil pointer = open boundary; see TypedKS.DeleteRange for the
// boundary-encoding semantics. Returns the number of members deleted.
func (t *TypedSetKS[K, V]) DeleteRange(start, end *K) (uint64, error) {
	sb, err := encodeBound(t.keyEnc, start)
	if err != nil {
		return 0, err
	}
	eb, err := encodeBound(t.keyEnc, end)
	if err != nil {
		return 0, err
	}
	return t.sks.DeleteRange(sb, eb)
}

// Cursor returns a member-level typed cursor (each position is a
// distinct (key, value) member).
func (t *TypedSetKS[K, V]) Cursor() *TypedSetCursor[K, V] {
	return &TypedSetCursor[K, V]{sc: t.sks.Cursor(), keyEnc: t.keyEnc, valEnc: t.valEnc}
}

// All yields every (key, value) member in (key, value) lex order. Range
// restricts to members whose key is in [*start, *end); Prefix to members
// whose encoded key has the encoded prefix as a byte prefix. Best-effort
// (a cursor / decode error ends the sequence — use Cursor()+Err() for
// error visibility), matching TypedKS.
func (t *TypedSetKS[K, V]) All() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		c := t.sks.Cursor()
		for kb, vb := c.First(); kb != nil; kb, vb = c.Next() {
			k, v, ok := decodeKV(t.keyEnc, t.valEnc, kb, vb)
			if !ok || !yield(k, v) {
				return
			}
		}
	}
}

func (t *TypedSetKS[K, V]) Range(start, end *K) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		sb, err := encodeBound(t.keyEnc, start)
		if err != nil {
			return
		}
		eb, err := encodeBound(t.keyEnc, end)
		if err != nil {
			return
		}
		c := t.sks.Cursor()
		var kb, vb []byte
		if sb != nil {
			kb, vb = c.SeekGE(sb)
		} else {
			kb, vb = c.First()
		}
		for ; kb != nil; kb, vb = c.Next() {
			if eb != nil && bytes.Compare(kb, eb) >= 0 {
				return
			}
			k, v, ok := decodeKV(t.keyEnc, t.valEnc, kb, vb)
			if !ok || !yield(k, v) {
				return
			}
		}
	}
}

func (t *TypedSetKS[K, V]) Prefix(prefix K) iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		pb, err := t.keyEnc.AppendEncode(nil, prefix)
		if err != nil {
			return
		}
		c := t.sks.Cursor()
		for kb, vb := c.SeekGE(pb); kb != nil; kb, vb = c.Next() {
			if !bytes.HasPrefix(kb, pb) {
				return
			}
			k, v, ok := decodeKV(t.keyEnc, t.valEnc, kb, vb)
			if !ok || !yield(k, v) {
				return
			}
		}
	}
}

// TypedSetCursor is a member-level type-safe cursor over a TypedSetKS.
// Navigation returns (K, V, ok); a decode/encode error is sticky and
// surfaces via Err().
type TypedSetCursor[K, V any] struct {
	sc     *SetCursor
	keyEnc Encoder[K]
	valEnc Encoder[V]
	err    error
}

// member lowers a byte (key, value) navigation result to (K, V, ok),
// recording the first decode error. A nil key (end / unpositioned)
// yields ok=false with no error — set keys are never empty, so nil is
// an unambiguous end sentinel.
func (c *TypedSetCursor[K, V]) member(kb, vb []byte) (K, V, bool) {
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

func (c *TypedSetCursor[K, V]) First() (K, V, bool)   { return c.member(c.sc.First()) }
func (c *TypedSetCursor[K, V]) Last() (K, V, bool)    { return c.member(c.sc.Last()) }
func (c *TypedSetCursor[K, V]) Next() (K, V, bool)    { return c.member(c.sc.Next()) }
func (c *TypedSetCursor[K, V]) Prev() (K, V, bool)    { return c.member(c.sc.Prev()) }
func (c *TypedSetCursor[K, V]) Current() (K, V, bool) { return c.member(c.sc.Current()) }

// Seek positions at the first member with key == target (or
// end-of-iteration on a key miss); SeekGE at the first member with key
// >= target. An encode error on target is recorded (Err()) and returns
// ok=false.
func (c *TypedSetCursor[K, V]) Seek(target K) (K, V, bool) {
	tb, err := c.keyEnc.AppendEncode(nil, target)
	if err != nil {
		if c.err == nil {
			c.err = err
		}
		var zk K
		var zv V
		return zk, zv, false
	}
	return c.member(c.sc.Seek(tb))
}

func (c *TypedSetCursor[K, V]) SeekGE(target K) (K, V, bool) {
	tb, err := c.keyEnc.AppendEncode(nil, target)
	if err != nil {
		if c.err == nil {
			c.err = err
		}
		var zk K
		var zv V
		return zk, zv, false
	}
	return c.member(c.sc.SeekGE(tb))
}

// Delete removes the current (key, value) member (same semantics as
// SetCursor.Delete).
func (c *TypedSetCursor[K, V]) Delete() error { return c.sc.Delete() }

// Err returns the first error: a sticky decode/encode error from the
// typed layer takes precedence, else the byte set cursor's error.
func (c *TypedSetCursor[K, V]) Err() error {
	if c.err != nil {
		return c.err
	}
	return c.sc.Err()
}
