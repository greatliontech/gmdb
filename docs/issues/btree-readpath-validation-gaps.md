# Btree read-path validation gaps

Lands: 4

## Findings

**[M] Unvalidated first-read branch/leaf decode in the cousin
descendant scan.** `internal/btree/delete.go:984-1003` decodes
candidate child pages read fresh from disk with `page.DecodeBranch` /
`leafUnderflow` without `validateBranchPage` / `LeafReader.Validate`
(also `DecodeBranch(scanBuf)` at `:972`). Violates the package's own
validate-at-first-resolver contract (`btree.go:50-58`). Failure: a
checksums-off (in-spec) tree with one corrupt grandchild page reachable
during a deep-cascade Delete → out-of-bounds slice panic instead of the
contracted ErrCorrupted.

**[L] `Cursor.Delete` ignores the internal re-position result.**
`internal/btree/cursor.go:402`: if the self-`SeekGE` fails on
structural corruption, Delete returns nil with the error parked in
`Err()` — corruption first observed during the reposition is reported
one call late, and only to callers that poll `Err()`. Spec-amend rider:
the Cursor.Delete post-delete clause cited by cursor.go leaves the
reposition-failure outcome unspecified; pin it (surfaced in the audit
spec-amend list).

## Fix direction

Validate pages at first resolve in the cousin scan (same gate as every
other first-read site); surface the reposition error from Delete per
the pinned contract. Regression tests: corrupt-grandchild deep-cascade
delete with checksums off returns ErrCorrupted (no panic).

## Provenance

2026-07-10 defect audit; btree reviewer.
