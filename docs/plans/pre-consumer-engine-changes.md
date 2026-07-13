# Plan: pre-consumer engine changes

Spec: `docs/specs/limits.md`, `docs/specs/page-formats.md`,
`docs/specs/keyspaces.md`, `docs/specs/durability.md`,
`docs/specs/cross-process.md`, `docs/specs/transactions.md`,
`docs/specs/api-surface.md`.
Riders: `change-notification-wait-primitive` (chunk 7).
Scope: the format break (long keys), the commit-outcome and
change-notification semantics, and the API verbs to settle before
external consumers exist (first consumer: the gitfs metadata
store). Format and error-contract breaks are pre-v1 clean breaks
(`development: true`, `.semrel.yaml`). Cross-level and within-page
separator truncation already exist (`page-formats.md
§Prefix-Truncated Branch Keys`); the key cap they cannot relax —
two no-shared-prefix separators must fit one branch page
(`limits.md §Maximum Key Size`) — is what chunks 1–3 remove.

- [ ] 1. Spec: long-key contract — overflow-key cell form for
      leaf keys AND branch separators (inline prefix +
      extent reference, reusing the existing run-length overflow
      mechanism), inline-prefix threshold, minimum-fanout
      invariant restated over the inline portion (two overflow
      separators must fit a branch by construction); new limits
      table (max key bounded by the overflow mechanism, not page
      size; set-keyspace value bound lifted by the same
      mechanism); decide the checksum rider (xxhash64 → XXH3,
      same 8-byte footer, benchmark-gated) while the format
      version bumps anyway; amendments to `limits.md`,
      `page-formats.md`, `keyspaces.md`.
- [ ] 2. Overflow-key cells: format version bump; leaf and branch
      write, read, and compare paths (inline prefix first, chase
      the extent only on prefix tie); retirement of key extents
      on delete/split/merge through the RPL like value overflow;
      `Check` walks key extents.
- [ ] 3. Long keys across the full surface: nested set-keyspace
      value trees (lifts the set-value bound), composite index
      keys, `BulkLoad`/extsort, `Check`/repair, incremental
      compaction relocation, `Compact`/`CopyTo`; fuzz and property
      generator grammars extended to long keys end to end.
- [ ] 4. Commit-outcome classification: the engine's
      publication-failure semantics (`durability.md §Checkpoint
      failure semantics`, pager commit contract) exposed to
      callers as distinct sentinels — definitely-not-visible vs
      visible (meta-slot readback under the still-held grant) vs
      durability-indeterminate (failed final fsync); `durability.md`
      taxonomy pin, including "committed-visible" as the state the
      root version reports; crash-harness coverage for each class.
- [ ] 5. `Insert` / `Replace` verbs (`ErrKeyExists` /
      `ErrNotFound`) on `Keyspace`, with `typed` mirrors; `Put`
      stays the upsert.
- [ ] 6. `Cursor.SeekLE` / `SeekLT`, `SetCursor` key-level
      equivalents, `typed` cursor mirrors.
- [ ] 7. `Version()` + `WaitVersion(ctx, from)` +
      `WaitKeyspaceVersion(ctx, name, from)`: committed-visible
      root version; the notification region in the shared lock
      file is a fixed array of counter words — slot 0 bumps on
      every commit (the global wait), slots 1..K bump per touched
      keyspace by name-hash (commit knows its touched keyspaces
      from the dirty-descriptor set; bump = atomic increment +
      wake). Futex wait on the slot word on Linux, adaptive-poll
      fallback elsewhere; spurious wakeups allowed — hash
      collisions included — callers re-check; cross-process wake
      tests for both scopes; `cross-process.md` spec section.
- [ ] 8. Transaction dirty-page spill: `MaxTxBufferBytes` becomes
      the spill threshold, not a correctness ceiling; past it,
      CoW pages write out to their allocated file pages before
      commit (crash image identical to died-holding-grant; the
      existing grant-handoff tear detection and leak reclamation
      cover it); `ErrTxTooLarge` narrows to RPL-slab assembly;
      large-transaction and crash tests.
- [ ] 9. Sugar pass: `DeletePrefix` on both keyspace kinds; a
      generic struct value-encoder helper in `typed` (values need
      round-trip only, not order preservation).
