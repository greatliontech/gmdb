//go:build linux

package pager

import "syscall"

// mmapRO maps the first reservationBytes of file as MAP_SHARED|PROT_READ.
// Per mmap-strategy.md, the reservation is sized to MaxSize; accesses
// beyond the file's current length SIGBUS the process — readers are
// responsible for respecting HighWaterMark.
func mmapRO(file uintptr, reservationBytes int64) ([]byte, error) {
	return syscall.Mmap(int(file), 0, int(reservationBytes),
		syscall.PROT_READ, syscall.MAP_SHARED)
}

// mprotectRO applies PROT_READ to the mapping as a belt-and-suspenders
// guard. The pages are already mapped read-only, so this is a no-op in
// the common case; it exists to make a stray PROT_WRITE remap during
// development fail loudly.
func mprotectRO(b []byte) error {
	return syscall.Mprotect(b, syscall.PROT_READ)
}

// munmap releases an mmap region.
func munmap(b []byte) error {
	return syscall.Munmap(b)
}
