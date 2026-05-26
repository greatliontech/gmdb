package gmdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/thegrumpylion/gmdb/internal/lock"
)

// makeLeak commits a keyspace "k" with n rows plus one leaked page
// (allocated + committed, linked into no tree). Returns the leaked id.
func makeLeak(t *testing.T, db *DB, n int) uint64 {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, _ := tx.CreateKeyspace("k")
	for i := range n {
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "v%05d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	leaked, err := tx.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if _, err := tx.AllocSlab(leaked); err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return leaked
}

func hasBitmapLeak(t *testing.T, db *DB) bool {
	t.Helper()
	for _, iss := range collectIssues(db.Check()) {
		if iss.Code == "BitmapLeak" {
			return true
		}
	}
	return false
}

// TestMaintenanceReclaimsLeak (Task 1 / Inv-M2): a single maintenance pass
// reclaims a leaked page and leaves the live data intact. Maintenance is
// disabled so the only pass is the one driven directly here (deterministic).
func TestMaintenanceReclaimsLeak(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	const n = 50
	makeLeak(t, db, n)
	if !hasBitmapLeak(t, db) {
		t.Fatal("expected a BitmapLeak before maintenance")
	}

	db.runMaintenancePass(ctx)

	if hasBitmapLeak(t, db) {
		t.Errorf("BitmapLeak still present after a maintenance pass")
	}
	// Live data survived (no live page mis-reclaimed).
	rtx, _ := db.Begin(ctx, true)
	defer rtx.Rollback()
	rks, _ := rtx.OpenKeyspace("k")
	for i := range n {
		if _, err := rks.Get(fmt.Appendf(nil, "key%05d", i)); err != nil {
			t.Fatalf("key%05d lost after reclamation: %v", i, err)
		}
	}
}

// TestMaintenancePassSkipsWithinInterval (Inv-M1): a second pass within the
// interval is a no-op — the claim fails. After the first pass reclaims the
// leak, a fresh leak created immediately is NOT reclaimed by an in-interval
// second pass.
func TestMaintenancePassSkipsWithinInterval(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true, Interval: time.Hour}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	makeLeak(t, db, 20)
	db.runMaintenancePass(ctx) // claims the interval + reclaims
	if hasBitmapLeak(t, db) {
		t.Fatal("first pass should have reclaimed the leak")
	}

	// A new leak + an in-interval second pass: the claim fails, so nothing
	// is reclaimed.
	tx, _ := db.Begin(ctx, true)
	leaked, _ := tx.AllocPage()
	_, _ = tx.AllocSlab(leaked)
	tx.Commit()
	db.runMaintenancePass(ctx) // within the 1h interval ⇒ claim fails ⇒ skip
	if !hasBitmapLeak(t, db) {
		t.Errorf("in-interval second pass should have skipped (claim fails), leaving the new leak")
	}
}

// TestMaintenanceGoroutineReclaimsLeak: the background goroutine (tiny
// interval) reclaims a leak end-to-end without any direct driving.
func TestMaintenanceGoroutineReclaimsLeak(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Interval: 5 * time.Millisecond}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	makeLeak(t, db, 30)

	deadline := time.Now().Add(3 * time.Second)
	for hasBitmapLeak(t, db) {
		if time.Now().After(deadline) {
			t.Fatal("maintenance goroutine did not reclaim the leak within 3s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestMaintenanceCleanClose (Inv-M6): Close with the maintenance goroutine
// actively running (tiny interval) returns cleanly — no hang, no panic, no
// touch of a torn-down mmap. Run under -race.
func TestMaintenanceCleanClose(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Interval: time.Millisecond}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx, true)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 200 {
		_ = ks.Put(fmt.Appendf(nil, "k%04d", i), []byte("v"))
	}
	leaked, _ := tx.AllocPage()
	_, _ = tx.AllocSlab(leaked)
	tx.Commit()

	time.Sleep(5 * time.Millisecond) // let some passes run
	if err := db.Close(); err != nil {
		t.Fatalf("Close with maintenance running: %v", err)
	}
}

// TestMaintenanceValidatesOptions (regression for review M-2): a negative
// Interval is rejected at Open rather than panicking time.NewTicker inside
// the goroutine.
func TestMaintenanceValidatesOptions(t *testing.T) {
	ctx := context.Background()
	_, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Interval: -1}})
	if !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("Open with negative Interval: got %v, want ErrInvalidOptions", err)
	}
	_, err = Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{CompactionThreshold: 1.5}})
	if !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("Open with CompactionThreshold>1: got %v, want ErrInvalidOptions", err)
	}
}

// TestMaintenanceSkipsReclamationUnderCorruption (Inv-M2 completeness gate):
// a maintenance pass over a structurally-corrupt snapshot reclaims NOTHING
// — a walk-aborting corrupt subtree leaves its live pages unvisited (thus
// mis-classified as leaked), so freeing them would be data loss. The pass
// detects sawError and frees nothing (NumFreePages unchanged).
func TestMaintenanceSkipsReclamationUnderCorruption(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	makeLeak(t, db, 800) // multi-level tree + a leak
	tx, _ := db.Begin(ctx, true)
	ks, _ := tx.OpenKeyspace("k")
	root := ks.desc.Root
	tx.Rollback()
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Forge the data-tree root's first cell-directory offset (same forge as
	// TestCheckForgedBranchNoPanic) so the structural walk aborts.
	f, _ := os.OpenFile(path, os.O_RDWR, 0o600)
	if _, err := f.WriteAt([]byte{0xFF, 0xFF}, int64(root)*4096+16); err != nil {
		t.Fatalf("forge write: %v", err)
	}
	f.Close()

	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()

	before := numFreePages(t, db2)
	db2.runMaintenancePass(ctx) // must NOT reclaim under corruption
	if after := numFreePages(t, db2); after != before {
		t.Errorf("maintenance reclaimed under corruption: NumFreePages %d → %d (live pages at risk)", before, after)
	}
}

// TestMaintenanceReapsStaleReaderSlot (Task 2 — background-maintenance.md
// §Stale Reader Slot Cleanup): a full maintenance pass clears a reader
// slot owned by a dead (cross-namespace) process and leaves a live one.
// Driving runMaintenancePass exercises the Task-2 wiring end-to-end —
// acquire LOCK_EX via the coord, scan, release — under a real flock, not
// just the lock-package primitive.
func TestMaintenanceReapsStaleReaderSlot(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := db.coord.Clock()
	staleNanos := uint64(lock.DefaultStaleTimeout)
	if now <= 2*staleNanos {
		t.Skip("machine uptime < 2×StaleTimeout; cannot forge an aged heartbeat deterministically")
	}
	// Forge slot 0 stale: cross-namespace (NS=0 ⇒ heartbeat path),
	// heartbeat aged well past StaleTimeout. Slot 1 live: cross-NS, fresh
	// heartbeat. Raw stores — a deliberate manufactured pre-state. (The
	// detection read-tx in Task 1 acquires a *different* free slot since
	// these carry non-zero TxnIDs, so it does not disturb them.)
	s0 := db.lockFile.Slot(0)
	lock.Store64(&s0.TxnID, 7)
	lock.Store64(&s0.PID, 9999)
	lock.Store64(&s0.Heartbeat, now-2*staleNanos)
	s1 := db.lockFile.Slot(1)
	lock.Store64(&s1.TxnID, 11)
	lock.Store64(&s1.PID, 8888)
	lock.Store64(&s1.Heartbeat, now)

	db.runMaintenancePass(ctx)

	if got := lock.Load64(&s0.TxnID); got != 0 {
		t.Errorf("stale reader slot not reaped: TxnID = %d, want 0", got)
	}
	if got := lock.Load64(&s1.TxnID); got != 11 {
		t.Errorf("live reader slot reaped spuriously: TxnID = %d, want 11", got)
	}
}

// TestMaintenanceDisabled: with Disable set, no goroutine is started and
// Close is a clean no-op for maintenance.
func TestMaintenanceDisabled(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if db.maintStarted {
		t.Errorf("maintenance goroutine started despite Disable")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
