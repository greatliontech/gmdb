package gmdb

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

func TestSyncModesAllAccepted(t *testing.T) {
	for name, m := range map[string]SyncMode{
		"SyncDurable":  SyncDurable,
		"SyncDataOnly": SyncDataOnly,
		"SyncLazy":     SyncLazy,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, err := Open(ctx, tmpPath(t), Options{
				PageSize: 4096, MinSize: 16, MaxSize: 64,
				SyncMode: m,
			})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer db.Close()
			if err := db.Update(ctx, func(tx *Tx) error {
				_, e := tx.AllocPage()
				return e
			}); err != nil {
				t.Errorf("Update: %v", err)
			}
		})
	}
}

func TestSyncLazyClearsCheckpointFlag(t *testing.T) {
	// Per durability.md: SyncLazy commits write meta with
	// MetaFlagCheckpoint CLEAR. Pin the contract by snapshotting
	// the meta after a SyncLazy commit and asserting the flag is
	// off.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		SyncMode: SyncLazy,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	// Genesis meta has Checkpoint set (per init.go).
	if !db.Meta().HasFlag(page.MetaFlagCheckpoint) {
		t.Fatal("genesis meta should have Checkpoint set")
	}
	// SyncLazy commit clears it.
	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.AllocPage()
		return e
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if db.Meta().HasFlag(page.MetaFlagCheckpoint) {
		t.Errorf("SyncLazy commit kept MetaFlagCheckpoint set")
	}
}

func TestSyncDurableKeepsCheckpointFlag(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		SyncMode: SyncDurable,
	})
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
	if !db.Meta().HasFlag(page.MetaFlagCheckpoint) {
		t.Errorf("SyncDurable commit cleared MetaFlagCheckpoint")
	}
}

func TestSyncDataOnlyKeepsCheckpointFlag(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		SyncMode: SyncDataOnly,
	})
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
	if !db.Meta().HasFlag(page.MetaFlagCheckpoint) {
		t.Errorf("SyncDataOnly commit cleared MetaFlagCheckpoint (spec keeps it set; data IS durable post-step-2 fsync)")
	}
}

func TestCheckpointSetsFlag(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		SyncMode: SyncLazy,
	})
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
	if db.Meta().HasFlag(page.MetaFlagCheckpoint) {
		t.Fatal("SyncLazy commit should have cleared Checkpoint")
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !db.Meta().HasFlag(page.MetaFlagCheckpoint) {
		t.Errorf("post-Checkpoint meta missing MetaFlagCheckpoint")
	}
}

func TestCheckpointAfterCloseReturnsErrClosed(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.Checkpoint(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Checkpoint after Close: got %v, want ErrClosed", err)
	}
}

func TestRecoveryPrefersCheckpointMeta(t *testing.T) {
	// Spec-tier invariant (durability.md): "Recovery prefers the
	// highest-TxnID valid meta whose Checkpoint flag is set;
	// non-checkpoint metas are never preferred over checkpoint
	// ones regardless of TxnID." Pin the contract:
	//
	//  1. Genesis (Init writes Checkpoint=set on meta-0 + meta-1,
	//     both at TxnID=0).
	//  2. One SyncDurable commit at TxnID=1 — meta-1 has
	//     Checkpoint=set + TxnID=1; meta-0 still has Checkpoint=set
	//     + TxnID=0 (unchanged).
	//  3. One SyncLazy commit at TxnID=2 — meta-0 now has
	//     Checkpoint=CLEAR + TxnID=2; meta-1 still Checkpoint=set
	//     + TxnID=1.
	//  4. Close + re-Open. Recovery picks meta-1 (higher-TxnID
	//     checkpoint-flagged) over meta-0 (higher TxnID but
	//     non-checkpoint).
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		SyncMode: SyncDurable,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// W1 (SyncDurable): writes meta-1 at TxnID=1, Checkpoint set.
	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.AllocPage()
		return e
	}); err != nil {
		t.Fatalf("W1: %v", err)
	}
	if db.Meta().TxnID != 1 || !db.Meta().HasFlag(page.MetaFlagCheckpoint) {
		t.Fatalf("after W1: TxnID=%d Checkpoint=%v, want 1+true", db.Meta().TxnID, db.Meta().HasFlag(page.MetaFlagCheckpoint))
	}
	// Switch to SyncLazy for W2.
	db.opts.SyncMode = SyncLazy
	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.AllocPage()
		return e
	}); err != nil {
		t.Fatalf("W2: %v", err)
	}
	if db.Meta().TxnID != 2 || db.Meta().HasFlag(page.MetaFlagCheckpoint) {
		t.Fatalf("after W2: TxnID=%d Checkpoint=%v, want 2+false", db.Meta().TxnID, db.Meta().HasFlag(page.MetaFlagCheckpoint))
	}
	db.Close()
	// Re-Open: recovery prefers Checkpoint-flagged meta-1 (TxnID=1)
	// over non-Checkpoint meta-0 (TxnID=2).
	db2, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	if db2.Meta().TxnID != 1 {
		t.Errorf("recovered TxnID = %d, want 1 (checkpoint-meta preference)", db2.Meta().TxnID)
	}
	if !db2.Meta().HasFlag(page.MetaFlagCheckpoint) {
		t.Error("recovered meta missing Checkpoint flag")
	}
}

func TestRecoveryFallbackWhenNoCheckpoint(t *testing.T) {
	// Spec rule (durability.md §Recovery step 3): if neither meta
	// has Checkpoint set, recovery falls back to highest-TxnID
	// valid meta. We construct the no-checkpoint state by:
	//   - Open with SyncLazy from the start.
	//   - One commit (clears Checkpoint on meta-1).
	//   - Tamper meta-0 (genesis Checkpoint-set) so its checksum
	//     fails and recovery discards it; only meta-1 remains.
	//
	// Wait — that gives ONE valid meta with Checkpoint clear. The
	// ActiveMetaCheckpointPreferring's single-meta path returns
	// (idx, noCheckpoint=true, ok=true). Recovery accepts; root
	// logs warning.
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		SyncMode: SyncLazy,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// SyncLazy commit puts the new (non-checkpoint) meta on disk.
	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.AllocPage()
		return e
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	db.Close()

	// Tamper meta-0 (the genesis-Checkpoint-set meta) so recovery
	// discards it. Active selection will only see meta-1 (the
	// SyncLazy non-checkpoint commit).
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for tamper: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xFF}, 8); err != nil {
		t.Fatalf("tamper meta-0: %v", err)
	}
	f.Close()

	// Re-Open: meta-0 corrupt; meta-1 valid but Checkpoint=clear.
	// Recovery picks meta-1 (noCheckpoint=true, logs warning).
	db2, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("re-Open with corrupt-checkpoint-meta + no-checkpoint-meta-1: %v", err)
	}
	defer db2.Close()
	if db2.Meta().TxnID == 0 {
		t.Errorf("recovered TxnID = 0 (genesis); want the non-checkpoint commit's TxnID")
	}
}
