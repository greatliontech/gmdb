package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// TestRPLSyncLazyReopenCorruptionMinimal is the regression for the [H]
// RPL-reclamation / recovery corruption (FIXED). Standard public API only —
// SyncLazy Put/Delete churn + periodic Checkpoint + Close/reopen, no
// compaction, no manual internals, background maintenance disabled. Before the
// fix this made the DB UNOPENABLE (reopen failed: "rebuild RPL chain: RPL
// segment at page N malformed"), flakily on allocation order — the 60
// churn+reopen rounds exercise many orders per run, and Check() after every
// reopen verifies integrity, not just openability.
//
// Two coupled root causes, both fixed:
//
//	(1) the reclamation bound used prevMeta.TxnID, which under SyncLazy runs
//	    ahead of the last checkpoint, freeing data pages a still-recoverable
//	    durable epoch's tree references. Fixed: the anchored-epoch bound
//	    (db.go; free-space.md §RPL Reclamation).
//	(2) recovery to a NON-latest meta walked its RPLHeadPage→tail chain into a
//	    reclaimed-and-reused segment page (reclamation frees segment pages
//	    from the live tail and advances the live meta's tail without rewriting
//	    older metas). Fixed in BOTH on-disk chain walkers — rebuildRPLChain
//	    (internal/pager/init.go) and Check's walkRPL (check.go) — which stop
//	    at the first reclaimed segment (free in the bitmap, or reused),
//	    checked here via checkClean after every reopen.
//
// Regression coverage is allocation-order-probabilistic (the underlying bug
// was map-randomized): a revert of EITHER tolerance (2) trips within one run
// (Open fails, or Check reports RPLSegmentMalformed in the stale-tail window);
// a revert of the bound (1) corrupts the data tree only on the narrower order
// where a reclaimed tree-referenced page is reused-then-overwritten before
// reopen, surfacing as Check ReachableInRPL — reliable only at higher
// iteration. CI should run this with -count to guard part (1); a single run
// reliably guards part (2).
func TestRPLSyncLazyReopenCorruptionMinimal(t *testing.T) {
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
	// checkClean asserts Open succeeded AND the recovered tree + RPL are
	// structurally clean — run in the stale-tail window (after a reopen, before
	// the next commit heals the on-disk RPLTailPage), so it exercises BOTH RPL
	// chain walkers (the Open-time rebuild and Check's walkRPL) against the
	// reclaimed tail of a non-latest recovered meta.
	checkClean := func(d *DB, tag string) {
		t.Helper()
		for _, iss := range collectIssues(d.Check()) {
			if iss.Severity == CheckError || iss.Severity == CheckFatal {
				t.Fatalf("%s: Check reported %v %s (page %d): %s", tag, iss.Severity, iss.Code, iss.PageID, iss.Message)
			}
		}
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
			db = open() // reopen — pre-fix, a malformed RPL chain surfaced here
			checkClean(db, fmt.Sprintf("Check after reopen@round%d (stale-tail window)", round))
		}
	}
	checkClean(db, "final Check after SyncLazy churn+reopen")
	_ = db.Close()
}
