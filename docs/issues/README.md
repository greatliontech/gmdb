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

Fourteen entries were filed from the **2026-05-30 architecture /
factoring audit** (concept, package decomposition, public API surface,
composition/duplication). The two High public-API leaks
(`meta-leaks-internal-page-type`, `raw-pager-primitives-on-public-tx`)
were resolved in that same pass and closed — their rationale is
promoted into `docs/specs/api-surface.md` §ReadTx and `export_test.go`.
Subsequent burn-down has also closed `keyspace-setkeyspace-shared-core`
(the keyspaceCore embed + shared guard / cursor helpers; rationale in
`keyspace_core.go`) and filed `keyspace-cursor-dead-append-guard` from
its review. The `[H]`/`[M]`/`[L]` tags in their
summaries are that audit's severity ordering (an audit artifact, not a
work plan) — High = public-API correctness/leak, Medium =
factoring/duplication, Low = naming/surface nit. Several are pre-v1
**clean-break** candidates gated by the first tagged release
(`development: true`, `.semrel.yaml`).

## Open

| Slug | Lands | Summary |
|------|-------|---------|
| [rpl-segment-relocation](rpl-segment-relocation.md) | condition (when RPL pages are shown to block consolidation, or when RPL relocation folds into the commit pipeline) | Online compaction (12.5b) relocates B+tree nodes + overflow chains but not RPL segment pages — they're managed by the commit pipeline (alloc/chain/reclaim) and rewriting them out-of-band races that machinery; they're transient (drain via reclamation, new ones self-place low). The 12.5b-3 orchestration treats them as immovable. User-approved deferral at 12.5b-2. |
| [pager-test-helper-export](pager-test-helper-export.md) | when chunk 5.3+ adds a second cross-package writer-pager fixture caller | `setupWriter` is duplicated into `internal/btree/pager_integration_test.go` as `setupPagerWriter` for the chunk-5.2 PageWriter parity test. Acceptable for one caller; factor when the second caller arrives. |
| [tx-responsibility-decomposition](tx-responsibility-decomposition.md) | proactive | **[M]** `Tx` god-object: 16 exported methods / 21 fields (down from 22 — the raw-pager cluster was removed, that H slice landed). Remaining: index-DDL (`RebuildIndex`/`DropIndex`) bolted onto the transaction; the `Create*IfNotExists` matrix. |
| [keyspace-cursor-dead-append-guard](keyspace-cursor-dead-append-guard.md) | proactive | **[L]** `Keyspace.Cursor` appends to `openCursors` unconditionally while `SetKeyspace.Cursor` guards `if !ks.dead`; a `for { Cursor() }` loop after `DeleteKeyspace` grows the slice unbounded within the tx (no correctness impact). Pre-existing; surfaced by the keyspaceCore cursor-factory review. |
| [index-maintenance-engine-duplication](index-maintenance-engine-duplication.md) | proactive | **[M]** The index diff/unique-probe/insert engine is forked per keyspace kind (`applyIndexMaintenanceOn*` on `*Keyspace` vs `*SetKeyspace`), incl. a verbatim probe loop. Factor a kind-agnostic engine. |
| [btree-pagewriter-pager-vocabulary](btree-pagewriter-pager-vocabulary.md) | proactive | **[M]** `btree.PageWriter` names pager internals (`CoW`, `AllocSlab`, RPL doc-refs) — behaviorally clean but the interface mirrors `*pager.Pager`. Rename to storage-neutral terms. |
| [db-handle-extract-coordinator-substructs](db-handle-extract-coordinator-substructs.md) | proactive | **[M]** `DB` inlines a 7-field batch-coordinator cluster and a 5-field maintenance cluster as loose fields. Extract sub-structs (mechanical, no behavior change). |
| [typed-layer-naming-and-duplication](typed-layer-naming-and-duplication.md) | before first tagged release | **[M]** `TypedKeyspace`/`TypedKS` (and Set variants) use an undocumented Decl-vs-Handle naming split; `Open`/`Create` quartet duplicated across the two factories. Rename + share. |
| [dev-process-artifacts-in-comments](dev-process-artifacts-in-comments.md) | proactive | **[M]** 293 `chunk-N` refs across 49 prod files (+ "spec amend"/`Inv-XXX`) cite the dev process — dangling now the roadmap is done, and contrary to the project's own no-cite convention. Repoint to specs/tests. |
| [open-ignores-context](open-ignores-context.md) | before first tagged release | **[L]** `Open(_ context.Context, …)` ignores its ctx (also diverges from the spec signature). Honor it or drop it. |
| [begin-vestigial-write-bool](begin-vestigial-write-bool.md) | before first tagged release | **[L]** `Begin(ctx, write bool)` — `write` must always be `true`; backcompat cruft for a non-existent base. Collapse to `Begin(ctx)`. |
| [dead-sentinel-errversionmismatch](dead-sentinel-errversionmismatch.md) | before first tagged release | **[L]** `ErrVersionMismatch` is declared but never returned (its own godoc admits it). Remove, or land it with its first producer. |
| [encoder-naming-and-namespace](encoder-naming-and-namespace.md) | before first tagged release | **[L]** Encoder set: inconsistent `BE` prefix, `BENanosEncoder` names the encoding not the type, 11 symbols crowd the root namespace (candidate `gmdb/encoders` sub-package). |
| [index-noun-overload](index-noun-overload.md) | before first tagged release | **[L]** `Index` is both the query handle and the `Index*` declaration-family prefix. Rename the handle (`IndexHandle`/`IndexQuery`), matching the typed side. |
