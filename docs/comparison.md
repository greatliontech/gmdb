# Architecture Comparison: gmdb vs libmdbx vs WiredTiger

## Core Architecture

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Language | Go | C | C |
| Storage engine | B+tree | B+tree | B+tree (row-store + column-store) |
| File layout | Single data file + lock file | Single data file + lock file | Multiple files (one per table + metadata + history store + WAL logs) |
| Page size | 4KB–64KB, immutable after creation | 256B–64KB | 512B–512MB, configurable per table |
| Multi-process | Yes (shared mmap + lock file) | Yes (shared mmap + lock file) | No (single process; RPC for multi-process) |

## Concurrency

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Writer concurrency | Single writer | Single writer | Multiple concurrent writers |
| Reader concurrency | Unlimited concurrent readers | Unlimited concurrent readers | Unlimited concurrent readers |
| MVCC mechanism | Copy-on-write pages; readers hold old root via snapshot TxnID | Copy-on-write pages; readers hold old root via snapshot TxnID | Per-key update chains (version lists); snapshot via global txn ID array |
| Write conflicts | Impossible (single writer) | Impossible (single writer) | Detected at commit time; conflicting txn gets rollback error |
| Reader isolation | Readers never block writer; writer never blocks readers | Readers never block writer; writer never blocks readers | Readers never block writers; writers may block readers on same page (hazard pointers) |
| Write lock | Intra-process channel queue + cross-process flock; single flock goroutine | POSIX robust mutexes (Linux), shared mutexes (macOS/FreeBSD), file locks (Windows) | Internal mutexes and spinlocks; no cross-process coordination needed |

## Write Path

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Modification strategy | Copy-on-write pages | Copy-on-write pages | In-memory update chains (WT_UPDATE linked lists per key) |
| Dirty page storage (default) | Anonymous mmap slab (GC-invisible) | malloc'd shadow pages | Heap-allocated WT_UPDATE + WT_INSERT structures |
| Dirty page storage (direct write) | Direct mmap writes | Direct mmap writes | N/A (always uses buffer pool) |
| Write-ahead log | No (CoW makes WAL unnecessary) | No (CoW makes WAL unnecessary) | Yes (slot-based consolidated WAL for commit-level durability) |
| When data reaches disk | At commit (pwrite or direct write + fdatasync) | At commit (pwrite or writemap + fdatasync) | At eviction (dirty pages) or checkpoint (all dirty pages); WAL at commit |
| Commit I/O | pwrite + fdatasync (or io_uring batch) | pwrite/writev + fdatasync (or msync in writemap) | WAL append + fsync; data pages written asynchronously by eviction/checkpoint |
| Atomic commit point | Meta page write (single page, checksummed) | Meta page write (two-phase txnid_a/txnid_b) | Checkpoint completion (turtle file update) |

## Dirty Page Management

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Tracking structure | Hash map (`map[uint64]*dirtyPage`) | Lazy-sorted array of (pgno, page pointer) pairs | Per-page WT_UPDATE chains + WT_INSERT skip lists; page marked dirty via WT_PAGE_MODIFY |
| Lookup | O(1) | O(log n) binary search on sorted portion | O(1) via page pointer |
| Insert | O(1) amortized | O(1) append (lazy sort) | O(1) prepend to update chain |
| Commit-time ordering | Sort keys, sequential pwrite | Sort dirty list, sequential pwrite | N/A (eviction writes pages individually; checkpoint reconciles) |
| Eviction trigger | Dirty count exceeds MaxDirtyPages | Dirty count exceeds dirtyroom | Cache usage exceeds eviction_target percentage |
| Eviction algorithm | Clock sweep (approximate LRU, O(1) per-access overhead) | LRU via dirtylru counter | Approximate LRU via read_gen (set to future value on access; eviction server increments base) |

## Page Format

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Page header size | 8 bytes (Type, Count, Overflow — no PageID) | 20 bytes (txnid, flags, lower/upper, pgno) | 28 bytes (WT_PAGE_HEADER) + 12 bytes (WT_BLOCK_HEADER) = 40 bytes |
| PageID in header | No (computed from file offset) | Yes (pgno stored in header) | No (determined by block address) |
| TxnID in page | No (only in meta page and RPL segments) | Yes (per-page txnid in header) | Per-update (each WT_UPDATE carries txn ID + timestamps) |
| Key storage (leaf) | Prefix-compressed with restart points (LevelDB-style delta encoding) | Full keys with sorted index array | Variable-length cells with optional prefix compression |
| Key storage (branch) | Prefix-truncated separators (shortest distinguishing prefix) | Full keys copied from leaves | Full keys |
| Value storage | Inline + overflow pages | Inline + overflow (large) pages | Inline cells + overflow pages; large values separated |

## Free Space Management

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Tracking mechanism | Allocation bitmap (1 bit per page) + Retired Page Log (RPL) | GC B+tree (FREE_DBI): keys = txnid, values = sorted page number lists | Extent lists (alloc/avail/discard) as skip lists per checkpoint |
| Self-referential problem | Eliminated: bitmap is a flat bitfield at fixed page offsets; RPL segments allocated from bitmap (O(1) bit flip, no recursion) | Present: updating the GC requires allocating pages, which may require reading the GC. Solved by iterative convergence loops in gc_update() | N/A: extent lists are written as blocks during checkpoint; no concurrent modification issue |
| Allocation complexity | O(1) amortized (LIFO hint + bitmap scan) | O(log n) lookup in GC B+tree + PNL management | Best-fit from avail skip list |
| Reclamation trigger | Writer scans reader table for oldest active TxnID; RPL entries older than oldest reader are moved to bitmap | Writer scans reader table for oldest active TxnID; GC entries older than oldest reader are reclaimed | Checkpoint deletes old checkpoint; blocks in both alloc and discard lists are freed to avail |
| Contiguous allocation | Bitmap scan with math/bits intrinsics for runs | Scan GC PNLs for sequences | Extent-based (skip list tracks ranges, not individual pages) |
| LIFO locality | LIFO hint (allocHint) points to most recently reclaimed region | MDBX_LIFORECLAIM mode: reclaim from newest txns first | N/A (best-fit allocation from avail list) |
| Loose pages | Hash map; O(1) reuse within same transaction | Linked list via page_next(); immediate reuse within same transaction | N/A (no equivalent concept; updates are in-memory chains) |

## Durability Modes

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Full durability | SyncDurable: fdatasync data + fdatasync meta | MDBX_SYNC_DURABLE: fdatasync data + fdatasync meta | transaction_sync enabled + fsync: WAL fsync per commit |
| Data-only sync | SyncDataOnly: fdatasync data, skip meta sync | MDBX_NOMETASYNC: fdatasync data, skip meta sync | N/A (WAL-based; no equivalent) |
| Lazy/deferred sync | SyncLazy: no fdatasync; rolls back to last checkpoint on crash | MDBX_SAFE_NOSYNC: no fdatasync; rolls back to last steady meta on crash | transaction_sync disabled: rely on periodic checkpoints for durability |
| No safety | SyncUnsafe: no fdatasync, no checkpoint fallback; requires AllowSyncUnsafe | MDBX_UTTERLY_NOSYNC: no fdatasync, wipes steady metas; risk of corruption | N/A (always has checkpoint mechanism) |
| Checkpoint concept | "Checkpoint": a commit whose data pages have been confirmed on stable storage; created by DB.Checkpoint() | "Steady meta": a meta page whose sign > DATASIGN_WEAK; created by fdatasync | Checkpoint: periodic/on-demand full snapshot of all dirty pages to disk; runs as snapshot isolation txn |
| Crash recovery | Select meta page with highest TxnID whose checksum is valid and checkpoint flag is set | Select meta page with valid txnid_a==txnid_b and prefer steady meta; check bootid for recency | Recover from last checkpoint + replay WAL from checkpoint LSN forward + rollback_to_stable |

## Meta Pages

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Count | 2 (pages 0 and 1) | 3 (pages 0, 1, and 2) | N/A (metadata is a separate B+tree file; turtle file tracks metadata checkpoint) |
| Selection | Highest valid TxnID with valid xxhash64 checksum | Troika system: recent (latest), prefer_steady (last synced), tail (write target) | Last checkpoint in turtle file |
| Integrity check | xxhash64 checksum of all preceding bytes | Two-phase txnid write (txnid_a at top, txnid_b at bottom; mismatch = torn write) + datasign for steady detection | Block-level CRC32 checksum on every page |
| Checkpoint marker | Checkpoint flag (bit 1 in Flags) | sign field: DATASIGN_WEAK vs calculated signature | N/A (checkpoint is always fully synced) |
| Why 2 vs 3 | Sufficient: writer updates the inactive one; crash falls back to the other | Third meta avoids any possibility of corrupting in-use metas; enables steady tracking while non-steady commits advance | N/A |

## Checksums

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Meta page | xxhash64 (always on) | Placeholder (TODO: hippeus_hash64); relies on two-phase txnid write for torn-write detection | N/A (metadata is a regular B+tree with block checksums) |
| Data pages | CRC32C footer (optional, immutable after creation) | None currently; `extra_pagehdr` reserved for future use | CRC32 in every WT_BLOCK_HEADER (always on) |
| Verification | On every page read from mmap (when enabled) | N/A for data pages; structural validation via MDBX_VALIDATION flag | On every block read from disk |

## Compression

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Page/block compression | No (explicit decision — incompatible with mmap zero-copy read path) | No (same reason) | Yes: pluggable (snappy, zstd, zlib, lz4, Intel IAA) — enabled by custom buffer pool architecture |
| Key compression | Prefix compression in leaves (restart-point delta encoding); prefix-truncated branch separators | No | Optional prefix compression (disabled by default) |
| Value compression | No | No | Optional dictionary compression (disabled by default); RLE for column-store |

Block compression requires decompressing pages into a buffer before use —
incompatible with mmap-based engines where the read path is a direct pointer
into the mapped file. WiredTiger can do it because it uses a custom buffer
pool with explicit `pread()`; every page read already goes through an
allocation + copy step, so adding decompression is natural. The mmap-based
engines (LMDB, libmdbx, gmdb) all omit block compression for this reason.
gmdb compensates with key-level prefix compression (leaf delta encoding +
branch separator truncation), which works within the mmap model.

## Cache / Memory

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Architecture | mmap-based (OS page cache manages everything) | mmap-based (OS page cache manages everything) | Custom buffer pool (application-level cache, heap-allocated) |
| Cache sizing | Implicit (OS page cache) | Implicit (OS page cache) | Explicit: `cache_size` option (default 100MB; MongoDB uses ~50% of RAM) |
| Eviction | OS-managed (mmap pages evicted by kernel) | OS-managed (mmap pages evicted by kernel) | Explicit: eviction server thread + worker thread pool; LRU-based with trigger/target thresholds |
| Dirty page memory | Anonymous mmap slab (pwrite mode) or shared mmap (direct write mode) | malloc'd pages (pwrite mode) or shared mmap (writemap mode) | Heap-allocated WT_UPDATE/WT_INSERT structures (always) |
| mmap usage | Primary I/O mechanism (read path) | Primary I/O mechanism (read path) | Optional read-only optimization for checkpoint data; not primary I/O |
| GC interaction | None (mmap slab is munmap'd at txn close) | None (malloc'd pages freed at txn close) | GC-unaware (C heap allocator) |
| Huge pages | MADV_HUGEPAGE opt-in (Linux) | MADV_HUGEPAGE opt-in (Linux) | Not applicable (heap-based cache) |
| Prefaulting | MADV_POPULATE_READ opt-in (Linux 5.14+) | MADV_POPULATE_READ opt-in (Linux 5.14+) | N/A (explicit read I/O) |
| Read txn cooldown | MADV_COLD opt-in (Linux 5.4+) | No equivalent | N/A (eviction handles cache pressure) |

## Multi-Value (DUPSORT)

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Feature name | SetKeyspace (separate type from Keyspace) | MDBX_DUPSORT flag on table | No native equivalent |
| Small sets | Subpage (inline sorted list in leaf cell) | Sub-page (P_SUBP, embedded mini-leaf in node data) | N/A |
| Large sets | Nested B+tree (root page ID in leaf cell) | Nested B+tree (N_TREE flag, tree_t in node data) | N/A |
| Fixed-size optimization | FixedValueSize option on SetKeyspace | MDBX_DUPFIXED flag (P_DUPFIX pages, no per-entry length prefix) | N/A |
| Promotion threshold | 50% of leaf page usable space | When sub-page exceeds node capacity | N/A |
| API model | Separate types: Keyspace (key→value) vs SetKeyspace (key→sorted set) with distinct method sets | Single type with runtime flags; cursor ops (MDB_FIRST_DUP, MDB_NEXT_DUP, etc.) | Would need application-level implementation |

## Transaction Features

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Isolation level | Snapshot (only) | Snapshot (only) | Snapshot (default), read-committed, read-uncommitted |
| Nested transactions | No | Yes (child txns inherit parent dirty list; not in WRITEMAP mode) | No (but has savepoints conceptually) |
| Transaction parking | No | Yes (mdbx_txn_park/unpark: release MVCC snapshot temporarily, can be ousted by writer) | No |
| Prepared transactions | No | No | Yes (two-phase commit: begin → prepare → commit/rollback) |
| Write batching | Yes (Batch() API: channel-based coordinator, rollback+retry) | No built-in equivalent | WriteBatch (bulk write API, but different mechanism) |
| Leak detection | runtime.AddCleanup on Tx and DB (Go 1.24+) | No automatic detection; manual mdbx_reader_check() | No automatic detection |

## Cross-Process Coordination

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Multi-process support | Yes | Yes | No (single process) |
| Lock file format | structs.HostLayout (Go 1.24+ C ABI guarantee) over shared mmap | C structs over shared mmap | Lock file for process exclusion only |
| Reader table | Flat array of 24-byte slots (TxnID + PID + ProcessStartTime); scan+CAS; hint-based scan start | Flat array of 32-byte cache-line-aligned slots (txnid + pid + tid + snapshot stats); mutex-protected slot allocation | N/A (in-process snapshot array) |
| Stale reader detection | kill(pid, 0) + process start time comparison (PID reuse safe) | kill(pid, 0) + process-level checks; no start time comparison; relies on fork detection | N/A |
| Stale writer detection | WriterPID + WriterStartTime in lock file header; flock auto-released by kernel on crash | Write mutex with owner-death detection (robust mutexes on Linux) | N/A (lock file prevents concurrent access) |
| Slow/lagging reader | Callback-based (LaggingReader callback returns wait/abort action) | Callback-based (MDBX_hsr_func returns kill/wait/abort); integrates with transaction parking and ousting | N/A |

## File Format / Sizing

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| Concept name | FileFormat | Geometry (geo_t in meta page) | N/A (per-table file management) |
| Min size | Configurable (MinSize) | Configurable (geo.lower) | N/A |
| Max size | Configurable, immutable after creation (MaxSize); determines bitmap region size | Configurable (geo.upper); determines mmap limit | N/A (files grow as needed) |
| Growth step | Configurable (GrowStep) | Configurable (geo.grow_pv, packed exponential) | Implicit (block allocation extends file) |
| Shrink threshold | Configurable (ShrinkThreshold) | Configurable (geo.shrink_pv, packed exponential) | Explicit compaction via WT_SESSION::compact |
| Auto-shrink | Yes (tail page refund at commit time) | Yes (tail truncation when first_unallocated drops) | No (manual compaction only) |
| Runtime modification | Tx.SetFileFormat() (all except MaxSize) | mdbx_env_set_geometry() (all params including upper) | N/A |

## Integrity Checking

| | gmdb | libmdbx | WiredTiger |
|---|---|---|---|
| API | DB.Check() iter.Seq[CheckIssue] (streaming, breakable) | mdbx_chk (offline tool) + MDBX_VALIDATION flag (runtime structural validation) | WT_SESSION::verify (per-table verification) |
| Scope | Full walk: all B+trees, bitmap, RPL, page accounting, leaked page detection | Full walk: all trees, GC, page accounting | Per-table: page structure, key ordering, overflow pages |
| Hot backup | DB.CopyTo(path, compact) from read txn snapshot | mdbx_env_copy / mdbx_env_copy2 with MDBX_CP_COMPACT | Incremental backup via cursor on backup: URI |

## Unique Features

### gmdb only
- Allocation bitmap (eliminates self-referential freelist problem)
- Retired Page Log (RPL) as append-only segment list (immutable segments, no old-head CoW)
- Leaf prefix compression with restart points (LevelDB-style)
- Prefix-truncated branch separators (shortest distinguishing prefix)
- Clock-based dirty page eviction with reference bits
- Anonymous mmap slab for dirty pages (GC-invisible in Go)
- io_uring commit path (Linux, optional)
- SetKeyspace as a separate type from Keyspace (compile-time safety)
- Typed keyspaces with Go generics
- runtime.AddCleanup leak detection for transactions and DB handles
- iter.Seq2 range iterators
- pwritev2 with RWF_DSYNC for small commits

### libmdbx only
- Three meta pages (troika system)
- Transaction parking (release and re-acquire MVCC snapshot)
- Nested write transactions
- LIFO reclaim mode for GC (shorter page circulation loops)
- Packed exponential encoding for geometry grow/shrink values
- Boot ID tracking for cross-reboot validation
- Robust mutexes with owner-death detection (Linux)
- Database GUID (dxbid)
- Per-page TxnID in page header

### WiredTiger only
- Multiple concurrent writers with conflict detection
- Write-ahead log (WAL) for commit-level durability
- Custom buffer pool with explicit eviction (not mmap-dependent)
- Block-level compression (snappy, zstd, zlib, lz4)
- Column-store (record-number keys with RLE)
- History Store (dedicated B+tree for old MVCC versions)
- Multiple isolation levels (snapshot, read-committed, read-uncommitted)
- Prepared transactions (two-phase commit)
- Hazard pointers for lock-free page access
- Shared cache pool across multiple connections
- Per-table file layout (separate .wt file per table)
- Checkpoint as a discrete background operation (vs. every-commit in LMDB-family)
