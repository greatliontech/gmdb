# Page Formats

On-disk formats for the per-page structures stored in data pages:
branch pages (internal B+tree nodes), leaf pages (prefix-compressed
key-value storage), and overflow pages (large value storage). Set-
keyspace subpages and nested B+tree references are leaf-cell variants
defined in `set-keyspace.md`. RPL segment pages live in
`free-space.md`.

Scope:
- Branch page layout, prefix-truncated separator computation.
- Leaf page layout with prefix-compressed restart groups, restart
  vs delta entries, overflow references.
- Leaf lookup, insert/delete, split, cursor key reconstruction.
- Overflow page format and run construction.
- NUL-escape encoding for composite index keys.

Depends on / interacts with:
- `file-layout.md` for the common page header.
- `checksums.md` for per-page xxhash64 footers.
- `indexing.md` consumes the NUL-escape rules defined here.
- `limits.md` derives max-key-size from the branch-page budget.

## Invariants

Invariant: kind=clause-explicit;
  property=A branch separator `S` between left child `L` and right
    child `R` satisfies `max(L) < S <= min(R)` — every key in the
    left subtree compares strictly less than `S`, every key in the
    right subtree compares greater than or equal to `S`;
  from=this spec §Prefix-Truncated Branch Keys;
  violation=A separator outside this range routes a search to the
    wrong subtree (key with `S <= k < min(R)` falls left and is not
    found; key with `max(L) < k < S` falls right and is not found),
    so Get returns `ErrNotFound` for keys that are actually present —
    silent data loss to the caller.

Invariant: kind=clause-explicit;
  property=Within a leaf restart group, the entry at the group's
    restart position stores a full key; every subsequent delta entry
    in the group encodes its key as `previous[0:SharedLen] ||
    UnsharedKey`;
  from=this spec §Leaf Page (Restart entry / Delta entry);
  violation=A delta entry whose `SharedLen` does not actually share
    `SharedLen` bytes with the previous entry's full key
    reconstructs a wrong key — cursor reads return wrong keys and
    binary search lands in the wrong group.

Invariant: kind=clause-explicit;
  property=A leaf's `RestartCount × 2` bytes immediately before the
    optional 8-byte xxhash64 footer constitute the restart table;
    the restart table indexes the byte offsets of restart-point
    entries in this page (and only this page);
  from=this spec §Leaf Page (page layout);
  violation=Misplacing the restart table (relative to the optional
    checksum footer) corrupts the binary-search index — lookups
    diverge into delta entries treated as restart points and decode
    garbage keys.

Invariant: kind=clause-explicit;
  property=An overflow run of `1 + N` pages stores
    `(PageSize - 8) + N * PageSize` bytes of value (subtract 8 per
    page for the footer when `PageChecksum` is enabled). The first
    page carries `AdditionalPages = N` in its header; follower pages
    carry no header;
  from=this spec §Overflow Page;
  violation=Reading a value with the wrong run-length truncates the
    value (short read) or runs past the run into another page
    (returning interleaved bytes from an unrelated allocation).

Invariant: kind=clause-explicit;
  property=The NUL-escape encoding (every `0x00` inside a column →
    `0x00 0xFF`; column boundary → `0x00 0x00`) is prefix-free: no
    escaped column is a prefix of another, and the column terminator
    `0x00 0x00` never appears inside an escaped column;
  from=this spec §NUL-escape encoding;
  violation=A column whose escaped form is a prefix of another's
    breaks lex ordering of the concatenated tuple — index range
    queries return wrong matches, and a unique-index probe can
    accept a "duplicate" because the prefix tuple sorts adjacent.

Invariant: kind=entailed;
  property=A full key reconstructed from any leaf delta entry must
    fit within the maximum key size derived in `limits.md` from
    the branch-page budget;
  from=entailed: branch separator capacity caps full-key size, and
    leaves can hold separators that propagate to branches;
  violation=An over-size full key cannot be promoted on split — the
    split aborts mid-operation and the tree is left in an
    inconsistent state.

## Branch Page (Internal B+tree Node)

Branch pages store keys and child page pointers. They do NOT store
values.

```
Branch Page
+-----------------------+
| Page Header (8 bytes) |
+-----------------------+
| Ptr[0] (uint64)       |  leftmost child pointer (8 bytes)
+-----------------------+
| Cell Directory        |  Array of (Offset uint16, KeyLen uint16)
| ...                   |  grows forward, 4 bytes per cell
+-----------------------+
|       free space      |
+-----------------------+
| ...                   |
| Cell Data 1           |  packed from end of page, grows backward
| Cell Data 0           |
+-----------------------+
```

Each cell in the data area:

```
Branch Cell
+----------+----------+
| Key bytes| ChildPtr |
|          | uint64   |
+----------+----------+
```

Keys are stored in sorted order. For a branch with N cells (N keys)
there are N+1 child pointers: `Ptr[0]` (leftmost, stored after the
page header) plus one `ChildPtr` per cell.

Search algorithm: binary-search the cell directory for the first
separator `Key[i]` such that `target < Key[i]`. If found, descend to
the child to the left of that separator — `ChildPtr` of cell `i-1`,
or `Ptr[0]` when `i == 0`. If no separator is greater than the
target, descend to the last cell's `ChildPtr` (rightmost child).
When `target == Key[i]`, the target belongs in the right child since
separators are lower bounds of the right child.

The cell directory stores `(Offset, KeyLen)` per cell, enabling
binary search over variable-length keys without parsing the key data
area.

### Prefix-Truncated Branch Keys

Branch pages store **prefix-truncated separator keys** — the shortest
byte string that distinguishes the left subtree from the right —
rather than full keys copied from leaf pages. A branch separator
satisfies:

- Every key in the left child compares **strictly less than** the
  separator.
- Every key in the right child compares **greater than or equal to**
  the separator.

Equivalently: `max(left) < separator <= min(right)`. The separator
is a lower bound of the right child.

**Separator computation** at leaf split: let `L` = the last key of
the left leaf, `R` = the first key of the right leaf. Compute the
shortest byte string `S` such that `L < S <= R` — the common prefix
of `L` and `R`, extended by one byte from `R` at the first
divergence position:
`S = R[0 : len(commonPrefix) + 1]`.
Insert `S` (not `R`) into the parent branch page.

At merge time, the separator is removed from the parent. At
redistribute time, the separator is recomputed from the new boundary
keys and the parent updated.

**Complementary with leaf prefix compression**: branch pages compress
keys *across* tree levels (separator < either boundary); leaf pages
compress redundancy *within* a page. The two techniques are
independent and complementary.

**Interaction with maximum key size**: the maximum-key-size limit
applies to full keys stored in leaves (reconstructed from delta
encoding). Branch separators are always shorter than or equal to the
full keys.

**Benefits**: higher fan-out → shallower trees → fewer page accesses
per lookup; less data read per branch traversal.

## Leaf Page

Leaf pages store the actual key-value pairs using **prefix
compression** — keys that share common prefixes with their neighbours
are stored as deltas.

```
Leaf Page
+-----------------------+
| Page Header (8 bytes) |
+-----------------------+
| RestartInterval uint16|  per-keyspace target (default 16)
| RestartCount uint16   |  number of restart points
+-----------------------+
| Entry 0 (restart)     |  entries in forward order, starting at fixed offset 12
| Entry 1 (delta)       |
| ...                   |
| Entry N               |
+-----------------------+
|       free space      |
+-----------------------+
| Restart Table         |  array of (Offset uint16), one per restart point
| ...                   |  RestartCount × 2 bytes, packed at content end
+-----------------------+
```

`RestartInterval` is the target restart interval set per keyspace via
`RestartGroupTarget` (see `keyspaces.md`). It is stored per page
so the leaf is self-describing for `Check()` and cursor decode —
changing the keyspace's `RestartGroupTarget` does not retroactively
recode existing leaves.

Entries are stored in forward memory order starting at a fixed offset
(12) because prefix compression requires sequential scanning. The
restart table is at the end of the page (before the optional xxhash64
footer). The reader locates the restart table at
`contentEnd - RestartCount × 2`, where `contentEnd = PageSize - 8`
when checksums are enabled and `PageSize` otherwise.

Entries at positions `0`, `K`, `2K`, … (where `K = RestartInterval`)
are **restart points** that store full keys. All other entries are
**delta entries** that encode the key as a difference from the
previous entry.

Each entry carries a `CellFlags` byte to distinguish cell formats:

```
CellFlags bit layout
Bit 0:    Overflow    (0 = inline value, 1 = overflow reference)
Bit 1:    MultiValue  (0 = single value, 1 = multi-value data — subpage or nested B+tree)
Bit 2:    NestedTree  (only when Bit 1 set: 0 = subpage, 1 = nested B+tree)
Bits 3-7: Reserved (must be 0)
```

`Overflow` and `MultiValue` are mutually exclusive in practice.

### Restart entry (full key, at positions 0, K, 2K, …)

```
Restart Entry (inline)
+-----------+----------+----------+-----------+-----------+
| CellFlags | KeyLen   | ValueLen | Key bytes | Val bytes |
| uint8     | uint16   | uint32   |           |           |
+-----------+----------+----------+-----------+-----------+
```

### Delta entry

```
Delta Entry (inline)
+-----------+-----------+-------------+----------+---------------+-----------+
| CellFlags | SharedLen | UnsharedLen | ValueLen | UnsharedKey   | Val bytes |
| uint8     | uint16    | uint16      | uint32   |               |           |
+-----------+-----------+-------------+----------+---------------+-----------+
```

`SharedLen` = leading bytes shared with the previous entry in the
same restart group. `UnsharedKey` contains only the bytes after the
shared prefix. Full key = first `SharedLen` bytes of the previous
entry's full key + `UnsharedKey`.

Delta entries cost 2 extra bytes per entry but save `SharedLen` bytes
of key data. Net saving per entry is `SharedLen - 2` bytes — positive
whenever keys share more than a 2-byte prefix.

`ValueLen` is `uint32` (max ~4 GB for inline values; bounded in
practice by leaf-page free space). Values exceeding leaf-page
capacity are stored as overflow pages, referenced via the formats
below which use `uint64 TotalLen`.

### Overflow reference at a restart point (CellFlags bit 0 set)

```
Restart Overflow Reference
+-----------+----------+-----------+----------+----------+
| CellFlags | KeyLen   | Key bytes | OvflPage | TotalLen |
| uint8     | uint16   |           | uint64   | uint64   |
+-----------+----------+-----------+----------+----------+
```

### Overflow reference at a delta position

```
Delta Overflow Reference
+-----------+-----------+-------------+---------------+----------+----------+
| CellFlags | SharedLen | UnsharedLen | UnsharedKey   | OvflPage | TotalLen |
| uint8     | uint16    | uint16      |               | uint64   | uint64   |
+-----------+-----------+-------------+---------------+----------+----------+
```

### Leaf Lookup

Two-phase binary search:

1. **Binary search over restart points** using the restart table.
   `O(log R)` where `R = RestartCount`.
2. **Linear scan within the restart group**, decoding delta entries
   until the target is found or passed. `O(K)` where
   `K = RestartInterval`.

Total: `O(log(n/K) + K)`. For a leaf with 30 entries at `K = 16`,
about 17 comparisons; the linear scan operates on data already in L1
cache.

### Leaf Density

Depends on the ratio of shared-prefix length to total key length. For
200-byte keys sharing a 150-byte common prefix + 50-byte values at
4 KB:

| Format | Bytes/entry (avg) | Entries/page | Improvement |
|--------|-------------------|-------------|-------------|
| Full keys | ~260 | ~15 | baseline |
| Prefix compressed (K=16) | ~117 | ~33 | 2.2× |

Short low-prefix keys see ~5% improvement. Compression adapts
automatically — high-prefix workloads benefit; random keys pay 2
bytes/entry overhead.

### Insert and Delete

Inserting a key between two delta entries within a restart group:

1. Encode the new entry as a delta against its predecessor.
2. Recompute the successor entry's delta (its `SharedLen` is now
   relative to the new entry).
3. If insertion shifts entry indices, restart-point positions may
   change — re-encode the affected group boundaries.

Deletion is symmetric. The restart table is rebuilt after any insert
or delete — `O(RestartCount)`, at most ~20 entries for a full leaf
page.

Hot-path insert/delete should splice the page in place rather than
full decode + re-encode of all entries
(`tryInsertAtCompressed` / `tryDeleteAtCompressed`). Decoded-form
fallback is used only when the splice cannot determine the layout
impact locally (group-boundary crossing).

### Leaf Split

On overflow, the leaf is split into two halves. Each half is
re-encoded independently with fresh restart points starting at
index 0. Boundary keys (last key of left leaf, first key of right
leaf) are full keys reconstructed from the delta encoding.
Separator computation for the parent branch uses these full keys
(see §Prefix-Truncated Branch Keys).

### Cursor Key Reconstruction

The cursor maintains a **key reconstruction buffer**
(`cursor.keyBuf []byte`) holding the full key at the current
position. On forward movement (`Next()`), truncate to `SharedLen`
and append `UnsharedKey`. `O(1)` amortized.

For reverse movement (`Prev()`), delta entries encode forward only.
The cursor caches all decoded keys for the current restart group
(**group cache**, `[K][]byte` array). When the cursor first enters a
group, all `K` entries are decoded into the cache; subsequent
`Prev()` within the group reads from cache in `O(1)`. At `K = 16`
and max key size ~2 KB, worst case ~32 KB per cursor — acceptable.

## Overflow Page

Overflow pages are contiguous runs that store large values. The
first page in the run carries the standard 8-byte page header with
`AdditionalPages` set to the number of follower pages; the remaining
bytes are value data. Follower pages carry no header — they are
entirely value data (minus 8 bytes for the xxhash64 footer when page
checksums are enabled). Total value capacity for a run of `1 + N`
pages: `(PageSize - 8) + N * PageSize` bytes (subtract 8 per page
for the footer when enabled).

When checksums are enabled, each page in the run carries its own
independent xxhash64 footer. The first page checksums header + data;
each follower checksums its data. Per-page footers allow identifying
which specific page is corrupted.

## NUL-escape encoding (composite keys)

This encoding is used wherever multiple lex-ordered columns are
concatenated into a single byte key — currently secondary indexes
(see `indexing.md`).

- Within each column's bytes, every `0x00` is escaped to `0x00 0xFF`.
- After each column's escaped bytes, append a `0x00 0x00`
  terminator.
- The full key is the concatenation of escaped columns + their
  terminators, optionally followed (for non-unique indexes) by the
  escaped primary key + a final `0x00 0x00`.

The encoding is **prefix-free**: no escaped column is a prefix of
another, and the column terminator `0x00 0x00` never appears inside
an escaped column (every internal `0x00` is followed by `0xFF`).
Concatenated columns sort lex-correctly regardless of contents,
including columns with embedded NULs.

**Worked example.** Two tuples to encode:

| Tuple | Col A | Col B | Encoded bytes |
|-------|-------|-------|---------------|
| T1 | `[]` (empty) | `[0x00]` | `00 00`  `00 FF 00 00` |
| T2 | `[0x00]` | `[]` (empty) | `00 FF 00 00`  `00 00` |
| T3 | `[0x00, 0xFF]` | `[0x00]` | `00 FF FF 00 00`  `00 FF 00 00` |

Byte-wise comparison yields `T1 < T2 < T3`, matching the lex order
of the original tuples. A decoder finds column boundaries
unambiguously by scanning for the `00 00` terminator.

A distinct separator `0x00 0x01` is used inside SetKeyspace
compound-PK encodings to separate the set key from the set value —
see `set-keyspace.md §Indexes on SetKeyspaces`.
