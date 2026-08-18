//go:build linux || darwin || freebsd

package flock

import (
	"errors"
	"syscall"
)

// Shared acquires the shared lock, blocking.
func Shared(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_SH)
}

// Exclusive acquires the exclusive lock, blocking. Callers hold
// no lock on fd when calling (the creator-init path).
func Exclusive(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_EX)
}

// TryExclusive attempts the exclusive lock without blocking.
// Callers hold no lock on fd when calling (the flock goroutine's
// grant loop).
func TryExclusive(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
}

// TryConvertToExclusive attempts the exclusive lock without
// blocking while fd may already hold the shared lock — flock(2)'s
// non-atomic conversion. On failure the shared lock's state is
// unspecified (the kernel may have released it); callers abort the
// attempt and retry from scratch, which every conversion site does.
func TryConvertToExclusive(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
}

// Unlock releases whatever lock fd holds. Unlocking an unheld
// fd is harmless.
func Unlock(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_UN)
}

// ErrContended reports whether err is the seam's contention
// signal from a non-blocking acquisition. Classification lives behind
// the seam because the signal is platform-specific (EWOULDBLOCK from
// flock(2); a lock-violation error on windows).
func ErrContended(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK)
}

// ErrRetryable reports whether err means the kernel did not
// determine contention (EINTR) and the acquisition should retry
// without consuming a pacing tick.
func ErrRetryable(err error) bool {
	return errors.Is(err, syscall.EINTR)
}
