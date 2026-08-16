//go:build darwin

package pager

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestDarwinBarrierSucceeds smoke-tests both darwin barrier paths on a
// real file: the F_FULLFSYNC default and the NoFullFsync fsync
// opt-down. The power-loss semantics themselves are not testable in
// software; this pins that the fcntl path is accepted by the running
// filesystem (or falls back cleanly where F_FULLFSYNC is unsupported).
func TestDarwinBarrierSucceeds(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "barrier"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("x"); err != nil {
		t.Fatal(err)
	}
	if err := syncBarrier(f, false); err != nil {
		t.Errorf("full barrier: %v", err)
	}
	if err := syncBarrier(f, true); err != nil {
		t.Errorf("fsync opt-down: %v", err)
	}
}

// TestFullFsyncFallbackGate pins the errno gate on the darwin
// fallback: only the filesystem-rejects-F_FULLFSYNC class may fall
// back to fsync(2); real flush failures must propagate (durability.md
// §Platform sync primitives — a barrier's success means stable
// storage).
func TestFullFsyncFallbackGate(t *testing.T) {
	for _, e := range []error{unix.ENOTSUP, unix.EOPNOTSUPP, unix.EINVAL, unix.ENOTTY} {
		if !fullFsyncUnsupported(e) {
			t.Errorf("fullFsyncUnsupported(%v) = false, want true", e)
		}
	}
	for _, e := range []error{unix.EIO, unix.ENOSPC, unix.EBADF, unix.EINTR} {
		if fullFsyncUnsupported(e) {
			t.Errorf("fullFsyncUnsupported(%v) = true, want false — this error must propagate", e)
		}
	}
}
