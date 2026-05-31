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
| [tx-responsibility-decomposition](tx-responsibility-decomposition.md) | proactive | **[M]** `Tx` god-object: 16 exported methods / 21 fields (down from 22 — the raw-pager cluster was removed, that H slice landed). Remaining: index-DDL (`RebuildIndex`/`DropIndex`) bolted onto the transaction; the `Create*IfNotExists` matrix. |
| [index-maintenance-engine-duplication](index-maintenance-engine-duplication.md) | proactive | **[M]** The index diff/unique-probe/insert engine is forked per keyspace kind (`applyIndexMaintenanceOn*` on `*Keyspace` vs `*SetKeyspace`), incl. a verbatim probe loop. Factor a kind-agnostic engine. |
| [btree-pagewriter-pager-vocabulary](btree-pagewriter-pager-vocabulary.md) | proactive | **[M]** `btree.PageWriter` names pager internals (`CoW`, `AllocSlab`, RPL doc-refs) — behaviorally clean but the interface mirrors `*pager.Pager`. Rename to storage-neutral terms. |
| [typed-layer-naming-and-duplication](typed-layer-naming-and-duplication.md) | before first tagged release | **[M]** `TypedKeyspace`/`TypedKS` (and Set variants) use an undocumented Decl-vs-Handle naming split; `Open`/`Create` quartet duplicated across the two factories. Rename + share. |
| [dev-process-artifacts-in-comments](dev-process-artifacts-in-comments.md) | proactive | **[M]** 293 `chunk-N` refs across 49 prod files (+ "spec amend"/`Inv-XXX`) cite the dev process — dangling now the roadmap is done, and contrary to the project's own no-cite convention. Repoint to specs/tests. |
| [encoder-naming-and-namespace](encoder-naming-and-namespace.md) | before first tagged release | **[L]** Encoder set: inconsistent `BE` prefix, `BENanosEncoder` names the encoding not the type, 11 symbols crowd the root namespace (candidate `gmdb/encoders` sub-package). |
| [index-noun-overload](index-noun-overload.md) | before first tagged release | **[L]** `Index` is both the query handle and the `Index*` declaration-family prefix. Rename the handle (`IndexHandle`/`IndexQuery`), matching the typed side. |

## Open — completeness / correctness / algorithm audit (2026-05-30)

Filed from deep audit run `wf_4ad12a2f-039` (7 finders + adversarial
verification; 27 confirmed, 2 refuted) plus a completeness pass. Rows are
severity-ordered (H → M → L); the `Subsumes` notes record which raw
findings each shared-fix row covers.

| Slug | Lands | Summary |
|------|-------|---------|
| [put-ascend-error-rollback](put-ascend-error-rollback.md) | proactive | **[L]** Pre-existing: `put.go`'s store loop doesn't roll back allocated pages when `ascendNoSplit`/`ascendWithSplit` fails (intra-tx page-reuse only; no durability impact — tx Rollback discards the bitmap). Surfaced reviewing on-demand promotion, which widens the leaked set. |
| [statistics-surface-unimplemented](statistics-surface-unimplemented.md) | proactive | **[M]** `DBStats`/`TxStats`/`KeyspaceStats` + the four `Stats()` methods (api-surface §Statistics) are entirely unbuilt and were an untracked downscope; plan's chunk-11 "final wiring" claim is false. Implement or file a concrete deferral. *Finding 9.* |
| [options-readonly-mode](options-readonly-mode.md) | proactive | **[M]** `Options.ReadOnly` (read-only open) is spec'd and **named by the live `options.go:98` godoc**, but the field and the read-only path don't exist. Implement, or strip the dangling godoc/spec reference. *Finding 10.* |
| [file-growstep-minsize](file-growstep-minsize.md) | proactive | **[M]** File growth does a 1-page `ftruncate` per allocation (persisted `GrowStep` has no effect); `maybeShrink` can truncate below the `MinSize` floor — a clause-explicit invariant violation. Implement GrowStep-aligned grow/shrink or amend the file-format spec. *Finding 12.* |
| [update-view-panic-recovery](update-view-panic-recovery.md) | proactive | **[M]** A panic in `fn` inside `DB.Update`/`DB.View` unwinds past `Rollback`, leaking the cross-process write grant / reader slot until GC → unbounded writer starvation. Add `defer` cleanup or document the non-recovery contract. *Finding 13.* |
| [options-coord-intervals](options-coord-intervals.md) | proactive | **[M]** `Options.StaleTimeout`/`HeartbeatInterval`/`LockRetryInterval` are spec'd but absent; `CoordOptions` plumbing is dead (always defaulted), `StaleTimeout` has none. Wire them or strike from the spec. *Finding 14.* |
| [nextsequence-unbuilt](nextsequence-unbuilt.md) | proactive | **[M]** `Keyspace.NextSequence`/`SetKeyspace.NextSequence` unbuilt — the `NextSeq` descriptor field is serialized but never incremented or read (a stub, not dead state). *Completeness pass.* |
| [setfileformat-unbuilt](setfileformat-unbuilt.md) | proactive | **[M]** `Tx.SetFileFormat` + the `FileFormat` type are unbuilt, yet the `CopyTo` godoc (`copy.go:35`) already points at them. Implement or remove the dangling reference. *Completeness pass.* |
| [madvise-options](madvise-options.md) | proactive | **[M]** mmap tuning (`PreloadPages`/`HugePages`/`ReclaimOnClose`) is spec'd in mmap-strategy/overview, but there is no Options field and **no `MADV_*` call anywhere**. Wire `unix.Madvise` or trim the spec. *Completeness pass.* |
| [sync-mode-surface-consolidation](sync-mode-surface-consolidation.md) | proactive (after cross-process hardening) | **[M]** `SyncUnsafe` is a phantom 4th mode — behaviourally identical to `SyncLazy` (same `SyncNone` policy + checkpoint-clear; no divergent code path), so its documented "corruption risk" is unbacked. Drop it (4→3); decide whether to rename the remaining 3 to match sibling project `grove`. Notes (separately) the larger highest-epoch-recovery redesign direction. *Sync-mode discussion.* |
| [errkeytoolarge-sentinel](errkeytoolarge-sentinel.md) | proactive | **[L]** `ErrKeyTooLarge` is documented public (`keyspace.go:543`) but is not a gmdb sentinel; the internal btree error leaks unwrapped from `Put` and `BulkLoad`, so `errors.Is(err, gmdb.ErrKeyTooLarge)` can't compile. *Finding 22.* |
| [indexstats-shape-and-depth](indexstats-shape-and-depth.md) | proactive | **[L]** Implemented `IndexStats{Count,TreeDepth}` is a smaller, divergent shape vs the spec's 7 fields, and `TreeDepth` is hardwired 0 (depth is available from `btree.Walk`). Reconcile + populate. *Subsumes findings 16,23.* |
| [byte-level-iterators](byte-level-iterators.md) | proactive | **[L]** Byte-level `Keyspace`/`SetKeyspace.All/Range/Prefix` unbuilt; the typed layer reimplements rather than delegates as the plan claims. Build them or correct the plan. *Finding 15.* |
| [deleterange-obsolete-comment](deleterange-obsolete-comment.md) | proactive | **[L]** `keyspace.go:766-769` comment claims indexed `DeleteRange` is unimplemented, but it's wired (`deleteRangeIndexed`). Pure doc rot — repoint the comment. *Finding 18.* |
| [commit-fdatasync](commit-fdatasync.md) | proactive | **[L]** Commit calls `fsync` where every spec and the Design Decisions table specify `fdatasync` (correctness-safe; the per-commit perf rationale is unmet). Use `unix.Fdatasync` or amend the spec. *Finding 24.* |
| [rpl-corruption-silent-halt](rpl-corruption-silent-halt.md) | proactive | **[L]** A corrupt RPL segment silently halts reclamation and grows the file, surfacing a misleading `ErrDBFull` instead of the real corruption (a deliberate but undocumented availability choice). Document the policy + add a warning/health flag. *Finding 25.* |
| [lock-ordering-phantom-mutexes](lock-ordering-phantom-mutexes.md) | proactive | **[L]** `lock-ordering.md:58-67` mandates acquisition order for `pager.mu`/`bitmap.mu`, but neither mutex exists. Correct the invariant doc to the locks that do exist. *Finding 26.* |
| [cross-namespace-reader-heartbeat-liveness](cross-namespace-reader-heartbeat-liveness.md) | condition (when cross-process stale-detection is revisited) | **[L]** Cross-namespace (container) readers have no `kill()` fallback — a >10s heartbeat pause (docker pause, cgroup freeze, swap) evicts a live reader → reads reclaimed-and-reused pages. Document the data-integrity bound + reconsider the default. *Finding 21.* |
| [writer-heartbeat-after-lock-release](writer-heartbeat-after-lock-release.md) | proactive | **[L]** The heartbeat goroutine can store `WriterHeartbeat` after `LOCK_UN`, contradicting a clause-explicit "only under `LOCK_EX`" invariant (benign on a shared clock; reachable on per-process clocks). Gate the store or downgrade the invariant text. *Finding 20.* |
