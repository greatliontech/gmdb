//go:build !linux

package pager

// madvise hints are Linux-only (mmap-strategy.md): MADV_POPULATE_READ,
// MADV_HUGEPAGE, and MADV_COLD have no portable equivalent. On every
// other platform the hints are silent no-ops, exactly as the spec
// specifies for unsupported kernels.

func madvisePopulateRead(b []byte, length int) error { return nil }

func madviseHugePage(b []byte) error { return nil }

func madviseCold(b []byte, off, length int) error { return nil }
