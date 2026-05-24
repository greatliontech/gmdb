# gmdb v0 Implementation Plan

Implementation roadmap for bringing the gmdb design (`docs/specs/`)
to a working v0.

Derived from every spec in `docs/specs/`. Chunks listed in
dependency order; sub-chunks `N.1` (planning/triage) and the
final close-out are fixed anchors per chunk, intermediate sub-
chunks are filled in during implementation per the adversarial-
review loop in `~/.claude/CLAUDE.md`.

## v0 stance: clean breaks, no installed base

This is **v0**. `.semrel.yaml` sets `development: true`; gmdb has
no released binary, no deployments, no on-disk state any user is
attached to. Implementation chunks default to **clean breaks**:
when a spec change implies a different on-disk encoding, a
different API shape, or a different protocol step, the change
lands directly and old code/state is replaced outright. No
dual-read shims, no format discriminators bridging "v0a → v0b",
no deprecation aliases on renamed identifiers, no migration code
for a non-existent installed base.

This follows the Breaking-changes rule in `~/.claude/CLAUDE.md`
("Clean break is the default before v1 / with no installed
base"): backwards-compatibility scaffolding for state that does
not exist is **over-engineering**, which the Quality bar
classifies as a defect. The same rule's distinction applies:
runtime feature semantics required for correctness once the
system is in use (e.g., CoW + meta-swap atomicity, reader
isolation) are always real and must be preserved — those live
in the spec invariants and are not affected by the clean-break
default.

Two practical consequences for the chunks below:

- A spec amendment landed mid-chunk does not require a separate
  migration sub-chunk; the implementation is brought into line
  with the new spec in the same change set, old encodings are
  removed, and tests are updated.
- When a chunk's adversarial review surfaces a better protocol
  shape than what an earlier chunk shipped (a spec-amend
  candidate per `~/.claude/CLAUDE.md`), the amend is applied
  and the earlier chunk's code rewritten in place; no
  "back-compat opt-in" is added.

The clean-break default holds until the first tagged release
that promises stability to external users; the plan will then
gain a §Stability stance and grow whatever migration discipline
the future installed base requires.

## Codebase layout

The root `gmdb` package is the **public API surface**. Implementation
lives under `internal/<subsystem>/` sub-packages, kept narrow enough
to avoid circular dependencies between tightly coupled subsystems.
This trades the flat-file layout's single-import simplicity (still
preserved at the public surface) for clearer internal seams and
package-level testability.

| Path | Responsibility |
|------|---------------|
| `*.go` (root `gmdb`) | Public surface only. `DB`, `Tx`, `Keyspace`, `SetKeyspace`, `Cursor`, `SetCursor`, `Open`, the typed-generics layer (`Encoder[T]`, `TypedKeyspace[K, V]`, `TypedIndex[K, V, IK]`), sentinel errors, stats types, options. Wires internal subsystems together; no algorithmic code beyond thin glue. Files: `db.go`, `tx.go`, `keyspace.go`, `cursor.go`, `iter.go`, `typed.go`, `index.go` (query API; storage lives in `internal/index`), `errors.go`, `stats.go`, `options.go`. |
| `internal/page/*.go` | Pure byte-slice page codecs: 8-byte page header (Type/Flags/Count/AdditionalPages), meta page encode/decode/validate (incl. file-format fields, bitmap/RPL pointers, Flags), RPL segment encode/decode, branch page format with prefix-truncated separators, prefix-compressed leaf format with per-page `RestartInterval`, overflow page header, subpage (set-keyspace inline list), xxhash64 footer compute/verify. No I/O, no OS dependency. |
| `internal/bitmap/*.go` | Allocation bitmap data structure: two-level (detail + in-memory summary), bit set/clear, contiguous-run search with `math/bits` intrinsics, LIFO hint tracking, dirty-page tracking. Pure data structure — operates on `[]byte` for the detail level and `[]uint64` for the summary; no file I/O. |
| `internal/pager/*.go` | The unified `Pager` (read + write paths), slab `map[uint64]*[]byte`, `MaxTxBufferBytes` accounting, sync.Pool of page-sized buffers, CoW (`Page(id)` resolves via slab then mmap; write path copies old content into a slab buffer). Owns the file handle, the read-only mmap (`MAP_SHARED \| PROT_READ`, `mprotect` after open, reservation sized to `MaxSize`), the freespace state (RPL append-only chain, loose-page set, tail-refund machinery, allocation priority loose → bitmap → RPL reclamation → file extension), the file-format machinery (grow/shrink via `ftruncate`), and the commit pipeline (pwrite ordering dirty data → bitmap → fdatasync → meta → fdatasync per `SyncMode`, plus file shrink after the commit point). Platform mmap/madvise shims live in build-tagged `mmap_linux.go`, `mmap_darwin.go`, `mmap_freebsd.go` siblings. Imports `internal/page`, `internal/bitmap`. |
| `internal/btree/*.go` | B+tree algorithms over the pager: search, insert (CoW path from leaf to root, split with prefix-truncated separator computation), delete (merge/rebalance with configurable `MergeThreshold`, separator recomputation), range delete (boundary path finding, interior subtree retirement, boundary leaf cleanup, rebalance), cursor state machine (stack of `(pageID, index)`, key reconstruction buffer, restart-group cache for `Prev`). Set-keyspace bulk free (recursive subtree retirement) and subpage / nested-B+tree promotion/demotion at the 50% threshold. Bottom-up bulk construction (slab bypass via streaming pwrite, index sort + spill via `Options.ScratchDir`). All operations work on page byte slices, never Go heap objects. Imports `internal/page`, `internal/pager`. |
| `internal/lock/*.go` | Lock-file creation and mmap (shared memory, `structs.HostLayout` structs, uint64 PIDs + process start times + PID namespace inodes + heartbeats). Writer lock (single flock goroutine with intra-process writer queue + cross-process flock, context-aware, zero goroutine accumulation). Stale writer recovery (namespace-aware). Reader table (hint-based scan + CAS slot acquire, atomic-ordered store release, namespace-aware stale-reader detection, `HintEpoch` orphan anchor). Heartbeat goroutine. Oldest-reader query for RPL reclamation. Lagging-reader detection + callback. Platform process helpers (`process_linux.go`: `/proc/[pid]/stat` field 22 + `/proc/self/ns/pid` inode; `process_darwin.go` / `process_freebsd.go`: sysctl `KERN_PROC_PID`). Pure Go, no cgo. |
| `internal/index/*.go` | Per-keyspace index registry (sub-B+tree at `IndexRegistryRoot`), `IndexDecl` validation (schema-hash computation, fingerprint compare), NUL-escape composite-key encoding (`escape`, `unescape`, terminator append), write-path atomic maintenance (extractor invocation on Put/Delete, diff old/new entry sets, unique-index probes, all within the parent transaction), `RebuildIndex`, `DropIndex`. Index storage as engine-internal keyspaces (`Kind = 2`). Query helpers (`Lookup`, `LookupKeys`, `Range`, `Prefix`, `Get`) — the public `Index` type in the root package wraps these. Imports `internal/btree`, `internal/pager`, `internal/page`. |

**Boundary discipline.** Sub-packages depend strictly downward
(`btree → pager → bitmap`, `btree → page`, `pager → page`,
`pager → bitmap`, `index → btree`); no upward or sibling imports.
The root package imports any internal sub-package. If a chunk's
adversarial review surfaces a forced upward / sibling import, that
is the seam to redraw — file a spec-amend candidate rather than
introducing an interface to "break the cycle" inside the layout
the plan committed to.

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

Primary paths: `internal/page/`, `internal/bitmap/`,
`internal/pager/` (incl. its build-tagged mmap shims). Root
package gains the minimum `Open`/`Close` glue + a write-tx
end-to-end harness needed to exercise the commit pipeline.

Sub-chunks `1.1` (planning + invariant triage) and final
close-out are fixed; intermediate sub-chunks filled lazily.

Other chunks' "Primary files" lines still use the original
flat-layout names; they are updated at each chunk's own `N.1`
planning gate (per the workflow) rather than rewritten en masse
now.

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

**Scope.** Reader-slot acquire (scan + CAS) and release (atomic-
ordered stores) with the `HintEpoch` orphan anchor; RPL
reclamation bound from real oldest-reader scan + checkpoint TxnID;
reader transactions (`*ReadTx`, `DB.BeginRead`, `DB.View`); the
SyncMode surface (`Options.SyncMode`, `MetaFlagCheckpoint` per
commit, `DB.Checkpoint`, recovery preference for checkpoint-
flagged metas, `AllowSyncUnsafe` opt-in). Reader leak-detection
cleanup with the heap-shared `*closeGate` (drains in-flight Tx
cleanups in `Close` before unmap — the chunk-2.8 `*atomic.Bool`
gate was insufficient for the read-tx slot-release path).

Write-tx Begin/Commit/Rollback, loose-page tracking, and tail-
refund commit step landed in chunks 1–2 and are exercised by
chunk-3's reader-pin tests.

Target: a write transaction observes its own modifications; a
concurrent read transaction observes the pre-commit snapshot;
RPL reclamation respects active-reader pinning; recovery picks
the checkpoint-flagged meta over a higher-TxnID non-checkpoint
meta; SyncUnsafe is rejected without explicit opt-in.

Primary specs: `transactions.md`, `cross-process.md §Reader
Table`, `leak-detection.md` (chunk-3.3 refcount-drain amend),
`free-space.md §RPL Reclamation`, `durability.md`.

Spec amends landed: `cross-process.md` (hint placement on Coord;
case 0c min-inclusion), `leak-detection.md` (txInflight drain
clause), `api-surface.md` (*ReadTx / *Tx split).

Primary files: `read_tx.go`, `checkpoint.go`, `closegate.go`
(new); `tx.go`, `db.go`, `options.go`, `errors.go` (modified);
`internal/lock/reader.go`, `internal/lock/coord_reader.go`
(new); `internal/lock/coord.go` (hint field); `internal/pager/
commit.go` (SyncPolicy); `internal/pager/init.go` (checkpoint-
preferring active-meta selector); `internal/page/meta.go`
(ActiveMetaCheckpointPreferring).

Filed: `docs/issues/lagging-reader-callback.md` (Lands: 5 —
defer until `Keyspace.Put` is the first real `AllocPage`
consumer).

### Chunk 4 — B+tree primitives + cursor

**Scope.** Branch and leaf page formats with prefix-truncated
separators and prefix-compressed restart groups. Search, insert,
delete, split, merge/redistribute. Cursor (stateful, bidirectional)
with key reconstruction buffer and restart-group cache for
`Prev`. Overflow pages for inline-value overflow. No range
delete yet, no SetKeyspace, no indexing. Target: a Keyspace can
`Put`/`Get`/`Delete`/`Cursor` over byte-oriented key-value pairs
on a single keyspace.

**Sub-chunk progress.**

- **4.1** ✅ Triage + invariant derivation (commit context only).
- **4.2** ✅ `068232d` Branch + leaf + overflow page codecs in
  `internal/page` (branch.go, leaf.go, overflow.go). Round-trip
  tests pin invariants 2 (delta SharedLen correctness), 3
  (restart-table position), 4 (overflow run-length). Decoder
  robustness contract: total over input, never panics on slice
  OOR — pins via M1 (forged KeyLen) and M3 (forged
  RestartCount) regression tests.
- **4.3** ✅ `d4cb485` `internal/btree` foundation: read-only
  `Get(rootID, key)` descending root→branch→leaf, `Has` membership.
  `page.LeafLookup` two-phase binary search (restart-table phase 1
  + delta scan phase 2). Tests cover empty/single-leaf/single-
  branch/three-level descent, corrupted-page-type and null-child
  rejection wrapped as `btree.ErrCorrupted`. Overflow-flagged leaf
  entry returns `ErrOverflowValueUnsupported` (lifted in 4.7).
- **4.4** ✅ `afe2b92` `Put` + split + CoW propagation + root
  growth. `PageWriter` interface (PageReader + AllocPage/CoW/
  AllocSlab/FreePage; *pager.Pager satisfies it). `page.Shortest
  Separator(L, R)` per page-formats.md §Prefix-Truncated Branch
  Keys. Tests cover empty-tree, single-leaf, key-update, leaf
  split + root growth, 500-key round-trip, 400-large-key forced
  multi-level tree, insertion-order content invariance, oversize-
  on-both-empty-and-non-empty paths, and the slab-leak invariant
  (every allocated page is reachable or freed, never neither).
  **Decode→Encode aliasing fix** (load-bearing): the btree's CoW-
  then-re-encode flow uses the same buffer as decode source AND
  encode target; `Encode*` clears the buffer before write,
  zeroing slices `Decode*` returned by zero-copy borrow. Fixed
  by deep-copying Keys/Values immediately after every Decode in
  `leafEntriesAsEncoded` and inline in `ascendWithSplit`.
- **4.5** ✅ `2810eef` `Delete` + merge/redistribute. Recursive
  descent, leaf CoW + entry removal, merge with sibling (combined
  fits in one page) or count-balanced redistribute; cascade
  through branches; root collapse when root branch shrinks to a
  single child; `rootID=0` when the only leaf entry is deleted.
  Separator handling per page-formats.md §Prefix-Truncated Branch
  Keys (removed at merge, recomputed via `ShortestSeparator` at
  leaf-redistribute, lifted from the middle cell at branch-
  redistribute). `MergeThreshold` is the `uint8` percentage
  parameter (range 1-50, default 25) from api-surface.md Options;
  `DefaultMergeThreshold` and `MaxMergeThreshold` constants re-
  exported. `ErrNotFound` is the provisional missing-key surface
  pending chunk-5 keyspace-level resolution
  (`docs/issues/keyspace-delete-missing-key.md`).
  `fakeWriter.FreePage` now asserts no-double-free so silent
  reclamation regressions surface. Tests pin three spec-tier
  invariants as the strongest artifact this stage affords:
  all-leaves-same-depth, every-non-root-≥-threshold, and slab-
  partition (allocated = reachable ⊕ freed) — the same partition
  test 4.4 introduced extended for delete-heavy workloads.
  Adversarial review: two rounds; round-1 surfaced H-1
  (empty-separator data-loss guard), M-1 (fakeWriter double-free
  detection), M-2 (merge `leftInterval` policy comment), M-3
  (`ErrCorrupted` wrap on redistribute oversize/<2 errors), L-1
  (test rootID=0 tightening); H-2 (root-collapse read-after-
  free) disputed with explanatory comment (`child` captured
  before `FreePage`, never re-read). Round-2 folded in a
  defense-in-depth `<3 combined cells` guard on branch
  redistribute.
- **4.6** ⏳ Cursor: forward Next, reverse Prev with group cache,
  key reconstruction buffer, Cursor.Delete state machine per
  transactions.md §Cursor State Machine.
- **4.7** ⏳ Overflow inline-value Put + Get (lifts the
  `ErrOverflowValueUnsupported` sentinel from 4.3).
- **4.8** ⏳ Close-out: chunk close-out gate (cite sweep, spec-
  tier invariant promotions, delete resolved issues).

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
