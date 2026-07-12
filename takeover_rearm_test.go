package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/pager"
)

// Grant-handoff tear detection (free-space.md §Crash-torn
// reclamation): a peer writer that dies MID-RECLAMATION — after
// some bitmap pwrites, before its meta publish — leaves an on-disk
// image whose RPL walk truncates at a reclaimed boundary while a
// surviving handle's in-memory chain, built before the tear, still
// lists the truncated segments. No TxnID advanced, so the ordinary
// Resync equality skip would keep the stale chain, and reclamation
// behind that boundary double-frees. The fix: an acquisition that
// observes the died-holding-grant writer header bumps the lock
// file's takeover sequence, and every handle whose cached sequence
// lags forces the full bitmap+RPL rebuild from the on-disk image.

// buildPendingChain grows a multi-segment pending RPL chain (lazy
// sync keeps the anchored epoch behind, so nothing reclaims) and
// returns the db.
// fixturePageSize ties buildPendingChain's Options to
// tearSegmentBit's offset arithmetic — change them together.
const fixturePageSize = 4096

func buildPendingChain(t *testing.T, path string) *DB {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, path, Options{
		PageSize: fixturePageSize, MinSize: 16, MaxSize: 512, SyncMode: SyncLazy,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for round := 0; round < 40; round++ {
		if err := db.Update(ctx, func(tx *Tx) error {
			ks, kerr := tx.CreateKeyspaceIfNotExists("k")
			if kerr != nil {
				return kerr
			}
			for i := 0; i < 20; i++ {
				k := fmt.Sprintf("r%03d-%03d", round, i)
				if perr := ks.Put([]byte(k), bytes.Repeat([]byte{byte('A' + round%26)}, 400)); perr != nil {
					return perr
				}
			}
			if round > 0 {
				for i := 0; i < 15; i++ {
					k := fmt.Sprintf("r%03d-%03d", round-1, i)
					if derr := ks.Delete([]byte(k)); derr != nil {
						return derr
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("Update(%d): %v", round, err)
		}
		if len(db.PgrForTest().RPLChain()) >= 3 {
			return db
		}
	}
	t.Fatal("fixture: pending RPL chain never reached 3 segments")
	return nil
}

// tearSegmentBit flips the segment page's own bitmap bit to FREE
// directly in the FILE (the surviving handle's in-memory state is
// untouched) — the persisted fragment of a peer's torn reclamation.
func tearSegmentBit(t *testing.T, path string, pageID uint64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open file: %v", err)
	}
	defer f.Close()
	// Bitmap starts at page 2 (file-format: two meta slots first).
	off := int64(2*fixturePageSize) + int64(pageID/8)
	var b [1]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		t.Fatalf("read bitmap byte: %v", err)
	}
	b[0] |= 1 << (pageID % 8) // set = free
	if _, err := f.WriteAt(b[:], off); err != nil {
		t.Fatalf("write bitmap byte: %v", err)
	}
}

// The pager-level force semantics: an unforced Resync with an
// unchanged TxnID keeps the stale chain (the documented gap); a
// forced one rebuilds from the torn image and truncates at the
// reclaimed boundary.
func TestResyncForceRebuildsFromTornImage(t *testing.T) {
	path := tmpPath(t)
	db := buildPendingChain(t, path)
	defer db.Close()
	pgr := db.PgrForTest()
	before := pgr.RPLChain()
	n := len(before)
	// Tear a NON-HEAD segment: reclamation is oldest-first, so a
	// peer's torn pass frees tailward segments — and the walk's
	// head keeps its hard-error exemption (free-space.md §Head
	// classification), so only a non-head free bit is a reclaimed
	// boundary.
	torn := before[n-2]
	tearSegmentBit(t, path, torn.PageID)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	cur := db.Meta()

	// Resync's production position is "grant held, NO open tx"
	// (resyncPagerLocked runs at Begin, before the tx is built;
	// attachState under an OPEN tx corrupts the tx's bitmap-snapshot
	// lifecycle). A test cannot hold the grant without an open tx,
	// so this drives the pager bare — safe here because nothing else
	// touches it: single goroutine, maintenance disabled, no peers.

	// Unforced: TxnID unchanged — the stale chain survives. This is
	// the residual gap the takeover force closes; the assertion
	// documents WHY force exists.
	if _, _, changed, err := pgr.Resync(f, cur.TxnID, false); err != nil || changed {
		t.Fatalf("unforced Resync: changed=%v err=%v, want false/nil", changed, err)
	}
	if got := len(pgr.RPLChain()); got != n {
		t.Fatalf("unforced Resync rebuilt the chain (%d -> %d segments)", n, got)
	}

	// Forced: full rebuild from the image — the walk truncates AT
	// the torn segment (its own bit reads free ⇒ fully-reclaimed
	// interpretation) together with everything tailward: the chain
	// now agrees with what any fresh walker sees.
	if _, _, changed, err := pgr.Resync(f, cur.TxnID, true); err != nil || !changed {
		t.Fatalf("forced Resync: changed=%v err=%v, want true/nil", changed, err)
	}
	after := pgr.RPLChain()
	for _, seg := range after {
		if seg.PageID == torn.PageID {
			t.Fatalf("torn segment %d still in the rebuilt chain", torn.PageID)
		}
	}
	// The boundary truncates the torn segment AND everything
	// tailward; the head survives.
	if len(after) != 1 || after[0].PageID != before[n-1].PageID {
		t.Fatalf("chain after force = %d segments (want 1: the head %d), got %v", len(after), before[n-1].PageID, after)
	}
}

// The end-to-end signal wiring: a Begin after a grant acquisition
// observed a died-holding-grant writer header (bumping the takeover
// sequence) forces the rebuild; a Begin with a clean writer header
// and an unchanged sequence does not.
func TestTakeoverAfterPeerTornReclamationRebuildsChain(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db := buildPendingChain(t, path)
	defer db.Close()
	pgr := db.PgrForTest()
	before := pgr.RPLChain()
	n := len(before)
	torn := before[n-2] // non-head: the head keeps its exemption
	tearSegmentBit(t, path, torn.PageID)

	// Control: the writer header is clean (every prior holder
	// released with clear-before-unlock) — no takeover, no forced
	// rebuild, the stale chain survives the Begin.
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (control): %v", err)
	}
	if got := len(pgr.RPLChain()); got != n {
		t.Fatalf("control Begin rebuilt the chain (%d -> %d)", n, got)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Plant a died-holding-grant writer header (the kernel released
	// the dead holder's flock; the header survives un-cleared). The
	// next acquisition observes it — definitionally dead under
	// LOCK_EX, no liveness classification — and bumps the takeover
	// sequence past our cached value.
	db.CoordForTest().SetWriterRecordForTest(999999999, 1, 0, 1)

	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (takeover): %v", err)
	}
	defer tx.Rollback()
	after := pgr.RPLChain()
	for _, seg := range after {
		if seg.PageID == torn.PageID {
			t.Fatalf("takeover Begin kept the torn segment %d — reclamation behind the boundary would double-free", torn.PageID)
		}
	}
	if len(after) >= n {
		t.Fatalf("takeover Begin did not truncate the chain: %d -> %d", n, len(after))
	}
	// The forced rebuild must advance the handle's cached sequence
	// (grant still held via tx — the read is stable): otherwise every
	// later grant re-runs the full rebuild. free-space.md
	// §Grant-handoff tear detection: "at most once per takeover".
	if got, want := db.TakeoverSeqSeenForTest(), db.CoordForTest().TakeoverSeq(); got != want {
		t.Fatalf("cached takeover seq %d != lock header %d — force would re-fire on every grant", got, want)
	}
}

// The takeover signal must be LEVEL-triggered: the crashed holder's
// writer header is consumed by the first acquisition after the death
// (recovery clears it; the stamp overwrites it), but the tear poisons
// every handle whose chain predates it. Here a second handle of the
// same process — whose own Begin sees a clean writer header and an
// unchanged TxnID — must still force its rebuild: only the takeover
// sequence carries the signal past the laundering acquisition.
func TestTakeoverSeqReachesLaunderedHandles(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db1 := buildPendingChain(t, path)
	defer db1.Close()
	db2, err := Open(ctx, path, Options{
		PageSize: fixturePageSize, MinSize: 16, MaxSize: 512, SyncMode: SyncLazy,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open db2 (live join): %v", err)
	}
	defer db2.Close()
	pgr2 := db2.PgrForTest()
	before := pgr2.RPLChain()
	n := len(before)
	if n < 3 {
		t.Fatalf("fixture: db2 joined with %d RPL segments, want >= 3", n)
	}
	torn := before[n-2] // non-head: the head keeps its exemption
	tearSegmentBit(t, path, torn.PageID)

	// Plant the died-holding-grant header, then LAUNDER it: db1's
	// Begin consumes it (recovery clears the header, the acquisition
	// stamps this live process) and rolls back, so no TxnID advances
	// either.
	db1.CoordForTest().SetWriterRecordForTest(999999999, 1, 0, 1)
	tx1, err := db1.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin db1 (launder): %v", err)
	}
	if err := tx1.Rollback(); err != nil {
		t.Fatalf("Rollback db1: %v", err)
	}

	tx2, err := db2.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin db2: %v", err)
	}
	defer tx2.Rollback()
	for _, seg := range pgr2.RPLChain() {
		if seg.PageID == torn.PageID {
			t.Fatalf("laundered handle kept the torn segment %d — reclamation behind the boundary would double-free", torn.PageID)
		}
	}
}

// Compact's reopen adopts a full rebuild from the fresh on-disk
// image; it must refresh the takeover-sequence cache like Open's
// attach arms, or the takeover its OWN acquisition observed forces a
// redundant full rebuild on the next grant.
func TestCompactAdoptionRefreshesTakeoverCache(t *testing.T) {
	path := tmpPath(t)
	db := buildPendingChain(t, path)
	defer db.Close()
	db.CoordForTest().SetWriterRecordForTest(999999999, 1, 0, 1)
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got, want := db.TakeoverSeqSeenForTest(), db.CoordForTest().TakeoverSeq(); got != want || want == 0 {
		t.Fatalf("cached takeover seq %d, lock header %d (want equal, nonzero) — a lagging cache re-forces the rebuild Compact just did", got, want)
	}
}

// failBelowOffsetOps forwards to the production FileOps but fails any
// write landing below limit — aimed at the meta slots (pages 0-1) so
// a commit's step-1 data/bitmap/RPL pwrites land and its publication
// fails.
type failBelowOffsetOps struct {
	pager.FileOps
	limit int64
}

func (f failBelowOffsetOps) WriteAt(p []byte, off int64) (int, error) {
	if off < f.limit {
		return 0, errors.New("injected publication failure")
	}
	return f.FileOps.WriteAt(p, off)
}

// The grant-holder-side tear source: a publication-phase commit
// failure leaves the same torn unpublished image as a
// died-holding-grant crash, but the author releases cleanly (no
// WriterPID evidence for the next acquirer). The poisoning author
// must bump the takeover sequence itself, under the grant it still
// holds.
func TestPoisonedPublicationBumpsTakeoverSeq(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db := buildPendingChain(t, path)
	defer db.Close()
	c := db.CoordForTest()
	seq0 := c.TakeoverSeq()

	restore := db.SetWriterFileOpsForTest(failBelowOffsetOps{
		FileOps: db.WriterFileOpsForTest(),
		limit:   2 * fixturePageSize,
	})
	err := db.Update(ctx, func(tx *Tx) error {
		ks, kerr := tx.CreateKeyspaceIfNotExists("k")
		if kerr != nil {
			return kerr
		}
		return ks.Put([]byte("poison-me"), []byte("v"))
	})
	restore()
	if err == nil {
		t.Fatal("Update with failing meta pwrite: want error, got nil")
	}
	if _, berr := db.Begin(ctx); !errors.Is(berr, ErrPoisoned) {
		t.Fatalf("Begin after publication failure: got %v, want ErrPoisoned", berr)
	}
	if got := c.TakeoverSeq(); got != seq0+1 {
		t.Fatalf("takeover seq %d after poisoned publication, want %d — surviving handles would keep their pre-tear chains", got, seq0+1)
	}
}
