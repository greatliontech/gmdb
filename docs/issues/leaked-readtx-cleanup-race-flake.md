# `TestLeakedReadTxReleasesSlotViaCleanup` is flaky under `-race`

**Lands:** when reader-slot cleanup test is rewritten to drive
finalizers deterministically (no chunk-number dependency — fold into
whichever chunk next touches read-tx slot lifecycle, or address
opportunistically when the slab/pool harness gets a deterministic-
finalizer hook).

## Symptom

`go test -race -count=N` against the root `gmdb` package fails
intermittently on `TestLeakedReadTxReleasesSlotViaCleanup`
(read_tx_test.go:340). Under non-race execution the test passes
consistently; under `-race` it fails roughly 1 in 2–3 iterations
(empirically: 4 failures in 10 iterations on commit cd34a40).

## Mechanism

The test calls `leakReadTx` (a helper that allocates a read tx, drops
the *Tx reference, then triggers GC) and expects
`runtime.AddCleanup` to fire synchronously enough that the reader
slot is observably released within the test's bounded wait. Under
`-race`, the runtime's finalizer-scheduling latency increases enough
that the bounded wait sometimes expires before cleanup fires.

The chunk-5.4 adversarial review (Round 2) re-verified the flake is
pre-existing — it reproduces on HEAD (cd34a40, chunk 5.3 close-out)
with chunk-5.4 changes stashed. Not introduced by chunk 5.4.

## Acceptance

Either:

1. Rewrite the test to drive cleanups deterministically — e.g., expose
   a test-only hook on the closeGate / cleanup pipeline that the test
   can poll/wait on (similar to the existing
   `commitStep4HookForTest`).
2. Make the wait bound proportional to GOMAXPROCS / race-runtime-cost
   and add explicit `runtime.GC()` + `runtime.GC()` cycles so the
   finalizer queue is fully drained before the assertion.
3. Skip the test under `-race` with `if testing.Short()` or a build
   tag, retaining the non-race coverage. (Least preferred —
   `-race` is the more rigorous mode and skipping defeats the
   purpose.)

## Notes

Surfaced by the chunk-5.4 Round 2 adversarial review (flagged as
pre-existing adjacent). Filed-and-proceed per the chunk-close
contract (Issue triage rule for adjacent H/M findings). Severity is
L (test-only flake; no production impact); included here because
the test failure is observable in CI / local runs and the diagnostic
"race-only finalizer-timing flake" is easy to lose track of.
