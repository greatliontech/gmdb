# Leak reclamation behind a truncated RPL walk double-frees live pages

Lands: 20

## Finding

**[H] Background leak reclamation frees pages that sit behind an RPL
walk-truncation boundary but are still tracked by the live writer's
in-memory chain — they get freed again later.** `maintenance.go:253-347`
(gate at 273-281 checks only `stopped || sawError`), `check.go:715-731`
(`walkRPL`: footer boundary → CheckWarning; decode boundary → silent),
`internal/pager/rplwalk.go:224-241` (boundary is a stop reason, not an
error). The detection walk truncates at the first non-head segment
failing footer/decode; entries of every older *intact* segment then
classify as leaked (`!reach && !free && !pending`, check.go:808) and
`maintReclaimLeaks` frees them — but those segments are still in the
same-process writer's `p.rplSegments` (rebuilt at Open; `Resync` skips
the rebuild when TxnID matches, init.go:502) and will be reclaimed
again. The code's safety argument ("a genuinely leaked page is
permanently stuck") is false for this state; `FreeLeakedPage`'s own doc
requires exclusive access, which maintenance lacks.

Failure: one flipped byte in a middle RPL segment (bitrot; silent with
checksums off via a type-byte flip) → maintenance frees page P listed
in an older intact segment → a user tx re-allocates P into the live
tree → reclamation later reaches the intact segment and marks P free
under its new owner → double-allocation, silent corruption. Variant:
a reader pinned below the segment's TxnID still references P when
maintenance frees it.

## Fix direction

Gate reclamation on the walk having reached the true tail
(`RPLWalkTailReached`): any truncation boundary means the "leaked" set
may intersect the real RPL — route those pages through exclusive
`CheckWithOptions(Repair)` only. Spec-amend rider:
background-maintenance.md §Bitmap Leak Reclamation's safety argument
must add the tail-reached (or exclusive-access) precondition, aligning
with free-space.md's "recoverable by a Check()/Repair structural walk";
also surface decode boundaries on live-projection walks observably
(today asymmetric with the quarantine path) — both surfaced in the
audit spec-amend list.

## Provenance

2026-07-10 defect audit; free-space reviewer. Existing maintenance
tests corrupt a tree page (CheckError-gated) or cover TxnID/pager
currency; none corrupt an RPL segment and run maintenance.
