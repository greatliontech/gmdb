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
| [pager-test-helper-export](pager-test-helper-export.md) | when chunk 5.3+ adds a second cross-package writer-pager fixture caller | `setupWriter` is duplicated into `internal/btree/pager_integration_test.go` as `setupPagerWriter` for the chunk-5.2 PageWriter parity test. Acceptable for one caller; factor when the second caller arrives. |
