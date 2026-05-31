package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// TestRPLSyncLazyReopenCorruptionMinimal is the minimal reproducer for the
// confirmed [H] RPL-reclamation-bound corruption (rpl-reclamation-checkpoint-
// bound): standard public API only — SyncLazy Put/Delete churn + periodic
// Checkpoint + Close/reopen, NO compaction, NO manual internals, background
// maintenance disabled. Flaky on allocation order (run under -race or -count
// to hit reliably). When it fires, the reopen fails with
//
//	gmdb: database is corrupted: pager: rebuild RPL chain:
//	  pager: RPL segment at page N malformed
//
// i.e. a still-recoverable meta's RPL chain walks through a reclaimed-and-
// reused segment page → the DB is UNOPENABLE.
//
// Root cause (confirmed empirically): RPL reclamation frees a segment PAGE
// that a still-recoverable meta's chain references; on reopen, recovery
// selects that meta and walks its chain into the freed-and-reused page.
// Setting bound = 0 (reclaim nothing) eliminates it; SyncDurable (recovery
// always picks the latest, just-written meta) does not reproduce. NOTE: the
// spec's bound min(oldestReaderTxnID, lastCheckpointTxnID) is INSUFFICIENT —
// implementing it exactly still corrupts (a recoverable checkpoint's chain
// tail references segments tagged below its own TxnID, which bound=checkpoint
// still frees). The real fix must not free a segment page any recoverable
// meta's chain references (see docs/issues/rpl-reclamation-checkpoint-bound).
// Remove the Skip when that lands — this becomes the regression.
func TestRPLSyncLazyReopenCorruptionMinimal(t *testing.T) {
	t.Skip("reproducer for the confirmed [H] rpl-reclamation-checkpoint-bound corruption (DB unopenable after SyncLazy churn+reopen). Un-skip when reclamation no longer frees segment pages a recoverable meta's chain references (the spec's lastCheckpointTxnID bound alone is insufficient).")
	ctx := context.Background()
	path := tmpPath(t)
	open := func() *DB {
		db, err := Open(ctx, path, Options{
			PageSize: 4096, MinSize: 64, MaxSize: 4096, SyncMode: SyncLazy,
			Maintenance: MaintenanceOptions{Disable: true},
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return db
	}
	db := open()
	if err := db.Update(ctx, func(tx *Tx) error { _, e := tx.CreateKeyspace("ks"); return e }); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatalf("initial checkpoint: %v", err)
	}
	for round := range 60 {
		if err := db.Update(ctx, func(tx *Tx) error {
			ks, err := tx.OpenKeyspace("ks")
			if err != nil {
				return err
			}
			for j := range 6 {
				if err := ks.Put(fmt.Appendf(nil, "k/%04d/%04d", round, j), bytes.Repeat([]byte("v"), 700)); err != nil {
					return err
				}
			}
			for j := range 4 {
				_ = ks.Delete(fmt.Appendf(nil, "k/%04d/%04d", round-1, j))
			}
			return nil
		}); err != nil {
			t.Fatalf("round %d churn: %v", round, err)
		}
		// Periodic checkpoint interleaved with SyncLazy churn — the ingredient
		// the no-checkpoint minimal version lacks.
		if round%7 == 6 {
			if err := db.Checkpoint(ctx); err != nil {
				t.Fatalf("round %d checkpoint: %v", round, err)
			}
		}
		if round%6 == 5 {
			_ = db.Close()
			db = open() // reopen — malformed RPL chain surfaces here
		}
	}
	_ = db.Close()
}
