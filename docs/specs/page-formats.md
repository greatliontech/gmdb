# Page Formats

On-disk formats for the per-page structures stored in data pages:
branch pages (internal B+tree nodes), leaf pages (two variants —
prefix-compressed `TypeLeaf` and uncompressed `TypeLeafUncompressed`),
and overflow pages (large value storage). Set-keyspace subpages and
nested B+tree references are leaf-cell variants defined in
`set-keyspace.md`. RPL segment pages live in `free-space.md`.

Scope:
- Branch page layout, prefix-truncated separator computation.
- Compressed leaf page layout with variable-size restart groups,
  restart vs delta entries, overflow references.
- Uncompressed leaf page layout (selected when `RestartGroupTarget
  == 1`).
- Leaf lookup, insert/delete, split, and the `LeafIter`
  bidirectional iterator used by `btree.Cursor`.
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

Invariant: kind=entailed;
  property=A branch cell stores only the separator suffix after the
    page-wide prefix `P` (`PrefixLen` bytes); the full separator is
    `P || Suffix[i]`. A reader MUST account for `P` before comparing
    suffixes — compare `target` against `P` first (descend leftmost
    when `target < P`, rightmost when `target > P` without sharing all
    of `P`) and binary-search suffixes only for a `target` that starts
    with `P`. The encoding round-trips: `decode(encode(cells))`
    reconstructs every cell's full key `P || Suffix[i]` and child
    exactly;
  from=entailed: the clause-explicit separator-routing invariant above
    fixes the separator VALUES but not how a within-page-compressed
    branch stores or searches them; no single clause states that the
    page-prefix split must round-trip and that search must reconstruct
    it;
  violation=A target `t` with `t[:k] == P[:k]` diverging at some
    `k < PrefixLen` routed by a suffix-only search (one that skips the
    prefix comparison) descends the wrong child — every
    separator-routing clause still holds, yet `Get` returns
    `ErrNotFound` for a key that is present. Equivalently, a
    prefix/suffix split that drops or duplicates a boundary byte
    reconstructs a wrong full key on decode and on the split-time lift.

Invariant: kind=clause-explicit;
  property=Within a leaf restart group, the entry at the group's
    restart position stores a full key; every subsequent delta entry
    in the group encodes its key as `previous[0:SharedLen] ||
    UnsharedKey`;
  from=this spec §Leaf Page (Restart entry / Delta entry);
  violation=A delta entry whose `SharedLen` does not actually share
    `SharedLen` bytes with the previous entry's full key
    reconstructs a wrong key — cursor reads return wrong keys and
    binary search lands in the wrong group. The structurally
    checkable half — `SharedLen` never exceeds the previous entry's
    full-key length — is enforced at the read trust boundary
    (`LeafReader.Validate`): an unbounded `SharedLen` makes decode
    either panic or fabricate key bytes from adjacent page content.
    (Enforced by `TestLeafReader_Validate_TotalOverInput` SharedLen
    cases in `leaf_test.go`.)

Invariant: kind=clause-explicit;
  property=A leaf page's entry data is one contiguous stream
    starting at the entry-data start (byte offset 12) and ending
    exactly at `DataEnd`, entries in index order; every lookup-table
    entry (compressed restart-table Offset, uncompressed
    offset-table slot) equals its entry's position in that stream;
  from=this spec §Cursor Iteration — the streaming iterator and
    first-key reads decode by continuation from the entry-data
    start with the unchecked hot-path decoders and never re-consult
    the lookup tables mid-stream;
  violation=A page whose table offsets each pass a range check but
    do not match the stream (garbage bytes at offset 12 with the
    table pointing past them; a gap or overlap between restart
    groups) passes a per-table-offset walk, then a streaming read
    decodes bytes validation never examined — a slice-bounds panic
    or fabricated entries on a checksums-off page. A `DataEnd` past
    the stream end (trailing slack) instead corrupts on WRITE: the
    splice paths validate and then append the next entry at
    `DataEnd`, placing it outside the stream the readers decode.
    (Enforced by `LeafReader.Validate` exact stream-position
    matching; pinned by `TestLeafReader_Validate_TotalOverInput`
    contiguity cases and `FuzzLeafValidateTotal` — Validate-accepted
    pages must survive a full Iter walk + SearchLeaf — in
    `leaf_test.go`.)

Invariant: kind=clause-explicit;
  property=A compressed leaf's `RestartCount × 4` bytes immediately
    before the optional 8-byte xxhash64 footer constitute the
    restart table; each entry stores the group's first-entry byte
    offset (uint16), the group's entry count in `[1, 255]` (uint8),
    and a reserved byte; the restart table indexes only entries on
    this page;
  from=this spec §Leaf Page (Compressed Leaf — page layout);
  violation=Misplacing the restart table (relative to the optional
    checksum footer) corrupts the binary-search index — lookups
    diverge into delta entries treated as restart points and decode
    garbage keys. A `Count` of zero in any restart-table entry
    leaves the next group's start ambiguous (the variable-group
    format derives group ranges by summing counts), so readers
    must treat `Count == 0` as structural corruption.

Invariant: kind=clause-explicit;
  property=An uncompressed leaf's `Count × 2` bytes immediately
    before the optional 8-byte xxhash64 footer constitute the
    offset table; the table is **positional** — slot `i` holds
    the byte offset of entry `i`'s first byte (`CellFlags`).
    Entries themselves are key-sorted, so iterating the table in
    slot order yields entries in key order, but the table is
    indexed by position, not by key;
  from=this spec §Leaf Page (Uncompressed Leaf — page layout);
  violation=An offset that points outside the entry-data region
    yields out-of-bounds reads in the UC decoder; swapping slots
    `i` and `j` for `i < j` with `entries[i].Key > entries[j].Key`
    silently violates the binary-search contract.

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
  property=At fixed column count, the encoder is tuple-prefix-free:
    for any two distinct tuples `T1`, `T2` with `len(T1) == len(T2)`,
    neither `encode(T1)` is a prefix of `encode(T2)` nor vice versa.
    (Different-column-count tuples CAN prefix-collide — a 2-col
    tuple's encoding is a prefix of a 3-col extension — which is by
    design: an index has a fixed schema, so the decoder always
    processes the same column count.);
  from=entailed: the clause-explicit column-level prefix-freeness
    above + the per-column `0x00 0x00` terminator together imply
    that two same-length tuples differing at column k diverge at
    column k's escaped bytes, with no terminator-induced prefix
    confusion; no single clause states the tuple-level property
    that index range queries actually depend on;
  violation=An index range query at a fixed schema returns a
    tuple whose encoded form prefix-matches a shorter-encoded
    same-arity tuple — the cursor mis-classifies adjacency,
    yielding wrong matches for `Range(start, end)` and false
    duplicates for unique-index probes. (Spec amendment;
    enforced by `TestEncodedTuplePrefixFreenessSameNColsProperty`
    in `index_key_codec_test.go`.)

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

Branch pages store separator keys and child page pointers. They do
NOT store values. Separators are **prefix-truncated within the page**:
the single common prefix `P` of all separators on the page is stored
once, and each cell stores only the separator's suffix after `P` (see
§Prefix-Truncated Branch Keys for why this is effective and how it
composes with the cross-level truncation).

```
Branch Page
+-----------------------+ offset 0
| Page Header (8 bytes) | Type=TypeBranch, Count=N
+-----------------------+ offset 8
| Ptr[0] (uint64)       |  leftmost child pointer (8 bytes)
+-----------------------+ offset 16
| PrefixLen (uint16)    |  length of the page-wide shared prefix P
| Reserved  (uint16)    |  zero on write (keeps the directory at offset 20)
+-----------------------+ offset 20
| Cell Directory        |  Array of (Offset uint16, SuffixLen uint16)
| ...                   |  grows forward, 4 bytes per cell
+-----------------------+
|       free space      |
+-----------------------+
| ...                   |
| Cell Data 1           |  each cell: SuffixBytes || ChildPtr(uint64),
| Cell Data 0           |  packed below the prefix region, growing backward
+-----------------------+ ContentEnd - PrefixLen
| Prefix bytes P        |  the page-wide shared prefix, PrefixLen bytes
+-----------------------+ ContentEnd (PageSize - optional 8-byte footer)
```

Each cell in the data area stores the separator **suffix** (the bytes
after the page-wide prefix `P`) followed by its right child pointer:

```
Branch Cell
+--------------+----------+
| Suffix bytes | ChildPtr |
| key[len(P):] | uint64   |
+--------------+----------+
```

The full separator key of cell `i` is `P || Suffix[i]`. Because cells
are stored in sorted key order, `P = commonPrefix(cell[0], cell[N-1])`
— the common prefix of the whole sorted set. When the separators share
no common prefix (e.g. a branch spanning a cluster boundary)
`PrefixLen` is 0 and each cell stores its full key, identical to the
uncompressed case with no size penalty beyond the fixed header fields.

Keys are stored in sorted order. For a branch with N cells (N keys)
there are N+1 child pointers: `Ptr[0]` (leftmost, stored after the
page header) plus one `ChildPtr` per cell.

**Search algorithm.** Let `m = PrefixLen` and `P` the prefix bytes. To
find the child for `target`:

1. If `len(target) >= m` and `target[:m] == P`: binary-search
   `target[m:]` against the cells' suffixes for the first suffix
   strictly greater than `target[m:]`. That index `i` is the descent
   index.
2. Otherwise `target` does not start with `P`: if `target < P` every
   separator exceeds it → descend leftmost (`i == 0`); if `target > P`
   every separator is below it → descend rightmost (`i == N`).

The descent index `i` selects the child exactly as in the uncompressed
case: `i == 0` → `Ptr[0]`; `0 < i <= N` → `ChildPtr` of cell `i-1`.
When `target` equals a separator key, the suffix binary search returns
the index past it, so the target descends into that separator's right
child (separators are right-child lower bounds). Comparing `target`
against `P` once per page — rather than against each full key at every
binary-search probe — also makes the descent compare fewer bytes than
an uncompressed branch would.

The cell directory stores `(Offset, SuffixLen)` per cell, enabling
binary search over the variable-length suffixes without parsing the
cell data area; `Offset` points at the suffix's first byte and the
child pointer follows the `SuffixLen` suffix bytes.

The encoding is a pure function of `(cfg, leftmost, cells)` — `P`,
the suffixes, the directory, and the cell packing order all follow
deterministically — so a `Check()` re-encode is byte-identical to the
original writer's output (the §Leaf Split deterministic-encoding
invariant, for branch pages).

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

**Two complementary compressions on branch pages.** Branch separators
are compressed *across* tree levels (the shortest distinguishing
prefix, this section) **and** *within* a page (page-level prefix
truncation, §Branch Page — the one common prefix of a page's
separators stored once + per-cell suffixes). Leaf pages independently
compress redundancy within a page via restart-group delta encoding
(§Compressed Leaf). Cross-level and within-page truncation compose: a
leaf-adjacent branch whose separators all share a long cluster prefix
stores that prefix once, so its fan-out stays high even when each
separator approaches the §Maximum Key Size bound — the case that,
without within-page truncation, collapses fan-out toward 2 (a branch
holding only ~2 near-max separators) and builds trees born below the
`range-delete.md §Invariants` fill-floor.

**Interaction with maximum key size**: the maximum-key-size limit
applies to full keys stored in leaves (reconstructed from delta
encoding). Branch separators are always shorter than or equal to the
full keys. Within-page prefix truncation does not relax the limit:
the worst case is two separators sharing no prefix (`PrefixLen == 0`),
each stored in full, so a branch must still hold two full-size
separators to split (see `limits.md §Maximum Key Size`).

**Benefits**: high fan-out → shallow trees → fewer page accesses per
lookup, *including* for keys that share deep prefixes (within-page
truncation stores the shared bytes once instead of once per
separator); less data read per branch traversal, and a single prefix
comparison per page instead of a full-key comparison at every
binary-search probe.

## Leaf Page

gmdb supports two leaf page variants chosen per-page at build time
by the keyspace's `RestartGroupTarget`:

- **`TypeLeaf` (prefix-compressed, default).** Keys that share common
  prefixes with their neighbours are stored as deltas grouped into
  **variable-size restart groups**; two-phase lookup (binary search
  over the restart table + linear scan within the matched group).
  Selected when `RestartGroupTarget ≥ 2`.
- **`TypeLeafUncompressed`.** Every key stored in full; lookup is a
  single O(log N) binary search via an offset table. Selected when
  `RestartGroupTarget == 1`.

Both variants share the 8-byte common page header and the
"entries-forward, lookup-table-backward" layout; only the per-entry
encoding and lookup machinery differ. The two type bytes let
`Check()` and the readers dispatch without probing further.

Each entry carries a `CellFlags` byte (same definitions across both
variants):

```
CellFlags bit layout
Bit 0:    Overflow    (0 = inline value, 1 = overflow reference)
Bit 1:    MultiValue  (0 = single value, 1 = multi-value data — subpage or nested B+tree)
Bit 2:    NestedTree  (only when Bit 1 set: 0 = subpage, 1 = nested B+tree)
Bits 3-7: Reserved (must be 0)
```

`Overflow` and `MultiValue` are mutually exclusive in practice.

`ValueLen` is `uint32` (max ~4 GB for inline values; bounded in
practice by leaf-page free space). Values exceeding leaf-page
capacity are stored as overflow pages, referenced via the formats
below which use `uint64 TotalLen`.

### Compressed Leaf (`TypeLeaf`)

```
Compressed Leaf Page
+-----------------------+ offset 0
| Page Header (8 bytes) | Type=TypeLeaf, Count=N
+-----------------------+ offset 8
| RestartCount uint16   |  number of restart groups
| DataEnd      uint16   |  byte offset after the last entry's bytes
+-----------------------+ offset 12
| Entry 0 (restart)     |  entries in forward order starting at offset 12
| Entry 1 (delta)       |
| ...                   |
| Entry N-1             |
+-----------------------+ DataEnd
|       free space      |
+-----------------------+
| Restart Table         |  RestartCount × 4 bytes, packed at content end
+-----------------------+ ContentEnd (PageSize - optional 8-byte footer)
```

`RestartCount` and `DataEnd` are little-endian `uint16` (per the
project-wide byte-order rule in `overview.md`). `DataEnd` is the
byte offset where entry data ends; `[DataEnd, ContentEnd -
RestartCount × 4)` is the free-space region used by the in-place
insert / delete splice helpers (`tryAppendCompressed`,
`tryInsertAtCompressed`, `tryDeleteAtCompressed`). The restart
table is located at `ContentEnd - RestartCount × 4`.

**Restart table entry** (4 bytes per group):

```
+----------+--------+-----------+
| Offset   | Count  | Reserved  |
| uint16   | uint8  | uint8     |
+----------+--------+-----------+
```

- `Offset`: byte offset within the page of the group's first
  (restart) entry. Little-endian.
- `Count`: number of entries in this group, in `[1, 255]`. The
  `uint8` width is the hard physical cap; `RestartGroupTarget` is
  bounded to `[1, 255]` correspondingly (see `keyspaces.md
  §Keyspace Descriptor` and `api-surface.md` Options). Groups end
  either at `RestartGroupTarget` entries (the cap), or earlier when
  the builder's split heuristic chooses a natural break — e.g.,
  a key that shares no prefix with its predecessor is a poor
  candidate for delta encoding, so the builder may start a fresh
  group at it rather than spend the 2-byte delta-header overhead
  on negative savings. There is no minimum group size other than 1
  (a single-entry group is the degenerate-but-valid case for keys
  with no prefix sharing); for workloads with systematically zero
  prefix sharing the keyspace should select `RestartGroupTarget =
  1` for the uncompressed variant rather than relying on the
  builder to detect the pattern.
- `Reserved`: zero on write; reserved-byte read policy is the
  project-wide rule in `file-layout.md §Reserved-byte policy
  (project-wide)` — per-page padding bytes are ignored on read
  and kept available for future format extensions.

The variable-group design replaces the prior uniform-K mapping
(`entryIndex / K` derived the group) with explicit per-group counts
read directly from the table. This decouples per-page group structure
from the keyspace's `RestartGroupTarget`: the target is a *builder
hint*, not a per-page invariant — old pages keep their group
structure across `RestartGroupTarget` config changes; new pages use
the new target.

#### Restart entry (full key, first entry of each group)

```
Restart Entry (inline)
+-----------+----------+----------+-----------+-----------+
| CellFlags | KeyLen   | ValueLen | Key bytes | Val bytes |
| uint8     | uint16   | uint32   |           |           |
+-----------+----------+----------+-----------+-----------+
```

#### Delta entry (subsequent entries within a group)

```
Delta Entry (inline)
+-----------+-----------+-------------+----------+---------------+-----------+
| CellFlags | SharedLen | UnsharedLen | ValueLen | UnsharedKey   | Val bytes |
| uint8     | uint16    | uint16      | uint32   |               |           |
+-----------+-----------+-------------+----------+---------------+-----------+
```

`SharedLen` = leading bytes shared with the previous entry in the
same restart group. `UnsharedKey` contains only the bytes after the
shared prefix. Full key = `prevEntry.Key[:SharedLen] || UnsharedKey`.

Per-entry overhead comparison: the restart-entry header is `1 + 2 +
4 = 7` bytes (`CellFlags + KeyLen + ValueLen`); the delta-entry
header is `1 + 2 + 2 + 4 = 9` bytes (`CellFlags + SharedLen +
UnsharedLen + ValueLen`). The delta header costs 2 extra bytes
beyond the restart header, and the delta avoids `SharedLen` bytes
of key data. Net saving per delta entry is `SharedLen - 2` bytes —
positive whenever keys share more than a 2-byte prefix.

**Field ordering — `ValueLen` precedes the key (decode-speed
rationale).** Every fixed-length length field (`KeyLen`, `ValueLen`,
and a delta's `SharedLen` / `UnsharedLen`) sits in the entry header
*before* the variable-length key and value, so the decoder computes
the next entry's start offset from the header alone (`KeyLen +
ValueLen`) without waiting on the key copy — the offset arithmetic
overlaps the copy (instruction-level parallelism). A microbenchmark
isolating this single variable (`internal/page/layout_bench_test.go`)
measures ~24% faster full-leaf decode versus placing `ValueLen`
after the key, for a wash on search; that decode cost is paid by
every non-spliced read and the splice fallback, so the order is
load-bearing, not incidental. (Overflow / nested-tree forms instead
carry a fixed 16-byte trailer after the key — `KeyLen` in the header
still makes the next offset header-computable.)

#### Overflow reference at a restart point (CellFlags bit 0 set)

```
Restart Overflow Reference
+-----------+----------+-----------+----------+----------+
| CellFlags | KeyLen   | Key bytes | OvflPage | TotalLen |
| uint8     | uint16   |           | uint64   | uint64   |
+-----------+----------+-----------+----------+----------+
```

#### Overflow reference at a delta position

```
Delta Overflow Reference
+-----------+-----------+-------------+---------------+----------+----------+
| CellFlags | SharedLen | UnsharedLen | UnsharedKey   | OvflPage | TotalLen |
| uint8     | uint16    | uint16      |               | uint64   | uint64   |
+-----------+-----------+-------------+---------------+----------+----------+
```

### Uncompressed Leaf (`TypeLeafUncompressed`)

```
Uncompressed Leaf Page
+-----------------------+ offset 0
| Page Header (8 bytes) | Type=TypeLeafUncompressed, Count=N
+-----------------------+ offset 8
| DataEnd      uint16   |  byte offset after the last entry's bytes
| Reserved     uint16   |  zero on write
+-----------------------+ offset 12
| Entry 0               |  entries in forward order starting at offset 12
| Entry 1               |
| ...                   |
+-----------------------+ DataEnd
|       free space      |
+-----------------------+
| Offset Table          |  N × 2 bytes, packed at content end
+-----------------------+ ContentEnd
```

The 2-byte `Reserved` at offset 10 keeps `Entry 0`'s offset at 12,
identical to the compressed variant. Readers and the `Check()` walk
don't need a per-variant entry-start offset. Reserved-byte read
policy is the project-wide rule in `file-layout.md §Reserved-byte
policy (project-wide)`: per-page padding bytes are zero on write
and ignored on read.

Every entry is a full key with no shared / unshared distinction:

```
Entry (inline)
+-----------+----------+----------+-----------+-----------+
| CellFlags | KeyLen   | ValueLen | Key bytes | Val bytes |
| uint8     | uint16   | uint32   |           |           |
+-----------+----------+----------+-----------+-----------+
```

(Overflow form: `ValueLen` replaced by `OvflPage uint64 + TotalLen
uint64`, same as the compressed restart-entry overflow form.)

Lookup is a single O(log N) binary search via the offset table. The
uncompressed variant motivation is **operational simplicity**, not
density: per-entry overhead in the uncompressed variant
(`CellFlags + KeyLen + ValueLen + 2-byte offset-table slot = 9`
bytes) equals the compressed delta-entry overhead at zero shared
prefix (`CellFlags + SharedLen + UnsharedLen + ValueLen = 9` bytes,
no per-entry offset). At `SharedLen == 0` the two formats consume
identical per-entry bytes; the only delta is the compressed
variant's 4-byte-per-group restart-table cost (~0.25 bytes/entry at
`RestartGroupTarget = 16`), which is negligible.

The uncompressed variant is the right choice when:

- Lookup determinism matters: single O(log N) binary search vs the
  compressed `O(log G + K)` two-phase walk.
- `Prev` cost matters: O(1) via offset table vs the compressed
  `LeafIter` buffered-mode group reload.
- Decode simplicity matters: no `LeafIter` machinery; `Check()` /
  recovery walks per-entry without delta-decode bookkeeping.

For keys with systematic prefix sharing (file paths, composite
indexes, ordered IDs), the compressed variant wins on density — see
§Leaf Density below. For keys that don't share prefixes, the two
variants are density-equivalent and the uncompressed variant is
operationally simpler.

### Leaf Lookup

Both variants expose the same `SearchLeaf(target) → (index, entry,
found)` and `SearchLeafIter(target, …) → (index, entry, found, iter)`
contracts; the variant-specific machinery is encapsulated.

- **Compressed**: phase 1 binary search over restart-table offsets,
  `O(log G)` where G = `RestartCount`. Each probe decodes one
  restart entry's full key (no delta state needed). Phase 2 linear
  scan within the matched group, `O(K)` where K is the matched
  group's `Count` (≤ `RestartGroupTarget`). Total `O(log(N/K) + K)`.
- **Uncompressed**: single O(log N) binary search over the offset
  table.

`SearchLeafIter` is the cursor-friendly form: returns the lookup
result plus a `LeafIter` whose **next** `Next()` call returns the
entry immediately *after* the returned (found-or-successor) entry —
i.e. the iterator is positioned past the result, ready to stream
forward without re-emitting the entry the caller just received. It
carries the delta-decode state accumulated during the scan, so
Cursor `Seek` / `SeekGE` avoid a second group walk.

### Cursor Iteration

The cursor delegates leaf traversal to a `LeafIter` exposed by the
page package. `LeafIter` is a bidirectional iterator that owns the
decode state for the current leaf; the cursor stack stays slim, and
the per-leaf-format machinery (compressed vs uncompressed) stays
encapsulated. `LeafIter` operates in two modes:

- **Forward-streaming mode** (initial state for compressed leaves).
  Maintains a `keyBuf []byte` carrying the current full key. `Next()`
  reads the next delta entry's `SharedLen` and `UnsharedKey` directly
  from the page, truncates `keyBuf` to `SharedLen`, and appends
  `UnsharedKey` in place — amortized O(1) without per-step
  allocation. Crossing into a new restart group reads the group's
  first (full) restart key directly from the page.
- **Buffered mode** (entered on the first `Prev()` or `At()` call
  on a compressed leaf). Decodes the current restart group into
  `bufEnts []LeafEntry + bufKeys []byte`. All subsequent
  `Next`/`Prev`/`At` calls serve from the buffer; group-boundary
  crossings reload the adjacent group. The buffer storage
  (`bufEnts`, `bufKeys`, and the `keyBuf`) is passed in to
  `IterAtForReuse` / `IterForReuse` and returned via `KeyBuf` /
  `BufKeys` / `BufEnts` so the cursor reclaims it across leaf
  transitions — per-cursor allocation amortizes to zero in the
  steady-state cursor loop.

Uncompressed leaves don't need the streaming/buffered distinction:
`Next`/`Prev`/`At` are all O(1) via the offset table.

This unifies what the original spec called the "key reconstruction
buffer" and the "group cache" behind a single iterator interface,
so all leaf-walking callers — `btree.Cursor`, `btree.Get` (via
`SearchLeaf`), and the range-delete scanner — share decode
infrastructure.

### Leaf Density

Depends on the ratio of shared-prefix length to total key length.
For 200-byte keys sharing a 150-byte common prefix + 50-byte values
at 4 KB (`ContentEnd = 4088` with checksums):

| Format | Bytes/entry (avg) | Entries/page | Improvement |
|--------|-------------------|-------------|-------------|
| Uncompressed (`TypeLeafUncompressed`) | ~259 | ~15 | baseline |
| Compressed (`TypeLeaf`, target K=16) | ~118 | ~34 | 2.2× |

For keys with **no** shared prefix (50-byte random keys + 50-byte
values), the two variants are essentially identical: compressed
~109.25 bytes/entry (≈37 entries/page) vs uncompressed ~109
bytes/entry (≈37 entries/page). The ~0.25 byte delta per entry is
the compressed variant's per-group restart-table overhead
(`4 / RestartGroupTarget` bytes amortized) — not material.

So `RestartGroupTarget = 1` (uncompressed) is **not** chosen for
density on random-key workloads — both formats are
density-equivalent there — but for the operational properties listed
in §Uncompressed Leaf above (single-phase O(log N) lookup, trivial
`Prev`, simpler `Check()` walk).

### Insert and Delete

Insert and delete within a leaf splice the page in place when the
resulting layout fits — `tryAppendCompressed`,
`tryInsertAtCompressed`, `tryDeleteAtCompressed` for the compressed
variant; `ucTryAppend`, `ucTryInsertAt`, `ucTryDeleteAt` for the
uncompressed. Inserting between two compressed delta entries
re-encodes the successor entry's delta against the new predecessor
and may shift the containing group's boundaries; the splice helpers
return false when the layout impact crosses a boundary in a way the
local rewrite can't predict, at which point the caller falls back to
the full decode-and-rebuild path (the `TryInsertAt` / `TryDeleteAt`
dispatchers' callers in `internal/btree`).

The restart table is rebuilt only when group composition changes —
not on every insert / delete. Uncompressed leaves rebuild only the
offset table; the per-entry data shifts but no key re-encoding
occurs.

**Delete-side rebuild fallback.** A delete's keep-set is NOT
removal-monotone under the canonical builder: re-packing the
survivors re-aligns restart-group boundaries (each shifted boundary
stores a full key where a delta sufficed), and the rebuild's variant
migration (a page whose on-disk variant differs from the configured
`RestartGroupTarget`) can inflate a delta-heavy page by far more
than one page. When the canonical decode-and-rebuild of a delete's
keep-set does not fit one page, the delete falls back to
native-variant splices of the original page bytes
(`TryDeleteAtNative`): a splice delete always shrinks (the
compressed splice's shared-prefix triangle-inequality bound; the
uncompressed sorted-array delete), so removing entries from a
fitting page in its own variant always fits. The page keeps its
on-disk variant and group structure — variant migration on the
delete path is opportunistic, never load-bearing. Consequence, and
the invariant the fallback restores: **the leaf keep-set rebuild
never fails for encoding reasons and never splits the leaf**. The
claim is deliberately scoped to the rebuild step: a delete may
still grow a page when a fits-but-larger variant migration is
taken, and a delete's merge/redistribute/root-collapse machinery
changes tree shape by design (its own encoding-infeasibility
handling is governed by `range-delete.md` §Invariants, not this
clause).

### Leaf Split

On overflow, the leaf is split into two halves at a *group boundary*
(compressed) or *entry boundary* (uncompressed) chosen by split
bias — typically 50% of data bytes. The `FindSplitGroup` /
`FindSplitIndex` helpers walk the restart / offset table for the
boundary closest to the bias target. **Tiebreak**: when two adjacent
boundaries are equidistant from the bias target, the lower-index
boundary wins. Each half is then encoded independently with fresh
group structure (compressed) or fresh offset table (uncompressed).

Boundary keys (last key of left leaf, first key of right leaf) are
reconstructed from the source page (full decode for the boundary
positions only — not the whole leaf). Separator computation for the
parent branch uses these full keys (see §Prefix-Truncated Branch
Keys).

**Deterministic encoding invariant** (consequence of the tiebreak +
the per-page group-target policy): the **same encoder version**
given the same input sequence, `RestartGroupTarget`, `PageSize`,
and `PageChecksum` configuration must produce byte-identical pages.
This is the property `Check()` repair, recovery testing, and any
future cross-process determinism tooling rely on; any encoder
change that breaks within-version determinism is a format break in
the same sense as a layout change. The spec deliberately does *not*
mandate byte-identical output across encoder versions: the §Compressed
Leaf "natural break" heuristic and the "typically 50%" split bias
are policy knobs the encoder may tune over time; what's pinned is
that any single deployed encoder produces the same bytes for the
same input, so a `Check()` repair re-encoding a page yields the
same bytes the original writer would have written.

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
- The full key shape is statically determined by `IndexDecl.Unique`
  (per `indexing.md §Storage Layout`):
  - **Unique indexes:** the key is `(escapedCol 0x00 0x00)+` — just
    the concatenated escaped column tuple with terminators.
  - **Non-unique indexes:** the key is extended with `escapedPK
    0x00 0x00` — the escaped primary key plus a final terminator.
    For SetKeyspace indexes the PK is the compound `escape(setKey)
    || 0x00 0x01 || escape(setValue)` per `set-keyspace.md §Indexes
    on SetKeyspaces`.

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
