# Issues

Tracked follow-ups for gmdb. Each entry is a `docs/issues/<slug>.md`
file with a `Lands:` trigger (a chunk number or a condition). Per
the workflow in `~/.claude/CLAUDE.md` (Issue triage), this index was
walked at every chunk-start gate (`N.1`) during the chunk roadmap —
entries whose `Lands:` resolved to the current chunk were folded,
redeferred, or closed.

The v0 chunk roadmap is complete and its plan was deleted at close-out
(`git log --all -- docs/plans/v0-implementation.md` recovers it). The
active plan is `docs/plans/architecture-consolidation.md`; its
chunk-start gates (`N.1`) walk this index, and entries may also be
pulled as a proactive burn-down — each resolved as its own change set:
diagnose → fix → regression test → adversarial review →
promote-then-delete.

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
| [rpl-segment-relocation](rpl-segment-relocation.md) | condition (when RPL pages are shown to block consolidation, or when RPL relocation folds into the commit pipeline) | Online compaction (v0 chunk 12.5b) relocates B+tree nodes + overflow chains but not RPL segment pages — they're managed by the commit pipeline (alloc/chain/reclaim) and rewriting them out-of-band races that machinery; they're transient (drain via reclamation, new ones self-place low). The v0 12.5b-3 orchestration treats them as immovable. User-approved deferral at v0 12.5b-2. |
| [plan-codename-residue](plan-codename-residue.md) | condition (own sweep change set, or per artifact as each listed spec/file is next amended) | **[M]** (artifact-homes, not factoring) ~80 retired-plan chunk-number references survive in specs and code/test comments (planning codenames in kept-current artifacts); both defining plans are deleted and the active plan reuses numbers 1–18, so bare "chunk N" now reads against the wrong plan. `indexing.md:619` also carries status narrative. 2026-07-07 close-out review finding M-3. |
| [pager-test-helper-export](pager-test-helper-export.md) | when a second cross-package writer-pager fixture caller (beyond `internal/btree`'s `setupPagerWriter`) arrives | `setupWriter` is duplicated into `internal/btree/pager_integration_test.go` as `setupPagerWriter` for the chunk-5.2 PageWriter parity test. Acceptable for one caller; factor when the second caller arrives. |

## Open — completeness / correctness / algorithm audit (2026-05-30)

Filed from deep audit run `wf_4ad12a2f-039` (7 finders + adversarial
verification; 27 confirmed, 2 refuted) plus a completeness pass. Rows are
severity-ordered (H → M → L); the `Subsumes` notes record which raw
findings each shared-fix row covers.

| Slug | Lands | Summary |
|------|-------|---------|
| [recovery-model-highest-epoch](recovery-model-highest-epoch.md) | chunks 8–9 (design landed at chunk 7: durable-sub-record model; resolves with the implementation) | **design direction** Replace per-commit checkpoint-preferring recovery with a highest-valid-epoch rule + an on-disk `durableEpoch` marker, retiring the live-vs-recovery meta asymmetry, the `lastCheckpointTxnID` bound, and the genesis-rollback gotcha. Multi-process design questions; substantial Spec-first effort. Spun out of `sync-mode-surface-consolidation`. |
| [cross-namespace-reader-heartbeat-liveness](cross-namespace-reader-heartbeat-liveness.md) | condition (when cross-process stale-detection is revisited) | **[L]** Cross-namespace (container) readers have no `kill()` fallback — a >10s heartbeat pause (docker pause, cgroup freeze, swap) evicts a live reader → reads reclaimed-and-reused pages. Document the data-integrity bound + reconsider the default. *Finding 21.* |
| [rpl-head-exemption-reclaimed](rpl-head-exemption-reclaimed.md) | chunk 8 (fix designed at chunk 7: persisted RPLHeadTxnID + epoch-ownership condition) | **[M]** rebuildRPLChain hard-fails Open on a bad head, but RPLHeadPage carries forward across no-retire commits, so an older checkpoint's head can be legitimately reclaimed+reused — recovery after a crash-mid-commit becomes unopenable. Audit-burndown (2026-07) chunk-4 review finding. |
| [dbcleanup-teardown-drain](dbcleanup-teardown-drain.md) | condition (AddCleanup execution model revisit) | **[L]** dbCleanupFn tears down coord/lockFile without the txInflight drain Close performs — safe only while the runtime runs cleanups sequentially. Audit-burndown (2026-07) chunk-11 review finding. |

