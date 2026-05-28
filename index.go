package gmdb

import (
	"bytes"
	"fmt"
	"iter"

	"github.com/thegrumpylion/gmdb/internal/btree"
)

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
	ks     *Keyspace
	sks    *SetKeyspace // nil iff ks != nil
	pinned *pinnedIndex
	err    error

	// coverValue, when true, makes Lookup/Range/Prefix/Get return the
	// row value from the index entry's covering blob instead of
	// back-looking-up the row keyspace (typed full-row covering). Set
	// only by the typed layer (TypedKS.Index) for indexes whose covering
	// it recognizes as a full encoded-value column; default false ⇒
	// back-lookup, the behavior for every byte-layer and non-covering
	// index. Keyspace-only (a SetKeyspace index's value lives in its
	// compound PK, so there is no back-lookup to skip).
	coverValue bool
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

// rowRoot returns the row keyspace's current B+tree root (the
// back-lookup target) and the keyspace's name (for error reporting).
// Routes through ks or sks depending on which is non-nil.
func (idx *Index) rowRoot() (uint64, string) {
	if idx.ks != nil {
		return idx.ks.desc.Root, idx.ks.name.Value()
	}
	return idx.sks.desc.Root, idx.sks.name.Value()
}

// rowTx returns the owning tx for back-lookup routing.
func (idx *Index) rowTx() *Tx {
	if idx.ks != nil {
		return idx.ks.tx
	}
	return idx.sks.tx
}

// extractPKAndValue decodes an index entry into (pk_bytes,
// row_value). For a unique index, the value carries the PK +
// optional covering; for a non-unique index, the PK is in the
// key suffix and the value carries only covering bytes.
//
// row_value is the user-facing value, selected by index shape:
//
//   - Typed full-row covering (idx.coverValue=true, set by
//     TypedKS.Index for an index whose covering is the typed full-row
//     sentinel column): the covering blob's single column is decoded
//     and returned as the encoded V — the typed wrapper then runs
//     valEnc.Decode on it.
//   - Byte-API covering (len(decl.Covering) > 0, coverValue=false):
//     the raw encoded covering blob (NUL-escape multi-column tuple)
//     is returned verbatim. The caller decodes via the exported
//     DecodeCoveringTuple — per indexing.md §Covering Indexes /
//     api-surface.md §Index Lookup API "byte-API covering return".
//   - No covering: back-lookup against the parent keyspace's row
//     tree (idx.rowRoot()).
//
// For a SetKeyspace index (idx.sks != nil), routes to the
// SetKeyspace-aware path which yields (setKey, setValue) tuples
// instead of (rowKey, rowValue) per chunk 7.9. Covering is not
// returned for SetKeyspace indexes — set-keyspace.md §Indexes
// makes the (setKey, setValue) pair the natural query result
// (no back-lookup to save), and the byte-layer covering-return
// contract is Keyspace-only.
//
// Returns (nil, nil, true, nil) on a silent-skip case (back-
// lookup failed to find the PK — corruption signal per
// indexing.md §Lookup API §Intra-transaction consistency); the
// caller treats this as a missed entry and continues iteration
// without setting idx.err.
func (idx *Index) extractPKAndValue(indexKey, indexValue []byte) (pk, value []byte, skip bool, err error) {
	if idx.sks != nil {
		return idx.extractSetKeyspacePKAndValue(indexKey, indexValue)
	}
	// encodedCovering holds the index entry's covering blob (the
	// NUL-escaped covering tuple): the value suffix for a unique index,
	// the whole value for a non-unique one. Used by both covering-return
	// branches below; empty for an index whose IndexDecl declares no
	// Covering (the engine still stores it as empty per
	// indexEntryValue).
	var encodedCovering []byte
	if idx.pinned.decl.Unique {
		extractedPK, encCov, decErr := decodeUniqueIndexValue(indexValue)
		if decErr != nil {
			return nil, nil, false, fmt.Errorf("%w: %w", ErrCorrupted, decErr)
		}
		pk = make([]byte, len(extractedPK))
		copy(pk, extractedPK)
		encodedCovering = encCov
	} else {
		// Non-unique: PK is the last component of the encoded key; the
		// entry value IS the covering blob.
		cols, decErr := decodeIndexKey(indexKey)
		if decErr != nil {
			return nil, nil, false, fmt.Errorf("%w: index %q: %w", ErrCorrupted, idx.pinned.decl.Name, decErr)
		}
		if len(cols) == 0 {
			return nil, nil, false, fmt.Errorf("%w: index %q: non-unique key has zero columns", ErrCorrupted, idx.pinned.decl.Name)
		}
		pk = cols[len(cols)-1]
		encodedCovering = indexValue
	}
	// Typed full-row covering (indexing.md §Covering Indexes,
	// typed-keyspaces.md §Covering). The covering blob is a single
	// NUL-escaped column holding the encoded row value, so the value
	// is returned directly from the index entry — skipping the
	// back-lookup against the row keyspace. The TypedIndexQuery
	// wrapper then runs valEnc.Decode on the returned bytes.
	//
	// Gated by idx.coverValue, set only by the typed layer for indexes
	// it recognizes as full-row covering (isTypedCoverValueIndex —
	// exactly one covering column with the typedCoverValuePrefix
	// sentinel). The byte-API path below handles arbitrary covering
	// projections; default (no Covering declared) is back-lookup.
	if idx.coverValue {
		coverCols, decErr := decodeIndexKey(encodedCovering)
		if decErr != nil {
			return nil, nil, false, fmt.Errorf("%w: index %q covering: %w", ErrCorrupted, idx.pinned.decl.Name, decErr)
		}
		if len(coverCols) == 0 {
			return nil, nil, false, fmt.Errorf("%w: index %q covering: zero columns", ErrCorrupted, idx.pinned.decl.Name)
		}
		value = make([]byte, len(coverCols[0]))
		copy(value, coverCols[0])
		return pk, value, false, nil
	}
	// Byte-API covering return (indexing.md §Covering Indexes,
	// api-surface.md §Index Lookup API). For any index whose
	// IndexDecl.Covering is non-empty (and which is not a typed
	// full-row sentinel — that branch is above), Lookup returns the
	// encoded covering blob verbatim. The caller decodes via the
	// exported DecodeCoveringTuple to recover the extractor's
	// IndexEntry.Cover slice in declaration order. Skips the
	// back-lookup against the row keyspace per spec.
	//
	// Copy the slice — encodedCovering may alias the value buffer the
	// cursor will reuse (decodeUniqueIndexValue returns slices into
	// indexValue; non-unique sets encodedCovering = indexValue).
	if len(idx.pinned.decl.Covering) > 0 {
		value = make([]byte, len(encodedCovering))
		copy(value, encodedCovering)
		return pk, value, false, nil
	}
	// Back-lookup against the row keyspace.
	tx := idx.rowTx()
	rowRoot, _ := idx.rowRoot()
	if rowRoot == 0 {
		// Row keyspace is empty — silent-skip per spec.
		return nil, nil, true, nil
	}
	v, found, blErr := btree.Get(tx.pgr, tx.pgr.Config(), rowRoot, pk)
	if blErr != nil {
		return nil, nil, false, mapBtreeErr(blErr)
	}
	if !found {
		// Silent-skip per indexing.md §Lookup API.
		return nil, nil, true, nil
	}
	value = make([]byte, len(v))
	copy(value, v)
	return pk, value, false, nil
}

// Lookup returns (pk, value) pairs whose index columns equal the
// supplied tuple. Per indexing.md §Lookup API: exact match on
// **all** declared columns. Supplying fewer or more columns than
// the index declares sets idx.Err() to an ErrInvalidOptions wrap
// and yields nothing — this prevents the partial-cols-silently-
// widens-to-Prefix footgun the chunk-7.7 Round-1 H-1 review
// surfaced. Use Prefix for partial-cols semantics.
//
// For a unique index, yields at most one pair. For a non-unique
// index, yields every row whose extractor produced the matching
// column tuple.
//
// Value semantics (indexing.md §Covering Indexes):
//   - When the index's IndexDecl.Covering is non-empty, value is
//     the on-disk encoded covering tuple — decode via
//     DecodeCoveringTuple to recover per-column Cover bytes.
//   - When IndexDecl.Covering is empty, value is the row's stored
//     bytes via back-lookup against the row keyspace.
//
// Per indexing.md §Lookup API §Intra-transaction consistency:
// entries whose back-lookup against the row keyspace fails to
// find the PK are silently skipped (corruption signal — surfaced
// later via Check()). The caller observes the skip as "this
// entry didn't yield" without any error on idx.Err(). The
// covering-return path skips the back-lookup entirely so the
// silent-skip case does not apply.
//
// idx.Err() is reset at the start of each call (per api-surface.md
// §Index Lookup API "first error encountered during the **last**
// sequence's iteration"). The returned iter.Seq2 is bound to this
// *Index's transaction; concurrent iteration on the same handle
// races. For concurrent queries, call ks.Index(name) once per
// goroutine.
func (idx *Index) Lookup(cols ...[]byte) iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
		// Per-sequence Err reset (chunk-7.7 Round-1 M-2 fix).
		idx.err = nil
		if got, want := len(cols), len(idx.pinned.decl.Columns); got != want {
			idx.err = fmt.Errorf("gmdb: index %q Lookup: got %d cols, want %d (exact match on all declared columns): %w",
				idx.pinned.decl.Name, got, want, ErrInvalidOptions)
			return
		}
		if idx.pinned.root == 0 {
			return // empty index — no matches
		}
		colSlices := make([][]byte, len(cols))
		copy(colSlices, cols)
		encoded := encodeIndexKey(colSlices)
		tx := idx.rowTx()
		cfg := tx.pgr.Config()
		if idx.pinned.decl.Unique {
			val, found, err := btree.Get(tx.pgr, cfg, idx.pinned.root, encoded)
			if err != nil {
				idx.err = mapBtreeErr(err)
				return
			}
			if !found {
				return
			}
			pk, rowValue, skip, err := idx.extractPKAndValue(encoded, val)
			if err != nil {
				idx.err = err
				return
			}
			if skip {
				return
			}
			yield(pk, rowValue)
			return
		}
		// Non-unique: scan the prefix `encoded` (each on-disk key is
		// encoded || escapeColumn(pk) || 0x00 0x00). Stop when the
		// cursor key no longer has encoded as a prefix.
		idx.iteratePrefix(encoded, yield)
	}
}

// iteratePrefix scans the index data tree from the smallest key
// with the supplied prefix, yielding (pk, value) for each entry
// whose key starts with prefix. Stops at the first key without
// the prefix. Used by Lookup (non-unique) and Prefix.
func (idx *Index) iteratePrefix(prefix []byte, yield func([]byte, []byte) bool) {
	tx := idx.rowTx()
	cfg := tx.pgr.Config()
	mergeThreshold := tx.db.opts.MergeThreshold
	c := btree.NewCursor(tx.pgr, cfg, idx.pinned.root, mergeThreshold)
	for k, v := c.SeekGE(prefix); k != nil; k, v = c.Next() {
		if !bytes.HasPrefix(k, prefix) {
			break
		}
		// Copy the key out — c.Next() may invalidate the slice.
		keyCopy := make([]byte, len(k))
		copy(keyCopy, k)
		valCopy := make([]byte, len(v))
		copy(valCopy, v)
		pk, rowValue, skip, err := idx.extractPKAndValue(keyCopy, valCopy)
		if err != nil {
			idx.err = err
			return
		}
		if skip {
			continue
		}
		if !yield(pk, rowValue) {
			return
		}
	}
	if err := c.Err(); err != nil {
		idx.err = mapBtreeErr(err)
	}
}

// LookupKeys returns matching primary keys without back-lookup or
// covering decode. Iteration cost is O(matches) leaf scans only.
// Per indexing.md §Lookup API: LookupKeys does NOT probe the row
// keyspace, so it does not observe the silent-skip case — every
// index entry yields its raw PK, even when the row has somehow
// vanished. Use Check() for row/index consistency verification.
func (idx *Index) LookupKeys(cols ...[]byte) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		// Per-sequence Err reset (M-2 fix).
		idx.err = nil
		// Chunk-7.9 Round-1 H-1: LookupKeys on a SetKeyspace index
		// has no well-defined iter.Seq[[]byte] surface — the "PK"
		// is a compound (setKey, setValue) pair per set-keyspace.md
		// §Indexes on SetKeyspaces. Returning the raw compound bytes
		// would yield bytes the caller cannot interpret without
		// out-of-band knowledge of the compound encoding; returning
		// setKey-only or setValue-only would lose information. Use
		// Lookup (iter.Seq2) instead — it yields (setKey, setValue)
		// pairs cleanly.
		if idx.sks != nil {
			idx.err = fmt.Errorf("gmdb: index %q LookupKeys on SetKeyspace: use Lookup (iter.Seq2) for the compound (setKey, setValue) pair: %w",
				idx.pinned.decl.Name, ErrInvalidOptions)
			return
		}
		if got, want := len(cols), len(idx.pinned.decl.Columns); got != want {
			idx.err = fmt.Errorf("gmdb: index %q LookupKeys: got %d cols, want %d (exact match): %w",
				idx.pinned.decl.Name, got, want, ErrInvalidOptions)
			return
		}
		if idx.pinned.root == 0 {
			return
		}
		colSlices := make([][]byte, len(cols))
		copy(colSlices, cols)
		encoded := encodeIndexKey(colSlices)
		tx := idx.rowTx()
		cfg := tx.pgr.Config()
		mergeThreshold := tx.db.opts.MergeThreshold
		if idx.pinned.decl.Unique {
			val, found, err := btree.Get(tx.pgr, cfg, idx.pinned.root, encoded)
			if err != nil {
				idx.err = mapBtreeErr(err)
				return
			}
			if !found {
				return
			}
			pk, _, decErr := decodeUniqueIndexValue(val)
			if decErr != nil {
				idx.err = fmt.Errorf("%w: %w", ErrCorrupted, decErr)
				return
			}
			pkCopy := make([]byte, len(pk))
			copy(pkCopy, pk)
			yield(pkCopy)
			return
		}
		// Non-unique: cursor-walk prefix, extract PK from key suffix.
		c := btree.NewCursor(tx.pgr, cfg, idx.pinned.root, mergeThreshold)
		for k, _ := c.SeekGE(encoded); k != nil; k, _ = c.Next() {
			if !bytes.HasPrefix(k, encoded) {
				break
			}
			keyCopy := make([]byte, len(k))
			copy(keyCopy, k)
			cols, decErr := decodeIndexKey(keyCopy)
			if decErr != nil {
				idx.err = fmt.Errorf("%w: index %q: %w", ErrCorrupted, idx.pinned.decl.Name, decErr)
				return
			}
			if len(cols) == 0 {
				idx.err = fmt.Errorf("%w: index %q: non-unique key has zero columns", ErrCorrupted, idx.pinned.decl.Name)
				return
			}
			pk := cols[len(cols)-1]
			if !yield(pk) {
				return
			}
		}
		if err := c.Err(); err != nil {
			idx.err = mapBtreeErr(err)
		}
	}
}

// Range returns matches whose column tuple falls in [start, end)
// (start inclusive, end exclusive). A nil start = open lower
// bound; a nil end = open upper bound. Each tuple is a slice of
// per-column byte slices.
//
// Value semantics are the same as Lookup: the yielded value is
// the encoded covering tuple (decode via DecodeCoveringTuple)
// when IndexDecl.Covering is non-empty, otherwise the row's
// stored bytes via back-lookup. See indexing.md §Covering Indexes.
func (idx *Index) Range(start, end [][]byte) iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
		// Per-sequence Err reset (M-2 fix).
		idx.err = nil
		if idx.pinned.root == 0 {
			return
		}
		var startKey, endKey []byte
		if start != nil {
			startKey = encodeIndexKey(start)
		}
		if end != nil {
			endKey = encodeIndexKey(end)
		}
		tx := idx.rowTx()
		cfg := tx.pgr.Config()
		mergeThreshold := tx.db.opts.MergeThreshold
		c := btree.NewCursor(tx.pgr, cfg, idx.pinned.root, mergeThreshold)
		var k, v []byte
		if startKey != nil {
			k, v = c.SeekGE(startKey)
		} else {
			k, v = c.First()
		}
		for ; k != nil; k, v = c.Next() {
			if endKey != nil && bytes.Compare(k, endKey) >= 0 {
				break
			}
			keyCopy := make([]byte, len(k))
			copy(keyCopy, k)
			valCopy := make([]byte, len(v))
			copy(valCopy, v)
			pk, rowValue, skip, err := idx.extractPKAndValue(keyCopy, valCopy)
			if err != nil {
				idx.err = err
				return
			}
			if skip {
				continue
			}
			if !yield(pk, rowValue) {
				return
			}
		}
		if err := c.Err(); err != nil {
			idx.err = mapBtreeErr(err)
		}
	}
}

// Prefix returns matches whose leading columns equal the prefix.
// Equivalent to `Range(prefix, nextPrefix)` but the caller doesn't
// have to compute the upper bound. Same value semantics as Lookup
// — covering tuple when IndexDecl.Covering is non-empty, row
// bytes via back-lookup otherwise.
func (idx *Index) Prefix(leadingCols ...[]byte) iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
		// Per-sequence Err reset (M-2 fix).
		idx.err = nil
		if got, want := len(leadingCols), len(idx.pinned.decl.Columns); got > want {
			idx.err = fmt.Errorf("gmdb: index %q Prefix: got %d cols, want <= %d (leading-cols prefix): %w",
				idx.pinned.decl.Name, got, want, ErrInvalidOptions)
			return
		}
		if idx.pinned.root == 0 {
			return
		}
		prefixSlices := make([][]byte, len(leadingCols))
		copy(prefixSlices, leadingCols)
		encoded := encodeIndexKey(prefixSlices)
		idx.iteratePrefix(encoded, yield)
	}
}

// Get is shorthand for unique indexes: returns the single (pk,
// value) tuple matching cols, or ErrNotFound if no match. Returns
// ErrIndexNotUnique when called on a non-unique index. Same value
// semantics as Lookup — covering tuple when IndexDecl.Covering is
// non-empty (decode via DecodeCoveringTuple), row bytes via
// back-lookup otherwise.
func (idx *Index) Get(cols ...[]byte) (pk, value []byte, err error) {
	// Per-sequence Err reset (M-2 fix; Get isn't strictly a sequence,
	// but the handle's Err() is shared and a stale prior error
	// should not surface to a fresh Get).
	idx.err = nil
	if !idx.pinned.decl.Unique {
		return nil, nil, ErrIndexNotUnique
	}
	if got, want := len(cols), len(idx.pinned.decl.Columns); got != want {
		return nil, nil, fmt.Errorf("gmdb: index %q Get: got %d cols, want %d (exact match): %w",
			idx.pinned.decl.Name, got, want, ErrInvalidOptions)
	}
	if idx.pinned.root == 0 {
		return nil, nil, ErrNotFound
	}
	colSlices := make([][]byte, len(cols))
	copy(colSlices, cols)
	encoded := encodeIndexKey(colSlices)
	tx := idx.rowTx()
	cfg := tx.pgr.Config()
	val, found, getErr := btree.Get(tx.pgr, cfg, idx.pinned.root, encoded)
	if getErr != nil {
		return nil, nil, mapBtreeErr(getErr)
	}
	if !found {
		return nil, nil, ErrNotFound
	}
	gotPK, rowValue, skip, decErr := idx.extractPKAndValue(encoded, val)
	if decErr != nil {
		return nil, nil, decErr
	}
	if skip {
		// Silent-skip on Lookup is a no-yield; on Get it surfaces
		// as ErrNotFound (the entry exists in the index but the
		// row vanished — caller asked for the row, the row isn't
		// there).
		return nil, nil, ErrNotFound
	}
	return gotPK, rowValue, nil
}

// extractSetKeyspacePKAndValue decodes a SetKeyspace index entry
// into the per-set-member (setKey, setValue) pair (chunk 7.9
// SetKeyspace lookup contract; see api-surface.md §Index Lookup
// API). For SetKeyspace indexes, the iter.Seq2 yields
// (setKey, setValue) rather than (rowKey, rowValue): the
// "primary key" of an index entry is the compound (setKey,
// setValue) pair per set-keyspace.md §Indexes on SetKeyspaces.
//
// Decoding routes:
//   - Unique: uvarint-prefixed compound PK in the index value;
//     decodeUniqueIndexValue extracts the compound bytes, then
//     decodeSetKeyspaceCompoundPK splits on the 0x00 0x01
//     separator.
//   - Non-unique: compound PK lives in the index key suffix after
//     the column-tuple terminator; extractSetKeyspaceCompoundPKFromIndexKey
//     walks the key counting real 0x00 0x00 terminators.
//
// Silent-skip applies per indexing.md §Lookup API: if the
// (setKey, setValue) pair has been removed from the SetKeyspace
// between index Put and Lookup (engine bug / external corruption),
// the iterator skips the entry without setting idx.err.
func (idx *Index) extractSetKeyspacePKAndValue(indexKey, indexValue []byte) (setKey, setValue []byte, skip bool, err error) {
	var compoundPK []byte
	if idx.pinned.decl.Unique {
		pk, _, decErr := decodeUniqueIndexValue(indexValue)
		if decErr != nil {
			return nil, nil, false, fmt.Errorf("%w: %w", ErrCorrupted, decErr)
		}
		compoundPK = pk
	} else {
		extracted, extErr := extractSetKeyspaceCompoundPKFromIndexKey(indexKey, len(idx.pinned.decl.Columns))
		if extErr != nil {
			return nil, nil, false, fmt.Errorf("%w: index %q: %w", ErrCorrupted, idx.pinned.decl.Name, extErr)
		}
		compoundPK = extracted
	}
	sk, sv, decErr := decodeSetKeyspaceCompoundPK(compoundPK)
	if decErr != nil {
		return nil, nil, false, fmt.Errorf("%w: %w", ErrCorrupted, decErr)
	}
	// Back-lookup: verify the (setKey, setValue) pair still
	// exists in the SetKeyspace. Silent-skip per spec on miss.
	has, hvErr := idx.sks.HasValue(sk, sv)
	if hvErr != nil {
		return nil, nil, false, hvErr
	}
	if !has {
		return nil, nil, true, nil
	}
	// Copy out — the caller may retain these slices past the
	// next cursor op.
	skCopy := make([]byte, len(sk))
	copy(skCopy, sk)
	svCopy := make([]byte, len(sv))
	copy(svCopy, sv)
	return skCopy, svCopy, false, nil
}
