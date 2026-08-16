//go:build linux || darwin || freebsd

package lock

import (
	"errors"
	"syscall"
)

// The flock seam: whole-file advisory locking on the lock file's
// descriptor (cross-process.md §Write Lock). On the unix family every
// operation is flock(2) directly. The conversion variant exists
// because flock documents shared→exclusive conversion as non-atomic
// (release then acquire) — on unix the kernel performs it in one call,
// on windows the seam performs the release explicitly — and every
// conversion caller tolerates the resulting race by design
// (contention ⇒ back off / retry, re-validating under the new lock).

// flockShared acquires the shared lock, blocking.
func flockShared(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_SH)
}

// flockExclusive acquires the exclusive lock, blocking. Callers hold
// no lock on fd when calling (the creator-init path).
func flockExclusive(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_EX)
}

// flockTryExclusive attempts the exclusive lock without blocking.
// Callers hold no lock on fd when calling (the flock goroutine's
// grant loop).
func flockTryExclusive(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
}

// flockTryConvertToExclusive attempts the exclusive lock without
// blocking while fd may already hold the shared lock — flock(2)'s
// non-atomic conversion. On failure the shared lock's state is
// unspecified (the kernel may have released it); callers abort the
// attempt and retry from scratch, which every conversion site does.
func flockTryConvertToExclusive(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
}

// flockUnlock releases whatever lock fd holds. Unlocking an unheld
// fd is harmless.
func flockUnlock(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_UN)
}

// flockErrContended reports whether err is the seam's contention
// signal from a non-blocking acquisition. Classification lives behind
// the seam because the signal is platform-specific (EWOULDBLOCK from
// flock(2); a lock-violation error on windows).
func flockErrContended(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK)
}

// flockErrRetryable reports whether err means the kernel did not
// determine contention (EINTR) and the acquisition should retry
// without consuming a pacing tick.
func flockErrRetryable(err error) bool {
	return errors.Is(err, syscall.EINTR)
}
