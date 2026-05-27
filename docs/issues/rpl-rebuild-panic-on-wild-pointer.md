# `rebuildRPLChain` panics (instead of `ErrCorrupted`) on a wild RPL pointer

**Lands:** when Open-time corrupt-meta robustness is next hardened
(e.g. alongside `TestCorruptionSentinelOnOpen` / a meta-fuzzing pass), or
opportunistically.

## Problem

`rebuildRPLChain` (`internal/pager/init.go`) walks the persisted RPL
chain at Open by calling `p.pageRaw(id)` on each segment id without
first bounding `id` against `meta.HighWaterMark`. `pageRaw` panics when
`id*PageSize+PageSize` exceeds the mmap reservation (sized to `MaxSize`).
So a corrupt meta whose `RPLHeadPage` — or a followed `OlderSegment`
reached when `RPLTailPage` is inconsistent with the chain — holds a
uint64 ≥ `MaxSize` pages makes **Open panic** rather than return a
graceful `ErrCorrupted`.

`checker.walkRPL` (`check.go`) already guards `if id >= hwm` →
`RPLSegmentOutOfRange` before `PageRaw`, so Check converts the same
wild pointer into a structured finding and never panics. The two
sibling walkers diverge: Check is graceful, Open is not.

## Why deferred

Pre-existing (the cause-line predates the chunk-12 RPL chain-walk
termination fix; the old `OlderSegment==0`-terminated walk called
`pageRaw(RPLHeadPage)` / `pageRaw(seg.OlderSegment)` with the same
exposure). The termination fix does **not** widen it — for well-formed
meta the new walk follows strictly fewer pointers (it stops *at*
`RPLTailPage` instead of following the tail link). Reachable only under
already-corrupt meta (out-of-spec input), and not covered by a
regression test today.

## Fix

Mirror `walkRPL`: in `rebuildRPLChain`, guard `if id >= m.HighWaterMark`
→ wrapped `ErrCorrupted` before `p.pageRaw(id)`. Add a regression test
that opens a database whose meta carries an out-of-range `RPLHeadPage`
and asserts `Open` returns `ErrCorrupted` (no panic), alongside the
existing corrupt-meta Open tests.

Surfaced by the chunk-12 RPL chain-walk termination fix's adversarial
review (L-1).
