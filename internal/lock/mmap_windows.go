//go:build windows

package lock

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// mmapRW maps the lock file MAP_SHARED-equivalent with read+write
// protection: a PAGE_READWRITE section over the exact file size,
// mapped as one FILE_MAP_WRITE view. Per cross-process.md the lock
// file's size is fixed at creation (derived from MaxReaders) and the
// mapping is established once at Open and never resized — no
// placeholder machinery is needed, unlike the data file's mapping.
// Views of one section share pages across processes, so the atomic
// slot protocol is cross-process visible exactly as on unix.
func mmapRW(fd uintptr, size int64) ([]byte, error) {
	section, err := windows.CreateFileMapping(windows.Handle(fd), nil,
		windows.PAGE_READWRITE, uint32(uint64(size)>>32), uint32(uint64(size)&0xFFFFFFFF), nil)
	if err != nil {
		return nil, fmt.Errorf("lock: CreateFileMapping %d: %w", size, err)
	}
	// The view holds a reference to the section; the handle can close
	// immediately.
	defer windows.CloseHandle(section)
	addr, err := windows.MapViewOfFile(section,
		windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, uintptr(size))
	if err != nil {
		return nil, fmt.Errorf("lock: MapViewOfFile %d: %w", size, err)
	}
	return unsafe.Slice(addrToBytePtr(addr), size), nil
}

// addrToBytePtr — see internal/pager's same-name helper: a
// kernel-returned mapping address is not a Go object; the
// reinterpretation through the local's address is the vet-accepted
// form of that conversion.
func addrToBytePtr(addr uintptr) *byte {
	return *(**byte)(unsafe.Pointer(&addr))
}

func munmap(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return windows.UnmapViewOfFile(uintptr(unsafe.Pointer(&b[0])))
}
