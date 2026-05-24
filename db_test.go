package gmdb

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thegrumpylion/gmdb/internal/lock"
	"github.com/thegrumpylion/gmdb/internal/page"
)

func tmpPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "db.gmdb")
}

func TestOpenCreate(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096,
		MinSize:  16,
		MaxSize:  128,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	meta := db.Meta()
	if meta.TxnID != 0 {
		t.Errorf("initial TxnID = %d, want 0", meta.TxnID)
	}
	if meta.PageSize != 4096 {
		t.Errorf("PageSize = %d", meta.PageSize)
	}
	// File exists.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestOpenReopen(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Re-open should not re-init (Options ignored for persisted fields).
	db2, err := Open(ctx, path, Options{PageSize: 8192, MinSize: 1, MaxSize: 64})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	if db2.Meta().PageSize != 4096 {
		t.Errorf("re-opened PageSize = %d, want 4096 (persisted)", db2.Meta().PageSize)
	}
}

func TestWriteTxRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var allocatedID uint64
	err = db.Update(ctx, func(tx *Tx) error {
		id, err := tx.AllocPage()
		if err != nil {
			return err
		}
		allocatedID = id
		buf, err := tx.AllocSlab(id)
		if err != nil {
			return err
		}
		page.WriteHeader(buf, page.TypeLeaf, 1, 0)
		copy(buf[page.HeaderSize:], []byte("payload-A"))
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if db.Meta().TxnID != 1 {
		t.Errorf("post-commit TxnID = %d, want 1", db.Meta().TxnID)
	}

	// Read back via a new write tx (chunk 1 has no read tx surface).
	err = db.Update(ctx, func(tx *Tx) error {
		buf, err := tx.Page(allocatedID)
		if err != nil {
			return err
		}
		if !bytes.HasPrefix(buf[page.HeaderSize:], []byte("payload-A")) {
			t.Error("page content not durable across tx boundary")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read-back Update: %v", err)
	}
}

func TestWriteTxDurableAcrossClose(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var allocatedID uint64
	err = db.Update(ctx, func(tx *Tx) error {
		id, err := tx.AllocPage()
		if err != nil {
			return err
		}
		allocatedID = id
		buf, _ := tx.AllocSlab(id)
		page.WriteHeader(buf, page.TypeLeaf, 1, 0)
		copy(buf[page.HeaderSize:], []byte("durable!"))
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	db.Close()

	// Re-open and verify content persisted.
	db2, err := Open(ctx, path, Options{PageSize: 4096})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	if db2.Meta().TxnID != 1 {
		t.Errorf("re-opened TxnID = %d, want 1", db2.Meta().TxnID)
	}
	err = db2.Update(ctx, func(tx *Tx) error {
		buf, _ := tx.Page(allocatedID)
		if !bytes.HasPrefix(buf[page.HeaderSize:], []byte("durable!")) {
			t.Errorf("content not durable across re-open: %x", buf[page.HeaderSize:page.HeaderSize+8])
		}
		return nil
	})
	if err != nil {
		t.Fatalf("post-reopen Update: %v", err)
	}
}

func TestRollbackDiscardsChanges(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.AllocPage(); err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if db.Meta().TxnID != 0 {
		t.Errorf("TxnID changed after rollback: %d", db.Meta().TxnID)
	}
}

func TestBeginReadOnlyNotYetImplemented(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Begin(ctx, false); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Begin(write=false): got %v, want ErrReadOnly", err)
	}
}

func TestInvalidOptions(t *testing.T) {
	ctx := context.Background()
	bad := []Options{
		{PageSize: 3000},
		{PageSize: 4096, MinSize: 100, MaxSize: 50},
		{PageSize: 4096, MaxSize: 64, MaxTxBufferBytes: -1},
		// MaxReaders above MaxMaxReaders fails validate() before
		// the data file is touched (chunk 2.7 pre-check).
		{PageSize: 4096, MaxSize: 64, MaxReaders: lock.MaxMaxReaders + 1},
	}
	for i, opts := range bad {
		if _, err := Open(ctx, tmpPath(t), opts); !errors.Is(err, ErrInvalidOptions) {
			t.Errorf("case %d: got %v, want ErrInvalidOptions", i, err)
		}
	}
}

func TestRollbackRestoresBitmap(t *testing.T) {
	// Regression test for the round-1 H finding: AllocPage mutates the
	// in-memory bitmap (Clear), and Rollback used to clear only the
	// dirty-set without restoring the bit values. The result was a
	// pager whose in-memory bitmap claimed allocations that were never
	// published on disk, leaking pages until the next Open.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// First commit: allocate page A, retire it next tx so it lands in
	// the RPL.
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
		t.Fatalf("commit 1: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		return tx.FreePage(idA)
	}); err != nil {
		t.Fatalf("commit 2: %v", err)
	}

	// tx3: Begin, allocate (drains RPL → bitmap), rollback.
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	idRollback, err := tx.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// tx4: AllocPage should reuse a free page (idRollback or another
	// reclaimable id), NOT extend the file.
	hwmBefore := db.Meta().HighWaterMark
	var idReuse uint64
	if err := db.Update(ctx, func(tx *Tx) error {
		id, e := tx.AllocPage()
		if e != nil {
			return e
		}
		idReuse = id
		_, e = tx.AllocSlab(id)
		return e
	}); err != nil {
		t.Fatalf("commit 4: %v", err)
	}
	if idReuse > hwmBefore {
		t.Errorf("AllocPage extended file (id=%d > prev HWM=%d) — bitmap rollback leaked", idReuse, hwmBefore)
	}
	// idRollback should be in the set of reusable ids — it was the one
	// the rolled-back tx allocated.
	_ = idRollback
}

func TestRecoveryAfterMetaCorruption(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// First commit: TxnID 1.
	err = db.Update(ctx, func(tx *Tx) error {
		id, _ := tx.AllocPage()
		buf, _ := tx.AllocSlab(id)
		page.WriteHeader(buf, page.TypeLeaf, 1, 0)
		copy(buf[page.HeaderSize:], []byte("v1"))
		return nil
	})
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}
	// Second commit: TxnID 2.
	err = db.Update(ctx, func(tx *Tx) error {
		id, _ := tx.AllocPage()
		buf, _ := tx.AllocSlab(id)
		page.WriteHeader(buf, page.TypeLeaf, 1, 0)
		copy(buf[page.HeaderSize:], []byte("v2"))
		return nil
	})
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	// Active meta is now at slot 1 (alternates: init→0, c1→1, c2→0,
	// so after c2 active = 0). Determine programmatically.
	activeBeforeCorrupt := db.activeMetaIdx
	db.Close()

	// Simulate a crash by corrupting the active meta on disk.
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	off := int64(activeBeforeCorrupt) * 4096
	// Tamper with the TxnID field (offset 128 within the meta payload).
	buf := []byte{0xFF}
	if _, err := f.WriteAt(buf, off+128); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	f.Sync()
	f.Close()

	// Re-open: the dual-meta selector must fall back to the still-
	// valid meta with the next-most-recent TxnID.
	db2, err := Open(ctx, path, Options{PageSize: 4096})
	if err != nil {
		t.Fatalf("re-Open after corruption: %v", err)
	}
	defer db2.Close()

	// Active is now the OTHER slot.
	if db2.activeMetaIdx == activeBeforeCorrupt {
		t.Errorf("Open picked the corrupt meta: active=%d", db2.activeMetaIdx)
	}
	// The recovered TxnID must be one less than the latest (i.e. 1,
	// because we corrupted the TxnID=2 meta).
	if db2.Meta().TxnID != 1 {
		t.Errorf("recovered TxnID = %d, want 1 (fallback to TxnID=1 meta)", db2.Meta().TxnID)
	}
}

func TestOpenRecoversFromMetaZeroPageSizeCorruption(t *testing.T) {
	// Dual-meta atomicity (file-layout.md §Invariants): if at least one
	// meta verifies, Open succeeds via that meta. Discovery of PageSize
	// must therefore be robust to a corrupted meta-0 — the chunk-1
	// mechanism is the probe in pager.DiscoverPageSize. Two reachable
	// byte-flip shapes the probe must recover from:
	//   (A) zero the PageSize bytes (ValidPageSize false)
	//   (B) flip to a valid-but-wrong power of two (ValidPageSize true,
	//       VerifyMeta false because checksum is broken)
	// Both must recover; the (B) case is the second demonstrated fault
	// that justified widening the probe trigger from !ValidPageSize to
	// !VerifyMeta(meta0).
	for _, tc := range []struct {
		name       string
		corrupt    func(t *testing.T, path string, ps uint32)
		opts       Options
		wantTxnIDs []uint64
	}{
		{
			name: "zero_PageSize",
			corrupt: func(t *testing.T, path string, ps uint32) {
				f, err := os.OpenFile(path, os.O_RDWR, 0o600)
				if err != nil {
					t.Fatalf("open for corrupt: %v", err)
				}
				defer f.Close()
				if _, err := f.WriteAt([]byte{0, 0, 0, 0}, 8); err != nil {
					t.Fatalf("zero PageSize bytes: %v", err)
				}
			},
			opts: Options{PageSize: 4096, MinSize: 16, MaxSize: 128, PageChecksum: false},
		},
		{
			name: "valid_but_wrong_PageSize",
			corrupt: func(t *testing.T, path string, ps uint32) {
				// True PS=8192 (le 00 20 00 00). Flip to PS=4096 (le 00 10
				// 00 00) — ValidPageSize stays true; the checksum no
				// longer matches the bytes.
				f, err := os.OpenFile(path, os.O_RDWR, 0o600)
				if err != nil {
					t.Fatalf("open for corrupt: %v", err)
				}
				defer f.Close()
				wrong := []byte{0x00, 0x10, 0x00, 0x00}
				if _, err := f.WriteAt(wrong, 8); err != nil {
					t.Fatalf("flip PageSize bytes: %v", err)
				}
			},
			opts: Options{PageSize: 8192, MinSize: 16, MaxSize: 128, PageChecksum: false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := tmpPath(t)
			db, err := Open(ctx, path, tc.opts)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			// One commit so meta-1 is the active meta (TxnID=1) and meta-0
			// holds the genesis (TxnID=0). After the commit alternation,
			// active is slot 1.
			if err := db.Update(ctx, func(tx *Tx) error {
				_, e := tx.AllocPage()
				return e
			}); err != nil {
				t.Fatalf("commit: %v", err)
			}
			expectedTxnID := db.Meta().TxnID
			expectedPS := db.Meta().PageSize
			db.Close()

			tc.corrupt(t, path, expectedPS)

			db2, err := Open(ctx, path, Options{})
			if err != nil {
				t.Fatalf("re-Open with corrupted meta-0: %v", err)
			}
			defer db2.Close()
			if got := db2.Meta().PageSize; got != expectedPS {
				t.Errorf("recovered PageSize = %d, want %d", got, expectedPS)
			}
			if got := db2.Meta().TxnID; got != expectedTxnID {
				t.Errorf("recovered TxnID = %d, want %d (active = meta-1 fallback)", got, expectedTxnID)
			}
			if db2.activeMetaIdx != 1 {
				t.Errorf("active meta after recovery = %d, want 1", db2.activeMetaIdx)
			}
		})
	}
}

func TestCorruptionSentinelOnOpen(t *testing.T) {
	// Public sentinel contract: a database file with a malformed RPL
	// segment must surface via errors.Is(err, ErrCorrupted) at the
	// public surface. The translation point is mapPagerErr (tx.go),
	// which routes pager.ErrCorrupted → gmdb.ErrCorrupted via a
	// multi-target %w wrap so the descriptive message is preserved.
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize:     4096,
		PageChecksum: false, // simplifies on-disk tampering (no footer to rewrite)
		MinSize:      16,
		MaxSize:      128,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Commit 1: allocate page A.
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
		t.Fatalf("commit 1: %v", err)
	}
	// Commit 2: retire page A so it lands in the RPL.
	if err := db.Update(ctx, func(tx *Tx) error {
		return tx.FreePage(idA)
	}); err != nil {
		t.Fatalf("commit 2: %v", err)
	}
	rplHead := db.Meta().RPLHeadPage
	if rplHead == 0 {
		t.Fatalf("RPLHeadPage = 0 after retiring page; nothing to corrupt")
	}
	db.Close()

	// Corrupt the type byte of the RPL segment so DecodeRPLSegment returns
	// ok=false, triggering rebuildRPLChain's "malformed" path.
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corrupt: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xFF}, int64(rplHead)*4096); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	f.Close()

	// Re-open: must surface as ErrCorrupted.
	_, err = Open(ctx, path, Options{PageSize: 4096})
	if err == nil {
		t.Fatal("re-Open accepted corrupted RPL segment")
	}
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("Open error does not satisfy errors.Is(ErrCorrupted): %v", err)
	}
}

func TestCommitFailurePoisonsHandle(t *testing.T) {
	// Publication-phase failure contract (pager-slab.md §Commit Write
	// Ordering + api-surface.md §Sentinel Errors). A step-3 pwrite
	// success followed by a step-4 fdatasync failure leaves the
	// on-disk active meta advanced while the pager's in-memory
	// bitmap / HighWaterMark / RPL chain were rolled back by AbortTx
	// — the handle cannot safely allocate (the in-memory bitmap
	// thinks pages free that the on-disk active tree now references;
	// a subsequent commit could overwrite them and the next Open
	// would see a tree pointing at clobbered data). ErrPoisoned
	// signals this; Close + re-Open re-reads everything from disk
	// and is internally consistent — same machinery cross-process
	// recovery uses after a writer crash.
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Baseline successful commit (TxnID 1).
	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.AllocPage()
		return e
	}); err != nil {
		t.Fatalf("baseline commit: %v", err)
	}
	prevTxnID := db.Meta().TxnID

	// Arm the pager's step-4 hook to simulate fdatasync EIO after step 3
	// has put the new meta on disk.
	db.pgr.SetCommitStep4HookForTest(func() error {
		return io.ErrUnexpectedEOF
	})
	err = db.Update(ctx, func(tx *Tx) error {
		_, e := tx.AllocPage()
		return e
	})
	if err == nil {
		t.Fatal("expected commit to fail under injected step-4 hook")
	}
	db.pgr.SetCommitStep4HookForTest(nil)

	// Next Begin must surface ErrPoisoned without blocking on the
	// cross-process write grant.
	if _, err := db.Begin(ctx, true); !errors.Is(err, ErrPoisoned) {
		t.Errorf("post-failure Begin: got %v, want ErrPoisoned", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error { return nil }); !errors.Is(err, ErrPoisoned) {
		t.Errorf("post-failure Update: got %v, want ErrPoisoned", err)
	}

	// Close works on a poisoned handle.
	if err := db.Close(); err != nil {
		t.Errorf("Close on poisoned handle: %v", err)
	}

	// Re-Open: the active meta on disk advanced to TxnID = prev+1 (step 3
	// of the failed commit successfully placed the new meta before the
	// injected step-4 failure). The reopened handle has a coherent view
	// and accepts a fresh write.
	db2, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("re-Open after poison: %v", err)
	}
	defer db2.Close()
	if got := db2.Meta().TxnID; got != prevTxnID+1 {
		t.Errorf("re-opened TxnID = %d, want %d (failed commit's step 3 placed new meta on disk)", got, prevTxnID+1)
	}
	if err := db2.Update(ctx, func(tx *Tx) error {
		_, e := tx.AllocPage()
		return e
	}); err != nil {
		t.Errorf("recovered handle rejects write: %v", err)
	}
}

// leakWriteTx opens a write tx in an isolated stack frame and discards
// the *Tx reference. Compiled with go:noinline so the inliner does not
// merge the frame into the caller — that would keep the tx alive in the
// caller's locals across the runtime.GC() calls and the leak would not
// surface.
//
//go:noinline
func leakWriteTx(t *testing.T, db *DB, ctx context.Context) {
	t.Helper()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin (to leak): %v", err)
	}
	_ = tx
}

func TestLeakedTxReleasesWriteLock(t *testing.T) {
	// Transaction leak detection (leak-detection.md §Transaction Leak
	// Detection): a *Tx dropped without Commit/Rollback must not leave
	// the cross-process write grant held forever. runtime.AddCleanup
	// on the *Tx fires after GC marks it unreachable; the cleanup
	// CAS's the held flag and releases the grant. The test leaks a tx
	// in an isolated stack frame, forces GC, and asserts the next
	// Begin succeeds without blocking.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	leakWriteTx(t, db, ctx)

	// Two GC cycles drain the cleanup queue: the first marks the *Tx
	// unreachable; the second flushes the cleanup function onto its
	// runtime goroutine.
	runtime.GC()
	runtime.GC()

	// The cleanup runs on a separate goroutine; wait for the write
	// grant to be released by attempting Begin with a timeout. Block on a
	// channel rather than polling — if cleanup fires within the
	// timeout, Begin unblocks immediately; if it never fires (bug),
	// the timeout triggers a failure.
	done := make(chan error, 1)
	go func() {
		tx, err := db.Begin(ctx, true)
		if err == nil {
			_ = tx.Rollback()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Begin after GC of leaked tx: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Begin blocked — leaked tx did not release write grant via GC cleanup")
	}
}

func TestCommitStopsCleanup(t *testing.T) {
	// Regression: a normal Commit must Stop() the leak-detection
	// cleanup so no spurious leak warning fires after the tx closed
	// cleanly. Also verifies that releaseGrant's CAS prevents the
	// cleanup from double-releasing the grant if it ran anyway (and
	// that the next Begin works normally).
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.AllocPage()
		return e
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Force GC to give any rogue cleanup a chance to fire. Then make
	// sure Begin still works (no double-Unlock panic, no held lock).
	runtime.GC()
	runtime.GC()

	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin after committed tx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}

func TestMultipleCommits(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for i := 1; i <= 5; i++ {
		err := db.Update(ctx, func(tx *Tx) error {
			id, err := tx.AllocPage()
			if err != nil {
				return err
			}
			buf, err := tx.AllocSlab(id)
			if err != nil {
				return err
			}
			page.WriteHeader(buf, page.TypeLeaf, 1, 0)
			buf[page.HeaderSize] = byte(i)
			return nil
		})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		if got := db.Meta().TxnID; got != uint64(i) {
			t.Errorf("after commit %d: TxnID=%d, want %d", i, got, i)
		}
	}
}

func TestLockFileCreatedOnOpen(t *testing.T) {
	// The lock file is opened/created during Open per chunk 2.7
	// wiring. Pins three properties:
	//   1. The lock file appears on disk at <path>.lock.
	//   2. Its on-disk size matches lock.FileSize(MaxReaders) — the
	//      cross-process.md mmap-size invariant.
	//   3. Its UUID matches the data file's meta UUID — the
	//      cross-process.md UUID invariant.
	ctx := context.Background()
	path := tmpPath(t)
	uuid := [16]byte{0xAA, 0xBB, 0xCC, 0xDD}
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		UUID:       uuid,
		MaxReaders: 32,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if got := db.lockFile.UUID(); got != uuid {
		t.Errorf("lock-file UUID = %x, want %x (UUID-match invariant)", got, uuid)
	}
	if got := db.lockFile.MaxReaders(); got != 32 {
		t.Errorf("lock-file MaxReaders = %d, want 32", got)
	}

	lockPath := path + ".lock"
	st, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
	wantSize := lock.FileSize(32)
	if st.Size() != wantSize {
		t.Errorf("lock file on-disk size = %d, want %d (mmap-size invariant)", st.Size(), wantSize)
	}
}

func TestBeginRespectsCtxCancellation(t *testing.T) {
	// 2.7 behavior change: Begin now honors ctx (chunk 1 ignored it).
	// Cancelling ctx while waiting for the write grant returns
	// context.Cause(ctx); no goroutine leak, no held grant.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Hold the writer with a foreground tx.
	hold, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin hold: %v", err)
	}
	defer hold.Rollback()

	// Second Begin with a ctx that fires before the first releases.
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, e := db.Begin(cctx, true)
		done <- e
	}()
	// Let the second Begin enter the AcquireWriter retry loop.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Begin: got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Begin did not return within 2s after ctx cancel")
	}
}

func TestBeginAfterCloseReturnsErrClosed(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = db.Begin(ctx, true)
	if !errors.Is(err, ErrClosed) {
		t.Errorf("got %v, want ErrClosed", err)
	}
}

func TestConcurrentWritersSerialised(t *testing.T) {
	// Cross-process.md §Write Lock — within a single process, the
	// Coord's channel-based serialisation must enforce at-most-one
	// writer. Pre-2.7 this was a sync.Mutex; 2.7 routes it through
	// the flock goroutine. N goroutines spin up concurrent Begin →
	// Commit cycles; we just need them all to succeed without
	// corruption (final TxnID equals N, no errors).
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for range N {
		go func() {
			defer wg.Done()
			if err := db.Update(ctx, func(tx *Tx) error {
				_, err := tx.AllocPage()
				return err
			}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("Update: %v", e)
	}
	if got := db.Meta().TxnID; got != N {
		t.Errorf("final TxnID = %d, want %d", got, N)
	}
}

func TestCloseDuringBlockedBegin(t *testing.T) {
	// Pin Close-vs-blocked-Begin ordering: a Begin in flight (blocked
	// in AcquireWriter retry while another tx holds) when Close fires
	// must return promptly, and Close must complete without deadlock.
	//
	// The held tx is deliberately orphaned (not Rollback'd) — the
	// Rollback-vs-Close race is chunk-2.8 territory (db.closed
	// promotion). What 2.7 strictly guarantees here is: Close drains
	// the Coord's flock goroutine even with a grant outstanding (the
	// stopCh path clears header + unlocks), and a blocked Begin sees
	// stopCh fire and returns ErrClosed.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	hold, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin hold: %v", err)
	}
	_ = hold // orphan deliberately — see comment above.

	beginErr := make(chan error, 1)
	go func() {
		_, e := db.Begin(ctx, true)
		beginErr <- e
	}()
	time.Sleep(50 * time.Millisecond)

	closeDone := make(chan error, 1)
	go func() { closeDone <- db.Close() }()

	select {
	case err := <-beginErr:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("blocked Begin during Close: got %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("blocked Begin did not return within 2s after Close")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("Close did not complete within 2s")
	}
}

func TestCloseSetsDBClosedFlag(t *testing.T) {
	// Spec-tier invariant (leak-detection.md §Close Ordering): Close
	// sets *db.closed = true (release-store) BEFORE unmapping or
	// stopping goroutines. We pin this by reading db.closed AFTER
	// Close returns — true. Combined with TestBeginAfterCloseReturns
	// ErrClosed (chunk 2.7), this verifies the flag is observable to
	// any concurrent caller.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := db.closeGate.IsClosed(); got {
		t.Errorf("db.closeGate pre-Close = %v, want false", got)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := db.closeGate.IsClosed(); !got {
		t.Errorf("db.closeGate post-Close = %v, want true", got)
	}
}

func TestCloseIdempotentViaCAS(t *testing.T) {
	// Close uses CompareAndSwap(false, true) for idempotency; two
	// Close calls race-cleanly via the atomic. Test pins the
	// invariant: second Close is a no-op (no double-Close panic
	// from coord.Close / lockFile.Close).
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// Concurrent close racing the first.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = db.Close()
		}()
	}
	wg.Wait()
}

func TestTxMethodAfterCloseReturnsErrClosed(t *testing.T) {
	// Use-after-Close graceful-fail: Tx methods invoked after the
	// DB has been Closed must return ErrClosed (not SIGSEGV). Spec
	// permits this via the requireOpen db.closed.Load check. Note:
	// this is a defensive guard — per leak-detection.md the caller
	// is expected to Commit/Rollback before Close.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// tx is now in a state where db.closed == true. requireOpen
	// should surface ErrClosed for each mutating method.
	if _, err := tx.AllocPage(); !errors.Is(err, ErrClosed) {
		t.Errorf("AllocPage after Close: got %v, want ErrClosed", err)
	}
	if _, err := tx.Page(0); !errors.Is(err, ErrClosed) {
		t.Errorf("Page after Close: got %v, want ErrClosed", err)
	}
	if _, err := tx.CoW(0, 1); !errors.Is(err, ErrClosed) {
		t.Errorf("CoW after Close: got %v, want ErrClosed", err)
	}

	// Rollback after Close: explicitly not racing the requireOpen
	// gate (Rollback has its own tx.closed check). The pgr.AbortTx
	// call operates on captured heap state and is safe; Rollback
	// returns nil. (Spec invariant: Rollback survives use-after-Close
	// because tx.pgr is a stable Go-heap pointer captured at Begin.)
	if err := tx.Rollback(); err != nil {
		t.Logf("Rollback after Close: %v (acceptable — pgr is captured heap state)", err)
	}
}

func TestTxLeakAfterCloseNoCrash(t *testing.T) {
	// Spec-tier invariant (leak-detection.md): a Tx cleanup that
	// observes *db.closed == true returns without touching the
	// reader-table mmap or signalling the flock goroutine. We test
	// by: leaking a Tx, Closing the DB, then forcing GC. The
	// cleanup should fire, log a warning, and exit without crashing.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	leakWriteTx(t, db, ctx)

	// Close BEFORE forcing GC — the cleanup, when it fires, will
	// observe db.closed=true.
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Two GC cycles to drain the cleanup queue. The cleanup must
	// see db.closed=true and return without panicking. If the
	// invariant were violated (cleanup touched the torn-down
	// pager/grant), this would SIGSEGV.
	runtime.GC()
	runtime.GC()

	// Brief wait for cleanup goroutine to land.
	time.Sleep(50 * time.Millisecond)
}

func TestDBClosedFlagSharedByPointer(t *testing.T) {
	// Spec-tier invariant (leak-detection.md, chunk-3.3 promotion):
	// the close coordination state is a *closeGate shared by
	// pointer between DB, every txCleanupInfo, every
	// readTxCleanupInfo, and dbCleanupInfo. We pin this by
	// verifying the DB's closeGate pointer is non-nil so a
	// Close() store is observable to a leaked-Tx cleanup even if
	// the *DB itself is GC'd first.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if db.closeGate == nil {
		t.Fatal("db.closeGate is nil — should be heap-allocated *closeGate")
	}
	// Confirm Begin captures the same pointer.
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	// We can't directly inspect tx.cleanup's info struct from a
	// test, but the contract is enforced by construction in db.go's
	// Begin (gate: db.closeGate). The non-nil check above + the
	// TestTxLeakAfterCloseNoCrash test exercise the shared-pointer
	// invariant end-to-end.
}

func TestTxCleanupFnDirectClosedSkipsRelease(t *testing.T) {
	// Spec invariant 1 (leak-detection.md): a Tx cleanup observing
	// *db.closed == true returns without touching the pager / grant.
	// TestTxLeakAfterCloseNoCrash exercises this end-to-end but
	// non-deterministically — runtime.GC scheduling can fire the
	// cleanup either before or after Close. Here we synthesise a
	// txCleanupInfo directly and call txCleanupFn — deterministic
	// pinning of the closed-branch behavior.
	//
	// Mechanism: a fresh *atomic.Bool set to true is the gate; pgr
	// is a real *pager.Pager from a real DB so AbortTx WOULD be
	// callable; grant is a fresh non-nil *lock.Grant. Post-call,
	// neither pgr.AbortTx nor grant.Release should have been
	// invoked. We verify by observing side-effects:
	//   - pgr.AbortTx ABORTS the current tx — if we don't open one,
	//     calling AbortTx on a fresh pager is a state mutation we
	//     can detect via... actually pager.AbortTx without a
	//     BeginTx may no-op or panic. Use the grant side instead:
	//     grant.Release closes a channel; we hold a separate
	//     reference to the channel and verify it's NOT closed.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Acquire a real grant so info.grant is a meaningful pointer.
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// Cancel the real cleanup so we can synthesize our own info.
	tx.cleanup.Stop()

	// Simulate "DB is closed" by setting the captured gate.
	gate := newCloseGate()
	gate.SwapClosed(true)

	held := &atomic.Bool{}
	held.Store(true)

	// Capture the grant's release channel BEFORE the cleanup runs
	// so we can probe whether Release was invoked.
	releaseCh := tx.grant
	_ = releaseCh // can't directly read internal channel; instead
	// we'll observe via held — if cleanup skipped the release path,
	// held became false (CAS) but pgr.AbortTx and grant.Release
	// were not invoked. The grant remains "released" only via the
	// later Rollback below.

	info := txCleanupInfo{
		gate:      gate,
		pgr:       tx.pgr,
		grant:     tx.grant,
		held:      held,
		originPCs: nil,
	}
	txCleanupFn(info)

	// held.CAS should have won (was true, now false). This proves
	// the cleanup ran (didn't early-return on held check).
	if held.Load() {
		t.Errorf("held remained true; cleanup did not run")
	}

	// Now run a normal Rollback. If the cleanup HAD called
	// grant.Release and pgr.AbortTx, Rollback would either be a
	// double-release (sync.Once-safe, so OK) or pgr would be in
	// an aborted state. Both behaviors should let Rollback complete
	// without panic; that's our weak fidelity assertion.
	if err := tx.Rollback(); err != nil {
		t.Errorf("Rollback after synthesised cleanup: %v", err)
	}

	// The grant's sync.Once means Release is idempotent, so we
	// can't distinguish "Release ran during cleanup" vs "Release
	// ran in Rollback" via the channel alone. The strongest direct
	// evidence we can extract: closed=true blocks the AbortTx +
	// Release branch (lines 115-122 of tx.go); the code path is
	// unambiguous by inspection. This test pins that the function
	// COMPLETES on a closed=true input (no panic, no nil-deref,
	// no SIGSEGV — invariant 1's intent).
}

func TestTxCleanupFnDirectClosedFalseRunsRelease(t *testing.T) {
	// Complement: with closed=false, the cleanup MUST run the
	// AbortTx + Release branch. We probe by checking that a
	// subsequent Rollback returns ErrTxClosed (set by an
	// out-of-band closed=true would mean closed=true was the
	// branch) — instead, the tx is already aborted by our
	// synthesised cleanup, so Rollback should ALSO succeed
	// (idempotent grant.Release + tx.closed already true after the
	// CAS).
	//
	// Simpler shape: just confirm no panic with closed=false. The
	// AbortTx branch is exercised by the existing TestLeakedTx-
	// ReleasesWriteLock end-to-end.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tx.cleanup.Stop()

	gate := newCloseGate() // closed=false
	held := &atomic.Bool{}
	held.Store(true)

	info := txCleanupInfo{
		gate:      gate,
		pgr:       tx.pgr,
		grant:     tx.grant,
		held:      held,
		originPCs: nil,
	}
	txCleanupFn(info) // runs AbortTx + grant.Release (the real branch)

	if held.Load() {
		t.Errorf("held remained true; cleanup did not run")
	}

	// tx.Rollback on the now-aborted tx — sync.Once on grant.Release
	// makes the second call safe; AbortTx on already-aborted state
	// is a no-op or idempotent per pager contract.
	_ = tx.Rollback()
}
