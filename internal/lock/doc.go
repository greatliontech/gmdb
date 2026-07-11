// Package lock owns the gmdb lock file: the transient `<dbname>.lock`
// companion mmap'd by every process sharing a database, holding the
// cross-process write-lock state (a single flock'd region) and the
// reader-table.
//
// This package exposes the lock-file lifecycle
// (create-on-first-open, validate-or-recreate on UUID mismatch, mmap
// MAP_SHARED) plus the typed shared-memory accessors for the
// header and reader-slot fields. The flock goroutine, heartbeat
// goroutine, stale-writer recovery, and reader-slot acquire/release
// build on this surface.
//
// Atomic discipline. Every read/write of a shared-memory uint64 goes
// through the function-based sync/atomic helpers in this package —
// typed atomics are not used because the memory backs a MAP_SHARED
// region visible across processes, which is outside Go's memory
// model. See cross-process.md §Atomic Operations Convention.
//
// Layout. The on-disk layout is the structs.HostLayout shape of
// LockFileHeader (HeaderSize bytes) followed by MaxReaders contiguous
// ReaderSlot entries (SlotSize bytes each). The mmap reservation equals
// HeaderSize + SlotSize×MaxReaders exactly — per cross-process.md
// the size is fixed at lock-file creation and never resized.
package lock
