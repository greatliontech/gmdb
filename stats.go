package gmdb

import (
	"time"

	"github.com/greatliontech/gmdb/internal/btree"
	"github.com/greatliontech/gmdb/internal/page"
	"github.com/greatliontech/gmdb/internal/verify"
)

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
	// (gated by the oldest live reader / the anchored durable epoch,
	// free-space.md §RPL Reclamation). In pages. 0 on a read-only
	// handle.
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

	// RPLCorruptSegments counts RPL-segment quarantine occurrences this
	// process saw during reclamation — a decode failure (free-space.md
	// §RPL Reclamation). Non-zero means a torn RPL segment was skipped:
	// its pages leak (bounded to that segment) until Check()/Repair
	// reclaims them, and the file may extend past genuinely-free space.
	// Distinguishes corruption from genuine capacity — an ErrDBFull
	// while this is non-zero is a corruption symptom, not a full DB.
	RPLCorruptSegments uint64
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
		s.RPLCorruptSegments = pgr.RPLCorruptCount()
	}
	if lockFile != nil {
		s.MaxReaders = lockFile.MaxReaders()
	}
	if coord != nil {
		s.ActiveReaders = coord.CountActiveReaders()
	}
	return s
}

// statsHWM returns the page-id bound for a stats tree walk: a write
// transaction's pager tracks the live HighWaterMark (which grows as the
// tx allocates), whereas a read transaction's pager is a read-only
// snapshot whose HighWaterMark is carried on prevMeta (the snapshot
// meta). Walk rejects any child pointer >= this bound, so it must cover
// every page the (in-tx or committed) tree can reference.
func (tx *Tx) statsHWM() uint64 {
	if tx.writable {
		return tx.pgr.HighWaterMark()
	}
	return tx.prevMeta.HighWaterMark
}

// KeyspaceStats is a point-in-time snapshot of one keyspace's B+tree
// shape (api-surface.md §Statistics). The page counts and depth come
// from an O(tree) walk of the keyspace's data tree; Entries is the O(1)
// descriptor count.
type KeyspaceStats struct {
	// Depth is the number of B+tree levels from root to leaf (1 for a
	// single-leaf tree, 0 for an empty keyspace). For a SetKeyspace it
	// is the deepest path including nested set-member subtrees.
	Depth int

	// BranchPages / LeafPages / OverflowPages count the pages reachable
	// from the data tree by kind (overflow = the pages of large values'
	// overflow runs). For a SetKeyspace these include the nested
	// set-member subtree pages.
	BranchPages   uint64
	LeafPages     uint64
	OverflowPages uint64

	// Entries is the number of key-value pairs (for a SetKeyspace, the
	// total members across all value sets) — the descriptor's Count.
	Entries uint64

	// IndexCount is the number of secondary indexes registered on the
	// keyspace (counted from the on-disk index registry, independent of
	// how many were opened with an IndexDecl this transaction).
	IndexCount int
}

// walkTreePageStats adapts verify.WalkTreePageStats to the public
// error surface: raw btree walk errors map to the exported
// sentinels exactly once, here, for every stats consumer
// (KeyspaceStats and IndexStats alike).
func walkTreePageStats(pr btree.PageReader, cfg page.Config, root, hwm uint64) (verify.TreePageStats, error) {
	ts, err := verify.WalkTreePageStats(pr, cfg, root, hwm)
	if err != nil {
		return verify.TreePageStats{}, mapBtreeErr(err)
	}
	return ts, nil
}

// Stats returns the keyspace's B+tree statistics, or ErrKeyspaceClosed
// when the keyspace was DeleteKeyspace'd in this tx (or the tx-state
// error from requireOpen). Defined on the shared keyspaceCore so both
// *Keyspace and *SetKeyspace expose it via embedding. The walk observes
// the handle's transactional view (in-tx mutations on a write tx; the
// snapshot on a read tx).
func (kc *keyspaceCore) Stats() (KeyspaceStats, error) {
	if err := kc.tx.requireOpen(false); err != nil {
		return KeyspaceStats{}, err
	}
	if kc.dead {
		return KeyspaceStats{}, ErrKeyspaceClosed
	}
	pr := kc.tx.pgr
	cfg := pr.Config()
	hwm := kc.tx.statsHWM()

	ts, err := walkTreePageStats(pr, cfg, kc.desc.Root, hwm)
	if err != nil {
		return KeyspaceStats{}, err
	}

	// IndexCount: the index registry tree maps index-name → registry
	// entry, so its key count is the number of registered indexes.
	var indexCount int
	if kc.desc.IndexRegistryRoot != 0 {
		if err := btree.WalkKV(pr, cfg, kc.desc.IndexRegistryRoot, hwm, func(_, _ []byte) error {
			indexCount++
			return nil
		}); err != nil {
			return KeyspaceStats{}, mapBtreeErr(err)
		}
	}

	return KeyspaceStats{
		Depth:         ts.Depth,
		BranchPages:   ts.BranchPages,
		LeafPages:     ts.LeafPages,
		OverflowPages: ts.OverflowPages,
		Entries:       kc.desc.Count,
		IndexCount:    indexCount,
	}, nil
}

// TxStats is a point-in-time snapshot of one write transaction's
// activity counters (api-surface.md §Statistics). Read it from the
// goroutine that owns the transaction (a write tx is single-threaded);
// it may be read after Commit / Rollback — before the next Begin in this
// process resets the counters — to capture the final values (e.g.
// WrittenPages, which is only known at commit). A child transaction
// (BeginChild) shares the parent's pager, so its counters are the
// cumulative parent+child totals; only its Duration is child-scoped.
//
// Counting scope: Gets/Puts/Deletes count the named Keyspace /
// SetKeyspace Get / Put / Delete calls — not the range or value variants
// (DeleteRange, DeleteValue) or cursor scans. Splits/Merges count B+tree
// node structural events. The counts reflect *attempted* operations: an
// op that fails partway (and rolls its page state back via the internal
// savepoint) still leaves its increments in place, so on heavy error
// paths the counts can exceed the committed work.
type TxStats struct {
	// Page-allocator activity (counts, in pages).
	CowPages       uint64 // pages copied-on-write
	LoosePages     uint64 // pages that went loose (alloc+free within the tx)
	ReclaimedPages uint64 // RPL pages reclaimed to the free bitmap
	WrittenPages   uint64 // data + RPL + bitmap + meta pages pwritten at commit

	// SlabPeakBytes is the maximum slab usage observed during the
	// transaction's lifetime — useful for tuning MaxTxBufferBytes.
	// Rollback resets it to 0 (the rolled-back work is not
	// representative); Commit preserves it so the caller can read it
	// immediately after Commit.
	SlabPeakBytes int64

	// SpilledPages counts slab pages written out to their file
	// locations before commit because the transaction's working set
	// exceeded MaxTxBufferBytes — the spill threshold (pager-slab.md
	// §Slab Budget). Non-zero means the engine traded early pwrites
	// for bounded memory; a persistently large value suggests raising
	// MaxTxBufferBytes.
	SpilledPages uint64

	// B+tree operation counts.
	Gets    uint64 // keyspace / set-keyspace Get calls
	Puts    uint64 // keyspace / set-keyspace Put calls
	Deletes uint64 // keyspace / set-keyspace Delete calls
	Splits  uint64 // node splits (leaf or branch)
	Merges  uint64 // node merges (two siblings combined into one)

	// Index maintenance counts.
	IndexEntriesInserted uint64
	IndexEntriesDeleted  uint64
	IndexUniqueProbes    uint64

	// Duration is the mutation window — from Begin until Commit /
	// Rollback is *called* (it does not include the commit's pwrite /
	// fdatasync I/O, which runs after the window closes). While the tx
	// is still live it is the elapsed time so far.
	Duration time.Duration
}

// Stats returns this write transaction's activity-counter snapshot (see
// TxStats). Safe to call while the tx is open or after it is finalized
// (until the next Begin in this process). Must be read from the
// transaction's own goroutine — a write tx, and thus its pager, is
// single-threaded.
func (tx *Tx) Stats() TxStats {
	snap := tx.pgr.TxStatsSnapshot()
	dur := time.Since(tx.startTime)
	if !tx.endTime.IsZero() {
		dur = tx.endTime.Sub(tx.startTime)
	}
	return TxStats{
		CowPages:             snap.CowPages,
		LoosePages:           snap.LoosePages,
		ReclaimedPages:       snap.ReclaimedPages,
		WrittenPages:         snap.WrittenPages,
		SlabPeakBytes:        snap.SlabPeakBytes,
		SpilledPages:         snap.SpilledPages,
		Gets:                 snap.Gets,
		Puts:                 snap.Puts,
		Deletes:              snap.Deletes,
		Splits:               snap.Splits,
		Merges:               snap.Merges,
		IndexEntriesInserted: snap.IndexInserted,
		IndexEntriesDeleted:  snap.IndexDeleted,
		IndexUniqueProbes:    snap.IndexProbes,
		Duration:             dur,
	}
}
