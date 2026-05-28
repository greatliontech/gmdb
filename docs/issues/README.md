# Issues

Tracked follow-ups for gmdb. Each entry is a `docs/issues/<slug>.md`
file with a `Lands:` trigger (a chunk number or a condition). Per
the workflow in `~/.claude/CLAUDE.md` (Issue triage), this index was
walked at every chunk-start gate (`N.1`) during the chunk roadmap —
entries whose `Lands:` resolved to the current chunk were folded,
redeferred, or closed.

**The v0 chunk roadmap is now complete** (see
`docs/plans/v0-implementation.md`), so the chunk-start gates no longer
fire. This index is now the **active v0 backlog**, worked as a
proactive burn-down: each follow-up is pulled when chosen (its `Lands:`
trigger records the original deferral rationale, not a blocker), and
resolved as its own change set — diagnose → fix → regression test →
adversarial review → promote-then-delete.

When an issue is resolved, the load-bearing rationale moves inline
into the spec / code where it belongs (kept-current artifact), all
cites are repointed at the new home, and the issue file is deleted.
`git log --all -- docs/issues/<file>.md` preserves the history.

## Open

| Slug | Lands | Summary |
|------|-------|---------|
| [rpl-segment-relocation](rpl-segment-relocation.md) | condition (when RPL pages are shown to block consolidation, or when RPL relocation folds into the commit pipeline) | Online compaction (12.5b) relocates B+tree nodes + overflow chains but not RPL segment pages — they're managed by the commit pipeline (alloc/chain/reclaim) and rewriting them out-of-band races that machinery; they're transient (drain via reclamation, new ones self-place low). The 12.5b-3 orchestration treats them as immovable. User-approved deferral at 12.5b-2. |
| [open-corrupt-meta-size-fields-panic](open-corrupt-meta-size-fields-panic.md) | opportunistic — Open-time corrupt-meta robustness hardening, or a meta-fuzzing pass | `ValidateMeta` validates no size/offset field, so a checksum-verifying meta with a corrupt `BitmapPages` (or `MaxSize`) panics Open (slice-out-of-range at `init.go:240` / OOM) instead of returning `ErrCorrupted`. Same fault class as the resolved RPL out-of-range panic; distinct proximate line. Adjacent; surfaced by that fix's Round-1 adversarial review. |
| [compaction-full-forest-walk-per-pass](compaction-full-forest-walk-per-pass.md) | profiling-driven, or opportunistically | Incremental compaction's `compactForest` re-walks every B+tree in the forest each pass (O(live pages) reads) to find the high-watermark pages worth relocating. The plan's "resumable keyspace cursor" was dropped (no fairness problem to solve with high-watermark evacuation); the residual per-pass read-walk is an efficiency cost. Filed at 12.5b-3b. |
| [pager-test-helper-export](pager-test-helper-export.md) | when chunk 5.3+ adds a second cross-package writer-pager fixture caller | `setupWriter` is duplicated into `internal/btree/pager_integration_test.go` as `setupPagerWriter` for the chunk-5.2 PageWriter parity test. Acceptable for one caller; factor when the second caller arrives. |
| [leaked-readtx-cleanup-race-flake](leaked-readtx-cleanup-race-flake.md) | condition (when read-tx slot lifecycle is next touched, or opportunistically) | `TestLeakedReadTxReleasesSlotViaCleanup` is flaky under `-race` due to finalizer-scheduling latency. Pre-existing on HEAD cd34a40; surfaced by chunk-5.4 Round 2 adversarial review. |
| [setkeyspace-delete-range-bulk-walker](setkeyspace-delete-range-bulk-walker.md) | opportunistic — profiling-driven or chunk-7 indexed-DeleteRange | Chunk-6.8 v1 uses a snapshot-then-Delete loop (O(K log N) per-key descents) instead of the chunk-5.7 three-phase walker (O(K + log N)). Correct but slower for large ranges. Filed for the per-cell-callback rewrite. |
| [index-handle-stale-after-rebuild-drop](index-handle-stale-after-rebuild-drop.md) | when concurrent-iteration safety hardening lands (chunk-11 close-out redefer: Check / Repair / CopyTo / Compact landed no concurrent-iteration hardening — markIndexHandlesStale wiring is still its own work bundle) | `Tx.RebuildIndex` / `Tx.DropIndex` mutate `pinnedIndex.root` in place + FreeSubtree the old data tree without invalidating in-flight `*Index` iterators. Pre-existing gap (chunk-7.6 atomic Put has the same shape) amplified by chunk-7.8/7.10 bulk-free retirement. Chunk-7.10 redeferred per Round-1 H-1 triage — full markIndexHandlesStale wiring is its own work bundle. Surfaced by chunk-7.8 Round-1 adversarial review (M-1). |
| [setkeyspace-indexing-perf-and-edge](setkeyspace-indexing-perf-and-edge.md) | profiling-driven (items A + B perf-only after chunk-7.10 partial-fold) | A) Double snapshot in indexed `SetKeyspace.Put`/`DeleteValue` (outer + inner) — perf only. B) Per-value snapshot in indexed bulk-key Delete loops over N members allocating N maps — perf only. **C closed at chunk-7.10 (validateIndexDecls rejects zero-column IndexDecls with ErrInvalidOptions).** Surfaced by chunk-7.9 Round-1 adversarial review (L-1, L-2, M-3). |
| [bulkload-index-merge-run-fanin](bulkload-index-merge-run-fanin.md) | profiling-driven — when indexed `BulkLoad` spills enough sort runs to approach the FD limit, or opportunistically | Indexed `BulkLoad`'s external sort opens every spilled run at once for a single k-way merge — O(#runs) FDs + read buffers, not O(depth). Unreachable at the default `MaxTxBufferBytes`; a pathological tiny buffer + huge input could hit `EMFILE` (graceful abort, not corruption). Spec permits single-pass merge. Cascaded multi-pass merge is the fix. Surfaced by chunk-8.6 Round-1 adversarial review (L-2). |
| [shallow-savepoint-clone-cost](shallow-savepoint-clone-cost.md) | profiling-driven — when per-Put `BeginShallowSavepoint` overhead is measurably material in indexed-OLTP workloads, or opportunistically | `Pager.BeginShallowSavepoint` clones `pendingAllocs`/`Frees`/`loosePages`/`dirtyKeys` per call → O(N²·depth) total clone work across N indexed Puts in one tx. The 0893be5 bitmap-undo-log fix closed the O(MaxSize) cost in `Bitmap.Snapshot`; the residual O(this-tx-state-at-Begin) on the other fields is benign for OLTP-N (≤ 100) and material only for large bulk-style N. Resolution candidates: extend undo-log pattern to the remaining fields, or `transactions.md` cost-clause amendment. Surfaced by the writenewindexregistry-partial-leak per-row resolution session's Round 2 adversarial review (L-3). |
