//go:build !linux

package pager

import "errors"

// errUnsupportedPlatform is returned by mmap shims on platforms that have
// not yet had their build-tagged sibling written. macOS and FreeBSD shims
// land in chunk 2's cross-process work; this file keeps the package
// buildable on every supported OS in the meantime.
var errUnsupportedPlatform = errors.New("pager: mmap not implemented on this platform yet")

func mmapRO(file uintptr, reservationBytes int64) ([]byte, error) {
	return nil, errUnsupportedPlatform
}

func mprotectRO(b []byte) error {
	return errUnsupportedPlatform
}

func munmap(b []byte) error {
	return errUnsupportedPlatform
}
