//go:build linux

package gmdb

import (
	"errors"
	"os"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// renameat2ForTest, when set, replaces the raw renameat2 syscall in
// renameNoReplace — the seam for exercising the errno-classification
// routing (unsupported-class degrades to best-effort; genuine
// failures surface) without a NOREPLACE-less filesystem. Same
// non-parallel rule as the other copy seams.
var renameat2ForTest atomic.Pointer[func(tmp, path string) error]

// renameNoReplace publishes tmp at path, refusing to replace an
// existing path. On Linux the atomic form is
// renameat2(RENAME_NOREPLACE); a kernel or filesystem without it
// (EINVAL / ENOSYS / ENOTSUP) degrades to the probe+rename
// best-effort form, matching the non-Linux publish (api-surface.md
// §Check, CopyTo, Compact per-filesystem contract).
func renameNoReplace(tmp, path string) error {
	renameat2 := func(tmp, path string) error {
		return unix.Renameat2(unix.AT_FDCWD, tmp, unix.AT_FDCWD, path, unix.RENAME_NOREPLACE)
	}
	if h := renameat2ForTest.Load(); h != nil {
		renameat2 = *h
	}
	err := renameat2(tmp, path)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) {
		return renameNoReplaceBestEffort(tmp, path)
	}
	return &os.LinkError{Op: "renameat2", Old: tmp, New: path, Err: err}
}
