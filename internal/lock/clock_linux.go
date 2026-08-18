//go:build linux

package lock

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// nowMonotonic returns the current value of the host's "boot
// monotonic" clock in nanoseconds. On Linux this is CLOCK_BOOTTIME —
// monotonic, survives suspend/resume, and is kernel-wide (not
// per-PID-namespace), so containers sharing a database file via
// volume mount see the same clock value (cross-process.md §Writer
// Heartbeat).
//
// Linux guarantees CLOCK_BOOTTIME on every kernel ≥ 2.6.39. A
// kernel-level failure here means the host environment is wildly
// abnormal; panic rather than silently degrade — a returned-zero
// fallback would let peers misread liveness.
func nowMonotonic() uint64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		panic(fmt.Sprintf("lock: clock_gettime(CLOCK_BOOTTIME): %v", err))
	}
	return uint64(ts.Sec)*1_000_000_000 + uint64(ts.Nsec)
}
