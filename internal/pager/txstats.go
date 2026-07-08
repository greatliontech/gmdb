package pager

// TxStatsSnapshot is the per-write-transaction activity counter set,
// consumed by the root package to build gmdb.TxStats (api-surface.md
// §Statistics) and used directly as the pager's live counter storage
// (p.tc) — one struct, no field-copy translation layer. Reset at each
// BeginTx; read via TxStatsSnapshot(). Single-threaded with the
// writer (one write tx at a time, owned by one goroutine), so plain
// non-atomic fields suffice.
//
// The pager owns the storage-level counters directly (CowPages /
// LoosePages / ReclaimedPages / WrittenPages / SlabPeakBytes). The
// structural and logical counters (splits / merges from the btree,
// gets / puts / deletes from the keyspace layer, index entry/probe
// counts from index maintenance) are driven through the exported
// Record* / Add* methods by those layers, which all reach the same
// per-tx *Pager.
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
func (p *Pager) resetTxCounters() { p.tc = TxStatsSnapshot{} }

// bumpSlabPeak records a new slab-usage high-water mark. Called after
// every dirtyBytes increase (CoW / AllocSlab / AllocSlabRun); a later
// Discard that lowers dirtyBytes never lowers the recorded peak.
func (p *Pager) bumpSlabPeak() {
	if int64(p.dirtyBytes) > p.tc.SlabPeakBytes {
		p.tc.SlabPeakBytes = int64(p.dirtyBytes)
	}
}

// zeroSlabPeak resets the slab peak to 0. Called on rollback (AbortTx):
// TxStats.SlabPeakBytes reports 0 after a Rollback because the
// rolled-back work is not representative of steady-state need
// (api-surface.md §Statistics TxStats.SlabPeakBytes).
func (p *Pager) zeroSlabPeak() { p.tc.SlabPeakBytes = 0 }

// setWrittenPages records the number of pages pwritten at commit (data +
// RPL + bitmap from the slab, plus the meta page). Called by Commit.
func (p *Pager) setWrittenPages(n uint64) { p.tc.WrittenPages = n }

// RecordSplit / RecordMerge are driven by the btree's split / merge
// paths via the SplitMergeRecorder optional interface (*Pager satisfies
// it; test PageWriters that don't care simply omit it).
func (p *Pager) RecordSplit() { p.tc.Splits++ }
func (p *Pager) RecordMerge() { p.tc.Merges++ }

// RecordGet / RecordPut / RecordDelete are driven by the keyspace-layer
// data ops (one per public Get / Put / Delete call).
func (p *Pager) RecordGet()    { p.tc.Gets++ }
func (p *Pager) RecordPut()    { p.tc.Puts++ }
func (p *Pager) RecordDelete() { p.tc.Deletes++ }

// AddIndexInserted / AddIndexDeleted / RecordIndexProbe are driven by
// index maintenance: the count of index entries inserted / deleted by a
// row mutation and each unique-constraint probe.
func (p *Pager) AddIndexInserted(n uint64) { p.tc.IndexInserted += n }
func (p *Pager) AddIndexDeleted(n uint64)  { p.tc.IndexDeleted += n }
func (p *Pager) RecordIndexProbe()         { p.tc.IndexProbes++ }

// TxStatsSnapshot returns a copy of the current per-tx counter values.
func (p *Pager) TxStatsSnapshot() TxStatsSnapshot { return p.tc }
