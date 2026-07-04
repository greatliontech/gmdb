package gmdb

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestCheckpointPublicationFailurePoisonsHandle pins the checkpoint
// failure contract (durability.md §Checkpoint failure semantics): a
// step-2/3/4 failure poisons the handle — a retried Checkpoint after
// a consumed fsync error would falsely certify non-durable data, and
// a torn active-slot write leaves the only on-disk copy of the meta
// invalid while this handle keeps serving it. Close + re-Open is the
// recovery path and must work.
func TestCheckpointPublicationFailurePoisonsHandle(t *testing.T) {
	for _, step := range []int{2, 3, 4} {
		t.Run(fmt.Sprintf("step-%d", step), func(t *testing.T) {
			ctx := context.Background()
			path := tmpPath(t)
			db, err := Open(ctx, path, Options{
				PageSize: 4096, MinSize: 16, MaxSize: 128,
				SyncMode:    SyncLazy, // steps 2-4 all run (meta not yet flagged)
				Maintenance: MaintenanceOptions{Disable: true},
			})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer db.Close()
			if err := db.Update(ctx, func(tx *Tx) error {
				ks, err := tx.CreateKeyspace("k")
				if err != nil {
					return err
				}
				return ks.Put([]byte("a"), []byte("v"))
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			injected := errors.New("injected I/O failure")
			restore := SetCheckpointStepHookForTest(func(s int) error {
				if s == step {
					return injected
				}
				return nil
			})
			err = db.Checkpoint(ctx)
			restore()
			if !errors.Is(err, injected) {
				t.Fatalf("Checkpoint: %v, want the injected step-%d error", err, step)
			}

			// Handle poisoned: every subsequent operation refuses.
			// Bounded ctx + rollback-on-unexpected-success keep the
			// probes hang-proof when the contract is violated (a
			// leaked write grant would otherwise block the next probe
			// forever — this bit an early mutation run).
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if tx, err := db.Begin(probeCtx); !errors.Is(err, ErrPoisoned) {
				t.Errorf("Begin after poisoned checkpoint: %v, want ErrPoisoned", err)
				if err == nil {
					_ = tx.Rollback()
				}
			}
			if rtx, err := db.BeginRead(probeCtx); !errors.Is(err, ErrPoisoned) {
				t.Errorf("BeginRead after poisoned checkpoint: %v, want ErrPoisoned", err)
				if err == nil {
					_ = rtx.Rollback()
				}
			}
			if err := db.Checkpoint(probeCtx); !errors.Is(err, ErrPoisoned) {
				t.Errorf("Checkpoint retry: %v, want ErrPoisoned (retry must not falsely certify)", err)
			}

			// Close + re-Open converges on the on-disk state.
			if err := db.Close(); err != nil {
				t.Fatalf("Close(poisoned): %v", err)
			}
			db2, err := Open(ctx, path, Options{
				PageSize: 4096, MinSize: 16, MaxSize: 128,
				Maintenance: MaintenanceOptions{Disable: true},
			})
			if err != nil {
				t.Fatalf("re-Open after poison: %v", err)
			}
			defer db2.Close()
			for iss := range db2.Check() {
				t.Errorf("Check issue after re-Open: %+v", iss)
			}
		})
	}
}

// TestCheckpointPoisonEdges pins two clauses around the poison
// contract: (a) a handle poisoned while Checkpoint waits for the
// write grant is refused by the post-grant re-check — the fsyncgate
// window where a concurrent commit's publication failure consumed
// the kernel error state and a proceeding checkpoint would falsely
// certify; (b) a pre-publication failure (grant acquisition) does
// NOT poison — the handle stays fully usable.
func TestCheckpointPoisonEdges(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 128,
		SyncMode:    SyncLazy,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx *Tx) error {
		_, err := tx.CreateKeyspace("k")
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	t.Run("pre-grant-failure-does-not-poison", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		tx, err := db.Begin(ctx) // hold the grant so Checkpoint must wait
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if err := db.Checkpoint(cancelled); err == nil {
			t.Fatalf("Checkpoint with cancelled ctx: nil error")
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if err := db.Checkpoint(ctx); err != nil {
			t.Fatalf("Checkpoint after pre-grant failure: %v (handle must not be poisoned)", err)
		}
	})

	t.Run("poisoned-while-waiting-for-grant", func(t *testing.T) {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		ks, err := tx.OpenKeyspace("k")
		if err != nil {
			t.Fatalf("OpenKeyspace: %v", err)
		}
		if err := ks.Put([]byte("x"), []byte("y")); err != nil {
			t.Fatalf("Put: %v", err)
		}

		cpErr := make(chan error, 1)
		cpStarted := make(chan struct{})
		go func() {
			close(cpStarted)
			cpErr <- db.Checkpoint(ctx) // blocks on the grant tx holds
		}()
		<-cpStarted
		time.Sleep(50 * time.Millisecond) // let Checkpoint pass its entry check and block

		// The commit fails in its publication phase and poisons the
		// handle, then releases the grant to the waiting Checkpoint.
		injected := errors.New("injected step-4 failure")
		db.PgrForTest().SetCommitStep4HookForTest(func() error { return injected })
		err = tx.Commit()
		db.PgrForTest().SetCommitStep4HookForTest(nil)
		if !errors.Is(err, injected) {
			t.Fatalf("Commit: %v, want injected publication failure", err)
		}

		if err := <-cpErr; !errors.Is(err, ErrPoisoned) {
			t.Fatalf("Checkpoint after poisoned-while-waiting: %v, want ErrPoisoned", err)
		}
	})
}
