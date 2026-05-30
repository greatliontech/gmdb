# Heartbeat goroutine can store WriterHeartbeat after releasing LOCK_EX, violating a clause-explicit invariant

**Lands:** proactive — clause-explicit invariant contradicted by the code
(benign on a shared clock; reachable on a per-process clock).

**Severity:** [L]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw finding 20.

**Governing spec:** `docs/specs/cross-process.md:179-191` (clause-explicit
"writes `WriterHeartbeat` only while holding `LOCK_EX`" invariant).

## Problem

The heartbeat goroutine (`internal/lock/coord.go:506-508`) can store
`WriterHeartbeat` **after** this process has released `LOCK_EX`, racing
the release path (`coord.go:439`, `:450-466`) — violating the
clause-explicit invariant that `WriterHeartbeat` is written only under
`LOCK_EX`. Inert when `WriterPID==0` (stale-detection never consults
`WriterHeartbeat` unless `WriterPID!=0` — `recovery.go:40`). But in the
narrow window where a peer acquires `LOCK_EX` and publishes its own
identity (`WriterPID` + `WriterHeartbeat`) between our `LOCK_UN` and our
delayed store, our store stomps the peer's `WriterHeartbeat` with **our**
clock value. On Linux (shared `CLOCK_BOOTTIME`) the value is ~microseconds
off and cross-namespace stale-detection is not fooled. But on a
per-process clock — which the codebase itself models for darwin/freebsd
(`lock.go:630-640`) — our clock origin differs from the peer's, so a third
process comparing the stomped heartbeat against its own clock could
mis-time staleness. Same per-process-clock root cause as the reader-guard
finding.

## Fix

Either keep `holdingWriter` true until strictly after the clear+unlock and
gate the heartbeat store behind a mechanism that cannot straddle the
unlock (perform the final `WriterHeartbeat` refresh inside the flock
goroutine's release critical section, or guard the store under the same
flock ownership), **or** downgrade the invariant text to document the
benign post-unlock window explicitly with the per-process-clock caveat.
As-is, the code contradicts a clause-explicit spec invariant.
