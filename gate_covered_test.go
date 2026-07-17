package gmdb

import (
	"context"
	"errors"
	"testing"
)

// TestCoveredThroughGateLifecycle pins the covered-through gate's
// state machine (durability.md §Anchoring; cross-process.md
// §Lock File Layout, RedirtyCoveredSeq): a healthy writable Open
// leaves the gate closed (covered == takeover), a publication-phase
// poison reopens it (the poison-site bump), and the next writable
// Open's covering rewrite closes it again.
func TestCoveredThroughGateLifecycle(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/gate.db"
	opts := Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}}

	db1, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db1.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("k")
		if e != nil {
			return e
		}
		return ks.Put([]byte("a"), []byte("1"))
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	// Accessor reads here and below run without the write grant —
	// benign in this single-process test (atomic loads, no concurrent
	// bumper); the MUST-hold contract targets cross-process stability.
	if c, s := db1.coord.RedirtyCoveredSeq(), db1.coord.TakeoverSeq(); c != s {
		t.Fatalf("healthy open: covered %d != takeover %d (gate should be closed)", c, s)
	}

	// A checkpoint publication failure poisons and bumps — the gate
	// must reopen.
	fail := errors.New("injected")
	hook := func(step int) error {
		if step == 2 {
			return fail
		}
		return nil
	}
	restore := SetCheckpointStepHookForTest(hook)
	err = db1.Checkpoint(ctx)
	restore()
	if err == nil {
		t.Fatal("hooked Checkpoint succeeded; want a poisoning failure")
	}
	if c, s := db1.coord.RedirtyCoveredSeq(), db1.coord.TakeoverSeq(); c == s {
		t.Fatalf("post-poison: covered %d == takeover %d (the poison-site bump must reopen the gate)", c, s)
	}
	db1.Close()

	// The documented recovery re-Open runs the covering rewrite and
	// closes the gate for everyone.
	db2, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer db2.Close()
	if c, s := db2.coord.RedirtyCoveredSeq(), db2.coord.TakeoverSeq(); c != s {
		t.Fatalf("recovery open: covered %d != takeover %d (the covering rewrite must close the gate)", c, s)
	}
	if err := db2.Update(ctx, func(tx *Tx) error {
		ks, e := tx.OpenKeyspace("k")
		if e != nil {
			return e
		}
		return ks.Put([]byte("b"), []byte("2"))
	}); err != nil {
		t.Fatalf("post-recovery update: %v", err)
	}
}
