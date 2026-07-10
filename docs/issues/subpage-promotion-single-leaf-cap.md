# Subpage→nested-tree promotion overflows a single leaf: small-member sets hard-cap

Lands: 5

## Finding

**[H] Promotion packs all subpage entries into ONE nested-root leaf and
errors on page-full instead of building a multi-leaf tree.**
`set_keyspace.go:755` → `internal/btree/subpage_promotion.go:108-126`.
The 50% subpage threshold bounds subpage bytes (2+len per variable
entry, `fvs` per fixed entry), but a leaf cell costs ≥7+len plus
restart tables, so small members cannot fit one leaf. Reproduced:
`CreateSetKeyspace` with `FixedValueSize: 8`, Put of 8-byte
low-shared-prefix values (hash IDs — the canonical postings workload)
under one key fails at **Put #254** with "nested-root leaf overflowed";
variable 2-byte values fail at Put #508. The per-op savepoint restores
state, but every subsequent new-member Put re-attempts promotion and
fails forever — the set is hard-capped far below spec limits
(limits.md bounds member SIZE only).

## Fix direction

Promotion builds a proper multi-leaf nested tree (bottom-up build over
the sorted subpage entries). Spec-amend rider: set-keyspace.md §Subpage
Promotion Threshold's 4-step algorithm ("copy all subpage entries into
the new leaf page") is unimplementable within the stated threshold for
small members — the algorithm clause moves to the multi-leaf build
(surfaced in the audit spec-amend list; the alternative — redefining
the threshold in projected-leaf-bytes — leaves the same defect class
reachable and was not taken). Regression tests: small fixed-size and
small variable-size member promotion well past one leaf; existing
promotion test keeps its heavy-prefix fixture as the compressed case.

## Provenance

2026-07-10 defect audit; keyspace reviewer, reproducer-confirmed.
Existing `TestSetKeyspacePutTriggersPromotion` uses 30-byte values with
28 shared bytes (heavy compression) and cannot see this.
