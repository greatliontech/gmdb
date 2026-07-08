package gmdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestSyncLazyCarriesSubRecordForward(t *testing.T) {
	// Per durability.md §Checkpoints and the durable sub-record: a
	// SyncLazy commit carries the previous meta's sub-record forward
	// unchanged — its own tree is not crash-durable.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		SyncMode: SyncLazy,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	// Genesis meta is self-durable at epoch 0 (api-surface.md
	// §Database Initialization).
	if m := db.Meta(); !m.SelfDurable() || m.Durable.TxnID != 0 {
		t.Fatalf("genesis meta: SelfDurable=%v Durable.TxnID=%d, want true/0", m.SelfDurable(), m.Durable.TxnID)
	}
	// SyncLazy commit carries epoch 0 forward.
	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.AllocPage()
		return e
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if m := db.Meta(); m.SelfDurable() || m.Durable.TxnID != 0 {
		t.Errorf("SyncLazy commit: SelfDurable=%v Durable.TxnID=%d, want false/0 (carried forward)", m.SelfDurable(), m.Durable.TxnID)
	}
}

func TestSyncDurableSelfDurableAndAnchored(t *testing.T) {
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
	m := db.Meta()
	if !m.SelfDurable() {
		t.Errorf("SyncDurable commit not self-durable: Durable.TxnID=%d TxnID=%d", m.Durable.TxnID, m.TxnID)
	}
	// The persisted anchored value must never run ahead of a completed
	// fsync at pwrite time (no-forward-promise): the meta was written
	// after step 2 but before its own step 4, so it may not name its
	// own assertion.
	if m.Durable.AnchoredTxnID >= m.TxnID {
		t.Errorf("persisted AnchoredTxnID %d >= own TxnID %d (forward promise)", m.Durable.AnchoredTxnID, m.TxnID)
	}
	// In-process, the completed step-4 anchors the commit's own
	// assertion immediately.
	if got := db.PgrForTest().AnchoredEpoch(); got != m.TxnID {
		t.Errorf("in-process anchored epoch = %d, want %d (own step-4 completed)", got, m.TxnID)
	}
}

func TestSyncDataOnlySelfDurableAnchorTrailsByOne(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		SyncMode: SyncDataOnly,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	for range 2 {
		if err := db.Update(ctx, func(tx *Tx) error {
			_, e := tx.AllocPage()
			return e
		}); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	m := db.Meta()
	if !m.SelfDurable() {
		t.Errorf("SyncDataOnly commit not self-durable (data IS durable post-step-2 fsync)")
	}
	// Pure SyncDataOnly: the anchored epoch trails the durable epoch
	// by exactly one commit (durability.md §Anchoring) — the own
	// assertion is anchored only by the NEXT fsync event.
	if got := db.PgrForTest().AnchoredEpoch(); got != m.TxnID-1 {
		t.Errorf("anchored epoch = %d, want %d (trails by one)", got, m.TxnID-1)
	}
}

func TestCheckpointBumpsSubRecord(t *testing.T) {
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
	if db.Meta().SelfDurable() {
		t.Fatal("SyncLazy commit should not be self-durable")
	}
	if err := db.Checkpoint(ctx); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	m := db.Meta()
	if !m.SelfDurable() {
		t.Errorf("post-Checkpoint meta not self-durable: Durable.TxnID=%d TxnID=%d", m.Durable.TxnID, m.TxnID)
	}
	// Checkpoint's step-4 anchors the bump in-process.
	if got := db.PgrForTest().AnchoredEpoch(); got != m.TxnID {
		t.Errorf("in-process anchored epoch = %d, want %d after Checkpoint", got, m.TxnID)
	}
	// The PERSISTED anchored value is the PRE-bump one (genesis 0 here)
	// — never the bump's own assertion (no-forward-promise).
	if m.Durable.AnchoredTxnID != 0 {
		t.Errorf("persisted AnchoredTxnID = %d, want 0 (pre-bump anchored)", m.Durable.AnchoredTxnID)
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

// TestRecoveryAdoptsDurableProjection pins the spec-tier invariant
// (durability.md §Recovery): recovery selects the highest-TxnID valid
// meta and adopts its DURABLE SUB-RECORD, then publishes it as a
// recovery commit (step 5) when no live author remains.
//
//  1. Genesis (both metas self-durable at TxnID 0).
//  2. One SyncDurable commit at TxnID=1 — self-durable epoch 1.
//  3. One SyncLazy commit at TxnID=2 — carries epoch 1 forward.
//  4. Crash-copy the file while the DB is open (a clean Close now
//     checkpoints per §Clean shutdown, so a Close-based fixture can
//     no longer produce an unfsynced tail) and open the copy — no
//     lock file exists for the copy, so the gate sees a dead author.
//     Recovery selects meta TxnID=2, adopts epoch 1's tree, and
//     publishes the recovery commit at TxnID=3.
func TestRecoveryAdoptsDurableProjection(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		SyncMode:    SyncDurable,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.AllocPage()
		return e
	}); err != nil {
		t.Fatalf("W1: %v", err)
	}
	epoch1 := db.Meta()
	if epoch1.TxnID != 1 || !epoch1.SelfDurable() {
		t.Fatalf("after W1: TxnID=%d SelfDurable=%v, want 1/true", epoch1.TxnID, epoch1.SelfDurable())
	}
	db.opts.SyncMode = SyncLazy
	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.AllocPage()
		return e
	}); err != nil {
		t.Fatalf("W2: %v", err)
	}
	if m := db.Meta(); m.TxnID != 2 || m.Durable.TxnID != 1 {
		t.Fatalf("after W2: TxnID=%d Durable.TxnID=%d, want 2/1", m.TxnID, m.Durable.TxnID)
	}
	crashPath := crashCopy(t, path)
	db.Close()

	db2, err := Open(ctx, crashPath, Options{Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	m := db2.Meta()
	if m.TxnID != 3 {
		t.Errorf("recovered TxnID = %d, want 3 (recovery commit at selected TxnID+1)", m.TxnID)
	}
	if !m.SelfDurable() {
		t.Errorf("recovery commit not self-durable: Durable.TxnID=%d", m.Durable.TxnID)
	}
	if m.HighWaterMark != epoch1.HighWaterMark {
		t.Errorf("recovered HighWaterMark = %d, want epoch 1's %d (durable projection adopted)",
			m.HighWaterMark, epoch1.HighWaterMark)
	}
	if m.Durable.AnchoredTxnID != epoch1.TxnID {
		t.Errorf("recovery commit AnchoredTxnID = %d, want the adopted epoch %d (no-forward-promise)", m.Durable.AnchoredTxnID, epoch1.TxnID)
	}
}

// TestLiveJoinDoesNotRollBack pins the recovery-commit gate's other
// half (durability.md §Recovery step 5): while the last writer's
// process is alive — even idle, holding no grant and no reader slots
// — a writable Open is a live join and must NOT roll back its
// unfsynced SyncLazy commits. The author handle stays OPEN (a clean
// Close checkpoints, erasing the unfsynced tail this test needs); the
// joiner is a second handle in the same process.
func TestLiveJoinDoesNotRollBack(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		SyncMode:    SyncLazy,
		Maintenance: MaintenanceOptions{Disable: true},
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

	// The author is alive and idle: the LastWriter record names this
	// (live) process, so the joiner's gate refuses and the live tree
	// stands.
	db2, err := Open(ctx, path, Options{Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("join Open: %v", err)
	}
	defer db2.Close()
	if m := db2.Meta(); m.TxnID != 1 || m.SelfDurable() {
		t.Errorf("live join: TxnID=%d SelfDurable=%v, want 1/false (live projection kept, no recovery commit)",
			m.TxnID, m.SelfDurable())
	}
}
func TestRecoverySingleValidSlotAdoptsItsSubRecord(t *testing.T) {
	// durability.md §Recovery with one valid slot: selection falls to
	// the surviving meta and recovery adopts ITS durable sub-record.
	// Construct: SyncLazy from the start, one commit (meta-1 at
	// TxnID=1 carrying epoch 0), tamper meta-0 so only meta-1
	// survives, dead-author reopen → recovery commit publishes epoch
	// 0's (genesis) tree at TxnID=2.
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		SyncMode: SyncLazy,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// SyncLazy commit puts the carried-forward meta on disk (page
	// cache); crash-copy while open — a clean Close would checkpoint.
	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.AllocPage()
		return e
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	path = crashCopy(t, path)
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

	// Re-Open the crash copy (no lock file — dead author): meta-0
	// corrupt; meta-1 valid (TxnID=1, Durable.TxnID=0). Recovery
	// adopts epoch 0 and publishes the recovery commit at TxnID=2.
	db2, err := Open(ctx, path, Options{Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("re-Open with corrupt meta-0 + lazy meta-1: %v", err)
	}
	defer db2.Close()
	m := db2.Meta()
	if m.TxnID != 2 || !m.SelfDurable() {
		t.Errorf("recovered: TxnID=%d SelfDurable=%v, want 2/true (recovery commit over epoch 0)", m.TxnID, m.SelfDurable())
	}
	if m.KeyspaceRoot != 0 {
		t.Errorf("recovered KeyspaceRoot = %d, want 0 (genesis epoch adopted)", m.KeyspaceRoot)
	}
}

// TestCleanCloseCheckpointsSyncLazy pins durability.md §Clean
// shutdown: a writable non-poisoned Close performs the Checkpoint
// sequence, so a pure-SyncLazy application that never calls
// Checkpoint() reopens — even after author death — with everything it
// committed. Without the shutdown checkpoint the dead-author reopen
// would roll back to genesis.
func TestCleanCloseCheckpointsSyncLazy(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		SyncMode:    SyncLazy,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("k")
		if e != nil {
			return e
		}
		return ks.Put([]byte("key"), []byte("value"))
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	lastTxnID := db.Meta().TxnID
	if db.Meta().SelfDurable() {
		t.Fatal("fixture: SyncLazy commit unexpectedly self-durable")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatalf("rm lock: %v", err)
	}

	db2, err := Open(ctx, path, Options{Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	m := db2.Meta()
	// The clean Close made the final meta self-durable, so the gated
	// reopen anchors it in place — same TxnID, no recovery commit.
	if m.TxnID != lastTxnID || !m.SelfDurable() {
		t.Fatalf("reopen: TxnID=%d SelfDurable=%v, want %d/true (clean close checkpointed)", m.TxnID, m.SelfDurable(), lastTxnID)
	}
	if err := db2.View(ctx, func(tx *ReadTx) error {
		ks, e := tx.OpenKeyspaceReadOnly("k")
		if e != nil {
			return e
		}
		v, e := ks.Get([]byte("key"))
		if e != nil {
			return e
		}
		if string(v) != "value" {
			t.Errorf("value = %q, want %q", v, "value")
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestPoisonedCloseSkipsShutdownCheckpoint pins §Clean shutdown's
// poison rule: a poisoned handle must NOT run the shutdown checkpoint
// (the retried-fsync trap — it would stamp a durable sub-record over
// data that may never have reached storage). Poison via the
// checkpoint step hook, then Close; the dead-author reopen must roll
// back to the durable epoch, proving no bump was stamped.
func TestPoisonedCloseSkipsShutdownCheckpoint(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		SyncMode:    SyncLazy,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.AllocPage()
		return e
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	lazyTxnID := db.Meta().TxnID

	// Poison through a failing Checkpoint step-2 (the sanctioned
	// publication-failure path).
	restore := SetCheckpointStepHookForTest(func(step int) error {
		if step == 2 {
			return fmt.Errorf("injected step-2 failure")
		}
		return nil
	})
	err = db.Checkpoint(ctx)
	restore()
	if err == nil {
		t.Fatal("Checkpoint with injected failure returned nil")
	}
	if !db.poisoned.Load() {
		t.Fatal("handle not poisoned after publication failure")
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close on poisoned handle: %v (teardown must proceed)", err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatalf("rm lock: %v", err)
	}
	db2, err := Open(ctx, path, Options{Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	// The poisoned Close stamped nothing: recovery rolled the lazy
	// commit back, publishing the recovery commit at lazyTxnID+1 over
	// the genesis epoch — rather than finding a shutdown-checkpointed
	// self-durable meta at lazyTxnID.
	m := db2.Meta()
	if m.TxnID == lazyTxnID && m.SelfDurable() {
		t.Fatal("reopen found a self-durable meta at the lazy TxnID — poisoned Close ran the shutdown checkpoint")
	}
	if m.TxnID != lazyTxnID+1 || m.Durable.AnchoredTxnID != 0 {
		t.Errorf("reopen: TxnID=%d A=%d, want %d/0 (recovery commit over genesis)", m.TxnID, m.Durable.AnchoredTxnID, lazyTxnID+1)
	}
}

// TestCloseWithLiveWriteTxSkipsShutdownCheckpoint pins the deadlock
// guard: Close while this handle's own write transaction holds the
// grant must return promptly (skipping the shutdown checkpoint)
// rather than block on a grant that cannot release until after Close
// returns.
func TestCloseWithLiveWriteTxSkipsShutdownCheckpoint(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		SyncMode:    SyncLazy,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- db.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close blocked on the live write transaction's grant")
	}
	_ = tx.Rollback()
}

// crashCopy byte-copies the open database file to a fresh path and
// returns it — the crash-simulation idiom: the copy captures the OS
// page cache's view (unfsynced SyncLazy commits included) with no
// lock file, so a subsequent Open classifies a dead author. Copying
// while the DB is open matters: a clean Close checkpoints
// (durability.md §Clean shutdown) and would erase the unfsynced tail.
func crashCopy(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("crashCopy read: %v", err)
	}
	copyPath := filepath.Join(t.TempDir(), "crash.gmdb")
	if err := os.WriteFile(copyPath, data, 0o600); err != nil {
		t.Fatalf("crashCopy write: %v", err)
	}
	return copyPath
}
