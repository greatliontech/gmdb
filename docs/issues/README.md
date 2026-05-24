# Issues

Tracked follow-ups for gmdb. Each entry is a `docs/issues/<slug>.md`
file with a `Lands:` trigger (a chunk number or a condition). Per
the workflow in `~/.claude/CLAUDE.md` (Issue triage), this index is
walked at every chunk-start gate (`N.1`) — entries whose `Lands:`
resolves to the current chunk are folded, redeferred, or closed.

When an issue is resolved, the load-bearing rationale moves inline
into the spec / code where it belongs (kept-current artifact), all
cites are repointed at the new home, and the issue file is deleted.
`git log --all -- docs/issues/<file>.md` preserves the history.

## Open

| Slug | Lands | Summary |
|------|-------|---------|
| [setkeyspace-put-added-bool](setkeyspace-put-added-bool.md) | 6 | Whether `SetKeyspace.Put` should return `(added bool, err error)`; the membership probe is already paid by the insert. |
| [slog-default-vs-spec](slog-default-vs-spec.md) | when DB gains an `Options.Logger` field | Cleanup callbacks use `slog.Default()`; spec describes a per-DB `*slog.Logger`. Wire `Options.Logger` through to cleanup-captured logger. |
| [bitmap-rollback-undo-log](bitmap-rollback-undo-log.md) | when profiling shows BeginTx allocation pressure is material | `Bitmap.Snapshot()` clones the full detail+summary per `Pager.BeginTx()` — 8 MB at 256 GB MaxSize. Replace with an undo log if profiling shows the per-tx allocation is hot. |
| [lagging-reader-callback](lagging-reader-callback.md) | 5.5 | `Options.LaggingReader` callback per `lock-ordering.md §Lagging Reader Handling` — invoked when bitmap+RPL exhausted and a reader is blocking reclamation. Folded into chunk 5.5 where `Keyspace.Put` is the first real `pager.AllocPage` consumer that exercises the spec-shaped pressure path. |
| [cursor-markstale-clear-cur](cursor-markstale-clear-cur.md) | 5.5 | `internal/btree.Cursor.MarkStale` bumps `gen` but leaves `curKey` / `curValue` aliasing leaf-page-buffer slices that an external mutator may free. Folded into chunk 5.5 where keyspace integration wires the first external `MarkStale` call-sites. |
| [tx-setkeyspaceconfig-missing-name-behavior](tx-setkeyspaceconfig-missing-name-behavior.md) | 5.5 | `Tx.SetKeyspaceConfig` missing-name sentinel unspecified. Chunk-5.1 narrowed the Delete-on-miss invariant to keyspace-content removal; configuration mutation is out of scope. |
| [tx-rebuildindex-missing-name-behavior](tx-rebuildindex-missing-name-behavior.md) | 7 | `Tx.RebuildIndex` missing-name sentinel unspecified (both the keyspace-missing and index-name-missing cases). Sibling of the SetKeyspaceConfig issue, split because they land in different chunks. |
| [pager-test-helper-export](pager-test-helper-export.md) | when chunk 5.3+ adds a second cross-package writer-pager fixture caller | `setupWriter` is duplicated into `internal/btree/pager_integration_test.go` as `setupPagerWriter` for the chunk-5.2 PageWriter parity test. Acceptable for one caller; factor when the second caller arrives. |
| [leaked-readtx-cleanup-race-flake](leaked-readtx-cleanup-race-flake.md) | condition (when read-tx slot lifecycle is next touched, or opportunistically) | `TestLeakedReadTxReleasesSlotViaCleanup` is flaky under `-race` due to finalizer-scheduling latency. Pre-existing on HEAD cd34a40; surfaced by chunk-5.4 Round 2 adversarial review. |
| [spec-numkeyspaces-semantics](spec-numkeyspaces-semantics.md) | 7 (or earlier if spec-amend preferred) | `keyspaces.md` is silent on whether `NumKeyspaces` counts all keyspace-B+tree leaf entries (incl Kind=2) or only user-visible (Kind=0+1). Chunk 5.4 implements the former; spec-amend candidate from the chunk-5.4 adversarial review. |
