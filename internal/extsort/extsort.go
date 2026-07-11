// Package extsort implements the external merge sort used by the
// indexed bulk-load path: (key, value) byte records accumulate in
// memory up to a budget, spill to sorted runs in a scratch
// directory, and merge back with bounded fan-in. Pure over
// os/bufio/heap — no engine state.
package extsort

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync/atomic"
)

// Record is one (indexKey, indexValue) pair awaiting bulk load. Both
// slices are owned by the sorter (the emit helpers hand it fresh copies),
// so they survive the iter.Seq2 caller's reuse of its key/value buffers.
type Record struct {
	Key []byte
	Val []byte
}

// recordMemOverhead approximates the per-record heap footprint beyond
// the key/value bytes (two slice headers + the struct) so the in-memory
// budget accounts for slice overhead, not just payload.
const recordMemOverhead = 48

// Sorter accumulates one index's records and yields them in lex key
// order for the bulk builder. Memory is bounded: once the in-memory chunk
// exceeds budget it is sorted and spilled to a scratch run file, and the
// final tree is built from a k-way merge of all spilled runs plus the last
// in-memory chunk (bulkload.md §Interaction with Indexes: aggregate in-memory footprint across all
// indexes ≈ MaxTxBufferBytes, since budget = MaxTxBufferBytes / #indexes).
type Sorter struct {
	scratchDir string
	budget     int      // max in-memory bytes before spilling
	mem        []Record // current in-memory chunk
	memBytes   int
	runs       []string // spilled sorted-run file paths
	spilled    bool
}

// New returns a sorter spilling to scratchDir once its
// in-memory chunk exceeds budget bytes. A budget < 1 is clamped to 1 so a
// pathologically small MaxTxBufferBytes still makes progress (one record
// per run) rather than dividing to a zero threshold.
func New(scratchDir string, budget int) *Sorter {
	if budget < 1 {
		budget = 1
	}
	return &Sorter{scratchDir: scratchDir, budget: budget}
}

// Add appends a record (which the sorter takes ownership of) and spills if
// the in-memory chunk now exceeds the budget.
func (s *Sorter) Add(key, val []byte) error {
	s.mem = append(s.mem, Record{Key: key, Val: val})
	s.memBytes += len(key) + len(val) + recordMemOverhead
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
func sortRecordsByKey(recs []Record) {
	sort.Slice(recs, func(i, j int) bool {
		return bytes.Compare(recs[i].Key, recs[j].Key) < 0
	})
}

// spill sorts the in-memory chunk and writes it as a fresh scratch run,
// then resets the chunk. A create/write/close failure removes any partial
// file and returns the wrapped I/O error — the BulkLoad aborts (bulkload.md
// §Interaction with Indexes: "A spill write failure aborts the BulkLoad
// with the underlying I/O error wrapped").
func (s *Sorter) spill() error {
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

// Cleanup best-effort removes every spilled run file. An unremovable file
// is logged via the DB's Logger and does NOT fail the operation
// (bulkload.md §Interaction with Indexes). Safe to call once
// per sorter regardless of success or failure; run readers are already
// closed by the merger before cleanup runs.
func (s *Sorter) Cleanup(logger *slog.Logger) {
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
func writeSortRun(f *os.File, recs []Record) error {
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
		if err := put(r.Key); err != nil {
			return err
		}
		if err := put(r.Val); err != nil {
			return err
		}
	}
	return w.Flush()
}

// runReader yields a single sorted run's records in order; ok=false marks
// clean end-of-run.
type runReader interface {
	next() (Record, bool, error)
}

// sliceRunReader serves the final (un-spilled) in-memory chunk as a run.
type sliceRunReader struct {
	recs []Record
	i    int
}

func (r *sliceRunReader) next() (Record, bool, error) {
	if r.i >= len(r.recs) {
		return Record{}, false, nil
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

func (r *fileRunReader) next() (Record, bool, error) {
	key, ok, err := readRunField(r.br)
	if err != nil || !ok {
		// ok=false with err=nil is a clean EOF on the key boundary
		// (end of run); err!=nil is a truncated/corrupt run.
		return Record{}, ok, err
	}
	val, ok, err := readRunField(r.br)
	if err != nil {
		return Record{}, false, err
	}
	if !ok {
		return Record{}, false, io.ErrUnexpectedEOF // key without a value
	}
	return Record{Key: key, Val: val}, true, nil
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
	rec    Record
	runIdx int
}

type recordHeap []recordHeapItem

func (h recordHeap) Len() int           { return len(h) }
func (h recordHeap) Less(i, j int) bool { return bytes.Compare(h[i].rec.Key, h[j].rec.Key) < 0 }
func (h recordHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *recordHeap) Push(x any)        { *h = append(*h, x.(recordHeapItem)) }
func (h *recordHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// Merger performs a k-way merge over a sorter's spilled runs plus its
// final in-memory chunk, yielding records in lex key order.
type Merger struct {
	readers []runReader
	files   []*fileRunReader // subset of readers needing close()
	h       recordHeap
}

// Spilled reports whether any run has been written to the scratch
// directory. The consumer's unique-violation semantics differ by
// mode: an in-memory build can pre-scan duplicates before writing
// any page; a spilled build detects them interleaved with the
// merge (bulkload.md §Interaction with Indexes).
func (s *Sorter) Spilled() bool { return s.spilled }

// InMemorySorted sorts the in-memory records in place and returns
// them. Only meaningful when !Spilled() (a spilled sorter's
// in-memory residue is consumed through NewMerger instead).
func (s *Sorter) InMemorySorted() []Record {
	sortRecordsByKey(s.mem)
	return s.mem
}

// ReleaseMemory drops the in-memory record chunk (the consumer has
// finished with it); the sorter must not be Added to afterwards.
func (s *Sorter) ReleaseMemory() { s.mem = nil }

// Cascade merges run files down to <= maxMergeFanIn final runs
// before NewMerger opens them all at once — pinning the
// merge-phase FD ceiling + read-buffer memory bound (bulkload.md
// §Interaction with Indexes "Merge fan-in cap" invariant). Fires
// the test cascade hook with the pre/post run counts.
func (s *Sorter) Cascade() error {
	pre := len(s.runs)
	if err := s.cascadeRuns(); err != nil {
		return err
	}
	if hook := mergeCascadeHookForTest.Load(); hook != nil {
		(*hook)(pre, len(s.runs))
	}
	return nil
}

// NewMerger opens every spilled run, sorts the final in-memory
// chunk into a run, and primes the merge heap. On any open/read
// error all opened files are closed before returning.
func (s *Sorter) NewMerger() (*Merger, error) {
	m := &Merger{}
	if len(s.mem) > 0 {
		sortRecordsByKey(s.mem)
		m.readers = append(m.readers, &sliceRunReader{recs: s.mem})
	}
	for _, name := range s.runs {
		fr, err := openFileRunReader(name)
		if err != nil {
			m.Close()
			return nil, fmt.Errorf("gmdb: BulkLoad merge: open run %q: %w", name, err)
		}
		m.files = append(m.files, fr)
		m.readers = append(m.readers, fr)
	}
	for i, r := range m.readers {
		rec, ok, err := r.next()
		if err != nil {
			m.Close()
			return nil, err
		}
		if ok {
			m.h = append(m.h, recordHeapItem{rec: rec, runIdx: i})
		}
	}
	heap.Init(&m.h)
	return m, nil
}

// Next returns the smallest remaining record across all runs.
func (m *Merger) Next() (Record, bool, error) {
	if m.h.Len() == 0 {
		return Record{}, false, nil
	}
	it := heap.Pop(&m.h).(recordHeapItem)
	rec, ok, err := m.readers[it.runIdx].next()
	if err != nil {
		return Record{}, false, err
	}
	if ok {
		heap.Push(&m.h, recordHeapItem{rec: rec, runIdx: it.runIdx})
	}
	return it.rec, true, nil
}

func (m *Merger) Close() {
	for _, f := range m.files {
		_ = f.close()
	}
	m.files = nil
	m.readers = nil
}

// maxMergeFanIn caps the number of spilled-run files the Merger
// opens simultaneously. When the sorter spilled more runs than this
// cap, runs are first cascaded through one or more intermediate merge
// passes (each merging up to maxMergeFanIn runs into a single larger
// run) until the final fan-in fits the cap. Bounds the merger's peak
// open-file count at O(maxMergeFanIn) and read-buffer memory at
// O(maxMergeFanIn × 64 KiB) regardless of #runs, at the cost of
// O(log_fanin(#runs)) extra scratch read+write passes. See
// bulkload.md §Interaction with Indexes "Merge fan-in cap" for the
// invariant + workload reasoning.
//
// 128 stays comfortably under the typical per-process FD limit (256 on
// macOS default, 1024+ on Linux distros) while keeping the merge-phase
// read-buffer budget at 128 × 64 KiB = 8 MiB — small relative to the
// default 256 MiB MaxTxBufferBytes.
//
// Declared as var (not const) so tests can swap to a small value via
// SetMaxMergeFanInForTest; production callers never mutate it.
var maxMergeFanIn = 128

// SetMaxMergeFanInForTest swaps the merge fan-in cap for the duration
// of the returned restore closure. Global state — tests that call this
// must NOT call t.Parallel(). Used by TestKeyspaceBulkLoadIndexedMerge
// CascadeBoundsFanIn to force the cascade path on a small workload.
func SetMaxMergeFanInForTest(n int) (restore func()) {
	if n < 2 {
		panic(fmt.Sprintf("SetMaxMergeFanInForTest: n=%d below minimum 2 (groups of one cannot reduce the run count)", n))
	}
	old := maxMergeFanIn
	maxMergeFanIn = n
	return func() { maxMergeFanIn = old }
}

// mergeCascadeHookForTest, when set, is invoked once per Cascade
// call (the consumer's spilled-build branch), with the
// (preCascadeRuns, postCascadeRuns) — len(s.runs) before and after
// cascadeRuns. When no cascade was needed (preCascadeRuns <=
// maxMergeFanIn), preCascadeRuns == postCascadeRuns. Used by tests to
// pin the merger's fan-in cap behavior. Global state — tests that
// install must NOT call t.Parallel(). The hook fires only on the
// cascade success path: a cascadeRuns error short-circuits before the
// hook, so tests of the cascade error path observe the wrapped error
// at the BulkLoad return site instead.
var mergeCascadeHookForTest atomic.Pointer[func(preCascadeRuns, postCascadeRuns int)]

func SetMergeCascadeHookForTest(hook func(preCascadeRuns, postCascadeRuns int)) {
	if hook == nil {
		mergeCascadeHookForTest.Store(nil)
		return
	}
	mergeCascadeHookForTest.Store(&hook)
}

// cascadeRuns reduces s.runs to at most maxMergeFanIn entries by
// merging groups of up to maxMergeFanIn runs into single intermediate
// runs and repeating until the count fits the cap. After each
// successful level the prior runs are removed from scratch and the
// intermediates replace them in s.runs so the standard cleanup defer
// still reaches every remaining file on any later failure.
//
// On a mid-level error, intermediates created in the failing pass are
// removed before return; s.runs is left at the start-of-level state
// so the standard cleanup defer reclaims them. The pre-level runs are
// NOT pre-emptively removed (only after the next level fully writes)
// so a write failure mid-pass never destroys data that hasn't been
// safely re-encoded into the next level.
//
// The cap is exclusive of s.mem — newMerger adds the in-memory chunk
// as one additional reader, so the final merger's open-file count is
// bounded by len(s.runs) <= maxMergeFanIn after cascadeRuns returns.
func (s *Sorter) cascadeRuns() error {
	for len(s.runs) > maxMergeFanIn {
		nextLevel := make([]string, 0, (len(s.runs)+maxMergeFanIn-1)/maxMergeFanIn)
		var cascadeErr error
		for start := 0; start < len(s.runs); start += maxMergeFanIn {
			end := min(start+maxMergeFanIn, len(s.runs))
			outPath, err := mergeGroupToScratchRun(s.scratchDir, s.runs[start:end])
			if err != nil {
				cascadeErr = err
				break
			}
			nextLevel = append(nextLevel, outPath)
		}
		if cascadeErr != nil {
			for _, p := range nextLevel {
				_ = os.Remove(p)
			}
			return cascadeErr
		}
		oldRuns := s.runs
		s.runs = nextLevel
		for _, p := range oldRuns {
			_ = os.Remove(p)
		}
	}
	return nil
}

// mergeGroupToScratchRun opens the named spilled-run files, k-way
// merges them into a single sorted run written to a fresh scratch file
// in scratchDir, and returns its path. On any open / read / write
// error all opened readers are closed, the partial output file (if
// created) is removed, and the wrapped error is returned.
//
// Holds at most len(runs)+1 simultaneously-open files (len(runs)
// readers + 1 output writer), so the cascade's per-pass FD ceiling is
// maxMergeFanIn+1.
func mergeGroupToScratchRun(scratchDir string, runs []string) (string, error) {
	readers := make([]*fileRunReader, 0, len(runs))
	closeReaders := func() {
		for _, r := range readers {
			_ = r.close()
		}
	}
	for _, name := range runs {
		r, err := openFileRunReader(name)
		if err != nil {
			closeReaders()
			return "", fmt.Errorf("gmdb: BulkLoad cascade open %q: %w", name, err)
		}
		readers = append(readers, r)
	}

	out, err := os.CreateTemp(scratchDir, "gmdb-bulkidx-merge-*.run")
	if err != nil {
		closeReaders()
		return "", fmt.Errorf("gmdb: BulkLoad cascade create scratch in %q: %w", scratchDir, err)
	}
	outName := out.Name()
	bw := bufio.NewWriterSize(out, 64<<10)

	abort := func(werr error) (string, error) {
		closeReaders()
		_ = out.Close()
		_ = os.Remove(outName)
		return "", werr
	}

	h := make(recordHeap, 0, len(readers))
	for i, r := range readers {
		rec, ok, err := r.next()
		if err != nil {
			return abort(fmt.Errorf("gmdb: BulkLoad cascade read %q: %w", runs[i], err))
		}
		if ok {
			h = append(h, recordHeapItem{rec: rec, runIdx: i})
		}
	}
	heap.Init(&h)

	var hdr [binary.MaxVarintLen64]byte
	putField := func(b []byte) error {
		n := binary.PutUvarint(hdr[:], uint64(len(b)))
		if _, werr := bw.Write(hdr[:n]); werr != nil {
			return werr
		}
		_, werr := bw.Write(b)
		return werr
	}

	for h.Len() > 0 {
		it := heap.Pop(&h).(recordHeapItem)
		if err := putField(it.rec.Key); err != nil {
			return abort(fmt.Errorf("gmdb: BulkLoad cascade write %q: %w", outName, err))
		}
		if err := putField(it.rec.Val); err != nil {
			return abort(fmt.Errorf("gmdb: BulkLoad cascade write %q: %w", outName, err))
		}
		rec, ok, err := readers[it.runIdx].next()
		if err != nil {
			return abort(fmt.Errorf("gmdb: BulkLoad cascade read %q: %w", runs[it.runIdx], err))
		}
		if ok {
			heap.Push(&h, recordHeapItem{rec: rec, runIdx: it.runIdx})
		}
	}

	closeReaders()
	if err := bw.Flush(); err != nil {
		_ = out.Close()
		_ = os.Remove(outName)
		return "", fmt.Errorf("gmdb: BulkLoad cascade flush %q: %w", outName, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(outName)
		return "", fmt.Errorf("gmdb: BulkLoad cascade close %q: %w", outName, err)
	}
	return outName, nil
}
