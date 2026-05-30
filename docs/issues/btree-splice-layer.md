# Build the in-place leaf splice layer (no-decode insert/delete/split)

**Lands:** proactive — a specced-but-unbuilt performance optimization;
multi-session feature, port from grove. Decide the format-alignment fork
(below) first.

**Severity:** [perf] — not a correctness defect; a measured throughput +
allocation optimization. The current decode/re-encode path is correct.

**Source:** 2026-05-30, while investigating the spec's `FindSplitGroup` /
splice design. Justified by a CPU profile (below).

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

## DECISION 1 (do this first): format-alignment fork

gmdb's and grove's compressed-leaf formats are **NOT byte-identical**.
Inline entry field order:

- gmdb: `[Flags][KeyLen][ValueLen][Key][Value]` (restart) /
  `[Flags][SharedLen][UnsharedLen][ValueLen][UnsharedKey][Value]` (delta)
  — `ValueLen` **before** the key
  (`internal/page/leaf_compressed.go:59,119`).
- grove: `[Flags][KeyLen][Key][ValueLen][Value]` — value part **after**
  the key (`grove/.../leaf_compressed.go:524-542` + `writeValuePart`).

gmdb's ordering is **not** actually advantageous (its
"fixed-fields-before-variable-key" rationale at decodeDeltaEntry:81-85
holds for grove's layout too; grove's puts the key marginally closer to
the entry start for the search hot path). A verbatim port of grove's
splice onto gmdb's layout would **silently corrupt pages** (fuzz-only).

- **(a) Keep gmdb's format, adapt each helper** to its byte order
  (re-encode using gmdb's `LeafBuilder` primitives). No format change;
  delicate per-helper byte work.
- **(b) Align this encoding to grove's order first** (grove → gmdb; a
  contained, clean-break format change — gmdb is pre-v1, no installed
  base — plus a `page-formats.md` update), then port the splice
  near-verbatim. Safer port; future grove backports stay aligned.

Before choosing (b): run a **full gmdb-vs-grove compressed+uncompressed
format diff** to size total alignment (header offsets, overflow/nested
encoding, restart table, value part). gmdb's format is a *superset*
(SetKeyspace subpage / nested-tree cells) — align only the common parts
gmdb has no reason to differ on.

## Port plan (after Decision 1)

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
closes it; if Decision 1 picks (b), update the §Compressed Leaf entry
layout too.
