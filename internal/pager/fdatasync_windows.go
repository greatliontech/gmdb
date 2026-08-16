//go:build windows

package pager

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
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

// procReOpenFile — not wrapped by x/sys/windows.
var procReOpenFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

// syncDirBarrier — the windows directory-entry ritual (durability.md
// §Platform sync primitives): FlushFileBuffers demands WRITE access,
// and Go's os.Open yields a read-access directory handle, so the
// handle is reopened with GENERIC_WRITE via ReOpenFile — by HANDLE,
// never by path, so the caller's os.Root symlink-guard open is not
// re-resolved — and that handle is flushed.
func syncDirBarrier(d *os.File, noFullFsync bool) error {
	r1, _, callErr := procReOpenFile.Call(
		d.Fd(),
		uintptr(uint32(windows.GENERIC_WRITE)),
		uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE),
		uintptr(windows.FILE_FLAG_BACKUP_SEMANTICS))
	h := windows.Handle(r1)
	if h == windows.InvalidHandle || h == 0 {
		return fmt.Errorf("pager: ReOpenFile(directory, GENERIC_WRITE): %w", callErr)
	}
	defer windows.CloseHandle(h)
	if err := windows.FlushFileBuffers(h); err != nil {
		return fmt.Errorf("pager: FlushFileBuffers(directory): %w", err)
	}
	return nil
}
