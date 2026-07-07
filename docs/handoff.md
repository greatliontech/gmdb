# gmdb v0 — issue burn-down continuation

> **STATUS (2026-07-05).** The 2026-07-04 audit burn-down is
> **complete**: all 22 chunks landed on main as individually reviewed
> commits (the plan file is deleted per close-out; `git log --all --
> docs/plans/audit-burndown-2026-07.md` recovers it). The standing
> directive that drove it — "fix them all, bottoms up … full authority
> over specs, docs, code, and commit cadence" — is discharged. The
> remaining `docs/issues/` backlog is condition-triggered only.
>
> **(2026-07-07)** Superseded as the session driver: the active
> roadmap is `docs/plans/architecture-consolidation.md` (commit/
> recovery/RPL groundwork → recovery-model redesign → independent
> collapses). The issue-selection flow below applies only to backlog
> entries not pulled by that plan's chunk gates.

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

- **`btree-post-merge-underflow`** (this session): the issue's
  "~10 LOC" framing was structurally insufficient. The user picked
  "Strong invariant + cousin rebalance" expecting ~100-200 LOC,
  but Rounds 1-3 of adversarial review surfaced a complexity spiral
  (each fix exposed another corner). **The actual root cause was an
  architectural gap I didn't diagnose until pressed**: the
  `deepUnderflowChild` propagation contract only gets healed in
  case-C (merge). Case-B (child healthy, no merge) just threads the
  signal upward without healing — once any code path produced
  `deepUnderflowChild != 0` while the carrying branch's encoded fill
  was ≥ MT, the deep cascaded to the top and got discarded. **The
  unifying fix** is a 5-line architectural rule: when a level returns
  `deepUnderflowChild != 0`, force `parentUnderflow=true` regardless
  of encoded fill, so the next level's case-C fires and gives the
  deep new siblings via the merge result. Plus a top-level
  final-heal pass at `Delete()` / `DeleteRange()` for cases where
  the cascade reaches root. This unifies every corner case Rounds
  1-3 surfaced. **Lesson**: when reviewers surface a sequence of
  surface-similar findings, stop pattern-matching at the symptom
  level and ask "what's the architectural invariant being violated?"
  — the answer is usually a 5-line rule, not a 200-line accumulation
  of patches.

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

- **`writenewindexregistry-partial-leak` DDL siblings** (the
  close-out commit): two surprises beyond the
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

- **`index-handle-stale-after-rebuild-drop`** (this session): two
  surprises beyond the issue's "user choice on sentinel" framing.

  **First**, *the spec's existing transactions.md §Cursor invalidation
  by DeleteKeyspace clause already enumerated `*Index` handles as
  things that get invalidated by DeleteKeyspace and return
  `ErrKeyspaceClosed`* — but the implementation has NEVER enforced
  this on the iter surface (no `idx.ks.dead` check in Lookup / Range
  / etc.). This is a same-fault-class adjacent gap, distinct from
  the issue's reported RebuildIndex / DropIndex / atomic-Put cases.
  The Escalation Rule says widening requires a second demonstrated
  same-fault case the narrower fix leaves failing — the
  DeleteKeyspace case IS a second demonstrated same-fault case (an
  `*Index` reads freed pages from a tx mutation), but it has a
  *different cause-line* and was filed as adjacent rather than
  folded in. The judgment call: "same reported fault" in the
  escalation rule means the specific cause-lines named in the
  issue, not the fault class. Filing-and-proceeding here matches
  the chunk-close adjacent-issue contract; widening to cover
  DeleteKeyspace would have been a scope violation. Lesson: when a
  fix touches a known spec clause that the impl doesn't honor,
  CHECK whether the impl actually honors it for the issue's
  reported triggers vs. other adjacent triggers — finding the
  honored-vs-unhonored split early is the difference between a
  same-scope fix and an over-scoped one.

  **Second**, the sentinel-choice analysis revealed the issue's
  framing ("ErrCursorStale vs new ErrIndexHandleStale — user
  choice") was incomplete. The post-Drop case is semantically
  "the index is gone," NOT "stale, re-iterate works" — and the
  correct sentinel is `ErrIndexNotFound` (matching what
  `ks.Index(name)` returns post-Drop). The mid-iter cursor case is
  ErrCursorStale (matching row-cursor sibling-mutation). Two
  *existing* sentinels for two distinct conditions, NO new
  sentinel. The "user choice" framing in the issue obscured this
  distinction — both proposed options would have conflated the
  two. Lesson: when an issue offers a binary "choose A or new B"
  on a sentinel question, the right answer is often "neither —
  decompose the cases and use distinct existing sentinels per
  case." First-principles re-derivation of the user-facing
  contract surfaces the decomposition.

  **Third (consolidation refactor)**, instead of adding a 1-line
  `ks.markIndexHandlesStale()` at each of 11+ post-mutation sites
  in set_keyspace.go (Put/Delete/DeleteValue genesis-/subpage-/
  nested-tree- branches), the cleaner shape is to make
  `markCursorsStale` / `markSetCursorsStale` internally call
  `markIndexHandlesStale`. The semantic justification: both are
  "stale all in-flight observers of this keyspace's
  post-mutation state" — they're tightly coupled by design. The
  Quality Bar's "smallest correct change" allows
  structurally-larger-but-simpler shapes, and this is one of
  them: 1 line in 2 functions instead of 11+ surgical site
  edits. Cost is zero for non-indexed keyspaces (empty
  openIndexHandles slice). The fresh-eyes Round-2 reviewer
  verified by neutering BOTH the consolidated call and the
  open-coded Cursor.Delete call → tests fail deterministically →
  the consolidation correctly covers every former markSetCursorsStale
  call site without missing one.

- **`open-corrupt-meta-size-fields-panic`** (`5750827`): two
  surprises beyond the issue's "two-shape fix sketch" framing.

  **First**, *the demonstrated fault is worse than the issue
  framed it.* The issue said "panics with slice-bounds-out-of-
  range (or the make OOMs first)." Reading "OOMs first" as "the
  OS process gets OOM-killed" implied a recoverable error path; in
  practice `make([]byte, BitmapPages*PageSize)` for wild-high
  BitmapPages triggers Go's `runtime: out of memory` via
  `runtime.throw` — which is NOT a catchable panic.
  **`recover()` does not catch `runtime.throw`; the test binary
  dies mid-run.** Confirmed by running the regression test against
  HEAD before the fix: the test process was killed in
  `runtime.mallocgc → runtime.sysMapOS` with no opportunity for
  the deferred recover to fire. Implication for the fix: **the
  bound MUST precede the `make`**, because by the time the
  `make` is called the unrecoverable throw is the failure mode, not
  the recoverable slice-bounds-out-of-range panic that fires at the
  `copy(... p.mmap[...:...])` site for the smaller-but-still-too-
  big intermediate case. A bound placed only inside or after the
  make (e.g. checking `len(detail)` post-allocation) would be
  unreachable for the high-end case. Lesson: when an issue cites
  "make OOMs first" as a parenthetical, ASSUME it's the
  unrecoverable `runtime.throw` shape and bound the size field
  BEFORE the allocation — not via `recover()`-then-translate.

  **Second**, the escalation rule's second-fault test fired
  cleanly and the result was the structurally-minimal two-bound
  fix. The naive "file-extent bound only" (the issue's option 1
  literal sketch) would catch wild-high but leave `BitmapPages=0`
  failing with the unrelated `bitmap.New` "totalPages exceeds
  capacity 0" panic — a second demonstrated fault on the same
  fault class. Per the escalation rule that authorizes widening
  only when a second demonstrated same-fault case the narrower fix
  leaves failing exists, the second bound (`BitmapPages*PageSize*8
  >= MaxSize`) was added — covers Fault-(ii) and the structurally-
  larger-but-simpler shape (two parallel bounds vs. one
  consistency check) was correct because the two bounds catch
  different fault mechanisms (file-extent SIGBUS / make-OOM vs.
  bitmap-capacity-check panic). The escalation rule is *exactly*
  the right tool for this case — the second bound is not "wider
  scope" in the smallest-correct-change sense; it's a co-equal
  fix for a co-equal demonstrated fault.

  **Trap pattern:** *"runtime OOM" in an issue doc may mean
  unrecoverable `runtime.throw`, not OS-level OOM-kill.* The two
  fail very differently — `runtime.throw` aborts the process from
  Go's runtime with no signal handler entry, while OS OOM-kill
  delivers SIGKILL after pressure builds. The Go-runtime variant
  is what `make([]byte, hugeN)` triggers when the requested size
  exceeds the runtime's max allocation; `recover()` cannot catch
  it because it's `runtime.throw`, not `panic()`. Bound size
  fields BEFORE the make, not via post-allocation checks. (Same
  no-crash spec invariant — `checksums.md §Structural and
  Allocation Bounds` and `integrity.md §Forged / structural
  corruption tolerance` — but with the implementation constraint
  that the bound CAN'T be after the make.)

- **`byte-api-covering-return-unwired`** (`682fa70`): two surprises
  beyond the user's pre-decided wiring shape.

  **First**, the issue's first-sentence framing ("Make
  `extractPKAndValue` return the **decoded** covering tuple for any
  covering index") was wrong; the parenthetical that *immediately
  followed* ("Defines a byte-level (NUL-escape) return contract …
  caller decodes the NUL-escape tuple") was the actual contract.
  Returning per-column `[][]byte` from `extractPKAndValue` is
  incompatible with the byte-API `Lookup`'s `iter.Seq2[[]byte,
  []byte]` signature — the value MUST be a single `[]byte`. The
  parenthetical's "caller decodes" reading is the only coherent one.
  Lesson: when an issue's headline and parenthetical disagree, the
  parenthetical's specificity wins; sanity-check the headline by
  pattern-matching against the existing API shape it would have to
  fit.

  **Second**, the neutral-sentinel pattern for public decoder
  surfaces. I argued for `ErrCorrupted` wrap on `DecodeCoveringTuple`
  malformed-input as "matches engine convention for decoded-bytes-
  from-disk failures." User pushed back, escalated as introduced L-2,
  required a new sentinel (`ErrCoveringTupleMalformed`). The deeper
  principle: **a public surface should not wrap in a meaning-laden
  sentinel when it cannot prove the meaning.** At the byte-stream
  level, `DecodeCoveringTuple` cannot distinguish on-disk corruption
  from caller misuse (mis-applying it to non-covering Lookup bytes).
  Engine-internal decoders sit at the corruption boundary by
  construction — they're called only on bytes from the engine.
  *Exported* decoders can be called on arbitrary inputs. So the
  internal `errIndexKeyMalformed` wraps in `ErrCorrupted` (correct
  for engine context); the exported `DecodeCoveringTuple` wraps in
  neutral `ErrCoveringTupleMalformed`. Lesson: when designing a
  public error class for a decoder, ask "what context CAN this
  function prove about the input?" — if it can't prove corruption,
  the sentinel must not imply corruption.

  **Third (process-level, Round 2 finding L-2 receipt)**, when
  adding a new branch to a spec-defined behavior class (here:
  "Lookup paths that DO NOT probe row keyspace"), the **authoritative
  enumeration paragraph elsewhere in the spec** needs the same edit.
  I added §Byte-API return contract and Lookup-godoc claim "covering-
  return skips silent-skip", but missed extending the authoritative
  §Intra-transaction consistency paragraph that enumerates the
  silent-skip exceptions (`LookupKeys` was the only one listed). The
  fresh-eyes reviewer caught it. Lesson: a spec edit that adds a new
  member to a behavior class needs to walk the enumeration sites
  (typically "X does/does-not Y" paragraphs), not just the §-where-
  the-feature-lives section.

- **`shallow-savepoint-clone-cost`** (this session, `43ac8df`): three
  surprises beyond the issue's "profile-driven, fix later" framing.

  **First (re-derivation finds clause-explicit violation, not
  profile-driven optimization).** The issue framed the work as
  profiling-driven (`Lands:` "when overhead is measurably material").
  First-principles re-derivation against the spec found
  `transactions.md §Nested Transactions`'s cost clause — "Cost is
  proportional to pages modified since the outermost open savepoint,
  plus O(bitmap-pages currently dirty) for the bitmap-dirty-set clone,
  not total database size" (as amended in `0893be5`) — was being
  violated. `captureSavepointState`'s 4× `maps.Clone` + dirtyKeys-set
  build paid O(this-tx-cumulative-state-at-Begin), not O(per-window
  mutations). For N indexed Puts in one tx, total cost was
  Σₖ O(k) = **O(N²)**. The "outermost open savepoint" reading the
  issue used (lenient: implicitly extends to tx-start) was textually
  defensible but the spec amend's specific wording — naming the
  bitmap-dirty-set as the ONE exception — made the strict reading
  (per-window mutations only) the intended one. **Same pattern as
  `bitmap-rollback-undo-log` (`0893be5`): profile-driven framing
  hides a clause-explicit defect.** Lesson: when an issue framing is
  "profile-driven for a perf concern," check the spec for a cost
  clause that the issue's mechanism may be violating — if found,
  the work is correctness, not perf.

  **Second (Round-1 adversarial review: scope completion).** The
  fix instrumented mutation sites at AllocPage's bitmap-hit branch,
  RPL-reclaim retry branch, FreePage's three branches,
  AllocContiguous, reserveBitmapRun, TailRefund, and the three slab
  installers (CoW / AllocSlab / AllocSlabRun). **Round-1 reviewer
  caught H-1: AllocPage's LaggingReaderWait → refreshReclamationBound
  → reclaimRPL > 0 → bitmap.FindFirst → success path was structurally
  identical to the bitmap-hit branch but uninstrumented.** Reachable
  via per-row indexed maintenance with `LaggingReader=Wait` under
  bitmap exhaustion. Fix: instrument the branch identically (~4
  lines). Lesson: when a fix instruments N mutation sites
  structurally identical to one another, **enumerate them by
  pattern match**, not by walking the original instrumentation list
  — a fast-path branch buried in a switch case is easy to miss.

  **Third (Round-1 H-2 + Round-2 M-1 — loose-pop interaction with
  in-window dirty additions).** The Round-1 reviewer surfaced H-2:
  the pre-fix `Restore`'s wholesale "iterate `dirty`, drop any id
  not in pre-window `dirtyKeys` set" cleanup handled BOTH (a) the
  in-window-CoW + loose-pop interleave (case for the
  savepointUndoLog-first replay order) AND (b) the in-window-alloc
  + loose-pop case where pre-window dirty[id] was absent.
  Post-fix's savepointUndoLog (fieldDirty, id, false) replay handles
  (a) cleanly, but the loose-pop replay then unconditionally re-
  attached entry.buf to dirty[id] — leaking the in-window-installed
  buffer into post-Restore dirty (Inv-N2 violation, dirtyBytes
  accounting desync). **Fix: add `wasPreWindow bool` to
  loosePopEntry, captured at loose-pop time by scanning
  sp.undoLogPos..end for a prior (fieldDirty, id, false) entry. The
  replay branches: true → re-attach; false → pool-Put and do NOT
  install.** Round-2 reviewer then found M-1 (adjacent, pre-
  existing): nested SHALLOW savepoints double-reference the same
  buf pointer across outer + inner loosePopLogs; outer Restore's
  wasPreWindow=true branch pool-Puts the buffer the inner Restore
  just re-installed → buffer in both pool and dirty[id]. Unreachable
  in production (6 per-row callers each open-and-resolve one
  shallow per call); filed as
  `nested-shallow-loose-pop-buffer-alias.md`. Lesson: **the pre-
  fix wholesale-dirty-cleanup loop was a single broad mechanism
  that silently covered two distinct cases (in-window-CoW vs
  in-window-alloc); replacing it with per-mutation undo entries
  requires explicitly handling each case the broad mechanism
  covered** — the in-window-alloc case is not subsumed by the
  savepointUndoLog's fieldDirty entries because the loose-pop's
  dirty-detach (handled by loosePopLog, not savepointUndoLog)
  occurs AFTER the in-window add, and the buffer pointer is only
  known to loosePopLog. The wasPreWindow flag is the bridge.

- **`nested-shallow-loose-pop-buffer-alias`** (this session, `d9ea7d4`):
  three surprises beyond the issue's "user choice on resolution
  candidates" framing.

  **First (the right scope decision is determined by the production-
  caller audit, not by the resolution candidates' formal symmetry).**
  The issue offered Option 1 (panic guard + spec amend) vs Options
  2/3 (keep nested-shallow in-spec, add bookkeeping). The candidates
  looked formally symmetric — both close the bug, just at different
  scope. But the smallest-correct-change reading depends on whether
  any production caller exercises the case: if no production caller
  nests SHALLOWs (which a grep of the 6 per-row callers confirmed),
  Option 1 is the smallest correct change (illegal-state-unrepresentable
  at API surface, no ongoing bookkeeping cost), and Options 2/3 are
  *over-engineering for a non-existent caller* — a Quality-bar
  defect, not diligence. **Lesson**: when an issue's resolution
  candidates are "A = panic + spec amend that removes the case from
  spec" vs "B/C = keep the case in-spec via bookkeeping," run the
  production-caller audit BEFORE picking. If no production caller
  exercises the case, A is the smallest correct change by
  construction; B/C carry runtime cost for hypothetical callers and
  should be rejected per the Quality bar's over-engineering clause.

  **Second (full-stack scan vs topmost-only — narrowest is not
  always topmost).** The Round-1 reviewer questioned whether the
  guard's full-stack scan was widening (vs a topmost-only check).
  The answer: full-stack is NOT widening. A topmost-only check
  would permit [SHALLOW outer, NESTED middle, attempt SHALLOW
  inner] because the topmost entry is NESTED. But once the inner
  NESTED resolves, the [SHALLOW outer, SHALLOW inner] alias
  configuration is restored and a loose-pop in the inner SHALLOW's
  remaining window still produces the buf alias. The narrowest
  correct check is full-stack-scan-for-kind. **Lesson**: "narrowest
  check" in panic guards is sometimes a structural rather than
  positional choice. A topmost-only check can defeat itself by
  letting an inner sub-window resolve and re-expose the forbidden
  configuration. Verify the guard's reachability set covers ALL
  paths back to the forbidden state, not just the immediate
  begin.

  **Third (test conversion when removing a code path from spec —
  retarget, don't delete).** Two existing tests
  (`TestShallowSavepointOutOfOrderPanics`,
  `TestSavepointRestoreOuterRevertsInnerReleasedWork`) exercised
  shallow-inside-shallow to test KIND-AGNOSTIC substrate
  properties: the LIFO panic discipline and the per-pager log
  shared-by-outer semantic. When the spec amend removed
  shallow-inside-shallow, these tests started panicking on the new
  guard, but their underlying invariants survive in the NESTED
  substrate (both kinds share `RestoreSavepoint`'s LIFO check and
  the per-pager `savepointUndoLog` lifecycle). Retargeting to
  `BeginSavepoint` (NESTED) kept the coverage without deleting it.
  **Lesson**: when a fix narrows a primitive's spec by removing a
  buggy code path, audit existing tests that exercised the removed
  path. Tests pinning the REMOVED behavior should be deleted;
  tests pinning a KIND-AGNOSTIC substrate property that the
  removed path happened to be a vehicle for should be RETARGETED
  to a surviving kind. The distinguishing question: "if the
  substrate had been correct, would this test have still been
  worth running?" If yes, retarget; if no, delete.

- **`index-handle-stale-after-deletekeyspace`** (prior session): two
  surprises beyond the issue's "single-line guards" framing.

  **First (Round-2 H-1, scope-widening regression introduced by the
  fix itself).** The issue's resolution was framed as "add `idx.ks.dead`
  / `idx.sks.dead` guards at every `*Index` entry method" — a clean
  symmetric extension. Round-1 reviewer found a closely-related L-1:
  Stats with a sticky `idx.err` from a prior bad-cols Lookup would
  mask post-DeleteKeyspace ErrKeyspaceClosed. The reviewer's
  recommended L-1 fix had two options: tighten the doc, OR reset
  `idx.err = nil` at Stats entry (matching the iter closures'
  chunk-7.7 M-2 per-sequence reset). I went wider — added the reset.
  **This was scope-widening per CLAUDE.md Root-cause discipline's
  escalation rule** (the narrower fix — Stats's keyspaceDead-first
  ordering already closes L-1 because Stats now never reads
  `idx.err`). Round-2 fresh-eyes reviewer surfaced the consequence:
  Stats silently destroys the chunk-7.6 Inv-IHS1 sticky-stale
  signal. A user observing `idx.Err() = ErrCursorStale` after a
  mid-iter sibling-Put, then calling `idx.Stats()` for an unrelated
  count read, sees `idx.Err() = nil` because Stats wiped the
  sentinel. **Lesson**: when a fix's mechanism (entry-method
  guard ordering) already closes the reported fault by structure,
  do NOT also add belt-and-suspenders reset code "to match the
  existing pattern" — that pattern existed for a reason (sequence
  semantics on iter closures vs. single-shot Stats) and copying it
  without that reason regresses an adjacent invariant. The
  escalation rule's "second demonstrated same-fault failing case"
  test is the gate: if no test fails with the narrower fix, don't
  widen.

  **Second (Round-2 M-1 + Round-3 fix, Err-vs-Stats sentinel
  asymmetry).** Round-1's M-1 said bare `idx.Err()` doesn't fire
  post-DeleteKeyspace. I fixed via "Option B": idx.err FIRST, then
  fall back to keyspaceDead / idx.dead. This preserved the
  chunk-7.6 mid-iter Drop ErrCursorStale contract pinned by
  `TestIndexHandleInFlightDropSurfacesCursorStaleAndDead`. Round-2
  surfaced the cost: (bad-cols Lookup → DeleteKeyspace → bare Err)
  returns `ErrInvalidOptions` wrap while `idx.Stats()` returns
  `ErrKeyspaceClosed` — same handle state, different sentinel.
  Round-3 fix: keyspaceDead-first ordering (Option 1). Closes the
  Inv-IHS3 asymmetry on the DeleteKeyspace side while preserving
  the chunk-7.6 mid-iter Drop ErrCursorStale contract on the
  Drop-only side (because on a live ks, keyspaceDead is false and
  the idx.err check fires next). The dial that closes the asymmetry
  *without* breaking the chunk-7.6 contract is precisely this
  ordering: parent-state-first, sticky-iter-cause-second,
  per-handle-state-third. **Lesson**: when the existing contract
  pins a specific sentinel for a mid-iter case, "fix the bare-Err
  symmetry by putting dead-checks first" breaks the contract.
  Instead, identify the broader-truth that holds and put THAT
  check first; the inner sticky-cause still wins on the narrower
  case where the broader truth doesn't hold.

  **Third (Round-3 M-1, spec-tier invariant encoded-but-not-
  enforced).** The Round-3 keyspaceDead-first ordering encoded a
  new explicit claim in the spec: "a handle whose index was
  dropped AND whose keyspace was then deleted in the same tx
  reports `ErrKeyspaceClosed` (the broader truth) rather than
  `ErrIndexNotFound`." Reviewer noted the implementation is
  correct but **no test pins the drop-then-delete ordering** —
  every existing test exercises either Drop alone or
  DeleteKeyspace alone. A future refactor swapping the entry-method
  guard order ("idx.dead first as a perceived field-read
  micro-opt") would pass the suite while silently violating the
  spec. Added `TestIndexHandleDropThenDeleteReportsErrKeyspaceClosed`
  pinning Stats/Err/Lookup/Get for the (Drop → DeleteKeyspace)
  sequence. **Lesson**: per CLAUDE.md Project invariants, every
  invariant encoded in the spec needs the *strongest artifact
  the project stage affords* — for code-stage, that's a
  regression test. Spec-tier alone is recorded-but-not-enforced.
  Walk the new spec claims with a "which test pins this?" question;
  any unenforced spec-tier claim that the code-stage could enforce
  IS a defect.

- **`rplchain-head-tail-terminology-conflict`** (this session,
  `9d060ba`): three surprises beyond the issue's "4 comment-site
  edits" framing.

  **First (an issue's enumeration of fix-sites is a first-pass
  finding, not a complete set).** The issue listed 5 sites (4 in
  freespace.go + 1 in transactions.md). A wide grep audit
  (`grep -rn "chain head\|chain tail\|head trim\|tail trim\|head
  segments\|tail segments\|head slot" internal/pager/ docs/specs/`)
  found 3 more sites the issue missed: `commit.go:235` (pre-existing
  appendRPL phase-1 comment using the slice-front idiom);
  `savepoint.go:189` (the `b83846c` amendment text inherited the
  misnomer); `transactions.md:472` (second instance of "drain head
  segments" in the same paragraph as the issue's enumerated site).
  User pre-approved fixing all per "full scope" authorization at the
  audit reveal. The pre-pick advisory's blast-radius check catches
  the function rename's call sites but not the broader textual
  misnomer in adjacent text or in spec amendments that landed after
  the issue was filed. Lesson: when an issue enumerates fix-sites,
  run the wide grep audit BEFORE committing to scope — the issue's
  list is the original reporter's first-pass finding, not the
  complete set of convention violations.

  **Second (a "pure docs/naming, no correctness" diff can carry a
  project-invariant promotion opportunity that the diff's own
  framing hides).** The issue framed the work as "pure docs/naming
  cleanup; no correctness implication." Round-1 fresh-eyes reviewer
  surfaced a spec-amend candidate: the chain-orientation invariant
  (`pager.go:369-370` SetRPLChain godoc + free-space.md §RPL
  in-memory segment list) is recorded-only — no test exercises
  multi-segment drain ordering. Existing single-segment fixtures
  (`TestReclaimRPL`, `TestReclaimRPLRespectsBound`, the
  lagging-reader tests) all seed one RPLSegmentRef and cannot
  distinguish tail-first from head-first drain. Per CLAUDE.md
  Project invariants, the violation= is "a future maintainer writes
  new chain-mutating code under the wrong orientation assumption,
  producing wrong-result reclamation or commit-encoding logic" — a
  reachable in-spec state→wrong-result violation, not a preference.
  User picked option (b) "add test now" over (a) "file as new
  issue" and (c) "keep godoc-only." `TestRPLChainOrientationMulti
  Segment` (3-segment chain, partial reclamation bound between mid
  and head TxnIDs, identity-tested survivor) promotes the invariant
  to enforced-by-test (neuter-verified: reversing `headPageID()` to
  read `rplSegments[0]` produces deterministic failure with
  `headPageID() = 10, want 12 (last index per chain convention)`).
  Lesson: when a docs-only diff touches a domain concept, audit
  whether the underlying invariant is enforced — the spec-amend-
  candidates channel surfaces the promotion opportunity without
  widening the diff's intent. Per CLAUDE.md Project invariants,
  recorded-only is weaker than enforced when stronger is available;
  the strongest reachable encoding for a code-stage project is a
  regression test.

  **Third (rewording an existing comment without auditing the
  underlying claim's accuracy preserves the wrong framing —
  Round-1 L-1 receipt).** The original `freespace.go:440-441`
  inline comment said "copy-trim to free the head slot for GC
  rather than a head-retaining reslice." My initial reword fixed
  the "head"/"tail" misnomer but preserved the GC-eligibility
  rationale: "copy-trim so the slice's first element is GC-
  eligible." Round-1 reviewer L-1: for `RPLSegmentRef` (a pure
  value type `{PageID uint64; TxnID uint64; Count uint32}`), there
  is no per-element GC concern — the backing array is one
  allocation, kept alive as long as any slice references it. The
  actual benefit of copy-trim over `s = s[1:]` is **capacity
  preservation**: `s[1:]` shrinks `cap` by 1 (Go slice cap is
  measured from the data pointer), forcing earlier reallocation as
  the chain re-grows; copy-trim preserves trailing slice capacity
  so the next `appendRPL` reuses the same backing array. Round-2
  fix used the capacity framing. Classified as adjacent (cause-
  line predates the diff: the original comment shipped with the
  GC-eligibility framing) — and the protocol-compliant disposition
  for an adjacent finding is filing OR fixing in-place under the
  smallest-correct-change structural-size rule. Fixing in-place
  was correct here because I was already inside the comment doing
  the rename reword — leaving misleading text I just edited would
  be poor stewardship even though the misframing predated me.
  Lesson: when rewording an existing comment, audit the underlying
  claim's accuracy, not just the surface language; a reword that
  preserves a wrong rationale is still wrong. The adversarial
  reviewer is the safety net here — the lesson is to slow down at
  the reword step and ask "is the existing claim even right?"
  rather than "does my reword change the surface meaning?".

- **`rplsegments-clone-cost`** (prior session, `b83846c`): three
  surprises beyond the issue's "profiling-driven, fix later"
  framing.

  **First (re-derivation distinguishes from the prior two
  profile-driven closes by *which* cost dimension scales).** The
  last two profile-driven closes (`bitmap-rollback-undo-log`
  `0893be5`, `shallow-savepoint-clone-cost` `43ac8df`) both
  re-derived as clause-explicit cost-clause violations because
  their unenumerated cost term scaled with **MaxSize** (bitmap
  clone: 8 MB at 256 GB MaxSize; per-tx map clones: O(N²) within
  one tx). The rplSegments case looked surface-similar (a
  profile-driven framing on the same `captureSavepointState`
  path) but re-derivation found the strict cost clause is
  **SATISFIED**: chain length is independent of `MaxSize` (chain
  grows with retired-pages-pending-reclamation count, not page
  count). Only the auxiliary "small constant in practice" claim in
  §Why this is cheap can be falsified (lagging-reader scenario
  accumulates the chain across writer commits). Per CLAUDE.md
  Project invariants, "an invariant with no statable violation= is
  a preference — do not record it" — slow ≠ wrong/unsafe, no
  demonstrated fault, so the claim is a preference not an
  invariant. Smallest correct change per Quality bar: **drop the
  preference from the spec**, not implement the substrate. **The
  distinguishing test**: does the unenumerated cost term scale
  with `MaxSize` (clause violation, must fix structurally) or with
  workload history the spec already permits (preference, drop from
  spec)? The 2:1 split (0893be5 fix, 43ac8df fix, b83846c drop)
  shows the right disposition is not predictable from the
  "profile-driven" framing alone — re-derivation must distinguish
  case-by-case.

  **Second (Round-1 H-1 self-introduced symbol cite of a phantom
  function).** My spec amend cited `finalizeRPLChain` as the
  commit-time RPL appender, but the actual function name is
  `appendRPL` (`internal/pager/commit.go:238`). I had read the
  function body earlier without scrolling to the signature line,
  inferred the name from memory + context, and wrote the wrong
  symbol into the authoritative spec. Round-1 reviewer caught it
  with a `grep finalizeRPLChain → 0 hits` mechanical check.
  **Lesson**: when citing a code symbol in spec text, verify the
  symbol exists *by signature* (`grep -n "^func.*<symbol>"`) not
  by recall — a phantom cite into the authoritative spec is a fault
  even if the surrounding claim is correct, because future readers
  / tooling will follow the cite to a missing symbol. The fault
  class here is "incidental name-recall error in authoritative
  prose"; the cheap defense is the explicit signature-grep before
  the spec edit lands.

  **Third (Round-1 M-1 surfaced a pre-existing terminology
  conflict that the prior spec text inherited unchanged).** The
  spec phrase "`reclaimRPL`'s head trim" is pre-existing wording,
  but the chain orientation defined at `pager.go:369-370`
  ("tail (index 0, oldest) → head (last, newest)") makes
  `trimRPLChainHead` actually trim the chain *tail* — the function
  name uses "head" to mean "front of the backing slice" while the
  documented chain convention uses "head" to mean "newest entry".
  The pre-existing function name conflicts with the pre-existing
  convention; the spec text inherits the function's naming. My
  diff did not introduce the conflict but propagated it. **Filed
  as adjacent** (`rplchain-head-tail-terminology-conflict.md`)
  because the cause-line predates this change set; the cleanest
  fix is a rename refactor (`trimRPLChainHead` →
  `trimRPLChainTail` + 4 comment-site edits) on a future session
  that touches the area. Adjacent classification per the diff
  arbiter: cause-line outside the delta; not widened by the
  diff. **Lesson**: when a spec amend inherits pre-existing
  wording, verify the inherited wording aligns with the local
  spec's other conventions (here, the chain-orientation
  convention defined elsewhere in the same package). The
  inherited phrase is "not introduced" by the diff arbiter, but
  the adversarial reviewer may still flag it as a co-located M
  worth filing — and filing-and-proceed is the protocol-compliant
  disposition.

- **`leaked-readtx-cleanup-race-flake`** (this session, `5800299`):
  three surprises beyond the issue's three-option resolution
  sketch.

  **First (the test's wait shape was structurally wrong, not just
  "GC-timing variant" — the deeper root cause is an asymmetry the
  issue framing missed).** The issue framed the flake as
  "runtime.AddCleanup finalizer-scheduling latency under -race
  outruns the test's bounded wait," implying the fix is either a
  longer wait (Option 2) or a deterministic hook (Option 1).
  First-principles re-derivation found the deeper root cause:
  `BeginRead` with a no-deadline context returns `ErrReadersFull`
  IMMEDIATELY (`internal/lock/coord_reader.go:75-77`); it does
  NOT block waiting for a slot. The test's pre-existing shape
  spawned a `BeginRead` goroutine and waited on a 5s wall-clock
  timer, but the goroutine's `BeginRead` returned with the error
  in microseconds — the 5s timer never fired. Distinguishing this
  matters because Option 2 ("race-cost-proportional wait bound +
  extra GC cycles") would NOT have fixed the bug — no number of
  GC cycles changes the fact that `BeginRead` returns immediately
  with `ErrReadersFull`, not blocks-and-unblocks on slot release.
  The write-path counterpart `TestLeakedTxReleasesWriteLock`
  passes 20/20 -race iterations because `Begin(ctx, true)` DOES
  block on the writer-flock channel — this structural asymmetry
  between block-on-slot vs return-error-on-full-slot is the
  semantic the read-path test missed. **Lesson**: when an issue
  frames a flake as "GC timing variant," verify the test's wait
  shape actually matches the wait SEMANTICS of the function under
  test — a goroutine + wall-clock timer assumes the function
  blocks; if it returns immediately with an error, the wait shape
  is wrong by construction, NOT merely slow.

  **Second (the issue's three resolution options were collectively
  insufficient — Option 1 was the right SHAPE but the wrong layer
  suggestion).** The issue's Option 1 said "expose a test-only
  hook on the closeGate / cleanup pipeline that the test can
  poll/wait on (similar to `commitStep4HookForTest`)." Per
  first-principles re-derivation, the hook must fire at the TAIL
  of `readTxCleanupFn`'s active-release path (AFTER
  `info.coord.ReleaseReader(info.slot)`), so a hook-signalled
  `BeginRead` deterministically observes the slot as free.
  Placing the hook on `closeGate` (as the issue suggested) would
  fire too early (before `ReleaseReader`'s atomic stores complete)
  OR on the wrong cleanup (closeGate.EnterCleanup fires for
  write-tx cleanups too — wrong granularity). The right
  placement is AFTER `ReleaseReader`, before the deferred
  `ExitCleanup` — INSIDE the EnterCleanup/ExitCleanup window so
  `Close`'s `BeginClose` drain naturally waits for the hook. The
  package-level `atomic.Pointer[func()]` pattern (mirroring
  `writeRegistryFailHookForTest` and
  `indexMaintenanceFailHookForTest`) is the right shape; a
  `closeGate` method would be the wrong layer. **Lesson**: when
  an issue's "expose a hook on X" suggests a specific subsystem,
  verify the hook actually belongs in that subsystem by tracing
  the required happens-before — the suggestion may point at a
  layer that fires too early or with wrong granularity.

  **Third (Round-1 reviewer caught an introduced no-cite-invariant
  violation — production godoc citing the resolving issue path
  during the same change set that closes it).** M-1 introduced:
  the new godoc on `readTxCleanupHookForTest` cited
  `docs/issues/leaked-readtx-cleanup-race-flake.md` as the
  rationale source. Per CLAUDE.md Issue triage's no-cite
  invariant, authoritative Spec and production code MUST cite
  only a kept-current artifact (Spec section, enforced invariant,
  test name) or a `git log` mechanism — NEVER a tracking
  artifact like an issue doc or ADR. Fixed in-place by
  retargeting the cite to the test name
  (`TestLeakedReadTxReleasesSlotViaCleanup`) plus a
  `git log --all -S readTxCleanupHookForTest` mechanism for the
  rationale history. The promote-then-delete contract makes the
  cite acceptable only AFTER the retarget: the issue file is
  deleted at close-out, `git log -S` preserves the rationale, and
  the test name is the kept-current regression-pinning artifact.
  **Lesson**: when adding new production godoc that references a
  fix's rationale, default to "cite the test name + `git log`
  mechanism"; never reach for the issue path as a convenience,
  EVEN during the same change set that closes the issue. The
  no-cite invariant is static — it fires regardless of whether
  the cite would be stale-on-merge.

- **`compaction-full-forest-walk-per-pass`** (this session,
  `91a268a`): two surprises beyond the issue's three-option
  resolution sketch.

  **First (the strict cost clause quantifies WRITE cost only —
  "I/O budget" framing in §Cost per pass does NOT cover reads
  even though the clause uses the unqualified word "I/O").** The
  issue framed the work as "O(total live pages) in reads vs only
  `CompactionBatchSize` relocated — wasteful." First-principles
  re-derivation against `background-maintenance.md §Cost per
  pass` found the clause-explicit "worst-case I/O is
  `CompactionBatchSize × (1 + depth) × PageSize`" is followed
  immediately by "the slab must hold the whole cascade" — the
  slab is the in-memory CoW write buffer pwritten at commit. The
  clause is exclusively about pwrite I/O. Read cost is documented
  in §Mechanism step 1 ("Walk every B+tree in the forest") but
  NOT bounded in the cost clause. So the strict clause is
  SATISFIED for writes; the issue's read-cost concern is an
  unenumerated cost dimension, not a clause violation. The
  MaxSize-scaling test (`shallow-savepoint-clone-cost` `43ac8df`
  / `bitmap-rollback-undo-log` `0893be5` → MaxSize-scaling → fix;
  `rplsegments-clone-cost` `b83846c` / this session → workload-
  history-dependent → preference, drop from spec) confirms the
  disposition: read cost scales with live B+tree node pages
  (workload state), structural ceiling `MaxSize`/`PageSize` for a
  fully-allocated database — same shape as `b83846c`'s
  rplSegments chain. **Lesson**: when a cost clause says "worst-
  case I/O is X," check whether X is a write-side or
  comprehensive expression — "the slab must hold the cascade" is
  the diagnostic signal that only writes are quantified, and the
  read-cost dimension may be separately unbounded. Don't conflate
  the clause's coverage with the clause's apparent breadth. The
  3:3 split now reinforces: three profile-driven concerns were
  clause violations needing fix (`0893be5`, `43ac8df`, `5800299`);
  three were preferences needing spec amend only (`b83846c`,
  `9d060ba`, `91a268a`). The disposition is not predictable from
  "profile-driven" framing alone — the cost-dimension audit must
  be explicit.

  **Second (an issue's "Related" sub-concern can flip independently
  from the main concern — re-derive each separately).** The
  issue bundled a "Related: compaction self-signals fragmentation
  trigger" sub-concern (`relocateOverflowChain`'s
  `AllocContiguous` bumps the same `contigAttempts` /
  `contigFragFails` counters consumed by the trigger). The issue
  itself characterized this as "self-limiting … arguably
  desirable … not a correctness defect" — explicitly a preference
  shape. First-principles re-derivation agreed (no statable
  `violation=` per CLAUDE.md Project invariants), so both the
  main concern and the Related sub-concern were preferences this
  session. But they could have gone the other way independently
  — if the spec had a clause about trigger-metric-purity, the
  Related concern alone might have been a clause violation while
  the main concern remained a preference. **Lesson**: an issue's
  "Related" or "Adjacent" sub-section is an independent piece of
  work requiring its own clause-explicit / entailed invariant
  check. Do not assume a sub-concern shares the main concern's
  disposition. Treat each as its own first-principles re-
  derivation: read the relevant spec section, derive the invariant
  with statable `violation=`, and disposition independently. The
  sub-concern's spec promotion can be folded into the same
  close-out commit (this session: brief §Trigger note that the
  inclusive count is self-limiting and intentional), but the
  derivation must be separate.

- **`setkeyspace-indexing-perf-and-edge`** (this session, `de9e7c1`):
  three surprises beyond the issue's "items A+B perf-only,
  fix shape obvious" framing.

  **First (the third disposition class — "fix" outside the
  MaxSize-clause-violation / preference-drop binary).** Six prior
  profile-driven closes split 3:3 between clause-explicit cost
  violations (fix via substrate) and preferences (drop from spec). This
  one fit NEITHER bucket: no spec clause was being violated (the bulk-
  op cost clause `O(entries × (indexes + extractor))` already permits
  the 2× constant factor); but the redundancy ALSO wasn't a
  preference-to-drop (no spec text recorded the implicit "one
  snapshot per op" expectation). Per first-principles re-derivation
  it's a *third class*: a constant-factor inefficiency where the
  CLEANUP is structurally small and correct-preserving. The wrappers
  had two layers (chunk-7.6 H-2 wrapper-internal snap + chunk-7.9
  caller rowSnap) that were *historically* both load-bearing, but
  after chunk-7.9 the caller layer subsumed the wrapper's contract
  entirely. Consolidation deletes the wrapper layer and moves the
  responsibility cleanly. Disposition: **fix** (not preference-drop),
  because the cleanup is small and "smallest correct change" applies
  to code shape even when the bigger O isn't violated. The 3:3:1 split
  across seven profile-driven closes now: `0893be5` / `43ac8df` /
  `5800299` → clause violation → fix; `b83846c` / `9d060ba` /
  `91a268a` → preference → spec-amend only; THIS session → neither →
  fix via cleanup. Lesson: don't pre-decide between "fix" and
  "preference-drop" by which profile-driven bucket the issue fits;
  re-derive whether a structurally-simple consolidation eliminates
  the redundancy (third bucket: cleanup-fix) before assuming the
  only options are "MaxSize-scaling fix" or "preference-drop."

  **Second (Round-1 M-2: test-coverage encoding density — every
  caller-site bearing an invariant deserves its own test, not "one
  test per distinct mechanism").** I initially encoded the
  consolidated atomicity invariant with 3 tests covering 3 mechanisms
  (Put helper / Delete helper / Bulk-loop), reasoning that the
  remaining 3 caller sites (Cursor.Delete + SetKeyspace.Put +
  SetKeyspace.DeleteValue) "share mechanism with the encoded sites"
  and don't need individual tests. Per Project invariants' "record
  the fewest invariants that pin the spec's correctness; a
  speculative catalogue is over-encoding." Round-1 reviewer
  empirically neuter-verified that removing the new caller-side
  `restoreIndexes(...)` at `set_keyspace.go:726` (SetKeyspace.Put)
  caused ZERO existing test failures — the encoding gap was real,
  not speculative. The mechanism-sharing argument is plausible by
  inspection but exactly the kind of claim regression tests exist
  to encode at the line level, not just the mechanism level. Per
  Project invariants' "encode each invariant in the strongest
  artifact the project stage affords" — for code-stage that's a
  test per neuter-able line. Added 3 more tests for the 3 uncovered
  caller sites; all 6 now neuter-verified individually. **Lesson**:
  "smallest correct change" governs PRODUCTION code; for TESTS, the
  rule is "every caller-site line that bears an encoded invariant
  deserves a test whose removal regresses the line." Empirical
  neuter-verify is the gate: if removing the line passes the suite,
  the encoding has a gap. The "speculative catalogue" prohibition
  applies to recording NEW invariants, not to test coverage of
  caller-sites that already bear one. The boundary: same invariant
  + same mechanism + different cause-line = different neuter target
  = different test.

  **Third (Round-1 M-1: no-cite invariant fires on TEST files,
  including during the same change set that closes the issue —
  same trap class as `5800299`'s godoc cite).** I cited
  `setkeyspace-indexing-perf-and-edge` in the new
  `TestSetKeyspaceBulkDeletePinnedStateRevertsAfterMidLoopFailure`
  godoc as "originally tracked as item B of <slug>". Per CLAUDE.md
  Issue triage gate 2's no-cite invariant: "Authoritative Spec and
  production code cite only a kept-current artifact or a `git log`
  mechanism — NEVER a tracking artifact." Test files are
  kept-current artifacts; the cite is no different from a code
  comment cite. The cite is dangling-on-merge once the issue file is
  deleted, even during the same change set. Same trap pattern the
  `leaked-readtx-cleanup-race-flake` `5800299` Round-1 M-1
  surfaced for production godoc; this session's lesson is that the
  pattern extends to test godoc as well. **Adjacent in-place fix**:
  `index_types_test.go:240`'s pre-existing cite of the same slug for
  the closed item-C reference — same anti-pattern, pre-existing
  cause-line, classified adjacent per the diff arbiter; fixed
  in-place under smallest-correct-change because we were already
  doing a wide grep-and-fix on the slug for close-out. Lesson:
  default to citing chunk numbers (kept-current via `git log
  --grep="chunk-N"`) + the test's own neuter clause when describing
  a test's rationale; reach for an issue-path cite ONLY when no
  durable anchor exists, which for a closing fix is never (the
  chunk evolution chain plus the test's neuter assertion are always
  available).

- **`setkeyspace-delete-range-bulk-walker`** (this session,
  `400c95d`): three surprises beyond the issue's "perf-driven
  walker rewrite" framing.

  **First (spec internal inconsistency as the gating trap).**
  The issue framed the work as profile-driven ("Lands:
  opportunistic — when profiling shows DeleteRange-heavy workloads
  bottlenecked"). First-principles re-derivation against three
  spec files surfaced an INCONSISTENCY:
  - `set-keyspace.md` line 17 said "Range delete on SetKeyspaces
    uses the bulk-free mechanism described here PLUS the
    range-delete walk of `range-delete.md`" — walker-normative.
  - `range-delete.md §Indexed-keyspace fallback` chunk-7.10
    amendment said "This matches the chunk-6.8
    `SetKeyspace.DeleteRange` partial-progress contract" —
    explicitly endorses v1 per-row-atomic.
  - `set_keyspace.go:1191-1193` inline + `api-surface.md`
    `SetKeyspace.DeleteRange` godoc both promised "The future
    O(K+logN) bulk-walker rewrite will honor the same
    (deleted_so_far, err) contract" — STRUCTURALLY IMPOSSIBLE.
    A walker is naturally all-or-nothing OR per-leaf atomic; the
    per-row contract requires per-row sub-savepoints OR
    structural rework of the walker's branch-rebuild atomicity.

  Three normative sources, two contradictions. The 3:3:1 + 1
  profile-driven split now reads: this session's disposition is
  a **fourth bucket** — spec internal inconsistency close +
  clause-explicit re-alignment via clean-break atomicity.
  Lesson: when an issue is filed as "perf-driven follow-up,"
  walk THREE artifacts (the canonical spec, the chunk-N
  amendments since filing, and the api-surface godoc) — they
  may contain contradictory normative claims that the
  re-derivation must surface BEFORE picking a mechanism. A
  "future X will honor same contract" claim is the diagnostic
  signal — verify the contract is structurally compatible with
  the proposed mechanism; if not, the claim is itself a defect
  the close-out must address (drop the claim and re-align the
  inconsistency).

  **Second (mechanism-space discussion + honest scope
  estimation forced a re-pick mid-decision).** The issue's
  proposed remediation was a single mechanism (PerCellFreeFn
  callback). The user's pushback on my binary framing surfaced
  a richer space: (a) v1 stays + spec-only, (b) walker
  all-or-nothing (issue's original) + clean break, (c) streaming
  cursor with new `SetCursor.DeleteKey()` primitive — preserves
  per-row, no CPU win without cached-path infrastructure, (d)
  hybrid walker-interior + per-key-boundary — mixed atomicity,
  CPU + memory win, structurally more complex, (e)
  configurable knob. I initially described (d) as "glue over
  existing walker"; reading `deleteRangeFromBranch`'s recursion
  more carefully revealed (d) needs leaf-level classification
  ("fully in range" vs "boundary") to handle the
  walker's-position-vs-actual-content asymmetry. **Honest scope
  estimate surfaced mid-design**: (d) ~420 LOC; (b) ~225 LOC.
  Per Quality bar smallest-correct-change + "Honest scope
  estimation: if a chosen approach turns out larger or riskier
  than the issue framed it, surface and re-confirm" — I
  surfaced the size delta to the user, they re-picked (b)
  walker-all-or-nothing. **Lesson**: when reviewing mechanism
  options, do the STRUCTURAL-FIT audit (read the recursion
  shape, identify the boundary classification logic) BEFORE
  comparing CPU/memory/atomicity trade-offs. If the
  structural-fit reveals a "glue over existing" claim is
  optimistic, surface the honest size estimate and let the user
  re-pick. Don't lock in on a non-Pareto-optimal mechanism just
  because you've over-described it.

  **Third (dispatch-direction invariants need hook-based
  test-pinning — generalizing the `readTxCleanupHookForTest`
  pattern).** The chunk-7.10 indexed-vs-un-indexed dispatch in
  `SetKeyspace.DeleteRange` determines the user-facing atomicity
  contract (atomic for un-indexed; per-row for indexed). R2 M-1
  flagged that the un-indexed dispatch direction was not
  test-pinned — a future refactor could silently route un-indexed
  traffic through `deleteRangePerKey` (correct counts, correct
  on-disk state, no leak — but per-row partial-progress contract
  instead of atomic). All existing tests would still pass.
  Mechanism: new btree-level `SetDeleteRangeCalledHookForTest
  atomic.Pointer[func()]` + setter, fired once per `DeleteRange`
  invocation at function entry. Tests install hook + run
  workload + assert hook fired (un-indexed) or NOT fired
  (indexed). Mirrors the `readTxCleanupHookForTest` from
  `5800299` (deterministic-synchronization hook). The pattern
  generalizes: when a dispatch direction determines a
  user-facing contract, the dispatch ITSELF needs a hook +
  positive-and-negative test to pin the contract differential.
  Without it, a future refactor preserving the dispatch GATE
  but changing its SEMANTIC (e.g., swapping the helper called)
  would silently violate the contract. **Lesson**: identify
  invariants where a dispatch-DIRECTION determines a
  contract-DIFFERENTIAL (atomicity, cost class, error shape).
  Spec-tier text alone is recorded-only per Project invariants;
  test-tier hook-based pinning makes the invariant enforced.
  Cost is ~20 LOC of hook infrastructure per dispatch.

  **Fourth (test workload sizing matters for "interior path"
  claims).** R1 M-1: my `Test...NoLeakInteriorSubtreeRetire`
  workload (80 keys + 1 nested-tree key) docstring claimed to
  "pin the interior-subtree retire path through FreeSubtree (the
  chunk-5.7 walker's Phase 2)" but the resulting tree was a
  single-level structure where Phase 2 (called at branch level
  for entirely-in-range children) never fired —
  `leftIdx+1 < rightIdx` empty loop. Scaling to 500 keys × 60
  bytes forced a multi-leaf parent tree with cellCount >= 2
  branches; added a probe assertion (`if cellCount < 2: Fatal`)
  to make the workload-size constraint impossible to silently
  shrink in future edits. Lesson: when a test docstring claims
  to "pin path X via mechanism Y," verify the test workload's
  STRUCTURAL SHAPE actually exercises mechanism Y. The probe
  pattern (in-test assertion that the workload reached the
  expected shape) is the defense against silent shrinkage —
  add it whenever the test's coverage depends on a particular
  tree depth, cellCount, or descent-path topology.

  **Fifth (spec-tier `violation=` clauses must describe
  reachable runtime input/state, not "future refactor"
  scenarios).** R3 M-1: my first cut of the new entailed
  dispatch invariant's `violation=` clause read "A future
  refactor routes un-indexed through the per-row path. ... A
  caller relying on the atomicity contract sees their assumed
  contract silently broken." This describes a code-MODIFICATION
  scenario, not a reachable input/state per CLAUDE.md Project
  invariants: "violation= must be a reachable in-spec
  input/state → wrong/unsafe result." Re-wrote to describe the
  reachable runtime failure: "A caller invokes DeleteRange on
  an un-indexed Kind=1 keyspace; the operation hits a pager
  error mid-walk; the caller's subsequent same-tx read sees N
  rows already gone — partial mutation contradicts the
  documented atomic contract." Lesson: when writing entailed-
  invariant `violation=` clauses for dispatch-direction or
  contract-shape invariants, frame the violation as a
  reachable user-input + user-observable wrong/unsafe result.
  "A future refactor" framings violate the reachability
  requirement and become recorded-only encoding nits per R3
  audit.

- **`bulkload-index-merge-run-fanin`** (this session, `ac4af82`):
  three surprises beyond the issue's "profile-driven, cascaded-merge
  is the fix" framing.

  **First (the user's pushback "is this matching b83846c/91a268a a
  rationalization?" caught the lazy pattern-match — and the prior
  two cases were correctly disposed of even though THIS one wasn't
  a preference).** I opened with "this fits preference-drop +
  spec-amend, mirrors `b83846c` (`rplsegments-clone-cost`) and
  `91a268a` (`compaction-full-forest-walk-per-pass`)" — a clean
  surface-level pattern-match: all three are profile-driven framings
  on cost-clause dimensions where the unenumerated term is
  workload-history-dependent. User pushed back. Re-examination with
  rigor found three concrete dimensions that distinguish:
  (i) **foreground vs background**: bulkload runs in the user's
  write tx; b83846c's clone is per-Begin-savepoint CPU under
  degraded state; 91a268a is the maintenance goroutine. Foreground
  tx work hitting bounds is materially different from background
  goroutine work going slow.
  (ii) **OS-hard-limit vs CPU-only**: bulkload's `EMFILE` is a
  per-process FD limit (typical Linux 1024, macOS default 256).
  b83846c is CPU + heap allocation, bounded only by available
  memory. 91a268a is read I/O, bounded only by disk bandwidth.
  Hitting an OS-enforced hard limit fails the operation; slow CPU /
  slow I/O degrades performance but doesn't fail.
  (iii) **user-bound-exceeded vs intrinsic**: bulkload's merger
  read buffers (64 KiB × #runs ≈ 256 MiB at 4000 runs) exceed the
  user-configured `MaxTxBufferBytes=256MiB` by ~2× — silently. A
  user budgeting in-tx memory for a cgroup limit sees their bound
  silently violated. b83846c's savepoint clone has no formal
  user-configured bound (MaxTxBufferBytes is slab-scoped, not
  savepoint-scoped). 91a268a's background reads have no user-
  configured bound on maintenance pass cost.
  Verdict: b83846c and 91a268a are defensible preferences — they
  pass all three dimension checks. Bulkload fails all three. My
  pattern-match was lazy because I conflated the framing similarity
  with disposition similarity. **Lesson**: when applying the
  established prior-session re-derivation pattern (the 3:3:2 split
  across 8 profile-driven closes), check each candidate against
  the THREE dimensions (foreground/background, OS-hard-limit/CPU,
  user-bound-exceeded/intrinsic) before assigning a bucket. Surface-
  framing similarity (profile-driven, unenumerated cost term,
  workload-dependent) does not entail disposition similarity. The
  prior preference-drops were correctly identified; the trap is
  inferring "this case too" without re-running the dimension audit
  per CLAUDE.md "Analyze, don't rationalize" + "no editorializing
  the user's call."

  **Second (the spec's "appears comprehensive" phrasing is broad-
  reading authoritative even when the implementation interprets
  narrowly).** The spec phrase
  "keeping the combined in-memory sort footprint bounded by
  MaxTxBufferBytes" (`bulkload.md §Interaction with Indexes`) has
  two readings: (narrow) "in-memory sort footprint" = phase-1
  sorter accumulator chunk only, matching impl comment at
  `bulkload_indexed.go:59`; (broad) "in-memory sort footprint" =
  entire sort's in-memory state, including the merge phase's
  read buffers. A user reading the spec naturally takes the broad
  reading — a memory bound described as "sort footprint" should
  bound the entire sort. The impl's narrow reading silently lets
  the merger allocate 256 MiB of additional read-buffer memory
  outside the user-configured 256 MiB budget. Per CLAUDE.md the
  spec is authoritative; the impl conforms to spec, not the
  reverse. So the broad-reading clause violation is real. This
  parallels the chunk-7.6 / chunk-7.9 patterns where spec
  phrasing turned out to bind implementation more tightly than
  the impl's narrow self-interpretation. **Lesson**: when an
  impl comment narrows a spec phrase's scope ("X = phase-1 only"
  when spec says "X" without qualifier), the impl's narrow
  reading is the BUG, not the spec's "appearing comprehensive"
  wording. The spec wins. Re-derivation that finds the impl is
  silently exceeding a user-configured bound is a clause violation
  even when the impl-comment "fits" the impl's behavior.

  **Third (Round-1 L-2 surfaced an adjacent gap where a new
  invariant inherits a pre-existing imprecision — fix in-place via
  spec disambiguation rather than file as adjacent).** My first
  cut of the new clause-explicit invariant claimed merger memory
  bounded at `O(maxMergeFanIn × 64 KiB)`. Round-1 reviewer L-2
  caught that this is the bufio READ-BUFFER bound; the merge
  heap's per-slot key+value bytes (one `make([]byte, n)` per
  `readRunField` call, lifetime = one merge step) are a separate
  `O(maxMergeFanIn × max-record-size)` term inherited unchanged
  from the pre-existing `sortMerger`. The new invariant inherited
  the imprecision — claiming a total memory bound when only
  read-buffer memory is bounded. **Classified adjacent** (the heap-
  memory gap is pre-existing in `sortMerger`, cause-line predates
  this diff). **Fixed in-place** under smallest-correct-change +
  spec-amend-candidate (~10-char tightening) rather than filing as
  adjacent: while I was authoring the new invariant text, leaving
  the imprecision in the diff would be a recently-touched gap that
  future readers attribute to me. The protocol-compliant
  disposition for an adjacent finding is filing OR fixing in-place
  under smallest-correct-change structural-simpler exception — and
  the spec disambiguation is structurally trivial. Round-2 also
  caught a self-introduced L-1 (cap=4 typo in the docstring after
  my Round-1 cap=2 rewrite — left-over from the test's prior
  workload sizing). One-char fix. **Lesson**: when adding a new
  invariant that touches a pre-existing imprecisely-stated
  property, the invariant inherits the imprecision; a Round-1 L
  surfaces this even when the underlying impl is correct. The
  smallest correct change is in-place spec disambiguation (cheap
  text edit), not filing as adjacent (which would leave the new
  invariant misleading). Same trap class as `rplchain-head-tail-
  terminology-conflict` Round-1 L-1 — rewording a comment without
  auditing the underlying claim's accuracy preserves wrong framing.

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
not-cached paths), OR **when adversarial review surfaces an
iterating sequence of surface-similar findings, the symptoms are not
the bug** — there is a single architectural invariant being violated
and the real fix is a 5-line rule, not a 200-line patch
accumulation (the `btree-post-merge-underflow` cousin-cascade gap is
the canonical instance: 3 rounds of H/M findings collapsed to one
force-underflow rule once I asked "what's the invariant being
violated?" instead of "how do I patch the latest finding?"), OR **an
issue's headline framing disagrees with its parenthetical
clarification** — the parenthetical's specificity usually wins; if
the headline-shape would not fit the existing API surface, the
parenthetical is the real contract, OR **a public decoder error
class implies corruption it cannot prove** — at the byte-stream
level, `DecodeCoveringTuple` cannot distinguish on-disk corruption
from caller misuse; wrap in a neutral sentinel, not `ErrCorrupted`,
OR **a new behavior-class member needs the authoritative
enumeration paragraph extended elsewhere in the spec** — not just
the section you edited (the silent-skip enumeration in indexing.md
§Intra-transaction consistency is the canonical instance), OR **an
issue's "make OOMs" parenthetical may mean Go-runtime
`runtime.throw` not OS-level OOM-kill** — `recover()` cannot catch
`runtime.throw`, so the size-field bound MUST precede the `make`,
not wrap it via recover (the `open-corrupt-meta-size-fields-panic`
BitmapPages bound is the canonical instance: `make([]byte, hugeN)`
aborts the process from Go's runtime before any deferred recover
gets a chance to fire), OR **an issue's binary "sentinel A vs new
sentinel B — user choice" obscures a decomposable case** — the
right answer is often "neither — decompose into distinct conditions
and use distinct existing sentinels per case" (the `index-handle-
stale-after-rebuild-drop` close-out is the canonical instance: mid-
iter cursor invalidation deserves `ErrCursorStale` while post-Drop
dead-handle deserves `ErrIndexNotFound`; both proposed options in
the issue would have conflated them), OR **a fix that touches a
known spec clause may discover the impl never honored it for OTHER
cause-lines** — the existing transactions.md §Cursor invalidation
by DeleteKeyspace clause already names `*Index` handles, but the
impl has never enforced it; same-fault-class adjacent gap to file,
not to fold in (the escalation rule's "same reported fault" means
the cause-lines named in the issue, not the fault class), OR
**copying a "matching existing pattern" reset can regress an
adjacent invariant** — the chunk-7.7 M-2 per-sequence `idx.err = nil`
reset exists on iter closures for sequence semantics; copying it
to a single-shot like Stats "to match" silently destroys the
chunk-7.6 Inv-IHS1 sticky-stale signal across unrelated Stats
calls (the `index-handle-stale-after-deletekeyspace` Round-2 H-1
is the canonical instance). The escalation rule's "second
demonstrated same-fault failing case" test catches this: if the
narrower fix (e.g. reordering check sequence) already closes the
reported fault by structure, do NOT also add belt-and-suspenders
code — that adjacent code is widening without justification and
will regress adjacent invariants, OR
**closing a sentinel asymmetry via "put dead-checks first
everywhere" can break a deliberate prior contract** — the
chunk-7.6 mid-iter Drop ErrCursorStale contract pins
`idx.Err() = ErrCursorStale` even though `idx.dead = true`,
because the iter cause is the user-actionable signal at that
moment; reordering `Err()` to put `idx.dead` first regresses it
(the `index-handle-stale-after-deletekeyspace` Round-2 M-1
disposition picked the narrowest viable shape: keyspaceDead first
to close the Inv-IHS3 case, then sticky idx.err to preserve the
chunk-7.6 Inv-IHS2 mid-iter case, then idx.dead last), OR
**a spec-tier invariant claim encoded in the spec needs a
regression test when code exists** — per CLAUDE.md Project
invariants the *strongest artifact the project stage affords*
governs; code-stage means a test, not just spec prose (the
`index-handle-stale-after-deletekeyspace` Round-3 M-1 is the
canonical instance: the spec recorded "drop-then-delete reports
ErrKeyspaceClosed (broader truth)" but no test pinned the
entry-method guard ordering — a refactor swapping
`idx.dead`-first / `keyspaceDead`-first would pass the suite
while violating the spec), OR
**a "profile-driven" issue framing may hide a clause-explicit
spec defect** — the `shallow-savepoint-clone-cost` issue was
filed as profile-driven ("when overhead is measurably material")
but re-derivation against `transactions.md §Nested Transactions`
found the cost clause being violated; same pattern as
`bitmap-rollback-undo-log` (`0893be5`). Lesson: when an issue
frames work as "perf concern, defer until measured," check the
spec for a cost/timing clause the mechanism may be violating —
if found, the work is correctness, not perf, OR
**a fix that instruments N structurally-identical mutation sites
must enumerate them by pattern match, not by walking the original
instrumentation list** — the `shallow-savepoint-clone-cost`
Round-1 H-1 was AllocPage's LaggingReaderWait branch buried in a
switch case, structurally identical to the bitmap-hit branch but
missed by linear enumeration. A fast-path branch in a switch is
easy to miss unless you grep for the pattern that defines a
mutation site, OR
**replacing a broad cleanup mechanism with per-mutation undo
entries requires explicitly handling every case the broad
mechanism covered** — the `shallow-savepoint-clone-cost` Round-1
H-2 surfaced this: the pre-fix `dirtyKeys`-set + iterate-and-drop
loop in Restore silently covered TWO cases (in-window-CoW where
loose-pop's original buf must be re-attached; in-window-alloc
where loose-pop's buf was the in-window install itself and
must NOT be re-attached). The per-savepointUndoLog undo entries
handled case 1 but not case 2 — the loose-pop's dirty-detach is
in loosePopLog, not savepointUndoLog, so the buffer pointer
identity (which decides "pre-window vs in-window") is only
known at loose-pop time. Fix: `wasPreWindow bool` on
loosePopEntry, captured by scanning sp's window slice of
savepointUndoLog for a prior `(fieldDirty, id, false)` entry.
Lesson: when a single broad mechanism is being replaced by
per-mutation undo, enumerate the cases the broad mechanism
covered separately and verify each is closed, OR
**an issue's "user choice between Option A (panic guard + spec
amend) vs Options B/C (keep case in-spec, add bookkeeping)" may
look formally symmetric but the smallest-correct-change reading
depends on the production-caller audit** — the `nested-shallow-
loose-pop-buffer-alias` issue framed both shapes as user-decision
candidates; first-principles audit found NO production caller
exercised nested-shallow (the 6 per-row helpers each open-and-
resolve one shallow per call, no nesting), making Option A the
smallest correct change by construction and Options B/C
over-engineering for a non-existent caller (Quality bar defect,
not diligence). Lesson: when an issue's candidates are
"A = panic + spec amend removing the case from spec" vs "B/C =
keep case in-spec via bookkeeping," run the production-caller
audit BEFORE picking. If no production caller nests, A is the
smallest correct change; B/C carry ongoing cost for hypothetical
callers and should be rejected, OR
**a panic guard's "narrowest check" is sometimes structural
rather than positional — full-stack scan can be narrower than
topmost-only** when an inner sub-window's resolution restores
the forbidden configuration. The `nested-shallow-loose-pop-
buffer-alias` Round-1 reviewer initially questioned whether
the full-stack scan was widening vs a topmost-only check; it
isn't. A topmost-only check would permit [SHALLOW, NESTED,
attempt SHALLOW] because the topmost entry is NESTED, but once
the inner NESTED resolves the [SHALLOW, SHALLOW] alias
configuration is back and a loose-pop in the inner SHALLOW's
remaining window produces the buf alias. Lesson: verify a guard's
reachability set covers ALL paths back to the forbidden state,
not just the immediate begin — sometimes that requires a
full-stack scan, OR
**when a fix narrows a primitive's spec by removing a buggy
code path, existing tests that exercised the path may pin
KIND-AGNOSTIC substrate properties worth keeping** — the
`nested-shallow-loose-pop-buffer-alias` close-out's two
existing tests (`TestShallowSavepointOutOfOrderPanics` for the
LIFO panic; `TestSavepointRestoreOuterRevertsInnerReleasedWork`
for the per-pager log shared-by-outer semantic) exercised
shallow-inside-shallow but tested kind-agnostic substrate
properties that survive in the NESTED-inside-NESTED case
(same code paths through `RestoreSavepoint`/`ReleaseSavepoint`,
same `savepointUndoLog` lifecycle). Retargeting them to
`BeginSavepoint` preserved the coverage without deletion.
Lesson: when narrowing a primitive's spec, audit existing
tests that exercised the removed path. The distinguishing
question is "if the substrate had been correct, would this
test have still been worth running?" — if yes, retarget to a
surviving kind; if no, delete. Don't reflexively delete tests
that started failing on the new guard, OR
**a "profile-driven" issue may converge on EITHER a clause-explicit
cost-clause violation OR a preference-disguised-as-invariant —
distinguish by *which* cost dimension scales.** Same-shape framing
as `bitmap-rollback-undo-log` (`0893be5`) and `shallow-savepoint-
clone-cost` (`43ac8df`) — both re-derived as clause-explicit fixes
because their unenumerated cost terms scaled with `MaxSize`. The
`rplsegments-clone-cost` case looked surface-similar but the chain
length is workload-history-dependent (lagging reader pins
`reclamationBound` → chain grows across commits), NOT `MaxSize`-
scaling. Strict cost clause "not total database size" is satisfied;
only the auxiliary "small constant in practice" hand-wave can be
falsified — slow ≠ wrong/unsafe, no demonstrated fault, the claim
is a preference not an invariant per Project invariants. Smallest
correct change: drop the preference from the spec (no code change),
NOT implement the substrate (over-engineering). 2:1 split across
three profile-driven closes shows the right disposition is not
predictable from the "profile-driven" framing alone, OR
**citing a code symbol in authoritative spec text requires a
signature-grep, not recall** — the `rplsegments-clone-cost` spec
amend (`b83846c`) cited `finalizeRPLChain` from memory after
reading the function body but not the signature line; the actual
function is `appendRPL` (`commit.go:238`). Round-1 reviewer caught
it with a mechanical `grep -n "^func.*finalizeRPLChain"` → 0 hits.
A phantom symbol cite into authoritative spec is a fault even when
the surrounding claim is correct: future readers / tooling will
follow the cite to a missing symbol. Cheap defense: explicit
signature-grep before the spec edit lands, OR
**a spec amend that inherits pre-existing wording may inherit a
co-located naming conflict the local spec defines elsewhere** —
the `rplsegments-clone-cost` close-out's spec phrase "`reclaimRPL`'s
head trim" was carried over from prior spec text. The chain
orientation defined at `pager.go:369-370` ("tail (index 0, oldest)
→ head (last, newest)") makes the existing `trimRPLChainHead`
function name a misnomer — it trims the chain's tail per the
local convention while its name says "head". Filed-as-adjacent
(`rplchain-head-tail-terminology-conflict.md`) because the
cause-line predates the change set; the diff arbiter classifies it
adjacent (not introduced), and the protocol-compliant disposition
for an adjacent M is filing, not in-place fix. Lesson: when a
spec amend carries over a phrase verbatim, verify the phrase
aligns with the local spec's other naming conventions; if not,
the adversarial reviewer may surface the conflict and the right
disposition is filing-and-proceed, not widening the change set, OR
**an issue's enumeration of fix-sites is a first-pass finding, not
a complete set — run the wide grep audit BEFORE committing to
scope.** The `rplchain-head-tail-terminology-conflict` issue
(`9d060ba`) listed 5 sites (4 in freespace.go + 1 in
transactions.md); a wide grep audit added 3 more (commit.go:235
pre-existing comment using slice-front idiom; savepoint.go:189
inherited from `b83846c` amendment; transactions.md:472 second
instance of "drain head segments" in the same paragraph as the
issue's enumerated site). The pre-pick advisory's blast-radius
check catches the function rename's call sites but misses the
broader textual misnomer in adjacent text and in spec amendments
that landed after the issue was filed. Lesson: when an issue
enumerates fix-sites, treat the list as the original reporter's
first-pass finding and run a wide grep audit (the same
greps you'd use during scope completion review) BEFORE estimating
scope to the user — discovering audit-found additions mid-fix
forces a re-confirmation under the Honest scope estimation rule
when a pre-fix sweep would have presented the full scope upfront,
OR
**a "pure docs/naming, no correctness" diff can carry a project-
invariant promotion opportunity that the diff's own framing
hides** — the `rplchain-head-tail-terminology-conflict` (`9d060ba`)
was framed as "pure docs/naming cleanup; no correctness
implication," but Round-1 reviewer surfaced spec-amend candidate:
the chain-orientation invariant (`pager.go:369-370`) is
recorded-only — godoc + free-space.md spec text — with no
multi-segment ordering test. Existing single-segment fixtures
cannot distinguish tail-first from head-first drain. Per CLAUDE.md
Project invariants, recorded-only is weaker than enforced when
stronger is available; the strongest reachable encoding for a
code-stage project is a regression test. User picked
"add-test-now" over "file-as-new-issue"; the
`TestRPLChainOrientationMultiSegment` test (3 segments, partial
reclamation bound, identity-tested survivor) promotes the
invariant to enforced-by-test, neuter-verified. Lesson: when a
docs-only diff touches a domain concept, audit whether the
underlying invariant is enforced — the spec-amend-candidates
channel surfaces the promotion opportunity without widening the
diff's intent. The trigger to check: a diff that touches a
domain concept (the Project-invariants trigger fires); ask "is
the relevant invariant recorded-only, or enforced?" before
shipping, OR
**rewording an existing comment without auditing the underlying
claim's accuracy preserves the wrong framing** — the
`rplchain-head-tail-terminology-conflict` Round-1 L-1 was the
original `freespace.go:440-441` "copy-trim to free the head slot
for GC" rationale being wrong for value-type slices.
`RPLSegmentRef` is `{PageID uint64; TxnID uint64; Count uint32}`,
no pointers — there is no per-element GC concern; the backing
array is one allocation kept alive as long as any slice
references it. My initial reword fixed the "head"/"tail" misnomer
but inherited the GC-eligibility framing. The actual benefit of
copy-trim over `s = s[1:]` is **capacity preservation**: `s[1:]`
shrinks `cap` by 1 (Go slice cap is measured from the data
pointer), forcing earlier reallocation as the chain re-grows;
copy-trim preserves trailing slice capacity so the next
`appendRPL` reuses the same backing array. Classified as adjacent
(cause-line predates the diff) but fixed in-place under the
smallest-correct-change structural-size rule (already inside the
comment doing the rename reword, leaving misleading text I just
edited would be poor stewardship). Lesson: when rewording an
existing comment, audit the underlying claim's accuracy, not just
the surface language. The reword step is the natural place to ask
"is the existing claim even right?" before pressing on — the
adversarial reviewer is the safety net, but the cheaper defense is
to slow down at the reword and audit first, OR
**a flake framed as "GC timing variant" may have a deeper root
cause: a test's wait shape that doesn't match the wait semantics
of the function under test** — the `leaked-readtx-cleanup-race-flake`
(`5800299`) case framed the flake as "finalizer scheduling latency
outruns bounded wait," but first-principles re-derivation found
`BeginRead` with no-deadline `ctx` returns `ErrReadersFull`
IMMEDIATELY — it does NOT block. The test's goroutine + 5s
wall-clock timer assumed `BeginRead` blocks; it doesn't. The 5s
timer never fired because the goroutine returned with the error
in microseconds. No "race-cost-proportional wait bound" or
extra `runtime.GC()` cycles would have closed the flake — the
wait shape was wrong by construction, not merely slow. The
write-path counterpart `TestLeakedTxReleasesWriteLock` passes
20/20 because `Begin(ctx, true)` DOES block on the writer-flock
channel — the asymmetry is the diagnostic signal. Lesson: when an
issue frames a flake as "GC timing variant," verify the test's
wait SHAPE matches the wait SEMANTICS of the function under test
— a goroutine + timer assumes the function blocks; if it returns
immediately with an error, the wait is structurally wrong, OR
**an issue's "expose a hook on X" resolution suggestion may
point at the wrong subsystem layer** — the
`leaked-readtx-cleanup-race-flake` Option 1 framing said "hook on
closeGate / cleanup pipeline." Per first-principles re-derivation,
the right placement is at the TAIL of `readTxCleanupFn`'s
active-release path (after `info.coord.ReleaseReader(info.slot)`),
NOT on `closeGate` — which would fire too early (before atomic
stores complete) or on the wrong cleanup (closeGate fires for
write-tx cleanups too, wrong granularity). The package-level
`atomic.Pointer[func()]` pattern mirroring
`writeRegistryFailHookForTest` is the right shape; a `closeGate`
method would be the wrong layer. Lesson: when an issue's hook
suggestion names a subsystem, trace the required happens-before
to verify the hook actually belongs there — the suggestion may
point at a layer that fires too early or with wrong granularity, OR
**production code citing the resolving issue path during the same
change set that closes it still violates the no-cite invariant**
— the `leaked-readtx-cleanup-race-flake` Round-1 M-1 was an
introduced production godoc citing
`docs/issues/leaked-readtx-cleanup-race-flake.md` as the rationale
source. Per CLAUDE.md Issue triage's no-cite invariant,
authoritative Spec and production code cite only a kept-current
artifact (Spec section, enforced invariant, test name) or a
`git log` mechanism — NEVER a tracking artifact, even during the
same change set that deletes it. Fixed by retargeting the cite
to the test name +
`git log --all -S readTxCleanupHookForTest`. Lesson: when adding
new production godoc that references a fix's rationale, default
to "cite the test name + `git log` mechanism"; the no-cite
invariant is static — it fires regardless of whether the cite
would be stale-on-merge or not. Promote-then-delete makes the
cite acceptable only AFTER retarget, not by intent to delete, OR
**a cost clause's "worst-case I/O is X" may quantify only ONE
cost dimension (e.g. writes) — the unqualified word "I/O" does
not imply comprehensive coverage** — the `compaction-full-forest-
walk-per-pass` (`91a268a`) `§Cost per pass` said "worst-case I/O
is `CompactionBatchSize × (1 + depth) × PageSize`" but was
immediately followed by "the slab must hold the whole cascade"
— the slab is the in-memory CoW write buffer, so the clause
is exclusively about pwrite I/O. The read cost ("Walk every
B+tree in the forest" per §Mechanism step 1) was documented but
NOT bounded in the cost clause. Strict clause SATISFIED for
writes; read cost is a separately-unenumerated dimension. The
diagnostic signal is the clause's adjacent context ("the slab,"
"the buffer," "the cascade") — these are write-side anchors.
Lesson: when a cost clause says "worst-case I/O is X," audit
which DIMENSION X quantifies; do not read coverage as broader
than the expression. The dimension-aware MaxSize-scaling test:
write cost is bounded by clause; check read cost separately —
if it's MaxSize-scaling it's a fix, if workload-history-
dependent it's a preference. The 3:3 split across profile-driven
closes (0893be5 / 43ac8df / 5800299 → fix; b83846c / 9d060ba /
91a268a → preference) is independent of which cost dimension is
being asked about, OR
**an issue's "Related" or "Adjacent" sub-concern is an
independent piece of work — re-derive each separately, do NOT
assume it shares the main concern's disposition** — the
`compaction-full-forest-walk-per-pass` (`91a268a`) bundled a
"Related: compaction self-signals fragmentation trigger" sub-
concern (relocateOverflowChain's AllocContiguous bumps the same
contigAttempts / contigFragFails the trigger reads). The issue's
own framing characterized it as "self-limiting … arguably
desirable … not a correctness defect" — preference shape. First-
principles re-derivation agreed: both main and Related were
preferences this session. But they could have gone differently
— if the spec had a clause about trigger-metric-purity, the
Related alone might have been a clause violation while the main
remained a preference. Lesson: an issue's "Related" / "Adjacent"
sub-section is its own clause-explicit / entailed invariant
check (read the relevant spec section; derive the invariant with
statable violation=; disposition independently). The sub-
concern's spec promotion can be folded into the same close-out
commit (this session: brief §Trigger note on intentional
inclusive count), but the derivation MUST be separate, OR
**a profile-driven issue may resolve to a "fix" outside the
MaxSize-clause-violation / preference-drop binary — re-derive
whether a structurally-simple consolidation eliminates the
redundancy.** The `setkeyspace-indexing-perf-and-edge` close (this
session) fit NEITHER bucket: no spec clause was being violated
(bulk-op cost clause already permits the 2× constant factor); but
the redundancy ALSO wasn't a preference-to-drop (no spec text
recorded the implicit "one snapshot per op" expectation). Third
class: a constant-factor inefficiency where the cleanup is
structurally small and correct-preserving. The two layers were
*historically* both load-bearing (chunk-7.6 H-2 wrapper-internal
snap + chunk-7.9 caller rowSnap) but after chunk-7.9 the caller
layer subsumed the wrapper's contract entirely. Disposition: fix
(not preference-drop), because the cleanup is small and
"smallest correct change" applies to code shape even when the
bigger O isn't violated. Lesson: the 3:3 profile-driven split
shouldn't pre-decide between fix and preference-drop — re-derive
whether a structurally-simple consolidation eliminates the
redundancy (third bucket: cleanup-fix) before assuming the only
options are "MaxSize-scaling fix" or "preference-drop", OR
**every caller-site bearing an encoded invariant deserves its own
test, not "one test per distinct mechanism" — empirical neuter-
verify is the gate, not mechanism-sharing arguments by
inspection.** The `setkeyspace-indexing-perf-and-edge` Round-1 M-2
finding: I initially encoded the consolidated atomicity invariant
with 3 tests covering 3 mechanisms (Put helper / Delete helper /
Bulk-loop), reasoning that the remaining 3 caller sites
(Cursor.Delete + SetKeyspace.Put + SetKeyspace.DeleteValue)
"share mechanism with the encoded sites" and don't need individual
tests. Reviewer empirically neuter-verified that removing the new
caller-side `restoreIndexes(...)` at `set_keyspace.go:726`
(SetKeyspace.Put) caused ZERO existing test failures — the
encoding gap was real, not speculative. "Smallest correct change"
governs PRODUCTION code; for TESTS, the rule is "every caller-site
line that bears an encoded invariant deserves a test whose removal
regresses the line." The "speculative catalogue" prohibition
applies to recording NEW invariants, not to test coverage of
caller-sites that already bear one. The boundary: same invariant
+ same mechanism + different cause-line = different neuter target
= different test. Added 3 more tests for the 3 uncovered caller
sites; all 6 now neuter-verified individually, OR
**the no-cite invariant fires on TEST files too, including during
the same change set that closes the issue — same trap class as
`5800299`'s production godoc cite.** The
`setkeyspace-indexing-perf-and-edge` Round-1 M-1: I cited the
issue slug in the new
`TestSetKeyspaceBulkDeletePinnedStateRevertsAfterMidLoopFailure`
godoc as "originally tracked as item B of <slug>". Test files are
kept-current artifacts; the cite is no different from a code
comment cite and is dangling-on-merge once the issue file is
deleted. Same trap pattern surfaced by `5800299` for production
godoc; this session shows it extends to test godoc. Adjacent
in-place fix: `index_types_test.go:240`'s pre-existing cite of
the same slug for the closed item-C reference — same anti-
pattern, classified adjacent per the diff arbiter; fixed in-place
under smallest-correct-change because we were already doing a
wide grep-and-fix for close-out. Lesson: default to citing chunk
numbers (kept-current via `git log --grep="chunk-N"`) + the
test's own neuter clause when describing a test's rationale; reach
for an issue-path cite ONLY when no durable anchor exists, which
for a closing fix is never (the chunk evolution chain plus the
test's neuter assertion are always available), OR
**surface-framing similarity to prior preference-drops does not
entail disposition similarity — check the THREE dimensions before
inferring "preference too"** (the `bulkload-index-merge-run-fanin`
(`ac4af82`) case framed as "profile-driven, matches b83846c /
91a268a" got CORRECTLY pushed back on by the user with "is this a
rationalization?" The lazy match collapsed under three concrete
distinguishing dimensions: (i) foreground-tx (bulkload merger) vs
background goroutine (91a268a) or degraded-state operation (b83846c);
(ii) OS-hard-limit (bulkload's EMFILE per-process FD limit) vs
CPU-only (b83846c) or background-I/O (91a268a); (iii) user-bound-
exceeded (bulkload's merger read buffers silently doubled the
MaxTxBufferBytes footprint) vs intrinsic-cost (the prior two had
no user-configured bound on the affected dimension). The prior
preference-drops were correctly identified — they pass all three
dimension checks. THIS case fails all three. The 3:3:2:1 split
across nine profile-driven closes (`0893be5` / `43ac8df` /
`5800299` → clause-violation fix via substrate; `b83846c` /
`9d060ba` / `91a268a` → preference → spec-amend only; `de9e7c1` →
cleanup-fix bucket; `400c95d` → spec-internal-inconsistency-
close bucket; `ac4af82` → clause-violation fix on the FD +
read-buffer dimension via cascaded multi-pass merger) shows the
right disposition is a per-candidate dimension audit, not a
prior-bucket pattern-match. Lesson: when applying the established
re-derivation rubric, run the THREE-DIMENSION check (foreground/
background, OS-hard-limit/CPU, user-bound-exceeded/intrinsic)
PER CANDIDATE before assigning a bucket. Framing similarity is
NOT disposition entailment, OR
**a spec phrase's broad-reading is authoritative even when the
implementation interprets narrowly — the impl's narrow reading is
the bug, not the spec's "appears comprehensive" wording.** The
`bulkload-index-merge-run-fanin` (`ac4af82`) case found
`bulkload.md §Interaction with Indexes` line 192 "keeping the
combined in-memory sort footprint bounded by MaxTxBufferBytes" was
being read narrowly by the impl (= phase-1 sorter accumulator
chunk only, matching impl comment at `bulkload_indexed.go:59`)
while a user reasonably reads it broadly (= entire sort's in-
memory state including the merge phase's 64 KiB-per-run read
buffers). The impl's narrow reading silently let the merger
allocate 256 MiB of additional read-buffer memory outside the
user-configured 256 MiB budget — a clause violation under the
broad reading. Per CLAUDE.md the spec is authoritative; the
impl conforms to spec, not the reverse. Lesson: when an impl
comment narrows a spec phrase's scope ("X = phase-1 only" when
spec says "X" without qualifier), the impl's narrow reading is
the BUG. Re-derivation that finds the impl silently exceeds a
user-configured bound is a clause violation even when the impl-
comment "fits" the impl's behavior. The diagnostic test: read
the spec phrase the way a USER reads it (without the impl's
comment in front of you); if a user would expect the broader
scope and the impl silently exceeds it, that's a violation, OR
**adding a new invariant that touches a pre-existing imprecisely-
stated property — the invariant inherits the imprecision; fix
in-place via spec disambiguation, not file as adjacent.** The
`bulkload-index-merge-run-fanin` (`ac4af82`) Round-1 L-2 surfaced
this: my first cut of the new clause-explicit invariant claimed
merger memory bounded at `O(maxMergeFanIn × 64 KiB)` — but that's
the bufio READ-BUFFER bound; the merge heap's per-slot key+value
bytes (one `make([]byte, n)` per `readRunField` call, lifetime =
one merge step) are a separate `O(maxMergeFanIn × max-record-
size)` term inherited unchanged from the pre-existing
`sortMerger`. The new invariant inherited the imprecision —
claiming a total memory bound when only read-buffer memory is
bounded. Classified adjacent (cause-line pre-existing) but fixed
in-place under smallest-correct-change + spec-amend-candidate (a
~10-char tightening saying "bufio read-buffer memory" with a
clarifying clause about the heap-memory term). Lesson: while
authoring NEW spec-tier invariants for a recently-touched area,
the new prose can pick up adjacent pre-existing imprecisions that
make the new invariant misleading even when the impl is correct.
The smallest-correct-change disposition is in-place
disambiguation (cheap text), not filing as adjacent (which would
leave the new invariant misleading). Same trap class as
`rplchain-head-tail-terminology-conflict` Round-1 L-1 —
rewording a comment without auditing the underlying claim's
accuracy preserves wrong framing.

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
   **When the review surfaces a sequence of similar findings,
   stop and ask: "is there a single architectural invariant being
   violated?" — if yes, fix the invariant, not the symptoms.**
6. **Close-out (promote-then-delete)** per §Issue triage. Wrap-aware
   cite search of authoritative spec (`docs/specs/*.md`) and production
   `.go`. Promote load-bearing rationale inline into kept-current
   artifacts (code comments, spec sections). Delete the
   `docs/issues/README.md` row AND the issue file. The v0 plan was
   deleted at its own close-out (`git log --all --
   docs/plans/v0-implementation.md` recovers it); the active plan is
   `docs/plans/architecture-consolidation.md`.
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

This session closed `bulkload-index-merge-run-fanin` (commit
`ac4af82`) — picked per the user's confirmation of the
recommendation at session-start. Re-validated on HEAD `a0093e7`:
`bulkload_indexed.go` `newMerger` opens all spilled-run files
simultaneously for a single k-way merge — `O(#runs)` open FDs +
64 KiB bufio per run, exactly as the issue framed. First-
principles re-derivation against `bulkload.md §Interaction with
Indexes` line 192 ("keeping the combined in-memory sort
footprint bounded by MaxTxBufferBytes") found two readings:
narrow (impl-comment, accumulator-only) vs broad (user
expectation, entire sort including merger). Under broad reading
the merger's 64 KiB-per-run buffers are a clause violation
(256 MiB at 4000 runs doubles the user-configured 256 MiB
budget); FD dimension separately hits per-process EMFILE
(macOS default 256, Linux 1024). 

**User-pushback corrected my lazy framing.** My first cut framed
the disposition as "preference-drop + spec-amend, mirrors
b83846c (rplsegments-clone-cost) and 91a268a (compaction-full-
forest-walk-per-pass)" — a surface-similar pattern-match. User
pushed back with "is matching b83846c and 91a268a a
rationalization?" Re-examination via THREE dimensions found
bulkload differs from both prior preference-drops on:
(i) foreground vs background (bulkload runs in user write tx;
b83846c is degraded-state savepoint Begin; 91a268a is
maintenance goroutine);
(ii) OS-hard-limit vs CPU-only (bulkload's EMFILE is a per-
process FD limit; b83846c is CPU + heap; 91a268a is background
read I/O);
(iii) user-bound-exceeded vs intrinsic (bulkload's merger read
buffers silently doubled the user's MaxTxBufferBytes; the prior
two had no user-configured bound on the affected dimension).
b83846c and 91a268a pass all three dimension checks → defensible
preferences. THIS case fails all three → clause-violation fix.

User picked Option C (cascaded multi-pass merger; issue's
proposed remediation; ~200 LOC) over Option A (preference-drop
spec amend; ~30 LOC; rejected after dimension analysis) and
Option B (cleanup-fix per-run buffer cap; ~15 LOC; rejected
because leaves the FD dimension uncovered).

Mechanism: `var maxMergeFanIn = 128` cap + new
`(*indexSorter).cascadeRuns()` + `mergeGroupToScratchRun(
scratchDir, runs)`. Cascade reduces `len(s.runs)` to ≤ cap via
groups of ≤ cap runs each, repeating until cap fits. Pre-level
runs removed only AFTER next level fully writes (mid-pass-error
safety). Bounds open FDs + bufio read-buffer memory at O(cap)
regardless of #runs, costs O(log_fanin(#runs)) extra scratch
I/O. Preserves unique-violation-at-merge-output (detection runs
on the post-cascade stream). New `setMaxMergeFanInForTest(n)`
swap-restore + `bulkLoadMergeCascadeHookForTest` (mirrors
`readTxCleanupHookForTest` from `5800299` /
`SetDeleteRangeCalledHookForTest` from `400c95d`).

Spec promotions: `bulkload.md §Invariants` new clause-explicit
invariant for the cap with gitfs SQLite → gmdb migration as
violation= (disambiguates bufio read-buffer memory vs heap
key+value memory per R1 L-2); `§Interaction with Indexes` new
"**Merge fan-in cap.**" subsection describing cascade mechanism,
intermediate-run lifecycle (next-level-writes-before-prior-
level-removed safety), per-pass FD ceiling (cap+1), end-to-end
memory contract.

1 new regression test (neuter-verified):
- `TestKeyspaceBulkLoadIndexedMergeCascadeBoundsFanIn`
  (`setMaxMergeFanInForTest(2)` + 900 rows × 8 KiB
  MaxTxBufferBytes; `preRuns >= 2*testCap+1` probe defends
  multi-pass exercise against silent workload shrinkage; pins
  postRuns ≤ cap + end-to-end correctness + scratch cleanup;
  neuter-verified by commenting out `s.cascadeRuns()` →
  postRuns=6 > cap=2 → deterministic fail).

R1=0H/0M/3L/3nit (introduced L-1 docstring math fragile → fixed
in-place via workload-drift-resilient phrasing; adjacent L-2
new invariant inherits merger heap-memory imprecision → fixed
in-place via spec disambiguation "bufio read-buffer memory" +
separate heap-memory clause; introduced L-3 single-element
cascade-group rewrite disputed as design-uniform; introduced
nit-1 cascadeRuns panic-window disputed unreachable; nit-2
panic guard sound; introduced nit-3 per-index hook scope by-
design).
R2=0H/0M/1L/0nit (introduced L-1 cap=4 typo in docstring after
R1 cap=2 rewrite → fixed in-place). Converged.

3:3:2:1 split now across nine profile-driven closes:
`0893be5` / `43ac8df` / `5800299` → clause-explicit violation →
fix via substrate; `b83846c` / `9d060ba` / `91a268a` →
preference → spec-amend only; `de9e7c1` → third-class cleanup-
fix (no clause violated, no preference to drop, structural
redundancy consolidation); `400c95d` → spec-internal-
inconsistency-close (set-keyspace.md line 17 walker promise +
spec ↔ impl gap + clean-break atomicity realignment); THIS
session → clause-violation fix on the broad-reading of "in-
memory sort footprint" (FD + read-buffer dimension) via
cascaded multi-pass merger.

Prior session closed `setkeyspace-delete-range-bulk-walker` — see
commit `400c95d` for that change set's details.

| Commit | Issue | Outcome |
|--------|-------|---------|
| `24ec951` | cursor-err-unpositioned-state | Closed (sentinel translation) |
| `ddb3831` | rpl-rebuild-panic-on-wild-pointer | Closed (Inv-RV3 bound) + filed adjacent `open-corrupt-meta-size-fields-panic` |
| `ab2d239` | kind2-one-parent-reachability-test | Closed (enforced test) |
| `e40cbdc` | setkeyspace-put-redundant-membership-probe | Closed (2 single-descent btree primitives) |
| `c1effd2` | writenewindexregistry-partial-leak | Partial — `writeNewIndexRegistry` site done via nested savepoint; 4 siblings remained |
| `0893be5` | bitmap-rollback-undo-log | Closed (undo-log substrate + spec amend in transactions.md §Nested Transactions + new `Discard` API) |
| `15f9b70` | writenewindexregistry-partial-leak per-row case | Partial — 6 per-row sites done via new `Pager.BeginShallowSavepoint` substrate; 3 cold-path DDL siblings remain. Filed `shallow-savepoint-clone-cost.md` (residual per-Begin clone cost). |
| `27361ac` | writenewindexregistry-partial-leak DDL siblings | Closed — `Tx.RebuildIndex`, `Tx.DropIndex`, `Keyspace.DeleteKeyspace` retirement wrapped in nested savepoint via defer-named-return |
| `f1d9ad7` | btree-post-merge-underflow | Closed — strict fill-floor invariant added to `range-delete.md §Invariants`; mechanism: (a) `mergeOrRedistribute*` callers do post-merge re-rebalance + cousin propagation; (b) architectural force-underflow rule — a level returning `deepUnderflowChild != 0` reports `underflow=true` regardless of encoded fill; (c) top-level final-heal pass. Three rounds + root-cause analysis collapsed a 1400-LOC complexity accumulation to a 5-line rule. |
| `682fa70` | byte-api-covering-return-unwired | Closed — byte-API branch added to `extractPKAndValue` returning encoded covering blob verbatim when `len(decl.Covering) > 0`. New public `DecodeCoveringTuple` + `ErrCoveringTupleMalformed` (NOT wrapped in `ErrCorrupted` — neutral sentinel). Spec promoted into `indexing.md §Covering Indexes` + `typed-keyspaces.md §Covering` + `api-surface.md`. 8 regression tests. |
| `5750827` | open-corrupt-meta-size-fields-panic | Closed — two walk-site bounds in `internal/pager/init.go` step 4 (bitmap rebuild): (1) firstDataPage = BitmapPages+2 ≤ min(fileSize/PageSize, MaxSize) catches wild-high BitmapPages before `make` triggers Go-runtime OOM throw; (2) BitmapPages*PageSize*8 ≥ MaxSize catches under-sized BitmapPages before `bitmap.New`'s totalPages-exceeds-capacity panic. Two-bound shape derives from escalation rule (Fault-ii is second demonstrated same-fault case Fault-i bound leaves failing). |
| `a114172` | index-handle-stale-after-rebuild-drop | **Closed** — mirrors chunk-5.6 markCursorsStale pattern for `*Index`. New fields: `Index.{openCursors,dead}` + `Keyspace.openIndexHandles` + `SetKeyspace.openIndexHandles`. New helpers per keyspace: `markIndexHandlesStale` (mark-all, called by Put/Delete/etc via markCursorsStale consolidation), `markIndexHandleStaleByName` (RebuildIndex), `markIndexHandleDead` (DropIndex). Iter closures (iteratePrefix / Range / LookupKeys non-unique) register/unregister via defer. Dead-handle check at Stats/Lookup/LookupKeys/Range/Prefix/Get entry. `mapIndexCursorErr` translates btree.ErrCursorStale → gmdb.ErrCursorStale (same sentinel-leak class as 24ec951). Sentinels: ErrCursorStale (mid-iter) + ErrIndexNotFound (post-Drop) — NO new sentinel. Spec promotions: `indexing.md §Handle Invalidation` (new) + `api-surface.md` Lookup/RebuildIndex/DropIndex godoc updates. R1=0H/1M/3L/1nit; R2=0H/0M/0L (reviewer neuter-verified the two new tests fail deterministically when calls removed). 12 regression tests. Filed adjacent `index-handle-stale-after-deletekeyspace` (pre-existing spec-impl gap, different cause-line). |
| `3eb3488` | index-handle-stale-after-deletekeyspace | **Closed** — extends chunk-7.6 `*Index` handle infrastructure to enforce the pre-existing `transactions.md §Cursor invalidation by DeleteKeyspace` clause. New `(idx *Index).keyspaceDead()` helper; entry-time `keyspaceDead`-first guards on Stats/Lookup/LookupKeys/Range/Prefix/Get (parent-dead wins over per-handle dead → drop-then-delete reports broader `ErrKeyspaceClosed`). `mapIndexCursorErr` made a method `(idx *Index).mapCursorErr` translating `btree.ErrCursorStale → ErrKeyspaceClosed` when `keyspaceDead()` (mid-iter broader-truth wins on iter cursor path). `Tx.DeleteKeyspace`'s in-memory invalidation block walks `openIndexHandles` via `markIndexHandlesStale` on both branches. `Err()` reordered to `keyspaceDead → idx.err → idx.dead`: closes Inv-IHS3 Err-vs-Stats asymmetry while preserving chunk-7.6 mid-iter Drop ErrCursorStale contract. Stale "chunk-6.7" comment on SetKeyspace branch fixed. NO new sentinel. Spec promotions: `indexing.md §Handle Invalidation` extended with Inv-IHS3 + "Three distinct invalidation conditions" preamble; `api-surface.md` 6 method godocs updated. 11 regression tests (8 deterministic-fail-on-HEAD + 3 invariant-pinning: `TestStatsPreservesInFlightStaleSignal`, `TestErrSymmetricWithStatsAfterDeleteKeyspace`, `TestIndexHandleDropThenDeleteReportsErrKeyspaceClosed`). R1=0H/1M/3L/2nit; R2=1H/2M/3L/1nit (introduced H-1 = Stats sticky-err reset regression caught by the loop, fixed Round-3); R3=0H/1M/2nit (introduced M-1 = drop-then-delete ordering encoded-but-not-enforced); R4=0H/0M/0L. |
| `43ac8df` | shallow-savepoint-clone-cost | **Closed** — re-derived as a `transactions.md §Nested Transactions` cost-clause violation (not the profile-driven perf concern the issue framed). Per-pager `savepointUndoLog []savepointUndoEntry` + per-Savepoint `undoLogPos int` marker replaces the 4 cloned maps (`pendingAllocs`/`pendingFrees`/`loosePages`/`dirtyKeys`); mirrors bitmap layer's `0893be5`. `captureSavepointState` becomes O(1) for those fields. 10 mutation sites instrumented (AllocPage's 4 branches incl. previously-missed LaggingReaderWait; FreePage's 3; AllocContiguous; reserveBitmapRun; TailRefund; CoW; AllocSlab; AllocSlabRun). `RestoreSavepoint` order: savepointUndoLog replay FIRST, then Shallow loose-pop replay. `loosePopEntry.wasPreWindow` captured at loose-pop time by scanning `sp.undoLogPos..end` for prior `(fieldDirty, id, false)` entry — true→re-attach, false→pool-Put-no-install (closes in-window-alloc + loose-pop leak the pre-fix `dirtyKeys`-cleanup silently handled). Unifies `activeShallowSavepoints`→`activeSavepoints`. `ReleaseSavepoint` truncates log on empty active stack. Spec amend: `transactions.md §Why this is cheap` extended with two paragraphs on pager substrate + wasPreWindow. 4 new tests (`TestShallowSavepointBeginCostConstantInTxState` alloc-count assertion; `TestShallowSavepointLoosePopReCoWRestore`; `TestShallowSavepointInWindowAllocLoosePopRestoreDoesNotLeak`; `TestSavepointUndoLogTruncatesOnLastRelease`; `TestSavepointRestoreOuterRevertsInnerReleasedWork`). R1=2H/0M/2L/1nit (introduced H-1 LaggingReaderWait uninstrumented + H-2 in-window-alloc loose-pop buffer leak — both fixed in-place + neuter-verified); R2=0H/1M (adjacent, filed)/2L (doc cross-refs)/1nit (dropped). 2 issues filed: `rplsegments-clone-cost.md` (residual RPL chain clone), `nested-shallow-loose-pop-buffer-alias.md` (pre-existing nested-shallow pointer alias, unreachable in production). |
| `d9ea7d4` | nested-shallow-loose-pop-buffer-alias | **Closed** — Option 1 (panic guard + spec amend) per user pick; justified by production-caller audit (the 6 per-row indexed-maintenance helpers each open-and-resolve one SHALLOW per call, never nest). `Pager.BeginShallowSavepoint` scans `p.activeSavepoints` for any `SavepointShallow` entry and panics on hit with message "shallow savepoint already active (single-active per pager)". Check runs BEFORE `captureSavepointState` so panic leaves no `bitmap.Snapshot` leaked into `openSnapshots` and no partial mutation of `activeSavepoints`. Full-stack scan (not topmost) so [SHALLOW, NESTED, attempt SHALLOW] still panics — topmost-only would defeat itself once inner NESTED resolves. SHALLOW-inside-NESTED and NESTED-inside-SHALLOW remain allowed (NESTED's `savepointDepth > 0` suspends loose-pop inside the nested window). Spec promotions: `transactions.md §Why this is cheap` extended with a paragraph on the `loosePopLog` single-owner contract on `*[]byte`, the buf-alias mechanism, and the structural enforcement; §Write-helper error contract §Implementation Shallow bullet states the single-active rule with cross-reference. `savepoint.go` `RestoreSavepoint` step-4 godoc retargeted from the issue-doc citation to the inline spec amend. 2 new tests (`TestShallowSavepointPanicsOnNestedShallow` + `TestNestedInsideShallowSavepointAllowed`); 2 existing tests retargeted from shallow-inside-shallow to NESTED-inside-NESTED (`TestSavepointOutOfOrderPanics` for kind-agnostic LIFO discipline; `TestSavepointRestoreOuterRevertsInnerReleasedWork` for kind-agnostic per-pager log shared-by-outer semantic). R1=0H/0M/2L/2nit (all introduced; all fixed in-place + neuter-verified — L-1 hardened panic identity, L-2 new cross-kind positive test, nit-1 godoc wording, nit-2 panic message aligned to spec). R2=0H/0M/0L/0nit (converged). |
| `b83846c` | rplsegments-clone-cost | **Closed** — Option A (spec amend + close, no code change) per user pick; justified by first-principles re-derivation finding the strict `transactions.md §Nested Transactions` cost clause "not total database size" is SATISFIED (chain length grows with retired-pages-pending-reclamation count, not page count → not MaxSize-scaling, distinguishing from `0893be5` and `43ac8df`). The auxiliary "small constant in practice" claim in §Why this is cheap is a preference with no statable violation= (slow ≠ wrong/unsafe) — per CLAUDE.md Project invariants, do not record. Spec promotions: §Nesting depth cost clause amended to name `O(rplSegments chain length)` explicitly with cross-ref; §Why this is cheap replaced with honest workload-dependent prose (lagging reader → chain accumulates across writer commits; structural ceiling `MaxSize/PageSize` is the only workload-independent bound). `internal/pager/savepoint.go` godoc alignment (Savepoint struct + captureSavepointState). NO code semantic change; NO new tests. R1=1H/1M/2L/1nit (introduced H-1 `finalizeRPLChain` phantom-symbol cite → fixed in-place to `appendRPL`; adjacent M-1 `trimRPLChainHead` vs `pager.go:370` chain-orientation conflict → filed `rplchain-head-tail-terminology-conflict.md`; introduced L-1 cross-tx-vs-within-tx ambiguity → fixed in-place with explicit 43ac8df cross-ref; adjacent L-2 alloc-count test empty-chain precondition → disputed; introduced L-3 `MaxTxBufferBytes` mention → disputed); R2=0H/0M/0L/1nit (disposition-narrative mis-citation of `MaxTxBufferBytes` location, no landed artifact; ship). |
| `9d060ba` | rplchain-head-tail-terminology-conflict | **Closed** — Option 1 (rename + comment/spec align) per user pick. Issue enumerated 5 sites; wide grep audit added 3 more (`commit.go:235` pre-existing comment using slice-front idiom; `savepoint.go:189` inherited from `b83846c` amendment; `transactions.md:472` second instance of "drain head segments"). 8 sites fixed for full convention conformance (rename `trimRPLChainHead` → `trimRPLChainTail` with godoc cross-ref to SetRPLChain; capacity-preservation framing on the reclaimRPL inline comment + `trimRPLChainTail` godoc, replacing the wrong "GC the head slot" framing that pre-dated this diff; tightened appendRPL phase-1 comment; fixed `savepoint.go:189` + the two transactions.md sites + free-space.md:378-379). Spec-amend candidate #1 (chain-orientation invariant recorded-only with violation= per Project invariants) → user picked add-test-now, `TestRPLChainOrientationMultiSegment` added at `internal/pager/freespace_test.go:340-460` (3-segment chain, partial reclamation bound, identity-tested survivor) — neuter-verified by reversing `headPageID()` to read `rplSegments[0]`. Existing single-segment fixtures cannot distinguish tail-first from head-first drain; this new test is the strongest available encoding per Project invariants. R1=0H/1M/2L/2nit (introduced M-1 close-out incomplete → fixed; adjacent L-1 GC-eligibility framing wrong for value-type slice → fixed in-place with capacity-preservation framing; introduced L-2 "in the limit including head" phrasing → fixed in-place; nit-1 cross-ref disposition recorded; adjacent nit-2/spec-amend candidate #2 free-space.md slice-front idiom → fixed in-place under user's full-scope authorization; spec-amend candidate #1 chain-orientation invariant → user picked add-test, landed). R2=0H/0M/0L/0nit (converged, ship). |
| `5800299` | leaked-readtx-cleanup-race-flake | **Closed** — first-principles re-derivation found the deeper root cause is `BeginRead` returning `ErrReadersFull` immediately (no-deadline `ctx` does NOT block on slot — `internal/lock/coord_reader.go:75-77`); the test's goroutine + 5s wall-clock timer assumed blocking. No "longer wait" Option 2 would have closed the flake — the wait shape was structurally wrong. Fix: deterministic test-only synchronization hook fired at the tail of `readTxCleanupFn`'s active-release path (after `info.coord.ReleaseReader(info.slot)`). New `readTxCleanupHookForTest atomic.Pointer[func()]` + `setReadTxCleanupHookForTest` setter in `read_tx.go`, mirroring `writeRegistryFailHookForTest` / `indexMaintenanceFailHookForTest` pattern. Hook fires INSIDE the EnterCleanup/ExitCleanup window — `closeGate.BeginClose`'s drain naturally waits for it; non-blocking constraint inherited per `leak-detection.md §Cleanup Behavior`. `TestLeakedReadTxReleasesSlotViaCleanup` refactored: installs hook (buffered cap=1 channel + select-default send), leaks rtx, GCs ×2, waits on hook signal (5s fatal), then synchronously calls BeginRead + Rollback. Pre-fix flake re-validated on HEAD `7fae978` (2/10 fail under -race); post-fix 100/100 pass. Neuter-verified: removing ReleaseReader fails 3/3 with `no reader slots available`; removing hook fire fails 3/3 with 5s timeout. Production cleanup contract UNCHANGED. R1=0H/1M/1L/3nit (introduced M-1 production cite to `docs/issues/leaked-readtx-cleanup-race-flake.md` violated CLAUDE.md Issue-triage no-cite invariant → fixed in-place by retargeting to test name + `git log --all -S` mechanism; introduced L-1 godoc panic clause missing → fixed in-place; nit-1 EnterCleanup-window framing folded into inline comment; nit-2 line numbers rot → disputed; nit-3 nil-branch pattern matches local convention → disputed). R2=0H/0M/0L/0nit (converged, ship). |
| `91a268a` | compaction-full-forest-walk-per-pass | **Closed** — Option 3 (close as obsolete + spec amend, no code change) per first-principles re-derivation finding the strict `background-maintenance.md §Cost per pass` clause "worst-case I/O is `CompactionBatchSize × (1 + depth) × PageSize` … the slab must hold the whole cascade" is SATISFIED for pwrite I/O (slab = in-memory CoW write buffer). Read cost is unenumerated, scales with live B+tree node pages (workload-history-dependent — matches `rplsegments-clone-cost` `b83846c` shape, NOT MaxSize-scaling like `0893be5` / `43ac8df`); structural ceiling `MaxSize`/`PageSize` for a fully-allocated database. No demonstrated fault — slow is not wrong/unsafe; per CLAUDE.md Project invariants the implicit "small read cost" expectation is a preference. The issue's "Related: compaction self-signals fragmentation trigger" sub-concern independently re-derived as preference (the issue itself framed it "self-limiting … arguably desirable … not a correctness defect"). Spec promotions: `background-maintenance.md §Cost per pass` rewritten into "Two cost dimensions, separately bounded" — Write cost (existing material kept, with "Bounded by `CompactionBatchSize` and depth, independent of total database size" sharpening) + new Read cost paragraph naming O(live B+tree node pages) workload-dependent with `MaxSize/PageSize` structural ceiling, citing `relocateNode`'s read-then-predicate on B+tree nodes and `relocateLeaf`'s predicate-then-relocate on overflow chains (so the O() expression precisely includes nested-tree subtree pages via the recursive `relocateNode` at `relocate.go:199`, excludes overflow chain pages which are not walked); `§Trigger` gains a paragraph documenting inclusive-count is intentional self-limiting, per-tx "don't count" flag explicitly rejected. NO code change; NO new tests. R1=0H/0M/3L/1nit (introduced L-1 §Cost-per-pass §Read-cost "page" vs "B+tree node" precision: original wording was precise for B+tree node descent but imprecise for overflow-chain case at `relocate.go:175` where predicate gates the chain read → fixed in-place: tightened to "on B+tree nodes shouldRelocate(id) runs only after the page is read … overflow chains are gated the other way, predicate-then-relocate, with no walk-time read of their pages" + "page" → "node" in 3 spots; adjacent L-2 `v0-implementation.md:2062` chunk-12.6 narrative cite of the closed slug — disputed as past-tense historical fact per close-out protocol; adjacent L-3 `handoff.md` candidate-list cites — fixed via end-of-session protocol rewrite). R2=0H/0M/0L/0nit (converged, ship). |
| `de9e7c1` | setkeyspace-indexing-perf-and-edge | **Closed** — consolidated two-layer atomicity (chunk-7.6 H-2 wrapper-internal snap + chunk-7.9 caller `rowSnap`) to caller-only by deleting the four `applyIndexMaintenanceOn{Put,Delete}` wrappers (Keyspace + SetKeyspace mirrors) and renaming each `*Inner` to the public name. Folded chunk-7.6 Keyspace symmetric sites (`Keyspace.Put` / `Delete` / `Cursor.Delete`) under "structurally-larger-but-simpler" exception. Added 5 caller-side `restoreIndexes(ks.indexes, rowSnap)` lines on the helper-error branches; `SetKeyspace.Delete` bulk branch already restored. Disposition is **third class** outside the prior 3:3 split: no spec clause violated, but NOT a preference-drop either — a constant-factor inefficiency where the cleanup is structurally small and correct-preserving (so 3:3:1 split). Spec promotions: chunk-7.6 H-2 → chunk-7.9 evolution chain moved inline into renamed `applyIndexMaintenanceOnPut` godoc + restructured `indexSnapshot` type godoc (3-bullet Capture/Restore/Purpose contract); each caller's helper-error branch comment documents the snapshot-less helper contract. 6 new regression tests (one per caller site, each neuter-verified individually — produces deterministic pinned-mutated failure on line removal). R1=0H/2M/2L/1nit (introduced M-1 = no-cite invariant violation in new test godoc citing the slug → fixed in-place by retargeting to chunk-7.6 → chunk-7.9 evolution chain + git log mechanism; ADJACENT same-class pre-existing cite at `index_types_test.go:240` → fixed in-place; introduced M-2 = test coverage gap, only 3 mechanisms had tests but 6 caller sites bear the encoded invariant individually, reviewer empirically neuter-verified the SetKeyspace.Put line could be removed with zero suite failures → fixed by adding 3 more tests; L-1/L-2 = bulk-test godoc phrasing → fixed; nit-1 = indexSnapshot godoc structure → restructured to lead with current 3-bullet contract). R2=0H/0M/0L/0nit (converged, ship). |
| `400c95d` | setkeyspace-delete-range-bulk-walker | **Closed** — un-indexed `SetKeyspace.DeleteRange` migrated from chunk-6.8 v1 snapshot-then-Delete loop (`O(K log N)` per-key descents + `O(K × keysize)` upfront snapshot) to the chunk-5.7 atomic three-phase walker via new `btree.PerCellFreeFn` callback abstraction. Indexed path preserved per-row via lifted `deleteRangePerKey` (chunk-7.10 contract retained). Atomicity change for un-indexed: per-row → atomic (`(0, err)` on failure), clean break of chunk-6.8 user-lock per pre-v1 default. `keyspaceCellFree` (overflow + count=1) + `setKeyspaceCellFree` (subpage Count / NestedCount via `FreeSubtree` with NestedCount sanity check / overflow + count=1) callbacks; the walker stays SetKeyspace-agnostic. Spec promotions: `range-delete.md §Set Keyspace Range Delete` (new section), `set-keyspace.md` §17 narrative align + §Invariants new entailed dispatch-direction invariant, `api-surface.md` symmetric atomicity-contract godoc on both `SetKeyspace.DeleteRange` (new spell-out) and `Keyspace.DeleteRange` (R2 spec-amend 1 — mirrored from SetKeyspace). New `SetDeleteRangeCalledHookForTest` instrumentation hook in `internal/btree/range_delete.go` (mirrors `readTxCleanupHookForTest` pattern from `5800299`) pins the dispatch-direction invariant via 2 dispatch-direction tests. 5 new regression tests: `TestSetKeyspaceDeleteRangeUnindexedNoLeakWithNestedTreeAtBoundary`, `TestSetKeyspaceDeleteRangeUnindexedNoLeakInteriorSubtreeRetire` (workload scaled per R1 M-1 + probe asserts cellCount >= 2), `TestSetKeyspaceDeleteRangeIndexedDispatchPreservesPerRowMaintenance`, `TestSetKeyspaceDeleteRangeUnindexedDispatchesToWalker`, `TestSetKeyspaceDeleteRangeIndexedDoesNotDispatchToWalker` — all neuter-verified individually. R1=0H/3M/2L/2nit (introduced M-1 interior-path workload too small + M-2 docstring over-claims + M-3 spec atomic-on-error wording missing in-memory pre-call state sentence; all fixed in-place; L-1/L-2 disputed; nit-1/2 fixed). R2=0H/1M/2L/0nit (introduced M-1 un-indexed dispatch not test-pinned → user picked fix-in-place via hook + 2 tests; introduced L-1 NestedCount sanity check absent → fixed in-place mirroring SetKeyspace.Delete; introduced L-2 `deleteRangePerKey` before/after subtraction → fixed in-place via wrap-immune per-iteration accumulator; spec-amend 1 → Keyspace.DeleteRange godoc parity; spec-amend 2 → entailed dispatch invariant added). R3=0H/2M/4L/1nit (introduced M-1 violation= reachability mis-framed → fixed in-place; M-2 dispatch tests missed count assertion → fixed by adding `n != 2`; introduced L-3 comment phrasing inaccurate + L-4 from= clause encoding nit → fixed in-place; L-1/L-2 adjacent; loop converged). |
| `ac4af82` | bulkload-index-merge-run-fanin | **Closed** — Option C (cascaded multi-pass merger) per user pick AFTER the user's "is matching b83846c/91a268a a rationalization?" pushback corrected my lazy preference-drop framing. Three-dimension re-derivation found bulkload differs from the prior preference-drops on (i) foreground-tx vs background, (ii) OS-hard-limit (EMFILE) vs CPU-only, (iii) user-bound-exceeded (256 MiB merger read buffers silently doubled the user's 256 MiB MaxTxBufferBytes) vs intrinsic. Disposition: clause-violation fix on the broad-reading of `bulkload.md §Interaction with Indexes` "in-memory sort footprint bounded by MaxTxBufferBytes" (the impl's narrow reading scoped this to phase-1 accumulator; merger read buffers exceeded the bound silently). Mechanism: `var maxMergeFanIn = 128` cap + new `(*indexSorter).cascadeRuns()` + `mergeGroupToScratchRun(scratchDir, runs)` helper; wired into `buildIndexFromSorter` spilled branch before `s.newMerger`. Cascade reduces `len(s.runs)` to ≤ cap via groups of ≤ cap runs each, repeating until cap fits. Pre-level runs removed only AFTER next level fully writes (mid-pass-error safety: data never destroyed before re-encoded). Bounds FDs + bufio read-buffer memory at O(cap) regardless of #runs, costs O(log_fanin(#runs)) extra scratch I/O. Preserves existing unique-violation-at-merge-output contract (detection runs on the post-cascade stream). Spec promotions: `bulkload.md §Invariants` new clause-explicit invariant for the cap with gitfs SQLite → gmdb migration as `violation=` (disambiguates bufio read-buffer memory vs heap key+value memory per R1 L-2); `§Interaction with Indexes` new "Merge fan-in cap" subsection describing cascade mechanism + intermediate lifecycle + per-pass FD ceiling (cap+1) + end-to-end memory contract. New `setMaxMergeFanInForTest(n)` swap-restore helper + `bulkLoadMergeCascadeHookForTest` (mirrors `readTxCleanupHookForTest` from `5800299` / `SetDeleteRangeCalledHookForTest` from `400c95d`). New regression test `TestKeyspaceBulkLoadIndexedMergeCascadeBoundsFanIn` (cap=2 + 900 rows + 8 KiB MaxTxBufferBytes forces multi-pass cascade per `preRuns >= 2*testCap+1` probe; pins postRuns ≤ cap + end-to-end correctness + scratch cleanup; neuter-verified by commenting out `s.cascadeRuns()` → postRuns=6 > cap=2 → deterministic fail). R1=0H/0M/3L/3nit (introduced L-1 docstring math fragile → fixed in-place via workload-drift-resilient phrasing; adjacent L-2 new invariant inherits merger heap-memory imprecision → fixed in-place via spec disambiguation "bufio read-buffer memory" + separate heap-memory clause; introduced L-3 single-element cascade-group rewrite disputed as design-uniform; introduced nit-1 cascadeRuns panic-window disputed unreachable; nit-2 panic guard sound; introduced nit-3 per-index hook scope by-design). R2=0H/0M/1L/0nit (introduced L-1 cap=4 typo in docstring after R1 cap=2 rewrite → fixed in-place). Converged. |

The authoritative live list is `docs/issues/README.md`. Below is a
snapshot of decisions and findings *known but not yet executed*; use
the README as ground truth, this as a hint.

### Decided, in-flight or queued

*(none — this session's `setkeyspace-indexing-perf-and-edge`
closed via consolidation fix + spec promote; no correctness-class
or condition-triggered-now entries remain in the backlog.)*

### Undecided / needs analysis

*(none — every remaining `docs/issues/` entry is profiling-driven
or condition-triggered.)*

### Profiling-driven / condition-triggered (re-validate before pulling)

`rpl-segment-relocation` (condition-triggered, design-heavy;
adjacent to recent RPL/savepoint area positionally — the only
remaining live entry that's pickable),
`pager-test-helper-export` (condition-triggered, still blocked
on second-caller arrival — not pickable until that count > 1).
Re-validate live before acting; some may now be obsolete.

---

## This session's task

Pick **one** issue from `docs/issues/README.md`. Confirm the pick
with the user at session start (offer your recommendation +
rationale; the user may override). Default order, applying the
Ordering criteria (every remaining entry is profiling-driven or
condition-triggered; the one condition-triggered entry —
`pager-test-helper-export` — is blocked on its trigger):

1. **Re-validate the profiling-driven set first** — the recurring
   lesson says issue framings often go stale, *especially* for
   profiling-driven items. The last nine sessions proved this
   nine times, with a 3:3:2:1 split on disposition (fix via
   substrate / spec-only / spec-internal-inconsistency-close /
   cleanup-fix); the user's "is this a rationalization?" pushback
   on `ac4af82` showed pattern-match similarity does NOT entail
   disposition similarity — run the three-dimension check per
   candidate (foreground/background, OS-hard-limit/CPU,
   user-bound-exceeded/intrinsic) before assigning a bucket:
   `shallow-savepoint-clone-cost` `43ac8df` → clause-explicit
   cost-clause violation, fix via undo-log substrate;
   `nested-shallow-loose-pop-buffer-alias` `d9ea7d4` → binary
   "user-choice A vs B/C" → production-caller audit collapsed
   to Option 1 by construction;
   `rplsegments-clone-cost` `b83846c` → strict cost clause
   SATISFIED, only an auxiliary "small constant" *preference*
   could be falsified → spec amend only, no code change;
   `rplchain-head-tail-terminology-conflict` `9d060ba` → filed
   as "pure docs/naming, no correctness" → first-principles
   surfaced a project-invariant promotion opportunity, user
   added the regression test inline;
   `leaked-readtx-cleanup-race-flake` `5800299` → filed as "GC
   timing variant," first-principles surfaced the deeper root
   cause is the test's wait shape not matching `BeginRead`'s
   wait semantics (returns ErrReadersFull IMMEDIATELY with
   no-deadline ctx, does NOT block) — fixed via deterministic
   test-only hook;
   `compaction-full-forest-walk-per-pass` `91a268a` → filed as
   "O(live pages) per pass, wasteful"; first-principles re-
   derivation found the §Cost per pass clause quantifies WRITE
   I/O only ("the slab must hold the whole cascade" — slab =
   CoW write buffer); read cost is workload-history-dependent,
   no clause violated → spec amend only, two-dimensions
   Cost-per-pass + §Trigger inclusive-count rationale;
   `setkeyspace-indexing-perf-and-edge` `de9e7c1` →
   filed as "perf-only items A+B (double / per-
   value snapshot)"; first-principles found NO clause violated
   AND NO preference to drop — third-bucket cleanup-fix: the
   two-layer atomicity scheme (chunk-7.6 H-2 wrapper + chunk-7.9
   caller `rowSnap`) had its wrapper-internal snap subsumed by
   the caller layer at chunk-7.9; consolidating to caller-only
   is structurally small and correct-preserving. Folded the
   chunk-7.6 Keyspace symmetric sites under structurally-larger-
   but-simpler exception.
   `setkeyspace-delete-range-bulk-walker` `400c95d` →
   filed as "v1 snapshot-then-Delete is O(K log N), the chunk-5.7
   walker is O(K + log N) — perf-driven follow-up"; first-principles
   re-derivation surfaced a spec internal inconsistency: `set-
   keyspace.md` line 17 said "Range delete on SetKeyspaces uses …
   the range-delete walk of range-delete.md" (walker-normative) but
   chunk-6.8 user-lock + chunk-7.10 amendment + api-surface.md godoc
   together preserved the v1 per-row-atomic contract while promising
   a "future walker that honors the same (deleted_so_far, err)
   contract" — structurally impossible (a walker is naturally all-
   or-nothing or per-leaf, not per-row). User picked clean-break
   atomic walker for un-indexed (Option B) over hybrid mixed-atomicity
   (Option D, ~2× larger) after honest-scope reveal mid-decision.
   Disposition fits a new bucket: **spec internal inconsistency
   close + clause-explicit re-alignment + clean-break atomicity to
   stronger contract**. Filed-and-closed via the chunk-5.7 walker
   substrate; chunk-7.10 indexed dispatch preserved.
   `bulkload-index-merge-run-fanin` `ac4af82` (this session) →
   filed as "single-pass merge fan-in; cascaded multi-pass merge is
   the fix; profiling-driven, unreachable at default
   MaxTxBufferBytes." My first cut framed disposition as
   "preference-drop + spec-amend, mirrors b83846c / 91a268a." The
   user pushed back with "is matching b83846c/91a268a a
   rationalization?" Three-dimension re-examination found bulkload
   FAILS all three checks the prior preference-drops pass:
   (i) foreground tx vs background; (ii) OS-hard-limit (EMFILE) vs
   CPU-only; (iii) user-bound-exceeded (256 MiB merger read buffers
   silently double the user's 256 MiB MaxTxBufferBytes per the broad
   reading of `bulkload.md §Interaction with Indexes` line 192) vs
   intrinsic. Clause violation under broad reading: spec is
   authoritative; impl's narrow self-interpretation is the bug. User
   picked Option C (cascaded multi-pass merger, ~200 LOC) over
   Option A (preference-drop, rejected after dimension analysis) and
   Option B (memory-only cleanup-fix, rejected because FD dimension
   uncovered). New bucket: **clause-violation fix on the broad-
   reading of a spec phrase the impl was interpreting narrowly,
   substrate adds new bounded primitive**.
   The disposition is NOT predictable from issue framing alone:
   re-derive each candidate against the spec, asking
   (i) does the unenumerated cost term scale with `MaxSize`
       (clause-explicit violation → fix via substrate) or with
       workload history the spec already permits (preference →
       drop from spec)?
   (ii) does the diff touch a domain concept whose invariant
       is recorded-only? if yes, audit promotion opportunity;
   (iii) does the test under inspection wait on a signal whose
       SEMANTICS match the function-under-test's actual
       behavior?
   (iv) when the cost clause says "worst-case I/O is X," check
       whether X is a WRITE-side or COMPREHENSIVE expression
       — "the slab must hold the cascade" / "the buffer must
       hold" / "the cascade" are diagnostic signals that only
       writes are quantified; read cost may be separately
       unbounded. Distinguish the DIMENSION, not just the
       magnitude;
   (v) an issue's "Related" / "Adjacent" sub-concern is its OWN
       clause-explicit / entailed invariant check — do not
       assume it shares the main concern's disposition. Treat
       each as an independent first-principles re-derivation;
       promote its spec conclusion into the same close-out
       commit but DERIVE it separately;
   (vi) if neither (i) clause violation nor (ii) preference-drop
       applies, check whether a structurally-simple consolidation
       eliminates the redundancy. This is the third bucket:
       cleanup-fix (smallest correct change applies to code shape
       even when no bigger-O bound is violated). The signal is
       "the redundant work was historically load-bearing but a
       later layer subsumed its contract" — the wrapper-snap +
       rowSnap consolidation is the canonical instance. Before
       inferring "preference, drop from spec," check whether
       the wasted work can simply be deleted under a structural-
       simpler exception.
   (vii) **new this session**: surface-framing similarity to prior
       preference-drops (profile-driven, unenumerated cost term,
       workload-dependent) does NOT entail disposition similarity.
       Per CLAUDE.md "Analyze, don't rationalize" — when applying
       the rubric, run the THREE-dimension check per candidate
       (foreground/background, OS-hard-limit/CPU,
       user-bound-exceeded/intrinsic) before pattern-matching to a
       prior bucket. b83846c / 91a268a are correctly preference-
       drops because they pass all three (CPU-only / background /
       no user-configured bound on affected dimension). The
       `bulkload-index-merge-run-fanin` `ac4af82` case FAILS all
       three (foreground tx + EMFILE OS-hard-limit + merger read
       buffers exceeding the user's MaxTxBufferBytes) and was
       correctly disposed as a clause-violation fix once the
       dimension audit ran. Surface-framing similarity is a TRAP;
       per-candidate dimension audit is the gate.
   (viii) **new this session**: a spec phrase's broad-reading is
       authoritative even when the implementation interprets
       narrowly. When the impl-comment narrows a spec phrase's
       scope ("X = phase-1 only" while spec says "X" unqualified),
       the impl's narrow reading is the BUG, not the spec's
       "appears comprehensive" wording. The diagnostic test: read
       the spec phrase the way a USER reads it (without the impl
       comment in front of you); if a user would expect the
       broader scope and the impl silently exceeds it, that's a
       clause violation even when the impl-comment "fits" the
       impl's behavior. `bulkload.md` line 192 "in-memory sort
       footprint bounded by MaxTxBufferBytes" was the canonical
       instance: impl narrow-read it as accumulator-only; user
       broad-reads it as sort-wide (including merger read
       buffers); the broad reading wins because the spec is
       authoritative.

2. **`rpl-segment-relocation`** is the only remaining pickable
   live entry. Condition-triggered ("when RPL pages are shown
   to block consolidation, or when RPL relocation folds into
   the commit pipeline"). Adjacent to recent RPL/savepoint
   area positionally (b83846c / 9d060ba context still warm).
   Design-heavy — the immovability assumption used by the
   chunk-12.5b-3 compaction orchestration may need to change.
   Re-validate whether either trigger has been demonstrated
   since the issue was filed.

3. **`pager-test-helper-export`** remains blocked on its
   trigger ("when a second cross-package writer-pager fixture
   caller arrives"); the count is still 1. Not pickable until
   that count > 1.

**Recommended next candidate:**
`rpl-segment-relocation`. Only pickable live entry.
Pre-pick advisory: read `free-space.md` and `commit.go` /
`free_space.go` to understand the current RPL pipeline (alloc /
chain / reclaim) + read the issue file for the immovability
assumption's history. Then apply the eight-prior-session
re-derivation rubric checks (i)-(viii). Specifically: is RPL
relocation a clause violation (and which clause), preference,
spec internal inconsistency, or clause-violation-via-broad-
reading? Run the THREE-dimension audit per (vii) BEFORE
pattern-matching to a prior bucket.

If the trigger condition genuinely hasn't fired — RPL pages
neither block consolidation nor fold into the commit pipeline
— the principled disposition is to redefer with a fresher
condition restatement (`Lands:` rewrite) OR file as obsolete
if the issue's framing no longer holds. NOT to pull it just
because it's the only pickable entry; per Ordering criterion
5 (correctness > perf), pulling without a triggered condition
is profile-driven-without-justification.

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
