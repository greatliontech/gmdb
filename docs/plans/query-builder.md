# Plan: structure phase + typed columns + query builder

Spec: `docs/specs/typed-columns.md`, `docs/specs/query-builder.md`.
Chunks 1–6 and 8 are
behavior-free structure moves except the named exported knobs;
tests move with their subjects, unmodified.

- [x] 1. `closeGate` → `internal/closegate` (zero-coupling sync
      primitive; pure move).
- [x] 2. Keyspace-descriptor codec (40-byte encode / decode /
      validate) → `internal/` (home decided in-chunk, beside the
      registry codec; pure move).
- [x] 3. Index codec consolidation into `internal/indexing`:
      set-keyspace compound-PK codec, `schemaHash` over a decl
      projection struct, `indexEntryKey` / `indexEntryValue` /
      `decodeUniqueIndexValue`, candidate-set collision check,
      `diffEntrySets` carve-out from `buildReplacePlans` (pure
      moves; extractor call sites stay in root).
- [x] 4. External merge sort (`indexSorter`, spill runs,
      `recordHeap`, `sortMerger`, cascade merging) →
      `internal/extsort` (pure move; no Tx/pager/cursor
      references).
- [x] 5. Compaction relocation core (`compactionWriter` +
      evacuation-floor math) → `internal/` (pure move; drivers
      stay in root).
- [x] 6. Check verifier (`checker` + page walkers) + stats
      page-walker → `internal/` behind a verifier-input struct
      (pgr + meta + coord snapshot); exported `Check*` types and
      entry points stay in root (pure move + one input-struct
      seam).
- [x] 7. Index-kind format groundwork: registry entry v2 (kind
      discriminator + length-prefixed per-kind payload for
      future extra roots / stats heads), `IndexDecl.Kind`
      (composite = the only kind), both folded into the schema
      hash; `pinnedIndex` / snapshot / flush ripple. Spec
      amendment riding: `indexing.md §Storage Layout` + §Drift
      Guard (requirements folded from the parked issue doc at 7.1).
      Pre-v1 clean break of the registry-entry encoding —
      ratified 2026-07-11.
- [x] 8. `gmdb/typed` extraction: move typed tier + encoders out
      of root; exported knobs `Keyspace.GuardIterConstruction` /
      `SetKeyspace.GuardIterConstruction`, `IndexHandle.Decl`,
      `IndexHandle.EnableCoverValueReturn` (+ the sentinel contract
      hoisted to internal/indexing — decided in-chunk);
      `Typed*` → `typed.*` renames. Spec amendments riding:
      `typed-keyspaces.md` (renames + knobs), `api-surface.md`,
      name cascade into `typed-columns.md` /
      `query-builder.md`.
- [x] 9. Reverse iteration `IterOption` on `Lookup` /
      `LookupKeys` / `Range` / `Prefix`; amendment clauses merge
      into `indexing.md §Lookup API` + `api-surface.md`;
      property test: reverse sequence == exact reversal of
      forward sequence over same snapshot, invalidation contract
      (Inv-IHS1..5) exercised on reverse walks.
- [x] 10. Column declaration tier (lands in `gmdb/typed`):
      `Column` / `MultiColumn` / `AnyColumn` /
      `AnySingleColumn` (sealed) / `ColumnIndex` implementing
      the sealed lowering; synthesized-name grammar (reserved
      `gmdb/col/` / `gmdb/multicol/` namespace) +
      `ErrIndexEncoderIDReserved` validation; extractor
      synthesis (multiset Cartesian expansion, no tier-side
      dedup, `Where` gating, panic-on-encode-error). Spec
      amendments riding: `typed-keyspaces.md §Limitation`
      pointer + Encoder-ID reserved-namespace clause, sealing
      parenthetical, `indexing.md` typed-caller pointer,
      `api-surface.md` rider. Tests: Inv-TC2 fingerprint +
      cross-form distinctness, Inv-TC4 compilation equivalence
      property (incl. duplicate-element unique-violation
      anchor), unique×multi element semantics.
- [x] 11. Typed covering projections + full-row `CoverValue`:
      `Covering` (`AnySingleColumn`), `CoverValue`/`Covering`
      mutual exclusion (`ErrInvalidOptions`), `Projection`,
      `Column.From` / `ErrColumnAbsent`. Spec amendments riding:
      `typed-keyspaces.md §Covering` "only covering shape"
      clause; `api-surface.md` rider. Tests: Inv-TC5 round-trip
      + covering-rewrite-on-update anchor.
- [x] 12. `gmdb/query` package + typed-homed declaration-tier
      types (`Term` / `OrderKey`, constructors as `Column`
      methods, free `Or`): term constructors, predicate
      representation (shared `internal/` package readable by
      both the decl tier and `gmdb/query` — term internals stay
      unexported in the public surface), scan-only execution
      with residual encoded-byte evaluation (Inv-QB2), `All` /
      `Keys` / `Limit` / `Offset` / `Err`; `api-surface.md`
      rider. Tests: Inv-QB2 anchors (NaN-safe float, folding
      encoder), equivalence-harness scaffold + generator grammar
      v1 (schemas, corpora, term lists).
- [ ] 13. Planner: single-index selection (EQ-prefix + one
      trailing range via `Range` partial-tuple prefix-bounds),
      full scoring rule, partial-index exclusion (rule 7),
      `Scan` / `IndexSeek` / `IndexPrefix` / `IndexRange` /
      `ResidualFilter` nodes, fresh-handle-per-leaf execution
      discipline over the `ByteIndex` typed→byte bridge (spec
      amendments riding: `typed-keyspaces.md` + `api-surface.md`
      bridge clauses), `Explain`. Plan/scan equivalence property
      (Inv-QB1) goes live; grammar covers index shapes incl.
      `Where`-partial (never chosen, results correct) + plan
      pinning via Explain.
- [ ] 14. Covering-aware execution: `Select` / `Rows`,
      index-only plans, CoverValue route, Inv-QB3 / Inv-QB7
      anchors; grammar adds covering mixes.
- [ ] 15. Combiners: `Or` pushdown via `Union`, `Intersect`,
      distinct-by-PK everywhere (Inv-QB4); grammar adds
      disjunction + overlap-dedup anchors.
- [ ] 16. Ordering + bounds: `OrderBy` (streaming asc/desc via
      chunk-9 surface, else `TopK` / `Sort`), materialization
      budget + `ErrQueryMaterializeLimit` (Inv-QB6), `Count`,
      directional PK tie-break determinism (Inv-QB5) enforced
      across all ordered nodes; grammar adds orderings, budgets,
      equal-key limit boundaries.
