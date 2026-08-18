//go:build linux || darwin || freebsd

package lock

import "syscall"

// mmapRW maps the lock file MAP_SHARED with read+write protection.
// The size is the full lock-file size derived from MaxReaders — per
// cross-process.md the mmap is established once at Open and never
// resized.
//
// The syscall package rather than x/sys/unix: the DST toolchain
// models the mmap family only behind the syscall package's named
// wrappers, and refuses x/sys's raw mmap trampoline (dst-testing.md
// §Simulated syscall surface — a mmap-family-specific contract, not
// a tree-wide x/sys ban). Unlike internal/pager's shim this one
// needs no Mprotect, so freebsd's frozen syscall package covers it
// too.
func mmapRW(fd uintptr, size int64) ([]byte, error) {
	return syscall.Mmap(int(fd), 0, int(size),
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED)
}

func munmap(b []byte) error {
	return syscall.Munmap(b)
}
