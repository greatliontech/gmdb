# Issue burndown & root decomposition

Burndown of every open issue doc plus the one decomposition the
coupling audit sanctions. Ordered bottom-up by functionality with
higher-value work first (user-directed); findings fold into this plan
under the same heuristic — no new issue docs. User dispositions: RPL
relocation = full design + implementation; cross-NS liveness = longer
cross-namespace window; index move = clean codec extraction only.
`N.1` triage and close-out gates fixed per chunk.

- [x] 2. RPL segment relocation — design (Spec-first): commit-pipeline
  fold per the issue's option 1; settle the chain-pointer cascade
  (relocating a segment rewrites its newer neighbor's
  `OlderSegment`, cascading toward the head), the evacuation
  trigger, and reclamation/recovery interplay; amend
  `free-space.md` + `background-maintenance.md`.
- [x] 3. RPL segment relocation — implementation + tests; resolves
  `rpl-segment-relocation`.
- [ ] 4. Cross-namespace reader liveness window: longer cross-NS
  classification timeout (tunable, same-NS kill(0) unchanged) —
  spec amendment + `identityLive` + Options surface + tests;
  resolves `cross-namespace-reader-heartbeat-liveness`.
- [ ] 5. Open-path fsync fault-injection seam + tests: pin the
  anchor rewrite (drop-the-rewrite mutation must die) and cover
  recovery-commit fsync failure; resolves `anchor-rewrite-fsync-pin`.
- [ ] 6. Leaked-DB cleanup drain: `dbCleanupFn` performs Close's
  inflight drain before coord/lockFile teardown; resolves
  `dbcleanup-teardown-drain`.
- [ ] 7. `internal/idxcodec`: extract the pure index codec
  (escape/encode/decode index keys, schema hash, registry-entry
  codec; `DecodeCoveringTuple` stays as a root wrapper); no API
  change (per the composition audit: the full `internal/index` is
  an anti-extraction; no other split/merge warranted).
- [ ] 8. Shared writer-pager test fixture consumable across packages;
  kill the `internal/btree` duplicate; resolves
  `pager-test-helper-export`; plan close-out.
- [x] 1. Codename-residue sweep (polish tier; landed first while the
  reorder was directed mid-flight): resolved
  `plan-codename-residue`, renamed the codename-bearing test file,
  deleted two orphaned functions and the superseded
  `docs/handoff.md`.
