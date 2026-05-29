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
the cause-lines named in the issue, not the fault class).

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

This session closed `index-handle-stale-after-rebuild-drop` (the
only remaining correctness-class entry in the prior backlog; all
others were profiling-driven). Fix: mirrors chunk-5.6
`markCursorsStale` pattern for `*Index` — `openIndexHandles` slice
on Keyspace/SetKeyspace, `openCursors`+`dead` on `*Index`, three
helpers (`markIndexHandlesStale` mark-all, `markIndexHandleStaleByName`
for RebuildIndex, `markIndexHandleDead` for DropIndex). Consolidated
the mark-all helper into existing `markCursorsStale`/
`markSetCursorsStale` bodies — structurally-larger-but-simpler
shape that avoids 11+ surgical edits at every post-mutation site
and is justified by the tight semantic coupling ("a successful
mutation invalidates all in-flight observer state"). Cursor.Delete
gets an explicit open-coded call (it doesn't go through
markCursorsStale because of the self-recovery exemption).
Sentinel: NO new sentinel — `ErrCursorStale` for mid-iter cursor
invalidation, `ErrIndexNotFound` for post-Drop dead-handle. New
`mapIndexCursorErr` translates `btree.ErrCursorStale` →
`gmdb.ErrCursorStale` at the public boundary (closes the same
sentinel-identity-leak class as `cursor-err-unpositioned-state`
commit 24ec951). Spec promotions: `indexing.md §Handle
Invalidation` (new section, records Inv-IHS1 + Inv-IHS2);
`api-surface.md` Lookup/RebuildIndex/DropIndex godoc updates. R1
findings: 0H/1M/3L/1nit — M-1 fix added 2 missing regression
tests (Cursor.Delete + SetCursor.Delete), L-1/L-2/L-3 fixed
in-place (spec wording, cite retarget, shape-change recovery
note). R2 converged: 0H/0M/0L; reviewer verified the fix by
neutering — both new tests fail deterministically when the calls
are removed. Filed adjacent
`index-handle-stale-after-deletekeyspace.md` (pre-existing
gap: `transactions.md §Cursor invalidation by DeleteKeyspace`
spec says `*Index` handles return ErrKeyspaceClosed post-
DeleteKeyspace, but the iter methods don't check `idx.ks.dead`
— same fault class, different cause-line; per escalation rule
out of scope for this fix).

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

The authoritative live list is `docs/issues/README.md`. Below is a
snapshot of decisions and findings *known but not yet executed*; use
the README as ground truth, this as a hint.

### Decided, in-flight or queued

*(none — the prior session's top candidate
`index-handle-stale-after-rebuild-drop` landed this session.)*

### Undecided / needs analysis

- **`index-handle-stale-after-deletekeyspace`** — adjacent gap
  filed at this session's close-out. Pre-existing spec-impl gap:
  `transactions.md §Cursor invalidation by DeleteKeyspace` says
  `*Index` handles return ErrKeyspaceClosed post-DeleteKeyspace,
  but the iter methods (Lookup / LookupKeys / Range / Prefix /
  Get / Stats) don't check `idx.ks.dead`. Same fault class as the
  closed `index-handle-stale-after-rebuild-drop` (in-flight Index
  reads freed pages), different cause-line. Resolution: single-
  line guards mirroring the existing `idx.dead` checks. Smaller
  than the just-closed bundle (no new tracking infrastructure
  required — the openIndexHandles slice already exists; just add
  the `idx.ks.dead` / `idx.sks.dead` guards alongside `idx.dead`).
  Adjacent-to-recently-closed per Ordering criterion 4.

### Profiling-driven / condition-triggered (re-validate before pulling)

`rpl-segment-relocation`, `compaction-full-forest-walk-per-pass`,
`pager-test-helper-export`, `leaked-readtx-cleanup-race-flake`,
`setkeyspace-delete-range-bulk-walker`, `bulkload-index-merge-run-
fanin`, `setkeyspace-indexing-perf-and-edge`, `shallow-savepoint-
clone-cost`. Re-validate live before acting; some may now be
obsolete.

---

## This session's task

Pick **one** issue from `docs/issues/README.md`. Confirm the pick with
the user at session start (offer your recommendation + rationale; the
user may override). Default order, applying the Ordering criteria
(decided > undecided is moot — decided slot empty; rank on
fresh-context / adjacent-to-recently-closed / correctness > perf):

1. **`index-handle-stale-after-deletekeyspace`** — the only
   remaining correctness-class entry in the backlog (everything
   else is profiling-driven / condition-triggered). Adjacent-to-
   recently-closed: same fault class as the just-closed
   `index-handle-stale-after-rebuild-drop`, different cause-line.
   The infrastructure (openIndexHandles slice + Index.dead/
   openCursors + markIndexHandles* helpers) is already in place
   from this session — the resolution is single-line `idx.ks.dead`
   / `idx.sks.dead` guards at each `*Index` entry method
   (mirroring the existing `idx.dead` checks) returning
   `ErrKeyspaceClosed` (per the existing transactions.md spec).
   Plus 4–6 regression tests mirroring the existing dead-handle
   suite. Smaller and more mechanical than the prior issue. Per
   Ordering criteria 4 (adjacent-to-recently-closed > unrelated):
   the shared mental context discount is real.
2. Anything in the profiling-driven set, after re-validation. Re-
   derive live — some may now be obsolete (e.g. the
   `shallow-savepoint-clone-cost` was filed at the end of a
   bitmap-undo-log close-out and a subsequent fix may have already
   reduced the per-Begin clone surface). Correctness > perf per
   Ordering criterion 5 — these stay last.

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
