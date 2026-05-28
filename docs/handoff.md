# gmdb v0 — issue burn-down continuation

You are continuing a multi-session burn-down of `docs/issues/` for gmdb,
an embedded MVCC KV store in Go. **One issue per context reset.** Each
session picks one issue, re-derives it from first principles, resolves
it as its own reviewed change set, then rewrites this file so the next
reset picks up cleanly.

## Repo
- Working dir: `/home/nikolas/repos/github.com/thegrumpylion/gmdb`
- Branch: `main` (the project commits directly to main — established
  pattern; the chunk roadmap landed that way).
- Conventions: `~/.claude/CLAUDE.md` is authoritative (Root-cause
  discipline, Adversarial review loop, Issue triage, Project invariants,
  Quality bar). Re-read it; it overrides any conflicting habit.
- Pre-v1: `.semrel.yaml` says `development: true`. Clean breaks are the
  default; no backcompat scaffolding for a non-existent installed base.
- Chunk roadmap is COMPLETE; only the issue burn-down remains.

---

## Session start (you are here after `/reset`)

You were just invoked after a fresh context reset. Your first actions:

1. **Read this entire file**, top to bottom. The "RECURRING LESSON"
   section is non-skippable.
2. **Re-read** `~/.claude/CLAUDE.md` (authoritative workflow). It
   overrides any conflicting habit.
3. **Check `docs/issues/README.md`** for the live backlog — that is
   ground truth; this file's snapshot may be a session behind.
4. **Propose** the top candidate from "This session's task" with a
   one-line rationale derived from the Ordering criteria, then
   **wait for the user to confirm or override**.
5. **Resolve the chosen issue** via the protocol in "THE RECURRING
   LESSON" (re-validate → diagnose → fix + regression test →
   adversarial review → close-out → commit). One issue per session;
   do not start a second.
6. **Before exiting** — for any reason: success, partial, or
   context-budget halt — follow the "End-of-session protocol" to
   rewrite this file so the next `/reset` picks up cleanly.

---

## THE RECURRING LESSON — re-derive every issue from first principles

**The issue docs' framing and proposed remediations are often wrong or
stale. Do not implement them blindly.** Re-derive from first principles
before designing the fix. The proof is in the receipts:

- **`cursor-err-unpositioned-state`** (`24ec951`): the issue said
  `Err()` returns `nil` in Unpositioned. On HEAD it returned the
  *internal* `btree.ErrCursorUnpositioned` (a sentinel-identity leak
  across the public boundary). **Both** the issue's proposed options
  were wrong: option 1 would have destroyed a deliberate Unpositioned/
  EOI discriminator that the spec's clause-explicit invariant requires;
  option 2's literal `if !c.positioned` returns the wrong sentinel at
  EOI (`positioned` is false for *both* states). Real fix: translate
  the sentinel beside the existing `ErrCursorStale` translation.

- **`rpl-rebuild-panic-on-wild-pointer`** (`ddb3831`): the first cut
  bounded `id >= HighWaterMark`. Adversarial review demonstrated a
  *second same-fault case* the narrower fix left failing — a corrupt
  meta with `HighWaterMark > MaxSize` (which `ValidateMeta` doesn't
  bound) still panics. Corrected to `min(fileSize/PageSize, MaxSize)`,
  the **Inv-RV3** bound `Pager.Page` and `walkRPL` already use. Filed
  `open-corrupt-meta-size-fields-panic` for the systemic
  `ValidateMeta` gap.

- **`setkeyspace-put-redundant-membership-probe`** (`e40cbdc`): the
  issue's single-`PutReportExisting` sketch would itself have
  introduced a leak. Always-write Put on a duplicate set-insert CoWs
  a fresh root that `putIntoNestedTree` discards → orphaned pages.
  *Ironically the same leak class as `writenewindexregistry-partial-
  leak`.* The two call sites need different write-on-present
  semantics. Real fix: two primitives over a shared single-descent
  core — `PutReportExisting` (replace+report) for `Keyspace.Put`,
  `InsertIfAbsent` (no-op-on-present) for `putIntoNestedTree`.

- **`btree-post-merge-underflow`** (decided, not yet executed):
  re-derivation found the spec's §Invariants in `docs/specs/
  range-delete.md` has **no** `fill >= MergeThreshold` invariant. The
  issue's "invariant #3" is invented. `MergeThreshold`'s godoc:
  *"the fill percentage **below which a page is merged**"* — a merge
  **trigger**, not a maintained floor. The user nonetheless elected
  to add the guarantee; framed as `Rationale:` (intended new behavior),
  not `Diagnosis:` of a current defect.

- **`writenewindexregistry-partial-leak`** (partial, `c1effd2`): the
  "bespoke lightweight page-tracking rollback" chosen for the hot-path
  sibling `applyIndexMaintenance`, when designed *safely* (un-retiring
  old pages so the chunk-7.6 H-2 `pinned.root` revert can't dangle),
  converges on the **bitmap-delta undo-log** infrastructure that's
  already its own deferred issue (`bitmap-rollback-undo-log`). One
  build resolves both.

- **`bitmap-rollback-undo-log`** (`0893be5`):
  the issue framed the work as profile-driven (`Lands:` "when
  profiling shows BeginTx allocation pressure is material"). First-
  principles re-derivation found `transactions.md §Nested Transactions`
  carries a **clause-explicit cost invariant** — *"Cost is
  proportional to pages modified at that level, not total database
  size"* — that `Bitmap.Snapshot()`'s `slices.Clone(detail) +
  slices.Clone(summary)` was **violating** (8 MB at 256 GB MaxSize,
  independent of mutation count). So it's a defect closing a
  spec→code divergence, not a perf optimisation; pulling it required
  no profiling justification. Second surprise: the issue claimed
  *"the API surface stays the same so the pager doesn't change"* — a
  new `Discard(s)` method was required so the bitmap could release
  per-Snapshot tracking on the commit-success path (without it the
  `openSnapshots` slice and `undoLog` would leak across tx
  boundaries). The pager DID need to change. Two wrong claims in one
  issue.

- **`writenewindexregistry-partial-leak` per-row case** (`15f9b70`):
  the **handoff's own framing was wrong**.
  Prior handoff said "per-row wrapping is correct-by-design now that
  Snapshot is O(window-flips)". That claim was about the bitmap
  snapshot only — it missed that `Pager.BeginSavepoint` ALSO
  increments `savepointDepth`, which suspends loose-page reuse
  (Inv-N1, free-space.md `AllocPage`'s loose-pop branch). For
  per-row wrapping of N indexed Puts in one tx, that suspension
  multiplies → file grows O(N·depth) → pre-existing
  `TestCheckIndexesSetKeyspaceNestedTreeCleanPasses` and
  `TestSetKeyspaceIndexedBulkDeleteAcrossNestedTree` fail with
  `pager: database is full`. Discovered when the "obvious" first
  cut (BeginSavepoint at the 6 caller sites) broke the test suite.
  The user explicitly picked "Build shallow-savepoint now" as the
  re-scope — surface a 3× larger primitive (`BeginShallowSavepoint`
  + `SavepointKind` + per-event `loosePopLog`) that decouples
  state-capture from loose-pop suspension. Adversarial review
  Round 1 separately surfaced that the demo tests gate only on
  `BitmapLeak`, NOT on loose-pop replay correctness — needed
  pager-level tests on the buffer-content round-trip
  (`TestShallowSavepointRestoreReversesLoosePop`'s 0xAA / 0xCC
  marker assertion).

- **`writenewindexregistry-partial-leak` DDL siblings** (this
  session, the close-out commit): two surprises beyond the
  "mechanical" framing.

  **First**, the *test detection shape was non-uniform*. Under
  neuter, `RebuildIndex` produces `BitmapLeak` (cached-path:
  `flushIndexRegistry(ks, ks.indexes)` re-writes the registry from
  the still-pinned map, ORPHANING the newly-built newRoot tree
  pages); but `DropIndex` under the cached-path is MASKED by the
  same `flushIndexRegistry` rewrite (the still-pinned entry is
  re-published, so the data tree gets re-referenced and Check
  shows zero leak — a false-negative on the neuter). The
  cached-path test was passing for the WRONG reason. Fix: use the
  not-cached path for DropIndex (no `OpenKeyspace` in tx 2), where
  the corruption surfaces as `ReachableInRPL` because
  `propagateNotCachedDescChange` is success-only and the on-disk
  descriptor stays pointing at the bitmap-freed registry root.
  Generalized this into a shared `assertNoBitmapCorruption(t, db,
  site)` helper that checks all four corruption codes
  (`BitmapLeak`, `ReachableButFree`, `ReachableInRPL`,
  `FreeAndPending`) — a single-code assertion would silently miss
  the actual failure mode.

  **Second**, `Tx.DeleteKeyspace`'s partial failure was framed in
  the issue doc as "leak-only" but is actually *corruption*: the
  retirement frees data-tree pages via the bitmap, but the
  in-memory invalidation (cache eviction, dead-mark,
  `pendingDeletes`) runs AFTER the retirement returns — on
  rest-of-tx-continues, the still-cached descriptor (which
  flushKeyspaces skips, because its state is Clean) keeps
  pointing at the freed pages; a future tx that re-allocates them
  overwrites still-referenced data. Severity is the same as
  RebuildIndex/DropIndex post-publish leaks (overwrite hazard on
  re-alloc), not a milder pure leak. Updated the inline comment to
  drop the "leak-only" framing per Round 1 nit-1.

  **Trap pattern:** *a test that passes against neutered code can
  be passing for the WRONG reason* — the cached-path's
  `flushIndexRegistry` rebuild from `ks.indexes` masks DropIndex's
  leak shape. Verify neuter actually exposes the demonstrated fault
  on the chosen test path; switch paths if the rewrite-from-cache
  hides it. (Same trap class: a test gating on `BitmapLeak` only,
  not on the broader corruption code set, would miss the
  `ReachableInRPL` shape entirely.)

**Trap pattern to watch for:** the issue cites a "well-known"
invariant or rationale that the actual spec doesn't state, OR proposes
a one-line fix that introduces a new bug (same class as the one it's
fixing), OR understates the work because the safe version of the
proposed mechanism converges on bigger infrastructure, OR **the
handoff's own framing of the unblocked-prerequisites is incomplete**
— a "now-unblocked" claim may only cover one of several cost
mechanisms the prior fix surfaced, OR **a regression test passes for
the wrong reason** — verify the neutered fix actually exposes the
demonstrated fault on the chosen test path (the cached-path
`flushIndexRegistry` rebuild can mask leak shapes that surface on
not-cached paths).

**The protocol that prevents these traps** (per `~/.claude/CLAUDE.md`):

1. **Re-validate** the issue on current HEAD — reproduce the failure
   or confirm the gap. Line numbers and code paths shift; the issue's
   reproducer text is often stale.
2. **Diagnose** per Root-cause discipline. Anchor to a *demonstrated
   fault*: a failing test, a reachable in-spec input/state → wrong
   result, or a cited reachable mechanism. Read the actual spec to
   verify the claimed invariant exists. Read the actual code to verify
   the symptom reproduces. The issue's "Proposed remediation" is a
   candidate, not the answer.
3. **Project invariants**: if the diff touches a domain concept, cross-
   cutting state, concurrency/ownership, persistence/serialization
   boundary, or trust/security boundary, derive `kind=clause-explicit`
   and `kind=entailed` invariants with the *strongest reachable
   counterexample* and state them before the first cut.
4. **Smallest correct change** + regression test. The test **must fail
   before the fix** — verify by neutering the fix (revert in a stash;
   re-run; confirm; restore). Structurally-larger-but-simpler is not
   "wider scope."
5. **Adversarial review loop** (mandatory, no exceptions). Land the
   first cut (tests green, uncommitted). Spawn a fresh-eyes sub-agent
   with the diff command, the spec in a `# Spec — authoritative` block,
   the plan in a separate `# Plan — roadmap, not authoritative` block,
   the change-set delta, explicit adversarial questions (anchor & scope
   audit, ordering/atomicity, panic paths, tests-that-pass-for-wrong-
   reasons, spec coverage). Disposition every finding (fixed | filed |
   disputed, with `class=introduced|adjacent` decided by the diff
   arbiter, not by judgment). Re-review on any introduced H/M.
6. **Close-out (promote-then-delete)** per §Issue triage. Wrap-aware
   cite search of authoritative spec (`docs/specs/*.md`) and production
   `.go`. Promote load-bearing rationale inline into kept-current
   artifacts (code comments, spec sections). Delete the
   `docs/issues/README.md` row AND the issue file. The plan
   (`docs/plans/v0-implementation.md`) is a roadmap; its historical
   chunk-completion records cite the issue by name as past-tense fact
   and stay as-is.
7. **Conventional commit** (consult `.semrel.yaml`). Commit directly to
   `main` matching project convention. `fix:` / `feat:` / `test:` /
   `perf:` as appropriate.

**Honest scope estimation:** if a chosen approach turns out larger or
riskier than the issue framed it, **surface that finding to the user
and re-confirm** before grinding through. Don't rush multiple change
sets into one context window if each deserves its own review and
close-out.

---

## Backlog state (updated at end of each session)

8 commits resolved 6 issues + 1 partial in prior sessions; this
session closed the DDL-siblings partial via the
nested-savepoint pattern AND promoted-then-deleted the originating
`writenewindexregistry-partial-leak` issue file (with its
load-bearing rationale promoted into a new
`transactions.md §Write-helper error contract` section that serves
all 4 sibling sites: writeNewIndexRegistry, the 6 per-row maintenance
sites, and the 3 DDL siblings):

| Commit | Issue | Outcome |
|--------|-------|---------|
| `24ec951` | cursor-err-unpositioned-state | Closed (sentinel translation) |
| `ddb3831` | rpl-rebuild-panic-on-wild-pointer | Closed (Inv-RV3 bound) + filed adjacent `open-corrupt-meta-size-fields-panic` |
| `ab2d239` | kind2-one-parent-reachability-test | Closed (enforced test) |
| `e40cbdc` | setkeyspace-put-redundant-membership-probe | Closed (2 single-descent btree primitives) |
| `c1effd2` | writenewindexregistry-partial-leak | Partial — `writeNewIndexRegistry` site done via nested savepoint; 4 siblings remained |
| `0893be5` | bitmap-rollback-undo-log | Closed (undo-log substrate + spec amend in transactions.md §Nested Transactions + new `Discard` API) |
| `15f9b70` | writenewindexregistry-partial-leak per-row case | Partial — 6 per-row sites done via new `Pager.BeginShallowSavepoint` substrate; 3 cold-path DDL siblings remain. Filed `shallow-savepoint-clone-cost.md` (residual per-Begin clone cost). |
| `27361ac` | writenewindexregistry-partial-leak DDL siblings | **Closed** — `Tx.RebuildIndex`, `Tx.DropIndex`, `Keyspace.DeleteKeyspace` retirement wrapped in nested savepoint via defer-named-return; new `transactions.md §Write-helper error contract` spec section codifies the all-or-nothing contract for all 4 sibling-set sites; `assertNoBitmapCorruption` test helper checks BitmapLeak / ReachableButFree / ReachableInRPL / FreeAndPending union; promote-then-delete completed (issue file removed, all test/spec cites repointed at the new spec section). |

The authoritative live list is `docs/issues/README.md`. Below is a
snapshot of decisions and findings *known but not yet executed*; use
the README as ground truth, this as a hint.

### Decided, in-flight or queued

- **`btree-post-merge-underflow`** — user elected to **tighten the
  contract** despite first-principles finding that the cited
  "invariant #3" doesn't exist in the spec. This is a hardening, not
  a defect fix. Frame as `Rationale:` (intended new behavior +
  invariant it must preserve + why this shape), not `Diagnosis:`.
  Work:
  1. Amend `docs/specs/range-delete.md §Invariants` to add
     `fill >= MergeThreshold` as a clause-explicit invariant (a new
     guarantee). Reconcile with `MergeThreshold`'s Options godoc
     (currently described as a merge **trigger**, not a floor).
  2. Implement: `mergeOrRedistributeLeaves` / `mergeOrRedistribute-
     Branches` return the real post-merge underflow state; callers
     (`internal/btree/delete.go` `patchBranchAfterChildDelete`
     case-C, `internal/btree/range_delete.go` `rebalanceSurvivors`)
     propagate it for a second-pass recursive rebalance.
  3. Enforced test for the post-merge fill-floor invariant.

- **`byte-api-covering-return-unwired`** — user elected to **wire**
  the byte-API projection-covering return. Make `extractPKAndValue`
  (`index.go`) return the decoded covering tuple for any covering
  index. Defines a byte-level (NUL-escape) return contract that
  changes byte `Lookup`'s value semantics for covering indexes
  (currently the row value via back-lookup; will become the covering
  tuple). Don't break existing byte covering tests
  (`TestIndexedPutWritesCoveringBytes` asserts stored bytes, not the
  Lookup return).

### Undecided / needs analysis

- **`index-handle-stale-after-rebuild-drop`** — substantial
  `markIndexHandlesStale` bundle (Keyspace + SetKeyspace + Index
  handle + iterator-side cursor tracking). Mirror the chunk-5.6
  `markCursorsStale` pattern. Sub-choice: `ErrCursorStale` vs new
  `ErrIndexHandleStale` sentinel.

- **`open-corrupt-meta-size-fields-panic`** — adjacent same-class to
  the resolved rpl-rebuild. Corrupt `BitmapPages`/`MaxSize` panics
  Open at `init.go:238-240` (slice-out-of-range from
  `bitmapBytes = MaxSize * pageSize` exceeding `len(p.mmap)`). Apply
  the Inv-RV3 bound pattern (use the file-resident extent
  `fileSize/PageSize` clamped by `MaxSize`); maybe extend
  `ValidateMeta`.

### Profiling-driven / condition-triggered (re-validate before pulling)

`rpl-segment-relocation`, `compaction-full-forest-walk-per-pass`,
`pager-test-helper-export`, `leaked-readtx-cleanup-race-flake`,
`setkeyspace-delete-range-bulk-walker`, `bulkload-index-merge-run-
fanin`, `setkeyspace-indexing-perf-and-edge`, `shallow-savepoint-
clone-cost` (NEW — filed this session). Re-validate live before
acting; some may now be obsolete.

---

## This session's task

Pick **one** issue from `docs/issues/README.md`. Confirm the pick with
the user at session start (offer your recommendation + rationale; the
user may override). Default order, given prior decisions:

1. **`btree-post-merge-underflow`** — spec amend + recursive
   rebalance + enforced test. User pre-decided to tighten the
   contract; frame as `Rationale:` (intended new behavior), not
   `Diagnosis:`. Design-heavy; suits fresh context. (Promoted to
   #1 now that the DDL siblings closed this session.)
2. **`byte-api-covering-return-unwired`** — byte-level return
   contract; touches `extractPKAndValue` + byte-API `Lookup`/`Get`
   semantics. Don't break existing covering tests.
3. **`open-corrupt-meta-size-fields-panic`** — adjacent to a closed
   issue; same Inv-RV3 bound pattern. Smaller; suits later resets
   with tighter context budgets.
4. **`index-handle-stale-after-rebuild-drop`** — undecided / needs
   analysis. Substantial bundle; consider deciding before pulling.

Then resolve it via the full protocol above. **One issue per session
is the contract — do not start a second.**

### Ordering criteria (apply when reordering candidates at end of session)

1. **Decided > undecided** — don't burn a session re-deciding when
   executable work exists. Items in "Decided, in-flight or queued"
   precede items in "Undecided / needs analysis".
2. **Infrastructural / unblocks others > standalone** — items whose
   completion unblocks other queued work (e.g. bitmap-undo-log
   unblocks `applyIndexMaintenance`) come earlier.
3. **Fresh-context-required > mechanical** — the largest, riskiest,
   or most-design-heavy items go first while context is freshest.
   Smaller / mechanical work suits later resets that may have less
   budget left if the user chains them.
4. **Adjacent to recently-closed > unrelated** — shared mental
   context is a real discount; e.g. an Inv-RV3-bound issue right
   after another Inv-RV3-bound issue.
5. **Correctness > perf** — profiling-driven items stay last and
   must be re-validated (the gap may have closed) before being
   pulled.

When the live backlog changes, re-rank by these in order — earlier
criteria dominate later ones.

---

## End-of-session protocol

Before exiting this session — **whether the issue closed cleanly, made
partial progress, or you halted on context** — rewrite this file
(`docs/handoff.md`) so the next `/reset` picks up cleanly:

1. **Preserve the RECURRING LESSON section** in full — keep every
   prior receipt. Append a *new* receipt if this session uncovered a
   new trap pattern (one bullet, citing the commit hash; describe the
   surprise plainly so future you doesn't fall into it).
2. **Update Backlog state**:
   - Add this session's commit(s) to the table. Mark the Outcome as
     `Closed (<one-line summary>)` if fully resolved + promote-then-
     deleted, or `Partial — <what's done, what remains>` if the
     issue still has open scope.
   - Update the "Decided, in-flight or queued" and "Undecided / needs
     analysis" sections: remove closed entries; add or amend entries
     with new findings (e.g. "tried X, discovered Y converges on Z");
     record any new user decisions verbatim.
3. **Update "This session's task"** — re-order or reword the
   candidates by applying the **Ordering criteria** above. State each
   candidate's one-line rationale next to it so the next agent
   inherits the reasoning.
4. **If the session halted on context mid-fix** (uncommitted work in
   the tree): add a short "Carry-over" subsection under "This
   session's task" describing exactly what's staged / unstaged / done /
   pending and what the next agent should do to either finish or
   safely revert it. Do not commit broken or unreviewed code to
   resume; either complete the protocol or revert and re-queue.
5. **Keep the Session start, Ordering criteria, and End-of-session
   protocol sections themselves unchanged** so the chain continues.

Commit the handoff file in the session's commit (when it's a small
docs touch) or as a separate `docs:` follow-up commit so the file is
never out of sync with the repo state. The handoff being committed is
the persistence boundary — anything not in the committed file is lost
to the next reset.
