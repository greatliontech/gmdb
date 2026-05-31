# Build the in-place leaf splice layer (no-decode insert/delete/split)

**Lands:** proactive — a specced-but-unbuilt performance optimization;
multi-session feature, port from grove. The format decision is **settled**
(keep gmdb's format — see DECISION 1, RESOLVED). Chunks 1-4 (`TryAppend`,
`TryInsertAt`, `TryDeleteAt` compressed + the uncompressed variants) are
**landed** — see Progress below; resume by porting `trySplitLeafByGroup` (the
no-decode split) next.

**Severity:** [perf] — not a correctness defect; a measured throughput +
allocation optimization. The current decode/re-encode path is correct.

**Source:** 2026-05-30, while investigating the spec's `FindSplitGroup` /
splice design. Justified by a CPU profile (below).

## Progress

- **Chunk 1 — `TryAppend` (compressed): LANDED.**
  `internal/page/leaf_splice.go` (`TryAppend` dispatcher +
  `tryAppendCompressed`); the per-entry byte layout is extracted to
  `writeCompressed{Restart,Delta}Entry` in `leaf_builder.go` (single source of
  truth, shared with `addCompressedEntry`); wired as the append fast path in
  `putReportCore` (`internal/btree/put.go`), falling back to decode/re-encode
  on decline. Determinism/parity test + `FuzzTryAppendCompressed` + ported
  invariant tests in `leaf_splice_test.go`. Bench (`BenchmarkPutSteadyState`,
  val=200): ~34% faster (7721→5128 ns/op), 4× fewer B/op (11211→2836), 2×
  fewer allocs/op (51→24).
  - **Two gmdb-specific divergences from grove (each verified against grove's
    actual source, not inferred):**
    1. `isRestart` includes the natural-break clause `shared == 0` — gmdb's
       builder applies it (`leaf_builder.go` `addCompressedEntry`), grove's does
       not (`grove .../leaf_compressed.go:421`). A verbatim port would break the
       determinism invariant whenever a zero-shared-prefix key lands in a
       non-full last group.
    2. the dispatcher declines on a compressed↔uncompressed variant mismatch
       (`EffectiveRestartGroupTarget() == 1` on a compressed page) so the leaf
       migrates via the rebuild path. gmdb has **mutable** RGT via
       `Tx.SetKeyspaceConfig` (keyspaces.md); grove's RGT is set once at create
       and is immutable, so grove never faces this and its dispatcher has no
       such guard.
  - **Known, spec-consistent (not a fault):** a *within-compressed* RGT change
    (e.g. 16→8) lets an append keep the page's existing group sizing while
    grouping the new entry per the new target; the page stays valid and
    read-correct but is not byte-reproduced by a fresh rebuild until it is next
    split/merged/rebuilt. keyspaces.md makes RGT a builder hint and lets
    existing leaves keep their structure; Check() validates structurally rather
    than re-encoding leaves, so nothing relies on fresh-rebuild reproducibility
    across an RGT change. Documented in `leaf_splice.go`'s header.
- **Chunk 2 — `TryInsertAt` (compressed): LANDED.**
  `internal/page/leaf_splice.go` (`TryInsertAt` + `tryInsertAtCompressed` +
  `entryTrailer`); wired as the mid-page insert fast path in `putReportCore`
  (`!searchFound && searchIdx < leafCount`). Semantic/structural fuzz
  (`FuzzTryInsertAtCompressed`, mixed cell kinds) + I-B/I-C/I-D + cap/full/
  variant invariant tests + `BenchmarkTryInsertAtCompressed`. End-to-end
  `BenchmarkPutRandom` (random prefix-sharing keys → interior inserts): ~41%
  faster (8056→4759 ns/op), 4× fewer B/op (9877→2497), ~2× fewer allocs/op
  (46→22). `task fuzz` now iterates each `Fuzz*` target (Go fuzzes one per run).
  - **DESIGN — localized (non-canonical), data-driven.** A canonical "insert
    == LeafBuilder rebuild" splice was rejected by measurement: the builder
    fills groups to `target` with no balancing, so after any build every group
    is full and a canonical insert would decline ~100% of the time (measured
    ~0% fast-path vs ~100% for the localized design). So `TryInsertAt` grows the
    containing group in place up to `min(2*target, 255)` and re-encodes only the
    displaced successor — a valid compressed leaf a fresh rebuild would group
    differently. Sanctioned by page-formats.md §Insert ("may shift the
    containing group's boundaries"; Count ∈ [1,255]); the determinism invariant
    governs builder output, and `Check()` validates structurally. Its guarantee
    is **semantic + structural** (decode == expected sequence; `Validate`;
    free-space zeroed), not byte-identity — so the test oracle is NOT the full
    rebuild used for `TryAppend`.
  - **Two gmdb-specific divergences from grove (verified against grove's
    source):** (1) cap `min(2*target, 255)` — gmdb's `MaxRestartGroupTarget` is
    255 so `2*target` can reach 510; grove's bare `2*restartTarget` would let a
    group exceed the uint8 `Count` and corrupt it. (2) ONE contiguous
    `[succEnd, dataEnd)` shift instead of grove's two copies — simpler and fixes
    a latent grove bug where, for a net-shrink insert (`byteDelta < 0`) with
    trailing in-group entries, grove's first copy clobbers the second copy's
    source.
- **Chunk 3 — `TryDeleteAt` (compressed): LANDED.**
  `internal/page/leaf_splice.go` (`TryDeleteAt` dispatcher + `tryDeleteAtCompressed`,
  four cases D-A/D-B/D-C/D-D); wired as the delete fast path in `deleteFromLeaf`
  (`internal/btree/delete.go`), which now validates once + `SearchLeaf` once, then
  forks fast (splice) / slow (decode-rebuild) with the slow path reusing the
  SearchLeaf index. Semantic/structural fuzz (`FuzzTryDeleteAtCompressed`, mixed
  cell kinds) + position (D-A×3 / D-B / D-C / D-D / overflow) + AlwaysShrinks +
  ReencodingGrowthIsBounded + empty/variant/uncompressed/checksum tests +
  `BenchmarkTryDeleteAtCompressed`. End-to-end `BenchmarkDeleteRandom` (val=200):
  ~23% faster (9263→7146 ns/op), ~1.9× fewer B/op (10257→5485), fewer allocs/op
  (112→95) — diluted by the merge/empty tail both paths share; the micro-bench
  shows the pure splice at 3 allocs/op.
  - **DESIGN — localized (non-canonical), like `TryInsertAt`** → same semantic +
    structural oracle (decode == expected sequence; `Validate`; free-space
    zeroed), NOT byte-identity. A delete ALWAYS shrinks the page (no fit-check,
    always succeeds for count>1): the removed entry outweighs the successor's
    re-encode growth by the shared-prefix triangle inequality on sorted keys —
    verified `byteDelta ≤ −5 − valuePart(removed)` for both D-B and D-C, and the
    successor's value-part cancels so subpage/nested/large-value cells are
    irrelevant. D-A (`gc==1`, single-entry group) is the only case that changes
    RestartCount.
  - **Three gmdb-specific divergences from grove (verified against grove's
    source):** (1) `bool` return vs grove's `int`/−1 ("page would empty, caller
    frees it") — gmdb's `deleteFromLeaf` has a decode-rebuild fallback that
    already frees an emptied leaf, migrates a stale variant, and recomputes
    underflow via `leafUnderflow`, so the freed-space value and empty code are
    unneeded; a decline routes to that existing slow path. (2) ONE contiguous
    left-shift `[spliceEnd, dataEnd)` vs grove's two copies (within-group-trailing
    + later-groups) — provably equivalent (the two regions are adjacent), and
    consistent with `TryInsertAt`. grove's two-copy *delete* is actually correct
    (unlike its two-copy insert); this is a simplicity divergence, not a bug-fix.
    (3) `bytes.Clone` capture of the predecessor/successor key+value vs grove's
    scratch-page stash.
- **Chunk 4 — uncompressed variants (`ucTryAppend` / `ucTryInsertAt` /
  `ucTryDeleteAt`): LANDED.** `internal/page/leaf_splice.go` (three `ucTry*`
  helpers; each dispatcher `TryAppend`/`TryInsertAt`/`TryDeleteAt` gained a
  variant switch — splice only when the page's on-disk variant matches the
  configured one, else decline → rebuild migrates). Shared `writeUCEntry`
  extracted in `leaf_builder.go` (single source of truth, used by `addUCEntry` +
  the uc splices). Append/insert reuse the existing put-fast-path wiring
  unchanged; delete relaxed its gate to `r.Count() > 1` (`internal/btree/
  delete.go`) so uncompressed leaves reach the splice. Byte-identity oracles
  (`assertUC{Insert,Delete}MatchesRebuild`) + position/variant/empty tests +
  `FuzzUCTry{Append,InsertAt,DeleteAt}` + micro-benches +
  `TestPutUncompressedInteriorInsert` (end-to-end, exercises `ucTryInsertAt` at
  100% via put.go). Micro-bench: **0 allocs/op** (in-place, zero-alloc).
  - **DESIGN — CANONICAL in-place (diverges from grove).** An uncompressed leaf
    is a sorted, packed entry array + sorted offset table, so all three splices
    are array-style edits (shift the data tail, splice one offset slot) that are
    **byte-identical to a LeafBuilder rebuild** — unlike the compressed
    insert/delete (localized/semantic), these get the byte-identity oracle.
    grove rebuilds insert/delete through a page-sized **scratch** buffer; gmdb's
    in-place form is consistent with the compressed splices (no scratch param)
    and zero-alloc. page-formats.md §Insert and Delete sanctions it ("the
    per-entry data shifts but no key re-encoding occurs").
  - **Three gmdb-specific divergences from grove (verified against grove's
    source):** (1) canonical in-place vs grove's scratch-rebuild (insert/delete);
    (2) all three return `bool` (delete: vs grove's `int`/−1 — gmdb's
    `deleteFromLeaf` fallback frees an emptied leaf); (3) the pre-staged
    `ucSkipEntry` was **deleted** — it had zero callers and was buggy (read
    inline `ValueLen` after the key, grove's order, and mishandled nested cells),
    and the canonical splices don't need it (the sorted+packed offset table gives
    entry extents directly).
- **Resume: port `trySplitLeafByGroup` next** (the final chunk — no-decode
  split). Read grove `splitLeafAtGroupCompressed` / `findSplitGroupCompressed`
  (`leaf_compressed.go`) + the uncompressed split path IN FULL first. This is the
  no-decode leaf split: pick a group/entry boundary near the byte-size bias and
  carve the page into two without decoding+re-encoding all entries. Relates to
  `btree-byte-balanced-split.md` (finding-19 branch path + the byte-balanced
  `FindSplitGroup`/`FindSplitIndex` the spec mandates) — check that issue at the
  chunk-start gate. Wiring anchor: the split path in `internal/btree/put.go`
  (`ascendWithSplit` / the leaf-overflow → split branch).
- **Shared infrastructure to REUSE (chunks 1-4; don't reinvent):** entry encode
  — `writeCompressed{Restart,Delta}Entry` (compressed) / `writeUCEntry`
  (uncompressed) / `entryTrailer` / `valuePartSize` / `cellHasTrailerOnly`
  (`leaf_builder.go` / `leaf_splice.go`); the group walk — `decodeRestartEntry` /
  `decodeDeltaEntry` with a reused keyBuf + cloned captured keys; the variant
  switch — splice only when the page's on-disk variant matches the configured one
  (compressed ⇔ RGT≥2, uncompressed ⇔ RGT==1), else decline → rebuild migrates
  (in all three dispatchers); the delete wiring's validate-once + `SearchLeaf`-
  once + fast/slow fork (`deleteFromLeaf`); test helpers in `leaf_splice_test.go`
  — `tryBuild`, `assertLeafDecodesTo` (semantic oracle, compressed insert/delete),
  the byte-identity oracles `assertAppendMatchesRebuild` /
  `assertUC{Insert,Delete}MatchesRebuild` (canonical splices), `assertFreeSpace-
  Zeroed`, `insertExpected`/`deleteExpected`, `randomFittingMixed`,
  `randomLeafEntry`, `sortedInsertIdx`, `keyBetween`. `task fuzz` iterates all six
  `Fuzz*` targets. For the split chunk, the uncompressed reader paths
  (`ucOffset`/`ucDecodeEntry`) and `compressedLastKey` are the boundary-key
  analogs.

## Why (measured)

`putReportCore` decodes the whole target leaf (`readLeafEntriesDeepCopy`)
and re-encodes it via `LeafBuilder` on **every** insert. Profile of
`BenchmarkPutSteadyState` (val=200, 7708 ns/op, 11172 B/op, 51 allocs/op):

- `readLeafEntriesDeepCopy` = **~26% of put time**, and its per-entry
  `bytes.Clone` of key+value drives **~50 allocs/op / 11 KB**.

An in-place splice (shift bytes, encode only the new/displaced entry,
update the restart table) eliminates that for the common fits-in-place
insert/delete → estimated **~25-35% faster puts, ~5-10× fewer allocs**.
`bench_overflow_test.go` / `bench_put_test.go` are the before/after
harness (`task bench`).

## Reference: grove built and wired this; gmdb did not

gmdb's page godocs *reference* "the splice helpers" and the free-space /
offset accessors were built for them, but the helpers were never written;
`putReportCore` always decodes. **grove implemented the whole layer**
(verified — read these before porting):

- `grove/internal/page/leaf_compressed.go`: `tryAppendCompressed:486`,
  `tryInsertAtCompressed:595` (I-B/I-C/I-D insert cases documented at
  577-594), `tryDeleteAtCompressed:851`.
- `grove/internal/page/leaf_uncompressed.go`: `ucTryAppend:39`,
  `ucTryInsertAt:84`, `ucTryDeleteAt:152`.
- `grove/internal/page/leaf.go`: `TryAppend:761` / `TryInsertAt:775` /
  `TryDeleteAt:791` dispatchers.
- Wiring (fast-path + decode fallback): `grove/internal/btree/put.go:192`
  (`TryAppend`), `:206` (`TryInsertAt`), `:218` (`trySplitLeafByGroup`);
  `grove/internal/btree/del.go:140` (`TryDeleteAt`).
- Port the invariant tests too (e.g. `TestTryDeleteAtAlwaysShrinks`).

Grove is the reference for the **algorithm**, not gospel — it is
use-case-simplified (e.g. its `splitLeaf` has no feasibility check / no
overflow promotion — gmdb's A fix covers that). Read grove per the
per-feature reference approach; verify against gmdb's spec.

## DECISION 1: RESOLVED — keep gmdb's format (measured)

gmdb and grove differ in **exactly one** place: the inline entry's
`ValueLen` (u32) sits **before** the key in gmdb
(`[Flags][KeyLen][ValueLen][Key][Value]` /
`[Flags][SharedLen][UnsharedLen][ValueLen][UnsharedKey][Value]`,
`internal/page/leaf_compressed.go:59,119` + `leaf_uncompressed.go:25,67`)
and **after** the key in grove. Consistent across compressed *and*
uncompressed leaves. Everything else is byte-identical: restart table
`[Offset u16][Count u8][Reserved u8]`, header offsets
`leafOffRestartCount=8`/`leafOffDataEnd=10`, overflow + nested cells
(`[…key…][u64][u64]`), and the prefix-delta scheme.

A microbenchmark isolating that one variable
(`internal/page/layout_bench_test.go`, median of 10 runs) shows gmdb's
order is **~24-26% faster for full-leaf decode** and a wash for search:

| | gmdb | grove |
|---|---|---|
| iterate val=16 / 256 | ~138 / ~139 ns | ~181 / ~187 ns |
| search val=16 / 256 | ~126 / ~132 ns | ~129 / ~135 ns |

Reason: with all length fields in the fixed header, the decoder computes
the next-entry offset without waiting on the key copy (ILP); grove's
`ValueLen`-after-key serializes each entry's decode. That decode cost is
paid by every non-spliced read and the splice fallback, so it is real.

**Decision: KEEP gmdb's format.** Aligning to grove (the earlier option
b) is rejected — it would regress decode ~24% to save port effort.

**Consequence for the port (good news):** the divergence is one localized
field position, so porting each grove splice helper means adapting only
the **inline value-part write** to gmdb's order (`[ValueLen][Key]`, not
`[Key][ValueLen]`) — reuse gmdb's `LeafBuilder` entry-encode for that
write. The restart-table updates, byte-shift logic, overflow handling,
and I-B/I-C/I-D insert structure port as-is. Adaptation is contained, not
pervasive.

## Resume — read these first, in order (open the code; don't infer)

**gmdb format** (the target byte layout the splice must produce):
- `internal/page/leaf_compressed.go`: `decodeRestartEntry:31`,
  `decodeDeltaEntry:86` — exact entry byte layout incl. gmdb's
  `ValueLen`-before-key order (the only thing that differs from grove).
- `internal/page/leaf_builder.go`: `LeafBuilder.addEntry` +
  `valuePartSize` — how gmdb encodes a single entry; **reuse this** for
  the splice's new/displaced-entry write, don't hand-roll bytes.
- `internal/page/leaf.go:32-38` (`leafEntryStart=12`,
  `leafOffRestartCount=8`, `leafOffDataEnd=10`); `restart.go`
  (restart-table accessors); `leaf_uncompressed.go` (uncompressed form +
  the already-present `ucSkipEntry:167` scaffolding built for this).

**grove algorithm**: read the matching helper before porting it (paths in
the Reference section above). Don't infer from names/commits — open it.

**gmdb wiring anchors** (where the fast-path goes):
- Insert: `internal/btree/put.go:143` `putReportCore` — after the leaf is
  found + CoW'd, try the splice; on `false` fall through to the existing
  `readLeafEntriesDeepCopy` (~put.go:207) decode path. (A's change folded
  fit/split/promote into one loop; the splice goes *before* that decode,
  leaving the loop as the fallback.)
- Delete: the single-key leaf-mutation site in `internal/btree/delete.go`
  (gmdb analog of grove's `del.go:140`).

## Determinism test recipe (the safety net for every helper)

The spliced page MUST be byte-identical to a decode-re-encode of the same
logical entries (page-formats.md determinism invariant, :566-579):
1. Build the original leaf via `LeafBuilder` from entries E.
2. `expected` = decode E → apply the op to the entry slice → re-encode
   via `LeafBuilder` (the proven slow path).
3. `actual` = copy the original page bytes → apply the splice helper.
4. Assert the helper returned true AND `bytes.Equal(actual, expected)`.
5. Also exercise the false-return cases (page full, group boundary) — the
   helper declines and the fallback handles it.

Add a `Fuzz*` target in `internal/page` (random sorted entries + random
op) asserting the same equality; `task fuzz` already runs that package.

## Port plan

Incremental, **fast-path + fallback**: each helper returns false on any
case it can't handle (page full → split, tricky boundary) and the
existing decode/re-encode path runs — so correctness never depends on the
splice. Wire as a fast-path at the top of `putReportCore` (and the delete
path) before `readLeafEntriesDeepCopy`.

Order (each its own commit): `TryAppend` (sequential inserts) →
`TryInsertAt` → `TryDeleteAt` → uncompressed variants →
`trySplitLeafByGroup` (no-decode split).

Each helper gets a **determinism/parity property test** — the spliced
page must be **byte-identical** to a decode-re-encode of the same logical
entries (page-formats.md §Leaf Split determinism invariant, :566-579) —
plus the existing `task fuzz` target (`internal/page`). Re-run `task
bench` for before/after.

## Spec note

`page-formats.md §Insert and Delete` (:531-538) already describes the
splice helpers as if they exist — a spec-vs-code gap. Implementing this
closes it (no §Compressed Leaf layout change — gmdb's format is kept per
DECISION 1).
