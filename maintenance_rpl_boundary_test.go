package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
	"github.com/thegrumpylion/gmdb/internal/pager"
)

// rplSegmentIDs walks the meta-claimed RPL chain head→tail and returns
// the segment page ids in walk order.
func rplSegmentIDs(t *testing.T, db *DB) []uint64 {
	t.Helper()
	meta := db.Meta()
	if meta.RPLHeadPage == 0 {
		return nil
	}
	cfg := page.Config{PageSize: meta.PageSize, PageChecksum: meta.HasFlag(pager.MetaFlagPageChecksum)}
	walk := pager.RPLChainWalk{
		ReadPage:     db.PgrForTest().PageRaw,
		Cfg:          cfg,
		Head:         meta.RPLHeadPage,
		HeadTxnID:    meta.RPLHeadTxnID,
		Tail:         meta.RPLTailPage,
		EntryCount:   meta.RPLEntryCount,
		ReclaimEpoch: meta.Durable.TxnID,
		LowBound:     db.FirstDataPageForTest(),
		HighBound:    meta.HighWaterMark,
	}
	var ids []uint64
	stop, werr := walk.Walk(func(id uint64, _ pager.RPLSegment) bool {
		ids = append(ids, id)
		return true
	})
	if werr != nil {
		t.Fatalf("fixture: RPL walk error: %+v", werr)
	}
	if stop.Reason != pager.RPLWalkTailReached {
		t.Fatalf("fixture: RPL walk stopped early (%v) — chain not intact", stop.Reason)
	}
	return ids
}

// buildPendingRPLChain grows a multi-segment RPL chain that stays
// PENDING: under SyncLazy nothing anchors, so the reclamation bound
// never advances and every retired segment stays both on disk and in
// the live writer's in-memory chain.
func buildPendingRPLChain(t *testing.T, db *DB, minSegments int) []uint64 {
	t.Helper()
	ctx := context.Background()
	for round := 0; round < 120; round++ {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin(%d): %v", round, err)
		}
		ks, err := tx.OpenKeyspace("k")
		if err != nil {
			if ks, err = tx.CreateKeyspace("k"); err != nil {
				t.Fatalf("CreateKeyspace: %v", err)
			}
		}
		for i := range 30 {
			k := fmt.Sprintf("r%03d-%03d", round, i)
			if err := ks.Put([]byte(k), bytes.Repeat([]byte{byte('A' + round%26)}, 500)); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		if round > 0 {
			for i := range 20 {
				if err := ks.Delete([]byte(fmt.Sprintf("r%03d-%03d", round-1, i))); err != nil {
					t.Fatalf("Delete: %v", err)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(%d): %v", round, err)
		}
		if ids := rplSegmentIDs(t, db); len(ids) >= minSegments {
			return ids
		}
	}
	t.Fatal("fixture: chain never reached the segment target")
	return nil
}

// Background leak reclamation must NOT free pages that sit behind an
// RPL walk-truncation boundary: a bitrotted middle segment truncates
// the detection walk, making every OLDER intact segment's entries
// classify as leaked — but those segments are still in the live
// writer's in-memory chain and will be reclaimed again later,
// double-freeing pages a user transaction may have re-allocated in
// between (background-maintenance.md §Bitmap Leak Reclamation: the
// walk must reach the authoritative tail or a reclaimed boundary).
func TestMaintenanceReclaimSkipsBehindRPLBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		checksum bool // true → footer boundary; false → decode boundary
	}{{"footer_boundary", true}, {"decode_boundary", false}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := tmpPath(t)
			opts := Options{
				PageSize: 4096, MinSize: 16, MaxSize: 2048,
				SyncMode:            SyncLazy,
				DisablePageChecksum: !tc.checksum,
				Maintenance:         MaintenanceOptions{Disable: true},
			}
			db, err := Open(ctx, path, opts)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer db.Close()
			ids := buildPendingRPLChain(t, db, 4)

			// Bitrot a middle segment (never the exempt head, never the
			// tail): flip a data byte (breaks the footer) or the type
			// byte (breaks decode on a checksums-off database).
			target := ids[1]
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatalf("open for bitrot: %v", err)
			}
			off := int64(target) * 4096
			if !tc.checksum {
				// Type byte → decode boundary.
				if _, err := f.WriteAt([]byte{9}, off); err != nil {
					t.Fatalf("bitrot: %v", err)
				}
			} else {
				// Any payload byte → footer boundary.
				buf := make([]byte, 1)
				if _, err := f.ReadAt(buf, off+100); err != nil {
					t.Fatalf("read for bitrot: %v", err)
				}
				buf[0] ^= 0xff
				if _, err := f.WriteAt(buf, off+100); err != nil {
					t.Fatalf("bitrot: %v", err)
				}
			}
			if err := f.Close(); err != nil {
				t.Fatalf("close bitrot handle: %v", err)
			}

			// The truncated walk makes the older segments' retired pages
			// classify as leaked; reclaiming them while the live chain
			// still lists them is the double-free setup. The pass must
			// skip.
			freed, _ := db.MaintReclaimLeaksForTest(ctx)
			if freed != 0 {
				t.Fatalf("maintenance freed %d pages behind an RPL truncation boundary (live chain still lists them)", freed)
			}

			// End-to-end: hammer more commits so the live chain's own
			// reclamation and fresh allocations churn the same pages,
			// then verify structural integrity. Pre-fix, the freed
			// pages get re-allocated and then double-freed under their
			// new owner (ReachableButFree / value corruption).
			for round := 200; round < 215; round++ {
				tx, err := db.Begin(ctx)
				if err != nil {
					t.Fatalf("hammer Begin: %v", err)
				}
				ks, err := tx.OpenKeyspace("k")
				if err != nil {
					t.Fatalf("hammer OpenKeyspace: %v", err)
				}
				for i := range 30 {
					k := fmt.Sprintf("h%03d-%03d", round, i)
					if err := ks.Put([]byte(k), bytes.Repeat([]byte{'H'}, 500)); err != nil {
						t.Fatalf("hammer Put: %v", err)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatalf("hammer Commit: %v", err)
				}
			}
			for iss := range db.Check() {
				if iss.Code == "ReachableButFree" || iss.Code == "FreeAndPending" || iss.Code == "PageDoubleReferenced" {
					t.Fatalf("post-hammer corruption: %+v", iss)
				}
			}
		})
	}
}

// CheckWithOptions(Repair) shares the gate: with the RPL walk stopped
// at a corrupt-segment boundary the "leaked" set may intersect the
// live chain of this process's writer, so Repair reports the would-be
// leaks unrepaired plus Repair.Skipped and frees nothing — exactly the
// structural-findings shape (api-surface.md §CheckOptions.Repair).
func TestRepairSkipsBehindRPLBoundary(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	opts := Options{
		PageSize: 4096, MinSize: 16, MaxSize: 2048,
		SyncMode:    SyncLazy,
		Maintenance: MaintenanceOptions{Disable: true},
	}
	db, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	ids := buildPendingRPLChain(t, db, 4)

	target := ids[1]
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open for bitrot: %v", err)
	}
	buf := make([]byte, 1)
	off := int64(target)*4096 + 100
	if _, err := f.ReadAt(buf, off); err != nil {
		t.Fatalf("read for bitrot: %v", err)
	}
	buf[0] ^= 0xff
	if _, err := f.WriteAt(buf, off); err != nil {
		t.Fatalf("bitrot: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close bitrot handle: %v", err)
	}

	skipped := false
	repaired := 0
	for iss := range db.CheckWithOptions(&CheckOptions{Repair: true}) {
		if iss.Code == "Repair.Skipped" {
			skipped = true
		}
		if iss.Code == "BitmapLeak" && iss.Repaired {
			repaired++
		}
	}
	if repaired != 0 {
		t.Fatalf("Repair freed %d pages behind an RPL truncation boundary", repaired)
	}
	if !skipped {
		t.Fatal("Repair did not emit Repair.Skipped for the RPL boundary")
	}
}

// Check surfaces a DECODE boundary with the same warning class as a
// footer boundary — pre-fix it was silent, hiding from the operator
// that the chain walk truncated (checksums.md-adjacent observability;
// the asymmetry was footer-warn / decode-silent).
func TestCheckWarnsOnRPLDecodeBoundary(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	opts := Options{
		PageSize: 4096, MinSize: 16, MaxSize: 2048,
		SyncMode:            SyncLazy,
		DisablePageChecksum: true,
		Maintenance:         MaintenanceOptions{Disable: true},
	}
	db, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	ids := buildPendingRPLChain(t, db, 4)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open for bitrot: %v", err)
	}
	if _, err := f.WriteAt([]byte{9}, int64(ids[1])*4096); err != nil {
		t.Fatalf("bitrot: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close bitrot handle: %v", err)
	}

	warned := false
	for iss := range db.Check() {
		if iss.Code == "RPLSegmentBoundary" && iss.PageID == ids[1] {
			warned = true
		}
	}
	if !warned {
		t.Fatal("Check emitted no boundary warning for the undecodable RPL segment (silent truncation)")
	}
}
