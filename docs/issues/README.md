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
| [refactor-design-to-specs](refactor-design-to-specs.md) | next session | Split monolithic `docs/design.md` (5155 lines) + `docs/set-keyspace.md` into structured `docs/specs/*.md` with explicit invariants; bootstrap `docs/plans/`. |
