package lock

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greatliontech/gmdb/internal/flock"
)

// crashTestUUID is shared between the parent test and the subprocess
// helper so both adopt the same lock file.
var crashTestUUID = [16]byte{0xC4, 0xA5, 0x11}

// TestSlotLockReleasedOnProcessDeath pins the kernel property the
// whole liveness design rides on, for the range backend on the real
// kernel: a process killed while holding a slot lock releases it at
// death, and a survivor's probe-and-clear then reclaims the slot
// (cross-process.md §Reader Table, slot locks; §Stale-slot
// reclamation). The DST crash suite exercises the FILE backend only
// (dst-testing.md §Simulated syscall surface), so this subprocess
// test is the OFD backend's crash coverage.
func TestSlotLockReleasedOnProcessDeath(t *testing.T) {
	if !flock.RangeSupported {
		t.Skip("range backend not in use on this platform/build")
	}
	root, base, fullPath := tmpLock(t)
	dir := filepath.Dir(fullPath)

	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: crashTestUUID, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	cmd := exec.Command(os.Args[0], "-test.run=TestHelperHoldReaderSlotUntilKilled$", "-test.v")
	cmd.Env = append(os.Environ(),
		"GMDB_LOCK_CRASH_HELPER_DIR="+dir,
		"GMDB_LOCK_CRASH_HELPER_BASE="+base,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	// Wait for the child to report its held slot.
	heldCh := make(chan uint32, 1)
	errCh := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			if rest, ok := strings.CutPrefix(line, "HELD "); ok {
				var idx uint32
				if _, err := fmt.Sscanf(rest, "%d", &idx); err != nil {
					errCh <- fmt.Errorf("parse %q: %w", line, err)
					return
				}
				heldCh <- idx
				return
			}
		}
		errCh <- fmt.Errorf("helper exited without HELD line: %v", sc.Err())
	}()
	var idx uint32
	select {
	case idx = <-heldCh:
	case err := <-errCh:
		t.Fatalf("helper: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for the helper to hold its slot")
	}

	// The child is alive and holds the slot lock: the probe must
	// refuse and the reap must not clear.
	if cleared, _ := f.ReapStaleReaderSlots(); cleared != 0 {
		t.Fatalf("reap cleared %d slots under a LIVE holder", cleared)
	}
	if got := Load64(&f.Slot(idx).TxnID); got != 42 {
		t.Fatalf("held slot TxnID = %d, want 42", got)
	}

	// Kill without any cleanup path running in the child.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_ = cmd.Wait()

	// Death released the kernel lock: probe-and-clear reclaims.
	if cleared, _ := f.ReapStaleReaderSlots(); cleared != 1 {
		t.Fatalf("reap after death cleared %d, want 1", cleared)
	}
	if got := Load64(&f.Slot(idx).TxnID); got != 0 {
		t.Fatalf("slot TxnID after reap = %d, want 0", got)
	}
	got, err := f.AcquireReaderSlot(idx, 7, 1)
	if err != nil {
		t.Fatalf("acquire freed slot: %v", err)
	}
	if got != idx {
		t.Fatalf("acquired %d, want the freed slot %d", got, idx)
	}
}

// TestHelperHoldReaderSlotUntilKilled is not a test: it is the
// subprocess body for TestSlotLockReleasedOnProcessDeath, gated on
// the env vars the parent sets. It acquires a reader slot, reports
// it on stdout, and parks until killed.
func TestHelperHoldReaderSlotUntilKilled(t *testing.T) {
	dir := os.Getenv("GMDB_LOCK_CRASH_HELPER_DIR")
	base := os.Getenv("GMDB_LOCK_CRASH_HELPER_BASE")
	if dir == "" || base == "" {
		t.Skip("crash-test helper; runs only as a subprocess")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()
	f, err := Open(OpenParams{Root: root, Base: base, DataUUID: crashTestUUID, MaxReaders: 4})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	idx, err := f.AcquireReaderSlot(0, 42, uint64(os.Getpid()))
	if err != nil {
		t.Fatalf("AcquireReaderSlot: %v", err)
	}
	fmt.Printf("HELD %d\n", idx)
	time.Sleep(time.Hour) // parked until the parent kills us
}
