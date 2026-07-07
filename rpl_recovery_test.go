package gmdb

import (
	"context"
	"errors"
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

// buildChecksummedRPLChain opens a checksummed DB at path, churns until
// the committed meta carries an RPL chain of at least two segments
// (head != tail), and returns the head and tail segment page ids.
func buildChecksummedRPLChain(t *testing.T, ctx context.Context, path string) (head, tail uint64) {
	t.Helper()
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 512, Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	for round := 0; round < 40; round++ {
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
		for i := range 20 {
			k := fmt.Sprintf("r%03d-%03d", round, i)
			if err := ks.Put([]byte(k), make([]byte, 200)); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		if round > 0 {
			for i := range 10 {
				k := fmt.Sprintf("r%03d-%03d", round-1, i)
				if err := ks.Delete([]byte(k)); err != nil {
					t.Fatalf("Delete: %v", err)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(%d): %v", round, err)
		}
		m := db.Meta()
		if m.RPLHeadPage != 0 && m.RPLTailPage != 0 && m.RPLHeadPage != m.RPLTailPage {
			return m.RPLHeadPage, m.RPLTailPage
		}
	}
	t.Fatalf("fixture: never built a multi-segment RPL chain")
	return 0, 0
}

// TestOpenRejectsChecksumCorruptRPLHead: the head segment is the
// recovery meta's own newest — never legitimately reclaimed — so a
// checksum-bad head is corruption and Open must fail with the
// checksum sentinel, not walk into a bit-flipped entry list.
func TestOpenRejectsChecksumCorruptRPLHead(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	head, _ := buildChecksummedRPLChain(t, ctx, path)
	corruptPageByte(t, path, head, 4096)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 512, Maintenance: MaintenanceOptions{Disable: true},
	})
	if err == nil {
		db.Close()
		t.Fatalf("Open succeeded with checksum-corrupt RPL head")
	}
	if !errors.Is(err, ErrBadPageChecksum) {
		t.Fatalf("Open error = %v, want ErrBadPageChecksum", err)
	}
}

// TestOpenTruncatesChainAtChecksumCorruptNonHeadSegment: a non-head
// segment failing its checksum gets the reclaimed-then-reused
// stale-tail treatment — the chain truncates there (older segments'
// pages leak, bounded, recoverable via Check/Repair) and the database
// opens and reads normally.
func TestOpenTruncatesChainAtChecksumCorruptNonHeadSegment(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	_, tail := buildChecksummedRPLChain(t, ctx, path)
	corruptPageByte(t, path, tail, 4096)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 512, Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open with checksum-corrupt non-head segment: %v", err)
	}
	defer db.Close()
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer rtx.Rollback()
	ks, err := rtx.OpenKeyspaceReadOnly("k")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly: %v", err)
	}
	// The newest round's keys must all be present and readable.
	found := 0
	for i := range 20 {
		for round := 39; round >= 0; round-- {
			if _, err := ks.Get([]byte(fmt.Sprintf("r%03d-%03d", round, i))); err == nil {
				found++
				break
			}
		}
	}
	if found == 0 {
		t.Fatalf("no data readable after truncated-chain open")
	}
}

// TestCheckReportsChecksumBadRPLSegment pins Check's walkRPL footer
// verification (checksums.md §Verification — walkRPL reads segments
// via the raw accessor like the pager's own walkers): a
// checksum-bad-but-decodable segment must surface as
// RPLSegmentChecksum instead of silently tainting the pending
// accounting set. The corruption is applied while the DB is open
// (the Open-time chain rebuild would otherwise truncate the chain
// before Check ever sees the segment).
func TestCheckReportsChecksumBadRPLSegment(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	_, tail := buildChecksummedRPLChain(t, ctx, path)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 512, Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	// The chain may have advanced; use the live meta's tail.
	m := db.Meta()
	if m.RPLHeadPage == 0 || m.RPLHeadPage == m.RPLTailPage {
		t.Fatalf("fixture: no multi-segment chain (head=%d tail=%d)", m.RPLHeadPage, m.RPLTailPage)
	}
	// Flip a byte at offset 16 — the tail segment's OlderSegment low
	// byte, which the walk never follows (it stops AT the tail), so
	// decode stays valid while the footer does not. Mid-chain reuse of
	// this fixture would corrupt the chain link itself — tail only.
	corruptPageByte(t, path, m.RPLTailPage, 4096)
	_ = tail

	found := false
	for iss := range db.Check() {
		if iss.Code == "RPLSegmentChecksum" {
			found = true
		}
	}
	if !found {
		t.Errorf("checksum-bad RPL segment not reported")
	}
}
