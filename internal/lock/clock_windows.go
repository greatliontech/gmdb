//go:build windows

package lock

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procQueryUnbiasedInterruptTime — not wrapped by x/sys/windows, so
// the proc is resolved lazily from kernel32 (present since Windows 7).
var procQueryUnbiasedInterruptTime = windows.
	NewLazySystemDLL("kernel32.dll").
	NewProc("QueryUnbiasedInterruptTime")

// nowMonotonic returns the current monotonic clock in nanoseconds.
// QueryUnbiasedInterruptTime per cross-process.md's WINDOWS PORT
// DESIGN: kernel-wide (one origin for every process on the host, so
// heartbeats are cross-process comparable), boot-relative, and it
// EXCLUDES time spent suspended — the same suspend posture as
// CLOCK_MONOTONIC on the non-Linux unix family, covered by the same
// StaleTimeout analysis (a suspended process accumulates no monotonic
// time, so its heartbeat cannot age past false-stale detection).
//
// The value is 100 ns units. A failure indicates a deeply abnormal
// host; panic rather than silently return zero, which would poison
// cross-process stale-detection (matching clock_other.go's posture).
func nowMonotonic() uint64 {
	var t uint64
	r1, _, err := procQueryUnbiasedInterruptTime.Call(uintptr(unsafe.Pointer(&t)))
	if r1 == 0 {
		panic(fmt.Sprintf("lock: QueryUnbiasedInterruptTime: %v", err))
	}
	return t * 100
}
