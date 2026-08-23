//go:build (!linux && !windows) || dst

package flock

import "errors"

// Description-scoped range locks exist on Linux (OFD) and windows
// (LockFileEx); the remaining unix platforms have only POSIX range
// locks, which are per-process (any close of any descriptor drops
// them) — unsound for multi-handle processes. This tier uses
// per-slot lock FILES instead (cross-process.md §Reader Table,
// slot locks).
//
// The dst build also lands here: the DST toolchain's simulation
// layer emulates flock(2) (with crash release) but not OFD range
// locks, so simulated runs exercise the per-slot lock-file backend —
// the same portable tier darwin/freebsd use (dst-testing.md
// §Coverage caps).

// RangeSupported reports whether this platform has OFD range locks.
const RangeSupported = false

var errRangeUnsupported = errors.New("flock: OFD range locks unsupported on this platform")

// TryExclusiveRange is unsupported here.
func TryExclusiveRange(fd uintptr, off, length int64) error {
	return errRangeUnsupported
}

// UnlockRange is unsupported here.
func UnlockRange(fd uintptr, off, length int64) error {
	return errRangeUnsupported
}

// ErrRangeContended reports whether err is the non-blocking range
// acquisition's contention signal.
func ErrRangeContended(err error) bool { return false }
