//go:build !linux && !darwin && !freebsd && !windows

package lock

import "errors"

// errUnsupportedPlatform mirrors internal/pager's same-name shim:
// mmap_unix.go covers the unix family and mmap_windows.go the section
// mapping; this file keeps the mmap seam buildable on any platform
// outside both sets.
var errUnsupportedPlatform = errors.New("lock: mmap not implemented on this platform yet")

func mmapRW(fd uintptr, size int64) ([]byte, error) {
	return nil, errUnsupportedPlatform
}

func munmap(b []byte) error {
	return errUnsupportedPlatform
}
