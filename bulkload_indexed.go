package gmdb

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/binary"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"sort"

	"github.com/thegrumpylion/gmdb/internal/btree"
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
//     index pages are unreferenced bounded leakage (Inv-IdxBulk-1).
//
// The index entries are encoded EXACTLY as the per-Put maintenance path
// (extractEntriesAsKeySet / setKeyspaceExtractEntries + indexEntryValue),
// so a BulkLoad-built keyspace answers Lookups identically to the same
// keyspace built via Put (Inv-IdxBulk-2).

// sortRecord is one (indexKey, indexValue) pair awaiting bulk load. Both
// slices are owned by the sorter (the emit helpers hand it fresh copies),
// so they survive the iter.Seq2 caller's reuse of its key/value buffers.
type sortRecord struct {
	key []byte
	val []byte
}

// sortRecordMemOverhead approximates the per-record heap footprint beyond
// the key/value bytes (two slice headers + the struct) so the in-memory
// budget accounts for slice overhead, not just payload.
const sortRecordMemOverhead = 48

// indexSorter accumulates one index's records and yields them in lex key
// order for the bulk builder. Memory is bounded: once the in-memory chunk
// exceeds budget it is sorted and spilled to a scratch run file, and the
// final tree is built from a k-way merge of all spilled runs plus the last
// in-memory chunk (Inv-IdxBulk-4: aggregate in-memory footprint across all
// indexes ≈ MaxTxBufferBytes, since budget = MaxTxBufferBytes / #indexes).
type indexSorter struct {
	scratchDir string
	budget     int          // max in-memory bytes before spilling
	mem        []sortRecord // current in-memory chunk
	memBytes   int
	runs       []string // spilled sorted-run file paths
	spilled    bool
}

// newIndexSorter returns a sorter spilling to scratchDir once its
// in-memory chunk exceeds budget bytes. A budget < 1 is clamped to 1 so a
// pathologically small MaxTxBufferBytes still makes progress (one record
// per run) rather than dividing to a zero threshold.
func newIndexSorter(scratchDir string, budget int) *indexSorter {
	if budget < 1 {
		budget = 1
	}
	return &indexSorter{scratchDir: scratchDir, budget: budget}
}

// add appends a record (which the sorter takes ownership of) and spills if
// the in-memory chunk now exceeds the budget.
func (s *indexSorter) add(key, val []byte) error {
	s.mem = append(s.mem, sortRecord{key: key, val: val})
	s.memBytes += len(key) + len(val) + sortRecordMemOverhead
	if s.memBytes > s.budget {
		return s.spill()
	}
	return nil
}

// sortRecordsByKey sorts records into lex key order. Key alone is a total
// order for the bulk builder: non-unique keys are globally distinct (the
// PK suffix differs per row/member, and within-row duplicates are already
// collapsed by the extract-as-key-set helpers), and a unique-index tie IS
// the violation the merge output detects — so no secondary value ordering
// is needed.
func sortRecordsByKey(recs []sortRecord) {
	sort.Slice(recs, func(i, j int) bool {
		return bytes.Compare(recs[i].key, recs[j].key) < 0
	})
}

// spill sorts the in-memory chunk and writes it as a fresh scratch run,
// then resets the chunk. A create/write/close failure removes any partial
// file and returns the wrapped I/O error — the BulkLoad aborts (bulkload.md
// §Interaction with Indexes: "A spill write failure aborts the BulkLoad
// with the underlying I/O error wrapped").
func (s *indexSorter) spill() error {
	sortRecordsByKey(s.mem)
	f, err := os.CreateTemp(s.scratchDir, "gmdb-bulkidx-*.run")
	if err != nil {
		return fmt.Errorf("gmdb: BulkLoad sort spill: create scratch file in %q: %w", s.scratchDir, err)
	}
	name := f.Name()
	if err := writeSortRun(f, s.mem); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return fmt.Errorf("gmdb: BulkLoad sort spill to %q: %w", name, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("gmdb: BulkLoad sort spill close %q: %w", name, err)
	}
	s.runs = append(s.runs, name)
	s.spilled = true
	s.mem = s.mem[:0]
	s.memBytes = 0
	return nil
}

// cleanup best-effort removes every spilled run file. An unremovable file
// is logged via the DB's Logger and does NOT fail the operation
// (bulkload.md §Interaction with Indexes; Inv-IdxBulk-5). Safe to call once
// per sorter regardless of success or failure; run readers are already
// closed by the merger before cleanup runs.
func (s *indexSorter) cleanup(logger *slog.Logger) {
	for _, name := range s.runs {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			if logger != nil {
				logger.Warn("gmdb: BulkLoad scratch file removal failed", "path", name, "error", err)
			}
		}
	}
	s.runs = nil
	s.spilled = false
}

// writeSortRun serialises records as a sequence of uvarint-length-prefixed
// (key, val) fields through a buffered writer.
func writeSortRun(f *os.File, recs []sortRecord) error {
	w := bufio.NewWriterSize(f, 64<<10)
	var hdr [binary.MaxVarintLen64]byte
	put := func(b []byte) error {
		n := binary.PutUvarint(hdr[:], uint64(len(b)))
		if _, err := w.Write(hdr[:n]); err != nil {
			return err
		}
		_, err := w.Write(b)
		return err
	}
	for _, r := range recs {
		if err := put(r.key); err != nil {
			return err
		}
		if err := put(r.val); err != nil {
			return err
		}
	}
	return w.Flush()
}

// runReader yields a single sorted run's records in order; ok=false marks
// clean end-of-run.
type runReader interface {
	next() (sortRecord, bool, error)
}

// sliceRunReader serves the final (un-spilled) in-memory chunk as a run.
type sliceRunReader struct {
	recs []sortRecord
	i    int
}

func (r *sliceRunReader) next() (sortRecord, bool, error) {
	if r.i >= len(r.recs) {
		return sortRecord{}, false, nil
	}
	rec := r.recs[r.i]
	r.i++
	return rec, true, nil
}

// fileRunReader reads a spilled run's length-prefixed records.
type fileRunReader struct {
	f  *os.File
	br *bufio.Reader
}

func openFileRunReader(name string) (*fileRunReader, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return &fileRunReader{f: f, br: bufio.NewReaderSize(f, 64<<10)}, nil
}

func (r *fileRunReader) next() (sortRecord, bool, error) {
	key, ok, err := readRunField(r.br)
	if err != nil || !ok {
		// ok=false with err=nil is a clean EOF on the key boundary
		// (end of run); err!=nil is a truncated/corrupt run.
		return sortRecord{}, ok, err
	}
	val, ok, err := readRunField(r.br)
	if err != nil {
		return sortRecord{}, false, err
	}
	if !ok {
		return sortRecord{}, false, io.ErrUnexpectedEOF // key without a value
	}
	return sortRecord{key: key, val: val}, true, nil
}

func (r *fileRunReader) close() error { return r.f.Close() }

// readRunField reads one uvarint-length-prefixed field. Returns
// (nil, false, nil) at a clean EOF on the length boundary (run exhausted);
// a truncated length or short payload surfaces as an error.
func readRunField(br *bufio.Reader) ([]byte, bool, error) {
	n, err := binary.ReadUvarint(br)
	if err != nil {
		if err == io.EOF {
			return nil, false, nil
		}
		return nil, false, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return nil, false, err
	}
	return buf, true, nil
}

// recordHeapItem is one run's current head record in the merge heap.
type recordHeapItem struct {
	rec    sortRecord
	runIdx int
}

type recordHeap []recordHeapItem

func (h recordHeap) Len() int           { return len(h) }
func (h recordHeap) Less(i, j int) bool { return bytes.Compare(h[i].rec.key, h[j].rec.key) < 0 }
func (h recordHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *recordHeap) Push(x any)        { *h = append(*h, x.(recordHeapItem)) }
func (h *recordHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// sortMerger performs a k-way merge over a sorter's spilled runs plus its
// final in-memory chunk, yielding records in lex key order.
type sortMerger struct {
	readers []runReader
	files   []*fileRunReader // subset of readers needing close()
	h       recordHeap
}

// newMerger opens every spilled run, sorts the final in-memory chunk into a
// run, and primes the merge heap. On any open/read error all opened files
// are closed before returning.
func (s *indexSorter) newMerger() (*sortMerger, error) {
	m := &sortMerger{}
	if len(s.mem) > 0 {
		sortRecordsByKey(s.mem)
		m.readers = append(m.readers, &sliceRunReader{recs: s.mem})
	}
	for _, name := range s.runs {
		fr, err := openFileRunReader(name)
		if err != nil {
			m.close()
			return nil, fmt.Errorf("gmdb: BulkLoad merge: open run %q: %w", name, err)
		}
		m.files = append(m.files, fr)
		m.readers = append(m.readers, fr)
	}
	for i, r := range m.readers {
		rec, ok, err := r.next()
		if err != nil {
			m.close()
			return nil, err
		}
		if ok {
			m.h = append(m.h, recordHeapItem{rec: rec, runIdx: i})
		}
	}
	heap.Init(&m.h)
	return m, nil
}

// next returns the smallest remaining record across all runs.
func (m *sortMerger) next() (sortRecord, bool, error) {
	if m.h.Len() == 0 {
		return sortRecord{}, false, nil
	}
	it := heap.Pop(&m.h).(recordHeapItem)
	rec, ok, err := m.readers[it.runIdx].next()
	if err != nil {
		return sortRecord{}, false, err
	}
	if ok {
		heap.Push(&m.h, recordHeapItem{rec: rec, runIdx: it.runIdx})
	}
	return it.rec, true, nil
}

func (m *sortMerger) close() {
	for _, f := range m.files {
		_ = f.close()
	}
	m.files = nil
	m.readers = nil
}

// indexBuildResult is one index's bulk-built data tree.
type indexBuildResult struct {
	root  uint64
	count uint64
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
func buildIndexFromSorter(pw bulkPageWriter, cfg page.Config, s *indexSorter, decl *IndexDecl, ksName string) (indexBuildResult, error) {
	b := newBulkBuilder(pw, cfg)
	unique := decl.Unique
	if !s.spilled {
		sortRecordsByKey(s.mem)
		if unique {
			for i := 1; i < len(s.mem); i++ {
				if bytes.Equal(s.mem[i].key, s.mem[i-1].key) {
					return indexBuildResult{}, bulkUniqueViolation(decl.Name, ksName, s.mem[i].key)
				}
			}
		}
		for i := range s.mem {
			if err := b.add(page.LeafEntry{Key: s.mem[i].key, Value: s.mem[i].val}); err != nil {
				return indexBuildResult{}, err
			}
		}
		root, count, err := b.finish()
		return indexBuildResult{root: root, count: count}, err
	}

	m, err := s.newMerger()
	if err != nil {
		return indexBuildResult{}, err
	}
	defer m.close()
	var prevKey []byte
	have := false
	for {
		rec, ok, err := m.next()
		if err != nil {
			return indexBuildResult{}, fmt.Errorf("gmdb: BulkLoad index %q merge: %w", decl.Name, err)
		}
		if !ok {
			break
		}
		if unique && have && bytes.Equal(rec.key, prevKey) {
			return indexBuildResult{}, bulkUniqueViolation(decl.Name, ksName, rec.key)
		}
		if err := b.add(page.LeafEntry{Key: rec.key, Value: rec.val}); err != nil {
			return indexBuildResult{}, err
		}
		prevKey = append(prevKey[:0], rec.key...)
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
// the Put path byte-for-byte (Inv-IdxBulk-2). The encoded key/value are
// fresh allocations, safe to retain past the caller's buffer reuse.
func emitKeyspaceIndexEntries(s *indexSorter, decl *IndexDecl, key, value []byte) error {
	m, err := extractEntriesAsKeySet(decl, key, value)
	if err != nil {
		return err // unique candidate-set violation surfaces here
	}
	hasCovering := len(decl.Covering) > 0
	for ik, entry := range m {
		val := indexEntryValue(entry, key, decl.Unique, hasCovering)
		if err := s.add([]byte(ik), val); err != nil {
			return err
		}
	}
	return nil
}

// emitSetKeyspaceIndexEntries is the SetKeyspace mirror: the per-member
// extractor runs on (setKey, setValue) and the PK is the compound
// (setKey, setValue) pair (set-keyspace.md §Indexes on SetKeyspaces).
func emitSetKeyspaceIndexEntries(s *indexSorter, decl *IndexDecl, setKey, setValue []byte) error {
	m, err := setKeyspaceExtractEntries(decl, setKey, setValue)
	if err != nil {
		return err
	}
	hasCovering := len(decl.Covering) > 0
	compoundPK := encodeSetKeyspaceCompoundPK(setKey, setValue)
	for ik, entry := range m {
		val := indexEntryValue(entry, compoundPK, decl.Unique, hasCovering)
		if err := s.add([]byte(ik), val); err != nil {
			return err
		}
	}
	return nil
}

// newIndexSorters builds one sorter per index, dividing MaxTxBufferBytes
// across them so the aggregate phase-1 in-memory footprint stays bounded by
// MaxTxBufferBytes (Inv-IdxBulk-4). names must be non-empty.
func newIndexSorters(opts Options, names []string) map[string]*indexSorter {
	budget := opts.MaxTxBufferBytes / len(names)
	sorters := make(map[string]*indexSorter, len(names))
	for _, n := range names {
		sorters[n] = newIndexSorter(opts.ScratchDir, budget)
	}
	return sorters
}

// finalizeIndexBuild builds every index's data tree (detecting unique
// violations) and — only after ALL succeed — publishes the new roots into
// the pinned map, returning the prior roots to retire. On any build error
// it returns (nil, err) having published NOTHING (Inv-IdxBulk-1): the
// caller then leaves desc.Root + pinned state untouched. Each sorter's
// in-memory chunk is released as soon as its tree is built.
func finalizeIndexBuild(pw bulkPageWriter, cfg page.Config, ksName string, names []string, indexes map[string]*pinnedIndex, sorters map[string]*indexSorter) (oldRoots map[string]uint64, err error) {
	built := make(map[string]indexBuildResult, len(names))
	for _, n := range names {
		res, err := buildIndexFromSorter(pw, cfg, sorters[n], indexes[n].decl, ksName)
		if err != nil {
			return nil, err
		}
		built[n] = res
		sorters[n].mem = nil // tree built; release the chunk
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
// mirroring RebuildIndex's H-2 ordering) so a FreeSubtree failure leaks the
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
			s.cleanup(ks.tx.db.opts.Logger)
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
		return 0, err
	}
	rowRoot, count, err := b.finish()
	if err != nil {
		return 0, err
	}

	// Build every index tree (no publish yet — a unique violation or I/O
	// error here must leave the keyspace at its pre-BulkLoad state).
	oldRoots, err := finalizeIndexBuild(ks.tx.pgr, cfg, ks.name.Value(), names, ks.indexes, sorters)
	if err != nil {
		return 0, err
	}

	// All-or-nothing publish: row root + count (pinned index roots were
	// advanced inside finalizeIndexBuild). flushIndexRegistry at commit
	// persists the pinned (root, count) to the registry.
	oldRowRoot := ks.desc.Root
	ks.desc.Root = rowRoot
	ks.desc.Count = count
	ks.markDirty()
	ks.markCursorsStale()
	if err := retireBulkOldRoots(ks.tx.pgr, cfg, oldRowRoot, oldRoots); err != nil {
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
			s.cleanup(ks.tx.db.opts.Logger)
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
		return 0, err
	}
	if err := sb.flush(); err != nil {
		return 0, err
	}
	rowRoot, _, err := sb.top.finish()
	if err != nil {
		return 0, err
	}

	oldRoots, err := finalizeIndexBuild(ks.tx.pgr, cfg, ks.name.Value(), names, ks.indexes, sorters)
	if err != nil {
		return 0, err
	}

	oldRowRoot := ks.desc.Root
	ks.desc.Root = rowRoot
	ks.desc.Count = sb.total
	ks.markDirty()
	ks.markSetCursorsStale()
	if err := retireBulkOldRoots(ks.tx.pgr, cfg, oldRowRoot, oldRoots); err != nil {
		return 0, err
	}
	return sb.total, nil
}
