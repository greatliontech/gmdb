# Delete-side leaf re-encode can grow: valid deletes surface ErrCorrupted

Lands: 2

The delete/relocate paths assume a smaller entry set always re-encodes
no larger. False for compressed leaves: removing an entry phase-shifts
restart-group alignment (each delta re-based across the shift loses up
to `SharedLen−2` bytes of savings), and a `RestartGroupTarget` config
change re-encodes old-structure pages under the new config.

## Findings

**[H] `deleteRangeFromLeaf` / `rebuildLeafAfterDelete` return
ErrCorrupted when the post-removal build overflows.**
`internal/btree/range_delete.go:256-264`, `delete.go:237-244`. Failure:
prefix-clustered keys (long intra-cluster prefix, 1-byte inter-cluster
prefix), leaf ≥~96% full, `DeleteRange` removing the first entry → the
keep-set rebuild grows by ~(SharedLen−2)×groups → build fails → an
in-spec delete deterministically reports database corruption; retries
fail identically. Single-key `Delete` reaches the same rebuild via the
variant-mismatch decline (`leaf_splice.go:525-531`) after a mid-life
RestartGroupTarget change (in-spec per page-formats.md).

**[M] `relocateLeaf` ref-rewrite re-encode can overflow after a
mid-life RestartGroupTarget change.** `internal/btree/relocate.go:218-231`
rebuilds with the *current* cfg and returns ErrCorrupted on overflow →
incremental compaction of that region fails persistently; background
maintenance can never evacuate it.

## Fix direction

Sanction and implement a split fallback on the delete-side rebuild and
the relocate re-encode (or a removal-monotone encoding — split fallback
is the narrower change). Spec-amend rider: page-formats.md §Insert and
Delete / §Leaf Split has no clause for the smaller-entry-set-encodes-
larger case; the chosen contract must land there (surfaced in the audit
spec-amend list; direction per spec-amend channel).

## Provenance

2026-07-10 defect audit; btree reviewer. No prefix-phase-adversarial or
RGT-migration-under-delete fixture exists in the suite.
