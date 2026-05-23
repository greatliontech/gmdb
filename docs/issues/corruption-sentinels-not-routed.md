# Corruption sentinels declared but not routed in `mapPagerErr`

**Lands:** chunk 11 (Check + integrity), where the corruption-
surfacing code paths arrive.

## Problem

`errors.go` declares `ErrCorrupted`, `ErrBadPageChecksum`, and
`ErrVersionMismatch` per `api-surface.md §Sentinel Errors`. Chunk 1
needs them in the surface so future chunks can route to them
incrementally.

But `tx.go mapPagerErr` only routes `pager.ErrReadOnly`,
`pager.ErrTxTooLarge`, and `pager.ErrDBFull`. Pager paths that return
structural-integrity errors as `fmt.Errorf`-wrapped strings
(`internal/pager/init.go` "malformed", "self-referential", "cycle",
"PageSize invalid"; `internal/pager/commit.go` self-reference) fall
into the `default` arm and surface as `gmdb: pager: <message>`
without the `ErrCorrupted` sentinel attached.

Callers doing `errors.Is(err, gmdb.ErrCorrupted)` cannot distinguish
corruption from any other pager-wrapped error.

## Acceptance

Add a pager-side `ErrCorrupted` sentinel; wrap every existing
fmt.Errorf integrity error with it (`fmt.Errorf("...: %w",
ErrCorrupted)` plus the descriptive message); extend `mapPagerErr`
to translate `pager.ErrCorrupted` → `gmdb.ErrCorrupted`. Same for
`ErrBadPageChecksum` (chunk 11 adds the chunk-1 page-footer
verification path on read) and `ErrVersionMismatch` (chunk 5+ when
the format version surfaces beyond Init).

Regression test: open a database with a corrupted RPL segment (write
garbage bytes to a page indexed by RPLHeadPage); verify the Open
error satisfies `errors.Is(err, gmdb.ErrCorrupted)`.

When this issue closes, the rationale moves inline into the
chunk-11 integrity path; this file is deleted per the no-cite
invariant in `~/.claude/CLAUDE.md §Issue triage`.

## Notes

Round-1 of the root-package review added the sentinel declarations
to `errors.go` as part of the M2 disposition, but the routing was
omitted. Round-2 surfaced this as a half-fix per Quality bar:
"a declared sentinel that is never returned is documentation, not
behavior."

Classified as **newly-exposed** by round 2 (the gap was created by
round-1's partial fix; routing was always part of the round-1
finding's scope but was deferred without explicit tracking).
