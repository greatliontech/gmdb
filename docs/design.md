# gmdb Design Document

A memory-mapped, multi-process, embedded key-value database for Go.

**Minimum Go version: 1.24.** gmdb uses `runtime.AddCleanup` (Go 1.24), `structs.HostLayout` (Go 1.24), `os.OpenRoot` (Go 1.24), `testing/synctest` (Go 1.24), `unique.Make` (Go 1.23), `cmp.Or` (Go 1.22), and `iter.Seq2` (Go 1.23).

## Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Data structure | B+tree on fixed-size pages | Only viable option for multi-process mmap |
| Concurrency | Single writer + N readers (MVCC/CoW) | Proven (LMDB), readers never block writer |
| File layout | Fixed-size pages (4KB–64KB, configurable, immutable after creation) | Matches OS page size, mmap-friendly |
| Page header | 8 bytes (Type uint8, Flags uint8, Count uint16, AdditionalPages uint32 — no PageID) | PageID is redundant (computable from file offset); Type/Flags split reserves 8 flag bits for future per-page metadata at zero cost |
| Value storage | Inline + overflow pages | Simple single read path, overflow for large values |
| Multiple values per key | Set keyspace with subpage + nested B+tree | Subpage for small sets, nested B+tree for large; fixed-size values for fixed-size optimization |
| Free space | Allocation bitmap + retired page log (RPL) | O(1) alloc via bitmap, no self-referential allocation, RPL tracks MVCC retirement |
| RPL entry format | Per-segment TxnID + array of PageIDs | TxnID stored once per segment (not per entry); doubles segment capacity |
| File format | Dynamic grow/shrink with configurable bounds; MaxSize immutable after creation | Auto-compaction via tail refund, no manual compaction needed; MaxSize fixed because bitmap region size depends on it |
| Isolation | Dual meta pages + CoW | No WAL needed, atomic commit |
| Crash safety | CoW + atomic meta page swap | File is always consistent |
| Durability | Three sync modes (Durable, DataOnly, Lazy) + unsafe opt-in Unsafe | Configurable ACID vs. performance; SyncUnsafe requires explicit `AllowSyncUnsafe` flag |
| Cross-process | Shared memory lock file (`structs.HostLayout` structs, uint64 PIDs + process start times) | C ABI layout guarantee for mmap'd structs; fixed-size reader table (scan+CAS), stale writer/reader recovery via PID liveness + start time comparison (PID reuse safe); uint64 PIDs for forward safety |
| Write lock | Intra-process writer queue (channel) + single flock goroutine (cross-process) | Context-aware blocking; zero goroutine accumulation on cancellation; flock alone doesn't block same-process goroutines |
| Lagging readers | Callback-based notification | Application controls policy; no silent unbounded growth |
| mmap | Read-write mmap (`MAP_SHARED \| PROT_READ \| PROT_WRITE`) | Single write mode; CoW to fresh pages in mmap; bitmap/meta via pwrite for ordered commits |
| Huge pages | `MADV_HUGEPAGE` (opt-in, Linux) | Reduces TLB pressure for large databases; transparent huge pages for file-backed mmap |
| CoW page tracking | Hash map (`map[uint64]struct{}`) of CoW'd page IDs | O(1) insert/lookup; `pendingAllocs`/`pendingFrees` track bitmap changes; commit writes only bitmap + meta via pwrite |
| Branch keys | Prefix-truncated separators | Shortest distinguishing prefix; maximizes fan-out; shallower trees; full keys in leaves only |
| Key ordering | Lexicographic (byte-ordered) | Simple, general, no custom comparator needed |
| Byte order | Little-endian (fixed) | Portable across architectures |
| Checksums | Meta: xxhash64 (always); Data: CRC32C footer (optional) | Meta checksum detects torn commits; optional data checksum catches silent bitrot |
| API | Transaction-based with `context.Context` | Explicit read/write txns; context governs lock acquisition, not txn lifetime; `context.Cause(ctx)` preserves cancellation reasons from `WithCancelCause` |
| Iteration | Cursor (stateful, bidirectional, mutable) + `iter.Seq2` (read-only, composable) | Cursor for mutation and bidirectional movement; `iter.Seq2` for idiomatic `for range` loops |
| Write batching | Channel-based `Batch()` API with nested transactions | Amortizes commit cost (fdatasync) across concurrent callers; each closure runs in a child transaction — no rollback+retry, closures execute exactly once |
| Nested transactions | Child transactions with snapshot-and-restore | In-memory only (no disk I/O); CoW to fresh pages means child rollback is just discarding bookkeeping; enables `Batch()` without idempotency requirement |
| Leak detection | `runtime.AddCleanup` on `Tx` and `DB` | Detects leaked transactions (releases reader slots) and leaked DB handles (releases mmap, fds, flock goroutine); logs origin stack trace |
| Range delete | Subtree retirement via `DeleteRange` | O(pages) not O(entries); bulk-free for set keyspace nested B+trees |
| Commit I/O | pwrite (bitmap + meta pages only) + fdatasync | Data pages are already in the mmap; only bitmap and meta need ordered writes; typically 2-5 pwrite calls per commit |
| Prefaulting | `MADV_POPULATE_READ` at open (opt-in, Linux 5.14+) | Eliminates first-access page faults; sequential kernel readahead; silent no-op on older kernels |
| Read txn cooldown | `MADV_COLD` on close (opt-in, Linux 5.4+) | Hints kernel to reclaim page cache after large scans; reduces memory pressure |
| Typed keyspaces | Generic `TypedKeyspace[K, V]` with `Encoder[T]` interface | Zero-cost type-safe API over byte-oriented Keyspace; append-style `AppendEncode(dst, v) ([]byte, error)` enables buffer reuse and surfaces encode/decode failures; interface enables stateful encoders and buffer pooling |
| Keyspace names | `unique.Handle[string]` interning | Avoids repeated allocations for frequently opened keyspace names across transactions |
| Integrity check | `Check() iter.Seq[CheckIssue]` with `CheckFatal` severity | All results (issues + walk failures) are uniform `CheckIssue` values; streaming via `iter.Seq`; `slices.Collect` for batch use; `break` for early abort |
| Byte slice ownership | Borrowed references: values valid until tx close, keys valid until next cursor op | Zero-copy from mmap; prefix compression requires key reconstruction buffer (reused per cursor movement); same contract as LMDB/libmdbx/BoltDB |
| Nil/empty semantics | Nil/empty keys invalid; nil values treated as empty; nil return = not found or end-of-iteration | Catches bugs at API boundary; empty values enable set-of-keys pattern; unambiguous nil semantics |
| Namespaces | Named keyspaces (separate Open/Create API) | Multiple B+trees in one file; clear creation vs. opening semantics |
| Block compression | Not supported (explicit decision) | Incompatible with mmap zero-copy read path; would require a decompression buffer pool, cache management, and eviction — fundamentally changing the architecture; key-level prefix compression provides density gains within the mmap model |
| TTL / Expiry | Not supported (explicit decision) | Adds per-cell overhead or a shadow index for a use case (caches, sessions) that gmdb doesn't target; users can implement TTL with a separate expiry keyspace and periodic cleanup using existing primitives |
| Named snapshots | Not supported (explicit decision) | Requires preserving historical meta roots and pinning TxnIDs (permanently blocking RPL reclamation); dual meta pages don't preserve old roots past two commits; `CopyTo()` covers the backup use case without ongoing space cost |
| Merge operators | Not supported (explicit decision) | LSM optimization (defer reads during writes); B+tree read and write paths traverse the same pages — no asymmetry to exploit; `Get` + `Put` does the same work |
| Sequences | `NextSeq uint64` in keyspace descriptor | Per-keyspace auto-incrementing counter; eliminates the need for a separate SeqKeyspace type — sequential-key workloads use a regular Keyspace with `NextSequence()` + big-endian uint64 keys (prefix compression makes the density gap negligible) |
| Per-keyspace page sizes | Not supported (explicit decision) | Requires either multi-block nodes in a shared bitmap (adds keyspace context to every page operation, non-uniform CoW tracking) or per-keyspace files (breaks cross-keyspace atomic commit); single file with uniform page size is a core design strength |
| Encryption at rest | Not supported (explicit decision) | Same mmap conflict as compression — encrypted pages can't be read in place, requires decryption buffer pool; LMDB and libmdbx also omit encryption for this reason; filesystem-level encryption (LUKS, FileVault, dm-crypt) covers the primary threat model transparently |
| Background maintenance | Periodic goroutine: bitmap leak reclamation, stale reader cleanup, checksum scrubbing, incremental compaction | Avoids accumulating issues that require offline intervention; coordinated across processes via lock file timestamp; one pass per interval regardless of process count |

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
bitmap pages is determined by `MaxSize` at database creation time:
`BitmapPages = ceil((MaxSize / PageSize) / (PageSize * 8))`. Data pages
(B+tree nodes, overflow pages, RPL segment pages) begin immediately after
the bitmap region. See Allocation Bitmap for details.

### Page Types

Every page starts with a common header:

```
Page Header (8 bytes)
+----------+----------+----------+----------+
| Type     | Flags    | Count    | AdditionalPages |
| uint8    | uint8    | uint16   | uint32          |
+----------+----------+----------+----------+
```

- **Type** (uint8): One of: Branch, Leaf, Overflow, RPLSegment.
  Meta pages and bitmap pages do not carry the page header — meta pages
  have their own layout (see Meta Page) and bitmap pages are raw bitfield
  data (see Allocation Bitmap). RPLSegment pages are the retired page
  log (see Free Space Management).
- **Flags** (uint8): Reserved for future per-page flags. Must be zero.
  Readers must reject pages with unknown flags set.
- **Count** (uint16): Number of items (keys in branch, key/value pairs in
  leaf, entries in RPL segment).
- **AdditionalPages** (uint32): Number of contiguous overflow pages following this
  one (0 for single-page nodes).

The page header does not contain a PageID field. A page's ID is implicit —
computable from its file offset (`offset / PageSize`). This avoids wasting
8 bytes per page on redundant information and eliminates any possibility of
inconsistency between the stored PageID and the actual file position.
`Check()` verifies page type and structural validity at each offset; no
stored PageID is needed for integrity checking.

When `Options.PageChecksum` is enabled, every data page (branch, leaf,
overflow, RPL segment) also carries a 4-byte CRC32C footer in the last 4
bytes of the page. See Checksums for details.

#### Meta Page

Two meta pages occupy pages 0 and 1. They alternate — the writer always
updates the one NOT currently active. Meta pages do not carry the standard
page header — their position is fixed (byte 0 and byte PageSize), so the
Type field is redundant, and Count/AdditionalPages are meaningless for metadata.
The meta layout starts directly with Magic:

```
Meta Page
+------------------+
| Magic            | uint32 - identifies file as gmdb
| Version          | uint32 - format version
| PageSize         | uint32 - page size in bytes
| Flags            | uint32 - bit 0: PageChecksum (immutable); bit 1: Checkpoint (mutable); bits 2-31: reserved (must be zero)
| BitmapPages      | uint32 - number of pages in the allocation bitmap
| Padding          | 4 bytes - alignment
| UUID             | [16]byte - database identity, generated at creation, immutable
| MinSize          | uint64 - minimum database size in pages
| MaxSize          | uint64 - maximum database size in pages
| GrowStep         | uint64 - growth step in pages
| ShrinkThreshold  | uint64 - shrink threshold in pages
| HighWaterMark    | uint64 - first unallocated page ID (high-water mark)
| RPLHeadPage      | uint64 - page ID of the newest RPL segment (0 = empty)
| RPLTailPage      | uint64 - page ID of the oldest RPL segment (0 = empty)
| RPLEntryCount    | uint64 - total entries across all RPL segments
| NumFreePages     | uint64 - total free pages (set bits in bitmap)
| KeyspaceRoot     | uint64 - root page of keyspace B+tree
| NumKeyspaces     | uint64 - number of keyspaces
| TxnID            | uint64 - transaction ID that wrote this meta
| Checksum         | uint64 - xxhash64 of all preceding bytes (Magic through TxnID)
+------------------+
```

Total meta page payload: 4×4 (Magic, Version, PageSize, Flags) +
4 (BitmapPages) + 4 (padding) + 16 (UUID) + 13×8 (uint64 fields
including Checksum) = 144 bytes. Fits comfortably in any supported
page size (min 4KB).

`UUID` is a 128-bit random identifier generated at database creation time
and copied identically to both meta pages. It uniquely identifies this
database instance — useful for backup validation ("is this backup from
the same database?") and lock file association ("does this lock file
belong to this data file?"). Immutable after creation.

`Flags` policy: `Open()` must reject databases where any unknown flag
bit is set (bits 2-31 in the current version). This prevents old code
from silently ignoring features it does not understand, which could
lead to data corruption. Bit 0 (PageChecksum) is immutable — set at
creation, never changes. Bit 1 (Checkpoint) is mutable — set/cleared per
commit depending on whether the commit's data pages have been confirmed
on stable storage.

The file format fields (`MinSize`, `MaxSize`, `GrowStep`,
`ShrinkThreshold`) are stored in the meta page so that they persist across
opens and are available to all processes (see File Format).

The active meta page is the one with the highest TxnID whose checksum is valid.
If a crash happens mid-write to the meta page, the checksum will be invalid and
the database falls back to the other meta page — which points to the previous
consistent state.

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

Keys are stored in sorted order. For a branch with N cells (N keys), there are
N+1 child pointers: `Ptr[0]` (leftmost, stored after the page header) plus one
`ChildPtr` per cell.

Search algorithm: binary search the cell directory to find the first
separator `Key[i]` where `target < Key[i]` (i.e., the first separator
greater than the target). If found, descend to the child to the left of
that separator — `ChildPtr` of cell `i-1`, or `Ptr[0]` if `i == 0`. If
no separator is greater than the target (`target >= all keys`), descend
to the last cell's `ChildPtr` (rightmost child). Note: when
`target == Key[i]`, the target belongs in the right child (since
separators are lower bounds of the right child), so the search correctly
continues past that separator.

The cell directory stores `(Offset, KeyLen)` per cell, enabling binary search
over variable-length keys without parsing the key data area.

##### Prefix-Truncated Branch Keys

Branch pages store **prefix-truncated separator keys** — the shortest byte
string that distinguishes the left subtree from the right — rather than
full keys copied from leaf pages. A branch separator must satisfy:

- Every key in the left child compares **strictly less than** the separator.
- Every key in the right child compares **greater than or equal to** the separator.

Equivalently: `max(left) < separator <= min(right)`. The separator is a
lower bound of the right child. This convention matches the descent
algorithm: find the first separator where `target < sep` and descend to
that child (so `target == sep` descends right, which is correct since
the separator is the right child's lower bound).

For example, if the left child's largest key is `"user:alice:profile"`
and the right child's smallest key is `"user:bob:settings"`, the
separator stored in the branch is `"user:b"` (7 bytes) instead of
the full key (20 bytes). The separator is the common prefix of the two
boundary keys extended by one byte from the right key at the divergence
point — always a prefix of the right child's first key (see Separator
computation below).

**Benefits:**
- **Higher fan-out**: smaller keys → more separators per branch page →
  wider tree. For workloads with long keys sharing common prefixes
  (URLs, file paths, composite keys like `tenant:user:resource`), a
  200-byte key might compress to 10–20 bytes in the branch, increasing
  fan-out by 10–20x.
- **Shallower trees**: higher fan-out → fewer levels → fewer page
  accesses per lookup. A tree that would be depth 4 with full keys
  might be depth 3 with truncated keys.
- **Reduced I/O**: less data read per branch page traversal.

**Separator computation:**

At **leaf split** time, when a leaf page overflows and is split into
two halves:
1. Let `L` = the last key of the left leaf (the split point).
2. Let `R` = the first key of the right leaf.
3. Compute the shortest byte string `S` such that `L < S <= R`.
   This is the common prefix of `L` and `R`, extended by one byte
   from `R` at the first divergence position: `S = R[0 : len(commonPrefix) + 1]`.
   `S` is always a prefix of `R`, guaranteeing `S <= R`. Since `L`
   either diverges at a lower byte value or is a proper prefix of `R`,
   `L < S` holds.
4. Insert `S` (not `R`) into the parent branch page.

At **merge** time, when two siblings are merged:
1. Remove the separator from the parent branch.
2. If the merge produces a new sibling boundary, recompute the
   separator from the new boundary keys using the same algorithm.

At **redistribute** time, when keys are moved between siblings to
balance fill ratios:
1. Recompute the separator from the new boundary keys (last key of
   left sibling, first key of right sibling).
2. Replace the old separator in the parent branch.

**Complementary with leaf prefix compression**: branch pages use
prefix-truncated separators (compressing keys *across* tree levels —
the separator is shorter than either boundary key). Leaf pages use
prefix compression (compressing redundancy *within* a page — each key
is stored as a delta from its neighbor). The two techniques are
independent and complementary: branch truncation reduces tree depth,
leaf compression increases leaf density. Cursor navigation reconstructs
full keys from the leaf delta encoding and compares against truncated
separators in branches; the B+tree search algorithm handles this
naturally since branch comparisons only determine which child to
descend into.

**Interaction with maximum key size**: the maximum key size limit
(see Limits) applies to full keys stored in leaf pages (reconstructed
from delta encoding). Branch page separators are always shorter than or
equal to the full keys, so they never exceed the limit.

#### Leaf Page

Leaf pages store the actual key-value pairs using **prefix compression** —
keys that share common prefixes with their neighbors are stored as deltas
rather than full keys. This increases leaf density significantly for
workloads with hierarchical or composite keys (e.g., `tenant:user:resource`
patterns).

```
Leaf Page
+-----------------------+
| Page Header (8 bytes) |
+-----------------------+
| RestartInterval uint16|  fixed: 16
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

Entries are stored in forward memory order starting at a fixed offset (12)
because prefix compression requires sequential scanning — each delta entry
reconstructs its key from the previous entry's key. The restart table is at
the end of the page (before the optional CRC32C footer) rather than after
the header, so that entries can be written directly as they are added without
knowing the final restart count upfront. The reader locates the restart table
at `contentEnd - RestartCount × 2`.

Entries at positions 0, 16, 32, ... are **restart points** that store full
keys. All other entries are **delta entries** that encode the key as a
difference from the previous entry.

Each entry carries a `CellFlags` byte in its header to distinguish cell
formats (inline value, overflow reference, set keyspace subpage, or nested
B+tree reference).

CellFlags bit layout:

```
Bit 0:    Overflow (0 = inline value, 1 = overflow reference)
Bit 1:    MultiValue (0 = single value, 1 = multi-value data — subpage or nested B+tree)
Bit 2:    NestedTree (only when Bit 1 is set: 0 = subpage, 1 = nested B+tree)
Bits 3-7: Reserved (must be 0)
```

Note: `Overflow` (bit 0) and `MultiValue` (bit 1) are mutually exclusive in
practice — a cell is either a single inline value, an overflow reference, or
a multi-value data container, never a combination.

**Restart entry** (full key, at positions 0, 16, 32, ...):

```
Restart Entry (inline)
+-----------+----------+----------+-----------+-----------+
| CellFlags | KeyLen   | ValueLen | Key bytes | Val bytes |
| uint8     | uint16   | uint32   |           |           |
+-----------+----------+----------+-----------+-----------+
```

**Delta entry** (between restart points):

```
Delta Entry (inline)
+-----------+-----------+-------------+----------+---------------+-----------+
| CellFlags | SharedLen | UnsharedLen | ValueLen | UnsharedKey   | Val bytes |
| uint8     | uint16    | uint16      | uint32   |               |           |
+-----------+-----------+-------------+----------+---------------+-----------+
```

`SharedLen` is the number of leading bytes shared with the previous entry in
the same restart group. `UnsharedKey` contains only the bytes after the shared
prefix. The full key is reconstructed by taking the first `SharedLen` bytes of
the previous entry's full key and appending `UnsharedKey`.

Delta entries cost 2 extra bytes (`SharedLen`) per entry but save `SharedLen`
bytes of key data. The net saving per entry is `SharedLen - 2` bytes — positive
whenever keys share more than a 2-byte prefix. For keys with no shared prefix,
`SharedLen` is 0 and the overhead is 2 bytes per entry — negligible.

`ValueLen` is uint32 (max ~4GB for inline values). In practice, inline values
are limited by leaf page free space — far below 4GB. Values that exceed leaf
page capacity are stored as overflow pages, referenced via the overflow format
below which uses uint64 `TotalLen` for unbounded value sizes.

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

The reader checks `CellFlags.Overflow` to determine which format to parse:
inline (key + ValueLen + Value) or overflow (key + OvflPage + TotalLen).
The key portion uses the restart or delta encoding depending on position.

##### Leaf Lookup

Binary search in a prefix-compressed leaf operates in two phases:

1. **Binary search over restart points**: the restart table stores byte
   offsets to entries at positions 0, 16, 32, .... Each restart entry has
   a full key, so comparison is direct. This finds the restart group
   containing the target key. Cost: O(log R) where R = RestartCount.

2. **Linear scan within the restart group**: decode delta entries
   sequentially from the restart point, reconstructing each full key,
   until the target is found or passed. Cost: O(K) where K = RestartInterval
   (16).

Total lookup cost: O(log(n/16) + 16). For a leaf with 30 entries, this is
~17 comparisons. For a leaf with 200 entries (high-prefix workload), this
is ~20 comparisons. The linear scan operates on data already in L1 cache
(the entire leaf page was fetched for the restart point binary search), so
the per-comparison cost is a memcpy + compare on short suffixes.

##### Leaf Density

The density improvement depends on the ratio of shared prefix length to
total key length. Example with 200-byte keys sharing a 150-byte common
prefix and 50-byte values, on a 4KB page:

| Format | Bytes/entry (avg) | Entries/page | Improvement |
|--------|-------------------|-------------|-------------|
| Full keys | ~260 | ~15 | baseline |
| Prefix compressed (K=16) | ~117 | ~33 | 2.2x |

For short keys with minimal sharing (20-byte keys, 2-byte shared prefix),
the improvement is negligible (~5%). The compression adapts automatically —
high-prefix workloads benefit; random-key workloads pay only 2 bytes per
entry overhead.

##### Insert and Delete

Inserting a key between two delta entries within a restart group:

1. Encode the new entry as a delta against its predecessor.
2. Recompute the successor entry's delta — its `SharedLen` is now relative
   to the new entry instead of the old predecessor. Only this one entry's
   encoding changes; entries beyond the successor still delta against their
   own predecessors, which are unchanged.
3. If the insertion shifts entry indices, restart point positions change
   (restarts are at indices 0, K, 2K, ...). The affected restart group
   must be re-encoded — one entry becomes a restart (full key) and one
   becomes a delta, or vice versa. This re-encoding is confined to the
   entries at the old and new restart boundaries.

Deletion is symmetric: remove the entry, recompute one successor's delta,
adjust restart boundaries if an index shifted.

The restart table (array of offsets) is rebuilt after any insert or delete.
This is O(RestartCount) — at most ~20 entries for a full leaf page.

##### Leaf Split

When a leaf page overflows, it is split into two halves. Each half is
re-encoded independently with fresh restart points starting at index 0.
The boundary keys (last key of the left leaf, first key of the right
leaf) are full keys reconstructed from the delta encoding. Separator
computation for the parent branch page is unchanged — it operates on
full keys.

##### Cursor Key Reconstruction

The cursor maintains a **key reconstruction buffer** (`cursor.keyBuf
[]byte`) that holds the full key at the current position. On forward
movement (`Next()`), the buffer is updated incrementally: truncate to
`SharedLen`, append `UnsharedKey`. This is O(1) amortized.

For reverse movement (`Prev()`), incremental reconstruction is not
possible — delta entries encode forward, not backward. The cursor
addresses this by caching all decoded keys for the current restart group
(**group cache**). When the cursor first enters a restart group (via
`Seek`, `Next` crossing a group boundary, or `Prev`), all K entries in
the group are decoded into the cache. Subsequent `Prev()` within the
group reads from the cache in O(1). Crossing a group boundary triggers
decoding the previous group.

The group cache is a `[16][]byte` array on the cursor struct. At K=16
and a maximum key size of ~2KB, the worst-case memory cost is ~32KB per
cursor — acceptable for a cursor that already holds a page-ID stack for
tree traversal. In practice, keys are much shorter and the cache is
small.

#### Overflow Page

Overflow pages are contiguous runs of pages that store large values. The
first page in the run carries the standard 8-byte page header with
`AdditionalPages` set to the number of follower pages; the remaining bytes of
the first page are value data. Follower pages carry no header — they are
entirely value data (minus 4 bytes for the CRC32C footer when
`PageChecksum` is enabled). Total value capacity for a run of `1 + N`
pages:
`(PageSize - 8) + N * PageSize` bytes (or subtract 4 from the first page
and from each follower when `PageChecksum` is enabled).

When `PageChecksum` is enabled, each page in the run carries its own
independent CRC32C footer. The first page checksums its header + data;
each follower checksums its data. Per-page checksums allow identifying
which specific page in the run is corrupted.

#### Set Keyspace Storage (Multiple Values Per Key)

Set keyspaces allow multiple sorted values per key.
Each key maps to a sorted set of values. This is the primary
mechanism for secondary indexes (e.g., an index key mapping to a sorted set of
primary key IDs).

##### Storage Strategy

Set keyspaces use two storage strategies based on the size of the value set:

**Subpage (small value sets):** When a key's values fit within
the leaf cell, they are stored inline as a **subpage** — a mini sorted list
embedded directly in the cell's value area. No extra page allocation is needed.

**Nested B+tree (large value sets):** When values grow too large for a
subpage, they are promoted to a full B+tree whose root page ID is stored in
the leaf cell. Each value in the set becomes a key in the nested
B+tree (with empty values).

##### Subpage Format

A subpage is stored in the leaf entry's value area. The `CellFlags.MultiValue` bit
is set and `CellFlags.NestedTree` is clear. The entry uses the standard
restart/delta key encoding (see Leaf Page); the subpage replaces the value
portion. At a restart point:

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

For **variable-size values** (standard set keyspace):
```
Entry (variable):
+----------+-----------+
| ValueLen | Val bytes |
| uint16   |           |
+----------+-----------+
```

For **fixed-size values** (set keyspace with fixed-size values):
```
Entry (fixed):
+-----------+
| Val bytes |  (size = keyspace's fixed value size, no length prefix)
+-----------+
```

`Count` is the number of entries. `DataSize` is the total byte size of all
entries (used to quickly compute the subpage's total size for cell allocation).

Values within the subpage are stored in sorted (lexicographic) order. Lookup
is binary search. For fixed-size value subpages, entries are a flat array — binary
search is O(log N) with direct offset calculation (no scanning).

Subpage entries are **not prefix-compressed**. Subpages store
*values* for a single key, which typically do not share prefixes with each
other (e.g., post IDs in a secondary index, user IDs in a reverse index).
The subpage is also small by definition — it exists precisely because the
data fits inline within a leaf cell (below the 50% promotion threshold).
When a value set grows large enough for prefix compression to matter,
it is promoted to a nested B+tree whose leaf pages use prefix compression
like all other leaf pages.

##### Subpage Promotion Threshold

A subpage is promoted to a nested B+tree when inserting a new value
would cause the subpage to exceed **50% of the leaf page's usable space**
(PageSize minus page header, restart metadata, and restart table overhead). This threshold
ensures:
- The leaf page can still hold other keys alongside the promoted cell.
- Promotion happens before the subpage dominates the leaf page.

Promotion:
1. Allocate a new leaf page for the nested B+tree.
2. Copy all subpage entries into the new leaf page as regular key-value cells
   (where "keys" are the values from the set and "values" are empty).
3. Replace the subpage cell with a nested B+tree reference cell.
4. Insert the new value into the nested B+tree.

##### Nested B+tree Reference Cell

When a key's values are stored in a nested B+tree, the entry has
`CellFlags.MultiValue` and `CellFlags.NestedTree` both set. The entry uses the
standard restart/delta key encoding; the nested B+tree reference replaces
the value portion. At a restart point:

```
SetKeyspace Nested B+tree Entry (restart)
+-----------+----------+-----------+----------+----------+
| CellFlags | KeyLen   | Key bytes | Root     | Count    |
| uint8     | uint16   |           | uint64   | uint64   |
+-----------+----------+-----------+----------+----------+
```

- **Root**: Page ID of the nested B+tree's root page.
- **Count**: Number of values in the set.

Depth (tree height) is not persisted — it is derived by reading the root
page on first access (a leaf root means depth 1; a branch root means
depth > 1, determined by descent). This avoids maintaining an extra field
across promotion, demotion, split, merge, and delete operations.

The nested B+tree uses the same B+tree implementation as the main keyspace,
with one difference: its "keys" are the values from the set, and all "values" are
empty (zero-length). The nested B+tree's leaf pages use prefix compression
(same format as all other leaf pages), which benefits value sets with
shared value prefixes (e.g., timestamp-keyed data). The nested B+tree's pages
are subject to normal CoW, free space management, and page allocation.

##### Demotion

When deletions reduce a nested B+tree to a single leaf page that would fit as
a subpage (below the promotion threshold), the B+tree is demoted back to a
subpage. The leaf page is freed (retired), and the entries are packed
inline into the parent leaf cell.

When the last value for a key is deleted, the key's cell is
removed from the parent leaf entirely — empty nested trees and empty
subpages never exist, not even transiently within a write transaction.

##### Fixed-Size Value Sets

When a set keyspace is created with fixed-size values, all
values must be the same fixed byte size (set at keyspace creation). This
enables:
- **No per-value length prefix** in subpages — entries are a flat array.
- **Direct offset binary search** — `entry[i]` is at offset `i * valueSize`.
- **Compact nested B+tree leaves** — no `ValueLen` field per cell.

The fixed value size is stored in the keyspace descriptor (see Keyspaces).
A `Put()` call with a value of the wrong size returns an error.

### Range Delete

`Keyspace.DeleteRange(start, end)` deletes all keys in the range
`[start, end)` in a single operation. This is significantly more efficient
than iterating with a cursor and deleting one key at a time, because it
can retire entire B+tree subtrees without visiting individual leaf entries.

#### Algorithm

The range delete operates in three phases:

**Phase 1: Find boundary paths.**

Descend the B+tree twice to find:
- The **left boundary path**: the path from root to the leaf containing
  `start` (or the first key, if `start` is nil).
- The **right boundary path**: the path from root to the leaf containing
  the last key before `end` (or the last key, if `end` is nil).

Each path is a stack of `(pageID, index)` pairs — the same structure used
by the cursor.

**Phase 2: Identify and retire interior subtrees.**

Walk up from the two boundary paths to find their **lowest common
ancestor** (LCA) — the deepest branch page that contains both boundaries.
At each level between the LCA and the leaves:

- **Interior children** — child pointers in branch pages that fall
  strictly between the left and right boundary indices — are entirely
  within the delete range. Their entire subtrees are retired without
  visiting individual leaves.
- **Boundary children** — the leftmost and rightmost child at each level
  — are partially within the range and must be descended into.

Retiring a subtree: walk the branch pages of the subtree recursively. For
each page encountered (branch or leaf), add its page ID to
`tx.retiredPages`. For leaf pages, accumulate the entry count for the
return value. For overflow pages referenced by leaf cells, retire the
entire overflow run. The walk visits every page in the subtree exactly
once — O(pages in subtree), not O(entries in subtree). Since branch pages
fan out by hundreds of keys, the number of pages is dramatically smaller
than the number of entries.

**Phase 3: Clean up boundary leaves and rebalance.**

- In the left boundary leaf: delete entries from `start` (or the cell
  matching `start`) through the end of the leaf (CoW the leaf first).
- In the right boundary leaf: delete entries from the beginning through
  the last key before `end` (CoW the leaf first).
- If the left and right boundaries are in the same leaf, delete the
  entries between them.
- Retire any overflow pages referenced by deleted entries.
- Walk up from the boundary leaves to the LCA, removing the interior
  child pointers that were retired in Phase 2 from each branch page
  (CoW each branch).
- Rebalance: check fill ratios on the modified branch and leaf pages.
  Merge or redistribute with siblings as needed, following the normal
  `MergeThreshold` logic. The rebalance propagates upward, potentially
  reducing tree depth if the root becomes empty.

#### Complexity

| Operation | Naive (cursor loop) | Range delete |
|-----------|-------------------|--------------|
| Delete N keys spanning P pages | O(N × depth) | O(P + depth²) |
| CoW'd pages | O(N × depth) | O(depth²) — only boundary paths |
| Retired pages | N leaf cells + splits | P pages (bulk) + boundary cleanup |

For a range covering 1 million keys across 10,000 leaf pages in a tree of
depth 4: naive deletion does ~4 million CoW operations; range delete walks
~10,000 pages for retirement + ~16 CoW operations on boundary paths.

#### Set Keyspace Bulk Free

When deleting a key in a set keyspace whose values are stored in a
nested B+tree, the nested tree is freed using the same subtree retirement
mechanism:

1. Read the nested B+tree root page ID and count from the leaf cell.
2. Walk the nested B+tree's branch pages recursively, retiring every page.
3. Remove the key's cell from the parent leaf page.

This is O(pages in nested tree), not O(values). A key with 1
million values stored across 10,000 pages is freed by visiting ~10,000
pages — not 1 million individual delete operations.

The same bulk-free applies when `DeleteRange` encounters keys with nested
B+trees within the range: the nested trees are retired as part of the
subtree retirement in Phase 2, or individually for keys in boundary leaves
in Phase 3.

#### Cursor-Based Range Delete

For callers who need finer control (e.g., conditional deletion, progress
reporting), cursor-based deletion remains available:

```go
c := ks.Cursor()
for k, _ := c.SeekGE(start); k != nil && bytes.Compare(k, end) < 0; k, _ = c.Next() {
    c.Delete()
}
```

This uses the naive one-at-a-time path. `DeleteRange` should be preferred
when deleting a contiguous range unconditionally.

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
based on `MaxSize`:

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
| 256GB    | 64KB     | 4,194,304   | 8           | 512KB       |

The bitmap pages themselves are never marked as free in the bitmap — their
bits are permanently clear (reserved). The same applies to meta pages (pages
0 and 1). Data pages start at page `2 + BitmapPages`.

##### Bitmap Storage

The bitmap is stored directly in the mmap but treated as **read-only during
transactions**. Bitmap modifications are deferred in memory: `tx.pendingAllocs`
tracks pages allocated during the transaction (bitmap bits to clear at commit)
and `tx.pendingFrees` tracks pages freed during the transaction (bitmap bits to
set at commit). At commit time, the modified bitmap pages are written to disk
via `pwrite()`, followed by `fdatasync()`, before the meta page is updated.
This ensures the bitmap on disk is only ever modified via ordered pwrite+fdatasync
— never directly via the mmap.

Bitmap pages do not use the standard page header. The entire page is usable
as bitmap data (PageSize × 8 bits per page). The page type is identified by
its position in the file (pages 2 through `2 + BitmapPages - 1`), not by a
header field.

##### Two-Level Summary

To accelerate allocation searches over large databases, the bitmap uses a
two-level structure:

- **Level 0 (detail):** One bit per page in the database, covering page IDs
  0 through `MaxSize / PageSize - 1`. Stored across bitmap pages 2
  through `2 + BitmapPages - 1`. Bits for meta pages (0, 1) and bitmap
  pages (2 through `2 + BitmapPages - 1`) are permanently clear.
- **Level 1 (summary):** A separate in-memory array, one bit per uint64
  word of the detail level. A summary bit is set if the corresponding
  64-page word in the detail level has **any** set bits (any free pages).
  Size: `ceil(TotalPages / 64 / 64)` uint64 words. The summary is rebuilt
  from the detail bitmap when the database is opened and maintained
  incrementally during transactions.

At 4KB page size with 256GB `MaxSize` (67M pages): the detail level is
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

The RPL is an append-only singly-linked list of segment pages. Each segment
page stores a single TxnID (the transaction that retired these pages) plus
an array of PageIDs. Each commit creates new segment pages — existing
segments are never modified. All entries in a segment share the same
TxnID — storing it once per segment instead of per entry doubles capacity:

```
RPL Segment Page
+--------------------------+
| Page Header (8 bytes)    |
+--------------------------+
| TxnID          | uint64  |  transaction that retired these pages
| OlderSegment   | uint64  |  page ID of the next older segment (0 = this is tail)
| EntryCount     | uint16  |  number of PageID entries in this segment
| Padding        | 6 bytes |
+--------------------------+
| PageID 0       | uint64  |
| PageID 1       | uint64  |
| ...                      |
+--------------------------+
```

Segment capacity at 4KB page size: 8 (page header) + 8 (TxnID) + 8
(link pointer) + 2 (EntryCount) + 6 (padding) = 32 bytes overhead.
Remaining `4096 - 32 = 4064` bytes / 8 bytes per PageID = **508 entries
per segment page** (507 with PageChecksum enabled, due to the 4-byte CRC
footer reducing available space: `4096 - 32 - 4 = 4060` / 8 = 507). A transaction freeing 10,000 pages fills ~20 segment pages
(compared to ~40 with per-entry TxnID).

The meta page stores `RPLHeadPage` (newest segment) and `RPLTailPage` (oldest
segment). Segments are singly linked from head toward tail via
`OlderSegment`. The forward direction (tail toward head) is maintained
as an in-memory segment list rebuilt at `Open()` time (see RPL Segment
List below).

##### RPL Append (At Commit Time)

When a write transaction commits with retired pages:

1. Allocate one or more new segment pages from the bitmap (or file extension).
   Each commit always creates **new** segment pages — existing segments are
   never modified (they belong to previous transactions' snapshots and may
   be referenced by active readers).
2. Fill segment pages with the current TxnID in the segment header and
   PageID entries sorted by page ID for cache-friendly processing during
   reclamation. If the retired list exceeds one segment page's capacity
   (508 entries at 4KB page size), allocate additional segment pages and
   link them via `OlderSegment`.
3. Set the new head's `OlderSegment` to the old `RPLHeadPage`. No
   modification of the old head is needed — segments are singly linked
   (head toward tail only).
4. Update `RPLHeadPage` in the meta page to point to the new head. If the
   RPL was empty, also set `RPLTailPage`.
5. Append the new segment page ID(s) to the in-memory segment list
   (see RPL Segment List).

RPL segment pages are allocated from the bitmap like any other data page.
Allocating a segment page clears a bit in the bitmap — O(1), no further
allocation needed. A transaction retiring N pages needs at most
`ceil(N / 508)` page allocations (segment pages only — no old-head CoW).
This is bounded and non-recursive.

##### RPL Reclamation

At the start of a write transaction (or lazily on first `pageAlloc()`), the
writer reclaims RPL entries whose pages are safe to reuse:

1. Compute the **reclamation bound**: the minimum of the oldest active
   reader's TxnID (from the reader table) and the last checkpoint's TxnID
   (from the meta page). In `SyncDurable` and `SyncDataOnly` modes, every
   commit is a checkpoint, so the checkpoint TxnID equals the current
   TxnID — no restriction beyond active readers. In `SyncLazy` mode, the
   checkpoint TxnID may be older than the current TxnID, restricting
   reclamation to ensure the bitmap on disk is consistent with the
   checkpoint meta that crash recovery would select.
2. Walk the in-memory segment list from the **tail** (oldest segments first).
3. For each segment where `TxnID < reclaimBound`:
   a. Set the corresponding bits in the allocation bitmap for all PageIDs
      in the segment.
4. When a segment is fully reclaimed, free the segment page itself (set its
   bit in the bitmap), remove it from the in-memory segment list, and
   advance `RPLTailPage` to the next segment in the list.
5. Update `RPLEntryCount` and `NumFreePages` in the meta page.

The checkpoint TxnID bound prevents a crash-recovery scenario where the
on-disk bitmap reflects a newer transaction's reclamation but recovery
selects an older checkpoint meta. Without this bound, a page retired by
T100 and reclaimed by T101 could be reallocated for new data by T101.
If recovery selects T100's checkpoint meta, T100's tree references the
page expecting its original content, but the page now contains T101's
data. The checkpoint bound ensures reclamation only processes pages freed
by transactions older than the last checkpoint — pages that are no longer
reachable from any recoverable tree state.

Reclamation is performed oldest-first so that the RPL shrinks from the tail.
Empty segment pages are immediately freed — their bitmap bits are set, making
them available for allocation in the same transaction.

Reclamation consumes **whole segments** — a segment is either fully
reclaimed or left untouched. Since each segment has a single TxnID, this
is a clean boundary: either all entries in a segment are safe to reclaim
(TxnID < reclaimBound) or none are. This avoids partial segment
modification and the CoW overhead it would require. Segments are immutable
on disk from the moment they are written.

##### Oldest Reader Caching

Scanning the reader table to find the minimum active TxnID is O(MaxReaders).
The writer caches this value (`tx.cachedOldestReader`) and combines it with
the last checkpoint TxnID to form the reclamation bound. The cache is
refreshed lazily — only when the bitmap has no free pages and reclamation
might unlock more. Reading a stale (higher) value is conservative: it
delays reclamation but never causes incorrect behavior.

##### RPL Segment List

The on-disk RPL is singly linked from head (newest) toward tail (oldest)
via `OlderSegment`. Reclamation needs to walk in the opposite direction
(tail to head). To avoid a full chain traversal on every reclamation pass,
the writer maintains an in-memory **segment list** — a `[]uint64` slice of
RPL segment page IDs ordered from tail (index 0) to head (last index).

The list is rebuilt at `Open()` time by walking the on-disk chain from
`RPLHeadPage` via `OlderSegment` links to `RPLTailPage`, then reversing
the collected page IDs. This is O(RPL segments) — typically tens to low
hundreds — and happens once.

During normal operation the list is maintained incrementally:
- **Append** (commit with retired pages): new segment page IDs are
  appended to the end of the slice.
- **Reclaim** (tail consumption): consumed segment page IDs are removed
  from the front of the slice (advance a start index or re-slice).

The list is stored on the `DB` struct and protected by the write lock
(only the writer modifies it). Readers do not access the RPL segment list.

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

Pages that are CoW'd and then freed within the **same
write transaction** are called "loose pages." This commonly occurs during
B+tree rebalancing: a merge operation may CoW a node, then free one of the
two original nodes. The CoW'd copy becomes unnecessary if its contents are
merged into a sibling.

Loose pages are tracked in a **hash map** (`tx.loosePages map[uint64]struct{}`).
This provides O(1) insertion, O(1) membership check, and O(1) deletion.
When a page becomes loose, its page ID is added to `tx.pendingFrees` (so
the bitmap bit can be set at commit time) or made available for immediate
reallocation within the same transaction.

A hash map is used instead of a simpler `[]uint64` slice because of the
**tail page refund** operation at commit time. Tail refund checks whether
consecutive page IDs at the file tail (`HighWaterMark - 1`,
`HighWaterMark - 2`, ...) are loose, requiring membership lookups by
page ID. With a slice, each lookup is O(n) where n is the loose page
count. In the worst case — a large `DeleteRange` triggering cascading
merges followed by rebalancing — the loose set can be very large. If
the loose pages are concentrated at
the file tail (e.g., the transaction extended the file, did work, then
freed most of it), tail refund performs up to n membership checks against
n loose pages: O(n²) with a slice vs. O(n) with a map.

| Loose pages (n) | Tail checks (t) | `[]uint64` total ops | `map` total ops |
|-----------------|------------------|----------------------|-----------------|
| 100             | 10               | 1,000                | 10              |
| 1,000           | 100              | 100,000              | 100             |
| 5,000           | 500              | 2,500,000            | 500             |
| 65,536          | 65,536           | ~4.3 billion         | 65,536          |

Loose pages are **immediately reusable** within the same transaction without
any bitmap or RPL interaction:
- `pageAlloc()` checks `tx.loosePages` first. For single-page allocations,
  any entry is popped from the map (O(1) amortized via `range` + `delete`).
- Loose pages that are reused via `pageAlloc()` never touch the bitmap or RPL
  — they were allocated and freed within the same transaction, so no reader
  can ever reference them.
- At commit time, any loose pages still in the map (allocated a page ID but
  never reused) are added to `tx.pendingFrees` — their bitmap bits are set
  directly, bypassing the RPL. Since loose pages were allocated and freed
  within the same transaction, no reader can reference them, so MVCC
  retirement via the RPL is unnecessary.

#### Page Allocation Priority

`pageAlloc(n)` allocates `n` contiguous pages using this priority:

1. **Loose pages** (n=1 only): pop any entry from `tx.loosePages` map. O(1) amortized.
2. **Allocation bitmap**: scan the bitmap for a free page (n=1) or a
   contiguous run of free pages (n>1), starting from the LIFO hint.
3. **RPL reclamation**: if the bitmap has no suitable free pages, reclaim
   entries from the RPL (TxnID < oldest reader) into the bitmap, then retry
   step 2.
4. **Lagging reader check**: if reclamation is blocked by a long-lived reader,
   invoke the lagging reader callback (see Cross-Process Coordination). If the
   reader releases, refresh the oldest reader cache and retry step 3.
5. **File extension**: if no free pages are available, grow the file according
   to the file format growth step and advance `HighWaterMark`.

##### Tail Page Refund

After reclamation or at commit time, the writer checks if any pages at the
tail of the database file (page IDs equal to `HighWaterMark - 1`,
`HighWaterMark - 2`, etc.) are free in the bitmap. If so, those bits are
cleared and `HighWaterMark` is decremented. This reclaims file space and
enables file shrinkage at commit time (see File Format).

The refund process iterates: clearing tail bits may expose new tail pages.
It runs until no more tail pages are free. Loose pages are checked first
(by O(1) lookup in the `tx.loosePages` map for each tail page ID), then
the bitmap (by checking bits from `HighWaterMark - 1` downward).

**Safety with concurrent readers.** Tail refund only decrements
HighWaterMark for pages that are free in the bitmap. Pages held by an
active reader's snapshot are not in the bitmap — they remain in the RPL
until the reclamation bound (oldest reader and checkpoint TxnID) allows
their reclamation. Therefore, tail refund cannot remove pages that an
active reader references. The subsequent file shrinkage via `ftruncate()`
at commit time only truncates pages beyond HighWaterMark, which are
guaranteed to be unreferenced by any active reader or recoverable tree
state.

#### Freeing Pages

When a CoW operation replaces an old page with a new copy:
- If the old page was **CoW'd in this transaction** (i.e., it was itself a
  CoW copy made earlier in this transaction), it becomes a **loose page** —
  added to `tx.loosePages`.
- If the old page was **from a previous transaction** (an immutable page in
  the mmap), its page ID is added to `tx.retiredPages` — a list of page IDs
  to append to the RPL at commit time (the TxnID is stored once per RPL
  segment, not per entry).

Note: retired pages are NOT immediately marked free in the bitmap. They enter
the RPL and are moved to the bitmap only when reclamation determines they are
safe to reuse (no active reader holds their snapshot).

#### Commit-Time Free Space Update

During commit, the writer:
1. Performs tail page refund: check the bitmap for free pages at the end of
   the file, decrement `HighWaterMark`.
2. Moves any remaining loose pages into `tx.pendingFrees` (bypasses RPL —
   no reader can reference same-transaction pages).
3. Appends all `tx.retiredPages` to the RPL by allocating new segment
   pages from the bitmap and appending them to the in-memory segment list.
4. Updates `NumFreePages`, `RPLHeadPage`, `RPLTailPage`, and `RPLEntryCount`
   in the meta page.

Step 3 may allocate RPL segment pages from the bitmap. This is a bounded,
non-recursive operation: each segment page holds 508 entries (at 4KB page
size), so a transaction retiring N pages needs at most `ceil(N / 508)`
page allocations (segment pages only — no old-head CoW). Each allocation
is a single bitmap bit flip — no further cascading allocations.

If the bitmap has no free pages and file extension would exceed MaxSize, RPL segment allocation fails and the commit returns `ErrTxTooLarge`. The transaction must be rolled back. This can happen when the database is near capacity and a large number of pages are retired in a single transaction.

Steps 1-4 happen before the bitmap pwrite and meta page swap.

## CoW Page Tracking

A write transaction must track which pages have been copied via CoW for
two purposes: avoiding double-CoW when the same page is modified multiple
times within a transaction, and tracking bitmap changes for commit-time
pwrite.

### Data Structure

The CoW set is a **hash map** of page IDs:

```
tx.cowPages      map[uint64]struct{} // page IDs CoW'd in this transaction
tx.pendingAllocs map[uint64]struct{} // pages allocated — bitmap bits to clear at commit
tx.pendingFrees  map[uint64]struct{} // pages freed — bitmap bits to set at commit
```

All B+tree page modifications happen directly in the read-write mmap.
`tx.cowPages` records which pages were CoW'd so that double-CoW is
avoided (if a page is already in the CoW set, it can be modified in place
without allocating a new copy). `tx.pendingAllocs` and `tx.pendingFrees`
track deferred bitmap changes — the mmap bitmap is never modified directly
during a transaction; instead, these sets accumulate the changes and the
bitmap pages are written via `pwrite()` at commit time.

Page lookup is always `mmap[pageID * pageSize]` — a single level with no
branches. There is no page buffer, no evicted page set, and no multi-level
lookup. The OS page cache manages all page memory.

### Operations

| Operation | Method | Complexity |
|-----------|--------|------------|
| Add CoW'd page | `tx.cowPages[pageID] = struct{}{}` | O(1) amortized |
| Check if CoW'd | `_, ok := tx.cowPages[pageID]` | O(1) |
| Count | `len(tx.cowPages)` | O(1) |

The hash map replaces the sorted-array approach used in LMDB/libmdbx, where
insertions required maintaining sort order (O(n) shift) and lookups required
binary search (O(log n)). The map provides O(1) for all single-element
operations.

### Page Allocation with Pending Bitmap Changes

`pageAlloc()` must account for pages that have been allocated in this
transaction but whose bitmap bits have not yet been cleared on disk.
Before scanning the mmap bitmap, `pageAlloc()` checks `tx.pendingAllocs`
— if a page appears in the pending set, it is already allocated and must
not be returned again. This ensures correct allocation even though the
on-disk bitmap still shows the page as free.

Similarly, pages in `tx.pendingFrees` are logically free within this
transaction but their bitmap bits have not yet been set on disk. These
pages are candidates for reallocation within the same transaction.

#### Map Reuse Across Transactions

The CoW page map (`tx.cowPages`), pending maps (`tx.pendingAllocs`,
`tx.pendingFrees`), and the loose page map (`tx.loosePages`) are pooled
on the `DB` struct and reused across write transactions. On rollback or
after commit cleanup, the maps are reset via the `clear` builtin rather
than discarded:

```go
clear(tx.cowPages)        // O(1): resets to empty, retains allocated buckets
clear(tx.pendingAllocs)
clear(tx.pendingFrees)
clear(tx.loosePages)
```

`clear` resets a map to empty without deallocating its internal hash table
storage. The next transaction inherits pre-allocated buckets sized for the
previous transaction's workload, avoiding the incremental growth phase
(repeated allocations and rehashes as the map doubles from its minimum
size). For write-heavy workloads with consistent transaction sizes, this
eliminates per-transaction map allocation overhead entirely.

The same pattern applies to `tx.retiredPages` (the `[]uint64` of page IDs
to append to the RPL): the slice is reset via `tx.retiredPages = tx.retiredPages[:0]`,
retaining its backing array for the next transaction.

### Commit-Time Write Ordering

At commit time, data pages are already in the mmap — no data page writes
are needed. The commit path writes only bitmap and meta pages via
`pwrite()`:

1. Compute the modified bitmap pages: for each entry in `tx.pendingAllocs`
   and `tx.pendingFrees`, determine which bitmap page contains that bit.
   Collect the set of modified bitmap pages.
2. For each modified bitmap page, construct the updated page content by
   applying the pending bit changes to the current mmap bitmap content.
   Write each modified bitmap page via `pwrite()`.
3. `fdatasync()` — ensures all bitmap changes are on stable storage.
4. Write the meta page via `pwrite()` with updated root pointers, TxnID,
   `HighWaterMark`, and checksum.
5. `fdatasync()` — ensures the meta page is on stable storage. This is the
   **atomic commit point**.

This ordering guarantees that the bitmap is always updated before the meta
page. If a crash occurs after step 3 but before step 5, the bitmap may
reflect the new transaction's allocations but the meta page still points
to the previous consistent state — the allocated pages are unreferenced
free pages (harmless). If a crash occurs before step 3, no bitmap changes
reach disk and the database is fully consistent.

Typically a commit writes 2-5 bitmap pages (the pages containing the bits
for allocated and freed pages) plus 1 meta page — a small, bounded number
of pwrite calls regardless of transaction size.

**Platform note:** On Linux, `fdatasync()` on a file descriptor flushes all
dirty pages for that file, including pages dirtied via `MAP_SHARED` mmap.
This is because the kernel's page cache is shared between the pwrite and
mmap paths. gmdb relies on this behavior for crash safety — the fdatasync
after bitmap pwrite also flushes the CoW'd data pages that were modified
directly in the mmap.

On macOS (and potentially other platforms), `fdatasync()` does **not**
guarantee that `MAP_SHARED` mmap dirty pages are flushed. On these
platforms, the commit path inserts an `msync(MS_SYNC)` call on the dirty
data page region before the bitmap pwrite. This ensures CoW'd data pages
are on stable storage before the bitmap and meta updates that reference
them. The msync call is implemented in `mmap_darwin.go` and gated by
platform — Linux skips it (fdatasync is sufficient). The commit sequence
on macOS becomes: `msync(MS_SYNC)` → bitmap pwrite → `fdatasync()` →
meta pwrite → `fdatasync()`.

**Bitmap leakage on crash.** A crash between the bitmap pwrite and the meta
pwrite can leak pages. The bitmap on disk has cleared bits (pages allocated
for the crashed transaction's CoW) but the meta on disk does not reference
those pages. These pages appear allocated but are unreferenced — they are
leaked, not free. The leaked page count is bounded by the number of pages
the crashed transaction allocated (typically small — a transaction
modifying a few keys in a depth-4 tree leaks 4-20 pages per crash).
`Check()` detects leaked pages as `CheckWarning` severity.
`CheckWithOptions(&CheckOptions{Repair: true})` reclaims leaked pages
in place (offline, exclusive access). `CopyTo(compact=true)` also
recovers them by rebuilding the file from scratch. In `SyncLazy` mode, the OS may also flush bitmap pwrite pages from
uncommitted (post-checkpoint) transactions, causing additional leakage on
crash. This is an accepted tradeoff of the deferred bitmap approach — the
alternative (versioned/CoW bitmap) would add significant complexity. The
B+tree structure is always consistent; only free space accounting may be
affected.

## Copy-on-Write (CoW) Transaction Model

### Write Transaction

1. Writer submits a request to the flock goroutine's writer queue and waits
   for the lock grant, respecting `ctx` cancellation (see Write Lock).
   Returns `context.Cause(ctx)` if cancelled while waiting, preserving the
   original cancellation reason.
2. Writer reads the active meta page to get current roots, TxnID, and file format.
3. For each modification (insert, update, delete):
   - Traverse the B+tree from root to leaf.
   - Copy each page along the path (CoW): allocate a fresh page from the
     bitmap via `pageAlloc()`, copy the old page's content from mmap
     position A to mmap position B, then modify position B in place.
   - Allocate new pages via `pageAlloc()` (loose pages → bitmap →
      RPL reclamation → lagging reader check → file extension).
   - The CoW set (`tx.cowPages`) tracks page IDs. Bitmap changes are
     deferred in `tx.pendingAllocs` and `tx.pendingFrees`.
   - Old pages are tracked: pages from previous transactions go to
     `tx.retiredPages`; pages CoW'd then freed in this transaction become
     loose pages in `tx.loosePages`.
4. Commit-time free space update:
   a. Perform tail page refund: check the bitmap for free pages at the end of
      the file, clear those bits, decrement `HighWaterMark`.
   b. Move remaining loose pages into `tx.pendingFrees` (bypass RPL — no
      reader can reference same-transaction pages).
   c. Append all `tx.retiredPages` to the RPL (allocating new segment pages
      from the bitmap if needed — bounded, non-recursive).
   d. Update `NumFreePages`, `RPLHeadPage`, `RPLTailPage`, `RPLEntryCount`.
5. Write modified bitmap pages to stable storage:
   - Compute modified bitmap pages from `tx.pendingAllocs` and
     `tx.pendingFrees`. Apply pending bit changes and write each modified
     bitmap page via `pwrite()`.
   - `fdatasync()` if `SyncMode` is `SyncDurable` or `SyncDataOnly`. Skipped for
     `SyncLazy` and `SyncUnsafe`. Data pages are already in the mmap —
     no data page writes are needed.
6. Update the inactive meta page with new root pointers, new TxnID, updated
   `HighWaterMark`, and checksum. Written via `pwrite()`.
7. `fdatasync()` the meta page if `SyncMode` is `SyncDurable`. Skipped for all
   other modes. This is the **atomic commit point**.
8. If the OS file size exceeds `HighWaterMark` by more than
   `ShrinkThreshold`, truncate the file via `ftruncate()`. This happens
   after the commit point — a crash before truncation leaves the file
   larger than necessary but consistent. The next commit will retry.
9. Writer signals the flock goroutine to release the lock (clears
   `WriterPID` and `WriterStartTime`, releases the flock, makes the
   goroutine available for the next writer in the queue).

### Read Transaction

1. Reader checks `ctx` — returns `context.Cause(ctx)` if already cancelled.
2. Reader acquires a slot in the reader table (shared memory) via scan+CAS
   and records the current TxnID from the active meta page. If no slots are
   available and the context has a deadline, the reader retries with short
   backoff (1ms) until a slot becomes free or the context expires. If the
   context has no deadline, returns `ErrReadersFull` immediately (no
   blocking). This allows callers to control wait behavior per-call via
   `context.WithTimeout`.
3. Reader traverses the B+tree using page pointers from that meta page. Because
   of CoW, all pages referenced by this TxnID are immutable — the writer will
   never modify them in place.
4. When done, the reader clears its slot in the reader table.

Readers never need locks on the data file. They never block writers. Writers
never block readers. The only contention point is the reader table slot
acquisition, which is a simple atomic CAS. The context governs the retry
window for slot acquisition but is not stored on the transaction.

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

4. Each collected closure is executed in its own **child transaction**
   (see Nested Transactions). Before executing a closure, its `ctx` is
   checked — if already cancelled, the closure is skipped and the caller
   receives `context.Cause(ctx)` to preserve the original cancellation
   reason.

5. If a closure returns an error, its child transaction is **rolled back**.
   The parent transaction is unaffected — other closures' child transactions
   remain intact. The failing caller receives the error.

6. If a closure succeeds, its child transaction is **committed** (merged
   into the parent). The caller will receive `nil` when the parent commits.

7. After all closures have run, the parent transaction is committed. All
   callers whose closures succeeded receive `nil` on their result channels.

8. If `Commit()` itself fails (e.g., I/O error), all callers in the batch
   receive the commit error.

#### Error Isolation

Each closure runs in its own child transaction. A failing closure is
rolled back independently — its modifications are discarded without
affecting other closures in the batch. Successful closures are committed
together in the parent transaction. This provides the same semantics as
`Update` from each caller's perspective: either their closure's effects
are committed, or they receive an error.

No rollback-and-retry is needed. No closure is ever re-executed. Each
closure runs **exactly once**.

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

#### Closure Contract

Each `Batch` closure executes **exactly once** within its own child
transaction. There is no rollback-and-retry — if a closure fails, only
its child transaction is rolled back. Closures may safely:

- Perform side effects (logging, metrics, channel sends) — they will
  not be replayed.
- Read and branch on values written by prior closures in the same
  batch (prior closures' child transactions have been committed into
  the parent, so their writes are visible).

The only constraint: the closure receives a `*Tx` (the child transaction)
and must perform all database operations through it, not through a
captured outer `*Tx`.

#### Coordinator Lifecycle

The batch coordinator goroutine is started lazily on the first `Batch()` call. It is stopped when `DB.Close()` is called: the close method closes `db.batchCh`, which causes the coordinator to drain any pending calls (returning `ErrTxClosed` to each), then exit. In-flight batch transactions are committed or rolled back before the coordinator exits.

### Nested Transactions

A write transaction can create child transactions that can be independently
committed (merged into the parent) or rolled back (discarded) without
affecting the parent's state. This is an in-memory mechanism — child
transactions never write to disk. Only the top-level parent commits.

#### Mechanics

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
- Copy `tx.pendingAllocs` (or record its length for truncation)
- Copy `tx.pendingFrees` (or record its length)
- Copy `tx.cowPages` (the set of CoW'd page IDs)
- Copy `tx.loosePages`
- Copy `tx.retiredPages` (or record its length)
- Snapshot keyspace root page IDs and counts

**Child does work:**
- CoW allocates fresh pages in the mmap, adding to `pendingAllocs` and
  `cowPages`. Old pages go to `retiredPages`. All modifications happen
  on the same maps as the parent — the child doesn't have its own maps.

**Child commit:**
- Discard the saved snapshots. The child's modifications remain in the
  parent's maps. No-op beyond freeing the snapshot memory. The parent
  continues with the merged state.

**Child rollback:**
- Restore `pendingAllocs`, `pendingFrees`, `cowPages`, `loosePages`,
  and `retiredPages` from the saved snapshots.
- Restore keyspace roots to their pre-child state.
- The child's CoW'd pages in the mmap are abandoned — they hold modified
  content at freshly allocated positions, but nobody references them.
  The bitmap on disk still shows them as free (bitmap modifications are
  deferred), so their content is irrelevant.
- Done. No buffer copying, no undo of mmap writes. The direct write
  architecture makes this cheap — CoW always writes to fresh pages, so
  the parent's pages are untouched.

**Nesting depth:** children can create their own children (arbitrary
nesting). Each level snapshots the current state. Rollback at any level
restores to that level's snapshot. Cost is proportional to the number
of pages modified at each level, not the total database size.

#### Why This Is Simple

In a pwrite/slab architecture, child rollback is hard: the child may
have modified a page buffer in the slab that the parent also uses.
Restoring the buffer requires saving its content before modification
(copy-on-first-write within the child) or maintaining layered buffer
sets.

With direct write mode, CoW **always** allocates a fresh page in the
mmap. The old page at the old position is untouched. Rolling back
means discarding the bookkeeping for the new pages. The mmap has
modified content at the abandoned positions, but since bitmap changes
are deferred (pwrite at commit), the on-disk bitmap still shows those
pages as free. No buffer restoration needed.

#### Interaction with Write Batching

Nested transactions eliminate the rollback-and-retry mechanism in
`Batch()`. Each closure runs in a child transaction. If a closure
fails, its child is rolled back — other closures' children are
unaffected. Closures execute **exactly once** and do not need to be
idempotent or side-effect-free. See Write Batching for details.

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
    // 2. Release the reader slot (or signal flock goroutine to release write lock).
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

3. **Release the write lock** (if writable): signal the flock goroutine
   to clear `WriterPID` and `WriterStartTime`, release the flock, and
   serve the next writer in the queue.

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

### Database Handle Leak Detection

The same `runtime.AddCleanup` pattern is applied to the `DB` struct
itself to detect `DB.Close()` leaks. A leaked `DB` holds open file
descriptors, mmap regions, and the flock goroutine — all of which are
process-scoped resources that outlive any individual transaction.

#### Setup

When `Open()` creates a `DB`, a cleanup is registered:

```go
db := &DB{...}
db.cleanup = runtime.AddCleanup(db, func(info dbCleanupInfo) {
    // 1. Log warning with the stack trace captured at Open() time.
    // 2. Stop the flock goroutine.
    // 3. munmap the data file and lock file mappings.
    // 4. Close all file descriptors (data file, lock fd).
}, dbCleanupInfo{
    openStack: captureStack(),
    logger:    opts.Logger,
    // ... fd and mmap references needed for cleanup ...
})
```

`dbCleanupInfo` is a separate struct — not the `DB` itself — to avoid
preventing GC collection. It contains only the information needed to
release resources and log a diagnostic.

#### Normal Close

When `Close()` is called, the cleanup is cancelled:

```go
func (db *DB) Close() error {
    db.cleanup.Stop()
    // ... normal close logic ...
}
```

#### Cleanup Behavior

When the GC collects a leaked `DB`:

1. **Log a warning** via the `*slog.Logger` captured at `Open()` time.
   The message includes the stack trace from `Open()` showing where
   the leaked handle was created.

2. **Stop the flock goroutine** by closing `db.writerCh`. The goroutine
   exits its loop and releases the flock if currently held.

3. **munmap** the data file mapping and the lock file mapping.

4. **Close file descriptors**: data file fd, lock file fd.

The same limitations apply as for `Tx` leak detection: timing is
non-deterministic (GC-dependent), the cleanup is a safety net for
debugging, and applications should not rely on it for normal operation.

## Cross-Process Coordination

### Lock File Layout

A separate file (`<dbname>.lock`) is mmap'd as shared memory by all processes.

```
Lock File
+---------------------------------------+
| Header (72 bytes)                     |
| Magic              | uint64          |  identifies file as gmdb lock file
| MaxReaders         | uint32          |  number of reader slots (set at creation)
| Padding            | 4 bytes         |  alignment
| UUID               | [16]byte        |  must match data file's UUID
| WriterPID          | uint64          |  PID of current write txn holder (0 = no writer)
| WriterStartTime    | uint64          |  process start time of writer
| WriterPIDNamespace | uint64          |  PID namespace inode of writer
| WriterHeartbeat    | uint64          |  monotonic clock, updated periodically
| LastMaintenanceTime| uint64          |  monotonic clock, updated after maintenance pass
+---------------------------------------+
| Reader Table                          |
| +-------+-----+-----+------+-------+ |
| | TxnID | PID | PST | PIDN | HB    | | Slot 0
| | u64   | u64 | u64 | u64  | u64   | |
| +-------+-----+-----+------+-------+ |
| | ...                                | | up to MaxReaders slots
| +-------+-----+-----+------+-------+ |
+---------------------------------------+
```

PST = Process Start Time. PIDN = PID Namespace. HB = Heartbeat.

The lock file structures are defined as Go structs with `structs.HostLayout`
(Go 1.24+), which guarantees the struct uses the host platform's C ABI
layout rules. This allows safely overlaying Go structs on the mmap'd
shared memory region without manual byte offset arithmetic or reliance
on unspecified Go compiler layout behavior:

```go
type LockFileHeader struct {
    _                  structs.HostLayout
    Magic              uint64
    MaxReaders         uint32
    _                  [4]byte  // explicit padding for 8-byte alignment
    UUID               [16]byte // must match data file's UUID
    WriterPID          uint64
    WriterStartTime    uint64   // process start time of writer (PID reuse detection)
    WriterPIDNamespace uint64   // PID namespace inode of writer (0 = unknown/non-Linux)
    WriterHeartbeat    uint64   // CLOCK_BOOTTIME nanos, updated by heartbeat goroutine
    LastMaintenanceTime uint64  // CLOCK_BOOTTIME nanos, updated after maintenance pass
}

type ReaderSlot struct {
    _                structs.HostLayout
    TxnID            uint64
    PID              uint64
    ProcessStartTime uint64 // process start time when slot was acquired
    PIDNamespace     uint64 // PID namespace inode (0 = unknown/non-Linux)
    Heartbeat        uint64 // CLOCK_BOOTTIME nanos, updated by heartbeat goroutine
}
```

The `HostLayout` marker is a compile-time guarantee — it applies only to
the lock file's shared memory structures. Data file page formats remain
defined as raw byte layouts with explicit encode/decode functions, since
those must be endian-aware and portable across architectures.

**Header (72 bytes):**
- `Magic` (uint64): Identifies the file as a gmdb lock file. Validates that
  the lock file belongs to this database and has not been corrupted.
- `MaxReaders` (uint32): Number of reader slots. Set at lock file creation
  time via `Options.MaxReaders` (default: 4096). Immutable after creation.
- Padding (4 bytes): Alignment to 8-byte boundary.
- `UUID` ([16]byte): Database UUID, copied from the data file's meta page
  at lock file creation time. On `Open()`, the lock file's UUID is compared
  against the data file's UUID. If they differ, the lock file is stale
  (belongs to a different database or the data file was replaced) — it is
  deleted and recreated. This prevents cross-database lock file confusion
  when files are moved, renamed, or replaced.
- `WriterPID` (uint64): PID of the process currently holding the write lock.
  Set when the write lock is acquired, cleared to 0 on release. Used for
  stale writer detection (see Stale Writer Recovery). Stored as uint64 for
  forward safety — Linux `pid_max` can reach 2^22 on 64-bit kernels, and
  uint64 provides consistent alignment with other fields.
- `WriterStartTime` (uint64): Process start time of the writer, stored
  alongside `WriterPID` for PID reuse detection. If `WriterPID` is non-zero
  and the PID is alive, the start time is compared against the PID's current
  start time — a mismatch means the PID was recycled and the original writer
  crashed. See Stale Writer Recovery and Process Start Time below.
- `WriterPIDNamespace` (uint64): PID namespace inode of the writer process
  (from `/proc/self/ns/pid` on Linux, 0 on other platforms). Used to
  determine whether the stale writer check can trust PID-based liveness
  detection. See PID Namespace Awareness below.
- `WriterHeartbeat` (uint64): Monotonic clock value (`CLOCK_BOOTTIME` on
  Linux, `CLOCK_MONOTONIC` on other platforms), updated periodically by
  the heartbeat goroutine while the write lock is held. Used as a fallback
  for stale writer detection when PID namespaces differ.
- `LastMaintenanceTime` (uint64): Monotonic clock value, updated after a
  maintenance pass completes. Used to coordinate maintenance across
  processes — each process checks this value before starting a pass and
  skips if a recent pass was completed within `MaintenanceOptions.Interval`.
  See Background Maintenance.

**Reader Slot (40 bytes):**
- `TxnID` (uint64, atomic): The snapshot transaction ID held by this reader.
  A value of 0 means the slot is free. Non-zero means the slot is active.
- `PID` (uint64, atomic): Process ID that owns this slot. Used for stale
  reader detection in the same PID namespace. Stored as uint64 for alignment
  consistency with TxnID and forward safety.
- `ProcessStartTime` (uint64, atomic): Process start time when the slot was
  acquired. Used alongside `PID` for PID reuse detection — if the PID is
  alive but its current start time differs from the stored value, the PID
  was recycled and the slot is stale. See Process Start Time below.
- `PIDNamespace` (uint64, atomic): PID namespace inode of the process that
  acquired this slot (from `/proc/self/ns/pid` on Linux, 0 on other
  platforms). See PID Namespace Awareness below.
- `Heartbeat` (uint64, atomic): Monotonic clock value, updated periodically
  (~1s) by the owning process's heartbeat goroutine. Used for stale
  detection when PID namespaces differ or PID-based checks are unavailable.

Total lock file size: 72 + (40 × MaxReaders). With default MaxReaders=4096:
72 + 163840 = 163912 bytes (~160KB).

The lock file is mmap'd with `MAP_SHARED` by all processes for the reader table.
The write lock is a separate concern handled via `flock()` (see below).

### Lock File Lifecycle

The lock file is ephemeral. The first process to open the database creates the
lock file, writes the header (including `Magic`, `MaxReaders`, `WriterPID=0`,
`WriterStartTime=0`), and initializes all reader slots to zero. Subsequent processes validate `Magic`,
read `MaxReaders` from the header, and mmap the file at the corresponding size.
If the lock file is deleted (e.g., after all processes exit), the next opener
recreates it. `MaxReaders` is NOT stored in the data file — it is a runtime
coordination property, not a data property.

On open, if the lock file already exists, the opener checks `WriterPID`. If
non-zero, the opener determines whether the writer is still alive using
`kill(pid, 0)` and `WriterStartTime` comparison (see Process Start Time).
If the writer is dead or the PID was recycled, the writer crashed while
holding the lock — see Stale Writer Recovery.

### Write Lock

Write serialization uses two layers:

- **Intra-process**: a writer queue managed by a single **flock goroutine**
  on the `DB` struct. Writers submit requests via a channel and receive
  the lock grant via a per-request response channel. This prevents two
  goroutines in the same process from attempting concurrent writes while
  supporting context-aware cancellation with zero goroutine accumulation.
- **Cross-process**: `flock(LOCK_EX)` on the lock file, acquired and
  released exclusively by the flock goroutine. This prevents writers in
  different processes.

#### Flock Goroutine

The `DB` struct maintains a single persistent goroutine (started at
`Open()` time, stopped at `Close()`) that is the sole owner of flock
acquisition and release. At most one goroutine is ever blocked in the
`flock()` syscall.

```
db.writerCh chan writerRequest

type writerRequest struct {
    ctx    context.Context
    result chan<- error  // nil = lock granted; non-nil = cancelled/error
}
```

The flock goroutine runs a loop:

1. Read the next `writerRequest` from `db.writerCh`.
2. Check `req.ctx` — if already cancelled, send `context.Cause(req.ctx)`
   on `req.result` and loop back to step 1.
3. If the flock is not currently held (no cross-process contention),
   acquire `flock(LOCK_EX)` — this blocks in the kernel until granted.
4. While blocked in `flock`, the goroutine cannot check `req.ctx`.
   However, since this is the only goroutine that ever calls `flock`,
   there is no goroutine accumulation. The flock completes when the
   external writer releases it.
5. On flock acquisition, check `req.ctx` again — if cancelled while
   waiting, release the flock immediately and send the cancellation
   error. Loop back to step 1 to serve the next waiter.
6. If `req.ctx` is still valid, store the caller's PID and process start
   time in the lock file header's `WriterPID` and `WriterStartTime`
   fields and send `nil` on `req.result` — the writer now holds the lock.
7. Wait for the writer to signal completion (via a release channel
   provided alongside the request). On release: clear `WriterPID` and
   `WriterStartTime` to 0, release the flock, loop back to step 1.

#### Writer Acquisition Flow

A `Begin(ctx, writable=true)` call:

1. Send a `writerRequest{ctx, result}` to `db.writerCh`.
2. `select` on `result` and `ctx.Done()`:
   - If `result` receives `nil`: lock is granted. Proceed with the
     write transaction.
   - If `result` receives a non-nil error: lock was not granted (e.g.,
     the flock goroutine detected a stale writer and recovery failed).
   - If `ctx.Done()` fires first: the writer gives up. The flock
     goroutine will detect the cancelled context when it processes the
     request (step 2 or 5 above) and skip or release accordingly.
     Return `context.Cause(ctx)`.

`Commit()` and `Rollback()` signal the flock goroutine to release the
lock via the release channel.

#### Why This Design

The previous approach — spawning a new goroutine per write lock attempt
to call `flock(LOCK_EX)` — suffered from goroutine accumulation under
rapid context cancellation. Each cancelled attempt left a goroutine
blocked in `flock` until it acquired and released, draining one-by-one.
Under pathological cancellation patterns (e.g., request timeouts in a
web server), this could accumulate hundreds of goroutines.

The single flock goroutine eliminates this: at most one goroutine is
ever in the `flock` syscall. Cancelled writers simply dequeue — they
never touch flock. The goroutine is a fixed-cost resource (one per `DB`
instance, ~8KB stack) that exists for the lifetime of the database
handle.

This two-layer approach (intra-process queue + cross-process flock) is
necessary because `flock()` is per-fd and per-process — a second
goroutine calling `flock()` on the same fd would succeed immediately
(the kernel considers the lock already held by this process).

The `DB` struct holds a single dedicated fd for the write lock
(`db.lockFd`), opened separately from the fd used for the reader table
mmap. This fd is used exclusively for `flock()`/`funlock()` calls.

#### Stale Writer Recovery

If a process crashes while holding the write lock, `WriterPID` remains non-zero
and the `flock()` is automatically released by the kernel (flock locks are
released on fd close / process exit). On `Open()` or when attempting to acquire
the write lock, if `WriterPID` is non-zero, the process determines whether the
writer is still alive using the same namespace-aware logic as reader stale
detection:

1. **Same PID namespace** (`WriterPIDNamespace` == checker's, both non-zero):
   a. `kill(pid, 0)` — if it returns `ESRCH`, the process is dead.
   b. If alive, compare `WriterStartTime` — if different, PID was recycled.
2. **Different PID namespace** (or either is 0): check `WriterHeartbeat`.
   If `now - WriterHeartbeat > StaleTimeout`, the writer is dead.

Based on the result:

- If alive (PID check or fresh heartbeat): the writer is still running —
  proceed with normal `flock()` which will block until the writer finishes.
- If dead (PID check or stale heartbeat): the writer crashed. The flock is
  already released by the kernel. The new writer acquires the flock, then
  performs recovery:
  1. Read both meta pages and select the valid one (highest TxnID with valid
     checksum). The crashed writer's partial commit is invisible — CoW ensures
     the previous meta page points to a consistent tree.
  2. Scan the reader table for slots with the dead writer's PID (in the same
     PID namespace) and clear them (the crashed process may have also held
     read transactions). Each slot is validated via `ProcessStartTime` and
     `PIDNamespace` to avoid clearing slots from a different process.
  3. Clear `WriterPID`, `WriterStartTime`, `WriterPIDNamespace`, and
     `WriterHeartbeat` to 0 (they will be set to the new writer's values
     shortly).

No special rollback logic is needed for tree consistency — the CoW model
guarantees that the previous meta page points to a fully consistent tree.

Bitmap modifications are deferred in memory (`tx.pendingAllocs` and
`tx.pendingFrees`) and only written to disk via `pwrite()` at commit time.
If the writer crashes before commit, no bitmap modifications reach disk —
the on-disk bitmap is fully consistent with the previous meta page. No
leaked pages. CoW'd data pages at new positions in the mmap may have been
flushed by the kernel, but they are unreferenced (free pages in the on-disk
bitmap) so their content is irrelevant.

### Reader Table

Slot allocation uses a simple scan with atomic CAS — no free stack or other
auxiliary data structure. The reader table is a flat array of 40-byte slots
stored in the lock file's shared mmap. All operations use atomic memory
operations visible across processes.

**Slot acquire (`Begin` read transaction):**
1. Start scanning from the **slot hint** (`db.readerSlotHint`, an
   `atomic.Uint32` on the `DB` struct) rather than slot 0. The hint
   caches the index of the last successfully acquired slot, so the scan
   begins in a region likely to contain free slots.
2. Scan forward (with wraparound) for a slot where `TxnID == 0` (free).
3. Atomically CAS the `TxnID` field from 0 to the current meta page's TxnID.
   If the CAS fails (another goroutine or process claimed the slot
   concurrently), continue scanning.
4. After successful CAS, store the caller's PID, cached process start
   time (`db.processStartTime`), PID namespace (`db.pidNamespace`), and
   current monotonic clock in the slot's `PID`, `ProcessStartTime`,
   `PIDNamespace`, and `Heartbeat` fields. The CAS establishes ownership
   — only the CAS winner writes these fields.
5. Register the slot index with the heartbeat goroutine's active slot list.
6. Update `db.readerSlotHint` to the acquired slot's index.
7. If all slots are occupied (full wraparound), return `ErrReadersFull`.

The CAS on `TxnID` establishes slot ownership. Only the CAS winner writes
PID, ProcessStartTime, PIDNamespace, and Heartbeat — there is no race
because the CAS serializes access to the slot. The writer's stale-reader
scan treats slots with non-zero TxnID and zero PID as "being acquired"
and skips them (does not clear it as stale). This window is brief — the
time between a successful CAS and the subsequent field stores.

The hint is process-local (stored on the `DB` struct, not in shared
memory) and updated with a relaxed atomic store — no cross-process
coordination. Under steady-state load where a process repeatedly opens
and closes read transactions, the hint points to a recently-freed slot
and the scan completes in 1–2 iterations. In the worst case (all slots
before the hint are occupied), the scan wraps around and degrades to
O(MaxReaders) — no worse than scanning from slot 0.

The CAS on `TxnID` is the serialization point. With 40-byte slots,
4096 slots = 160KB — fits in L2 cache, sequential scan with hardware
prefetching.

**Slot release (`Commit`/`Rollback` read transaction):**
1. Store `TxnID = 0` (atomic store). This single operation makes the slot
   free. The PID field is left as-is — it is only meaningful when `TxnID`
   is non-zero.

The release is a single atomic store. No CAS needed — only the slot owner
writes to its own slot.

**Stale reader detection:** During the writer's reader table scan (to find
the minimum active TxnID), if a slot has a non-zero `TxnID`, the writer
checks whether the owning process is still alive and is the *same* process
that acquired the slot:

0. If `PID == 0` (but `TxnID != 0`), the slot is being acquired — a reader
   has won the CAS on TxnID but has not yet stored its PID. The writer
   skips this slot (does not clear it as stale). This avoids a race where
   the writer clears a slot between the reader's CAS and its PID store.
1. **Same PID namespace** (slot's `PIDNamespace` == checker's, both non-zero):
   use the fast PID + StartTime path:
   a. `kill(pid, 0)` — if it returns `ESRCH`, the process is dead. Stale.
   b. If alive, compare `ProcessStartTime` — if different, PID was recycled.
      Stale.
   c. If both match, the process is alive and holding the slot legitimately.
2. **Different PID namespace** (or either is 0): PID-based checks would
   inspect the wrong process. Fall back to heartbeat:
   a. If `now - Heartbeat > StaleTimeout` → stale. Clear `TxnID = 0`.
   b. If heartbeat is fresh → not stale. The owning process is still
      updating the slot.
3. If neither PID nor heartbeat can determine liveness (e.g., `PIDNamespace`
   is 0 and `Heartbeat` is 0 — a slot from a process that predates heartbeat
   support), fall back to PID-only liveness (`kill(pid, 0)`). This is the
   legacy path, vulnerable to cross-namespace false results but safe within
   a single namespace.

The PID namespace check prevents the dangerous cross-namespace failure mode
where `kill(pid, 0)` inspects a different process than the slot owner. When
two containers share a database file via volume mount, each process stores
its PID namespace inode in the slot. A writer in container B sees that
container A's slot has a different namespace and uses the heartbeat instead
of trusting `kill()`. See PID Namespace Awareness below.

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

#### Process Start Time

To detect PID reuse (where a new process is assigned the same PID as a
crashed process), both reader slots and the writer header store the
process's **start time** alongside its PID. The start time is a
monotonically-increasing value that changes when a PID is recycled,
providing a unique `(PID, StartTime)` tuple per process lifetime.

At `Open()` time, the process reads its own start time once and caches it
on the `DB` struct (`db.processStartTime uint64`). This cached value is
stored in reader slots on `Begin()` and in `WriterStartTime` on write lock
acquisition.

During stale detection, the writer reads the current start time for a
given PID via `processStartTime(pid int) (uint64, error)`. If the PID is
alive but its current start time differs from the stored value, the PID
was recycled and the slot/writer is stale.

**Platform-specific implementations:**

| Platform | Source | Value | Notes |
|----------|--------|-------|-------|
| Linux | `/proc/[pid]/stat` field 22 (`starttime`) | Clock ticks since boot (`uint64`) | Readable without privileges. Monotonic, survives PID reuse. Pure Go: `os.ReadFile` + parse. |
| macOS | `sysctl` with `KERN_PROC_PID` → `kinfo_proc.kp_proc.p_starttime` | `timeval` packed as `sec*1_000_000 + usec` (`uint64`) | Accessible for same-user processes. Pure Go via `syscall.Sysctl`. |
| FreeBSD | `sysctl` with `KERN_PROC_PID` → `kinfo_proc.ki_start` | `timeval` packed as `sec*1_000_000 + usec` (`uint64`) | Same interface as macOS. Pure Go via `syscall.Sysctl`. |

All implementations are pure Go (no cgo required). The
`processStartTime` function is defined per platform via build tags
(`process_linux.go`, `process_darwin.go`, `process_freebsd.go`).

If `processStartTime` fails (e.g., insufficient permissions to read
another process's info), the stale check falls back to heartbeat-based
detection if a heartbeat is available, or PID-only liveness
(`kill(pid, 0)`) as a last resort.

#### PID Namespace Awareness

PID-based liveness detection (`kill(pid, 0)` and `/proc/[pid]/stat`)
operates within the caller's PID namespace. When multiple containers
share a database file via volume mount, each container has its own PID
namespace — a PID in one namespace refers to a different (or nonexistent)
process in another. This causes two failure modes:

- **False dead**: Container A holds a reader slot at PID 42. Container B
  has no PID 42. `kill(42, 0)` from B returns `ESRCH` — B clears the
  slot, removing snapshot protection for A's active reader.
- **False alive**: Container A crashes with PID 42 in a slot. Container B
  happens to also have a PID 42. `kill(42, 0)` from B succeeds — the
  slot is never reclaimed.

To prevent this, each reader slot and the writer header store the
process's **PID namespace inode** alongside the PID. On Linux, this is
read from `/proc/self/ns/pid` via `readlink` at `Open()` time and cached
on the `DB` struct (`db.pidNamespace uint64`). On non-Linux platforms,
the value is 0 (no PID namespaces).

During stale detection, the writer compares its own PID namespace against
the slot's. If they match (same namespace), the PID + StartTime fast path
is safe. If they differ (cross-namespace), the writer uses the heartbeat
instead. This eliminates both false-dead and false-alive cross-namespace
failures.

**Platform-specific implementations:**

| Platform | Source | Value | Notes |
|----------|--------|-------|-------|
| Linux | `readlink /proc/self/ns/pid` | Inode number (`uint64`) | Unique per PID namespace. Pure Go: `os.Readlink`. |
| macOS | N/A | 0 | No PID namespaces. PID-based checks always valid. |
| FreeBSD | N/A | 0 | No PID namespaces (jails use different mechanism). PID-based checks always valid within the same jail. |

#### Heartbeat Goroutine

The `DB` struct maintains a **heartbeat goroutine** (started at `Open()`
time, stopped at `Close()`) that periodically updates the `Heartbeat`
field on all reader slots and the writer header held by this process.

The goroutine ticks every ~1 second and writes the current monotonic clock
value (`CLOCK_BOOTTIME` on Linux, `CLOCK_MONOTONIC` on other platforms)
to each active slot. The `DB` struct maintains an in-process list of
active reader slot indices (appended on `Begin()`, removed on
`Commit()`/`Rollback()`). Each tick iterates this list and performs one
atomic store per slot.

`CLOCK_BOOTTIME` is used on Linux because it is monotonic, survives
suspend/resume, and is shared across all containers on the same host (it
is a kernel-wide clock, not per-PID-namespace). Two containers mmap'ing
the same lock file see consistent clock values.

The `StaleTimeout` option (default: 10 seconds) controls how long a
heartbeat must be stale before the slot is reclaimed. This must be
significantly larger than the heartbeat interval (1s) to account for
scheduling delays under heavy load.

```go
// StaleTimeout is the duration after which a reader slot with a stale
// heartbeat is considered abandoned. Only used for cross-PID-namespace
// stale detection (same-namespace detection uses instant PID liveness
// checks). Default: 10s.
StaleTimeout time.Duration
```

The heartbeat goroutine is a fixed-cost resource: one goroutine per `DB`
handle (~8KB stack), one atomic store per active reader slot per second.
No syscalls, no allocations. Same lifecycle pattern as the flock goroutine.

#### Atomic Operations Convention

The codebase uses two distinct atomic access patterns depending on the
memory being accessed:

- **In-process fields** (`DB`, `Tx` struct fields such as
  `db.readerSlotHint`, stats counters, the clock hand, etc.) use Go's
  **typed atomics** (`atomic.Uint64`, `atomic.Uint32`, `atomic.Int64`).
  Typed atomics prevent accidental non-atomic reads — the compiler
  enforces that all access goes through the atomic methods. These fields
  are never visible to other processes.

- **Shared-memory fields** (reader table `TxnID`, `PID`,
  `ProcessStartTime`, `PIDNamespace`, `Heartbeat`; header `WriterPID`,
  `WriterStartTime`, `WriterPIDNamespace`, `WriterHeartbeat` in the
  mmap'd lock file) use the **function-based atomics**
  (`atomic.LoadUint64`, `atomic.StoreUint64`,
  `atomic.CompareAndSwapUint64`) on `unsafe.Pointer`-derived addresses.
  Typed atomics cannot be used here because the memory is not a Go
  struct field — it is a raw region in a `MAP_SHARED` mmap visible
  across processes.

### Writer's Page Reclamation

Before reclaiming retired pages, the writer scans the reader table to find the
minimum active TxnID. Any RPL entries with TxnID < min_active are safe to
reclaim — their bits are set in the allocation bitmap, making them available
for allocation.

### Lagging Reader Handling

A single long-lived reader prevents all RPL reclamation for transactions
newer than its snapshot, causing unbounded file growth. To address this, the
application can register a `LaggingReader` callback via `Options` (see API
Surface) that is invoked when a reader is blocking page allocation.

The callback is invoked from `pageAlloc()` when:
1. The allocation bitmap has no suitable free pages.
2. The RPL has no more reclaimable entries (all remaining entries have
   `TxnID >= oldestReader`).
3. A reader in the reader table is blocking reclamation.

The callback receives information about the lagging reader and returns an
action. `LaggingReaderWait` causes `pageAlloc()` to refresh the reader table
and retry (the reader may have released its slot in the meantime).
`LaggingReaderAbort` causes `pageAlloc()` to return `ErrDBFull`.

The callback is invoked at most once per `pageAlloc()` call to avoid busy
loops. The application can use the callback to log warnings, send alerts,
or take corrective action (e.g., killing a stuck process identified by PID).

## mmap Strategy

The data file is mapped read-write by default. All B+tree page modifications
happen directly in the mmap. Bitmap and meta pages are written via
`pwrite()` at commit time to ensure ordered writes.

When `Options.ReadOnly` is true, the data file is mapped with `PROT_READ`
only. Write transactions are rejected with `ErrReadOnly`. The lock file
remains writable for reader slot acquisition (CAS operations). This allows
opening databases on read-only media or with read-only filesystem
permissions, provided the lock file is on writable storage.

### Page Memory Management

All page memory is managed by the OS page cache. CoW'd pages in the mmap
are backed by the kernel's page cache like any other file-backed
`MAP_SHARED` page. When memory pressure is high, the kernel writes modified
mmap pages to disk and reclaims physical memory. gmdb performs no
application-level page eviction. This eliminates the need for anonymous
mmap slabs, eviction algorithms, or page count limits. The OS has global
visibility into memory pressure across all processes and is better
positioned to make eviction decisions.

### Read Path

All processes mmap the data file with:
```
MAP_SHARED | PROT_READ | PROT_WRITE    (default)
MAP_SHARED | PROT_READ                 (ReadOnly mode)
```

Reads go directly through the mmap. No system calls, no copies. The OS page
cache serves the data. Page lookup is always `mmap[pageID * pageSize]` — one
level, no branches.

### Write Path

The writer modifies B+tree pages directly in the mmap:
- CoW applies: the writer allocates a fresh page from the bitmap via
  `pageAlloc()`, copies the old page's content from mmap position A to mmap
  position B, then modifies position B in place.
- Bitmap modifications are deferred in memory (`tx.pendingAllocs` and
  `tx.pendingFrees`) — the mmap bitmap is read-only during transactions.
- At commit time, modified bitmap pages are written via `pwrite()` →
  `fdatasync()` → meta page via `pwrite()` → `fdatasync()`. This ensures
  the bitmap is always updated on disk before the meta page.
- Data pages are already in the mmap and do not need explicit writes.
  The OS page cache flushes them to disk in the background; the commit-time
  `fdatasync()` before the meta page write ensures they are on stable
  storage before the meta page makes them reachable.

**Crash safety**: bitmap and meta on disk are only updated via ordered
pwrite+fdatasync. CoW'd data pages at new positions in the mmap may be
flushed by the kernel at any time, but they are unreferenced (free pages
in the on-disk bitmap) until the commit completes — so their content is
irrelevant if a crash occurs before commit.

**Rollback**: discard `tx.pendingAllocs` and `tx.pendingFrees`, done.
The mmap bitmap is unchanged (it was never modified directly). CoW'd pages
in the mmap are at positions that the on-disk bitmap considers free, so
they are harmless.

The full commit path is described in the Write Transaction section (see
Copy-on-Write Transaction Model, steps 5–7).

### mmap Resizing

The mmap region is sized to `MaxSize` (the maximum database size in pages).
This over-allocates virtual address space — only the file-backed portion is
usable, but the mapping does not need to change as the file grows or shrinks.
The unmapped region beyond the file size will SIGBUS if accessed, so readers
must check `HighWaterMark` from the meta page.

**Note**: `MAP_SHARED` file-backed mappings are not charged against Linux
`vm.overcommit_memory` accounting — the file is the backing store, not swap.
However, per-process `RLIMIT_AS` limits do apply to virtual address space
reservations regardless of mapping type. On most default configurations
`RLIMIT_AS` is unlimited and this is not an issue. Users with restrictive
`RLIMIT_AS` settings may need to lower `MaxSize`.

### Prefaulting (Linux 5.14+)

When `Options.PreloadPages` is true, the database calls
`madvise(MADV_POPULATE_READ)` on the file-backed portion of the mmap
(pages 0 through `HighWaterMark - 1`) at open time. This pre-faults
all pages into the OS page cache, eliminating page faults on first access.

Benefits:
- **Predictable latency**: the first read transaction after open does not
  pay per-page fault costs. Useful for latency-sensitive workloads where
  cold-start performance matters.
- **Sequential I/O**: the kernel reads pages sequentially during prefault,
  which is more efficient than the random-access pattern of demand paging.

`MADV_POPULATE_READ` (Linux 5.14+) is used instead of `MAP_POPULATE`
because it works on `MAP_SHARED` mappings and returns errors synchronously
(e.g., if the file is truncated concurrently). If the kernel does not
support `MADV_POPULATE_READ`, the madvise call fails silently and pages
are faulted on demand as usual.

Prefaulting is also performed internally during `CopyTo()` on the source
database's mmap, since the copy reads the entire file sequentially.

The `PreloadPages` option defaults to false — most workloads benefit from
demand paging where only accessed pages enter the page cache.

### Huge Pages (Linux)

When `Options.HugePages` is true, the database calls
`madvise(MADV_HUGEPAGE)` on the data file mmap after mapping. This enables
transparent huge page (THP) backing for the file-mapped region, allowing the
kernel to use 2MB pages instead of 4KB pages where possible.

Benefits:
- **Reduced TLB pressure**: A 1GB database at 4KB pages requires 262,144
  TLB entries. With 2MB huge pages, only 512 entries are needed — a 512x
  reduction. This is significant for random-access workloads (B+tree
  traversals) where TLB misses dominate latency.
- **Fewer page faults**: Each fault maps 2MB instead of 4KB, reducing total
  fault count for sequential access patterns.

THP for file-backed `MAP_SHARED` mappings is mature on Linux 6.x kernels.
The kernel promotes pages to huge pages opportunistically based on alignment
and availability — not all pages will be huge-page-backed.

The `HugePages` option defaults to false. On non-Linux platforms the option
is ignored. On Linux kernels without THP support for file-backed mappings,
the madvise call has no effect.

### Read Transaction Cooldown (Linux 5.4+)

When `Options.ReclaimOnClose` is true, closing a read transaction calls
`madvise(MADV_COLD)` on the mmap region that the transaction accessed.
This hints the kernel that the pages are no longer actively used and may
be reclaimed from the page cache under memory pressure.

This is useful for batch processing workloads that perform large sequential
scans (e.g., exports, analytics queries) and then release the transaction.
Without `MADV_COLD`, the scanned pages remain in the page cache, potentially
evicting more useful pages from other workloads.

The implementation tracks the min/max page IDs accessed during the
transaction (lightweight — just two atomic min/max updates per page read)
and issues a single `madvise(MADV_COLD, min*PageSize, (max-min+1)*PageSize)`
on close.

The `ReclaimOnClose` option defaults to false. On non-Linux platforms or kernels
older than 5.4, the madvise call is silently ignored.

## Durability Modes

The database supports three safe durability modes and one unsafe mode,
configurable via `Options.SyncMode`. The mode controls which `fdatasync()`
calls are performed during commit. All safe modes preserve **database
integrity** (the file is always structurally valid). `SyncUnsafe` is the
unsafe mode — it risks corruption on crash and requires explicit opt-in
via `Options.AllowSyncUnsafe = true`. The tradeoff is between commit latency
and how much data may be lost on a crash.

| Mode | Data Sync | Meta Sync | On Crash | Performance |
|------|-----------|-----------|----------|-------------|
| `SyncDurable` (default) | `fdatasync()` | `fdatasync()` | No data loss. Full ACID. | Slowest |
| `SyncDataOnly` | `fdatasync()` | skip | Last committed transaction may be lost. DB is consistent — falls back to previous meta page. | ~2x faster |
| `SyncLazy` | skip | skip | Rolls back to the last **checkpoint** (the last commit that was explicitly synced via `DB.Checkpoint()` or the last `SyncDurable`/`SyncDataOnly` commit). DB is always consistent — no corruption. | Much faster |
| `SyncUnsafe` | skip | skip | **Risk of corruption.** No guarantees. Requires `Options.AllowSyncUnsafe = true`. For benchmarks and ephemeral data only. | Fastest |

### Checkpoints

In `SyncLazy` mode, a commit writes bitmap and meta pages via `pwrite()`
but skips all `fdatasync()` calls. Data pages are already in the mmap. The
OS page cache holds the writes, which will eventually reach disk, but the
order is not guaranteed.

A **checkpoint** is a commit whose data pages have been confirmed on
stable storage. (The meta page itself may or may not be synced — what
matters is that the data it references is durable. If the meta survived a
crash, recovery can trust it without further validation.) Checkpoints
occur when:
- `DB.Checkpoint()` is called explicitly (forces `fdatasync()` of the data file).
- A commit happens in `SyncDurable` or `SyncDataOnly` mode (these sync data
  pages as part of their normal commit path).

Each meta page carries a **checkpoint flag** — a boolean indicating whether the
data pages it references have been confirmed on stable storage. The checkpoint flag
is set when `fdatasync()` completes successfully (either from a
`SyncDurable`/`SyncDataOnly` commit or an explicit `DB.Checkpoint()` call). In
`SyncLazy` mode, commits write the meta page with the checkpoint flag **clear**.
A subsequent `DB.Checkpoint()` re-writes the meta page with the checkpoint flag
**set** (this is safe — the meta page is small and atomic).

On recovery after a crash, `Open()` performs the following:

1. Read both meta pages. Discard any with an invalid xxhash64 checksum.
2. Of the valid meta pages, select the one with the higher TxnID whose
   checkpoint flag is **set**. This is the last commit whose data pages are
   confirmed on stable storage.
3. If neither meta page has the checkpoint flag set (the user never called
   `DB.Checkpoint()` and never used `SyncDurable`/`SyncDataOnly`), select the
   meta page with the higher TxnID. In this case, the database has no
   checkpoint to fall back to — data integrity depends on whether
   the OS flushed pages in the right order, which is not guaranteed.
   `Open()` logs a warning via the configured `slog.Logger` when recovery
   selects a meta page with no checkpoint flag set, indicating the database
   was running in `SyncLazy` mode without periodic `Checkpoint()` calls.
4. Non-checkpoint meta pages (checkpoint flag clear) are never preferred over
   checkpoint ones, regardless of TxnID. A checkpoint meta at TxnID 100 is
   chosen over a non-checkpoint meta at TxnID 105 — the 5 transactions
   since the last checkpoint are lost.

Recovery does not attempt to validate a non-checkpoint meta's tree (e.g.,
by checking the root page checksum). A valid root page does not prove
that all reachable child pages, bitmap state, and RPL segments are
durable — the OS may have flushed pages in any order. Accepting a
partially-durable tree would risk surfacing `ErrCorrupted` on later
reads when traversals reach unflushed pages. The checkpoint's tree
is guaranteed intact because CoW never modifies existing pages.

### SyncUnsafe Warning

`SyncUnsafe` provides no crash safety whatsoever. Because `pwrite()` ordering
is not guaranteed without `fdatasync()`, the meta page could reach disk before
the bitmap pages. A crash in this state leaves the meta page pointing to a
tree whose bitmap state is inconsistent — the database may be **corrupted**.
Use this mode only for ephemeral data or benchmarks where the database can
be discarded after a crash.

To prevent accidental use, `SyncUnsafe` requires `Options.AllowSyncUnsafe = true`
to be set explicitly. Setting `SyncMode = SyncUnsafe` without `AllowSyncUnsafe`
returns an error from `Open()`. This ensures that unsafe mode is always a
deliberate choice, never an accidental misconfiguration.

The full commit path with mode-dependent behavior is described in the
Write Transaction section (see Copy-on-Write Transaction Model, steps 5–7).

## File Format

The database file size is managed dynamically between configurable lower and
upper bounds. The file format is stored in the meta page and controls how the
file grows and shrinks.

### File Format Parameters

| Parameter | Meta Field | Description | Default |
|-----------|-----------|-------------|---------|
| Lower bound | `MinSize` | Minimum file size in pages. File never shrinks below this. | `2 + BitmapPages` (meta + bitmap) |
| Upper bound | `MaxSize` | Maximum file size in pages. Determines mmap reservation and bitmap size. **Immutable after creation.** | 256GB / PageSize |
| Growth step | `GrowStep` | Number of pages to grow by when extending the file. | 65536 pages (256MB at 4KB pages) |
| Shrink threshold | `ShrinkThreshold` | Shrink the file when `fileSize - HighWaterMark > threshold`. | 131072 pages (512MB at 4KB pages) |

File format is set at database creation time via `Options` and persisted in the
meta page. `MinSize`, `GrowStep`, and `ShrinkThreshold` can be modified
by calling `Tx.SetFileFormat()` on a write transaction — the new values take
effect when the transaction commits.

**`MaxSize` is immutable after creation.** The allocation bitmap occupies a
fixed region of pages (starting at page 2) whose size is determined by
`MaxSize` at creation time (see Allocation Bitmap). Increasing `MaxSize`
would require expanding the bitmap region, which would shift all data page
offsets — every page ID in every B+tree, RPL segment, and keyspace descriptor
would become invalid. Decreasing `MaxSize` below the current
`HighWaterMark` would orphan allocated pages. Neither operation is
feasible without a full database rebuild.

To change `MaxSize`, use `CopyTo(path, compact)` to create a new database
with different `Options.FileFormat.MaxSize`, then replace the original file.
`SetFileFormat()` returns an error if the caller attempts to change `MaxSize`.

### File Growth

When `pageAlloc()` needs to extend the file:
1. Calculate new size: `alignUp(HighWaterMark + needed, GrowStep)`.
2. Clamp to `MaxSize`. If the new size would exceed `MaxSize`, return
   `ErrDBFull`.
3. Extend the file via `ftruncate()`. The existing mmap (which reserves up to
   `MaxSize`) covers the new pages automatically — no remap needed.

### File Shrinkage

After the commit point (step 7 of the write transaction), if the OS file size
exceeds `HighWaterMark` by more than `ShrinkThreshold`:
1. Calculate new size: `alignUp(HighWaterMark, GrowStep)`.
2. Clamp to `MinSize`.
3. Truncate the file via `ftruncate()`. The mmap reservation remains at
   `MaxSize` — the truncated region becomes unmapped (SIGBUS on access),
   which is safe because `HighWaterMark` in the meta page prevents any
   reader from accessing those pages.

File shrinkage is automatic and zero-overhead — it happens as a natural
consequence of the tail page refund mechanism during commit. No explicit
compaction is needed for the common case of data deletion.

## Keyspaces

The root meta page points to a "keyspace B+tree" — a B+tree whose keys are
keyspace names (byte strings) and whose values are keyspace descriptors:

```
Keyspace Descriptor (32 bytes)
+----------+----------+----------+----------------+----------+----------+
| Root     | Count    | Kind     | FixedValueSize | NextSeq  | Reserved |
| uint64   | uint64   | uint8    | uint16         | uint64   | [5]byte  |
+----------+----------+----------+----------------+----------+----------+
```

Total descriptor size: 8 + 8 + 1 + 2 + 8 + 5 = 32 bytes.

- **Root** (uint64): Page ID of this keyspace's B+tree root. 0 = empty
  keyspace (no data yet).
- **Count** (uint64): Number of key-value pairs. For SetKeyspace, this is
  the total number of key-value pairs across all value sets.

Depth (tree height) is not persisted — it is derived by reading the root
page on first access, consistent with the nested B+tree reference format
(see Set Keyspace Storage). This avoids maintaining a redundant
field across split, merge, and rebalance operations.
- **Kind** (uint8): Keyspace type. `0` = Keyspace (key → value),
  `1` = SetKeyspace (key → sorted set of values). `Open()` rejects
  unknown Kind values. Set at creation time, immutable after. Opening a
  keyspace with the wrong type (e.g., `OpenKeyspace` on a SetKeyspace)
  returns `ErrKeyspaceKindMismatch`.
- **FixedValueSize** (uint16): Fixed value size in bytes for SetKeyspace.
  0 = variable-size values. Must be 0 when Kind=0. A `Put()` with a
  value of the wrong size returns `ErrValueSizeMismatch`. Set at creation
  time, immutable after.
- **NextSeq** (uint64): Next sequence number for `NextSequence()`. Starts
  at 0 (first call returns 1). Updated on each `NextSequence()` call
  within a write transaction, persisted when the transaction commits.
  Available on both Keyspace and SetKeyspace.
- **Reserved** ([5]byte): Must be zero. Reserved for future fields.
  `Open()` rejects descriptors with non-zero reserved bytes.

Opening a keyspace within a transaction reads the descriptor from the keyspace
B+tree. Modifications to the keyspace update the descriptor (and its root)
which propagates up through the keyspace B+tree via CoW.

### Keyspace Name Interning

Keyspace names are interned via `unique.Make[string]` (Go 1.23+). The
`TypedKeyspace[K, V]` descriptor and internal keyspace lookup caches store
a `unique.Handle[string]` instead of a raw `string` or `[]byte`. This
avoids repeated allocations when the same keyspace is opened across many
transactions (a common pattern). The `unique.Handle` provides O(1) equality
comparison and is safe for concurrent use.

## API Surface

```go
// Sentinel errors.
var (
    ErrNotFound           = errors.New("gmdb: key not found")
    ErrKeyExists          = errors.New("gmdb: key already exists")
    ErrDBFull             = errors.New("gmdb: database full (MaxSize reached)")
    ErrTxTooLarge         = errors.New("gmdb: transaction too large") // returned when RPL segment allocation fails at commit (database near full)
    ErrReadersFull        = errors.New("gmdb: no reader slots available")
    ErrKeyTooLarge        = errors.New("gmdb: key exceeds maximum size")
    ErrKeyEmpty           = errors.New("gmdb: key is nil or empty")
    ErrCorrupted          = errors.New("gmdb: database corrupted")
    ErrVersionMismatch    = errors.New("gmdb: format version mismatch")
    ErrReadOnly           = errors.New("gmdb: write operation on read-only transaction")
    ErrTxClosed           = errors.New("gmdb: transaction already committed or rolled back")
    ErrCursorUnpositioned = errors.New("gmdb: cursor not positioned")
    ErrKeyspaceKindMismatch = errors.New("gmdb: keyspace kind does not match existing keyspace")
    ErrValueSizeMismatch    = errors.New("gmdb: value size does not match fixed value size")
)

// Open a database. Creates the file if it doesn't exist.
//
// The data file is created with O_CREATE|O_EXCL to prevent races when
// multiple processes call Open() simultaneously on a non-existent path.
// If the exclusive create fails with EEXIST, Open() retries as a normal
// open (the other process created the file). The lock file uses the
// same pattern.
func Open(path string, opts *Options) (*DB, error)
```

### Byte Slice Ownership

All `[]byte` slices returned by gmdb (from `Get`, `Cursor.Next`, `Cursor.Seek`,
etc.) are **borrowed references** — they point into either the mmap or an
internal cursor buffer. The caller does not own them.

**Value slices** point directly into the mmap (for inline values) or into
overflow pages in the mmap. They are valid until the **read transaction
closes** (`Commit()` or `Rollback()`). No copy, no allocation — true
zero-copy from the mmap.

**Key slices** may point into the mmap (for keys at restart points in
prefix-compressed leaves) or into the cursor's key reconstruction buffer
(`keyBuf`). The reconstruction buffer is reused on each cursor movement.
Key slices are valid until the **next cursor operation** (`Next()`,
`Prev()`, `Seek()`, `First()`, `Last()`, etc.) or transaction close,
whichever comes first.

Callers who need a key or value to outlive these scopes must copy it:

```go
k, v := c.Next()
savedKey := bytes.Clone(k)    // key survives next cursor movement
savedVal := bytes.Clone(v)    // value survives transaction close
```

`Keyspace.Get()` returns a value slice pointing into the mmap — valid
until transaction close. No cursor buffer is involved, so the value
remains stable for the entire transaction.

This contract is the standard for mmap-based B+tree databases (LMDB,
libmdbx, BoltDB). The zero-copy read path is a core performance
property of gmdb's architecture.

### Nil and Empty Semantics

**Keys:**

Empty keys (`[]byte{}`) and nil keys (`nil`) are both **invalid**. Any
operation that takes a key (`Get`, `Put`, `Delete`, `Seek`, etc.) returns
an error if the key is nil or empty. There is no technical reason a
zero-length key can't exist in a B+tree, but it is almost always a bug.
Rejecting it at the API boundary catches errors early.

**Values:**

Empty values (`[]byte{}`) are **valid**. A key can exist with no associated
data — this is useful for using a keyspace as a set of keys
(presence/absence). Nil values are treated as empty values: `Put(key, nil)`
stores the key with a zero-length value, equivalent to `Put(key, []byte{})`.

**Return value conventions:**

| Call | Key exists (empty value) | Key exists (non-empty value) | Key not found | End of iteration |
|------|--------------------------|------------------------------|---------------|------------------|
| `Keyspace.Get(k)` | `([]byte{}, nil)` | `(value, nil)` | `(nil, ErrNotFound)` | N/A |
| `Cursor.Next()` | `(key, []byte{})` | `(key, value)` | N/A | `(nil, nil)` |
| `Cursor.Err()` | — | — | — | returns non-nil if iteration ended due to error |

Nil return from `Get` always means "not found" (accompanied by
`ErrNotFound`). Nil return from cursor navigation always means "end of
iteration" (check `Err()` to distinguish normal end from error). Empty
`[]byte{}` return from `Get` means "key exists, value is empty."

### Database Initialization

When `Open()` creates a new database (file does not exist):

1. Create the data file with `O_CREATE|O_EXCL` to prevent races when
   multiple processes call `Open()` simultaneously. If the exclusive
   create fails with `EEXIST`, `Open()` retries as a normal open.
2. Write both meta pages identically:
   - TxnID = 0
   - HighWaterMark = 2 + BitmapPages (first data page)
   - KeyspaceRoot = 0 (empty keyspace B+tree)
   - NumKeyspaces = 0
   - RPLHeadPage = 0, RPLTailPage = 0, RPLEntryCount = 0
   - NumFreePages = 0
   - Checkpoint flag set on both meta pages
   - All file format fields from `Options.FileFormat` (or defaults)
   - UUID generated via `crypto/rand`
3. Initialize the bitmap region (pages 2 through 2 + BitmapPages - 1):
   all bits clear (no free pages — all data pages are beyond
   HighWaterMark and will be allocated via file extension).
4. Create the lock file with `O_CREATE|O_EXCL`, matching UUID, and
   empty reader table.
5. fdatasync the data file.

The first write transaction increments TxnID to 1.

### Path Traversal Safety

`Open()` uses `os.OpenRoot` (Go 1.24+) to confine all file operations
to the database directory. The path argument is split into a directory
and base name:

```go
root, err := os.OpenRoot(filepath.Dir(path))
defer root.Close()
dataFile, err := root.Open(filepath.Base(path), ...)
lockFile, err := root.Open(filepath.Base(path)+".lock", ...)
```

`os.OpenRoot` returns an `os.Root` handle that rejects symlink traversal
outside the root directory. This prevents an attacker who controls the
database path (e.g., in multi-tenant or container environments) from
redirecting file operations to arbitrary locations via symlinks. Without
this, a symlink at the database path could cause `Open()` to create or
overwrite files outside the intended directory.

The `os.Root` handle is used for all file creation and opening during
`Open()` — both the data file and the lock file. After `Open()` returns,
the resolved file descriptors are used directly and the `os.Root` is
closed.

```go
// SyncMode controls the durability guarantees of committed transactions.
type SyncMode int

const (
    // SyncDurable syncs both data and meta pages. Full ACID. Default.
    SyncDurable SyncMode = iota
    // SyncDataOnly syncs data pages but not the meta page. Last transaction
    // may be lost on crash, but the database is always consistent.
    SyncDataOnly
    // SyncLazy skips all syncs. The database rolls back to the last checkpoint
    // on crash. No corruption risk. Use DB.Checkpoint() to create
    // checkpoints.
    SyncLazy
    // SyncUnsafe skips all syncs with no safety net. Risk of corruption on
    // crash. For benchmarks and ephemeral data only. Requires
    // Options.AllowSyncUnsafe = true.
    SyncUnsafe
)

// Options for opening a database.
type Options struct {
    // PageSize in bytes. Only used when creating a new database.
    // Must be a power of 2 in range [4096, 65536]. Default: 4096.
    // Ignored when opening an existing database (read from meta page).
    PageSize int

    // PageChecksum enables CRC32C checksums on data pages (branch, leaf,
    // overflow, RPL segment). Stored as a flag in the meta page — immutable
    // after creation. When enabled, every page read is verified and every
    // page write computes a checksum footer. Default: false.
    // Only used when creating a new database. Ignored when opening an
    // existing database (read from meta page Flags).
    PageChecksum bool

    // FileFormat controls database file size bounds and growth behavior.
    // Only used when creating a new database. When opening an existing
    // database, file format is read from the meta page. Use Tx.SetFileFormat()
    // to modify file format of an existing database.
    FileFormat FileFormat

    // SyncMode controls the durability guarantees of committed
    // transactions. Default: SyncDurable.
    SyncMode SyncMode

    // AllowSyncUnsafe must be set to true when using SyncUnsafe mode.
    // This explicit opt-in prevents accidental use of the unsafe mode.
    // Open() returns an error if SyncMode is SyncUnsafe and AllowSyncUnsafe
    // is false. Default: false.
    AllowSyncUnsafe bool

    // MaxReaders is the maximum number of concurrent reader slots.
    // Default: 4096. Only used when creating a new lock file.
    // Ignored when the lock file already exists (read from lock file header).
    MaxReaders int

    // MergeThreshold is the B+tree page fill percentage below which a
    // page is merged with a sibling after a deletion. Range: 1-50.
    // Lower values waste more space but reduce merge/split churn.
    // Higher values keep pages fuller but cause more rebalancing.
    // Default: 25 (merge when page is less than 25% full).
    MergeThreshold int

    // LaggingReader is called when a long-lived reader is blocking RPL
    // reclamation during page allocation. If nil, pageAlloc() falls
    // through to file extension when reclamation is blocked.
    LaggingReader func(info LaggingReaderInfo) LaggingReaderAction

    // MaxBatchSize is the maximum number of Batch() calls to collect
    // before executing them in a single transaction. Default: 1000.
    MaxBatchSize int

    // MaxBatchDelay is the maximum time to wait for additional Batch()
    // calls before executing the current batch. Lower values reduce
    // latency; higher values increase throughput by collecting more
    // callers per transaction. Default: 10ms. Set to 0 to disable
    // delay (batch fires immediately when the coordinator runs).
    MaxBatchDelay time.Duration

    // StaleTimeout is the duration after which a reader slot or writer
    // with a stale heartbeat is considered abandoned. Only used for
    // cross-PID-namespace stale detection (same-namespace detection uses
    // instant PID liveness checks). Must be significantly larger than the
    // heartbeat interval (1s). Default: 10s.
    StaleTimeout time.Duration

    // Logger for diagnostic messages (leaked transactions, stale reader
    // recovery, stale writer recovery). If nil, diagnostics are discarded.
    // Default: nil.
    Logger *slog.Logger

    // FileMode for newly created files. Default: 0644.
    FileMode os.FileMode

    // PreloadPages pre-faults the mmap into the page cache at open time
    // via madvise(MADV_POPULATE_READ) (Linux 5.14+). Eliminates page
    // faults on first access at the cost of slower Open(). Ignored if
    // the kernel does not support MADV_POPULATE_READ. Default: false.
    PreloadPages bool

    // HugePages enables transparent huge page support via
    // madvise(MADV_HUGEPAGE) on the data file mmap (Linux only).
    // Reduces TLB pressure for large databases by allowing the kernel
    // to back the mmap with 2MB huge pages where possible. A 1GB
    // database drops from 262,144 TLB entries (4KB pages) to 512
    // (2MB huge pages). Ignored on non-Linux platforms. Default: false.
    HugePages bool

    // ReclaimOnClose calls madvise(MADV_COLD) on the mmap region accessed
    // by a read transaction when the transaction closes (Linux 5.4+).
    // This hints the kernel to reclaim page cache for pages that are
    // no longer hot, reducing memory pressure after large scans.
    // Useful for batch readers that scan the entire database. Ignored
    // on non-Linux platforms or older kernels. Default: false.
    ReclaimOnClose bool

    // ReadOnly opens the database in read-only mode. The data file is
    // mapped with PROT_READ only (no PROT_WRITE). Write transactions
    // return ErrReadOnly. The lock file is still writable — reader slot
    // acquisition requires atomic CAS operations on the shared mmap.
    // ReadOnly is suitable for deployments where the data file is on
    // read-only media or has read-only filesystem permissions, provided
    // the lock file is on writable storage.
    ReadOnly bool

    // Maintenance controls the background maintenance goroutine.
    // See Background Maintenance for details. If nil, default options
    // are used (maintenance enabled, 5m interval).
    Maintenance *MaintenanceOptions
}

// FileFormat controls the database file size bounds and growth/shrink behavior.
// All sizes are specified in bytes and must be multiples of PageSize. They are
// converted to pages internally and stored in the meta page as page counts.
// All fields are uint64 — negative sizes are meaningless for file format,
// and uint64 matches the internal meta page representation.
type FileFormat struct {
    // Lower is the minimum database file size in bytes. The file never
    // shrinks below this. Must be a multiple of PageSize.
    // Default: (2 + BitmapPages) * PageSize (meta + bitmap pages).
    Lower uint64

    // Upper is the maximum database file size in bytes. Determines mmap
    // reservation size and allocation bitmap size. Must be a multiple of
    // PageSize. Immutable after creation — SetFileFormat cannot change this;
    // use CopyTo to create a new database with different Upper.
    // Default: 256GB.
    Upper uint64

    // GrowStep is the number of bytes to grow by when extending the file.
    // Must be a multiple of PageSize. Default: 256MB.
    GrowStep uint64

    // ShrinkThreshold is the minimum number of bytes of unused space at
    // the end of the file before shrinking occurs. Must be a multiple of
    // PageSize. Default: 512MB.
    ShrinkThreshold uint64
}

// LaggingReaderInfo describes a reader that is blocking RPL reclamation.
type LaggingReaderInfo struct {
    PID       int    // process ID of the lagging reader
    TxnID     uint64 // transaction ID the reader is holding
    Lag       uint64 // number of transactions behind current
    HeldPages uint64 // estimated number of pages held unreclaimable
}

// LaggingReaderAction determines how pageAlloc responds to a lagging reader.
type LaggingReaderAction int

const (
    LaggingReaderWait  LaggingReaderAction = iota // retry, reader may release
    LaggingReaderAbort                            // abort with ErrDBFull
)

// DB is a handle to an open database.
type DB struct { ... }

func (db *DB) Close() error

// Checkpoint flushes all outstanding writes to stable storage. In SyncLazy mode,
// this creates a checkpoint — the database will roll back to this
// point (at most) on crash. In SyncDurable and SyncDataOnly modes, this is a
// no-op (commits already sync). In SyncUnsafe mode, this syncs but does not
// retroactively fix the lack of ordering guarantees from prior commits.
func (db *DB) Checkpoint() error

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
// the caller's closure executes, Batch returns context.Cause(ctx). Once
// the closure begins executing, the context is not checked.
//
// Each closure runs in its own child transaction (see Nested Transactions).
// If fn returns an error, only its child transaction is rolled back —
// other closures in the batch are unaffected. fn executes exactly once
// and may safely perform external side effects (logging, metrics, etc.).
// See Write Batching for details.
//
// Batch is a throughput optimization for workloads with many concurrent
// small writes. For exclusive write access or large transactions, use
// Update or Begin directly.
func (db *DB) Batch(ctx context.Context, fn func(tx *Tx) error) error

// Begin starts a transaction manually. The context governs lock/slot
// acquisition:
//   - For write transactions: blocks on the write lock, respecting
//     context cancellation. Returns context.Cause(ctx) if cancelled
//     while waiting.
//   - For read transactions: if no reader slots are available and the
//     context has a deadline, retries with backoff until a slot becomes
//     free or the context expires. If the context has no deadline,
//     returns ErrReadersFull immediately (no blocking). Use
//     context.WithTimeout to control the wait window.
//
// Once Begin returns a *Tx, the context is not stored — the caller
// controls the transaction lifetime via Commit()/Rollback().
func (db *DB) Begin(ctx context.Context, writable bool) (*Tx, error)

// Tx is a database transaction.
type Tx struct { ... }

func (tx *Tx) Commit() error
func (tx *Tx) Rollback() error

// BeginChild creates a child transaction within the current write
// transaction. The child can be independently committed (merged into
// the parent) or rolled back (discarded) without affecting the parent.
// Only valid on a write transaction. Children can be nested arbitrarily.
// The child receives the same *Tx type — all Keyspace/SetKeyspace
// operations work identically.
func (tx *Tx) BeginChild() (*Tx, error)

// SetFileFormat updates the file format. Only valid on a write
// transaction. The new file format takes effect when the transaction commits.
//
// MaxSize (FileFormat.MaxSize) is immutable after creation — the allocation
// bitmap size is fixed at creation time based on MaxSize, and changing it
// would invalidate all page IDs. SetFileFormat returns an error if
// FileFormat.MaxSize differs from the current MaxSize. To change MaxSize,
// use CopyTo to create a new database with different file format.
//
// MinSize, GrowStep, and ShrinkThreshold may be modified freely.
func (tx *Tx) SetFileFormat(g FileFormat) error

// OpenKeyspace opens an existing named keyspace (single-value) within this
// transaction. Returns ErrNotFound if the keyspace does not exist. Returns
// ErrKeyspaceKindMismatch if the keyspace is a SetKeyspace.
func (tx *Tx) OpenKeyspace(name []byte) (*Keyspace, error)

// CreateKeyspace creates a new named single-value keyspace within this
// transaction. Returns ErrKeyExists if the keyspace already exists.
func (tx *Tx) CreateKeyspace(name []byte) (*Keyspace, error)

// CreateKeyspaceIfNotExists opens a single-value keyspace if it exists,
// or creates it if it does not. If the keyspace already exists as a
// SetKeyspace, returns ErrKeyspaceKindMismatch.
func (tx *Tx) CreateKeyspaceIfNotExists(name []byte) (*Keyspace, error)

// OpenSetKeyspace opens an existing named set keyspace (multiple sorted
// values per key) within this transaction. Returns ErrNotFound if the
// keyspace does not exist. Returns ErrKeyspaceKindMismatch if the
// keyspace is a single-value Keyspace.
func (tx *Tx) OpenSetKeyspace(name []byte) (*SetKeyspace, error)

// SetKeyspaceOptions controls set keyspace behavior. All fields are set
// at creation time and immutable after.
type SetKeyspaceOptions struct {
    // FixedValueSize, when non-zero, requires all values in the set to
    // be exactly this many bytes. Enables storage optimizations: no
    // per-value length prefix in subpages, direct offset binary search.
    // A Put() with a value of the wrong size returns ErrValueSizeMismatch.
    FixedValueSize int
}

// CreateSetKeyspace creates a new named set keyspace within this
// transaction. Returns ErrKeyExists if the keyspace already exists.
// If opts is nil, default options are used.
func (tx *Tx) CreateSetKeyspace(name []byte, opts *SetKeyspaceOptions) (*SetKeyspace, error)

// CreateSetKeyspaceIfNotExists opens a set keyspace if it exists, or
// creates it if it does not. If the keyspace already exists as a
// single-value Keyspace, returns ErrKeyspaceKindMismatch.
func (tx *Tx) CreateSetKeyspaceIfNotExists(name []byte, opts *SetKeyspaceOptions) (*SetKeyspace, error)

// DeleteKeyspace removes a keyspace and all of its data. For set
// keyspaces, all nested B+trees are retired via bulk subtree
// retirement. Returns ErrNotFound if the keyspace does not exist.
// Works for both Keyspace and SetKeyspace types — the Kind field
// is not checked. Only valid on a write transaction.
func (tx *Tx) DeleteKeyspace(name []byte) error

// Keyspace is a handle to a named single-value keyspace within a transaction.
type Keyspace struct { ... }

// Get returns the value for the given key. Returns ErrNotFound if the key
// does not exist.
func (ks *Keyspace) Get(key []byte) ([]byte, error)

// Put inserts or updates a key-value pair. An existing value is replaced.
func (ks *Keyspace) Put(key, value []byte) error

// Delete removes the key and its value. If the key does not exist,
// Delete is a no-op and returns nil — Delete means "ensure this key
// does not exist."
func (ks *Keyspace) Delete(key []byte) error

// DeleteRange deletes all keys in the range [start, end). Returns the
// number of deleted key-value pairs. If start is nil, deletes from the
// first key. If end is nil, deletes through the last key. If both are
// nil, deletes all keys.
//
// DeleteRange retires entire B+tree subtrees that fall within the range
// without visiting individual leaf entries — O(pages) not O(entries).
// See Range Delete for the algorithm.
func (ks *Keyspace) DeleteRange(start, end []byte) (uint64, error)

// NextSequence returns the next auto-incrementing sequence number for
// this keyspace. The counter starts at 0; the first call returns 1.
// The counter is persisted in the keyspace descriptor and updated when
// the transaction commits. Only valid on a write transaction.
func (ks *Keyspace) NextSequence() (uint64, error)

// Cursor for iterating over key-value pairs.
func (ks *Keyspace) Cursor() *Cursor

type Cursor struct { ... }

func (c *Cursor) First() (key, value []byte)
func (c *Cursor) Last() (key, value []byte)
func (c *Cursor) Next() (key, value []byte)
func (c *Cursor) Prev() (key, value []byte)

// Seek positions the cursor at the exact key. Returns the key-value pair,
// or nil if the key does not exist.
func (c *Cursor) Seek(target []byte) (key, value []byte)

// SeekGE positions the cursor at the first key >= target.
// Returns the key-value pair, or nil if no such key exists.
func (c *Cursor) SeekGE(target []byte) (key, value []byte)

// Current returns the key-value pair at the current cursor position
// without moving the cursor.
func (c *Cursor) Current() (key, value []byte)

// Delete removes the current key-value pair. After deletion, the
// cursor is positioned at the next key (the key that followed the
// deleted key). If the deleted key was the last key, the cursor
// becomes unpositioned (Next() returns nil). Only valid on a write
// transaction cursor.
func (c *Cursor) Delete() error

// Err returns the first error encountered during cursor navigation.
// Navigation methods (First, Last, Next, Prev, Seek, SeekGE) do not
// return errors directly — they return nil key/value when iteration
// ends or an error occurs. After a navigation loop, the caller checks
// Err() to distinguish normal end-of-range (nil) from an error (e.g.,
// ErrCorrupted from a page checksum failure). This follows the
// bufio.Scanner / sql.Rows pattern.
func (c *Cursor) Err() error

// SetKeyspace is a handle to a named set keyspace (multiple sorted values
// per key) within a transaction.
type SetKeyspace struct { ... }

// Has reports whether the key exists (has at least one value).
func (ks *SetKeyspace) Has(key []byte) (bool, error)

// HasValue reports whether a specific key-value pair exists.
func (ks *SetKeyspace) HasValue(key, value []byte) (bool, error)

// Put adds a value to the key's sorted value set (no-op if the exact
// key-value pair already exists).
func (ks *SetKeyspace) Put(key, value []byte) error

// Delete removes the key and all of its values. If the key does not
// exist, Delete is a no-op and returns nil. Uses bulk subtree
// retirement for nested B+trees. To remove a single value, use
// DeleteValue.
func (ks *SetKeyspace) Delete(key []byte) error

// DeleteValue removes a single value from the key's sorted set.
// Returns ErrNotFound if the key or value does not exist. When the last
// value is removed, the key is also removed — empty sets never exist.
func (ks *SetKeyspace) DeleteValue(key, value []byte) error

// CountValues returns the number of values for the given key.
// Returns 0 if the key does not exist.
func (ks *SetKeyspace) CountValues(key []byte) (uint64, error)

// DeleteRange deletes all keys in the range [start, end). Returns the
// number of deleted key-value pairs (each value counts as one). If start
// is nil, deletes from the first key. If end is nil, deletes through
// the last key. If both are nil, deletes all keys.
//
// DeleteRange retires entire B+tree subtrees that fall within the range
// without visiting individual leaf entries — O(pages) not O(entries).
// See Range Delete for the algorithm.
func (ks *SetKeyspace) DeleteRange(start, end []byte) (uint64, error)

// NextSequence returns the next auto-incrementing sequence number for
// this keyspace. See Keyspace.NextSequence for details.
func (ks *SetKeyspace) NextSequence() (uint64, error)

// Cursor for iterating over key-value pairs in a set keyspace.
func (ks *SetKeyspace) Cursor() *SetCursor

type SetCursor struct { ... }

// --- Core navigation ---

func (c *SetCursor) First() (key, value []byte)
func (c *SetCursor) Last() (key, value []byte)
func (c *SetCursor) Next() (key, value []byte)
func (c *SetCursor) Prev() (key, value []byte)

// Seek positions the cursor at the exact key. Returns the key and the
// first (smallest) value for the key, or nil if the key does not exist.
func (c *SetCursor) Seek(target []byte) (key, value []byte)

// SeekGE positions the cursor at the first key >= target.
// Returns the key and the first value for that key, or nil if no such key exists.
func (c *SetCursor) SeekGE(target []byte) (key, value []byte)

// Current returns the key-value pair at the current cursor position
// without moving the cursor.
func (c *SetCursor) Current() (key, value []byte)

// Delete removes the current value from the current key's set. After
// deletion, the cursor is positioned at the next value for the
// current key. If the deleted value was the last value for the key,
// the cursor advances to the first value of the next key. If there
// is no next key, the cursor becomes unpositioned. When the last
// value for a key is deleted, the key itself is removed.
func (c *SetCursor) Delete() error

// Err returns the first error encountered during cursor navigation.
func (c *SetCursor) Err() error

// --- Set cursor operations (value navigation within a key) ---

// FirstValue positions the cursor at the first value for the
// current key. Returns the value, or nil if the cursor is not positioned.
func (c *SetCursor) FirstValue() (value []byte)

// LastValue positions the cursor at the last value for the
// current key.
func (c *SetCursor) LastValue() (value []byte)

// NextValue moves to the next value for the current key.
// Returns nil when there are no more values (the cursor does NOT
// advance to the next key). The key does not change during value
// navigation — use Current() to read the key.
func (c *SetCursor) NextValue() (value []byte)

// PrevValue moves to the previous value for the current key.
// Returns nil when at the first value.
func (c *SetCursor) PrevValue() (value []byte)

// NextKey moves to the first value of the next key, skipping
// remaining values of the current key.
func (c *SetCursor) NextKey() (key, value []byte)

// PrevKey moves to the last value of the previous key,
// skipping remaining values of the current key.
func (c *SetCursor) PrevKey() (key, value []byte)

// SeekValue positions the cursor at the first value >= target
// for the current key. The cursor must already be positioned on a key
// (via Seek, SeekGE, First, etc.). Returns the value, or nil if no
// value >= target exists for the current key.
func (c *SetCursor) SeekValue(target []byte) (value []byte)

// CountValues returns the number of values for the current key.
func (c *SetCursor) CountValues() (uint64, error)

// --- Range iterators (read-only, for use with for-range) ---

// All returns an iterator over all key-value pairs in the keyspace.
// The iterator yields pairs in key order.
func (ks *Keyspace) All() iter.Seq2[[]byte, []byte]

// Range returns an iterator over key-value pairs in [start, end).
// If start is nil, iteration begins at the first key. If end is nil,
// iteration continues through the last key.
func (ks *Keyspace) Range(start, end []byte) iter.Seq2[[]byte, []byte]

// Prefix returns an iterator over all key-value pairs whose keys
// share the given prefix.
func (ks *Keyspace) Prefix(prefix []byte) iter.Seq2[[]byte, []byte]

// --- Statistics ---

// DBStats contains environment-level statistics.
type DBStats struct {
    // Free space
    FreePages    uint64 // total free pages (set bits in allocation bitmap)
    RetiredPages uint64 // pages in RPL, not yet reclaimable (held by readers)

    // File format
    FileSize     uint64 // current data file size in bytes
    MinSize     uint64 // minimum file size in bytes
    MaxSize     uint64 // maximum file size in bytes
    HighWaterMark uint64 // first unallocated page ID (high-water mark)

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
    CheckFatal                        // walk could not continue past this point
)

// CheckIssue describes a single integrity problem found during a database check.
// All results — including walk failures — are represented as issues. A
// CheckFatal issue means the walk stopped at that point and nothing beyond
// it was checked.
type CheckIssue struct {
    Severity CheckSeverity
    PageID   uint64 // page where the issue was found (0 if N/A)
    Keyspace string // keyspace name (empty if global/bitmap/RPL)
    Message  string // human-readable description
    Repaired bool   // true if the issue was fixed (only when CheckOptions.Repair is true)
}

// Check performs a full structural integrity walk of the database. It opens
// a read transaction, walks all B+trees (keyspace trees), the allocation
// bitmap, and the RPL, and verifies:
//   - Meta page checksum validity
//   - B+tree structural integrity (page reachability, no cycles, key ordering)
//   - Bitmap consistency (no overlap between free and in-use pages)
//   - RPL consistency (valid segment chain, no duplicate entries)
//   - Page accounting (all pages accounted for: data + bitmap + RPL + free + unallocated)
//   - Leaked pages (bitmap bit clear but page not reachable from any structure —
//     reported as CheckWarning, not CheckError, since tree integrity is unaffected;
//     recoverable via CopyTo(compact=true))
//   - Leaf page prefix compression integrity (restart table offsets within page
//     bounds, restart entries at correct positions, delta chain consistency within
//     each restart group, reconstructed keys in sorted order)
//   - Keyspace descriptor consistency (root page validity, counts)
//   - Set keyspace subpage and nested B+tree integrity
//
// Check returns an iter.Seq[CheckIssue] that yields issues as they are
// found during the walk. All results — including walk failures (I/O
// errors, unreadable pages) — are represented as CheckIssue values.
// Walk failures are reported as CheckFatal severity and are always the
// last issue yielded.
//
// The caller can break early, collect all issues via
// slices.Collect(db.Check()), or stream issues for immediate display.
//
//   // Health check
//   issues := slices.Collect(db.Check())
//
//   // Streaming display
//   for issue := range db.Check() {
//       fmt.Println(issue.Severity, issue.PageID, issue.Message)
//   }
//
//   // Testing
//   require.Empty(t, slices.Collect(db.Check()))
func (db *DB) Check() iter.Seq[CheckIssue]

// CheckOptions controls Check behavior.
type CheckOptions struct {
    // Repair enables offline repair of detected issues. When true, the
    // database must be opened with exclusive access (no concurrent
    // readers or writers). Check walks the full tree to build the set
    // of referenced pages, then corrects the allocation bitmap for any
    // leaked pages (allocated but unreferenced). Leaked pages are set
    // free in the bitmap; the corrected bitmap pages are written via
    // pwrite + fdatasync.
    //
    // Repair only fixes bitmap leakage — pages that appear allocated
    // but are not reachable from any tree structure. It does not fix
    // structural B+tree corruption (CheckError/CheckFatal issues).
    // For structural corruption, use CopyTo(compact=true) to rebuild
    // from the intact portion of the tree.
    //
    // Repaired issues are yielded as CheckIssue with the original
    // severity and an additional Repaired bool field set to true.
    Repair bool
}

// CheckWithOptions performs a full structural integrity walk with
// optional repair. See Check for the list of verified invariants.
// When opts is nil, behaves identically to Check.
func (db *DB) CheckWithOptions(opts *CheckOptions) iter.Seq[CheckIssue]

// CopyTo creates a consistent copy of the database at the given path.
// The copy is taken from a read transaction snapshot — writers are not
// blocked. When compact is true, the copy is compacted: free pages are
// omitted and B+tree pages are written sequentially for optimal read
// performance. The new database inherits the source's PageSize,
// BitmapPages, and MaxSize. To create a copy with different FileFormat
// settings (e.g., a different MaxSize), use CopyTo to create the copy
// and then re-open it with the desired settings via SetFileFormat().
func (db *DB) CopyTo(path string, compact bool) error

// Compact rebuilds the database file in place. It creates a compacted
// copy via CopyTo(compact=true) to a temporary file in the same
// directory, then atomically renames it over the original. The
// temporary file is cleaned up on error.
//
// Compact runs the copy in a read transaction — writers are not blocked
// during the copy phase. The atomic rename requires briefly closing and
// reopening the database. Callers must ensure no transactions are open
// when Compact returns (the DB handle is still valid — the underlying
// file has changed).
//
// Effects:
//   - Reclaims leaked pages (bitmap allocated but unreferenced).
//   - Defragments the file — B+tree pages are written sequentially,
//     restoring contiguous free runs for overflow page allocation.
//   - Shrinks the file to its minimum size.
//
// Compact requires enough free disk space for the temporary copy
// (up to the size of the live data). For databases where temporary
// disk space is not available, an incremental in-place compaction
// (relocating pages in batches across multiple write transactions)
// is a possible future addition.
func (db *DB) Compact() error

// TxStats contains per-transaction statistics. Accumulated during the
// transaction's lifetime and returned as a snapshot.
type TxStats struct {
    // Page management
    CowPages       uint64 // pages copied via CoW
    LoosePages     uint64 // pages CoW'd then freed in this txn
    ReclaimedPages uint64 // pages reclaimed from RPL into bitmap
    WrittenPages   uint64 // bitmap + meta pages written at commit time

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
    Entries       uint64 // total key-value pairs (for set keyspaces, each value counts)
}

func (ks *Keyspace) Stats() (KeyspaceStats, error)
```

### Typed Keyspaces (Generics)

A higher-level API layer built on top of the byte-oriented `Keyspace` API.
`TypedKeyspace[K, V]` provides type-safe access to a keyspace by handling
key/value serialization automatically via the `Encoder[T]` interface:

```go
// Encoder handles serialization between a Go type and byte slices.
// Implementations may be stateful (e.g., buffer pooling).
//
// AppendEncode appends the encoded form of v to dst and returns the
// extended buffer. Callers pass dst[:0] from a sync.Pool to reuse
// allocations on the hot path. Returning an error allows encoders to
// reject values that cannot be represented (e.g., keys exceeding the
// maximum size).
//
// Decode deserializes src into a value of type T. Returning an error
// allows encoders to surface malformed or truncated data rather than
// panicking or silently producing corrupt values.
type Encoder[T any] interface {
    AppendEncode(dst []byte, v T) ([]byte, error)
    Decode(src []byte) (T, error)
}

// FuncEncoder adapts plain functions into the Encoder interface for
// simple cases where no state is needed.
type FuncEncoder[T any] struct {
    EncodeFunc func(dst []byte, v T) ([]byte, error)
    DecodeFunc func(src []byte) (T, error)
}

func (f FuncEncoder[T]) AppendEncode(dst []byte, v T) ([]byte, error) { return f.EncodeFunc(dst, v) }
func (f FuncEncoder[T]) Decode(src []byte) (T, error)                 { return f.DecodeFunc(src) }

// TypedKeyspace wraps a single-value Keyspace with type-safe key/value encoding.
type TypedKeyspace[K, V any] struct {
    name   []byte
    keyEnc Encoder[K]
    valEnc Encoder[V]
}

// NewTypedKeyspace creates a typed single-value keyspace descriptor. The key
// encoder MUST produce lexicographically ordered output for the desired key
// ordering — the underlying B+tree sorts keys as raw bytes.
func NewTypedKeyspace[K, V any](
    name string,
    keyEnc Encoder[K],
    valEnc Encoder[V],
) *TypedKeyspace[K, V]

// Open opens an existing typed keyspace within a transaction.
func (tks *TypedKeyspace[K, V]) Open(tx *Tx) (*TypedKS[K, V], error)

// Create creates a new typed keyspace within a transaction.
func (tks *TypedKeyspace[K, V]) Create(tx *Tx) (*TypedKS[K, V], error)

// CreateIfNotExists opens or creates the typed keyspace.
func (tks *TypedKeyspace[K, V]) CreateIfNotExists(tx *Tx) (*TypedKS[K, V], error)

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

type TypedCursor[K, V any] struct { ... }

func (c *TypedCursor[K, V]) First() (K, V, bool)
func (c *TypedCursor[K, V]) Last() (K, V, bool)
func (c *TypedCursor[K, V]) Next() (K, V, bool)
func (c *TypedCursor[K, V]) Prev() (K, V, bool)
func (c *TypedCursor[K, V]) Seek(target K) (K, V, bool)
func (c *TypedCursor[K, V]) SeekGE(target K) (K, V, bool)
func (c *TypedCursor[K, V]) Err() error
```

The typed layer is a **zero-cost abstraction** at the API level — all
methods delegate to the underlying `Keyspace` and `Cursor` methods with
`Encoder` calls. The `AppendEncode` signature follows the standard Go
append pattern (`strconv.AppendInt`, `time.AppendFormat`, etc.), allowing
callers to pass a reusable `[]byte` buffer (e.g., from `sync.Pool`) to
eliminate per-call allocations on the hot path. Returning `error` from both
`AppendEncode` and `Decode` ensures malformed data is surfaced cleanly
rather than panicking or silently producing corrupt values. Using an
interface instead of closures allows stateful encoders (e.g., with buffer
pooling via `sync.Pool`) and is more idiomatic Go — encoders can be
implemented as method sets on types. The `FuncEncoder` adapter is provided
for simple stateless cases.

**Key ordering constraint**: The key encoder must produce byte sequences
whose lexicographic order matches the desired key order. For `uint64` keys,
this means big-endian encoding. For `string` keys, the natural byte
representation already sorts lexicographically. The typed API does not
support custom comparators — the underlying B+tree always uses byte ordering.

The typed API is a convenience layer. Callers who need full control over
serialization or need to avoid allocation overhead from encoder calls use
the byte-oriented `Keyspace` API directly.

## Implementation Layout

All code lives in a single `gmdb` package (flat, no sub-packages). This avoids
circular dependency issues between tightly coupled components (pages, B+tree,
transactions, mmap) and keeps the public API to one import path. The code is
organized by file:

| File | Responsibility |
|------|---------------|
| `page.go` | Page header encoding/decoding (8-byte header: Type uint8, Flags uint8, Count uint16, AdditionalPages uint32 — no PageID). Optional CRC32C footer: compute on write, verify on read (when PageChecksum enabled). Branch page: cell directory, key lookup (binary search), insert/split. Leaf page: prefix-compressed format with restart table (interval 16), restart/delta entry encode/decode, restart-point binary search + linear group scan for lookup, delta recomputation on insert/delete, restart table rebuild, full-page re-encoding on split. Overflow references in both restart and delta entry formats. Set keyspace subpage format (uncompressed). Meta page: encode/decode/validate xxhash64 checksum (including file format fields, bitmap/RPL pointers, Flags). RPL segment page: per-segment TxnID + PageID array, encode/decode. |
| `btree.go` | B+tree search, insert (CoW path from leaf to root, split with prefix-truncated separator computation), delete (CoW, merge/rebalance with configurable `MergeThreshold`, separator recomputation). Range delete: boundary path finding, interior subtree retirement, boundary leaf cleanup, rebalance. Set keyspace bulk free: recursive subtree retirement for nested B+trees. Cursor: stateful iterator holding a stack of (pageID, index) pairs, key reconstruction buffer (`keyBuf []byte`) for incremental forward decoding, restart group cache (`[16][]byte`) for reverse traversal. Set keyspace: subpage management (inline sorted list), nested B+tree promotion/demotion, set cursor operations. All operations work on page byte slices (from mmap), never Go heap objects. |
| `iter.go` | `iter.Seq2`-based read-only iterators: `All()`, `Range()`, `Prefix()` for both byte-oriented `Keyspace` and generic `TypedKS[K, V]`. Built on top of cursor operations. |
| `alloc.go` | Allocation bitmap: two-level (detail + in-memory summary) bitmap at fixed page offsets, bit set/clear, contiguous-run search with `math/bits` intrinsics, LIFO hint tracking. Pending bitmap changes: `tx.pendingAllocs` and `tx.pendingFrees` track deferred bit changes; `pageAlloc()` checks pending sets before scanning the mmap bitmap. Retired page log (RPL): append-only singly-linked list of immutable segment pages (per-segment TxnID + PageID arrays), in-memory segment list (`[]uint64` tail-to-head) rebuilt at open for forward traversal, whole-segment reclamation (walk from tail, move entries to bitmap, free empty segments). Loose page tracking: hash map (`map[uint64]struct{}`) of intra-transaction recycled page IDs for O(1) tail refund lookups. Page allocation priority: loose pages → bitmap → RPL reclamation → lagging reader check → file extension. Tail page refund for auto-compaction. Commit-time update: apply pending bitmap changes via pwrite, append retired pages to RPL via new segment pages (bounded, non-recursive). |
| `fileformat.go` | File format management: grow/shrink bounds, growth step, shrink threshold. File growth via `ftruncate()`. File shrinkage at commit time after tail refund. `Tx.SetFileFormat()` for runtime modification of `MinSize`, `GrowStep`, `ShrinkThreshold` (rejects `MaxSize` changes — bitmap region is fixed at creation). |
| `mmap.go` | Platform-agnostic mmap interface. Initial mapping with over-allocated virtual address space (sized to MaxSize). Read-write by default (`MAP_SHARED \| PROT_READ \| PROT_WRITE`); read-only when `Options.ReadOnly` is set (`MAP_SHARED \| PROT_READ`). |
| `mmap_linux.go` | Linux mmap/munmap/msync syscalls. `MADV_POPULATE_READ` page preloading (Linux 5.14+, opt-in). `MADV_HUGEPAGE` for transparent huge pages (opt-in). `MADV_COLD` for read transaction cooldown (Linux 5.4+, opt-in). |
| `mmap_darwin.go` | macOS mmap/munmap/msync syscalls. Commit-time `msync(MS_SYNC)` before bitmap pwrite (required because macOS `fdatasync` does not flush mmap dirty pages). |
| `lock.go` | Lock file creation and mmap (shared memory, `structs.HostLayout` structs, uint64 PIDs + process start times + PID namespace inodes + heartbeats). Writer lock (single flock goroutine with intra-process writer queue + flock cross-process + WriterPID/WriterStartTime/WriterPIDNamespace/WriterHeartbeat, context-aware, zero goroutine accumulation). Stale writer recovery (namespace-aware: same-namespace uses PID liveness + start time; cross-namespace uses heartbeat timeout). Reader table: hint-based scan+CAS slot acquire (stores PID + ProcessStartTime + PIDNamespace + Heartbeat), atomic store release, namespace-aware stale reader detection. Heartbeat goroutine (started at Open, stopped at Close, updates active slots every ~1s). Oldest-reader query for RPL reclamation. Lagging reader detection and callback invocation. |
| `process_linux.go` | `processStartTime(pid) (uint64, error)`: reads `/proc/[pid]/stat` field 22 (`starttime`, clock ticks since boot). `pidNamespace() uint64`: reads `/proc/self/ns/pid` inode via `os.Readlink`. Pure Go, no cgo. |
| `process_darwin.go` | `processStartTime(pid) (uint64, error)`: `sysctl` with `KERN_PROC_PID` → `kinfo_proc.kp_proc.p_starttime`. Packed as `sec*1_000_000 + usec`. Pure Go via `syscall.Sysctl`. |
| `process_freebsd.go` | `processStartTime(pid) (uint64, error)`: `sysctl` with `KERN_PROC_PID` → `kinfo_proc.ki_start`. Packed as `sec*1_000_000 + usec`. Pure Go via `syscall.Sysctl`. |
| `tx.go` | Read transaction: snapshot meta, acquire reader slot, read-only B+tree access, optional MADV_COLD on close. Write transaction: snapshot meta, acquire write lock, CoW page map (`tx.cowPages map[uint64]struct{}`), pending bitmap changes (`tx.pendingAllocs`, `tx.pendingFrees`), CoW operations (allocate fresh page, copy from mmap position A to B, modify B in place), page lookup (`mmap[pageID * pageSize]` — single level), commit (apply pending bitmap changes via pwrite + fdatasync + meta pwrite + fdatasync + RPL append + file format shrink), rollback (discard pending maps). Nested transactions: `BeginChild()` snapshots pending maps and keyspace roots; child commit discards snapshot (no-op merge); child rollback restores from snapshot. Leak detection: `runtime.AddCleanup` at Begin, `cleanup.Stop()` at Commit/Rollback. Stats accumulation. |
| `db.go` | Open/Close (path traversal safety via `os.OpenRoot`). Environment setup (mmap with read-write mapping, lock file, file format, AllowSyncUnsafe validation). DB handle leak detection via `runtime.AddCleanup`. Transaction lifecycle (Begin/Commit/Rollback, View/Update helpers). Write batching: Batch() channel, coordinator goroutine, per-closure child transactions. Keyspace management (OpenKeyspace, CreateKeyspace, CreateKeyspaceIfNotExists, DeleteKeyspace). Keyspace name interning via `unique.Handle[string]`. Checkpoint(). Check(). CheckWithOptions(). CopyTo(). Compact(). Background maintenance goroutine (bitmap leak reclamation, stale reader cleanup, checksum scrubbing, incremental compaction; coordinated across processes via LastMaintenanceTime in lock file). |
| `typed.go` | `Encoder[T]` interface, `FuncEncoder[T]` adapter. `TypedKeyspace[K, V]` and `TypedKS[K, V]` generic wrappers with `iter.Seq2` iterators. `TypedCursor[K, V]`. Delegates all operations to byte-oriented `Keyspace`/`Cursor` with `Encoder` calls. |
| `errors.go` | Sentinel error definitions. |
| `stats.go` | DBStats, TxStats, KeyspaceStats types and collection. |

### Coding Conventions

**Default values via `cmp.Or`** (Go 1.22+): Options fields with zero-value
defaults use `cmp.Or` for concise initialization:

```go
pageSize := cmp.Or(opts.PageSize, 4096)
maxReaders := cmp.Or(opts.MaxReaders, 4096)
maxBatchSize := cmp.Or(opts.MaxBatchSize, 1000)
```

`cmp.Or` returns the first non-zero argument. This replaces verbose
`if field == 0 { field = default }` blocks throughout `Open()` and
transaction setup, reducing boilerplate and making the defaults
scannable at a glance.

**Concurrency tests via `testing/synctest`** (Go 1.24+): All
concurrency-critical code paths use `synctest.Run` for deterministic
testing. `synctest.Run` controls goroutine scheduling in tests,
eliminating flaky timing-dependent assertions. Key areas:

- **Batch coordinator**: verifying `MaxBatchDelay` timeout fires at the
  correct time, batch collection fills to `MaxBatchSize`, and
  per-closure child transactions commit/rollback correctly — without
  `time.Sleep` or racy channel coordination.
- **Flock goroutine**: verifying context cancellation while the flock is
  pending correctly dequeues the writer, and that the flock goroutine
  releases the lock on behalf of a cancelled waiter.
- **Reader table**: verifying concurrent slot acquisition via CAS under
  contention, and stale reader detection clearing the correct slots.

## Limits

### Page Size

Configurable at database creation time. Must be a power of 2 in the range
4096–65536 (4KB–64KB). Stored in the meta page and immutable after creation.
Default: 4096 bytes.

### Maximum Key Size

Determined by page size. A branch page must fit at least 2 keys to allow
splitting. The fixed overhead is 16 bytes (8-byte page header + 8-byte
leftmost child pointer). Each key requires 4 bytes (cell directory entry) +
key bytes + 8 bytes (child pointer). The maximum key size is approximately
`(PageSize - 40) / 2` (or `(PageSize - 44) / 2` with PageChecksum enabled):

| Page Size | Max Key Size (approx) | With PageChecksum |
|-----------|----------------------|-------------------|
| 4KB       | ~2028 bytes          | ~2026 bytes       |
| 8KB       | ~4076 bytes          | ~4074 bytes       |
| 16KB      | ~8172 bytes          | ~8170 bytes       |
| 64KB      | ~32748 bytes         | ~32746 bytes      |

Enforced at `Put()` time. Keys exceeding the limit return an error.

Note: this limit is determined by branch pages, not leaf pages. Leaf pages
with prefix compression can store keys up to this size at restart points
(full keys). Delta entries store only the unshared suffix, so their on-disk
size is smaller, but the reconstructed full key must still be within the
branch page limit to allow splitting at any level.

### Maximum Value Size

For single-value keyspaces: inline values are limited by available space in the
leaf page. Values that exceed this are automatically stored as overflow pages.
There is no practical upper limit on value size (bounded only by disk space and
`MaxSize`). Note that leaf prefix compression reduces per-entry key overhead,
leaving more page space for inline values — a leaf with high prefix sharing
can fit larger inline values before triggering overflow.

### Maximum Value Size (Set Keyspaces)

For set keyspaces, each value becomes a key in the nested B+tree
(or an entry in a subpage). The maximum value size is therefore the
same as the maximum key size — approximately `(PageSize - 40) / 2`. Overflow
pages are not used for set keyspace values. A `Put()` call with a value
exceeding this limit returns an error.

## Checksums

### Meta Page Checksums (Always On)

Both meta pages carry an xxhash64 checksum of all preceding fields. This is
mandatory and cannot be disabled. The meta page is the atomic commit point —
a torn write here would silently point to an inconsistent tree. The checksum
detects this and triggers fallback to the other meta page.

### Data Page Checksums (Optional)

Data pages (branch, leaf, overflow, RPL segment) optionally carry a CRC32C
checksum for defense against silent bitrot, firmware bugs, and storage
corruption that the filesystem does not detect.

Enabled via `Options.PageChecksum = true` at database creation time. The
setting is stored as a flag in the meta page's `Flags` field (bit 0) and
is **immutable after creation** — all pages in a checksummed database have
checksums, all pages in a non-checksummed database do not.

#### Storage

The checksum is stored as a **page footer** — the last 4 bytes of the page:

```
Page (with checksum enabled)
+-----------------------+
| Page Header (8 bytes) |
+-----------------------+
| Page Content          |
| (PageSize - 12 bytes) |
+-----------------------+
| CRC32C (4 bytes)      |  footer: checksum of bytes 0 through PageSize-5
+-----------------------+
```

The footer approach keeps the page header unchanged at 8 bytes. Usable
page content shrinks by 4 bytes when checksums are enabled — negligible
(0.1% at 4KB page size). The checksum covers the entire page from byte 0
through byte `PageSize - 5` (inclusive), including the page header.

Bitmap pages do not carry checksums (they have no page header or footer).
Bitmap integrity is guaranteed by the CoW model and meta page checksum.

#### Algorithm: CRC32C

CRC32C (Castagnoli) is used for data page checksums:
- **Hardware-accelerated** on amd64 (SSE4.2) and arm64 (CRC instructions).
  Go's `hash/crc32` package uses these automatically.
- **4 bytes** — minimal space overhead.
- **~200ns for a 4KB page** with hardware acceleration — comparable to a
  TLB miss, dominated by memory access rather than computation.

xxhash is faster for large inputs but CRC32C is sufficient for page-sized
data and has the advantage of hardware acceleration and smaller output.

#### Verification (Read Path)

When checksums are enabled, every page read from the mmap is verified:

1. Compute CRC32C of bytes 0 through `PageSize - 5`.
2. Compare with the 4-byte footer.
3. If mismatch, return `ErrCorrupted` with the page ID.

This adds ~200ns per page read. For a point lookup traversing 3-4 B+tree
levels, the overhead is ~800ns — negligible compared to the tree traversal
and potential page fault cost. For full-database scans reading millions of
pages, the overhead is measurable but bounded by memory bandwidth (the
CRC computation runs at memory speed with hardware acceleration).

Pages in `tx.cowPages` that have been CoW'd in the current transaction
are verified against their checksum when first read from the mmap (before
CoW). After CoW and modification, the new checksum is computed before the
commit-time fdatasync.

#### Computation (Write Path)

When checksums are enabled, the CRC32C checksum is computed on the mmap
page content after all modifications are complete, before the commit-time
`fdatasync()`. The footer is written directly into the mmap at the last 4
bytes of each CoW'd page.

#### What Checksums Do and Do Not Catch

**Catches:**
- Silent bitrot on disk (bit flips in stored data).
- Firmware bugs in SSD/NVMe controllers that corrupt data at rest.
- RAID controller or storage stack corruption.
- Kernel bugs that corrupt the page cache after successful write.

**Does not catch:**
- Torn writes (already handled by CoW + meta page checksum).
- In-memory corruption between `pwrite()` and disk (the checksum is
  computed on the same buffer that is written — if the buffer is corrupt,
  the checksum matches the corrupt data).
- Corruption introduced by the application via stray pointers or
  `unsafe.Pointer` misuse (the mmap is writable; the checksum is computed
  on the already-corrupted page).

#### Default

Checksums are **disabled by default**. The rationale: CoW already provides
crash consistency, and most production deployments use filesystems (ZFS,
btrfs, ext4 with metadata checksums) or storage controllers that detect
bitrot. The optional checksum is for users who want defense-in-depth or
run on storage without integrity guarantees.

## Integrity and Safety

- **No partial writes visible**: CoW ensures all modifications happen on new
  pages. The old tree is intact until the meta page swap. Bitmap leakage
  (pages that appear allocated but are unreferenced) is possible on crash
  between the bitmap pwrite and the meta pwrite, but tree integrity is always
  preserved. See "Bitmap leakage on crash" in Commit-Time Write Ordering.
- **Atomic commit**: A single meta page write (< page size, aligned) is the
  commit point. Even if it's torn, the checksum will fail and the DB falls
  back to the other meta page.
- **Write ordering**: In `SyncDurable` mode, CoW'd data pages are fdatasync'd
  BEFORE the meta page update, and the meta page is fdatasync'd AFTER writing
  it. In other sync modes, ordering relies on CoW (see Durability Modes).
- **Reader isolation**: Readers see an immutable snapshot. Pages they reference
  cannot be reused until all readers on that TxnID have finished.
- **Stale reader recovery**: If a process crashes without releasing its reader
  slot, the PID liveness check + process start time comparison allows the
  writer to reclaim the slot — even if the PID has been recycled by a new
  process (common in containerized environments with PID namespaces).
- **Stale writer recovery**: If the writer process crashes, `WriterPID` and
  `WriterStartTime` in the lock file header identify the dead process. The
  kernel releases the flock automatically. The next writer detects the dead
  or recycled PID (via start time comparison), cleans up reader slots from
  the crashed process, and proceeds — CoW guarantees the tree is consistent.
  Bitmap integrity is guaranteed by the deferred pwrite approach: bitmap
  modifications are held in memory (`tx.pendingAllocs`/`tx.pendingFrees`)
  and only written to disk via `pwrite()` at commit time. If the writer
  crashes before commit, no bitmap modifications reach disk — no leaked pages.
- **Silent bitrot detection**: When `PageChecksum` is enabled, every data page
  read is verified against its CRC32C footer. Corruption is detected at read
  time with `ErrCorrupted` identifying the affected page.
- **Disk full (ENOSPC)**: If `ftruncate()` (file growth) or `pwrite()` (bitmap/meta) fails with ENOSPC, the operation returns an error. A failed `pwrite()` during commit may result in a partially written bitmap page on disk. Since the meta page has not been updated, recovery falls back to the previous meta — the partially written bitmap page is superseded by the next successful commit. File growth failures during the transaction (before commit) cause `pageAlloc()` to return `ErrDBFull`.

## Background Maintenance

The `DB` struct runs a **maintenance goroutine** (started at `Open()`,
stopped at `Close()`) that performs periodic housekeeping to prevent
issues from accumulating during normal operation. The goal is to avoid
reaching a state that requires offline intervention.

### Coordination

Multiple processes sharing the same database coordinate via a
`LastMaintenanceTime` field in the lock file header — a `uint64`
monotonic clock value (`CLOCK_BOOTTIME` on Linux) updated after each
maintenance pass. Before starting a pass, the goroutine checks this
timestamp. If a recent pass was completed by any process (within
`MaintenanceOptions.Interval`), the goroutine skips. This ensures
only one process runs maintenance per interval, regardless of how
many processes have the database open.

### Tasks

The maintenance goroutine performs four tasks per pass:

#### 1. Bitmap Leak Reclamation

Reclaims pages that are allocated in the bitmap but unreferenced by any
tree structure — "leaked" pages caused by crashes between the bitmap
pwrite and meta pwrite (see Bitmap leakage on crash).

**Detection phase** (read transaction, non-blocking):
1. Open a read transaction.
2. Walk the full tree (all keyspaces, RPL segments, overflow pages) to
   build the set of all referenced page IDs.
3. Scan the allocation bitmap. Any page with its bit clear (allocated)
   that is not in the referenced set and is not a meta page, bitmap
   page, or RPL segment page is leaked.
4. Close the read transaction.

**Reclamation phase** (write transaction):
1. Open a write transaction.
2. For each leaked page ID, set its bitmap bit (free the page).
3. Commit. The reclaimed pages are now available for allocation.

**Safety**: A leaked page is permanently stuck — its bitmap bit is clear
so no future transaction can allocate it, and no tree node references it.
A page identified as leaked in the read transaction's snapshot cannot
become un-leaked by the time the write transaction runs. Detection in a
read transaction is therefore safe.

**Trigger**: Runs on every maintenance pass. Additionally, if `Open()`
detects crash recovery (selected a fallback meta page), the first
maintenance pass is scheduled immediately rather than waiting for the
interval.

#### 2. Stale Reader Slot Cleanup

Proactively scans the reader table and clears slots owned by dead
processes. This uses the same namespace-aware detection logic as the
writer's stale reader scan (see Stale reader detection): same PID
namespace uses PID + StartTime, cross-namespace uses heartbeat timeout.

No transaction is needed — slot cleanup is an atomic store (`TxnID = 0`)
on the shared mmap.

**Why this matters**: The writer already clears stale slots during RPL
reclamation, but only when it needs free pages. If no writer is active
for an extended period, stale slots from crashed containers sit
indefinitely, blocking RPL reclamation for the next writer. The
maintenance goroutine clears them proactively so the next write
transaction starts with a clean reader table.

#### 3. Checksum Scrubbing

When `PageChecksum` is enabled, the maintenance goroutine performs a
background read-only scan that verifies CRC32C checksums on data pages
proactively — before they are accessed by a user transaction. This
catches silent bitrot early, rather than surfacing `ErrCorrupted`
during a user read.

Each maintenance pass verifies `ScrubBatchSize` pages (default: 4096)
in a read transaction, advancing through the file sequentially across
passes. A `ScrubCursor` (the next page ID to verify) is tracked on the
`DB` struct and wraps around when it reaches `HighWaterMark`. A full
scrub cycle covers the entire database over
`ceil(HighWaterMark / ScrubBatchSize)` maintenance passes.

Detected corruption is logged via `slog.Logger` as a `CheckWarning`
with the affected page ID. The scrubber does not repair — it only
reports. Repair requires `CheckWithOptions(Repair)` or
`CopyTo(compact=true)`.

Scrubbing is skipped when `PageChecksum` is not enabled.

#### 4. Incremental Compaction

Defragments the database file by relocating pages in batches to restore
contiguous free runs for overflow page allocation. This is the online
alternative to `Compact()` (CopyTo + atomic rename).

**Trigger**: The allocator tracks the contiguous allocation failure
rate — the fraction of multi-page `pageAlloc(n)` calls (n > 1) that
fail to find a contiguous run on the first bitmap scan despite
sufficient total free pages. When this rate exceeds
`CompactionThreshold` (default: 0.5), the maintenance goroutine
schedules compaction work.

**Mechanism**: Each maintenance pass opens a write transaction and
relocates up to `CompactionBatchSize` pages (default: 1024):
1. Identify fragmented regions — pages that interrupt potential
   contiguous runs in the bitmap.
2. For each such page, CoW it to a new location (allocated from
   a region with more free neighbors).
3. The old page goes to the RPL and is reclaimed in a future
   transaction.
4. Commit.

Over multiple maintenance passes, scattered pages consolidate and
contiguous free runs emerge. The compaction converges when the failure
rate drops below the threshold.

**Cost per pass**: `CompactionBatchSize` CoW copies (memcpy +
parent pointer update). At 1024 pages × 4KB = 4MB of I/O per pass
plus the cascading CoW up the tree for each moved page. This is
bounded and amortized across the maintenance interval.

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

    // ScrubBatchSize is the number of pages to verify per
    // checksum scrubbing pass. Only meaningful when PageChecksum
    // is enabled. Default: 4096.
    ScrubBatchSize int

    // CompactionThreshold triggers incremental compaction when the
    // contiguous allocation failure rate exceeds this fraction.
    // Range: 0.0 (disabled) to 1.0. Default: 0.5.
    CompactionThreshold float64

    // CompactionBatchSize is the number of pages to relocate per
    // write transaction during incremental compaction.
    // Default: 1024.
    CompactionBatchSize int
}
```

Maintenance is a fixed-cost resource: one goroutine per `DB` handle.
Same lifecycle pattern as the flock and heartbeat goroutines. The
explicit tools (`Check`, `CheckWithOptions`, `Compact`, `CopyTo`)
remain available for on-demand use.
