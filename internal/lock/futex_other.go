//go:build !linux

package lock

import "time"

// Adaptive-poll notification waits — the portable fallback for
// platforms without a shared-futex primitive (cross-process.md §Lock
// File Layout, notification region). A waiter re-reads the version
// word on a backoff that starts near-instant and caps low enough to
// keep wake latency in the single-digit milliseconds.
//
// Coverage caveat: this file only compiles off-Linux, so a
// Linux-only test run exercises the futex path exclusively; the
// poll path needs a darwin/windows/BSD run (it also cross-compiles
// there, which catches build breakage but not behavior).

const (
	notifyPollFloor = 100 * time.Microsecond
	notifyPollCap   = 5 * time.Millisecond
)

// notifyWaitState carries the poll backoff across one WaitNotify
// call's sleeps; the zero value starts at the floor.
type notifyWaitState struct {
	backoff time.Duration
}

// sleep waits out the current backoff and doubles it toward the cap.
// The seen value is unused — polling detects changes by the caller's
// re-read, not by a kernel comparison.
func (s *notifyWaitState) sleep(_ *uint64, _ uint64) {
	if s.backoff < notifyPollFloor {
		s.backoff = notifyPollFloor
	}
	time.Sleep(s.backoff)
	s.backoff *= 2
	if s.backoff > notifyPollCap {
		s.backoff = notifyPollCap
	}
}

// notifyWake is a no-op: poll waiters notice the store on their next
// re-read within the backoff cap.
func notifyWake(_ *uint64) {}
