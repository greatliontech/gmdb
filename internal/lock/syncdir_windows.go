//go:build windows

package lock

import "os"

// syncDir is a no-op on windows: FlushFileBuffers on a directory
// handle is refused (Access is denied) through os.File.Sync.
// Production never reaches it here — windows runs the RANGE slot-
// lock backend (flock.RangeSupported), so populateReadersDir, the
// only caller, is gated off; the symbol exists because the file-
// tier code compiles on every platform.
func syncDir(root *os.Root, name string) error { return nil }
