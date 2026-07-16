# Plan: pre-consumer engine changes

Spec: `docs/specs/limits.md`, `docs/specs/page-formats.md`,
`docs/specs/keyspaces.md`, `docs/specs/durability.md`,
`docs/specs/cross-process.md`, `docs/specs/transactions.md`,
`docs/specs/api-surface.md`.
Scope: the format break (long keys plus the page-layout and
overflow-digest backports from the sibling engine's measured
layout work — pando `docs/specs/file-format.md`, evidence via
`git log --all -- spike/leaflayout spike/branchlayout` there),
the commit-outcome and change-notification semantics, and the API
verbs to settle before external consumers exist (first consumer:
the gitfs metadata store). Format and error-contract breaks are
pre-v1 clean breaks (`development: true`, `.semrel.yaml`).
Cross-level separator truncation already exists
(`page-formats.md §Separator Computation`); the key cap it
cannot relax — two no-shared-prefix separators must fit one
branch page (`limits.md §Maximum Key Size`) — is what chunks 1–2
and 10 remove.

- [x] 1. Spec: long-key contract — overflow-key cell form for
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
- [x] 2. Overflow-key cells: format version bump; leaf and branch
      write, read, and compare paths (inline prefix first, chase
      the extent only on prefix tie); retirement of key extents
      on delete/split/merge through the RPL like value overflow;
      `Check` walks key extents.
- [x] 3. Spec: page-layout variants + overflow-run digest — the
      sibling-engine backport. Segregated leaf variant (headers +
      key bytes packed from the page front, values located by
      entry-order `VOff` in a separate region; overflow key-extent
      reference carried in the value region so the search region
      stays pure key bytes; `VOff` maintenance decides by entry
      order, never value address — zero-length values alias).
      Branch variants plain (full separators, offset+length
      directory) and segregated (shared prefix stored once,
      heap-relative offsets-only directory + sentinel, separate
      child-pointer array) replacing prefix-truncated branch keys
      (measured dominated: equal to plain at zero prefix, slower
      than plain and less dense than segregated otherwise).
      Per-variant overflow-separator markers; `T` re-derived from
      (PageSize, PageChecksum, declared layouts) keeping the
      two-overflow-cells floor; per-keyspace layout declaration;
      overflow runs move to a head-resident whole-run XXH3-64
      digest with footer-free followers (extent bytes contiguous);
      overflow-value ownership contract (borrowed single slice vs
      copied) settled. Defaults (leaf layout, branch layout,
      restart-group target 16→6) recorded as benchmark-gated.
      Amends `page-formats.md`, `checksums.md`, `limits.md`,
      `keyspaces.md`, `api-surface.md`.
- [x] 4. Segregated leaf implementation: encode/decode/lookup/
      splice/split, entry-order `VOff` maintenance, overflow-key
      and overflow-value forms in the value region, per-keyspace
      declaration + config plumbing, `Check`/`Validate` coverage;
      fuzz and property generator grammars extended to the
      variant.
- [x] 5. Plain branch: full-separator cells with offset+length
      directory replace the prefix-truncated form; overflow
      marker per the amended spec; separator computation, split,
      merge, and the landed overflow-separator paths re-anchored;
      generator grammars extended.
- [x] 6. Segregated branch: shared-prefix-once layout, heap-
      relative offsets-only directory with sentinel, child-pointer
      array, child-pointer-bit-63 overflow marker; per-keyspace
      declaration; generator grammars extended.
- [x] 7. Whole-run overflow digest: head-resident XXH3-64 over
      extent bytes, follower pages lose per-page footers,
      verification once per run per transaction cached on the
      pager; contiguous extent assembly and the settled
      overflow-value ownership contract; `Check` and relocation
      walkers updated.
- [x] 8. Layout benchmarks + defaults: spike-methodology corpus
      benchmarks over gmdb's real encodings; validate the
      spec-recorded defaults already in effect (segregated leaf —
      live since chunk 4 — segregated branch, restart-group
      target 6), confirming or reverting each, and pin the chosen
      constants by test.
- [x] 9. Append-aware splits: lopsided right-edge split point +
      rightmost-leaf hint so ascending-key workloads stop
      stranding half-full left pages; sequential-insert corpus
      and bench coverage.
- [x] 10. Long keys across the full surface: nested set-keyspace
      value trees (lifts the set-value bound), composite index
      keys, `BulkLoad`/extsort, `Check`/repair, incremental
      compaction relocation, `Compact`/`CopyTo`; fuzz and property
      generator grammars extended to long keys end to end.
- [x] 11. Commit-outcome classification: the engine's
      publication-failure semantics (`durability.md §Checkpoint
      failure semantics`, pager commit contract) exposed to
      callers as distinct sentinels — definitely-not-visible vs
      visible (meta-slot readback under the still-held grant) vs
      durability-indeterminate (failed final fsync); `durability.md`
      taxonomy pin, including "committed-visible" as the state the
      root version reports; crash-harness coverage for each class.
- [x] 12. `Insert` / `Replace` verbs (`ErrKeyExists` /
      `ErrNotFound`) on `Keyspace`, with `typed` mirrors; `Put`
      stays the upsert.
- [x] 13. `Cursor.SeekLE` / `SeekLT`, `SetCursor` key-level
      equivalents, `typed` cursor mirrors.
- [x] 14. `Version()` + `WaitVersion(ctx, from)` +
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
- [x] 15. Transaction dirty-page spill: `MaxTxBufferBytes` becomes
      the spill threshold, not a correctness ceiling; past it,
      CoW pages write out to their allocated file pages before
      commit (crash image identical to died-holding-grant; the
      existing grant-handoff tear detection and leak reclamation
      cover it); `ErrTxTooLarge` narrows to RPL-slab assembly;
      large-transaction and crash tests.
- [ ] 16. Sugar pass: `DeletePrefix` on both keyspace kinds; a
      generic struct value-encoder helper in `typed` (values need
      round-trip only, not order preservation).
