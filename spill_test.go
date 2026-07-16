package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
)

// MaxTxBufferBytes is a SPILL threshold, not a transaction-size cap
// (pager-slab.md §Slab Budget): a transaction's working set past the
// budget is written out to its allocated file pages at operation
// boundaries, and the commit publishes the union of spilled and
// slab-resident pages atomically.

// TestLargeTransactionSpillsAndCommits: one transaction writes many
// times the budget, spills (observable via TxStats.SpilledPages),
// commits, and every row survives a reopen with a clean Check.
func TestLargeTransactionSpillsAndCommits(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 1 << 16,
		MaxTxBufferBytes: 64 << 10, // 16 pages; the tx writes ~2 MB
		Maintenance:      MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		t.Fatal(err)
	}
	const rows = 2000
	val := make([]byte, 1000)
	for i := range rows {
		val[0] = byte(i)
		val[999] = byte(i >> 8)
		if err := ks.Put(fmt.Appendf(nil, "row%06d", i), val); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	if got := tx.Stats().SpilledPages; got == 0 {
		t.Fatal("no pages spilled writing ~2MB through a 64KB budget")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2, err := Open(ctx, path, Options{Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	rtx, err := db2.BeginRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rks, err := rtx.OpenKeyspaceReadOnly("k")
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		v, err := rks.Get(fmt.Appendf(nil, "row%06d", i))
		if err != nil {
			t.Fatalf("Get %d after reopen: %v", i, err)
		}
		if v[0] != byte(i) || v[999] != byte(i>>8) {
			t.Fatalf("row %d content wrong after reopen", i)
		}
	}
	rtx.Rollback()
	for iss := range db2.Check() {
		t.Errorf("Check: %+v", iss)
	}
}

// TestSpillCrashLeavesPreviousCommit: a crash at ANY point during a
// spilling (uncommitted) transaction leaves exactly the previous
// commit — spilled pwrites land at pages the on-disk bitmap still
// marks free and no recoverable meta references, the
// died-holding-grant image (pager-slab.md §Slab Budget).
func TestSpillCrashLeavesPreviousCommit(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 1 << 16,
		MaxTxBufferBytes: 64 << 10,
		SyncMode:         SyncDurable,
		Maintenance:      MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Baseline commit.
	tx0, _ := db.Begin(ctx)
	ks0, err := tx0.CreateKeyspace("k")
	if err != nil {
		t.Fatal(err)
	}
	if err := ks0.Put([]byte("base"), []byte("v0")); err != nil {
		t.Fatal(err)
	}
	if err := tx0.Commit(); err != nil {
		t.Fatal(err)
	}

	// Record every write from here on; the baseline image is the
	// pre-recorder on-disk state.
	rec := &crashRecorder{inner: db.WriterFileOpsForTest()}
	restore := db.SetWriterFileOpsForTest(rec)
	baseline, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// A spilling transaction, never committed.
	tx, _ := db.Begin(ctx)
	ks, err := tx.OpenKeyspace("k")
	if err != nil {
		t.Fatal(err)
	}
	val := make([]byte, 1000)
	for i := range 1500 {
		if err := ks.Put(fmt.Appendf(nil, "doomed%06d", i), val); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	if got := tx.Stats().SpilledPages; got == 0 {
		t.Fatal("fixture: nothing spilled")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	restore()

	// Synthesize crash images at several cut points through the
	// spill writes and reopen each: always the baseline state.
	rec.mu.Lock()
	nOps := len(rec.ops)
	rec.mu.Unlock()
	if nOps == 0 {
		t.Fatal("fixture: no writes recorded (spill did not pwrite)")
	}
	for _, cut := range []int{nOps / 4, nOps / 2, nOps} {
		img := append([]byte(nil), baseline...)
		rec.mu.Lock()
		for _, op := range rec.ops[:cut] {
			img = applyOp(img, op)
		}
		rec.mu.Unlock()
		crashPath := fmt.Sprintf("%s.crash%d", path, cut)
		if err := os.WriteFile(crashPath, img, 0o600); err != nil {
			t.Fatal(err)
		}
		cdb, err := Open(ctx, crashPath, Options{Maintenance: MaintenanceOptions{Disable: true}})
		if err != nil {
			t.Fatalf("reopen crash image (cut %d): %v", cut, err)
		}
		rtx, err := cdb.BeginRead(ctx)
		if err != nil {
			t.Fatalf("BeginRead (cut %d): %v", cut, err)
		}
		rks, err := rtx.OpenKeyspaceReadOnly("k")
		if err != nil {
			t.Fatalf("OpenKeyspaceReadOnly (cut %d): %v", cut, err)
		}
		if v, err := rks.Get([]byte("base")); err != nil || !bytes.Equal(v, []byte("v0")) {
			t.Fatalf("baseline row after crash (cut %d): %q err=%v", cut, v, err)
		}
		if _, err := rks.Get([]byte("doomed000000")); err == nil {
			t.Fatalf("uncommitted spilled row visible after crash (cut %d)", cut)
		}
		rtx.Rollback()
		for iss := range cdb.Check() {
			t.Errorf("Check after crash (cut %d): %+v", cut, iss)
		}
		cdb.Close()
	}
	db.Close()
}

// TestChildTransactionSpillRollback: spills at a child transaction's
// operation boundaries write in-window pages out early; the child's
// rollback must leave the parent's state exactly — spilled bytes are
// unreferenced garbage the restore's bitmap rewind orphans.
func TestChildTransactionSpillRollback(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 1 << 16,
		MaxTxBufferBytes: 64 << 10,
		Maintenance:      MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tx, _ := db.Begin(ctx)
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Put([]byte("parent"), []byte("kept")); err != nil {
		t.Fatal(err)
	}

	child, err := tx.BeginChild()
	if err != nil {
		t.Fatal(err)
	}
	cks, err := child.OpenKeyspace("k")
	if err != nil {
		t.Fatal(err)
	}
	val := make([]byte, 1000)
	for i := range 1000 {
		if err := cks.Put(fmt.Appendf(nil, "child%06d", i), val); err != nil {
			t.Fatalf("child Put %d: %v", i, err)
		}
	}
	if got := tx.Stats().SpilledPages; got == 0 {
		t.Fatal("fixture: child writes did not spill")
	}
	if err := child.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Parent state intact through the rollback of spilled child work.
	if v, err := ks.Get([]byte("parent")); err != nil || !bytes.Equal(v, []byte("kept")) {
		t.Fatalf("parent row after child rollback: %q err=%v", v, err)
	}
	if _, err := ks.Get([]byte("child000000")); err == nil {
		t.Fatal("child row visible after rollback")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for iss := range db.Check() {
		t.Errorf("Check: %+v", iss)
	}
}

// TestChildTransactionChurnBoundedMemory (regression): pages CoW'd
// and freed inside a nested window go loose with their buffers held —
// they cannot loose-pop (suspended) and cannot drop (a restore could
// resurrect the frees) — so they must SPILL (a pwrite of the held
// buffer preserves the content a restore would re-reference, since a
// loose page's bitmap bit stays clear until commit). Without that, a
// child transaction's churn accumulates unbounded slab memory: the
// exact OOM the spill-threshold invariant forbids (pager-slab.md).
func TestChildTransactionChurnBoundedMemory(t *testing.T) {
	ctx := context.Background()
	const budget = 64 << 10
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 1 << 16,
		MaxTxBufferBytes: budget,
		Maintenance:      MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Committed rows for the child to churn through.
	tx0, _ := db.Begin(ctx)
	ks0, err := tx0.CreateKeyspace("k")
	if err != nil {
		t.Fatal(err)
	}
	val := make([]byte, 900)
	for i := range 6000 {
		if err := ks0.Put(fmt.Appendf(nil, "row%06d", i), val); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx0.Commit(); err != nil {
		t.Fatal(err)
	}

	tx, _ := db.Begin(ctx)
	child, err := tx.BeginChild()
	if err != nil {
		t.Fatal(err)
	}
	cks, err := child.OpenKeyspace("k")
	if err != nil {
		t.Fatal(err)
	}
	peak := 0
	for i := range 6000 {
		if err := cks.Delete(fmt.Appendf(nil, "row%06d", i)); err != nil {
			t.Fatalf("child Delete %d: %v", i, err)
		}
		if d := tx.DirtyBytes(); d > peak {
			peak = d
		}
	}
	// The bound: the threshold plus a few pages of per-op overshoot.
	if peak > budget+16*4096 {
		t.Fatalf("child churn peaked at %d slab bytes (budget %d) — loose buffers not spilling inside the nested window", peak, budget)
	}
	if err := child.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for iss := range db.Check() {
		t.Errorf("Check: %+v", iss)
	}
}

// TestBorrowedSlicesSurviveSpill (mutation-hardening): buffers leave
// the slab via GC-drop, NEVER pool-recycle — a pool-recycled buffer
// is zero-filled and reused, corrupting every borrowed []byte that
// aliases it (the byte-slice ownership invariant, pager-slab.md).
func TestBorrowedSlicesSurviveSpill(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 1 << 16,
		MaxTxBufferBytes: 64 << 10,
		Maintenance:      MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tx, _ := db.Begin(ctx)
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		t.Fatal(err)
	}
	// A borrowed slice taken before heavy churn: whichever way its
	// backing page leaves the slab (spill, loose-drop, detach), the
	// bytes must survive. (The per-path deterministic pins live in
	// the pager package: TestSpillNeverPoolRecycles.)
	want := bytes.Repeat([]byte{0xAB}, 500)
	if err := ks.Put([]byte("aaa-anchor"), want); err != nil {
		t.Fatal(err)
	}
	borrowed, err := ks.Get([]byte("aaa-anchor"))
	if err != nil {
		t.Fatal(err)
	}
	val := make([]byte, 1000)
	for i := range 1500 {
		if err := ks.Put(fmt.Appendf(nil, "zzz%06d", i), val); err != nil {
			t.Fatal(err)
		}
	}
	if tx.Stats().SpilledPages == 0 {
		t.Fatal("fixture: nothing spilled")
	}
	if !bytes.Equal(borrowed, want) {
		t.Fatal("borrowed []byte corrupted after its page left the slab (pool-recycled instead of GC-dropped?)")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
