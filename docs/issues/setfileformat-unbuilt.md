# Tx.SetFileFormat + FileFormat type unbuilt (CopyTo godoc points at it)

**Lands:** proactive — spec'd public method referenced by live godoc.

**Severity:** [M]

**Source:** 2026-05-30 completeness pass (this audit session).

**Governing spec:** `docs/specs/api-surface.md:792`.

## Problem

`Tx.SetFileFormat` and the `FileFormat` type are spec'd but unimplemented.
The runtime file-bound mutation (set the user-defined file-format
identifier / version stamp on the open database) does not exist, yet the
`CopyTo` godoc (`copy.go:35`) already points to it. Doc-vs-code
contradiction for a spec'd capability.

## Fix

Implement the `FileFormat` type and `Tx.SetFileFormat` per
`api-surface.md:792` (persisted in the meta/header, mutated within a write
tx, committed atomically), **or** file a concrete deferral and remove the
dangling reference from the `copy.go:35` godoc until built. Add a
regression test covering set → commit → reopen → read-back.
