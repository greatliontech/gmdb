//go:build linux

package lock

import (
	"os"
	"strings"
	"testing"
)

// TestLockMappingSharedReadWrite pins the lock-file mapping flags
// required by cross-process.md: MAP_SHARED (slot writes are visible to
// every process without msync) with read+write protection. A
// MAP_PRIVATE mapping would silently confine heartbeats and reader
// slots to the writing process, so the flags are pinned directly via
// /proc/self/maps (hence the linux build tag; the flags are required
// on every unix platform the shim builds for).
func TestLockMappingSharedReadWrite(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "lock")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(4096); err != nil {
		t.Fatal(err)
	}

	m, err := mmapRW(f.Fd(), 4096)
	if err != nil {
		t.Fatalf("mmapRW: %v", err)
	}
	defer func() {
		if err := munmap(m); err != nil {
			t.Errorf("munmap: %v", err)
		}
	}()

	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatalf("read /proc/self/maps: %v", err)
	}
	var perms []string
	for _, line := range strings.Split(string(maps), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 && fields[5] == f.Name() {
			perms = append(perms, fields[1])
		}
	}
	if len(perms) == 0 {
		t.Fatalf("no mapping of %s in /proc/self/maps", f.Name())
	}
	for _, p := range perms {
		if p != "rw-s" {
			t.Errorf("mapping perms = %q, want %q (PROT_READ|PROT_WRITE, MAP_SHARED)", p, "rw-s")
		}
	}
}
