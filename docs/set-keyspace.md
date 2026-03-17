# Keyspace / SetKeyspace Type Separation

## Problem

The current design uses a single `Keyspace` type with runtime flags (`DupSort`,
`DupFixed`) to switch between two fundamentally different data models:

1. **Key → Value** (single value per key)
2. **Key → Sorted Set of Values** (multiple sorted values per key)

These have different semantics for every operation:

| Operation | Keyspace | SetKeyspace |
|-----------|----------|-------------|
| `Put(k, v)` | Replace existing value | Add value to set (no-op if exists) |
| `Get(k)` | Return the value | Return... the first value? Unclear |
| `Delete(k)` | Delete the key | Delete key + all values in the set |
| Delete one value | N/A | Requires cursor (3 calls) |
| Cursor navigation | Keys only | Keys + intra-key value navigation |

Combining these into one type means:
- Methods need "only valid when..." documentation caveats
- Runtime errors for calling the wrong method on the wrong keyspace kind
- `Get` on a set keyspace returns an arbitrary (first) value — misleading
- Cursor has value-navigation methods that error on single-value keyspaces
- The typed generic layer (`TypedKeyspace[K, V]`) inherits all this ambiguity

## Design: Two Types

### Keyspace (key → value)

A keyspace where each key maps to exactly one value.

```go
type Keyspace struct { /* unexported */ }
```

**Creation and opening:**

```go
func (tx *Tx) CreateKeyspace(name []byte) (*Keyspace, error)
func (tx *Tx) OpenKeyspace(name []byte) (*Keyspace, error)
func (tx *Tx) CreateKeyspaceIfNotExists(name []byte) (*Keyspace, error)
```

No flags parameter. A `Keyspace` is always a simple key-value map.

**Operations:**

```go
// Get returns the value for the given key.
// Returns ErrNotFound if the key does not exist.
func (ks *Keyspace) Get(key []byte) ([]byte, error)

// Put inserts or replaces the value for the given key.
func (ks *Keyspace) Put(key, value []byte) error

// Delete removes the key and its value.
// Returns ErrNotFound if the key does not exist.
func (ks *Keyspace) Delete(key []byte) error

// DeleteRange deletes all keys in the range [start, end).
func (ks *Keyspace) DeleteRange(start, end []byte) (uint64, error)

// Cursor returns a cursor for iterating over key-value pairs.
func (ks *Keyspace) Cursor() *Cursor

// Range iterators (read-only, for use with for-range).
func (ks *Keyspace) All() iter.Seq2[[]byte, []byte]
func (ks *Keyspace) Range(start, end []byte) iter.Seq2[[]byte, []byte]
func (ks *Keyspace) Prefix(prefix []byte) iter.Seq2[[]byte, []byte]

// Stats returns keyspace statistics.
func (ks *Keyspace) Stats() (KeyspaceStats, error)
```

**Cursor:**

```go
type Cursor struct { /* unexported */ }

func (c *Cursor) First() (key, value []byte)
func (c *Cursor) Last() (key, value []byte)
func (c *Cursor) Next() (key, value []byte)
func (c *Cursor) Prev() (key, value []byte)
func (c *Cursor) Seek(target []byte) (key, value []byte)
func (c *Cursor) SeekGE(target []byte) (key, value []byte)
func (c *Cursor) Current() (key, value []byte)
func (c *Cursor) Delete() error
func (c *Cursor) Err() error
```

No value-navigation methods. No ambiguity.

### SetKeyspace (key → sorted set of values)

A keyspace where each key maps to a sorted set of values. Designed for
secondary indexes, tags, relationships, and any data model where a key
has multiple associated values.

```go
type SetKeyspace struct { /* unexported */ }
```

**Creation and opening:**

```go
// CreateSetKeyspace creates a new set keyspace. If opts is nil, default
// options are used.
func (tx *Tx) CreateSetKeyspace(name []byte, opts *SetKeyspaceOptions) (*SetKeyspace, error)
func (tx *Tx) OpenSetKeyspace(name []byte) (*SetKeyspace, error)
func (tx *Tx) CreateSetKeyspaceIfNotExists(name []byte, opts *SetKeyspaceOptions) (*SetKeyspace, error)
```

```go
// SetKeyspaceOptions controls set keyspace behavior. All fields are set
// at creation time and immutable after.
type SetKeyspaceOptions struct {
    // FixedValueSize, when non-zero, requires all values in the set to
    // be exactly this many bytes. Enables storage optimizations: no
    // per-value length prefix in subpages, direct offset binary search.
    // A Put() with a value of the wrong size returns ErrValueSizeMismatch.
    FixedValueSize int
}
```

The `FixedValueSize` replaces both the `DupFixed` flag and the
`DupFixedSize` descriptor field. Zero means variable-size (the default).
Non-zero means fixed. One field, no flags.

**Operations:**

```go
// Has reports whether the key exists (has at least one value).
func (ks *SetKeyspace) Has(key []byte) (bool, error)

// HasValue reports whether a specific key-value pair exists.
func (ks *SetKeyspace) HasValue(key, value []byte) (bool, error)

// Put adds a value to the key's sorted set. No-op if the exact
// key-value pair already exists. If FixedValueSize is set and
// len(value) does not match, returns ErrValueSizeMismatch.
func (ks *SetKeyspace) Put(key, value []byte) error

// Delete removes a key and all of its values. Uses bulk subtree
// retirement for nested B+trees — O(pages) not O(values).
func (ks *SetKeyspace) Delete(key []byte) error

// DeleteValue removes a single value from the key's sorted set.
// Returns ErrNotFound if the key or value does not exist.
func (ks *SetKeyspace) DeleteValue(key, value []byte) error

// DeleteRange deletes all keys in the range [start, end) and all
// their values.
func (ks *SetKeyspace) DeleteRange(start, end []byte) (uint64, error)

// CountValues returns the number of values for the given key.
// Returns 0 if the key does not exist.
func (ks *SetKeyspace) CountValues(key []byte) (uint64, error)

// Cursor returns a cursor for iterating over key-value pairs.
func (ks *SetKeyspace) Cursor() *SetCursor

// Range iterators. Each key-value pair (including each value in a
// set) is yielded separately.
func (ks *SetKeyspace) All() iter.Seq2[[]byte, []byte]
func (ks *SetKeyspace) Range(start, end []byte) iter.Seq2[[]byte, []byte]
func (ks *SetKeyspace) Prefix(prefix []byte) iter.Seq2[[]byte, []byte]

// Stats returns keyspace statistics.
func (ks *SetKeyspace) Stats() (KeyspaceStats, error)
```

**No `Get` method.** A set keyspace maps keys to sets, not to single
values. Returning the "first" value is arbitrary and misleading. Use
`Has` for existence, `HasValue` for membership, the cursor for iteration.

**SetCursor:**

```go
type SetCursor struct { /* unexported */ }

// --- Key navigation (same as Cursor) ---

func (c *SetCursor) First() (key, value []byte)
func (c *SetCursor) Last() (key, value []byte)
func (c *SetCursor) Next() (key, value []byte)
func (c *SetCursor) Prev() (key, value []byte)
func (c *SetCursor) Seek(target []byte) (key, value []byte)
func (c *SetCursor) SeekGE(target []byte) (key, value []byte)
func (c *SetCursor) Current() (key, value []byte)
func (c *SetCursor) Delete() error
func (c *SetCursor) Err() error

// --- Value navigation (within the current key's set) ---

// FirstValue positions the cursor at the first (smallest) value
// for the current key.
func (c *SetCursor) FirstValue() []byte

// LastValue positions the cursor at the last (largest) value
// for the current key.
func (c *SetCursor) LastValue() []byte

// NextValue moves to the next value for the current key. Returns
// nil when there are no more values (does NOT advance to the next
// key).
func (c *SetCursor) NextValue() (key, value []byte)

// PrevValue moves to the previous value for the current key.
// Returns nil when at the first value.
func (c *SetCursor) PrevValue() (key, value []byte)

// NextKey moves to the first value of the next key, skipping
// remaining values of the current key.
func (c *SetCursor) NextKey() (key, value []byte)

// PrevKey moves to the last value of the previous key, skipping
// remaining values of the current key.
func (c *SetCursor) PrevKey() (key, value []byte)

// SeekValue positions the cursor at the first value >= target
// for the current key. The cursor must already be positioned on
// a key. Returns nil if no value >= target exists for the current
// key.
func (c *SetCursor) SeekValue(target []byte) []byte

// CountValues returns the number of values for the current key.
func (c *SetCursor) CountValues() (uint64, error)
```

### Typed Variants

The typed generic layer follows the same split:

```go
// TypedKeyspace[K, V] wraps Keyspace with type-safe encoding.
type TypedKeyspace[K, V any] struct { /* ... */ }

func NewTypedKeyspace[K, V any](
    name string,
    keyEnc Encoder[K],
    valEnc Encoder[V],
) *TypedKeyspace[K, V]

// TypedSetKeyspace[K, V] wraps SetKeyspace with type-safe encoding.
type TypedSetKeyspace[K, V any] struct { /* ... */ }

func NewTypedSetKeyspace[K, V any](
    name string,
    keyEnc Encoder[K],
    valEnc Encoder[V],
    opts *SetKeyspaceOptions,
) *TypedSetKeyspace[K, V]
```

`TypedKeyspace` has `Get`, `Put`, `Delete` — straightforward.

`TypedSetKeyspace` has `Has`, `HasValue`, `Put`, `Delete`, `DeleteValue`,
`CountValues` — set operations.

```go
// TypedKS[K, V] is a handle to an opened typed keyspace within a transaction.
type TypedKS[K, V any] struct { /* ... */ }

func (t *TypedKS[K, V]) Get(key K) (V, error)
func (t *TypedKS[K, V]) Put(key K, value V) error
func (t *TypedKS[K, V]) Delete(key K) error
func (t *TypedKS[K, V]) DeleteRange(start, end *K) (uint64, error)
func (t *TypedKS[K, V]) Cursor() *TypedCursor[K, V]
func (t *TypedKS[K, V]) All() iter.Seq2[K, V]
func (t *TypedKS[K, V]) Range(start, end *K) iter.Seq2[K, V]
func (t *TypedKS[K, V]) Prefix(prefix K) iter.Seq2[K, V]

// TypedSetKS[K, V] is a handle to an opened typed set keyspace.
type TypedSetKS[K, V any] struct { /* ... */ }

func (t *TypedSetKS[K, V]) Has(key K) (bool, error)
func (t *TypedSetKS[K, V]) HasValue(key K, value V) (bool, error)
func (t *TypedSetKS[K, V]) Put(key K, value V) error
func (t *TypedSetKS[K, V]) Delete(key K) error
func (t *TypedSetKS[K, V]) DeleteValue(key K, value V) error
func (t *TypedSetKS[K, V]) CountValues(key K) (uint64, error)
func (t *TypedSetKS[K, V]) DeleteRange(start, end *K) (uint64, error)
func (t *TypedSetKS[K, V]) Cursor() *TypedSetCursor[K, V]
func (t *TypedSetKS[K, V]) All() iter.Seq2[K, V]
func (t *TypedSetKS[K, V]) Range(start, end *K) iter.Seq2[K, V]
func (t *TypedSetKS[K, V]) Prefix(prefix K) iter.Seq2[K, V]
```

### Keyspace Descriptor (On-Disk)

The keyspace B+tree stores a descriptor per keyspace. The descriptor
gains a `Kind` field to distinguish keyspace types:

```
Keyspace Descriptor
+----------+----------+----------+----------------+
| Root     | Count    | Kind     | FixedValueSize |
| uint64   | uint64   | uint8    | uint16         |
+----------+----------+----------+----------------+
```

- **Kind**: `0` = Keyspace, `1` = SetKeyspace.
- **FixedValueSize**: Only meaningful when `Kind == 1` and non-zero.
  Zero means variable-size values.

`Kind` replaces the `Flags` uint16 field. A single byte is sufficient —
there are two kinds, not a bitmask of independent flags. This also
eliminates the `ErrIncompatibleFlags` error — opening a `Keyspace` as a
`SetKeyspace` (or vice versa) is a type mismatch detected by comparing
`Kind`, reported as `ErrKeyspaceKindMismatch`.

Total descriptor size: 8 + 8 + 1 + 2 = 19 bytes (was 20 bytes with
the old `Flags uint16 + DupFixedSize uint16`). Could add 1 byte padding
to maintain 4-byte alignment: 20 bytes.

### Error Changes

| Old | New | Reason |
|-----|-----|--------|
| `ErrIncompatibleFlags` | `ErrKeyspaceKindMismatch` | Opening a keyspace with the wrong type |
| `ErrDupSizeFixed` | `ErrValueSizeMismatch` | Value size doesn't match `FixedValueSize` |
| `ErrMultiVal` | Removed | Was "ambiguous operation on multi-value key" — no longer possible since types are separate |

### Naming: What Happens to "DUPSORT" in the Design Doc

All prose references to "DUPSORT" and "DUPFIXED" are replaced:

| Old term | New term |
|----------|----------|
| DUPSORT keyspace | set keyspace |
| DUPFIXED keyspace | set keyspace with fixed-size values |
| duplicate values / duplicates | values (in a set keyspace) |
| duplicate set | value set |
| "a key's duplicates" | "a key's values" |
| subpage (for small duplicate sets) | subpage (for small value sets) |
| nested B+tree (for large duplicate sets) | nested B+tree (for large value sets) |

The internal storage mechanisms (subpage, nested B+tree, promotion,
demotion) remain unchanged — only the user-facing terminology changes.

### Storage: No Changes

The on-disk format is identical. The split is purely at the API level:

- `SetKeyspace` uses the same subpage and nested B+tree storage as
  the current DUPSORT implementation.
- `FixedValueSize` maps to the same flat-array subpage optimization.
- `CellFlags` bits are renamed (`MultiValue`, `NestedTree`) but the
  bit positions and semantics are unchanged.
- The keyspace descriptor changes `Flags uint16` to `Kind uint8` +
  `FixedValueSize uint16`, but the total size and the information
  content are equivalent.

### Delete Semantics

| Method | Behavior |
|--------|----------|
| `Keyspace.Delete(key)` | Delete the key and its value. `ErrNotFound` if key doesn't exist. |
| `SetKeyspace.Delete(key)` | Delete the key and all its values. Uses bulk subtree retirement. `ErrNotFound` if key doesn't exist. |
| `SetKeyspace.DeleteValue(key, value)` | Remove one value from the set. `ErrNotFound` if key or value doesn't exist. When the last value is removed, the key is also removed — empty sets never exist. |
| `SetCursor.Delete()` | Delete the current key-value pair. On a set keyspace, this deletes the current value from the set (not all values for the key). If it was the last value, the key is also removed. |

### Iteration Semantics

Both `Keyspace` and `SetKeyspace` yield `(key, value)` pairs from their
`iter.Seq2` iterators and their cursor's `Next()`/`Prev()` methods.

For a `Keyspace`, each key appears once.

For a `SetKeyspace`, each key appears once per value in its set. The
`Next()` method advances through values within a key's set before
moving to the next key. This matches the behavior expected from
`for k, v := range sks.All()` — you see every key-value pair.

`SetCursor.NextKey()` and `PrevKey()` provide the "skip to next/previous
key" navigation when the caller only cares about keys, not individual
values.

### Open Questions

1. **Should `Keyspace.Delete` return `ErrNotFound` or be a silent no-op?**
   BoltDB's `Delete` is a no-op on missing keys. LMDB returns
   `MDB_NOTFOUND`. Silent no-op is more convenient for callers who
   don't care. `ErrNotFound` is more precise. The design doc currently
   doesn't specify. Leaning toward no-op for `Keyspace.Delete` (you're
   saying "make this key not exist" — if it already doesn't, that's
   success) and `ErrNotFound` for `SetKeyspace.DeleteValue` (you're
   asking to remove a specific element from a set — if it's not there,
   that's meaningful information).

2. **Should `SetKeyspace` have a `Get(key) ([]byte, error)` that returns
   the first value?** Current proposal says no. But some callers may
   want "give me any value for this key" as a quick existence-plus-data
   check. `Has` covers existence. If you need a value, use the cursor.
   Keeping `Get` off `SetKeyspace` avoids the "which value?" confusion.

3. **Should `SetKeyspace.Put` return a `bool` indicating whether the
   value was already present?** Useful for callers who need to know if
   the set grew. Cost: reading the existing set before writing. The
   B+tree already does this during insert (to find the insertion point),
   so the information is available. Leaning toward yes:
   `Put(key, value []byte) (added bool, err error)`. But this breaks
   symmetry with `Keyspace.Put` which doesn't need this.
