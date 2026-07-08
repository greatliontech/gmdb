package gmdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// buildChurnedDB fills keyspace "k" with 3000 rows then DeleteRanges most
// of them, leaving many free pages below a high HighWaterMark — the
// fragmentation Compact should reclaim. Returns the surviving boundary key
// indices (low survivors + high survivors).
func buildChurnedDB(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 3000 {
		if err := ks.Put(fmt.Appendf(nil, "key%06d", i), fmt.Appendf(nil, "val%06d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	tx2, _ := db.Begin(ctx)
	ks2, _ := tx2.OpenKeyspace("k")
	if _, err := ks2.DeleteRange(fmt.Appendf(nil, "key%06d", 100), fmt.Appendf(nil, "key%06d", 2900)); err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit delete: %v", err)
	}
}

// TestCompactReclaimsAndPreservesData: Compact shrinks a fragmented DB
// (smaller HighWaterMark, zero free pages), preserves the UUID (unlike
// CopyTo), keeps all surviving data, leaves the handle usable for further
// writes, and Checks clean.
func TestCompactReclaimsAndPreservesData(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	buildChurnedDB(t, db)

	before := db.Meta()
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	after := db.Meta()

	if after.HighWaterMark >= before.HighWaterMark {
		t.Errorf("HighWaterMark not reduced: before=%d after=%d", before.HighWaterMark, after.HighWaterMark)
	}
	if after.NumFreePages != 0 {
		t.Errorf("compacted NumFreePages = %d, want 0", after.NumFreePages)
	}
	if after.UUID != before.UUID {
		t.Errorf("Compact changed UUID: before=%x after=%x", before.UUID, after.UUID)
	}
	// No temp file left behind.
	if _, err := os.Stat(path + ".compact"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file not cleaned up: %v", err)
	}

	// Surviving data intact; Check clean.
	for _, iss := range collectIssues(db.Check()) {
		if iss.Severity != CheckWarning {
			t.Errorf("post-Compact Check error: code=%s msg=%s", iss.Code, iss.Message)
		}
		if iss.Code == "BitmapLeak" {
			t.Errorf("post-Compact BitmapLeak at page %d", iss.PageID)
		}
	}
	rtx, _ := db.Begin(ctx)
	rks, _ := rtx.OpenKeyspace("k")
	for _, i := range []int{0, 50, 99, 2900, 2999} {
		if _, err := rks.Get(fmt.Appendf(nil, "key%06d", i)); err != nil {
			t.Errorf("survivor key%06d missing post-Compact: %v", i, err)
		}
	}
	if _, err := rks.Get(fmt.Appendf(nil, "key%06d", 1500)); err == nil {
		t.Errorf("deleted key%06d present post-Compact", 1500)
	}
	rtx.Rollback()

	// The handle is usable for further writes after the reopen.
	wtx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin after Compact: %v", err)
	}
	wks, _ := wtx.OpenKeyspace("k")
	if err := wks.Put([]byte("postcompact"), []byte("ok")); err != nil {
		t.Fatalf("Put after Compact: %v", err)
	}
	if err := wtx.Commit(); err != nil {
		t.Fatalf("Commit after Compact: %v", err)
	}
	vtx, _ := db.Begin(ctx)
	vks, _ := vtx.OpenKeyspace("k")
	if got, err := vks.Get([]byte("postcompact")); err != nil || string(got) != "ok" {
		t.Errorf("post-Compact write not durable: got %q err %v", got, err)
	}
	vtx.Rollback()
}

// TestCompactReclaimsAcrossReopen: the compacted state survives a full
// Close + re-Open (the rename + meta are durable).
func TestCompactReclaimsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	buildChurnedDB(t, db)
	uuid := db.Meta().UUID
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	hwm := db.Meta().HighWaterMark
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	if db2.Meta().UUID != uuid {
		t.Errorf("UUID changed across reopen: %x vs %x", db2.Meta().UUID, uuid)
	}
	if db2.Meta().HighWaterMark != hwm {
		t.Errorf("HighWaterMark drifted across reopen: %d vs %d", db2.Meta().HighWaterMark, hwm)
	}
	for _, iss := range collectIssues(db2.Check()) {
		if iss.Severity != CheckWarning {
			t.Errorf("reopened Check error: code=%s msg=%s", iss.Code, iss.Message)
		}
	}
	rtx, _ := db2.Begin(ctx)
	defer rtx.Rollback()
	rks, _ := rtx.OpenKeyspace("k")
	if _, err := rks.Get(fmt.Appendf(nil, "key%06d", 2999)); err != nil {
		t.Errorf("survivor missing after reopen: %v", err)
	}
}

// TestCompactReadersActive: with an active in-process read transaction and
// a short drain timeout, Compact aborts with ErrCompactReadersActive,
// leaves no temp file, and the DB remains usable; once the reader closes,
// Compact succeeds.
func TestCompactReadersActive(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096, CompactDrainTimeout: 60 * time.Millisecond})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	buildChurnedDB(t, db)

	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}

	start := time.Now()
	err = db.Compact()
	if !errors.Is(err, ErrCompactReadersActive) {
		t.Fatalf("Compact with active reader: got %v, want ErrCompactReadersActive", err)
	}
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Errorf("Compact returned before drain timeout (%v)", elapsed)
	}
	if _, serr := os.Stat(path + ".compact"); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("temp file left after aborted Compact: %v", serr)
	}
	// DB still usable + data intact after the aborted Compact (the reader
	// is still open here).
	r2, _ := db.Begin(ctx)
	rks, _ := r2.OpenKeyspace("k")
	if _, gerr := rks.Get(fmt.Appendf(nil, "key%06d", 0)); gerr != nil {
		t.Errorf("data unreadable after aborted Compact: %v", gerr)
	}
	r2.Rollback()

	// Release the reader; Compact now succeeds.
	rtx.Rollback()
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact after reader released: %v", err)
	}
	if db.Meta().NumFreePages != 0 {
		t.Errorf("NumFreePages = %d after successful Compact, want 0", db.Meta().NumFreePages)
	}
}

// TestCompactConcurrentWriterNoCrash: a
// write Begin that captures the pager then blocks in AcquireWriter behind
// Compact's grant must use the POST-Compact pager, never the closed old
// one. Without the post-grant pager re-read, the racing Put panics on the
// munmap'd old pager. Run under -race.
func TestCompactConcurrentWriterNoCrash(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 8192})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	// A sizable keyspace so each Compact copy takes long enough that the
	// racing Begin reliably blocks in AcquireWriter mid-Compact.
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 8000 {
		if err := ks.Put(fmt.Appendf(nil, "key%06d", i), fmt.Appendf(nil, "val%06d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	for iter := range 8 {
		done := make(chan error, 1)
		go func() { done <- db.Compact() }()
		// Bias toward Compact holding the grant first so Begin blocks
		// behind it (the H1 window).
		time.Sleep(300 * time.Microsecond)

		wtx, err := db.Begin(ctx)
		if err != nil {
			<-done
			t.Fatalf("iter %d Begin: %v", iter, err)
		}
		wks, err := wtx.OpenKeyspace("k")
		if err != nil {
			wtx.Rollback()
			<-done
			t.Fatalf("iter %d OpenKeyspace: %v", iter, err)
		}
		key := fmt.Appendf(nil, "w%04d", iter)
		if err := wks.Put(key, []byte("ok")); err != nil {
			wtx.Rollback()
			<-done
			t.Fatalf("iter %d Put: %v", iter, err)
		}
		if err := wtx.Commit(); err != nil {
			<-done
			t.Fatalf("iter %d Commit: %v", iter, err)
		}
		if cerr := <-done; cerr != nil {
			t.Fatalf("iter %d Compact: %v", iter, cerr)
		}

		// The concurrent write landed (against whichever pager).
		vtx, _ := db.Begin(ctx)
		vks, _ := vtx.OpenKeyspace("k")
		if got, gerr := vks.Get(key); gerr != nil || string(got) != "ok" {
			vtx.Rollback()
			t.Fatalf("iter %d write lost: got %q err %v", iter, got, gerr)
		}
		vtx.Rollback()
	}

	for _, iss := range collectIssues(db.Check()) {
		if iss.Severity != CheckWarning {
			t.Errorf("post-race Check error: code=%s msg=%s", iss.Code, iss.Message)
		}
	}
}

// TestCompactPreservesFileMode: the
// compacted file must keep the source DB's 0600 mode, not widen to 0644.
func TestCompactPreservesFileMode(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	buildChurnedDB(t, db)

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("Compact changed file mode: before=%o after=%o", before.Mode().Perm(), after.Mode().Perm())
	}
	if after.Mode().Perm() != 0o600 {
		t.Errorf("compacted file mode = %o, want 0600", after.Mode().Perm())
	}
}

// TestPoisonedHandleRejectsReads: a
// poisoned handle (e.g. after a failed Compact reopen, which maps a stale
// unlinked inode) must reject BeginRead — not just writes — so reads
// cannot silently observe pre-Compact data. (Poison is set directly here;
// triggering a real reopen-failure needs fault injection.)
func TestPoisonedHandleRejectsReads(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	_ = ks.Put([]byte("a"), []byte("b"))
	tx.Commit()

	db.poisoned.Store(true)
	if _, err := db.BeginRead(ctx); !errors.Is(err, ErrPoisoned) {
		t.Errorf("BeginRead on poisoned handle: got %v, want ErrPoisoned", err)
	}
	if _, err := db.Begin(ctx); !errors.Is(err, ErrPoisoned) {
		t.Errorf("Begin on poisoned handle: got %v, want ErrPoisoned", err)
	}
	// Close still works on a poisoned handle (the recovery path).
	db.poisoned.Store(false) // let deferred Close run the normal path
}

// TestCompactEmptyDB: compacting a DB with no keyspaces is a no-op-shaped
// success — the handle stays usable.
func TestCompactEmptyDB(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	uuid := db.Meta().UUID
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact empty: %v", err)
	}
	if db.Meta().UUID != uuid {
		t.Errorf("Compact changed UUID on empty DB")
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin after empty Compact: %v", err)
	}
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		t.Fatalf("CreateKeyspace after Compact: %v", err)
	}
	_ = ks.Put([]byte("a"), []byte("b"))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after empty Compact: %v", err)
	}
}
