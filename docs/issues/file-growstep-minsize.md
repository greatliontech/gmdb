# File growth/shrink ignores the persisted GrowStep and the MinSize floor

**Lands:** proactive — one clause-explicit invariant violation (shrink
below MinSize) plus a persisted-but-inert file-format parameter.

**Severity:** [M]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw finding 12.

**Governing spec:** `docs/specs/file-format.md` (GrowStep + MinSize
invariants and their violation clauses).

## Problem

`GrowStep` is a persisted, advertised file-format parameter (default 64;
spec default 65536) with **no implemented effect**, and `MinSize` is not
honored on shrink:

- **(a) GrowStep ignored.** Every file-extending allocation does a
  one-page `ftruncate` (`internal/pager/freespace.go:292-302`
  `ensureFileCovers`; `:255-259` AllocPage extension) instead of one
  `ftruncate` per `GrowStep` pages. `file-format.md`'s violation clause
  states this "defeats the OS readahead behaviour GrowStep is sized for."
- **(b) MinSize floor violated on shrink.** A DB created with `MinSize=N`
  (`Init` truncates up to N pages, `init.go:60-63`) that later has all
  data deleted has `maybeShrink` (`commit.go:384-397`) truncate the file
  to ~`firstDataPage*PageSize`, far below `MinSize*PageSize` — violating
  the clause-explicit "Clamp to MinSize" / "Pages at offsets <
  HighWaterMark are never truncated" floor, silently discarding the
  user's pre-allocated minimum file size.

## Fix

Implement `GrowStep`-aligned growth in `ensureFileCovers`/`AllocPage`
(`newSize = alignUp(neededPages, GrowStep)` clamped to `MaxSize`, marking
the gap pages `[allocated+1, newFileEnd)` free in the bitmap — as
`AllocPage`'s own doc at `pager.go:21-22` already claims it does) and
`GrowStep`-aligned shrink clamped to `MinSize` in `maybeShrink`.
**Or**, if growth-by-need is the intended design, file deferrals for each
unimplemented `file-format.md` invariant and amend the spec — do not
leave the clause-explicit invariants silently violated.
