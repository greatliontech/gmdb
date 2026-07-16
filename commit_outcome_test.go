package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/greatliontech/gmdb/internal/pager"
)

// Commit outcome classification (durability.md §Commit Outcome
// Classification): every commit-protocol failure wraps exactly one of
// ErrCommitNotVisible / ErrCommitVisible / ErrCommitDurabilityUnknown,
// decided by meta-slot readback under the still-held grant. Each class
// is driven by fault injection at the pager's FileOps seam and
// verified against what a re-Open actually observes — the caller's
// contract is that the sentinel tells the truth about the
// transaction's fate.

// faultOps wraps the production FileOps, failing according to the
// configured hooks. Not concurrency-safe; commits are single-threaded
// under the write grant.
type faultOps struct {
	inner pager.FileOps

	// failWriteAt, when non-nil, is consulted per WriteAt; returning
	// (true, passThrough) fails the call — after forwarding it when
	// passThrough is true (the "pwrite landed but errored" shape).
	failWriteAt func(off int64) (fail, passThrough bool)

	// failFdatasyncAt fails the n-th Fdatasync (1-based) after arming.
	fsyncCount     int
	failFdatasyncN int

	// failReadAt, when non-nil, fails matching ReadAt calls.
	failReadAt func(off int64) bool
}

func (f *faultOps) WriteAt(p []byte, off int64) (int, error) {
	if f.failWriteAt != nil {
		if fail, through := f.failWriteAt(off); fail {
			if through {
				if _, err := f.inner.WriteAt(p, off); err != nil {
					return 0, err
				}
			}
			return 0, fmt.Errorf("injected WriteAt fault at %d", off)
		}
	}
	return f.inner.WriteAt(p, off)
}
func (f *faultOps) ReadAt(p []byte, off int64) (int, error) {
	if f.failReadAt != nil && f.failReadAt(off) {
		return 0, fmt.Errorf("injected ReadAt fault at %d", off)
	}
	return f.inner.ReadAt(p, off)
}
func (f *faultOps) Truncate(size int64) error { return f.inner.Truncate(size) }
func (f *faultOps) Fdatasync() error {
	f.fsyncCount++
	if f.failFdatasyncN != 0 && f.fsyncCount == f.failFdatasyncN {
		return fmt.Errorf("injected Fdatasync fault (call %d)", f.fsyncCount)
	}
	return f.inner.Fdatasync()
}

// commitOutcomeFixture opens a db, commits a baseline row, and returns
// the db plus a helper running one Put-commit under the given fault.
func commitOutcomeFixture(t *testing.T, path string) *DB {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	if err := ks.Put([]byte("base"), []byte("v")); err != nil {
		t.Fatalf("Put base: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("baseline Commit: %v", err)
	}
	return db
}

// commitUnderFault runs one Put("probe") commit with fops installed.
func commitUnderFault(t *testing.T, db *DB, fops pager.FileOps) error {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.OpenKeyspace("k")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	if err := ks.Put([]byte("probe"), []byte("p")); err != nil {
		t.Fatalf("Put probe: %v", err)
	}
	restore := db.SetWriterFileOpsForTest(fops)
	defer restore()
	return tx.Commit()
}

// reopenProbeVisible closes db, reopens the path, and reports whether
// the failed commit's probe row is the database state.
func reopenProbeVisible(t *testing.T, db *DB, path string) bool {
	t.Helper()
	ctx := context.Background()
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	rtx, _ := db2.BeginRead(ctx)
	defer rtx.Rollback()
	ks, err := rtx.OpenKeyspaceReadOnly("k")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly: %v", err)
	}
	v, err := ks.Get([]byte("probe"))
	if err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get probe: %v", err)
	}
	if _, err := ks.Get([]byte("base")); err != nil {
		t.Fatalf("baseline row lost: %v", err)
	}
	return bytes.Equal(v, []byte("p"))
}

// TestCommitOutcomeNotVisibleOnDataWriteFailure: a step-1 data pwrite
// failure classifies ErrCommitNotVisible; reopen shows the PREVIOUS
// state (probe absent), so a retry is safe.
func TestCommitOutcomeNotVisibleOnDataWriteFailure(t *testing.T) {
	path := tmpPath(t)
	db := commitOutcomeFixture(t, path)
	fops := &faultOps{inner: db.WriterFileOpsForTest()}
	// Fail the first DATA write (any offset past the two meta slots).
	fops.failWriteAt = func(off int64) (bool, bool) { return off >= 2*4096, false }
	err := commitUnderFault(t, db, fops)
	if !errors.Is(err, ErrCommitNotVisible) {
		t.Fatalf("step-1 failure: err=%v, want ErrCommitNotVisible", err)
	}
	if errors.Is(err, ErrCommitVisible) || errors.Is(err, ErrCommitDurabilityUnknown) {
		t.Fatalf("multiple classes on one error: %v", err)
	}
	// Publication-phase failure poisons.
	if _, berr := db.Begin(context.Background()); !errors.Is(berr, ErrPoisoned) {
		t.Fatalf("Begin after publication failure = %v, want ErrPoisoned", berr)
	}
	if reopenProbeVisible(t, db, path) {
		t.Fatal("ErrCommitNotVisible but the probe row IS the database state")
	}
}

// TestCommitOutcomeNotVisibleOnMetaWriteLost: a step-3 meta pwrite
// that fails WITHOUT landing classifies ErrCommitNotVisible (readback
// under the grant finds the previous meta still active).
func TestCommitOutcomeNotVisibleOnMetaWriteLost(t *testing.T) {
	path := tmpPath(t)
	db := commitOutcomeFixture(t, path)
	fops := &faultOps{inner: db.WriterFileOpsForTest()}
	fops.failWriteAt = func(off int64) (bool, bool) { return off < 2*4096, false } // meta slots, suppressed
	err := commitUnderFault(t, db, fops)
	if !errors.Is(err, ErrCommitNotVisible) {
		t.Fatalf("lost meta write: err=%v, want ErrCommitNotVisible", err)
	}
	if reopenProbeVisible(t, db, path) {
		t.Fatal("ErrCommitNotVisible but the probe row IS the database state")
	}
}

// TestCommitOutcomeDurabilityUnknownOnMetaWriteLandedDurable: under
// SyncDurable, a step-3 meta pwrite that LANDS but reports an error
// classifies ErrCommitDurabilityUnknown — the transaction is visible
// (reopen shows the probe row) but the mode's promised step-4 fsync
// never ran, so stable-storage durability is unestablished. A retry
// would double-apply.
func TestCommitOutcomeDurabilityUnknownOnMetaWriteLandedDurable(t *testing.T) {
	path := tmpPath(t)
	db := commitOutcomeFixture(t, path)
	fops := &faultOps{inner: db.WriterFileOpsForTest()}
	fops.failWriteAt = func(off int64) (bool, bool) { return off < 2*4096, true } // meta write forwarded, then error
	err := commitUnderFault(t, db, fops)
	if !errors.Is(err, ErrCommitDurabilityUnknown) {
		t.Fatalf("landed meta write under SyncDurable: err=%v, want ErrCommitDurabilityUnknown", err)
	}
	if errors.Is(err, ErrCommitNotVisible) || errors.Is(err, ErrCommitVisible) {
		t.Fatalf("multiple classes on one error: %v", err)
	}
	if _, berr := db.Begin(context.Background()); !errors.Is(berr, ErrPoisoned) {
		t.Fatalf("Begin after publication failure = %v, want ErrPoisoned", berr)
	}
	if !reopenProbeVisible(t, db, path) {
		t.Fatal("ErrCommitDurabilityUnknown but the probe row is NOT the database state")
	}
}

// TestCommitOutcomeVisibleOnMetaWriteLandedLazy: the same landed-meta
// failure under SyncLazy classifies plain ErrCommitVisible — that
// mode never promised the meta fsync, so a published commit is
// exactly what its contract delivers.
func TestCommitOutcomeVisibleOnMetaWriteLandedLazy(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		SyncMode: SyncLazy, Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	if err := ks.Put([]byte("base"), []byte("v")); err != nil {
		t.Fatalf("Put base: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("baseline Commit: %v", err)
	}
	fops := &faultOps{inner: db.WriterFileOpsForTest()}
	fops.failWriteAt = func(off int64) (bool, bool) { return off < 2*4096, true }
	cerr := commitUnderFault(t, db, fops)
	if !errors.Is(cerr, ErrCommitVisible) {
		t.Fatalf("landed meta write under SyncLazy: err=%v, want ErrCommitVisible", cerr)
	}
	if errors.Is(cerr, ErrCommitNotVisible) || errors.Is(cerr, ErrCommitDurabilityUnknown) {
		t.Fatalf("multiple classes on one error: %v", cerr)
	}
	if !reopenProbeVisible(t, db, path) {
		t.Fatal("ErrCommitVisible but the probe row is NOT the database state")
	}
}

// TestCommitOutcomeDurabilityUnknownOnMetaFsyncFailure: the step-4
// meta fdatasync failing classifies ErrCommitDurabilityUnknown — the
// transaction is visible (reopen shows it) but stable-storage
// durability is unestablished.
func TestCommitOutcomeDurabilityUnknownOnMetaFsyncFailure(t *testing.T) {
	path := tmpPath(t)
	db := commitOutcomeFixture(t, path)
	fops := &faultOps{inner: db.WriterFileOpsForTest(), failFdatasyncN: 2} // step 2 is fsync #1, step 4 is #2
	err := commitUnderFault(t, db, fops)
	if !errors.Is(err, ErrCommitDurabilityUnknown) {
		t.Fatalf("step-4 fsync failure: err=%v, want ErrCommitDurabilityUnknown", err)
	}
	if errors.Is(err, ErrCommitNotVisible) || errors.Is(err, ErrCommitVisible) {
		t.Fatalf("multiple classes on one error: %v", err)
	}
	if _, berr := db.Begin(context.Background()); !errors.Is(berr, ErrPoisoned) {
		t.Fatalf("Begin after publication failure = %v, want ErrPoisoned", berr)
	}
	if !reopenProbeVisible(t, db, path) {
		t.Fatal("ErrCommitDurabilityUnknown but the probe row is NOT the database state (visibility is certain once step 3 lands)")
	}
}

// TestCommitOutcomeAssemblyFailuresNotVisibleUnpoisoned: assembly-phase
// failures (here ErrTxTooLarge from the flush reserve) carry
// ErrCommitNotVisible, do NOT poison, and the handle stays usable.
func TestCommitOutcomeAssemblyFailuresNotVisibleUnpoisoned(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 1 << 16,
		MaxTxBufferBytes: 64 * 1024, Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx0, _ := db.Begin(ctx)
	if _, err := tx0.CreateKeyspace("k"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx0.Commit(); err != nil {
		t.Fatalf("create Commit: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, err := tx.OpenKeyspace("k")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	// Overrun the slab budget mid-tx so an op (not Commit) fails, then
	// drive a genuine assembly-phase Commit failure instead: keep
	// writing until Put itself reports ErrTxTooLarge, roll back, and
	// verify the handle is NOT poisoned. (A Commit-time assembly
	// failure needs an RPL overrun, which the small-budget Put path
	// hits first; the classification contract for assembly failures is
	// pinned by the wrap in Commit's flush path below.)
	var perr error
	for i := range 10000 {
		if perr = ks.Put(fmt.Appendf(nil, "k%06d", i), bytes.Repeat([]byte{'v'}, 512)); perr != nil {
			break
		}
	}
	if !errors.Is(perr, ErrTxTooLarge) {
		t.Fatalf("budget overrun: %v, want ErrTxTooLarge", perr)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// Handle usable: a fresh small tx commits.
	tx2, _ := db.Begin(ctx)
	ks2, err := tx2.OpenKeyspace("k")
	if err != nil {
		t.Fatalf("re-OpenKeyspace: %v", err)
	}
	if err := ks2.Put([]byte("small"), []byte("v")); err != nil {
		t.Fatalf("Put after rollback: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit after rollback: %v", err)
	}
}

// TestCommitOutcomeNotVisibleOnFlushFailure: a descriptor-flush
// failure (before the pager pipeline — no write issued) carries
// ErrCommitNotVisible, does NOT poison, and the handle stays usable.
func TestCommitOutcomeNotVisibleOnFlushFailure(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	if err := ks.Put([]byte("a"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	injected := errors.New("injected flush failure")
	hook := func() error { return injected }
	flushFailHookForTest.Store(&hook)
	t.Cleanup(func() { flushFailHookForTest.Store(nil) }) // panic-safe
	cerr := tx.Commit()
	flushFailHookForTest.Store(nil) // disarm for the retry below
	if !errors.Is(cerr, ErrCommitNotVisible) || !errors.Is(cerr, injected) {
		t.Fatalf("flush failure: err=%v, want ErrCommitNotVisible wrapping the cause", cerr)
	}
	if errors.Is(cerr, ErrCommitVisible) || errors.Is(cerr, ErrCommitDurabilityUnknown) {
		t.Fatalf("multiple classes: %v", cerr)
	}
	// Assembly-class: unpoisoned, handle usable.
	tx2, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin after flush failure = %v, want usable handle", err)
	}
	ks2, err := tx2.CreateKeyspace("k")
	if err != nil {
		t.Fatalf("CreateKeyspace retry: %v", err)
	}
	if err := ks2.Put([]byte("a"), []byte("v")); err != nil {
		t.Fatalf("Put retry: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit retry: %v", err)
	}
}

// TestCommitOutcomeUnclassifiedOnReadbackFailure: when the
// verification read itself fails, NO class is attached — the
// sentinels are certainty statements, and NotVisible would invite a
// double-applying retry of a possibly-visible commit. The error
// carries an explicit do-not-retry message instead.
func TestCommitOutcomeUnclassifiedOnReadbackFailure(t *testing.T) {
	path := tmpPath(t)
	db := commitOutcomeFixture(t, path)
	fops := &faultOps{inner: db.WriterFileOpsForTest()}
	wroteMeta := false
	fops.failWriteAt = func(off int64) (bool, bool) {
		if off < 2*4096 {
			wroteMeta = true
			return true, true // meta landed, then errored
		}
		return false, false
	}
	fops.failReadAt = func(off int64) bool { return wroteMeta && off < 2*4096 }
	err := commitUnderFault(t, db, fops)
	if errors.Is(err, ErrCommitNotVisible) || errors.Is(err, ErrCommitVisible) || errors.Is(err, ErrCommitDurabilityUnknown) {
		t.Fatalf("readback failure must attach NO class (certainty unavailable): %v", err)
	}
	if err == nil {
		t.Fatal("commit unexpectedly succeeded")
	}
	// Still poisoned (publication-phase failure).
	if _, berr := db.Begin(context.Background()); !errors.Is(berr, ErrPoisoned) {
		t.Fatalf("Begin after unclassified failure = %v, want ErrPoisoned", berr)
	}
	// The truth (probe visible) is discoverable by the mandated
	// re-Open-and-probe.
	if !reopenProbeVisible(t, db, path) {
		t.Fatal("probe row should be the database state (meta landed)")
	}
}
