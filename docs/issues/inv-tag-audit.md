# Audit the `Inv-XXX` coded-invariant tags in production comments

**Lands:** condition — proactive burn-down. Split out of
`dev-process-artifacts-in-comments` after that issue's chunk-N +
`spec amend` / `user-locked` scrub landed (the chunk-ref portion is
done; this is the remaining, distinct sub-task).

## Problem

Production comments carry **~39 distinct `Inv-XXX` coded-invariant
tags** (≈100+ uses) — `Inv-A`, `Inv-B`, `Inv-C{,1,2,5}`, `Inv-D`,
`Inv-E`, `Inv-F`, `Inv-I`, `Inv-M{1,2,3,5,6}`, `Inv-N{1,2,4,5}`,
`Inv-RV{1,2,3,4}`, `Inv-T{1,2,3,4,6,7}`, `Inv-WD`, `Inv-{1,2,3,6}`, …

The project's no-cite invariant (Issue triage) says a comment may cite
only a **kept-current artifact** (a spec section, an enforced
invariant, a test) — never a dangling label. The trouble: the specs
record invariants as **unlabeled** `Invariant: kind=…; property=…`
blocks, so **most `Inv-XXX` tags name a label the specs do not
define** — they are dangling cross-references.

The exceptions — tags that **are** real spec labels and should be
kept (optionally with the spec doc named) — are:

- `Inv-IHS1` / `Inv-IHS2` / `Inv-IHS3` — `indexing.md` §Handle
  Invalidation (also in `api-surface.md`).
- `Inv-N3` — `transactions.md` (parent-freeze rule).
- `Inv-M4` — `api-surface.md` (compaction budget-halving retry).

## Resolution

For each `Inv-XXX` occurrence in a non-test `.go` file:

- **Real spec label** (the three families above): keep the tag; where
  it stands alone, qualify it with the spec doc (e.g.
  `indexing.md Inv-IHS1`) so the cross-reference is navigable.
- **Dangling label** (everything else): inline the property in plain
  prose, or repoint to the spec **doc / §section** that states it
  (the spec doc is a kept-current artifact even though it does not use
  the `Inv-XXX` label). Drop the bare tag.

Comment-only; no behaviour change. Best done file-by-file to keep
diffs reviewable, mirroring the chunk-N scrub.

## Notes

Filed during the chunk-N scrub (the `dev-process-artifacts` change
set) once it became clear the `Inv-XXX` portion is a distinct,
judgment-heavy audit — most tags dangling, a few load-bearing — rather
than a mechanical sweep. The chunk-N scrub deliberately left every
`Inv-XXX` tag untouched (dropping only an adjacent `chunk-N` prefix,
e.g. `(chunk-5.6 Inv-D)` → `(Inv-D)`), so this audit starts from a
clean, tag-only surface.
