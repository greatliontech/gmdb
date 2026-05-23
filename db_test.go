package gmdb

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
