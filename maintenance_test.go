package gmdb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/thegrumpylion/gmdb/internal/lock"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// captureHandler is a minimal slog.Handler that records every Record so a
// test can assert what the maintenance goroutine logged. Safe for the
// concurrent logging the maintenance goroutine could do (tests drive it
// synchronously, but the mutex keeps -race clean regardless).
type captureHandler struct {
	mu   *sync.Mutex
	recs *[]slog.Record
}

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.recs = append(*h.recs, r.Clone())
	return nil
}
func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

// newCaptureLogger returns a *slog.Logger backed by a captureHandler plus
// the slice it appends into and the mutex guarding it.
func newCaptureLogger() (*slog.Logger, *[]slog.Record, *sync.Mutex) {
	var mu sync.Mutex
	recs := new([]slog.Record)
	return slog.New(captureHandler{mu: &mu, recs: recs}), recs, &mu
}

const scrubBadChecksumMsg = "gmdb: scrub detected bad page checksum"

// scrubWarnedPages returns the page ids reported by scrub-detected
// bad-checksum warnings among the captured records.
func scrubWarnedPages(t *testing.T, recs *[]slog.Record, mu *sync.Mutex) []uint64 {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	var pages []uint64
	for i := range *recs {
		r := (*recs)[i]
		if r.Message != scrubBadChecksumMsg {
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "page" {
				pages = append(pages, a.Value.Uint64())
			}
			return true
		})
	}
	return pages
}

// makeLeak commits a keyspace "k" with n rows plus one leaked page
// (allocated + committed, linked into no tree). Returns the leaked id.
func makeLeak(t *testing.T, db *DB, n int) uint64 {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
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
	rtx, _ := db.Begin(ctx)
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
	tx, _ := db.Begin(ctx)
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
	tx, _ := db.Begin(ctx)
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

// TestMaintenanceValidatesOptions: a negative
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
	tx, _ := db.Begin(ctx)
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

// writeKeyspaceForScrub opens a checksummed db at path, creates keyspace
// "k" with n rows, commits, and returns the active meta's KeyspaceRoot (an
// allocated data page ≥ firstData) and the page size. The db is closed on
// return so the caller can corrupt the file offline.
func writeKeyspaceForScrub(t *testing.T, path string, n int) (root uint64, pageSize uint32) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range n {
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "v%05d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	meta := rtx.Meta()
	root, pageSize = meta.KeyspaceRoot, meta.PageSize
	firstData := uint64(2) + uint64(meta.BitmapPages)
	_ = rtx.Rollback()
	if root < firstData {
		t.Fatalf("KeyspaceRoot %d < firstData %d; not a corruptible data page", root, firstData)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return root, pageSize
}

// corruptPageByte flips one byte inside page pageID's checksummed region
// (offset 16, well clear of the footer) directly on the file, breaking its
// xxhash64 footer. The db must be closed (no live mmap).
func corruptPageByte(t *testing.T, path string, pageID uint64, pageSize uint32) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corrupt: %v", err)
	}
	defer f.Close()
	off := int64(pageID)*int64(pageSize) + 16
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, off); err != nil {
		t.Fatalf("readat: %v", err)
	}
	b[0] ^= 0xFF
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatalf("writeat: %v", err)
	}
}

// TestMaintenanceScrubDetectsAndReportsBadChecksum (Task 3 / Inv-M5 +
// Inv-M3): scrub footer-verifies an allocated data page, reports a bitflip
// as a CheckWarning carrying the page id, and does NOT repair it.
func TestMaintenanceScrubDetectsAndReportsBadChecksum(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	root, pageSize := writeKeyspaceForScrub(t, path, 200)
	corruptPageByte(t, path, root, pageSize)

	logger, recs, mu := newCaptureLogger()
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096,
		Maintenance: MaintenanceOptions{Disable: true}, Logger: logger})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db.Close()

	db.maintScrubChecksums(ctx)

	if pages := scrubWarnedPages(t, recs, mu); !slices.Contains(pages, root) {
		t.Errorf("scrub did not report bad checksum for page %d; warned pages = %v", root, pages)
	}
	// Inv-M3 (report-only): the page is NOT repaired — it still fails.
	rtx, _ := db.BeginRead(ctx)
	defer rtx.Rollback()
	if page.VerifyPageFooter(rtx.pgr.PageRaw(root), pageSize) {
		t.Errorf("scrub repaired page %d (footer now valid); must be report-only", root)
	}
}

// TestMaintenanceScrubWiredIntoPass: a full runMaintenancePass runs Task 3
// (the scrub warning for a corrupted page appears among the pass's logs).
func TestMaintenanceScrubWiredIntoPass(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	root, pageSize := writeKeyspaceForScrub(t, path, 200)
	corruptPageByte(t, path, root, pageSize)

	logger, recs, mu := newCaptureLogger()
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096,
		Maintenance: MaintenanceOptions{Disable: true}, Logger: logger})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db.Close()

	db.runMaintenancePass(ctx)

	if pages := scrubWarnedPages(t, recs, mu); !slices.Contains(pages, root) {
		t.Errorf("full maintenance pass did not scrub-warn page %d; warned = %v", root, pages)
	}
}

// TestMaintenanceScrubCleanDBNoWarnings (footer-gate invariant): on a
// healthy database scrub warns about nothing. This enforces the gate —
// without it, the meta/bitmap region (< firstData, no footer) and any free
// pages would each emit a spurious BadPageChecksum.
func TestMaintenanceScrubCleanDBNoWarnings(t *testing.T) {
	ctx := context.Background()
	logger, recs, mu := newCaptureLogger()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096,
		Maintenance: MaintenanceOptions{Disable: true}, Logger: logger})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 300 {
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "v%05d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	db.maintScrubChecksums(ctx)

	if pages := scrubWarnedPages(t, recs, mu); len(pages) != 0 {
		t.Errorf("clean db: scrub warned on pages %v (gate must exclude meta/bitmap + free pages; all data footers valid)", pages)
	}
}

// TestMaintenanceScrubCursorAdvancesAndWraps: the persistent ScrubCursor
// advances by the batch size each pass and wraps at HighWaterMark, so a
// full cycle covers the data region (no stuck cursor, no skipped region).
// A small ScrubBatchSize forces many passes over the region.
func TestMaintenanceScrubCursorAdvancesAndWraps(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096,
		Maintenance: MaintenanceOptions{Disable: true, ScrubBatchSize: 2}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 400 {
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "v%05d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	rtx, _ := db.BeginRead(ctx)
	meta := rtx.Meta()
	firstData := uint64(2) + uint64(meta.BitmapPages)
	hwm := meta.HighWaterMark
	if bound := min(uint64(rtx.pgr.FileSize())/uint64(meta.PageSize), meta.MaxSize); hwm > bound {
		hwm = bound
	}
	_ = rtx.Rollback()
	span := hwm - firstData
	if span < 5 {
		t.Skipf("data region too small (span=%d) to exercise multi-pass cursor", span)
	}

	if db.maint.scrubCursor != 0 {
		t.Fatalf("precondition: scrubCursor=%d, want 0", db.maint.scrubCursor)
	}
	db.maintScrubChecksums(ctx)
	if db.maint.scrubCursor != firstData+2 { // start 0 → clamp firstData → +batch
		t.Errorf("after pass 1: scrubCursor=%d, want %d (firstData=%d, hwm=%d)", db.maint.scrubCursor, firstData+2, firstData, hwm)
	}

	// Run a full region's worth of passes; the cursor must stay in range
	// every pass and wrap at least once (decrease) — proving coverage.
	prev := db.maint.scrubCursor
	wrapped := false
	for pass := range int(span) {
		db.maintScrubChecksums(ctx)
		cur := db.maint.scrubCursor
		if cur < firstData || cur >= hwm {
			t.Fatalf("pass %d: cursor %d out of [%d,%d)", pass+2, cur, firstData, hwm)
		}
		if cur < prev {
			wrapped = true
		}
		prev = cur
	}
	if !wrapped {
		t.Errorf("cursor never wrapped over %d passes (span=%d, hwm=%d)", span, span, hwm)
	}
}

// TestMaintenanceScrubSkippedWithoutChecksum: with PageChecksum disabled
// scrub is a no-op (no footers exist, so verifying would flood spurious
// warnings — the early skip prevents that).
func TestMaintenanceScrubSkippedWithoutChecksum(t *testing.T) {
	ctx := context.Background()
	logger, recs, mu := newCaptureLogger()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096,
		DisablePageChecksum: true, Maintenance: MaintenanceOptions{Disable: true}, Logger: logger})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 100 {
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "v%05d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	db.maintScrubChecksums(ctx)

	if pages := scrubWarnedPages(t, recs, mu); len(pages) != 0 {
		t.Errorf("PageChecksum off: scrub should be a no-op, warned on %v", pages)
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
	if db.maint.started {
		t.Errorf("maintenance goroutine started despite Disable")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
