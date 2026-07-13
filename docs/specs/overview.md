# gmdb Overview

A memory-mapped, multi-process, embedded key-value database for Go.

gmdb targets two concrete consumers: metadata stores for filesystem-
like systems (gitfs replacing SQLite) and document stores for read-
heavy multi-daemon services (notes shared across LLM sessions over
MCP). Both are read-dominated with intermittent small writes from
multiple processes, need atomic cross-keyspace commits, and benefit
from declarative secondary indexes maintained by the engine.

**Minimum Go version: 1.24.** gmdb uses `runtime.AddCleanup` (Go 1.24),
`structs.HostLayout` (Go 1.24), `os.OpenRoot` (Go 1.24),
`testing/synctest` (Go 1.24), `unique.Make` (Go 1.23), `cmp.Or`
(Go 1.22), and `iter.Seq2` (Go 1.23).

## How to read these specs

The settled design is split across `docs/specs/*.md`. Each
spec opens with a scope statement, declares its load-bearing
invariants explicitly, and is self-contained for a reader scoped to
that file. Cross-references are by spec file + section heading.

Implementation roadmaps live under `docs/plans/`; tracked follow-ups
live under `docs/issues/`.

## Package boundaries

The root `gmdb` package is the public API surface and the
integration layer; implementation lives in `internal/*` sub-packages
that depend strictly downward — exactly `btree → page`,
`pager → page`, and `pager → bitmap`; `internal/lock` stands alone.
`btree` never imports `pager`: it reaches storage only through its
own `PageWriter` interface, satisfied by the pager. No upward or
sibling imports. A change that appears to force an upward or sibling
import has found a misplaced seam: redraw the boundary (surface it
as a spec-amend candidate) rather than introducing an interface to
break the cycle in place.

## Invariants

This spec records no invariants of its own — it indexes the rest. The
invariants that span the whole engine are encoded in the specs of the
concept they govern: tree integrity in `page-formats.md` and
`free-space.md`; commit atomicity in `pager-slab.md`; reader
isolation in `transactions.md` and `cross-process.md`; index
consistency in `indexing.md`.

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
| Key storage | Inline up to threshold `T` + key extents past it (overflow-key cells, singleton restart groups) | Keys unbounded by page size (filesystem paths, composite index tuples); comparisons resolve in the inline prefix except on deep shared-prefix ties |
| Multiple values per key | Set keyspace with subpage + nested B+tree | First-class data primitive for set-shaped data (graph adjacency, postings lists, ZSET-shaped storage). **Not the indexing mechanism** — secondary indexes use composite-key plain keyspaces |
| Secondary indexes | Engine-maintained, composite-key storage, declarative extractor with persisted drift guard | Removes the manual-maintenance bug class without giving up the single-keyspace primitive; schema hash + user version tag catches drift at Open |
| Free space | Allocation bitmap + retired page log (RPL) | O(1) alloc via bitmap, no self-referential allocation, RPL tracks MVCC retirement |
| RPL entry format | Per-segment TxnID + array of PageIDs | TxnID stored once per segment (not per entry); doubles segment capacity |
| File format | Dynamic grow/shrink with configurable bounds; MaxSize immutable after creation | Auto-compaction via tail refund, no manual compaction needed; MaxSize fixed because bitmap region size depends on it |
| Isolation | Dual meta pages + CoW | No WAL needed, atomic commit |
| Crash safety | CoW + atomic meta page swap; lazy bitmap-leak reclamation via background maintenance | Tree is always consistent (CoW); on-disk bitmap leakage bounded by crashed txn's allocations and reclaimed by background maintenance — fast Open after crash |
| Durability | Three sync modes (Durable, DataOnly, Lazy) | Configurable ACID vs. performance |
| Cross-process | Shared memory lock file (`structs.HostLayout` structs, uint64 PIDs + process start times + PID namespace inodes + heartbeats) | C ABI layout guarantee for mmap'd structs; fixed-size reader table (scan+CAS); stale writer/reader recovery via PID liveness + start time comparison; cross-namespace via heartbeat |
| Write lock | Intra-process writer queue (channel) + single flock goroutine (cross-process) | Context-aware blocking; zero goroutine accumulation on cancellation; flock alone doesn't block same-process goroutines |
| Lock ordering | Documented globally (lifecycle → registry → per-keyspace → commit → bitmap) | Prevents deadlock; mandatory acquisition order for all internal mutex paths |
| Lagging readers | Callback-based notification | Application controls policy; no silent unbounded growth |
| Branch keys | Prefix-truncated separators (across levels + within page) | Shortest distinguishing prefix across levels; page-level prefix truncation stores a page's shared separator prefix once, so fan-out stays high even for deep-shared-prefix keys; shallow trees; full keys in leaves only |
| Leaf compression | Two variants: prefix-compressed leaves (variable-size restart groups, default) and uncompressed leaves (`RestartGroupTarget = 1`) | Density gains for shared-prefix workloads (directory listings, composite keys); per-keyspace tuning picks compressed or uncompressed; the uncompressed variant trades compression for single-O(log N) lookup, O(1) `Prev`, and simpler `Check()` walks |
| Key ordering | Lexicographic (byte-ordered) | Simple, general, no custom comparator needed |
| Byte order | Little-endian (fixed) | Portable across architectures |
| Checksums | Unified XXH3-64 footer (8 bytes) on meta and data pages; on by default | One hash family across the file; software-fast (benchmark-favored over CRC32C); defense against silent bitrot on commodity filesystems |
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
| Typed keyspaces | Generic `typed.Keyspace[K, V]` with `Encoder[T]` interface; `typed.Index[K, V, IK]` follow-on | Zero-cost type-safe API over byte-oriented Keyspace; index extractors as `func(K, V) []IK` |
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
