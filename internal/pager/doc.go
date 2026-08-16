// Package pager implements the gmdb pager: the unified read+write page
// access layer, the slab-based copy-on-write path, the freespace state
// machine (allocation bitmap + retired page log + loose-page set + tail
// refund), the read-only mmap of the data file, the file-format machinery
// (grow/shrink via ftruncate), and the commit pipeline (pwrite ordering
// dirty data → bitmap → fdatasync → meta → fdatasync per SyncMode).
//
// The pager owns the file handle and the mmap, and it owns its two
// on-disk formats outright: the meta page (meta.go) and the RPL
// segment (rplformat.go) — no other layer reads or writes them. It
// depends downward on internal/page (the shared wire/header base and
// the node formats) and internal/bitmap (the allocation bitmap data
// structure). It does not depend on internal/btree or any higher
// subsystem; the btree operates on top of the pager.
//
// Platform mmap/madvise syscall shims live in build-tagged siblings:
// mmap_unix.go (linux, darwin, freebsd) and mmap_other.go. No
// platform-conditional code lives in the commit path itself.
package pager
