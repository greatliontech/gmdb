//go:build !linux && !darwin

package pager

import "os"

// fdatasync falls back to a full fsync on platforms where x/sys does
// not expose fdatasync. On these platforms (freebsd, windows, …) the
// claim "fsync is strictly stronger" holds — fsync(2) / FlushFileBuffers
// reach stable storage — so the fallback only forgoes the
// inode-time-flush optimization the Linux path gets. darwin is the
// exception (fsync does not flush the drive cache there) and has its
// own shim: fdatasync_darwin.go.
func fdatasync(f *os.File) error {
	return f.Sync()
}

// syncBarrier is the platform durability barrier behind SyncBarrier and
// the FileOps seam. The noFullFsync knob is darwin-only; here both
// settings take the full-strength fsync fallback.
func syncBarrier(f *os.File, noFullFsync bool) error {
	return fdatasync(f)
}

// syncDirBarrier — see SyncDirBarrier. A full fsync, the documented
// dirent ritual on these platforms.
func syncDirBarrier(d *os.File, noFullFsync bool) error {
	return d.Sync()
}
