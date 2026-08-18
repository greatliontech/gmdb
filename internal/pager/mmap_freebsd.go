//go:build freebsd

package pager

import (
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

// freebsd variant of the unix mmap shim (mmap_unix.go): x/sys/unix
// rather than the syscall package, because freebsd's frozen syscall
// package does not expose Mprotect. freebsd is outside the DST build
// set, so the DST toolchain's syscall-wrapper-only contract does not
// bind here.

// mmapRO maps the first reservationBytes of file as MAP_SHARED|PROT_READ.
// Per mmap-strategy.md, the reservation is sized to MaxSize; accesses
// beyond the file's current length SIGBUS the process — readers are
// responsible for respecting HighWaterMark.
func mmapRO(file uintptr, reservationBytes int64) ([]byte, error) {
	// On 32-bit platforms (freebsd/386, …) int(reservationBytes) would
	// silently truncate a >2^31 reservation: mmap succeeds undersized
	// and an in-reservation, in-HighWaterMark read past the truncated
	// mapping SIGSEGVs — outside the spec's SIGBUS-beyond-file model.
	// Unreachable on 64-bit, where int is 64 bits.
	if reservationBytes > math.MaxInt {
		return nil, fmt.Errorf("pager: mmap reservation %d bytes exceeds this platform's address space", reservationBytes)
	}
	return unix.Mmap(int(file), 0, int(reservationBytes),
		unix.PROT_READ, unix.MAP_SHARED)
}

// mprotectRO applies PROT_READ to the mapping as a belt-and-suspenders
// guard. The pages are already mapped read-only, so this is a no-op in
// the common case; it exists to make a stray PROT_WRITE remap during
// development fail loudly.
func mprotectRO(b []byte) error {
	return unix.Mprotect(b, unix.PROT_READ)
}

// munmap releases an mmap region.
func munmap(b []byte) error {
	return unix.Munmap(b)
}

// mmapEnsureCoverage and mmapPrepareShrink are no-ops on unix: the
// single MAP_SHARED VMA over the MaxSize reservation tracks file
// growth and shrink automatically (mmap-strategy.md §mmap Resizing);
// they exist for the windows placeholder model, whose views must be
// extended on growth and unmapped ahead of truncation.
func mmapEnsureCoverage(m []byte, file uintptr, size int64) error { return nil }
func mmapPrepareShrink(m []byte, file uintptr, size int64) error  { return nil }

// platformTruncate — plain ftruncate; the windows implementation
// rounds up to the allocation granularity to keep view layout legal.
func platformTruncate(f *os.File, size int64) error { return f.Truncate(size) }
