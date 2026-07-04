# dbCleanupFn tears down coord/lockFile without Close's inflight drain

**Lands:** condition — when the runtime.AddCleanup execution model is
next revisited, or if the Go runtime documents concurrent cleanup
execution.

**Severity:** [L] — latent: safe today only because runtime cleanups
execute sequentially on one goroutine (an implementation detail the
API does not guarantee), so a leaked-DB cleanup cannot interleave
with a leaked-ReadTx cleanup's slot release.

**Source:** 2026-07-05 adversarial review of the
beginread-close-lifecycle change set (chunk 11), adjacent finding.

**Governing spec:** `docs/specs/leak-detection.md` (Close ordering:
release-store, drain, teardown).

## Problem

`DB.Close` stores closed, drains `gate.txInflight`, then tears down
coord/lockFile. The leaked-DB cleanup path (`dbCleanupFn`, db.go)
performs the teardown WITHOUT the drain step. If the runtime ever
runs cleanups concurrently, a leaked-ReadTx cleanup that passed the
gate could race the leaked-DB cleanup's unmap — the exact SIGSEGV
class the drain exists to prevent.

## Fix direction

Mirror Close's drain in dbCleanupFn before the teardown (bounded spin
— cleanups hold microsecond windows), or pin the sequential-cleanup
assumption with a build-breaking reference to the runtime docs if it
becomes guaranteed.
