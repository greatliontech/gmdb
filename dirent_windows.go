//go:build windows

package gmdb

import (
	"os"
	"path/filepath"

	"github.com/greatliontech/gmdb/internal/pager"
)

// syncDir makes name's directory entry durable. The POSIX dir-fsync
// ritual is unavailable on windows — directory handles refuse the
// write access FlushFileBuffers requires (empirically ACCESS_DENIED
// even on a fully-owned directory) — so the barrier flushes the NAMED
// file itself, opened under the same pinned root (no path
// re-resolution beyond the root's confinement). NTFS journals
// name-space operations sequentially, and a file's FlushFileBuffers
// forces the log past that file's latest metadata record — the very
// create or rename that published `name` — making the dirent durable
// (durability.md §Platform sync primitives). FAT-class destinations
// keep the publish ladder's documented weaker tier.
func syncDir(root *os.Root, name string, noFullFsync bool) error {
	f, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := pager.SyncBarrier(f, noFullFsync); err != nil {
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
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := pager.SyncBarrier(f, noFullFsync); err != nil {
		return err
	}
	if hook := syncDirHookForTest.Load(); hook != nil {
		return (*hook)(dir)
	}
	return nil
}
