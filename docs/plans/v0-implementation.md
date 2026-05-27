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
| `internal/page/*.go` | Pure byte-slice page codecs: 8-byte page header (Type/Flags/Count/AdditionalPages), meta page encode/decode/validate (incl. file-format fields, bitmap/RPL pointers, Flags), RPL segment encode/decode, branch page format with prefix-truncated separators, leaf page in two variants (compressed: variable-size restart groups + prefix-compressed delta entries; uncompressed: full keys + positional offset table — selected per `Config.RestartGroupTarget`) exposed via `LeafReader` / `LeafBuilder` / `LeafIter`, overflow page header, subpage (set-keyspace inline list), xxhash64 footer compute/verify. No I/O, no OS dependency. |
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

Filed: the LaggingReader-callback follow-up (deferred to chunk 5
until `Keyspace.Put` becomes the first real `AllocPage` consumer
that can trip the bound-blocked path). Resolved at chunk 5.5; see
that chunk's entry for the landed surface.

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
  exported. `ErrNotFound` is the missing-key surface (the
  chunk-5.1 spec amendment pinned `ErrNotFound` at the public
  Keyspace.Delete surface, so the chunk-4 strict variant
  propagates unchanged).
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
- **4.6** Leaf format reset + cursor. The original 4.6 sub-chunk
  (cursor on the chunk-4.2 spec-literal `RestartInterval` +
  per-leaf keyBuf design) was reset mid-chunk after the
  adversarial review surfaced that variable-size restart groups,
  an uncompressed-variant escape hatch, and a page-package-owned
  bidirectional iterator were a structurally better shape than
  the spec-literal one. Pre-v1 clean-break policy applied: the
  chunk-4.2 leaf codecs were deleted outright (no migration, no
  format discriminator) and the page surface rewritten before
  the cursor landed on top.
  - **4.6α** ✅ `55492c4` Spec amend: `docs/specs/page-formats.md`
    §Leaf Page rewritten to specify variable-size restart groups
    (`RestartGroupTarget` is a *target* not a hard interval;
    natural breaks at SharedLen=0; restart-table entry carries
    `Count` so group boundaries can be derived from the table
    alone), the uncompressed leaf variant (`TypeLeafUncompressed`,
    selected via `RestartGroupTarget = 1`), and the cursor-side
    `LeafIter` contract. `api-surface.md` Options
    `RestartGroupTarget` semantics aligned (0 = engine default;
    1 = uncompressed; 2..255 = compressed group target).
  - **4.6β** ✅ `37ce80a` Page-package rewrite. New surface:
    `LeafReader` (variant-dispatching reader; O(1) construction +
    explicit `Validate` at pager-resolve boundary), `LeafBuilder`
    (variant-dispatching builder with natural-break heuristic +
    inline scratch arrays for zero-allocation steady state),
    `LeafIter` (bidirectional iterator with forward-streaming +
    buffered-mode group-boundary handling), `LeafEntry` struct.
    Old codecs (`DecodeLeaf` / `EncodeLeaf` / `LeafLookup` /
    `LeafEncodedSize` / `LeafRestartInterval` / `EncodedEntry`)
    deleted outright per the clean-break policy.
  - **4.6γ** ✅ `5636fad` Btree port. `internal/btree/{put,delete,
    btree}.go` and tests ported onto `LeafReader` / `LeafBuilder`.
    `Put` lost its `restartInterval uint16` parameter (now in
    `Config.RestartGroupTarget`). Decode→encode aliasing fix
    preserved via `readLeafEntriesDeepCopy`; CoW + FreePage
    ordering preserved; spec-tier invariants (all-leaves-same-
    depth, every-non-root-≥-threshold, slab-partition) survive
    the port. New test
    `TestPutDeleteGetUncompressedLeafVariant` covers
    `RestartGroupTarget=1` end-to-end through Put + Get +
    Delete + merge. Pre-existing adjacent same-key-overflow-
    replace chain-orphan finding filed for chunk-4.7
    resolution (issue history in `git log`).
  - **4.6δ** ✅ `b7730c4` Bidirectional cursor + generation
    counter. `internal/btree/cursor.go` implements the
    state machine per `transactions.md §Cursor State Machine`
    (Unpositioned / Positioned / End-of-iteration). Methods:
    `First / Last / Next / Prev / Seek (exact) / SeekGE (≥) /
    Current / Delete / Err / MarkStale`. Path-based descent
    with leaf-to-leaf transitions; scratch buffers
    (`keyBuf` / `bufKeys` / `bufEnts`) reclaimed across leaf
    transitions for zero-allocation steady state. Cursor.Delete
    tolerates CoW + merge cascade via internal `SeekGE(deletedKey)`
    re-position. Three spec-tier invariants pinned by tests:
    Unpositioned-distinct-from-EOI; Delete-advances-to-successor;
    Delete-tolerates-CoW-cascade. Forward-compat scaffolding for
    chunk-5 external-mutation invalidation filed as a follow-up
    (cursor MarkStale should clear curKey / curValue / iter so a
    caller bypassing the gen check sees nil rather than dangling
    references to potentially-freed leaf-buffer slices). Resolved
    at chunk 5.5.
- **4.7** ✅ Overflow inline-value Put + Get + chain free on
  replace/Delete. Lifts the `ErrOverflowValueUnsupported`
  sentinel from 4.3; resolves the chunk-4.6γ-filed chain-orphan
  finding via `TestPutOverflowReplaceFreesOldChain` +
  `TestDeleteOverflowEntryFreesChain`. `PageWriter` interface
  extended with `AllocContiguous` / `AllocSlabRun` / `FreeRun`;
  pager-side implementation lands in chunk 5+. `api-surface.md
  §Byte Slice Ownership` amended (overflow values are heap-
  allocated, caller-owned — the first-page header + optional
  per-page footers make a single contiguous mmap slice
  structurally impossible).
- **4.8** ✅ Chunk-4 close-out gate. Cite sweep (wrap-aware: date-
  stem + rejoined-comment grep, not single-line `git grep`) of
  `docs/specs/**/*.md` and `internal/**/*.go` for cites to chunk-4-
  resolved tracking artifacts: 0 hits — `put-overflow-replace-
  orphans-chain.md` was already promoted (load-bearing rationale
  moved into `TestPutOverflowReplaceFreesOldChain` docstring +
  `collectReachable` overflow-chain extension + 4.7 plan entry) and
  deleted by 4.7. Spec-tier invariant audit: all 7 chunk-4
  invariants confirmed enforced at the strongest artifact this
  stage affords (each pinned by a test):
  all-leaves-same-depth → `checkBalance` (delete_test.go);
  every-non-root ≥ MergeThreshold% → `checkUnderflowInvariant`
  (delete_test.go); slab partition (allocated = reachable ⊕ freed,
  extended for overflow chains) → `checkSlabPartition` +
  `collectReachable` (delete_test.go / put_test.go); cursor state
  machine (Unpositioned ≠ EOI; Delete advances to successor;
  Delete at last entry transitions to End; Delete tolerates CoW +
  merge cascade) → four `TestCursor*` cases (cursor_test.go);
  overflow run capacity → `TestOverflowRunLengthBoundaries`
  (page/overflow_test.go); overflow chain reachability →
  `collectReachable` overflow-chain walk (put_test.go); overflow
  Put-replace / Delete frees chain →
  `TestPutOverflowReplaceFreesOldChain` +
  `TestDeleteOverflowEntryFreesChain` +
  `TestCursorDeleteOverflowEntryFreesChain`. Zero spec-tier
  invariants promote at this gate (every chunk-4 invariant was
  encoded as a test at its introducing sub-chunk, never recorded-
  only). README index current: 6 open entries, all `Lands:` in
  chunks 5+ or condition-triggered, none fire on chunk-4 work.

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

**Sub-chunk progress.**

- **5.1** ✅ Triage + invariant derivation + API decisions
  (docs-only).
  *Triage gate (chunk-start).* Two `Lands: 5` entries matched:
  the keyspace-Delete-on-miss policy follow-up (folded into 5.1
  then **closed** — decision recorded in spec, rationale promoted,
  file deleted); the LaggingReader-callback follow-up (folded into
  5.5 — chunk 5.5's `Keyspace.Put` is the first real
  `pager.AllocPage` consumer). Two condition-triggered entries
  resolved: the cursor-MarkStale buffer-clear follow-up (folded
  into 5.5 — keyspace integration wires the first external
  `MarkStale` call-sites); the slog-default-vs-spec follow-up
  (**redeferred** at 5.1 because chunk 5 originally did not
  introduce `Options.Logger`; subsequently folded into 5.5 — see
  that chunk's entry). The bitmap-rollback-undo-log entry
  (profiling-driven, no match).
  *API decision (user-locked).* Missing-key Delete semantics:
  `ErrNotFound` everywhere (LMDB-style) — applies to
  `Keyspace.Delete`, `SetKeyspace.Delete`,
  `SetKeyspace.DeleteValue`, and their `TypedKS` /
  `TypedSetKS` equivalents. `DeleteRange` returns `(0, nil)`
  for an empty range (bulk semantics: rows-affected, not
  membership). `Cursor.Delete` / `SetCursor.Delete` stay
  state-bound (`ErrCursorUnpositioned`), never membership-bound.
  Recorded as a new `Invariant: kind=clause-explicit` block in
  `api-surface.md §Invariants`; signatures in `api-surface.md
  §Keyspace API` / `§SetKeyspace API` and
  `typed-keyspaces.md` carry inline pointers.
  *Spec-tier invariant enforcement schedule.* 5.1 lands **zero**
  invariant promotions itself — the new Delete-on-miss invariant
  is spec-tier (recorded in `api-surface.md §Invariants`) until
  its enforcing test lands at 5.5. Each later sub-chunk runs its
  own chunk-start gate and promotes the relevant spec-tier
  invariants via tests at the introducing sub-chunk. The schedule
  below is informational, not a 5.1 commitment:
  `keyspaces.md` #1 (40-byte descriptor) → 5.3 codec round-trip
  test; #2 (Kind enumeration / immutability) and #3
  (`ErrKeyspaceKindMismatch`) → 5.4, **Kind=0 portions only** —
  the Kind=1 / Kind=2 reachability lands at chunks 6 / 7
  respectively; #5 (`FixedValueSize != 0 AND Kind != 1`
  rejection) → 5.4 promotes the **Kind=0 portion** (reject
  `FixedValueSize != 0` when creating a Kind=0 keyspace); the
  full `FixedValueSize` immutability + Kind=1 meaningfulness
  promotes at chunk 6 when Kind=1 lands; #6 (`SetKeyspaceConfig`
  mutability semantics) → 5.5; `range-delete.md` #1/#2/#3 →
  5.7; the new Delete-on-miss invariant → 5.5 tests across
  `Keyspace.Delete` (the `SetKeyspace.Delete` /
  `SetKeyspace.DeleteValue` enforcement lands at chunk 6 when
  the SetKeyspace surface itself lands).
- **5.2** ✅ Pager: `AllocContiguous` / `AllocSlabRun` /
  `FreeRun` — implements the chunk-4.7 `PageWriter` extensions
  on `*pager.Pager`. `AllocContiguous(n)` walks the
  free-space.md §Page Allocation Priority n>1 path (bitmap
  `FindContiguous` → RPL reclaim + retry → file extension);
  `AllocSlabRun(firstID, n)` installs the slab buffers with a
  single int64-arithmetic budget check; `FreeRun(firstID, n)`
  dispatches per-id through `FreePage`. Adjacent fix: extended
  `FreePage` to handle the chunk-4.7 overflow-rollback case
  (page in `pendingAllocs` but not yet in `p.dirty` — allocated
  this tx but never CoW'd / AllocSlab'd) by restoring the
  bitmap bit instead of routing to `retiredPages`; the same-tx
  rule (no prior-tx reader holds a snapshot) means RPL retirement
  would be wrong. Tests pin three chunk-5.2 spec-tier
  invariants: Inv-1 (`AllocContiguous` atomicity on
  `ensureFileCovers` failure), Inv-2 (alloc + immediate
  `FreeRun` round-trip on bitmap path and on HWM-extension
  path), Inv-3 (chunk-4.7 overflow chain `Put` / `Get` /
  `Delete` parity over `*pager.Pager` — the integration test
  in `internal/btree/pager_integration_test.go` is the first
  cross-package writer-pager fixture). Filed:
  `docs/issues/pager-test-helper-export.md` (Lands: condition
  — when chunk 5.3+ adds a second cross-package fixture caller,
  factor the duplicated `setupWriter` helper).
- **5.3** ✅ Keyspace descriptor codec
  (`internal/page/keyspace_descriptor.go`). 40-byte struct
  encode/decode/validate per `keyspaces.md §Keyspace Descriptor`.
  Surface: `KeyspaceDescriptor` struct, `KeyspaceDescriptorSize`
  constant, `KeyspaceKind{Keyspace,SetKeyspace,IndexInternal}`
  constants, `EncodeKeyspaceDescriptor` / `DecodeKeyspaceDescriptor`
  / `ValidateKeyspaceDescriptor`. Encode zeroes the 3 reserved
  bytes; Decode does not validate (use `ValidateKeyspaceDescriptor`
  separately, matching the chunk-1 `DecodeMeta` / `ValidateMeta`
  pattern). Tests promote four spec-tier invariants:
  `keyspaces.md` #1 (40-byte format + field offsets) →
  `TestKeyspaceDescriptorRoundTripAllFields` with per-field
  offset/width assertions; #2 (Kind ∈ {0,1,2}) →
  `TestKeyspaceDescriptorValidateRejectsUnknownKind`; #5
  (FixedValueSize ≠ 0 ⇒ Kind == 1) →
  `TestKeyspaceDescriptorValidateRejectsFixedValueSizeOnNonSet`;
  the reserved-bytes-zero clause →
  `TestKeyspaceDescriptorValidateRejectsNonZeroReserved`. Plus
  `TestKeyspaceDescriptorValidateRejectsRestartGroupTargetOverflow`
  pins the 255-cap from `page-formats.md §Compressed Leaf` (the
  uint8 Count field). The chunk-5.1 enforcement schedule attributed
  the Kind/FixedValueSize/RestartGroupTarget rejections to 5.4 —
  refined here at 5.3 because the codec is the rejection
  mechanism; 5.4's API surface inherits via callsite.
- **5.4** ✅ Keyspace B+tree machinery + `Open*` / `Create*` /
  `CreateKeyspaceIfNotExists` / `ListKeyspaces` + name interning
  via `unique.Handle[string]` + `meta.KeyspaceRoot` /
  `NumKeyspaces` CoW propagation through `*pager.Pager`. New
  files: `keyspace.go`, `keyspace_test.go`. Tx state extended
  with `keyspaceRoot` + `numKeyspaces` + per-tx open-keyspace
  cache (seeded from `prevMeta` at `Begin`; passed to
  `pager.Commit` at `Commit`). Public sentinels added:
  `ErrNotFound`, `ErrKeyExists`, `ErrKeyEmpty`,
  `ErrKeyspaceKindMismatch`, `ErrKeyspaceReserved`. Open uses
  `btree.Get` on the keyspace B+tree (cfg from pager); Create
  uses `btree.Put`; List uses `btree.NewReadCursor` and filters
  Kind=2 entries per `keyspaces.md §Keyspace Descriptor` (Kind=2
  internals are not name-addressable from user API).
  Promotes four chunk-5.4 invariants (Inv-A `ErrNotFound`/
  `ErrKeyExists` semantics; Inv-B CoW propagation across commit;
  Inv-C `numKeyspaces` matches B+tree-leaf-entry count;
  Inv-D ListKeyspaces filters Kind=2) and the API-level
  inheritance of `keyspaces.md` #2/#3/#4/#5 (forged-descriptor
  tests for Kind=3 → wrapped `ErrCorrupted`, Kind=1 →
  `ErrKeyspaceKindMismatch`, Kind=2 → `ErrKeyspaceReserved`,
  `FixedValueSize ≠ 0` on Kind=0 → wrapped `ErrCorrupted`).
  Cross-Kind `ErrKeyspaceKindMismatch` tested at 5.4 via codec-
  level forging (chunk-5.1 plan deferred this to chunk 6 on
  the basis that chunk 5 lacks CreateSetKeyspace; refined at
  5.4 since codec-forging makes the path testable now).
  Adjacent fix (chunk-1 latent bug, demonstrated-fault anchor
  via `TestListKeyspacesReturnsSortedNames`): the `AllocPage`
  loose-pop branch left the previously-CoW'd slab buffer in
  `p.dirty[id]` with stale content; subsequent `pw.CoW(srcID,
  loose-popped-id)` hit CoW's idempotent-re-CoW shortcut and
  returned the stale buffer instead of refreshing from srcID,
  silently losing data on multi-Put workloads against the same
  B+tree. Fix: AllocPage's loose-pop now detaches the buffer
  into a new `p.detachedBufs` slice (preserving byte-slice
  ownership — the original caller's borrowed `[]byte` stays
  valid through tx close; the buffer is pool-Put'd by
  `ReleaseAll` alongside `p.dirty`'s buffers). loose-pop also
  adds the id to `pendingAllocs` so a subsequent `FreePage` on
  the loose-popped id without intermediate `CoW`/`AllocSlab`
  takes the chunk-5.2 `pendingAllocs` branch (bitmap-bit
  restored) rather than the prior-tx `retiredPages` branch.
  Regression test:
  `internal/pager/freespace_run_test.go::TestLoosePoolPopDetachesStaleBuffer`.
- **5.5** ✅ Keyspace data ops + cursor surface +
  SetKeyspaceConfig + LaggingReader + Options.Logger. User
  re-confirmed the chunk-5.1 bundling at 5.5.1 and added
  Options.Logger to the bundle (folded the slog-default-vs-spec
  follow-up: pre-v1 clean-break to nil-default instead of
  `slog.Default()`).
  Surface (in `keyspace.go`):
  `Keyspace.Get` / `Put` / `Delete` on Kind=0 wires chunk-4
  `btree.*` through `desc.Root` + `desc.Count` maintenance +
  descriptor CoW propagation (each mutation re-encodes the
  descriptor and writes it back via `tx.storeDescriptor`).
  `Keyspace.Cursor()` returns a public `*Cursor` wrapping the
  chunk-4 internal cursor; cursors register on the keyspace so
  Put/Delete `MarkStale`'s them. Internal MarkStale fix
  (`internal/btree/cursor.go`): nils `curKey` / `curValue` and
  resets `iter` per the cursor-MarkStale buffer-clear follow-up
  (chunk-4.6δ filed, chunk-5.5 resolved) — a caller bypassing the
  gen check now sees nil rather than dangling references to
  potentially-freed leaf-buffer slices.
  `Tx.SetKeyspaceConfig(name, KeyspaceConfig)`: Kind-agnostic
  at the descriptor layer; `cfg.RestartGroupTarget == 0` is
  "leave unchanged"; `[1, 255]` updates; `> 255` returns
  `ErrInvalidOptions`; missing name returns `ErrNotFound`
  (the user-locked decision recorded as a spec amend at
  `api-surface.md §Keyspace API SetKeyspaceConfig` godoc per
  the chunk-5.4-filed SetKeyspaceConfig missing-name-behavior
  follow-up).
  `Options.LaggingReader` callback + `LaggingReaderInfo` /
  `LaggingReaderAction` types added to the public surface.
  Pager-side: new `Pager.SetLaggingReaderCallback` /
  `Pager.SetReclamationBoundRefresh` + `AllocPage` /
  `AllocContiguous` step 4 invokes the callback when bitmap-
  exhausted AND RPL is non-empty (bound-blocked). At most once
  per AllocPage call; Wait → refresh bound + retry once; Abort
  → ErrDBFull. `DB.Begin` wires the user callback through with
  a coord-driven bound-refresh closure.
  `Options.Logger` + `DB.logger` field captured at Open with
  nil → discard handler (clean-break vs the chunk-1
  `slog.Default()` default per pre-v1 policy). Cleanup paths in
  `tx.go` / `read_tx.go` / `db.go` use the captured logger via
  cleanup-closure-captured `*slog.Logger` (resolves the
  slog-default-vs-spec follow-up).
  `Options` additions: `MergeThreshold` (default 25),
  `RestartGroupTarget` (default 0 = engine default),
  `LaggingReader`, `Logger`. New sentinels:
  `ErrCursorUnpositioned`, `ErrCursorStale`.
  Resolves four chunk-4.x/5.x follow-ups (load-bearing rationale
  promoted inline to the surfaces above; tracking-artifact
  references deleted at 5.8 close-out per Issue-triage gate 2):
  the cursor-MarkStale buffer-clear follow-up, the
  LaggingReader-callback follow-up, the slog-default-vs-spec
  follow-up, and the SetKeyspaceConfig missing-name-behavior
  follow-up.
  Tests promote four chunk-5.5 invariants: Inv-A
  (Put/Get round-trip), Inv-B (descriptor CoW across commit +
  re-Open), Inv-C (Count tracks leaf entries; Put-replace does
  not bump Count; Delete decrements), Inv-D (sibling cursor
  MarkStale'd by Put/Delete; cur*/iter cleared; subsequent ops
  return `ErrCursorStale`); Inv-E (SetKeyspaceConfig 0/[1,255]
  />255/missing semantics); Inv-F (LaggingReader at-most-once
  + Wait/Abort + nil-callback-falls-through-to-file-extension).
  Plus chunk-5.1 Kind=0 portion of Delete-on-miss invariant
  promoted at `Keyspace.Delete` (`ErrNotFound` on miss).
- **5.6** ✅ `DeleteKeyspace` — single-keyspace subtree
  retirement + deferred descriptor flush refactor.
  *Triage gate (chunk-start).* One condition-triggered entry
  resolved: the chunk-5.5 round-1 H1 descriptor-drift class
  (partial-mutation failure: a per-op storeDescriptor write
  could fail AFTER a successful data-tree mutation, producing
  on-disk orphan pages on a subsequent Commit) **folded** — the
  user picked option 4 (atomic-commit-time descriptor flush) over
  option 1 (tx-poison) because the same reshape closes both the
  chunk-5.5 H1 two-write-no-atomicity shape and chunk-5.6
  DeleteKeyspace's structurally-identical failure surface in one
  design move. Other open entries: no
  fires at 5.6 (`pager-test-helper-export.md` may fire at 5.6
  test-time if a second cross-package writer-pager fixture
  materialises — opportunistically address; current plan keeps
  5.6 tests inside the `gmdb` package against `tx.pgr`, so the
  helper duplication does not grow).
  *Project-invariants trigger (new domain concept: keyspace
  retirement; new persistence boundary: `meta.KeyspaceRoot`
  via deferred flush; new trust boundary: `ErrKeyspaceClosed`).*
  Four chunk-5.6 invariants encoded as enforced tests:
    - **Inv-A** (clause-explicit, `api-surface.md §Keyspace API
      DeleteKeyspace`): `DeleteKeyspace(name)` followed by
      `OpenKeyspace(name)` returns `ErrNotFound` (same tx and
      post-commit re-Open).
    - **Inv-B** (entailed; from "bulk subtree retirement"):
      every page reachable from `desc.Root` pre-Delete enters
      `loosePages` (same-tx allocations) or `retiredPages`
      (prior-tx pages, RPL'd at commit). No page reachable
      pre-Delete remains both bitmap-allocated AND unreachable
      post-commit.
    - **Inv-C** (entailed; from `file-layout.md §Meta Page`):
      `tx.numKeyspaces` decrements on success; `meta.NumKeyspaces`
      and `meta.KeyspaceRoot` reflect the post-Delete state
      across commit + re-Open.
    - **Inv-D** (clause-explicit, `api-surface.md §Keyspace API
      DeleteKeyspace`): every `*Keyspace` / `*Cursor`
      previously opened on `name` within the tx returns
      `ErrKeyspaceClosed` on subsequent ops; re-creating the
      same name via `CreateKeyspace` does NOT reactivate the
      dead handle.
  *Deferred descriptor flush refactor (folds chunk-5.5 H1).*
  Removes per-op `tx.storeDescriptor` from `Keyspace.Put` /
  `Delete` / `Cursor.Delete` / `SetKeyspaceConfig` /
  `CreateKeyspace*`; in-memory `*Keyspace.desc` mutations only.
  `Tx.Commit` runs a deterministic flush walk before
  `pager.Commit`: for each name in `tx.pendingDeletes` →
  `btree.Delete(keyspaceRoot, name)`; for each
  `tx.openKeyspaces[name]` with `state ∈ {created, dirty}` →
  `btree.Put(keyspaceRoot, name, EncodeKeyspaceDescriptor(desc))`.
  Walk-failure path: `AbortTx` (restores bitmap snapshot, clears
  loose + retired pages, releases slab buffers — no on-disk
  state has been written yet, so no poisoning needed; failure
  is a plain Commit error, the caller's Rollback equivalent is
  the AbortTx that already ran). The two-set design
  (`openKeyspaces` for live + dirty, `pendingDeletes` for
  remove-from-disk) makes Create+Delete in same tx a no-op on
  the keyspace-B+tree, and Delete+Create in same tx a single
  `btree.Put` overwrite (the `Create` removes the
  `pendingDeletes` entry).
  *Surface.*
    - New file: `internal/btree/subtree.go` (`FreeSubtree(pw,
      cfg, rootID)` walks the tree, retiring every branch +
      leaf + overflow-chain via `FreePage` / `FreeRun`).
    - `keyspace.go`: `Tx.DeleteKeyspace(name)`; `*Keyspace.state`
      enum; `*Keyspace.dead` invalidation flag; deferred-flush
      paths through `Keyspace.Put` / `Delete` / `Cursor.Delete`.
    - `tx.go`: `tx.pendingDeletes map[string]struct{}`;
      `tx.deadKeyspaces []*Keyspace`; `tx.flushKeyspaces()`
      Commit-time helper; flush invocation in `Tx.Commit` before
      `pager.Commit`.
    - `errors.go`: `ErrKeyspaceClosed` sentinel.
  *Scope notes.* Chunk-5.6 bulk-free covers the single-keyspace
  B+tree (Kind=0 data tree) only. Chunk 6 extends `FreeSubtree`
  to walk SetKeyspace nested-tree promotions per
  `set-keyspace.md`. Chunk 7 extends `DeleteKeyspace` to also
  bulk-free each engine-internal index keyspace and the
  per-keyspace index registry sub-tree per `indexing.md`. The
  api-surface.md DeleteKeyspace godoc documents all three; the
  5.6 implementation handles only step 1.
  *Promotes:* Inv-A/B/C/D as enforced tests in
  `keyspace_test.go` and `keyspace_dataops_test.go`.
  *Resolves:* chunk-5.5 round-1 H1 (descriptor-drift class);
  load-bearing rationale promoted inline to
  `api-surface.md §Database and Transaction API Tx.Commit` godoc
  and to the `flushKeyspaces` godoc in `tx.go`. Tracking-artifact
  reference deleted at 5.8 close-out per Issue-triage gate 2.
  *Adversarial review (round 1).* Fresh-eyes general-purpose
  reviewer surfaced 0 H, 2 introduced M, plus L1-L5 + N1-N3.
  Dispositions:
    - **M1** (`keyspace.go`, IndexRegistryRoot assertion fired
      AFTER FreeSubtree): **fixed in-place** — reordered the
      `IndexRegistryRoot != 0` check before the bulk-free call.
    - **M2** (`Cursor.Err()` did not surface `ErrKeyspaceClosed`
      when called directly on a dead handle without an intervening
      nav op): **fixed in-place** — `Err()` now probes `c.ks.dead`
      before consulting the underlying btree cursor's sticky
      error; godoc updated; regression test
      `TestCursorErrReturnsKeyspaceClosedOnDeadHandle` added.
    - **L1** (defense-in-depth: `btree.Get`/`Has`/`Delete`/
      `Cursor`/`FreeSubtree` validate leaf pages via
      `LeafReader.Validate` but iterate branch children without an
      equivalent `ValidateBranch`): **filed** as
      `docs/issues/btree-branch-page-validation.md` —
      `class=adjacent` (pre-existing surface shared by chunks 1-4
      callers; chunk-5.6 inherited the pattern). Lands
      opportunistically at chunk 11 (`Check()`) or fuzz repro.
    - **L2** (variable name `wasOnDisk` confusing): **renamed** to
      `needsBtreeDelete` — names the flush-time contract instead
      of an ambiguous history fact.
    - **L3** (missing test: SetKeyspaceConfig → dirtyDescriptors →
      DeleteKeyspace round-trip): **added**
      `TestDeleteKeyspaceAfterSetKeyspaceConfigClearsDirtyDescriptor`.
    - **L4** (em-dash in error message): **disputed** — consistent
      with the codebase's existing voice (chunk-1 through chunk-5
      error messages and godocs all use em-dashes / Unicode).
    - **L5** (flushKeyspaces ErrCorrupted message specificity):
      **disputed** — message names the specific case
      (pendingDeletes) and chains via fmt.Errorf so callers using
      errors.Is satisfy gmdb.ErrCorrupted while logs show the
      precise path.
    - **N1** (Cursor() godoc on dead handle): **fixed** — godoc
      now correctly describes the requireOpen + Err probe.
    - **N2** (Cursor.Err godoc missing ErrKeyspaceClosed): **fixed**
      by the M2 fix's godoc rewrite.
    - **N3** (`mapBtreeErr` chunk-5.4 caveat): pre-existing on
      `eec7fc6`; not in this change set's diff. No action.
  Spec-amend candidates surfaced + user-locked:
    - (1) `api-surface.md §Keyspace API DeleteKeyspace` godoc
      missed `ErrKeyEmpty` in its error list: **amended** — added
      `ErrKeyEmpty when name is empty.` to the error list.
    - (3) Document that descriptor mutations are deferred to
      Commit (failure-mode surface shifts ErrTxTooLarge to
      Commit): **amended** — added a §Transactions clause on
      `Tx.Commit`'s godoc capturing the deferred-flush contract +
      the in-memory-success vs. on-disk-Commit-publishes
      distinction.
  Ship gate met: no introduced H/M unaddressed; every L/nit has a
  recorded disposition.
- **5.7** ✅ `DeleteRange` — three-phase algorithm
  (boundary paths + interior-subtree retire + boundary cleanup
  with rebalance + root collapse). Promotes `range-delete.md`
  #1/#2/#3.
  *Triage gate (chunk-start).* No `Lands:` entries resolved to
  chunk 5.7 (the chunk-5.5 round-1 H1 descriptor-drift class was
  resolved at 5.6, awaiting cite-promote at 5.8;
  `btree-branch-page-validation.md` is opportunistic-deferred).
  *Project-invariants trigger.* `range-delete.md` already
  records four invariants; chunk 5.7 enforces #1/#2/#3 via tests
  and defers #4 (indexed-keyspace per-row parity) to chunk 7
  alongside the indexing surface.
  *Surface.*
    - New file: `internal/btree/range_delete.go` (~430 LOC).
      `DeleteRange(pw, cfg, rootID, mergeThreshold, start, end)
      → (count, newRoot, err)` runs a single recursive descent
      that fuses phases 1+2 (boundary path identification +
      interior-subtree retire via the chunk-5.6 `FreeSubtree`)
      and handles phase 3 (boundary leaf rebuild + branch
      rebalance via `mergeOrRedistribute*`) on the unwinding
      path. Top-level root collapse iterates with a MaxTreeDepth
      bound (L-4 fix from the chunk-5.7 review).
    - `internal/btree/delete.go` refactor: factored
      `deleteFromBranch`'s post-recursion body into a reusable
      `patchBranchAfterChildDelete` helper so DeleteRange's
      single-child case shares the existing case-A/B/C
      merge/redistribute machinery. No behavior change for
      chunk-4 callers.
    - `internal/btree/subtree.go` refactor: `FreeSubtree`
      signature changed from `error` → `(uint64, error)` to
      count leaf entries retired. Chunk-5.6 `Tx.DeleteKeyspace`
      caller discards the count.
    - `keyspace.go`: `Keyspace.DeleteRange(start, end)` public
      surface. ErrKeyspaceClosed guard, empty-tree short-circuit,
      empty-but-non-nil boundary rejection (L-2 user-locked
      decision: nil = open, `[]byte{}` = ErrKeyEmpty),
      desc.Count underflow defense (L-3 fix: returns ErrCorrupted
      if `count > desc.Count` rather than wrapping under uint64
      arithmetic), in-memory descriptor update via the chunk-5.6
      deferred-flush path, markCursorsStale on success.
  *Adjacent fix (introduced-tests surface a chunk-5.5 latent
  bug).* `Cursor.rootID` was captured at construction time and
  never refreshed after a sibling mutation. Re-positioning via
  `First/Last/Seek/SeekGE` descended from a `FreePage`'d
  (now-retired) root whose mmap-resident bytes survive only
  until the loose-pool reuses the id, producing stale or
  corrupted reads. Fix: added `Cursor.SetRootID(rootID)` to
  btree.Cursor, and `Keyspace.markCursorsStale` (plus
  `Cursor.Delete`'s sibling-MarkStale loop) now refresh
  `c.inner.SetRootID(ks.desc.Root)` alongside MarkStale.
  Regression test extended in
  `TestCursorMarkStaleAfterSiblingPut` to verify post-re-position
  cursor sees sibling-Put'd entries. `class=adjacent` per the
  chunk-5.7 diff arbiter (cause-line predates this change set in
  chunk 5.5).
  *Spec amends (user-locked).*
    - `api-surface.md §Keyspace API DeleteRange` godoc: documented
      `nil` open-boundary sentinel vs. `[]byte{}` rejection
      semantic.
    - `range-delete.md` invariant #1: extended to clarify the
      `nil` vs. `[]byte{}` distinction in addition to the
      original `[start, end)` boundary clause.
  *Adversarial review (round 1).* Fresh-eyes general-purpose
  reviewer surfaced 0 introduced H/M, 1 disputed M (M-1
  post-merge underflow flag — pre-existing adjacent, filed),
  L-1/L-3/L-4 + nit-1..4 with dispositions:
    - **L-2** (empty-byte boundary policy): user-locked option (a)
      → reject empty-non-nil with `ErrKeyEmpty`; **fixed
      in-place** + spec amend applied.
    - **L-3** (`desc.Count` underflow defense): user-locked
      "fix both in-place" → ErrCorrupted return guard added.
    - **L-4** (root-collapse loop MaxTreeDepth guard): user-locked
      "fix both in-place" → bound added consistent with
      `freeSubtreeAt`.
    - **L-1** (DeleteRange's tx.requireOpen ordering): disputed by
      reviewer on re-read.
    - **M-1** (post-merge underflow flag forcibly cleared,
      regardless of fill ratio): **filed** as
      `docs/issues/btree-post-merge-underflow.md`,
      `class=adjacent` — cause-line is chunk-4
      `mergeOrRedistribute*` contract; `Lands: when invariant #3
      fill-ratio enforcement test is added`.
    - **nit-1** (`Cursor.SetRootID` exported without precondition
      guard): documentation handles the future caller;
      DeleteKeyspace's MarkStale path correctly skips SetRootID
      because cursors hit `ks.dead` first.
    - **nit-2** (`entries[:0]` aliasing): comment added.
    - **nit-3** / **nit-4**: disputed.
  *Promotes:* `range-delete.md` #1/#2/#3 as enforced tests in
  `internal/btree/range_delete_test.go` (9 tests) and
  `delete_range_test.go` (10 tests). Inv-#4 remains spec-tier
  (chunk 7).
  Ship gate met: no introduced H/M unaddressed; every L/nit has
  a recorded disposition.
- **5.8** ✅ Close-out: cite sweep, spec-tier invariant audit,
  delete resolved issues.
  *Cite sweep (Issue-triage gate 2).* Wrap-aware grep across
  `docs/specs/*.md`, `docs/plans/*.md`, and every `*.go` for
  references to chunk-3/4/5 resolved-and-deleted issue slugs +
  paths. Surfaced 13 dangling cites accumulated from prior
  close-outs that missed the cite-promote step (chunk 5.4's
  cursor-MarkStale forward-compat filing, chunk 5.5's bundle of
  four resolved issues, chunk 5.6's descriptor-drift resolution).
  Promote-then-retarget pass:
    - `tx.go` `Tx.Commit` godoc: descriptor-drift cite removed;
      load-bearing rationale (two-write-no-atomicity → single
      commit-time apply) kept inline.
    - `keyspace.go` `*Keyspace.openCursors` godoc + Tx
      SetKeyspaceConfig godoc: slug cites replaced with
      descriptive prose (sibling-mutation CoW/free hazard;
      Delete-on-miss invariant family attribution).
    - `internal/btree/cursor.go` `MarkStale` godoc: slug cite
      removed; rationale (buffer-clear safeguard for callers
      bypassing the gen check) stays inline.
    - `db.go` `DB.logger` + Open Options.Logger capture: slug
      cites replaced with descriptive references to the chunk-5.5
      spec-amend default (nil → discard handler).
    - `delete_keyspace_test.go` two cites in the file header +
      `TestDeferredFlushClosesDescriptorDrift`: slug-and-path
      cites replaced with descriptive prose pointing at
      Tx.Commit's deferred-flush godoc.
    - `docs/plans/v0-implementation.md`: 9 dangling cites in
      chunk-3.x, chunk-4.6δ, chunk-5.1, chunk-5.5, chunk-5.6, and
      chunk-5.7 narrative entries replaced with descriptive
      follow-up labels (e.g., "cursor-MarkStale buffer-clear
      follow-up (chunk-4.6δ filed, chunk-5.5 resolved)").
  *Delete.* `docs/issues/descriptor-drift-on-partial-failure.md`
  + README entry deleted. Chunks 5.4 / 5.5 resolved issues were
  already file-deleted in their respective commits (`fd0fe19`
  and `eec7fc6`); only the dangling cites needed promoting at
  this close-out.
  *Spec-tier invariant audit.* Every chunk-5.1 enforcement-
  schedule item verified landed:
    - `keyspaces.md` #1 (40-byte descriptor) → 5.3
      `TestKeyspaceDescriptorRoundTripAllFields` + per-field
      offset assertions.
    - `keyspaces.md` #2 (Kind enum / immutability, Kind=0
      portion) → 5.4 forged-descriptor tests; Kind=1 / Kind=2
      reachability deferred to chunks 6 / 7.
    - `keyspaces.md` #3 (`ErrKeyspaceKindMismatch`, Kind=0
      portion) → 5.4 `TestOpenKeyspaceRejectsForgedKindMismatch`.
    - `keyspaces.md` #4 (`ErrKeyspaceReserved` on Kind=2 +
      ListKeyspaces filter) → 5.4
      `TestListKeyspacesFiltersKindIndexInternal` +
      `TestOpenKeyspaceRejectsForgedKindReserved`. Kind=2
      reachability via index registry deferred to chunk 7.
    - `keyspaces.md` #5 (Kind=0 portion: FixedValueSize ≠ 0 on
      Kind=0 rejected) → 5.4
      `TestOpenKeyspaceRejectsForgedFixedValueSizeOnKind0`.
      Full FixedValueSize immutability + Kind=1 meaningfulness
      deferred to chunk 6.
    - `keyspaces.md` #6 (SetKeyspaceConfig mutability semantics)
      → 5.5 `TestSetKeyspaceConfigUpdatesRestartGroupTarget` +
      `TestKeyspacePutHonorsPerKeyspaceRestartGroupTarget`.
    - Delete-on-miss invariant (Kind=0 portion) → 5.5
      `TestKeyspaceDeleteMissingReturnsErrNotFound`. SetKeyspace
      enforcement deferred to chunk 6.
    - `range-delete.md` #1 / #2 / #3 → 5.7 nine btree-layer
      tests + ten public-surface tests. #4 (indexed per-row
      parity) deferred to chunk 7.
    - Chunk-5.6 Inv-A/B/C/D (DeleteKeyspace + handle
      invalidation) → 5.6 `delete_keyspace_test.go` (12 tests).
    - Chunk-5.7 adjacent fix (Cursor.SetRootID) regression test
      → extended `TestCursorMarkStaleAfterSiblingPut`.
  No spec-tier invariant whose `Lands:` resolved to a chunk-5
  sub-chunk was left in spec-only form.
  *Open issues post-chunk-5* (8 entries in `docs/issues/`):
  `setkeyspace-put-added-bool` (Lands: 6),
  `bitmap-rollback-undo-log` (profiling-driven),
  `tx-rebuildindex-missing-name-behavior` (Lands: 7),
  `pager-test-helper-export` (condition: 2nd cross-package
  writer-pager fixture caller — chunk 5 added no second caller,
  remains deferred),
  `leaked-readtx-cleanup-race-flake` (condition),
  `spec-numkeyspaces-semantics` (Lands: 7),
  `btree-branch-page-validation` (opportunistic),
  `btree-post-merge-underflow` (Lands: invariant #3 fill-ratio
  enforcement test). None fire on chunk-6 entry; the chunk-6
  chunk-start gate runs the next triage.
  Chunk 5 complete; suite + `-race` green across all 6 packages.

Primary specs: `keyspaces.md` (`Kind = 0` parts), `range-
delete.md`.

Primary files: `db.go`, `btree.go`, `tx.go`,
`internal/pager/*.go` (5.2), `keyspace.go` (5.4 onward).

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

Sub-chunk plan (9 sub-chunks; chunk-5 8-sub-chunk pattern + 1
because subpage codec, leaf integration, and promotion are three
structurally distinct pieces that need independent adversarial
review):

- **6.1** ✅ Chunk-start gate: triage (1 entry matched `Lands: 6`:
  `setkeyspace-put-added-bool`, folded as user-locked). Project-
  invariants trigger derived 4 entailed invariants for SetKeyspace
  (nested-cell Count equality, desc.Count accounting,
  promotion/demotion atomicity bounded to same-tx, SetCursor
  NextValue key-boundary) appended to `set-keyspace.md §Invariants`.
  User-locked `SetKeyspace.Put(key, value []byte) (added bool, err
  error)` + spec amendments to `api-surface.md` (new
  `### SetKeyspace API` sub-heading, new `ErrFixedValueSizeMismatch`
  sentinel, `CreateSetKeyspaceIfNotExists` opts-conflict semantics)
  + the typed mirror in `typed-keyspaces.md`. Adversarial review
  2 rounds, 0H 1M (R1 cite error) → 0H 0M.
- **6.2** ✅ Subpage codec: new `internal/page/subpage.go` (550
  lines) — variable + fixed-size encode/decode/Insert/Delete/
  Search with uint16 overflow guards; `EncodeSubpage` builder;
  `SubpagePromotionThreshold` helper. 38 unit tests. Spec
  amendment in `set-keyspace.md §Subpage Format` documenting the
  fixed=binary / variable=linear search trade-off (density >
  log-N for variable-size). Adversarial 2 rounds, severity
  trending down (1M → 0M).
- **6.3** ✅ Leaf integration: `LeafBuilder.AddSubpage` +
  extended `AddEntry` dispatch on `CellFlagMultiValue && !NestedTree`;
  retired the chunk-3 panic on MultiValue cells. `validateCellFlagsCombo`
  helper centralises Validate's flag-pair rejection so the chunk-4
  leaf-rebuild path now carries SetKeyspace subpage cells through
  every split / merge / rebuild without silent demotion. 12
  page-layer tests. `set-keyspace.md §Subpage Format` diagram
  amended to show the standard inline `ValueLen u32` prefix
  explicitly (pre-existing spec/code divergence surfaced by the
  first writer of subpage cells). Adversarial 2 rounds, 1M
  (Validate trust boundary) fixed in-place.
- **6.4** ✅ Nested-tree promotion: `LeafEntry.NestedRoot` +
  `NestedCount` fields + IsSubpage / IsNestedTree helpers;
  decoder + encoder + validator branches share the 16-byte
  trailer wire format with overflow but populate distinct
  fields; `LeafBuilder.AddNestedTreeRef` retires chunk-6.3's
  NestedTree panic. New `internal/btree/subpage_promotion.go`
  with `PromoteSubpageToNestedTree` (4-step algorithm; caller
  installs the new cell via `AddNestedTreeRef`). Atomicity
  invariant E3 enforced by 3 fault-injection tests using a
  `failingFakeWriter` wrapper. 10 page-layer + 10 btree-layer
  tests. Adversarial 2 rounds, 1M (E3 unexercised) fixed.
- **6.5** ✅ Demotion + per-key bulk-free + FreeSubtree
  extension: `DemoteNestedTreeIfFits` (single-leaf-fits-as-
  subpage detection + leaf retire); `FreeSubtree` extended to
  recurse into NestedTree cells + add subpage Count to the
  returned value (count semantic redefined to "user-visible
  values freed"; Kind=0 trees unchanged). Closes the chunk-5.6
  inheritance gap re: SetKeyspace nested-tree pages not walked
  during DeleteKeyspace. 11 new tests. Adversarial 1 round,
  0H 1M (ErrSubpageValueSize wrap as ErrCorrupted) fixed.
- **6.6** ✅ SetKeyspace public surface: `SetKeyspaceOptions`,
  `Tx.OpenSetKeyspace` / `OpenSetKeyspaceReadOnly` /
  `CreateSetKeyspace` / `CreateSetKeyspaceIfNotExists`; per-name
  cache `openSetKeyspaces` + flush Step 2b in
  `flushKeyspaces`; `Has` / `HasValue` / `Put` (added bool, err
  error) / `Delete` / `DeleteValue` / `CountValues`. New btree
  primitives `GetEntry` + `PutEntry` (return / install
  arbitrary cell). DeleteKeyspace + lookupDescriptor +
  ListKeyspaces + SetKeyspaceConfig extended to handle
  Kind=1. Round-1 H finding: SetKeyspaceConfig silently no-op'd
  on same-tx-created SetKeyspaces (keyspaces.md inv #6
  violation) — fixed in-place with regression test. 32 tests.
- **6.7** ✅ `SetCursor`: core nav + intra-key value nav with
  E4 enforcement (NextValue / PrevValue do not cross key
  boundaries). Materialization strategy (per-key transition
  decodes the cell into a `values [][]byte` slice); two-tier
  `requireOpen` / `requireFresh` permission gates separate
  re-positioning ops from non-repositioning ops so the stale
  flag clears via `clearPosition`. Wired
  `markSetCursorsStale` into 5 SetKeyspace mutation paths
  (3 nested-if-block sites that chunk-6.6's replace_all
  missed). 25 tests. Adversarial 1 round, 0H 2M (1
  introduced + fixed, 1 adjacent — filed
  `cursor-err-unpositioned-state.md`).
- **6.8** ✅ `SetKeyspace.DeleteRange`: snapshot-keys-in-range
  via read cursor + per-key Delete loop. User-locked
  partial-progress contract (chunk-6.8): returns
  `(deleted_so_far, err)` on per-key error — unlike chunk-5.7
  Keyspace.DeleteRange which is atomic — so caller sees real
  scope of in-memory state change. Spec amendment in
  `api-surface.md §SetKeyspace API DeleteRange` documents the
  contract. 11 tests. Perf follow-up filed
  (`setkeyspace-delete-range-bulk-walker.md`) — replace
  O(K log N) loop with O(K + log N) adaptation of the
  chunk-5.7 walker.
- **6.9** ✅ Close-out: cite sweep, spec-tier invariant audit,
  delete resolved issues.
  *Cite sweep (Issue-triage gate 2).* Wrap-aware grep across
  `docs/specs/*.md`, `docs/plans/*.md`, and every `*.go` for
  references to `setkeyspace-put-added-bool` (chunk-6.1
  user-locked, resolved at 6.1). The load-bearing rationale was
  promoted inline at chunk 6.1 into `api-surface.md §SetKeyspace
  API` (Put godoc carrying the "membership probe is already paid
  by the insert" reasoning + the chunk-6.1 attribution) and
  `typed-keyspaces.md §TypedSetKS API` (Put mirror godoc citing
  api-surface.md for the rationale). No production-code or
  authoritative-spec cites of the issue path remain — the only
  remaining references are this plan-doc's own historical audit
  trail (chunk-5.8 open-issues enumeration at line 915 and
  this entry's 6.1 plan summary), which are not subject to the
  no-cite invariant (plan-doc is the implementation roadmap,
  not the authoritative spec).
  *Delete.* `docs/issues/setkeyspace-put-added-bool.md` +
  `docs/issues/README.md` row removed; git history preserved
  via `git log --all -- docs/issues/setkeyspace-put-added-bool.md`.
  *Spec-tier invariant audit.* Every chunk-6.1 enforcement-
  schedule item verified landed:
    - `keyspaces.md` #2 / #3 / #5 (Kind=1 parts) → 6.6
      `TestOpenKeyspaceOnSetKeyspaceReturnsKindMismatch` +
      `TestOpenSetKeyspaceOnKeyspaceReturnsKindMismatch` +
      `TestCreateSetKeyspaceFixedValueSize` +
      `TestCreateSetKeyspaceIfNotExistsMismatchedFixedValueSize` +
      `TestSetKeyspacePutFixedValueSizeMismatch` +
      `TestSetKeyspaceDeleteValueFixedValueSizeMismatch`.
    - Delete-on-miss invariant (SetKeyspace portion) → 6.6
      `TestSetKeyspaceDeleteMissingReturnsErrNotFound` +
      `TestSetKeyspaceDeleteValueMissingKey` +
      `TestSetKeyspaceDeleteValueMissingValueInSubpage`.
    - `set-keyspace.md` Inv-1 (empty sets do not persist) → 6.6
      `TestSetKeyspaceDeleteValueLastValueRemovesParentCell` +
      6.7 `TestSetCursorDeleteLastValueAdvancesToNextKey` +
      `TestSetKeyspaceDeleteValueNestedTreeDropsOnZeroCount`.
    - `set-keyspace.md` Inv-2 (sorted-order subpage + nested) →
      6.2 codec tests (`TestSearchVariableSizeMiss…`,
      `TestSearchFixedSizeBinarySearch`,
      `TestEncodeSubpageRejectsOutOfOrder`) + 6.4 promotion
      preserves order tests.
    - `set-keyspace.md` Inv-3 (FixedValueSize stride + no
      per-value prefix) → 6.2 codec tests + 6.6 Put /
      DeleteValue wrong-length rejection.
    - `set-keyspace.md` Inv-4 (50% promotion threshold) → 6.2
      `TestSubpagePromotionThresholdValues` + 6.6
      `TestSetKeyspacePutTriggersPromotion`.
    - `set-keyspace.md` Inv-5 (demotion to single
      subpage-fitting leaf) → 6.5 `TestDemoteNestedTree*` (6
      tests) + 6.6
      `TestSetKeyspaceDeleteValueFromNestedTreeTriggersDemotion`.
    - `set-keyspace.md` Inv-6 (compound-PK separator) — chunk-7
      indexing scope, deferred (recorded but not enforced;
      lands when first SetKeyspace index writer exists).
    - `set-keyspace.md` E1 (nested-cell Count = leaf entries) →
      6.4 `TestPromoteSubpageBasicVariableSize` +
      `TestPromoteSubpageMatchesEncoderOutput` + 6.6 Delete
      sanity-check (`freed != e.NestedCount → ErrCorrupted`)
      + 6.6 `TestSetKeyspaceCommitReopenWithPromotedNestedTree`.
    - `set-keyspace.md` E2 (desc.Count = sum of values) → 6.6
      `TestSetKeyspaceDescCountAcrossMutations` + 6.8
      `TestSetKeyspaceDeleteRangeMixedCellTypes`.
    - `set-keyspace.md` E3 (promotion/demotion atomicity) → 6.4
      `TestPromoteSubpageAtomicity*` (3 fault-injection tests
      via `failingFakeWriter`); demotion side exercised
      transitively via 6.6 DeleteValue tests (function-internal
      step is atomic; multi-step caller atomicity documented in
      `DemoteNestedTreeIfFits` godoc and SetKeyspace surface).
    - `set-keyspace.md` E4 (SetCursor.NextValue does not cross
      key boundaries) → 6.7 `TestSetCursorNextValueDoesNotCrossKeys`
      + `TestSetCursorPrevValueDoesNotCrossKeys`.
  No spec-tier invariant whose `Lands:` resolved to a chunk-6
  sub-chunk was left in spec-only form.
  *Open issues post-chunk-6* (10 entries in `docs/issues/`):
  `bitmap-rollback-undo-log` (profiling-driven),
  `tx-rebuildindex-missing-name-behavior` (Lands: 7),
  `pager-test-helper-export` (condition; chunk 6 added no
  cross-package writer-pager fixture caller, remains
  deferred),
  `leaked-readtx-cleanup-race-flake` (condition),
  `spec-numkeyspaces-semantics` (Lands: 7),
  `btree-branch-page-validation` (opportunistic),
  `btree-post-merge-underflow` (Lands: when invariant #3
  fill-ratio enforcement test is added),
  `setkeyspace-put-redundant-membership-probe` (Lands:
  opportunistic — chunk-6.6 surfaced the chunk-6.1
  "no-cost" claim's gap on the nested-tree-cell path),
  `cursor-err-unpositioned-state` (Lands: opportunistic —
  chunk-6.7 surfaced a chunk-5 shared spec/impl divergence),
  `setkeyspace-delete-range-bulk-walker` (Lands:
  opportunistic — chunk-6.8 perf follow-up). None fire on
  chunk-7 entry; the chunk-7 chunk-start gate runs the next
  triage.
  Chunk 6 complete; suite + `-race` green across all 6 packages.

Chunk-5-precedent deferrals carried forward at chunk-6
(no explicit issue doc filed; chunk-5 set the same precedent for
`Keyspace`): `SetKeyspace.NextSequence`, `SetKeyspace.Stats`,
`SetKeyspace.All / Range / Prefix` `iter.Seq2` helpers. (Lands:
when corresponding `Keyspace.*` lands.) `SetKeyspace.Index` is
chunk 7. `SetKeyspace.BulkLoad` is chunk 8 (covered in §Chunk 8
scope alongside `Keyspace.BulkLoad`).

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

Primary files: `index.go`, `index_types.go`, `index_codec.go`,
`keyspace.go`, `set_keyspace.go`, `tx.go`, `errors.go`.

**Sub-chunk roster.**

- **7.1** Triage + invariants + spec amendments (no code).
  Folds `tx-rebuildindex-missing-name-behavior.md` (locked:
  `ErrNotFound` for keyspace-missing + `ErrIndexNotFound` for
  decl.Name-missing) and `spec-numkeyspaces-semantics.md`
  (locked: `NumKeyspaces` = keyspace-B+tree leaf count incl
  `Kind = 2`; user-visible count via cursor walk). Records
  amendments inline in `api-surface.md` (Tx.RebuildIndex /
  Tx.DropIndex godoc), `keyspaces.md` (new clause-explicit
  invariant), `file-layout.md` (meta-page NumKeyspaces field
  comment), `indexing.md` (two new entailed invariants:
  Kind=2 one-parent reachability uniqueness; empty-registry
  representation canonical at IndexRegistryRoot=0). Deletes
  both folded issue files + their README rows. Re-defers
  conditional candidates: `pager-test-helper-export` (no 2nd
  caller in chunk-7 scope), `setkeyspace-put-redundant-
  membership-probe` (assess at 7.6), `setkeyspace-delete-
  range-bulk-walker` (assess at 7.10).
- **7.2** Public types + sentinels + schema-hash. `errors.go`
  ErrIndex* sentinels (8 sentinels per `api-surface.md
  §Sentinel errors`). New `index_types.go`: `IndexDecl`,
  `IndexColumn`, `CoveringColumn`, `IndexEntry`,
  `IndexExtractor`, `IndexFingerprintError` (with Error +
  Unwrap). Schema-hash function (xxhash64 over uvarint-
  prefixed byte sequences per `indexing.md §Drift Guard`).
  Duplicate-Name validation in the variadic IndexDecl slice
  → `ErrIndexExists`. No on-disk surface; no I/O. Tests
  cover hash determinism + duplicate-name rejection.
- **7.3** Index registry codec + sub-tree wiring. Internal
  registry-entry struct + binary codec per `indexing.md
  §Storage Layout`. Per-keyspace registry B+tree
  (Kind=0-shaped) rooted at `desc.IndexRegistryRoot`:
  allocate on first index, grow/shrink, dirty-flush via
  existing `tx.dirtyDescriptors` machinery. CRUD helpers
  (`registryGet`/`registryPut`/`registryDelete`/
  `registryList`). No public surface yet; tests via
  internal helpers.
- **7.4** NUL-escape composite-key codec for index keys.
  Encode/decode per `page-formats.md §NUL-escape encoding` +
  `indexing.md §Column Encoding`. Property tests covering
  prefix-freeness (clause-explicit invariant from
  page-formats.md).
- **7.5** Open/Create with IndexDecl + Kind=2 enforcement.
  Variadic IndexDecl plumbed through `Tx.OpenKeyspace` /
  `Tx.CreateKeyspace` / `Tx.CreateKeyspaceIfNotExists` + the
  SetKeyspace mirrors. Open-time validation per `indexing.md
  §Index Declaration`: name set match +
  `ErrIndexExtractorRequired` (missing decl) /
  `ErrIndexUnknown` (extra decl) /
  `ErrIndexFingerprintMismatch` (drift, wrapped in
  `IndexFingerprintError` naming Field + IndexName + Stored*
  + Supplied*). Same-tx re-open idempotence per `indexing.md
  §Re-opening`: chunk-6.6's `openKeyspaces` /
  `openSetKeyspaces` caches extended; first-Extract-wins.
  Kind=2 enforcement: existing `ErrKeyspaceReserved` from
  chunks 5.4 / 6.6 verified against **real** registry-created
  Kind=2 entries (not just forged ones) — extends the existing
  chunk-5.4 `TestListKeyspacesFiltersKindIndexInternal`
  forge-test coverage to the production code path. Spec-tier
  promotion: the new `indexing.md §Invariants` entailed
  invariant on Kind=2 one-parent-reachability uniqueness
  (added at chunk-7.1) lands enforced here — tests verify a
  registry-created Kind=2 root has exactly one parent
  keyspace's registry pointing at it.
- **7.6** Atomic Put/Delete + unique probes (Keyspace).
  Wraps `Keyspace.Put` / `Delete` / `Cursor.Delete` with the
  per-`indexing.md §Write Path: Atomic Index Maintenance`
  sequence: read existing value → extract(old) →
  extract(new) → diff → unique-probe candidate-set + index
  → apply deletes → apply inserts → write row → bump
  per-index Count in registry. IndexEntry collision
  detection within a single extractor invocation (entailed
  set-not-multiset semantics). All steps in one CoW tx.
  `Keyspace.Index(name)` returns Index handle (Stats / Err
  working; Lookup surface stubbed → 7.7). Assesses
  `setkeyspace-put-redundant-membership-probe.md` for fold
  if an atomic-Put primitive that fuses the existing-value
  read with the row write materializes.
- **7.7** Index lookup API (Keyspace). `Index.Lookup`
  (`iter.Seq2` with back-lookup + covering decode).
  `Index.LookupKeys` (`iter.Seq`, no back-lookup).
  `Index.Range`, `Index.Prefix`. `Index.Get` (unique-only;
  `ErrIndexNotUnique` on non-unique). Per-handle `Err`
  state. `Index.Stats`. Silent-skip of missing-PK
  back-lookups per `indexing.md §Lookup API`.
- **7.8** `RebuildIndex` + `DropIndex` + three-subtree
  retirement (Keyspace). `Tx.RebuildIndex`: allocate fresh
  Kind=2 keyspace, cursor-walk parent, write fresh entries,
  swap registry-entry Root+SchemaHash+Version, retire old
  Kind=2 root via FreeSubtree. Honors `ErrNotFound`
  (keyspace) / `ErrIndexNotFound` (name) per chunk-7.1.
  `Tx.DropIndex`: retire Kind=2 root + remove registry
  entry + reset `desc.IndexRegistryRoot = 0` if registry
  becomes empty (retiring the registry sub-tree too). Three-
  subtree retirement in `Keyspace.DeleteKeyspace`: replaces
  the chunk-5.6 defensive `desc.IndexRegistryRoot != 0 →
  ErrCorrupted` gate with the actual retirement sequence per
  `api-surface.md §DeleteKeyspace`. Spec-tier promotion: the
  new `indexing.md §Invariants` entailed invariant on
  empty-registry-canonical-at-zero (added at chunk-7.1)
  lands enforced here — tests verify that DropIndex of the
  last index resets `IndexRegistryRoot` to 0 and retires
  the registry sub-tree pages.
- **7.9** SetKeyspace indexing. Compound-PK encoding
  (`escape(setKey) 0x00 0x01 escape(setValue)`) per
  `set-keyspace.md §Indexes on SetKeyspaces`. Atomic
  per-`(key, value)` extractor invocation in
  `SetKeyspace.Put` / `DeleteValue` / `SetCursor.Delete`;
  bulk-key `Delete` walks every set member when indexes
  present (per `indexing.md §Indexes on SetKeyspaces`).
  `SetKeyspace.Index(name)` handle. Spec-tier promotion:
  `set-keyspace.md` Inv-6 (compound-PK separator
  prefix-freeness) lands enforced here.
- **7.10** SetKeyspace retire+rebuild+drop + indexed
  DeleteRange fallback. Three-subtree retirement in
  `SetKeyspace.DeleteKeyspace` (mirror 7.8). `RebuildIndex`
  + `DropIndex` on SetKeyspace. Indexed-keyspace
  `DeleteRange` fallback (per-row walk per
  `range-delete.md §Indexed-keyspace fallback`) for
  Keyspace + SetKeyspace. Per-row Delete invokes the
  atomic-Delete path from 7.6 / 7.9. Assesses
  `setkeyspace-delete-range-bulk-walker.md` for fold if a
  unified bulk walker materializes.
- **7.11** ✅ Close-out: cite sweep, spec-tier invariant audit,
  no resolved issues remaining at close-out (the chunk-7.1 pair
  was deleted at 7.1 itself; all other chunk-7-touched issues
  are either filed-and-open with conditional `Lands:` triggers
  or were partial-folded with sub-items remaining open).
  *Cite sweep (Issue-triage gate 2).* Wrap-aware
  `git grep -nE "docs/issues/" -- 'docs/specs/*.md' '*.go'`
  excluding test files. Promoted inline + stripped 4 production-
  code cites that violated the no-cite invariant: `index_maintain.go:162,420`
  (writenewindexregistry-partial-leak refs → "engine's rest-of-tx-
  continues contract"), `index_types.go:217` (zero-column
  decoder rationale promoted inline), `set_keyspace.go:1046`
  (setkeyspace-delete-range-bulk-walker ref → "perf-driven
  follow-up"), `set_cursor.go:38` (setcursor-lazy-value-iteration
  hypothetical ref → "perf-driven follow-up"). The single
  remaining `docs/issues/` mention in production-doc-tier text
  is `overview.md:26` ("follow-ups live under `docs/issues/`")
  which is a project-organization meta-reference, not a
  specific-issue cite; kept.
  *Spec-tier invariant audit.* Every chunk-7 enforcement-schedule
  item verified landed:
    - `keyspaces.md` #4 (Kind=2 ErrKeyspaceReserved + ListKeyspaces
      filter, real-registry path portion) → 7.5
      `TestCreateKeyspaceWithIndexDoesNotPolluteListKeyspaces`
      + chunk-5.4 forge test
      `TestListKeyspacesFiltersKindIndexInternal` (verified
      against real-registry-created Kind=2 — the entries don't
      enter the top-level keyspace B+tree per the chunk-7.1
      design clarification).
    - `keyspaces.md` #7 entailed (IndexRegistryRoot=0 iff no
      indexes) → 7.3 `TestRegistryDeleteLastResetsRootToZero`
      + 7.8 `TestDropIndexLastResetsIndexRegistryRoot`
      + 7.10 `TestDropIndexOnSetKeyspaceSucceeds`.
    - `indexing.md` §Invariants (added at chunk 7.1) — Kind=2
      one-parent-reachability uniqueness: structurally guaranteed
      by per-keyspace IndexRegistryRoot allocation; "no top-level
      pollution" direction tested at 7.5; "exactly one parent"
      direction filed as `kind2-one-parent-reachability-test.md`
      (Lands: 7.8 originally, redeferred to track with the
      `index-handle-stale-after-rebuild-drop` Round at chunk 11).
    - `indexing.md` §Invariants (added at chunk 7.1) — DropIndex
      atomic empty-registry-canonical-at-zero: tested at 7.8 +
      7.10 (see above).
    - `indexing.md` schema-hash determinism (clause-explicit) →
      7.2 `TestSchemaHash*` (10 sensitivity tests + length-prefix
      disambiguation + Name-prefix-prevents-collision regression).
    - `indexing.md` atomic Put/Delete (clause-explicit) → 7.6
      `TestIndexedPut*` / `TestIndexedDelete*` / 7.9
      `TestSetKeyspaceIndexedPut*` / `TestSetKeyspaceIndexedDelete*`
      + chunk-7.6/7.9 snapshot-restore atomicity tests
      (`TestIndexedPutPinnedStateRevertsOnCandidateCollision`).
    - `indexing.md` unique-index uniqueness (clause-explicit) →
      7.6 `TestIndexedPutUniqueViolationOnDiskConflict` +
      `TestIndexedPutUniqueViolationOnCandidateSetCollision` +
      7.9 `TestSetKeyspaceIndexedPutUniqueViolation` + 7.8
      `TestRebuildIndexUniqueViolationFailsCleanly` + 7.10
      `TestRebuildIndexOnSetKeyspaceUniqueViolation`.
    - `indexing.md` ErrIndexFingerprintMismatch wrap discipline
      (clause-explicit) → 7.5 `TestOpenKeyspaceSchemaHashMismatch`
      + `TestOpenKeyspaceVersionMismatch` + 7.2 `TestIndexFingerprintError*`.
    - `set-keyspace.md` Inv-6 (compound-PK separator
      prefix-freeness) → 7.9
      `TestSetKeyspaceCompoundPKSeparatorPrefixFree`.
    - `page-formats.md` §Invariants clause-explicit (NUL-escape
      prefix-freeness) + entailed (added at chunk-7.4 spec-amend:
      fixed-column-count tuple-prefix-freeness) → 7.4
      `TestEncodedColumnPrefixFreeness` + property test +
      `TestEncodedTuplePrefixFreenessSameColumnCount` + property test.
    - `keyspaces.md` NumKeyspaces clause-explicit (chunk-7.1
      spec-amend) → no new enforcement needed; the amendment
      locks chunk-5.4's existing implementation as spec-correct;
      `TestListKeyspacesFiltersKindIndexInternal` continues to
      verify the user-visible-count derivation works via cursor
      walk + `Kind != 2` filter.
  No spec-tier invariant whose `Lands:` resolved to a chunk-7
  sub-chunk was left in spec-only form.
  *Spec amendments landed across chunk 7.* `api-surface.md`
  Tx.RebuildIndex / Tx.DropIndex godoc (ErrNotFound +
  ErrIndexNotFound sentinels, chunk-7.1); `api-surface.md`
  §Index Lookup API godoc (Range partial-tuple prefix-bound
  semantics + Lookup/LookupKeys exact-cols requirement,
  chunk-7.7); `keyspaces.md` NumKeyspaces invariant (chunk-7.1);
  `indexing.md` schema-hash Name-prefix + 2 entailed invariants
  (chunk-7.1/7.2); `set-keyspace.md` Inv-6 promotion (chunk 7.9);
  `page-formats.md` tuple-prefix-freeness entailed (chunk 7.4);
  `range-delete.md` §Indexed-keyspace fallback +
  Cursor-Based Range Delete (Current vs Next correction +
  partial-progress contract, chunk 7.10); `transactions.md`
  §Cursor.Delete() post-delete state + the kind=entailed
  invariant property= rewording (chunk 7.10 M-A).
  *Open issues post-chunk-7* (13 entries in `docs/issues/`):
  none new opened at 7.11; chunk-7 contributed 4 new files
  (`index-handle-stale-after-rebuild-drop`,
  `index-registry-decoder-bounds`,
  `kind2-one-parent-reachability-test`,
  `setkeyspace-indexing-perf-and-edge`,
  `writenewindexregistry-partial-leak`) — all open with
  conditional `Lands:` triggers (chunk 11 / profiling-driven /
  concurrent-iteration safety hardening). Pre-chunk-7 carried
  forward: `bitmap-rollback-undo-log`,
  `pager-test-helper-export`, `leaked-readtx-cleanup-race-flake`,
  `btree-branch-page-validation`, `btree-post-merge-underflow`,
  `setkeyspace-put-redundant-membership-probe`,
  `cursor-err-unpositioned-state`,
  `setkeyspace-delete-range-bulk-walker`.
  Chunk 7 complete; suite + `-race` green across all 6 packages.

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

**Sub-chunk roster (as landed).**

- **8.1** Triage + invariants (no separate commit; folded into
  8.2). No `docs/issues/` entry resolved to chunk 8.
- **8.2** `pager.WriteDirect` slab-bypass primitive (Inv-WD:
  pendingAllocs-only, never in the slab, checksum parity,
  reversed by `AbortTx`'s bitmap snapshot), `Options.ScratchDir`
  (default `os.TempDir`), and the `ErrBulkLoadOutOfOrder` /
  `ErrBulkLoadNonEmpty` sentinels. Files: `internal/pager/
  pager.go`, `options.go`, `errors.go`.
- **8.3** Bottom-up B+tree bulk builder (`bulkBuilder`): leaves →
  branches → root over `WriteDirect`, O(depth × pageSize) memory,
  prefix-truncated leaf separators, bubbled branch separators
  (Inv-Builder routing contract). File: `bulkload.go`.
- **8.4** `Keyspace.BulkLoad` (non-indexed) + streaming
  slab-bypass overflow-chain writer (byte-identical to
  `page.EncodeOverflowRun`, O(pageSize)); exported
  `btree.NeedsOverflow` / `OverflowRefFitsLeaf` so the
  inline/overflow boundary is shared with `Put`.
- **8.5** `SetKeyspace.BulkLoad` (non-indexed): per-key subpage /
  nested-tree storage matching `Put`'s shape via a streaming
  incremental-promotion accumulator (no full-set buffering);
  adjacent-dedup. Also fixed a latent `iter.Seq2` key-reuse bug
  in the builder (the `page.LeafBuilder` order-assertion borrows
  the caller's key slice).
- **8.6** Indexed `BulkLoad` for both `Keyspace` and
  `SetKeyspace` (`bulkload_indexed.go`): per-row/member extractor
  → per-index external sort (`indexSorter`) with `ScratchDir`
  disk-spill bounded by `MaxTxBufferBytes` (divided across
  indexes), k-way `sortMerger`, unique-violation detection at the
  sorted output, bottom-up index-tree build, all-or-nothing
  in-process publish. Shared row-stream drivers `bulkLoadRows` /
  `bulkLoadStream` factor the row side from the non-indexed
  paths. Filed `bulkload-index-merge-run-fanin.md` (L-2:
  single-pass merge fan-in; profiling-driven cascaded-merge fix).
- **8.7** Close-out: cite sweep (no-cite invariant holds — no
  production/spec cite of a tracking artifact); spec sync to
  `bulkload.md` (aggregate sort-budget wording; the all-or-nothing
  in-process publish recorded as the entailed invariant; the
  bounded-leakage invariant refined to distinguish a clean
  rollback (no leak, reusable) from commit-after-error / crash
  (bounded leak)) and the `AbortTx` godoc (WriteDirect pages
  revert to free on rollback). All `bulkload.md` §Invariants now
  enforced by tests.

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

**Sub-chunk roster (as landed).**

- **9.1** Triage + invariants (no separate commit; folded into
  9.2). No `docs/issues/` entry resolved to chunk 9. Invariants
  T1–T8 (encoder lex order, ID stability/uniqueness/immutability,
  empty-ID rejection, sealing, round-trip, encoder-ID drift,
  extractor wiring) derived and encoded as tests.
- **9.2** `Encoder[T]` + `FuncEncoder[T]` + the canonical encoders
  (string, bytes, be-uint64/32, be-int64/32 sign-bit-XOR,
  be-time-nanos, uuid-v4/v7). Lex-order + round-trip + golden-ID
  tests; BENanos rejects out-of-range times. File:
  `typed_encoder.go`.
- **9.3+9.4** `TypedKeyspace[K,V]` / `TypedKS[K,V]`
  (Get/Put/Delete/DeleteRange) + `TypedCursor[K,V]` +
  All/Range/Prefix iterators, delegating to the byte
  Keyspace/Cursor through the encoders. `encodeBound` preserves
  the open-vs-real-bound distinction. File: `typed.go`.
- **9.5** `TypedSetKeyspace`/`TypedSetKS`/`TypedSetCursor`
  (member-level cursor; value-level intra-key navigation
  deliberately omitted — the key is an unambiguous end sentinel
  where an empty-bytes set value would not be). File:
  `typed_set.go`.
- **9.6a** `TypedIndex[K,V,IK]` core + sealed `AnyTypedIndex`
  (unexported `indexDecl`) + schema-hash IK-encoder-ID folding
  (Inv-T7: column name = `IKEnc.ID()`) + `ErrIndexEncoderIDEmpty`
  + `TypedIndexHandle`/`NewTypedIndexQuery`/`TypedIndexQuery`
  (Lookup/LookupKeys/Range/Prefix/Get/Err). The extractor closure
  panics on a decode/encode failure (the byte IndexExtractor is
  infallible) — reviewer-verified to run before any pinned-index
  mutation. File: `typed_index.go`.
- **9.6b** Typed full-row covering (`TypedIndex.CoverValue`) +
  byte-layer covering-return (gated `Index.coverValue`, enabled
  only for the recognized typed sentinel; default unchanged
  back-lookup). Value-encoder ID folded into the fingerprint.
  Spec amended (`typed-keyspaces.md §Covering`). Filed
  `byte-api-covering-return-unwired.md` (M-1, adjacent: byte-API
  projection-covering-return unwired; spec-amend candidate).
  Files: `index.go`, `typed_index.go`.
- **9.7** Close-out: no-cite sweep (fixed a spec→issue-doc cite
  introduced in 9.6b; invariant now holds), spec-tier invariant
  audit (none pending), api-surface.md confirmed in sync, plan
  roster. All typed-keyspaces.md §Invariants enforced by tests.

### Chunk 10 — Batch + nested transactions

**Scope.** Channel-based batch coordinator, per-closure child
transactions with exactly-once invocation, child-commit /
rollback semantics (no buffer-content restoration), nested
arbitrary depth. Target: `Batch()` amortises commit cost across
N concurrent callers; failing closures don't affect siblings;
a final-commit failure surfaces uniformly.

Primary specs: `transactions.md §Write Batching` and §Nested
Transactions, `api-surface.md`.

Primary files: `db.go`, `tx.go`, `nested.go`, `batch.go`,
`internal/pager/savepoint.go`.

**Sub-chunk roster (as landed).**

- **10.1** Triage + invariants + design decisions (no separate
  commit; folded into 10.2). Chunk-start gate: **0** `Lands: 10`
  README entries; two condition-triggered entries redeferred —
  `bitmap-rollback-undo-log` (chunk 10 *amplifies* it: each
  BeginChild adds a bitmap snapshot, but the trigger is still
  profiling) and `writenewindexregistry-partial-leak` (chunk 10
  changes loose-page *reuse*, not error-recovery, policy).
  User-locked decisions: (1) **parent-freeze** — a tx with an
  unresolved child/descendant is frozen (`ErrChildActive`), LMDB
  model; (2) a panicking `Batch` closure fails just that closure
  (recover → rollback child → continue siblings). Invariants
  N1–N5 derived (N1/N3/N5 clause-explicit, N2/N4 entailed),
  encoded as tests at their introducing sub-chunk.
- **10.2** Nested transactions. Pager savepoint primitive
  (`internal/pager/savepoint.go`: BeginSavepoint / RestoreSavepoint
  / ReleaseSavepoint, full freespace snapshot + loose-pop
  suspension while a savepoint is active — Inv-N1). `Tx.BeginChild`
  + child commit/rollback (`nested.go`): shared pager + grant,
  keyspace-state clone at begin and merge-by-name at commit
  (parent handles updated in place / fresh handles for child-
  created / dead for child-deleted), parent-freeze in
  `requireOpen` (Inv-N5), transient `ErrChildActive` in cursors.
  Adversarial review (2 rounds): M-1 (child-handle lifecycle
  asymmetry) fixed — child handles are tx-scoped, never promoted;
  L-1 documented; L-2/L-3 indexed-child coverage added. Commit
  `f440383`.
- **10.3** `DB.Batch` coordinator (`batch.go`): channel-based, lazy
  start, collect to MaxBatchSize / MaxBatchDelay, each closure in a
  child tx (exactly-once), panic-recovery → `ErrBatchClosurePanic`,
  per-caller ctx skip, parent commit → uniform result. Coordinator
  lifecycle stopped by `Close` (cancel coordinator context + join).
  `Options.MaxBatchSize`/`MaxBatchDelay` + validation. Adversarial
  review (2 rounds): H-1 (a closure leaving a nested child open
  froze the whole batch) fixed via `cascadeRollback`; M-1 (grant
  leak on Close-race) fixed; L-1 (option validation) fixed; all
  coverage gaps closed (size-cap + delay-coalescing via synctest,
  parent-commit-failure via the step-4 hook). Commit `9c86d26`.
- **10.4** Close-out: no-cite sweep (fixed a `savepoint.go` →
  issue-doc cite introduced in 10.2), spec updates
  (`transactions.md` §Nested Transactions parent-freeze +
  savepoint mechanism + handle lifetime + Inv-N5; §Write Batching
  panic/coordinator-context/lifecycle; `api-surface.md`
  `ErrChildActive` + `ErrBatchClosurePanic` + BeginChild/Batch
  godocs), plan roster. Invariants N1–N5 all enforced by tests.

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

**Sub-chunk roster (as landed).**

- **11.1** Triage + invariants. Folded `btree-branch-page-
  validation` and `index-registry-decoder-bounds` (both
  `Lands:` chunk 11) into the chunk plan; derived the Check
  invariants Inv-C1..C5.
- **11.2** `Check()` structural integrity walk — `iter.Seq[
  CheckIssue]`, `btree.Walk`/`WalkKV`, `page.ValidateBranch`.
  Commit `6cfc6ea`. Its round-3 fresh-eyes pass found a forged
  overflow-`TotalLen` OOM (Inv-C1 class); folded into 11.3d.
- **11.3** Read-path corruption tolerance. Mid-implementation
  discovery: `checksums.md §Verification` (every page read
  verified on first access ⇒ `ErrBadPageChecksum`, cached) was
  a clause-explicit spec requirement that was entirely
  unimplemented, and the read path dereferenced content-derived
  page ids without a file-resident bound (SIGBUS on the
  `[fileSize, MaxSize)` reservation gap). Surfaced to the user;
  scoped as "full corruption tolerance now." Invariants
  Inv-RV1..RV4 derived and enforced by tests.
  - **11.3a+b** `PageReader.Page → ([]byte, error)` plumbing;
    pager verifying+bounding `Page` (RV1/RV2 checksum verify +
    per-tx cache, RV3 file-resident bound). Commit `c00bcf2`.
  - **11.3c+d** `ValidateBranch` wired into every branch
    first-read descent incl. the merge-sibling read (resolves
    `btree-branch-page-validation`); `OverflowRunLength64` +
    incremental `readOverflowValue` + run64 guards fix the 11.2
    forged-`TotalLen` OOM; `decodeRegistryEntry` count
    pre-checks (resolves `index-registry-decoder-bounds`).
    Commit `cd9f445`.
  - **11.3e** Close-out: promoted the two issues' rationale
    inline (`validateBranchPage` / Inv-RV4 comments /
    `registryList` no-cap note), deleted both issue docs +
    README rows (`git log --all -- docs/issues/<f>.md`), synced
    `checksums.md §Structural and Allocation Bounds` +
    `integrity.md`, this roster. Spec-amend candidate (Check's
    reader-slot release is deterministic — defer-on-break —
    stronger than the spec's GC-via-AddCleanup wording) approved
    by the user and applied: commit `9a90581`.
- **11.4a** `CheckWithOptions(CheckIndexes)` — extractor-
  equivalence for plain Keyspaces AND SetKeyspaces (full-entry
  key+value comparison reproducing the exact stored entries; new
  guarded `btree.WalkLeafEntries` for the SetKeyspace outer tree).
  Commit `c51f85f`. 11.4.1 triage redeferred
  `writenewindexregistry-partial-leak` (Check+Repair reclaim its
  leak, but the orphaning root is unchanged) and
  `index-handle-stale-after-rebuild-drop` (11.4 implements no
  concurrent-iteration hardening). Round-2 review filed
  `check-subpage-structural-validation` (adjacent: the STRUCTURAL
  walk does not validate subpage payloads though `api-surface.md
  §Check` claims it does — a spec-vs-code gap).
- **11.4b** ✅ `CheckWithOptions(Repair)` — exclusive leaked-page
  reclamation enforcing Inv-C5. `CheckOptions.Repair` opens a
  WRITE tx (cross-process write lock ⇒ no concurrent writers) and
  gates on `coord.OldestReaderTxnID()==noReader` (⇒ no concurrent
  readers, else CheckError `Repair.ReadersActive`, frees nothing).
  The `checker` was refactored from `rtx *ReadTx` to `pgr
  *pager.Pager` so it runs against either a read or write tx.
  Three Inv-C5 invariants promoted from spec-tier (recorded at
  11.1) to enforced tests: **exclusivity** (clause-explicit),
  **completeness gate** (entailed — `emit` latches `sawError` on
  any CheckError/CheckFatal; Repair frees ONLY when the walk both
  completed and stayed error-free, else reports `Repaired=false` +
  `Repair.Skipped`; the gate prevents freeing live pages left
  unvisited under a walk-aborting corrupt subtree), **atomicity**
  (entailed, by construction — new `pager.FreeLeakedPage` dirties
  only the in-memory bitmap; the sole persistence path is
  `pgr.Commit`'s atomic meta-swap). Reclaimed pages report as
  `BitmapLeak` with `Repaired=true`. Spec synced
  (`api-surface.md §CheckOptions` Repair godoc expanded with the
  refusal codes + conservative gate; `integrity.md §No partial
  writes visible` points to Repair). Adversarial review: ship, 0
  introduced H/M; dispositions — L-1 (no ctx param) disputed
  (adjacent, matches the Check API family), nit-1 (sentinel
  mirror) disputed, nit-2 (tighten corruption test) fixed,
  coverage-gap L (untested Repair fatals) fixed for
  `WriteTxUnavailable` + `CommitFailed` (`FreeFailed` is
  unreachable-by-construction defensive code).
- **11.5a** ✅ `CopyTo(path, compact=false)` — verbatim copy +
  bitmap rebuild from a read snapshot (writers not blocked;
  `copy.go`). Walks the snapshot's reachable set (keyspace tree +
  each data tree incl. nested + overflow + index registry + index
  trees) via the chunk-11.2 guarded `btree.Walk`/`WalkKV`, writes
  each reachable page verbatim at its original id into a fresh
  `O_EXCL` file, REBUILDS the bitmap from the reachable set (so the
  copy's free list is consistent with its tree even under
  concurrent commits, and source leaks are dropped), and writes a
  fresh-UUID meta at TxnID 0 (the post-init tie-at-zero state).
  Reader-slot pinning makes the snapshot's pages stable for the
  copy's duration. Invariants: consistency (clause-explicit —
  bitmap rebuilt, not copied) + standalone-validity (entailed).
  Spec synced (`api-surface.md §CopyTo` godoc promotes fresh-UUID
  / bitmap-rebuild / O_EXCL-no-clobber to contract). `compact=true`
  returns `errCompactCopyPending` (11.5b). Adversarial review:
  ship, 0 introduced H/M; M-1 (round-trip didn't hit nested-tree/
  overflow recursion) + M-2 (concurrent-writer untested) + L-1
  (empty DB) + L-2 (PageChecksum) all fixed with added tests;
  SAC-1 (spec promotion) + SAC-2 (target-survives assertion)
  applied.
- **11.5b** ✅ `CopyTo(path, compact=true)` — per-tree bottom-up
  rebuild into a fresh file with sequential page ids (defragment).
  Each B+tree is rebuilt structurally from its existing entries
  (index trees included — extractors aren't on disk): Keyspace via
  WalkKV → `bulkLeafEntry` → `bulkBuilder`; SetKeyspace via
  WalkLeafEntries → `setBulk` (re-optimised subpage/nested per
  threshold); index registry via WalkKV with each entry's Root
  rewritten to the rebuilt index tree; then a fresh descriptor
  tree. `freshFileWriter` (sequential alloc + WriteDirect) is the
  shared `bulkPageWriter`/`bulkOverflowWriter`. **Approved reuse
  shape (user decision):** decoupled `bulkLeafEntry` from
  `*Keyspace` to a free function taking `bulkOverflowWriter` so
  BulkLoad and compact-copy share ONE overflow encoder; `setBulk`
  reused unchanged. Inv (entailed): rebuild preserves every
  keyspace's logical contents + emits valid storage. Adversarial
  review: ship, 0 introduced H/M; L-1 (registry/index trees now
  use base cfg, matching runtime maintenance) + L-2 (compact
  empty-DB + PageChecksum tests) fixed; SAC-1 (spec wording: ids
  globally gap-free, not per-tree contiguous) applied. Tests:
  compact round-trip (indexed + nested-tree set + overflow,
  CheckIndexes clean), defragments (DeleteRange churn → smaller
  HWM than verbatim + zero free), empty, PageChecksum.
- **11.6** ✅ `Compact()` — in-place defragment via compacting copy +
  atomic rename + pager reopen (`compact.go`). Acquires the
  cross-process write lock, drains in-process readers up to
  `Options.CompactDrainTimeout` (new; default 30s) via
  `coord.ActiveReaderSlots()` (new — counts this handle's reader
  slots) → `ErrCompactReadersActive` (new sentinel) on timeout,
  `copyCompact` to a 0600 temp preserving the UUID, atomic rename +
  dir-fsync, then swaps `db.file`/`db.pgr` against the new inode
  under `db.mu` (lock file + Coord + grant persist). `db.path`
  field added for the temp/rename. Invariants: drain-before-swap
  (clause-explicit), UUID-preserved + atomic-field-swap (entailed).
  Adversarial review: **2 rounds**. Round 1 found 2 introduced H +
  1 M — all fixed: **H1** (a writer that captured the pager then
  blocked in `AcquireWriter` behind Compact's grant used the closed
  old pager → panic; fixed by re-reading `db.pgr` under the
  post-grant `db.mu`, pinned by `TestCompactConcurrentWriterNoCrash`
  — confirmed to repanic when the fix is reverted), **H2** (Compact
  widened the file mode 0600→0644; fixed: copies are 0600,
  `TestCompactPreservesFileMode`), **M2** (reopen-failure
  split-brain on the unlinked inode; fixed: poison the handle →
  Close+reopen), **M1** (Compact-in-write-tx deadlock; documented).
  Round 2 found 1 introduced M — **BeginRead didn't reject a
  poisoned handle** (reads off the stale inode); fixed with a poison
  gate in `BeginRead` (`TestPoisonedHandleRejectsReads`). Spec:
  `api-surface.md §Compact` amended (dir-fsync, mode preservation,
  reopen-failure poison contract, in-tx-Compact prohibition).
- **11.7** ✅ Chunk close-out.
  - **11.7a** (fold) — `check-subpage-structural-validation` folded:
    plain `Check`'s structural walk now validates SetKeyspace subpage
    internals (`checkSetKeyspaceSubpages`: guarded `WalkLeafEntries`
    + `SubpageReader.Validate` with the descriptor's FixedValueSize →
    `SubpageCorrupt` CheckError), honouring `api-surface.md §Check`'s
    subpage-integrity claim (previously a spec-vs-code gap — a forged
    subpage passed plain Check silently). Pinned by
    `TestCheckStructuralDetectsForgedSubpage` (teeth-verified: fails
    pre-fix) + a fixed-value-size clean set in
    `TestCheckCleanPopulatedDB`. Review: ship, 0 H/M/L.
  - **Triage (close-out gate).** `check-subpage-structural-validation`
    promoted-then-deleted (rationale now inline in the
    `checkSetKeyspaceSubpages` godoc + the `§Check` spec claim + the
    regression test — no tracking-artifact cite). Two redefers
    (chunk-11 trigger spent without meeting the condition):
    `writenewindexregistry-partial-leak` (Repair reclaims the orphans
    but did not canonicalize the loose-pages-on-error contract) and
    `index-handle-stale-after-rebuild-drop` (no concurrent-iteration
    hardening landed). README `Lands:` reworded for both.
  - **Cite sweep** (wrap-aware) of `docs/specs/**` + production `*.go`:
    no tracking-artifact cites (the lone `overview.md` hit is a general
    directory description, not a specific-artifact cite). The two
    11.3e-deleted issues left no dangling pointers.
  - **Spec-tier invariant audit.** All chunk-11 invariants enforced as
    tests at their introducing sub-chunk: Inv-C1/C2 (11.2), Inv-RV1–4
    (11.3), Inv-C5 (11.4b). None recorded-only; none pending promotion.

Chunk 11 complete: Check (+ CheckIndexes, + Repair), CopyTo
(verbatim + compacting), Compact — all landed, reviewed, green.

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

**Sub-chunk roster.**

- **12.1** Triage + invariants (this gate). *Triage:* 0 entries match
  `Lands: 12`; two condition-triggered entries walked as their task
  lands — `leaked-readtx-cleanup-race-flake` (reader-slot lifecycle ↔
  Task 2) and `writenewindexregistry-partial-leak` (Task 1 reclaims
  such orphans like Repair; orphaning root unchanged ⇒ stays
  deferred). *Lock layout:* `LastMaintenanceTime` already exists
  (`internal/lock/format.go` + getter/setter; `cross-process.md`
  documents it) — no layout change. *Invariants:* spec records
  Inv-M1..M5; derived **Inv-M6** (entailed — the maintenance
  goroutine drains before Close unmaps lock file / pager). Schedule:
  M1+M6+M2 → 12.2, M3+M5 → 12.4, M4 → 12.5. (Task-order amended at
  12.2 — see below.)
- **12.2** ✅ `MaintenanceOptions` + `Options.Maintenance` + defaults
  (validated: negative Interval etc. → `ErrInvalidOptions`, else the
  goroutine's `time.NewTicker` would panic); the DB-level maintenance
  goroutine (start at Open, stop at Close — batch-coordinator
  lifecycle pattern) with the interval ticker + cross-process
  coordination via `lock.File.TryClaimMaintenance` (atomic CAS
  claim-at-START of `LastMaintenanceTime`, the Inv-M1-correct shape —
  see spec-amend below) + `coord.Clock()`. **Reorder (amends 12.1's
  roster):** bundles **Task 1** (bitmap leak reclamation — detection
  read-tx reusing the Check `checker` collect-leaks mode + reclamation
  write-tx via `FreeLeakedPage`, non-blocking: a leaked page's bit is
  clear so no allocator hands it out ⇒ it cannot become un-leaked,
  Inv-M2; gated on a clean walk like Repair) instead of Task 2,
  because Task 1 reuses already-reviewed pieces while Task 2 needs new
  lock-package lock-free stale-scan work. Crash-recovery-at-Open
  (`opened.NoCheckpoint`) schedules the first pass immediately.
  Promotes Inv-M1, Inv-M2, Inv-M6. Spec-amends applied:
  §Coordination claim-at-start (the literal "update after each pass"
  has an Inv-M1 TOCTOU hole); §Trigger immediate-pass keyed on
  `NoCheckpoint`. Filed `maintenance-compaction-threshold-disable`
  (Lands: 12.5 — `CompactionThreshold=0.0`-disabled unreachable, the
  field is inert until compaction). Review: 0 introduced H; M-1
  (this reorder note) + M-2 (option validation) + M-3 (off-Linux
  clock underflow guard) fixed; M-4 filed; SA-1/SA-2 applied.
- **12.3** ✅ **Task 2** stale reader-slot cleanup.
  `Coord.ReapStaleReaderSlots` acquires the write lock (`AcquireWriter`)
  and runs the existing LOCK_EX-preconditioned `OldestReaderTxnID` scan
  for its in-place stale-clear side effect; wired into
  `runMaintenancePass` after Task 1 (no write tx — clearing a slot is a
  lock-file mmap store). **Amends 12.1's roster note** ("needs a
  lock-free stale-scan"): a lock-free clear is *unsafe* — the clear
  stores + the `HintEpoch` first-observer CAS race peer clearers (a
  writer's RPL-reclamation scan, `RecoverStaleWriter`); a phantom
  eviction of a mid-publish reader slot lets RPL reclamation free pages
  a live reader still reads = corruption. Reusing the proven scan under
  LOCK_EX is the smallest correct change. *Spec-amend:* §Stale Reader
  Slot Cleanup now states the scan acquires `flock(LOCK_EX)` (the prior
  "no transaction needed — atomic store" could be misread as "lock-free
  OK"). Preserves the existing reader-table no-false-positive-clear
  invariant *by construction* (the only maintenance clear path acquires
  the grant first) — promotes no new Inv-M#. *Triage:* 0 README entries
  match `Lands: 12.3`; condition-triggered
  `leaked-readtx-cleanup-race-flake` walked (reader-slot lifecycle) →
  redeferred (Task 2 clears cross-process *stale* slots under LOCK_EX;
  it does not touch the in-process finalizer cleanup path the flaky test
  exercises). Review: 0 introduced H/M (Rounds 1+2); R1 L (test-comment
  mechanism) + nit (abnormal-error log) fixed, nit (clock re-read)
  disputed-accepted; R2 clean.
- **12.4** ✅ **Task 3** checksum scrubbing. `maintScrubChecksums`
  scans `ScrubBatchSize` page IDs/pass from the persistent
  `db.scrubCursor`, wrapping at HighWaterMark, footer-verifying only
  **allocated** pages (snapshot bitmap bit clear) in `[firstData, hwm)`
  — meta/bitmap region carries no footer, free pages hold none. A
  mismatch logs a `CheckWarning` with the page ID (Inv-M5), never
  repairs (Inv-M3). Skipped when PageChecksum off. Promotes Inv-M3 +
  Inv-M5 (spec-tier → enforced via tests) and adds + enforces an
  entailed **footer-bearing-gate** invariant (free/meta pages excluded;
  violation = non-full-db warning flood). *Triage:* 0 README entries
  match `Lands: 12.4`; no condition-triggered entry relates. Review:
  0 introduced H; R1 found 2 M sharing one root — *bitmap-allocated ≠
  valid-stable-footer*: (M1) a newer concurrent writer's in-flight page
  below the snapshot hwm can be observed torn, and (M2) a page allocated
  via low-level `Tx.AllocPage` and committed unwritten carries no
  footer. Both report-only. Fixed by removing a `torn-read-free`
  overclaim **I introduced this round** (the scrubber was always
  best-effort with `Check` as authority) + **re-verify-once** on
  mismatch (transient torn read clears on re-read; genuine bitrot /
  unwritten page persists, reported truthfully) + honest best-effort
  spec/doc. R1 L (multi-pass cursor test) + nits fixed; R2 clean (one
  nit: bitmap gate is a frozen pass-start copy, not live — corrected).
- **Task 4 (incremental compaction) split into 12.5a/12.5b** — the
  relocation mechanism (online CoW-cascade page relocation, resumable
  across passes) is intricate and corruption-prone, so the well-defined
  instrumentation + API lands first and is reviewed independently of the
  relocation.
- **12.5a** ✅ Allocator contiguous-allocation-failure-rate
  instrumentation + the compaction-control API. `Pager` gains atomic
  `contigAttempts`/`contigFragFails` counters incremented in
  `AllocContiguous` (every `n>1` call is an attempt; a first-scan
  `FindContiguous` miss with `bitmap.NumFree() >= n` is a fragmentation
  failure — "despite sufficient total free pages"), consumed
  (read-and-reset) via `ConsumeContiguousAllocStats` so the rate is
  windowed and converges. **Folds `maintenance-compaction-threshold-disable`**
  (Lands: 12.5): resolves the spec's broken "0.0 (disabled)" — `0.0` is
  semantically the *most aggressive* setting, and `cmp.Or` makes it
  unexpressible anyway, so `CompactionThreshold` is now a plain `[0,1]`
  rate (0 aggressive … 1 ≈ never, default 0.5) and a new
  `MaintenanceOptions.DisableCompaction bool` gives an exact
  compaction-only off-switch (user-approved: the two concepts —
  on/off vs aggressiveness — are orthogonal; compaction is the one task
  that rewrites live data, so disabling it while keeping leak
  reclamation/scrub/stale-reader is a real control). Instrumentation +
  API only; the trigger/relocation that consumes the rate is 12.5b (the
  counters are inert-but-tested until then, as `CompactionThreshold`
  was from 12.2). Spec §Incremental Compaction Trigger + §Options
  updated. Tested: `TestAllocContiguousFragmentationStats` (fragmented /
  contiguous / insufficient-free gate / n=1).
- **12.5b** **Task 4** relocation mechanism + wiring. **Scope decision
  (user): full coverage** — relocate B+tree nodes, overflow chains, and
  RPL segment pages, so a region can actually be fully evacuated into a
  contiguous free run (B+tree-only would leave overflow pages — the very
  contiguous consumers whose alloc-failure is the trigger — stranded, a
  functional gap). No reusable relocation primitive exists today (the
  btree CoW cascade is only reachable via key-based Put/Delete;
  `copyCompact` is a full rebuild), so built in phases, each reviewed
  independently given the corruption risk:
  - **12.5b-1** ✅ B+tree path-relocation primitive
    (`internal/btree/relocate.go`): `RelocatePages` walks bottom-up,
    CoW-relocating every page matching a predicate to a fresh id and
    forcing the mandatory ancestor pointer-fix cascade; `maxMoves` bounds
    *eligible* relocations (ancestor fixes uncounted ⇒ total CoWs ≤
    maxMoves×(1+depth), the caller sizes vs `MaxTxBufferBytes` in
    12.5b-3). Leaves relocate verbatim — their overflow refs point at
    overflow pages this primitive does NOT relocate (those stay valid;
    12.5b-2 relocates them). Tested: full round-trip (every page moved,
    contents + page-count identical, all old ids retired, none reused),
    budget bound, targeted predicate, overflow-leaf survival, single
    leaf, empty. Review: 0 H/M (the dangling-pointer / aliasing /
    stale-buffer classes traced clean against the pager's
    CoW/Alloc/Free semantics); L (use CoW's return; structural-count
    assertion; overflow-leaf test) fixed.
  - **12.5b-2** ✅ overflow-chain relocation, folded into
    `RelocatePages`. A leaf's overflow chain whose first page is eligible
    is copied to a fresh contiguous run (`relocateOverflowChain`), the
    owning entry's ref rewritten, and the leaf re-encoded 1:1 via
    `LeafBuilder` (keys unchanged ⇒ no split); `pw.Page` footer-verifies
    each source chain page so bitrot aborts (rolls back) rather than
    propagating. **RPL-segment relocation deferred** (user-approved,
    discovered mid-impl): RPL pages are owned by the commit pipeline
    (`appendRPL`/`reclaimRPL` alloc/chain/reclaim them) so out-of-band
    relocation races that machinery for low value — they're transient
    (drain via reclamation; new ones self-place low). Filed
    `rpl-segment-relocation` (condition); 12.5b-3's predicate excludes
    them. Tested: chain relocation (values round-trip, old runs retired,
    new refs disjoint, moved == Σ run lengths). Review: 0 H/M (re-encode
    fidelity across all cell types, follower copy, free-after-copy
    ordering, rollback all correct-by-construction); nits fixed
    (verbatim-leaf-keeps-ref test tightened, indivisible-chain-quantum
    doc); 2 L coverage gaps deferred to their natural homes — re-encode
    over nested/subpage/multivalue cells → 12.5b-2b, follower
    footer-verify on a corrupt chain (real pager) → 12.5b-3.
  - **12.5b-2b** ✅ nested-tree subtree relocation — **discovered mid-12.5b-2,
    not in the original "full coverage" enumeration**: SetKeyspace promotes
    large sets into nested B+trees rooted at a leaf cell's `NestedRoot`.
    `relocateLeaf` now recurses `relocateNode(NestedRoot, depth+1)` (the
    nested tree's own branches/leaves/overflow chains/further nesting all
    handled by the same machinery) and rewrites the owning entry's
    `NestedRoot` like an overflow ref when the nested root's id changes;
    `NestedCount` rides through the re-encode untouched (set-keyspace.md E1).
    Depth is continued (not reset) across the nesting boundary, matching
    `freeSubtreeAt`. A nested cell with `NestedRoot==0` aborts as
    `ErrCorrupted` (matching `freeSubtreeAt` / the `Walk*` readers) — checked
    unconditionally, even when the budget can't fund the descent. Encodes the
    12.5b-2b entailed invariant (referential integrity across the nesting
    boundary + `NestedCount` preservation) as a regression test. Also lands
    the 12.5b-2 review's deferred coverage: a re-encode test over a leaf
    holding inline/overflow/subpage/nested cells, asserting every cell type
    round-trips verbatim through the relocation re-encode. No spec change —
    the spec's generic "relocate pages to restore contiguous runs" already
    entails covering nested-tree subtree pages (an un-relocatable type pins
    its region); per-type handling is documented in `relocate.go`. RPL
    remains the sole excluded type (`rpl-segment-relocation`).
  - **Evacuation strategy (user decision, 12.5b-3):** **high-watermark
    evacuation** (over the spec-literal "fragmentation-region" approach).
    Predicate `shouldRelocate(id) = id >= evacFloor && id ∉ RPLsegments`;
    `evacFloor` is sized near `HighWaterMark` so the top band holds ~budget
    allocated pages (estimated from free-page density). Relocated pages get
    low ids from the consolidating allocator, the top band drains into a
    contiguous run, the file shrinks; monotone-convergent (moved pages stop
    matching the floor). Spec-amend applied: `background-maintenance.md
    §Incremental Compaction (Mechanism)` reworded from fragmented-region to
    high-watermark. Chosen as the smallest correct change that serves the
    trigger + enables shrink, reusing `RelocatePages`' budget/cursor as-is.
  - **12.5b-3a** forest-relocation engine `(tx).compactForest(pred, budget)`:
    walk the forest from `tx.keyspaceRoot` (keyspace descriptor tree → each
    keyspace's data tree + index registry sub-tree → each index's data tree;
    nested trees handled transitively by `RelocatePages`), calling
    `RelocatePages` per root with a shared descending budget. Re-root the
    cascade via the **existing** persistence machinery: index `entry.Root`
    rewritten by `btree.Put` into the (relocated) registry tree; the
    keyspace's `desc.Root`/`desc.IndexRegistryRoot` staged in
    `tx.dirtyDescriptors[name]` (re-`Put` at `flushKeyspaces`); the keyspace
    tree itself relocated last and assigned to `tx.keyspaceRoot` (→
    `meta.KeyspaceRoot` at commit). cfg discipline mirrors `copyCompact`:
    data tree uses the keyspace-overridden cfg, registry + index trees use
    base cfg. **RPL exclusion is inherent, not a predicate check:**
    `RelocatePages` only offers `shouldRelocate` the pages it reaches via
    the tree structure (branch/leaf ids, overflow first-page ids, nested
    roots); RPL segment pages hang off `meta.RPLHeadPage` on a separate
    chain the forest walk never visits, so they are never relocated without
    any `id ∉ RPL` term (a `Pager.IsRPLSegmentPage` query would be dead
    code). Predicate is simply `id >= evacFloor`. New entailed invariant
    (re-rooting referential integrity across all four root-holder kinds)
    recorded in the spec + enforced by the round-trip/Check-clean test.
    ✅ Tests: forest round-trip (plain + indexed + tiny + set keyspaces,
    overflow values, fragmented; all KV survives, index `LookupKeys`
    unchanged, SetKeyspace `CountValues`/`HasValue` intact incl. a
    nested-tree-promoted set, `KeyspaceRoot` changes, Check clean,
    moved>0), budget bound, empty/zero-budget. Review: 0 introduced H/M
    (re-rooting completeness, borrow-after-free, RPL exclusion, multi-pass
    convergence all verified — reviewer ran 8 passes Check-clean); L1
    (unmapped re-encode error) fixed, L2 (bare TxTooLarge sentinel — matches
    `mapPagerErr` convention) + nit (dead budget decrement) disputed; SA1
    (re-rooting invariant → spec), SA2 (no-open-keyspaces precondition →
    godoc) applied.
  - **12.5b-3b** orchestration + Inv-M4 + wiring: trigger eval
    (`ConsumeContiguousAllocStats` → rate vs `CompactionThreshold`; skip on
    `DisableCompaction` / no-signal `attempts==0`), `evacFloor` from density,
    resumable keyspace cursor (`db.compactCursor` — resume forest walk after
    the last keyspace whose budget exhausted, for fairness), wire as **Task
    4** in `runMaintenancePass`. **Inv-M4 promotion** (spec-tier →
    enforced): maintenance catches `ErrTxTooLarge` from the compaction tx,
    rolls back, logs non-fatally, and reduces the effective budget /
    reschedules — it never *surfaces* `ErrTxTooLarge` (the invariant's
    "reduces `CompactionBatchSize` or aborts and re-schedules"). Test:
    tiny `MaxTxBufferBytes` + large `CompactionBatchSize` + fragmented DB →
    pass returns nil, DB stays consistent, no user-visible error. Plus the
    12.5b-2 review's deferred real-pager test: relocate a committed overflow
    chain with `PageChecksum` on, corrupt a follower, assert
    `ErrBadPageChecksum` (not silent propagation). **Shrink verification
    (12.5b-3a review SA3/C1):** the spec's "the file can shrink" benefit is
    delivered only when `evacFloor` is sized *near* `HighWaterMark` — a
    midpoint floor consolidates but grows HWM for a pass (relocated-from
    pages are same-tx-unreclaimable RPL; fresh allocations extend the file)
    then plateaus. The density-based `evacFloor` must target the trailing
    band so commit's tail-refund/shrink truncates; 3b's test MUST assert
    `HighWaterMark` strictly *decreases* across passes (with intervening
    RPL drain), else the headline benefit ships unverified. *(pending)*
- **12.6** Chunk close-out. *(pending)*

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
