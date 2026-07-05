# Options.PageChecksum spec-default is true; the code default is off

**Lands:** condition — when the Options surface next takes a breaking
change (pre-v1 clean break makes inverting the flag or a *bool
straightforward), or with the chunk-22 limits/options sweep if pulled
there.

**Severity:** [M] — spec'd protection silently absent: checksums.md
("opt-out, on by default") and the Options.PageChecksum godoc
("Default true") both promise footer-checksummed pages for zero-value
Options, but applyDefaults never touches the plain bool, so the
effective default is OFF — bitrot on commodity filesystems goes
undetected by scrub/Check exactly where the spec says it will not.

**Source:** 2026-07-05 adversarial review of the
check-consistency-classes change set (chunk 15), reconciling why
checksums-off fixtures worked without asking for it.

**Governing spec:** `docs/specs/checksums.md` §Data Page Checksums
(On by Default); `Options.PageChecksum` godoc.

## Fix direction

Either implement default-on (invert to `DisablePageChecksum bool` so
the zero value means on — the pre-v1 clean break — or a *bool), or
amend the spec/godoc to default-off. Spec wins by default; the
protection argument in checksums.md ("worth the 0.2% overhead")
favors implementing default-on. Regression: zero-value Options →
assert new pages carry verifying footers.
