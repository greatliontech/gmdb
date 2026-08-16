//go:build linux || darwin || freebsd

package pager

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// mmapRO maps the first reservationBytes of file as MAP_SHARED|PROT_READ.
// Per mmap-strategy.md, the reservation is sized to MaxSize; accesses
// beyond the file's current length SIGBUS the process — readers are
// responsible for respecting HighWaterMark.
//
// x/sys/unix rather than syscall: freebsd's frozen syscall package
// does not expose Mprotect, and x/sys/unix provides all three calls
// uniformly across the unix family.
func mmapRO(file uintptr, reservationBytes int64) ([]byte, error) {
	// On 32-bit platforms (linux/386, freebsd/386, …) int(reservationBytes)
	// would silently truncate a >2^31 reservation: mmap succeeds undersized
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
