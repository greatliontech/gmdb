//go:build !linux && !darwin && !freebsd && !(windows && (amd64 || arm64))

package pager

import (
	"errors"
	"os"
)

// errUnsupportedPlatform is returned by mmap shims on platforms that have
// not yet had their build-tagged sibling written (mmap_unix.go covers the
// unix family, mmap_windows.go the placeholder model). This file keeps
// the package buildable on every supported OS in the meantime.
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

func mmapEnsureCoverage(m []byte, file uintptr, size int64) error { return errUnsupportedPlatform }
func mmapPrepareShrink(m []byte, file uintptr, size int64) error  { return errUnsupportedPlatform }

func platformTruncate(f *os.File, size int64) error { return f.Truncate(size) }
