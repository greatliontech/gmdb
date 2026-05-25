# Defense-in-depth: validate branch page structure before iterating children

**Lands:** opportunistic — when a corrupted-branch reproducer surfaces
in fuzz / integration testing, or when chunk 11 (`Check()` /
`CheckWithOptions`) builds the integrity-walk and needs the same
per-page validator. Surfaced as L1 in the chunk-5.6 adversarial
review.

## Problem

`internal/btree/subtree.go` `freeSubtreeAt`, `internal/btree/btree.go`
`Get`, `internal/btree/btree.go` `Has`, and the
`internal/btree/cursor.go` descent paths all read a branch page's
header (`page.ReadHeader` → `count`) and immediately iterate
`page.BranchChildAt(buf, cfg, i)` for `i ∈ [0, count]`. Only LEAF
pages get a `page.NewLeafReader(buf, cfg).Validate()` call before
iteration — `chunk-4.6β` introduced `LeafReader.Validate` precisely
to catch this class of fault at the boundary.

A corrupted branch page (e.g. count field forged past the cell-array
capacity, or a cell directory entry whose offset+klen points outside
the cells region) would produce junk child page IDs from
`BranchChildAt`. The downstream null-child check `if c == 0 { return
ErrCorrupted }` catches the all-zeros junk case but not a non-zero
random integer that happens to be in `[FirstDataPage, HighWaterMark)`
range — that path recurses into an arbitrary page and surfaces a
"page N has unexpected type T" ErrCorrupted at depth+1, but only
after touching whatever's on that page (incl. the keyspace B+tree's
own pages — could trigger a re-traverse of a branch in the
keyspace-B+tree as if it were data).

This is defense-in-depth, not a demonstrated fault — no in-spec
input produces a corrupt branch. Filing per Issue triage so the
gap is tracked alongside the chunk-11 integrity-walk landing.

## Mechanism (cited reachable path)

Pre-conditions: a corrupted disk surface with a branch page whose
`count` field is forged, OR a future encoder bug that produces an
invalid branch directory.

Read path: `BranchChildAt(buf, cfg, i)` for `i ∈ [0, count]` returns
the value at the i-th directory slot. With a forged `count`, the
loop reads past the cell directory into the free-space region OR
into the cells region's tail — both produce uint64 garbage. If the
garbage equals zero, the null-child check fires; otherwise it does
not.

## Fix sketch

Add `page.ValidateBranch(buf []byte, cfg Config) error` mirroring
`LeafReader.Validate`:

- Header type == TypeBranch (already implied by the switch).
- `count * branchDirEntrySize + branchHeaderEnd <= ContentEnd()`.
- Each cell directory entry's offset + klen + childPtrSize stays in
  `[branchHeaderEnd + count*branchDirEntrySize, ContentEnd())`.
- Cell offsets monotonic (optional — depends on whether the encoder
  guarantees a packed layout).

Call from `Get`, `Has`, `Delete`'s descent, `Cursor` descent, and
`FreeSubtree`'s branch arm.

## Notes

`class=adjacent` per the chunk-5.6 diff arbiter — every existing
btree.descent caller shares the same pre-existing pattern (chunk-1
through chunk-4). The chunk-5.6 `FreeSubtree` did not introduce the
gap; it inherited it from `Get`/`Has`/`Delete`/cursor descent. Per
Workflow's adjacent-issues rule, file-and-proceed; the fix lands
when the integrity-walk machinery does (chunk 11) or
opportunistically.
