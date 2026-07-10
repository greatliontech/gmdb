# Daemon goroutines pin *DB: handle-leak detection structurally unreachable

Lands: 15

## Findings

**[M] The background maintenance goroutine holds `*DB` as a method
receiver, so a dropped handle is never GC-unreachable and the
leak-detection cleanup can never fire under default options.**
`db.go:501-506` (`go db.maintenanceLoop(...)`); same pinning via
`go db.batchCoordinator(...)` (`batch.go:99`) once Batch is used. For
any default-options writable handle, dropping the reference leaks the
flock + heartbeat + maintenance goroutines, both mmaps, and the fds for
process lifetime — silently, with maintenance continuing to take write
grants against an abandoned handle. Inverts leak-detection.md
§Database Handle Leak Detection: the safety net is inert exactly in the
configurations it targets.

**[L] `LaggingReader` callback reentrancy self-deadlocks.**
`db.go:787-808` + `options.go:222`: the callback runs on the writer's
goroutine holding the write grant; a callback calling any write entry
point queues behind its own grant (permanent with a no-deadline ctx).
Neither lock-ordering.md §Lagging Reader Handling nor the Options godoc
warns against reentrancy while suggesting "corrective action".
Doc-level guard at minimum.

## Fix direction

Pass required state into the daemon loops instead of the `*DB` receiver
(as `dbCleanupInfo` already does for the cleanup) or hold `*DB` weakly
from daemons, so an abandoned handle becomes unreachable and the
cleanup fires; add a test that the DB cleanup actually fires. Document
the LaggingReader reentrancy constraint (spec + godoc).

## Provenance

2026-07-10 defect audit; transaction-layer reviewer. No test exercises
the DB cleanup firing.
