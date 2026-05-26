# Check's structural walk does not validate SetKeyspace subpage payloads

**Lands:** chunk 11 subpage-structural-validation hardening (or later) —
when `btree.Walk`'s leaf handling gains a subpage `Validate()` step, or a
fuzz/Check pass surfaces a corrupt-subpage repro.

## Problem

`api-surface.md §Check` (CheckIssue / Check godoc) states `Check`
"verifies … set keyspace subpage / nested B+tree integrity." The
structural reachability walk `btree.walkAt` (`internal/btree/walk.go`),
however, only special-cases **overflow** and **nested-tree** leaf cells;
**subpage** cells (`CellFlagMultiValue`) are visited as part of the leaf
but their internal payload (`[Count u16][DataSize u16][members…]`) is
never validated. `LeafReader.Validate` bounds only each entry's
`keyLen + valLen` within the page — it does not validate a subpage's
internal Count/DataSize. So a forged subpage (bad Count, truncated value)
passes the entire structural `Check` with **no CheckIssue**, contradicting
the spec's subpage-integrity claim.

This is a spec-vs-code divergence (spec wins): structural `Check` should
detect and report a corrupt subpage (e.g. a `SubpageCorrupt` CheckError),
the same way it reports `BadPageChecksum` / `TreeCorrupted`.

## Scope / not this

- **Not the CheckIndexes panic (chunk-11.4a H-1).** That was a *separate*
  defect — the SetKeyspace `CheckIndexes` pass decoded a raw subpage
  without `SubpageReader.Validate()` and could panic. It was fixed
  in-place at 11.4a (`check.go expectedSetKeyspaceIndex` now guards
  `len(e.Value) >= SubpageHeaderSize` + `sp.Validate()` → a
  `CheckIndexes.RowsUnreadable` warning, pinned by
  `TestCheckIndexesSetKeyspaceForgedSubpageNoPanic`). This issue is the
  *remaining* gap: the **structural** walk still under-reports subpage
  corruption (a corrupt subpage on a keyspace with no supplied
  `CheckIndexes` decl, or with `CheckIndexes` off entirely, is silently
  missed).

## Fix sketch

Add a subpage `Validate()` step to `btree.walkAt`'s leaf-entry handling
(it already has the leaf reader): for a `CellFlagMultiValue` cell, bound
`len(value) >= page.SubpageHeaderSize` and call
`page.NewSubpageReader(value, fvs).Validate()`, surfacing a wrapped
`ErrCorrupted` that `Check.walkTree` reports as a CheckError. `walkAt`
needs the keyspace's `FixedValueSize` to construct the reader — thread it
through, or validate the Count/DataSize header generically (fvs-independent
bounds: `Count*minStride <= DataSize <= len(value)-SubpageHeaderSize`).

## Notes

`class=adjacent` per the chunk-11.4a diff arbiter — the cause-line
(`walkAt` skipping subpages) predates 11.4a (it is at the chunk-11.2 Walk
introduction). Surfaced by the chunk-11.4a round-3 adversarial review
(reported as an adjacent M alongside the introduced H-1). File-and-proceed:
the 11.4a CheckIndexes path is panic-safe; this is the broader
structural-coverage gap.
