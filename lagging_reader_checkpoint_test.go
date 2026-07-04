package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestSyncLazyLaggingReaderRefreshRespectsCheckpointBound pins the
// reclamation-bound refresh formula (free-space.md §RPL Reclamation:
// min(oldestReader, lastCheckpointTxnID) — checkpoint term included).
// Under SyncLazy the previous meta's TxnID runs ahead of the last
// checkpoint; a refresh derived from prevMeta.TxnID instead of
// lastCheckpointTxnID reclaims RPL segments the checkpoint's tree
// still references, so crash recovery — which selects the checkpoint
// meta — walks reclaimed-and-reused pages. SyncDurable is immune
// (checkpoint == prev), which is why the pre-existing lagging-reader
// tests never caught it.
//
// The crash is simulated by byte-copying the database file (SyncLazy
// commits are in the page cache; the copy sees them, but recovery
// still selects the checkpoint meta because lazy metas carry no
// checkpoint flag) and opening the copy.
func TestSyncLazyLaggingReaderRefreshRespectsCheckpointBound(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	var laggingCalls atomic.Int64
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 384,
		SyncMode:    SyncLazy,
		Maintenance: MaintenanceOptions{Disable: true},
		LaggingReader: func(LaggingReaderInfo) LaggingReaderAction {
			laggingCalls.Add(1)
			return LaggingReaderWait
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	checkpointVal := bytes.Repeat([]byte{'C'}, 200)
	churn := func(round int) {
		t.Helper()
		val := bytes.Repeat([]byte{byte('a' + round%20)}, 200)
		if err := db.Update(ctx, func(tx *Tx) error {
			ks, err := tx.OpenKeyspace("k")
			if err != nil {
				if ks, err = tx.CreateKeyspace("k"); err != nil {
					return err
				}
			}
			for i := range 150 {
				if err := ks.Put(fmt.Appendf(nil, "k%04d", i), val); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("churn round %d: %v", round, err)
		}
	}

	// Seed with the sentinel values, checkpoint (TxnID = C).
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("k")
		if err != nil {
			return err
		}
		for i := range 150 {
			if err := ks.Put(fmt.Appendf(nil, "k%04d", i), checkpointVal); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// Snapshot both meta slots as the checkpoint left them on disk.
	fileNow, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read post-checkpoint file: %v", err)
	}
	metaSlots := append([]byte(nil), fileNow[:2*4096]...)

	// Two lazy commits retiring checkpoint-tree pages (segments C+1,
	// C+2), then a reader pinned ABOVE the checkpoint — the shape
	// where the buggy refresh (reader < prev but > checkpoint)
	// reclaims segment C+1 and corrupts the checkpoint tree.
	churn(0)
	churn(1)
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer rtx.Rollback()

	// Churn under alloc pressure until the lagging-reader path has
	// fired and freed pages have been reused; the pinned reader keeps
	// the table non-empty so the callback (not the no-reader skip)
	// runs.
	for round := 2; round < 24; round++ {
		churn(round)
	}
	if laggingCalls.Load() == 0 {
		t.Fatalf("fixture: lagging-reader callback never fired; no reclamation pressure")
	}

	// Crash image: SyncLazy syncs nothing after Checkpoint(), so a
	// real crash can leave the meta slots exactly as the checkpoint
	// left them while any subset of the later lazy DATA writes hit
	// disk via writeback. Construct the worst case: full page-cache
	// data (every post-checkpoint overwrite present — including any
	// reuse of wrongly-reclaimed pages) with both meta slots restored
	// to their post-checkpoint bytes. Recovery must select the
	// checkpoint meta and find its tree intact — which is exactly
	// what the checkpoint term of the reclamation bound guarantees.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db file: %v", err)
	}
	copy(data[:2*4096], metaSlots)
	copyPath := filepath.Join(t.TempDir(), "crash.gmdb")
	if err := os.WriteFile(copyPath, data, 0o600); err != nil {
		t.Fatalf("write copy: %v", err)
	}
	db2, err := Open(ctx, copyPath, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 384,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open(crash copy): %v (checkpoint tree reclaimed?)", err)
	}
	defer db2.Close()
	for iss := range db2.Check() {
		t.Errorf("Check issue on recovered checkpoint: %+v", iss)
	}
	rtx2, err := db2.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead(recovered): %v", err)
	}
	defer rtx2.Rollback()
	ks, err := rtx2.OpenKeyspaceReadOnly("k")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly(recovered): %v", err)
	}
	for i := range 150 {
		v, err := ks.Get(fmt.Appendf(nil, "k%04d", i))
		if err != nil {
			t.Fatalf("Get(k%04d) on recovered checkpoint: %v", i, err)
		}
		if !bytes.Equal(v, checkpointVal) {
			t.Fatalf("Get(k%04d) = %q..., want checkpoint values", i, v[:4])
		}
	}
}
