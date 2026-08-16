//go:build !linux && !darwin && !freebsd

package pager

import "errors"

// errUnsupportedPlatform is returned by mmap shims on platforms that have
// not yet had their build-tagged sibling written (mmap_unix.go covers the
// unix family; windows is the notable absentee). This file keeps the
// package buildable on every supported OS in the meantime.
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
