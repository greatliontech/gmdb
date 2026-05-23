//go:build linux || darwin || freebsd

package lock

import (
	"errors"
	"syscall"
)

// IsAlive reports whether a process with the given PID exists in the
// caller's PID namespace. Implementation: kill(pid, 0) — POSIX's
// standard "test that the target is a valid signal target without
// actually delivering anything" interrogation.
//
// Mapping of kernel responses:
//   - success         → process exists, we can signal it ⇒ alive.
//   - EPERM           → process exists, we lack permission to signal
//     it (different UID, no cap_kill, etc.) ⇒ still
//     alive for our purposes.
//   - ESRCH           → no such process ⇒ dead.
//   - any other error → treat as dead conservatively (kernel
//     refusing the syscall is not "alive enough"
//     to trust the slot the PID owns).
//
// pid ≤ 0 is invalid input and returns false unconditionally —
// kill(0, …) signals the caller's process group, kill(-N, …)
// signals process group N, kill(-1, …) signals every process the
// caller may signal; none of these correspond to "a process exists
// with id N" so the function's documented contract cannot be honoured
// on non-positive pids. Surface the misuse as "not alive" so a
// reader/writer slot stamped with PID==0 (the unused-slot state) is
// not silently classified as a live owner.
//
// Cross-namespace caveat: kill(pid, 0) operates on the caller's PID
// namespace. A PID that is "alive" here may be unrelated to the
// process that originally took out the reader/writer slot in
// another container. Callers must combine the IsAlive answer with
// the PIDNamespace check before trusting it — see cross-process.md
// §PID Namespace Awareness.
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
