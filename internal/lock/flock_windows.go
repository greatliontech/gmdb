//go:build windows

package lock

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// The flock seam on windows (cross-process.md §Heartbeat Goroutine,
// WINDOWS PORT DESIGN): whole-file advisory semantics emulated with a
// one-byte LockFileEx/UnlockFileEx range at offset 2^63−1. Windows
// byte-range locks are MANDATORY against ReadFile/WriteFile (mapped
// views are not checked) — so the range sits beyond any byte the
// lock file ever contains and never intersects the read/write paths.
// Shared/exclusive map to LOCKFILE_EXCLUSIVE_LOCK; non-blocking to
// LOCKFILE_FAIL_IMMEDIATELY; blocking acquisitions poll the
// non-blocking variant (windows has no EINTR, and the only blocking
// windows are brief creator-init races).

// flockRangeOffset is 2^63−1 split into the OVERLAPPED offset halves.
const (
	flockRangeOffsetLow  = 0xFFFFFFFF
	flockRangeOffsetHigh = 0x7FFFFFFF
)

// flockPollInterval paces the blocking acquisitions' poll loop. The
// contended windows are creator-init races measured in milliseconds;
// 1 ms keeps the wait bounded without busy-spinning.
const flockPollInterval = time.Millisecond

func flockRange() *windows.Overlapped {
	return &windows.Overlapped{
		Offset:     flockRangeOffsetLow,
		OffsetHigh: flockRangeOffsetHigh,
	}
}

func flockTry(fd uintptr, exclusive bool) error {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	return windows.LockFileEx(windows.Handle(fd), flags, 0, 1, 0, flockRange())
}

func flockBlocking(fd uintptr, exclusive bool) error {
	for {
		err := flockTry(fd, exclusive)
		if err == nil {
			return nil
		}
		if err != windows.ERROR_LOCK_VIOLATION {
			return err
		}
		time.Sleep(flockPollInterval)
	}
}

// flockShared acquires the shared lock, blocking.
func flockShared(fd uintptr) error {
	return flockBlocking(fd, false)
}

// flockExclusive acquires the exclusive lock, blocking. Callers hold
// no lock on fd when calling.
func flockExclusive(fd uintptr) error {
	return flockBlocking(fd, true)
}

// flockTryExclusive attempts the exclusive lock without blocking.
// Callers hold no lock on fd when calling.
func flockTryExclusive(fd uintptr) error {
	return flockTry(fd, true)
}

// flockTryConvertToExclusive attempts the exclusive lock without
// blocking while fd may hold the shared lock. LockFileEx cannot
// convert — an exclusive request overlapping our own shared range
// fails — so the seam releases first, matching flock(2)'s documented
// non-atomic conversion, which every conversion caller tolerates
// (abort and retry from scratch, re-validating under the new lock).
func flockTryConvertToExclusive(fd uintptr) error {
	_ = flockUnlock(fd)
	return flockTry(fd, true)
}

// flockUnlock releases whatever lock fd holds. ERROR_NOT_LOCKED on an
// unheld fd is reported but harmless — every caller ignores unlock
// errors.
func flockUnlock(fd uintptr) error {
	return windows.UnlockFileEx(windows.Handle(fd), 0, 1, 0, flockRange())
}

// flockErrContended reports whether err is the seam's contention
// signal from a non-blocking acquisition — LockFileEx's
// FAIL_IMMEDIATELY refusal.
func flockErrContended(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// flockErrRetryable — windows has no EINTR; nothing is
// retry-without-tick.
func flockErrRetryable(err error) bool {
	return false
}
