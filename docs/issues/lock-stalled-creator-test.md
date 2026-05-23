# Lock-file lifecycle: missing test for a stalled but live creator

**Lands:** chunk 2.4 (flock goroutine — brings the test-hook
infrastructure that lets us inject latency into `createAndInit`
without modifying the production code path).

## Problem

Chunk-2.2's H1 fix (the round-2 / round-3 work) introduces a
retry path in `lock.Open` that handles the `O_CREATE|O_EXCL` →
`flock(LOCK_EX)` window: an adopter that lands inside the window
observes `size < HeaderSize` or `Magic == 0`, surfaces
`errPartialInit`, and `Open` retries with backoff until the
creator publishes.

The success branch of this retry — adopter retries, sees the
publication, adopts — is not directly tested. The existing
`TestConcurrentOpenRaceWithCrossMmapVisibility` races 10
goroutines whose init is sub-millisecond, so adopters almost
always block on the creator's `LOCK_EX` (the "happy path" inside
the window) rather than land in the `errPartialInit` retry. The
two corruption tests
(`TestOpenRejectsCorruptMagic`,
`TestOpenRejectsUndersizedFile`) exercise only the
budget-exhaustion path that surfaces `ErrCorrupted`.

Round-3 reviewer L2: "no test for a stalled-but-live creator".

## Acceptance

A test that injects latency between `O_CREATE|O_EXCL` and
`flock(LOCK_EX)` in `createAndInit` (e.g. 100 ms sleep) and
spawns a concurrent adopter. The adopter must converge to a
successful `Open` via the `errPartialInit` retry path — NOT
return `ErrCorrupted`, NOT block forever.

The injection point needs a test-only hook similar to
`pager.SetCommitStep4HookForTest`. Chunk 2.4 introduces the
flock-goroutine test infrastructure, which is the natural place
to also add `lock.SetCreateLatencyForTest(time.Duration)` or
similar.

## Notes

Pre-fix verification:
`go test -race ./internal/lock/... -count=200 -run TestConcurrentOpenRaceWithCrossMmapVisibility`
is green; the H1 mechanism does not surface under contention
with real (sub-ms) init. The L2 test would add coverage for the
slow-creator branch that current tests don't reach.
