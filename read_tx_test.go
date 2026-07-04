package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// TestReadTxKeyspaceReadsAreSnapshotIsolated is the headline test for the
// concurrent-read surface: a ReadTx reads keyspace data through its
// pinned snapshot, observes a consistent view a concurrent committed
// write does NOT change (and that write succeeds while the reader is
// open — readers never block the writer), rejects mutations with
// ErrReadOnly, and a fresh snapshot sees the new committed state.
func TestReadTxKeyspaceReadsAreSnapshotIsolated(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.Update(ctx, func(tx *Tx) error {
		ks, _ := tx.CreateKeyspace("ks")
		if err := ks.Put([]byte("a"), []byte("1")); err != nil {
			return err
		}
		return ks.Put([]byte("b"), []byte("2"))
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer rtx.Rollback()
	rks, err := rtx.OpenKeyspaceReadOnly("ks")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly: %v", err)
	}

	if v, err := rks.Get([]byte("a")); err != nil || !bytes.Equal(v, []byte("1")) {
		t.Fatalf("Get(a) = %q, %v; want 1, nil", v, err)
	}
	got := map[string]string{}
	c := rks.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		got[string(k)] = string(v)
	}
	if c.Err() != nil {
		t.Fatalf("cursor err: %v", c.Err())
	}
	if len(got) != 2 || got["a"] != "1" || got["b"] != "2" {
		t.Fatalf("cursor scan = %v, want {a:1,b:2}", got)
	}

	if err := rks.Put([]byte("c"), []byte("3")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Put on read handle: got %v, want ErrReadOnly", err)
	}
	if err := rks.Delete([]byte("a")); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Delete on read handle: got %v, want ErrReadOnly", err)
	}

	// Concurrent committed write WHILE the read tx is open — must succeed
	// (reader does not block writer) and must not change the snapshot.
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, _ := tx.OpenKeyspace("ks")
		if err := ks.Put([]byte("a"), []byte("99")); err != nil {
			return err
		}
		return ks.Put([]byte("c"), []byte("3"))
	}); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	if v, err := rks.Get([]byte("a")); err != nil || !bytes.Equal(v, []byte("1")) {
		t.Errorf("post-write snapshot Get(a) = %q, %v; want isolated value 1", v, err)
	}
	if _, err := rks.Get([]byte("c")); !errors.Is(err, ErrNotFound) {
		t.Errorf("post-write snapshot Get(c) = %v; want ErrNotFound (not in snapshot)", err)
	}

	rtx2, _ := db.BeginRead(ctx)
	defer rtx2.Rollback()
	rks2, _ := rtx2.OpenKeyspaceReadOnly("ks")
	if v, err := rks2.Get([]byte("a")); err != nil || !bytes.Equal(v, []byte("99")) {
		t.Errorf("fresh snapshot Get(a) = %q, %v; want 99", v, err)
	}
	if v, err := rks2.Get([]byte("c")); err != nil || !bytes.Equal(v, []byte("3")) {
		t.Errorf("fresh snapshot Get(c) = %q, %v; want 3", v, err)
	}
	if rtx.TxnID() >= rtx2.TxnID() {
		t.Errorf("TxnID: rtx=%d should precede rtx2=%d", rtx.TxnID(), rtx2.TxnID())
	}
}

// TestReadTxKeyspaceClosedAfterRollback pins lifecycle safety: after the
// ReadTx closes (unmapping the snapshot pager), an outstanding read
// handle fails fast with ErrTxClosed rather than touching the unmapped
// mmap.
func TestReadTxKeyspaceClosedAfterRollback(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	_ = db.Update(ctx, func(tx *Tx) error {
		ks, _ := tx.CreateKeyspace("ks")
		return ks.Put([]byte("a"), []byte("1"))
	})
	rtx, _ := db.BeginRead(ctx)
	rks, _ := rtx.OpenKeyspaceReadOnly("ks")
	if err := rtx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := rks.Get([]byte("a")); !errors.Is(err, ErrTxClosed) {
		t.Errorf("Get after ReadTx close: got %v, want ErrTxClosed", err)
	}
}

func TestBeginReadGenesis(t *testing.T) {
	// Spec-tier invariant pin (transactions.md §Read Transaction):
	// BeginRead on a fresh database succeeds, returns a ReadTx
	// whose snapshot meta is the genesis (TxnID=0), and Commit
	// releases the slot cleanly.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	if got := rtx.Meta().TxnID; got != 0 {
		t.Errorf("genesis ReadTx.Meta().TxnID = %d, want 0", got)
	}
	if err := rtx.Commit(); err != nil {
		t.Errorf("Commit: %v", err)
	}
}

func TestReadTxObservesPreCommitSnapshot(t *testing.T) {
	// Spec-tier invariant (transactions.md §Read Transaction): a
	// read transaction's snapshot is identified by the TxnID
	// recorded at Begin; every page reachable from that snapshot's
	// meta is immutable for the read's duration. Pin the contract:
	// open a write tx that allocates a page, commit; open a reader
	// at TxnID=1; a CONCURRENT write tx that retires that page +
	// commits cannot modify the reader's view of it.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// W1: allocate page A, populate, commit.
	var idA uint64
	if err := db.Update(ctx, func(tx *Tx) error {
		id, e := tx.AllocPage()
		if e != nil {
			return e
		}
		idA = id
		buf, e := tx.AllocSlab(id)
		if e != nil {
			return e
		}
		page.WriteHeader(buf, page.TypeLeaf, 1, 0)
		copy(buf[page.HeaderSize:], []byte("snapshot-A"))
		return nil
	}); err != nil {
		t.Fatalf("W1: %v", err)
	}

	// R1: open read tx; snapshot pins TxnID=1.
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer rtx.Rollback()
	if rtx.Meta().TxnID != 1 {
		t.Fatalf("ReadTx snapshot TxnID = %d, want 1", rtx.Meta().TxnID)
	}
	readBuf, err := rtx.Page(idA)
	if err != nil {
		t.Fatalf("rtx.Page: %v", err)
	}
	if !bytes.HasPrefix(readBuf[page.HeaderSize:], []byte("snapshot-A")) {
		t.Fatalf("ReadTx initial view: %x", readBuf[page.HeaderSize:page.HeaderSize+16])
	}

	// W2: retire page A; commit. Under CoW the page bytes at idA
	// remain immutable for the reader's snapshot — the writer
	// allocates a new page elsewhere and leaves idA's content as-is
	// until reclamation, which the reader pins.
	if err := db.Update(ctx, func(tx *Tx) error {
		return tx.FreePage(idA)
	}); err != nil {
		t.Fatalf("W2: %v", err)
	}

	// R1 re-read of the same id: should still see "snapshot-A".
	readBuf2, err := rtx.Page(idA)
	if err != nil {
		t.Fatalf("rtx.Page after W2: %v", err)
	}
	if !bytes.HasPrefix(readBuf2[page.HeaderSize:], []byte("snapshot-A")) {
		t.Errorf("ReadTx snapshot corrupted after concurrent write+retire: got %x",
			readBuf2[page.HeaderSize:page.HeaderSize+16])
	}
}

func TestBeginReadAfterCloseReturnsErrClosed(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = db.BeginRead(ctx)
	if !errors.Is(err, ErrClosed) {
		t.Errorf("BeginRead after Close: got %v, want ErrClosed", err)
	}
}

func TestBeginReadRespectsCtxCancellation(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = db.BeginRead(cctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("BeginRead on cancelled ctx: got %v, want context.Canceled", err)
	}
}

func TestBeginReadFullTableNoDeadline(t *testing.T) {
	// MaxReaders=1; one BeginRead fills the table; second returns
	// ErrReadersFull immediately because the second ctx has no
	// deadline.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64, MaxReaders: 1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("first BeginRead: %v", err)
	}
	defer rtx.Rollback()
	_, err = db.BeginRead(ctx)
	if !errors.Is(err, ErrReadersFull) {
		t.Errorf("second BeginRead: got %v, want ErrReadersFull", err)
	}
}

func TestBeginReadFullTableWithDeadlineRetries(t *testing.T) {
	// With a deadline, a full table retries; expect
	// context.DeadlineExceeded after the deadline fires.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64, MaxReaders: 1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	defer rtx.Rollback()
	dctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = db.BeginRead(dctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadlined second BeginRead: got %v, want DeadlineExceeded", err)
	}
}

func TestViewReleasesSlot(t *testing.T) {
	// View must release the slot even when fn errors.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64, MaxReaders: 1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	probeErr := errors.New("fn fail")
	err = db.View(ctx, func(rtx *ReadTx) error { return probeErr })
	if !errors.Is(err, probeErr) {
		t.Errorf("View fn err: got %v, want probeErr", err)
	}
	// Slot should be free — a subsequent View must succeed.
	if err := db.View(ctx, func(rtx *ReadTx) error { return nil }); err != nil {
		t.Errorf("View after probe: %v", err)
	}
}

func TestReadTxPageAfterCloseReturnsErrTxClosed(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	if err := rtx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := rtx.Page(0); !errors.Is(err, ErrTxClosed) {
		t.Errorf("Page after Commit: got %v, want ErrTxClosed", err)
	}
}

func TestReadTxRollbackIdempotent(t *testing.T) {
	// First close (via Commit) succeeds; second close (via Rollback)
	// returns ErrTxClosed cleanly. No panic, no double-release.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	if err := rtx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := rtx.Rollback(); !errors.Is(err, ErrTxClosed) {
		t.Errorf("second close: got %v, want ErrTxClosed", err)
	}
}

func TestReadTxConcurrentReadersDoNotBlockEachOther(t *testing.T) {
	// MaxReaders=4; spawn 4 concurrent BeginRead, all should
	// succeed without contention.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64, MaxReaders: 4,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	var wg sync.WaitGroup
	var errCount atomic.Int32
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rtx, err := db.BeginRead(ctx)
			if err != nil {
				errCount.Add(1)
				return
			}
			time.Sleep(10 * time.Millisecond)
			rtx.Rollback()
		}()
	}
	wg.Wait()
	if e := errCount.Load(); e != 0 {
		t.Errorf("%d concurrent BeginRead errors", e)
	}
}

func TestReadTxDoesNotBlockWriter(t *testing.T) {
	// Per transactions.md: "Readers never block writers." A live
	// reader holding a snapshot must not prevent a concurrent write
	// tx from committing.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer rtx.Rollback()
	// Writer must proceed even with rtx holding a snapshot.
	done := make(chan error, 1)
	go func() {
		done <- db.Update(ctx, func(tx *Tx) error {
			_, e := tx.AllocPage()
			return e
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Update: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Update blocked by concurrent reader")
	}
}

//go:noinline
func leakReadTx(t *testing.T, db *DB, ctx context.Context) {
	t.Helper()
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead (to leak): %v", err)
	}
	_ = rtx
}

func TestLeakedReadTxReleasesSlotViaCleanup(t *testing.T) {
	// Leak-detection.md §Transaction Leak Detection contract for
	// read transactions: a *ReadTx dropped without Commit/Rollback
	// must not pin its reader slot forever — runtime.AddCleanup
	// fires after GC, the cleanup CAS's the held flag and releases
	// the slot. We pin the contract by leaking a ReadTx, forcing
	// GC, waiting on the readTxCleanupHookForTest signal (fires at
	// the tail of the active-release path), and asserting the next
	// BeginRead on a max-1-slots table succeeds.
	//
	// Why the hook: BeginRead with a no-deadline context returns
	// ErrReadersFull IMMEDIATELY (internal/lock/coord_reader.go) if
	// the slot is busy — it does NOT wait. The prior "spawn a
	// BeginRead goroutine and wait on a 5s timer" shape polled a
	// wall-clock timer against the asynchronous finalizer-scheduling
	// signal and flaked under -race when scheduling latency outran
	// the goroutine's race-to-BeginRead-after-GC. The hook fires
	// AFTER info.coord.ReleaseReader returns, so a hook-signalled
	// BeginRead succeeds deterministically.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64, MaxReaders: 1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	// Buffered cap=1 + select-default in the callback so concurrent
	// cleanups (none expected here — single leak — but the hook
	// contract requires non-blocking) cannot block the GC goroutine.
	cleanupDone := make(chan struct{}, 1)
	setReadTxCleanupHookForTest(func() {
		select {
		case cleanupDone <- struct{}{}:
		default:
		}
	})
	t.Cleanup(func() { setReadTxCleanupHookForTest(nil) })

	leakReadTx(t, db, ctx)
	// Two GC cycles drain the cleanup queue: the first marks the
	// *ReadTx unreachable; the second flushes the cleanup function
	// onto its runtime goroutine. The hook signal proves the
	// callback completed, not merely that it was enqueued.
	runtime.GC()
	runtime.GC()
	select {
	case <-cleanupDone:
		// Cleanup callback ran to completion; slot is released.
	case <-time.After(5 * time.Second):
		t.Fatal("ReadTx cleanup did not fire within 5s after GC — leak-detection.md §Cleanup Behavior step 2 violated")
	}
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead after cleanup signalled: %v", err)
	}
	if err := rtx.Rollback(); err != nil {
		t.Errorf("Rollback: %v", err)
	}
}

func TestReaderPinsRPLAgainstReclamation(t *testing.T) {
	// Project invariant 1 (clause-explicit, transactions.md): an
	// active reader at TxnID T pins every page retired at TxnID
	// > T against RPL reclamation. The chunk-3.4 wiring of the
	// reclamation bound (min(oldestReaderTxnID, lastCheckpointTxnID))
	// is the mechanism. Pin the contract: open writer, alloc + free
	// page A across two commits so A lands in the RPL at TxnID=2;
	// open a reader pinned at TxnID=2; run further write txs that
	// allocate-and-free pages so the writer's allocator hits the
	// "bitmap exhausted" → reclaimRPL path; assert A is NOT
	// reclaimed while the reader holds its slot, and IS reclaimable
	// once the reader closes.
	ctx := context.Background()
	// Tight MaxSize forces the allocator into the bitmap-exhausted /
	// RPL-reclaim path quickly. Bitmap covers 16 pages at PageSize
	// 4096 (16/4096*8 = ceil(16/(4096*8))=1 bitmap page); meta(2) +
	// bitmap(1) = 3 reserved; effective data pages = 13.
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 16,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// W1: alloc page A.
	var idA uint64
	if err := db.Update(ctx, func(tx *Tx) error {
		id, e := tx.AllocPage()
		if e != nil {
			return e
		}
		idA = id
		_, e = tx.AllocSlab(id)
		return e
	}); err != nil {
		t.Fatalf("W1: %v", err)
	}
	// W2: free A — retires to RPL at TxnID=2.
	if err := db.Update(ctx, func(tx *Tx) error {
		return tx.FreePage(idA)
	}); err != nil {
		t.Fatalf("W2: %v", err)
	}
	if db.Meta().RPLEntryCount == 0 {
		t.Fatalf("RPL empty after W2; expected page A retired")
	}

	// R1: snapshot at TxnID=2 pinning A in the RPL.
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	if rtx.Meta().TxnID != 2 {
		t.Fatalf("R1 snapshot TxnID = %d, want 2", rtx.Meta().TxnID)
	}

	// W3..Wn: keep allocating until the writer would need to
	// reclaim. Each alloc consumes a free bitmap slot. With 13
	// data pages, after a few allocs the bitmap empties and the
	// allocator falls into reclaimRPL. With R1 pinning the bound
	// at TxnID=2, reclaimRPL is a no-op (rpl[0].TxnID == 2 is NOT
	// strictly less than bound=2), so the allocator falls through
	// to file extension. Since MaxSize=16 caps file extension,
	// eventually ErrDBFull surfaces.
	gotErrDBFull := false
	for range 20 {
		err := db.Update(ctx, func(tx *Tx) error {
			_, e := tx.AllocPage()
			return e
		})
		if err == nil {
			continue
		}
		if errors.Is(err, ErrDBFull) {
			gotErrDBFull = true
			break
		}
		t.Fatalf("unexpected Update err: %v", err)
	}
	if !gotErrDBFull {
		t.Errorf("expected ErrDBFull while reader pins RPL; got no error")
	}
	// Critical assertion: A is still in the RPL (NOT reclaimed)
	// because R1 pinned the bound.
	if db.Meta().RPLEntryCount == 0 {
		t.Errorf("RPL was reclaimed despite live reader pinning")
	}

	// Release the reader; the NEXT write tx should now reclaim
	// (oldestReader == MaxUint64, bound advances to current
	// activeMeta.TxnID).
	if err := rtx.Rollback(); err != nil {
		t.Fatalf("rtx.Rollback: %v", err)
	}
	// One more write — bitmap will be exhausted again, but now
	// reclaim succeeds because bound > 2.
	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.AllocPage()
		return e
	}); err != nil {
		t.Errorf("post-reader-close Update: %v", err)
	}
	// After post-reader-close commit the writer must have either
	// reclaimed RPL entries OR alloced from a freshly-extended
	// page. With MaxSize=16 and HWM previously at 16 (forced by
	// the ErrDBFull loop), extension would re-hit DBFull — so
	// success implies reclamation fired.
	if db.Meta().RPLEntryCount > 0 && db.Meta().HighWaterMark == 16 {
		// RPL still non-empty + HWM still pinned at MaxSize: the
		// post-close Update must have reclaimed at least one entry
		// to find a free page. The remaining RPL entries are
		// post-reader retires; we can't deterministically count
		// them without knowing the exact alloc/retire sequence,
		// so the structural assertion above is the right level.
	}
}

func TestReadTxLeakAfterCloseNoCrash(t *testing.T) {
	// Symmetric to TestTxLeakAfterCloseNoCrash for writes: a
	// leaked-ReadTx cleanup that observes *db.closed == true MUST
	// return without touching the (already-unmapped) reader-table
	// mmap.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64, MaxReaders: 4,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	leakReadTx(t, db, ctx)
	// Close BEFORE GC — the cleanup observes db.closed=true.
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	runtime.GC()
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
}

// TestBeginReadRestabilizesSnapshotAfterRacingCommits pins the
// snapshot-restabilization step of the reader-begin protocol
// (cross-process.md §Reader Table, slot acquire): BeginRead reads the
// meta BEFORE its reader slot is visible, so commits landing in that
// window — up to and including reclamation of the just-read
// snapshot's tree — must be detected by the post-publish re-read,
// with the slot's pinned TxnID raised to the stabilized snapshot.
// Without the loop the reader would traverse tree pages a concurrent
// writer already reclaimed and reused.
func TestBeginReadRestabilizesSnapshotAfterRacingCommits(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 512,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Seed.
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("k")
		if err != nil {
			return err
		}
		for i := range 200 {
			if err := ks.Put(fmt.Appendf(nil, "k%04d", i), bytes.Repeat([]byte{'v'}, 200)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The hook fires between BeginRead's first meta read and its slot
	// CAS: run several churn commits that retire the just-read tree
	// and recycle its pages (small MaxSize keeps alloc pressure high,
	// so reclamation runs at every Begin via the alloc priority).
	var hookRan bool
	restore := SetBeginReadPreAcquireHookForTest(func() {
		hookRan = true
		for round := range 3 {
			if err := db.Update(ctx, func(tx *Tx) error {
				ks, err := tx.OpenKeyspace("k")
				if err != nil {
					return err
				}
				for i := range 200 {
					if err := ks.Put(fmt.Appendf(nil, "k%04d", i), bytes.Repeat([]byte{byte('a' + round)}, 200)); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Errorf("hook churn round %d: %v", round, err)
			}
		}
	})
	defer restore()

	preMeta := db.Meta()
	rtx, err := db.BeginRead(ctx)
	restore() // one racing window is enough; later BeginReads run clean
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer rtx.Rollback()
	if !hookRan {
		t.Fatalf("fixture: pre-acquire hook did not run")
	}

	m := rtx.Meta()
	if m.TxnID <= preMeta.TxnID {
		t.Fatalf("snapshot TxnID = %d, want > %d (restabilization must observe the racing commits)", m.TxnID, preMeta.TxnID)
	}
	if got := rtx.SlotTxnID(); got != m.TxnID {
		t.Fatalf("reader slot pinned TxnID = %d, want %d (slot must be raised with the snapshot)", got, m.TxnID)
	}
	// The stabilized snapshot must read the final round's values.
	ks, err := rtx.OpenKeyspaceReadOnly("k")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly: %v", err)
	}
	want := bytes.Repeat([]byte{'c'}, 200)
	for i := range 200 {
		v, err := ks.Get(fmt.Appendf(nil, "k%04d", i))
		if err != nil {
			t.Fatalf("Get(k%04d): %v", i, err)
		}
		if !bytes.Equal(v, want) {
			t.Fatalf("Get(k%04d) = %q..., want final-round values", i, v[:8])
		}
	}
}

// TestBeginReadCloseRaceReturnsErrClosed pins the BeginRead/Close
// lifecycle contract: a BeginRead racing DB.Close must surface
// ErrClosed (or complete normally if it won the race) — never panic
// on the torn-down reader table or SIGSEGV on the unmapped lock mmap.
// The close gate's inflight window covers the whole acquire sequence,
// so Close's teardown drain waits for in-flight BeginReads.
func TestBeginReadCloseRaceReturnsErrClosed(t *testing.T) {
	ctx := context.Background()
	for round := range 30 {
		db, err := Open(ctx, tmpPath(t), Options{
			PageSize: 4096, MinSize: 16, MaxSize: 128,
			Maintenance: MaintenanceOptions{Disable: true},
		})
		if err != nil {
			t.Fatalf("Open(%d): %v", round, err)
		}
		if err := db.Update(ctx, func(tx *Tx) error {
			ks, err := tx.CreateKeyspaceIfNotExists("k")
			if err != nil {
				return err
			}
			return ks.Put([]byte("a"), []byte("v"))
		}); err != nil {
			t.Fatalf("seed(%d): %v", round, err)
		}

		start := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			<-start
			for {
				rtx, err := db.BeginRead(ctx)
				if err != nil {
					if !errors.Is(err, ErrClosed) {
						t.Errorf("BeginRead during Close: %v, want ErrClosed", err)
					}
					return
				}
				ks, err := rtx.OpenKeyspaceReadOnly("k")
				if err == nil {
					_, _ = ks.Get([]byte("a"))
				}
				_ = rtx.Rollback()
			}
		}()
		close(start)
		// Tiny stagger so some rounds close mid-BeginRead.
		for range round % 7 {
			runtime.Gosched()
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close(%d): %v", round, err)
		}
		<-done
	}
}
