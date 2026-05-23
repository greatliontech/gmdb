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
| [keyspace-delete-missing-key](keyspace-delete-missing-key.md) | 5 | `Keyspace.Delete` / `SetKeyspace.Delete` / `SetKeyspace.DeleteValue` semantics on missing key — `ErrNotFound` vs silent no-op; needs a single decision applied uniformly. |
| [setkeyspace-put-added-bool](setkeyspace-put-added-bool.md) | 6 | Whether `SetKeyspace.Put` should return `(added bool, err error)`; the membership probe is already paid by the insert. |
| [tx-leak-deadlock](tx-leak-deadlock.md) | 3 | A leaked write `*Tx` holds `db.writeMu` indefinitely, wedging subsequent `Begin(write=true)`. Resolved by `runtime.AddCleanup`-based leak detection per `leak-detection.md`. |
| [open-meta0-corruption-probe](open-meta0-corruption-probe.md) | 11 | `Open` cannot recover from a torn `PageSize` field in meta 0 even when meta 1 is intact. A 5-candidate page-size probe at Open closes the gap. |
| [commit-failure-stale-meta-cache](commit-failure-stale-meta-cache.md) | 11 | A publication-phase commit failure (step 3/4 error) leaves `db.currentMeta` stale in memory while the on-disk active meta has advanced — the next in-process `Begin` writes a meta with a duplicate non-zero TxnID. Fixed by re-reading meta from disk on commit failure or poisoning the DB handle. |
| [corruption-sentinels-not-routed](corruption-sentinels-not-routed.md) | 11 | `ErrCorrupted`, `ErrBadPageChecksum`, `ErrVersionMismatch` are declared in `errors.go` but never returned; pager corruption paths surface as `fmt.Errorf` wraps. `mapPagerErr` must route corruption errors to the sentinels for `errors.Is` to work. |
| [bitmap-rollback-undo-log](bitmap-rollback-undo-log.md) | when profiling shows BeginTx allocation pressure is material | `Bitmap.Snapshot()` clones the full detail+summary per `Pager.BeginTx()` — 8 MB at 256 GB MaxSize. Replace with an undo log if profiling shows the per-tx allocation is hot. |
