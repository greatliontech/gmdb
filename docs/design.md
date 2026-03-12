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
| Free space | Freelist B+tree (composite key, LIFO reclaim) | Fixed-size entries, no overflow in freelist, cache-friendly reclamation |
| File geometry | Dynamic grow/shrink with configurable bounds | Auto-compaction via tail refund, no manual compaction needed |
| Isolation | Dual meta pages + CoW | No WAL needed, atomic commit |
| Crash safety | CoW + atomic meta page swap | File is always consistent |
| Durability | Four sync modes (Durable, NoMeta, Safe, None) | Configurable ACID vs. performance tradeoff |
| Cross-process | Shared memory lock file | Reader table for tracking oldest active reader |
| Write lock | Go mutex (intra-process) + flock (cross-process) | flock alone doesn't block same-process goroutines |
| Slow readers | Callback-based notification | Application controls policy; no silent unbounded growth |
| mmap | pwrite mode (default) or writemap mode | pwrite: heap isolation; writemap: direct mmap writes for performance |
| Page spilling | LRU-based spill to disk mid-transaction (pwrite mode only) | Bounds memory usage for large write transactions |
| Key ordering | Lexicographic (byte-ordered) | Simple, general, no custom comparator needed |
| Byte order | Little-endian (fixed) | Portable across architectures |
| Checksums | Meta pages only | CoW protects data pages; meta checksum detects torn commits |
| API | Transaction-based | Explicit read/write txns |
| Namespaces | Named keyspaces | Multiple B+trees in one file |

## File Layout

The database is a single file, divided into fixed-size pages. All pages are the
same size (configurable at creation time, immutable after). Supported page sizes
are powers of 2 from 4KB to 64KB. Default: 4096 bytes (OS page size).

All multi-byte integers are stored in little-endian byte order.

```
+--------+--------+--------+--------+--------+--------+----
| Meta 0 | Meta 1 | Page 2 | Page 3 | Page 4 | Page 5 | ...
+--------+--------+--------+--------+--------+--------+----
```

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
- **Type**: One of: Meta, Branch, Leaf, Overflow. (The freelist uses a regular
  B+tree with standard Branch and Leaf pages — no special page type needed.)
- **Count**: Number of items (keys in branch, key/value pairs in leaf).
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
| FreelistRoot     | uint64 - root page of freelist B+tree
| NumFreePages     | uint64 - total free pages
| KeyspaceRoot     | uint64 - root page of keyspace B+tree
| NumKeyspaces     | uint64 - number of keyspaces
| TxnID            | uint64 - transaction ID that wrote this meta
| Checksum         | uint64 - xxhash of all preceding bytes (header through TxnID)
+------------------+
```

Total meta page payload: 16 (header) + 4 + 4 + 4 + 4 + 11×8 = 120 bytes.
Fits comfortably in any supported page size (min 4KB).

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
freelist management, and page allocation.

##### Demotion

When deletions reduce a nested B+tree to a single leaf page that would fit as
a subpage (below the promotion threshold), the B+tree is demoted back to a
subpage. The leaf page is freed to the freelist, and the entries are packed
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

#### Freelist B+tree

Free pages are tracked in a dedicated B+tree (separate from user data). Pages
freed by a given transaction can only be reused once no reader is still using
that transaction's snapshot.

The freelist B+tree uses a **composite key** encoding with empty values:

```
Key: TxnID (uint64, big-endian) || PageID (uint64, big-endian)
Value: (empty — zero bytes)
```

Each freed page is a separate entry in the B+tree. Both components are stored
in big-endian byte order so that the standard lexicographic key comparison
sorts entries first by TxnID, then by PageID within the same transaction.

This design has several advantages:
- **No special value encoding**: reuses the existing B+tree leaf format as-is.
  Each entry is just a 16-byte key with no value.
- **No overflow concern**: a single transaction freeing many pages creates many
  small entries rather than one large value that could overflow a leaf page.
- **Efficient range scan for reclamation**: all entries with
  `TxnID < oldest_reader` are reclaimable. The scan direction is LIFO
  (newest eligible first — see Freelist Runtime Optimizations).
- **Simple allocation**: pop entries from the reclaimable range and delete them
  from the B+tree.
- **Bounded self-referential impact**: each insert/delete is a fixed-size
  operation (16-byte key, no value). A leaf split produces at most 1 extra
  freed page and requires at most 1 new entry to record — the perturbation
  is bounded and convergence is fast.

The writer checks the reader table (in shared memory) to find the oldest active
reader's TxnID. Any freelist entries with TxnID < oldest_reader are safe to
reclaim.

#### Freelist Runtime Optimizations

Several runtime optimizations reduce freelist B+tree churn and improve
allocation performance. These are inspired by LMDB and libmdbx, adapted to
the composite key design.

##### Loose Pages

Pages that are dirtied (copied via CoW) and then freed within the **same write
transaction** are called "loose pages." This commonly occurs during B+tree
rebalancing: a merge operation may CoW a node, then free one of the two
original nodes. The CoW'd copy becomes unnecessary if its contents are merged
into a sibling.

Loose pages are tracked in a singly-linked list (`tx.loosePages`) using the
page's own memory to store the link pointer (the page is already in memory
since it was dirtied). A counter (`tx.looseCount`) tracks the list length.

Loose pages are **immediately reusable** within the same transaction without
any freelist B+tree interaction:
- `pageAlloc()` checks `tx.loosePages` first (O(1) pop from the linked list).
- Loose pages that are reused via `pageAlloc()` never touch the freelist
  B+tree at all — they were allocated and freed within the same transaction,
  so no reader can ever reference them.
- At commit time, any loose pages still in the list (allocated a page ID but
  never reused) are moved to `tx.pendingFrees` for insertion into the
  freelist B+tree. Their page IDs must be tracked so future transactions
  can reclaim them.

This optimization is critical for the self-referential problem: freelist B+tree
operations that split/merge nodes produce loose pages that are recycled without
further B+tree modifications, preventing cascading changes.

##### Batched Reclamation (Reclaimed Page Cache)

Rather than querying the freelist B+tree on every `pageAlloc()` call, the
writer maintains an in-memory cache of reclaimed pages (`tx.reclaimedPages` —
a sorted slice of page IDs).

The reclamation flow:
1. On the first `pageAlloc()` call that finds no loose pages, the writer scans
   the freelist B+tree collecting all entries where `TxnID < oldestReader`
   into `tx.reclaimedPages`. The scan direction is LIFO (see below).
2. The collected page IDs are sorted in ascending order.
3. Subsequent `pageAlloc()` calls pop pages from `tx.reclaimedPages` (O(1)).
4. For multi-page (contiguous) allocations, `tx.reclaimedPages` is scanned for
   runs of consecutive page IDs.
5. At commit time, any consumed entries not already removed by early GC
   cleanup are deleted from the freelist B+tree.

This turns N individual B+tree lookups into one range scan + one batch delete,
significantly reducing B+tree operations during a write transaction.

##### LIFO Reclamation

When scanning the freelist B+tree for reclaimable pages, the writer scans in
**reverse order** — starting from the newest eligible transaction and working
backward. The scan begins by seeking to the composite key
`(oldestReader - 1, MaxUint64)` and iterating with `Prev()`.

LIFO reclamation reuses recently-freed pages first. This has several benefits:
- **Cache efficiency**: recently freed pages are more likely to still be in the
  OS page cache. Reusing them avoids disk reads for the new transaction's
  writes.
- **Smaller working set**: the set of pages that cycle through
  write/free/reuse stays small, improving both page cache hit rates and
  write-back efficiency.
- **Better for SSD/NVMe**: reduces write amplification by concentrating writes
  on recently-written pages rather than spreading across the entire file.

##### Early GC Cleanup

During reclamation, when the writer reads a freelist entry into
`tx.reclaimedPages`, it immediately deletes that entry from the freelist
B+tree (rather than deferring all deletes to commit time). The B+tree pages
freed by the deletion become loose pages, which are immediately available for
allocation. This reduces commit-time work and makes more pages available
during the transaction.

Early cleanup is performed only when the writer has sufficient "stockpile"
(loose pages + reclaimed pages) to cover the CoW cost of the deletion itself,
preventing the deletion from triggering file extension.

##### Oldest Reader Caching

Scanning the reader table to find the minimum active TxnID is O(MaxReaders).
The writer caches this value (`tx.cachedOldestReader`) and refreshes it
lazily — only when the reclaimed page cache is exhausted and a new scan is
needed. Reading a stale (higher) value is conservative: it delays reclamation
but never causes incorrect behavior.

##### Time-Bounded Reclamation

The freelist B+tree scan in `pageAlloc()` is bounded by both an iteration
limit and a time limit (`Options.GCTimeLimit`, default 0 = unlimited). If the
scan exceeds either bound without finding a suitable page (or contiguous run),
it stops and falls through to file extension. This prevents pathological
latency spikes on large, fragmented freelists. The limit applies only to
multi-page (contiguous) allocation searches — single-page allocation from the
reclaimed cache is always O(1).

#### Page Allocation Priority

`pageAlloc(n)` allocates `n` contiguous pages using this priority:

1. **Loose pages** (n=1 only): pop from `tx.loosePages`. O(1).
2. **Reclaimed page cache**: search `tx.reclaimedPages` for a suitable page
   (n=1) or a contiguous run (n>1).
3. **Freelist B+tree scan**: if the cache is empty or lacks a contiguous run,
   scan the freelist B+tree for reclaimable entries (TxnID < oldest reader),
   populate the cache, and retry step 2. Subject to time/iteration bounds.
4. **Slow reader check**: if reclamation is blocked by a long-lived reader,
   invoke the slow reader callback (see Cross-Process Coordination). If the
   reader releases, refresh the oldest reader cache and retry step 3.
5. **File extension**: if no free pages are available, grow the file according
   to the geometry growth step and advance `FirstUnallocated`.

##### Tail Page Refund

After populating or modifying `tx.reclaimedPages` or `tx.loosePages`, the
writer checks if any pages at the tail of the database file (page IDs equal
to `FirstUnallocated - 1`, `FirstUnallocated - 2`, etc.) are present in these
lists. If so, those pages are removed from the lists and `FirstUnallocated` is
decremented. This reclaims space without going through the freelist and
enables file shrinkage at commit time (see Database Geometry).

The refund process iterates: removing tail pages may expose new tail pages in
the lists. It runs until no more tail pages are found. Loose pages are checked
first (by scanning the linked list), then reclaimed pages (by checking the
sorted slice from the end).

#### Freeing Pages

When a CoW operation replaces an old page with a new copy:
- If the old page was **dirtied in this transaction** (i.e., it was itself a
  CoW copy made earlier in this transaction), it becomes a **loose page** —
  added to `tx.loosePages`.
- If the old page was **from a previous transaction** (an immutable page in the
  mmap), its page ID is added to `tx.pendingFrees` — a list of page IDs to
  insert into the freelist B+tree at commit time, keyed under the current
  TxnID.

#### Commit-Time Freelist Update

During commit, the writer:
1. Moves any remaining loose pages into `tx.pendingFrees`.
2. Inserts all `tx.pendingFrees` entries into the freelist B+tree as
   `(currentTxnID || pageID)` keys with empty values.
3. Deletes any remaining consumed entries from the freelist B+tree that were
   not already removed by early GC cleanup (pages that were in
   `tx.reclaimedPages` and allocated during this transaction).
4. The insertion/deletion loop may itself dirty or free freelist B+tree pages.
   Because each operation has bounded space impact (fixed-size entries, no
   overflow values), the loop converges quickly — typically in 1-2 iterations.

Steps 2-3 happen before the dirty page flush and meta page swap.

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
3. Remove spilled pages from the dirty set and record their page IDs in
   `tx.spilledPages` (a sorted slice).
4. If a spilled page is later accessed again (e.g., a B+tree traversal reaches
   it), it is re-read from the mmap and re-dirtied. The `spilledPages` list is
   checked during page lookup to detect this case.

### Interaction with Freelist

Spilled pages remain allocated — they are not freed or added to the freelist.
They are simply written to their final on-disk location early. At commit time,
spilled pages do not need to be written again (they are already on disk). The
commit only needs to write the remaining (non-spilled) dirty pages and the
meta page.

Freelist B+tree pages are never spilled. They are always kept in memory
because the freelist is modified during commit-time freelist update and
spilling them would cause unnecessary re-dirtying.

**Note**: Page spilling only applies in pwrite mode. In writemap mode, dirty
pages live in the mmap (backed by the OS page cache) and the OS handles
eviction transparently. The `MaxDirtyPages` option and spilling logic are
ignored when `WriteMap` is true.

## Copy-on-Write (CoW) Transaction Model

### Write Transaction

1. Writer acquires the intra-process Go mutex, then the cross-process
   `flock(LOCK_EX)` on the lock file (see Write Lock).
2. Writer reads the active meta page to get current roots, TxnID, and geometry.
3. For each modification (insert, update, delete):
   - Traverse the B+tree from root to leaf.
   - Copy each page along the path (CoW — never modify in place).
   - Allocate new pages via `pageAlloc()` (loose pages → reclaimed cache →
     freelist B+tree scan → slow reader check → file extension).
   - In **pwrite mode**: modified pages are held as heap-allocated dirty pages
     (with LRU counters for spill priority). If the dirty page count exceeds
     `MaxDirtyPages`, spill the coldest dirty pages to disk via `pwrite()`
     (see Page Spilling).
   - In **writemap mode**: modifications happen directly in the mmap. The
     dirty set tracks page IDs only (no heap copies, no spilling).
   - Old pages are tracked: pages from previous transactions go to
     `tx.pendingFrees`; pages dirtied then freed in this transaction become
     loose pages in `tx.loosePages`.
4. Commit-time freelist update:
   a. Perform tail page refund: remove pages at the end of the file from
      `tx.loosePages` and `tx.reclaimedPages`, decrement `FirstUnallocated`.
   b. Move remaining loose pages into `tx.pendingFrees`.
   c. Insert all `tx.pendingFrees` into the freelist B+tree under the current
      TxnID.
   d. Delete any remaining consumed entries (pages allocated from the
      reclaimed cache) not already removed by early GC cleanup.
   e. Repeat c-d if the freelist B+tree operations produced new pending frees
      (convergence loop — typically 1-2 iterations).
5. Flush dirty data pages to stable storage:
   - **pwrite mode**: write all non-spilled dirty pages via `pwrite()`.
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
9. Writer releases the flock, then releases the Go mutex.

### Read Transaction

1. Reader acquires a slot in the reader table (shared memory) and records the
   current TxnID from the active meta page.
2. Reader traverses the B+tree using page pointers from that meta page. Because
   of CoW, all pages referenced by this TxnID are immutable — the writer will
   never modify them in place.
3. When done, the reader clears its slot in the reader table.

Readers never need locks on the data file. They never block writers. Writers
never block readers. The only contention point is the reader table slot
acquisition, which is a simple atomic CAS.

## Cross-Process Coordination

### Lock File Layout

A separate file (`<dbname>.lock`) is mmap'd as shared memory by all processes.

```
Lock File
+------------------------+
| Header                 |
| Magic      | uint32    |  identifies file as gmdb lock file
| Version    | uint16    |  lock file format version
| MaxReaders | uint16    |  number of reader slots
+------------------------+
| Reader Table           |
| +---------+----------+ |
| | TxnID   | PID      | | Slot 0
| | uint64  | uint32   | |
| | Padding | 4 bytes  | |
| +---------+----------+ |
| | TxnID   | PID      | | Slot 1
| | ...                 | |
| +---------+----------+ |
| | ...                 | | up to MaxReaders slots
| +---------+----------+ |
+------------------------+
```

Header size: 8 bytes (aligned). Total lock file size: 8 + (16 * MaxReaders).
With default MaxReaders=126: 8 + 2016 = 2024 bytes (fits in one page).

The lock file is mmap'd with `MAP_SHARED` by all processes for the reader table.
The write lock is a separate concern handled via `flock()` (see below).

### Lock File Lifecycle

The lock file is ephemeral. The first process to open the database creates the
lock file and writes the header (including MaxReaders). Subsequent processes
read MaxReaders from the header and use it. If the lock file is deleted (e.g.,
after all processes exit), the next opener recreates it. MaxReaders is NOT
stored in the data file — it is a runtime coordination property, not a data
property.

### Write Lock

Write serialization uses two layers:

- **Intra-process**: a `sync.Mutex` on the `DB` struct. This prevents two
  goroutines in the same process from attempting concurrent writes.
- **Cross-process**: `flock(LOCK_EX)` on the lock file. This prevents writers
  in different processes.

A `Begin(writable=true)` call first acquires the Go mutex, then acquires the
flock. `Commit()` and `Rollback()` release in reverse order: flock first, then
mutex. This two-layer approach is necessary because `flock()` is per-fd and
per-process — a second goroutine calling `flock()` on the same fd would succeed
immediately (the kernel considers the lock already held by this process).

The `DB` struct holds a single dedicated fd for the write lock (`db.lockFd`),
opened separately from the fd used for the reader table mmap. This fd is used
exclusively for `flock()`/`funlock()` calls.

### Reader Table

- On `BeginRead()`: scan the reader table for a slot with PID == 0. Atomically
  CAS the PID field from 0 to the caller's PID to claim the slot. If the CAS
  fails (another process or goroutine claimed it), try the next slot. Once the
  slot is claimed, store the current meta TxnID into the slot's TxnID field.
- On `EndRead()`: set the slot's TxnID to 0, then set PID to 0. This order
  ensures the writer never sees a stale TxnID in an unclaimed slot.
- Stale reader detection: if a PID in the reader table is no longer alive
  (checked via `kill(pid, 0)` or `/proc/<pid>`), the slot can be reclaimed
  by setting both TxnID and PID to 0.

#### Go Goroutine Model

Go multiplexes goroutines across OS threads, but this does not affect the
reader table design. Each concurrent read transaction — regardless of which
goroutine or OS thread runs it — claims its own slot via atomic CAS. Multiple
slots may share the same PID (same process), which is correct:

- **Slot allocation**: the CAS on the PID field serializes slot claims across
  both goroutines (same process) and external processes.
- **Stale detection**: `kill(pid, 0)` checks process liveness, not thread
  liveness. If a process crashes, all its slots (potentially many) are stale
  and can be reclaimed. This is the desired behavior.
- **Oldest reader scan**: the writer finds the minimum TxnID across all
  occupied slots. Multiple slots from the same process with different TxnIDs
  are handled naturally — the oldest one governs freelist reclamation.

The consequence is that a single Go process running N concurrent read
transactions consumes N reader slots. Applications must set `MaxReaders`
high enough to accommodate the expected total across all processes.

### Writer's Freelist Reclamation

Before reclaiming pages, the writer scans the reader table to find the minimum
active TxnID. Any pages freed by transactions with TxnID < min_active are safe
to reuse.

### Slow Reader Handling

A single long-lived reader prevents all freelist reclamation for transactions
newer than its snapshot, causing unbounded file growth. To address this, the
application can register a `SlowReader` callback via `Options` (see API
Surface) that is invoked when a reader is blocking page allocation.

The callback is invoked from `pageAlloc()` when:
1. The reclaimed page cache is empty.
2. The freelist B+tree has no more reclaimable entries.
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
- CoW still applies: the writer allocates a new page (from freelist or file
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
  (for the freelist and CoW bookkeeping), but the dirty set stores page IDs
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
| Lower bound | `GeoLower` | Minimum file size in pages. File never shrinks below this. | 2 pages (meta pages only) |
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
    // Default: 126. Only used when creating a new lock file.
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

    // GCTimeLimit is the maximum time to spend scanning the freelist
    // for contiguous page runs during multi-page allocation. Zero means
    // unlimited. Default: 0.
    GCTimeLimit time.Duration

    // SlowReader is called when a long-lived reader is blocking freelist
    // reclamation during page allocation. If nil, pageAlloc() falls
    // through to file extension when reclamation is blocked.
    SlowReader func(info SlowReaderInfo) SlowReaderAction

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
    // Default: PageSize * 2 (meta pages only).
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

// SlowReaderInfo describes a reader that is blocking freelist reclamation.
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

// View executes a read-only transaction.
func (db *DB) View(fn func(tx *Tx) error) error

// Update executes a read-write transaction.
func (db *DB) Update(fn func(tx *Tx) error) error

// Begin starts a transaction manually.
func (db *DB) Begin(writable bool) (*Tx, error)

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
    // Freelist
    FreePages    uint64 // total pages in freelist B+tree
    PendingPages uint64 // pages freed but not yet reclaimable (held by readers)

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
    Keyspace string // keyspace name (empty if global/freelist)
    Message  string // human-readable description
}

// Check performs a full structural integrity walk of the database. It opens
// a read transaction, walks all B+trees (keyspace trees, freelist tree),
// and verifies:
//   - Meta page checksum validity
//   - B+tree structural integrity (page reachability, no cycles, key ordering)
//   - Freelist consistency (no overlap with data pages)
//   - Page accounting (all pages accounted for: data + freelist + unallocated)
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
    ReclaimedPages uint64 // pages reclaimed from freelist
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
| `page.go` | Page header encoding/decoding. Branch page: cell directory, key lookup (binary search), insert/split. Leaf page: cell directory, KV lookup, insert/split, overflow references, DUPSORT subpage format. Meta page: encode/decode/validate checksum (including geometry fields). |
| `btree.go` | B+tree search, insert (CoW path from leaf to root, split), delete (CoW, merge/rebalance with configurable `MergeThreshold`). Cursor: stateful iterator holding a stack of (pageID, index) pairs. DUPSORT: subpage management (inline sorted list), nested B+tree promotion/demotion, dup cursor operations. All operations work on page byte slices (from mmap), never Go heap objects. |
| `freelist.go` | Freelist B+tree with composite keys (TxnID \|\| PageID, big-endian, empty values). Page allocation with priority: loose pages → reclaimed cache → B+tree scan (LIFO, time-bounded) → slow reader check → file extension. Batched reclamation with early GC cleanup. Loose page tracking: singly-linked list of intra-transaction recycled pages. Tail page refund for auto-compaction. Commit-time freelist update: insert pending frees, delete consumed entries, convergence loop. |
| `spill.go` | Dirty page spilling to disk mid-transaction (pwrite mode only, no-op in writemap mode). LRU-based priority selection. Spill list tracking for re-dirtying detection. Adjacent page grouping for efficient I/O. |
| `geometry.go` | Database geometry management: grow/shrink bounds, growth step, shrink threshold. File growth via `ftruncate()`. File shrinkage at commit time after tail refund. `Tx.SetGeometry()` for runtime modification. |
| `mmap.go` | Platform-agnostic mmap interface. Initial mapping with over-allocated virtual address space (sized to GeoUpper). Supports both read-only (pwrite mode) and read-write (writemap mode) mappings. |
| `mmap_linux.go` | Linux mmap/munmap/msync syscalls. |
| `mmap_darwin.go` | macOS mmap/munmap/msync syscalls. |
| `lock.go` | Lock file creation and mmap (shared memory). Writer lock (Go mutex intra-process + flock cross-process). Reader table: slot acquire/release, stale PID detection. Oldest-reader query for freelist reclamation. Slow reader detection and callback invocation. |
| `tx.go` | Read transaction: snapshot meta, acquire reader slot, read-only B+tree access. Write transaction: snapshot meta, acquire write lock, track dirty pages (with LRU counters in pwrite mode, page ID set in writemap mode), CoW operations, spill trigger (pwrite only), commit (freelist update + write/flush + meta swap + sync per SyncMode + geometry shrink), rollback. Stats accumulation. |
| `db.go` | Open/Close. Environment setup (mmap, lock file, geometry, write mode selection). Transaction lifecycle (Begin/Commit/Rollback, View/Update helpers). Keyspace management. Sync(). Check(). CopyTo(). |
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
