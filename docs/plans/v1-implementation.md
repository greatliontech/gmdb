# gmdb v1 Implementation Plan

Implementation roadmap for bringing the gmdb design (`docs/specs/`)
to a working v1.

Derived from every spec in `docs/specs/`. Chunks listed in
dependency order; sub-chunks `N.1` (planning/triage) and the
final close-out are fixed anchors per chunk, intermediate sub-
chunks are filled in during implementation per the adversarial-
review loop in `~/.claude/CLAUDE.md`.

## Codebase layout

All code lives in a single `gmdb` package (flat, no sub-packages —
avoids circular dependency issues between tightly-coupled
components and keeps the public API to one import path).
Organized by file:

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

## Coding conventions

**Default values via `cmp.Or`** (Go 1.22+):

```go
pageSize := cmp.Or(opts.PageSize, 4096)
maxReaders := cmp.Or(opts.MaxReaders, 4096)
maxBatchSize := cmp.Or(opts.MaxBatchSize, 1000)
maxTxBufferBytes := cmp.Or(opts.MaxTxBufferBytes, 256<<20)
restartGroupTarget := cmp.Or(opts.RestartGroupTarget, 16)
```

`cmp.Or` returns the first non-zero argument. Replaces verbose
`if field == 0 { field = default }` blocks throughout `Open()`
and transaction setup.

**Concurrency tests via `testing/synctest`** (Go 1.24+):

- **Batch coordinator.** Verify `MaxBatchDelay` timeout fires at
  the correct time, batch collection fills to `MaxBatchSize`,
  per-closure child txns commit/rollback correctly — without
  `time.Sleep` or racy channel coordination.
- **Flock goroutine.** Verify context cancellation while flock
  is pending correctly dequeues the writer.
- **Reader table.** Verify concurrent slot acquisition via CAS
  under contention, stale-reader detection clearing the correct
  slots.

**Read-only pager reuse.** Read transactions reuse `*Pager`
instances via a `sync.Pool` on the DB. Each `ReadTx` is also
pooled to avoid per-transaction allocations under high read
load.

**Slab buffers.** Page-sized `[]byte` buffers are pooled via a
process-global `sync.Pool` on the DB. Returning a buffer to the
pool clears it (zero-fill); reuse avoids GC pressure for steady
write workloads.

## Chunks

### Chunk 1 — Pager + slab foundations

**Scope.** Bring up the read-only mmap, the unified `Pager`, the
slab-based CoW path, and the commit write ordering on a single-
process database. Includes meta-page encode/decode/validate,
8-byte page header, xxhash64 footer compute/verify (when
enabled), the allocation bitmap (two-level summary, bit ops,
contiguous-run search), and the RPL append + tail-refund
machinery. No keyspace operations, no concurrency beyond the
single-process write lock. Target: a write transaction can
mutate raw page bytes through the pager and commit atomically;
recovery selects the active meta correctly after a crash.

Primary specs: `pager-slab.md`, `file-layout.md`,
`free-space.md`, `mmap-strategy.md`, `checksums.md`,
`durability.md`, `file-format.md`, `limits.md`.

Primary files: `page.go`, `pager.go`, `commit.go`, `alloc.go`,
`fileformat.go`, `mmap.go`, `mmap_linux.go`, `mmap_darwin.go`.

Sub-chunks `1.1` (planning + invariant triage) and final
close-out are fixed; intermediate sub-chunks filled lazily.

### Chunk 2 — Lock file + cross-process write lock

**Scope.** Stand up the lock-file layout (`structs.HostLayout`
header + reader table), the single flock goroutine with the
intra-process writer queue + cross-process flock, the heartbeat
goroutine, and the `LaggingReader` callback hook in
`pageAlloc()`. Includes namespace-aware stale-writer recovery.
Target: two processes (or two goroutines in one process) attempt
to write concurrently; the engine serialises them via the
write-lock protocol; context cancellation and `Close()` unwind
the goroutine deterministically.

Primary specs: `cross-process.md`, `lock-ordering.md`,
`leak-detection.md`.

Primary files: `lock.go`, `process_linux.go`, `process_darwin.go`,
`process_freebsd.go`.

### Chunk 3 — Write transaction lifecycle + reader table

**Scope.** Begin/Commit/Rollback for write transactions, reader-
slot acquire (scan + CAS) and release (atomic-ordered stores)
with the `HintEpoch` orphan anchor, RPL reclamation bound
(oldest-reader scan + checkpoint TxnID), loose-page tracking,
tail-refund commit step. Leak-detection cleanups on `Tx` and
`DB`. Target: a write transaction observes its own modifications;
a concurrent read transaction observes the pre-commit snapshot;
RPL reclamation respects active-reader pinning.

Primary specs: `transactions.md`, `leak-detection.md`,
`free-space.md §RPL Reclamation` and §Loose Pages, §Tail Page
Refund.

Primary files: `tx.go`, `lock.go`, `alloc.go`, `db.go`.

### Chunk 4 — B+tree primitives + cursor

**Scope.** Branch and leaf page formats with prefix-truncated
separators and prefix-compressed restart groups. Search, insert,
delete, split, merge/redistribute. Cursor (stateful, bidirectional)
with key reconstruction buffer and restart-group cache for
`Prev`. Overflow pages for inline-value overflow. No range
delete yet, no SetKeyspace, no indexing. Target: a Keyspace can
`Put`/`Get`/`Delete`/`Cursor` over byte-oriented key-value pairs
on a single keyspace.

Primary specs: `page-formats.md`, `transactions.md §Cursor State
Machine`.

Primary files: `page.go`, `branch.go`, `leaf.go`, `btree.go`,
`cursor.go`, `iter.go`.

### Chunk 5 — Keyspace API + DeleteRange

**Scope.** The keyspace B+tree (root meta → keyspace descriptors),
`Open/Create*Keyspace` family for `Kind = 0` only, descriptor
mutations propagating up via CoW, `DeleteRange` on un-indexed
keyspaces (three-phase boundary + interior-subtree algorithm),
keyspace-name interning. `Tx.SetKeyspaceConfig` for
`RestartGroupTarget`. Target: multiple keyspaces in one
database; `DeleteRange` is O(pages); `DeleteKeyspace` retires
the keyspace subtree atomically.

Primary specs: `keyspaces.md` (`Kind = 0` parts), `range-
delete.md`.

Primary files: `db.go`, `btree.go`, `tx.go`.

### Chunk 6 — SetKeyspace storage + API

**Scope.** SetKeyspace subpage encode/decode (variable and
fixed-value-size), nested-B+tree promotion / demotion, bulk-
free of a key's nested tree. `SetKeyspace` and `SetCursor`
APIs including intra-key value navigation. Target: a
`SetKeyspace` correctly stores, iterates, and deletes
`(key, value)` set members; promotion / demotion happens at
the 50% threshold; empty sets never persist.

Primary specs: `set-keyspace.md`, `keyspaces.md` (`Kind = 1`
parts).

Primary files: `subpage.go`, `btree.go`, `cursor.go`, `db.go`.

### Chunk 7 — Indexing

**Scope.** `IndexDecl` validation + schema-hash, per-keyspace
index registry sub-tree, NUL-escape composite-key encoding,
internal index keyspaces (`Kind = 2`), atomic Put/Delete index
maintenance, unique-index probes,
`ErrIndexFingerprintMismatch` + `IndexFingerprintError`
wrapping, `RebuildIndex` and `DropIndex`. Indexes on
SetKeyspaces (compound-PK encoding with `0x00 0x01`
separator). `DeleteRange` falls back to the per-row walk on
indexed keyspaces. Target: indexed `Put` updates the row and
all indexes atomically; drift on Open returns the specific
mismatch with field + names; rebuild loop completes.

Primary specs: `indexing.md`, `set-keyspace.md §Indexes on
SetKeyspaces`, `range-delete.md §Indexed-keyspace fallback`.

Primary files: `index.go`, `db.go`, `btree.go`.

### Chunk 8 — BulkLoad

**Scope.** Bottom-up B+tree construction for `Keyspace` and
`SetKeyspace`, slab bypass via streaming pwrite, indexed-
keyspace path with per-index sort (in-memory + spill via
`ScratchDir`), unique-violation detection at merge output,
atomicity via meta swap, bounded-leakage on abort. Target:
gitfs-scale migration of a sorted SQLite extract into a fresh
keyspace; indexed BulkLoad produces consistent row + index data
or aborts cleanly.

Primary specs: `bulkload.md`, `indexing.md §Bulk Operations`.

Primary files: `bulkload.go`, `index.go`, `db.go`.

### Chunk 9 — Typed (generics) layer

**Scope.** `Encoder[T]` interface, `FuncEncoder[T]`, engine-
provided canonical encoders, `TypedKeyspace[K, V]` /
`TypedKS[K, V]` (and the `SetKeyspace` variant),
`TypedCursor`, `TypedIndex[K, V, IK]` with the sealed
`AnyTypedIndex` interface, `TypedIndexQuery`. Encoder-ID empty
checks for indexed typed declarations. Target: zero-cost typed
API delegating to the byte layer; encoder-ID drift triggers
fingerprint mismatch correctly.

Primary specs: `typed-keyspaces.md`.

Primary files: `typed.go`.

### Chunk 10 — Batch + nested transactions

**Scope.** Channel-based batch coordinator, per-closure child
transactions with exactly-once invocation, child-commit /
rollback semantics (no buffer-content restoration), nested
arbitrary depth. Target: `Batch()` amortises commit cost across
N concurrent callers; failing closures don't affect siblings;
a final-commit failure surfaces uniformly.

Primary specs: `transactions.md §Write Batching` and §Nested
Transactions, `api-surface.md`.

Primary files: `db.go`, `tx.go`.

### Chunk 11 — Check + CopyTo + Compact

**Scope.** `Check()` and `CheckWithOptions(CheckIndexes,
Repair)` integrity walk producing `iter.Seq[CheckIssue]`,
`CopyTo(path, compact)` from a read snapshot,
`Compact()` with in-process reader drain + cross-process
flock-held atomic-rename swap. Bounded `ErrCompactReadersActive`
path. Target: `Check()` flags every category of corruption
catalogued in `integrity.md`; `Compact()` reclaims leaked
pages and shrinks the file without breaking concurrent
cross-process readers.

Primary specs: `api-surface.md §Check, CopyTo, Compact`,
`integrity.md`.

Primary files: `db.go`.

### Chunk 12 — Background maintenance goroutine

**Scope.** Maintenance loop (interval + `LastMaintenanceTime`
coordination), bitmap-leak reclamation, stale-reader cleanup,
checksum scrubbing (with `ScrubCursor`), incremental
compaction (with `CompactionThreshold` /
`CompactionBatchSize`). `MaintenanceOptions` plumbed through
`Options.Maintenance`. Target: a long-running service reclaims
leaked pages without offline intervention; cross-process
coordination avoids duplicate passes.

Primary specs: `background-maintenance.md`.

Primary files: `db.go`, `alloc.go`, `lock.go`.

## Cross-chunk concerns

- **`SyncMode` plumbing** lands incrementally: `SyncDurable` in
  Chunk 1, `SyncDataOnly` / `SyncLazy` / `SyncUnsafe` in Chunk
  3 (alongside RPL reclamation's checkpoint-bound consumer).
- **`Stats()` collection** evolves per chunk — `DBStats` gains
  fields as the corresponding subsystems land; final wiring in
  Chunk 11.
- **Error sentinels** in `errors.go` are added as their
  surfacing path arrives; the final inventory is the one in
  `api-surface.md`.
- **Adversarial review** runs per chunk (per `~/.claude/
  CLAUDE.md`); spec-amend candidates surfaced by a review pass
  route through the user before either spec or code changes
  land.

The plan is expected to evolve as the implementation reveals
constraints the specs did not anticipate; spec-amend channel
handles those per the workflow rules.
