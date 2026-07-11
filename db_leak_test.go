package gmdb

import (
	"context"
	"runtime"
	"testing"
	"time"
	"weak"
)

//go:noinline
func leakDBHandle(t *testing.T, path string) (maintDone, batchDone chan struct{}) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		Maintenance: MaintenanceOptions{Interval: 50 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Start the batch coordinator too — both daemons must hold the
	// handle weakly for the leak cleanup to ever fire.
	if err := db.Batch(ctx, func(tx *Tx) error {
		_, e := tx.CreateKeyspace("ks")
		return e
	}); err != nil {
		t.Fatalf("Batch: %v", err)
	}
	maintDone, batchDone = db.DaemonDoneChansForTest()
	return maintDone, batchDone
	// Dropped without Close.
}

// A dropped *DB with LIVE daemons (maintenance + batch coordinator)
// must still become GC-unreachable so the handle-leak cleanup fires
// (leak-detection.md §Database Handle Leak Detection): the daemons
// hold the handle weakly, taking a strong reference only per pass.
// Pre-fix the daemons' method receivers pinned the handle reachable
// forever and the safety net was structurally inert.
func TestLeakedDBHandleCleanupFires(t *testing.T) {
	cleanupDone := make(chan struct{}, 1)
	restore := SetDBCleanupHookForTest(func() {
		select {
		case cleanupDone <- struct{}{}:
		default:
		}
	})
	defer restore()

	maintDone, batchDone := leakDBHandle(t, tmpPath(t))
	deadline := time.After(10 * time.Second)
loop:
	for {
		runtime.GC()
		runtime.GC()
		select {
		case <-cleanupDone:
			break loop // cleanup fired: daemons did not pin the handle
		case <-deadline:
			t.Fatal("DB leak cleanup never fired: a daemon goroutine pins the handle reachable")
		case <-time.After(20 * time.Millisecond):
		}
	}
	// The daemons must also EXIT once the handle is collected
	// (maintenance at its next tick; the batch coordinator via its
	// liveness ticker, MaxBatchDelay+1s).
	exitDeadline := time.After(5 * time.Second)
	select {
	case <-maintDone:
	case <-exitDeadline:
		t.Fatal("maintenance daemon did not exit after handle collection")
	}
	select {
	case <-batchDone:
	case <-exitDeadline:
		t.Fatal("batch coordinator did not exit after handle collection")
	}
}

// A Batch call IN FLIGHT must keep the handle reachable for its full
// duration — without the entry-point KeepAlive, the compiler ends the
// handle's liveness before the blocking result wait and a
// fire-and-forget caller's DB is torn down under its own accepted
// batch.
func TestBatchInFlightPinsHandle(t *testing.T) {
	cleanupDone := make(chan struct{}, 1)
	restore := SetDBCleanupHookForTest(func() {
		select {
		case cleanupDone <- struct{}{}:
		default:
		}
	})
	defer restore()

	release := make(chan struct{})
	batchRet := make(chan error, 1)
	wp := startBlockedBatch(t, tmpPath(t), release, batchRet)
	// While the closure is blocked, the handle must NOT be
	// collectable — asserted per-handle via the weak pointer (the
	// global cleanup hook could cross-fire from an unrelated leaked
	// handle; it serves only the positive phase below).
	for range 20 {
		runtime.GC()
		runtime.GC()
		if wp.Value() == nil {
			t.Fatal("handle collected while a Batch call was in flight")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	if err := <-batchRet; err != nil {
		t.Fatalf("Batch: %v", err)
	}
	// With the call finished and the handle dropped, the cleanup NOW
	// fires.
	deadline := time.After(10 * time.Second)
	for {
		runtime.GC()
		runtime.GC()
		select {
		case <-cleanupDone:
			return
		case <-deadline:
			t.Fatal("cleanup never fired after the in-flight call completed")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

//go:noinline
func startBlockedBatch(t *testing.T, path string, release chan struct{}, ret chan error) weak.Pointer[DB] {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 64,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	started := make(chan struct{})
	go func() {
		ret <- db.Batch(ctx, func(tx *Tx) error {
			close(started)
			<-release
			_, e := tx.CreateKeyspace("ks")
			return e
		})
	}()
	<-started
	// db goes out of scope here; only the in-flight Batch pins it.
	return weak.Make(db)
}

//go:noinline
func leakDBHandleAfterUpdate(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 64, Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.CreateKeyspace("ks")
		return e
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

// The long-lived writer pager stores per-tx callbacks; any that
// captured *db would pin an abandoned handle reachable through the
// cleanup's own info (runtime → dbCleanupInfo → pgr → callback → db)
// — the review-demonstrated third pinning path, distinct from the
// daemons.
func TestLeakedDBHandleCleanupFiresAfterWriteTx(t *testing.T) {
	cleanupDone := make(chan struct{}, 1)
	restore := SetDBCleanupHookForTest(func() {
		select {
		case cleanupDone <- struct{}{}:
		default:
		}
	})
	defer restore()
	leakDBHandleAfterUpdate(t, tmpPath(t))
	deadline := time.After(10 * time.Second)
	for {
		runtime.GC()
		runtime.GC()
		select {
		case <-cleanupDone:
			return
		case <-deadline:
			t.Fatal("DB leak cleanup never fired after a write tx: a pager-stored callback captures *db")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
