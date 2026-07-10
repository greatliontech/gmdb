# Uncompressed-leaf read path: seek panic + iterator desync

Lands: 1

Two reachable wrong-result/crash defects plus two contract-doc faults in
the `RestartGroupTarget = 1` (uncompressed leaf) read path. Both H
defects were confirmed with runnable reproducers during the 2026-07-10
audit.

## Findings

**[H] Panic on exact-match search of a leaf's last entry with checksums
disabled.** `internal/page/leaf_uncompressed.go:77` (via `ucOffset`,
line 33): the found-case constructs the iterator with a speculative
`off: r.ucOffset(mid+1)` *before* the `mid+1 >= r.count` guard.
`ucOffset(count)` reads 2 bytes at `ContentEnd`; with
`DisablePageChecksum` there is no footer slack, so `ContentEnd ==
len(buf)` and the read panics. Failure: DB with `DisablePageChecksum`
+ a `RestartGroupTarget=1` keyspace; `Seek`/`SeekGE` matching the last
key of any leaf → process panic on a `Validate`-clean page. Path:
`btree/cursor.go:623 → LeafReader.SearchLeafIter → ucSearchLeafIter`.

**[H] `LeafIter` uncompressed `Prev`/`At` update `idx` but not `off`;
`Next` decodes by stream continuation from `off`.**
`internal/page/leaf_iter.go:203-204` (`At`), `:258-259` (`Prev`).
Failure (reproduced): on leaf `a b c d`, `Next,Next,Next,Prev,Next`
yields `…c, b, d` (skips `c`), then a fabricated empty-key entry
decoded from the zeroed free region; `IterAtForReuse(count)` (the
`Last()` setup) leaves `off == 0`, so `Prev…,Next` decodes the page
header as an entry. Ordinary cursor alternation returns wrong
keys/values; checksums do not help. page-formats.md §Cursor Iteration
says "Next/Prev/At are all O(1) via the offset table" — making
uncompressed `Next` table-driven fixes this structurally and matches
the spec as written.

**[L] `Prev` doc describes semantics neither variant implements.**
`internal/page/leaf_iter.go:234-236` claims "after Prev() → N-1,
Next() → N-1"; the compressed (correct) behavior is Next → N.

**[L] `SearchLeaf` doc overstates robustness.**
`internal/page/leaf.go:226-228` claims "total over input" while the
per-variant search paths use the unchecked hot decoders (and the H
panic above fires even on validated pages). Invites callers to skip
the `Validate` gate; also carries a TODO contrary to repo policy.

## Fix direction

Move the `ucOffset` guard before the read; make uncompressed
`Next`/`Prev`/`At` consistently table-driven (per spec); correct both
doc contracts. Regression tests: exact-match-last-entry seek with
checksums off; uncompressed Prev/Next alternation incl. the
`Last()`-then-`Prev`-then-`Next` shape.

## Provenance

2026-07-10 defect audit (nine subsystem reviewers); page-encodings
reviewer, both H findings reproducer-confirmed. Existing suites
(`internal/page`, `internal/btree`) green on HEAD — no coverage of
uncompressed search or Prev/Next alternation.
