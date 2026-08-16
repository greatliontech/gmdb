//go:build windows

package pager

import (
	"errors"
	"os"
)

// fdatasync falls back to a full fsync: FlushFileBuffers is
// full-strength on windows — it reaches stable storage, so the
// "fsync is strictly stronger" claim holds here (durability.md
// §Platform sync primitives).
func fdatasync(f *os.File) error {
	return f.Sync()
}

// syncBarrier — the NoFullFsync knob is darwin-only; both settings
// take the full-strength fsync fallback.
func syncBarrier(f *os.File, noFullFsync bool) error {
	return fdatasync(f)
}

// syncDirBarrier is UNAVAILABLE on windows: directory handles refuse
// the write access FlushFileBuffers requires (empirically
// ACCESS_DENIED even on a fully-owned directory — the first windows
// CI run). Dirent durability rides the named-file barrier instead
// (the root package's windows syncDir flushes the published file;
// durability.md §Platform sync primitives). Defensive error, never a
// silent no-op.
func syncDirBarrier(d *os.File, noFullFsync bool) error {
	return errors.New("pager: directory flush unavailable on windows; dirent durability uses the named-file barrier")
}
