package pager

// txCounters holds the per-write-transaction activity counters that back
// the root package's TxStats (api-surface.md §Statistics). Reset at each
// BeginTx; read live via TxStatsSnapshot. Single-threaded with the
// writer (one write tx at a time, owned by one goroutine), so plain
// non-atomic fields suffice.
//
// The pager owns the storage-level counters directly (cow / loose /
// reclaimed / written / slabPeak). The structural and logical counters
// (splits / merges from the btree, gets / puts / deletes from the
// keyspace layer, index entry/probe counts from index maintenance) are
// driven through the exported Record* / Add* methods by those layers,
// which all reach the same per-tx *Pager.
type txCounters struct {
	cow         uint64
	loose       uint64
	reclaimed   uint64
	written     uint64
	splits      uint64
	merges      uint64
	gets        uint64
	puts        uint64
	deletes     uint64
	idxInserted uint64
	idxDeleted  uint64
	idxProbes   uint64
	slabPeak    int
}

// TxStatsSnapshot is the pager-side view of one write transaction's
// counters, consumed by the root package to build gmdb.TxStats.
type TxStatsSnapshot struct {
	CowPages       uint64
	LoosePages     uint64
	ReclaimedPages uint64
	WrittenPages   uint64
	Splits         uint64
	Merges         uint64
	Gets           uint64
	Puts           uint64
	Deletes        uint64
	IndexInserted  uint64
	IndexDeleted   uint64
	IndexProbes    uint64
	SlabPeakBytes  int64
}

// resetTxCounters zeroes the per-tx counters. Called from BeginTx so
// each write transaction starts from zero.
func (p *Pager) resetTxCounters() { p.tc = txCounters{} }

// bumpSlabPeak records a new slab-usage high-water mark. Called after
// every dirtyBytes increase (CoW / AllocSlab / AllocSlabRun); a later
// Discard that lowers dirtyBytes never lowers the recorded peak.
func (p *Pager) bumpSlabPeak() {
	if p.dirtyBytes > p.tc.slabPeak {
		p.tc.slabPeak = p.dirtyBytes
	}
}

// zeroSlabPeak resets the slab peak to 0. Called on rollback (AbortTx):
// TxStats.SlabPeakBytes reports 0 after a Rollback because the
// rolled-back work is not representative of steady-state need
// (api-surface.md §Statistics TxStats.SlabPeakBytes).
func (p *Pager) zeroSlabPeak() { p.tc.slabPeak = 0 }

// setWrittenPages records the number of pages pwritten at commit (data +
// RPL + bitmap from the slab, plus the meta page). Called by Commit.
func (p *Pager) setWrittenPages(n uint64) { p.tc.written = n }

// RecordSplit / RecordMerge are driven by the btree's split / merge
// paths via the SplitMergeRecorder optional interface (*Pager satisfies
// it; test PageWriters that don't care simply omit it).
func (p *Pager) RecordSplit() { p.tc.splits++ }
func (p *Pager) RecordMerge() { p.tc.merges++ }

// RecordGet / RecordPut / RecordDelete are driven by the keyspace-layer
// data ops (one per public Get / Put / Delete call).
func (p *Pager) RecordGet()    { p.tc.gets++ }
func (p *Pager) RecordPut()    { p.tc.puts++ }
func (p *Pager) RecordDelete() { p.tc.deletes++ }

// AddIndexInserted / AddIndexDeleted / RecordIndexProbe are driven by
// index maintenance: the count of index entries inserted / deleted by a
// row mutation and each unique-constraint probe.
func (p *Pager) AddIndexInserted(n uint64) { p.tc.idxInserted += n }
func (p *Pager) AddIndexDeleted(n uint64)  { p.tc.idxDeleted += n }
func (p *Pager) RecordIndexProbe()         { p.tc.idxProbes++ }

// TxStatsSnapshot returns the current per-tx counter values.
func (p *Pager) TxStatsSnapshot() TxStatsSnapshot {
	return TxStatsSnapshot{
		CowPages:       p.tc.cow,
		LoosePages:     p.tc.loose,
		ReclaimedPages: p.tc.reclaimed,
		WrittenPages:   p.tc.written,
		Splits:         p.tc.splits,
		Merges:         p.tc.merges,
		Gets:           p.tc.gets,
		Puts:           p.tc.puts,
		Deletes:        p.tc.deletes,
		IndexInserted:  p.tc.idxInserted,
		IndexDeleted:   p.tc.idxDeleted,
		IndexProbes:    p.tc.idxProbes,
		SlabPeakBytes:  int64(p.tc.slabPeak),
	}
}
