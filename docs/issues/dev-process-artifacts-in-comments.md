# Development-process artifacts pervade production code comments

**Lands:** condition — proactive burn-down; can be swept in one pass or
folded in opportunistically as files are touched. Comment-only, not
breaking.

## Problem

Production (non-test) code comments cite the internal development
process — chunk numbers, spec-amend events, user-lock decisions, and
coded-invariant tags — which are meaningless to a reader who does not
have the plan/handoff docs in hand:

- **293** `chunk-N` references across **49** non-test `.go` files.
- **16** `spec amend` / `user-locked` / `lock-in` references.
- **25** distinct `Inv-XXX` coded-invariant tags.

Examples (all in `db.go` alone): "chunk-5.6 design", "Chunk-3 spec
amend", "chunk-3.3 refcount-drain promotion", "chunk-5.5 LaggingReader
wiring".

The *rationale* embedded in these comments is frequently valuable — it
is the *citation form* that is wrong. A comment that points at
"chunk-5.6" points at a tracking artifact that recedes into git
history, exactly what this project's own Issue-triage **No-cite
invariant** forbids for spec/code cross-references ("cite only a
kept-current artifact or a `git log` mechanism — never a tracking
artifact"). The chunk roadmap is now complete, so these citations are
already dangling.

## Resolution

Sweep production comments to keep the rationale but repoint or drop the
citation:

- Replace "chunk-N" / "spec amend" provenance with a pointer to the
  governing **spec section** or the **enforcing test** (the
  kept-current artifacts), or simply state the rationale without the
  process citation.
- `Inv-XXX` tags: keep them only where the same tag names a recorded
  spec invariant *and* its enforcing test (so the tag is a real
  cross-reference); otherwise inline the property.

Scope is comment-only; no code changes. Can be done file-by-file to
keep diffs reviewable, or as one mechanical sweep.

## Notes

Surfaced during the 2026-05-30 architecture/factoring audit. Counts
verified by `git grep`. This is documentation hygiene, not a
correctness issue — but at 293 occurrences it is the most pervasive
single finding and directly contradicts the project's own no-cite
convention.
