//go:build windows

package lock

import (
	"math"

	"golang.org/x/sys/windows"
)

// stillActive is GetExitCodeProcess's "the process has not exited"
// exit code (STATUS_PENDING). Not exposed by x/sys/windows.
const stillActive = 259

// IsAlive reports whether a process with the given PID exists — the
// windows analog of proc.go's kill(pid, 0) interrogation.
//
// Mapping of kernel responses:
//   - OpenProcess succeeds and the exit code is STILL_ACTIVE ⇒ alive.
//   - OpenProcess succeeds but the exit code is anything else ⇒ the
//     process has terminated (a held handle keeps the object around;
//     the PID no longer names a running process) ⇒ dead.
//   - ERROR_ACCESS_DENIED ⇒ the process exists but we may not query
//     it (another user's) ⇒ alive for our purposes, matching the
//     unix EPERM posture.
//   - any other error (ERROR_INVALID_PARAMETER for a nonexistent
//     PID) ⇒ dead.
//
// pid ≤ 0 is invalid input and returns false, per the seam contract
// in proc.go. NOTE: on windows the classifier never reaches IsAlive
// in production — PIDNamespace() is 0, so identityLive routes every
// record through the heartbeat leg (cross-process.md §PID Namespace
// Awareness) — but the symbol must exist and behave correctly for
// the build and for any future same-host classification.
func IsAlive(pid int) bool {
	if pid <= 0 || pid > math.MaxUint32 {
		// The upper guard restores unix parity for a corrupt record's
		// impossible pid: kill() would ESRCH, but uint32 truncation
		// could alias a live process into false-alive.
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		// Queryable handle but unreadable exit code — deeply abnormal;
		// treat as dead, matching proc.go's "kernel refusing the
		// syscall is not alive enough to trust the slot" posture.
		return false
	}
	// Accepted residual (MSDN's documented caveat): a process that
	// genuinely exits WITH code 259 reads alive until its handles
	// close — false-alive only, which defers reclamation but never
	// evicts a live holder.
	return code == stillActive
}
