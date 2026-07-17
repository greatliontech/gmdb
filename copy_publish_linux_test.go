//go:build linux

package gmdb

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// The errno-classification routing in renameNoReplace: the
// unsupported-class errnos (EINVAL / ENOSYS / ENOTSUP — a kernel or
// filesystem without RENAME_NOREPLACE, the vfat/FUSE motivating case)
// degrade to the probe+rename best-effort publish; genuine failures
// surface without a rename attempt (api-surface.md §Check, CopyTo,
// Compact: "degrading to a probe-then-rename where NOREPLACE is
// unsupported").
func TestRenameNoReplaceErrnoRouting(t *testing.T) {
	newPair := func(t *testing.T) (tmp, path string) {
		dir := t.TempDir()
		tmp = filepath.Join(dir, "tmp")
		path = filepath.Join(dir, "dst")
		if err := os.WriteFile(tmp, []byte("copy"), 0o600); err != nil {
			t.Fatal(err)
		}
		return tmp, path
	}
	inject := func(t *testing.T, errno error) {
		t.Helper()
		f := func(string, string) error { return errno }
		renameat2ForTest.Store(&f)
		t.Cleanup(func() { renameat2ForTest.Store(nil) })
	}

	for _, errno := range []unix.Errno{unix.EINVAL, unix.ENOSYS, unix.ENOTSUP} {
		t.Run("degrades_"+errno.Error(), func(t *testing.T) {
			tmp, path := newPair(t)
			inject(t, errno)
			if err := renameNoReplace(tmp, path); err != nil {
				t.Fatalf("unsupported-class %v did not degrade to best-effort: %v", errno, err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, []byte("copy")) {
				t.Fatalf("best-effort publish missing: (%q, %v)", got, err)
			}
			// The degraded form still refuses an existing path.
			tmp2, _ := newPair(t)
			if err := renameNoReplace(tmp2, path); !errors.Is(err, os.ErrExist) {
				t.Fatalf("degraded publish over existing path = %v, want ErrExist", err)
			}
		})
	}

	t.Run("genuine_failure_surfaces", func(t *testing.T) {
		tmp, path := newPair(t)
		inject(t, unix.EACCES)
		err := renameNoReplace(tmp, path)
		if !errors.Is(err, unix.EACCES) {
			t.Fatalf("EACCES = %v, want the surfaced errno", err)
		}
		// No best-effort attempt: path stays absent, tmp intact.
		if _, serr := os.Lstat(path); !errors.Is(serr, os.ErrNotExist) {
			t.Fatalf("genuine failure still published: %v", serr)
		}
		if _, serr := os.Lstat(tmp); serr != nil {
			t.Fatalf("tmp consumed on a surfaced failure: %v", serr)
		}
	})

	t.Run("real_syscall_noreplace", func(t *testing.T) {
		tmp, path := newPair(t)
		if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := renameNoReplace(tmp, path); !errors.Is(err, os.ErrExist) {
			t.Fatalf("real NOREPLACE over existing path = %v, want ErrExist", err)
		}
		got, _ := os.ReadFile(path)
		if !bytes.Equal(got, []byte("existing")) {
			t.Fatal("real NOREPLACE clobbered the existing path")
		}
	})
}
