# Typed Keyspaces (Generics)

Higher-level API on top of the byte-oriented `Keyspace` and
`SetKeyspace`. Provides type-safe access by handling key/value
serialisation via the `Encoder[T]` interface and typed-index
declarations via `TypedIndex[K, V, IK]`.

Scope:
- `Encoder[T]` interface and `FuncEncoder[T]` adapter.
- Engine-provided canonical encoders.
- `TypedKeyspace[K, V]` / `TypedKS[K, V]` (single-value typed
  keyspace) and `TypedSetKeyspace[K, V]` / `TypedSetKS[K, V]`
  (typed set keyspace) wrappers.
- `TypedCursor[K, V]` and `TypedSetCursor[K, V]`.
- `TypedIndex[K, V, IK]`, sealed-interface `AnyTypedIndex`,
  `TypedIndexHandle`, `TypedIndexQuery[K, V, IK]`.
- Key-ordering constraint on `Encoder[K]`.
- Encoder-ID immutability and the partial-prefix-range
  limitation through the typed API.

Depends on / interacts with:
- `keyspaces.md` for the underlying `Keyspace` / `SetKeyspace`
  contracts that this layer wraps.
- `indexing.md` for the byte-oriented `IndexDecl` the typed
  layer constructs internally.
- `api-surface.md` for the byte-oriented `Cursor` / `Index`
  methods that this layer delegates to.

## Invariants

Invariant: kind=clause-explicit;
  property=A key `Encoder[K]` MUST produce byte sequences whose
    lex order matches the desired key order. For `uint64`
    keys, big-endian. For `string` keys, natural byte
    representation;
  from=this spec §Key ordering constraint;
  violation=An encoder that does not lex-sort by the intended
    semantic order makes every range and prefix query return
    the wrong set of rows — silent correctness failure.

Invariant: kind=clause-explicit;
  property=`Encoder.ID()` returns a stable, non-empty string.
    Two distinct encoders MUST NOT share an ID. The ID is
    hashed into the schema fingerprint of any typed index
    that uses the encoder; collisions make schema changes
    undetectable;
  from=this spec §Encoder ID contract;
  violation=A shared ID across encoders with different
    encodings lets a schema change (encoder swap) bypass the
    drift guard — every on-disk index entry decodes as garbage.

Invariant: kind=clause-explicit;
  property=Empty IDs are rejected at `OpenKeyspace` /
    `CreateKeyspace` time with `ErrIndexEncoderIDEmpty` when
    an encoder is referenced by a typed index's schema hash
    (`IKEnc`, covering encoders). Key and value encoders on
    a `TypedKeyspace[K, V]` *without* indexes are not
    validated for empty IDs;
  from=this spec §Encoder ID empty check + Empty encoder IDs
    on TypedKeyspace without indexes;
  violation=Allowing empty IDs through indexing bypasses the
    fingerprint-uniqueness invariant; rejecting them on
    indexless typed keyspaces would gratuitously break
    callers that never plan to add indexes.

Invariant: kind=clause-explicit;
  property=Canonical engine encoder IDs are **forever
    immutable**. Once shipped, an engine-provided `ID()`
    string cannot change. Any bug fix in a canonical
    encoder ships under a NEW ID (e.g.
    `"gmdb/be-int64/v2"`) with a separate type, leaving the
    old ID available for backward read;
  from=this spec §Canonical engine encoder IDs;
  violation=Changing the encoding logic under an existing ID
    silently corrupts every on-disk index built with the
    old encoder; recovery requires a coordinated rebuild
    nobody knew was needed.

Invariant: kind=clause-explicit;
  property=`AnyTypedIndex[K, V]` is a **sealed** interface —
    the method `indexDecl()` is unexported, so only types in
    the `gmdb` package can implement it (in practice: only
    `*TypedIndex[K, V, IK]`). Decoration must happen at the
    *extractor function* level by wrapping the user's
    `Extract` func inside a fresh `TypedIndex[K, V, IK]`
    declaration;
  from=this spec §`AnyTypedIndex` sealing;
  violation=A user-supplied implementation could bypass
    encoder-ID consistency, deterministic schema-hash, or
    well-formed extractor wiring — the typed layer's
    safety guarantees rest on these.

## Encoder interface

```go
// Encoder handles serialization between a Go type and byte slices.
//
// AppendEncode appends the encoded form of v to dst and returns the
// extended buffer. Callers pass dst[:0] from a sync.Pool to reuse
// allocations on the hot path. Returns an error to reject values that
// cannot be represented (e.g., keys exceeding the maximum size).
//
// Decode deserializes src into a value of type T. Returns an error to
// surface malformed or truncated data rather than panicking.
//
// ID returns a stable, non-empty string identifier for this encoder
// type. The ID is hashed into the schema fingerprint of any typed
// index that uses the encoder, so two distinct encoders with the
// same ID make a schema change undetectable. The caller MUST mint a
// unique ID per encoder.
//
// Recommended naming convention: "<pkg>/<type>[/<version>]".
// Examples:
//   - "gmdb/string"               // engine-provided
//   - "gmdb/be-uint64"            // engine-provided
//   - "myapp/User-json/v2"        // application-defined
//   - "myapp/Timestamp-be-nanos"  // application-defined
//
// Empty IDs are rejected at OpenKeyspace / CreateKeyspace time with
// ErrIndexEncoderIDEmpty, naming the offending encoder by index name
// and column position. This catches the common misconfiguration of
// declaring a FuncEncoder without setting EncoderID.
type Encoder[T any] interface {
    AppendEncode(dst []byte, v T) ([]byte, error)
    Decode(src []byte) (T, error)
    ID() string
}

// FuncEncoder adapts plain functions into the Encoder interface for
// simple stateless cases.
type FuncEncoder[T any] struct {
    EncodeFunc func(dst []byte, v T) ([]byte, error)
    DecodeFunc func(src []byte) (T, error)
    EncoderID  string
}

func (f FuncEncoder[T]) AppendEncode(dst []byte, v T) ([]byte, error) { return f.EncodeFunc(dst, v) }
func (f FuncEncoder[T]) Decode(src []byte) (T, error)                 { return f.DecodeFunc(src) }
func (f FuncEncoder[T]) ID() string                                   { return f.EncoderID }
```

## Typed Keyspace

```go
// TypedKeyspace wraps a single-value Keyspace with type-safe encoding.
type TypedKeyspace[K, V any] struct {
    name   string
    keyEnc Encoder[K]
    valEnc Encoder[V]
}

// NewTypedKeyspace creates a typed keyspace descriptor. The key
// encoder MUST produce lexicographically ordered output for the
// desired key ordering.
func NewTypedKeyspace[K, V any](
    name string,
    keyEnc Encoder[K],
    valEnc Encoder[V],
) *TypedKeyspace[K, V]

// Open / Create / CreateIfNotExists within a transaction.
// The variadic indexes are TypedIndex declarations.
func (tks *TypedKeyspace[K, V]) Open(tx *Tx, indexes ...AnyTypedIndex[K, V]) (*TypedKS[K, V], error)
func (tks *TypedKeyspace[K, V]) OpenReadOnly(tx *Tx) (*TypedKS[K, V], error)
func (tks *TypedKeyspace[K, V]) Create(tx *Tx, indexes ...AnyTypedIndex[K, V]) (*TypedKS[K, V], error)
func (tks *TypedKeyspace[K, V]) CreateIfNotExists(tx *Tx, indexes ...AnyTypedIndex[K, V]) (*TypedKS[K, V], error)

// TypedKS is a handle to an opened typed keyspace within a transaction.
type TypedKS[K, V any] struct { ... }

func (t *TypedKS[K, V]) Get(key K) (V, error)
func (t *TypedKS[K, V]) Put(key K, value V) error

// Delete returns ErrNotFound when the key does not exist
// (per api-surface.md §Invariants — keyed-removal returns
// ErrNotFound on miss; applies to TypedKS / TypedSetKS too).
func (t *TypedKS[K, V]) Delete(key K) error

// DeleteRange returns (0, nil) for an empty range.
func (t *TypedKS[K, V]) DeleteRange(start, end *K) (uint64, error)
func (t *TypedKS[K, V]) Cursor() *TypedCursor[K, V]
func (t *TypedKS[K, V]) All() iter.Seq2[K, V]
func (t *TypedKS[K, V]) Range(start, end *K) iter.Seq2[K, V]
func (t *TypedKS[K, V]) Prefix(prefix K) iter.Seq2[K, V]
func (t *TypedKS[K, V]) Index(name string) (*TypedIndexHandle, error)

type TypedCursor[K, V any] struct { ... }

func (c *TypedCursor[K, V]) First() (K, V, bool)
func (c *TypedCursor[K, V]) Last() (K, V, bool)
func (c *TypedCursor[K, V]) Next() (K, V, bool)
func (c *TypedCursor[K, V]) Prev() (K, V, bool)
func (c *TypedCursor[K, V]) Seek(target K) (K, V, bool)
func (c *TypedCursor[K, V]) SeekGE(target K) (K, V, bool)
func (c *TypedCursor[K, V]) Current() (K, V, bool)

// Delete removes the current entry. Same semantics as Cursor.Delete
// — see `transactions.md §Cursor State Machine`. The third bool in
// the navigation methods is `ok` (false when the cursor is at
// end-of-iteration or unpositioned); Err() distinguishes those two
// states.
func (c *TypedCursor[K, V]) Delete() error
func (c *TypedCursor[K, V]) Err() error
```

The typed layer is a **zero-cost abstraction** at the API level
— all methods delegate to the underlying `Keyspace` and `Cursor`
methods with `Encoder` calls. `AppendEncode` follows the standard
Go append pattern, allowing callers to pass reusable buffers
(e.g. from `sync.Pool`) to eliminate per-call allocations.

**Key ordering constraint.** The key encoder must produce byte
sequences whose lex order matches the desired key order. For
`uint64` keys, big-endian. For `string` keys, natural byte
representation.

## Typed Set Keyspace

The typed split between `Keyspace` and `SetKeyspace` extends to
the typed layer.

```go
// TypedSetKeyspace[K, V] wraps SetKeyspace with type-safe encoding.
type TypedSetKeyspace[K, V any] struct { /* ... */ }

func NewTypedSetKeyspace[K, V any](
    name string,
    keyEnc Encoder[K],
    valEnc Encoder[V],
    opts *SetKeyspaceOptions,
) *TypedSetKeyspace[K, V]

// TypedSetKS[K, V] is a handle to an opened typed set keyspace.
type TypedSetKS[K, V any] struct { /* ... */ }

func (t *TypedSetKS[K, V]) Has(key K) (bool, error)
func (t *TypedSetKS[K, V]) HasValue(key K, value V) (bool, error)

// Put inserts value into the key's sorted set. added reports whether
// the set actually grew (false iff (key, value) was already present).
// Mirrors SetKeyspace.Put — see api-surface.md §SetKeyspace API for
// the load-bearing rationale (membership probe is already paid by
// the insert path, surfacing the bool collapses Put + HasValue retry
// patterns without a TOCTOU window). Chunk-6.1 user-locked decision.
func (t *TypedSetKS[K, V]) Put(key K, value V) (added bool, err error)

// Delete returns ErrNotFound when the key does not exist (per
// api-surface.md §Invariants).
func (t *TypedSetKS[K, V]) Delete(key K) error

// DeleteValue returns ErrNotFound when the (key, value) pair
// does not exist (per api-surface.md §Invariants).
func (t *TypedSetKS[K, V]) DeleteValue(key K, value V) error

func (t *TypedSetKS[K, V]) CountValues(key K) (uint64, error)

// DeleteRange returns (0, nil) for an empty range.
func (t *TypedSetKS[K, V]) DeleteRange(start, end *K) (uint64, error)
func (t *TypedSetKS[K, V]) Cursor() *TypedSetCursor[K, V]
func (t *TypedSetKS[K, V]) All() iter.Seq2[K, V]
func (t *TypedSetKS[K, V]) Range(start, end *K) iter.Seq2[K, V]
func (t *TypedSetKS[K, V]) Prefix(prefix K) iter.Seq2[K, V]
```

`TypedKS` has `Get`, `Put`, `Delete` — straightforward.
`TypedSetKS` has `Has`, `HasValue`, `Put`, `Delete`,
`DeleteValue`, `CountValues` — set operations.

## Typed Indexes

```go
// TypedIndex declares a typed index on TypedKeyspace[K, V] with
// extracted index key type IK.
type TypedIndex[K, V, IK any] struct {
    Name       string
    IKEnc      Encoder[IK]      // produces lex-safe bytes from IK
    Unique     bool
    Version    string           // bump on extractor logic changes
    Extract    func(K, V) []IK  // empty slice ⇒ skip (partial index)
    CoverValue bool             // full-row covering (see §Covering)
}

// AnyTypedIndex is the type-erased interface satisfied by every
// TypedIndex[K, V, IK]. It exists solely so a single
// Open / Create / CreateIfNotExists call can declare indexes with
// heterogeneous IK types in one variadic argument.
//
// The interface is intentionally SEALED — the method indexDecl() is
// unexported, so only types in the gmdb package can implement it (in
// practice: only *TypedIndex[K, V, IK]). This is deliberate: the
// engine relies on every supplied *IndexDecl having been constructed
// through the typed-index path, which guarantees encoder ID
// consistency, deterministic schema-hash, and well-formed extractor
// wiring. A user-supplied implementation could bypass these
// invariants.
//
// Library code that needs to wrap or decorate a typed index (for
// observability, retry, etc.) must compose at the *extractor
// function* level — wrap the user's Extract func inside a fresh
// TypedIndex[K, V, IK] declaration. Wrapping at the IndexDecl level
// is not supported and not needed.
type AnyTypedIndex[K, V any] interface {
    indexDecl(keyEnc Encoder[K], valEnc Encoder[V]) (*IndexDecl, error)
}

// indexDecl is unexported (the seal) and receives the owning keyspace's
// K/V encoders so it can build the extractor closure (decode (key,value)
// → run Extract → encode each IK) and validate encoder IDs; it returns
// ErrIndexEncoderIDEmpty for an empty IKEnc (or, for a CoverValue index,
// value-encoder) ID. The exact signature is an implementation detail of
// the sealed method.
func (t *TypedIndex[K, V, IK]) indexDecl(keyEnc Encoder[K], valEnc Encoder[V]) (*IndexDecl, error) { /* implements AnyTypedIndex */ }

// TypedIndexHandle is the typed wrapper around Index for queries
// where IK is known.
type TypedIndexHandle struct { /* unexported */ }

// For static-type lookup, NewTypedIndexQuery binds an open
// TypedIndexHandle with a specific IK type.
func NewTypedIndexQuery[K, V, IK any](h *TypedIndexHandle, ikEnc Encoder[IK]) *TypedIndexQuery[K, V, IK]

type TypedIndexQuery[K, V, IK any] struct { ... }

func (q *TypedIndexQuery[K, V, IK]) Lookup(ik IK) iter.Seq2[K, V]
func (q *TypedIndexQuery[K, V, IK]) LookupKeys(ik IK) iter.Seq[K]
func (q *TypedIndexQuery[K, V, IK]) Range(start, end *IK) iter.Seq2[K, V]
func (q *TypedIndexQuery[K, V, IK]) Prefix(prefix IK) iter.Seq2[K, V]
func (q *TypedIndexQuery[K, V, IK]) Get(ik IK) (K, V, error) // unique only
func (q *TypedIndexQuery[K, V, IK]) Err() error
```

The schema-hash inputs for a typed index include the index-key
encoder's ID (and, for a `CoverValue` index, the value encoder's
ID) — so changing from `be-uint64` to `varint-zigzag` for the same
column triggers `ErrIndexFingerprintMismatch` at Open. (The typed
layer folds these IDs in by synthesizing the byte-`IndexDecl`'s
column / covering-column **names** from the encoder IDs; the
byte schema-hash already hashes column names.)

### Covering: `CoverValue` (full-row covering)

`CoverValue: true` makes the index a **full-row covering index**:
the encoded row value is stored in each index entry, so
`TypedIndexQuery` `Lookup` / `Range` / `Prefix` / `Get` return `V`
directly from the index entry **without a back-lookup** against
the row keyspace. It is a pure read optimization — identical
`(K, V)` results, fewer reads.

Mechanics: the extractor stores the row's stored value bytes
(which are exactly `encode(V)`) as the entry's single covering
column; the byte layer's covering-return path (gated, enabled only
for an index the typed layer recognizes as full-row covering)
returns that column instead of back-looking-up. The value
encoder's `ID()` is folded into the fingerprint (an empty value
encoder `ID()` is rejected with `ErrIndexEncoderIDEmpty`).

`CoverValue` has effect only on a `TypedKeyspace`-backed index: a
`TypedSetKeyspace` index's value (`setValue`) is already carried in
its compound primary key, so there is no back-lookup to skip. This
is the only covering shape the typed API exposes — arbitrary
covering *projections* have no typed return surface (every
`TypedIndexQuery` method returns the row value `V`). For arbitrary
covering projections, use the byte-oriented `IndexDecl` API: its
`Lookup` / `Range` / `Prefix` / `Get` return the encoded covering
tuple, decoded by the caller via `DecodeCoveringTuple` (see
`indexing.md §Covering Indexes` and `api-surface.md §Index Lookup
API`).

A typed extractor returning multiple `IK` values models composite
indexes naturally (the `IK` type is itself a struct whose
`Encoder[IK]` produces the concatenated lex-safe bytes). For
columns of different types where a single `IK` struct is
awkward, fall back to the byte-oriented `IndexDecl` API.

### Limitation: partial-prefix queries through the typed API

When `IK` is a composite struct, the typed layer treats the whole
`IK` as one opaque column (one `Encoder[IK]` → one byte slice).
Consequently `TypedIndexQuery.Range(start, end *IK)` compares
full `IK` values; there is **no partial-prefix Range on a
sub-field of IK** through the typed API.

Workarounds:

- Use the byte-oriented `IndexDecl` directly, declaring each
  sub-field as a separate `IndexColumn` (one column per
  sub-field). Byte-API `Range` and `Prefix` then accept per-
  column tuples and support partial prefixes naturally.
- Design `Encoder[IK]` so the desired prefix sort key is exactly
  a byte prefix of the full encoding; then callers can construct
  partial-key `IK` values that serialise to the desired prefix
  and pass them to `Range`. This requires careful encoding
  design and loses generality.

## Engine-Provided Canonical Encoders

The engine ships canonical `Encoder[T]` implementations for
common column types. The full canonical set:

| Encoder | ID() | Lex order matches | Notes |
|---|---|---|---|
| `gmdb.StringEncoder` | `"gmdb/string"` | natural string order | UTF-8 bytes, no normalization |
| `gmdb.BytesEncoder` | `"gmdb/bytes"` | natural byte order | identity |
| `gmdb.BEUint64Encoder` | `"gmdb/be-uint64"` | natural uint64 order | 8-byte big-endian |
| `gmdb.BEUint32Encoder` | `"gmdb/be-uint32"` | natural uint32 order | 4-byte big-endian |
| `gmdb.BEInt64Encoder` | `"gmdb/be-int64"` | natural int64 order | 8-byte big-endian with sign bit XOR'd (XOR `0x80` on the top byte); maps two's-complement to lex order |
| `gmdb.BEInt32Encoder` | `"gmdb/be-int32"` | natural int32 order | 4-byte big-endian with sign bit XOR'd |
| `gmdb.BENanosEncoder` | `"gmdb/be-time-nanos"` | natural time order | int64 nanos since epoch, same sign-bit-XOR transform as `be-int64` |
| `gmdb.UUIDv4Encoder` | `"gmdb/uuid-v4"` | natural lex (random) | 16 bytes raw |
| `gmdb.UUIDv7Encoder` | `"gmdb/uuid-v7"` | natural time order | 16 bytes raw; v7 timestamp prefix preserves lex=time |

The signed-integer transform is sign-bit XOR
(`x ^ 0x8000000000000000`), not zigzag — zigzag is a different
protobuf-style transform that interleaves negatives among
positives and is *not* lex-preserving for big-endian byte
order. The naming uses plain `gmdb/be-intN` to avoid the
misleading "zigzag" label.

**Canonical engine encoder IDs are forever immutable.** Once
shipped, an engine-provided `ID()` string cannot change — any
change to the encoding logic for an existing ID would silently
corrupt every on-disk index built with the old encoder. If a bug
is discovered in a canonical encoder, the fix ships under a NEW
ID (e.g. `"gmdb/be-int64/v2"`) with a separate type (e.g.
`gmdb.BEInt64EncoderV2`); the old type and ID remain available
for backward read of existing indexes. Operators migrating from
the buggy encoder rebuild the affected indexes via
`tx.RebuildIndex` with the new typed decl. This convention
extends to application-defined encoders
(`"<pkg>/<type>[/<version>]"` — bump the version segment when
the encoding logic changes; see `Encoder.ID()` godoc).

**Empty encoder IDs on TypedKeyspace without indexes.** The
`Encoder.ID()` empty check fires only when an encoder is
referenced by a typed index's schema hash (`IKEnc`, covering
encoders). The key and value encoders on
`TypedKeyspace[K, V]` *without* indexes are not validated for
empty IDs — a TypedKeyspace with no declared indexes may use
encoders with empty `ID()` without error. This is inadvisable
if indexes may be added later (declaring a typed index that
depends on the key encoder will then fail at `OpenKeyspace`
with `ErrIndexEncoderIDEmpty`); application code should set
non-empty encoder IDs as a matter of hygiene regardless.
