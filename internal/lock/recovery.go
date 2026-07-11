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
// NOTE: production stale-writer recovery does NOT consult this
// classifier — the flock goroutine's acquisition path recovers any
// nonzero writer header unconditionally under LOCK_EX (the
// clear-before-unlock invariant makes it definitionally stale).
// IsStaleWriter serves tests and diagnostics; the cross-namespace
// window here follows the shared classification for consistency, not
// because it gates recovery.
func IsStaleWriter(f *File, ourPIDNamespace uint64, nowNanos, staleTimeoutNanos, crossNSTimeoutNanos uint64) bool {
	pid := f.WriterPID()
	if pid == 0 {
		// No writer recorded — nothing to recover (NOT "stale").
		return false
	}
	// The start-time equality inside the shared classifier matches
	// within the timestamp resolution (cross-process.md §Process
	// Start Time): same-tick collisions benignly classify as live —
	// caught by the heartbeat path on actual reuse. The unreadable-
	// start-time fallback is conservative-safer: false-live means no
	// recovery, just blocking on flock until a live writer releases.
	return !identityLive(pid, f.WriterStartTime(), f.WriterPIDNamespace(),
		f.WriterHeartbeat(), ourPIDNamespace, nowNanos, staleTimeoutNanos, crossNSTimeoutNanos, false)
}

// identityLive reports whether a persisted process identity —
// pid/startTime/pidNS plus a heartbeat stamp — names a live process:
// the ONE classification rule shared by the reader-slot scan,
// stale-writer recovery, and the recovery-commit gate's last-writer
// record (cross-process.md §Reader Table stale detection).
// Same-namespace uses kill(0) + start-time PID-reuse detection,
// falling back to the heartbeat when the start time is unreadable
// (conservative toward LIVE — a false-live merely defers recovery);
// cross-namespace or namespace-unknown uses heartbeat freshness alone
// (heartbeatStale — future stamps are fresh, never underflow).
//
// pid == 0 classifies dead ("no identity recorded"); callers for whom
// zero identity means something else (IsStaleWriter's "no writer to
// recover", the reader scan's mid-acquire protocol) gate before
// calling.
//
// timeoutNanos governs the same-namespace heartbeat FALLBACK (start
// time unreadable on a kill(0)-alive process); crossNSTimeoutNanos —
// validated >= timeoutNanos at the Options boundary — governs every
// cross-namespace (or namespace-unknown) classification, where the
// heartbeat is the ONLY signal and a paused/frozen container stops
// heartbeating while its reads stay live (cross-process.md
// §Stale-reader detection, cross-namespace window).
//
// zeroHeartbeatFresh selects the heartbeat==0 semantics, a real
// protocol distinction between the consumers: the reader scan passes
// TRUE — a slot observed with pid != 0 but heartbeat == 0 is a
// release in flight (PID zeroes first, Heartbeat second), and
// clearing it could stomp a third reader's fresh CAS (the phantom
// eviction §Reader Table's clear ordering exists to prevent); writer
// and last-writer records pass FALSE — their four fields are
// published atomically under LOCK_EX, so pid != 0 with heartbeat == 0
// immediately stale — a torn cross-namespace header, or a crashed
// peer, must never block recovery (both consumers evaluate under
// LOCK_EX, so a live mid-publish writer is unobservable there).
func identityLive(pid, startTime, pidNS, heartbeat, ourNS, nowNanos, timeoutNanos, crossNSTimeoutNanos uint64, zeroHeartbeatFresh bool) bool {
	if pid == 0 {
		return false
	}
	hbLive := func(window uint64) bool {
		if heartbeat == 0 {
			return zeroHeartbeatFresh
		}
		return !heartbeatStale(nowNanos, heartbeat, window)
	}
	sameNS := pidNS != 0 && ourNS != 0 && pidNS == ourNS
	if sameNS {
		if !IsAlive(int(pid)) {
			return false
		}
		actual, err := ProcessStartTime(int(pid))
		if err != nil {
			// Alive but unreadable start time (exit race / platform
			// gap): heartbeat fallback — same-NS, short window.
			return hbLive(timeoutNanos)
		}
		return actual == startTime
	}
	return hbLive(crossNSTimeoutNanos)
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

	// A writer that crashed between its shrink-seqlock bumps leaves
	// the counter ODD — readers would burn their bracket retries on
	// every BeginRead until it settles. The dead writer's truncate
	// either never ran or already completed, so re-evening is safe
	// (file-format.md §File Shrinkage).
	if f.ShrinkSeq()%2 == 1 {
		f.BumpShrinkSeq()
	}

	// Clear the writer-header. Per cross-process.md §Stale Writer
	// Recovery step 3 all four fields are cleared (WriterHeartbeat
	// too, unlike normal release which leaves it as-is).
	f.SetWriterPID(0)
	f.SetWriterStartTime(0)
	f.SetWriterPIDNamespace(0)
	f.SetWriterHeartbeat(0)
}
