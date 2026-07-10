package gmdb

import (
	"context"
	"os"
	"testing"
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
