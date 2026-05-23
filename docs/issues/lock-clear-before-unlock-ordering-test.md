# Lock-file write lock: missing direct test for clear-before-unlock ordering

**Lands:** chunk 2.6 (stale-writer-recovery brings the peer-thread
test infrastructure that lets a same-process witness observe the
header state immediately after `flock(LOCK_UN)`).

## Problem

Chunk 2.4's spec amendment (cross-process.md §Invariants) promoted
"clear writer-header BEFORE `flock(LOCK_UN)`" to a clause-explicit
invariant. The Coord's `process()` step-4 cleanup
(`internal/lock/coord.go:325-330`) implements the ordering:

```
SetWriterPID(0)
SetWriterStartTime(0)
SetWriterPIDNamespace(0)
syscall.Flock(LOCK_UN)
```

Tests assert post-conditions — `TestCoordGrantAndRelease` and
`TestCoordCloseWhileHolding` check that `WriterPID == 0` *after*
release / Close completes. That is necessary but not sufficient: a
broken `Unlock-then-Clear` ordering would still leave the final
state correct; only a peer that observed the header during the
window between LOCK_UN and the (delayed) clear would notice.

The violation mode the invariant guards against requires a peer
process / thread that:

1. Acquires `LOCK_EX` immediately after the goroutine's `LOCK_UN`,
2. Reads `WriterPID` before the (hypothetically out-of-order) clear
   has landed,
3. Mis-classifies the stale `WriterPID` as a crashed-writer
   recovery candidate.

Chunk 2.4 does not yet have a stale-writer-recovery path, so
(3) is not exercisable. Once chunk 2.6 lands the recovery code,
a same-process peer-thread test can directly observe the post-
LOCK_UN header state under a tight `LOCK_EX` handoff and assert
the invariant.

## Acceptance

A test exercising the clear-before-unlock invariant directly:

1. Coord A acquires the write lock; releases.
2. Coord B (different `*File`, same path) acquires immediately.
3. Coord B's stale-writer-recovery path on grant inspects the
   header; it must observe `WriterPID == 0` even when there is
   no inter-acquire delay — the kernel-side `LOCK_EX` handoff
   happens directly on Coord A's `LOCK_UN`.

The mechanic needs either:
- A stale-writer-recovery hook (chunk 2.6) that snapshots the
  header at exactly the LOCK_EX-grant instant, or
- A test-only `coord.SetReleaseHookForTest(func())` that injects
  a pause between the field clear and the `LOCK_UN` syscall —
  allowing the test to assert the invariant from the same
  process by reading the header in the injected window. (This
  is the inverse pattern of the chunk-2.4
  `SetCreateInitHookForTest`.)

Either approach lands naturally in chunk 2.6.

## Notes

The Round-2 reviewer (chunk 2.4) flagged this as a coverage gap.
The current Coord implementation is correct by inspection; the
gap is in *enforcement* of the new clause-explicit invariant.
Filing rather than blocking 2.4 because no production path in
chunk 2 currently relies on the ordering being correct (only
chunk 2.6's recovery would, and it lands with the test).
