//go:build darwin || freebsd

package lock

// Build tag matches proc.go's `linux || darwin || freebsd` — the
// package's effective supported set. netbsd/openbsd are not
// covered by chunk-2's proc/flock helpers either, so this file
// stays in lockstep rather than diverging from proc.go's tag.
// Adding a platform requires extending this set in both files.

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// nowMonotonic returns the current monotonic clock in nanoseconds.
// On non-Linux Unix this is CLOCK_MONOTONIC, which does NOT survive
// suspend/resume (cross-process.md §Heartbeat Goroutine accepts this
// — a suspended process accumulates no monotonic time, so its
// "stale" heartbeat remains exactly StaleTimeout seconds away from
// triggering false-detection rather than aging past it).
//
// A CLOCK_MONOTONIC failure indicates a deeply abnormal host
// environment; panic rather than silently return zero, which would
// poison cross-process stale-detection.
func nowMonotonic() uint64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		panic(fmt.Sprintf("lock: clock_gettime(CLOCK_MONOTONIC): %v", err))
	}
	return uint64(ts.Sec)*1_000_000_000 + uint64(ts.Nsec)
}
