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
| [compaction-full-forest-walk-per-pass](compaction-full-forest-walk-per-pass.md) | profiling-driven, or opportunistically | Incremental compaction's `compactForest` re-walks every B+tree in the forest each pass (O(live pages) reads) to find the high-watermark pages worth relocating. The plan's "resumable keyspace cursor" was dropped (no fairness problem to solve with high-watermark evacuation); the residual per-pass read-walk is an efficiency cost. Filed at 12.5b-3b. |
| [pager-test-helper-export](pager-test-helper-export.md) | when chunk 5.3+ adds a second cross-package writer-pager fixture caller | `setupWriter` is duplicated into `internal/btree/pager_integration_test.go` as `setupPagerWriter` for the chunk-5.2 PageWriter parity test. Acceptable for one caller; factor when the second caller arrives. |
| [leaked-readtx-cleanup-race-flake](leaked-readtx-cleanup-race-flake.md) | condition (when read-tx slot lifecycle is next touched, or opportunistically) | `TestLeakedReadTxReleasesSlotViaCleanup` is flaky under `-race` due to finalizer-scheduling latency. Pre-existing on HEAD cd34a40; surfaced by chunk-5.4 Round 2 adversarial review. |
| [setkeyspace-delete-range-bulk-walker](setkeyspace-delete-range-bulk-walker.md) | opportunistic — profiling-driven or chunk-7 indexed-DeleteRange | Chunk-6.8 v1 uses a snapshot-then-Delete loop (O(K log N) per-key descents) instead of the chunk-5.7 three-phase walker (O(K + log N)). Correct but slower for large ranges. Filed for the per-cell-callback rewrite. |
| [setkeyspace-indexing-perf-and-edge](setkeyspace-indexing-perf-and-edge.md) | profiling-driven (items A + B perf-only after chunk-7.10 partial-fold) | A) Double snapshot in indexed `SetKeyspace.Put`/`DeleteValue` (outer + inner) — perf only. B) Per-value snapshot in indexed bulk-key Delete loops over N members allocating N maps — perf only. **C closed at chunk-7.10 (validateIndexDecls rejects zero-column IndexDecls with ErrInvalidOptions).** Surfaced by chunk-7.9 Round-1 adversarial review (L-1, L-2, M-3). |
| [bulkload-index-merge-run-fanin](bulkload-index-merge-run-fanin.md) | profiling-driven — when indexed `BulkLoad` spills enough sort runs to approach the FD limit, or opportunistically | Indexed `BulkLoad`'s external sort opens every spilled run at once for a single k-way merge — O(#runs) FDs + read buffers, not O(depth). Unreachable at the default `MaxTxBufferBytes`; a pathological tiny buffer + huge input could hit `EMFILE` (graceful abort, not corruption). Spec permits single-pass merge. Cascaded multi-pass merge is the fix. Surfaced by chunk-8.6 Round-1 adversarial review (L-2). |
| [rplsegments-clone-cost](rplsegments-clone-cost.md) | profiling-driven — when in-memory RPL chain length is shown to materially affect per-`BeginSavepoint` cost, or when the bounded-by-small-constant claim is contradicted | `captureSavepointState` still clones `rplSegments` per call; the chain length is bounded by in-memory RPL state (not MaxSize), but the bound has no enforcing assertion or test. Pathological workloads (stuck reclamation bound, lagging-reader pinning every segment) could grow the chain past the small-constant assumption. Filed at the `shallow-savepoint-clone-cost` close-out (Round-1 adversarial review L-2). |
| [nested-shallow-loose-pop-buffer-alias](nested-shallow-loose-pop-buffer-alias.md) | condition — when nested SHALLOW savepoints become in-spec, OR when a panic-on-nested-shallow guard lands in `BeginShallowSavepoint` | Nested SHALLOW savepoints can double-reference the same loose-popped `buf` pointer across outer + inner `loosePopLog`s. Outer Restore's `wasPreWindow=true` branch pool-Puts the buffer the inner Restore just re-installed → buffer in both pool free list AND `dirty[id]`; a subsequent `bufPool.Get` corrupts dirty silently. Unreachable in production (6 per-row callers each open-and-resolve one shallow per call); reachable via test code. Pre-existing per the diff arbiter (`15f9b70` baseline); surfaced at the `shallow-savepoint-clone-cost` close-out (Round-2 adversarial review M-1). |
