package pager

import "os"

// FileOps is the pager's seam over the durability-critical file operations
// on the data file: the pwrites that publish pages, the truncations that
// grow/shrink the file, the fdatasync barrier, and the reads the recovery
// path uses. Every such call in the pager routes through this interface
// rather than touching *os.File directly, so a test can substitute a
// fault-injecting or write-recording implementation.
//
// The seam is deliberately per-Pager (a struct field, set at construction),
// never a package-global hook table: parallel tests each carry their own
// FileOps and cannot perturb one another. mmap / madvise / the raw fd are
// NOT part of this interface — the writer never writes through the
// read-only mmap (mmap-strategy.md), so the durability seam is exactly the
// pwrite + fdatasync + truncate + read set. The Pager keeps its *os.File
// for the mmap and fd; a fault FileOps wraps that same real file, faulting
// or recording writes while reads still fault in through the real mapping.
//
// The production implementation is osFileOps, a thin forward to *os.File.
type FileOps interface {
	WriteAt(p []byte, off int64) (int, error)
	ReadAt(p []byte, off int64) (int, error)
	Truncate(size int64) error
	// Fdatasync flushes the file's data (and the size metadata needed to
	// read it back) to stable storage. The production path uses fdatasync
	// on Linux, fcntl(F_FULLFSYNC) on darwin (subject to the noFullFsync
	// opt-down), and a full fsync elsewhere (fdatasync_*.go).
	Fdatasync() error
}

// osFileOps is the production FileOps: a direct forward to the underlying
// *os.File plus the platform barrier policy. The seam is transparent in
// production — with osFileOps installed the pager's I/O is byte-identical
// to a direct *os.File call path; noFullFsync only selects which barrier
// call Fdatasync issues on darwin (Options.NoFullFsync).
type osFileOps struct {
	f           *os.File
	noFullFsync bool
}

func (o osFileOps) WriteAt(p []byte, off int64) (int, error) { return o.f.WriteAt(p, off) }
func (o osFileOps) ReadAt(p []byte, off int64) (int, error)  { return o.f.ReadAt(p, off) }
func (o osFileOps) Truncate(size int64) error                { return platformTruncate(o.f, size) }
func (o osFileOps) Fdatasync() error                         { return syncBarrier(o.f, o.noFullFsync) }

// SyncBarrier issues the platform durability barrier on a file outside
// any Pager: artifact files (CopyTo destinations) and directory handles
// (dirent durability) route here so the darwin full-flush policy
// (durability.md §Platform sync primitives) covers every barrier the
// engine issues, not only the pager's. noFullFsync mirrors
// Options.NoFullFsync; on non-darwin platforms it is ignored.
func SyncBarrier(f *os.File, noFullFsync bool) error {
	return syncBarrier(f, noFullFsync)
}

// SyncDirBarrier is the directory-entry durability barrier: fsync(2) —
// the documented dirent ritual; fdatasync's data-only contract is
// defined for file data — upgraded to fcntl(F_FULLFSYNC) on darwin
// under the same Options.NoFullFsync policy as SyncBarrier.
func SyncDirBarrier(d *os.File, noFullFsync bool) error {
	return syncDirBarrier(d, noFullFsync)
}
