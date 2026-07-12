package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/greatliontech/gmdb/internal/pager"
)

// forgePeerCheckpointBump rewrites the active meta slot as a peer's
// Checkpoint step 3 would: durable sub-record bumped to the meta's
// own live state, AnchoredTxnID kept at the previously persisted
// value, checksum recomputed, same slot, TxnID unchanged. The
// kept-anchor is byte-faithful for a pure-SyncLazy chain (there the
// peer's post-step-2 anchored epoch coincides with the previously
// persisted value; in mixed chains a real bump may write a newer
// anchor — irrelevant to what these tests pin). Returns the bumped
// on-disk bytes and the slot offset.
func forgePeerCheckpointBump(t *testing.T, path string, txnID uint64) ([]byte, int64) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	for slot := int64(0); slot < 2; slot++ {
		buf := raw[slot*4096 : (slot+1)*4096]
		if !pager.VerifyMeta(buf) {
			continue
		}
		m := pager.DecodeMeta(buf)
		if m.TxnID != txnID {
			continue
		}
		preAnchor := m.Durable.AnchoredTxnID
		m.Durable = m.LiveSubRecord()
		m.Durable.AnchoredTxnID = preAnchor
		out := make([]byte, 4096)
		pager.EncodeMeta(out, &m)
		f, err := os.OpenFile(path, os.O_RDWR, 0o600)
		if err != nil {
			t.Fatalf("open for bump: %v", err)
		}
		defer f.Close()
		if _, err := f.WriteAt(out, slot*4096); err != nil {
			t.Fatalf("write bump: %v", err)
		}
		if err := f.Sync(); err != nil {
			t.Fatalf("sync bump: %v", err)
		}
		return out, slot * 4096
	}
	t.Fatalf("no valid meta slot at TxnID %d", txnID)
	return nil, 0
}

// peerBumpFixture: a SyncLazy handle with committed state whose active
// meta then receives a PEER's checkpoint bump — the durable sub-record
// changes while TxnID does not, the exact shape a TxnID-equality
// resync gate goes stale on.
func peerBumpFixture(t *testing.T) (*DB, string, pager.Meta) {
	t.Helper()
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 512, SyncMode: SyncLazy,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for round := range 3 {
		tx, _ := db.Begin(ctx)
		ks, err := tx.OpenKeyspace("k")
		if err != nil {
			if ks, err = tx.CreateKeyspace("k"); err != nil {
				t.Fatalf("CreateKeyspace: %v", err)
			}
		}
		for i := range 20 {
			if err := ks.Put(fmt.Appendf(nil, "r%d-%03d", round, i), []byte("v")); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	pre := db.Meta()
	if pre.SelfDurable() {
		t.Fatal("fixture: SyncLazy meta unexpectedly self-durable")
	}
	bumped, _ := forgePeerCheckpointBump(t, path, pre.TxnID)
	return db, path, pager.DecodeMeta(bumped)
}

// A peer's checkpoint bump changes the active slot's durable
// sub-record WITHOUT changing TxnID. The handle's next grant
// acquisition must refresh its cached meta anyway: a TxnID-equality
// gate leaves the cache pre-bump, and the next commit's meta then
// carries a RETREATED durable epoch — a crash after that commit
// discards the epochs the peer's checkpoint made durable and
// acknowledged (durable-epoch monotonicity across commits).
func TestPeerCheckpointBumpSurvivesNextCommit(t *testing.T) {
	ctx := context.Background()
	db, path, bumpedMeta := peerBumpFixture(t)

	tx, _ := db.Begin(ctx)
	ks, _ := tx.OpenKeyspace("k")
	if err := ks.Put([]byte("after-bump"), []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// The NEW meta (TxnID+1) must not retreat below the bumped durable
	// epoch.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var newest pager.Meta
	for slot := 0; slot < 2; slot++ {
		buf := raw[slot*4096 : (slot+1)*4096]
		if pager.VerifyMeta(buf) {
			if m := pager.DecodeMeta(buf); m.TxnID > newest.TxnID {
				newest = m
			}
		}
	}
	if newest.TxnID != bumpedMeta.TxnID+1 {
		t.Fatalf("newest meta TxnID = %d, want %d", newest.TxnID, bumpedMeta.TxnID+1)
	}
	if newest.Durable.TxnID < bumpedMeta.Durable.TxnID {
		t.Fatalf("durable epoch RETREATED: new meta carries %d, the peer's checkpoint had made %d durable — a crash now discards acknowledged-durable epochs",
			newest.Durable.TxnID, bumpedMeta.Durable.TxnID)
	}
}

// After the peer's bump the active meta is SELF-DURABLE: the handle's
// own Checkpoint must take the sole-carrier SKIP (durability.md
// §Checkpoint mechanics step 3) — a stale pre-bump cached meta would
// route to the bump branch and pwrite CHANGED bytes over the slot
// that is the sole durable carrier of its own assertion, the exact
// torn-fsync hazard the skip exists to prevent.
func TestPeerCheckpointBumpThenOwnCheckpointSkips(t *testing.T) {
	ctx := context.Background()
	db, path, bumpedMeta := peerBumpFixture(t)

	// The tear-safe anchor gate's effect (pinned in internal/pager):
	// an eager-reclaim transaction adopted the bumped meta and
	// advanced the in-process anchored epoch to its DurableTxnID; the
	// transaction then rolled back (the advance rightly survives the
	// rollback — it names a completed fsync). The handle's anchored
	// knowledge now RUNS AHEAD of the persisted field, so a stale
	// cached meta at Checkpoint would produce genuinely CHANGED bytes.
	db.PgrForTest().AdvanceAnchoredEpoch(bumpedMeta.Durable.TxnID)

	slotOff := int64(-1)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for slot := int64(0); slot < 2; slot++ {
		buf := raw[slot*4096 : (slot+1)*4096]
		if pager.VerifyMeta(buf) && pager.DecodeMeta(buf).TxnID == bumpedMeta.TxnID {
			slotOff = slot * 4096
		}
	}
	if slotOff < 0 {
		t.Fatal("bumped slot not found")
	}
	before := append([]byte(nil), raw[slotOff:slotOff+4096]...)

	if err := db.Checkpoint(ctx); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	after := make([]byte, 4096)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if _, err := f.ReadAt(after, slotOff); err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("own Checkpoint rewrote the self-durable slot with changed bytes (stale cached meta routed to the bump branch instead of the sole-carrier skip)")
	}
}
