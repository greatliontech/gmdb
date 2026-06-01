//go:build linux

package pager

import (
	"os"

	"golang.org/x/sys/unix"
)

// fdatasync flushes f's data — and the file-size metadata needed to
// read it back — to stable storage WITHOUT the inode-time flush a full
// fsync pays. Per durability.md §Durability Modes the commit hot path
// (step 2 data, step 4 meta) uses fdatasync so each SyncDurable /
// SyncDataOnly commit avoids the extra inode-metadata write. Linux
// exposes it via unix.Fdatasync; other platforms fall back to a full
// fsync (fdatasync_other.go).
func fdatasync(f *os.File) error {
	return unix.Fdatasync(int(f.Fd()))
}
