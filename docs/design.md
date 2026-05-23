# gmdb Design Document

A memory-mapped, multi-process, embedded key-value database for Go.

gmdb targets two concrete consumers: metadata stores for filesystem-like
systems (gitfs replacing SQLite) and document stores for read-heavy
multi-daemon services (notes shared across LLM sessions over MCP). Both
are read-dominated with intermittent small writes from multiple processes,
need atomic cross-keyspace commits, and benefit from declarative
secondary indexes maintained by the engine.

**Minimum Go version: 1.24.** gmdb uses `runtime.AddCleanup` (Go 1.24),
`structs.HostLayout` (Go 1.24), `os.OpenRoot` (Go 1.24), `testing/synctest`
(Go 1.24), `unique.Make` (Go 1.23), `cmp.Or` (Go 1.22), and `iter.Seq2`
(Go 1.23).

## Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Data structure | B+tree on fixed-size pages | Only viable option for multi-process mmap |
| Concurrency | Single writer + N readers (MVCC/CoW) | Proven (LMDB); readers never block writer; cross-process per-keyspace concurrent writers (grove model) incompatible with single shared meta root |
| Mmap mode | `MAP_SHARED \| PROT_READ` for every process (writer included), `mprotect(PROT_READ)` after Open | Read-only mmap eliminates stray-pointer corruption of the data file; one commit path across OSes; no macOS `msync(MS_SYNC)` special case |
| Write path | Pager + slab: read through mmap, copy into slab buffer on first modify, pwrite at commit | Reuses existing Linux/macOS pwrite + fdatasync semantics uniformly; bounded per-txn memory via `MaxTxBufferBytes`; bulk operations bypass the slab via streaming pwrites |
| File layout | Fixed-size pages (4KB–64KB, configurable, immutable after creation) | Matches OS page size, mmap-friendly |
| Page header | 8 bytes (Type uint8, Flags uint8, Count uint16, AdditionalPages uint32 — no PageID) | PageID is redundant (computable from file offset); Type/Flags split reserves 8 flag bits for future per-page metadata at zero cost |
| Value storage | Inline + overflow pages | Simple single read path, overflow for large values |
| Multiple values per key | Set keyspace with subpage + nested B+tree | First-class data primitive for set-shaped data (graph adjacency, postings lists, ZSET-shaped storage). **Not the indexing mechanism** — secondary indexes use composite-key plain keyspaces |
| Secondary indexes | Engine-maintained, composite-key storage, declarative extractor with persisted drift guard | Removes the manual-maintenance bug class without giving up the single-keyspace primitive; schema hash + user version tag catches drift at Open |
| Free space | Allocation bitmap + retired page log (RPL) | O(1) alloc via bitmap, no self-referential allocation, RPL tracks MVCC retirement |
| RPL entry format | Per-segment TxnID + array of PageIDs | TxnID stored once per segment (not per entry); doubles segment capacity |
| File format | Dynamic grow/shrink with configurable bounds; MaxSize immutable after creation | Auto-compaction via tail refund, no manual compaction needed; MaxSize fixed because bitmap region size depends on it |
| Isolation | Dual meta pages + CoW | No WAL needed, atomic commit |
| Crash safety | CoW + atomic meta page swap; lazy bitmap-leak reclamation via background maintenance | Tree is always consistent (CoW); on-disk bitmap leakage bounded by crashed txn's allocations and reclaimed by background maintenance — fast Open after crash |
| Durability | Three sync modes (Durable, DataOnly, Lazy) + unsafe opt-in Unsafe | Configurable ACID vs. performance; SyncUnsafe requires explicit `AllowSyncUnsafe` flag |
| Cross-process | Shared memory lock file (`structs.HostLayout` structs, uint64 PIDs + process start times + PID namespace inodes + heartbeats) | C ABI layout guarantee for mmap'd structs; fixed-size reader table (scan+CAS); stale writer/reader recovery via PID liveness + start time comparison; cross-namespace via heartbeat |
| Write lock | Intra-process writer queue (channel) + single flock goroutine (cross-process) | Context-aware blocking; zero goroutine accumulation on cancellation; flock alone doesn't block same-process goroutines |
| Lock ordering | Documented globally (lifecycle → registry → per-keyspace → commit → bitmap) | Prevents deadlock; mandatory acquisition order for all internal mutex paths |
| Lagging readers | Callback-based notification | Application controls policy; no silent unbounded growth |
| Branch keys | Prefix-truncated separators | Shortest distinguishing prefix; maximizes fan-out; shallower trees; full keys in leaves only |
| Leaf compression | Prefix-compressed restart groups; per-keyspace `RestartGroupTarget` | Density gains for shared-prefix workloads (directory listings, composite keys); per-keyspace tuning lets each keyspace pick its own restart interval |
| Key ordering | Lexicographic (byte-ordered) | Simple, general, no custom comparator needed |
| Byte order | Little-endian (fixed) | Portable across architectures |
| Checksums | Unified xxhash64 footer (8 bytes) on meta and data pages; on by default | One hash family across the file; software-fast (benchmark-favored over CRC32C); defense against silent bitrot on commodity filesystems |
| API | Transaction-based with `context.Context` | Explicit read/write txns; context governs lock acquisition, not txn lifetime; `context.Cause(ctx)` preserves cancellation reasons |
| Iteration | Cursor (stateful, bidirectional, mutable) + `iter.Seq2` (read-only, composable) | Cursor for mutation and bidirectional movement; `iter.Seq2` for idiomatic `for range` loops |
| Bulk insert | `BulkLoad` API (sorted-input bottom-up tree construction; streaming pwrite, bypasses slab) | Fast SQLite→gmdb migration in gitfs, initial import in notes; bounded memory regardless of input size |
| Write batching | Channel-based `Batch()` API with nested transactions | Amortizes commit cost (fdatasync) across concurrent callers in one process; each closure runs in a child transaction — no rollback+retry, closures execute exactly once |
| Nested transactions | Child transactions snapshot pending maps and keyspace roots; CoW-to-fresh-slab-buffer | Rollback drops child's slab buffers and returns page IDs — same simplicity as a hypothetical "CoW to fresh mmap position" model, achieved with the pager/slab |
| Leak detection | `runtime.AddCleanup` on `Tx` and `DB` | Detects leaked transactions (releases reader slots) and leaked DB handles (releases mmap, fds, flock goroutine); logs origin stack trace |
| Range delete | Subtree retirement via `DeleteRange` (per-row walk fallback for indexed keyspaces) | O(pages) on un-indexed keyspaces; O(entries × indexes) on indexed keyspaces (engine must compute prior-index-keys per row before retiring entries) |
| Commit I/O | pwrite (dirty data pages + bitmap pages + meta) + fdatasync | Slab buffers flushed at commit in defined order; one commit path across OSes |
| Prefaulting | `MADV_POPULATE_READ` at open (opt-in, Linux 5.14+) | Eliminates first-access page faults; sequential kernel readahead; silent no-op on older kernels |
| Huge pages | `MADV_HUGEPAGE` (opt-in, Linux) | Reduces TLB pressure for large databases |
| Read txn cooldown | `MADV_COLD` on close (opt-in, Linux 5.4+) | Hints kernel to reclaim page cache after large scans |
| Typed keyspaces | Generic `TypedKeyspace[K, V]` with `Encoder[T]` interface; `TypedIndex[K, V, IK]` follow-on | Zero-cost type-safe API over byte-oriented Keyspace; index extractors as `func(K, V) []IK` |
| Keyspace names | `unique.Handle[string]` interning | Avoids repeated allocations for frequently opened keyspace names across transactions |
| Integrity check | `Check() iter.Seq[CheckIssue]` with `CheckFatal` severity; opt-in `CheckIndexes` mode | Streaming `iter.Seq`; index-content verification is opt-in because it re-runs every extractor (O(rows × indexes)) |
| Byte slice ownership | Borrowed references: values valid until tx close, keys valid until next cursor op | Zero-copy from mmap; prefix compression requires key reconstruction buffer (reused per cursor movement) |
| Nil/empty semantics | Nil/empty keys invalid; nil values treated as empty; nil return = not found or end-of-iteration | Catches bugs at API boundary; empty values enable set-of-keys pattern |
| Block compression | Not supported (explicit decision) | Incompatible with mmap zero-copy read path; key-level prefix compression provides density gains within the mmap model |
| TTL / Expiry | Not supported (explicit decision) | Adds per-cell overhead for a use case (caches, sessions) gmdb doesn't target; users can implement TTL with a separate expiry keyspace |
| Named snapshots | Not supported (explicit decision) | Requires preserving historical meta roots; `CopyTo()` covers the backup use case |
| Merge operators | Not supported (explicit decision) | LSM optimization; B+tree read and write paths traverse the same pages |
| Sequences | `NextSeq uint64` in keyspace descriptor | Per-keyspace auto-incrementing counter |
| Per-keyspace page sizes | Not supported (explicit decision) | Single file with uniform page size is a core design strength |
| Encryption at rest | Not supported (explicit decision) | Mmap conflict; filesystem-level encryption (LUKS, FileVault, dm-crypt) covers the threat model transparently |
| Background maintenance | Periodic goroutine: bitmap leak reclamation, stale reader cleanup, checksum scrubbing, incremental compaction | Avoids accumulating issues that require offline intervention; coordinated across processes via lock file timestamp |

## File Layout

The database is a single file, divided into fixed-size pages. All pages are
the same size (configurable at creation time, immutable after). Supported
page sizes are powers of 2 from 4KB to 64KB. Default: 4096 bytes.

All multi-byte integers are stored in little-endian byte order.

```
+--------+--------+------------------+--------+--------+----
| Meta 0 | Meta 1 | Bitmap Pages ... | Data pages ...       |
| Page 0 | Page 1 | Page 2 .. N      | Page N+1, N+2, ...   |
+--------+--------+------------------+--------+--------+----
```

Bitmap pages occupy a contiguous region starting at page 2. The number of
bitmap pages is determined by `MaxSize` at database creation time:
`BitmapPages = ceil((MaxSize / PageSize) / (PageSize * 8))`. Data pages
(B+tree nodes, overflow pages, RPL segment pages) begin immediately after
the bitmap region. See Allocation Bitmap for details.

### Page Types

Every page starts with a common 8-byte header:

```
Page Header (8 bytes)
+----------+----------+----------+-----------------+
| Type     | Flags    | Count    | AdditionalPages |
| uint8    | uint8    | uint16   | uint32          |
+----------+----------+----------+-----------------+
```

- **Type** (uint8): One of: Branch, Leaf, Overflow, RPLSegment.
  Meta pages and bitmap pages do not carry the page header.
- **Flags** (uint8): Reserved for future per-page flags. Must be zero.
  Readers must reject pages with unknown flags set.
- **Count** (uint16): Number of items.
- **AdditionalPages** (uint32): Number of contiguous overflow pages
  following this one (0 for single-page nodes).

A page's ID is implicit — computable from its file offset
(`offset / PageSize`). This avoids wasting 8 bytes per page on redundant
information and eliminates any possibility of inconsistency between the
stored PageID and the actual file position. `Check()` verifies page type
and structural validity at each offset; no stored PageID is needed.

When page checksums are enabled (the default), every data page (branch,
leaf, overflow, RPL segment) carries an 8-byte xxhash64 footer in the
last 8 bytes of the page. See Checksums for details.

#### Meta Page

Two meta pages occupy pages 0 and 1. The writer always updates the one
NOT currently active. Meta pages do not carry the standard page header.

```
Meta Page
+------------------+
| Magic            | uint32 - identifies file as gmdb
| Version          | uint32 - format version
| PageSize         | uint32 - page size in bytes
| Flags            | uint32 - bit 0: PageChecksum (immutable); bit 1: Checkpoint (mutable); bits 2-31: reserved
| BitmapPages      | uint32 - number of pages in the allocation bitmap
| Padding          | 4 bytes
| UUID             | [16]byte - database identity, generated at creation, immutable
| MinSize          | uint64 - minimum database size in pages
| MaxSize          | uint64 - maximum database size in pages
| GrowStep         | uint64 - growth step in pages
| ShrinkThreshold  | uint64 - shrink threshold in pages
| HighWaterMark    | uint64 - first unallocated page ID
| RPLHeadPage      | uint64 - page ID of the newest RPL segment (0 = empty)
| RPLTailPage      | uint64 - page ID of the oldest RPL segment (0 = empty)
| RPLEntryCount    | uint64 - total entries across all RPL segments
| NumFreePages     | uint64 - total free pages (set bits in bitmap)
| KeyspaceRoot     | uint64 - root page of keyspace B+tree
| NumKeyspaces     | uint64 - number of keyspaces
| TxnID            | uint64 - transaction ID that wrote this meta
| Checksum         | uint64 - xxhash64 of all preceding bytes
+------------------+
```

Total meta page payload: 4×4 + 4 + 4 + 16 + 13×8 = 144 bytes. Fits
comfortably in any supported page size (min 4KB).

`UUID` is a 128-bit random identifier generated at database creation
time and copied identically to both meta pages. Useful for backup
validation and lock file association. Immutable after creation.

`Flags` policy: `Open()` must reject databases where any unknown flag
bit is set. Bit 0 (PageChecksum) is immutable. Bit 1 (Checkpoint) is
mutable — set/cleared per commit depending on whether the commit's data
pages have been confirmed on stable storage.

The file format fields (`MinSize`, `MaxSize`, `GrowStep`,
`ShrinkThreshold`) persist across opens.

The active meta page is the one with the highest TxnID whose checksum
is valid. If a crash happens mid-write to the meta page, the checksum
will be invalid and the database falls back to the other meta page —
which points to the previous consistent state.

#### Branch Page (Internal B+tree Node)

Branch pages store keys and child page pointers. They do NOT store values.

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

Keys are stored in sorted order. For a branch with N cells (N keys), there
are N+1 child pointers: `Ptr[0]` (leftmost, stored after the page header)
plus one `ChildPtr` per cell.

Search algorithm: binary search the cell directory to find the first
separator `Key[i]` where `target < Key[i]`. If found, descend to the
child to the left of that separator — `ChildPtr` of cell `i-1`, or
`Ptr[0]` if `i == 0`. If no separator is greater than the target,
descend to the last cell's `ChildPtr` (rightmost child). When
`target == Key[i]`, the target belongs in the right child since
separators are lower bounds of the right child.

The cell directory stores `(Offset, KeyLen)` per cell, enabling binary
search over variable-length keys without parsing the key data area.

##### Prefix-Truncated Branch Keys

Branch pages store **prefix-truncated separator keys** — the shortest
byte string that distinguishes the left subtree from the right — rather
than full keys copied from leaf pages. A branch separator must satisfy:

- Every key in the left child compares **strictly less than** the separator.
- Every key in the right child compares **greater than or equal to** the separator.

Equivalently: `max(left) < separator <= min(right)`. The separator is a
lower bound of the right child.

For example, if the left child's largest key is `"user:alice:profile"`
and the right child's smallest key is `"user:bob:settings"`, the
separator stored in the branch is `"user:b"` (7 bytes) instead of
the full key (20 bytes).

**Benefits:**
- **Higher fan-out**: smaller keys → more separators per branch page →
  wider tree.
- **Shallower trees**: higher fan-out → fewer levels → fewer page
  accesses per lookup.
- **Reduced I/O**: less data read per branch page traversal.

**Separator computation** at leaf split: let `L` = the last key of the
left leaf, `R` = the first key of the right leaf. Compute the shortest
byte string `S` such that `L < S <= R` — the common prefix of `L` and
`R`, extended by one byte from `R` at the first divergence position:
`S = R[0 : len(commonPrefix) + 1]`. Insert `S` (not `R`) into the
parent branch page.

At merge time, the separator is removed from the parent. At redistribute
time, the separator is recomputed from the new boundary keys and the
parent updated.

**Complementary with leaf prefix compression**: branch pages compress
keys *across* tree levels (the separator is shorter than either boundary
key); leaf pages compress redundancy *within* a page. The two techniques
are independent and complementary.

**Interaction with maximum key size**: the maximum key size limit
applies to full keys stored in leaves (reconstructed from delta
encoding). Branch separators are always shorter than or equal to the
full keys.

#### Leaf Page

Leaf pages store the actual key-value pairs using **prefix compression** —
keys that share common prefixes with their neighbors are stored as deltas.

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
`RestartGroupTarget` in the keyspace descriptor (see Per-Keyspace
Configuration). Stored per page rather than read from the descriptor
so the leaf is self-describing for `Check()` and cursor decode.

Entries are stored in forward memory order starting at a fixed offset
(12) because prefix compression requires sequential scanning. The
restart table is at the end of the page (before the optional xxhash64
footer). The reader locates the restart table at
`contentEnd - RestartCount × 2`.

Entries at positions 0, RestartInterval, 2×RestartInterval, ... are
**restart points** that store full keys. All other entries are **delta
entries** that encode the key as a difference from the previous entry.

Each entry carries a `CellFlags` byte to distinguish cell formats
(inline value, overflow reference, set keyspace subpage, or nested
B+tree reference).

CellFlags bit layout:

```
Bit 0:    Overflow    (0 = inline value, 1 = overflow reference)
Bit 1:    MultiValue  (0 = single value, 1 = multi-value data — subpage or nested B+tree)
Bit 2:    NestedTree  (only when Bit 1 is set: 0 = subpage, 1 = nested B+tree)
Bits 3-7: Reserved (must be 0)
```

`Overflow` and `MultiValue` are mutually exclusive in practice.

**Restart entry** (full key, at positions 0, K, 2K, …):

```
Restart Entry (inline)
+-----------+----------+----------+-----------+-----------+
| CellFlags | KeyLen   | ValueLen | Key bytes | Val bytes |
| uint8     | uint16   | uint32   |           |           |
+-----------+----------+----------+-----------+-----------+
```

**Delta entry**:

```
Delta Entry (inline)
+-----------+-----------+-------------+----------+---------------+-----------+
| CellFlags | SharedLen | UnsharedLen | ValueLen | UnsharedKey   | Val bytes |
| uint8     | uint16    | uint16      | uint32   |               |           |
+-----------+-----------+-------------+----------+---------------+-----------+
```

`SharedLen` = leading bytes shared with the previous entry in the same
restart group. `UnsharedKey` contains only the bytes after the shared
prefix. Full key = first `SharedLen` bytes of previous entry's full key
+ `UnsharedKey`.

Delta entries cost 2 extra bytes per entry but save `SharedLen` bytes of
key data. Net saving per entry is `SharedLen - 2` bytes — positive whenever
keys share more than a 2-byte prefix.

`ValueLen` is uint32 (max ~4GB for inline values, bounded in practice by
leaf page free space). Values exceeding leaf page capacity are stored as
overflow pages, referenced via the overflow format below which uses uint64
`TotalLen`.

**Overflow reference** at a restart point (CellFlags bit 0 set):

```
Restart Overflow Reference
+-----------+----------+-----------+----------+----------+
| CellFlags | KeyLen   | Key bytes | OvflPage | TotalLen |
| uint8     | uint16   |           | uint64   | uint64   |
+-----------+----------+-----------+----------+----------+
```

**Overflow reference** at a delta position:

```
Delta Overflow Reference
+-----------+-----------+-------------+---------------+----------+----------+
| CellFlags | SharedLen | UnsharedLen | UnsharedKey   | OvflPage | TotalLen |
| uint8     | uint16    | uint16      |               | uint64   | uint64   |
+-----------+-----------+-------------+---------------+----------+----------+
```

##### Leaf Lookup

Two-phase binary search:

1. **Binary search over restart points** using the restart table. O(log R)
   where R = RestartCount.
2. **Linear scan within the restart group**, decoding delta entries until
   the target is found or passed. O(K) where K = RestartInterval.

Total: O(log(n/K) + K). For a leaf with 30 entries at K=16, ~17 comparisons.
The linear scan operates on data already in L1 cache.

##### Leaf Density

Depends on the ratio of shared prefix length to total key length.
200-byte keys sharing a 150-byte common prefix + 50-byte values at 4KB:

| Format | Bytes/entry (avg) | Entries/page | Improvement |
|--------|-------------------|-------------|-------------|
| Full keys | ~260 | ~15 | baseline |
| Prefix compressed (K=16) | ~117 | ~33 | 2.2x |

Short low-prefix keys see ~5% improvement. Compression adapts
automatically — high-prefix workloads benefit; random keys pay 2
bytes/entry overhead.

##### Insert and Delete

Inserting a key between two delta entries within a restart group:

1. Encode the new entry as a delta against its predecessor.
2. Recompute the successor entry's delta (its `SharedLen` is now
   relative to the new entry).
3. If insertion shifts entry indices, restart point positions may
   change — re-encode the affected group boundaries.

Deletion is symmetric. The restart table is rebuilt after any insert
or delete — O(RestartCount), at most ~20 entries for a full leaf page.

**Implementation guidance: compressed-leaf splice.** Hot-path insert
and delete should splice the page in place rather than full
decode+re-encode of all entries (the `tryInsertAtCompressed` /
`tryDeleteAtCompressed` pattern from grove). Decoded-form fallback only
when the splice cannot determine the layout impact locally (group
boundary crossing).

##### Leaf Split

On overflow, the leaf is split into two halves. Each half is re-encoded
independently with fresh restart points starting at index 0. Boundary
keys (last key of left leaf, first key of right leaf) are full keys
reconstructed from the delta encoding. Separator computation for the
parent branch uses these full keys.

##### Cursor Key Reconstruction

The cursor maintains a **key reconstruction buffer** (`cursor.keyBuf
[]byte`) holding the full key at the current position. On forward
movement (`Next()`), truncate to `SharedLen` and append `UnsharedKey`.
O(1) amortized.

For reverse movement (`Prev()`), delta entries encode forward only.
The cursor caches all decoded keys for the current restart group
(**group cache**, `[K][]byte` array). When the cursor first enters a
group, all K entries are decoded into the cache; subsequent `Prev()`
within the group reads from cache in O(1). At K=16 and max key size
~2KB, worst-case ~32KB per cursor — acceptable.

#### Overflow Page

Overflow pages are contiguous runs that store large values. The first
page in the run carries the standard 8-byte page header with
`AdditionalPages` set to the number of follower pages; the remaining
bytes are value data. Follower pages carry no header — they are
entirely value data (minus 8 bytes for the xxhash64 footer when page
checksums are enabled). Total value capacity for a run of `1 + N`
pages: `(PageSize - 8) + N * PageSize` bytes (subtract 8 per page for
the footer when enabled).

When checksums are enabled, each page in the run carries its own
independent xxhash64 footer. The first page checksums its header +
data; each follower checksums its data. Per-page footers allow
identifying which specific page is corrupted.

#### Set Keyspace Storage (Multiple Values Per Key)

Set keyspaces allow multiple sorted values per key. Each key maps to a
sorted set of values. This is a **general-purpose data primitive** for
set-shaped data: graph adjacency lists, inverted-index postings lists,
many-to-many relationships, pub/sub subscription registries,
Redis-ZSET-shaped storage (score-prefixed members), and audit logs per
entity.

**SetKeyspace is not the secondary-index mechanism.** Secondary indexes
use the dedicated indexing subsystem, which stores composite-key entries
in a plain keyspace internally (see Indexing). Set keyspaces are
exposed for data models whose natural shape is "key → set of values."

##### Storage Strategy

Two storage strategies based on value-set size:

**Subpage (small value sets):** values fit within the leaf cell, stored
inline as a mini sorted list. No extra page allocation.

**Nested B+tree (large value sets):** promoted to a full B+tree whose
root page ID is stored in the leaf cell. Each value becomes a key in
the nested B+tree (with empty values).

##### Subpage Format

A subpage is stored in the leaf entry's value area. `CellFlags.MultiValue`
is set and `CellFlags.NestedTree` is clear. The entry uses the standard
restart/delta key encoding; the subpage replaces the value portion.

```
SetKeyspace Subpage Entry (restart)
+-----------+----------+-----------+-----------+
| CellFlags | KeyLen   | Key bytes | Subpage   |
| uint8     | uint16   |           |           |
+-----------+----------+-----------+-----------+

Subpage (embedded in cell value area):
+----------+----------+---------+---------+-----+
| Count    | DataSize | Entry 0 | Entry 1 | ... |
| uint16   | uint16   |         |         |     |
+----------+----------+---------+---------+-----+
```

For **variable-size values**:
```
Entry (variable):
+----------+-----------+
| ValueLen | Val bytes |
| uint16   |           |
+----------+-----------+
```

For **fixed-size values** (keyspace declared with `FixedValueSize`):
```
Entry (fixed):
+-----------+
| Val bytes |  (size = keyspace's fixed value size, no length prefix)
+-----------+
```

`Count` is the number of entries. `DataSize` is the total byte size of
all entries.

Values within the subpage are stored in sorted (lexicographic) order.
Lookup is binary search. For fixed-size subpages, entries are a flat
array — binary search is O(log N) with direct offset calculation.

Subpage entries are **not prefix-compressed**. Subpages store *values*
for a single key, which typically don't share prefixes by construction
(e.g., post IDs in a postings list). The subpage is also small by
definition (below the 50% promotion threshold).

##### Subpage Promotion Threshold

A subpage is promoted to a nested B+tree when inserting a new value
would cause the subpage to exceed **50% of the leaf page's usable
space** (PageSize minus header, restart metadata, restart table, and
optional checksum footer).

Promotion:
1. Allocate a new leaf page for the nested B+tree.
2. Copy all subpage entries into the new leaf page as regular cells
   (where "keys" are the values from the set and "values" are empty).
3. Replace the subpage cell with a nested B+tree reference cell.
4. Insert the new value into the nested B+tree.

##### Nested B+tree Reference Cell

```
SetKeyspace Nested B+tree Entry (restart)
+-----------+----------+-----------+----------+----------+
| CellFlags | KeyLen   | Key bytes | Root     | Count    |
| uint8     | uint16   |           | uint64   | uint64   |
+-----------+----------+-----------+----------+----------+
```

- **Root**: Page ID of the nested B+tree's root.
- **Count**: Number of values in the set (O(1) access).

Depth is not persisted — derived by reading the root page on first
access. The nested B+tree uses the same B+tree implementation as the
main keyspace; its "keys" are the values from the set, all "values" are
empty (zero-length). Nested-tree leaves use prefix compression like all
other leaves.

##### Demotion

When deletions reduce a nested B+tree to a single leaf page that would
fit as a subpage, the B+tree is demoted back to a subpage. The leaf
page is freed; entries are packed inline into the parent leaf cell.

When the last value for a key is deleted, the key's cell is removed
from the parent leaf entirely — empty nested trees and empty subpages
never exist, not even transiently within a write transaction.

##### Fixed-Size Value Sets

When a set keyspace is created with `FixedValueSize`, all values must
be exactly that byte size. Enables:
- No per-value length prefix in subpages (flat array).
- Direct offset binary search (`entry[i]` at `i * valueSize`).
- Compact nested B+tree leaves (no `ValueLen` field per cell).

A `Put()` with a value of the wrong size returns `ErrValueSizeMismatch`.

### Range Delete

`Keyspace.DeleteRange(start, end)` deletes all keys in `[start, end)` in
a single operation. **For un-indexed keyspaces**, this is significantly
more efficient than cursor iteration because it retires entire subtrees
without visiting individual leaves. **For indexed keyspaces**, the
engine must compute prior-index-keys per row before retiring, so the
operation falls back to a per-row walk — see Bulk Operations on Indexed
Keyspaces in the Indexing section.

#### Algorithm (un-indexed keyspaces)

Three phases:

**Phase 1: Find boundary paths.** Descend the B+tree twice to find the
left and right boundary paths (path = stack of `(pageID, index)` pairs).

**Phase 2: Identify and retire interior subtrees.** Walk up from the
two boundary paths to find their lowest common ancestor (LCA). At each
level between LCA and leaves:
- **Interior children** (between left and right boundaries) are
  entirely within the range — their entire subtrees are retired
  without visiting individual leaves.
- **Boundary children** are partially within the range and must be
  descended.

Retiring a subtree: walk the branch pages recursively. For each page
encountered, add its page ID to `tx.retiredPages`. For leaf pages,
accumulate the entry count. For overflow pages referenced by leaf
cells, retire the entire overflow run. The walk visits every page in
the subtree exactly once — O(pages in subtree).

**Phase 3: Clean up boundary leaves and rebalance.**

- In the left boundary leaf: delete entries from `start` through end of leaf.
- In the right boundary leaf: delete entries from start through last key before `end`.
- If both boundaries are in the same leaf, delete entries between them.
- Retire any overflow pages referenced by deleted entries.
- Walk up from boundary leaves to LCA, removing the retired interior
  child pointers from each branch (CoW each branch).
- Rebalance: check fill ratios on modified branches and leaves. Merge
  or redistribute per `MergeThreshold`.
- **Root collapse.** If rebalance reduces the keyspace's root to a
  single child (a branch with one child pointer and no separators),
  retire the root and promote the surviving child to the new root —
  update the keyspace descriptor's `Root` field. If `DeleteRange`
  emptied the keyspace entirely, retire the root and set `Root = 0`
  (empty keyspace). The descriptor update is part of the same write
  transaction and propagates up through the keyspace B+tree via CoW.

#### Complexity

| Operation | Naive (cursor loop) | Range delete |
|-----------|-------------------|--------------|
| Delete N keys spanning P pages | O(N × depth) | O(P + depth²) |
| CoW'd pages | O(N × depth) | O(depth²) |
| Retired pages | N leaf cells + splits | P pages (bulk) + boundary cleanup |

For 1M keys across 10K leaves at depth 4: naive ~4M CoWs; range delete
walks ~10K pages + ~16 CoWs on boundary paths.

#### Set Keyspace Bulk Free

Deleting a key in a set keyspace whose values are in a nested B+tree
frees the nested tree via the same subtree retirement: read root + count
from the cell, walk the nested tree recursively retiring every page,
remove the cell. O(pages in nested tree), not O(values).

#### Cursor-Based Range Delete

For callers needing finer control:

```go
c := ks.Cursor()
for k, _ := c.SeekGE(start); k != nil && bytes.Compare(k, end) < 0; k, _ = c.Next() {
    c.Delete()
}
```

One-at-a-time path. `DeleteRange` should be preferred for contiguous
unconditional deletes.

### Free Space Management

Free space is managed by two on-disk structures separating the two
concerns: **what is free** (the allocation bitmap) and **when it became
free** (the retired page log). This separation eliminates the
self-referential allocation problem found in freelist-B+tree designs,
where modifying the freelist during commit could itself allocate or
free pages.

#### Allocation Bitmap

A flat bitfield — one bit per page in the database. Set bit = free and
safe to allocate. Clear bit = in use OR retired but not yet reclaimable
(still visible to an active reader's snapshot).

Stored in a contiguous region starting at page 2. Number of bitmap
pages fixed at creation:

```
BitmapPages = ceil(MaxSize / PageSize / BitsPerPage)
BitsPerPage = PageSize * 8
```

| MaxSize | PageSize | Total Pages | BitmapPages | Bitmap Size |
|----------|----------|-------------|-------------|-------------|
| 1GB      | 4KB      | 262,144     | 8           | 32KB        |
| 64GB     | 4KB      | 16,777,216  | 512         | 2MB         |
| 256GB    | 4KB      | 67,108,864  | 2,048       | 8MB         |
| 1TB      | 4KB      | 268,435,456 | 8,192       | 32MB        |

Bitmap pages are never marked free in the bitmap (bits permanently
clear). Same for meta pages. Data pages start at `2 + BitmapPages`.

##### Bitmap Storage

The bitmap is stored in the data file and accessed via the mmap (read
path) and pwrite (write path). Bitmap modifications are deferred in
memory: `tx.pendingAllocs` tracks pages allocated during the
transaction (bits to clear at commit), `tx.pendingFrees` tracks pages
freed (bits to set at commit). At commit, modified bitmap pages are
pwritten before the meta page. Bitmap pages on disk are only ever
modified via ordered pwrite + fdatasync — never via the mmap (which is
read-only across all processes).

Bitmap pages do not use the standard page header. The entire page is
usable as bitmap data. The page type is identified by position in the
file (pages 2 through `2 + BitmapPages - 1`).

##### Two-Level Summary

The bitmap uses a two-level structure for fast allocation searches:

- **Level 0 (detail):** one bit per page in the database, across bitmap
  pages 2 through `2 + BitmapPages - 1`.
- **Level 1 (summary):** in-memory `[]uint64`, one bit per uint64 word
  of the detail level. Set if the corresponding 64-page word has any
  set bits. Size: `ceil(TotalPages / 64 / 64)` uint64 words. Rebuilt
  from detail at Open and maintained incrementally.

At 4KB pages with 256GB MaxSize (67M pages): detail is ~1M uint64 words
(8MB); summary is ~16K uint64 words (128KB in memory). The summary
allows skipping 64-page regions during allocation scans.

For contiguous-run searches (overflow allocation), the writer scans
summary words for regions with free pages, then scans detail words
within using `math/bits.TrailingZeros64` / `LeadingZeros64`. A single
uint64 word covers 64 pages — a run of N < 64 can be found within one
word; larger runs span word boundaries with a carry-forward scan.

##### Bitmap Operations

**Set bit (free a page):** load uint64 word, OR in the bit, write back.
Update summary if word transitioned 0 → non-zero. O(1).

**Clear bit (allocate a page):** load word, AND out bit, write back.
Update summary if word transitioned non-zero → 0. O(1).

**Find first free (single-page alloc):** scan summary from the LIFO
hint for a non-zero word, then scan detail words within. Clear and
return. O(1) amortized with hint; O(TotalPages/64) worst case.

**Find N contiguous free:** scan detail words for runs of consecutive
set bits. `math/bits.TrailingZeros64` on the complement finds run
length from LSB. Across word boundaries, track trailing run of one
word + leading run of next. O(scanned words).

**Count free pages:** `math/bits.OnesCount64` (hardware `popcnt`)
across all detail words. Cached in `NumFreePages` in the meta page.

#### Retired Page Log (RPL)

The RPL tracks which pages were freed by which transaction — needed for
MVCC safety: a page freed by transaction T cannot move into the
allocation bitmap until no active reader holds a snapshot ≤ T.

Append-only singly-linked list of segment pages. Each segment stores a
single TxnID + an array of PageIDs. Each commit creates new segment
pages — existing segments are never modified. All entries in a segment
share the same TxnID — storing it once doubles capacity:

```
RPL Segment Page
+--------------------------+
| Page Header (8 bytes)    |
+--------------------------+
| TxnID          | uint64  |  transaction that retired these pages
| OlderSegment   | uint64  |  page ID of the next older segment (0 = tail)
| EntryCount     | uint16  |  number of PageID entries
| Padding        | 6 bytes |
+--------------------------+
| PageID 0       | uint64  |
| PageID 1       | uint64  |
| ...                      |
+--------------------------+
```

Segment capacity at 4KB: 8 (header) + 8 (TxnID) + 8 (link) + 2
(EntryCount) + 6 (padding) = 32 bytes overhead. Remaining `4096 - 32
= 4064` / 8 = **508 entries per segment** (507 with checksums enabled,
due to the 8-byte xxhash64 footer: `4096 - 32 - 8 = 4056` / 8 = 507).

Meta stores `RPLHeadPage` (newest) and `RPLTailPage` (oldest). Segments
are singly linked head → tail via `OlderSegment`. Tail-toward-head
direction is maintained as an in-memory segment list rebuilt at Open.

##### RPL Append (At Commit Time)

When a write transaction commits with retired pages:

1. Allocate one or more new segment pages from the bitmap (or file
   extension). Each commit creates **new** segment pages — existing
   segments are never modified (they belong to previous snapshots).
2. Fill segments with current TxnID in the segment header and PageID
   entries sorted by page ID. If retired list exceeds one segment's
   `EntriesPerSegment` capacity (508 at 4KB without checksums; 507
   with the xxhash64 footer), allocate additional segments linked
   via `OlderSegment`.
3. Set the new head's `OlderSegment` to the old `RPLHeadPage`.
4. Update `RPLHeadPage` (and `RPLTailPage` if RPL was empty).
5. Append the new segment page ID(s) to the in-memory segment list.

A transaction retiring N pages needs at most
`ceil(N / EntriesPerSegment)` segment allocations
(`EntriesPerSegment` = 508 at 4KB without checksums, 507 with).
Bounded, non-recursive.

##### RPL Reclamation

At the start of a write transaction (or lazily on first `pageAlloc()`),
the writer reclaims RPL entries safe to reuse:

1. Compute the **reclamation bound**: minimum of (oldest active reader's
   TxnID, last checkpoint's TxnID). In `SyncDurable`/`SyncDataOnly`,
   every commit is a checkpoint. In `SyncLazy`, the checkpoint TxnID
   may be older than current, restricting reclamation.
2. Walk the in-memory segment list from **tail** (oldest first).
3. For each segment where `TxnID < reclaimBound`: set bitmap bits for
   all PageIDs in the segment.
4. When a segment is fully reclaimed, free the segment page itself,
   remove it from the in-memory list, advance `RPLTailPage`.
5. Update `RPLEntryCount` and `NumFreePages`.

The checkpoint bound prevents a crash-recovery scenario where the
on-disk bitmap reflects a newer transaction's reclamation but recovery
selects an older checkpoint meta. Reclamation only processes pages
freed by transactions older than the last checkpoint — pages
unreachable from any recoverable tree state.

**Why the bound is sufficient.** Suppose the last checkpoint is at
TxnID `C`. Reclamation at any later TxnID `T > C` uses
`reclaimBound = min(oldestReader, C) ≤ C`, so it can only set bitmap
bits for pages freed by transactions with `TxnID < C`. Those pages
were freed *before* the checkpoint, so the checkpoint's tree does
not reference them. If a crash forces recovery to fall back to the
checkpoint meta `C`, the on-disk bitmap may show those pages as free
(reclaimed between `C` and the crash) or as not-yet-reclaimed (still
in the RPL at `C`) — either is consistent with `C`'s tree, because
`C`'s tree never referenced them in the first place. Pages freed
*after* `C` (`TxnID ≥ C`) are excluded by the bound, so the bitmap
never gains spurious free-bits for pages the recovered tree might
still reference.

**SyncLazy and partial bitmap flush.** In `SyncLazy` mode the
bitmap-update pwrites for intermediate (post-checkpoint) transactions
happen without `fdatasync`. The OS may flush some of those bitmap
pwrites before a crash and not others. On recovery to checkpoint `C`,
the on-disk bitmap can therefore be in a partial state: some pages
freed by transactions in `(C, crash]` may have their bits set on
disk; others may not. The argument above guarantees the *tree* is
safe (no tree reference points into pages the bitmap is wrong about),
but the meta's `NumFreePages` counter (last written at `C`) may
disagree with the actual bit-count of the on-disk bitmap.

This is handled by background maintenance's bitmap-leak reclamation
pass: it walks the tree from `C`'s roots, identifies pages that are
allocated-but-unreferenced (the partial-flush leaks), and either
sets their bits free (if they're truly unreferenced) or recomputes
`NumFreePages` from the current bitmap. `Check()` does not trust
`NumFreePages` — it recomputes the free count directly from the
bitmap bits via `math/bits.OnesCount64` and reports any discrepancy
with the meta as a `CheckWarning`.

Reclamation is oldest-first so the RPL shrinks from the tail. Empty
segments are immediately freed.

Reclamation consumes **whole segments** — clean boundary since each
segment has a single TxnID. Avoids partial segment modification.

##### Oldest Reader Caching

Scanning the reader table is O(MaxReaders). The writer caches
(`tx.cachedOldestReader`) and combines with last checkpoint TxnID to
form the reclamation bound. Refreshed lazily — only when the bitmap
has no free pages and reclamation might unlock more. Reading a stale
(higher) value is conservative — delays reclamation but never
incorrect.

##### RPL Segment List

On-disk RPL is singly linked head → tail. Reclamation walks tail → head.
To avoid full chain traversal, the writer maintains an in-memory list
— a `[]uint64` of segment page IDs ordered tail (index 0) to head (last).

Rebuilt at `Open()` by walking the on-disk chain head → tail then
reversing. O(RPL segments) — typically tens to low hundreds.

Maintained incrementally:
- **Append** (commit with retired pages): new segment IDs appended.
- **Reclaim** (tail consumption): consumed segment IDs removed from
  the front.

Stored on the `DB` struct, protected by the write lock.

#### LIFO Allocation Locality

The bitmap doesn't inherently provide LIFO. The writer maintains a
**LIFO hint** (`tx.allocHint`) — the page ID of the last page
reclaimed during the most recent reclamation pass. `pageAlloc()`
begins its scan at this hint. Reclamation walks RPL oldest → newest,
so the last entries processed are most-recently-freed pages — the hint
naturally points to recently-freed regions.

For workloads with steady write/free/reuse cycles, this keeps the
active page set small and concentrated.

#### Loose Pages

Pages CoW'd and then freed within the **same write transaction**.
Common during rebalancing: a merge CoWs a node then frees one of two
originals.

Tracked in a hash map (`tx.loosePages map[uint64]struct{}`) for O(1)
insert/lookup/delete. The hash map is required because **tail page
refund** does up to n membership lookups by page ID against n loose
pages — O(n²) with a slice vs. O(n) with a map.

Loose pages are immediately reusable within the same transaction:
- `pageAlloc()` checks `tx.loosePages` first (single-page allocs).
- Loose pages reused via `pageAlloc()` never touch the bitmap or RPL.
- At commit time, any loose pages still in the map are added to
  `tx.pendingFrees` (bypass RPL — same-txn pages cannot be referenced
  by any reader).

#### Page Allocation Priority

`pageAlloc(n)` allocates `n` contiguous pages in priority order:

1. **Loose pages** (n=1 only): pop from `tx.loosePages`. O(1).
2. **Allocation bitmap**: scan for free page (n=1) or contiguous run
   (n>1) starting at LIFO hint.
3. **RPL reclamation**: if bitmap exhausted, reclaim entries with
   TxnID < oldest reader.
4. **Lagging reader check**: if reclamation blocked by long-lived
   reader, invoke `LaggingReader` callback.
5. **File extension**: if no free pages, grow per file format and
   advance `HighWaterMark`.

##### Tail Page Refund

After reclamation or at commit time, the writer checks if any tail
pages (`HighWaterMark - 1`, `HighWaterMark - 2`, …) are free in the
bitmap. If so, bits cleared and `HighWaterMark` decremented. Reclaims
file space and enables file shrinkage at commit. Iterates until no
more tail pages free.

**Safety with concurrent readers.** Tail refund only decrements
HighWaterMark for pages free in the bitmap. Pages held by an active
reader remain in the RPL until the reclamation bound allows their
reclamation. Tail refund cannot remove pages a reader references.
File shrinkage via `ftruncate()` only truncates pages beyond
HighWaterMark.

#### Freeing Pages

When a CoW operation replaces an old page with a new copy:
- If the old page was **CoW'd in this transaction** (already a CoW
  copy from earlier in this txn), it becomes a **loose page** —
  added to `tx.loosePages`.
- If the old page was **from a previous transaction** (an immutable
  page accessible via mmap), its page ID is added to `tx.retiredPages`
  — a list of page IDs appended to the RPL at commit time (TxnID
  stored once per RPL segment).

Retired pages are NOT immediately marked free. They enter the RPL and
move to the bitmap only when reclamation deems them safe (no active
reader holds their snapshot).

#### Commit-Time Free Space Update

The free-space side of commit happens entirely inside step 0 of
Commit Write Ordering (pre-pwrite assembly). The work is:

1. Tail page refund: check bitmap for tail free pages, decrement
   `HighWaterMark`.
2. Move remaining loose pages into `tx.pendingFrees` (bypass RPL).
3. Append all `tx.retiredPages` to the RPL by allocating new segment
   pages and appending to the in-memory segment list. The newly
   allocated segment pages enter `p.dirty` and are flushed by Commit
   Write Ordering step 1 alongside data and bitmap pages.
4. Update `NumFreePages`, `RPLHeadPage`, `RPLTailPage`,
   `RPLEntryCount` in the new meta-page buffer.

Sub-step 3 may allocate RPL segments from the bitmap — bounded,
non-recursive. Each segment holds `EntriesPerSegment` entries
(508 at 4KB pages without checksums, 507 with the xxhash64 footer),
so a transaction retiring N pages needs `ceil(N / EntriesPerSegment)`
allocations.

If the bitmap has no free pages and file extension would exceed
MaxSize, RPL segment allocation fails and commit returns
`ErrTxTooLarge` or `ErrDBFull` from step 0 — no pwrite has happened,
so rollback is clean.

## Pager and Slab Architecture

The data file is memory-mapped read-only by every process, including
the writer. All page modifications go through a **pager** that owns a
**slab** of page-sized buffers; modifications happen in slab buffers
and are pwritten to disk at commit time.

This is a deliberate move away from a direct-write mmap design:

- **Portability.** One commit path on all OSes — no macOS-specific
  `msync(MS_SYNC)` round trip, no platform-conditional code in the
  commit hot path.
- **Defense in depth.** A stray pointer or `unsafe` misuse anywhere in
  the host process produces SIGSEGV instead of silently corrupting the
  data file (the data mmap is `PROT_READ`-only, enforced by
  `mprotect` after Open).
- **Crash semantics.** On-disk state is exactly what has been
  pwritten and fsynced. There is no "kernel may have flushed dirty
  CoW mmap pages at arbitrary times" failure window; bitmap leakage
  between bitmap-pwrite and meta-pwrite remains the only crash failure
  mode and is bounded by the crashed transaction's allocations.
- **Read coherence across pwrite and mmap.** Both Linux and macOS
  use a unified page cache for `MAP_SHARED` + `pwrite` on the same
  file: a `pwrite()` updates the page cache directly, and a
  subsequent read through the `MAP_SHARED` mapping (regardless of
  `PROT_READ`-only vs `PROT_READ | PROT_WRITE`) returns the new
  contents. `msync(MS_SYNC)` is only required when *writes go through
  the mmap pointer itself* — gmdb's writer never does this, so no
  `msync` call is needed on any supported OS.

### Pager Roles

A single `Pager` type per transaction handles both reads and writes.
Read transactions get a read-only pager that only resolves pages from
mmap. Write transactions get a writable pager that maintains the dirty
slab.

```
type Pager struct {
    mmap        []byte                // read-only view of the data file
    pageSize    int
    dirty       map[uint64]*[]byte    // page ID → slab buffer (write txn only)
    dirtyBytes  int                   // current slab usage in bytes
    maxBytes    int                   // Options.MaxTxBufferBytes
    bufPool     *sync.Pool            // page-sized scratch buffers
    readOnly    bool
}
```

### Page Resolution

`pager.Page(id) []byte` returns a borrowed byte slice for the page at
`id`:

```
if buf, ok := p.dirty[id]; ok {
    return *buf            // writer's own dirty page
}
return p.mmap[id*p.pageSize : (id+1)*p.pageSize]    // read from mmap
```

Branches: one. No layered buffer cache, no eviction policy. The OS page
cache handles everything except the writer's own in-flight changes.

### CoW via the Slab

When the writer modifies a page from a prior transaction:

1. Allocate a fresh page ID via `pageAlloc()`.
2. Acquire a page-sized buffer from `bufPool`.
3. Copy current page content (from `pager.Page(oldID)` — which may be
   the mmap or a same-txn dirty buffer) into the slab buffer.
4. Insert the buffer into `p.dirty[newID]`.
5. `dirtyBytes += pageSize`. If `dirtyBytes > maxBytes`, return
   `ErrTxTooLarge` — the caller must roll back.
6. Track the old page ID for retirement (`tx.retiredPages` or
   `tx.loosePages` depending on origin).
7. Track the new page ID in `tx.pendingAllocs` and `tx.cowPages`.
8. Mutate the slab buffer in place.

When the writer modifies a page already CoW'd in this transaction (same
page ID, already in `p.dirty`), it mutates the existing slab buffer in
place — no new allocation. `tx.cowPages` records this so the writer
knows the page is already CoW'd.

### Slab Budget and `ErrTxTooLarge`

`Options.MaxTxBufferBytes` (default: 256 MiB) bounds the slab. A write
transaction that dirties more pages than the budget allows fails the
next CoW with `ErrTxTooLarge`. The transaction must be rolled back;
the caller chunks the work into smaller transactions.

The slab budget covers every page-sized buffer the transaction has
allocated — live (currently routed via `dirty[id]`), loose (CoW'd
then freed within the same tx, retained for the byte-slice ownership
contract — see Byte Slice Ownership), and any buffers held by the
commit machinery (RPL segment pages and modified bitmap pages, both
assembled in step 0 of Commit Write Ordering before any pwrite).
`ErrTxTooLarge` can therefore also fire at commit time when the
step-0 assembly pushes the slab over budget — detected before any
pwrite begins, so rollback is clean (no disk writes have occurred).

**Cost-model note for callers.** Because slab buffers stay alive
until commit/rollback (the loose-page retention rule above), a
transaction that CoWs the *same logical page* multiple times
accumulates one buffer per CoW. The budget should be sized relative
to the transaction's expected count of *unique CoW destinations*
(typically: leaves touched × tree depth, plus index entries × indexed
columns), not the operation count.

Bulk operations have a dedicated escape hatch — see BulkLoad.

The slab itself is backed by anonymous mmap pages obtained via
`sync.Pool` of `[]byte` slices. Returning a buffer to the pool clears it
(zero-fill) and makes it available for reuse. Cross-transaction reuse
keeps allocator pressure low for steady write loads.

Buffers are **not** returned to the pool when a page becomes loose
within the transaction — only at commit or rollback. This preserves
the byte-slice ownership contract (a borrowed `[]byte` pointing into
a loose-page buffer remains valid for the full transaction). The
buffer pool is shared process-wide via `sync.Pool`; cross-process
slab usage is not visible from any one DB handle.

### Commit Write Ordering

The commit path is partitioned into a **pure-buffer assembly phase**
(no syscalls, fully reversible) followed by a **pwrite phase** whose
failures are bounded to crash-equivalent leakage. The split keeps the
"abort is clean" guarantee unambiguous.

0. **Pre-pwrite assembly (no syscalls, no pwrite).**
   - Tail page refund: check the bitmap for tail free pages, decrement
     `HighWaterMark`.
   - Move remaining loose pages into `tx.pendingFrees`.
   - Allocate any required RPL segment pages and fill them with
     retired-page entries. Insert the new segment pages into
     `p.dirty` (counts against `MaxTxBufferBytes`).
   - For each modified bitmap page (derived from `tx.pendingAllocs` ∪
     `tx.pendingFrees`), read the current bitmap page from the mmap,
     apply the pending bit changes into a freshly allocated slab
     buffer, and insert the buffer into `p.dirty` keyed by its bitmap
     page ID (counts against `MaxTxBufferBytes`).
   - Construct the new meta page payload (new roots, new TxnID,
     updated `HighWaterMark`, recomputed xxhash64 checksum) into a
     fresh buffer held on the transaction (not in `p.dirty`; the meta
     page lives at a fixed slot and is pwritten in step 3).
   - Newly-allocated RPL segment page IDs are also appended to the
     in-memory segment list (see RPL Segment List) as part of
     sub-step 3 — keeps the in-memory view consistent with what
     step 1 will pwrite.
   - Any failure in step 0 (slab budget exceeded, RPL alloc cannot
     find capacity, file extension would exceed `MaxSize`) returns
     `ErrTxTooLarge` or `ErrDBFull` and is **fully reversible** — no
     pwrite has been issued, so rollback releases buffers and leaves
     the on-disk state untouched. Rollback's clearing of
     `tx.pendingAllocs` is what unwinds any RPL segment page
     allocations made during step 0: those page IDs were drawn from
     the bitmap's pending-allocs set but never had their on-disk
     bits cleared (bitmap pwrite is step 1), so dropping the
     pendingAllocs entries makes the IDs immediately re-available
     to the next transaction's allocator. The in-memory segment
     list appends from sub-step 3 are also rolled back at this point.

1. **Data + RPL + bitmap pwrite.** For each `(pageID, buf)` in
   `p.dirty` — which now contains data pages, RPL segment pages, and
   modified bitmap pages all together — compute the xxhash64 footer
   (if checksums enabled) and `pwrite(fd, *buf, pageID*pageSize)`.
   Order within step 1 is unspecified; on Linux the implementation
   may coalesce contiguous runs via `pwritev2`. A partial-success
   pwrite (some pages reach the page cache, others fail mid-step) is
   crash-equivalent: meta is untouched, so on next Open the previous
   meta is selected and the partially-written pages are unreferenced
   bounded leakage.

2. **`fdatasync(fd)`** (skipped in `SyncLazy` and below) — data, RPL,
   and bitmap pages on stable storage.

3. **Meta pwrite.** `pwrite()` the meta-page buffer constructed in
   step 0 to the inactive meta slot.

4. **`fdatasync(fd)`** (skipped in `SyncDataOnly` and below) — this
   is the **atomic commit point**.

After step 2 succeeds, data + RPL + bitmap are durable. After step 4
succeeds, the commit is durable end-to-end. A crash between 2 and 4
leaves the previous meta page active; the new data, RPL, and bitmap
pages are unreferenced free space until the next commit reclaims them
(bounded leakage — see Crash Safety).

**Commit-failure cleanup.** A pwrite error during step 1 or step 3
returns the error to the caller; the transaction must be rolled back.
Rollback releases every slab buffer (live, loose, assembled bitmap,
RPL segments, and the meta-page buffer) back to the pool. The on-disk
meta is untouched, so the database remains consistent with the
previous meta; any partially-pwritten data, RPL, or bitmap pages are
unreferenced and become bounded crash leakage reclaimed by background
maintenance.

Typical commit: tens to low hundreds of data-page pwrites + 0–N RPL
segment pwrites + 2–5 bitmap pwrites (all in step 1) + 1 meta pwrite
(step 3), with two fdatasync calls total (step 2 + step 4). On Linux,
`pwritev2` may issue multiple contiguous-run pwrites in one syscall.

### Slab Lifecycle Across Nested Transactions

Child transactions never modify a parent's slab buffer in place.
Every child CoW allocates a fresh page ID and a fresh slab buffer
(copied from the current value of the parent's view of that page).
On child commit, the child's `(pageID, buf)` entries are merged into
the parent's `p.dirty` and the child's allocations join the parent's
`tx.pendingAllocs`. On child rollback, the child's buffers are released
back to the pool and the child's page IDs are returned to the
allocator (cleared from `tx.pendingAllocs`).

This preserves the simplicity of "rollback discards bookkeeping, never
restores prior buffer state" — at the slab layer rather than the mmap
layer. See Nested Transactions for full mechanics.

### Read-Path Memory

Reads go directly through the mmap; no copies are made on the read
path. The OS page cache backs the data. Page lookup is
`mmap[pageID * pageSize : (pageID+1) * pageSize]` — one level, no
branches.

CoW'd pages from a write transaction become visible through the mmap
to other processes only **after** the commit's data-page pwrites
complete (Linux/macOS unified page cache). Readers serialize their
snapshot via the meta page's `TxnID`; a reader at TxnID T sees only
pages reachable from meta-page-T's roots, which are all already on
stable storage by the time meta-T is published.

## Copy-on-Write Transaction Model

### Write Transaction

1. Writer submits a request to the flock goroutine's writer queue and
   waits for the lock grant, respecting `ctx` cancellation (see Write
   Lock). Returns `context.Cause(ctx)` if cancelled while waiting.
2. Writer reads the active meta page to get current roots, TxnID, and
   file format.
3. For each modification:
   - Traverse the B+tree from root to leaf via `pager.Page(id)`.
   - On modify: pager CoWs to a fresh page ID + fresh slab buffer
     (see Pager and Slab Architecture).
   - Allocate new pages via `pageAlloc()` (loose → bitmap → RPL
     reclamation → lagging reader check → file extension).
   - Old pages from previous transactions go to `tx.retiredPages`;
     pages CoW'd then freed in this transaction become loose pages.
   - For modifications to indexed keyspaces, the engine invokes the
     index extractor on old + new values and applies the index delta
     in the same write — see Indexing.
4. Run **Commit Write Ordering** (see Pager and Slab Architecture):
   - Step 0: pre-pwrite assembly (tail page refund, loose-page move,
     RPL segment allocation, modified-bitmap-page assembly,
     meta-page buffer construction). All buffers enter `p.dirty` or
     the per-transaction meta-page buffer. No syscalls. May abort
     with `ErrTxTooLarge` or `ErrDBFull` — clean rollback.
   - Step 1: pwrite all of `p.dirty` (data + RPL + bitmap).
   - Step 2: `fdatasync(fd)` (skipped in `SyncLazy` and below).
   - Step 3: pwrite the meta-page buffer to the inactive meta slot.
   - Step 4: `fdatasync(fd)` (skipped in all modes below `SyncDurable`).
     **Atomic commit point.**
5. If OS file size exceeds `HighWaterMark` by more than
   `ShrinkThreshold`, `ftruncate()`. After the commit point — a crash
   before truncation leaves the file larger than necessary but
   consistent.
6. Release all slab buffers (live, loose, assembled bitmap, RPL
   segments, meta-page buffer) back to the pool; clear the pager's
   `p.dirty` map and the transaction's `tx.pendingAllocs`,
   `tx.pendingFrees`, `tx.cowPages`, `tx.loosePages`,
   `tx.retiredPages`.
7. Signal the flock goroutine to release the lock.

### Read Transaction

1. Reader checks `ctx` — returns `context.Cause(ctx)` if already cancelled.
2. Reader acquires a slot in the reader table via scan+CAS and records
   the current TxnID from the active meta page. If no slots are
   available and the context has a deadline, the reader retries with
   short backoff until a slot becomes free or the context expires. With
   no deadline, returns `ErrReadersFull` immediately. Use
   `context.WithTimeout` to control the wait window.
3. Reader traverses the B+tree using page pointers from that meta page
   via the read-only pager. Because of CoW, all pages referenced by
   this TxnID are immutable — the writer will never modify them in place.
4. When done, the reader clears its slot in the reader table.

Readers never need locks on the data file. They never block writers.
Writers never block readers. The only contention point is the reader
table slot acquisition (atomic CAS). The context governs the retry
window for slot acquisition but is not stored on the transaction.

**Lagging-reader contract (critical for multi-daemon workloads).** A
read transaction holds its snapshot's TxnID, which pins every page in
the snapshot against RPL reclamation. Daemons that keep a single read
transaction open across many client operations cause unbounded file
growth — pages freed by intervening write transactions accumulate in
the RPL and cannot be reclaimed. The correct pattern in a service
(MCP server, request handler, RPC) is a **short read transaction per
request**, not per session. The `LaggingReader` callback exists as a
last-resort signal, not a substitute for short-lived snapshots.

### Write Batching

`DB.Batch()` amortizes write transaction commit costs across multiple
concurrent in-process callers.

```
db.batchCh chan batchCall

type batchCall struct {
    fn     func(tx *Tx) error
    ctx    context.Context
    result chan<- error
}
```

1. `db.Batch(ctx, fn)` sends the closure, context, and a result
   channel to `db.batchCh`. The caller blocks on the result channel.
2. A coordinator goroutine reads from `db.batchCh`, collecting calls
   until either `Options.MaxBatchSize` calls have accumulated (default
   1000) or `Options.MaxBatchDelay` has elapsed since the first call
   in the batch (default 10ms). Lower delay reduces latency; higher
   increases throughput. Set 0 to fire as soon as the coordinator runs.
3. The coordinator opens a write transaction via `db.Begin(ctx, true)`
   using `context.Background()` — caller contexts are checked separately.
4. Each collected closure runs in its own **child transaction** (see
   Nested Transactions). Before executing, the caller's `ctx` is
   checked — if cancelled, the closure is skipped and the caller
   receives `context.Cause(ctx)`.
5. If a closure returns an error, its child is **rolled back**. The
   parent transaction is unaffected — other closures' children remain
   intact. The failing caller receives the error.
6. If a closure succeeds, its child is **committed** (merged into the
   parent). The caller will receive `nil` when the parent commits.
7. After all closures have run, the parent commits. All callers whose
   closures succeeded receive `nil`. If commit fails, all callers in
   the batch receive the commit error.

Each closure is **invoked exactly once** — there is no rollback-and-retry
loop. This guarantee is about invocation count, NOT about the
atomicity of the closure's external side effects against the
database write. External side effects (logging, metrics, channel
sends, gRPC calls) run *inside* the closure and are unconditional;
the parent batch commit can still fail afterward (e.g., ENOSPC at
fdatasync), in which case the caller receives the commit error
while the side effect has already taken place. Closures whose side
effects must be atomic with the write should defer the side effect
until after `Batch()` returns nil:

```go
err := db.Batch(ctx, func(tx *Tx) error {
    // database write only — no external side effects here
    return ks.Put(key, value)
})
if err != nil { return err }
// safe to notify now: this caller's write is durable
notifyChan <- key
```

**Cross-closure side-effect ordering.** Closures within a single
batch run sequentially in implementation-defined order during the
parent transaction. In-closure side effects from closures A and B
fire in whatever order the coordinator dispatched them; the
deferred-notification pattern above guarantees per-caller
*durability* but does NOT guarantee any ordering relative to
*other* callers' in-closure side effects. If a downstream observer
must see caller A's notification before caller B's notification,
the callers must coordinate that ordering themselves — Batch does
not provide it.

**Cross-process group commit is not provided.** Each process has its
own batch coordinator. Cross-process write coalescing would require
shipping closures/redo records between processes — a large complexity
addition not warranted by the target workloads. Cross-process writers
serialize via the flock; each individual commit is short enough
(microseconds to low milliseconds in cheap-commit modes) that queuing
is not a bottleneck for the target N-daemon profiles.

#### Coordinator Lifecycle

Started lazily on the first `Batch()` call. Stopped when `DB.Close()`
is called: `db.batchCh` is closed, the coordinator drains pending
calls (returning `ErrTxClosed` to each), then exits.

### Nested Transactions

A write transaction can create child transactions independently
committed (merged into parent) or rolled back (discarded) without
affecting the parent. Children never write to disk; only the top-level
parent commits.

```go
child, err := tx.BeginChild()
if err != nil { ... }

err = riskyOperation(child)
if err != nil {
    child.Rollback()  // undo child's work; parent unchanged
} else {
    child.Commit()    // merge into parent
}
```

**Child begin** — snapshot the parent's state:
- Snapshot `tx.pendingAllocs` length (or copy).
- Snapshot `tx.pendingFrees` length.
- Snapshot `tx.cowPages` (CoW'd page IDs).
- Snapshot `tx.loosePages`.
- Snapshot `tx.retiredPages` length.
- Snapshot keyspace root page IDs and counts.
- Snapshot the slab `dirty` map (which page IDs the parent has dirtied)
  — for rollback comparison, not for state restoration. The child does
  not get its own slab; it shares the parent's pager but never modifies
  a page already in the parent's `dirty` set in place.

**Child does work.** CoW always allocates a fresh page ID and a fresh
slab buffer. If the page being CoW'd is already in the parent's `dirty`
set, the child copies the parent's buffer into a new buffer at a new
page ID. The child never mutates a parent's slab buffer in place.

**Child commit.** Discard the saved snapshots. The child's
modifications (slab buffers, pending sets, retired list, root
updates) remain in the parent's pager. No-op beyond freeing the
snapshot memory.

**Child rollback.**
- Release child's slab buffers (those added since child begin) back to
  the pool; remove from the parent pager's `dirty` map.
- Restore `pendingAllocs`, `pendingFrees`, `cowPages`, `loosePages`,
  `retiredPages` from snapshots.
- Restore keyspace roots to their pre-child state.
- The child's CoW'd page IDs are returned to the allocator (they were
  pending allocations and never reached disk).
- Done. No buffer-content restoration needed — every page the child
  touched lives at a fresh page ID, with a fresh slab buffer; dropping
  the buffer drops the modification.

**Nesting depth:** children can create their own children (arbitrary
nesting). Each level snapshots its current state. Rollback at any
level restores to that level's snapshot. Cost is proportional to
pages modified at that level, not total database size.

#### Why This Is Cheap

A page CoW'd by a child lives in a fresh slab buffer at a fresh page
ID. Nothing the parent holds was overwritten. Rolling back means
releasing the buffer (back to a `sync.Pool`) and clearing the page ID
from the bookkeeping sets — no buffer-content restoration, no
parent-state reconstruction. This is the slab analogue of the
fresh-mmap-position CoW model: same simplicity, different storage.

#### Interaction with Write Batching

Each `Batch()` closure runs in a child transaction. If a closure
fails, its child is rolled back — other closures' children
unaffected. Closures execute **exactly once** and do not need to be
idempotent. See Write Batching.

### Transaction Leak Detection

A transaction garbage-collected without `Commit()` or `Rollback()` is a
resource leak: the reader slot (or write lock) is held indefinitely,
blocking RPL reclamation. The most common user error with
LMDB/BoltDB-style databases.

gmdb uses `runtime.AddCleanup` (Go 1.24+) to detect and recover.

#### Setup

When `Begin()` creates a `Tx`, a cleanup is registered:

```go
tx := &Tx{...}
tx.cleanup = runtime.AddCleanup(tx, func(info txCleanupInfo) {
    // 1. Log warning with the stack trace captured at Begin() time.
    // 2. Release the reader slot (or signal flock goroutine to release write lock).
}, txCleanupInfo{
    slotIndex:  tx.readerSlot,
    writable:   tx.writable,
    beginStack: captureStack(),
    db:         tx.db,
})
```

`txCleanupInfo` is a separate struct — `AddCleanup` requires that the
cleanup function not reference the object being cleaned up (no
resurrection). The struct contains only what's needed to release
resources and log a diagnostic.

`captureStack()` calls `runtime.Callers()` at `Begin()` time to record
the call stack — included in the warning so the user can identify
where the leaked transaction was opened.

#### Normal Close

`Commit()` or `Rollback()` cancels the cleanup:

```go
func (tx *Tx) Commit() error {
    tx.cleanup.Stop()
    // ... normal commit logic ...
}
```

In the non-leak case, `AddCleanup` at `Begin()` + `Stop()` at close are
both cheap, allocation-free operations.

#### Cleanup Behavior

When the GC collects a leaked `Tx`:

0. **Check `db.closed` first.** `db.closed` is a `*atomic.Bool`
   allocated on the heap and **shared by pointer** between the `DB`
   struct, every `txCleanupInfo`, and the `dbCleanupInfo` itself.
   The pointer is captured into each cleanup at `runtime.AddCleanup`
   time. Allocating the flag separately (not as an inline field of
   `DB`) is required because `runtime.AddCleanup` provides no
   ordering guarantee between a `DB` cleanup and the `Tx` cleanups
   that depend on observing the close state — if `DB` is collected
   first, an inline-on-`DB` flag would become a dangling pointer.
   With the shared-heap flag, the underlying `atomic.Bool` lives
   until the last referencing cleanup releases its capture.
   `Close()` sets `*db.closed = true` (release-store) *before* it
   begins unmapping. If a Tx cleanup observes `*db.closed == true`,
   it logs the warning and returns immediately — it does NOT touch
   the reader-table mmap (already unmapped or about to be) or signal
   the flock goroutine (already stopped). Without this guard, a
   leaked `Tx` collected after `Close()` would SIGSEGV the GC goroutine.
1. **Log a warning** via the `*slog.Logger` on the `DB` struct (read
   txn / write txn, TxnID, Begin stack).
2. **Release the reader slot** by storing `TxnID = 0` (atomic) in the
   reader table.
3. **Release the write lock** (if writable): non-blocking signal to
   the flock goroutine (channel send with `default:` branch — if the
   channel is closed because `Close()` raced us past the guard at
   step 0, the cleanup logs and returns).

Cleanup runs on a GC background goroutine — must not block or panic.
All operations are non-blocking.

#### Limitations

- **Non-deterministic timing**: GC-collection-dependent. A leaked
  transaction may hold its slot for an extended period.
- **Cross-process**: cleanup only runs in the creating process. Other
  processes' leaks are reclaimed via PID-based stale detection
  (Reader Table).
- **Debug, not control flow**: applications should not rely on
  cleanup. Safety net only.

### Database Handle Leak Detection

`runtime.AddCleanup` applied to the `DB` struct too. A leaked `DB`
holds open file descriptors, mmap regions, and the flock goroutine —
process-scoped resources outliving any individual transaction.

Same pattern: cleanup logs a warning with the Open stack trace, stops
the flock goroutine, munmaps data + lock files, closes file
descriptors. `Close()` cancels the cleanup.

#### `Close()` Ordering

To make per-Tx leak cleanups safe against an early `Close()` (see
Cleanup Behavior step 0 above), `Close()` runs in this order:

1. Store `*db.closed = true` (release-store on the shared
   `*atomic.Bool`) — visible to any subsequent Tx cleanup invocation
   regardless of `runtime.AddCleanup` ordering between the DB and
   its Txs.
2. Stop the heartbeat goroutine via its stop channel and wait for
   its done channel (bounded by the tick interval).
3. Stop the flock goroutine: close `db.stopCh` (the goroutine's
   `select` honors it within at most `Options.LockRetryInterval`).
   Wait for the goroutine's done channel to signal exit; on exit
   it has released any held flock and cleared its writer header
   fields. **After** the done signal, `Close()`'s own goroutine
   closes `db.writerCh` and ranges over it, sending `ErrTxClosed`
   on each pending `writerRequest.result` channel — the flock
   goroutine is no longer reading from the channel at this point,
   so `Close()` is the sole drainer.
4. Stop the batch coordinator (if started): close `db.batchCh`,
   drain pending calls with `ErrTxClosed`, wait for exit.
5. Stop the maintenance goroutine (if running): stop channel + wait.
6. Munmap the data file and lock file.
7. Close all file descriptors.

Any Tx cleanup invoked between steps 1 and 6 sees `db.closed = true`
and exits without touching the soon-to-be-unmapped memory. After
step 6 the mmap is gone but the cleanup's guard at step 0 prevents
the SEGV. Any Tx cleanup invoked *after* step 7 is fine — the guard
still prevents access.

`Close()` is **not** safe to call concurrently with active write or
batch transactions in the same process. Active *read* transactions
hold reader slots that `Close()` will leave occupied; they continue
to operate against the now-unmapped lock file ⇒ undefined behavior.
Callers must ensure all transactions in the process are committed or
rolled back before calling `Close()` — see also `Compact()` for the
related drain pattern.

## Cross-Process Coordination

### Lock File Layout

A separate file (`<dbname>.lock`) is mmap'd as shared memory by all
processes.

```
Lock File
+----------------------------------------------+
| Header (72 bytes)                            |
| Magic              | uint64                  |
| MaxReaders         | uint32                  |
| Padding            | 4 bytes                 |
| UUID               | [16]byte                |  must match data file's UUID
| WriterPID          | uint64                  |
| WriterStartTime    | uint64                  |
| WriterPIDNamespace | uint64                  |
| WriterHeartbeat    | uint64                  |
| LastMaintenanceTime| uint64                  |
+----------------------------------------------+
| Reader Table                                 |
| +-------+-----+-----+------+-------+-------+ |
| | TxnID | PID | PST | PIDN | HB    | HEpoch| | Slot 0 (48 bytes)
| | u64   | u64 | u64 | u64  | u64   | u64   | |
| +-------+-----+-----+------+-------+-------+ |
| | ...                                       | | up to MaxReaders slots
| +-------+-----+-----+------+-------+-------+ |
+----------------------------------------------+
```

PST = Process Start Time. PIDN = PID Namespace. HB = Heartbeat.
HEpoch = HintEpoch (cross-process orphan-detection anchor for slots
stuck mid-acquire; see Stale reader detection).

The lock file structures use Go structs with `structs.HostLayout` (Go
1.24+) which guarantees the host platform's C ABI layout. This allows
safely overlaying Go structs on the mmap'd shared memory region without
manual byte offset arithmetic.

```go
type LockFileHeader struct {
    _                  structs.HostLayout
    Magic              uint64
    MaxReaders         uint32
    _                  [4]byte
    UUID               [16]byte
    WriterPID          uint64
    WriterStartTime    uint64
    WriterPIDNamespace uint64
    WriterHeartbeat    uint64
    LastMaintenanceTime uint64
}

type ReaderSlot struct {
    _                structs.HostLayout
    TxnID            uint64
    PID              uint64
    ProcessStartTime uint64
    PIDNamespace     uint64
    Heartbeat        uint64
    HintEpoch        uint64 // monotonic clock; first observer of PID==0+Heartbeat==0 sets this
}
```

The `HostLayout` marker applies only to the lock file's shared-memory
structures. Data file page formats remain raw byte layouts with
explicit endian-aware encode/decode functions for portability.

**Cross-platform portability of the lock file is not a goal.** The
lock file is ephemeral (deleted when all processes exit) and its
layout deliberately follows the host platform's C ABI via
`HostLayout`. A lock file written by a little-endian process is not
readable by a big-endian process; mounting the database on a
different architecture requires deleting any stale lock file
(which the next opener does automatically when the UUID does not
match the data file's). The data file itself is fully portable
(little-endian, explicit encode/decode) and is not affected.

**Header (72 bytes):**
- `Magic`: identifies the file as a gmdb lock file.
- `MaxReaders`: number of reader slots, set at lock file creation via
  `Options.MaxReaders` (default 4096). Immutable.
- `UUID`: copied from data file's meta at creation. On `Open()`, the
  lock file's UUID is compared to the data file's UUID; mismatch ⇒
  stale lock file ⇒ deleted and recreated.
- `WriterPID`: PID of the current write-lock holder (0 = no writer).
  uint64 for forward safety (Linux `pid_max` can reach 2^22).
- `WriterStartTime`: process start time of the writer for PID-reuse
  detection.
- `WriterPIDNamespace`: PID namespace inode of the writer (Linux), 0
  on other platforms.
- `WriterHeartbeat`: `CLOCK_BOOTTIME` nanos (Linux) /
  `CLOCK_MONOTONIC` elsewhere, updated periodically by the heartbeat
  goroutine while the write lock is held.
- `LastMaintenanceTime`: updated after a maintenance pass completes;
  coordinates maintenance across processes.

**Reader Slot (48 bytes):**
- `TxnID` (atomic): snapshot TxnID held by this reader. 0 = free.
- `PID` (atomic): owning process. uint64 for alignment + forward safety.
- `ProcessStartTime` (atomic): owning process's start time when slot
  was acquired.
- `PIDNamespace` (atomic): PID namespace inode of owner.
- `Heartbeat` (atomic): monotonic clock, updated periodically (~1s) by
  owning process's heartbeat goroutine.
- `HintEpoch` (atomic): cross-process orphan-detection anchor. Zero
  during normal operation. The first writer-scan that observes the
  slot in the "stuck mid-acquire" state (`TxnID != 0 AND PID == 0 AND
  Heartbeat == 0`) CAS-stores its current monotonic clock here;
  subsequent scans by *any* process compare against this stored
  epoch, so the orphan timer survives writer-process turnover (M1 of
  Round 2: transient cron-style writers cycling faster than
  StaleTimeout no longer leave an orphan permanently pinned).
  Cleared back to 0 by slot release and by successful acquire's
  field-write phase.

Total: 72 + (48 × MaxReaders). Default 4096: 72 + 196608 = 196680
bytes (~192 KB).

The lock file is mmap'd `MAP_SHARED` by all processes for the reader
table. The write lock is a separate concern via `flock()`.

### Lock File Lifecycle

Ephemeral. The first process to open the database creates the lock
file, writes the header (Magic, MaxReaders, WriterPID=0,
WriterStartTime=0), initializes all slots to zero. Subsequent
processes validate `Magic`, read `MaxReaders`, mmap at the
corresponding size. If deleted (e.g., after all processes exit), the
next opener recreates it. `MaxReaders` is NOT in the data file — it's
a runtime coordination property.

On open, if the lock file exists, the opener checks `WriterPID`.
Non-zero: determine whether the writer is still alive via
`kill(pid, 0)` + `WriterStartTime` comparison (see Process Start Time).
Dead or recycled: writer crashed; see Stale Writer Recovery.

### Write Lock

Two layers:

- **Intra-process**: a writer queue managed by a single **flock
  goroutine** on the `DB` struct. Writers submit via a channel and
  receive the lock grant via a per-request response channel. Prevents
  two same-process goroutines from concurrent writes while supporting
  context-aware cancellation with zero goroutine accumulation.
- **Cross-process**: `flock(LOCK_EX)` on the lock file, acquired and
  released by the flock goroutine.

#### Flock Goroutine

A single persistent goroutine (started at Open, stopped at Close)
solely owns flock acquisition/release. At most one goroutine is ever
attempting `flock()`. The goroutine never **blocks indefinitely** in
the kernel — it uses `flock(LOCK_EX | LOCK_NB)` (non-blocking) in a
retry loop with a `select` on the stop channel, so `Close()` can
always unwind the goroutine even when another process holds the
write lock for an extended period.

```
db.writerCh chan writerRequest
db.stopCh   chan struct{}     // closed by Close()
db.lockTry  *time.Ticker      // retry interval, default 50ms

type writerRequest struct {
    ctx    context.Context
    ctxDone <-chan struct{}    // equivalent to req.ctx.Done()
    result chan<- error  // nil = lock granted; non-nil = cancelled/error
}
```

Loop:

1. `select` on `db.writerCh` (next request), `db.stopCh` (Close
   signal), and (when flock is held) the writer's release channel.
   On `db.stopCh`: release flock if held, exit.
2. On a new `writerRequest`:
   a. Check `req.ctx` — if already cancelled, send
      `context.Cause(req.ctx)` on `req.result` and loop.
   b. Enter the **non-blocking acquisition loop**:
      - `flock(db.lockFd, LOCK_EX | LOCK_NB)`.
      - On success: proceed to step 3.
      - On `EWOULDBLOCK`: `select` on
        (`db.lockTry.C`, `req.ctxDone`, `db.stopCh`):
          - tick → retry the `flock` syscall.
          - `req.ctxDone` → caller cancelled; send
            `context.Cause(req.ctx)` and loop to step 1 (slot is
            free for the next request).
          - `db.stopCh` → release flock if held, exit.
      - On any other error: send the error to `req.result` and loop.
3. Store `WriterPID`, `WriterStartTime`, `WriterPIDNamespace` in the
   lock file header; send `nil` on `req.result` — writer holds the
   lock.
4. `select` on (writer's release channel, `db.stopCh`).
   - Release: clear `WriterPID`/`WriterStartTime`/`WriterPIDNamespace`,
     `flock(LOCK_UN)`, loop to step 1.
   - `db.stopCh`: clear writer header fields, `flock(LOCK_UN)`, exit.

The non-blocking + ticker pattern is a small CPU/syscall cost
(`Options.LockRetryInterval`, default 50 ms; one extra syscall per
tick while contention persists) in exchange for bounded `Close()`
latency and bounded cancellation latency.

#### Writer Acquisition Flow

`Begin(ctx, writable=true)`:

1. Send `writerRequest{ctx, result}` to `db.writerCh`.
2. `select` on `result` and `ctx.Done()`:
   - `result` ← `nil`: lock granted. Proceed.
   - `result` ← non-nil error: lock not granted.
   - `ctx.Done()` first: writer gives up. The flock goroutine will
     detect cancellation when it processes the request and skip or
     release. Return `context.Cause(ctx)`.

`Commit()` and `Rollback()` signal the flock goroutine to release.

#### Why This Design

A goroutine-per-attempt model would suffer under rapid context
cancellation: each cancelled attempt leaves a goroutine blocked in
`flock` until it acquires-and-releases. Under pathological cancel
patterns this accumulates hundreds of goroutines.

Single flock goroutine eliminates that. Cancelled writers simply
dequeue — they never touch flock. Fixed cost (one goroutine per DB
handle, ~8 KB stack + one `time.Ticker`) for the lifetime of the
database handle.

The non-blocking `flock` + retry pattern (rather than blocking
`flock(LOCK_EX)`) means the goroutine is always cooperatively
cancellable: `Close()` and per-writer `ctx` cancellation are both
honored within at most one retry interval, even when another process
holds the lock indefinitely. The cost is one wasted syscall per tick
of contention.

The DB holds a dedicated fd for the write lock (`db.lockFd`), separate
from the fd for the reader-table mmap. Used exclusively for
`flock()`/`funlock()`.

**Cancellation latency under pathological queue patterns.** When a
writer's `ctx` cancels while the request sits in `db.writerCh` behind
a still-pending predecessor, the cancelled writer's caller returns
immediately with `context.Cause(ctx)`. The request itself remains in
the channel and is processed in turn by the flock goroutine, which
discards already-cancelled requests via the step-2a check *before*
attempting `flock`. Under sustained high-cancellation load this
produces no extra flock syscalls — cancelled requests cost only a
channel receive and a `select`.

#### Stale Writer Recovery

If a process crashes holding the write lock, `WriterPID` remains
non-zero and the `flock()` is automatically released by the kernel (fd
close on process exit). On `Open()` or write-lock acquisition with
`WriterPID` non-zero, the process determines whether the writer is
alive using the same namespace-aware logic as reader stale detection:

1. **Same PID namespace** (`WriterPIDNamespace` == checker's, both
   non-zero):
   a. `kill(pid, 0)` — `ESRCH` ⇒ dead.
   b. If alive, compare `WriterStartTime` — different ⇒ PID recycled.
2. **Different PID namespace** (or either 0): check `WriterHeartbeat`.
   `now - WriterHeartbeat > StaleTimeout` ⇒ dead.

If dead, recovery:
1. Read both meta pages, select the valid one (highest TxnID + valid
   checksum). The crashed writer's partial commit is invisible — CoW
   guarantees the previous meta points to a consistent tree.
2. Scan the reader table for slots with the dead writer's PID (in the
   same PID namespace) and clear them.
3. Clear `WriterPID`, `WriterStartTime`, `WriterPIDNamespace`,
   `WriterHeartbeat`.

No special rollback logic for tree consistency — CoW guarantees the
previous meta points to a fully consistent tree.

Bitmap modifications are deferred in memory (`tx.pendingAllocs` /
`tx.pendingFrees`) and only written to disk via pwrite at commit time.
If the writer crashes before commit, no bitmap modifications reach
disk — the on-disk bitmap is fully consistent with the previous meta.
No leaked pages. The slab buffers were anonymous mmap pages that are
released to the OS on process exit — no on-disk artifacts.

### Reader Table

Slot allocation uses a simple scan with atomic CAS — no free stack or
other auxiliary data structure. The reader table is a flat array of
48-byte slots in the lock file's shared mmap. All operations use atomic
memory ops visible across processes.

**Slot acquire (`Begin` read transaction):**

The acquire sequence is structured so that a crash at *any* point
after the CAS leaves the slot in a state the stale-detector can
reclaim. Heartbeat is written first (so a crash mid-acquire still
gives the slot a "recent liveness" anchor that will eventually go
stale); PID is written last (so the detector's PID-based fast path
is only used once the full identity has been populated).

1. Start scanning from the **slot hint** (`db.readerSlotHint`, an
   `atomic.Uint32` on the DB struct) rather than slot 0.
2. Scan forward (with wraparound) for `TxnID == 0` (free).
3. Atomically CAS the `TxnID` field from 0 to the current meta page's
   TxnID. CAS failure ⇒ continue scanning.
4. Immediately after a successful CAS, in this exact order:
   a. Store `Heartbeat = nowMonotonic()` (atomic).
   b. Store `HintEpoch = 0` (atomic, clears any prior orphan-anchor
      left over from a stale-cleared slot).
   c. Store `PIDNamespace = db.pidNamespace` (atomic).
   d. Store `ProcessStartTime = db.processStartTime` (atomic).
   e. Store `PID = currentPID` (atomic).
5. Register the slot index with the heartbeat goroutine's active list.
6. Update `db.readerSlotHint`.
7. If all slots occupied (full wraparound), return `ErrReadersFull`.

The hint is process-local, updated with a relaxed atomic store — no
cross-process coordination. Under steady-state load, the hint points
to a recently-freed slot and the scan completes in 1–2 iterations.
Worst case wraps around to O(MaxReaders).

The CAS on `TxnID` is the serialization point. 48-byte slots × 4096
= 192 KB — fits in L2 cache, sequential scan with hardware prefetching.

**Slot release (`Commit`/`Rollback` read transaction):**

In order:
1. `PID = 0` (atomic store). Prevents the next stale-reader scan from
   inspecting this process's (about-to-be-stale) PID after release.
2. `Heartbeat = 0` (atomic store). Resets the heartbeat-based
   liveness marker so a subsequent acquirer is in a clean state.
   *Race note:* the heartbeat goroutine may concurrently store a
   fresh value to `Heartbeat` for this slot if it has not yet
   observed the corresponding `activeSlotsMu`-protected
   `Begin/Commit` list update. The race is benign — both stores
   are valid uint64 values and step 4 (`TxnID = 0`) lands shortly
   after, putting the slot in the unambiguously-free state. The
   active-slot list removal happens *before* this step 2 store, so
   the heartbeat goroutine's next tick will not target this slot.
3. `HintEpoch = 0` (atomic store). Clears any orphan-detection
   anchor.
4. `TxnID = 0` (atomic store). Final release — slot is now free.

No CAS — only the slot owner writes its own slot. Step 1 before
step 4 closes the prior-owner-PID race: a writer scanning between
the next acquirer's CAS and its PID store sees `PID == 0` and falls
through to the heartbeat path rather than running `kill()` against
the previous (now-exited) owner's PID.

**Stale reader detection:** during the writer's reader-table scan (to
find min active TxnID), if a slot has non-zero `TxnID`, classify it
by inspecting `PID` and `Heartbeat`:

0. **PID == 0 path** (slot is mid-acquire, mid-release, or orphaned):

   a. **Fresh heartbeat** (`Heartbeat != 0 AND now - Heartbeat ≤
      StaleTimeout`): a live owner is mid-acquire/release. **Skip.**
   b. **Stale heartbeat** (`Heartbeat != 0 AND now - Heartbeat >
      StaleTimeout`): the acquirer made it past step 4a (heartbeat
      store) but crashed before step 4e (PID store), and the
      heartbeat has now aged out. **Orphan: clear `TxnID = 0`.**
   c. **Zero heartbeat** (`Heartbeat == 0`): the acquirer crashed
      *before* step 4a, leaving the slot with `TxnID != 0, PID == 0,
      Heartbeat == 0`. There is no per-slot age signal yet; use
      `HintEpoch` as the cross-process orphan anchor:
        - If `HintEpoch == 0`: this is the first observation. CAS
          `HintEpoch` from 0 to `now`. **Skip** this round; the next
          scan (from any process) compares against the stored epoch.
        - If `HintEpoch != 0 AND now - HintEpoch > StaleTimeout`:
          confirmed orphan. **Clear `TxnID = 0`** (and `HintEpoch`
          via the post-clear cleanup below).
        - Otherwise: **skip**.
1. **PID != 0, same PID namespace** (slot's `PIDNamespace` == checker's,
   both non-zero):
   a. `kill(pid, 0)` — `ESRCH` ⇒ stale.
   b. If alive, compare `ProcessStartTime` — different ⇒ PID recycled,
      stale.
   c. Match ⇒ alive and holding the slot legitimately.
2. **PID != 0, different PID namespace** (or either namespace inode
   is 0): heartbeat check.
   a. `now - Heartbeat > StaleTimeout` ⇒ stale, clear `TxnID = 0`.
   b. Fresh ⇒ not stale.
3. If neither PID nor heartbeat can determine liveness, fall back to
   PID-only liveness (legacy path).

When the writer clears a stale slot, it stores in this exact order:
1. `HintEpoch = 0` (atomic). Resets the orphan-detection anchor
   *while the slot is still observably non-free*, so no acquirer
   can race into the slot and inherit a stale epoch.
2. `TxnID = 0` (atomic). Final release — slot is now free.

The slot's PID/PST/PIDN/Heartbeat are left as-is and will be
overwritten by the next acquirer per the acquire ordering above.

The HintEpoch-first ordering is load-bearing: if these two stores
were reversed, a window would exist between `TxnID = 0` and
`HintEpoch = 0` during which a fresh acquirer could CAS-win TxnID
and crash before step 4a (heartbeat store). A subsequent stale-
detection scan would then see `TxnID != 0, PID == 0, Heartbeat == 0,
HintEpoch = <stale value from prior cycle>` and immediately
re-clear the slot via case (c)'s timer (already aged out), evicting
the (genuinely dead) new acquirer faster than StaleTimeout — which
is benign for *that* slot but violates the per-occupant timer
invariant. Zeroing HintEpoch first closes the window.

**Why `HintEpoch` lives in shared memory.** A process-local epoch
would not survive writer-process turnover: short-lived writers
(cron jobs, batch scripts) each observe the orphaned slot once,
record their own local epoch, and exit before the StaleTimeout
elapses, leaving the slot permanently pinned. The shared-memory
`HintEpoch` accumulates observation time across all writer
processes — the first observer sets it; any later writer in any
process clears the slot once `now - HintEpoch > StaleTimeout`.

The PID namespace check prevents cross-namespace failure modes (false
dead when containers don't share PIDs; false alive when distinct
processes happen to share a PID).

#### Go Goroutine Model

Multiple slots may share the same PID (same process running multiple
read transactions). This is correct:

- **Slot allocation**: CAS on TxnID serializes claims across goroutines
  and external processes.
- **Stale detection**: `kill(pid, 0)` checks process liveness, not
  thread liveness. If a process crashes, all its slots are stale.
- **Oldest reader scan**: writer finds min TxnID across all occupied
  slots. Multiple slots from one process with different TxnIDs handled
  naturally.

A single Go process running N concurrent read transactions consumes N
reader slots. Set `MaxReaders` high enough for the expected total
across all processes.

#### Process Start Time

PID reuse detection: both reader slots and writer header store
**start time** alongside PID. Monotonically-increasing value that
changes when a PID is recycled — unique `(PID, StartTime)` per
process lifetime.

At `Open()`, the process reads its own start time once and caches it
on the DB struct (`db.processStartTime uint64`). Stored in reader
slots on `Begin()` and in `WriterStartTime` on write lock acquisition.

During stale detection, the writer reads the current start time for a
given PID via `processStartTime(pid int) (uint64, error)`. If the PID
is alive but the current start time differs from the stored value, the
PID was recycled.

| Platform | Source | Value | Notes |
|----------|--------|-------|-------|
| Linux | `/proc/[pid]/stat` field 22 | Clock ticks since boot (uint64) | No privileges. Pure Go: `os.ReadFile` + parse. |
| macOS | `sysctl KERN_PROC_PID` → `kinfo_proc.kp_proc.p_starttime` | timeval packed as `sec*1e6+usec` | Same-user processes. Pure Go via `syscall.Sysctl`. |
| FreeBSD | `sysctl KERN_PROC_PID` → `kinfo_proc.ki_start` | timeval packed | Same as macOS interface. |

All pure Go. `processStartTime` per-platform via build tags.

If `processStartTime` fails, falls back to heartbeat (if available) or
PID-only liveness.

**Resolution caveat.** `ProcessStartTime` is *non-decreasing*, not
strictly unique. Linux `/proc/[pid]/stat` field 22 reports clock
ticks since boot (typically 100 Hz = 10 ms resolution); two processes
spawned within the same tick share a start time. macOS / FreeBSD
sysctl encodes `sec*1e6 + usec` which is microsecond-resolution but
still collision-prone under heavy fork bursts. In particular,
container PID 1 typically has a start time very near zero relative
to the container's boot. The protocol's correctness does **not**
rely on uniqueness of `(PID, StartTime)` — it relies on the
*combination* of (same-namespace PID liveness, start-time match,
fresh heartbeat) and the heartbeat path being available as a
fallback. A same-namespace `(PID, StartTime)` collision between
distinct process lifetimes is benign because either (a) the prior
holder is dead and the heartbeat is stale (caught by the heartbeat
goroutine if the new holder rebooted heartbeat tracking, or by the
zero-heartbeat orphan rule in stale detection) or (b) the prior
holder is alive and legitimately holds the slot.

#### PID Namespace Awareness

PID-based liveness operates within the caller's PID namespace. When
multiple containers share a database file via volume mount, each
container has its own PID namespace — a PID in one refers to a
different (or nonexistent) process in another. Two failure modes:

- **False dead**: container A holds slot at PID 42; container B has
  no PID 42; `kill(42, 0)` from B returns `ESRCH` — B clears the slot,
  removing snapshot protection for A's active reader.
- **False alive**: container A crashes with PID 42 in a slot;
  container B also has a PID 42; `kill(42, 0)` from B succeeds — slot
  never reclaimed.

Each slot and the writer header store the process's **PID namespace
inode** alongside the PID. On Linux, read from `/proc/self/ns/pid` via
`readlink` at Open and cached on the DB struct. On non-Linux, 0. If
the `readlink` fails (no `/proc` mounted, hardened sandbox), the DB
caches 0 and logs the failure via `slog.Logger` — this forces every
cross-process stale check involving this process to fall through to
the heartbeat path, which is safe but slower than PID-based.

The writer compares its own PID namespace to the slot's. Match ⇒ PID
+ StartTime fast path is safe. Differ ⇒ use heartbeat. A process
with `PIDNamespace == 0` (Linux without `/proc`, or non-Linux) is
treated as "different namespace" for the purposes of stale detection
when the peer has a non-zero namespace inode — the asymmetry routes
both directions through heartbeat, which is the correct conservative
behavior.

#### Heartbeat Goroutine

The DB struct maintains a **heartbeat goroutine** (started at Open,
stopped at Close) that periodically updates the `Heartbeat` field on
all reader slots and the writer header held by this process.

Ticks every ~1s. Writes current monotonic clock (`CLOCK_BOOTTIME` on
Linux, `CLOCK_MONOTONIC` on other platforms) to each active slot.
The DB maintains an in-process **active slot list** — a `[]uint32`
of slot indices protected by `db.activeSlotsMu` (a `sync.Mutex`).
`Begin()` appends under the mutex; `Commit()`/`Rollback()` removes
under the mutex; the heartbeat goroutine takes a brief snapshot of
the list under the mutex each tick and issues the atomic stores
outside the lock to keep tick cost bounded.

`CLOCK_BOOTTIME` on Linux because it is monotonic, survives
suspend/resume, and is shared across all containers on the same host
(kernel-wide, not per-PID-namespace). `CLOCK_MONOTONIC` on macOS /
FreeBSD does not survive suspend; on a laptop that resumes after a
long sleep, the heartbeat clock jumps forward by less than wall-time
elapsed, so `StaleTimeout`'s 10-second default is safe — false-stale
detection requires a heartbeat older than 10 s of *monotonic* time,
which a suspended process cannot accumulate.

`StaleTimeout` (default 10s) controls how long a heartbeat must be
stale before the slot is reclaimed. Must be significantly larger than
the heartbeat interval (1s) for scheduling jitter.

**Shutdown coordination.** `Close()` sets `db.closed = true`
(atomic, see Database Handle Leak Detection), closes the heartbeat
goroutine's stop channel, and **waits** for the goroutine to
acknowledge via a done channel before unmapping the lock file. The
heartbeat goroutine checks the stop channel before each tick and
exits promptly. Without the wait, a final tick could race with the
munmap and SIGSEGV. The wait is bounded by the tick interval (~1s)
since the goroutine sleeps in a `select` that includes the stop
channel.

Fixed-cost resource: one goroutine per DB handle, one atomic store
per active slot per second. No syscalls, no allocations.

#### Atomic Operations Convention

- **In-process fields** (DB/Tx struct fields like `db.readerSlotHint`)
  use Go's **typed atomics** (`atomic.Uint64`, `atomic.Uint32`,
  `atomic.Int64`).
- **Shared-memory fields** (reader table fields, header writer fields
  in the mmap'd lock file) use **function-based atomics**
  (`atomic.LoadUint64`, `atomic.StoreUint64`,
  `atomic.CompareAndSwapUint64`) on `unsafe.Pointer`-derived addresses.
  Typed atomics cannot be used here because the memory is a raw region
  in `MAP_SHARED` mmap visible across processes.

**Memory-model caveat.** Go's memory model formally describes
synchronization only on memory the Go runtime owns. Cross-process
shared memory via `MAP_SHARED` is outside that model — the protocol's
correctness rests on (a) Go's `sync/atomic` functions emitting
hardware atomic instructions (`LOCK CMPXCHG` / aligned `MOV` on
amd64; `LDAR` / `STLR` / `LDXR`-`STXR` on arm64) and (b) the
underlying hardware guaranteeing single-copy atomicity for
naturally-aligned 64-bit loads and stores. Both gmdb-supported
architectures (amd64, arm64) satisfy this.

All shared-memory fields are 8-byte aligned at runtime by composition:
each field is a `uint64` whose natural C ABI alignment is 8 bytes
(enforced inside the struct by `structs.HostLayout`'s "no Go-internal
reordering" guarantee), and the struct's base address is page-aligned
because it lives inside a `MAP_SHARED` mapping (≥ 4096-byte alignment).
`HostLayout` itself controls field layout, not struct-pointer alignment
— the page-aligned mmap base is what makes the struct-start address
naturally aligned. Ports to architectures with weaker single-copy
atomicity guarantees would require revisiting this section.

### Lock Ordering

gmdb maintains several mutex/lock primitives. To prevent deadlock,
they are acquired in the following strict order. Code that violates
this order is a bug.

```
Outer  →  flock goroutine queue (db.writerCh)
       →  cross-process flock(LOCK_EX) on lock file fd
       →  intra-process write lock (held implicitly by write txn)
       →  per-keyspace open registry (db.keyspaceRegistry.mu)
       →  active-slot list (db.activeSlotsMu — for heartbeat coord)
       →  pager mutex (per write txn, db.pager.mu — for slab map updates)
       →  reader-table slot CAS (no mutex — atomic CAS only)
Inner  →  bitmap mutex (in-process, db.bitmap.mu — for two-level summary)
```

Notes:

- A read transaction only ever touches the reader-table CAS path and
  the active-slot list mutex (briefly, on Begin/Commit/Rollback). It
  does not enter any of the writer-side locks above.
- The flock goroutine never calls into the application; application
  goroutines never call into the flock goroutine except by sending on
  `db.writerCh`. This breaks any potential cycle through application
  code.
- The maintenance goroutine acquires the same locks as a writer when
  performing reclamation or compaction; it must respect this order.
- The heartbeat goroutine only acquires `activeSlotsMu` (briefly, to
  snapshot the slot list) and issues atomic stores to shared-memory
  reader-slot fields outside the mutex. It does not enter any
  writer-side lock.
- A write transaction must NOT open an internal read snapshot
  (which would acquire `activeSlotsMu`) while holding `pager.mu`,
  because that would invert the documented order. Internal read
  snapshots taken by write-flow operations (e.g., the read-snapshot
  side of `Compact()`'s copy phase) must be initiated *before* the
  writer's pager work begins, or after it completes.
- Cleanup callbacks (`runtime.AddCleanup`) run on GC background
  goroutines and only do non-blocking operations (atomic check of
  `db.closed`, atomic store on reader slot, non-blocking channel send
  to flock goroutine) — they do not acquire any of the above locks.
- The snapshot/reader-table mmap and the data-file mmap are separate
  mappings; no lock is required to access either, only the atomic
  conventions above.

### Writer's Page Reclamation

Before reclaiming retired pages, the writer scans the reader table to
find the minimum active TxnID. Any RPL entries with `TxnID <
min_active` are safe to reclaim — their bits are set in the bitmap.

### Lagging Reader Handling

A single long-lived reader prevents all RPL reclamation for
transactions newer than its snapshot, causing unbounded file growth.

The application can register a `LaggingReader` callback via `Options`
that is invoked when a reader is blocking allocation. Invoked from
`pageAlloc()` when:
1. The bitmap has no suitable free pages.
2. The RPL has no more reclaimable entries.
3. A reader in the reader table is blocking reclamation.

The callback receives `LaggingReaderInfo` and returns an action.
`LaggingReaderWait` causes `pageAlloc()` to refresh the reader table
and retry. `LaggingReaderAbort` causes `pageAlloc()` to return
`ErrDBFull`.

Invoked at most once per `pageAlloc()` call to avoid busy loops. The
application can log warnings, send alerts, or take corrective action
(e.g., killing a stuck process identified by PID).

**The callback is a safety net, not a substitute for short read
transactions.** Services should structure read access as "one read
transaction per request/operation," not per session. See Read
Transaction.

## mmap Strategy

The data file is mapped `MAP_SHARED | PROT_READ` by every process,
including the writer. `mprotect(PROT_READ)` is applied after Open as a
belt-and-suspenders guard against accidental writable mappings.

All writes go through pwrite (see Pager and Slab Architecture and
Commit Write Ordering). The read-only mapping ensures that:

- A stray pointer or `unsafe` misuse in the host process produces
  SIGSEGV instead of silently corrupting the file.
- Cross-process readers observe a stable, well-defined view of any
  page: a page transitions atomically (relative to the meta swap) from
  "previous content visible via mmap" to "new content visible via mmap"
  because the writer pwrites the new content into the unified page
  cache before publishing the meta page that references it.

### Page Memory Management

OS-managed. Reads through the mmap are file-cache-backed; the kernel
handles eviction under memory pressure. No application-level page
buffer, no eviction algorithm, no page-count limit. The OS has global
visibility into memory pressure across all processes and is better
positioned to make eviction decisions.

The writer additionally holds slab buffers (page-sized) for pages it
has CoW'd in the current transaction. Slab usage is bounded by
`Options.MaxTxBufferBytes`; exceeded ⇒ `ErrTxTooLarge`.

### Read Path

All processes mmap the data file with:

```
MAP_SHARED | PROT_READ
```

Page lookup is `mmap[pageID * pageSize]` — one level, no branches.
Branch + leaf page reads go directly through this mmap. The OS page
cache serves the data.

`Options.ReadOnly` controls whether the writer path is initialized at
all (the data mmap mode does not change — it is always read-only).
When `ReadOnly = true`, the lock file is not opened for write, the
flock goroutine is not started, and write transactions return
`ErrReadOnly`. Suitable for read-only media or read-only filesystem
permissions.

### Write Path

The writer does **not** modify the mmap. All modifications:

1. Read current page content via `pager.Page(id)` (which checks
   `dirty[id]` first, falling back to mmap).
2. Allocate fresh page ID + slab buffer; copy old content into buffer.
3. Mutate buffer.
4. At commit: pwrite buffers, bitmap pages, then meta page (see Commit
   Write Ordering).

There is no platform-specific code in the commit path. Linux and macOS
both use `pwrite + fdatasync`. No `msync(MS_SYNC)` is needed because
the writer never writes through the mmap.

### mmap Resizing

The mmap region is sized to `MaxSize`. This over-allocates virtual
address space — only the file-backed portion is usable, but the
mapping does not need to change as the file grows or shrinks. The
unmapped region beyond the file size will SIGBUS if accessed, so
readers must check `HighWaterMark` from the meta page.

`MAP_SHARED` file-backed mappings are not charged against Linux
`vm.overcommit_memory` accounting (the file is the backing store). But
per-process `RLIMIT_AS` does apply to virtual address space
reservations regardless of mapping type. Most defaults are unlimited;
restricted environments may need a lower `MaxSize`.

### Prefaulting (Linux 5.14+)

When `Options.PreloadPages` is true, the database calls
`madvise(MADV_POPULATE_READ)` on the file-backed portion of the mmap
(pages 0 through `HighWaterMark - 1`) at open time. Pre-faults all
pages into the OS page cache, eliminating page faults on first access.

- **Predictable latency**: first read txn doesn't pay per-page fault
  costs.
- **Sequential I/O**: kernel reads pages sequentially during prefault,
  more efficient than random-access demand paging.

`MADV_POPULATE_READ` (Linux 5.14+) works on `MAP_SHARED` and returns
errors synchronously. Silent no-op on older kernels.

Prefaulting is also performed internally during `CopyTo()` on the
source database's mmap.

Default: false — most workloads benefit from demand paging where only
accessed pages enter the cache.

### Huge Pages (Linux)

When `Options.HugePages` is true, the database calls
`madvise(MADV_HUGEPAGE)` on the data file mmap. Enables transparent
huge page (THP) backing, allowing the kernel to use 2MB pages instead
of 4KB.

- **Reduced TLB pressure**: a 1GB database drops from 262,144 TLB
  entries to 512 (4KB → 2MB).
- **Fewer page faults**: each fault maps 2MB instead of 4KB.

THP for file-backed `MAP_SHARED` is mature on Linux 6.x. Kernel
promotes opportunistically based on alignment and availability.

Default: false. Ignored on non-Linux and on kernels without THP for
file-backed mappings.

### Read Transaction Cooldown (Linux 5.4+)

When `Options.ReclaimOnClose` is true, closing a read transaction
calls `madvise(MADV_COLD)` on the mmap region the transaction
accessed. Hints the kernel that the pages are no longer actively used
and may be reclaimed under memory pressure.

Useful for batch processing workloads with large sequential scans
(exports, analytics queries). Without `MADV_COLD`, scanned pages
remain in the cache, potentially evicting more useful pages.

Implementation tracks min/max page IDs accessed during the
transaction (two atomic min/max updates per page read) and issues a
single `madvise(MADV_COLD, min*PageSize, (max-min+1)*PageSize)` on
close.

Default: false. Silent no-op on non-Linux or kernels < 5.4.

## Durability Modes

Three safe modes and one unsafe mode, configurable via
`Options.SyncMode`. The mode controls which `fdatasync()` calls are
performed during commit. All safe modes preserve **database integrity**
(the file is always structurally valid). `SyncUnsafe` requires explicit
opt-in via `Options.AllowSyncUnsafe = true`.

| Mode | Data Sync | Meta Sync | On Crash | Performance |
|------|-----------|-----------|----------|-------------|
| `SyncDurable` (default) | `fdatasync()` | `fdatasync()` | No data loss. Full ACID. | Slowest |
| `SyncDataOnly` | `fdatasync()` | skip | Last committed transaction may be lost. DB is consistent — falls back to previous meta page. | ~2x faster |
| `SyncLazy` | skip | skip | Rolls back to the last **checkpoint**. DB is always consistent — no corruption. | Much faster |
| `SyncUnsafe` | skip | skip | **Risk of corruption.** Requires `AllowSyncUnsafe`. Benchmarks and ephemeral data only. | Fastest |

### Checkpoints

In `SyncLazy` mode, commits pwrite bitmap, data, and meta but skip all
`fdatasync()` calls. The OS page cache holds the writes; order is not
guaranteed.

A **checkpoint** is a commit whose data pages have been confirmed on
stable storage. Checkpoints occur when:
- `DB.Checkpoint()` is called explicitly (`fdatasync` of the data file).
- A commit happens in `SyncDurable` or `SyncDataOnly` mode (these sync
  data pages as part of their normal commit path).

Each meta page carries a **checkpoint flag** (Flags bit 1). Set when
`fdatasync()` completes. In `SyncLazy`, commits write meta with the
flag **clear**. `DB.Checkpoint()` re-writes meta with the flag **set**.

**Checkpoint() mechanics:**

1. Acquire the write lock via the flock goroutine — same path as
   `Begin(writable=true)`, respecting the supplied `ctx`. This
   serializes Checkpoint against any concurrent write transaction
   and any concurrent `Compact()` in the queue; concurrent reads
   are unaffected. Returns `context.Cause(ctx)` if cancelled before
   the lock is acquired.
2. `fdatasync(fd)` to flush all data, RPL, bitmap, and meta pages
   pwritten by prior `SyncLazy` commits that are sitting in the OS
   page cache. (The data mmap is `PROT_READ` and the writer never
   writes through it, so there are no mmap dirty pages from gmdb;
   the fdatasync's job is purely to flush pwritten page-cache
   contents.)
3. Read the currently active meta page; toggle its checkpoint flag
   on; recompute the xxhash64 checksum over the full meta payload
   (flag change shifts the hash); `pwrite()` it back to the same
   slot. The TxnID is unchanged — Checkpoint records that the
   already-committed state is durable, not a new transaction.
4. `fdatasync(fd)` again so the flag set itself reaches stable
   storage.
5. Release the write lock.

Steps 2 and 4 are both required: step 2 makes prior lazy commits
durable; step 4 makes the flag-set durable so recovery can trust it.
The single-meta-slot pwrite in step 3 is atomic because it stays
within one page (an unaligned tear cannot affect a single contiguous
sub-page region, and the xxhash64 checksum catches any partial
write — recovery falls back to the other slot).

On recovery:

1. Read both meta pages. Discard any with invalid xxhash64 checksum.
2. Of the valid metas, select the one with the highest TxnID whose
   checkpoint flag is **set**.
3. If neither meta has the checkpoint flag set (the user never called
   `Checkpoint()` and never used `SyncDurable`/`SyncDataOnly`), select
   the higher-TxnID valid meta. Data integrity depends on whether the
   OS flushed pages in the right order — not guaranteed. `Open()`
   logs a warning via `slog.Logger`.
4. Non-checkpoint metas are never preferred over checkpoint ones,
   regardless of TxnID.

Recovery does not attempt to validate a non-checkpoint meta's tree.
Accepting a partially-durable tree would risk surfacing `ErrCorrupted`
on later reads when traversals reach unflushed pages. The checkpoint's
tree is guaranteed intact because CoW never modifies existing pages.

### SyncUnsafe Warning

Provides no crash safety. Without `fdatasync()` after pwrite, the
meta page could reach disk before the bitmap pages. A crash leaves
the meta pointing to a tree whose bitmap state is inconsistent — the
database may be **corrupted**.

`SyncUnsafe` requires `Options.AllowSyncUnsafe = true`. Setting it
without the opt-in returns an error from `Open()`.

**Cross-process SyncMode interleaving.** `SyncMode` is a per-process
`Options` setting, not stored on disk. Different processes attached
to the same database may run with different SyncModes. The on-disk
checkpoint flag reflects whichever mode the *committer* used: a
commit by a `SyncDurable` process sets the flag; a commit by a
`SyncLazy` process clears it. Recovery selects the highest-TxnID
**checkpoint-flagged** meta, so interleaving `SyncLazy` and
`SyncDurable` writers across processes works correctly — a crash
rolls back to the most recent `SyncDurable`-or-`Checkpoint`-set
meta, possibly losing intervening `SyncLazy` commits from any
process. This is the same trade-off as `SyncLazy` within a single
process; the multi-process composition is consistent with that.

## File Format

The database file size is managed dynamically between configurable
lower and upper bounds. File format is stored in the meta page.

### File Format Parameters

| Parameter | Meta Field | Description | Default |
|-----------|-----------|-------------|---------|
| Lower bound | `MinSize` | Minimum file size in pages. | `2 + BitmapPages` |
| Upper bound | `MaxSize` | Maximum file size in pages. Determines mmap reservation and bitmap size. **Immutable after creation.** | 256GB / PageSize |
| Growth step | `GrowStep` | Pages to grow by when extending. | 65536 (256MB at 4KB) |
| Shrink threshold | `ShrinkThreshold` | Shrink when `fileSize - HighWaterMark > threshold`. | 131072 (512MB at 4KB) |

Set at creation via `Options` and persisted. `MinSize`, `GrowStep`,
and `ShrinkThreshold` can be modified via `Tx.SetFileFormat()`.

**`MaxSize` is immutable.** The bitmap region size is fixed at
creation; changing `MaxSize` would shift all data page offsets,
invalidating every page ID. To change `MaxSize`, use `CopyTo(path,
compact)` to create a new database.

### File Growth

When `pageAlloc()` needs to extend:
1. `newSize = alignUp(HighWaterMark + needed, GrowStep)`.
2. Clamp to `MaxSize`. Exceeded ⇒ `ErrDBFull`.
3. `ftruncate()` the file. The existing mmap (sized to `MaxSize`)
   covers new pages automatically — no second `mprotect` call is
   needed, because the `mprotect(PROT_READ)` applied at Open covers
   the full `MaxSize` virtual reservation, and the newly file-backed
   pages inherit `PROT_READ` from that reservation. This inheritance
   holds on all supported targets (Linux, macOS, FreeBSD): on each,
   `MAP_SHARED` over the reservation is a single VMA that `ftruncate`
   does not split, so the VMA's protection applies uniformly to the
   newly-backed pages without additional syscalls. Ports to other
   OSes must re-verify this property.

### File Shrinkage

After the commit point, if file size exceeds `HighWaterMark` by more
than `ShrinkThreshold`:
1. `newSize = alignUp(HighWaterMark, GrowStep)`.
2. Clamp to `MinSize`.
3. `ftruncate()`. The mmap reservation remains at `MaxSize` — the
   truncated region becomes unmapped (SIGBUS on access), safe because
   `HighWaterMark` in the meta page prevents any reader from
   accessing those pages.

Automatic and zero-overhead — happens as a natural consequence of
tail page refund during commit. No explicit compaction needed for
the common case.

## Keyspaces

The root meta page points to a **keyspace B+tree** — a B+tree whose
keys are keyspace names (byte strings) and whose values are keyspace
descriptors. Both user keyspaces and engine-internal keyspaces (per-index
storage, per-index registries) live in this tree.

### Keyspace Descriptor

The descriptor is a fixed-layout 40-byte struct stored as the value
for the keyspace's entry in the keyspace B+tree:

```
Keyspace Descriptor (40 bytes)
+----------+----------+----------+----------------+----------+--------------+--------------------+----------+
| Root     | Count    | Kind     | FixedValueSize | NextSeq  | RestartGroup | IndexRegistryRoot  | Reserved |
| uint64   | uint64   | uint8    | uint16         | uint64   | uint16       | uint64             | [3]byte  |
+----------+----------+----------+----------------+----------+--------------+--------------------+----------+
```

Total: 8 + 8 + 1 + 2 + 8 + 2 + 8 + 3 = 40 bytes.

- **Root** (uint64): page ID of this keyspace's B+tree root. 0 = empty.
- **Count** (uint64): number of key-value pairs. For SetKeyspace, total
  pairs across all value sets.
- **Kind** (uint8): `0` = Keyspace (key → value), `1` = SetKeyspace
  (key → sorted set of values). `2` = engine-internal index keyspace
  (not directly openable by users). `Open()` rejects unknown values.
  Set at creation, immutable.
- **FixedValueSize** (uint16): for SetKeyspace, the fixed value size in
  bytes (0 = variable). Must be 0 when Kind != 1.
- **NextSeq** (uint64): next sequence number for `NextSequence()`. First
  call returns 1.
- **RestartGroupTarget** (uint16): per-keyspace target leaf
  restart-group size. 0 ⇒ engine default (16). Set at creation, mutable
  via `Tx.SetKeyspaceConfig()` — new value applies to leaves written
  after the change; existing leaves keep their stored `RestartInterval`
  until they next split or are rewritten.
- **IndexRegistryRoot** (uint64): page ID of this keyspace's per-keyspace
  index registry sub-tree (see Indexing → Storage Layout). 0 ⇒ no
  indexes declared on this keyspace.
- **Reserved** (3 bytes): must be zero. `Open()` rejects descriptors
  with non-zero reserved bytes.

Depth (tree height) is not persisted — derived by reading the root
page on first access. Avoids maintaining a redundant field across
split/merge/rebalance.

Opening a keyspace reads the descriptor from the keyspace B+tree.
Modifications update the descriptor (and its root) which propagates
up through the keyspace B+tree via CoW.

Opening a keyspace with the wrong type (`OpenKeyspace` on a
SetKeyspace, etc.) returns `ErrKeyspaceKindMismatch`. Attempting to
open an engine-internal index keyspace via the user API returns
`ErrKeyspaceReserved`.

### Per-Keyspace Configuration

Two per-keyspace properties currently:

- `FixedValueSize` — SetKeyspace only, immutable after creation.
- `RestartGroupTarget` — mutable via `Tx.SetKeyspaceConfig()`. Defaults
  to engine-global 16. Tune higher (e.g., 32) for keyspaces with very
  long shared prefixes (directory listings, deeply nested composite
  keys); tune lower (e.g., 8) for keyspaces with mostly distinct keys
  to reduce per-`Prev()` group decode cost.

Per-keyspace page size is **not** supported — see Design Decisions.

### Keyspace Name Interning

Keyspace names are interned via `unique.Make[string]` (Go 1.23+). The
internal keyspace lookup cache stores a `unique.Handle[string]` instead
of a raw `string` or `[]byte`. Avoids repeated allocations when the
same keyspace is opened across many transactions. `unique.Handle`
provides O(1) equality comparison and is safe for concurrent use.

## Indexing

gmdb maintains secondary indexes on keyspaces declaratively. The
caller declares one or more indexes per keyspace at open time,
supplying an extractor function that produces index entries from a
row. The engine applies index changes inside every write transaction
that modifies the keyspace, atomic with the row write.

### Overview

```go
// IndexDecl describes one secondary index on a byte-oriented keyspace.
type IndexDecl struct {
    Name     string             // unique within the keyspace
    Columns  []IndexColumn      // ordered; concatenated lex-safely
    Covering []CoveringColumn   // optional; stored in the index value
    Unique   bool               // engine rejects extractor-produced duplicates
    Version  string             // user-supplied; bump after extractor-logic changes
    Extract  IndexExtractor
}

type IndexColumn struct {
    // Name is a semantic anchor for the column: it identifies the
    // logical role of this position in the column tuple and contributes
    // to the index's schema-hash fingerprint. The Name is never
    // interpreted by the engine at read time — column storage is
    // purely positional. Renaming a column changes the schema hash and
    // forces RebuildIndex. Reusing a name for a column whose semantic
    // content has changed is the user's responsibility — bump Version
    // in that case (see Drift Guard).
    Name string
}

type CoveringColumn struct {
    Name string // same semantics as IndexColumn.Name
}

type IndexEntry struct {
    Cols  [][]byte // one byte slice per IndexColumn; lex-safe encoded by caller
    Cover [][]byte // one per CoveringColumn (omit when Covering is nil)
}

// IndexExtractor produces zero or more IndexEntry values for a row.
// Returning a nil slice or a zero-length slice both signal "do not
// index this row" (partial-index semantics) and are equivalent.
type IndexExtractor func(key, value []byte) []IndexEntry
```

A row that should not be indexed (partial-index case) is signaled by
returning an empty slice or `nil` from the extractor — both are
equivalent.

For typed callers, `TypedIndex[K, V, IK]` wraps `IndexDecl` and
generates column bytes automatically from a typed `Encoder[IK]` — see
Typed Keyspaces.

### Index Declaration

Indexes are declared at the call that opens the keyspace for write
access:

```go
ks, err := tx.OpenKeyspace("workspaces",
    &IndexDecl{
        Name:    "by_repository",
        Columns: []IndexColumn{{Name: "repository_id"}},
        Unique:  false,
        Version: "v1",
        Extract: func(key, value []byte) []IndexEntry {
            repoID := decodeRepoID(value)
            return []IndexEntry{{Cols: [][]byte{repoID}}}
        },
    },
    &IndexDecl{
        Name:    "active_lease_unique",
        Columns: []IndexColumn{{Name: "workspace_id"}, {Name: "lease_kind"}},
        Unique:  true,
        Version: "v1",
        Extract: func(key, value []byte) []IndexEntry {
            r := decodeLease(value)
            if r.State != "active" {
                return nil
            }
            return []IndexEntry{{Cols: [][]byte{r.WorkspaceID, r.LeaseKind}}}
        },
    },
)
```

Every transaction that opens this keyspace for write must supply
matching `IndexDecl`s — same name set, same column specs, same
`Unique` flag, same `Version`. Mismatch surfaces as
`ErrIndexFingerprintMismatch` at open time.

Duplicate `IndexDecl.Name` values in one `OpenKeyspace` call's
variadic slice (programmer error) are rejected with `ErrIndexExists`
naming the offending duplicate. Index names are keys in the schema
hash and in the on-disk registry — duplicates would either collide
on the registry write or render the recovery-loop's linear search
non-deterministic.

### Drift Guard: Schema Hash + Version Tag

For each declared index, the engine computes a deterministic
**schema hash**:

```
xxhash64(
  index.Name ||
  uvarint(len(Columns)) || for each col: uvarint(len(Name)) || Name ||
  uvarint(len(Covering)) || for each col: uvarint(len(Name)) || Name ||
  uint8(Unique)
)
```

The schema hash + the user-supplied `Version` string are stored on
disk in the per-index registry entry. At Open, the engine compares
the **supplied** schema hash and version against the **stored** ones.
Any mismatch returns an `ErrIndexFingerprintMismatch` value whose
error message names (a) the drifted index, (b) which field differed
(`schema-hash` vs `version`), and (c) the stored and supplied values
so the operator can attribute the change at a glance:

```
gmdb: index "by_repository" fingerprint mismatch (schema-hash):
  stored=0x3f2a... supplied=0xc104... — caller must RebuildIndex
```

The caller's recovery path is `tx.RebuildIndex` — see Rebuild below
for the signature, which takes the `*IndexDecl` directly and bypasses
the fingerprint check that gated the failed open.

The schema hash catches structural drift (column add/remove/reorder,
unique flag flipped, covering changes). The user `Version` tag catches
extractor-logic drift that the engine cannot inspect (e.g., the
extractor now masks a column, returns entries in a different order,
or applies a different partial-index predicate). Bump `Version` after
any extractor change that produces different output for the same input.

The engine never auto-rebuilds. Auto-rebuild would silently double the
cost of an Open after a deploy and obscure the schema change in
operational logs.

The schema hash inputs are exclusively byte sequences with explicit
`uvarint` length prefixes — no `gob`, no JSON, no struct layout — so
the hash is deterministic across Go versions, build flags, and host
architectures.

### Column Encoding

The byte API treats `IndexEntry.Cols[i]` as opaque, lex-ordered bytes.
The caller is responsible for producing encodings whose byte order
matches the desired index order (e.g., big-endian for ordered
numerics).

The engine concatenates columns into a single index key using a
**NUL-escaped, NUL-terminated** scheme:

- Within each column's bytes, every `0x00` is escaped to `0x00 0xFF`.
- After each column's escaped bytes, the engine appends a `0x00 0x00`
  terminator.
- The full index key is the concatenation of escaped columns + their
  terminators, followed (for non-unique indexes) by the escaped row PK
  + a final `0x00 0x00`.

This encoding is prefix-free — no escaped column is a prefix of
another — so concatenated columns sort lex-correctly regardless of
contents (including columns containing arbitrary binary data with
embedded NULs).

**Worked example.** Two tuples to encode:

| Tuple | Col A | Col B | Encoded bytes |
|-------|-------|-------|---------------|
| T1 | `[]` (empty) | `[0x00]` | `00 00`  `00 FF 00 00` |
| T2 | `[0x00]` | `[]` (empty) | `00 FF 00 00`  `00 00` |
| T3 | `[0x00, 0xFF]` | `[0x00]` | `00 FF FF 00 00`  `00 FF 00 00` |

Byte-wise comparison: T1 < T2 < T3, matching the lex order of the
original tuples. The terminator `00 00` cannot appear inside any
escaped column (every internal `0x00` is followed by `0xFF`), so a
decoder finds column boundaries unambiguously.

The typed layer (`TypedIndex[K, V, IK]`) automates lex-safe encoding
via stable `Encoder[T]` implementations. See Typed Keyspaces.

### Storage Layout

Each keyspace has its own per-keyspace **index registry** — a B+tree
rooted at `IndexRegistryRoot` in the keyspace descriptor. Keys are
index names; values are the per-index descriptor:

```
Index Registry Entry (value bytes)
+----------------+----------------------------------+
| SchemaHash     | uint64                           |
| Unique         | uint8                            |
| Padding        | [7]byte                          |
| Root           | uint64    (index B+tree root)    |
| Count          | uint64    (entries in the index) |
| UserVersionLen | uint16                           |
| UserVersion    | bytes                            |
| ColumnCount    | uint16                           |
| For each col:                                     |
|   NameLen      | uint16                           |
|   Name         | bytes                            |
| CoveringCount  | uint16                           |
| For each col:                                     |
|   NameLen      | uint16                           |
|   Name         | bytes                            |
+----------------+----------------------------------+
```

Variable-length. Stored as a single byte string value in the index
registry tree. Padding after the `Unique` byte aligns the subsequent
`Root` / `Count` uint64s.

Each index's data lives in its own engine-internal keyspace
descriptor (`Kind = 2`) referenced indirectly through `Root` in the
registry entry. Internal keyspaces do not appear in the user
keyspace B+tree directly — their descriptors are reachable only via
the parent keyspace's index registry. This keeps the user-facing
keyspace namespace clean.

Index entries are stored as plain B+tree key-value pairs:

- **Unique index**: key = concatenated lex-safe columns; value =
  `(PK bytes, optional Covering bytes)`.
- **Non-unique index**: key = concatenated lex-safe columns + escaped
  PK; value = optional covering tuple.

The `Count` field on the index descriptor is maintained incrementally
on Put/Delete. `Stats()` returns it in O(1).

### Unique Indexes

When `Unique` is true, the engine rejects extractor output that would
introduce a duplicate index key. `Put` on the indexed keyspace returns
`ErrIndexUniqueViolation` (with the index name) instead of writing the
row.

Implementation: before writing index entries, the engine probes each
new index key. If found, abort with `ErrIndexUniqueViolation`. The
row write does not happen — the caller's `Put` returns the error and
the transaction can `Rollback()` or continue with other work.

A single extractor invocation may return multiple `IndexEntry`
values. If two of those entries produce the same index key for a
unique index, the `Put` is rejected with `ErrIndexUniqueViolation`
naming the offending key — the row is not written, no index entries
are written. The check happens against the candidate-set, so the
collision is detected even when the index keyspace is empty.

Unique indexes naturally model partial-unique constraints by combining
with extractor filtering: the extractor returns entries only for rows
matching the condition; uniqueness is enforced over the filtered set.

### Covering Indexes

When `Covering` is non-empty, the index entry value carries the
covering columns (in declaration order, concatenated with the same
NUL-escape scheme used for keys). `Lookup` returns covering bytes
directly, skipping the back-lookup to the row keyspace.

A covering column declaration is identified by its `Name`; the
extractor populates `IndexEntry.Cover[i]` with the corresponding lex-
safe bytes. The schema hash includes covering column names in
declaration order, so adding/removing/reordering covering columns
triggers `ErrIndexFingerprintMismatch`.

**Names are semantic anchors, not positional labels.** Covering and
indexed column names are inputs to the schema hash specifically to
catch *structural* changes. They do not catch the case where a caller
reuses the same name for a column whose meaning has changed (e.g.,
renaming `"price"` to `"qty"` and `"qty"` to `"price"`, then
populating each with the other's value — schema hash unchanged,
stored entries silently decode into the wrong logical columns). That
case requires bumping `Version` — the `Version` tag exists precisely
to catch logic-level drift the engine cannot see.

### Partial Indexes

The extractor returns an empty `[]IndexEntry` for rows that should not
be indexed. The engine does not write any entries for those rows. On
Update, the old and new entry sets are diffed: an entry present in the
old set but absent from the new is deleted; one present in the new but
absent from the old is inserted.

There is no separate "predicate" primitive — the extractor *is* the
predicate. Simpler API, equivalent expressive power.

### Lookup API

```go
type Index struct { /* unexported */ }

// Lookup returns matching (pk, value) pairs for an exact match on the
// declared columns. If the index is covering and covers the requested
// columns, value is read from the index entry's covering bytes;
// otherwise value is fetched via back-lookup against the row keyspace.
// Iteration ends when no more matches; check Err() for errors.
//
// Intra-transaction consistency: index cursor and back-lookup both
// read the current transaction's dirty state. Row writes and index
// updates happen atomically in the same Put/Delete/Cursor.Delete,
// so a back-lookup for an index entry always finds the row. If a
// back-lookup ever fails to find its PK (engine bug or external
// corruption), the entry is silently skipped from iteration and the
// inconsistency is reportable via Check().
func (idx *Index) Lookup(cols ...[]byte) iter.Seq2[[]byte, []byte]

// LookupKeys returns matching primary keys without back-lookup or
// covering decode. Iteration cost is O(matches) leaf scans only.
// Because LookupKeys never probes the row keyspace, it does not
// observe missing-PK inconsistencies (the silent-skip case noted
// on Lookup) — every index entry yields its raw PK, even if the
// corresponding row has somehow vanished. Use Check() for
// row/index consistency verification.
func (idx *Index) LookupKeys(cols ...[]byte) iter.Seq[[]byte]

// Range returns matches in [start, end). start and end are slices of
// per-column tuples; nil tuple = open-ended.
func (idx *Index) Range(start, end []TupleValue) iter.Seq2[[]byte, []byte]

// Prefix returns matches whose leading columns match the given prefix.
func (idx *Index) Prefix(leadingCols ...[]byte) iter.Seq2[[]byte, []byte]

// Get is shorthand for unique indexes: returns the single (pk, value)
// for an exact column match, or ErrNotFound. Returns
// ErrIndexNotUnique when called on a non-unique index.
func (idx *Index) Get(cols ...[]byte) (pk, value []byte, err error)

// Err returns the first error encountered during iteration of the
// last sequence returned by Lookup / Range / Prefix.
//
// Index handles are not safe for concurrent use by multiple
// goroutines. The Err state is per-handle, so two overlapping
// iterators on the same *Index would race. Open the keyspace in
// separate transactions, or call ks.Index(name) once per goroutine,
// for concurrent index queries.
func (idx *Index) Err() error
```

Both `Lookup` and `LookupKeys` are provided. `Lookup` is the default
API; `LookupKeys` is the escape hatch for cost-sensitive callers
iterating large result sets where the back-lookup or covering decode
is unnecessary.

In the API Surface below, `Range` takes `[][]byte` per side (one
slice per declared column; the slice itself is `nil` for an
open-ended bound).

### Write Path: Atomic Index Maintenance

For an indexed keyspace, every `Put`, `Delete`, and `Cursor.Delete`
operation is wrapped:

**Put(key, newValue):**
1. Read the existing value at `key` (if present), call it `oldValue`.
2. Call `extract(key, oldValue)` → `oldEntries` (empty list if no
   existing row).
3. Call `extract(key, newValue)` → `newEntries`.
4. Diff `oldEntries` and `newEntries`: compute deletes (in old, not in
   new) and inserts (in new, not in old).
5. For each unique-index insert, probe the index for an existing
   entry; conflict ⇒ return `ErrIndexUniqueViolation` (no row write,
   no index write).
6. Apply index deletes.
7. Apply index inserts (each writes to the index's internal keyspace).
8. Write the row to the main keyspace.
9. Update each index's `Count` in the registry.

All steps happen in the same CoW transaction. A failure at any step
(including the unique probe) leaves the transaction in a consistent
state — either rolled back, or continuing with the row unchanged.

**Delete(key):**
1. Read the existing value at `key` (if present). Absent ⇒ no-op.
2. Call `extract(key, oldValue)` → `oldEntries`.
3. Delete all entries in `oldEntries` from their indexes.
4. Decrement each affected index's `Count`.
5. Delete the row.

**Cursor.Delete():** same as Delete but uses the cursor's current
key/value (already in-hand).

### Bulk Operations on Indexed Keyspaces

`DeleteRange(start, end)` on an indexed keyspace **does not** use
the O(pages) subtree-retirement fast path. The engine cannot retire
a subtree without knowing the prior-index-keys for every row in it
(the extractor output depends on the row's value, which the
subtree-retirement walk does not visit).

Implementation: the engine iterates the range with a cursor, calling
`Delete()` for each row. Cost is O(entries × (indexes + extractor)).
The cursor must remain stable across CoW + rebalance triggered by the
per-row deletes — `Cursor.Delete()` followed by `Cursor.Next()` is
defined to correctly resume at the post-delete successor (see Cursor
State Machine in the Cursor API).

This is the same cost a SQL engine pays for `DELETE … WHERE … IN
range` with secondary indexes. Predictable and correct.

Callers needing the O(pages) fast path on indexed data can:
- Drop the indexes before the bulk operation, run `DeleteRange`, then
  rebuild the indexes (`tx.RebuildIndex`).
- Or use `DeleteKeyspace` to drop the whole keyspace (which also
  drops its indexes — engine cleans up internal index keyspaces and
  the per-keyspace index registry).

### Rebuild

```go
// RebuildIndex drops the named index's data and re-runs the extractor
// supplied in decl over every row in the keyspace, writing fresh
// index entries. Blocking — runs inside the current write transaction.
// The previous index is preserved until commit; mid-rebuild crash
// leaves the old index intact.
//
// decl.Name must match the name of an index already declared on
// the keyspace (the registry entry's stored Name). The supplied
// decl replaces the stored SchemaHash and Version on success; this
// is the canonical recovery path after ErrIndexFingerprintMismatch
// because the rebuild bypasses the open-time fingerprint check.
//
// The keyspace itself is opened internally for cursor iteration
// without re-validating other indexes' fingerprints. If the same
// transaction also needs to open the keyspace for writes, it must
// supply matching IndexDecls for every still-drifted index — or
// call RebuildIndex once per drifted index before calling
// OpenKeyspace.
//
// decl.Extract MUST be non-nil; a nil Extract returns
// ErrIndexExtractorRequired (admin tools that need to rebuild
// without the application's extractor functions cannot —
// reconstruction requires the extractor logic, by definition).
func (tx *Tx) RebuildIndex(keyspace string, decl *IndexDecl) error
```

**Recovery pattern after `ErrIndexFingerprintMismatch`.** A single
`OpenKeyspace` call reports drift on *one* index at a time (the
first mismatch encountered while iterating the declared set). When
multiple indexes have drifted simultaneously — common during a
schema-bumping deploy — the recovery requires a loop: rebuild the
named index, retry the open, rebuild whichever index the *next*
mismatch names, retry, until OpenKeyspace succeeds. The decl set
passed to OpenKeyspace stays constant; only the RebuildIndex calls
fire in succession:

```go
// defer tx.Rollback() — partial rebuilds discard cleanly if the
// loop exits with an error.
decls := []*IndexDecl{byRepoDecl, activeLeaseDecl}
for {
    ks, err = tx.OpenKeyspace("workspaces", decls...)
    if err == nil {
        break
    }
    var fpErr *IndexFingerprintError
    if !errors.As(err, &fpErr) {
        return err // some other error — propagate
    }
    // Find the matching decl by Name and rebuild.
    var d *IndexDecl
    for _, candidate := range decls {
        if candidate.Name == fpErr.IndexName {
            d = candidate
            break
        }
    }
    if d == nil {
        return fmt.Errorf("drifted index %q not in supplied decls", fpErr.IndexName)
    }
    if err := tx.RebuildIndex("workspaces", d); err != nil {
        // Distinguish recoverable from terminal:
        //  - ErrTxTooLarge → the keyspace exceeds MaxTxBufferBytes;
        //    rollback and use BulkLoad / a chunked rebuild instead.
        //  - ErrIndexUniqueViolation → the new extractor produces
        //    duplicates that the unique constraint rejects; the
        //    extractor logic is wrong, fix it and retry in a fresh tx.
        //  - Any other error (I/O, ErrDBFull, ...): hard failure;
        //    rollback and bubble up.
        return err
    }
}
// ks is now usable.
```

**Recovery on RebuildIndex failure.** A `RebuildIndex` call that
returns an error leaves the transaction in a partially-rebuilt
state — earlier indexes in the loop iteration may already have their
new SchemaHash/Version staged for commit, while the failing index
was rolled back to its prior state. The transaction is **not** safe
to commit in that state; the caller must `tx.Rollback()` (the
`defer` above) and start a fresh transaction. Specifically:

- `ErrTxTooLarge` from RebuildIndex means the keyspace's row corpus
  exceeds `MaxTxBufferBytes` for a single rebuild. Use `BulkLoad`
  (which bypasses the slab) into a fresh keyspace, or chunk the
  rebuild manually across multiple write transactions using a
  shadow-index + cutover pattern.
- `ErrIndexUniqueViolation` means the new extractor produced
  duplicate keys that the unique constraint rejected. The extractor
  logic is wrong (or the partial-index predicate is wrong);
  rollback, fix the extractor in source, redeploy, retry.
- Any other error (I/O, `ErrDBFull`, etc.) is a hard failure;
  rollback and surface upstream.

A degenerate-but-safe simplification for callers that don't care
about per-index reporting is to call `RebuildIndex` for *every*
declared index unconditionally on first mismatch — at the cost of
rebuilding indexes that may not have drifted.

Implementation:
1. Allocate a new internal index keyspace (fresh root page).
2. Cursor-iterate the parent keyspace. The internal cursor sees the
   current write transaction's dirty state — rows Put earlier in the
   same transaction are included in the rebuilt index. For each row,
   run the extractor from `decl` and write entries into the new
   index keyspace. For unique indexes, any extractor-produced
   duplicate aborts the rebuild with `ErrIndexUniqueViolation` —
   the rebuild does not commit and the existing registry entry is
   unchanged.
3. Update the registry entry: new `Root`, new `Count`, new
   `SchemaHash` (computed from `decl`), new `UserVersion` (from
   `decl.Version`). The old internal index keyspace's pages enter
   `tx.retiredPages`.
4. On `tx.Commit()`, the new index becomes active; old pages reclaim
   via the RPL.

`Index.Stats()` called on a handle to the still-rebuilding index
returns the *old* registry entry's count and tree statistics until
the transaction commits — the new index is invisible until the
registry write in step 3 lands at commit. A caller calling Stats()
mid-RebuildIndex therefore sees the pre-rebuild state, not an
intermediate.

`RebuildIndex` runs in one write transaction. For very large
keyspaces this may exceed `MaxTxBufferBytes` — the rebuild fails
with `ErrTxTooLarge` and the caller must use `BulkLoad` instead (see
BulkLoad → Interaction with Indexes), or chunk the rebuild manually.

### Indexes on SetKeyspaces

A SetKeyspace can carry indexes. The extractor signature is the same
`func(key, value []byte) []IndexEntry`, but it runs **per (key, value)
set member**, not per top-level key. The "primary key" in non-unique
index entries is the (key, value) pair — neither alone identifies the
set member.

**Compound-PK encoding.** Because the column terminator `0x00 0x00`
is already used to delimit columns in the index key, the PK's
internal split between its `key` and `value` halves uses a distinct
separator `0x00 0x01`. The PK is encoded as:

```
escape(key) || 0x00 0x01 || escape(value)
```

then appended to the index key (after the trailing `0x00 0x00`
column terminator), followed by a final `0x00 0x00` to terminate the
PK portion. `0x00 0x01` is lex-safely distinguishable from both the
column terminator (`0x00 0x00`) and any escaped byte sequence
(`0x00 0xFF`), and never appears inside an escaped column (the only
`0x00` bytes in an escaped column are immediately followed by
`0xFF`). The full grammar for a non-unique SetKeyspace index key:

```
indexKey := escapedCol (0x00 0x00 escapedCol)* 0x00 0x00 escapedPK 0x00 0x00
escapedPK := escape(setKey) 0x00 0x01 escape(setValue)
```

A decoder splits the index key on the first `0x00 0x00` after the
last column terminator, then splits the PK on `0x00 0x01` to recover
`(setKey, setValue)`.

`Cursor.Delete()` on a set keyspace deletes one set member; index
updates affect only that member's contribution. `Delete(key)` on a
set keyspace removes all members; index updates run the extractor on
each removed (key, value) pair.

Bulk-free of a key's nested B+tree (via `Delete(key)`) reverts to a
per-member walk when the SetKeyspace has indexes — same reasoning as
`DeleteRange` on indexed keyspaces.

### Open Semantics

Two distinct open functions:

```go
// OpenKeyspace opens a keyspace for read+write. Requires every
// declared index on the keyspace to be supplied with a matching
// IndexDecl. Missing or extra IndexDecls return
// ErrIndexExtractorRequired or ErrIndexUnknown. Drift returns
// ErrIndexFingerprintMismatch (caller must RebuildIndex).
func (tx *Tx) OpenKeyspace(name string, indexes ...*IndexDecl) (*Keyspace, error)

// OpenKeyspaceReadOnly opens a keyspace for reads only. No IndexDecls
// required (and none accepted — pass them via OpenKeyspace if you
// want write access). Index lookups still work — they read stored
// index entries directly.
func (tx *Tx) OpenKeyspaceReadOnly(name string) (*Keyspace, error)
```

Strict — opening for write without the extractors is unrepresentable.
Two open functions instead of "open succeeds, writes error" because
the failure-at-open path:
- Surfaces drift / missing extractors immediately, before any work.
- Lets backup/inspector/read-only-tools open without schema awareness,
  using `OpenKeyspaceReadOnly`.
- Avoids the "open succeeded, but every subsequent write fails" state
  that's easy to miss in operational settings.

`OpenSetKeyspace` / `OpenSetKeyspaceReadOnly` follow the same pattern.

A keyspace handle returned from `OpenKeyspaceReadOnly` rejects all
mutating operations with `ErrReadOnly`. Index lookups, cursor reads,
and range iteration work normally.

**Re-opening a keyspace in the same transaction.** A second
`OpenKeyspace` call for the same name within one transaction:

- If the supplied IndexDecl set is identical to the first call's set
  by **all hashable inputs** — names, Unique flags, schema hashes,
  Versions, and (for typed indexes) encoder IDs — returns the
  *same* `*Keyspace` handle (idempotent).
- If the supplied IndexDecl set differs by any hashable input —
  even by one decl — returns `ErrKeyspaceAlreadyOpen` with the
  conflicting index name(s). Indexes declared on a keyspace are
  pinned for the lifetime of the transaction at first open.

**First-Extract-wins.** Go function values are not comparable, so
the `Extract` function pointer is NOT part of the hashable-inputs
comparison. Two `OpenKeyspace` calls with structurally identical
IndexDecls but **different** `Extract` functions are treated as
identical: the first call's `Extract` is registered and wins for
all subsequent index maintenance within the transaction; the second
call's `Extract` is silently dropped.

The two callers receive the *same* `*Keyspace` handle by design
(idempotent re-open), so writes from either caller through that
shared handle go through the first-registered `Extract`. If both
goroutines legitimately want distinct extractor behaviors, the only
correct pattern is **separate transactions** — there is no
in-transaction recovery path because index maintenance is pinned
at first open. Forcing recognition via a hashable input (typically
bumping `Version`) is *not* recovery: it converts the second call
to `ErrKeyspaceAlreadyOpen` (the schema-hash now differs), which
also doesn't yield a working second handle in the same txn.

If two callers happen to share a transaction and one needs a
different extractor, the design treats this as a coordination
requirement at the caller layer (typically: route writes for that
keyspace through a single owner in each transaction). The shared
`*Keyspace` handle is not safe for concurrent goroutine use anyway —
the per-handle contract is single-goroutine — so the coordination
needed here is the same coordination already needed for any shared
keyspace access within one txn.

Mixing `OpenKeyspace` and `OpenKeyspaceReadOnly` for the same name in
one transaction is also rejected with `ErrKeyspaceAlreadyOpen`. The
rationale: the read-only handle and the read-write handle have
different operational contracts (Extractors required vs. forbidden;
Put/Delete allowed vs. ErrReadOnly), and pinning one shape per
transaction keeps the per-keyspace open-registry invariants simple.
Callers needing both shapes use separate transactions.

### Removing an Index

```go
func (tx *Tx) DropIndex(keyspace, indexName string) error
```

Removes the index entry from the per-keyspace registry and retires the
index's internal keyspace pages. Future `OpenKeyspace` calls must omit
the corresponding `IndexDecl`, or a fresh declaration with the same
name re-creates the index empty (next `Put` populates it as rows are
written; existing rows are NOT auto-indexed — call `RebuildIndex` if
you want existing rows indexed).

### Statistics

`Index.Stats()` returns the index's persistent count + B+tree
statistics (depth, pages). Iteration via `Lookup` does not count
under-the-hood pages read; that comes from `Tx.Stats()`.

## BulkLoad

`BulkLoad` constructs a keyspace's B+tree bottom-up from a sorted
input stream, bypassing the per-key insert path entirely. Targets two
concrete scenarios:

- **gitfs**: one-shot migration of SQLite tables into gmdb at first
  open.
- **notes**: initial import of a corpus from filesystem dumps.

### API

```go
// BulkLoad replaces the contents of an empty keyspace with the
// sorted key-value stream produced by yield. Input MUST be in
// strictly ascending lex key order; a non-ascending key returns
// ErrBulkLoadOutOfOrder.
//
// The keyspace must be empty (Count == 0); otherwise returns
// ErrBulkLoadNonEmpty. Use ks.DeleteRange(nil, nil) to clear first
// if necessary.
//
// For indexed keyspaces, BulkLoad runs the index extractor on every
// row and bulk-loads each index in parallel using the same bottom-up
// algorithm. Indexes are written to fresh index keyspace roots; the
// existing index keyspace data is retired at commit. Unique-index
// violations abort the BulkLoad with ErrIndexUniqueViolation; nothing
// is written to disk before the abort because all bulk-loaded pages
// are at fresh page IDs invisible until the meta swap.
//
// For SetKeyspaces, input is a stream of (key, value) pairs in
// (key, value) lex order; duplicate (key, value) pairs are silently
// deduplicated.
//
// BulkLoad bypasses the per-txn slab budget: pages are pwritten
// directly to fresh page IDs as they are constructed, not buffered.
// Memory usage is O(depth × pageSize), independent of input size.
//
// Returns the number of input pairs written.
func (ks *Keyspace) BulkLoad(yield func(yield func(key, value []byte) bool)) (uint64, error)
func (ks *SetKeyspace) BulkLoad(yield func(yield func(key, value []byte) bool)) (uint64, error)
```

### Algorithm

Standard bottom-up B+tree construction:

1. Allocate a fresh leaf page from `pageAlloc()`. Fill with input
   entries (prefix-compressed with the keyspace's `RestartGroupTarget`)
   until the page is full or the input is exhausted.
2. When a leaf page is full, pwrite it directly to its allocated page
   ID (slab bypass — see below), free the page-sized scratch buffer
   back to the buffer pool, and start a new leaf page.
3. Each completed leaf contributes one (separator, pageID) pair to the
   in-progress branch page at the level above.
4. Recurse: when a branch page is full, write it directly and start a
   new branch at that level.
5. When input is exhausted, finalize all in-progress branches up to
   the root.
6. Set the keyspace descriptor's `Root` to the final root page ID.
   Increment `Count` by the input count.
7. At commit, the keyspace's old pages (the empty tree's root or a
   prior population) are retired and the meta page is swapped.

For each level of the tree there is exactly one in-progress page at a
time. Memory is O(depth × pageSize) — for a depth-5 tree at 4KB pages,
20 KB.

### Slab Bypass

Bulk-loaded pages are written to disk as they are completed, not held
in the slab. The pwrite goes to a fresh page ID — invisible until the
meta swap commits the new tree, so the partial write is safe (a crash
before commit leaves the pages as unreferenced "leaked" pages in the
bitmap, reclaimed by the next maintenance pass exactly like any other
crash leakage).

This bypass keeps memory usage flat regardless of input size and makes
BulkLoad the recommended path for inputs that would otherwise exceed
`MaxTxBufferBytes`.

### Interaction with Indexes

For an indexed keyspace, the engine runs the extractor on every row
and accumulates index entries per index. Each index's entries are
**re-sorted** to lex order (the extractor may produce entries in
arbitrary order even if rows are sorted by primary key) and bulk-loaded
into a fresh index keyspace using the same algorithm. The sort is
external (chunked sort with disk-spill if needed; chunk size bounded
by `MaxTxBufferBytes`).

When the sort fits in memory the indexes load in a single in-memory
pass. When it does not, spill chunks are written to a per-DB scratch
directory (configurable via `Options.ScratchDir`, default `os.TempDir`)
and merge-sorted in the final pass. Scratch files are best-effort
deleted on success and failure; an unremovable scratch file (e.g.,
`ScratchDir` on a vanishing tmpfs) is logged via `slog.Logger` and
does not fail the operation. A spill *write* failure (ENOSPC on
`ScratchDir`) aborts the BulkLoad with the underlying I/O error
wrapped; no rebuilt index entries are committed.

**Unique-violation detection happens at the merge output** — the
external sort's final merge pass yields entries in sorted order, and
the bulk-loader observes the first adjacent-duplicate pair as it
consumes the stream. Detection therefore happens *during* the index
pwrite phase, not before it:

- For in-memory sorts (the row count fits in `MaxTxBufferBytes`),
  the sort completes before any index-page pwrite, so the first
  duplicate is found *before* the index pwrite phase starts — the
  abort is fully reversible at the index layer.
- For spilling sorts, the merge output is consumed interleaved with
  index-page pwrites; the first duplicate may be found after some
  index pages have already been pwritten.

Either way, when `ErrIndexUniqueViolation` fires, BulkLoad returns
naming the index and offending key. Any pages already pwritten to
disk — row pages, index pages, RPL segments — are at fresh page IDs
**unreferenced by the un-swapped meta**. They become bounded leakage
reclaimed by the next bitmap-leak reclamation pass (see Background
Maintenance), identical in mechanism to any other mid-commit crash
leakage. The transaction's caller observes a clean error and can
roll back; the on-disk state is consistent with the pre-BulkLoad
meta.

**Leakage scale warning.** "Bounded" here refers to crash-safety
(no UB, no tree corruption), not magnitude. For a spilling-sort
BulkLoad that aborts on a late index unique violation, the row
corpus is *already on disk* as unreferenced pages — leakage is
O(input size), potentially gigabytes for a large migration (e.g.,
gitfs SQLite-→-gmdb import). Background maintenance's bitmap-leak
reclamation does reclaim it, but only on its next scheduled pass;
in the meantime the leaked pages are invisible to the allocator
(bits clear in the on-disk bitmap until the reclamation pass sets
them), so subsequent write transactions cannot reuse the space.
Callers performing large BulkLoads that may fail should trigger
`CheckWithOptions(&CheckOptions{Repair: true})` or wait for a
maintenance pass before retrying.

A two-pass (validate-then-load) mode that guarantees "no pwrite
before violation detection" even for spilling sorts is straightforward
to add later as an option; v1 ships the single-pass merge-output
detection above.

### Atomicity

BulkLoad is a transactional operation. It runs inside a write
transaction and only takes effect on commit. Either the entire load
(keyspace data + all index data + count updates) commits atomically,
or none of it does. Mid-BulkLoad crash leaks pages exactly as a
mid-commit crash does — bounded leakage reclaimed by background
maintenance.

## API Surface

```go
// Sentinel errors.
var (
    ErrNotFound                = errors.New("gmdb: key not found")
    ErrKeyExists               = errors.New("gmdb: key already exists")
    ErrDBFull                  = errors.New("gmdb: database full (MaxSize reached)")
    ErrTxTooLarge              = errors.New("gmdb: transaction too large")
    ErrReadersFull             = errors.New("gmdb: no reader slots available")
    ErrKeyTooLarge             = errors.New("gmdb: key exceeds maximum size")
    ErrKeyEmpty                = errors.New("gmdb: key is nil or empty")
    ErrCorrupted               = errors.New("gmdb: database corrupted")
    ErrBadPageChecksum         = errors.New("gmdb: page checksum mismatch")
    ErrVersionMismatch         = errors.New("gmdb: format version mismatch")
    ErrReadOnly                = errors.New("gmdb: write operation on read-only transaction")
    ErrTxClosed                = errors.New("gmdb: transaction already committed or rolled back")
    ErrCursorUnpositioned      = errors.New("gmdb: cursor not positioned")
    ErrKeyspaceKindMismatch    = errors.New("gmdb: keyspace kind does not match existing keyspace")
    ErrKeyspaceReserved        = errors.New("gmdb: keyspace name reserved for engine use")
    ErrValueSizeMismatch       = errors.New("gmdb: value size does not match fixed value size")

    // Indexing.
    ErrIndexExtractorRequired   = errors.New("gmdb: index extractor required for OpenKeyspace")
    ErrIndexUnknown             = errors.New("gmdb: IndexDecl supplied for index not declared in registry")
    ErrIndexFingerprintMismatch = errors.New("gmdb: index fingerprint mismatch — RebuildIndex required")
    ErrIndexUniqueViolation     = errors.New("gmdb: unique index violation")
    ErrIndexNotUnique           = errors.New("gmdb: Get called on non-unique index")
    ErrIndexExists              = errors.New("gmdb: index already exists")
    ErrIndexNotFound            = errors.New("gmdb: index not found")
    ErrIndexEncoderIDEmpty      = errors.New("gmdb: typed index encoder returned empty ID() — encoder IDs must be unique non-empty strings")

    // Keyspace lifecycle.
    ErrKeyspaceAlreadyOpen      = errors.New("gmdb: keyspace already opened in this transaction with a different index set")
    ErrKeyspaceClosed           = errors.New("gmdb: keyspace handle is invalid (keyspace deleted in this transaction)")

    // Compact.
    ErrCompactReadersActive     = errors.New("gmdb: Compact drain timed out — in-process read transactions still active")

    // BulkLoad.
    ErrBulkLoadOutOfOrder       = errors.New("gmdb: BulkLoad input not in ascending key order")
    ErrBulkLoadNonEmpty         = errors.New("gmdb: BulkLoad requires an empty keyspace")
)

// ErrIndexFingerprintMismatch is returned wrapped in an
// *IndexFingerprintError whose fields name the drifted index and
// distinguish schema-hash vs version-tag drift.
//
// Field is the discriminant; callers MUST inspect Field before
// reading the corresponding pair:
//   - Field == "schema-hash" → StoredHash and SuppliedHash are valid;
//     StoredVersion and SuppliedVersion are empty strings (not
//     meaningful).
//   - Field == "version" → StoredVersion and SuppliedVersion are
//     valid; StoredHash and SuppliedHash are zero (not meaningful).
// The zero values for the inactive pair are NOT a real hash/version
// collision — they are sentinel placeholders. Callers logging the
// error should branch on Field, not on uint64==0 or string=="".
type IndexFingerprintError struct {
    Keyspace        string
    IndexName       string
    Field           string // "schema-hash" or "version"
    StoredHash      uint64 // valid when Field == "schema-hash"
    SuppliedHash    uint64 // valid when Field == "schema-hash"
    StoredVersion   string // valid when Field == "version"
    SuppliedVersion string // valid when Field == "version"
}

func (e *IndexFingerprintError) Error() string { /* ... */ }
func (e *IndexFingerprintError) Unwrap() error { return ErrIndexFingerprintMismatch }

// Open a database. Creates the file if it doesn't exist.
//
// The data file is created with O_CREATE|O_EXCL to prevent races when
// multiple processes call Open() simultaneously on a non-existent
// path. If exclusive create fails with EEXIST, Open() retries as a
// normal open. The lock file uses the same pattern.
func Open(path string, opts *Options) (*DB, error)
```

### Byte Slice Ownership

All `[]byte` slices returned by gmdb (from `Get`, `Cursor.Next`,
`Cursor.Seek`, etc.) are **borrowed references** — they point into
either the mmap, the writer's slab buffer (when reading own writes
in a write txn), or an internal cursor buffer. The caller does not
own them.

**Value slices** point directly into the mmap (for inline values from
committed pages) or into overflow pages in the mmap, or into the
writer's slab buffer (for inline values from same-txn modifications).
Valid until the **transaction closes** (`Commit()` or `Rollback()`).

**Key slices** may point into the mmap (for keys at restart points in
prefix-compressed leaves), into a slab buffer, or into the cursor's
key reconstruction buffer (`keyBuf`). The reconstruction buffer is
reused on each cursor movement. Key slices are valid until the
**next cursor operation** or transaction close, whichever comes first.

**Slab buffer lifetime guarantee.** Within a write transaction, a
value or key slice that points into a slab buffer (own-writes read
path) remains valid for the entire transaction even if the page that
buffer represented is subsequently CoW'd, rebalanced, or freed within
the same transaction. The pager **does not** return a slab buffer to
its `sync.Pool` until commit or rollback, even when the buffer
becomes a loose page mid-transaction. Loose-page tracking
(`tx.loosePages`) only removes the page ID from active routing — the
underlying `[]byte` remains alive and untouched until tx close. This
preserves the "valid until tx close" contract under any intra-tx
mutation pattern. The cost is bounded by `MaxTxBufferBytes`, since
every slab buffer in the transaction (live or loose) counts against
the same budget.

Callers who need a key or value to outlive these scopes must copy it:

```go
k, v := c.Next()
savedKey := bytes.Clone(k)
savedVal := bytes.Clone(v)
```

`Keyspace.Get()` returns a value slice; valid until transaction close.

This contract is the standard for mmap-based B+tree databases (LMDB,
libmdbx, BoltDB). Zero-copy reads are a core performance property.

### Nil and Empty Semantics

**Keys:** empty (`[]byte{}`) and nil keys are both **invalid**. Any
operation taking a key returns `ErrKeyEmpty` if the key is nil or empty.

**Values:** empty (`[]byte{}`) are **valid**. A key can exist with no
associated data — useful for using a keyspace as a set of keys. Nil
values are treated as empty: `Put(key, nil)` stores a zero-length value.

**Return value conventions:**

| Call | Key exists (empty value) | Key exists (non-empty) | Key not found | End of iteration |
|------|--------------------------|------------------------|---------------|------------------|
| `Keyspace.Get(k)` | `([]byte{}, nil)` | `(value, nil)` | `(nil, ErrNotFound)` | N/A |
| `Cursor.Next()` | `(key, []byte{})` | `(key, value)` | N/A | `(nil, nil)` |
| `Cursor.Err()` | — | — | — | non-nil if iteration ended due to error |

Nil return from `Get` always means "not found" with `ErrNotFound`. Nil
return from cursor navigation always means "end of iteration" (check
`Err()` to distinguish normal end from error). Empty `[]byte{}` return
from `Get` means "key exists, value is empty."

### Database Initialization

When `Open()` creates a new database:

1. Create the data file with `O_CREATE|O_EXCL`. If `EEXIST`, retry as
   normal open.
2. Write both meta pages identically:
   - TxnID = 0
   - HighWaterMark = 2 + BitmapPages
   - KeyspaceRoot = 0 (empty)
   - NumKeyspaces = 0
   - RPLHeadPage = 0, RPLTailPage = 0, RPLEntryCount = 0
   - NumFreePages = 0
   - Checkpoint flag set on both
   - File format fields from `Options.FileFormat` (or defaults)
   - UUID via `crypto/rand`
3. Initialize the bitmap region: all bits clear.
4. Create the lock file with `O_CREATE|O_EXCL`, matching UUID, empty
   reader table.
5. `fdatasync` the data file.

The first write transaction increments TxnID to 1.

### Path Traversal Safety

`Open()` uses `os.OpenRoot` (Go 1.24+) to confine file operations to
the database directory:

```go
root, err := os.OpenRoot(filepath.Dir(path))
defer root.Close()
dataFile, err := root.Open(filepath.Base(path), ...)
lockFile, err := root.Open(filepath.Base(path)+".lock", ...)
```

`os.OpenRoot` rejects symlink traversal outside the root directory.
Prevents an attacker who controls the database path from redirecting
file operations to arbitrary locations via symlinks. Without this, a
symlink at the database path could cause `Open()` to create or
overwrite files outside the intended directory.

Used for both the data file and the lock file during `Open()`. After
return, resolved fds are used directly and the `os.Root` is closed.

### Types and Options

```go
// SyncMode controls the durability guarantees of committed transactions.
type SyncMode int

const (
    SyncDurable    SyncMode = iota // syncs data + meta. Full ACID. Default.
    SyncDataOnly                   // syncs data; not meta. Last txn may be lost on crash.
    SyncLazy                       // skips all syncs. Rolls back to last Checkpoint() on crash.
    SyncUnsafe                     // skips all syncs, no safety net. Requires AllowSyncUnsafe.
)

// Options for opening a database.
type Options struct {
    // PageSize in bytes. Only used when creating a new database.
    // Must be a power of 2 in [4096, 65536]. Default: 4096.
    PageSize int

    // PageChecksum enables xxhash64 footers on data pages. Stored as
    // a flag in the meta page — immutable after creation. Default: true.
    // Only used when creating; ignored when opening existing.
    PageChecksum bool

    // FileFormat controls database file size bounds and growth.
    // Only used when creating; modify via Tx.SetFileFormat() at runtime.
    FileFormat FileFormat

    // SyncMode controls durability. Default: SyncDurable.
    SyncMode SyncMode

    // AllowSyncUnsafe must be true when using SyncUnsafe mode.
    // Without it, Open() returns an error when SyncMode = SyncUnsafe.
    // Default: false.
    AllowSyncUnsafe bool

    // MaxReaders is the maximum number of concurrent reader slots.
    // Default: 4096. Only used when creating a new lock file.
    MaxReaders int

    // MaxTxBufferBytes bounds the per-write-transaction slab (live +
    // loose + commit-time assembly buffers). A write transaction
    // that dirties more pages than this fails the next CoW (or
    // step 0 of commit) with ErrTxTooLarge.
    //
    // Sizing guide: each Put/Delete on an indexed keyspace with I
    // indexes can CoW up to depth × (I + 1) pages in the worst case
    // (row tree + each index tree, one CoW per level). At 4 KB
    // pages, depth 5, and 3 indexes: ~80 KB of unique CoW
    // destinations per maximally-touching Put; the 256 MiB default
    // accommodates ~3,000–3,200 such Puts before ErrTxTooLarge. For
    // larger workloads, use BulkLoad (which bypasses the slab via
    // streaming pwrite) or chunk the work across multiple write
    // transactions. Default: 256 MiB.
    MaxTxBufferBytes int64

    // RestartGroupTarget is the engine-wide default for the leaf
    // prefix-compression restart interval. Per-keyspace overrides via
    // Tx.SetKeyspaceConfig(). Default: 16.
    RestartGroupTarget int

    // MergeThreshold is the B+tree page fill percentage below which a
    // page is merged with a sibling after deletion. Range: 1-50.
    // Default: 25.
    MergeThreshold int

    // LaggingReader is called when a long-lived reader is blocking
    // RPL reclamation during page allocation. If nil, pageAlloc()
    // falls through to file extension when reclamation is blocked.
    LaggingReader func(info LaggingReaderInfo) LaggingReaderAction

    // MaxBatchSize is the maximum number of Batch() calls collected
    // before executing in one transaction. Default: 1000.
    MaxBatchSize int

    // MaxBatchDelay is the maximum time to wait for additional
    // Batch() calls before executing the current batch. Set to 0 to
    // fire immediately. Default: 10ms.
    MaxBatchDelay time.Duration

    // StaleTimeout for cross-PID-namespace stale detection via
    // heartbeats. Default: 10s.
    StaleTimeout time.Duration

    // LockRetryInterval is the polling interval the flock goroutine
    // uses when flock(LOCK_EX|LOCK_NB) returns EWOULDBLOCK. Bounds
    // both Close() shutdown latency and per-writer ctx cancellation
    // latency under cross-process write-lock contention. Default: 50ms.
    LockRetryInterval time.Duration

    // Logger for diagnostic messages. If nil, discarded.
    Logger *slog.Logger

    // FileMode for newly created files. Default: 0644.
    FileMode os.FileMode

    // PreloadPages calls madvise(MADV_POPULATE_READ) at open
    // (Linux 5.14+). Default: false.
    PreloadPages bool

    // HugePages calls madvise(MADV_HUGEPAGE) on the data mmap
    // (Linux). Default: false.
    HugePages bool

    // ReclaimOnClose calls madvise(MADV_COLD) on the accessed mmap
    // region when a read transaction closes (Linux 5.4+).
    // Default: false.
    ReclaimOnClose bool

    // ReadOnly opens the database in read-only mode: lock file not
    // opened for write, flock goroutine not started, write
    // transactions return ErrReadOnly. The data mmap is always
    // PROT_READ regardless.
    //
    // When ReadOnly is true and the data file does not exist, Open()
    // returns the underlying os.ErrNotExist (it never creates a
    // database in read-only mode — that would be a contradiction).
    // Default: false.
    ReadOnly bool

    // ScratchDir is the directory used for BulkLoad sort spill on
    // indexed keyspaces. Must be on the same filesystem as the
    // database file when Compact() is used (atomic rename
    // requirement). Default: os.TempDir().
    ScratchDir string

    // CompactDrainTimeout bounds how long Compact() waits for active
    // in-process read transactions to commit/rollback before
    // proceeding with the copy. Exceeded → Compact returns
    // ErrCompactReadersActive without doing any work. Default: 30s.
    CompactDrainTimeout time.Duration

    // Maintenance controls the background maintenance goroutine.
    // If nil, defaults are used (maintenance enabled, 5m interval).
    Maintenance *MaintenanceOptions
}

// FileFormat controls file size bounds and growth/shrink behavior.
// All sizes are in bytes and must be multiples of PageSize.
type FileFormat struct {
    // Lower is the minimum file size in bytes. File never shrinks below.
    // Default: (2 + BitmapPages) * PageSize.
    Lower uint64

    // Upper is the maximum file size in bytes. Determines mmap
    // reservation size and bitmap size. Must be a multiple of PageSize.
    // Immutable after creation. Default: 256 GiB.
    Upper uint64

    // GrowStep is the number of bytes to grow by when extending.
    // Must be a multiple of PageSize. Default: 256 MiB.
    GrowStep uint64

    // ShrinkThreshold is the minimum unused bytes at file tail before
    // shrink occurs. Must be a multiple of PageSize. Default: 512 MiB.
    ShrinkThreshold uint64
}

// LaggingReaderInfo describes a reader blocking RPL reclamation.
type LaggingReaderInfo struct {
    PID       uint64
    TxnID     uint64
    Lag       uint64 // number of transactions behind current
    HeldPages uint64 // estimated pages held unreclaimable
}

type LaggingReaderAction int

const (
    LaggingReaderWait  LaggingReaderAction = iota // retry; reader may release
    LaggingReaderAbort                            // abort with ErrDBFull
)
```

### Database and Transaction API

```go
// DB is a handle to an open database.
type DB struct { ... }

func (db *DB) Close() error

// Checkpoint flushes all outstanding writes to stable storage. In
// SyncLazy mode this creates a checkpoint (database will roll back to
// this point at most on crash). In SyncDurable/SyncDataOnly modes,
// no-op (commits already sync). In SyncUnsafe, syncs but does not
// retroactively fix ordering from prior commits.
//
// Checkpoint acquires the write lock for its duration via the flock
// goroutine's FIFO queue; it serializes with concurrent write
// transactions and Compact(). Concurrent reads are not affected.
//
// The context governs the wait for the write lock — if Compact() is
// running ahead of this call in the queue, the wait can be long
// (Compact takes the lock for CopyTo's full duration). Callers on a
// timer (periodic Checkpoint in a service) should pass a context
// with a deadline. Once Checkpoint has the lock, ctx is not checked
// further — the fsync + pwrite sequence completes unconditionally
// (it is bounded and short relative to a Compact wait).
func (db *DB) Checkpoint(ctx context.Context) error

// View executes a read-only transaction. The context governs slot
// acquisition only — once the callback is entered, the context is not
// checked by the engine. Long-scan cancellation is a caller concern:
// the supplied fn can capture ctx and poll it (ctx.Err()) at natural
// break points (e.g., between cursor pages, between key ranges) and
// return early if cancelled. For request-driven services, the right
// pattern is one short View per request, not a long View polled for
// cancellation.
func (db *DB) View(ctx context.Context, fn func(tx *Tx) error) error

// Update executes a read-write transaction.
func (db *DB) Update(ctx context.Context, fn func(tx *Tx) error) error

// Batch submits a write operation to be batched with other concurrent
// callers into a single transaction. The context governs the wait for
// batch inclusion. Each closure runs in its own child transaction and
// executes exactly once. See Write Batching.
//
// The closure MUST NOT call Commit() or Rollback() on the supplied
// *Tx — the batch coordinator owns child-transaction lifecycle. A
// closure that calls either causes the coordinator's subsequent
// child-commit-or-rollback to error with ErrTxClosed, which
// propagates to the caller as the closure's result.
func (db *DB) Batch(ctx context.Context, fn func(tx *Tx) error) error

// Begin starts a transaction manually. The context governs lock/slot
// acquisition; once Begin returns a *Tx the context is not stored.
func (db *DB) Begin(ctx context.Context, writable bool) (*Tx, error)

// Tx is a database transaction.
type Tx struct { ... }

func (tx *Tx) Commit() error
func (tx *Tx) Rollback() error

// BeginChild creates a child transaction within the current write
// transaction. Children can be committed (merged into parent) or
// rolled back (discarded) independently. Only valid on a write txn.
func (tx *Tx) BeginChild() (*Tx, error)

// SetFileFormat updates the file format. MaxSize is immutable and
// cannot be changed; returns an error if FileFormat.Upper differs.
// Only valid on a write transaction.
func (tx *Tx) SetFileFormat(f FileFormat) error
```

### Keyspace API

```go
// OpenKeyspace opens an existing single-value keyspace for read+write.
// Every declared index on the keyspace must be supplied as an
// IndexDecl. Missing indexes return ErrIndexExtractorRequired; extras
// return ErrIndexUnknown; drifted fingerprints return
// ErrIndexFingerprintMismatch.
func (tx *Tx) OpenKeyspace(name string, indexes ...*IndexDecl) (*Keyspace, error)

// OpenKeyspaceReadOnly opens an existing keyspace for reads only.
// No IndexDecls required (and none accepted). Index lookups still work.
func (tx *Tx) OpenKeyspaceReadOnly(name string) (*Keyspace, error)

// CreateKeyspace creates a new single-value keyspace and (optionally)
// declares indexes. Returns ErrKeyExists if the keyspace exists.
func (tx *Tx) CreateKeyspace(name string, indexes ...*IndexDecl) (*Keyspace, error)

// CreateKeyspaceIfNotExists opens the keyspace if it exists (matching
// indexes required) or creates it (with the supplied indexes).
// ErrKeyspaceKindMismatch if it exists as a SetKeyspace.
func (tx *Tx) CreateKeyspaceIfNotExists(name string, indexes ...*IndexDecl) (*Keyspace, error)

// OpenSetKeyspace, OpenSetKeyspaceReadOnly, CreateSetKeyspace,
// CreateSetKeyspaceIfNotExists follow the same pattern.
type SetKeyspaceOptions struct {
    FixedValueSize int
}

func (tx *Tx) OpenSetKeyspace(name string, indexes ...*IndexDecl) (*SetKeyspace, error)
func (tx *Tx) OpenSetKeyspaceReadOnly(name string) (*SetKeyspace, error)
func (tx *Tx) CreateSetKeyspace(name string, opts *SetKeyspaceOptions, indexes ...*IndexDecl) (*SetKeyspace, error)
func (tx *Tx) CreateSetKeyspaceIfNotExists(name string, opts *SetKeyspaceOptions, indexes ...*IndexDecl) (*SetKeyspace, error)

// DeleteKeyspace removes a keyspace and everything reachable from
// its descriptor as a single atomic CoW operation. Three sub-trees
// are retired together:
//
//   1. The keyspace's own B+tree (row data, including SetKeyspace
//      nested B+trees for value sets) — bulk subtree retirement.
//   2. Each engine-internal index keyspace (Kind=2) referenced from
//      the per-keyspace index registry — bulk subtree retirement
//      per index.
//   3. The per-keyspace index registry sub-tree itself (rooted at
//      IndexRegistryRoot in the keyspace descriptor) — bulk
//      subtree retirement.
//
// All three retirements happen inside the same write transaction.
// The keyspace descriptor is then removed from the keyspace B+tree
// (which propagates CoW to the meta page's KeyspaceRoot). On commit,
// the meta swap publishes all of (1)+(2)+(3)+descriptor-removal
// atomically; a mid-DeleteKeyspace crash leaves the prior meta
// active and none of the work visible.
//
// For indexed keyspaces, no per-row extractor call is needed because
// no index entries survive after step (2).
//
// Errors: ErrNotFound if the keyspace does not exist.
// ErrKeyspaceReserved if the supplied name is an engine-internal
// index keyspace (Kind=2 — not enumerable, not user-deletable).
//
// Any Keyspace/SetKeyspace/Cursor/Index handle previously opened on
// the named keyspace within this transaction is invalidated by
// DeleteKeyspace — subsequent operations on those handles return
// ErrKeyspaceClosed. **Re-creating the keyspace in the same
// transaction via CreateKeyspace does NOT reactivate the old
// handle**: invalidation is permanent for the handle's lifetime.
// The new CreateKeyspace returns a fresh *Keyspace; the old handle
// stays dead until it is dropped by the caller.
func (tx *Tx) DeleteKeyspace(name string) error

// ListKeyspaces returns the names of all user keyspaces (Kind=0
// Keyspace or Kind=1 SetKeyspace). Engine-internal index keyspaces
// (Kind=2) are filtered out — they are addressable only via their
// parent keyspace's index registry, not by name.
func (tx *Tx) ListKeyspaces() ([]string, error)

// SetKeyspaceConfig updates mutable per-keyspace settings.
// Currently only RestartGroupTarget. Returns an error for invalid
// values (e.g. RestartGroupTarget = 0 means engine default).
// Only valid on a write transaction.
func (tx *Tx) SetKeyspaceConfig(name string, cfg KeyspaceConfig) error

type KeyspaceConfig struct {
    RestartGroupTarget uint16 // 0 = leave unchanged
}

// RebuildIndex drops and re-populates the named index using the
// supplied IndexDecl (whose Name must match an existing registry
// entry on the keyspace). Bypasses the open-time fingerprint check;
// this is the recovery path after ErrIndexFingerprintMismatch.
// Blocking — runs inside the current write transaction. See the
// Indexing → Rebuild section for the recovery pattern.
func (tx *Tx) RebuildIndex(keyspace string, decl *IndexDecl) error

// DropIndex removes the named index entirely.
func (tx *Tx) DropIndex(keyspace, indexName string) error

// Keyspace is a handle to a named single-value keyspace.
type Keyspace struct { ... }

func (ks *Keyspace) Get(key []byte) ([]byte, error)
func (ks *Keyspace) Put(key, value []byte) error
func (ks *Keyspace) Delete(key []byte) error
func (ks *Keyspace) DeleteRange(start, end []byte) (uint64, error)
func (ks *Keyspace) NextSequence() (uint64, error)
func (ks *Keyspace) Cursor() *Cursor

// Index returns a handle for querying the named index on this
// keyspace. Returns ErrIndexNotFound if no index with this name is
// registered.
//
// For handles returned by OpenKeyspace: the engine resolves against
// the IndexDecl set supplied to OpenKeyspace, which is enforced to
// match the on-disk registry (every on-disk index must be supplied
// as an IndexDecl, else ErrIndexExtractorRequired). ErrIndexNotFound
// therefore means the supplied name is genuinely absent from the
// registry — not a timing or race condition.
//
// For handles returned by OpenKeyspaceReadOnly: no IndexDecls were
// supplied; the engine resolves the name against the on-disk index
// registry directly. ErrIndexNotFound means no index with this name
// is registered on disk. Index reads (Lookup/LookupKeys/Range/
// Prefix/Get) do not need an extractor — they read stored index
// entries — so a read-only handle's Index resolution and queries
// work identically to a read-write handle's.
//
// The returned *Index is not safe for concurrent use; see
// Index.Err godoc.
func (ks *Keyspace) Index(name string) (*Index, error)

func (ks *Keyspace) BulkLoad(yield func(yield func(key, value []byte) bool)) (uint64, error)

// Cursor for iterating over key-value pairs. See "Cursor State
// Machine" below for state semantics, Delete-post-state rules, and
// invalidation conditions.
type Cursor struct { ... }

func (c *Cursor) First() (key, value []byte)
func (c *Cursor) Last() (key, value []byte)
func (c *Cursor) Next() (key, value []byte)
func (c *Cursor) Prev() (key, value []byte)
func (c *Cursor) Seek(target []byte) (key, value []byte)
func (c *Cursor) SeekGE(target []byte) (key, value []byte)
func (c *Cursor) Current() (key, value []byte)

// Delete removes the current entry. Cursor must be Positioned;
// otherwise returns ErrCursorUnpositioned. After delete, advances
// to the next entry or transitions to End-of-iteration. Possible
// errors: ErrCursorUnpositioned, ErrReadOnly (on a read-only
// transaction or keyspace handle), ErrTxClosed, ErrKeyspaceClosed
// (parent keyspace deleted), ErrIndexUniqueViolation (only on
// indexed keyspaces if the engine's bookkeeping discovers an
// inconsistency).
func (c *Cursor) Delete() error

func (c *Cursor) Err() error

// SetKeyspace handle to a named set keyspace.
type SetKeyspace struct { ... }

func (ks *SetKeyspace) Has(key []byte) (bool, error)
func (ks *SetKeyspace) HasValue(key, value []byte) (bool, error)
func (ks *SetKeyspace) Put(key, value []byte) error
func (ks *SetKeyspace) Delete(key []byte) error
func (ks *SetKeyspace) DeleteValue(key, value []byte) error
func (ks *SetKeyspace) CountValues(key []byte) (uint64, error)
func (ks *SetKeyspace) DeleteRange(start, end []byte) (uint64, error)
func (ks *SetKeyspace) NextSequence() (uint64, error)
func (ks *SetKeyspace) Cursor() *SetCursor
func (ks *SetKeyspace) Index(name string) (*Index, error)
func (ks *SetKeyspace) BulkLoad(yield func(yield func(key, value []byte) bool)) (uint64, error)

// SetCursor for iterating over set keyspace key-value pairs.
type SetCursor struct { ... }

// Core navigation (same as Cursor).
func (c *SetCursor) First() (key, value []byte)
func (c *SetCursor) Last() (key, value []byte)
func (c *SetCursor) Next() (key, value []byte)
func (c *SetCursor) Prev() (key, value []byte)
func (c *SetCursor) Seek(target []byte) (key, value []byte)
func (c *SetCursor) SeekGE(target []byte) (key, value []byte)
func (c *SetCursor) Current() (key, value []byte)
func (c *SetCursor) Delete() error
func (c *SetCursor) Err() error

// Value navigation (within current key's set).
func (c *SetCursor) FirstValue() []byte
func (c *SetCursor) LastValue() []byte
func (c *SetCursor) NextValue() (value []byte)
func (c *SetCursor) PrevValue() (value []byte)
func (c *SetCursor) NextKey() (key, value []byte)
func (c *SetCursor) PrevKey() (key, value []byte)
func (c *SetCursor) SeekValue(target []byte) (value []byte)
func (c *SetCursor) CountValues() (uint64, error)
```

#### Cursor State Machine

Every cursor (Keyspace `Cursor`, SetCursor, TypedCursor) is at any
moment in exactly one of three states:

| State | Meaning | Behavior |
|-------|---------|----------|
| Unpositioned | Cursor was created but never moved, or was Reset | `Current()` returns `(nil, nil)` and `Err()` returns `ErrCursorUnpositioned`. `Next`/`Prev` from this state behaves like `First`/`Last`. |
| Positioned | Cursor refers to an existing entry | `Current()` returns `(key, value)`. `Next`/`Prev`/`Seek*` move; `Delete()` removes the entry and transitions per the rules below. |
| End-of-iteration | Last `Next`/`Prev`/`Seek*`/`Delete()` advanced past the end | `Current()` returns `(nil, nil)` (no error). `Err()` returns nil (normal end). The next `Next`/`Prev` returns `(nil, nil)` again. `First`/`Last`/`Seek*` re-positions. |

Distinguishing end-of-iteration from unpositioned: end-of-iteration's
`Err()` is nil; unpositioned-state `Err()` is `ErrCursorUnpositioned`.

**`Cursor.Delete()` post-delete state** (single-value Keyspace cursor):
- Cursor must be Positioned. Otherwise returns `ErrCursorUnpositioned`.
- After successful delete, the cursor advances to the entry that
  followed the deleted entry. If no such entry exists, the cursor
  transitions to End-of-iteration (subsequent `Next` returns
  `(nil, nil)`, `Err()` is nil).
- The cursor stack tolerates CoW + rebalance triggered by the delete:
  `Next()` after `Delete()` is the supported pattern and always
  resumes correctly at the post-delete successor.
- Possible errors: `ErrReadOnly` (cursor on a read-only txn or
  read-only keyspace), `ErrCursorUnpositioned`, `ErrTxClosed`.

**`SetCursor.Delete()` post-delete state**:
- Cursor must be Positioned on a (key, value) pair.
- Deletes the current value from the current key's set.
- If the deleted value was not the last value for the key, advances
  to the next value for the same key.
- If the deleted value was the last value for the key, the key itself
  is removed (empty sets never exist) and the cursor advances to the
  first value of the next key. If there is no next key, transitions
  to End-of-iteration.
- The cursor stack tolerates CoW + rebalance triggered by the
  key-removal case — the same guarantee as `Cursor.Delete()` — so
  `Next()` after `Delete()` always resumes correctly at the
  post-delete successor, including across leaf splits and merges
  caused by the parent-keyspace key removal.
- Same error set as `Cursor.Delete()`.

**Cursor invalidation by `DeleteKeyspace`.** Calling
`tx.DeleteKeyspace(name)` invalidates every cursor and Index handle
previously opened on that keyspace within the same transaction.
Subsequent use of an invalidated cursor or Index returns
`ErrKeyspaceClosed`. The caller is responsible for not retaining
handles past a `DeleteKeyspace` call.

### Range Iterators

```go
// Read-only iterators on Keyspace.
func (ks *Keyspace) All() iter.Seq2[[]byte, []byte]
func (ks *Keyspace) Range(start, end []byte) iter.Seq2[[]byte, []byte]
func (ks *Keyspace) Prefix(prefix []byte) iter.Seq2[[]byte, []byte]

// On SetKeyspace, each (key, value) pair yields separately.
func (ks *SetKeyspace) All() iter.Seq2[[]byte, []byte]
func (ks *SetKeyspace) Range(start, end []byte) iter.Seq2[[]byte, []byte]
func (ks *SetKeyspace) Prefix(prefix []byte) iter.Seq2[[]byte, []byte]
```

### Index Lookup API

```go
type Index struct { /* unexported */ }

// Lookup returns (pk, value) pairs matching the exact column tuple.
// value is read from the index's covering bytes when the index covers
// the requested column set; otherwise via back-lookup to the row
// keyspace. Iteration ends when no more matches.
func (idx *Index) Lookup(cols ...[]byte) iter.Seq2[[]byte, []byte]

// LookupKeys returns just primary keys — no back-lookup, no covering
// decode. Use for cost-sensitive iteration over large result sets.
func (idx *Index) LookupKeys(cols ...[]byte) iter.Seq[[]byte]

// Range returns matches in [start, end). Each tuple is a slice of
// per-column byte slices; nil tuple = open-ended.
func (idx *Index) Range(start, end [][]byte) iter.Seq2[[]byte, []byte]

// Prefix returns matches whose leading columns match the prefix.
func (idx *Index) Prefix(leadingCols ...[]byte) iter.Seq2[[]byte, []byte]

// Get is shorthand for unique indexes: returns the single (pk, value)
// or ErrNotFound. Returns ErrIndexNotUnique when called on a
// non-unique index.
func (idx *Index) Get(cols ...[]byte) (pk, value []byte, err error)

// Err returns the first error encountered during the last sequence
// returned by Lookup / Range / Prefix.
func (idx *Index) Err() error

// Stats returns the index's persistent count + tree statistics.
func (idx *Index) Stats() (IndexStats, error)
```

### Statistics

```go
type DBStats struct {
    FreePages     uint64
    RetiredPages  uint64
    FileSize      uint64
    MinSize       uint64
    MaxSize       uint64
    HighWaterMark uint64
    // ActiveReaders is a non-atomic scan of the lock-file reader
    // table (cluster-wide). The count can be off by ±N for N reader
    // transitions in flight during the scan. Use for metrics and
    // health diagnostics only — never as a synchronization barrier
    // ("ActiveReaders == 0" does NOT imply no reads are starting).
    ActiveReaders int
    MaxReaders    int

    // SlabBytes reports slab usage for THIS PROCESS's current write
    // transaction (0 when no write txn is open in this process).
    // Cross-process writer slab usage is not visible from any one DB
    // handle — only the holder of the cross-process write lock has a
    // local view of it. Aggregate cluster-wide slab usage is not
    // tracked.
    SlabBytes int64
}

func (db *DB) Stats() DBStats

type TxStats struct {
    CowPages       uint64
    LoosePages     uint64
    ReclaimedPages uint64
    WrittenPages   uint64 // data + bitmap + meta pages pwritten at commit

    // SlabPeakBytes is the maximum slab usage observed during the
    // transaction's lifetime. Useful for tuning MaxTxBufferBytes.
    //
    // Reset behavior is a deliberate choice, not a forced contract:
    // Rollback resets the value to 0 because the rolled-back work is
    // not representative of steady-state need (rollbacks are
    // exceptional and the peak they reach should not influence
    // tuning); Commit preserves it so the caller can read it
    // immediately before the *Tx becomes invalid. Tooling that
    // wants visibility into rolled-back peaks should snapshot
    // SlabPeakBytes from a Stats() call inside the txn before
    // calling Rollback.
    SlabPeakBytes int64

    Gets    uint64
    Puts    uint64
    Deletes uint64
    Splits  uint64
    Merges  uint64

    // Indexing.
    IndexEntriesInserted uint64
    IndexEntriesDeleted  uint64
    IndexUniqueProbes    uint64

    Duration time.Duration
}

func (tx *Tx) Stats() TxStats

type KeyspaceStats struct {
    Depth         int
    BranchPages   uint64
    LeafPages     uint64
    OverflowPages uint64
    Entries       uint64
    IndexCount    int
}

func (ks *Keyspace) Stats() (KeyspaceStats, error)
func (ks *SetKeyspace) Stats() (KeyspaceStats, error)

type IndexStats struct {
    Depth         int
    BranchPages   uint64
    LeafPages     uint64
    Entries       uint64
    Unique        bool
    Covering      bool
    SizeBytes     uint64
}
```

### Check, CopyTo, Compact

```go
type CheckSeverity int

const (
    CheckWarning CheckSeverity = iota // non-critical (e.g., suboptimal layout)
    CheckError                        // structural integrity violation
    CheckFatal                        // walk could not continue past this point
)

type CheckIssue struct {
    Severity CheckSeverity
    // Code is a stable, machine-parseable token for the issue class
    // (e.g., "BitmapLeak", "CheckIndexes.KeyspaceNotFound",
    // "BadPageChecksum", "RPLChainBroken"). Stable across gmdb
    // versions for the purposes of tooling that pattern-matches on
    // issues; new codes may be added but existing ones never change
    // meaning. Use Code for programmatic decisions; use Message for
    // human-facing display.
    Code     string
    PageID   uint64
    Keyspace string
    Index    string // empty for non-index issues
    // Message is a human-readable description of the issue. Free-form
    // and free to change between versions; do NOT pattern-match on it.
    Message  string
    Repaired bool
}

// Check performs a structural integrity walk. Verifies meta + page
// checksums, B+tree integrity, bitmap consistency, RPL chain, page
// accounting, prefix-compression integrity, keyspace descriptor
// consistency, and set keyspace subpage / nested B+tree integrity.
// Returns issues as an iter.Seq.
//
// Walk failures (I/O errors, unreadable pages) are reported as
// CheckFatal severity and are always the last issue yielded.
//
// Check internally opens a read transaction. The transaction is
// released when the iterator is exhausted OR when the caller
// abandons iteration (a runtime.AddCleanup attached to the iter.Seq
// closure releases the reader slot on GC). Callers iterating to
// completion always see the slot released promptly; callers that
// break early should not assume immediate release.
func (db *DB) Check() iter.Seq[CheckIssue]

type CheckOptions struct {
    // Repair enables offline repair: reclaims leaked pages in the
    // bitmap. Requires exclusive access (no concurrent readers or
    // writers).
    Repair bool

    // CheckIndexes additionally verifies that stored index entries
    // match what the supplied extractors would produce. Re-runs every
    // extractor over every row — O(rows × indexes). Off by default.
    //
    // When true, Indexes below MUST contain an IndexDecl set for each
    // indexed keyspace whose indexes should be verified. Indexed
    // keyspaces absent from the map are skipped for the
    // extractor-equivalence check (structural integrity is still
    // verified) and reported as a CheckWarning with
    // Code = "CheckIndexes.KeyspaceNotSupplied". Mismatched
    // fingerprints for a supplied IndexDecl are reported as
    // CheckError issues with Code = "CheckIndexes.FingerprintDrift"
    // and the offending index name — they do NOT abort the walk and
    // do NOT trigger a rebuild.
    CheckIndexes bool

    // Indexes supplies extractors for the CheckIndexes mode, keyed by
    // keyspace name. Ignored when CheckIndexes is false.
    //
    // Entries in Indexes whose keyspace name does not exist in the
    // database are reported as CheckWarning with
    // Code = "CheckIndexes.KeyspaceNotFound". Entries whose
    // IndexDecl.Name does not match any index registered on the
    // existing keyspace are reported as CheckWarning with
    // Code = "CheckIndexes.IndexNotInRegistry". Both surface common
    // misconfiguration (typos, out-of-date callers) instead of
    // silently skipping.
    Indexes map[string][]*IndexDecl
}

func (db *DB) CheckWithOptions(opts *CheckOptions) iter.Seq[CheckIssue]

// CopyTo creates a consistent copy at the given path. Taken from a
// read-tx snapshot — writers are not blocked. When compact is true,
// the copy is compacted: free pages omitted, B+tree pages written
// sequentially. Inherits source's PageSize, BitmapPages, MaxSize.
// To change file format, re-open the copy and use SetFileFormat.
func (db *DB) CopyTo(path string, compact bool) error

// Compact rebuilds the database file in place. CopyTo(compact=true)
// to a temporary file in the same directory, then atomic rename.
//
// Coordination protocol (Compact is the most invasive single-process
// operation; the caller does not have to "ensure no transactions are
// open" — Compact arranges this itself):
//
//  1. Acquire the cross-process write lock via the flock goroutine,
//     blocking concurrent writers and Checkpoint() for the duration.
//  2. Wait up to Options.CompactDrainTimeout (default 30s) for active
//     read transactions in THIS process to commit/rollback. If any
//     read transaction remains after the timeout, abort with
//     ErrCompactReadersActive (no copy is started, no file rename).
//  3. Open a read snapshot at the current TxnID and run
//     CopyTo(tmpPath, compact=true) — writers in other processes are
//     blocked (the cross-process flock is held), but reads in other
//     processes that already opened a snapshot continue to work
//     against the original inode via their existing mmap; new
//     read-open attempts from other processes during this window
//     succeed against the original inode (open() resolves before
//     rename).
//  4. fsync(tmpPath); atomic rename(tmpPath, originalPath); reopen
//     the file descriptors and mmap; release the cross-process write
//     lock.
//
// Cross-process readers post-rename: pre-rename readers' mmap still
// references the original inode (rename unlinks the directory entry
// but the inode stays alive until the last mapping is released);
// SIGBUS is not possible. Post-rename openers (in other processes)
// observe the new inode via a fresh open() — their UUID check
// matches (Compact preserves UUID), so coordination continues
// normally. There is no observable inconsistency window for
// cross-process readers.
//
// Effects: reclaims leaked pages; defragments file; shrinks to
// minimum size.
//
// Requires enough free disk space for the temporary copy (up to the
// size of the live data) on the SAME filesystem as originalPath
// (otherwise the atomic rename degrades to a copy + delete, breaking
// atomicity).
//
// Fallback when Compact() returns ErrCompactReadersActive: long-lived
// readers cannot be drained in this process. Use
// CopyTo(path, compact=true) instead — it runs from a read snapshot
// without draining in-process readers and produces an offline
// compacted copy you can swap in during scheduled downtime.
func (db *DB) Compact() error
```

### Typed Keyspaces (Generics)

Higher-level API on top of the byte-oriented `Keyspace`. Provides
type-safe access by handling key/value serialization via the
`Encoder[T]` interface.

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
func (t *TypedKS[K, V]) Delete(key K) error
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
// — see Cursor State Machine. The third bool in the navigation
// methods is `ok` (false when the cursor is at end-of-iteration or
// unpositioned); Err() distinguishes those two states.
func (c *TypedCursor[K, V]) Delete() error
func (c *TypedCursor[K, V]) Err() error
```

The typed layer is a **zero-cost abstraction** at the API level — all
methods delegate to the underlying `Keyspace` and `Cursor` methods
with `Encoder` calls. `AppendEncode` follows the standard Go append
pattern, allowing callers to pass reusable buffers (e.g., from
`sync.Pool`) to eliminate per-call allocations.

**Key ordering constraint**: key encoder must produce byte sequences
whose lex order matches the desired key order. For `uint64` keys,
big-endian. For `string` keys, natural byte representation.

#### Typed Indexes

```go
// TypedIndex declares a typed index on TypedKeyspace[K, V] with
// extracted index key type IK.
type TypedIndex[K, V, IK any] struct {
    Name     string
    IKEnc    Encoder[IK]            // produces lex-safe bytes from IK
    Unique   bool
    Version  string                 // bump on extractor logic changes
    Extract  func(K, V) []IK        // empty slice ⇒ skip (partial index)
    Covering []TypedCoveringColumn  // optional (currently parameterized by name + Encoder)
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
    indexDecl() *IndexDecl
}

func (t *TypedIndex[K, V, IK]) indexDecl() *IndexDecl { /* implements AnyTypedIndex */ }

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

The schema-hash inputs for a typed index include the encoder IDs of
the index-key encoder and any covering encoders — so changing from
`be-uint64` to `varint-zigzag` for the same column triggers
`ErrIndexFingerprintMismatch` at Open.

A typed extractor returning multiple `IK` values models composite
indexes naturally (the `IK` type is itself a struct whose `Encoder[IK]`
produces the concatenated lex-safe bytes). For columns of different
types where a single `IK` struct is awkward, fall back to the
byte-oriented `IndexDecl` API.

**Limitation: partial-prefix queries through the typed API.** When
`IK` is a composite struct, the typed layer treats the whole IK as
one opaque column (one `Encoder[IK]` → one byte slice). Consequently
`TypedIndexQuery.Range(start, end *IK)` compares full IK values;
there is **no partial-prefix Range on a sub-field of IK** through the
typed API. Workarounds:

- Use the byte-oriented `IndexDecl` directly, declaring each
  sub-field as a separate `IndexColumn` (one column per sub-field).
  Byte-API `Range` and `Prefix` then accept per-column tuples and
  support partial prefixes naturally.
- Design `Encoder[IK]` so the desired prefix sort key is exactly a
  byte prefix of the full encoding; then callers can construct
  partial-key `IK` values that serialize to the desired prefix and
  pass them to `Range`. This requires careful encoding design and
  loses generality.

#### Engine-Provided Canonical Encoders

The engine ships canonical `Encoder[T]` implementations for common
column types. The full canonical set:

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

The transform is sign-bit XOR (`x ^ 0x8000000000000000`), not
zigzag — zigzag is a different protobuf-style transform that
interleaves negatives among positives and is *not* lex-preserving
for big-endian byte order. The naming uses plain `gmdb/be-intN` to
avoid the misleading "zigzag" label.

**Canonical engine encoder IDs are forever immutable.** Once shipped,
an engine-provided `ID()` string cannot change — any change to the
encoding logic for an existing ID would silently corrupt every
on-disk index built with the old encoder. If a bug is discovered in
a canonical encoder, the fix ships under a NEW ID (e.g.,
`"gmdb/be-int64/v2"`) with a separate type (e.g.,
`gmdb.BEInt64EncoderV2`); the old type and ID remain available for
backward read of existing indexes. Operators migrating from the
buggy encoder rebuild the affected indexes via `tx.RebuildIndex`
with the new typed decl. This convention extends to
application-defined encoders (`"<pkg>/<type>[/<version>]"` — bump
the version segment when the encoding logic changes; see
`Encoder.ID()` godoc).

**Empty encoder IDs on TypedKeyspace without indexes.** The
`Encoder.ID()` empty check fires only when an encoder is referenced
by a typed index's schema hash (`IKEnc`, covering encoders). The
key and value encoders on `TypedKeyspace[K, V]` *without* indexes
are not validated for empty IDs — a TypedKeyspace with no declared
indexes may use encoders with empty `ID()` without error. This is
inadvisable if indexes may be added later (declaring a typed index
that depends on the key encoder will then fail at OpenKeyspace with
`ErrIndexEncoderIDEmpty`); application code should set non-empty
encoder IDs as a matter of hygiene regardless.

## Implementation Layout

All code lives in a single `gmdb` package (flat, no sub-packages —
avoids circular dependency issues between tightly-coupled components
and keeps the public API to one import path). Organized by file:

| File | Responsibility |
|------|---------------|
| `page.go` | Page header encode/decode (8-byte header: Type uint8, Flags uint8, Count uint16, AdditionalPages uint32 — no PageID). xxhash64 footer (compute on write, verify on read) when PageChecksum enabled. Meta page encode/decode/validate (including file format fields, bitmap/RPL pointers, Flags). RPL segment encode/decode. |
| `branch.go` | Branch page format, cell directory binary search, prefix-truncated separator computation, insert, split. |
| `leaf.go` | Prefix-compressed leaf format with per-page `RestartInterval`, restart/delta entry encode/decode, restart-point binary search + linear group scan for lookup, delta recomputation on insert/delete, restart table rebuild, full-page re-encoding on split. Compressed-leaf splice operations (`tryInsertAtCompressed`, `tryDeleteAtCompressed`) for hot-path in-place edits without full decode+re-encode. Overflow references in both restart and delta entry formats. |
| `subpage.go` | Set keyspace subpage encode/decode (uncompressed inline list, variable or fixed-value-size). |
| `btree.go` | B+tree search, insert (CoW path from leaf to root, split with prefix-truncated separator computation), delete (CoW, merge/rebalance with configurable `MergeThreshold`, separator recomputation). Range delete: boundary path finding, interior subtree retirement, boundary leaf cleanup, rebalance. Set keyspace bulk free: recursive subtree retirement for nested B+trees. Set keyspace operations: subpage management (inline sorted list), nested B+tree promotion/demotion. All operations work on page byte slices, never Go heap objects. |
| `cursor.go` | Stateful cursor: stack of (pageID, index) pairs, key reconstruction buffer for incremental forward decoding, restart group cache for reverse traversal. SetCursor operations (key + intra-key value navigation). |
| `iter.go` | `iter.Seq2`-based read-only iterators (`All`, `Range`, `Prefix`) for both byte-oriented and typed APIs. |
| `bulkload.go` | Bottom-up B+tree construction from sorted input. Per-level in-progress page, direct pwrite of completed pages (slab bypass). Index sort + spill to `Options.ScratchDir`. |
| `alloc.go` | Allocation bitmap: two-level (detail + in-memory summary) at fixed page offsets, bit set/clear, contiguous-run search with `math/bits` intrinsics, LIFO hint tracking. Pending bitmap changes (`tx.pendingAllocs`, `tx.pendingFrees`). RPL: append-only singly-linked segment chain (per-segment TxnID + PageID arrays), in-memory segment list rebuilt at Open, whole-segment reclamation. Loose page tracking (hash map). Page allocation priority: loose → bitmap → RPL reclamation → lagging reader check → file extension. Tail page refund. Commit-time update: apply pending bitmap changes via pwrite, append retired pages to RPL. |
| `pager.go` | Single unified `Pager` type for read and write paths. `Page(id) []byte` resolves via `dirty[id]` then mmap. Writable pager owns the slab `map[uint64]*[]byte`, `dirtyBytes` accounting, `sync.Pool` of page-sized buffers, `MaxTxBufferBytes` enforcement. Read-only pager rejects mutating ops. CoW: allocate fresh page ID, copy old content into a slab buffer, mutate buffer. |
| `commit.go` | Commit-time pwrite ordering: dirty data pages → bitmap pages → meta. Per-`SyncMode` fdatasync placement. File shrink after commit point. |
| `fileformat.go` | File format management: grow/shrink bounds, growth step, shrink threshold. File growth via `ftruncate()`. File shrinkage at commit time after tail refund. `Tx.SetFileFormat()` (rejects `MaxSize` changes). |
| `mmap.go` | Read-only mmap of the data file (`MAP_SHARED \| PROT_READ`). `mprotect(PROT_READ)` after open. mmap reservation sized to `MaxSize`. Platform-agnostic interface; per-platform shims in `mmap_linux.go` / `mmap_darwin.go` / `mmap_freebsd.go` only for `madvise` and `mmap` syscall differences. No platform-conditional code in the commit path. |
| `mmap_linux.go` | Linux mmap/munmap syscalls. `MADV_POPULATE_READ` (Linux 5.14+, opt-in). `MADV_HUGEPAGE` (opt-in). `MADV_COLD` (Linux 5.4+, opt-in). |
| `mmap_darwin.go` | macOS mmap/munmap syscalls. No `msync(MS_SYNC)` in commit path — the writer never touches the mmap. |
| `lock.go` | Lock file creation and mmap (shared memory, `structs.HostLayout` structs, uint64 PIDs + process start times + PID namespace inodes + heartbeats). Writer lock (single flock goroutine with intra-process writer queue + flock cross-process + WriterPID/WriterStartTime/WriterPIDNamespace/WriterHeartbeat, context-aware, zero goroutine accumulation). Stale writer recovery (namespace-aware). Reader table: hint-based scan+CAS slot acquire, atomic store release, namespace-aware stale reader detection. Heartbeat goroutine (started at Open, stopped at Close, updates active slots every ~1s). Oldest-reader query for RPL reclamation. Lagging reader detection and callback invocation. |
| `process_linux.go` | `processStartTime(pid)`: reads `/proc/[pid]/stat` field 22. `pidNamespace()`: reads `/proc/self/ns/pid` inode via `os.Readlink`. Pure Go, no cgo. |
| `process_darwin.go` | `processStartTime(pid)`: sysctl `KERN_PROC_PID` → `kinfo_proc.kp_proc.p_starttime`. |
| `process_freebsd.go` | `processStartTime(pid)`: sysctl `KERN_PROC_PID` → `kinfo_proc.ki_start`. |
| `tx.go` | Read transaction: snapshot meta, acquire reader slot, read-only B+tree access, optional MADV_COLD on close. Write transaction: snapshot meta, acquire write lock, slab-based CoW (`tx.pendingAllocs`, `tx.pendingFrees`, `tx.cowPages`, `tx.loosePages`, `tx.retiredPages`), pager dirty map, commit (commit.go), rollback (release slab buffers, clear pending sets). Nested transactions: `BeginChild()` snapshots pending state and keyspace roots; child commit discards snapshot; child rollback releases child slab buffers + restores snapshot. Per-tx pooling of slab buffer pool via `sync.Pool`. Pool of `ReadTx` structs to reduce allocations on high-throughput read paths. Leak detection via `runtime.AddCleanup`. Stats accumulation. |
| `index.go` | Index registry per keyspace (sub-B+tree at `IndexRegistryRoot`). IndexDecl validation (schema-hash computation, fingerprint compare). NUL-escape column encoding (`escape`, `unescape`, terminator append). Index write path: extractor invocation on Put/Delete, diff old/new entry sets, unique-index probes, atomic application within the parent transaction. `Index` query type: `Lookup`, `LookupKeys`, `Range`, `Prefix`, `Get`. `RebuildIndex`, `DropIndex`. Index storage as engine-internal keyspaces (`Kind = 2`). |
| `db.go` | Open/Close (path traversal safety via `os.OpenRoot`). Environment setup (mmap with read-only mapping, `mprotect`, lock file, file format, AllowSyncUnsafe validation, MaxTxBufferBytes default, RestartGroupTarget default). DB handle leak detection via `runtime.AddCleanup`. Transaction lifecycle (Begin/Commit/Rollback, View/Update helpers). Write batching: `Batch()` channel, coordinator goroutine, per-closure child transactions. Keyspace management (Open/Create variants, `DeleteKeyspace`, `SetKeyspaceConfig`, `RebuildIndex`, `DropIndex`). Keyspace name interning via `unique.Handle[string]`. Checkpoint(). Check(). CheckWithOptions(). CopyTo(). Compact(). Background maintenance goroutine (bitmap leak reclamation, stale reader cleanup, checksum scrubbing, incremental compaction; coordinated across processes via `LastMaintenanceTime` in the lock file). |
| `typed.go` | `Encoder[T]` interface, `FuncEncoder[T]` adapter. `TypedKeyspace[K, V]` and `TypedKS[K, V]` generic wrappers with `iter.Seq2` iterators. `TypedCursor[K, V]`. `TypedIndex[K, V, IK]` and `TypedIndexQuery[K, V, IK]` for typed index declarations and queries. Delegates all operations to byte-oriented `Keyspace`/`Cursor`/`Index` with `Encoder` calls. |
| `errors.go` | Sentinel error definitions. |
| `stats.go` | DBStats, TxStats, KeyspaceStats, IndexStats types and collection. |

### Coding Conventions

**Default values via `cmp.Or`** (Go 1.22+):

```go
pageSize := cmp.Or(opts.PageSize, 4096)
maxReaders := cmp.Or(opts.MaxReaders, 4096)
maxBatchSize := cmp.Or(opts.MaxBatchSize, 1000)
maxTxBufferBytes := cmp.Or(opts.MaxTxBufferBytes, 256<<20)
restartGroupTarget := cmp.Or(opts.RestartGroupTarget, 16)
```

`cmp.Or` returns the first non-zero argument. Replaces verbose
`if field == 0 { field = default }` blocks throughout `Open()` and
transaction setup.

**Concurrency tests via `testing/synctest`** (Go 1.24+):

- **Batch coordinator**: verify `MaxBatchDelay` timeout fires at the
  correct time, batch collection fills to `MaxBatchSize`, per-closure
  child txns commit/rollback correctly — without `time.Sleep` or racy
  channel coordination.
- **Flock goroutine**: verify context cancellation while flock is
  pending correctly dequeues the writer.
- **Reader table**: verify concurrent slot acquisition via CAS under
  contention, stale reader detection clearing the correct slots.

**Read-only pager reuse**: read transactions reuse `*Pager` instances
via a `sync.Pool` on the DB. Each `ReadTx` is also pooled to avoid
per-transaction allocations under high read load.

**Slab buffers**: page-sized `[]byte` buffers are pooled via a
process-global `sync.Pool` on the DB. Returning a buffer to the pool
clears it (zero-fill); reuse avoids GC pressure for steady write
workloads.

## Limits

### Page Size

Configurable at creation. Power of 2 in [4KB, 64KB]. Stored in meta,
immutable. Default: 4096 bytes.

### Maximum Key Size

Determined by page size. A branch page must fit at least 2 keys to
allow splitting. Fixed overhead: 16 bytes (8-byte header + 8-byte
leftmost child pointer). Each key needs 4 bytes (cell directory) + key
bytes + 8 bytes (child pointer). Maximum key size approx
`(PageSize - 40) / 2`, less 4 bytes when PageChecksum is enabled
(8-byte footer instead of 4-byte CRC32C in previous designs):

| Page Size | Max Key Size (no checksum) | With PageChecksum (xxhash64) |
|-----------|----------------------------|------------------------------|
| 4KB       | ~2028 bytes                | ~2024 bytes                  |
| 8KB       | ~4076 bytes                | ~4072 bytes                  |
| 16KB      | ~8172 bytes                | ~8168 bytes                  |
| 64KB      | ~32748 bytes               | ~32744 bytes                 |

Enforced at `Put()`. Keys exceeding return `ErrKeyTooLarge`.

The limit applies to branch separator capacity. Leaf prefix
compression can store keys up to this size at restart points (full
keys). Delta entries store only the unshared suffix, so their on-disk
size is smaller, but the reconstructed full key must still fit the
branch limit.

### Maximum Value Size

Single-value keyspaces: inline values limited by leaf page free space.
Larger values automatically stored as overflow pages. No practical
upper limit (bounded by disk space and `MaxSize`).

### Maximum Value Size (Set Keyspaces)

Each value becomes a key in the nested B+tree (or entry in a subpage).
Maximum value size = maximum key size — approximately `(PageSize -
40) / 2`. Overflow pages are not used for set keyspace values. A
`Put()` with an over-sized value returns `ErrKeyTooLarge`.

### Maximum Index Key Size

Composite index key = NUL-escaped column tuple (+ PK suffix for
non-unique indexes). After escaping, the key is stored in the index
keyspace's leaf, subject to the same maximum key size as ordinary
keys. The escape encoding can up to double the byte count for
columns with many NULs; tooling should reject column values that
would exceed the limit at the declaration layer.

### Maximum Indexes Per Keyspace

Bounded only by the per-keyspace index registry tree's capacity
(thousands per keyspace at typical page sizes). The engine does not
enforce a hard limit — practical limits come from the cost of
running every extractor on every write.

## Checksums

gmdb uses a single hash algorithm across the entire file: **xxhash64**
(`github.com/cespare/xxhash/v2`, `xxhash.Sum64`). The same algorithm
covers the meta page (mandatory, always on) and data pages (optional,
on by default). One hash family means one implementation, one
performance profile, and no algorithm version flags.

### Meta Page Checksum (Always On)

Both meta pages carry an xxhash64 checksum of all preceding fields.
Mandatory, cannot be disabled. The meta page is the atomic commit
point — a torn write here would silently point to an inconsistent
tree. The checksum detects this and triggers fallback to the other
meta page.

Stored as the trailing `uint64` of the meta page payload (see Meta
Page format).

### Data Page Checksums (On by Default)

Data pages (branch, leaf, overflow, RPL segment) carry an 8-byte
xxhash64 footer in the last 8 bytes of the page when checksums are
enabled.

Enabled via `Options.PageChecksum = true` at creation. Default: true.
The setting is stored as a flag in the meta page's `Flags` field
(bit 0) and is **immutable after creation** — all pages in a checksummed
database have checksums; all pages in a non-checksummed database do
not.

The default is on (a deliberate change from earlier designs). xxhash64
is fast enough in software (no hardware-acceleration requirement
unlike CRC32C) that the cost is negligible compared to mmap page-fault
and B+tree traversal costs, and the protection against silent bitrot
on commodity filesystems (ext4 without `data=journal`, xfs without
checksums) is worth the 0.2% page-space overhead.

### Storage

```
Page (with checksum enabled)
+-----------------------+
| Page Header (8 bytes) |
+-----------------------+
| Page Content          |
| (PageSize - 16 bytes) |
+-----------------------+
| xxhash64 (8 bytes)    |  footer: hash of bytes 0 through PageSize-9
+-----------------------+
```

The footer keeps the page header at 8 bytes. Usable content shrinks
by 8 bytes when checksums are enabled — 0.2% at 4KB. The checksum
covers the entire page from byte 0 through `PageSize - 9` inclusive,
including the page header.

Bitmap pages do not carry checksums (no page header or footer; the
entire page is bitfield data). Bitmap integrity is guaranteed by the
CoW model and the meta page checksum (the meta references the bitmap
indirectly through `NumFreePages` and the page-allocation invariants
that `Check()` verifies).

### Algorithm: xxhash64

`xxhash.Sum64` from `github.com/cespare/xxhash/v2`. Pure Go,
SIMD-accelerated on amd64/arm64 where the compiler can vectorize.

- ~4 ns per 64 bytes; ~50–80 ns per 4KB page in practice.
- Faster than CRC32C in pure software; competitive with CRC32C+SSE4.2
  on amd64.
- 8-byte output — slightly larger than CRC32C's 4 bytes but a stronger
  hash and consistent with the meta page checksum.

The same library and algorithm power the meta page checksum, so the
runtime cost is amortized across one hash implementation in the binary.

### Verification (Read Path)

When checksums are enabled, every page read from the pager is verified
on first access in a transaction:

1. Compute xxhash64 of bytes 0 through `PageSize - 9`.
2. Compare with the 8-byte footer.
3. Mismatch ⇒ return `ErrBadPageChecksum` with the page ID.

Per-page verification is cached on the pager — a page verified once in
a transaction is not re-verified on subsequent accesses within the
same transaction. For a depth-4 lookup the cost is ~200–320 ns —
negligible compared to traversal and potential page-fault costs. For
full-database scans the cost is bounded by memory bandwidth.

Pages CoW'd in the current transaction have their footers computed at
commit time on the dirty slab buffer, before the pwrite.

### Computation (Write Path)

When checksums are enabled, the xxhash64 footer is computed on each
dirty slab buffer at commit time, before the pwrite. The footer is
written into the last 8 bytes of the buffer.

### What Checksums Do and Do Not Catch

**Catches:**
- Silent bitrot on disk (bit flips in stored data).
- Firmware bugs in SSD/NVMe controllers that corrupt data at rest.
- RAID controller or storage stack corruption.
- Kernel bugs that corrupt the page cache after a successful write.

**Does not catch:**
- Torn writes (handled by CoW + meta page checksum).
- In-memory corruption between buffer-fill and pwrite (the checksum is
  computed on the same buffer that is written — if the buffer is
  corrupt, the checksum matches the corrupt data).
- Corruption introduced by the application via stray pointers — the
  data mmap is `PROT_READ` only, so stray writes there SIGSEGV
  immediately; the slab buffer is application memory, where typical
  unsafe-pointer bugs would land. Defense via `mprotect` mitigates the
  most common variant.

### Default

Checksums are **enabled by default** (`Options.PageChecksum = true`).
Disable via `PageChecksum = false` at creation only when running on a
filesystem with end-to-end checksums (ZFS, btrfs, ReFS) or storage
controllers with built-in integrity — and the 0.2% page-space saving
is meaningful for the workload.

## Integrity and Safety

- **No partial writes visible**: CoW ensures all modifications happen
  on new pages. The old tree is intact until the meta page swap.
  Bitmap leakage (pages that appear allocated but are unreferenced)
  is possible on crash between the bitmap pwrite and the meta pwrite,
  but tree integrity is always preserved.
- **Atomic commit**: A single meta page write (< page size, aligned)
  is the commit point. Even if torn, the checksum fails and the DB
  falls back to the other meta page.
- **Write ordering**: In `SyncDurable`, data + bitmap pwrites are
  fdatasync'd BEFORE the meta page write, and the meta is fdatasync'd
  AFTER. In other sync modes, ordering relies on CoW (see Durability
  Modes).
- **Reader isolation**: Readers see an immutable snapshot. Pages they
  reference cannot be reused until all readers on that TxnID have
  finished.
- **mmap is `PROT_READ`**: a stray pointer in the host process
  produces SIGSEGV rather than silently corrupting the file. The
  writer's mutations live in slab buffers (process memory), where
  unsafe-pointer bugs can still cause harm — but they cannot reach
  disk except via the controlled pwrite path.
- **Stale reader recovery**: If a process crashes without releasing
  its reader slot, the PID liveness check + process start time
  comparison allows the writer to reclaim the slot — even if the PID
  has been recycled.
- **Stale writer recovery**: If the writer process crashes, the
  kernel releases the flock automatically. The next writer detects
  the dead or recycled PID, cleans up reader slots from the crashed
  process, and proceeds — CoW guarantees the tree is consistent.
  Bitmap integrity is guaranteed by the deferred pwrite approach:
  bitmap modifications are held in memory (`tx.pendingAllocs` /
  `tx.pendingFrees`) and only written to disk via `pwrite()` at
  commit time. If the writer crashes before commit, no bitmap
  modifications reach disk — no leaked pages. Slab buffers in
  anonymous mmap are released to the OS on process exit; no on-disk
  artifacts.
- **Index consistency**: Every index update happens in the same CoW
  transaction as the row write. Either both succeed or both are
  rolled back. Index drift can only occur if the user changes the
  extractor without bumping `Version` (or vice versa) — caught at
  Open by the schema-hash + version fingerprint check; the engine
  refuses to open the keyspace until `RebuildIndex` is called.
- **Silent bitrot detection**: When `PageChecksum` is enabled (the
  default), every data page read is verified against its xxhash64
  footer. Corruption is detected at read time with
  `ErrBadPageChecksum` identifying the affected page.
- **Disk full (ENOSPC)**: If `ftruncate()` (growth) or `pwrite()`
  (data / bitmap / meta) fails with ENOSPC, the operation returns an
  error. A failed `pwrite()` during commit may result in a partially
  written page on disk. Since the meta page has not been updated,
  recovery falls back to the previous meta — the partially written
  pages are superseded by the next successful commit. File growth
  failures during the transaction cause `pageAlloc()` to return
  `ErrDBFull`. Slab-buffer pwrite failures during commit abort the
  commit (the transaction must be rolled back at the application
  level; the on-disk state is consistent with the previous meta).

## Background Maintenance

The `DB` struct runs a **maintenance goroutine** (started at `Open()`,
stopped at `Close()`) that performs periodic housekeeping to prevent
issues from accumulating. Goal: avoid reaching a state that requires
offline intervention.

### Coordination

Multiple processes sharing the same database coordinate via a
`LastMaintenanceTime` field in the lock file header — a `uint64`
monotonic clock value (`CLOCK_BOOTTIME` on Linux) updated after each
pass. Before starting a pass, the goroutine checks this timestamp. If
a recent pass was completed by any process (within
`MaintenanceOptions.Interval`), the goroutine skips. Ensures only one
process runs maintenance per interval.

### Tasks

Four tasks per pass:

#### 1. Bitmap Leak Reclamation

Reclaims pages allocated in the bitmap but unreferenced by any tree
structure — "leaked" pages caused by crashes between bitmap pwrite
and meta pwrite, or by slab-flush partial writes interrupted by ENOSPC.

**Detection phase** (read transaction, non-blocking):
1. Open a read transaction.
2. Walk the full tree (all keyspaces incl. internal index keyspaces,
   per-keyspace index registries, RPL segments, overflow pages) to
   build the set of all referenced page IDs.
3. Scan the bitmap. Any page with its bit clear (allocated) that is
   not in the referenced set and is not meta / bitmap / RPL is leaked.
4. Close the read transaction.

**Reclamation phase** (write transaction):
1. Open a write transaction.
2. For each leaked page, set its bitmap bit.
3. Commit.

**Safety**: a leaked page is permanently stuck — its bitmap bit is
clear so no future transaction can allocate it, and no tree references
it. A page identified as leaked in the read snapshot cannot become
un-leaked by the time the write transaction runs.

**Trigger**: every maintenance pass. Additionally, if `Open()` detects
crash recovery (selected a fallback meta), the first maintenance pass
is scheduled immediately rather than waiting for the interval.

#### 2. Stale Reader Slot Cleanup

Proactively scans the reader table and clears slots owned by dead
processes. Same namespace-aware logic as the writer's stale reader
scan: same-namespace uses PID + StartTime, cross-namespace uses
heartbeat timeout.

No transaction needed — slot cleanup is an atomic store (`TxnID = 0`)
on the shared mmap.

**Why this matters**: the writer already clears stale slots during RPL
reclamation, but only when it needs free pages. If no writer is
active for an extended period, stale slots from crashed containers
sit indefinitely, blocking RPL reclamation for the next writer.
Proactive cleanup keeps the reader table clean.

#### 3. Checksum Scrubbing

When `PageChecksum` is enabled, the maintenance goroutine performs a
background read-only scan that verifies xxhash64 footers on data
pages proactively — before they are accessed by a user transaction.
Catches silent bitrot early.

Each pass verifies `ScrubBatchSize` pages (default 4096) in a read
transaction, advancing through the file sequentially across passes.
A `ScrubCursor` on the DB tracks the next page ID to verify, wrapping
at `HighWaterMark`. A full scrub cycle covers the database over
`ceil(HighWaterMark / ScrubBatchSize)` passes.

Detected corruption is logged via `slog.Logger` as `CheckWarning`
with the affected page ID. The scrubber does not repair — only
reports. Repair: `CheckWithOptions(Repair)` or `CopyTo(compact=true)`.

Skipped when `PageChecksum` is not enabled.

#### 4. Incremental Compaction

Defragments the database by relocating pages in batches to restore
contiguous free runs for overflow allocation. Online alternative to
`Compact()`.

**Trigger**: the allocator tracks the contiguous-allocation failure
rate — the fraction of multi-page `pageAlloc(n)` calls (n > 1) that
fail to find a contiguous run on the first bitmap scan despite
sufficient total free pages. When this rate exceeds
`CompactionThreshold` (default 0.5), the maintenance goroutine
schedules compaction work.

**Mechanism**: each pass opens a write transaction and relocates up
to `CompactionBatchSize` pages (default 1024):
1. Identify fragmented regions — pages that interrupt potential
   contiguous runs.
2. For each, CoW it to a new location (allocated from a region with
   more free neighbors).
3. The old page goes to the RPL and is reclaimed in a future txn.
4. Commit.

Over multiple passes, scattered pages consolidate and contiguous
free runs emerge. Converges when the failure rate drops below the
threshold.

**Cost per pass**: each moved leaf forces a CoW cascade up the tree
(every ancestor branch needs CoW + new child pointer), so worst-case
I/O is `CompactionBatchSize × (1 + depth) × PageSize` plus
`CompactionBatchSize × (1 + depth)` RPL entries for the retired
originals. At 1024 pages, depth 5, 4 KB pages: ~24 MB of pwrite I/O
per pass, ~6 K RPL entries (~12 segment pages at 508
entries/segment). Size `CompactionBatchSize` against
`MaxTxBufferBytes` accordingly — the slab must hold the whole
cascade plus assembly buffers in step 0 of the commit. Bounded and
amortized across the maintenance interval.

### Options

```go
type MaintenanceOptions struct {
    // Disable disables the background maintenance goroutine.
    // Default: false (maintenance enabled).
    Disable bool

    // Interval is the minimum time between maintenance runs.
    // Coordinated across processes via the lock file.
    // Default: 5m.
    Interval time.Duration

    // ScrubBatchSize is the number of pages to verify per checksum
    // scrubbing pass. Only meaningful when PageChecksum is enabled.
    // Default: 4096.
    ScrubBatchSize int

    // CompactionThreshold triggers incremental compaction when the
    // contiguous-allocation failure rate exceeds this fraction.
    // Range: 0.0 (disabled) to 1.0. Default: 0.5.
    CompactionThreshold float64

    // CompactionBatchSize is the number of pages relocated per write
    // transaction during incremental compaction.
    // Default: 1024.
    CompactionBatchSize int
}
```

Maintenance is a fixed-cost resource: one goroutine per DB handle.
Same lifecycle pattern as the flock and heartbeat goroutines. The
explicit tools (`Check`, `CheckWithOptions`, `Compact`, `CopyTo`)
remain available for on-demand use.
