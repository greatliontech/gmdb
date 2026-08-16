//go:build linux || darwin || freebsd

package lock

import "golang.org/x/sys/unix"

// mmapRW maps the lock file MAP_SHARED with read+write protection.
// The size is the full lock-file size derived from MaxReaders — per
// cross-process.md the mmap is established once at Open and never
// resized.
//
// x/sys/unix rather than syscall, matching internal/pager's shim: it
// provides the mmap family uniformly across the unix build-tag set.
func mmapRW(fd uintptr, size int64) ([]byte, error) {
	return unix.Mmap(int(fd), 0, int(size),
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_SHARED)
}

func munmap(b []byte) error {
	return unix.Munmap(b)
}
