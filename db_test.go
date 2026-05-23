package gmdb

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

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

	// Next Begin must surface ErrPoisoned without blocking on writeMu.
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
	// db.writeMu locked forever. runtime.AddCleanup on the *Tx fires
	// after GC marks it unreachable; the cleanup CAS's the held flag
	// and unlocks. The test leaks a tx in an isolated stack frame,
	// forces GC, and asserts the next Begin succeeds without blocking.
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

	// The cleanup runs on a separate goroutine; wait for the writeMu
	// to be released by attempting Begin with a timeout. Block on a
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
		t.Fatal("Begin blocked — leaked tx did not release writeMu via GC cleanup")
	}
}

func TestCommitStopsCleanup(t *testing.T) {
	// Regression: a normal Commit must Stop() the leak-detection
	// cleanup so no spurious leak warning fires after the tx closed
	// cleanly. Also verifies that releaseWriteLock's CAS prevents the
	// cleanup from double-Unlock'ing if it ran anyway (and that the
	// next Begin works normally).
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
