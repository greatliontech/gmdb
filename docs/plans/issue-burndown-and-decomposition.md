# Issue burndown & root decomposition

Burndown of every open issue doc (user-directed dispositions: RPL
relocation = full design + implementation; cross-NS liveness = longer
cross-namespace window; index move = clean codec extraction only) plus
the one decomposition the coupling audit sanctions. Chunks in
dependency order; `N.1` triage and close-out gates fixed per chunk.

- [ ] 1. Codename-residue sweep: rewrite the ~80 dead-plan chunk
  references across `range-delete.md`, `api-surface.md`,
  `indexing.md`, `keyspaces.md`, `transactions.md`,
  `page-formats.md` (+ remaining files listed in the issue) to
  descriptive references; resolves `plan-codename-residue`.
- [ ] 2. Leaked-DB cleanup drain: `dbCleanupFn` performs Close's
  inflight drain before coord/lockFile teardown; resolves
  `dbcleanup-teardown-drain`.
- [ ] 3. Shared writer-pager test fixture consumable across packages;
  kill the `internal/btree` duplicate; resolves
  `pager-test-helper-export`.
- [ ] 4. Open-path fsync fault-injection seam + tests: pin the
  anchor rewrite (drop-the-rewrite mutation must die) and cover
  recovery-commit fsync failure; resolves `anchor-rewrite-fsync-pin`.
- [ ] 5. Cross-namespace reader liveness window: longer cross-NS
  classification timeout (tunable, same-NS kill(0) unchanged) —
  spec amendment + `identityLive` + Options surface + tests;
  resolves `cross-namespace-reader-heartbeat-liveness`.
- [ ] 6. `internal/idxcodec`: extract the pure index codec
  (escape/encode/decode index keys, schema hash, registry-entry
  codec; `DecodeCoveringTuple` stays as a root wrapper); no API
  change (per the composition audit: the full `internal/index` is
  an anti-extraction; no other split/merge warranted).
- [ ] 7. RPL segment relocation — design (Spec-first):
  commit-pipeline fold per the issue's option 1; settle the
  chain-pointer cascade (relocating a segment rewrites its newer
  neighbor's `OlderSegment`, cascading toward the head), the
  evacuation trigger, and reclamation/recovery interplay; amend
  `free-space.md` + `background-maintenance.md`.
- [ ] 8. RPL segment relocation — implementation + tests; resolves
  `rpl-segment-relocation`; plan close-out.
