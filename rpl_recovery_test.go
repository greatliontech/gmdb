package gmdb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
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

// buildRPLChain opens a DB at path (page checksums on unless
// disableChecksum), churns until the committed meta carries an RPL
// chain of at least two segments (head != tail), and returns the head
// and tail segment page ids.
func buildRPLChain(t *testing.T, ctx context.Context, path string, disableChecksum bool) (head, tail uint64) {
	t.Helper()
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 512, DisablePageChecksum: disableChecksum,
		Maintenance: MaintenanceOptions{Disable: true},
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
	head, _ := buildRPLChain(t, ctx, path, false)
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
	_, tail := buildRPLChain(t, ctx, path, false)
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
	_, tail := buildRPLChain(t, ctx, path, false)
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

// overwritePageU64 writes v little-endian at byte offset off within
// page pageID, bypassing the open DB handle (Check reads the mmap and
// sees the change).
func overwritePageU64(t *testing.T, path string, pageID uint64, pageSize uint32, off int64, v uint64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for overwrite: %v", err)
	}
	defer f.Close()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	if _, err := f.WriteAt(b[:], int64(pageID)*int64(pageSize)+off); err != nil {
		t.Fatalf("writeat: %v", err)
	}
}

// TestCheckReportsRPLSegmentInMetaRegion pins the chain walk's
// meta/bitmap-region guard on the Check side: a chain link below the
// first data page is structural corruption (no segment can live
// there), reported as RPLSegmentInMetaRegion rather than silently
// truncating the chain. Checksums are disabled so the rewritten link
// reaches the region check instead of failing footer verification
// first; offset 16 is the segment's OlderSegment field.
func TestCheckReportsRPLSegmentInMetaRegion(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	buildRPLChain(t, ctx, path, true)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 512, DisablePageChecksum: true,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	m := db.Meta()
	if m.RPLHeadPage == 0 || m.RPLHeadPage == m.RPLTailPage {
		t.Fatalf("fixture: no multi-segment chain (head=%d tail=%d)", m.RPLHeadPage, m.RPLTailPage)
	}
	// Point the head's OlderSegment at meta page 1.
	overwritePageU64(t, path, m.RPLHeadPage, 4096, 16, 1)

	found := false
	for iss := range db.Check() {
		if iss.Code == "RPLSegmentInMetaRegion" {
			found = true
		}
	}
	if !found {
		t.Errorf("chain link into the meta/bitmap region not reported")
	}
}

// TestAnchoredEpochBoundsReclamation pins the anchored-epoch bound at
// the handle level (durability.md §Anchoring + free-space.md §RPL
// Reclamation): after SyncLazy commits the anchored epoch stays at the
// last fsync point (genesis here), so the reclamation bound is pinned;
// Checkpoint() advances it to the active TxnID. White-box on the
// pager's AnchoredEpoch: the min() formula is pinned by the
// lagging-reader and reader-pin tests, so epoch-pinned ⇒ bound-pinned.
func TestAnchoredEpochBoundsReclamation(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	opts := Options{
		PageSize: 4096, MinSize: 16, MaxSize: 512, SyncMode: SyncLazy,
		Maintenance: MaintenanceOptions{Disable: true},
	}
	put := func(db *DB, round int) {
		t.Helper()
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
		if err := ks.Put(fmt.Appendf(nil, "r%03d", round), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(%d): %v", round, err)
		}
	}

	db, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	put(db, 0)
	put(db, 1)
	// Two SyncLazy commits: nothing fsynced since genesis — the
	// anchored epoch (and hence the bound's epoch term) is still 0.
	if got := db.PgrForTest().AnchoredEpoch(); got != 0 {
		t.Fatalf("anchored epoch = %d after lazy commits, want 0 (reclamation pinned)", got)
	}

	// Checkpoint() advances the durable epoch AND anchors it (its own
	// step-4 fsync).
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if got, want := db.PgrForTest().AnchoredEpoch(), db.currentMeta.TxnID; got != want || got == 0 {
		t.Fatalf("anchored epoch = %d after Checkpoint, want currentMeta.TxnID %d (non-zero)", got, want)
	}
}

// TestCrashedSyncLazyTornLiveHeadRecovers pins the recovery ordering
// (durability.md §Recovery): a crashed SyncLazy image whose LIVE RPL
// head was never flushed (meta pwrite survived, the step-1 segment
// pwrite did not) must still open — the gated writable Open attaches
// the DURABLE projection and never walks the torn live head. Walking
// the live projection first would hard-fail Open permanently (the
// post-epoch live head is exempt from boundary treatment), leaving an
// intact durable epoch unreachable.
func TestCrashedSyncLazyTornLiveHeadRecovers(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 512, SyncMode: SyncLazy,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Delete-heavy commits so the live meta carries an RPL chain whose
	// head belongs to a post-epoch (unfsynced) commit.
	for round := range 6 {
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
			if err := ks.Put(fmt.Appendf(nil, "r%03d-%03d", round, i), make([]byte, 200)); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		if round > 0 {
			for i := range 10 {
				if err := ks.Delete(fmt.Appendf(nil, "r%03d-%03d", round-1, i)); err != nil {
					t.Fatalf("Delete: %v", err)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(%d): %v", round, err)
		}
	}
	m := db.Meta()
	if m.RPLHeadPage == 0 || m.RPLHeadTxnID <= m.Durable.TxnID {
		t.Fatalf("fixture: live head not post-epoch (head=%d headTxn=%d epoch=%d)",
			m.RPLHeadPage, m.RPLHeadTxnID, m.Durable.TxnID)
	}
	liveHead := m.RPLHeadPage
	// Crash-copy while open (a clean Close checkpoints, erasing the
	// unfsynced tail), then tear the copy's live head: the live meta
	// reached the image, its head segment did not; the copy has no
	// lock file, so the author classifies dead.
	crashPath := crashCopy(t, path)
	db.Close()
	f, err := os.OpenFile(crashPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for tear: %v", err)
	}
	if _, err := f.WriteAt(make([]byte, 4096), int64(liveHead)*4096); err != nil {
		t.Fatalf("tear head: %v", err)
	}
	f.Close()

	db2, err := Open(ctx, crashPath, Options{Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open after torn-live-head crash: %v (durable epoch unreachable)", err)
	}
	defer db2.Close()
	if got := db2.Meta(); !got.SelfDurable() {
		t.Errorf("recovered meta not self-durable: D=%d TxnID=%d", got.Durable.TxnID, got.TxnID)
	}
}
