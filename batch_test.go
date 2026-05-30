package gmdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// batchTestDB opens a DB and creates an empty keyspace "ks" so batch
// closures can OpenKeyspace it.
func batchTestDB(t *testing.T) *DB {
	t.Helper()
	db := openNestedTestDB(t)
	tx, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.CreateKeyspace("ks"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return db
}

// TestBatchExactlyOnceAllLand (Inv-N3): N concurrent Batch calls each run
// their closure exactly once and all writes are durable.
func TestBatchExactlyOnceAllLand(t *testing.T) {
	ctx := context.Background()
	db := batchTestDB(t)

	const n = 64
	var invocations atomic.Int64
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = db.Batch(ctx, func(tx *Tx) error {
				invocations.Add(1)
				ks, err := tx.OpenKeyspace("ks")
				if err != nil {
					return err
				}
				return ks.Put(fmt.Appendf(nil, "k%03d", i), fmt.Appendf(nil, "v%03d", i))
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("Batch[%d] err = %v", i, err)
		}
	}
	if got := invocations.Load(); got != n {
		t.Errorf("closure invocations = %d, want %d (exactly once each)", got, n)
	}

	// All keys durable.
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.OpenKeyspace("ks")
	for i := range n {
		v, err := ks.Get(fmt.Appendf(nil, "k%03d", i))
		if err != nil {
			t.Errorf("Get k%03d: %v", i, err)
			continue
		}
		if want := fmt.Sprintf("v%03d", i); string(v) != want {
			t.Errorf("k%03d = %q, want %q", i, v, want)
		}
	}
}

// TestBatchClosureErrorIsolated (Inv-N4): a failing closure's caller gets
// the error; sibling closures commit.
func TestBatchClosureErrorIsolated(t *testing.T) {
	ctx := context.Background()
	db := batchTestDB(t)

	sentinel := errors.New("boom")
	var wg sync.WaitGroup
	const n = 16
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = db.Batch(ctx, func(tx *Tx) error {
				ks, err := tx.OpenKeyspace("ks")
				if err != nil {
					return err
				}
				if i == 7 {
					// Write then fail — the write must be rolled back.
					_ = ks.Put([]byte("k007"), []byte("doomed"))
					return sentinel
				}
				return ks.Put(fmt.Appendf(nil, "k%03d", i), []byte("ok"))
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if i == 7 {
			if !errors.Is(err, sentinel) {
				t.Errorf("Batch[7] err = %v, want sentinel", err)
			}
		} else if err != nil {
			t.Errorf("Batch[%d] err = %v, want nil", i, err)
		}
	}

	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.OpenKeyspace("ks")
	// Failed closure's write rolled back.
	if _, err := ks.Get([]byte("k007")); !errors.Is(err, ErrNotFound) {
		t.Errorf("failed-closure key k007 present: %v", err)
	}
	// A sibling's write landed.
	if v, err := ks.Get([]byte("k000")); err != nil || string(v) != "ok" {
		t.Errorf("sibling k000 = %q, %v; want ok, nil", v, err)
	}
}

// TestBatchClosurePanicIsolated: a panicking closure's caller gets
// ErrBatchClosurePanic; siblings commit; the coordinator survives.
func TestBatchClosurePanicIsolated(t *testing.T) {
	ctx := context.Background()
	db := batchTestDB(t)

	var wg sync.WaitGroup
	const n = 16
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = db.Batch(ctx, func(tx *Tx) error {
				ks, err := tx.OpenKeyspace("ks")
				if err != nil {
					return err
				}
				if i == 5 {
					panic("closure boom")
				}
				return ks.Put(fmt.Appendf(nil, "k%03d", i), []byte("ok"))
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if i == 5 {
			if !errors.Is(err, ErrBatchClosurePanic) {
				t.Errorf("Batch[5] err = %v, want ErrBatchClosurePanic", err)
			}
		} else if err != nil {
			t.Errorf("Batch[%d] err = %v, want nil", i, err)
		}
	}

	// Coordinator survived: a fresh batch still works.
	if err := db.Batch(ctx, func(tx *Tx) error {
		ks, _ := tx.OpenKeyspace("ks")
		return ks.Put([]byte("after"), []byte("ok"))
	}); err != nil {
		t.Errorf("post-panic Batch err = %v", err)
	}

	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.OpenKeyspace("ks")
	if v, err := ks.Get([]byte("k000")); err != nil || string(v) != "ok" {
		t.Errorf("sibling k000 = %q, %v; want ok, nil", v, err)
	}
	if v, err := ks.Get([]byte("after")); err != nil || string(v) != "ok" {
		t.Errorf("after = %q, %v; want ok, nil", v, err)
	}
}

// TestBatchClosureLeavesChildOpen (H1 regression): a closure that calls
// BeginChild without resolving the grandchild must fail only that caller —
// the coordinator cascade-resolves the orphan so the batch tx is not left
// frozen, and sibling closures still commit.
func TestBatchClosureLeavesChildOpen(t *testing.T) {
	ctx := context.Background()
	db := batchTestDB(t)

	var wg sync.WaitGroup
	const n = 8
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = db.Batch(ctx, func(tx *Tx) error {
				ks, err := tx.OpenKeyspace("ks")
				if err != nil {
					return err
				}
				if i == 3 {
					// Misuse: open a nested child and never resolve it.
					if _, err := tx.BeginChild(); err != nil {
						return err
					}
					return nil
				}
				return ks.Put(fmt.Appendf(nil, "k%d", i), []byte("ok"))
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if i == 3 {
			if !errors.Is(err, ErrChildActive) {
				t.Errorf("Batch[3] (left child open) err = %v, want ErrChildActive", err)
			}
		} else if err != nil {
			t.Errorf("Batch[%d] err = %v, want nil (sibling must not be frozen)", i, err)
		}
	}

	// Sibling writes landed; the misuser's did not.
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.OpenKeyspace("ks")
	if v, err := ks.Get([]byte("k0")); err != nil || string(v) != "ok" {
		t.Errorf("sibling k0 = %q, %v; want ok, nil", v, err)
	}
	if _, err := ks.Get([]byte("k3")); !errors.Is(err, ErrNotFound) {
		t.Errorf("misuser k3 present: %v", err)
	}
}

// TestBatchSizeCapFires: with MaxBatchSize set low, the coordinator fires
// a batch as soon as the cap is reached (verified by all calls succeeding
// + landing; the cap path is exercised by sending more than the cap).
func TestBatchSizeCapFires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		db, err := Open(ctx, tmpPath(t), Options{
			PageSize: 4096, MinSize: 16, MaxSize: 4096,
			MaxBatchSize: 2, MaxBatchDelay: time.Hour, // delay never fires; cap must
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer db.Close()
		tx, _ := db.Begin(ctx)
		tx.CreateKeyspace("ks")
		tx.Commit()

		const n = 6
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if err := db.Batch(ctx, func(tx *Tx) error {
					ks, _ := tx.OpenKeyspace("ks")
					return ks.Put(fmt.Appendf(nil, "k%d", i), []byte("v"))
				}); err != nil {
					t.Errorf("Batch[%d]: %v", i, err)
				}
			}(i)
		}
		// With a 1-hour delay, the only way these complete is the size cap
		// firing batches of 2. If the cap were broken they'd block forever
		// and synctest would deadlock the bubble.
		wg.Wait()

		rtx, _ := db.Begin(ctx)
		defer rtx.Rollback()
		ks, _ := rtx.OpenKeyspace("ks")
		for i := range n {
			if _, err := ks.Get(fmt.Appendf(nil, "k%d", i)); err != nil {
				t.Errorf("Get k%d: %v", i, err)
			}
		}
	})
}

// TestBatchOptionsValidation: negative batch options are rejected at Open.
func TestBatchOptionsValidation(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"negative size", Options{PageSize: 4096, MinSize: 16, MaxSize: 256, MaxBatchSize: -1}},
		{"negative delay", Options{PageSize: 4096, MinSize: 16, MaxSize: 256, MaxBatchDelay: -time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Open(ctx, tmpPath(t), tc.opts)
			if err == nil {
				db.Close()
				t.Fatalf("Open accepted invalid options")
			}
			if !errors.Is(err, ErrInvalidOptions) {
				t.Errorf("Open err = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

// TestBatchCallerContextCancelled: a caller whose context is already
// cancelled when its closure would run is skipped with context.Cause and
// its write does not land.
func TestBatchCallerContextCancelled(t *testing.T) {
	db := batchTestDB(t)

	// Cancelled context: the call may be rejected before queueing or
	// skipped at execution; either way the result is the cause and no
	// write lands.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	err := db.Batch(cancelledCtx, func(tx *Tx) error {
		ks, _ := tx.OpenKeyspace("ks")
		return ks.Put([]byte("cancelled"), []byte("x"))
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Batch err = %v, want context.Canceled", err)
	}

	tx, _ := db.Begin(context.Background())
	defer tx.Rollback()
	ks, _ := tx.OpenKeyspace("ks")
	if _, err := ks.Get([]byte("cancelled")); !errors.Is(err, ErrNotFound) {
		t.Errorf("cancelled write landed: %v", err)
	}
}

// TestBatchParentCommitFailure (spec §Write Batching clause 7): when the
// parent batch commit fails, every caller whose closure succeeded
// receives the commit error. Uses the pager's step-4 injection hook to
// fail the parent commit's publication phase.
func TestBatchParentCommitFailure(t *testing.T) {
	ctx := context.Background()
	db := batchTestDB(t)

	// Arm the shared writer pager's step-4 hook so the parent batch
	// commit's publication phase fails (simulated fdatasync EIO).
	db.pgr.SetCommitStep4HookForTest(func() error {
		return io.ErrUnexpectedEOF
	})

	err := db.Batch(ctx, func(tx *Tx) error {
		ks, e := tx.OpenKeyspace("ks")
		if e != nil {
			return e
		}
		return ks.Put([]byte("a"), []byte("1"))
	})
	// The successful closure's caller receives the parent commit error
	// (the injected step-4 failure), not nil.
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("Batch parent-commit-failure err = %v, want wrapped io.ErrUnexpectedEOF", err)
	}
	db.pgr.SetCommitStep4HookForTest(nil)

	// The publication-phase failure poisons the handle (same contract as
	// a direct Update commit failure); subsequent writes surface it.
	if err := db.Batch(ctx, func(tx *Tx) error { return nil }); !errors.Is(err, ErrPoisoned) {
		t.Errorf("post-failure Batch err = %v, want ErrPoisoned", err)
	}
}

// TestBatchAfterClose: Batch on a closed DB returns ErrClosed.
func TestBatchAfterClose(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Use the coordinator once so it is started, then close.
	tx, _ := db.Begin(ctx)
	tx.CreateKeyspace("ks")
	tx.Commit()
	if err := db.Batch(ctx, func(tx *Tx) error {
		ks, _ := tx.OpenKeyspace("ks")
		return ks.Put([]byte("a"), []byte("1"))
	}); err != nil {
		t.Fatalf("pre-close Batch: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.Batch(ctx, func(tx *Tx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Errorf("post-close Batch err = %v, want ErrClosed", err)
	}
}

// TestBatchCoalescesWithinDelay (synctest): multiple Batch calls arriving
// within MaxBatchDelay are coalesced into a single write transaction.
func TestBatchCoalescesWithinDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		db, err := Open(ctx, tmpPath(t), Options{
			PageSize: 4096, MinSize: 16, MaxSize: 4096,
			MaxBatchSize: 1000, MaxBatchDelay: 10 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer db.Close()
		tx, _ := db.Begin(ctx)
		tx.CreateKeyspace("ks")
		tx.Commit()

		startTxnID := db.Meta().TxnID

		const n = 5
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if err := db.Batch(ctx, func(tx *Tx) error {
					ks, _ := tx.OpenKeyspace("ks")
					return ks.Put(fmt.Appendf(nil, "k%d", i), []byte("v"))
				}); err != nil {
					t.Errorf("Batch[%d]: %v", i, err)
				}
			}(i)
		}
		wg.Wait()

		// All n calls arrived well within the 10ms window (synthetic time
		// did not advance while they queued), so the coordinator coalesced
		// them into ONE write transaction.
		if got := db.Meta().TxnID - startTxnID; got != 1 {
			t.Errorf("%d batched calls produced %d transactions, want 1", n, got)
		}

		// All writes durable.
		rtx, _ := db.Begin(ctx)
		defer rtx.Rollback()
		ks, _ := rtx.OpenKeyspace("ks")
		for i := range n {
			if _, err := ks.Get(fmt.Appendf(nil, "k%d", i)); err != nil {
				t.Errorf("Get k%d: %v", i, err)
			}
		}
	})
}
