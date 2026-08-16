package gmdb

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/greatliontech/gmdb/internal/pager"
)

// TestArtifactBarrierReachesFile pins that the production copyDest's
// Sync is not a no-op: on a closed file a real barrier surfaces an
// error from the fd layer (EBADF from the kernel, or os.ErrClosed
// from Go's fd guard), while a neutered barrier returns nil. The
// probe pins "the barrier path is taken", not that a specific syscall
// is issued — power-loss semantics are not observable from userspace.
func TestArtifactBarrierReachesFile(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "dest"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, noFull := range []bool{false, true} {
		if err := (barrierDest{f: f, noFullFsync: noFull}).Sync(); err == nil {
			t.Errorf("barrierDest.Sync(noFullFsync=%v) on closed fd = nil, want error", noFull)
		}
	}
}

// TestDirBarrierReachesFile — the same closed-fd probe for the
// directory-entry barrier used by syncDir/syncDirPath on unix. On
// windows pager.SyncDirBarrier is a defensive always-error (the
// dirent barrier flushes the named file instead — durability.md
// §Platform sync primitives), which would satisfy this probe
// vacuously — skip rather than pretend coverage.
func TestDirBarrierReachesFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows dirent durability uses the named-file barrier; SyncDirBarrier is a defensive error there")
	}
	d, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	for _, noFull := range []bool{false, true} {
		if err := pager.SyncDirBarrier(d, noFull); err == nil {
			t.Errorf("SyncDirBarrier(noFullFsync=%v) on closed fd = nil, want error", noFull)
		}
	}
}
