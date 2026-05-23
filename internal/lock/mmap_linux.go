//go:build linux

package lock

import "syscall"

// mmapRW maps the lock file MAP_SHARED with read+write protection.
// The size is the full lock-file size derived from MaxReaders — per
// cross-process.md the mmap is established once at Open and never
// resized.
func mmapRW(fd uintptr, size int64) ([]byte, error) {
	return syscall.Mmap(int(fd), 0, int(size),
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED)
}

func munmap(b []byte) error {
	return syscall.Munmap(b)
}
