//go:build windows && !dst

package flock

import (
	"errors"

	"golang.org/x/sys/windows"
)

// Per-slot byte-range locks on windows: LockFileEx ranges are
// HANDLE-scoped — two handles conflict, a handle's lock survives
// until UnlockFileEx, handle close, or process termination — the
// same shape as Linux's open-file-description locks, so windows
// runs the range backend rather than per-slot lock files (which
// cost one CreateFile per slot per incarnation).
//
// Windows range locks are MANDATORY against ReadFile/WriteFile from
// other handles (mapped views are not policed). The slot ranges lie
// inside the reader table, which every steady-state path accesses
// through the section mapping only; the file-I/O paths that do touch
// the lock file either stay inside the header (below every slot
// range) or run where no slot lock can exist (creation under the
// creator's exclusivity, the boot-epoch reset under its no-live-
// holders precondition) — cross-process.md §Reader Table, slot
// locks, windows arm.

// RangeSupported reports whether this platform has handle-scoped
// range locks.
const RangeSupported = true

func rangeOverlapped(off int64) *windows.Overlapped {
	return &windows.Overlapped{
		Offset:     uint32(off),
		OffsetHigh: uint32(off >> 32),
	}
}

// TryExclusiveRange attempts the exclusive lock on [off, off+length)
// without blocking.
func TryExclusiveRange(fd uintptr, off, length int64) error {
	return windows.LockFileEx(windows.Handle(fd),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, uint32(length), uint32(length>>32), rangeOverlapped(off))
}

// UnlockRange releases this handle's lock on the range.
func UnlockRange(fd uintptr, off, length int64) error {
	return windows.UnlockFileEx(windows.Handle(fd),
		0, uint32(length), uint32(length>>32), rangeOverlapped(off))
}

// ErrRangeContended reports whether err is the non-blocking range
// acquisition's contention signal.
func ErrRangeContended(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
