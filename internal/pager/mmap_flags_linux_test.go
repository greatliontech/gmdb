//go:build linux

package pager

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// mappingPerms returns the perms column of every /proc/self/maps entry
// backed by path. Proc-based, hence the linux build tag; the flags it
// pins are required on every unix platform the shim builds for.
func mappingPerms(t *testing.T, path string) []string {
	t.Helper()
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatalf("read /proc/self/maps: %v", err)
	}
	var perms []string
	for _, line := range strings.Split(string(maps), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 && fields[5] == path {
			perms = append(perms, fields[1])
		}
	}
	return perms
}

// TestDataMappingSharedReadOnly pins the data-file mapping flags
// required by mmap-strategy.md §Invariants: MAP_SHARED (a pwrite by any
// process is visible through the mapping without msync) and PROT_READ
// only (the writer never writes through the mmap pointer). The
// behavioral difference is invisible to a pure reader on Linux — a
// never-written MAP_PRIVATE mapping stays page-cache-backed — so the
// flags are pinned directly via /proc/self/maps.
func TestDataMappingSharedReadOnly(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "data")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(4096); err != nil {
		t.Fatal(err)
	}

	m, err := mmapRO(f.Fd(), 4096)
	if err != nil {
		t.Fatalf("mmapRO: %v", err)
	}
	defer func() {
		if err := munmap(m); err != nil {
			t.Errorf("munmap: %v", err)
		}
	}()

	check := func(stage string) {
		t.Helper()
		perms := mappingPerms(t, f.Name())
		if len(perms) == 0 {
			t.Fatalf("%s: no mapping of %s in /proc/self/maps", stage, f.Name())
		}
		for _, p := range perms {
			if p != "r--s" {
				t.Errorf("%s: mapping perms = %q, want %q (PROT_READ, MAP_SHARED)", stage, p, "r--s")
			}
		}
	}
	check("after mmapRO")

	if err := mprotectRO(m); err != nil {
		t.Fatalf("mprotectRO: %v", err)
	}
	check("after mprotectRO")
}

// TestMprotectGuardDowngradesWritableMapping pins that mprotectRO
// actively applies PROT_READ rather than merely preserving an already
// read-only mapping: on a scratch rw mapping the perms must flip to
// r--s, so a neutered guard (a body of `return nil`) fails the flip
// instead of riding the status quo.
func TestMprotectGuardDowngradesWritableMapping(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "guard")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(4096); err != nil {
		t.Fatal(err)
	}

	m, err := unix.Mmap(int(f.Fd()), 0, 4096,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap rw: %v", err)
	}
	defer func() {
		if err := unix.Munmap(m); err != nil {
			t.Errorf("munmap: %v", err)
		}
	}()

	assertPerms := func(stage, want string) {
		t.Helper()
		perms := mappingPerms(t, f.Name())
		if len(perms) == 0 {
			t.Fatalf("%s: no mapping of %s in /proc/self/maps", stage, f.Name())
		}
		for _, p := range perms {
			if p != want {
				t.Errorf("%s: mapping perms = %q, want %q", stage, p, want)
			}
		}
	}
	assertPerms("before mprotectRO", "rw-s")

	if err := mprotectRO(m); err != nil {
		t.Fatalf("mprotectRO: %v", err)
	}
	assertPerms("after mprotectRO", "r--s")
}
