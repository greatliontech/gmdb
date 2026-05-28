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

**Trap pattern to watch for:** the issue cites a "well-known"
invariant or rationale that the actual spec doesn't state, OR proposes
a one-line fix that introduces a new bug (same class as the one it's
fixing), OR understates the work because the safe version of the
proposed mechanism converges on bigger infrastructure.

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

5 commits resolved 4 issues + 1 partial in prior sessions:

| Commit | Issue | Outcome |
|--------|-------|---------|
| `24ec951` | cursor-err-unpositioned-state | Closed (sentinel translation) |
| `ddb3831` | rpl-rebuild-panic-on-wild-pointer | Closed (Inv-RV3 bound) + filed adjacent `open-corrupt-meta-size-fields-panic` |
| `ab2d239` | kind2-one-parent-reachability-test | Closed (enforced test) |
| `e40cbdc` | setkeyspace-put-redundant-membership-probe | Closed (2 single-descent btree primitives) |
| `c1effd2` | writenewindexregistry-partial-leak | **Partial** — `writeNewIndexRegistry` site done via savepoint; 4 siblings remain |

The authoritative live list is `docs/issues/README.md`. Below is a
snapshot of decisions and findings *known but not yet executed*; use
the README as ground truth, this as a hint.

### Decided, in-flight or queued

- **`writenewindexregistry-partial-leak`** — 4 sites remain.
  - 3 cold-path DDL siblings (`Tx.RebuildIndex` index_rebuild.go:119,
    `Tx.DropIndex` :419, `retireIndexRegistry` :377). Apply the same
    `BeginSavepoint` / `RestoreSavepoint(on error)` /
    `ReleaseSavepoint(on success)` pattern as `writeNewIndexRegistry`
    (`c1effd2`). Each needs a failure-injection test using the
    `atomic.Pointer[func()]` seam idiom (precedent:
    `createInitHookForTest` in `internal/lock/lock.go:339`; first
    instance in production: `writeRegistryFailHookForTest` in
    `index_open.go`). Subtle: savepoint restores pager state but NOT
    caller-descriptor fields — explicitly restore any descriptor /
    pinned-index field the helper mutates.
  - `applyIndexMaintenanceOnPut/Delete` (index_maintain.go:168) —
    **needs the bitmap-undo-log build**. Per-row savepoint is a perf
    catastrophe (8 MB bitmap clone at 256 GB MaxSize per row).
    Bespoke rollback must un-retire old pages so the chunk-7.6 H-2
    `pinned.root` revert can't dangle → converges on
    `bitmap-rollback-undo-log`. One infrastructure build resolves
    both.

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

- **`bitmap-rollback-undo-log`** — couples with `applyIndexMaintenance`
  above. One build resolves both.

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
fanin`, `setkeyspace-indexing-perf-and-edge`. Re-validate live before
acting; some may now be obsolete.

---

## This session's task

Pick **one** issue from `docs/issues/README.md`. Confirm the pick with
the user at session start (offer your recommendation + rationale; the
user may override). Default order, given prior decisions:

1. **The bitmap-undo-log build** — resolves
   `bitmap-rollback-undo-log` + unblocks `applyIndexMaintenance`. The
   largest piece, infrastructural; tackle when context is fresh.
2. **The 3 DDL savepoint siblings** of
   `writenewindexregistry-partial-leak` — mechanical pattern, three
   failure-injection tests; could be one increment.
3. **`btree-post-merge-underflow`** — spec amend + recursive rebalance.
4. **`byte-api-covering-return-unwired`** — byte-level return
   contract.
5. **`open-corrupt-meta-size-fields-panic`** — adjacent to a closed
   issue; same Inv-RV3 bound pattern.

Then resolve it via the full protocol above. **One issue per session
is the contract — do not start a second.**

---

## End-of-session protocol

Before exiting this session, rewrite this file (`docs/handoff.md`) so
the next reset picks up cleanly. Specifically:

1. **Preserve the RECURRING LESSON section** — keep all prior receipts.
   Append any *new* trap pattern or finding this session uncovered.
2. **Update Backlog state** — add this session's commit to the table,
   update the in-flight / queued / undecided sections with new
   decisions, removed entries (if an issue was fully closed), or
   findings (e.g., "tried X, discovered Y converges on Z").
3. **Update This session's task** — re-order or reword the candidates
   based on what's now live.
4. **Keep this End-of-session protocol section unchanged** so the
   chain continues.

Commit the handoff file with the session's work (or as a small
follow-up commit) so it's never out of sync with the repo state.
