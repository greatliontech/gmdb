//go:build !linux && !darwin && !freebsd

package lock

import "errors"

// errUnsupportedPlatform mirrors internal/pager's same-name shim:
// mmap_unix.go covers the unix family; platforms outside it (windows
// is the notable absentee) have no lock-file mmap yet. Until then this
// file keeps the mmap seam buildable outside the unix family (the
// package as a whole has other unix-only dependencies).
var errUnsupportedPlatform = errors.New("lock: mmap not implemented on this platform yet")

func mmapRW(fd uintptr, size int64) ([]byte, error) {
	return nil, errUnsupportedPlatform
}

func munmap(b []byte) error {
	return errUnsupportedPlatform
}
