package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// Cross-process coordination harness: two simultaneously-live handles on a
// single file (two os.Open file descriptions → flock contends correctly, so
// one in-process pair is a faithful proxy for two processes — the corruption
// is per-handle in-memory state, not anything the shared lock file mediates).
//
// This is the acceptance gate for the re-sync fix (a writer/reader that
// acquires the grant — or a reader that begins — must rebuild its in-memory
// meta/bitmap/RPL from the current on-disk state, never a view that predates a
// peer's commit). Every existing multi-handle test uses open→use→Close→reopen;
// none keeps two handles live while one observes the other's commit, which is
// exactly why the staleness corruption went unnoticed. Run under -race.

func cpOpenMode(t *testing.T, path string, mode SyncMode) *DB {
	t.Helper()
	db, err := Open(context.Background(), path, Options{
		PageSize: 4096, MinSize: 64, MaxSize: 4096, SyncMode: mode,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	return db
}

func cpOpen(t *testing.T, path string) *DB { return cpOpenMode(t, path, SyncDurable) }

func cpUpdate(t *testing.T, db *DB, fn func(tx *Tx) error) {
	t.Helper()
	if err := db.Update(context.Background(), fn); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func cpCheckClean(t *testing.T, db *DB, tag string) {
	t.Helper()
	for _, iss := range collectIssues(db.Check()) {
		if iss.Severity == CheckError || iss.Severity == CheckFatal {
			t.Fatalf("%s: Check reported %v %s (page %d): %s", tag, iss.Severity, iss.Code, iss.PageID, iss.Message)
		}
	}
}

// cpBootstrap creates keyspace "ks" (optionally seeding keys) on a throwaway
// handle and closes it, so peer handles opened afterward share a snapshot in
// which "ks" exists.
func cpBootstrap(t *testing.T, path string, seed func(ks *Keyspace) error) {
	cpBootstrapMode(t, path, SyncDurable, seed)
}

func cpBootstrapMode(t *testing.T, path string, mode SyncMode, seed func(ks *Keyspace) error) {
	t.Helper()
	boot := cpOpenMode(t, path, mode)
	cpUpdate(t, boot, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("ks")
		if err != nil {
			return err
		}
		if seed != nil {
			return seed(ks)
		}
		return nil
	})
	if err := boot.Close(); err != nil {
		t.Fatalf("bootstrap Close: %v", err)
	}
}

// TestCrossProcessReaderSeesPeerCommit — case (a). Handle B is already open
// when handle A commits k1; a fresh read tx on B must observe k1 and pin the
// post-commit TxnID. Pre-fix B's read tx snapshots B's Open-time meta (never
// refreshed after a peer commit), so k1 is invisible and the pinned TxnID is
// stale.
func TestCrossProcessReaderSeesPeerCommit(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	cpBootstrap(t, path, nil)

	a := cpOpen(t, path)
	defer a.Close()
	b := cpOpen(t, path)
	defer b.Close()

	cpUpdate(t, a, func(tx *Tx) error {
		ks, err := tx.OpenKeyspace("ks")
		if err != nil {
			return err
		}
		return ks.Put([]byte("k1"), []byte("v1"))
	})

	if err := b.View(ctx, func(rtx *ReadTx) error {
		ks, err := rtx.OpenKeyspaceReadOnly("ks")
		if err != nil {
			return err
		}
		v, err := ks.Get([]byte("k1"))
		if err != nil {
			return err
		}
		if !bytes.Equal(v, []byte("v1")) {
			t.Fatalf("handle B read k1=%q, want v1 — B's read tx never observed A's commit (stale meta)", v)
		}
		return nil
	}); err != nil {
		t.Fatalf("B.View: %v", err)
	}
}

// TestCrossProcessInterleavedWritersNoLostUpdate — case (b), the core
// corruption test. Two handles alternate committing distinct keys. Pre-fix,
// each handle builds its tx on its own stale Open-time root: commits are lost
// (the other handle's keys never visited), the meta is written to the wrong
// slot (clobbering the peer's newer meta → two metas claiming one TxnID), and
// the stale bitmap hands out pages the peer's committed tree references (page
// aliasing). Post-fix every interleaved write survives and the file is clean.
func TestCrossProcessInterleavedWritersNoLostUpdate(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	cpBootstrap(t, path, nil)

	a := cpOpen(t, path)
	b := cpOpen(t, path)

	const rounds = 20
	put := func(db *DB, key string) {
		cpUpdate(t, db, func(tx *Tx) error {
			ks, err := tx.OpenKeyspace("ks")
			if err != nil {
				return err
			}
			return ks.Put([]byte(key), []byte("v"))
		})
	}
	for i := range rounds {
		put(a, fmt.Sprintf("a/%03d", i))
		put(b, fmt.Sprintf("b/%03d", i))
	}
	if err := a.Close(); err != nil {
		t.Fatalf("a.Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("b.Close: %v", err)
	}

	// Fresh handle: structurally clean (no clobbered meta / aliased pages)
	// and every interleaved write survived (no lost update).
	c := cpOpen(t, path)
	defer c.Close()
	cpCheckClean(t, c, "after interleaved cross-handle writers")
	if err := c.View(ctx, func(rtx *ReadTx) error {
		ks, err := rtx.OpenKeyspaceReadOnly("ks")
		if err != nil {
			return err
		}
		for i := range rounds {
			for _, key := range []string{fmt.Sprintf("a/%03d", i), fmt.Sprintf("b/%03d", i)} {
				v, err := ks.Get([]byte(key))
				if err != nil {
					t.Fatalf("Get(%s): %v", key, err)
				}
				if !bytes.Equal(v, []byte("v")) {
					t.Fatalf("key %s = %q, want v — cross-handle lost update / corruption", key, v)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("C.View: %v", err)
	}
}

// TestCrossProcessCheckpointDoesNotClobberPeerCommit — Checkpoint() acquires
// the write grant then (pre-fix) re-flags + pwrites its own stale
// db.currentMeta back to its stale db.activeMetaIdx slot (checkpoint.go step
// 3), overwriting whatever a peer has since committed to that slot.
//
// This is a genuine SIBLING of the write-tx staleness, and the scenario pins
// it as a 3-state gate (verified red unfixed, red with the Begin/Commit
// re-sync alone, green only once Checkpoint re-syncs too):
//
//   - "ks"+k0 are checkpointed (SyncDurable bootstrap) so both handles recover
//     them; the live handles run SyncLazy so commits are unflagged.
//   - B commits kb → B's cached meta is now unflagged (so Checkpoint step 3
//     actually re-pwrites) and B's cached activeMetaIdx is slot0.
//   - A commits ka1, ka2 (even count → A's newest meta lands back on slot0).
//   - B.Checkpoint targets its stale slot0. Without the Checkpoint re-sync it
//     writes B's stale kb-era meta over A's newest (ka2) meta; recovery then
//     lands on a meta whose CoW lineage is missing ka1/ka2.
//
// With both re-syncs in place B adopts A's newest meta before re-flagging it,
// so every key (k0, kb, ka1, ka2 — one CoW lineage) survives.
func TestCrossProcessCheckpointDoesNotClobberPeerCommit(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	cpBootstrap(t, path, func(ks *Keyspace) error { // SyncDurable: ks+k0 recoverable
		return ks.Put([]byte("k0"), []byte("v0"))
	})

	a := cpOpenMode(t, path, SyncLazy)
	defer a.Close()
	b := cpOpenMode(t, path, SyncLazy)
	defer b.Close()

	put := func(db *DB, k, v string) {
		cpUpdate(t, db, func(tx *Tx) error {
			ks, err := tx.OpenKeyspace("ks")
			if err != nil {
				return err
			}
			return ks.Put([]byte(k), []byte(v))
		})
	}
	put(b, "kb", "vb")  // B's cached meta now unflagged; activeMetaIdx = slot0
	put(a, "ka1", "va1")
	put(a, "ka2", "va2") // even A-commit count returns the newest meta to slot0

	// B checkpoints against its stale slot0 — must not clobber A's ka2 meta.
	if err := b.Checkpoint(ctx); err != nil {
		t.Fatalf("B.Checkpoint: %v", err)
	}

	c := cpOpen(t, path)
	defer c.Close()
	cpCheckClean(t, c, "after peer Checkpoint over fresh commits")
	if err := c.View(ctx, func(rtx *ReadTx) error {
		ks, err := rtx.OpenKeyspaceReadOnly("ks")
		if err != nil {
			return err
		}
		for k, want := range map[string]string{"k0": "v0", "kb": "vb", "ka1": "va1", "ka2": "va2"} {
			v, err := ks.Get([]byte(k))
			if err != nil {
				t.Fatalf("Get(%s): %v", k, err)
			}
			if !bytes.Equal(v, []byte(want)) {
				t.Fatalf("key %s = %q, want %s — peer Checkpoint clobbered the commit", k, v, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("C.View: %v", err)
	}
}

// TestCrossProcessLongLivedReaderSurvivesReclaim — case (c). A reader on
// handle B pins a snapshot; handle A then churns (Put/Delete of large values)
// for many rounds, retiring pages. The reader's pin must floor the writer's
// reclamation bound (free-space.md §RPL Reclamation), so its snapshot pages
// are never reclaimed and its reads stay intact. Guards both that reader
// pinning holds back cross-handle reclamation and that the re-sync changes
// don't break it. (The future-heartbeat underflow that could falsely evict
// this reader is exercised deterministically in internal/lock reader_test.go.)
func TestCrossProcessLongLivedReaderSurvivesReclaim(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	pinVal := bytes.Repeat([]byte("p"), 600)
	cpBootstrap(t, path, func(ks *Keyspace) error {
		for j := range 8 {
			if err := ks.Put(fmt.Appendf(nil, "pin/%02d", j), pinVal); err != nil {
				return err
			}
		}
		return nil
	})

	a := cpOpen(t, path)
	defer a.Close()
	b := cpOpen(t, path)
	defer b.Close()

	rtx, err := b.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer rtx.Rollback()
	pinnedTxn := rtx.TxnID()
	rks, err := rtx.OpenKeyspaceReadOnly("ks")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly: %v", err)
	}

	for round := range 40 {
		cpUpdate(t, a, func(tx *Tx) error {
			ks, err := tx.OpenKeyspace("ks")
			if err != nil {
				return err
			}
			for j := range 6 {
				if err := ks.Put(fmt.Appendf(nil, "churn/%03d/%02d", round, j), bytes.Repeat([]byte("c"), 700)); err != nil {
					return err
				}
			}
			for j := range 4 {
				_ = ks.Delete(fmt.Appendf(nil, "churn/%03d/%02d", round-1, j))
			}
			return nil
		})
	}

	if got := rtx.TxnID(); got != pinnedTxn {
		t.Fatalf("pinned reader TxnID drifted: %d != %d", got, pinnedTxn)
	}
	for j := range 8 {
		v, err := rks.Get(fmt.Appendf(nil, "pin/%02d", j))
		if err != nil {
			t.Fatalf("pinned Get(pin/%02d): %v", j, err)
		}
		if !bytes.Equal(v, pinVal) {
			t.Fatalf("pinned read pin/%02d corrupted (len %d) — snapshot pages reclaimed under the reader", j, len(v))
		}
	}
}
