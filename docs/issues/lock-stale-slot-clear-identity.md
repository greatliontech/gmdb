# Stale-slot clear leaves the dead occupant's PID/heartbeat; the next acquirer can be falsely evicted mid-publish

**Lands:** audit-burndown-2026-07 chunk 6.

**Severity:** [H] — live reader evicted → reclamation bound advances
past its snapshot → use-after-reclaim (torn snapshot); secondary
cascading eviction via the evicted reader's later ReleaseReaderSlot.

**Source:** 2026-07-04 full-codebase audit (concurrency auditor).

**Governing spec:** `docs/specs/cross-process.md` §Clearing a stale
slot — which *prescribes* the defective "leave PID/HB as-is" behavior;
spec and code must change together (spec-amend, user-approved
2026-07-04 with blanket fix authority).

## Problem

`ClearStaleReaderSlot` (`internal/lock/reader.go:145-155`) stores only
`HintEpoch=0, TxnID=0`, leaving PID, Heartbeat, PST, PIDN. The acquire
path CASes TxnID first, then stores Heartbeat→…→PID
(`reader.go:78-87`). Between the CAS and the Heartbeat store the slot
is observably `TxnID=fresh, PID=deadPID, Heartbeat=stale` — a
concurrent scan (`OldestReaderTxnID` from every write-Begin, or
`ReapStaleReaderSlots`) classifies via `pid != 0`: same-NS →
IsAlive(deadPID)=false → clear (`reader.go:264-266`); cross-NS → stale
heartbeat → clear (`reader.go:294`). The acquirer's snapshot leaves
the table while it still believes it owns the slot; its later
`ReleaseReaderSlot(i)` (`reader.go:113-125`) then zeroes a slot a
third reader may have since won.

## Fix direction

Stale-clear must zero PID and Heartbeat (and PST/PIDN) *before*
`TxnID=0`, mirroring the release-path ordering the spec already
mandates for owners. Amend cross-process.md §Clearing a stale slot
accordingly. Regression (lock package): stamp slot as dead same-NS
owner, ClearStaleReaderSlot, goroutine A CASes TxnID (pre-publish
acquirer), goroutine B runs the scan — assert the slot survives.
