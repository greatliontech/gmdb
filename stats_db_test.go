package gmdb

import (
	"context"
	"testing"

	"github.com/greatliontech/gmdb/internal/lock"
)

// TestDBStats exercises db.Stats(): meta-derived fields, the cluster
// reader-table scan (ActiveReaders), the RPL retired-page count, and the
// current-tx slab usage (read from the writing goroutine).
func TestDBStats(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Quiescent snapshot of a freshly-initialised DB.
	s := db.Stats()
	if s.MinSize != 16 || s.MaxSize != 128 {
		t.Errorf("MinSize/MaxSize = %d/%d, want 16/128", s.MinSize, s.MaxSize)
	}
	if s.MaxReaders != lock.DefaultMaxReaders {
		t.Errorf("MaxReaders = %d, want %d", s.MaxReaders, lock.DefaultMaxReaders)
	}
	if s.HighWaterMark == 0 {
		t.Error("HighWaterMark = 0 on an initialised DB")
	}
	if s.FileSize == 0 {
		t.Error("FileSize = 0 (bytes)")
	}
	if s.SlabBytes != 0 {
		t.Errorf("SlabBytes quiescent = %d, want 0", s.SlabBytes)
	}
	if s.ActiveReaders != 0 {
		t.Errorf("ActiveReaders with no reader = %d, want 0", s.ActiveReaders)
	}

	// Allocate + commit a page so it can be retired by a later tx.
	var idA uint64
	if err := db.Update(ctx, func(tx *Tx) error {
		id, e := tx.AllocPage()
		idA = id
		return e
	}); err != nil {
		t.Fatalf("alloc Update: %v", err)
	}

	// Pin a reader (so the retired page can't be reclaimed) and confirm
	// the cluster scan counts it.
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	if got := db.Stats().ActiveReaders; got != 1 {
		t.Errorf("ActiveReaders with one reader = %d, want 1", got)
	}

	// Retire idA: a prior-tx page freed now joins the RPL at commit.
	if err := db.Update(ctx, func(tx *Tx) error { return tx.FreePage(idA) }); err != nil {
		t.Fatalf("free Update: %v", err)
	}
	if got := db.Stats().RetiredPages; got == 0 {
		t.Error("RetiredPages after retiring a reader-pinned page = 0, want >= 1")
	}

	if err := rtx.Rollback(); err != nil {
		t.Fatalf("reader Rollback: %v", err)
	}
	if got := db.Stats().ActiveReaders; got != 0 {
		t.Errorf("ActiveReaders after reader close = %d, want 0", got)
	}

	// SlabBytes mid-tx, read from the writing goroutine (no concurrent
	// pager mutation — the single-threaded pager contract holds).
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	id, err := tx.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if _, err := tx.AllocSlab(id); err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	if got := db.Stats().SlabBytes; got <= 0 {
		t.Errorf("SlabBytes mid-tx = %d, want > 0", got)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := db.Stats().SlabBytes; got != 0 {
		t.Errorf("SlabBytes after rollback = %d, want 0", got)
	}
}
