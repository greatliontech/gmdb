package pager

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

var errInjectedIO = errors.New("pager: injected I/O fault (test)")

// countingFaultOps is a test FileOps that forwards to inner but can fail
// every pwrite or every fdatasync. It is the minimal fault surface the
// runtime commit-path seam exposes; the crash harness (Phase 2) builds
// richer write-recording and torn-write modes on the same seam.
type countingFaultOps struct {
	inner      FileOps
	writes     atomic.Int64
	failWrites bool // every WriteAt returns errInjectedIO
	failSync   bool // every Fdatasync returns errInjectedIO
	syncCalls  atomic.Int64
}

func (c *countingFaultOps) WriteAt(p []byte, off int64) (int, error) {
	c.writes.Add(1)
	if c.failWrites {
		return 0, errInjectedIO
	}
	return c.inner.WriteAt(p, off)
}

func (c *countingFaultOps) ReadAt(p []byte, off int64) (int, error) { return c.inner.ReadAt(p, off) }
func (c *countingFaultOps) Truncate(size int64) error               { return c.inner.Truncate(size) }

func (c *countingFaultOps) Fdatasync() error {
	c.syncCalls.Add(1)
	if c.failSync {
		return errInjectedIO
	}
	return c.inner.Fdatasync()
}

// recordingOps forwards to inner and records each pwrite offset and the
// fdatasync count. Not concurrency-safe — the pager is single-threaded.
type recordingOps struct {
	inner  FileOps
	writes []int64
	syncs  int
}

func (r *recordingOps) WriteAt(p []byte, off int64) (int, error) {
	r.writes = append(r.writes, off)
	return r.inner.WriteAt(p, off)
}
func (r *recordingOps) ReadAt(p []byte, off int64) (int, error) { return r.inner.ReadAt(p, off) }
func (r *recordingOps) Truncate(size int64) error               { return r.inner.Truncate(size) }
func (r *recordingOps) Fdatasync() error {
	r.syncs++
	return r.inner.Fdatasync()
}

// TestFileOpsSeamRecordsAllCommitIO pins the COMPLETENESS of the seam on
// the always-executed commit I/O: a SyncBoth commit of one new data page
// must route its data pwrite (step 1), meta pwrite (step 3), and BOTH
// fdatasync barriers (steps 2+4) through FileOps. Any durability op that
// bypasses the seam is a write (or barrier) the Phase 2 crash log would
// miss. Reverting any of those four call sites to raw p.file drops its
// region (or a sync) from the record and fails this test.
//
// The step-1 BITMAP pwrite (commit.go, bitmap.DirtyPages loop) is NOT
// exercised here: at the pager-unit level a freed page is reused via the
// loose/RPL path without rewriting the on-disk bitmap — a bitmap pwrite
// only fires under genuine RPL→bitmap reclamation, which the Phase 2
// crash harness drives at the DB level (it shares the identical
// p.fops.WriteAt call shape as the data pwrite pinned here).
func TestFileOpsSeamRecordsAllCommitIO(t *testing.T) {
	f, db, cleanup := initDB(t, true)
	defer cleanup()
	p := db.Pager
	ps := int64(testPageSize)

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	buf, err := p.AllocSlab(id)
	if err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	page.WriteHeader(buf, page.TypeLeaf, 1, 0)
	p.SetCurrentTxnID(1)

	rec := &recordingOps{inner: osFileOps{f: f}}
	restore := p.SetFileOpsForTest(rec)
	_, err = p.Commit(CommitParams{NewTxnID: 1, Flags: db.Meta.Flags, Sync: SyncBoth}, db.Meta, db.ActiveMetaIdx)
	restore()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var meta, data bool
	for _, off := range rec.writes {
		if off/ps < 2 { // meta slots are pages 0 and 1
			meta = true
		} else {
			data = true
		}
	}
	if !meta {
		t.Error("no meta-region pwrite through the seam — step 3 (meta publish) bypassed")
	}
	if !data {
		t.Error("no data-region pwrite through the seam — step 1 data bypassed")
	}
	if rec.syncs != 2 {
		t.Errorf("fdatasync count through seam = %d, want 2 (steps 2+4) — a bypassed barrier is a lost sync point in the crash log", rec.syncs)
	}
}

// TestFileOpsSeamFaultsCommitPwrite: a FileOps that fails every pwrite
// makes Commit fail before publication — the seam is genuinely on the
// commit write path, and a reopen sees the unpublished genesis meta
// (TxnID 0), confirming step 3 never landed.
func TestFileOpsSeamFaultsCommitPwrite(t *testing.T) {
	f, db, cleanup := initDB(t, true)
	defer cleanup()
	p := db.Pager

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	buf, err := p.AllocSlab(id)
	if err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	page.WriteHeader(buf, page.TypeLeaf, 1, 0)
	p.SetCurrentTxnID(1)

	fops := &countingFaultOps{inner: osFileOps{f: f}, failWrites: true}
	restore := p.SetFileOpsForTest(fops)
	_, err = p.Commit(CommitParams{NewTxnID: 1, Flags: db.Meta.Flags, Sync: SyncBoth}, db.Meta, db.ActiveMetaIdx)
	restore()
	if !errors.Is(err, errInjectedIO) {
		t.Fatalf("Commit over failing pwrite = %v, want errInjectedIO", err)
	}
	if fops.writes.Load() == 0 {
		t.Fatal("seam not exercised: no WriteAt reached the fault ops")
	}

	// No publication: reopen sees genesis TxnID 0.
	_ = p.Close()
	pool := NewBufPool(testPageSize)
	db2 := openAttachedForTest(t, f, OpenParams{Pool: pool, MaxTxBufferBytes: 16 << 20})
	defer db2.Pager.Close()
	if db2.Meta.TxnID != 0 {
		t.Errorf("after faulted commit, on-disk TxnID = %d, want 0 (unpublished)", db2.Meta.TxnID)
	}
}

// TestFileOpsSeamFaultsFdatasync: a FileOps that fails every fdatasync
// makes a SyncBoth Commit fail at the step-2 data barrier — the seam is on
// the fdatasync path, not only the pwrite path.
func TestFileOpsSeamFaultsFdatasync(t *testing.T) {
	f, db, cleanup := initDB(t, true)
	defer cleanup()
	p := db.Pager

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	buf, err := p.AllocSlab(id)
	if err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	page.WriteHeader(buf, page.TypeLeaf, 1, 0)
	p.SetCurrentTxnID(1)

	fops := &countingFaultOps{inner: osFileOps{f: f}, failSync: true}
	restore := p.SetFileOpsForTest(fops)
	_, err = p.Commit(CommitParams{NewTxnID: 1, Flags: db.Meta.Flags, Sync: SyncBoth}, db.Meta, db.ActiveMetaIdx)
	restore()
	if !errors.Is(err, errInjectedIO) {
		t.Fatalf("Commit over failing fdatasync = %v, want errInjectedIO", err)
	}
	if fops.syncCalls.Load() == 0 {
		t.Fatal("seam not exercised: Fdatasync never reached the fault ops")
	}
}
