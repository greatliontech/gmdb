package gmdb

import (
	"context"
	"os"
	"testing"
	"time"
)

// File shrinkage DEFERS while any reader transaction is live
// (file-format.md §File Shrinkage): a reader's file-resident page
// bound is fixed at Begin, and truncating under it would turn a
// corrupt content-derived page id — the class checksums.md promises
// ErrCorrupted for — into a SIGBUS on the newly-unbacked region. The
// shrink retries at the next eligible commit once the reader exits.
func TestShrinkDefersWhileReaderLive(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	const pageSize, initPages = 4096, 64
	// Init the file large (MinSize floor 64), then lower the floor so
	// the tiny HighWaterMark makes every commit shrink-eligible.
	db, err := Open(ctx, path, Options{
		PageSize: pageSize, MinSize: initPages, MaxSize: 4096,
		ShrinkThreshold: 1, GrowStep: 4,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("ks")
		if e != nil {
			return e
		}
		if e := ks.Put([]byte("k"), []byte("v")); e != nil {
			return e
		}
		return tx.SetFileFormat(FileFormat{Lower: 16 * pageSize, Upper: 4096 * pageSize, GrowStep: 4 * pageSize, ShrinkThreshold: 1 * pageSize})
	}); err != nil {
		t.Fatalf("setup commit: %v", err)
	}

	// Hold a reader; a shrink-eligible commit must DEFER.
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.OpenKeyspace("ks")
		if e != nil {
			return e
		}
		return ks.Put([]byte("k"), []byte("v2"))
	}); err != nil {
		t.Fatalf("gated commit: %v", err)
	}
	withReader, _ := os.Stat(path)
	if withReader.Size() < int64(initPages)*pageSize {
		t.Fatalf("file shrank to %d bytes while a reader was live", withReader.Size())
	}

	// Release the reader; the next commit executes the deferred shrink.
	if err := rtx.Rollback(); err != nil {
		t.Fatalf("release reader: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.OpenKeyspace("ks")
		if e != nil {
			return e
		}
		return ks.Put([]byte("k"), []byte("v3"))
	}); err != nil {
		t.Fatalf("post-release commit: %v", err)
	}
	after, _ := os.Stat(path)
	if after.Size() >= withReader.Size() {
		t.Fatalf("file did not shrink after the reader exited (%d -> %d bytes)", withReader.Size(), after.Size())
	}
}

// A reader whose slot publishes AFTER the shrink gate's scan passed it
// must not retain the pre-truncate file-resident bound: the shrink
// seqlock bracket around its size read detects the overlapping
// truncate span and re-reads (file-format.md §File Shrinkage). The
// hook blocks the writer inside the scan→truncate window while a
// full BeginRead runs; the reader must end up with the POST-shrink
// bound — a corrupt content-derived page id in the truncated range
// then fails with ErrCorrupted instead of SIGBUSing on the unbacked
// tail.
func TestReaderBracketsShrinkSeqlock(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	const pageSize, initPages = 4096, 64
	db, err := Open(ctx, path, Options{
		PageSize: pageSize, MinSize: initPages, MaxSize: 4096,
		ShrinkThreshold: 1, GrowStep: 4,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("ks")
		if e != nil {
			return e
		}
		if e := ks.Put([]byte("k"), []byte("v")); e != nil {
			return e
		}
		return tx.SetFileFormat(FileFormat{Lower: 16 * pageSize, Upper: 4096 * pageSize, GrowStep: 4 * pageSize, ShrinkThreshold: 1 * pageSize})
	}); err != nil {
		t.Fatalf("setup commit: %v", err)
	}

	var rtx *ReadTx
	var beginErr error
	readerDone := make(chan struct{})
	fired := false
	restore := SetShrinkGateHookForTest(func() {
		if fired {
			return
		}
		fired = true
		// The gate's scan has passed (saw no readers); the reader
		// publishes and sizes NOW, inside the open bracket. Its first
		// size read races the pending truncate; the odd seqlock makes
		// it retry until the writer settles — which happens only
		// after this hook returns, so the reader goroutine is left
		// spinning on the bracket while we return and the truncate
		// lands. Give it a moment to demonstrably enter the bracket.
		go func() {
			rtx, beginErr = db.BeginRead(ctx)
			close(readerDone)
		}()
		select {
		case <-readerDone:
			// Reader finished while the bracket was still open — it
			// accepted via the crashed-writer cap. Rare (needs 64
			// retries × 1ms while we sleep); the asserts below still
			// hold it to the safe outcome or the test fails.
		case <-time.After(10 * time.Millisecond):
			// Reader is (very likely) parked in the bracket loop.
		}
	})
	defer restore()

	// Shrink-eligible commit: no readers visible at the scan.
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.OpenKeyspace("ks")
		if e != nil {
			return e
		}
		return ks.Put([]byte("k"), []byte("v2"))
	}); err != nil {
		t.Fatalf("shrink commit: %v", err)
	}
	if !fired {
		t.Fatal("fixture: shrink gate never reached the truncate window")
	}
	// Wait for the reader to settle post-truncate (channel-synchronised
	// — rtx/beginErr are only read after readerDone closes).
	select {
	case <-readerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reader never completed")
	}
	if beginErr != nil {
		t.Fatalf("BeginRead: %v", beginErr)
	}
	defer rtx.Rollback()

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	shrunkPages := uint64(st.Size()) / pageSize
	// A page id just past the shrunk EOF but well within the
	// pre-shrink size: with the bracket, the reader's bound reflects
	// the post-truncate size and the access fails ErrCorrupted; with
	// a stale pre-shrink bound it passes the bound check and SIGBUSes
	// on the unbacked mmap tail.
	if shrunkPages >= initPages {
		t.Fatalf("fixture: file did not shrink (%d pages)", shrunkPages)
	}
	if _, err := rtx.Page(shrunkPages + 2); err == nil {
		t.Fatalf("Page(%d) past the shrunk EOF succeeded", shrunkPages+2)
	}
}
