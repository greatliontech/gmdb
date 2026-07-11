package gmdb

import (
	"bytes"
	"fmt"
	"iter"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/extsort"
	"github.com/thegrumpylion/gmdb/internal/indexing"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// Indexed BulkLoad (bulkload.md §Interaction with Indexes). For an
// indexed keyspace the engine must build the row tree AND every index's
// data tree from a single pass over the (one-shot) input stream:
//
//  1. Stream rows → row bulk builder (bulkload.go) and, per row/member,
//     run each index's extractor and feed the produced (indexKey,
//     indexValue) records into that index's external sorter.
//  2. Per index: external merge-sort the records (the extractor produces
//     them in arbitrary order even though rows are PK-sorted), detect
//     unique-index violations at the merge output, and bulk-build the
//     index data tree bottom-up from the sorted records.
//  3. All-or-nothing publish: only after EVERY index tree is built (no
//     unique violation, no I/O error) are the row root + all pinned index
//     roots advanced. A failure in step 2 publishes nothing — the meta
//     swap at commit keeps the pre-BulkLoad state; the pwritten row /
//     index pages are unreferenced bounded leakage (bulkload.md §Interaction with Indexes).
//
// The index entries are encoded EXACTLY as the per-Put maintenance path
// (extractEntriesAsKeySet / setKeyspaceExtractEntries + indexing.EntryValue),
// so a BulkLoad-built keyspace answers Lookups identically to the same
// keyspace built via Put (bulkload.md §Interaction with Indexes).

// indexBuildResult is one index's bulk-built data tree.
type indexBuildResult struct {
	root  uint64
	count uint64
}

// bulkIndexWriter is the union the index build needs: tree pages via
// the page-writer half, overflow chains via AllocContiguous —
// *pager.Pager implements both.
type bulkIndexWriter interface {
	bulkPageWriter
	AllocContiguous(n uint32) (uint64, error)
}

// buildIndexFromSorter sorts the index's records and bulk-builds its data
// tree, detecting unique-index violations at the (merged) sorted output.
//
//   - Not spilled (fit in memory): the chunk is sorted and, for a unique
//     index, pre-scanned for adjacent-duplicate keys BEFORE any index page
//     is pwritten — so the violation is found before the build starts, the
//     abort fully reversible at the index layer (bulkload.md in-memory
//     bullet).
//   - Spilled: the merge output is consumed interleaved with index-page
//     pwrites; the first adjacent-duplicate key is observed mid-build, so
//     some index pages may already be pwritten before the abort (bounded
//     leakage; bulkload.md spilling bullet).
//
// A non-unique index never has adjacent-equal keys at the output (Inv: PKs
// distinct + within-row dedup), so the unique branch is the only one that
// can fire ErrIndexUniqueViolation.
func buildIndexFromSorter(pw bulkIndexWriter, cfg page.Config, s *extsort.Sorter, decl *IndexDecl, ksName string) (indexBuildResult, error) {
	b := newBulkBuilder(pw, cfg)
	unique := decl.Unique
	if !s.Spilled() {
		mem := s.InMemorySorted()
		if unique {
			for i := 1; i < len(mem); i++ {
				if bytes.Equal(mem[i].Key, mem[i-1].Key) {
					return indexBuildResult{}, bulkUniqueViolation(decl.Name, ksName, mem[i].Key)
				}
			}
		}
		for i := range mem {
			// The SAME gate + overflow-promotion shape as the row
			// path (bulkLeafEntry): the split-safety key bound and
			// value overflow behave exactly as the per-Put
			// maintenance path (bulkload.md §API sentinel parity;
			// pre-fix, oversize index keys persisted un-updatably
			// and large covering values aborted the load).
			e, err := bulkLeafEntry(pw, cfg, mem[i].Key, mem[i].Val)
			if err != nil {
				return indexBuildResult{}, fmt.Errorf("gmdb: BulkLoad index %q: %w", decl.Name, err)
			}
			if err := b.add(e); err != nil {
				return indexBuildResult{}, err
			}
		}
		root, count, err := b.finish()
		return indexBuildResult{root: root, count: count}, err
	}

	// extsort.Cascade caps the final run count before NewMerger opens
	// them all at once — pinning the merge-phase FD ceiling +
	// read-buffer memory bound (bulkload.md §Interaction with Indexes
	// "Merge fan-in cap" invariant).
	if err := s.Cascade(); err != nil {
		return indexBuildResult{}, err
	}

	m, err := s.NewMerger()
	if err != nil {
		return indexBuildResult{}, err
	}
	defer m.Close()
	var prevKey []byte
	have := false
	for {
		rec, ok, err := m.Next()
		if err != nil {
			return indexBuildResult{}, fmt.Errorf("gmdb: BulkLoad index %q merge: %w", decl.Name, err)
		}
		if !ok {
			break
		}
		if unique && have && bytes.Equal(rec.Key, prevKey) {
			return indexBuildResult{}, bulkUniqueViolation(decl.Name, ksName, rec.Key)
		}
		e, eerr := bulkLeafEntry(pw, cfg, rec.Key, rec.Val)
		if eerr != nil {
			return indexBuildResult{}, fmt.Errorf("gmdb: BulkLoad index %q: %w", decl.Name, eerr)
		}
		if err := b.add(e); err != nil {
			return indexBuildResult{}, err
		}
		prevKey = append(prevKey[:0], rec.Key...)
		have = true
	}
	root, count, err := b.finish()
	return indexBuildResult{root: root, count: count}, err
}

// bulkUniqueViolation builds the ErrIndexUniqueViolation surfaced when a
// unique index's sorted output holds two entries with the same column
// tuple — whether from one row (candidate-set) or two distinct rows
// (cross-row); both reach the merge output as adjacent-equal keys.
func bulkUniqueViolation(indexName, ksName string, key []byte) error {
	return fmt.Errorf("%w: index %q on keyspace %q: duplicate key %x (BulkLoad)",
		ErrIndexUniqueViolation, indexName, ksName, key)
}

// emitKeyspaceIndexEntries runs every-declared-index maintenance for one
// Keyspace row, encoding each produced entry to its on-disk (key, value)
// and feeding it to that index's sorter. Reuses extractEntriesAsKeySet so
// the within-row candidate-set dedup + unique candidate-set rejection match
// the Put path byte-for-byte (bulkload.md §Interaction with Indexes). The encoded key/value are
// fresh allocations, safe to retain past the caller's buffer reuse.
func emitKeyspaceIndexEntries(s *extsort.Sorter, decl *IndexDecl, key, value []byte) error {
	m, err := extractEntriesAsKeySet(decl, key, value)
	if err != nil {
		return err // unique candidate-set violation surfaces here
	}
	hasCovering := len(decl.Covering) > 0
	for ik, entry := range m {
		val := indexing.EntryValue(entry, key, decl.Unique, hasCovering)
		if err := s.Add([]byte(ik), val); err != nil {
			return err
		}
	}
	return nil
}

// emitSetKeyspaceIndexEntries is the SetKeyspace mirror: the per-member
// extractor runs on (setKey, setValue) and the PK is the compound
// (setKey, setValue) pair (set-keyspace.md §Indexes on SetKeyspaces).
func emitSetKeyspaceIndexEntries(s *extsort.Sorter, decl *IndexDecl, setKey, setValue []byte) error {
	m, err := setKeyspaceExtractEntries(decl, setKey, setValue)
	if err != nil {
		return err
	}
	hasCovering := len(decl.Covering) > 0
	compoundPK := indexing.EncodeSetCompoundPK(setKey, setValue)
	for ik, entry := range m {
		val := indexing.EntryValue(entry, compoundPK, decl.Unique, hasCovering)
		if err := s.Add([]byte(ik), val); err != nil {
			return err
		}
	}
	return nil
}

// newIndexSorters builds one sorter per index, dividing MaxTxBufferBytes
// across them so the aggregate phase-1 in-memory footprint stays bounded by
// MaxTxBufferBytes (bulkload.md §Interaction with Indexes). names must be non-empty.
func newIndexSorters(opts Options, names []string) map[string]*extsort.Sorter {
	budget := opts.MaxTxBufferBytes / len(names)
	sorters := make(map[string]*extsort.Sorter, len(names))
	for _, n := range names {
		sorters[n] = extsort.New(opts.ScratchDir, budget)
	}
	return sorters
}

// finalizeIndexBuild builds every index's data tree (detecting unique
// violations) and — only after ALL succeed — publishes the new roots into
// the pinned map, returning the prior roots to retire. On any build error
// it returns (nil, err) having published NOTHING (bulkload.md §Interaction with Indexes): the
// caller then leaves desc.Root + pinned state untouched. Each sorter's
// in-memory chunk is released as soon as its tree is built.
func finalizeIndexBuild(pw bulkIndexWriter, cfg page.Config, ksName string, names []string, indexes map[string]*pinnedIndex, sorters map[string]*extsort.Sorter) (oldRoots map[string]uint64, err error) {
	built := make(map[string]indexBuildResult, len(names))
	for _, n := range names {
		res, err := buildIndexFromSorter(pw, cfg, sorters[n], indexes[n].decl, ksName)
		if err != nil {
			return nil, err
		}
		built[n] = res
		sorters[n].ReleaseMemory() // tree built; release the chunk
	}
	oldRoots = make(map[string]uint64, len(names))
	for _, n := range names {
		p := indexes[n]
		oldRoots[n] = p.root
		p.root = built[n].root
		p.count = built[n].count
	}
	return oldRoots, nil
}

// retireBulkOldRoots frees the pre-BulkLoad row root and each index's prior
// data root. Called AFTER the new roots are published (publish-then-retire,
// mirroring RebuildIndex's registry-first ordering) so a FreeSubtree failure leaks the
// old pages (Rollback-recoverable) but never leaves a published root
// pointing at freed pages. For a BulkLoad-eligible (empty) keyspace every
// old root is 0, so this is defensive — FreeSubtree(0) is a no-op.
func retireBulkOldRoots(pw btree.PageWriter, cfg page.Config, oldRowRoot uint64, oldRoots map[string]uint64) error {
	if _, err := btree.FreeSubtree(pw, cfg, oldRowRoot); err != nil {
		return mapBtreeErr(err)
	}
	for _, r := range oldRoots {
		if r != 0 {
			if _, err := btree.FreeSubtree(pw, cfg, r); err != nil {
				return mapBtreeErr(err)
			}
		}
	}
	return nil
}

// bulkLoadIndexed bulk-loads an indexed Keyspace: it builds the row tree and
// every index's data tree from a single pass over rows, with all-or-nothing
// publication and unique-violation detection at each index's sorted output.
// See the file-level comment for the phase structure.
func (ks *Keyspace) bulkLoadIndexed(rows iter.Seq2[[]byte, []byte]) (uint64, error) {
	cfg := ks.builderCfg()
	names := sortedIndexNames(ks.indexes)
	sorters := newIndexSorters(ks.tx.db.opts, names)
	defer func() {
		for _, s := range sorters {
			s.Cleanup(ks.tx.db.opts.Logger)
		}
	}()

	b := newBulkBuilder(ks.tx.pgr, cfg)
	onRow := func(key, value []byte) error {
		for _, n := range names {
			if err := emitKeyspaceIndexEntries(sorters[n], ks.indexes[n].decl, key, value); err != nil {
				return err
			}
		}
		return nil
	}
	if err := ks.bulkLoadRows(rows, cfg, b, onRow); err != nil {
		return 0, mapBtreeErr(err) // btree.ErrKeyTooLarge → gmdb.ErrKeyTooLarge
	}
	rowRoot, count, err := b.finish()
	if err != nil {
		return 0, err
	}

	// Build every index tree (no publish yet — a unique violation or I/O
	// error here must leave the keyspace at its pre-BulkLoad state).
	oldRoots, err := finalizeIndexBuild(ks.tx.pgr, ks.tx.pgr.Config(), ks.name.Value(), names, ks.indexes, sorters)
	if err != nil {
		return 0, mapBtreeErr(bulkMapEntryTooLarge(err))
	}

	// All-or-nothing publish: row root + count (pinned index roots were
	// advanced inside finalizeIndexBuild). flushIndexRegistry at commit
	// persists the pinned (root, count) to the registry.
	oldRowRoot := ks.desc.Root
	ks.desc.Root = rowRoot
	ks.desc.Count = count
	ks.markDirty()
	ks.markCursorsStale()
	if err := retireBulkOldRoots(btreeWriter{ks.tx.pgr}, cfg, oldRowRoot, oldRoots); err != nil {
		return 0, err
	}
	return count, nil
}

// bulkLoadIndexed bulk-loads an indexed SetKeyspace: the row side is the
// setBulk accumulator (per-key subpage / nested tree), and the per-member
// extractor (run on each accepted (setKey, setValue)) feeds the per-index
// sorters. Same all-or-nothing publish + unique detection as the Keyspace
// path.
func (ks *SetKeyspace) bulkLoadIndexed(rows iter.Seq2[[]byte, []byte]) (uint64, error) {
	cfg := ks.builderCfg()
	names := sortedIndexNames(ks.indexes)
	sorters := newIndexSorters(ks.tx.db.opts, names)
	defer func() {
		for _, s := range sorters {
			s.Cleanup(ks.tx.db.opts.Logger)
		}
	}()

	sb := &setBulk{
		top:       newBulkBuilder(ks.tx.pgr, cfg),
		pw:        ks.tx.pgr,
		cfg:       cfg,
		fvs:       ks.desc.FixedValueSize,
		threshold: page.SubpagePromotionThreshold(cfg),
	}
	onMember := func(setKey, setValue []byte) error {
		for _, n := range names {
			if err := emitSetKeyspaceIndexEntries(sorters[n], ks.indexes[n].decl, setKey, setValue); err != nil {
				return err
			}
		}
		return nil
	}
	if err := ks.bulkLoadStream(rows, sb, onMember); err != nil {
		return 0, mapBtreeErr(bulkMapEntryTooLarge(err))
	}
	if err := sb.flush(); err != nil {
		return 0, mapBtreeErr(bulkMapEntryTooLarge(err))
	}
	rowRoot, _, err := sb.top.finish()
	if err != nil {
		return 0, mapBtreeErr(err)
	}

	oldRoots, err := finalizeIndexBuild(ks.tx.pgr, ks.tx.pgr.Config(), ks.name.Value(), names, ks.indexes, sorters)
	if err != nil {
		return 0, mapBtreeErr(bulkMapEntryTooLarge(err))
	}

	oldRowRoot := ks.desc.Root
	ks.desc.Root = rowRoot
	ks.desc.Count = sb.total
	ks.markDirty()
	ks.markSetCursorsStale()
	if err := retireBulkOldRoots(btreeWriter{ks.tx.pgr}, cfg, oldRowRoot, oldRoots); err != nil {
		return 0, err
	}
	return sb.total, nil
}
