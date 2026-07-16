package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/greatliontech/gmdb/internal/btree"
	"github.com/greatliontech/gmdb/internal/lock"
	"github.com/greatliontech/gmdb/internal/page"
	"github.com/greatliontech/gmdb/internal/verify"
	"github.com/zeebo/xxh3"
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

const (
	scrubBadChecksumMsg  = "gmdb: scrub detected bad page checksum"
	scrubBadRunDigestMsg = "gmdb: scrub detected bad overflow-run digest"
)

// scrubWarnedPages returns the page ids reported by scrub-detected
// bad-checksum warnings among the captured records.
func scrubWarnedPages(t *testing.T, recs *[]slog.Record, mu *sync.Mutex) []uint64 {
	t.Helper()
	return scrubWarnedPagesMsg(t, recs, mu, scrubBadChecksumMsg)
}

// scrubWarnedPagesMsg returns the page ids carried by captured records
// with the given message.
func scrubWarnedPagesMsg(t *testing.T, recs *[]slog.Record, mu *sync.Mutex, msg string) []uint64 {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	var pages []uint64
	for i := range *recs {
		r := (*recs)[i]
		if r.Message != msg {
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
	// A cross-namespace (NS=0 ⇒ heartbeat-path) slot is governed by
	// the CROSS-NAMESPACE window — 6 × StaleTimeout at defaults
	// (cross-process.md §Stale-reader detection).
	staleNanos := uint64(6 * lock.DefaultStaleTimeout)
	if now <= 2*staleNanos {
		t.Skip("machine uptime < 2×CrossNamespaceStaleTimeout; cannot forge an aged heartbeat deterministically")
	}
	// Forge slot 0 stale: cross-namespace (NS=0 ⇒ heartbeat path),
	// heartbeat aged well past the cross-NS window. Slot 1 live: cross-NS, fresh
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
// XXH3-64 footer. The db must be closed (no live mmap).
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

// TestMaintenanceScrubCursorAdvancesAndWraps: the persistent scrub
// anchor advances each pass and wraps at HighWaterMark, so a full
// cycle covers the data region (no stuck anchor, no skipped region).
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

	if db.maint.scrubAnchor.valid {
		t.Fatalf("precondition: scrubAnchor=%+v, want invalid (fresh handle)", db.maint.scrubAnchor)
	}
	db.maintScrubChecksums(ctx)
	if a := db.maint.scrubAnchor; !a.valid || a.id < firstData || a.id >= hwm {
		t.Errorf("after pass 1: anchor=%+v, want valid in [%d,%d)", a, firstData, hwm)
	}

	// Run a full region's worth of passes; the anchor must stay in range
	// every pass and wrap at least once (its id decreases) — proving
	// coverage of the whole region across the cycle.
	prev := db.maint.scrubAnchor.id
	wrapped := false
	for pass := range int(span) {
		db.maintScrubChecksums(ctx)
		a := db.maint.scrubAnchor
		if !a.valid || a.id < firstData || a.id >= hwm {
			t.Fatalf("pass %d: anchor %+v out of [%d,%d)", pass+2, a, firstData, hwm)
		}
		if a.id < prev {
			wrapped = true
		}
		prev = a.id
	}
	if !wrapped {
		t.Errorf("anchor never wrapped over %d passes (span=%d, hwm=%d)", span, span, hwm)
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

// scrubOverflowFixture commits keyspace "k" holding node pages plus
// several multi-page overflow runs, returning one run's head id. The
// shape exercises the scrubber's run classification: footer-less
// followers interleaved with footer-bearing node pages in the scan
// window.
func scrubOverflowFixture(t *testing.T, ctx context.Context, db *DB) (root, head uint64) {
	t.Helper()
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 50 {
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "v%05d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	for i := range 4 {
		if err := ks.Put(fmt.Appendf(nil, "big%02d", i), bytes.Repeat([]byte{byte('A' + i)}, 9000)); err != nil {
			t.Fatalf("Put big: %v", err)
		}
	}
	root = ks.desc.Root
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := btree.WalkLeafEntries(verify.RawPageReader{P: db.pgr}, db.pgr.Config(), root, db.pgr.HighWaterMark(), func(e page.LeafEntry) error {
		if e.IsOverflow() && head == 0 {
			head = e.OverflowPage
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkLeafEntries: %v", err)
	}
	if head == 0 {
		t.Fatal("no overflow run found")
	}
	return root, head
}

// TestMaintenanceScrubSkipsOverflowRunFooters (the scrub run gate,
// background-maintenance.md §Checksum Scrubbing): overflow-run pages
// carry no footers — the scrubber must verify a run standalone by its
// head digest and advance past the followers. Without the gate every
// follower would emit a spurious BadPageChecksum warning on a healthy
// database.
func TestMaintenanceScrubSkipsOverflowRunFooters(t *testing.T) {
	ctx := context.Background()
	logger, recs, mu := newCaptureLogger()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096,
		Maintenance: MaintenanceOptions{Disable: true}, Logger: logger})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	_, _ = scrubOverflowFixture(t, ctx, db)

	// Full cycles: every allocated object gets verified at least once.
	for range 4 {
		db.maintScrubChecksums(ctx)
	}
	if pages := scrubWarnedPages(t, recs, mu); len(pages) != 0 {
		t.Errorf("healthy db with overflow runs: scrub warned on pages %v (followers must be covered by the run digest, not footer-verified)", pages)
	}
	if pages := scrubWarnedPagesMsg(t, recs, mu, scrubBadRunDigestMsg); len(pages) != 0 {
		t.Errorf("healthy db: scrub reported bad run digests on %v", pages)
	}
}

// TestMaintenanceScrubDetectsRunBitrot: a flipped byte in a follower is
// reported by the scrubber as a bad whole-run digest carrying the HEAD
// page id (checksums.md §Overflow-Run Digest — the run is verified
// standalone, no referencing cell needed).
func TestMaintenanceScrubDetectsRunBitrot(t *testing.T) {
	const pageSize = 4096
	ctx := context.Background()
	path := tmpPath(t)
	logger, recs, mu := newCaptureLogger()
	db, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 4096,
		Maintenance: MaintenanceOptions{Disable: true}, Logger: logger})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	_, head := scrubOverflowFixture(t, ctx, db)

	// Corrupt a follower byte while the DB is open (the scrubber's read
	// tx observes the external write through the shared page cache).
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xEE}, int64(head+1)*pageSize+123); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	f.Close()

	for range 4 {
		db.maintScrubChecksums(ctx)
	}
	if pages := scrubWarnedPagesMsg(t, recs, mu, scrubBadRunDigestMsg); !slices.Contains(pages, head) {
		t.Errorf("scrub did not report the corrupted run's head %d; run-digest warnings = %v", head, pages)
	}
}

// TestMaintenanceScrubAnchorInvalidationRestartsSilently
// (background-maintenance.md §Checksum Scrubbing, Cursor re-anchoring):
// an anchor whose digest no longer matches produces NO warning; the
// cursor resets to the first data page and the cycle restarts — pinned
// by comparing against the position an uninvalidated pass reaches.
func TestMaintenanceScrubAnchorInvalidationRestartsSilently(t *testing.T) {
	ctx := context.Background()
	logger, recs, mu := newCaptureLogger()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096,
		Maintenance: MaintenanceOptions{Disable: true, ScrubBatchSize: 4}, Logger: logger})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	root, head := scrubOverflowFixture(t, ctx, db)

	db.maintScrubChecksums(ctx)
	afterFirst := db.maint.scrubAnchor
	if !afterFirst.valid {
		t.Fatal("pass 1 left no anchor")
	}
	db.maintScrubChecksums(ctx)
	afterSecond := db.maint.scrubAnchor
	if afterSecond.id <= afterFirst.id {
		t.Fatalf("pass 2 did not advance the anchor (%d -> %d)", afterFirst.id, afterSecond.id)
	}

	// Invalidate, per anchor KIND — the digest comparison exists once
	// per branch of revalidateScrubAnchor (node page hash vs whole-run
	// digest), so each must be pinned independently. root is a node
	// page; head is a run head; both are allocated and in-region, so
	// only the wrong digest can be what fails revalidation.
	rtx, _ := db.BeginRead(ctx)
	nodeDigest := xxh3.Hash(rtx.pgr.PageRaw(root))
	runDigest := page.StoredOverflowRunDigest(rtx.pgr.PageRaw(head))
	_ = rtx.Rollback()
	for _, tc := range []struct {
		name   string
		anchor scrubAnchor
	}{
		{"node anchor", scrubAnchor{valid: true, id: root, digest: nodeDigest ^ 1}},
		{"run anchor", scrubAnchor{valid: true, id: head, isRun: true, digest: runDigest ^ 1}},
	} {
		db.maint.scrubAnchor = tc.anchor
		db.maintScrubChecksums(ctx)
		afterRestart := db.maint.scrubAnchor
		if !afterRestart.valid || afterRestart.id != afterFirst.id {
			t.Errorf("%s invalidated: pass ended at %+v, want a restart from firstData reproducing pass 1's anchor %+v",
				tc.name, afterRestart, afterFirst)
		}
	}
	// And the converse: a CORRECT anchor resumes rather than restarts —
	// re-installing pass 1's anchor must reproduce pass 2's.
	db.maint.scrubAnchor = afterFirst
	db.maintScrubChecksums(ctx)
	if got := db.maint.scrubAnchor; got != afterSecond {
		t.Errorf("valid anchor did not resume: got %+v, want pass 2's %+v", got, afterSecond)
	}
	if pages := scrubWarnedPages(t, recs, mu); len(pages) != 0 {
		t.Errorf("anchor invalidation must be silent; warned on %v", pages)
	}
	if pages := scrubWarnedPagesMsg(t, recs, mu, scrubBadRunDigestMsg); len(pages) != 0 {
		t.Errorf("anchor invalidation must be silent; run-digest warnings on %v", pages)
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

// TestMaintenanceScrubAdvancesPastPersistentCorruption
// (background-maintenance.md §Checksum Scrubbing, Cursor re-anchoring):
// a persistently corrupt object is anchored with its current content's
// digest, so the NEXT pass resumes past it — one warning per scrub
// cycle, never one per pass, and the region behind it keeps getting
// covered. Without this the anchor pins at the last verified object
// and every pass re-warns the same page forever.
func TestMaintenanceScrubAdvancesPastPersistentCorruption(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	// 5000 rows: enough allocated objects that a batch-1 pass cannot
	// wrap the whole cycle (the once-per-CYCLE re-warn is legitimate;
	// the defect under test is a once-per-PASS re-warn). Batch 1 makes
	// the corrupt page fill its whole window — the starvation shape:
	// with no successfully-verifying object in the window, an anchor
	// that only records successes never advances and every later pass
	// re-warns the same page while the region behind it starves.
	root, pageSize := writeKeyspaceForScrub(t, path, 5000)
	corruptPageByte(t, path, root, pageSize)

	logger, recs, mu := newCaptureLogger()
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096,
		Maintenance: MaintenanceOptions{Disable: true, ScrubBatchSize: 1}, Logger: logger})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db.Close()

	// Drive passes until the corrupt page is reported (bounded by one
	// full cycle at batch 1).
	warned := false
	for range 4096 {
		db.maintScrubChecksums(ctx)
		if slices.Contains(scrubWarnedPages(t, recs, mu), root) {
			warned = true
			break
		}
	}
	if !warned {
		t.Fatalf("scrub never reported the corrupt page %d", root)
	}
	before := len(scrubWarnedPages(t, recs, mu))

	// The very next pass resumes PAST the corrupt page — no re-warn
	// (a wrap back to it within one batch-1 pass is impossible for
	// this region size).
	db.maintScrubChecksums(ctx)
	if after := len(scrubWarnedPages(t, recs, mu)); after != before {
		t.Errorf("pass after the warning re-reported (warnings %d -> %d): the anchor pinned on the corrupt page instead of advancing past it", before, after)
	}
	if a := db.maint.scrubAnchor; !a.valid {
		t.Errorf("no anchor after passes over a corrupt region")
	}
}

// TestMaintenanceScrubRunCorruptionWarnsOncePerCycle: the run analog —
// a persistently corrupt overflow run anchors (with its current
// digest) and is resumed past, not re-warned by the next pass.
func TestMaintenanceScrubRunCorruptionWarnsOncePerCycle(t *testing.T) {
	const pageSize = 4096
	ctx := context.Background()
	path := tmpPath(t)
	logger, recs, mu := newCaptureLogger()
	db, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 4096,
		Maintenance: MaintenanceOptions{Disable: true, ScrubBatchSize: 2}, Logger: logger})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	_, head := scrubOverflowFixture(t, ctx, db)

	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xEE}, int64(head+1)*pageSize+77); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	f.Close()

	warned := false
	for range 4096 {
		db.maintScrubChecksums(ctx)
		if slices.Contains(scrubWarnedPagesMsg(t, recs, mu, scrubBadRunDigestMsg), head) {
			warned = true
			break
		}
	}
	if !warned {
		t.Fatalf("scrub never reported the corrupt run head %d", head)
	}
	before := len(scrubWarnedPagesMsg(t, recs, mu, scrubBadRunDigestMsg))
	db.maintScrubChecksums(ctx)
	if after := len(scrubWarnedPagesMsg(t, recs, mu, scrubBadRunDigestMsg)); after != before {
		t.Errorf("pass after the run warning re-reported (%d -> %d): the anchor did not advance past the corrupt run", before, after)
	}
}

// TestMaintenanceScrubFreePagesDoNotConsumeBudget
// (background-maintenance.md §Checksum Scrubbing, budget domain): the
// per-pass batch counts VERIFIED pages — free ids are advanced over
// without consuming it. With ScrubBatchSize=1, a pass whose resume
// position lands on a free window must still verify one object (the
// anchor advances every pass); if free ids consumed the budget, a
// free window would leave the anchor unmoved and pin the cycle.
func TestMaintenanceScrubFreePagesDoNotConsumeBudget(t *testing.T) {
	ctx := context.Background()
	logger, recs, mu := newCaptureLogger()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096,
		Maintenance: MaintenanceOptions{Disable: true, ScrubBatchSize: 1}, Logger: logger})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Rows + a contiguous window of leaked pages; reclaiming the leaks
	// leaves a multi-page FREE window inside [firstData, hwm).
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 40 {
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "v%05d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	for range 6 {
		id, err := tx.AllocPage()
		if err != nil {
			t.Fatalf("AllocPage: %v", err)
		}
		if _, err := tx.AllocSlab(id); err != nil {
			t.Fatalf("AllocSlab: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	db.maintReclaimLeaks(ctx)
	if hasBitmapLeak(t, db) {
		t.Fatal("fixture: leaks not reclaimed — no free window to exercise")
	}

	// Every pass must examine a NEW object — the anchor changes each
	// pass even when the resume position crosses the free window —
	// and the cycle wraps.
	prev := uint64(0)
	wrapped := false
	db.maintScrubChecksums(ctx)
	a := db.maint.scrubAnchor
	if !a.valid {
		t.Fatal("pass 1 left no anchor")
	}
	prev = a.id
	for pass := range 512 {
		db.maintScrubChecksums(ctx)
		a := db.maint.scrubAnchor
		if a.id == prev {
			t.Fatalf("pass %d did not advance the anchor (stuck at %d) — a free window is consuming the verification budget", pass+2, a.id)
		}
		if a.id < prev {
			wrapped = true
			break
		}
		prev = a.id
	}
	if !wrapped {
		t.Errorf("cycle never wrapped — coverage stalled")
	}
	if pages := scrubWarnedPages(t, recs, mu); len(pages) != 0 {
		t.Errorf("healthy db: scrub warned on %v", pages)
	}
}
