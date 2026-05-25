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
| [bitmap-rollback-undo-log](bitmap-rollback-undo-log.md) | when profiling shows BeginTx allocation pressure is material | `Bitmap.Snapshot()` clones the full detail+summary per `Pager.BeginTx()` — 8 MB at 256 GB MaxSize. Replace with an undo log if profiling shows the per-tx allocation is hot. |
| [pager-test-helper-export](pager-test-helper-export.md) | when chunk 5.3+ adds a second cross-package writer-pager fixture caller | `setupWriter` is duplicated into `internal/btree/pager_integration_test.go` as `setupPagerWriter` for the chunk-5.2 PageWriter parity test. Acceptable for one caller; factor when the second caller arrives. |
| [leaked-readtx-cleanup-race-flake](leaked-readtx-cleanup-race-flake.md) | condition (when read-tx slot lifecycle is next touched, or opportunistically) | `TestLeakedReadTxReleasesSlotViaCleanup` is flaky under `-race` due to finalizer-scheduling latency. Pre-existing on HEAD cd34a40; surfaced by chunk-5.4 Round 2 adversarial review. |
| [btree-branch-page-validation](btree-branch-page-validation.md) | opportunistic — chunk 11 (`Check()`) or fuzz-surfaced repro | Defense-in-depth: `btree.Get`/`Has`/`Delete`/`Cursor`/`FreeSubtree` validate leaf pages via `LeafReader.Validate` but iterate branch children without an equivalent `ValidateBranch`. Adjacent to chunk-5.6, surfaced as L1 in its Round 1 review. |
| [btree-post-merge-underflow](btree-post-merge-underflow.md) | when invariant #3 fill-ratio enforcement test is added (chunk 5.8 hardening or later) | Post-`mergeOrRedistribute*` underflow flag is forcibly cleared regardless of fill ratio. A merge of two below-MT siblings leaves the merged page below MT without triggering second-pass rebalance. Adjacent to chunk-5.7, surfaced as M-1 in its Round 1 review; cause-line in chunk-4 merge contract. |
| [setkeyspace-put-redundant-membership-probe](setkeyspace-put-redundant-membership-probe.md) | opportunistic — when `btree.Put` gets a "report existing" variant | Chunk-6.1's "no-cost API enhancement" claim for `SetKeyspace.Put(added bool, …)` is currently violated on the nested-tree-cell path: `putIntoNestedTree` pays `btree.Has` + `btree.Put` (two full descents). Same shape as `Keyspace.Put`'s pre-existing redundant probe. Surfaced by chunk-6.6 Round-1 review (M-2). |
| [cursor-err-unpositioned-state](cursor-err-unpositioned-state.md) | opportunistic — next audit of `transactions.md §Cursor State Machine` | `Cursor.Err()` / `SetCursor.Err()` do not return `ErrCursorUnpositioned` in the Unpositioned state, contradicting the spec's state-machine table. Pre-existing in chunk-5; surfaced by chunk-6.7 Round-1 review (M-2). |
| [setkeyspace-delete-range-bulk-walker](setkeyspace-delete-range-bulk-walker.md) | opportunistic — profiling-driven or chunk-7 indexed-DeleteRange | Chunk-6.8 v1 uses a snapshot-then-Delete loop (O(K log N) per-key descents) instead of the chunk-5.7 three-phase walker (O(K + log N)). Correct but slower for large ranges. Filed for the per-cell-callback rewrite. |
| [index-registry-decoder-bounds](index-registry-decoder-bounds.md) | 11 (`Check()` integrity walk) | `decodeRegistryEntry` and `registryList` lack pre-checks on `colCount*2` / `covCount*2` vs remaining data and on `registryList` total byte count, so a maliciously-large on-disk count value forces a multi-MB slice allocation before the per-iteration bounds check trips. Adversarial-input-only; addressed alongside chunk-11 `Check(CheckIndexes)`. Surfaced by chunk-7.3 Round-1 adversarial review (M-1 + L-2); promoted from inline forward-references at chunk-7.3 Round 2. |
