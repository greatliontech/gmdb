package gmdb

// DBStats is a point-in-time snapshot of database-level metrics
// (api-surface.md §Statistics). All values are for metrics / health
// diagnostics only — never synchronization barriers.
//
// Concurrency: Stats() snapshots the handle's pager / coord / meta under
// db.mu. The pager-derived allocator fields (FreePages, RetiredPages,
// FileSize, SlabBytes) reflect THIS process's pager and obey the pager's
// single-threaded contract — read them from the goroutine that owns the
// write transaction, or when no write transaction is in flight in this
// process. ActiveReaders is an explicitly non-atomic cluster-wide scan.
type DBStats struct {
	// FreePages is the count of bitmap-free data pages immediately
	// available to allocation without reclamation or file growth. In
	// pages. 0 on a read-only handle (the allocator bitmap is not
	// loaded).
	FreePages uint64

	// RetiredPages is the count of page entries pinned in the committed
	// RPL chain — freed by prior transactions but not yet reclaimed
	// (gated by the oldest live reader / last checkpoint). In pages. 0
	// on a read-only handle.
	RetiredPages uint64

	// FileSize is the current on-disk size of the data file, in BYTES
	// (the one byte-valued field here; the rest are page counts).
	FileSize uint64

	// MinSize / MaxSize are the file-growth floor / ceiling from the
	// active meta, in pages (matching Options' page model). MaxSize is
	// immutable after creation.
	MinSize uint64
	MaxSize uint64

	// HighWaterMark is the first never-allocated page id — the count of
	// pages the file has ever grown to use. In pages.
	HighWaterMark uint64

	// ActiveReaders is a non-atomic scan of the lock-file reader table
	// (cluster-wide — every attached process's readers). The count can
	// be off by ±N for N reader transitions in flight during the scan.
	// Use for metrics and health diagnostics only — never as a
	// synchronization barrier ("ActiveReaders == 0" does NOT imply no
	// reads are starting). 0 on a read-only handle that fell back to
	// lock-free reads (no lock file).
	ActiveReaders int

	// MaxReaders is the reader-table capacity (lock-file header). Falls
	// back to the configured Options.MaxReaders when no lock file is
	// open (a read-only handle on read-only media).
	MaxReaders uint32

	// SlabBytes reports slab usage for THIS PROCESS's current write
	// transaction (0 when no write txn is open in this process).
	// Cross-process writer slab usage is not visible from any one DB
	// handle, and aggregate cluster-wide slab usage is not tracked.
	SlabBytes int64
}

// Stats returns a database-level metrics snapshot (api-surface.md
// §Statistics). See DBStats for the per-field units and the concurrency
// contract. Cheap: no tree walk, no flock — at most one lock-free scan
// of the reader table for ActiveReaders.
func (db *DB) Stats() DBStats {
	db.mu.Lock()
	coord := db.coord
	lockFile := db.lockFile
	pgr := db.pgr
	meta := db.currentMeta
	db.mu.Unlock()

	s := DBStats{
		MinSize:       meta.MinSize,
		MaxSize:       meta.MaxSize,
		HighWaterMark: meta.HighWaterMark,
		MaxReaders:    db.opts.MaxReaders,
	}
	if pgr != nil {
		s.FreePages = pgr.NumFreePages()
		s.RetiredPages = pgr.RPLPageCount()
		s.FileSize = uint64(pgr.FileSize())
		s.SlabBytes = int64(pgr.DirtyBytes())
	}
	if lockFile != nil {
		s.MaxReaders = lockFile.MaxReaders()
	}
	if coord != nil {
		s.ActiveReaders = coord.CountActiveReaders()
	}
	return s
}
