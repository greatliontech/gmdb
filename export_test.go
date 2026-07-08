package gmdb

import (
	"context"

	"github.com/thegrumpylion/gmdb/internal/pager"
)

// Test-only handle methods.
//
// These live in a _test.go file deliberately: methods defined in test
// files compile only into the package's test binary, so they are NOT
// part of gmdb's importable API — `go doc` omits them and external
// callers cannot reach them. White-box tests in package gmdb use them
// to drive the pager and snapshot-meta layers directly; no production
// code path does.
//
// They were moved here from the production sources to close two
// public-API-surface findings: raw pager primitives (AllocPage, CoW,
// AllocSlab, FreePage, Page) leaking onto *Tx, and the internal
// pager.Meta storage type leaking through *DB / *ReadTx accessors. The
// public contract these are excluded from is docs/specs/api-surface.md
// (ReadTx surface = Commit / Rollback only; public stats via DB.Stats).

// AllocPage allocates a single page following the freespace priority
// (loose -> bitmap -> RPL reclamation -> file extension). Callers
// typically follow with AllocSlab(id) or CoW(src, id) to populate it.
func (tx *Tx) AllocPage() (uint64, error) {
	if err := tx.requireOpen(true); err != nil {
		return 0, err
	}
	tx.pgr.SetCurrentTxnID(tx.newTxnID)
	id, err := tx.pgr.AllocPage()
	return id, mapPagerErr(err)
}

// FreePage marks id for retirement. Same-tx pages become loose;
// prior-tx pages join the RPL at commit.
func (tx *Tx) FreePage(id uint64) error {
	if err := tx.requireOpen(true); err != nil {
		return err
	}
	return mapPagerErr(tx.pgr.FreePage(id))
}

// Page resolves id to a borrowed byte slice (slab for own-writes this
// tx, else mmap). Valid until tx close.
func (tx *Tx) Page(id uint64) ([]byte, error) {
	if err := tx.requireOpen(false); err != nil {
		return nil, err
	}
	return tx.pgr.Page(id)
}

// CoW copies srcID's content into a fresh slab buffer keyed at dstID
// and returns the writable buffer. Idempotent on re-CoW at dstID.
func (tx *Tx) CoW(srcID, dstID uint64) ([]byte, error) {
	if err := tx.requireOpen(true); err != nil {
		return nil, err
	}
	buf, err := tx.pgr.CoW(srcID, dstID)
	return buf, mapPagerErr(err)
}

// AllocSlab installs a fresh zero-filled slab buffer at id and returns
// it (no source page read).
func (tx *Tx) AllocSlab(id uint64) ([]byte, error) {
	if err := tx.requireOpen(true); err != nil {
		return nil, err
	}
	buf, err := tx.pgr.AllocSlab(id)
	return buf, mapPagerErr(err)
}

// Page returns the content of page id within this snapshot, borrowed
// from the read-only mmap and valid until the ReadTx closes. Callers
// must gate id by the snapshot's HighWaterMark (accessing past it
// SIGBUSes). Returns ErrTxClosed after close; ErrClosed if the DB is
// closed.
func (rtx *ReadTx) Page(id uint64) ([]byte, error) {
	if rtx.closed {
		return nil, ErrTxClosed
	}
	if rtx.db.closeGate.IsClosed() {
		return nil, ErrClosed
	}
	return rtx.pgr.Page(id)
}

// Meta returns a copy of the snapshot meta, independent of any
// subsequent commit. Production code reads the rtx.meta field directly.
func (rtx *ReadTx) Meta() pager.Meta { return rtx.meta }

// Meta returns a snapshot of the currently-active meta under db.mu.
func (db *DB) Meta() pager.Meta {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.currentMeta
}

// RetiredPagesLen reports how many prior-tx pages the transaction has
// retired so far (the set Commit publishes to the RPL). White-box
// probe for the per-op rollback tests: a failed row mutation must not
// leave retiredPages entries behind.
func (tx *Tx) RetiredPagesLen() int { return len(tx.pgr.RetiredPages()) }

// DirtyBytes reports the transaction's current slab budget usage.
// White-box probe for tests that need to stay clear of the
// MaxTxBufferBytes cap (e.g. to keep commit headroom while
// exercising per-op budget failures).
func (tx *Tx) DirtyBytes() int { return tx.pgr.DirtyBytes() }

// CommitReserveBytes reports the pager's live commit-time RPL
// reserve. Ops-phase admission stops at
// MaxTxBufferBytes − CommitReserveBytes; budget-edge tests compute
// effective headroom against that limit.
func (tx *Tx) CommitReserveBytes() int { return tx.pgr.CommitReserveBytes() }

// SetBeginReadPreAcquireHookForTest installs (or clears, with nil) the
// hook firing between BeginRead's first meta read and its reader-slot
// acquire. Returns a restore func for defer.
func SetBeginReadPreAcquireHookForTest(hook func()) (restore func()) {
	if hook == nil {
		beginReadPreAcquireHookForTest.Store(nil)
		return func() {}
	}
	beginReadPreAcquireHookForTest.Store(&hook)
	return func() { beginReadPreAcquireHookForTest.Store(nil) }
}

// SlotTxnID reports the pinned TxnID of this read transaction's
// reader slot (0 when running lock-free).
func (rtx *ReadTx) SlotTxnID() uint64 {
	if rtx.db.coord == nil {
		return 0
	}
	return rtx.db.coord.ReaderSlotTxnID(rtx.readerSlot)
}

// SetCheckpointStepHookForTest injects an error at Checkpoint's
// step 2, 3, or 4 (the hook receives the step number and returns the
// error to simulate for that step; nil return = no failure). Returns
// a restore func.
func SetCheckpointStepHookForTest(hook func(step int) error) (restore func()) {
	if hook == nil {
		checkpointStepHookForTest.Store(nil)
		return func() {}
	}
	checkpointStepHookForTest.Store(&hook)
	return func() { checkpointStepHookForTest.Store(nil) }
}

// PgrForTest exposes the writer pager for white-box fault injection.
func (db *DB) PgrForTest() *pager.Pager { return db.pgr }

// SetSyncDirHookForTest observes (and optionally injects a failure
// into) syncDir calls, after the real directory fsync succeeded.
func SetSyncDirHookForTest(hook func(dir string) error) (restore func()) {
	if hook == nil {
		syncDirHookForTest.Store(nil)
		return func() {}
	}
	syncDirHookForTest.Store(&hook)
	return func() { syncDirHookForTest.Store(nil) }
}

// SetMaintDetectHookForTest installs the hook firing inside
// maintReclaimLeaks's detection window. Returns a restore func.
func SetMaintDetectHookForTest(hook func()) (restore func()) {
	if hook == nil {
		maintDetectHookForTest.Store(nil)
		return func() {}
	}
	maintDetectHookForTest.Store(&hook)
	return func() { maintDetectHookForTest.Store(nil) }
}

// MaintReclaimLeaksForTest drives one leak-reclamation pass directly,
// reporting (freed, discarded).
func (db *DB) MaintReclaimLeaksForTest(ctx context.Context) (int, bool) {
	return db.maintReclaimLeaks(ctx)
}

// SetMaintPreReclaimHookForTest installs the hook firing between
// maintReclaimLeaks's detection and reclamation phases.
func SetMaintPreReclaimHookForTest(hook func()) (restore func()) {
	if hook == nil {
		maintPreReclaimHookForTest.Store(nil)
		return func() {}
	}
	maintPreReclaimHookForTest.Store(&hook)
	return func() { maintPreReclaimHookForTest.Store(nil) }
}

// RPLEntriesPerSegmentForTest exposes the per-segment RPL entry
// capacity for the tx's page geometry — budget-edge tests use it to
// land retired-page counts exactly on segment boundaries.
func RPLEntriesPerSegmentForTest(tx *Tx) int {
	return pager.RPLEntriesPerSegment(tx.pgr.Config())
}

// SetWriterFileOpsForTest installs a FileOps seam on the DB's stable
// single-writer pager and returns a restore closure. The crash-consistency
// harness wraps FileOpsForTest() in a recorder to log the write +
// fdatasync trace produced by the workload's commits, then synthesizes
// crash images from that trace.
func (db *DB) SetWriterFileOpsForTest(fops pager.FileOps) (restore func()) {
	return db.pgr.SetFileOpsForTest(fops)
}

// WriterFileOpsForTest returns the DB writer pager's current FileOps (the
// production forward unless already swapped) so a harness recorder can
// forward real I/O to it while logging.
func (db *DB) WriterFileOpsForTest() pager.FileOps {
	return db.pgr.FileOpsForTest()
}

// FirstDataPageForTest returns the id of the first data page (past the two
// meta slots and the bitmap region) for the DB's current geometry — the
// crash harness uses it to target intra-page tears at data pages only.
func (db *DB) FirstDataPageForTest() uint64 {
	return 2 + uint64(db.currentMeta.BitmapPages)
}
