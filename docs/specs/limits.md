# Limits

Hard limits and engine-enforced bounds. Most limits derive from
the page format (see `page-formats.md`) and the file format (see
`file-format.md`); this spec is the single place to look up
worst-case sizes.

Scope:
- Page size.
- Maximum key size.
- Maximum value size (single-value keyspaces and set
  keyspaces).
- Maximum index key size.
- Maximum indexes per keyspace.

## Invariants

Invariant: kind=clause-explicit;
  property=`Put()` returns `ErrKeyTooLarge` for any key whose
    length exceeds the encoding bound (`KeyTotalLen` is
    `uint32`); keys up to that bound are storable — over the
    inline threshold `T` they take the overflow-key form
    (`page-formats.md §Overflow-Key Cells`), never a rejected
    or truncated one;
  from=this spec §Maximum Key Size;
  violation=A key past the encoding bound that reaches the
    builders wraps `KeyTotalLen` — the cell decodes a shorter
    key than was stored, silently aliasing distinct keys;
    rejecting below the bound reintroduces an arbitrary cap the
    overflow-key mechanism exists to remove.

Invariant: kind=clause-explicit;
  property=Set-keyspace values share the key bound (each value
    is a key in the nested B+tree — over-`T` values become
    overflow-key members); `FixedValueSize` set keyspaces are
    the exception: `FixedValueSize` must be `<= T`, validated
    at creation (`ErrInvalidOptions`), because fixed-stride
    nested leaves and subpages carry no per-value length and
    cannot hold an extent reference;
  from=this spec §Maximum Value Size (Set Keyspaces);
  violation=A `FixedValueSize > T` keyspace admits values whose
    nested-leaf cells cannot be encoded at the declared stride —
    the direct offset arithmetic reads garbage entries; without
    the create-time gate the failure surfaces only on the first
    over-`T` insert.

Invariant: kind=clause-explicit;
  property=The engine does not enforce a hard upper bound on
    the number of indexes per keyspace; bounds come from the
    per-keyspace index registry tree's capacity and from the
    cost of running every extractor on every write;
  from=this spec §Maximum Indexes Per Keyspace;
  violation=A surprise hard cap would surface as
    `ErrIndexExists` on a legitimate `OpenKeyspace` call —
    the API contract is to let the user pay the cost of as
    many indexes as fit, not to second-guess them.

## Page Size

Configurable at creation. Power of 2 in `[4 KB, 64 KB]`. Stored
in meta, immutable. Default: 4096 bytes. See
`file-layout.md §Meta Page`.

## Maximum Key Size

Keys are not bounded by the page size. The engine bound is the
encoding's `KeyTotalLen uint32` — `2^32 - 1` bytes — enforced
deterministically at every entry gate (`btree.Put`,
`btree.PutEntry` for set keys, the bulk-load builders including
set members, and CopyTo's rebuild: one threshold, no drift — the
same input is accepted or rejected identically on every path;
pinned by TestErrKeyTooLargeSentinel and
TestKeyTooLargeDeterministicAtBound).
Practical bounds arrive earlier: every deep
comparison materializes shared-prefix bytes (`MaxTxBufferBytes`
bounds steady-state slab memory, not written volume — excess spills
at operation boundaries).

Keys up to the **inline threshold `T`** are stored in the inline
cell forms; longer keys take the overflow-key form —
`page-formats.md §Overflow-Key Cells`, which also derives `T`:

| Page Size | `T` (with PageChecksum) | `T` (without) |
|-----------|------------------------:|--------------:|
| 4 KB | 2010 | 2014 |
| 8 KB | 4058 | 4062 |
| 16 KB | 8154 | 8158 |
| 64 KB | 32730 | 32734 |

`T` is a density/latency knee, not a limit: past it, each key
costs a key-extent run (≥ 1 page) plus an extent read on
comparisons that tie through the inline portion. Workloads whose
keys routinely exceed `T` should prefer a larger page size for
density even though correctness no longer requires it.

Leaf prefix compression stores full keys at restart points and
suffix deltas within a group; delta-reconstructed keys are always
`<= T` (overflow-key entries are singleton restart groups —
`page-formats.md §Overflow-Key Cells`).

## Maximum Value Size

**Single-value keyspaces.** Inline values are limited by leaf-
page free space. Larger values are automatically stored as
overflow pages. No practical upper limit (bounded by disk space
and `MaxSize`). See `page-formats.md §Overflow Page`.

## Maximum Value Size (Set Keyspaces)

Each value becomes a key in the nested B+tree (or entry in a
subpage), so set values share the key bound above: nested-tree
members over `T` are overflow-key cells. `T` binds a value only
in its KEY role — a subpage stores value bytes with its own
length bookkeeping, so an over-`T` value may legally reside in a
subpage while the set stays below the promotion threshold (for
keys of ordinary length the window `(T, per-key budget]` exists at
every page size; the budget is the 50% threshold capped by the
per-key splittability bound, so long — overflow-form — keys skip
the subpage window and go straight to nested trees —
`set-keyspace.md §Subpage Promotion Threshold`); promotion
converts such values to overflow-key members. Exception:
`FixedValueSize` set keyspaces require `FixedValueSize <= T`,
validated at creation (`ErrInvalidOptions`) — fixed-stride
storage carries no per-value length field and cannot hold an
extent reference.

## Maximum Index Key Size

Composite index key = NUL-escaped column tuple (+ PK suffix for
non-unique indexes). After escaping, the key is stored in the
index keyspace's leaf and shares the ordinary key bound — the
escape encoding's worst-case doubling for NUL-heavy columns is a
density cost, no longer a correctness cliff.

See `page-formats.md §NUL-escape encoding` and `indexing.md
§Column Encoding`.

## Maximum Indexes Per Keyspace

Bounded only by the per-keyspace index registry tree's capacity
(thousands per keyspace at typical page sizes). The engine does
not enforce a hard limit — practical limits come from the cost
of running every extractor on every write.
