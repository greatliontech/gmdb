// Package pager implements the gmdb pager: the unified read+write page
// access layer, the slab-based copy-on-write path, the freespace state
// machine (allocation bitmap + retired page log + loose-page set + tail
// refund), the read-only mmap of the data file, the file-format machinery
// (grow/shrink via ftruncate), and the commit pipeline (pwrite ordering
// dirty data → bitmap → fdatasync → meta → fdatasync per SyncMode).
//
// The pager owns the file handle and the mmap. It depends downward on
// internal/page (for page-format encoders) and internal/bitmap (for the
// allocation bitmap data structure). It does not depend on internal/btree
// or any higher subsystem; the btree operates on top of the pager.
//
// Platform mmap/madvise syscall shims live in build-tagged siblings:
// mmap_linux.go, mmap_darwin.go, mmap_freebsd.go. No platform-conditional
// code lives in the commit path itself.
package pager
