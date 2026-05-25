package gmdb

import "fmt"

// Index is a handle for querying a declared index on a Keyspace or
// SetKeyspace. Returned by Keyspace.Index(name) /
// SetKeyspace.Index(name). The query surface (Lookup / LookupKeys /
// Range / Prefix / Get) lands at chunk 7.7; chunk 7.6 ships only
// the Stats / Err surface, plus the handle plumbing.
//
// Index handles are not safe for concurrent use by multiple
// goroutines. Each call to ks.Index(name) returns a fresh handle
// (or, for chunk-7.6 implementations, the same handle bound to the
// same pinnedIndex — overlapping iterators on one handle race per
// api-surface.md §Index Lookup API).
type Index struct {
	ks       *Keyspace
	sks      *SetKeyspace // nil iff ks != nil
	pinned   *pinnedIndex
	err      error
}

// IndexStats is the persistent count + tree statistics for an index.
// Returned by Index.Stats(). The Count is sourced from the
// in-memory pinnedIndex (updated eagerly by chunk-7.6 atomic
// Put/Delete); the tree depth/page count statistics land at chunk
// 7.7 (depth requires walking the index data tree).
type IndexStats struct {
	// Count is the number of entries in the index. O(1) — read
	// from the in-memory pinnedIndex, kept in sync with the on-
	// disk registry entry via flushIndexRegistry at Tx.Commit.
	Count uint64
	// TreeDepth is the index data B+tree depth. Zero at chunk
	// 7.6 (will be populated at chunk 7.7 alongside the Lookup
	// API which already walks the tree).
	TreeDepth int
}

// Stats returns the index's persistent count + B+tree statistics.
// Chunk-7.6 surface: Count is populated; TreeDepth is zero
// (deferred to chunk 7.7).
func (idx *Index) Stats() (IndexStats, error) {
	if idx.err != nil {
		return IndexStats{}, idx.err
	}
	return IndexStats{
		Count: idx.pinned.count,
	}, nil
}

// Err returns the first error encountered during the last sequence
// returned by Lookup / Range / Prefix. Chunk-7.6 surface: only
// returns the handle-construction error (e.g. ErrIndexNotFound)
// since the lookup API is not yet wired.
func (idx *Index) Err() error { return idx.err }

// Index returns a query handle for the named index on this
// Keyspace. Returns ErrIndexNotFound if no index with that name is
// declared. The handle is valid for the lifetime of the owning
// transaction; subsequent operations on this Keyspace's parent
// tx after Commit/Rollback will return ErrTxClosed.
func (ks *Keyspace) Index(name string) (*Index, error) {
	if ks.dead {
		return nil, ErrKeyspaceClosed
	}
	if name == "" {
		return nil, ErrKeyEmpty
	}
	p, ok := ks.indexes[name]
	if !ok {
		return nil, fmt.Errorf("gmdb: index %q on keyspace %q: %w",
			name, ks.name.Value(), ErrIndexNotFound)
	}
	return &Index{ks: ks, pinned: p}, nil
}

// Index returns a query handle for the named index on this
// SetKeyspace. Mirror of Keyspace.Index.
func (sks *SetKeyspace) Index(name string) (*Index, error) {
	if sks.dead {
		return nil, ErrKeyspaceClosed
	}
	if name == "" {
		return nil, ErrKeyEmpty
	}
	p, ok := sks.indexes[name]
	if !ok {
		return nil, fmt.Errorf("gmdb: index %q on keyspace %q: %w",
			name, sks.name.Value(), ErrIndexNotFound)
	}
	return &Index{sks: sks, pinned: p}, nil
}
