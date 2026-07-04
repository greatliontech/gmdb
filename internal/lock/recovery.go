package lock

import "time"

// DefaultStaleTimeout matches the cross-process.md §Heartbeat
// Goroutine "StaleTimeout" default: a heartbeat must be at least
// 10 s older than now before the writer/slot is reclaimed. Must
// be significantly larger than the heartbeat interval (1 s default)
// for scheduling jitter. The value is exposed as a constant for
// callers (the flock goroutine in coord.go and tests).
const DefaultStaleTimeout = 10 * time.Second

// IsStaleWriter reports whether the lock file's writer-header refers
// to a process that is no longer alive. Returns false for the
// no-writer case (`WriterPID == 0`).
//
// Decision logic (cross-process.md §Stale Writer Recovery):
//
//  1. WriterPID == 0 ⇒ not stale (no writer recorded).
//  2. Same PID namespace as ourPIDNamespace (both non-zero, match):
//     a. kill(pid, 0) ESRCH ⇒ stale.
//     b. Alive but ProcessStartTime mismatch ⇒ PID recycled ⇒ stale.
//     c. Match ⇒ alive and legitimately holds the lock.
//  3. Different namespace (or either zero): now - WriterHeartbeat >
//     staleTimeout ⇒ stale; fresh ⇒ alive.
//
// Note: callers that have just acquired flock(LOCK_EX) on f's fd
// can rely on the simpler `WriterPID() != 0 ⇒ stale` test — the
// clear-before-unlock invariant guarantees a live writer would
// still hold flock, so we wouldn't have acquired LOCK_EX. The
// fully-correct check below is provided for diagnostic and
// read-only contexts where LOCK_EX is not held.
//
// staleTimeoutNanos and nowNanos use the same monotonic-clock units
// as cross-process.md (CLOCK_BOOTTIME on Linux, CLOCK_MONOTONIC
// elsewhere — see clock_linux.go / clock_other.go). The flock
// goroutine uses Coord.clock() for both.
func IsStaleWriter(f *File, ourPIDNamespace uint64, nowNanos uint64, staleTimeoutNanos uint64) bool {
	pid := f.WriterPID()
	if pid == 0 {
		return false
	}
	wNS := f.WriterPIDNamespace()
	sameNS := wNS != 0 && ourPIDNamespace != 0 && wNS == ourPIDNamespace
	if sameNS {
		if !IsAlive(int(pid)) {
			return true
		}
		// Alive: PID-reuse check via ProcessStartTime.
		actualStart, err := ProcessStartTime(int(pid))
		if err != nil {
			// Process exists but we can't read its start time
			// (race with exit, or platform unsupported). Fall back
			// to heartbeat-based liveness, which is the
			// conservative-safer route (false positives → no
			// recovery, just block on flock until live writer
			// releases).
			return staleByHeartbeat(f, nowNanos, staleTimeoutNanos)
		}
		recordedStart := f.WriterStartTime()
		// Match within the timestamp resolution: equality. The
		// resolution caveat (cross-process.md §Process Start Time)
		// permits same-tick collisions that benignly classify as
		// "match" — caught by the heartbeat path on actual reuse.
		if actualStart != recordedStart {
			return true
		}
		return false
	}
	return staleByHeartbeat(f, nowNanos, staleTimeoutNanos)
}

// staleByHeartbeat is the cross-namespace (or namespace-unknown)
// liveness check: a heartbeat older than staleTimeoutNanos ago is
// stale. A WriterHeartbeat of 0 (never published) is stale only if
// the timeout has elapsed since 0, which on any non-trivial clock
// reading is immediately true — captured by the same comparison.
func staleByHeartbeat(f *File, nowNanos uint64, staleTimeoutNanos uint64) bool {
	return heartbeatStale(nowNanos, f.WriterHeartbeat(), staleTimeoutNanos)
}

// heartbeatStale reports whether a monotonic-clock stamp — a reader/writer
// Heartbeat, or a reader slot's HintEpoch orphan anchor — is older than
// staleTimeoutNanos relative to nowNanos. A stamp in the FUTURE
// (stamp > nowNanos) is treated as fresh, never stale: the unsigned
// subtraction nowNanos-stamp would otherwise underflow to ~2^64 (> any
// timeout) and evict a live owner. "Future" is reachable two ways —
// (1) a mid-publish reader whose own monotonic read landed fractionally
// after the scanner's (there is no happens-before between the two clock
// reads, and the HB-first/PID-last acquire ordering of §Reader Table
// relies on this stamp being honoured as live), and (2) backward clock
// skew (NTP step-back, manual set, or cross-host on shared storage where
// CLOCK_BOOTTIME origins differ per cross-process.md's own model). Every
// reader-scan comparison and the writer-recovery check share this guard
// (cross-process.md §Reader Table stale detection / §Stale Writer Recovery).
func heartbeatStale(nowNanos, stamp, staleTimeoutNanos uint64) bool {
	if stamp > nowNanos {
		return false
	}
	return nowNanos-stamp > staleTimeoutNanos
}

// RecoverStaleWriter clears the lock file's writer-header fields
// (PID/StartTime/PIDNamespace/Heartbeat) and, if the dead writer
// was in the same PID namespace as ourPIDNamespace, scans the
// reader table and clears any slots owned by the dead writer.
//
// Caller MUST hold flock(LOCK_EX) on f's fd — otherwise the recovery
// races a concurrent live writer (cross-process.md §Stale Writer
// Recovery + the clear-before-unlock clause-explicit invariant).
//
// IsStaleWriter does NOT need to have been called first when the
// caller already holds LOCK_EX: any non-zero WriterPID at LOCK_EX
// acquisition is by definition stale (a live writer would still
// hold flock). The flock goroutine in coord.go uses this property
// directly.
func RecoverStaleWriter(f *File, ourPIDNamespace uint64) {
	deadPID := f.WriterPID()
	deadNS := f.WriterPIDNamespace()
	deadStartTime := f.WriterStartTime()

	// Reader-slot cleanup is only attempted for the same-namespace
	// case — cross-namespace PIDs are not directly comparable, so
	// reader-stale-detection's heartbeat path will clean those up
	// asynchronously (cross-process.md §Reader Table stale
	// detection case 2).
	//
	// Match condition is (PID, PIDNamespace, ProcessStartTime) —
	// all three must agree (cross-process.md §Stale Writer Recovery
	// step 2). Matching only on (PID, PIDNamespace) would wipe a
	// live reader's slot if the OS recycled deadPID to another
	// in-namespace process that subsequently opened a read tx:
	// new reader's slot has the same PID + namespace but a
	// different ProcessStartTime (its own spawn-tick), so the
	// startTime check distinguishes them and skips the clear.
	if deadPID != 0 && deadNS != 0 && ourPIDNamespace != 0 && deadNS == ourPIDNamespace {
		max := f.MaxReaders()
		for i := range max {
			slot := f.Slot(i)
			if Load64(&slot.PID) != deadPID {
				continue
			}
			if Load64(&slot.PIDNamespace) != deadNS {
				continue
			}
			if Load64(&slot.ProcessStartTime) != deadStartTime {
				// Same (PID, namespace) but different start time
				// — PID recycled; this slot belongs to a
				// different (live) process. Skip.
				continue
			}
			// Single home for the 4-store clear ordering (spec
			// §Reader Table clear ordering): PID first so any
			// concurrent other-process scan sees PID == 0 and
			// falls through to the heartbeat path rather than
			// running kill() against the dead (or recycled) PID.
			f.ClearStaleReaderSlot(uint32(i))
		}
	}

	// Clear the writer-header. Per cross-process.md §Stale Writer
	// Recovery step 3 all four fields are cleared (WriterHeartbeat
	// too, unlike normal release which leaves it as-is).
	f.SetWriterPID(0)
	f.SetWriterStartTime(0)
	f.SetWriterPIDNamespace(0)
	f.SetWriterHeartbeat(0)
}
