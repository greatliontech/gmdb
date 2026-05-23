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
  property=`Put()` returns `ErrKeyTooLarge` for any key
    exceeding the maximum derived from the branch-page budget
    (see Maximum Key Size below);
  from=this spec §Maximum Key Size;
  violation=A key larger than the branch can hold cannot be
    used as a separator at split time — the split aborts and
    the tree is corrupted at the failing branch.

Invariant: kind=clause-explicit;
  property=Set-keyspace values are bounded by the maximum key
    size (each value becomes a key in the nested B+tree's
    leaf). `Put()` with an over-sized value returns
    `ErrKeyTooLarge`. Overflow pages are NOT used for set-
    keyspace values;
  from=this spec §Maximum Value Size (Set Keyspaces);
  violation=An over-sized set value cannot be stored in either
    a subpage (no overflow path) or a nested leaf — silent
    truncation or out-of-bounds writes.

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

Determined by page size. A branch page must fit at least 2 keys
to allow splitting. Fixed overhead: 16 bytes (8-byte header +
8-byte leftmost child pointer). Each key needs 4 bytes (cell
directory) + key bytes + 8 bytes (child pointer). Maximum key
size approximately `(PageSize - 40) / 2`, less 4 bytes when
`PageChecksum` is enabled (8-byte footer instead of 4-byte
CRC32C in earlier designs):

| Page Size | Max Key Size (no checksum) | With PageChecksum (xxhash64) |
|-----------|----------------------------|------------------------------|
| 4 KB | ~2028 bytes | ~2024 bytes |
| 8 KB | ~4076 bytes | ~4072 bytes |
| 16 KB | ~8172 bytes | ~8168 bytes |
| 64 KB | ~32748 bytes | ~32744 bytes |

Enforced at `Put()`. Keys exceeding return `ErrKeyTooLarge`.

The limit applies to branch separator capacity. Leaf prefix
compression can store keys up to this size at restart points
(full keys). Delta entries store only the unshared suffix, so
their on-disk size is smaller, but the reconstructed full key
must still fit the branch limit.

## Maximum Value Size

**Single-value keyspaces.** Inline values are limited by leaf-
page free space. Larger values are automatically stored as
overflow pages. No practical upper limit (bounded by disk space
and `MaxSize`). See `page-formats.md §Overflow Page`.

## Maximum Value Size (Set Keyspaces)

Each value becomes a key in the nested B+tree (or entry in a
subpage). Maximum value size = maximum key size — approximately
`(PageSize - 40) / 2`. Overflow pages are not used for set
keyspace values. A `Put()` with an over-sized value returns
`ErrKeyTooLarge`.

## Maximum Index Key Size

Composite index key = NUL-escaped column tuple (+ PK suffix for
non-unique indexes). After escaping, the key is stored in the
index keyspace's leaf, subject to the same maximum key size as
ordinary keys. The escape encoding can up to double the byte
count for columns with many NULs; tooling should reject column
values that would exceed the limit at the declaration layer.

See `page-formats.md §NUL-escape encoding` and `indexing.md
§Column Encoding`.

## Maximum Indexes Per Keyspace

Bounded only by the per-keyspace index registry tree's capacity
(thousands per keyspace at typical page sizes). The engine does
not enforce a hard limit — practical limits come from the cost
of running every extractor on every write.
