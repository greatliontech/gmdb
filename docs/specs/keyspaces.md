# Keyspaces

A **keyspace** is a named B+tree in the data file. The root meta
page points to a **keyspace B+tree** whose keys are keyspace names
and whose values are keyspace descriptors. Only USER keyspaces live
in this tree: index storage hangs off each owning keyspace's
descriptor (`IndexRegistryRoot` → the per-keyspace registry
sub-tree, whose entries carry the index data-tree roots —
`indexing.md §Storage Layout`), never as descriptor rows of its
own.

This spec defines the descriptor format, the keyspace `Kind`
discriminator (Keyspace vs SetKeyspace vs engine-internal),
per-keyspace mutable configuration, and the API-layer split between
`Keyspace` and `SetKeyspace`. Per-set-member storage and PK
encodings live in `set-keyspace.md`; indexing storage lives in
`indexing.md`.

Scope:
- Keyspace descriptor (40 bytes, stored on disk).
- `Kind` enumeration.
- Per-keyspace configuration (`RestartGroupTarget`,
  `FixedValueSize`, `NextSeq`).
- Keyspace-name interning (`unique.Handle[string]`).
- API-level type split (`Keyspace` for key→value, `SetKeyspace`
  for key→sorted-set).
- Iteration semantics.
- Error-class rationale for type mismatch.

Depends on / interacts with:
- `file-layout.md` for the meta-page `KeyspaceRoot` /
  `NumKeyspaces`.
- `page-formats.md` for the leaf restart-group structure (and
  the uncompressed-leaf variant selected at
  `RestartGroupTarget == 1`) that `RestartGroupTarget`
  configures.
- `set-keyspace.md` for SetKeyspace storage and per-member API
  detail.
- `indexing.md` for `IndexRegistryRoot` semantics.
- `api-surface.md` for Go-level signatures of all keyspace APIs.

## Invariants

Invariant: kind=clause-explicit;
  property=The keyspace descriptor is a fixed 40-byte struct
    stored as the value for the keyspace's entry in the keyspace
    B+tree. The exact field order and sizes are:
    `Root(u64) Count(u64) Kind(u8) FixedValueSize(u16)
    NextSeq(u64) RestartGroupTarget(u16)
    IndexRegistryRoot(u64) Reserved[3]byte`;
  from=this spec §Keyspace Descriptor;
  violation=Mis-sized or reordered fields break every
    keyspace-table read on disk; an extra byte shifts every
    descriptor's `IndexRegistryRoot` into the reserved space and
    silently disconnects every index registry from its keyspace.

Invariant: kind=clause-explicit;
  property=`Kind` is one of `0` (Keyspace), `1` (SetKeyspace),
    `2` (RESERVED: engine-internal index keyspace — the current
    engine never CREATES Kind=2 rows, since index storage hangs
    off `IndexRegistryRoot` instead; the value is defended
    everywhere so a forged/corrupt descriptor cannot smuggle one
    past the guards). `Open()` rejects descriptors with unknown
    `Kind` values. `Kind` is set at creation and immutable;
  from=this spec §Keyspace Descriptor;
  violation=A mutable `Kind` lets a SetKeyspace silently
    transmute into a single-value Keyspace at a future open —
    `Get` then returns one arbitrary set member while the rest
    are silently dropped from the user-visible API.

Invariant: kind=clause-explicit;
  property=Opening a keyspace whose stored `Kind` does not match
    the API used (e.g., `OpenKeyspace` on a `Kind = 1`
    SetKeyspace) returns `ErrKeyspaceKindMismatch` without
    modifying state;
  from=this spec §API split;
  violation=A silent kind-coercion (or ignoring `Kind` on
    open) lets writes go through the wrong code path —
    SetKeyspace `Put` semantics on a single-value Keyspace
    rewrites the only value instead of adding to a set.

Invariant: kind=clause-explicit;
  property=Attempting to open an engine-internal index keyspace
    (`Kind = 2`) via the user API returns `ErrKeyspaceReserved`.
    `ListKeyspaces()` filters out `Kind = 2` entries;
  from=this spec §`Kind` enumeration + `api-surface.md`;
  violation=Letting user code open an index keyspace directly
    permits user writes that bypass the registry-managed index
    invariants — every subsequent `Lookup` returns stale or
    invented results.

Invariant: kind=clause-explicit;
  property=`FixedValueSize` is meaningful only when `Kind == 1`
    and non-zero. It is immutable after creation, and
    `Open()` rejects descriptors with `FixedValueSize != 0
    AND Kind != 1`;
  from=this spec §Keyspace Descriptor;
  violation=A mutable or wrong-kind `FixedValueSize` silently
    re-interprets the on-disk subpage entry stride (no
    `ValueLen` prefix when fixed; explicit prefix when
    variable) — every existing entry decodes garbage.

Invariant: kind=clause-explicit;
  property=`RestartGroupTarget` is mutable via
    `Tx.SetKeyspaceConfig()`. The new value is a builder hint
    for leaves written after the change. Existing leaves keep
    their stored group structure — the per-page restart table
    records explicit group counts (see `page-formats.md §Leaf
    Page §Compressed Leaf`) — and are not retroactively
    re-encoded; they migrate to the new shape only when they
    next split, merge, or are rebuilt. `RestartGroupTarget == 1`
    selects the uncompressed leaf variant
    (`TypeLeafUncompressed`); the keyspace can hold a mix of
    compressed and uncompressed pages during a transition;
  from=this spec §Per-Keyspace Configuration + `page-formats.md
    §Leaf Page`;
  violation=Retroactively rewriting all leaves on
    `SetKeyspaceConfig` is an unbounded write cost the API
    does not advertise; conversely, ignoring the new value
    forever defeats the configuration knob.

Invariant: kind=entailed;
  property=A keyspace's `IndexRegistryRoot` is `0` iff no
    indexes are declared on the keyspace. Adding the first
    index allocates the registry sub-tree and updates the
    descriptor; dropping the last index resets `Root` to `0`;
  from=entailed: the registry is a sub-tree rooted at this
    field, with no separate "empty index set" representation
    (`indexing.md`);
  violation=A non-zero `IndexRegistryRoot` pointing at an
    empty or unallocated page corrupts the next opener's
    registry walk; a zero root for a keyspace that has
    indexes leaks every index entry (no registry → no
    lookup).

Invariant: kind=clause-explicit;
  property=The meta-page `NumKeyspaces` field equals the count
    of leaf entries in the keyspace B+tree — **all leaves,
    including engine-internal `Kind = 2` entries**. It is **not**
    the user-visible keyspace count (which `ListKeyspaces` filters
    out of `Kind = 2`). Every write that adds or removes a
    descriptor from the keyspace B+tree adjusts `NumKeyspaces` by
    ±1, regardless of `Kind`. Any user-facing surface that exposes
    a keyspace count (e.g. a future `DBStats.NumKeyspaces`)
    derives the user-visible count via a cursor walk over the
    keyspace B+tree filtering `Kind == 2`, the same machinery
    `ListKeyspaces` already uses — no separate persisted
    counter is maintained;
  from=this spec §Keyspace Descriptor + `file-layout.md
    §Meta Page Layout` `NumKeyspaces` field;
  violation=Diverging interpretations — "leaf count" in one code
    path and "user-visible count" in another — make
    `NumKeyspaces` non-canonical: an audit-mode integrity check
    using one definition and a `DBStats` surface using the other
    silently disagree when any `Kind = 2` keyspace exists,
    leaking the divergence into operational dashboards and
    `Check()` reports.

(User-locked decision; ratifies the implemented shape as
spec-correct.)

## Keyspace Descriptor

```
Keyspace Descriptor (40 bytes)
+----------+----------+----------+----------------+----------+--------------+--------------------+----------+
| Root     | Count    | Kind     | FixedValueSize | NextSeq  | RestartGroup | IndexRegistryRoot  | Reserved |
| uint64   | uint64   | uint8    | uint16         | uint64   | uint16       | uint64             | [3]byte  |
+----------+----------+----------+----------------+----------+--------------+--------------------+----------+
```

Total: `8 + 8 + 1 + 2 + 8 + 2 + 8 + 3 = 40` bytes.

- **Root** (uint64): page ID of this keyspace's B+tree root.
  `0` ⇒ empty.
- **Count** (uint64): number of key-value pairs. For a
  SetKeyspace, total pairs across all value sets.
- **Kind** (uint8): `0` = Keyspace (key → value), `1` =
  SetKeyspace (key → sorted set of values), `2` = RESERVED
  (engine-internal index keyspace; never created by the current
  engine — index storage hangs off `IndexRegistryRoot` — but
  defensively rejected at every user surface). `Open()` rejects
  unknown values. Set at creation, immutable.
- **FixedValueSize** (uint16): for SetKeyspace, the fixed value
  size in bytes (`0` = variable). Must be `0` when `Kind != 1`.
- **NextSeq** (uint64): next sequence number for `NextSequence()`.
  First call returns `1`.
- **RestartGroupTarget** (uint16): per-keyspace target leaf
  restart-group size, bounded to `[0, 255]`. `0` ⇒ engine
  default (16). `1` ⇒ uncompressed leaves
  (`TypeLeafUncompressed`); `[2, 255]` ⇒ compressed leaves
  (`TypeLeaf`) with variable-size groups capped at the target.
  Values `> 255` are rejected by `Tx.SetKeyspaceConfig()` with
  `ErrInvalidOptions` and by descriptor validation at open with
  `ErrCorrupted` — correctly different classes: the config API
  gates every legitimate write path, so an out-of-range stored
  value can only be on-disk corruption — the
  compressed-leaf restart-table `Count` field is `uint8`, so 255
  is the hard physical cap (see `page-formats.md §Compressed
  Leaf`). Set at creation, mutable via `Tx.SetKeyspaceConfig()`
  — new value is a builder hint for leaves written after the
  change; existing leaves keep their stored group structure
  until they next split, merge, or are rewritten. The keyspace
  can hold a mix of compressed and uncompressed leaves during
  a transition (per the per-keyspace invariant above and
  `page-formats.md §Leaf Page`).
- **IndexRegistryRoot** (uint64): page ID of this keyspace's
  per-keyspace index registry sub-tree (see `indexing.md
  §Storage Layout`). `0` ⇒ no indexes declared on this
  keyspace.
- **Reserved** (3 bytes): must be zero. `Open()` rejects
  descriptors with non-zero reserved bytes.

Depth (tree height) is not persisted — derived by reading the
root page on first access. Avoids maintaining a redundant
field across split/merge/rebalance.

Opening a keyspace reads the descriptor from the keyspace
B+tree. Modifications update the descriptor (and its root),
which propagates up through the keyspace B+tree via CoW.

Opening a keyspace with the wrong type (`OpenKeyspace` on a
SetKeyspace, etc.) returns `ErrKeyspaceKindMismatch`.
Attempting to open an engine-internal index keyspace via the
user API returns `ErrKeyspaceReserved`.

## Per-Keyspace Configuration

Two per-keyspace properties currently:

- `FixedValueSize` — SetKeyspace only, immutable after creation.
- `RestartGroupTarget` — mutable via `Tx.SetKeyspaceConfig()`.
  Defaults to engine-global 16. Tune higher (e.g. 32) for
  keyspaces with very long shared prefixes (directory listings,
  deeply nested composite keys); tune lower (e.g. 8) for
  keyspaces with mostly distinct keys to reduce per-`Prev()`
  group decode cost.

`SetKeyspaceConfig` works whether or not the keyspace is open in
the transaction, and its effect is order-independent within the
transaction: a later same-tx open of the keyspace observes the
new configuration, and the change persists at commit regardless
of intervening opens (the same rule index administration follows,
`indexing.md §Removing an Index`).

Per-keyspace page size is **not** supported — single file with
uniform page size is a core design strength (see
`overview.md §Design Decisions`).

## Keyspace Name Interning

Keyspace names are interned via `unique.Make[string]` (Go
1.23+). The internal keyspace lookup cache stores a
`unique.Handle[string]` instead of a raw `string` or `[]byte`.
Avoids repeated allocations when the same keyspace is opened
across many transactions. `unique.Handle` provides O(1)
equality comparison and is safe for concurrent use.

## API-level type split: `Keyspace` vs `SetKeyspace`

Two distinct Go types map to the two `Kind` values. The split
exists at the API level only — the on-disk page format is
identical between them aside from the cell-flag bits and the
`Kind` discriminator.

| Operation | `Keyspace` | `SetKeyspace` |
|-----------|------------|----------------|
| `Put(k, v)` | Replace existing value | Add `v` to the key's set (no-op if exact pair exists) |
| `Get(k)` | Return the value | Not defined — use `Has`, `HasValue`, or the cursor |
| `Delete(k)` | Delete the key and its value | Delete the key and all its values (bulk subtree retirement) |
| Delete one value | N/A | `DeleteValue(k, v)` or `SetCursor.Delete()` |
| Cursor navigation | Keys only | Keys plus intra-key value navigation |

A `Keyspace` carries no flags: it is unconditionally a key→value
map. A `SetKeyspace` is created via `CreateSetKeyspace(name,
*SetKeyspaceOptions)` where `SetKeyspaceOptions.FixedValueSize`
optionally pins values to a uniform size (see `set-keyspace.md
§Fixed-Size Value Sets`).

`SetKeyspace` deliberately does not expose `Get` — returning an
arbitrary (e.g. first) value would be misleading because the
storage model is genuinely a set, not a multimap with positional
semantics. Use `Has` for existence, `HasValue` for membership,
or a cursor for iteration.

For Go-level signatures, see `api-surface.md §Keyspace API`.

## Iteration Semantics

Both `Keyspace` and `SetKeyspace` yield `(key, value)` pairs
from their `iter.Seq2` iterators and their cursor's `Next()` /
`Prev()` methods.

- For a `Keyspace`, each key appears once.
- For a `SetKeyspace`, each key appears once per value in its
  set. `Next()` advances through values within a key's set
  before moving to the next key. This matches the behaviour
  expected from `for k, v := range sks.All()` — every
  key-value pair is yielded.

`SetCursor.NextKey()` / `PrevKey()` skip the remainder of the
current key's value set when the caller only cares about
top-level keys (see `set-keyspace.md` and `api-surface.md`).
