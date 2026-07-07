# Kept-current artifacts carry retired-plan chunk-number references

**Lands:** condition — as its own sweep change set, or opportunistically
per artifact whenever one of the listed specs/files is next amended
(fix that artifact's residue in the same change set).

**Severity:** [M] per the artifact-homes rule (planning codenames in
kept-current artifacts), aggravated by ambiguity: both plans that
defined the numbering are deleted, and the active plan
(`docs/plans/architecture-consolidation.md`) reuses chunk numbers 1–18,
so a bare "chunk N" in a spec now reads as a reference to the wrong
plan.

**Source:** 2026-07-07 adversarial review of the v0-plan close-out
change set, finding M-3 (adjacent — all references pre-date the
close-out).

## Problem

Roughly 80 references to v0/audit-burndown chunk numbers survive in
kept-current artifacts. Specs:
`docs/specs/range-delete.md:305,329,364,375,408`,
`api-surface.md:276,756,925,1038–1144,1216,1253`,
`indexing.md:283,619,705`, `keyspaces.md:153`,
`transactions.md:680,700`, `page-formats.md:176,623`,
`typed-keyspaces.md:257`, `set-keyspace.md:133`,
`leak-detection.md:72`. Code/test comments: `index.go:700`,
`typed.go:22`, `options.go:6`, `db_test.go:102+`,
`keyspace_dataops_test.go:13+`, `set_keyspace_test.go:14+`, plus 20+
in internal-package test comments (e.g.
`internal/page/leaf_subpage_test.go`,
`internal/page/keyspace_descriptor_test.go`,
`internal/lock/lock_test.go`, `internal/pager/lagging_reader_test.go`).
(`grep -rniE "chunk[ -]?[0-9]" docs/ --include="*.go" .` re-derives
the full list.)

`indexing.md:619` ("fix that landed at chunk 5.6") is additionally
implementation-status narrative, which specs must not carry.

## Fix direction

Rewrite each reference descriptively (name the behavior or the
governing section; where provenance genuinely matters, a
`git log --all -- <path>` recovery line), or drop it. Specs gain no
status narrative in the rewrite. Test-comment references are renamed
for the behavior they pin, per the planning-codenames rule.
