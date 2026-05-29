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

- **`rplsegments-clone-cost`** (this session, `b83846c`): three
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
disposition is filing-and-proceed, not widening the change set.

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

This session closed `rplsegments-clone-cost` — the adjacent issue
filed at the prior-prior session's `shallow-savepoint-clone-cost`
close-out. User picked Option A (spec amend + close, no code
change) over Option B (implement undo-log substrate) and Option C
(defer per profile-driven Lands trigger), justified by
first-principles re-derivation: the strict cost clause "Cost is
proportional to pages modified since the outermost open savepoint,
plus O(bitmap-pages currently dirty), not total database size" is
**satisfied** by the rplSegments clone (chain length is independent
of `MaxSize`, growing with retired-pages-pending-reclamation count
not page count). Only the auxiliary "small constant in practice"
claim in §Why this is cheap can be falsified by a lagging-reader
workload, but slow ≠ wrong/unsafe — per CLAUDE.md Project
invariants, "an invariant with no statable violation= is a
preference — do not record it." Different fault class from the
prior two profile-driven closes (`bitmap-rollback-undo-log`
`0893be5`, `shallow-savepoint-clone-cost` `43ac8df`) which both
re-derived as clause-explicit MaxSize-scaling violations. Fix:
drop the preference from `transactions.md §Why this is cheap`;
replace with honest workload-dependent prose (lagging reader pins
`reclamationBound` → chain accumulates across writer commits;
structural ceiling at `MaxSize`/`PageSize` is the only
workload-independent bound). §Nested Transactions §Nesting depth
extended to name the chain-clone as an explicit cost term with
cross-ref to §Why this is cheap. Code godoc alignment in
`internal/pager/savepoint.go` (Savepoint struct cost-contract
citation + captureSavepointState godoc) — disambiguates the
"across-writer-commits scaling" (orthogonal to within-tx 43ac8df
work). Promote-then-delete: rationale folded inline; issue file
deleted; README row removed. NO code semantic change; NO new tests
(spec no longer asserts a constant the test would need to
enforce). R1=1H/1M/2L/1nit (introduced H-1 `finalizeRPLChain`
phantom symbol cite → fixed in-place to `appendRPL`; adjacent M-1
`trimRPLChainHead` vs `pager.go:370` chain-orientation convention
conflict → filed as `rplchain-head-tail-terminology-conflict.md`;
introduced L-1 cross-tx-vs-within-tx ambiguity → fixed in-place
with explicit 43ac8df cross-ref; adjacent L-2 existing alloc-count
test → disputed; introduced L-3 per-tx `MaxTxBufferBytes` mention
→ disputed); R2=0H/0M/0L/1nit (disposition-narrative mis-citation
of `MaxTxBufferBytes` location, no landed artifact affected; ship).

Prior session closed `nested-shallow-loose-pop-buffer-alias` — see
commit `d9ea7d4` for that change set's details.

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

The authoritative live list is `docs/issues/README.md`. Below is a
snapshot of decisions and findings *known but not yet executed*; use
the README as ground truth, this as a hint.

### Decided, in-flight or queued

*(none — this session's `rplsegments-clone-cost` closed via
spec-amend-only; no correctness-class entries remain in the
backlog. The adjacent issue filed this session —
`rplchain-head-tail-terminology-conflict` — is pure docs/naming
cleanup with no correctness implication, condition-triggered when
the RPL chain area is next touched.)*

### Undecided / needs analysis

*(none — every remaining `docs/issues/` entry is profiling-driven
or condition-triggered.)*

### Profiling-driven / condition-triggered (re-validate before pulling)

`rpl-segment-relocation`, `compaction-full-forest-walk-per-pass`,
`pager-test-helper-export`, `leaked-readtx-cleanup-race-flake`,
`setkeyspace-delete-range-bulk-walker`, `bulkload-index-merge-run-
fanin`, `setkeyspace-indexing-perf-and-edge`,
`rplchain-head-tail-terminology-conflict` (this session's filed
adjacent — pure docs/naming cleanup: `trimRPLChainHead` is a
misnomer per `pager.go:369-370`'s chain-orientation convention;
`Lands:` triggers when the RPL chain area is next touched, or
opportunistically). Re-validate live before acting; some may now
be obsolete.

---

## This session's task

Pick **one** issue from `docs/issues/README.md`. Confirm the pick
with the user at session start (offer your recommendation +
rationale; the user may override). Default order, applying the
Ordering criteria (every remaining entry is profiling-driven /
condition-triggered; correctness-class slot is empty):

1. **Re-validate the profiling-driven set first** — the recurring
   lesson says issue framings often go stale, *especially* for
   profiling-driven items. The last three sessions proved the
   lesson three times (with a 2:1 split on disposition):
   `shallow-savepoint-clone-cost` (filed profile-driven →
   first-principles found a clause-explicit cost-clause violation,
   fixed via undo-log substrate);
   `nested-shallow-loose-pop-buffer-alias` (filed as a binary
   "user-choice A vs B/C" → production-caller audit collapsed it
   to Option 1 by construction);
   `rplsegments-clone-cost` (filed profile-driven → first-principles
   found the strict cost clause is SATISFIED and only an auxiliary
   "small constant" *preference* could be falsified → spec amend
   only, no code change). The disposition is NOT predictable from
   the "profile-driven" framing alone: re-derive each candidate
   against the spec, asking "does the unenumerated cost term scale
   with `MaxSize` (clause violation, fix) or with workload history
   the spec already permits (preference, drop from spec)?"

2. **Among re-validated live items, prefer ones with active
   triggers and concrete acceptance** (Ordering criteria 2 + 3).
   - `pager-test-helper-export` is **blocked on its trigger**
     ("when a second cross-package writer-pager fixture caller
     arrives"); the count is still 1, so it is not pickable now.
   - `rplchain-head-tail-terminology-conflict` (this session's
     filed adjacent) has an **opportunistic** Lands trigger —
     pickable any time. Pure docs/naming cleanup, narrow
     well-defined fix shape (rename `trimRPLChainHead` →
     `trimRPLChainTail` + 4 comment-site edits + spec amend).
     Inherits fresh adjacent context from this session.
   - `leaked-readtx-cleanup-race-flake` has a real engineering
     value (CI noise elimination); pre-existing flake on HEAD,
     reproduces 1-in-2-3 under `go test -race`. Concrete
     acceptance options sketched in the issue (deterministic
     hook on closeGate / cleanup pipeline; race-cost-proportional
     wait bound; skip-under-race as last resort). Independent of
     savepoint area — fresh context, not adjacent.
   - Others (`rpl-segment-relocation`,
     `compaction-full-forest-walk-per-pass`,
     `setkeyspace-delete-range-bulk-walker`,
     `bulkload-index-merge-run-fanin`,
     `setkeyspace-indexing-perf-and-edge`) — bounded by needing a
     measured workload that hasn't been produced. Profile work +
     spec re-derivation per the recurring lesson; potentially
     heavier sessions.

3. **Adjacent to recently-closed > unrelated** (Ordering criterion
   4). `rplchain-head-tail-terminology-conflict` is the direct
   adjacent to this session's `rplsegments-clone-cost` close-out
   (same RPL/savepoint area; the conflict was surfaced by the
   adversarial reviewer); the next session inherits the
   `pager.go:369-370` chain-convention mental model.

**Recommended next candidate:** `rplchain-head-tail-terminology-conflict`.
Combines criteria 2 + 3 + 4 favorably: adjacent-to-just-closed (fresh
context); active opportunistic trigger; concrete acceptance options
(rename + 4 comment edits + spec amend; ~30 LOC docs commit);
pre-existing pure cleanup (no correctness implication, low
adversarial-review surface). **Pre-pick advisory:** verify the
rename's blast radius by `grep -rn "trimRPLChainHead\|head trim"
internal/ docs/` before committing to the shape — if the function
name is referenced in tests or other comments beyond the 4 sites
this issue enumerates, the fix may be larger than "mechanical
rename" and warrants user re-confirmation per the Honest scope
estimation rule.

**Alternative candidate** (if user prefers meatier work):
`leaked-readtx-cleanup-race-flake`. Real CI value, fresh context
(separate area from this session's savepoint work), concrete
acceptance options. Trade-off: the deterministic-finalizer-hook
shape (Option 1) is the right fix but requires designing a test-
only hook on the closeGate / cleanup pipeline — a small new test
surface that needs adversarial review for ownership / lifecycle
correctness. Heavier session than the rename.

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
