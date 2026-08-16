//go:build !windows

package gmdb

import (
	"os"

	"github.com/greatliontech/gmdb/internal/pager"
)

// syncDir makes name's directory entry durable by fsyncing the parent
// directory through the handle's pinned os.Root — POSIX's dirent
// ritual (durability.md §Directory-entry durability): dirents are
// durable only after the parent directory is fsynced, and fsyncing
// through the pinned root hits the SAME directory the data file was
// opened under even if a path component was re-pointed since (the
// symlink-guard rationale on DB.root). Callers treat failure as fatal
// for their operation. name is unused on unix; the windows
// implementation flushes the named file instead.
func syncDir(root *os.Root, name string, noFullFsync bool) error {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	if err := pager.SyncDirBarrier(d, noFullFsync); err != nil {
		return err
	}
	if hook := syncDirHookForTest.Load(); hook != nil {
		return (*hook)(root.Name())
	}
	return nil
}

// syncDirPath is the path-addressed variant for targets with no
// pinned root (CopyTo's freshly-created output file).
func syncDirPath(dir, name string, noFullFsync bool) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := pager.SyncDirBarrier(d, noFullFsync); err != nil {
		return err
	}
	if hook := syncDirHookForTest.Load(); hook != nil {
		return (*hook)(dir)
	}
	return nil
}
