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

This backlog spans **two audit waves**, kept in separate tables below
because their severity scales differ:

1. **Architecture / factoring audit** (concept, package decomposition,
   public API surface, composition/duplication). Its `[H]`/`[M]`/`[L]`
   tags mean High = public-API correctness/leak, Medium =
   factoring/duplication, Low = naming/surface nit.
2. **Completeness / correctness / algorithm audit (2026-05-30)** — run
   with the project treated as **in-progress, not finished**. Its
   `[H]`/`[M]`/`[L]` scale is **correctness-based**: High = a reachable
   in-spec input/state that yields a wrong result (data loss, corruption,
   use-after-reclaim); Medium = a spec'd capability missing or a reachable
   resource leak; Low = doc/spec-vs-code drift, diagnostics, or
   latent/narrow-reachability defects. 27 confirmed findings (2 refuted)
   collapse to the rows below by **shared fix** — the provenance (raw
   finding numbers, audit run `wf_4ad12a2f-039`) is recorded in each issue
   file. Several spec'd-but-unbuilt rows carry an explicit
   *implement-or-document* decision for the user; the cross-process rows
   carry a scope decision (is concurrent multi-process write in v0?).

In both, the severity tag is an **audit artifact, not a work plan**.
Several factoring rows are pre-v1 **clean-break** candidates gated by the
first tagged release (`development: true`, `.semrel.yaml`). Resolved
entries are removed from their table and preserved in git history
(`git log --all -- docs/issues/<file>.md`).

## Open — architecture / factoring audit

| Slug | Lands | Summary |
|------|-------|---------|
| [rpl-segment-relocation](rpl-segment-relocation.md) | condition (when RPL pages are shown to block consolidation, or when RPL relocation folds into the commit pipeline) | Online compaction (12.5b) relocates B+tree nodes + overflow chains but not RPL segment pages — they're managed by the commit pipeline (alloc/chain/reclaim) and rewriting them out-of-band races that machinery; they're transient (drain via reclamation, new ones self-place low). The 12.5b-3 orchestration treats them as immovable. User-approved deferral at 12.5b-2. |
| [pager-test-helper-export](pager-test-helper-export.md) | when chunk 5.3+ adds a second cross-package writer-pager fixture caller | `setupWriter` is duplicated into `internal/btree/pager_integration_test.go` as `setupPagerWriter` for the chunk-5.2 PageWriter parity test. Acceptable for one caller; factor when the second caller arrives. |

## Open — completeness / correctness / algorithm audit (2026-05-30)

Filed from deep audit run `wf_4ad12a2f-039` (7 finders + adversarial
verification; 27 confirmed, 2 refuted) plus a completeness pass. Rows are
severity-ordered (H → M → L); the `Subsumes` notes record which raw
findings each shared-fix row covers.

| Slug | Lands | Summary |
|------|-------|---------|
| [recovery-model-highest-epoch](recovery-model-highest-epoch.md) | condition (commit/recovery/RPL redesign, or grove-backport rotation) | **design direction** Replace per-commit checkpoint-preferring recovery with a highest-valid-epoch rule + an on-disk `durableEpoch` marker, retiring the live-vs-recovery meta asymmetry, the `lastCheckpointTxnID` bound, and the genesis-rollback gotcha. Multi-process design questions; substantial Spec-first effort. Spun out of `sync-mode-surface-consolidation`. |
| [cross-namespace-reader-heartbeat-liveness](cross-namespace-reader-heartbeat-liveness.md) | condition (when cross-process stale-detection is revisited) | **[L]** Cross-namespace (container) readers have no `kill()` fallback — a >10s heartbeat pause (docker pause, cgroup freeze, swap) evicts a live reader → reads reclaimed-and-reused pages. Document the data-integrity bound + reconsider the default. *Finding 21.* |
| [commit-headroom-at-tx-budget-cap](commit-headroom-at-tx-budget-cap.md) | condition (slab-budget accounting / commit-pipeline allocation model revisit) | **[M]** Commit-pipeline allocations count against MaxTxBufferBytes, so a tx driven to the cap cannot commit (ErrTxTooLarge at Commit) — partial work only recoverable by Rollback. Found building the chunk-3 regression fixture (2026-07-05). |

## Open — full-codebase audit (2026-07-04)

Filed from the 2026-07-04 audit (5 parallel subsystem auditors:
durability/recovery, concurrency/locking, btree/pager/free-space,
indexing/typed, bulkload/compaction/maintenance; several findings
carry failing-on-HEAD reproducers noted in the issue files). Worked as
an active burn-down in the chunk order of
`docs/plans/audit-burndown-2026-07.md` under the user's 2026-07-04
blanket authority (fix all, bottoms-up, spec amendments included).
Rows in chunk order; severity tags are the audit's.

| Slug | Lands | Summary |
|------|-------|---------|
| [pager-rpl-footer-verification](pager-rpl-footer-verification.md) | chunk 4 | **[H]** RPL segments never checksum-verified on reclaim/Open walks — decodable bit-flip frees live pages or panics in the bitmap. |
| [pager-freed-page-write-skip](pager-freed-page-write-skip.md) | chunk 5 | **[L]** Freed/tail-refunded pages' slab buffers still pwritten at commit — write amplification only. |
| [lock-stale-slot-clear-identity](lock-stale-slot-clear-identity.md) | chunk 6 | **[H]** Stale-clear leaves dead PID/heartbeat; next acquirer falsely evicted mid-publish → use-after-reclaim. Spec prescribes the defect (amend with fix). |
| [reader-begin-publish-race](reader-begin-publish-race.md) | chunk 7 | **[H]** Meta read before reader-slot publish, no re-validation — reclamation can free the snapshot's pages in the gap. Two auditors converged. |
| [lagging-reader-bound-checkpoint-term](lagging-reader-bound-checkpoint-term.md) | chunk 8 | **[H]** Bound-refresh uses prevMeta.TxnID not lastCheckpointTxnID — reclaims past the checkpoint under SyncLazy; deterministic corruption after crash recovery. |
| [checkpoint-failure-poisoning](checkpoint-failure-poisoning.md) | chunk 9 | **[H]** Checkpoint step-2/3/4 failures don't poison the handle — torn active meta (split brain) or fsyncgate false certification. |
| [create-dirent-durability](create-dirent-durability.md) | chunk 10 | **[M]** No parent-dir fsync at create; acked SyncDurable commits can vanish with the file. Compact downgrades a syncDir failure to a warning. |
| [beginread-close-lifecycle](beginread-close-lifecycle.md) | chunk 11 | **[M]** BeginRead racing Close panics/SIGSEGVs instead of ErrClosed — close gate never covers the BeginRead window. |
| [update-unresolved-child-grant](update-unresolved-child-grant.md) | chunk 12 | **[M]** Update with an unresolved child tx leaks the cross-process write grant until GC; all writers block. Demonstrated. |
| [maintenance-reclaim-snapshot-guard](maintenance-reclaim-snapshot-guard.md) | chunk 13 | **[H]** Leak detection uses snapshot tree + live bitmap; concurrent commit → live pages freed (demonstrated, default config). Cross-process overlap variant shares the fix. |
| [compact-peer-handle-generation](compact-peer-handle-generation.md) | chunk 14 | **[H]** Compact renames the inode under peer handles; peer writes land on the unlinked inode — silent write loss. Generation stamp in the lock header. |
| [check-consistency-classes](check-consistency-classes.md) | chunk 15 | **[M]** Check() verifies none of: key ordering, separator routing, desc.Count, NestedCount, NumKeyspaces — classes api-surface.md claims. |
| [iterator-cursor-unregistration](iterator-cursor-unregistration.md) | chunk 16 | **[M]** All/Range/Prefix leak cursor registrations for the tx lifetime — quadratic degradation in long transactions. |
| [index-covering-value-diff](index-covering-value-diff.md) | chunk 17 | **[H]** Covering bytes never rewritten on value-only updates (key-only diff) — stale covering lookups; Check flags normal workloads. Failing repro. + align rebuild/maintenance dup tie-break. |
| [index-child-merge-handle-reconciliation](index-child-merge-handle-reconciliation.md) | chunk 18 | **[H]** Child commit swaps pks.indexes but never re-points parent IndexHandles — silently stale lookups; freed-page reads after child Drop. Failing repro. |
| [readonly-index-lookups](readonly-index-lookups.md) | chunk 19 | **[M]** RO opens never load declared indexes — spec'd RO index lookups unreachable on every surface. Failing repro. |
| [setkeyspace-bulkload-error-mapping](setkeyspace-bulkload-error-mapping.md) | chunk 20 | **[M]** Indexed SetKeyspace.BulkLoad leaks internal sentinels (missing mapBtreeErr); oversize-first-value returns errBulkEntryTooLarge. |
| [set-cursor-materialization-bound](set-cursor-materialization-bound.md) | chunk 21 | **[L]** SetCursor materializes whole value sets per position; CountValues O(set) vs advertised O(1). Fix streaming or spec the bound. |
| [api-and-doc-drift-sweep](api-and-doc-drift-sweep.md) | chunk 22 | **[L]** 9-item sweep: Range arity check, lock-ordering.md phantom locks, leak-detection.md Close ordering, pager-slab budget clause, limits.md max-key decision, checkpoint stale-read note, RO-fleet reaping, plain/overflow cell contradiction, truncated comment. |
