# Reader stale-detection lacks the future-heartbeat guard → live readers evicted, RPL frees pages they read

**Lands:** proactive — demonstrated reachable correctness defect
(use-after-reclaim / torn snapshot).

**Severity:** [H]

**Source:** 2026-05-30 deep audit (run `wf_4ad12a2f-039`), raw findings 5
(High, race-interleave) and 17 (Low, clock-skew) — same underflow, same
one-line-class fix, two triggers.

**Governing spec:** `docs/specs/cross-process.md` (slot-acquire invariant:
heartbeat-first, PID-last ordering is the load-bearing mid-publish-safety
mechanism); `docs/specs/free-space.md §RPL Reclamation`.

## Problem

The reader-table stale-detection scan in `OldestReaderTxnID`
(`internal/lock/reader.go:213, 243, 271, 292`) computes `nowNanos - hb`
as **unsigned** and compares against the stale timeout **without** the
future-heartbeat guard that `internal/lock/recovery.go:85` already
applies. When a slot's heartbeat is *ahead* of the scanner's `nowNanos`,
the subtraction underflows to ~2^64, which is **not** `<= staleTimeout`,
so the slot falls through and gets cleared as stale — evicting a
genuinely-live reader. Two reachable triggers:

1. **Mid-publish race** (finding 5, High, reachable on Linux). A writer's
   `Begin` captures `now=T0` then scans slots. A reader in
   `AcquireReader` captures its own `now=T1`, CASes `TxnID`, and stores
   `Heartbeat=T1` (`reader.go:82`) **before** storing PID (`reader.go:86`).
   With no happens-before between the two clock reads, if the reader's
   read is issued fractionally later, `T1 > T0`. The writer then sees the
   slot as `TxnID!=0`, `PID==0`, `hb=T1>T0`: `T0-T1` underflows, the
   slot is not recognized as fresh, and `ClearStaleReaderSlot` evicts the
   live mid-publish reader (`reader.go:223`). Its snapshot `TxnID` leaves
   the table, the next `bound = min(OldestReaderTxnID, prevMeta.TxnID)`
   (`db.go:585`) advances past it, and RPL reclamation frees pages the
   reader navigates once it finishes publishing — **silent
   use-after-reclaim / torn snapshot.** The HB-first/PID-last acquire
   ordering exists *specifically* to make mid-publish safe; this underflow
   defeats it.

2. **Backward clock skew** (finding 17, Low). An NTP step-back, a manual
   clock set, or cross-host skew (networked-FS multi-host) between a live
   reader's heartbeat write and the scan produces the same `hb > nowNanos`
   underflow, clearing a live slot and advancing the reclamation bound
   past its pinned `TxnID`.

The same underflow also fires in the cross-namespace `case 2` branch, and
on darwin/freebsd the codebase's own model (`lock.go:630-640`) states
`CLOCK_MONOTONIC` origins are per-process so a peer's stamp "can exceed
our `nowNanos`" — making it routinely reachable there.

## Fix

Mirror `staleByHeartbeat`'s guard at **every** reader-scan comparison:
when `hb > nowNanos` (or `epoch > nowNanos`), treat the slot as
fresh/live — skip the clear and include its `TxnID` in the oldest-min —
rather than subtracting. Equivalently, clear only when
`hb <= nowNanos && nowNanos-hb > staleTimeout`. Apply uniformly to every
`nowNanos-hb` / `nowNanos-epoch` comparison in the scan. This also
resolves the internal inconsistency where 2 of the 3 stale-detection
sites already defend against `now < stamp`.

## Verification

Regression test in `reader_test.go`: a slot whose `Heartbeat` exceeds the
scan's `nowNanos` is **not** cleared and its `TxnID` participates in the
oldest-reader min. (Companion: the cross-process case (c) test in
`cross-process-coordination-untested`.)
