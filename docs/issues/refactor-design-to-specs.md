# Refactor design docs into structured specs/plans/issues

**Lands:** next session — refactor work is the next chunk; no other
work depends on it landing here.

## Problem

The settled gmdb design currently lives in two monolithic files:

- `docs/design.md` — 5155 lines, 17 top-level sections, mixes
  spec, rationale, implementation hints, and API surface.
- `docs/set-keyspace.md` — companion doc for the keyspace-type
  split, partially obsolete after three review rounds rewrote the
  related sections of `design.md`.

Both documents reached their current state via a heavyweight
rewrite + three adversarial review rounds (commit `5fbe71d`). The
content is good; the *structure* is now the bottleneck:

- **Invariants are implicit.** Per CLAUDE.md (`Project invariants`),
  every domain concept should have its invariants stated explicitly
  using the
  `Invariant: kind=<clause-explicit|entailed>; property=<…>; from=<…>; violation=<…>`
  format. The current design states most invariants as prose
  scattered through long sections. A future fresh-eyes reviewer (or
  implementer) cannot enumerate them.
- **Plans live nowhere.** Per CLAUDE.md (`Conventions`), a Plan is
  the implementation roadmap derived from the Spec, broken into
  chunks. No `docs/plans/` exists.
- **Issues live nowhere yet.** `docs/issues/` was just created with
  this file as its first entry.
- **One file is hard to read/edit/cite.** A spec change to one
  concept forces context across the whole 5155-line doc; a reviewer
  cannot scope-limit a session to a single concern.

## Target structure

```
docs/
├── issues/
│   ├── README.md                          # index
│   └── <slug>.md                          # one per tracked follow-up
├── plans/
│   └── <feature-or-chunk-set>.md          # implementation roadmaps
└── specs/
    ├── 00-overview.md
    ├── 01-file-layout.md
    ├── 02-page-formats.md
    ├── 03-set-keyspace.md
    ├── 04-range-delete.md
    ├── 05-free-space.md
    ├── 06-pager-slab.md
    ├── 07-transactions.md
    ├── 08-leak-detection.md
    ├── 09-cross-process.md
    ├── 10-lock-ordering.md
    ├── 11-mmap-strategy.md
    ├── 12-durability.md
    ├── 13-file-format.md
    ├── 14-keyspaces.md
    ├── 15-indexing.md
    ├── 16-bulkload.md
    ├── 17-api-surface.md
    ├── 18-typed-keyspaces.md
    ├── 19-checksums.md
    ├── 20-integrity.md
    ├── 21-background-maintenance.md
    ├── 22-limits.md
    └── 23-implementation-layout.md
```

The proposed split is a starting point — the refactor session may
merge/split files based on what reads naturally. Constraints below
matter more than the exact file boundaries.

## Spec file conventions (load-bearing)

Each `docs/specs/*.md` file MUST:

1. **Open with a one-paragraph scope statement** — what concept this
   spec covers, what it does NOT cover, and which sibling specs it
   depends on or interacts with.

2. **Declare its invariants explicitly** in a dedicated `## Invariants`
   section near the top, in the CLAUDE.md format:

   ```
   Invariant: kind=<clause-explicit|entailed>;
     property=<the property that must hold>;
     from=<source spec clause | "entailed: <one-line justification>">;
     violation=<reachable in-spec input/state → wrong/unsafe result>
   ```

   Each invariant must have a statable `violation=`. An invariant
   with no statable violation is a preference — do not record it
   (Quality bar).

   Examples already implicit in the current design that should be
   surfaced as explicit invariants:
   - "On-disk bitmap is consistent with the active meta's tree"
     (free-space spec, entailed).
   - "No reader's TxnID-pinned pages are reclaimed before the
     reader releases" (RPL spec, entailed).
   - "Index schema-hash + Version on disk match the supplied
     IndexDecl set, or OpenKeyspace fails" (indexing spec,
     clause-explicit).
   - "Slab buffer backing a borrowed `[]byte` is not returned to
     the pool until tx close" (pager/slab spec, clause-explicit).
   - "The meta page's HighWaterMark is monotonically non-decreasing
     within any single recoverable history" (file-format / commit
     spec, entailed).

3. **Be spec-only.** No implementation choices ("use `sync.Pool`",
   "implemented in `pager.go`") unless they are load-bearing
   protocol details. Move implementation hints into a sibling
   `docs/plans/*.md` or remove them.

4. **Cross-reference by spec file + section heading**, not by
   line number. (Line numbers in the old `design.md` will be
   meaningless post-refactor.)

5. **Be self-contained for the reader scoped to that file.** A
   reviewer who only opens `15-indexing.md` should be able to
   understand the indexing subsystem without flipping to other
   specs (except for explicit cross-references like
   "PK encoding rules — see 02-page-formats.md §NUL-escape").

## Plan file conventions

Each `docs/plans/*.md` file:

- Derives from one or more spec files (cite them at the top).
- Breaks the work into numbered chunks (1, 2, …); sub-chunks
  `N.1`, `N.2`, … per the workflow in CLAUDE.md.
- Each chunk: one paragraph of scope, sub-chunk anchors `N.1`
  (planning/triage) and the final close-out are fixed; intermediate
  sub-chunks are filled in during implementation.
- Plans may evolve; rewrite freely as understanding sharpens.

For this codebase the initial plan to derive will be:
`docs/plans/v1-implementation.md` — the chunk roadmap from
"pager/slab foundations" through "background maintenance," covering
the work of bringing the spec to a working implementation. A first
cut of that plan is in scope for this refactor session.

## Source materials

- `docs/design.md` — 5155 lines, 17 sections. Section map (line
  numbers are approximate, post-round-3-fixes commit `5fbe71d`):
  - `## Design Decisions` (~17)
  - `## File Layout` (~70) + Page Types subsections
  - `### Set Keyspace Storage` (~457)
  - `### Range Delete` (~584)
  - `### Free Space Management` (~666)
  - `## Pager and Slab Architecture` (~969)
  - `## Copy-on-Write Transaction Model` (~1167)
  - `## Cross-Process Coordination` (~1541)
  - `## mmap Strategy` (~2200)
  - `## Durability Modes` (~2330)
  - `## File Format` (~2440)
  - `## Keyspaces` (~2475)
  - `## Indexing` (~2550)
  - `## BulkLoad` (~3105)
  - `## API Surface` (~3265)
  - `## Typed Keyspaces (Generics)` (~4400)
  - `## Implementation Layout` (~4570)
  - `## Limits` (~4700)
  - `## Checksums` (~4760)
  - `## Integrity and Safety` (~4890)
  - `## Background Maintenance` (~4945)
- `docs/set-keyspace.md` — companion doc; the descriptor section
  was updated to match `design.md`. Some content is now duplicated
  in both. Audit and de-duplicate.

## Acceptance criteria

A refactor session is complete when:

1. `docs/design.md` and `docs/set-keyspace.md` are **deleted**
   (`git log` preserves history; the no-cite invariant in CLAUDE.md
   forbids retaining superseded docs as "architectural records").
2. Every settled decision, mechanism, and API from those two docs
   is present in exactly one `docs/specs/*.md` file. No content
   has been silently dropped (No silent downscoping).
3. Each spec file has an explicit `## Invariants` section in the
   CLAUDE.md format.
4. `docs/plans/v1-implementation.md` exists with a first-cut
   numbered chunk roadmap (chunks 1–N, scope only — sub-chunks
   filled in lazily).
5. `docs/issues/README.md` lists this issue as closed (entry
   removed) and any new issues created during the refactor are
   linked.
6. This file is deleted (per CLAUDE.md issue close-out gate:
   load-bearing rationale moves inline; all cites repointed at
   kept-current artifacts; file removed).
7. A single conventional commit lands the refactor.

## Out of scope

- Code changes. This is a documentation refactor only.
- Re-litigating settled decisions from the three adversarial
  review rounds. The content is correct; only the structure
  changes.
- New feature additions. New invariants surfaced *during* the
  refactor (e.g., something implicit in the prose that becomes
  explicit when promoted) are in scope; new design decisions are
  not.

## Notes for the refactor session

- The 5155-line `design.md` exceeds the per-call read budget of
  the file tool. Read it in chunks via `offset`/`limit`, or grep
  for section anchors first.
- The work is mostly cut + paste + minor rewording to fit each
  spec's scope. The heavy lift is the **invariants extraction** —
  expect to spend ~half the session on that, per spec.
- Consider drafting one spec file end-to-end (suggest:
  `06-pager-slab.md` — moderate size, clear scope, exemplary
  invariants) before doing the rest, so the conventions stabilize.
- The `docs/plans/v1-implementation.md` first cut should reuse
  the chunk ordering I'd sketched verbally (pager/slab → lock
  file → write tx → cursor → keyspace API → SetKeyspace →
  indexing → BulkLoad → typed → batch/nested → Check → Compact →
  maintenance), but the plan author can re-order.

## Related commits

- `5fbe71d` `docs: rewrite design — pager/slab, indexing, 3 review rounds` —
  the current source-of-truth design state.
- This issue + `.semrel.yaml` bootstrap should land in the commit
  that introduces this file.
