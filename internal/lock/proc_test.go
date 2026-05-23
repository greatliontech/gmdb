package lock

import (
	"os"
	"testing"
)

func TestIsAliveSelf(t *testing.T) {
	if !IsAlive(os.Getpid()) {
		t.Errorf("IsAlive(self) = false; want true")
	}
}

func TestIsAliveImpossiblePID(t *testing.T) {
	// 0x7FFFFFFF is well above any realistic pid_max ceiling (Linux
	// default 4194304; the value we pick is ~2 billion). The
	// underlying syscall.Kill returns ESRCH which IsAlive maps to
	// false.
	if IsAlive(0x7FFFFFFF) {
		t.Errorf("IsAlive(0x7FFFFFFF) = true; want false")
	}
}

func TestIsAliveRejectsNonPositivePIDs(t *testing.T) {
	// kill(0, …) signals the caller's process group; kill(-N, …)
	// signals process group N. Neither corresponds to "a process
	// with this id," so IsAlive returns false unconditionally on
	// pid ≤ 0. This pins the contract: a slot whose PID field is
	// zero (unused-slot state) is never silently classified as
	// "alive" by stale-detection.
	for _, pid := range []int{0, -1, -42} {
		if IsAlive(pid) {
			t.Errorf("IsAlive(%d) = true; want false", pid)
		}
	}
}
