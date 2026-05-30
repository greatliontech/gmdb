package gmdb

import (
	"context"
	"fmt"
	"testing"
)

// TestRPLChainSurvivesPartialReclaimAndReuse is the regression test for the
// RPL chain-walk termination bug (fixed by bounding the walk at RPLTailPage
// instead of OlderSegment==0). A delete-heavy churn workload on a small DB
// forces AllocPage to reclaim the oldest RPL segments and reuse their pages as
// data; the new tail's on-disk OlderSegment then dangles at a reused page.
// Before the fix this made the database UNOPENABLE (rebuildRPLChain followed
// the dangling pointer → ErrCorrupted) and Check report RPLSegmentMalformed.
// No compaction is involved — the fault is in the core RPL recovery path.
func TestRPLChainSurvivesPartialReclaimAndReuse(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	open := func() *DB {
		db, err := Open(ctx, path, Options{
			PageSize: 4096, MinSize: 16, MaxSize: 512, // small ⇒ bitmap pressure ⇒ reclaim+reuse
			Maintenance: MaintenanceOptions{Disable: true},
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return db
	}
	db := open()

	// Churn: fill then delete, repeatedly — builds a multi-segment RPL and
	// partially reclaims it (reusing reclaimed segment pages as data).
	for round := range 40 {
		tx, _ := db.Begin(ctx)
		ks, err := tx.OpenKeyspace("k")
		if err != nil {
			ks, err = tx.CreateKeyspace("k")
			if err != nil {
				t.Fatalf("CreateKeyspace: %v", err)
			}
		}
		for i := range 200 {
			if err := ks.Put(fmt.Appendf(nil, "r%02d-k%04d", round, i), fmt.Appendf(nil, "v%04d", i)); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit fill: %v", err)
		}
		txd, _ := db.Begin(ctx)
		ksd, _ := txd.OpenKeyspace("k")
		if _, err := ksd.DeleteRange(fmt.Appendf(nil, "r%02d-k%04d", round, 0), fmt.Appendf(nil, "r%02d-k%04d", round, 199)); err != nil {
			t.Fatalf("DeleteRange: %v", err)
		}
		if err := txd.Commit(); err != nil {
			t.Fatalf("Commit delete: %v", err)
		}
	}

	// Check sees no RPL corruption.
	for _, iss := range collectIssues(db.Check()) {
		if iss.Severity == CheckError || iss.Severity == CheckFatal {
			t.Errorf("Check: code=%s page=%d msg=%s", iss.Code, iss.PageID, iss.Message)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The database reopens cleanly — rebuildRPLChain walks the persisted chain
	// bounded by RPLTailPage and never follows a dangling tail OlderSegment.
	db2 := open()
	db2.Close()
}
