# Cross-namespace readers reclaimable purely on a 10s heartbeat window with no fallback

**Lands:** condition — when the cross-process stale-detection model is
revisited (relates to `options-coord-intervals` and the heartbeat
stale-detection guard — `cross-process.md §Reader Table` stale
detection / `lock.heartbeatStale`).

**Severity:** [L] (partly inherent to heartbeat-based liveness; needs a
documented bound + a user decision on defaults)

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw finding 21.

**Governing spec:** `docs/specs/cross-process.md:888-892`;
`StaleTimeout` at `internal/lock/recovery.go:11`
(`DefaultStaleTimeout=10s`).

## Problem

A cross-namespace (container) reader has no `kill()`+ProcessStartTime
fallback — same-namespace readers are protected by that path regardless of
heartbeat staleness, but cross-namespace readers are classified live
**purely** on the 10s heartbeat window (`reader.go:291-295`, case 2). A
cross-namespace reader whose heartbeat goroutine is starved for >10s —
plausible under `docker pause`, cgroup freeze, heavy swap/thrash, or a
long stop-the-world pause in a constrained container — is classified stale
and cleared by a peer's scan. Its snapshot `TxnID` leaves the table, the
writer's reclamation bound (`db.go:585`) advances, RPL frees the pages it
pinned, and when the container resumes its read tx reads
reclaimed-then-reused pages: **silent torn reads / wrong results**,
reachable without any crash. This is the "evict a live reader → RPL frees
pages it still reads" failure mode, specific to cross-namespace readers.

## Fix

Partly inherent to heartbeat-based cross-namespace liveness. The spec
should **(a)** state the *hard-correctness* consequence of a
>`StaleTimeout` pause for cross-namespace readers (not merely "jitter"),
and **(b)** consider a larger default or an explicit guidance/clamp so
operators sizing `StaleTimeout` understand it is a **data-integrity
bound, not a performance knob**. At minimum, surface it to the user as a
documented limitation rather than leaving it implicit. (Ties to
`options-coord-intervals`, which makes `StaleTimeout` tunable.)
