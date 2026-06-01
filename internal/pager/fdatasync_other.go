//go:build !linux

package pager

import "os"

// fdatasync falls back to a full fsync on platforms where x/sys does not
// expose fdatasync (darwin, windows, …). This is correctness-equivalent
// — fsync is strictly stronger — and only forgoes the inode-time-flush
// optimization the Linux path gets. See fdatasync_linux.go.
func fdatasync(f *os.File) error {
	return f.Sync()
}
