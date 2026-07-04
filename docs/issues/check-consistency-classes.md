# Check() misses ordering/consistency corruption classes api-surface.md claims it verifies

**Lands:** audit-burndown-2026-07 chunk 15.

**Severity:** [M] — with checksums off, misrouting corruption (out-of-
order leaf, wrong separator) reports clean; count drifts surface later
as ErrCorrupted mid-operation instead of at Check.

**Source:** 2026-07-04 full-codebase audit (bulkload/maintenance
auditor).

**Governing spec:** `docs/specs/api-surface.md:1449-1452` (claims);
`docs/specs/range-delete.md:57-70` (separator invariant);
`docs/specs/set-keyspace.md:104-116` (NestedCount);
`docs/specs/keyspaces.md:130-151` (NumKeyspaces).

## Problem

`walkTree` (`check.go:560-605`) verifies only checksums + per-page
structural bounds (`LeafReader.Validate` is bounds-only,
`internal/page/leaf.go:383`; ValidateBranch likewise). Not verified
anywhere: intra-leaf / cross-leaf key ordering; branch separator
routing (max(left) < S ≤ min(right)); desc.Count vs walked entries;
nested-tree cell NestedCount vs actual members; NumKeyspaces vs
descriptor-leaf count.

Additional class (redeferred here from the chunk-4 review, finding
M3): `walkRPL` is a third raw-accessor RPL reader that performs no
footer verification — a checksum-bad-but-decodable segment passes
Check clean while reclamation quarantines it, and its flipped entries
taint the pending accounting set (walkTree footer-verifies tree
pages; the asymmetry is silent).

## Fix direction

Extend the Check walk with the five classes above plus RPL-segment
footer verification in walkRPL (ordering + separator
bounds threaded through the walk; counts tallied per keyspace/cell).
Keep O(live pages), no extra I/O passes. Regression: forge each
corruption class on a checksums-off DB; assert Check reports it.
