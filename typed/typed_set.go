package typed

import (
	"fmt"
	"iter"

	"github.com/greatliontech/gmdb"
	"github.com/greatliontech/gmdb/internal/qrep"
)

// Typed set keyspace layer (typed-keyspaces.md §Typed Set Keyspace).
// Mirrors the typed Keyspace wrapper for the byte SetKeyspace: every
// method encodes K / V through the keyspace's encoders and delegates to
// the corresponding byte-layer set operation.
//
// The SetCursor exposes member-level navigation — each position is
// a distinct (key, value) set member, and First/Next iterate every
// member in (key, value) lex order. The byte SetCursor's value-level
// intra-key navigation (FirstValue / NextValue / …) is intentionally
// not mirrored: its nil-return end sentinel is ambiguous with an
// empty-bytes set value, whereas member-level navigation keys its end
// sentinel on the (never-empty) key, and reaches every member anyway.

// SetKeyspace wraps a gmdb.SetKeyspace with type-safe encoding. Stateless
// descriptor (name + encoders + creation options); Open / Create return
// a transaction-scoped SetKeyspaceHandle.
type SetKeyspace[K, V any] struct {
	name   string
	keyEnc Encoder[K]
	valEnc Encoder[V]
	opts   *gmdb.SetKeyspaceOptions
}

// NewSetKeyspace creates a typed set keyspace descriptor. keyEnc
// MUST produce lexicographically ordered output (typed-keyspaces.md §Invariants). opts carries
// the create-time SetKeyspace options (e.g. FixedValueSize); it is
// consulted only by Create / CreateIfNotExists.
func NewSetKeyspace[K, V any](name string, keyEnc Encoder[K], valEnc Encoder[V], opts *gmdb.SetKeyspaceOptions) *SetKeyspace[K, V] {
	return &SetKeyspace[K, V]{name: name, keyEnc: keyEnc, valEnc: valEnc, opts: opts}
}

// wrap ignores the planner index metadata openTypedHandle threads:
// the query surface plans over single-value keyspaces only
// (query-builder.md §Query surface takes a KeyspaceHandle).
func (tsk *SetKeyspace[K, V]) wrap(sks *gmdb.SetKeyspace, _ []qrep.IndexInfo) *SetKeyspaceHandle[K, V] {
	return &SetKeyspaceHandle[K, V]{sks: sks, keyEnc: tsk.keyEnc, valEnc: tsk.valEnc}
}

// rejectCoveringDecls bars covering-declaring typed indexes —
// both forms: ColumnIndex Covering/CoverValue and Index
// CoverValue — from set keyspaces (typed-keyspaces.md §Covering):
// a set index's covering payload has no read path (the byte layer
// never serves covering for set indexes; the compound PK already
// carries the member value), while the write path would pay the
// covering bytes per set member — fingerprinted write
// amplification for bytes no read can reach.
func rejectCoveringDecls[K, V any](indexes []AnyIndex[K, V]) error {
	for _, idx := range indexes {
		if p, ok := idx.(interface{ coveringDeclared() (string, bool) }); ok {
			if name, has := p.coveringDeclared(); has {
				return fmt.Errorf("gmdb: typed index %q: Covering/CoverValue on a SetKeyspace has no read path: %w",
					name, gmdb.ErrInvalidOptions)
			}
		}
	}
	return nil
}

// Open opens the set keyspace for read+write within tx with the supplied
// typed indexes. OpenReadOnly opens for reads only (no index decls).
// Create / CreateIfNotExists create with the descriptor's options.
// All three share openTypedHandle (typed.go) with the byte SetKeyspace
// factory target; only OpenReadOnly takes no index decls.
func (tsk *SetKeyspace[K, V]) Open(tx *gmdb.Tx, indexes ...AnyIndex[K, V]) (*SetKeyspaceHandle[K, V], error) {
	if err := rejectCoveringDecls(indexes); err != nil {
		return nil, err
	}
	return openTypedHandle(tsk.keyEnc, tsk.valEnc, indexes,
		func(decls []*gmdb.IndexDecl) (*gmdb.SetKeyspace, error) {
			return tx.OpenSetKeyspace(tsk.name, decls...)
		},
		tsk.wrap)
}

// OpenReadOnly opens the set keyspace for reads only. src is either a
// write transaction (*gmdb.Tx) or a snapshot read transaction
// (*gmdb.ReadTx); the handle is bound to src's lifetime and mutations
// on it return gmdb.ErrReadOnly.
func (tsk *SetKeyspace[K, V]) OpenReadOnly(src ReadOpener) (*SetKeyspaceHandle[K, V], error) {
	sks, err := src.OpenSetKeyspaceReadOnly(tsk.name)
	if err != nil {
		return nil, err
	}
	return tsk.wrap(sks, nil), nil
}

func (tsk *SetKeyspace[K, V]) Create(tx *gmdb.Tx, indexes ...AnyIndex[K, V]) (*SetKeyspaceHandle[K, V], error) {
	if err := rejectCoveringDecls(indexes); err != nil {
		return nil, err
	}
	return openTypedHandle(tsk.keyEnc, tsk.valEnc, indexes,
		func(decls []*gmdb.IndexDecl) (*gmdb.SetKeyspace, error) {
			return tx.CreateSetKeyspace(tsk.name, tsk.opts, decls...)
		},
		tsk.wrap)
}

func (tsk *SetKeyspace[K, V]) CreateIfNotExists(tx *gmdb.Tx, indexes ...AnyIndex[K, V]) (*SetKeyspaceHandle[K, V], error) {
	if err := rejectCoveringDecls(indexes); err != nil {
		return nil, err
	}
	return openTypedHandle(tsk.keyEnc, tsk.valEnc, indexes,
		func(decls []*gmdb.IndexDecl) (*gmdb.SetKeyspace, error) {
			return tx.CreateSetKeyspaceIfNotExists(tsk.name, tsk.opts, decls...)
		},
		tsk.wrap)
}

// SetKeyspaceHandle is a handle to an opened typed set keyspace within a tx.
type SetKeyspaceHandle[K, V any] struct {
	sks    *gmdb.SetKeyspace
	keyEnc Encoder[K]
	valEnc Encoder[V]
}

// Has reports whether key has any members.
func (t *SetKeyspaceHandle[K, V]) Has(key K) (bool, error) {
	kb, err := t.keyEnc.AppendEncode(nil, key)
	if err != nil {
		return false, err
	}
	return t.sks.Has(kb)
}

// HasValue reports whether (key, value) is a member.
func (t *SetKeyspaceHandle[K, V]) HasValue(key K, value V) (bool, error) {
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
func (t *SetKeyspaceHandle[K, V]) Put(key K, value V) (added bool, err error) {
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

// Delete removes key and all its members, returning gmdb.ErrNotFound if key
// has no members.
func (t *SetKeyspaceHandle[K, V]) Delete(key K) error {
	kb, err := t.keyEnc.AppendEncode(nil, key)
	if err != nil {
		return err
	}
	return t.sks.Delete(kb)
}

// DeleteValue removes one (key, value) member, returning gmdb.ErrNotFound if
// the pair does not exist.
func (t *SetKeyspaceHandle[K, V]) DeleteValue(key K, value V) error {
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
func (t *SetKeyspaceHandle[K, V]) CountValues(key K) (uint64, error) {
	kb, err := t.keyEnc.AppendEncode(nil, key)
	if err != nil {
		return 0, err
	}
	return t.sks.CountValues(kb)
}

// DeleteRange deletes every key (and all its members) with *start <= key
// < *end. nil pointer = open boundary; see KeyspaceHandle.DeleteRange for the
// boundary-encoding semantics. Returns the number of members deleted.
func (t *SetKeyspaceHandle[K, V]) DeleteRange(start, end *K) (uint64, error) {
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
func (t *SetKeyspaceHandle[K, V]) Cursor() *SetCursor[K, V] {
	return &SetCursor[K, V]{sc: t.sks.Cursor(), keyEnc: t.keyEnc, valEnc: t.valEnc}
}

// All yields every (key, value) member in (key, value) lex order. Range
// restricts to members whose key is in [*start, *end); Prefix to members
// whose encoded key has the encoded prefix as a byte prefix. Best-effort
// (a cursor / decode error ends the sequence — use Cursor()+Err() for
// error visibility), matching KeyspaceHandle.
func (t *SetKeyspaceHandle[K, V]) All() iter.Seq2[K, V] {
	seq := t.sks.All() // eager: the construction guard fires HERE, not at loop start
	return func(yield func(K, V) bool) {
		for kb, vb := range seq {
			k, v, ok := decodeKV(t.keyEnc, t.valEnc, kb, vb)
			if !ok || !yield(k, v) {
				return
			}
		}
	}
}

func (t *SetKeyspaceHandle[K, V]) Range(start, end *K) iter.Seq2[K, V] {
	// Eager construction — uniform with the raw iterators and typed
	// All; encode failure keeps its silent-empty contract.
	sb, serr := encodeBound(t.keyEnc, start)
	eb, eerr := encodeBound(t.keyEnc, end)
	if serr != nil || eerr != nil {
		t.sks.GuardIterConstruction()
		return func(yield func(K, V) bool) {}
	}
	seq := t.sks.Range(sb, eb)
	return func(yield func(K, V) bool) {
		for kb, vb := range seq {
			k, v, ok := decodeKV(t.keyEnc, t.valEnc, kb, vb)
			if !ok || !yield(k, v) {
				return
			}
		}
	}
}

func (t *SetKeyspaceHandle[K, V]) Prefix(prefix K) iter.Seq2[K, V] {
	// Eager construction — see Range.
	pb, perr := t.keyEnc.AppendEncode(nil, prefix)
	if perr != nil {
		t.sks.GuardIterConstruction()
		return func(yield func(K, V) bool) {}
	}
	seq := t.sks.Prefix(pb)
	return func(yield func(K, V) bool) {
		for kb, vb := range seq {
			k, v, ok := decodeKV(t.keyEnc, t.valEnc, kb, vb)
			if !ok || !yield(k, v) {
				return
			}
		}
	}
}

// SetCursor is a member-level type-safe cursor over a SetKeyspaceHandle.
// Navigation returns (K, V, ok); a decode/encode error is sticky and
// surfaces via Err().
type SetCursor[K, V any] struct {
	sc     *gmdb.SetCursor
	keyEnc Encoder[K]
	valEnc Encoder[V]
	err    error
}

// member lowers a byte (key, value) navigation result to (K, V, ok),
// recording the first decode error. A nil key (end / unpositioned)
// yields ok=false with no error — set keys are never empty, so nil is
// an unambiguous end sentinel.
func (c *SetCursor[K, V]) member(kb, vb []byte) (K, V, bool) {
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

func (c *SetCursor[K, V]) First() (K, V, bool)   { return c.member(c.sc.First()) }
func (c *SetCursor[K, V]) Last() (K, V, bool)    { return c.member(c.sc.Last()) }
func (c *SetCursor[K, V]) Next() (K, V, bool)    { return c.member(c.sc.Next()) }
func (c *SetCursor[K, V]) Prev() (K, V, bool)    { return c.member(c.sc.Prev()) }
func (c *SetCursor[K, V]) Current() (K, V, bool) { return c.member(c.sc.Current()) }

// Seek positions at the first member with key == target (or
// end-of-iteration on a key miss); SeekGE at the first member with key
// >= target. An encode error on target is recorded (Err()) and returns
// ok=false.
func (c *SetCursor[K, V]) Seek(target K) (K, V, bool) {
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

func (c *SetCursor[K, V]) SeekGE(target K) (K, V, bool) {
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
func (c *SetCursor[K, V]) Delete() error { return c.sc.Delete() }

// Close releases the cursor before the transaction ends (same
// semantics as gmdb.SetCursor.Close: unregisters from staleness
// tracking; subsequent operations surface gmdb.ErrCursorClosed,
// though an earlier sticky error is preserved by Err; terminal,
// idempotent, optional).
func (c *SetCursor[K, V]) Close() { c.sc.Close() }

// Err returns the first error: a sticky decode/encode error from the
// typed layer takes precedence, else the byte set cursor's error.
func (c *SetCursor[K, V]) Err() error {
	if c.err != nil {
		return c.err
	}
	return c.sc.Err()
}
