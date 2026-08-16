//go:build windows && (amd64 || arm64)

package pager

import (
	"fmt"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows data-file mapping (mmap-strategy.md §Windows): a
// VirtualAlloc2 PLACEHOLDER reservation of MaxSize whose base address
// never moves, with the file's backed extent mapped into it as
// read-only views via MapViewOfFile3 — the faithful collapse of the
// unix fixed-reservation contract. File growth maps the new extent
// into the remaining placeholder (existing views and borrowed page
// pointers stay valid); file shrink unmaps the tail views FIRST
// (windows refuses SetEndOfFile under a mapped view) and remaps the
// surviving partial extent after truncation. Access beyond the mapped
// coverage raises an access violation — the platform's analog of the
// SIGBUS-beyond-file model; the HighWaterMark guard remains the only
// protection.
//
// Alignment regime: placeholder operations and view offsets work at
// the allocation granularity G (64 KiB), so the engine keeps windows
// data-file lengths G-aligned — platformTruncate rounds every
// truncation up to G, and mmapRO aligns a foreign (unix-created)
// file once at open when the handle is writable. Coverage equals the
// real file length on every path — a failed shrink RESTORES the
// coverage it unmapped — and every view is whole-G.
//
// Fragmentation invariant: the unmapped remainder
// [coverage, reservation) is always a SINGLE placeholder — every
// unmap path re-coalesces (unmapFrom), so mapExtent's split
// precondition (range within one placeholder) holds by construction
// and full teardown is one release.

// Placeholder constants x/sys/windows does not define.
const (
	memReservePlaceholder  = 0x00040000 // MEM_RESERVE_PLACEHOLDER
	memReplacePlaceholder  = 0x00004000 // MEM_REPLACE_PLACEHOLDER
	memPreservePlaceholder = 0x00000002 // MEM_PRESERVE_PLACEHOLDER (VirtualFree / UnmapViewOfFile2)
	memCoalescePlaceholder = 0x00000001 // MEM_COALESCE_PLACEHOLDERS
)

// allocGranularity is the windows allocation granularity — 64 KiB on
// every windows platform Go supports (a documented, ABI-stable
// constant, not a tunable).
const allocGranularity = 64 << 10

var (
	modkernelbase        = windows.NewLazySystemDLL("kernelbase.dll")
	procVirtualAlloc2    = modkernelbase.NewProc("VirtualAlloc2")
	procMapViewOfFile3   = modkernelbase.NewProc("MapViewOfFile3")
	procUnmapViewOfFile2 = modkernelbase.NewProc("UnmapViewOfFile2")
)

func ceilG(n int64) int64 {
	return (n + allocGranularity - 1) &^ (allocGranularity - 1)
}

// winMapping tracks one data-file reservation's view layout. viewStarts
// holds each live view's base offset in ascending order; coverage is
// the mapped extent [0, coverage) and always equals the (G-aligned)
// file length.
type winMapping struct {
	base        uintptr
	reservation int64 // G-rounded placeholder size
	coverage    int64
	viewStarts  []int64
}

var (
	winMapsMu sync.Mutex
	winMaps   = map[uintptr]*winMapping{}
)

// findPlaceholderAPIs verifies the kernelbase exports exist so an
// older windows (pre-10 1803 / Server 2016) gets a clean error from
// Open instead of a LazyProc panic on first call.
func findPlaceholderAPIs() error {
	for _, p := range []*windows.LazyProc{procVirtualAlloc2, procMapViewOfFile3, procUnmapViewOfFile2} {
		if err := p.Find(); err != nil {
			return fmt.Errorf("pager: windows placeholder mapping requires Windows 10 1803+ / Server 2019+: %w", err)
		}
	}
	return nil
}

func fileLength(fd uintptr) (int64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(fd), &info); err != nil {
		return 0, fmt.Errorf("pager: GetFileInformationByHandle: %w", err)
	}
	return int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow), nil
}

// mapExtent maps [from, to) of the file (G-aligned, within the
// placeholder) as a read-only view. The section object is closed
// immediately — the view keeps it alive.
func (wm *winMapping) mapExtent(fd uintptr, from, to int64) error {
	// Split the remaining placeholder so [from, to) is its own
	// placeholder block (MapViewOfFile3 must replace an entire
	// placeholder). No split needed when the view reaches the
	// reservation end. Every failure exit AFTER a successful split
	// must re-coalesce [from, reservation) — exactly two placeholders
	// on those paths (to < reservation there) — so the fragmentation
	// invariant survives a transient map failure and a later retry or
	// growth is not permanently wedged.
	split := false
	if to < wm.reservation {
		if err := windows.VirtualFree(wm.base+uintptr(from), uintptr(to-from),
			windows.MEM_RELEASE|memPreservePlaceholder); err != nil {
			return fmt.Errorf("pager: split placeholder [%d,%d): %w", from, to, err)
		}
		split = true
	}
	unsplit := func(cause error) error {
		if !split {
			return cause
		}
		if err := windows.VirtualFree(wm.base+uintptr(from),
			uintptr(wm.reservation-from),
			windows.MEM_RELEASE|memCoalescePlaceholder); err != nil {
			return fmt.Errorf("pager: re-coalesce after failed map [%d,%d): %w (map failure: %v)",
				from, wm.reservation, err, cause)
		}
		return cause
	}
	section, err := windows.CreateFileMapping(windows.Handle(fd), nil,
		windows.PAGE_READONLY, uint32(uint64(to)>>32), uint32(uint64(to)&0xFFFFFFFF), nil)
	if err != nil {
		return unsplit(fmt.Errorf("pager: CreateFileMapping to %d: %w", to, err))
	}
	defer windows.CloseHandle(section)
	addr, _, callErr := procMapViewOfFile3.Call(
		uintptr(section),
		uintptr(windows.CurrentProcess()),
		wm.base+uintptr(from),
		uintptr(uint64(from)),
		uintptr(to-from),
		memReplacePlaceholder,
		windows.PAGE_READONLY,
		0, 0)
	if addr == 0 {
		return unsplit(fmt.Errorf("pager: MapViewOfFile3 [%d,%d): %w", from, to, callErr))
	}
	wm.viewStarts = append(wm.viewStarts, from)
	wm.coverage = to
	return nil
}

// unmapFrom unmaps every view whose extent intersects [from, ∞),
// preserving the placeholders. Views partition [0, coverage) in
// viewStarts order, so popping from the end keeps the layout
// invariant: after each pop, coverage is the popped view's start.
// Coverage may end below `from` when a view straddled it — the
// caller remaps the surviving prefix after truncation.
func (wm *winMapping) unmapFrom(from int64) error {
	// Placeholder count in [newCoverage, reservation) after the loop:
	// one per popped view, plus the pre-existing tail placeholder if
	// coverage was below the reservation. Coalescing is only invoked
	// when there are ≥ 2 — MEM_COALESCE_PLACEHOLDERS over a single
	// placeholder is undocumented and never needed.
	placeholders := 0
	if wm.coverage < wm.reservation {
		placeholders = 1
	}
	for len(wm.viewStarts) > 0 {
		last := len(wm.viewStarts) - 1
		start := wm.viewStarts[last]
		if wm.coverage <= from { // last view ends at coverage
			break
		}
		r1, _, callErr := procUnmapViewOfFile2.Call(
			uintptr(windows.CurrentProcess()),
			wm.base+uintptr(start),
			memPreservePlaceholder)
		if r1 == 0 {
			return fmt.Errorf("pager: UnmapViewOfFile2 at %d: %w", start, callErr)
		}
		wm.viewStarts = wm.viewStarts[:last]
		wm.coverage = start
		placeholders++
	}
	// Re-establish the fragmentation invariant: coalesce
	// [coverage, reservation) back into a single placeholder so a
	// later mapExtent's split precondition holds. Failure surfaces —
	// silent fragmentation would make every later growth past this
	// point fail.
	if placeholders >= 2 {
		if err := windows.VirtualFree(wm.base+uintptr(wm.coverage),
			uintptr(wm.reservation-wm.coverage),
			windows.MEM_RELEASE|memCoalescePlaceholder); err != nil {
			return fmt.Errorf("pager: coalesce placeholders [%d,%d): %w",
				wm.coverage, wm.reservation, err)
		}
	}
	return nil
}

func mmapRO(file uintptr, reservationBytes int64) ([]byte, error) {
	if err := findPlaceholderAPIs(); err != nil {
		return nil, err
	}
	res := ceilG(reservationBytes)
	r1, _, callErr := procVirtualAlloc2.Call(
		uintptr(windows.CurrentProcess()),
		0,
		uintptr(res),
		windows.MEM_RESERVE|memReservePlaceholder,
		windows.PAGE_NOACCESS,
		0, 0)
	if r1 == 0 {
		return nil, fmt.Errorf("pager: VirtualAlloc2 reserve %d: %w", res, callErr)
	}
	wm := &winMapping{base: r1, reservation: res}
	// Establishment retry: a writer's shrink can land between our
	// length stat and the section creation (the writer holds zero
	// views during its truncate), failing the map with a
	// section-beyond-EOF error. Re-stat and retry — once our section
	// exists the file cannot shrink under us, so a success is
	// consistent by construction.
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		var fileLen int64
		fileLen, err = fileLength(file)
		if err != nil {
			break
		}
		if fileLen%allocGranularity != 0 {
			// Foreign (unix-created) file: align once. A read-only
			// handle cannot — the documented degradation — and a real
			// I/O error surfaces as itself.
			if terr := windows.Ftruncate(windows.Handle(file), ceilG(fileLen)); terr != nil {
				err = fmt.Errorf("pager: windows mapping requires a %d-aligned file length; aligning %d → %d failed (read-only handle, or I/O error): %w",
					allocGranularity, fileLen, ceilG(fileLen), terr)
				break
			}
			fileLen = ceilG(fileLen)
		}
		cover := min(fileLen, res)
		if cover == 0 {
			err = nil
			break
		}
		if err = wm.mapExtent(file, 0, cover); err == nil {
			break
		}
	}
	if err != nil {
		// mapExtent re-coalesced on its failure exits; release the
		// whole reservation (best-effort coalesce first for safety).
		_ = windows.VirtualFree(r1, uintptr(res),
			windows.MEM_RELEASE|memCoalescePlaceholder)
		_ = windows.VirtualFree(r1, 0, windows.MEM_RELEASE)
		return nil, err
	}
	winMapsMu.Lock()
	winMaps[r1] = wm
	winMapsMu.Unlock()
	return unsafe.Slice(addrToBytePtr(r1), reservationBytes), nil
}

// addrToBytePtr reinterprets a kernel-returned mapping address as a
// byte pointer. The address names kernel-established mapped memory —
// never a Go object the GC tracks — so the conversion is safe; the
// reinterpretation through the local's address is the form vet's
// unsafeptr heuristic accepts for exactly this case.
func addrToBytePtr(addr uintptr) *byte {
	return *(**byte)(unsafe.Pointer(&addr))
}

// mprotectRO is a no-op: every view is created PAGE_READONLY, so the
// belt-and-suspenders downgrade the unix path performs is structural
// here.
func mprotectRO(b []byte) error { return nil }

func munmap(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	base := uintptr(unsafe.Pointer(&b[0]))
	winMapsMu.Lock()
	wm := winMaps[base]
	delete(winMaps, base)
	winMapsMu.Unlock()
	if wm == nil {
		return fmt.Errorf("pager: munmap of unknown mapping %#x", base)
	}
	if err := wm.unmapFrom(0); err != nil {
		return err
	}
	// unmapFrom's coalesce left the whole reservation as one
	// placeholder; one release frees it.
	return windows.VirtualFree(wm.base, 0, windows.MEM_RELEASE)
}

// mmapEnsureCoverage extends the mapping's file-backed views to cover
// size bytes (G-rounded, capped at the reservation). The file length
// must already be ≥ the rounded size — the grow path truncates first,
// and attach-time callers observe a same-OS peer's G-aligned length.
func mmapEnsureCoverage(m []byte, file uintptr, size int64) error {
	if len(m) == 0 {
		return nil
	}
	base := uintptr(unsafe.Pointer(&m[0]))
	winMapsMu.Lock()
	defer winMapsMu.Unlock()
	wm := winMaps[base]
	if wm == nil {
		return fmt.Errorf("pager: ensure coverage of unknown mapping %#x", base)
	}
	target := min(ceilG(size), wm.reservation)
	if target <= wm.coverage {
		return nil
	}
	return wm.mapExtent(file, wm.coverage, target)
}

// mmapPrepareShrink unmaps the ENTIRE local mapping ahead of a
// shrinking truncation: windows refuses SetEndOfFile while ANY view
// of the file exists — not merely views past the new EOF (pinned
// empirically by the first windows soak run of the lifecycle test).
// The caller MUST call mmapEnsureCoverage after truncating to remap
// [0, target); the placeholder keeps the base address stable, so
// every borrowed page pointer is valid again after the remap. Reader
// safety is the caller's shrink gate: no live reader observes the
// unmapped window.
func mmapPrepareShrink(m []byte, file uintptr, size int64) error {
	if len(m) == 0 {
		return nil
	}
	base := uintptr(unsafe.Pointer(&m[0]))
	winMapsMu.Lock()
	defer winMapsMu.Unlock()
	wm := winMaps[base]
	if wm == nil {
		return fmt.Errorf("pager: shrink of unknown mapping %#x", base)
	}
	if ceilG(size) >= wm.coverage {
		return nil // not a shrink of the mapped extent — nothing to unmap
	}
	return wm.unmapFrom(0)
}

// platformTruncate keeps windows data-file lengths G-aligned: every
// truncation rounds up to the allocation granularity so view layout
// constraints (whole-G views, G-aligned offsets) always hold. The
// pager's page accounting keeps the un-rounded size; the slack pages
// sit above HighWaterMark and are never referenced.
func platformTruncate(f *os.File, size int64) error {
	return f.Truncate(ceilG(size))
}
