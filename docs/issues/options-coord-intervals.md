# Options.StaleTimeout / HeartbeatInterval / LockRetryInterval spec'd but missing from the implementation

**Lands:** proactive — public Options surface gap; the cross-process spec
leans on these for container / multi-host timing.

**Severity:** [M]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw finding 14.

**Governing spec:** `docs/specs/api-surface.md:539-555`.

## Problem

Callers cannot tune the cross-PID-namespace stale-detection window, the
heartbeat cadence, or the lock-retry/Close-latency interval — a
documented part of the public `Options` surface the cross-process spec
relies on for container / multi-host-storage timing. `options.go` has no
such fields (grep finds none). `db.go:231-238` builds the `Coord` without
the intervals; `internal/lock/coord_reader.go:9-15` hard-codes
`DefaultStaleTimeout`. The `CoordOptions` plumbing for
`RetryInterval`/`HeartbeatInterval` exists but is **dead** (always
defaulted); `StaleTimeout` has no plumbing at all. The DB works on
defaults, so this is a capability gap, not a crash.

## Fix

Add the three fields to `options.go`, wire
`HeartbeatInterval`/`LockRetryInterval` into `CoordOptions`, and thread
`StaleTimeout` through `Coord → OldestReaderTxnID` /
`ReapStaleReaderSlots` / `RecoverStaleWriter` (replacing the hard-coded
`DefaultStaleTimeout`). **Or**, if deferring, file a concrete deferral and
strike the fields from `api-surface.md` so spec and code agree.

**Note:** `StaleTimeout` is a *data-integrity* bound for cross-namespace
readers, not a performance knob — see `cross-namespace-reader-heartbeat-liveness`.
