//go:build !linux

package lock

import "errors"

// errStartTimeUnsupported is returned by ProcessStartTime on platforms
// where the per-platform implementation is not present in this build —
// macOS / FreeBSD (sysctl designs) and windows (GetProcessTimes
// design) all ship this stub; the designs stay PORT DESIGN rows in
// cross-process.md §Process Start Time. On error, callers fall back
// to the heartbeat-based liveness check per that section.
var errStartTimeUnsupported = errors.New("lock: ProcessStartTime not implemented on this platform")

func ProcessStartTime(pid int) (uint64, error) {
	return 0, errStartTimeUnsupported
}

// PIDNamespace returns 0 on non-Linux platforms — PID namespaces are
// a Linux concept; cross-process.md treats a 0 namespace value as
// "different namespace" for stale-detection purposes, which forces
// the heartbeat fallback. The nil error reflects "no error, this
// platform legitimately has no namespace inode," not a failure.
func PIDNamespace() (uint64, error) {
	return 0, nil
}
