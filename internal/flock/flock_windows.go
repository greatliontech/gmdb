//go:build windows

package flock

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// The flock seam on windows (cross-process.md §Writer Heartbeat,
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
	rangeOffsetLow  = 0xFFFFFFFF
	rangeOffsetHigh = 0x7FFFFFFF
)

// pollInterval paces the blocking acquisitions' poll loop. The
// contended windows are creator-init races measured in milliseconds;
// 1 ms keeps the wait bounded without busy-spinning.
const pollInterval = time.Millisecond

func lockRange() *windows.Overlapped {
	return &windows.Overlapped{
		Offset:     rangeOffsetLow,
		OffsetHigh: rangeOffsetHigh,
	}
}

func tryRange(fd uintptr, exclusive bool) error {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	return windows.LockFileEx(windows.Handle(fd), flags, 0, 1, 0, lockRange())
}

func blocking(fd uintptr, exclusive bool) error {
	for {
		err := tryRange(fd, exclusive)
		if err == nil {
			return nil
		}
		if err != windows.ERROR_LOCK_VIOLATION {
			return err
		}
		time.Sleep(pollInterval)
	}
}

// Shared acquires the shared lock, blocking.
func Shared(fd uintptr) error {
	return blocking(fd, false)
}

// Exclusive acquires the exclusive lock, blocking. Callers hold
// no lock on fd when calling.
func Exclusive(fd uintptr) error {
	return blocking(fd, true)
}

// TryExclusive attempts the exclusive lock without blocking.
// Callers hold no lock on fd when calling.
func TryExclusive(fd uintptr) error {
	return tryRange(fd, true)
}

// TryConvertToExclusive attempts the exclusive lock without
// blocking while fd may hold the shared lock. LockFileEx cannot
// convert — an exclusive request overlapping our own shared range
// fails — so the seam releases first, matching flock(2)'s documented
// non-atomic conversion, which every conversion caller tolerates
// (abort and retry from scratch, re-validating under the new lock).
func TryConvertToExclusive(fd uintptr) error {
	_ = Unlock(fd)
	return tryRange(fd, true)
}

// Unlock releases whatever lock fd holds. ERROR_NOT_LOCKED on an
// unheld fd is reported but harmless — every caller ignores unlock
// errors.
func Unlock(fd uintptr) error {
	return windows.UnlockFileEx(windows.Handle(fd), 0, 1, 0, lockRange())
}

// ErrContended reports whether err is the seam's contention
// signal from a non-blocking acquisition — LockFileEx's
// FAIL_IMMEDIATELY refusal.
func ErrContended(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// ErrRetryable — windows has no EINTR; nothing is
// retry-without-tick.
func ErrRetryable(err error) bool {
	return false
}
