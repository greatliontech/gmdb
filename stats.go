package gmdb

import (
	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
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

// treePageStats tallies B+tree page kinds and the maximum descent depth.
type treePageStats struct {
	depth         int // number of levels (root→leaf); 0 for an empty tree
	branchPages   uint64
	leafPages     uint64
	overflowPages uint64
}

// walkTreePageStats walks the B+tree rooted at root (a no-op for root ==
// 0) and tallies branch / leaf / overflow page counts and the max
// branch-or-leaf descent depth. Overflow pages are counted but do NOT
// contribute to depth (Walk reports them at their leaf's depth + 1). For
// a SetKeyspace the walk recurses into nested set-member subtrees, so
// the depth is the deepest path including nesting. O(tree pages).
func walkTreePageStats(pr btree.PageReader, cfg page.Config, root, hwm uint64) (treePageStats, error) {
	var s treePageStats
	maxLevel := -1
	err := btree.Walk(pr, cfg, root, hwm, func(_ uint64, kind btree.PageKind, depth int) error {
		switch kind {
		case btree.PageKindBranch:
			s.branchPages++
			if depth > maxLevel {
				maxLevel = depth
			}
		case btree.PageKindLeaf:
			s.leafPages++
			if depth > maxLevel {
				maxLevel = depth
			}
		case btree.PageKindOverflow:
			s.overflowPages++
		}
		return nil
	})
	if err != nil {
		return treePageStats{}, mapBtreeErr(err)
	}
	s.depth = maxLevel + 1 // empty tree: maxLevel stays -1 ⇒ depth 0
	return s, nil
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
		Depth:         ts.depth,
		BranchPages:   ts.branchPages,
		LeafPages:     ts.leafPages,
		OverflowPages: ts.overflowPages,
		Entries:       kc.desc.Count,
		IndexCount:    indexCount,
	}, nil
}
