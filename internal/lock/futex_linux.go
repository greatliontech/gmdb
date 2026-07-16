//go:build linux

package lock

import (
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Futex-backed notification waits (cross-process.md §Lock File
// Layout, notification region). The kernel futex API operates on
// 32-bit words; the version words are uint64, so both sides target
// the word's LOW-order 32 bits — every publish is a +1 bump or a
// stamp of a just-bumped value, so the low half changes on every
// version change (a same-low-half change would need a single publish
// to advance the version by an exact multiple of 2^32). The futex is
// SHARED (no FUTEX_PRIVATE_FLAG): waiters and publishers are
// different processes mapping the same file.

// notifyWaitSlice bounds one futex sleep so the WaitNotify loop
// re-checks its stop condition even if every wake is missed; wakes
// (publish, context cancellation) end the sleep immediately.
const notifyWaitSlice = 100 * time.Millisecond

// Futex operation codes (uapi/linux/futex.h; x/sys/unix exports only
// the syscall numbers). Deliberately WITHOUT FUTEX_PRIVATE_FLAG.
const (
	futexOpWait = 0
	futexOpWake = 1
)

// lowHalfOffset is the byte offset of a uint64's low-order 32 bits
// on this host (0 little-endian, 4 big-endian).
var lowHalfOffset = func() uintptr {
	x := uint64(1)
	if *(*uint32)(unsafe.Pointer(&x)) == 1 {
		return 0
	}
	return 4
}()

func notifyLowHalf(w *uint64) *uint32 {
	return (*uint32)(unsafe.Add(unsafe.Pointer(w), lowHalfOffset))
}

// notifyWaitState carries per-wait pacing state. The futex path
// needs none: wake latency is the kernel's, and the slice is fixed.
type notifyWaitState struct{}

// sleep blocks until the word changes from seen, a wake arrives, or
// the slice expires. All error returns (EAGAIN: word already
// changed; EINTR: signal; ETIMEDOUT) mean "re-check now", which is
// exactly what the caller's loop does.
func (notifyWaitState) sleep(w *uint64, seen uint64) {
	ts := unix.NsecToTimespec(notifyWaitSlice.Nanoseconds())
	_, _, _ = unix.Syscall6(
		unix.SYS_FUTEX,
		uintptr(unsafe.Pointer(notifyLowHalf(w))),
		uintptr(futexOpWait),
		uintptr(uint32(seen)),
		uintptr(unsafe.Pointer(&ts)),
		0, 0,
	)
}

// notifyWake wakes every waiter on the word (publishers after a
// stamp; context cancellation).
func notifyWake(w *uint64) {
	_, _, _ = unix.Syscall6(
		unix.SYS_FUTEX,
		uintptr(unsafe.Pointer(notifyLowHalf(w))),
		uintptr(futexOpWake),
		uintptr(int32(1<<31-1)),
		0, 0, 0,
	)
}
