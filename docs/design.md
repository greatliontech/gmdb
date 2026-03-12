# gmdb Design Document

A memory-mapped, multi-process, embedded key-value database for Go.

## Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Data structure | B+tree on fixed-size pages | Only viable option for multi-process mmap |
| Concurrency | Single writer + N readers (MVCC/CoW) | Proven (LMDB), readers never block writer |
| File layout | Fixed-size pages (4KB–64KB, configurable, immutable after creation) | Matches OS page size, mmap-friendly |
| Value storage | Inline + overflow pages | Simple single read path, overflow for large values |
| Duplicate values | DUPSORT with subpage + nested B+tree | Subpage for small sets, nested B+tree for large; DUPFIXED for fixed-size optimization |
| Free space | Allocation bitmap + retired page log (RPL) | O(1) alloc via bitmap, no self-referential allocation, RPL tracks MVCC retirement |
| File geometry | Dynamic grow/shrink with configurable bounds | Auto-compaction via tail refund, no manual compaction needed |
| Isolation | Dual meta pages + CoW | No WAL needed, atomic commit |
| Crash safety | CoW + atomic meta page swap | File is always consistent |
| Durability | Four sync modes (Durable, NoMeta, Safe, None) | Configurable ACID vs. performance tradeoff |
| Cross-process | Shared memory lock file | Fixed-size reader table (scan+CAS), stale writer/reader recovery via PID liveness |
| Write lock | Channel semaphore (intra-process) + flock (cross-process) | Context-aware blocking; flock alone doesn't block same-process goroutines |
| Slow readers | Callback-based notification | Application controls policy; no silent unbounded growth |
| mmap | pwrite mode (default) or writemap mode | pwrite: heap isolation; writemap: direct mmap writes for performance |
| Dirty page tracking | Hash map (`map[uint64]`) | O(1) insert/lookup/delete; sort once at commit for sequential I/O |
| Page spilling | LRU-based spill to disk mid-transaction (pwrite mode only) | Bounds memory usage for large write transactions |
| Key ordering | Lexicographic (byte-ordered) | Simple, general, no custom comparator needed |
| Byte order | Little-endian (fixed) | Portable across architectures |
| Checksums | Meta pages only | CoW protects data pages; meta checksum detects torn commits |
| API | Transaction-based with `context.Context` | Explicit read/write txns; context governs lock acquisition, not txn lifetime |
| Write batching | Channel-based `Batch()` API | Amortizes commit cost (fdatasync) across concurrent callers; rollback+retry on failure |
| Leak detection | `runtime.AddCleanup` on `Tx` | Detects leaked transactions, releases reader slots, logs origin stack trace |
| Namespaces | Named keyspaces | Multiple B+trees in one file |

## File Layout

The database is a single file, divided into fixed-size pages. All pages are the
same size (configurable at creation time, immutable after). Supported page sizes
are powers of 2 from 4KB to 64KB. Default: 4096 bytes (OS page size).

All multi-byte integers are stored in little-endian byte order.

```
+--------+--------+------------------+--------+--------+----
| Meta 0 | Meta 1 | Bitmap Pages ... | Data pages ...       |
| Page 0 | Page 1 | Page 2 .. N      | Page N+1, N+2, ...   |
+--------+--------+------------------+--------+--------+----
```

Bitmap pages occupy a contiguous region starting at page 2. The number of
bitmap pages is determined by `GeoUpper` at database creation time:
`BitmapPages = ceil((GeoUpper / PageSize) / (PageSize * 8))`. Data pages
(B+tree nodes, overflow pages, RPL segment pages) begin immediately after
the bitmap region. See Allocation Bitmap for details.

### Page Types

Every page starts with a common header:

```
Page Header (16 bytes)
+----------+----------+----------+----------+
| PageID   | Type     | Count    | Overflow |
| uint64   | uint16   | uint16   | uint32   |
+----------+----------+----------+----------+
```

- **PageID**: The page number (offset = PageID * PageSize).
- **Type**: One of: Meta, Branch, Leaf, Overflow, RPLSegment. Bitmap pages
  do not carry a page header (see Allocation Bitmap). RPLSegment pages are
  the retired page log (see Free Space Management).
- **Count**: Number of items (keys in branch, key/value pairs in leaf, entries
  in RPL segment).
- **Overflow**: Number of contiguous overflow pages following this one (0 for
  single-page nodes).

#### Meta Page

Two meta pages exist at page 0 and page 1. They alternate — the writer always
updates the one NOT currently active. Each meta page contains:

```
Meta Page
+------------------+
| Page Header      |
+------------------+
| Magic            | uint32 - identifies file as gmdb
| Version          | uint32 - format version
| PageSize         | uint32 - page size in bytes
| Flags            | uint32 - reserved
| GeoLower         | uint64 - minimum database size in pages
| GeoUpper         | uint64 - maximum database size in pages
| GeoGrowPages     | uint64 - growth step in pages
| GeoShrinkPages   | uint64 - shrink threshold in pages
| FirstUnallocated | uint64 - first unallocated page ID (high-water mark)
| BitmapPages      | uint32 - number of pages in the allocation bitmap
| RPLHeadPage      | uint64 - page ID of the newest RPL segment (0 = empty)
| RPLTailPage      | uint64 - page ID of the oldest RPL segment (0 = empty)
| RPLEntryCount    | uint64 - total entries across all RPL segments
| NumFreePages     | uint64 - total free pages (set bits in bitmap)
| KeyspaceRoot     | uint64 - root page of keyspace B+tree
| NumKeyspaces     | uint64 - number of keyspaces
| TxnID            | uint64 - transaction ID that wrote this meta
| Checksum         | uint64 - xxhash of all preceding bytes (header through TxnID)
+------------------+
```

Total meta page payload: 16 (header) + 4×4 (Magic, Version, PageSize, Flags) +
4 (BitmapPages) + 4 (padding) + 13×8 (uint64 fields including Checksum) =
144 bytes. Fits comfortably in any supported page size (min 4KB).

The geometry fields (`GeoLower`, `GeoUpper`, `GeoGrowPages`,
`GeoShrinkPages`) are stored in the meta page so that they persist across
opens and are available to all processes (see Database Geometry).

The active meta page is the one with the highest TxnID whose checksum is valid.
If a crash happens mid-write to the meta page, the checksum will be invalid and
the database falls back to the other meta page — which points to the previous
consistent state.

#### Branch Page (Internal B+tree Node)

Branch pages store keys and child page pointers. They do NOT store values.

```
Branch Page
+------------------------+
| Page Header (16 bytes) |
+------------------------+
| Ptr[0] (uint64)        |  leftmost child pointer (8 bytes)
+------------------------+
| Cell Directory         |  Array of (Offset uint16, KeyLen uint16)
| ...                    |  grows forward, 4 bytes per cell
+------------------------+
|       free space       |
+------------------------+
| ...                    |
| Cell Data 1            |  packed from end of page, grows backward
| Cell Data 0            |
+------------------------+
```

Each cell in the data area:

```
Branch Cell
+----------+----------+
| Key bytes| ChildPtr |
|          | uint64   |
+----------+----------+
```

Keys are stored in sorted order. For a branch with N cells (N keys), there are
N+1 child pointers: `Ptr[0]` (leftmost, stored after the page header) plus one
`ChildPtr` per cell.

Search algorithm: binary search the cell directory to find the first cell where
`target < Key[i]`. If found, descend to the child pointer of cell `i-1` (or
`Ptr[0]` if `i == 0`). If target >= all keys, descend to the last cell's
`ChildPtr`.

The cell directory stores `(Offset, KeyLen)` per cell, enabling binary search
over variable-length keys without parsing the key data area.

#### Leaf Page

Leaf pages store the actual key-value pairs. Note: the cell directory entry
format differs from branch pages — leaf cells use `(Offset, CellFlags)` instead
of `(Offset, KeyLen)`, because leaf cells encode `KeyLen` inside the cell data
itself and need the flags to distinguish cell formats (inline value, overflow
reference, DUPSORT subpage, or nested B+tree reference).

```
Leaf Page
+------------------+
| Page Header      |
+------------------+
| Cell Directory   | Array of (Offset uint16, CellFlags uint16)
| ...              |
+------------------+
|     free space   |
+------------------+
| ...              |
| KV Data N        | packed from end of page
| KV Data 1        |
| KV Data 0        |
+------------------+
```

Each cell in the data area:

```
KV Cell (inline)
+----------+----------+-----------+-----------+
| KeyLen   | ValueLen | Key bytes | Val bytes |
| uint16   | uint32   |           |           |
+----------+----------+-----------+-----------+
```

`ValueLen` is uint32 (max ~4GB for inline values). In practice, inline values
are limited by leaf page free space — far below 4GB. Values that exceed leaf
page capacity are stored as overflow pages, referenced via the overflow format
below which uses uint64 `TotalLen` for unbounded value sizes.

If a value is too large to fit in the leaf page, the CellFlags field in the cell
directory indicates it's an overflow reference.

CellFlags bit layout:

```
Bit 0:    Overflow (0 = inline value, 1 = overflow reference)
Bit 1:    DupData (0 = single value, 1 = duplicate data — subpage or nested B+tree)
Bit 2:    DupTree (only when Bit 1 is set: 0 = subpage, 1 = nested B+tree)
Bit 3:    Compressed (reserved, 0 for now)
Bit 4:    Encrypted (reserved, 0 for now)
Bits 5-7: Compression algorithm ID (reserved, 0 for now)
Bits 8-15: Reserved (must be 0)
```

Note: `Overflow` (bit 0) and `DupData` (bit 1) are mutually exclusive in
practice — a cell is either a single inline value, an overflow reference, or
a duplicate data container, never a combination.

Overflow reference format (used when CellFlags bit 0 is set):

```
Overflow Reference (instead of inline value)
+----------+-----------+----------+----------+
| KeyLen   | Key bytes | OvflPage | TotalLen |
| uint16   |           | uint64   | uint64   |
+----------+-----------+----------+----------+
```

The overflow cell has a different layout from the inline cell — there is no
`ValueLen` field. The reader checks `CellFlags.Overflow` to determine which
format to parse: inline (KeyLen + ValueLen + Key + Value) or overflow
(KeyLen + Key + OvflPage + TotalLen).

#### Overflow Page

Overflow pages are contiguous runs of pages that store large values. The first
page in the run has the standard page header with `Overflow` set to the number
of additional pages. The rest is raw value bytes.

#### Duplicate Sorted Values (DUPSORT)

Keyspaces opened with the `DupSort` flag allow multiple sorted values per key.
Each key maps to a sorted set of values (duplicates). This is the primary
mechanism for secondary indexes (e.g., an index key mapping to a sorted set of
primary key IDs).

##### Storage Strategy

DUPSORT uses two storage strategies based on the size of the duplicate set:

**Subpage (small duplicate sets):** When a key's duplicate values fit within
the leaf cell, they are stored inline as a **subpage** — a mini sorted list
embedded directly in the cell's value area. No extra page allocation is needed.

**Nested B+tree (large duplicate sets):** When duplicates grow too large for a
subpage, they are promoted to a full B+tree whose root page ID is stored in
the leaf cell. Each value in the duplicate set becomes a key in the nested
B+tree (with empty values).

##### Subpage Format

A subpage is stored in the leaf cell's value area. The `CellFlags.DupData` bit
is set and `CellFlags.DupTree` is clear. The subpage layout:

```
DupSort Subpage Cell
+----------+-----------+-----------+
| KeyLen   | Key bytes | Subpage   |
| uint16   |           |           |
+----------+-----------+-----------+

Subpage (embedded in cell value area):
+----------+----------+---------+---------+-----+
| Count    | DataSize | Entry 0 | Entry 1 | ... |
| uint16   | uint16   |         |         |     |
+----------+----------+---------+---------+-----+
```

For **variable-size values** (standard DUPSORT):
```
Entry (variable):
+----------+-----------+
| ValueLen | Val bytes |
| uint16   |           |
+----------+-----------+
```

For **fixed-size values** (DUPFIXED):
```
Entry (fixed):
+-----------+
| Val bytes |  (size = keyspace's fixed value size, no length prefix)
+-----------+
```

`Count` is the number of entries. `DataSize` is the total byte size of all
entries (used to quickly compute the subpage's total size for cell allocation).

Values within the subpage are stored in sorted (lexicographic) order. Lookup
is binary search. For DUPFIXED subpages, entries are a flat array — binary
search is O(log N) with direct offset calculation (no scanning).

##### Subpage Promotion Threshold

A subpage is promoted to a nested B+tree when inserting a new duplicate value
would cause the subpage to exceed **50% of the leaf page's usable space**
(PageSize minus page header and cell directory overhead). This threshold
ensures:
- The leaf page can still hold other keys alongside the promoted cell.
- Promotion happens before the subpage dominates the leaf page.

Promotion:
1. Allocate a new leaf page for the nested B+tree.
2. Copy all subpage entries into the new leaf page as regular key-value cells
   (where "keys" are the duplicate values and "values" are empty).
3. Replace the subpage cell with a nested B+tree reference cell.
4. Insert the new value into the nested B+tree.

##### Nested B+tree Reference Cell

When a key's duplicates are stored in a nested B+tree, the leaf cell has
`CellFlags.DupData` and `CellFlags.DupTree` both set:

```
DupSort Nested B+tree Cell
+----------+-----------+----------+----------+----------+
| KeyLen   | Key bytes | Root     | Count    | Depth    |
| uint16   |           | uint64   | uint64   | uint16   |
+----------+-----------+----------+----------+----------+
```

- **Root**: Page ID of the nested B+tree's root page.
- **Count**: Number of duplicate values.
- **Depth**: Height of the nested B+tree (for optimization).

The nested B+tree uses the same B+tree implementation as the main keyspace,
with one difference: its "keys" are the duplicate values, and all "values" are
empty (zero-length). The nested B+tree's pages are subject to normal CoW,
free space management, and page allocation.

##### Demotion

When deletions reduce a nested B+tree to a single leaf page that would fit as
a subpage (below the promotion threshold), the B+tree is demoted back to a
subpage. The leaf page is freed (retired), and the entries are packed
inline into the parent leaf cell.

##### DUPFIXED Keyspaces

When a DUPSORT keyspace is also opened with the `DupFixed` flag, all duplicate
values must be the same fixed byte size (set at keyspace creation). This
enables:
- **No per-value length prefix** in subpages — entries are a flat array.
- **Direct offset binary search** — `entry[i]` is at offset `i * valueSize`.
- **Compact nested B+tree leaves** — no `ValueLen` field per cell.

The fixed value size is stored in the keyspace descriptor (see Keyspaces).
A `Put()` call with a value of the wrong size returns an error.

### Free Space Management

Free space is managed by two on-disk structures that separate the two
concerns: **what is free** (the allocation bitmap) and **when it became free**
(the retired page log). This separation eliminates the self-referential
allocation problem found in LMDB/libmdbx's freelist B+tree, where modifying
the freelist during commit could itself allocate or free pages, requiring
complex convergence loops.

#### Allocation Bitmap

The allocation bitmap is a flat bitfield — one bit per page in the database.
A set bit means the page is **free and safe to allocate**. A clear bit means
the page is either in use or retired but not yet reclaimable (still visible
to an active reader's snapshot).

The bitmap occupies a contiguous region of pages starting at page 2 (after the
two meta pages). The number of bitmap pages is fixed at database creation time
based on `GeoUpper`:

```
BitmapPages = ceil(GeoUpper / PageSize / BitsPerPage)
BitsPerPage = PageSize * 8
```

| GeoUpper | PageSize | Total Pages | BitmapPages | Bitmap Size |
|----------|----------|-------------|-------------|-------------|
| 1GB      | 4KB      | 262,144     | 8           | 32KB        |
| 64GB     | 4KB      | 16,777,216  | 512         | 2MB         |
| 256GB    | 4KB      | 67,108,864  | 2,048       | 8MB         |
| 1TB      | 4KB      | 268,435,456 | 8,192       | 32MB        |
| 256GB    | 64KB     | 4,194,304   | 8           | 512KB       |

The bitmap pages themselves are never marked as free in the bitmap — their
bits are permanently clear (reserved). The same applies to meta pages (pages
0 and 1). Data pages start at page `2 + BitmapPages`.

##### Bitmap Storage

The bitmap is stored directly in the mmap. In pwrite mode, bitmap
modifications are written via `pwrite()` as part of the dirty page set. In
writemap mode, bitmap modifications happen directly in the mmap. Either way,
bitmap pages participate in the same `fdatasync()` ordering as data pages —
they are flushed before the meta page swap.

Bitmap pages do not use the standard page header. The entire page is usable
as bitmap data (PageSize × 8 bits per page). The page type is identified by
its position in the file (pages 2 through `2 + BitmapPages - 1`), not by a
header field.

##### Two-Level Summary

To accelerate allocation searches over large databases, the bitmap uses a
two-level structure:

- **Level 0 (detail):** One bit per page in the database, covering page IDs
  0 through `GeoUpper / PageSize - 1`. Stored across bitmap pages 2
  through `2 + BitmapPages - 1`. Bits for meta pages (0, 1) and bitmap
  pages (2 through `2 + BitmapPages - 1`) are permanently clear.
- **Level 1 (summary):** A separate in-memory array, one bit per uint64
  word of the detail level. A summary bit is set if the corresponding
  64-page word in the detail level has **any** set bits (any free pages).
  Size: `ceil(TotalPages / 64 / 64)` uint64 words. The summary is rebuilt
  from the detail bitmap when the database is opened and maintained
  incrementally during transactions.

At 4KB page size with 256GB `GeoUpper` (67M pages): the detail level is
~1M uint64 words (8MB across 2048 bitmap pages). The summary level is
~16K uint64 words = 128KB in memory. The summary allows skipping 64-page
regions with no free space during allocation scans.

For contiguous-run searches (overflow page allocation), the writer scans
summary words to find regions with free pages, then scans detail words
within those regions using `math/bits.TrailingZeros64` and
`math/bits.LeadingZeros64` to find runs. A single uint64 word covers 64
pages — a run of N < 64 can be found within one word; larger runs span
word boundaries with a carry-forward scan.

##### Bitmap Operations

**Set bit (free a page):** Load the uint64 word containing the page's bit,
OR in the bit, write the word back. Update the summary word if the detail
word transitioned from 0 to non-zero. O(1).

**Clear bit (allocate a page):** Load the uint64 word, AND out the bit,
write back. Update the summary word if the detail word transitioned from
non-zero to 0. O(1).

**Find first free (single-page alloc):** Scan summary words starting from
the LIFO hint (see LIFO Allocation Locality) for a non-zero word. Within
that summary region, scan detail words for a set bit. Clear it and return.
O(1) amortized with the LIFO hint; O(TotalPages/64) worst case.

**Find N contiguous free (multi-page alloc):** Scan detail words for runs
of consecutive set bits. Within a word, `math/bits.TrailingZeros64` on the
complement finds the length of a run from the LSB. Across word boundaries,
track the trailing run of one word and the leading run of the next. O(scanned
words).

**Count free pages:** `math/bits.OnesCount64` (hardware `popcnt`) across
all detail words. Cached in `NumFreePages` in the meta page and maintained
incrementally.

#### Retired Page Log (RPL)

The retired page log tracks which pages were freed by which transaction. This
information is needed for MVCC safety: a page freed by transaction T cannot be
moved into the allocation bitmap until no active reader holds a snapshot ≤ T.

The RPL is an append-only doubly-linked list of segment pages. Each segment
page contains a batch of `(TxnID, PageID)` entries:

```
RPL Segment Page
+---------------------------+
| Page Header (16 bytes)    |
+---------------------------+
| NewerSegment | uint64     |  page ID of the next newer segment (0 = this is head)
| OlderSegment | uint64     |  page ID of the next older segment (0 = this is tail)
| EntryCount   | uint16     |  number of entries in this segment
| Padding      | 6 bytes    |
+---------------------------+
| Entry 0: TxnID (uint64) + PageID (uint64)  |
| Entry 1: TxnID (uint64) + PageID (uint64)  |
| ...                                         |
+---------------------------------------------+
```

Segment capacity at 4KB page size: 16 (page header) + 8 + 8 (link pointers)
+ 2 (EntryCount) + 6 (padding) = 40 bytes overhead. Remaining
`4096 - 40 = 4056` bytes / 16 bytes per entry = **253 entries per segment
page**. A transaction freeing 10,000 pages fills ~40 segment pages.

The meta page stores `RPLHeadPage` (newest segment) and `RPLTailPage` (oldest
segment). Segments are doubly linked: `OlderSegment` links from head toward
tail; `NewerSegment` links from tail toward head. The doubly-linked structure
allows efficient operations at both ends: appending new segments at the head
and reclaiming old segments from the tail.

##### RPL Append (At Commit Time)

When a write transaction commits with retired pages:

1. Allocate one or more new segment pages from the bitmap (or file extension).
   Each commit always creates **new** segment pages rather than appending to
   existing segments. This avoids CoW of previous segments — the old head
   segment is immutable (it belongs to a previous transaction's snapshot).
2. Fill segment pages with `(currentTxnID, pageID)` entries, sorted by page
   ID for cache-friendly processing during reclamation. If the retired list
   exceeds one segment page's capacity (253 entries), allocate additional
   segment pages and link them via `OlderSegment`/`NewerSegment`.
3. Link the new head to the previous head: set the new head's
   `OlderSegment` to the old `RPLHeadPage`, and update the old head's
   `NewerSegment` to point to the new head. The old head must be CoW'd for
   this link update — the old head page is added to the dirty set and its
   original page ID is added to `tx.retiredPages` (this is bounded: at most
   one extra retired entry per commit for the old head page).
4. Update `RPLHeadPage` in the meta page to point to the new head. If the
   RPL was empty, also set `RPLTailPage`.

RPL segment pages are allocated from the bitmap like any other data page.
Allocating a segment page clears a bit in the bitmap — O(1), no further
allocation needed. A transaction retiring N pages needs at most
`ceil(N / 253) + 1` page allocations (segment pages + CoW of old head).
This is bounded and non-recursive.

##### RPL Reclamation

At the start of a write transaction (or lazily on first `pageAlloc()`), the
writer reclaims RPL entries whose pages are safe to reuse:

1. Read the oldest active reader's TxnID from the reader table (see
   Cross-Process Coordination).
2. Walk the RPL from the **tail** (oldest segments first).
3. For each entry where `TxnID < oldestReader`:
   a. Set the corresponding bit in the allocation bitmap.
   b. Remove the entry from the segment.
4. When a segment becomes empty, free the segment page itself (set its bit in
   the bitmap) and advance `RPLTailPage` to the segment's `NewerSegment`.
5. Update `RPLEntryCount` and `NumFreePages` in the meta page.

Reclamation is performed oldest-first so that the RPL shrinks from the tail.
Empty segment pages are immediately freed — their bitmap bits are set, making
them available for allocation in the same transaction.

Modifying a segment page during reclamation (removing entries) requires CoW:
the old segment page is copied to a new page, and the original page ID is
added to `tx.retiredPages`. This is bounded — each segment CoW allocates one
page from the bitmap (which is being populated by the very entries being
reclaimed) and retires one page. The reclaimed pages always outnumber the
CoW overhead.

##### Oldest Reader Caching

Scanning the reader table to find the minimum active TxnID is O(MaxReaders).
The writer caches this value (`tx.cachedOldestReader`) and refreshes it
lazily — only when the bitmap has no free pages and reclamation might unlock
more. Reading a stale (higher) value is conservative: it delays reclamation
but never causes incorrect behavior.

#### LIFO Allocation Locality

The allocation bitmap does not inherently provide LIFO (most-recently-freed
first) behavior, which is important for cache efficiency and SSD write
amplification. To achieve LIFO locality without complicating the bitmap:

The writer maintains a **LIFO hint** (`tx.allocHint`) — the page ID of the
last page reclaimed during the most recent reclamation pass. `pageAlloc()`
begins its bitmap scan at this hint and wraps around. Reclamation walks the
RPL from oldest to newest (tail to head). The last entries processed are
from the newest reclaimable transaction — the most recently freed pages.
The hint therefore naturally points to recently-freed regions.

For workloads with steady write/free/reuse cycles, this keeps the active
page set small and concentrated, achieving the same cache locality benefits
as LMDB's LIFO reclamation.

#### Loose Pages

Pages that are dirtied (copied via CoW) and then freed within the **same
write transaction** are called "loose pages." This commonly occurs during
B+tree rebalancing: a merge operation may CoW a node, then free one of the
two original nodes. The CoW'd copy becomes unnecessary if its contents are
merged into a sibling.

Loose pages are tracked in a singly-linked list (`tx.loosePages`) using the
page's own memory to store the link pointer (the page is already in memory
since it was dirtied). A counter (`tx.looseCount`) tracks the list length.

Loose pages are **immediately reusable** within the same transaction without
any bitmap or RPL interaction:
- `pageAlloc()` checks `tx.loosePages` first (O(1) pop from the linked list).
- Loose pages that are reused via `pageAlloc()` never touch the bitmap or RPL
  — they were allocated and freed within the same transaction, so no reader
  can ever reference them.
- At commit time, any loose pages still in the list (allocated a page ID but
  never reused) are added to `tx.retiredPages` for inclusion in the RPL.

#### Page Allocation Priority

`pageAlloc(n)` allocates `n` contiguous pages using this priority:

1. **Loose pages** (n=1 only): pop from `tx.loosePages`. O(1).
2. **Allocation bitmap**: scan the bitmap for a free page (n=1) or a
   contiguous run of free pages (n>1), starting from the LIFO hint.
3. **RPL reclamation**: if the bitmap has no suitable free pages, reclaim
   entries from the RPL (TxnID < oldest reader) into the bitmap, then retry
   step 2.
4. **Slow reader check**: if reclamation is blocked by a long-lived reader,
   invoke the slow reader callback (see Cross-Process Coordination). If the
   reader releases, refresh the oldest reader cache and retry step 3.
5. **File extension**: if no free pages are available, grow the file according
   to the geometry growth step and advance `FirstUnallocated`.

##### Tail Page Refund

After reclamation or at commit time, the writer checks if any pages at the
tail of the database file (page IDs equal to `FirstUnallocated - 1`,
`FirstUnallocated - 2`, etc.) are free in the bitmap. If so, those bits are
cleared and `FirstUnallocated` is decremented. This reclaims file space and
enables file shrinkage at commit time (see Database Geometry).

The refund process iterates: clearing tail bits may expose new tail pages.
It runs until no more tail pages are free. Loose pages are checked first
(by scanning the linked list for tail page IDs), then the bitmap (by
checking bits from `FirstUnallocated - 1` downward).

#### Freeing Pages

When a CoW operation replaces an old page with a new copy:
- If the old page was **dirtied in this transaction** (i.e., it was itself a
  CoW copy made earlier in this transaction), it becomes a **loose page** —
  added to `tx.loosePages`.
- If the old page was **from a previous transaction** (an immutable page in
  the mmap), its page ID is added to `tx.retiredPages` — a list of
  `(currentTxnID, pageID)` pairs to append to the RPL at commit time.

Note: retired pages are NOT immediately marked free in the bitmap. They enter
the RPL and are moved to the bitmap only when reclamation determines they are
safe to reuse (no active reader holds their snapshot).

#### Commit-Time Free Space Update

During commit, the writer:
1. Performs tail page refund: check the bitmap for free pages at the end of
   the file, decrement `FirstUnallocated`.
2. Moves any remaining loose pages into `tx.retiredPages`.
3. Appends all `tx.retiredPages` entries to the RPL (allocating new segment
   pages from the bitmap if the current head segment is full).
4. Updates `NumFreePages`, `RPLHeadPage`, `RPLTailPage`, and `RPLEntryCount`
   in the meta page.

Step 3 may allocate RPL segment pages from the bitmap. This is a bounded,
non-recursive operation: each segment page holds 253 entries, so a transaction
retiring N pages needs at most `ceil(N / 253) + 1` page allocations (segment
pages plus CoW of the old RPL head). Each allocation is a single bitmap bit
flip — no further cascading allocations.

Steps 1-4 happen before the dirty page flush and meta page swap.

## Dirty Page Tracking

A write transaction must track which pages have been modified (dirtied via
CoW) for two purposes: writing them to disk at commit time, and avoiding
double-CoW when the same page is modified multiple times within a
transaction.

### Data Structure

The dirty set is a **hash map** keyed by page ID. The value depends on the
write mode:

**pwrite mode:**
```
tx.dirtyPages map[uint64]*dirtyPage

type dirtyPage struct {
    data     []byte // heap-allocated page content (len = PageSize * (1 + overflow))
    lastUsed uint64 // monotonic counter for LRU spill priority
}
```

**writemap mode:**
```
tx.dirtyPages map[uint64]struct{} // page IDs only, content lives in the mmap
```

### Operations

| Operation | Method | Complexity |
|-----------|--------|------------|
| Add dirty page | `tx.dirtyPages[pageID] = &dirtyPage{...}` | O(1) amortized |
| Check if dirty | `_, ok := tx.dirtyPages[pageID]` | O(1) |
| Remove (spill) | `delete(tx.dirtyPages, pageID)` | O(1) |
| Count | `len(tx.dirtyPages)` | O(1) |
| Commit-time iteration | Sort keys, iterate in page-ID order | O(n log n) once |

The hash map replaces the sorted-array approach used in LMDB/libmdbx, where
insertions required maintaining sort order (O(n) shift) and lookups required
binary search (O(log n)). The map provides O(1) for all single-element
operations. The only operation that costs more is commit-time sequential
iteration, which requires extracting and sorting the keys — but this is a
one-time O(n log n) cost amortized against N `pwrite()` syscalls, making it
negligible.

### LRU Counter (pwrite mode only)

Each dirty page in pwrite mode carries a `lastUsed` counter — a monotonic
value incremented on each page access or modification. This counter drives
the LRU-based spill priority (see Page Spilling). The counter is stored in
the `dirtyPage` struct alongside the page data, so updating it on access is
a simple field write with no additional map lookup.

In writemap mode, there is no `lastUsed` counter because spilling does not
apply — the OS manages mmap page eviction transparently.

### Spilled Page Set

Pages that have been spilled to disk mid-transaction are tracked in a
separate **hash map**:

```
tx.spilledPages map[uint64]struct{}
```

When a B+tree traversal reaches a page, the lookup path checks:
1. `tx.dirtyPages` — if present, use the in-memory dirty copy.
2. `tx.spilledPages` — if present, the page was written to disk earlier in
   this transaction. Re-read it from the mmap (the spilled content is at
   the page's allocated position) and re-dirty it if modification is needed.
3. Otherwise, read directly from the mmap (immutable page from a previous
   transaction).

Using a hash map for `spilledPages` provides O(1) membership checks instead
of binary search on a sorted slice. The set is typically small (only
populated when `MaxDirtyPages` is exceeded), so the overhead is minimal
either way, but O(1) is simpler and consistent with the dirty page map.

### Commit-Time Write Ordering

At commit time in pwrite mode, dirty pages are written to disk via
`pwrite()`. For I/O efficiency, pages are written in ascending page-ID
order to produce sequential disk writes:

1. Extract keys from `tx.dirtyPages` into a slice.
2. Sort the slice.
3. Walk the sorted slice, issuing `pwrite()` for each page. Group
   adjacent pages (consecutive page IDs) into single write calls where
   possible (scatter-gather optimization).

Spilled pages are excluded from this write — they are already on disk at
their final positions.

In writemap mode, no explicit writes are needed — dirty pages are already
in the mmap. The sorted key extraction is still used for `fdatasync()`
range hints if the OS supports them, but this is an optimization, not a
correctness requirement.

## Page Spilling

When a write transaction dirties more pages than can fit in available memory,
dirty pages must be spilled (written to disk mid-transaction) to make room for
further modifications. Without spilling, very large write transactions would
consume unbounded memory.

### Spill Trigger

Spilling is triggered when the dirty page count exceeds `Options.MaxDirtyPages`
(default: 65536). The writer selects a subset of dirty pages to write to disk
via `pwrite()`, removing them from the in-memory dirty set.

### LRU-Based Spill Priority

Pages are selected for spilling based on a Least Recently Used (LRU) policy.
Each dirty page carries a `lastUsed` counter that is updated whenever the page
is accessed or modified during the transaction. When spilling, pages with the
lowest `lastUsed` values are spilled first — these are the pages least likely
to be accessed again.

This is significantly better than arbitrary or page-number-ordered spilling.
Pages that are actively being modified (e.g., B+tree internal nodes on the
current insertion path) stay in memory, while cold pages (e.g., leaf pages
from earlier bulk insertions) are spilled.

### Spill Mechanics

1. Sort dirty pages by `lastUsed` (ascending — coldest first).
2. Write the selected pages to their allocated positions via `pwrite()`. Group
   adjacent pages into single write calls where possible.
3. Remove spilled pages from `tx.dirtyPages` and add their page IDs to
   `tx.spilledPages` (see Dirty Page Tracking).
4. If a spilled page is later accessed again (e.g., a B+tree traversal reaches
   it), it is re-read from the mmap and re-dirtied. The `tx.spilledPages` map
   is checked during page lookup to detect this case (O(1)).

### Interaction with Free Space Management

Spilled pages remain allocated — they are not freed or added to the RPL.
They are simply written to their final on-disk location early. At commit time,
spilled pages do not need to be written again (they are already on disk). The
commit only needs to write the remaining (non-spilled) dirty pages and the
meta page.

Bitmap pages and RPL segment pages are never spilled. Bitmap pages are
modified during commit-time free space update (tail refund, RPL segment
allocation), and RPL segment pages are written as part of the commit-time
RPL append. Spilling them would cause unnecessary re-dirtying.

**Note**: Page spilling only applies in pwrite mode. In writemap mode, dirty
pages live in the mmap (backed by the OS page cache) and the OS handles
eviction transparently. The `MaxDirtyPages` option and spilling logic are
ignored when `WriteMap` is true.

## Copy-on-Write (CoW) Transaction Model

### Write Transaction

1. Writer acquires the intra-process semaphore, then the cross-process
   `flock(LOCK_EX)` on the lock file, both respecting `ctx` cancellation
   (see Write Lock). Returns `ctx.Err()` if cancelled while waiting.
2. Writer reads the active meta page to get current roots, TxnID, and geometry.
3. For each modification (insert, update, delete):
   - Traverse the B+tree from root to leaf.
   - Copy each page along the path (CoW — never modify in place).
   - Allocate new pages via `pageAlloc()` (loose pages → bitmap →
     RPL reclamation → slow reader check → file extension).
   - In **pwrite mode**: modified pages are held as heap-allocated dirty pages
     (with LRU counters for spill priority). If the dirty page count exceeds
     `MaxDirtyPages`, spill the coldest dirty pages to disk via `pwrite()`
     (see Page Spilling).
   - In **writemap mode**: modifications happen directly in the mmap. The
     dirty set tracks page IDs only (no heap copies, no spilling).
   - Old pages are tracked: pages from previous transactions go to
     `tx.retiredPages`; pages dirtied then freed in this transaction become
     loose pages in `tx.loosePages`.
4. Commit-time free space update:
   a. Perform tail page refund: check the bitmap for free pages at the end of
      the file, clear those bits, decrement `FirstUnallocated`.
   b. Move remaining loose pages into `tx.retiredPages`.
   c. Append all `tx.retiredPages` to the RPL (allocating new segment pages
      from the bitmap if needed — bounded, non-recursive).
   d. Update `NumFreePages`, `RPLHeadPage`, `RPLTailPage`, `RPLEntryCount`.
5. Flush dirty data pages to stable storage (see Dirty Page Tracking for
   commit-time write ordering):
   - **pwrite mode**: sort `tx.dirtyPages` keys, write non-spilled dirty
     pages via `pwrite()` in page-ID order.
   - **writemap mode**: no explicit write needed (pages are already in the mmap).
   - `fdatasync()` if `SyncMode` is `SyncDurable` or `SyncNoMeta`. Skipped for
     `SyncSafe` and `SyncNone`.
6. Update the inactive meta page with new root pointers, new TxnID, updated
   `FirstUnallocated`, and checksum. Written via `pwrite()` (pwrite mode) or
   directly in the mmap (writemap mode).
7. `fdatasync()` the meta page if `SyncMode` is `SyncDurable`. Skipped for all
   other modes. This is the **atomic commit point**.
8. If the OS file size exceeds `FirstUnallocated` by more than
   `GeoShrinkPages`, truncate the file via `ftruncate()`. This happens
   after the commit point — a crash before truncation leaves the file
   larger than necessary but consistent. The next commit will retry.
9. Writer clears `WriterPID`, releases the flock, then releases the
   intra-process semaphore.

### Read Transaction

1. Reader checks `ctx` — returns `ctx.Err()` if already cancelled.
2. Reader acquires a slot in the reader table (shared memory) via scan+CAS
   and records the current TxnID from the active meta page. Returns
   `ErrReadersFull` immediately if no slots are available (no blocking).
3. Reader traverses the B+tree using page pointers from that meta page. Because
   of CoW, all pages referenced by this TxnID are immutable — the writer will
   never modify them in place.
4. When done, the reader clears its slot in the reader table.

Readers never need locks on the data file. They never block writers. Writers
never block readers. The only contention point is the reader table slot
acquisition, which is a simple atomic CAS. The context is checked once before
acquisition but is not stored on the transaction — slot acquisition is
non-blocking so there is nothing to cancel.

### Write Batching

`DB.Batch()` amortizes write transaction commit costs across multiple
concurrent callers. Instead of each goroutine acquiring the write lock and
committing independently (paying fdatasync per transaction), multiple
callers' closures are collected and executed within a single transaction.

#### Mechanics

The `DB` struct maintains a batch channel and a batch coordinator:

```
db.batchCh chan batchCall

type batchCall struct {
    fn     func(tx *Tx) error
    ctx    context.Context
    result chan<- error
}
```

1. A caller invokes `db.Batch(ctx, fn)`. The closure, context, and a result
   channel are sent to `db.batchCh`. The caller blocks on the result channel.

2. A coordinator goroutine (started lazily on first `Batch` call) reads from
   `db.batchCh`. It collects calls until either:
   - `Options.MaxBatchSize` calls have accumulated (default: 1000), or
   - `Options.MaxBatchDelay` has elapsed since the first call in the current
     batch (default: 10ms).

   This delay allows more callers to join the batch, increasing throughput.
   The tradeoff is added latency — callers wait up to `MaxBatchDelay` for
   the batch to fill. For latency-sensitive workloads, set `MaxBatchDelay`
   to 0 (batch fires as soon as the coordinator goroutine runs).

3. The coordinator opens a write transaction via `db.Begin(ctx, true)` (using
   `context.Background()` — individual caller contexts are checked separately).

4. Each collected closure is executed sequentially within the transaction.
   Before executing a closure, its `ctx` is checked — if already cancelled,
   the closure is skipped and the caller receives `ctx.Err()`.

5. If all closures succeed, the transaction is committed. All callers receive
   `nil` on their result channels.

6. If any closure returns an error, the transaction is **rolled back**. The
   batch is then split: closures that succeeded are re-batched and retried
   in a new transaction, and the failing closure is retried individually via
   `db.Update()`. This ensures that one bad closure does not penalize the
   others. If the individual retry also fails, that caller receives the error.

7. If `Commit()` itself fails (e.g., I/O error), all callers in the batch
   receive the commit error.

#### Error Isolation

The rollback-and-retry strategy means that `Batch` provides the same
semantics as `Update` from each caller's perspective: either their closure's
effects are committed, or they receive an error. Callers do not need to know
or care that their work was batched.

The retry cost for a failing closure is bounded: the failing closure is
retried exactly once individually. If it fails again, the error is returned.
The successful closures are re-executed in a new batch, which may itself
collect additional pending callers.

#### When to Use Batch

`Batch` is optimal for workloads with many goroutines performing small,
independent writes (e.g., incrementing counters, appending log entries,
updating individual keys). The throughput improvement scales with the
number of concurrent callers — with N callers, commit cost is amortized
N-ways.

`Batch` is NOT suitable for:
- Large transactions that modify many keys (use `Update` directly).
- Transactions that depend on reading their own writes across callers
  (closures within a batch see each other's writes, which may cause
  unexpected interactions).
- Transactions that need exclusive control over commit timing.

### Transaction Leak Detection

A transaction that is garbage collected without `Commit()` or `Rollback()`
is a resource leak: the reader slot (or write lock) is held indefinitely,
blocking RPL reclamation and potentially causing unbounded file growth.
This is the most common user error with LMDB/libmdbx-style databases.

gmdb uses `runtime.AddCleanup` (Go 1.24+) to detect and recover from
leaked transactions automatically.

#### Setup

When `Begin()` creates a `Tx`, a cleanup is registered:

```go
tx := &Tx{...}
tx.cleanup = runtime.AddCleanup(tx, func(info txCleanupInfo) {
    // 1. Log warning with the stack trace captured at Begin() time.
    // 2. Release the reader slot (or write lock + semaphore).
}, txCleanupInfo{
    slotIndex:  tx.readerSlot,
    writable:   tx.writable,
    beginStack: captureStack(),
    db:         tx.db,
})
```

`txCleanupInfo` is a separate struct — not the `Tx` itself. This is
required by `AddCleanup`: the cleanup function must not reference the
object being cleaned up (no resurrection). The struct contains only the
information needed to release resources and log a diagnostic.

`captureStack()` calls `runtime.Callers()` at `Begin()` time to record
the call stack. This is stored in `txCleanupInfo` and included in the
warning message so the user can identify exactly where the leaked
transaction was opened.

#### Normal Close

When `Commit()` or `Rollback()` is called, the cleanup is cancelled:

```go
func (tx *Tx) Commit() error {
    tx.cleanup.Stop()
    // ... normal commit logic ...
}
```

`runtime.Cleanup.Stop()` prevents the cleanup function from running. In
the normal (non-leak) case, `AddCleanup` at `Begin()` time and `Stop()`
at close time are the only overhead — both are cheap operations with no
allocation.

#### Cleanup Behavior

When the GC collects a leaked `Tx`:

1. **Log a warning** via the `*slog.Logger` on the `DB` struct (see
   Options). The message includes:
   - Whether it was a read or write transaction.
   - The TxnID held by the transaction.
   - The stack trace from `Begin()` showing where the leak originated.

2. **Release the reader slot** by storing `TxnID = 0` (atomic store) in
   the reader table. This unblocks RPL reclamation for pages held by the
   leaked transaction's snapshot.

3. **Release the write lock** (if writable): clear `WriterPID`, release
   the flock, release the intra-process semaphore. This unblocks other
   writers.

The cleanup runs on a GC background goroutine — it must not block or
panic. All operations above are non-blocking (atomic store, syscall
flock/funlock, channel send).

#### Limitations

- **Timing is non-deterministic**: the cleanup runs when the GC collects
  the `Tx`, which depends on memory pressure and GC scheduling. A leaked
  transaction may hold its reader slot for an extended period before the
  GC runs. This is a safety net, not a substitute for correct resource
  management.
- **Cross-process**: the cleanup only runs in the process that created
  the transaction. If a process exits without closing transactions, the
  reader slots are reclaimed via PID-based stale detection (see Reader
  Table), not via cleanup.
- **Debug, not control flow**: applications should not rely on cleanup
  for normal operation. It exists solely to detect bugs and limit their
  blast radius.

## Cross-Process Coordination

### Lock File Layout

A separate file (`<dbname>.lock`) is mmap'd as shared memory by all processes.

```
Lock File
+---------------------------+
| Header (16 bytes)         |
| Magic        | uint64     |  identifies file as gmdb lock file
| MaxReaders   | uint32     |  number of reader slots (set at creation)
| WriterPID    | uint32     |  PID of current write txn holder (0 = no writer)
+---------------------------+
| Reader Table              |
| +----------+----------+   |
| | TxnID    | PID      |   | Slot 0
| | uint64   | uint32   |   |
| | Padding  | 4 bytes  |   |
| +----------+----------+   |
| | TxnID    | PID      |   | Slot 1
| | ...                  |   |
| +----------+----------+   |
| | ...                  |   | up to MaxReaders slots
| +----------+----------+   |
+---------------------------+
```

**Header (16 bytes):**
- `Magic` (uint64): Identifies the file as a gmdb lock file. Validates that
  the lock file belongs to this database and has not been corrupted.
- `MaxReaders` (uint32): Number of reader slots. Set at lock file creation
  time via `Options.MaxReaders` (default: 4096). Immutable after creation.
- `WriterPID` (uint32): PID of the process currently holding the write lock.
  Set when the write lock is acquired, cleared to 0 on release. Used for
  stale writer detection (see Stale Writer Recovery).

**Reader Slot (16 bytes):**
- `TxnID` (uint64, atomic): The snapshot transaction ID held by this reader.
  A value of 0 means the slot is free. Non-zero means the slot is active.
- `PID` (uint32): Process ID that owns this slot. Used for stale reader
  detection (`kill(pid, 0)`).
- Padding (4 bytes): Aligns slot to 16 bytes.

Total lock file size: 16 + (16 × MaxReaders). With default MaxReaders=4096:
16 + 65536 = 65552 bytes (~64KB, fits in 16 pages at 4KB page size).

The lock file is mmap'd with `MAP_SHARED` by all processes for the reader table.
The write lock is a separate concern handled via `flock()` (see below).

### Lock File Lifecycle

The lock file is ephemeral. The first process to open the database creates the
lock file, writes the header (including `Magic`, `MaxReaders`, `WriterPID=0`),
and initializes all reader slots to zero. Subsequent processes validate `Magic`,
read `MaxReaders` from the header, and mmap the file at the corresponding size.
If the lock file is deleted (e.g., after all processes exit), the next opener
recreates it. `MaxReaders` is NOT stored in the data file — it is a runtime
coordination property, not a data property.

On open, if the lock file already exists, the opener checks `WriterPID`. If
non-zero and the PID is no longer alive (`kill(pid, 0)` returns `ESRCH`), the
writer crashed while holding the lock — see Stale Writer Recovery.

### Write Lock

Write serialization uses two layers:

- **Intra-process**: a channel-based semaphore (`chan struct{}` with capacity
  1) on the `DB` struct. This prevents two goroutines in the same process
  from attempting concurrent writes. A channel is used instead of
  `sync.Mutex` because it supports `select` with `ctx.Done()` for
  context-aware blocking.
- **Cross-process**: `flock(LOCK_EX)` on the lock file. This prevents writers
  in different processes.

A `Begin(ctx, writable=true)` call acquires the write lock in two phases,
both respecting context cancellation:

1. **Intra-process**: `select` on the semaphore channel and `ctx.Done()`. If
   the context is cancelled while waiting, return `ctx.Err()` immediately.
2. **Cross-process**: attempt `flock(LOCK_EX)` in a separate goroutine. The
   calling goroutine `select`s on the flock completion channel and
   `ctx.Done()`. If the context is cancelled while waiting for flock, the
   flock attempt is abandoned (the background goroutine will complete the
   flock and immediately release it via `flock(LOCK_UN)`).
3. Store the caller's PID in the lock file header's `WriterPID` field.

`Commit()` and `Rollback()` clear `WriterPID` to 0, release the flock, then
release the semaphore. This two-layer approach is necessary because `flock()`
is per-fd and per-process — a second goroutine calling `flock()` on the same
fd would succeed immediately (the kernel considers the lock already held by
this process).

The `DB` struct holds a single dedicated fd for the write lock (`db.lockFd`),
opened separately from the fd used for the reader table mmap. This fd is used
exclusively for `flock()`/`funlock()` calls.

#### Stale Writer Recovery

If a process crashes while holding the write lock, `WriterPID` remains non-zero
and the `flock()` is automatically released by the kernel (flock locks are
released on fd close / process exit). On `Open()` or when attempting to acquire
the write lock, if `WriterPID` is non-zero, the process checks whether the PID
is still alive via `kill(pid, 0)`:

- If alive: the writer is still running — proceed with normal `flock()` which
  will block until the writer finishes.
- If dead (`ESRCH`): the writer crashed. The flock is already released by the
  kernel. The new writer acquires the flock, then performs recovery:
  1. Read both meta pages and select the valid one (highest TxnID with valid
     checksum). The crashed writer's partial commit is invisible — CoW ensures
     the previous meta page points to a consistent tree.
  2. Scan the reader table for slots with the dead writer's PID and clear them
     (the crashed process may have also held read transactions).
  3. Clear `WriterPID` to 0 (it will be set to the new writer's PID shortly).

No special rollback logic is needed for tree consistency — the CoW model
guarantees that the previous meta page points to a fully consistent tree.

In **pwrite mode**, bitmap modifications are held in the dirty page set on
the heap and only written at commit time. If the writer crashes before
commit, no bitmap modifications reach disk — the on-disk bitmap is fully
consistent with the previous meta page. No leaked pages.

In **writemap mode**, bitmap modifications happen directly in the mmap and
the OS may flush them to disk at any time. If the writer crashes, some
bitmap bits may have been cleared (pages allocated) for pages that were
never committed to any tree. These pages are "leaked" — the bitmap says
they are in use, but no reachable structure references them. Leaked pages
are recovered lazily: `Check()` can detect them (pages with cleared bitmap
bits not reachable from any B+tree, RPL, or meta structure), and a
recovery write transaction can set their bitmap bits. Alternatively,
`CopyTo(compact=true)` produces a clean copy with no leaks. In practice,
leaked pages from a crash are bounded by the crashed transaction's dirty
set — a small, one-time space cost until recovered.

### Reader Table

Slot allocation uses a simple scan with atomic CAS — no free stack or other
auxiliary data structure. The reader table is a flat array of 16-byte slots
stored in the lock file's shared mmap. All operations use atomic memory
operations visible across processes.

**Slot acquire (`Begin` read transaction):**
1. Scan the reader table for a slot where `TxnID == 0` (free).
2. Atomically CAS the `TxnID` field from 0 to the current meta page's TxnID.
   If the CAS fails (another goroutine or process claimed the slot
   concurrently), continue scanning.
3. Store the caller's PID in the slot's `PID` field.
4. If all slots are occupied, return `ErrReadersFull`.

The CAS on `TxnID` is the serialization point. The scan is O(MaxReaders) in
the worst case, but in practice most slots are concentrated at the low end
of the array (temporal locality). With 16-byte slots, 4096 slots = 64KB —
fits in L2 cache, sequential scan with hardware prefetching.

**Slot release (`Commit`/`Rollback` read transaction):**
1. Store `TxnID = 0` (atomic store). This single operation makes the slot
   free. The PID field is left as-is — it is only meaningful when `TxnID`
   is non-zero.

The release is a single atomic store. No CAS needed — only the slot owner
writes to its own slot.

**Stale reader detection:** During the writer's reader table scan (to find
the minimum active TxnID), if a slot has a non-zero `TxnID` and its `PID`
is no longer alive (checked via `kill(pid, 0)` returning `ESRCH`), the
writer clears the slot by storing `TxnID = 0`. This reclaims slots from
crashed processes.

#### Go Goroutine Model

Go multiplexes goroutines across OS threads, but this does not affect the
reader table design. Each concurrent read transaction — regardless of which
goroutine or OS thread runs it — claims its own slot via atomic CAS. Multiple
slots may share the same PID (same process), which is correct:

- **Slot allocation**: the CAS on the TxnID field serializes slot claims across
  both goroutines (same process) and external processes.
- **Stale detection**: `kill(pid, 0)` checks process liveness, not thread
  liveness. If a process crashes, all its slots (potentially many) are stale
  and can be reclaimed. This is the desired behavior.
- **Oldest reader scan**: the writer finds the minimum TxnID across all
  occupied slots. Multiple slots from the same process with different TxnIDs
  are handled naturally — the oldest one governs RPL reclamation.

The consequence is that a single Go process running N concurrent read
transactions consumes N reader slots. Applications must set `MaxReaders`
high enough to accommodate the expected total across all processes.

### Writer's Page Reclamation

Before reclaiming retired pages, the writer scans the reader table to find the
minimum active TxnID. Any RPL entries with TxnID < min_active are safe to
reclaim — their bits are set in the allocation bitmap, making them available
for allocation.

### Slow Reader Handling

A single long-lived reader prevents all RPL reclamation for transactions
newer than its snapshot, causing unbounded file growth. To address this, the
application can register a `SlowReader` callback via `Options` (see API
Surface) that is invoked when a reader is blocking page allocation.

The callback is invoked from `pageAlloc()` when:
1. The allocation bitmap has no suitable free pages.
2. The RPL has no more reclaimable entries (all remaining entries have
   `TxnID >= oldestReader`).
3. A reader in the reader table is blocking reclamation.

The callback receives information about the lagging reader and returns an
action. `SlowReaderWait` causes `pageAlloc()` to refresh the reader table
and retry (the reader may have released its slot in the meantime).
`SlowReaderAbort` causes `pageAlloc()` to return `ErrDBFull`.

The callback is invoked at most once per `pageAlloc()` call to avoid busy
loops. The application can use the callback to log warnings, send alerts,
or take corrective action (e.g., killing a stuck process identified by PID).

## mmap Strategy

The database supports two write modes: **pwrite mode** (default) and
**writemap mode** (`Options.WriteMap = true`). Both share the same read path
and mmap resizing strategy. The mode is chosen at `Open()` time and applies
to all write transactions for the lifetime of the `DB` handle.

### Read Path

All processes mmap the data file with at least:
```
MAP_SHARED | PROT_READ
```

In writemap mode the mapping adds `PROT_WRITE` (see below).
Reads go directly through the mmap. No system calls, no copies. The OS page
cache serves the data.

### Write Path: pwrite Mode (Default)

The writer does NOT write through the mmap. Instead:
- Dirty pages are allocated on the Go heap as `[]byte` slices.
- Modifications happen on these heap copies.
- At commit, dirty pages are written to their allocated positions via
  `pwrite()`.
- `fdatasync()` flushes data, then the meta page is written and synced.

This mode provides **heap isolation**: a stray pointer or buffer overrun in
user code cannot corrupt the on-disk database because the mmap is read-only.
Page spilling applies in this mode (see Page Spilling).

### Write Path: Writemap Mode

When `Options.WriteMap` is true, the data file is mapped with:
```
MAP_SHARED | PROT_READ | PROT_WRITE
```

The writer modifies pages directly in the mmap:
- CoW still applies: the writer allocates a new page (from bitmap or file
  extension) and copies the old page's content into the new location **in the
  mmap**.
- Modifications happen directly on the mmap'd page. No heap copy.
- At commit, `msync()` or `fdatasync()` flushes the dirty range, then the
  meta page is updated and synced.

**Advantages:**
- No heap allocation for dirty pages — reduces GC pressure significantly.
- Single flush operation instead of N `pwrite()` calls + flush.
- Lower memory usage: no separate heap copies of modified pages.
- Better performance for write-heavy workloads.

**Tradeoffs:**
- **No heap isolation**: a bug (stray pointer, buffer overrun) can corrupt the
  mmap'd file directly. The database file *is* the mutable working memory.
- **No page spilling**: since pages are already in the mmap (backed by the OS
  page cache), spilling is unnecessary and does not apply. The OS handles
  eviction of mmap'd pages to disk transparently.
- **Dirty page tracking**: the writer still tracks which pages were modified
  (for the free space management and CoW bookkeeping), but the dirty set stores page IDs
  only — not page content.

The full commit path covering both write modes and all sync modes is described
in the Write Transaction section (see Copy-on-Write Transaction Model,
steps 5–7).

### mmap Resizing

The mmap region is sized to `GeoUpper` (the maximum database size in pages).
This over-allocates virtual address space — only the file-backed portion is
usable, but the mapping does not need to change as the file grows or shrinks.
The unmapped region beyond the file size will SIGBUS if accessed, so readers
must check `FirstUnallocated` from the meta page.

**Note**: Large virtual address reservations may be affected by Linux
`vm.overcommit_memory` settings or per-process `RLIMIT_AS` limits. On most
default configurations this is not an issue — the kernel distinguishes between
reserved virtual address space and committed memory. Users with restrictive
settings may need to lower `GeoUpper`.

## Durability Modes

The database supports four durability modes, configurable via `Options.SyncMode`.
The mode controls which `fdatasync()` calls are performed during commit. All
modes preserve **database integrity** (the file is always structurally valid)
except `SyncNone`. The tradeoff is between commit latency and how much
data may be lost on a crash.

| Mode | Data Sync | Meta Sync | On Crash | Performance |
|------|-----------|-----------|----------|-------------|
| `SyncDurable` (default) | `fdatasync()` | `fdatasync()` | No data loss. Full ACID. | Slowest |
| `SyncNoMeta` | `fdatasync()` | skip | Last committed transaction may be lost. DB is consistent — falls back to previous meta page. | ~2x faster |
| `SyncSafe` | skip | skip | Rolls back to the last **steady commit** (the last commit that was explicitly synced via `DB.Sync()` or the last `SyncDurable`/`SyncNoMeta` commit). DB is always consistent — no corruption. | Much faster |
| `SyncNone` | skip | skip | **Risk of corruption.** No guarantees. For benchmarks and ephemeral data only. | Fastest |

### Steady Commits

In `SyncSafe` mode, a commit writes data and meta pages to their on-disk
locations (via `pwrite()` in pwrite mode, or directly in the mmap in writemap
mode) but skips all `fdatasync()` calls. The OS page cache holds the writes,
which will eventually reach disk, but the order is not guaranteed.

A **steady commit** is a commit where both data and meta pages have been
confirmed on stable storage. Steady commits occur when:
- `DB.Sync()` is called explicitly (forces `fdatasync()` of the data file).
- A commit happens in `SyncDurable` or `SyncNoMeta` mode (these sync as part
  of their normal commit path).

The meta page tracks whether the last commit is steady via the existing
checksum and TxnID mechanism. On recovery after a crash:
- The database reads both meta pages.
- If a meta page's data pages are not on disk (because the crash happened
  before the OS flushed them), reading those pages will return stale or zero
  data — the B+tree will be inconsistent.
- The database falls back to the other meta page, which points to the last
  steady commit's tree. All transactions after that steady commit are lost.

This is safe because CoW never modifies existing pages — the steady commit's
tree is entirely intact on disk.

### SyncNone Warning

`SyncNone` provides no crash safety whatsoever. Because `pwrite()` ordering
is not guaranteed without `fdatasync()`, the meta page could reach disk before
the data pages it references. A crash in this state leaves the meta page
pointing to unwritten or partially written data pages — the database is
**corrupted**. Use this mode only for ephemeral data or benchmarks where the
database can be discarded after a crash.

The full commit path with mode-dependent behavior is described in the
Write Transaction section (see Copy-on-Write Transaction Model, steps 5–7).

## Database Geometry

The database file size is managed dynamically between configurable lower and
upper bounds. The geometry is stored in the meta page and controls how the
file grows and shrinks.

### Geometry Parameters

| Parameter | Meta Field | Description | Default |
|-----------|-----------|-------------|---------|
| Lower bound | `GeoLower` | Minimum file size in pages. File never shrinks below this. | `2 + BitmapPages` (meta + bitmap) |
| Upper bound | `GeoUpper` | Maximum file size in pages. Determines mmap reservation size. | 256GB / PageSize |
| Growth step | `GeoGrowPages` | Number of pages to grow by when extending the file. | 65536 pages (256MB at 4KB pages) |
| Shrink threshold | `GeoShrinkPages` | Shrink the file when `fileSize - FirstUnallocated > threshold`. | 131072 pages (512MB at 4KB pages) |

Geometry is set at database creation time via `Options` and persisted in the
meta page. It can be modified by calling `Tx.SetGeometry()` on a write
transaction — the new values take effect when the transaction commits.

### File Growth

When `pageAlloc()` needs to extend the file:
1. Calculate new size: `alignUp(FirstUnallocated + needed, GeoGrowPages)`.
2. Clamp to `GeoUpper`. If the new size would exceed `GeoUpper`, return
   `ErrDBFull`.
3. Extend the file via `ftruncate()`. The existing mmap (which reserves up to
   `GeoUpper`) covers the new pages automatically — no remap needed.

### File Shrinkage

After the commit point (step 7 of the write transaction), if the OS file size
exceeds `FirstUnallocated` by more than `GeoShrinkPages`:
1. Calculate new size: `alignUp(FirstUnallocated, GeoGrowPages)`.
2. Clamp to `GeoLower`.
3. Truncate the file via `ftruncate()`. The mmap reservation remains at
   `GeoUpper` — the truncated region becomes unmapped (SIGBUS on access),
   which is safe because `FirstUnallocated` in the meta page prevents any
   reader from accessing those pages.

File shrinkage is automatic and zero-overhead — it happens as a natural
consequence of the tail page refund mechanism during commit. No explicit
compaction is needed for the common case of data deletion.

## Keyspaces

The root meta page points to a "keyspace B+tree" — a B+tree whose keys are
keyspace names (byte strings) and whose values are keyspace descriptors:

```
Keyspace Descriptor
+----------+----------+----------+----------+---------------+
| Root     | Depth    | Count    | Flags    | DupFixedSize  |
| uint64   | uint16   | uint64   | uint16   | uint16        |
+----------+----------+----------+----------+---------------+
```

Total descriptor size: 8 + 2 + 8 + 2 + 2 = 22 bytes.

- **Root**: Page ID of this keyspace's B+tree root.
- **Depth**: Height of the B+tree (for optimization).
- **Count**: Number of key-value pairs (for DUPSORT keyspaces, this is the
  total number of key-value pairs across all duplicate sets).
- **Flags**: Keyspace behavior flags:
  - Bit 0: `DupSort` — multiple sorted values per key.
  - Bit 1: `DupFixed` — all duplicate values have fixed size (requires
    `DupSort`). `DupFixedSize` stores the value size.
  - Bits 2-15: Reserved (must be 0).
- **DupFixedSize**: Fixed duplicate value size in bytes. Only meaningful when
  `DupFixed` flag is set. Zero otherwise.

Flags are set at keyspace creation time and immutable after. Opening an
existing keyspace with different flags returns an error.

Opening a keyspace within a transaction reads the descriptor from the keyspace
B+tree. Modifications to the keyspace update the descriptor (and its root)
which propagates up through the keyspace B+tree via CoW.

## API Surface

```go
// Sentinel errors.
var (
    ErrNotFound           = errors.New("gmdb: key not found")
    ErrKeyExist           = errors.New("gmdb: key already exists")
    ErrDBFull             = errors.New("gmdb: database full (GeoUpper reached)")
    ErrTxnFull            = errors.New("gmdb: transaction too large")
    ErrReadersFull        = errors.New("gmdb: no reader slots available")
    ErrKeyTooLarge        = errors.New("gmdb: key exceeds maximum size")
    ErrCorrupted          = errors.New("gmdb: database corrupted")
    ErrVersionMismatch    = errors.New("gmdb: format version mismatch")
    ErrReadOnly           = errors.New("gmdb: write operation on read-only transaction")
    ErrTxnDone            = errors.New("gmdb: transaction already committed or rolled back")
    ErrCursorUnpositioned = errors.New("gmdb: cursor not positioned")
    ErrIncompatibleFlags  = errors.New("gmdb: keyspace flags do not match existing keyspace")
    ErrDupSizeFixed       = errors.New("gmdb: value size does not match fixed duplicate size")
    ErrMultiVal           = errors.New("gmdb: ambiguous operation on key with multiple values")
)

// Open a database. Creates the file if it doesn't exist.
func Open(path string, opts *Options) (*DB, error)

// SyncMode controls the durability guarantees of committed transactions.
type SyncMode int

const (
    // SyncDurable syncs both data and meta pages. Full ACID. Default.
    SyncDurable SyncMode = iota
    // SyncNoMeta syncs data pages but not the meta page. Last transaction
    // may be lost on crash, but the database is always consistent.
    SyncNoMeta
    // SyncSafe skips all syncs. The database rolls back to the last steady
    // commit on crash. No corruption risk. Use DB.Sync() to create steady
    // commit points.
    SyncSafe
    // SyncNone skips all syncs with no safety net. Risk of corruption on
    // crash. For benchmarks and ephemeral data only.
    SyncNone
)

// Options for opening a database.
type Options struct {
    // PageSize in bytes. Only used when creating a new database.
    // Must be a power of 2 in range [4096, 65536]. Default: 4096.
    // Ignored when opening an existing database (read from meta page).
    PageSize int

    // Geometry controls database file size bounds and growth behavior.
    // Only used when creating a new database. When opening an existing
    // database, geometry is read from the meta page. Use Tx.SetGeometry()
    // to modify geometry of an existing database.
    Geometry Geometry

    // SyncMode controls the durability guarantees of committed
    // transactions. Default: SyncDurable.
    SyncMode SyncMode

    // WriteMap enables writemap mode. The data file is mapped read-write
    // and dirty pages are modified directly in the mmap instead of being
    // heap-allocated and written via pwrite(). Significantly faster for
    // write-heavy workloads but offers no protection against stray
    // pointer bugs corrupting the database. Default: false.
    WriteMap bool

    // MaxReaders is the maximum number of concurrent reader slots.
    // Default: 4096. Only used when creating a new lock file.
    // Ignored when the lock file already exists (read from lock file header).
    MaxReaders int

    // MaxDirtyPages is the maximum number of dirty pages held in memory
    // before spilling to disk. Default: 65536. Ignored in writemap mode
    // (spilling does not apply — see Page Spilling).
    MaxDirtyPages int

    // MergeThreshold is the B+tree page fill percentage below which a
    // page is merged with a sibling after a deletion. Range: 1-50.
    // Lower values waste more space but reduce merge/split churn.
    // Higher values keep pages fuller but cause more rebalancing.
    // Default: 25 (merge when page is less than 25% full).
    MergeThreshold int

    // SlowReader is called when a long-lived reader is blocking RPL
    // reclamation during page allocation. If nil, pageAlloc() falls
    // through to file extension when reclamation is blocked.
    SlowReader func(info SlowReaderInfo) SlowReaderAction

    // MaxBatchSize is the maximum number of Batch() calls to collect
    // before executing them in a single transaction. Default: 1000.
    MaxBatchSize int

    // MaxBatchDelay is the maximum time to wait for additional Batch()
    // calls before executing the current batch. Lower values reduce
    // latency; higher values increase throughput by collecting more
    // callers per transaction. Default: 10ms. Set to 0 to disable
    // delay (batch fires immediately when the coordinator runs).
    MaxBatchDelay time.Duration

    // Logger for diagnostic messages (leaked transactions, stale reader
    // recovery, stale writer recovery). If nil, diagnostics are discarded.
    // Default: nil.
    Logger *slog.Logger

    // FileMode for newly created files. Default: 0644.
    FileMode os.FileMode

    // ReadOnly opens the database in read-only mode.
    ReadOnly bool
}

// Geometry controls the database file size bounds and growth/shrink behavior.
// All sizes are specified in bytes and must be multiples of PageSize. They are
// converted to pages internally and stored in the meta page as page counts.
type Geometry struct {
    // Lower is the minimum database file size in bytes. The file never
    // shrinks below this. Must be a multiple of PageSize.
    // Default: (2 + BitmapPages) * PageSize (meta + bitmap pages).
    Lower int64

    // Upper is the maximum database file size in bytes. Determines mmap
    // reservation size. Must be a multiple of PageSize. Default: 256GB.
    Upper int64

    // GrowStep is the number of bytes to grow by when extending the file.
    // Must be a multiple of PageSize. Default: 256MB.
    GrowStep int64

    // ShrinkThreshold is the minimum number of bytes of unused space at
    // the end of the file before shrinking occurs. Must be a multiple of
    // PageSize. Default: 512MB.
    ShrinkThreshold int64
}

// SlowReaderInfo describes a reader that is blocking RPL reclamation.
type SlowReaderInfo struct {
    PID       int    // process ID of the slow reader
    TxnID     uint64 // transaction ID the reader is holding
    Lag       uint64 // number of transactions behind current
    HeldPages uint64 // estimated number of pages held unreclaimable
}

// SlowReaderAction determines how pageAlloc responds to a slow reader.
type SlowReaderAction int

const (
    SlowReaderWait  SlowReaderAction = iota // retry, reader may release
    SlowReaderAbort                         // abort with ErrDBFull
)

// DB is a handle to an open database.
type DB struct { ... }

func (db *DB) Close() error

// Sync flushes all outstanding writes to stable storage. In SyncSafe mode,
// this creates a steady commit point — the database will roll back to this
// point (at most) on crash. In SyncDurable and SyncNoMeta modes, this is a
// no-op (commits already sync). In SyncNone mode, this syncs but does not
// retroactively fix the lack of ordering guarantees from prior commits.
func (db *DB) Sync() error

// View executes a read-only transaction. The context governs slot
// acquisition only — once the transaction callback is entered, the
// context is not checked. Use context.Background() when no cancellation
// is needed.
func (db *DB) View(ctx context.Context, fn func(tx *Tx) error) error

// Update executes a read-write transaction. The context governs write
// lock acquisition — if the lock is held by another writer, the caller
// blocks until the lock is available or the context is cancelled. Once
// the transaction callback is entered, the context is not checked.
func (db *DB) Update(ctx context.Context, fn func(tx *Tx) error) error

// Batch submits a write operation to be batched with other concurrent
// callers into a single transaction. Multiple goroutines calling Batch
// concurrently will have their closures executed in one write transaction,
// amortizing the commit cost (fdatasync) across all of them.
//
// The context governs the wait for batch inclusion — if cancelled before
// the caller's closure executes, Batch returns ctx.Err(). Once the
// closure begins executing, the context is not checked.
//
// If fn returns an error, the entire batch is rolled back and retried:
// successful closures are re-executed in a new batch, and the failing
// closure is retried individually via Update. See Write Batching for
// details.
//
// Batch is a throughput optimization for workloads with many concurrent
// small writes. For exclusive write access or large transactions, use
// Update or Begin directly.
func (db *DB) Batch(ctx context.Context, fn func(tx *Tx) error) error

// Begin starts a transaction manually. The context governs lock/slot
// acquisition:
//   - For write transactions: blocks on the write lock, respecting
//     context cancellation. Returns ctx.Err() if cancelled while waiting.
//   - For read transactions: returns ErrReadersFull immediately if no
//     slots are available (no blocking). The context is checked once
//     before attempting slot acquisition.
//
// Once Begin returns a *Tx, the context is not stored — the caller
// controls the transaction lifetime via Commit()/Rollback().
func (db *DB) Begin(ctx context.Context, writable bool) (*Tx, error)

// Tx is a database transaction.
type Tx struct { ... }

func (tx *Tx) Commit() error
func (tx *Tx) Rollback() error

// SetGeometry updates the database geometry. Only valid on a write
// transaction. The new geometry takes effect when the transaction commits.
func (tx *Tx) SetGeometry(g Geometry) error

// KeyspaceFlags controls keyspace behavior. Set at creation time, immutable after.
type KeyspaceFlags uint16

const (
    // KfDupSort enables multiple sorted values per key (DUPSORT).
    KfDupSort KeyspaceFlags = 1 << iota
    // KfDupFixed requires all duplicate values to be the same fixed size
    // (requires KfDupSort). The size is specified via the fixedSize parameter
    // of OpenKeyspace at keyspace creation time.
    KfDupFixed
)

// OpenKeyspace opens a named keyspace within this transaction.
// If create is true and the keyspace doesn't exist, it is created with
// the given flags. If the keyspace already exists, flags must match the
// existing keyspace's flags or be zero (accept existing flags).
// fixedSize is only used when creating a KfDupFixed keyspace — it sets
// the fixed duplicate value size in bytes. Ignored otherwise.
func (tx *Tx) OpenKeyspace(name []byte, create bool, flags KeyspaceFlags, fixedSize uint16) (*Keyspace, error)

// DeleteKeyspace deletes a named keyspace and all its data.
func (tx *Tx) DeleteKeyspace(name []byte) error

// Keyspace is a handle to a named keyspace within a transaction.
type Keyspace struct { ... }

// Get returns the value for the given key. For DUPSORT keyspaces, returns
// the first (smallest) duplicate value. Returns ErrNotFound if the key
// does not exist.
func (ks *Keyspace) Get(key []byte) ([]byte, error)

// Put inserts or updates a key-value pair. For non-DUPSORT keyspaces, an
// existing value is replaced. For DUPSORT keyspaces, the value is added to
// the key's sorted duplicate set (no-op if the exact key-value pair already
// exists).
func (ks *Keyspace) Put(key, value []byte) error

// Delete removes a key and its value. For DUPSORT keyspaces, removes all
// duplicate values for the key. To delete a single duplicate, use
// Cursor.SeekDup() + Cursor.Delete().
func (ks *Keyspace) Delete(key []byte) error

// Cursor for iterating over key-value pairs.
func (ks *Keyspace) Cursor() *Cursor

type Cursor struct { ... }

// --- Core navigation ---

func (c *Cursor) First() (key, value []byte)
func (c *Cursor) Last() (key, value []byte)
func (c *Cursor) Next() (key, value []byte)
func (c *Cursor) Prev() (key, value []byte)

// Seek positions the cursor at the exact key. Returns the key-value pair,
// or nil if the key does not exist. For DUPSORT keyspaces, returns the
// first (smallest) duplicate value for the key.
func (c *Cursor) Seek(target []byte) (key, value []byte)

// SeekGE positions the cursor at the first key >= target.
// Returns the key-value pair, or nil if no such key exists.
// For DUPSORT keyspaces, returns the first duplicate value for that key.
func (c *Cursor) SeekGE(target []byte) (key, value []byte)

// Current returns the key-value pair at the current cursor position
// without moving the cursor.
func (c *Cursor) Current() (key, value []byte)

// Put inserts or updates a key-value pair at the current cursor position.
// More efficient than Keyspace.Put() when the cursor is already positioned
// near the target key (avoids a second tree traversal). If the cursor is
// not positioned, behaves like Keyspace.Put().
func (c *Cursor) Put(key, value []byte) error

// Delete deletes the key-value pair at the current cursor position.
func (c *Cursor) Delete() error

// --- DUPSORT operations (only valid on DupSort keyspaces) ---

// FirstDup positions the cursor at the first duplicate value for the
// current key. Returns the value, or nil if the cursor is not positioned.
func (c *Cursor) FirstDup() (value []byte)

// LastDup positions the cursor at the last duplicate value for the
// current key.
func (c *Cursor) LastDup() (value []byte)

// NextDup moves to the next duplicate value for the current key.
// Returns nil when there are no more duplicates (the cursor does NOT
// advance to the next key).
func (c *Cursor) NextDup() (key, value []byte)

// PrevDup moves to the previous duplicate value for the current key.
// Returns nil when at the first duplicate.
func (c *Cursor) PrevDup() (key, value []byte)

// NextKey moves to the first duplicate value of the next key, skipping
// remaining duplicates of the current key.
func (c *Cursor) NextKey() (key, value []byte)

// PrevKey moves to the last duplicate value of the previous key,
// skipping remaining duplicates of the current key.
func (c *Cursor) PrevKey() (key, value []byte)

// SeekDup positions the cursor at the first duplicate value >= target
// for the given key. Returns the value, or nil if not found.
func (c *Cursor) SeekDup(key, target []byte) (value []byte)

// CountDup returns the number of duplicate values for the current key.
func (c *Cursor) CountDup() (int, error)

// --- Statistics ---

// DBStats contains environment-level statistics.
type DBStats struct {
    // Free space
    FreePages    uint64 // total free pages (set bits in allocation bitmap)
    RetiredPages uint64 // pages in RPL, not yet reclaimable (held by readers)

    // Geometry
    FileSize     int64  // current data file size in bytes
    GeoLower     int64  // minimum file size in bytes
    GeoUpper     int64  // maximum file size in bytes
    FirstUnalloc uint64 // first unallocated page ID (high-water mark)

    // Readers
    ActiveReaders int // currently occupied reader slots
    MaxReaders    int // total reader slots
}

func (db *DB) Stats() DBStats

// CheckSeverity indicates the severity of an integrity issue.
type CheckSeverity int

const (
    CheckWarning CheckSeverity = iota // non-critical (e.g., suboptimal layout)
    CheckError                        // structural integrity violation
)

// CheckIssue describes a single integrity problem found during a database check.
type CheckIssue struct {
    Severity CheckSeverity
    PageID   uint64 // page where the issue was found (0 if N/A)
    Keyspace string // keyspace name (empty if global/bitmap/RPL)
    Message  string // human-readable description
}

// Check performs a full structural integrity walk of the database. It opens
// a read transaction, walks all B+trees (keyspace trees), the allocation
// bitmap, and the RPL, and verifies:
//   - Meta page checksum validity
//   - B+tree structural integrity (page reachability, no cycles, key ordering)
//   - Bitmap consistency (no overlap between free and in-use pages)
//   - RPL consistency (valid segment chain, no duplicate entries)
//   - Page accounting (all pages accounted for: data + bitmap + RPL + free + unallocated)
//   - Keyspace descriptor consistency (root page validity, counts)
//   - DUPSORT subpage and nested B+tree integrity
//
// The callback fn is invoked for each issue found. If fn returns a non-nil
// error, the check aborts early and Check returns that error. If all issues
// are reported without fn returning an error, Check returns nil.
func (db *DB) Check(fn func(issue CheckIssue) error) error

// CopyTo creates a hot backup of the database to the given path. The copy
// is taken from a consistent read transaction snapshot — writers are not
// blocked during the copy.
//
// If compact is false, the data file is copied as-is (including free space).
// This is fast but the copy may be larger than the live data.
//
// If compact is true, only live pages are written to the new file in
// B+tree order, producing a compacted copy with no free space and optimal
// page layout. This is slower but produces the smallest possible file.
func (db *DB) CopyTo(path string, compact bool) error

// TxStats contains per-transaction statistics. Accumulated during the
// transaction's lifetime and returned as a snapshot.
type TxStats struct {
    // Page management
    DirtyPages     uint64 // pages dirtied (CoW copies)
    LoosePages     uint64 // pages dirtied then freed in this txn
    ReclaimedPages uint64 // pages reclaimed from RPL into bitmap
    SpilledPages   uint64 // pages spilled to disk mid-transaction
    WrittenPages   uint64 // pages written at commit time

    // B+tree operations
    Gets    uint64 // key lookups
    Puts    uint64 // key inserts/updates
    Deletes uint64 // key deletions
    Splits  uint64 // page splits
    Merges  uint64 // page merges

    // Timing
    Duration time.Duration // time from Begin to Stats() call
}

func (tx *Tx) Stats() TxStats

// KeyspaceStats contains per-keyspace statistics.
type KeyspaceStats struct {
    Depth         int    // B+tree height
    BranchPages   uint64 // number of branch pages
    LeafPages     uint64 // number of leaf pages
    OverflowPages uint64 // number of overflow pages
    Entries       uint64 // total key-value pairs (including duplicates)
}

func (ks *Keyspace) Stats() (KeyspaceStats, error)
```

## Implementation Layout

All code lives in a single `gmdb` package (flat, no sub-packages). This avoids
circular dependency issues between tightly coupled components (pages, B+tree,
transactions, mmap) and keeps the public API to one import path. The code is
organized by file:

| File | Responsibility |
|------|---------------|
| `page.go` | Page header encoding/decoding. Branch page: cell directory, key lookup (binary search), insert/split. Leaf page: cell directory, KV lookup, insert/split, overflow references, DUPSORT subpage format. Meta page: encode/decode/validate checksum (including geometry fields, bitmap/RPL pointers). RPL segment page: encode/decode entry list, segment linking. |
| `btree.go` | B+tree search, insert (CoW path from leaf to root, split), delete (CoW, merge/rebalance with configurable `MergeThreshold`). Cursor: stateful iterator holding a stack of (pageID, index) pairs. DUPSORT: subpage management (inline sorted list), nested B+tree promotion/demotion, dup cursor operations. All operations work on page byte slices (from mmap), never Go heap objects. |
| `freelist.go` | Allocation bitmap: two-level (detail + in-memory summary) bitmap at fixed page offsets, bit set/clear, contiguous-run search with `math/bits` intrinsics, LIFO hint tracking. Retired page log (RPL): append-only doubly-linked list of segment pages, reclamation (walk from tail, move entries to bitmap, free empty segments). Loose page tracking: singly-linked list of intra-transaction recycled pages. Page allocation priority: loose pages → bitmap → RPL reclamation → slow reader check → file extension. Tail page refund for auto-compaction. Commit-time update: append retired pages to RPL, allocate segment pages from bitmap (bounded, non-recursive). |
| `spill.go` | Dirty page spilling to disk mid-transaction (pwrite mode only, no-op in writemap mode). LRU-based priority selection from `tx.dirtyPages` map. Spilled pages moved to `tx.spilledPages` map for re-dirtying detection. Adjacent page grouping for efficient I/O. |
| `geometry.go` | Database geometry management: grow/shrink bounds, growth step, shrink threshold. File growth via `ftruncate()`. File shrinkage at commit time after tail refund. `Tx.SetGeometry()` for runtime modification. |
| `mmap.go` | Platform-agnostic mmap interface. Initial mapping with over-allocated virtual address space (sized to GeoUpper). Supports both read-only (pwrite mode) and read-write (writemap mode) mappings. |
| `mmap_linux.go` | Linux mmap/munmap/msync syscalls. |
| `mmap_darwin.go` | macOS mmap/munmap/msync syscalls. |
| `lock.go` | Lock file creation and mmap (shared memory). Writer lock (channel semaphore intra-process + flock cross-process + WriterPID, context-aware). Stale writer recovery. Reader table: scan+CAS slot acquire, atomic store release, stale reader detection via PID liveness. Oldest-reader query for RPL reclamation. Slow reader detection and callback invocation. |
| `tx.go` | Read transaction: snapshot meta, acquire reader slot, read-only B+tree access. Write transaction: snapshot meta, acquire write lock, dirty page map (`map[uint64]*dirtyPage` in pwrite, `map[uint64]struct{}` in writemap), spilled page map, CoW operations, page lookup (dirty → spilled → mmap), spill trigger (pwrite only), commit (sort dirty keys + sequential pwrite + RPL append + bitmap update + meta swap + sync per SyncMode + geometry shrink), rollback. Leak detection: `runtime.AddCleanup` at Begin, `cleanup.Stop()` at Commit/Rollback. Stats accumulation. |
| `db.go` | Open/Close. Environment setup (mmap, lock file, geometry, write mode selection). Transaction lifecycle (Begin/Commit/Rollback, View/Update helpers). Write batching: Batch() channel, coordinator goroutine, rollback+retry on failure. Keyspace management. Sync(). Check(). CopyTo(). |
| `errors.go` | Sentinel error definitions. |
| `stats.go` | DBStats, TxStats, KeyspaceStats types and collection. |

## Limits

### Page Size

Configurable at database creation time. Must be a power of 2 in the range
4096–65536 (4KB–64KB). Stored in the meta page and immutable after creation.
Default: 4096 bytes.

### Maximum Key Size

Determined by page size. A branch page must fit at least 2 keys to allow
splitting. The fixed overhead is 24 bytes (16-byte page header + 8-byte
leftmost child pointer). Each key requires 4 bytes (cell directory entry) +
key bytes + 8 bytes (child pointer). The maximum key size is approximately
`(PageSize - 48) / 2`:

| Page Size | Max Key Size (approx) |
|-----------|----------------------|
| 4KB       | ~2024 bytes          |
| 8KB       | ~4024 bytes          |
| 16KB      | ~8024 bytes          |
| 64KB      | ~32744 bytes         |

Enforced at `Put()` time. Keys exceeding the limit return an error.

### Maximum Value Size

For non-DUPSORT keyspaces: inline values are limited by available space in the
leaf page. Values that exceed this are automatically stored as overflow pages.
There is no practical upper limit on value size (bounded only by disk space and
`GeoUpper`).

### Maximum Duplicate Value Size (DUPSORT)

For DUPSORT keyspaces, each duplicate value becomes a key in the nested B+tree
(or an entry in a subpage). The maximum duplicate value size is therefore the
same as the maximum key size — approximately `(PageSize - 48) / 2`. Overflow
pages are not used for duplicate values. A `Put()` call with a duplicate value
exceeding this limit returns an error.

## Checksums

Only meta pages carry checksums (xxhash64 of all fields). Data pages (branch,
leaf, overflow) do not have checksums.

**Rationale**: The meta page is the atomic commit point — a torn write here
would silently point to an inconsistent tree. The checksum detects this and
triggers fallback to the other meta page.

Data pages are protected by CoW: they are written to new locations before the
meta page is updated (with ordering enforced by `fdatasync()` in `SyncDurable`
and `SyncNoMeta` modes). A crash during a data page write leaves the meta page
pointing to the old (consistent) tree. The half-written page is orphaned and
never referenced. Per-page checksums would only catch silent bitrot after a
successful write, which modern filesystems (ext4, ZFS, btrfs) already detect.

## Integrity and Safety

- **No partial writes visible**: CoW ensures all modifications happen on new
  pages. The old tree is intact until the meta page swap.
- **Atomic commit**: A single meta page write (< page size, aligned) is the
  commit point. Even if it's torn, the checksum will fail and the DB falls
  back to the other meta page.
- **Write ordering**: In `SyncDurable` mode, dirty pages are fdatasync'd
  BEFORE the meta page update, and the meta page is fdatasync'd AFTER writing
  it. In other sync modes, ordering relies on CoW (see Durability Modes).
- **Reader isolation**: Readers see an immutable snapshot. Pages they reference
  cannot be reused until all readers on that TxnID have finished.
- **Stale reader recovery**: If a process crashes without releasing its reader
  slot, the PID-based detection allows the writer to reclaim the slot.
- **Stale writer recovery**: If the writer process crashes, `WriterPID` in
  the lock file header identifies the dead process. The kernel releases the
  flock automatically. The next writer detects the dead PID, cleans up reader
  slots from the crashed process, and proceeds — CoW guarantees the tree is
  consistent. In pwrite mode, no bitmap corruption occurs (dirty pages never
  reached disk). In writemap mode, some bitmap bits may leak (allocated but
  uncommitted pages); recoverable via `Check()` or `CopyTo(compact=true)`.
