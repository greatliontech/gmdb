//go:build linux

package lock

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ProcessStartTime returns the start time of the process with the
// given PID, expressed in clock ticks since system boot (typically
// 100 Hz = 10 ms resolution on Linux). The value is read from
// /proc/[pid]/stat field 22 and is monotonically non-decreasing
// across the process's lifetime; together with the PID it forms a
// PID-reuse discriminator used by stale-reader / stale-writer
// detection.
//
// Returns an error when the PID is dead (the file doesn't exist) or
// the file is malformed; callers fall back to the heartbeat-based
// liveness check on error.
//
// Resolution caveat (cited in cross-process.md §Process Start Time):
// two processes spawned within the same clock tick share a start
// time. The protocol does not rely on uniqueness — it combines
// (PID, StartTime, namespace, heartbeat) and tolerates same-time
// collisions because either the prior holder is dead (heartbeat
// stale) or alive (legitimately holds the slot).
func ProcessStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("lock: read /proc/%d/stat: %w", pid, err)
	}
	return parseStartTime(string(data))
}

// parseStartTime extracts field 22 (starttime) from a /proc/[pid]/stat
// line. The comm field (2) is parens-wrapped and can legitimately
// contain arbitrary bytes — including ')' — so the canonical parse
// is to split on the LAST ')'. After that, the remaining whitespace-
// separated fields are state (orig field 3) onward, which means
// starttime (orig field 22) is the 20th post-')' field, index 19
// 0-based.
func parseStartTime(stat string) (uint64, error) {
	rparen := strings.LastIndex(stat, ")")
	if rparen < 0 || rparen+1 >= len(stat) {
		return 0, fmt.Errorf("lock: /proc/stat malformed: no ')' or no fields after it")
	}
	fields := strings.Fields(stat[rparen+1:])
	const startTimeFieldIndex = 19 // 22 (1-based, orig) - 2 (pid, comm) - 1 (0-based)
	if len(fields) <= startTimeFieldIndex {
		return 0, fmt.Errorf("lock: /proc/stat has %d post-')' fields, need > %d",
			len(fields), startTimeFieldIndex)
	}
	v, err := strconv.ParseUint(fields[startTimeFieldIndex], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("lock: parse starttime %q: %w", fields[startTimeFieldIndex], err)
	}
	return v, nil
}

// PIDNamespace returns the calling process's PID namespace inode by
// reading the symlink target of /proc/self/ns/pid. The link target
// has the form "pid:[<inode>]"; we extract the inode.
//
// Returns (0, err) when readlink fails (hardened sandbox, no /proc
// mount) — distinguishable from a hypothetical legitimate (0, nil),
// though Linux procfs never assigns inode 0. The caller can
// inspect the error to log once at Open time (per cross-process.md
// §PID Namespace Awareness: "the DB caches 0 and logs the failure
// via slog.Logger") and then normalise the value to 0 for stale-
// detection purposes ("0 ⇒ different namespace, route through
// heartbeat"). The error is also wrapped from a malformed link so
// the caller can branch on the failure mode if desired.
func PIDNamespace() (uint64, error) {
	target, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return 0, fmt.Errorf("lock: readlink /proc/self/ns/pid: %w", err)
	}
	return parseNSLink(target)
}

// parseNSLink parses "pid:[<inode>]" into the inode value. Surfaces
// an error on a malformed link so the caller can distinguish parse
// failure from readlink failure (both surface as non-nil error from
// PIDNamespace).
func parseNSLink(target string) (uint64, error) {
	lbracket := strings.Index(target, "[")
	rbracket := strings.Index(target, "]")
	if lbracket < 0 || rbracket < 0 || rbracket <= lbracket+1 {
		return 0, fmt.Errorf("lock: PID namespace link %q malformed", target)
	}
	v, err := strconv.ParseUint(target[lbracket+1:rbracket], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("lock: parse PID namespace inode %q: %w", target[lbracket+1:rbracket], err)
	}
	return v, nil
}
