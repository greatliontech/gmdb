//go:build linux && !dst

package flock

import "golang.org/x/sys/unix"

// Per-slot byte-range locks (cross-process.md §Reader Table, slot
// locks): open-file-description (OFD) locks, so ownership rides the
// description — two descriptions conflict, one description does not
// conflict with itself, and the kernel releases at process death.
// Only Linux has OFD semantics; the darwin/freebsd tier uses
// per-slot lock FILES instead (range_other.go).

// RangeSupported reports whether this platform has OFD range locks.
const RangeSupported = true

func ofdLock(fd uintptr, typ int16, off, length int64, cmd int) error {
	return unix.FcntlFlock(fd, cmd, &unix.Flock_t{
		Type:   typ,
		Whence: 0,
		Start:  off,
		Len:    length,
	})
}

// TryExclusiveRange attempts the exclusive OFD lock on
// [off, off+length) without blocking.
func TryExclusiveRange(fd uintptr, off, length int64) error {
	return ofdLock(fd, unix.F_WRLCK, off, length, unix.F_OFD_SETLK)
}

// UnlockRange releases this description's lock on the range.
func UnlockRange(fd uintptr, off, length int64) error {
	return ofdLock(fd, unix.F_UNLCK, off, length, unix.F_OFD_SETLK)
}

// ErrRangeContended reports whether err is the non-blocking range
// acquisition's contention signal.
func ErrRangeContended(err error) bool {
	return err == unix.EAGAIN || err == unix.EACCES || err == unix.EWOULDBLOCK
}
