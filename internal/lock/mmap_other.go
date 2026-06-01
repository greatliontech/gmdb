//go:build !linux

package lock

import "errors"

// errUnsupportedPlatform mirrors internal/pager's same-name shim:
// macOS and FreeBSD shims for the lock file are not yet implemented.
// Until then this file keeps the package buildable on every supported
// OS.
var errUnsupportedPlatform = errors.New("lock: mmap not implemented on this platform yet")

func mmapRW(fd uintptr, size int64) ([]byte, error) {
	return nil, errUnsupportedPlatform
}

func munmap(b []byte) error {
	return errUnsupportedPlatform
}
