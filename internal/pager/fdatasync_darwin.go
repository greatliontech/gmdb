//go:build darwin

package pager

import (
	"os"

	"golang.org/x/sys/unix"
)

// fdatasync — the durability barrier on darwin. Apple documents
// fsync(2) as flushing host buffers only, NOT the drive's own cache;
// the only call that reaches stable storage is fcntl(F_FULLFSYNC)
// (durability.md §Platform sync primitives). Only filesystems that
// REJECT F_FULLFSYNC (network mounts and some non-Apple filesystems)
// fall back to fsync(2) — the medium's strongest available flush. A
// real flush failure (EIO/ENOSPC class) must propagate: masking it
// behind a succeeding fsync would ack a commit whose drive flush
// failed, the exact fsyncgate class the poison machinery exists for.
func fdatasync(f *os.File) error {
	_, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0)
	if err == nil {
		return nil
	}
	if fullFsyncUnsupported(err) {
		return f.Sync()
	}
	return &os.PathError{Op: "fcntl(F_FULLFSYNC)", Path: f.Name(), Err: err}
}

// fullFsyncUnsupported reports whether err is the
// filesystem-rejects-F_FULLFSYNC class — the only class where the
// fsync(2) fallback is sound.
func fullFsyncUnsupported(err error) bool {
	switch err {
	// ENOTSUP and EOPNOTSUPP are distinct errnos on darwin (0x2d vs
	// 0x66), unlike linux — admit both.
	case unix.ENOTSUP, unix.EOPNOTSUPP, unix.EINVAL, unix.ENOTTY:
		return true
	}
	return false
}

// syncBarrier is the platform durability barrier behind SyncBarrier and
// the FileOps seam. noFullFsync (Options.NoFullFsync) opts the barrier
// down to plain fsync(2): faster, but an ack'd write still in the
// drive cache is lost on power loss — the documented weaker tier.
func syncBarrier(f *os.File, noFullFsync bool) error {
	if noFullFsync {
		return f.Sync()
	}
	return fdatasync(f)
}

// syncDirBarrier — see SyncDirBarrier. On darwin the dirent barrier
// needs the same F_FULLFSYNC treatment as file barriers (a dirent in
// the drive cache is as lost as a page on power loss), with the same
// NoFullFsync opt-down and fsync fallback.
func syncDirBarrier(d *os.File, noFullFsync bool) error {
	return syncBarrier(d, noFullFsync)
}
