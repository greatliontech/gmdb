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
	hb := f.WriterHeartbeat()
	// Guard against underflow if the recorded heartbeat is somehow
	// in the future (clock skew across hosts is impossible in
	// single-host CLOCK_BOOTTIME; cross-host would only matter for
	// shared-storage deployments which aren't supported). Treat
	// future-stamped heartbeat as fresh (conservative).
	if hb > nowNanos {
		return false
	}
	return nowNanos-hb > staleTimeoutNanos
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

	// Reader-slot cleanup is only attempted for the same-namespace
	// case — cross-namespace PIDs are not directly comparable, so
	// reader-stale-detection's heartbeat path will clean those up
	// asynchronously (cross-process.md §Reader Table stale
	// detection case 2).
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
			// Spec slot-release ordering (§Reader Table slot
			// release): PID → Heartbeat → HintEpoch → TxnID. PID
			// first ensures any concurrent (other-process) scan
			// sees PID == 0 and falls through to the heartbeat
			// path rather than running kill() against the dead
			// (or recycled) PID.
			Store64(&slot.PID, 0)
			Store64(&slot.Heartbeat, 0)
			Store64(&slot.HintEpoch, 0)
			Store64(&slot.TxnID, 0)
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
