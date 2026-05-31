package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
)

// TestShrinkHonorsMinSizeFloor: maybeShrink must never truncate the file below
// MinSize*PageSize (file-format.md §File Shrinkage — a clause-explicit
// invariant). Pre-fix it truncated to ~HighWaterMark, silently discarding the
// user's pre-allocated minimum: a tiny commit on a 64-page Init'd file shrank
// it to a handful of pages.
func TestShrinkHonorsMinSizeFloor(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	const pageSize, minPages = 4096, 64
	db, err := Open(ctx, path, Options{
		PageSize: pageSize, MinSize: minPages, MaxSize: 4096,
		ShrinkThreshold: 1, GrowStep: 8,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// A tiny commit triggers maybeShrink (ShrinkThreshold=1) on the Init'd
	// minPages-page file.
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("ks")
		if e != nil {
			return e
		}
		return ks.Put([]byte("k"), []byte("v"))
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Size(); got < int64(minPages)*pageSize {
		t.Fatalf("file shrank to %d bytes (%d pages), below the MinSize floor of %d pages", got, got/pageSize, minPages)
	}
}

// TestGrowStepAlignedGrowth: file growth extends in GrowStep-aligned
// increments (file-format.md §File Growth), so after any growth the file size
// is a multiple of GrowStep pages — pages above HighWaterMark are file-backed
// and absorbed by later allocations without another ftruncate. Pre-fix every
// extension ftruncated by exactly one page, so the file size tracked
// HighWaterMark and was almost never GrowStep-aligned.
func TestGrowStepAlignedGrowth(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	const pageSize, growStep, minPages = 4096, 64, 8
	db, err := Open(ctx, path, Options{
		PageSize: pageSize, MinSize: minPages, MaxSize: 4096,
		GrowStep: growStep, ShrinkThreshold: 0, // no shrink to muddy the size
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	grew := false
	for round := range 30 {
		if err := db.Update(ctx, func(tx *Tx) error {
			ks, e := tx.CreateKeyspaceIfNotExists("ks")
			if e != nil {
				return e
			}
			for i := range 20 {
				if e := ks.Put(fmt.Appendf(nil, "k/%03d/%03d", round, i), bytes.Repeat([]byte("v"), 600)); e != nil {
					return e
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		pages := fi.Size() / pageSize
		if pages > minPages {
			grew = true
			if pages%growStep != 0 {
				t.Fatalf("round %d: file is %d pages, not GrowStep(%d)-aligned — growth not batched", round, pages, growStep)
			}
		}
	}
	if !grew {
		t.Fatalf("file never grew past MinSize; test is ineffective")
	}
}
