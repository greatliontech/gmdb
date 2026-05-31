//go:build linux

package pager

import (
	"errors"

	"golang.org/x/sys/unix"
)

// madvise hints are advisory: per mmap-strategy.md they are a silent
// no-op on kernels that lack the requested advice. A kernel that does
// not recognise an advice value returns EINVAL; a build/runtime without
// the syscall returns ENOSYS. Both mean "unsupported here" — swallow
// them so an opt-in tuning knob never turns into an Open / read-close
// failure on an older kernel. Any other errno (e.g. EIO during a
// MADV_POPULATE_READ prefault) is a real condition and is returned.
//
// EINVAL also flags a bad range (unaligned address, non-page-multiple
// length, out-of-mapping). The three callers below pass only
// page-aligned (mmap base + id*PageSize offsets), page-multiple,
// in-mapping ranges (each clamps to len(b)), so EINVAL here can only
// mean "advice unsupported". A future caller that breaks that
// precondition would have its range error silently eaten.
func tolerateUnsupportedMadvise(err error) error {
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		return nil
	}
	return err
}

// madvisePopulateRead prefaults b[:length] into the OS page cache via
// MADV_POPULATE_READ (Linux 5.14+). length is clamped to len(b); a
// non-positive length is a no-op.
func madvisePopulateRead(b []byte, length int) error {
	if length <= 0 {
		return nil
	}
	if length > len(b) {
		length = len(b)
	}
	return tolerateUnsupportedMadvise(unix.Madvise(b[:length], unix.MADV_POPULATE_READ))
}

// madviseHugePage hints transparent-huge-page backing on the whole
// mapping via MADV_HUGEPAGE (Linux). No-op on an empty mapping.
func madviseHugePage(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return tolerateUnsupportedMadvise(unix.Madvise(b, unix.MADV_HUGEPAGE))
}

// madviseCold hints that b[off:off+length] may be reclaimed under
// memory pressure via MADV_COLD (Linux 5.4+). The range is clamped to
// the mapping; a non-positive or out-of-range range is a no-op.
func madviseCold(b []byte, off, length int) error {
	if off < 0 || length <= 0 || off >= len(b) {
		return nil
	}
	if off+length > len(b) {
		length = len(b) - off
	}
	return tolerateUnsupportedMadvise(unix.Madvise(b[off:off+length], unix.MADV_COLD))
}
