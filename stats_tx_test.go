package gmdb

import (
	"context"
	"testing"
	"time"
)

// TestTxStats exercises the per-transaction counters: the keyspace
// op counts (Gets/Puts/Deletes), structural events (Splits/Merges),
// allocator activity (CoW/Loose/Written/SlabPeak), index maintenance
// counts, Duration, and the rollback SlabPeak reset. Stats() is read
// from the transaction's own goroutine (the single-threaded contract).
func TestTxStats(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// --- Puts / Gets / Splits / SlabPeak / Duration; WrittenPages post-commit ---
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("ks")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	const n = 2000
	for i := range n {
		if err := ks.Put([]byte{byte(i >> 8), byte(i)}, []byte("v")); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	for i := range 10 {
		if _, err := ks.Get([]byte{byte(i >> 8), byte(i)}); err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
	}
	mid := tx.Stats()
	if mid.Puts != n {
		t.Errorf("Puts = %d, want %d", mid.Puts, n)
	}
	if mid.Gets != 10 {
		t.Errorf("Gets = %d, want 10", mid.Gets)
	}
	if mid.Splits == 0 {
		t.Error("Splits = 0, want > 0 (2000 entries force splits)")
	}
	if mid.SlabPeakBytes <= 0 {
		t.Error("SlabPeakBytes = 0 mid-tx, want > 0")
	}
	if mid.WrittenPages != 0 {
		t.Errorf("WrittenPages mid-tx = %d, want 0 (not yet committed)", mid.WrittenPages)
	}
	if mid.Duration <= 0 {
		t.Error("Duration = 0 mid-tx, want > 0")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	post := tx.Stats()
	if post.WrittenPages == 0 {
		t.Error("WrittenPages post-commit = 0, want > 0")
	}
	if post.Puts != n {
		t.Errorf("post-commit Puts = %d, want %d", post.Puts, n)
	}
	if post.Duration <= 0 {
		t.Error("post-commit Duration = 0, want > 0")
	}

	// --- CoW / Deletes / Merges: mutate the committed tree ---
	tx2, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx2: %v", err)
	}
	ks2, err := tx2.OpenKeyspace("ks")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	if err := ks2.Put([]byte{0, 0}, []byte("w")); err != nil { // overwrite → CoW path
		t.Fatalf("overwrite Put: %v", err)
	}
	const dels = n - 5
	for i := range dels {
		if err := ks2.Delete([]byte{byte(i >> 8), byte(i)}); err != nil {
			t.Fatalf("Delete %d: %v", i, err)
		}
	}
	s2 := tx2.Stats()
	if s2.CowPages == 0 {
		t.Error("CowPages = 0, want > 0 (overwrite CoWs the path)")
	}
	if s2.Deletes != dels {
		t.Errorf("Deletes = %d, want %d", s2.Deletes, dels)
	}
	if s2.Merges == 0 {
		t.Error("Merges = 0, want > 0 (bulk delete forces merges)")
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("Rollback tx2: %v", err)
	}

	// --- LoosePages: alloc + free within one tx (low-level helpers) ---
	tx3, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx3: %v", err)
	}
	id, err := tx3.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if _, err := tx3.AllocSlab(id); err != nil { // install a slab buffer
		t.Fatalf("AllocSlab: %v", err)
	}
	if err := tx3.FreePage(id); err != nil { // same-tx free of a slab'd page → loose
		t.Fatalf("FreePage: %v", err)
	}
	if got := tx3.Stats().LoosePages; got == 0 {
		t.Error("LoosePages = 0, want >= 1 (alloc+slab+free in one tx)")
	}
	_ = tx3.Rollback()

	// --- SlabPeak resets to 0 on rollback ---
	tx4, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx4: %v", err)
	}
	id4, err := tx4.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage tx4: %v", err)
	}
	if _, err := tx4.AllocSlab(id4); err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	if tx4.Stats().SlabPeakBytes <= 0 {
		t.Error("SlabPeakBytes pre-rollback = 0, want > 0")
	}
	if err := tx4.Rollback(); err != nil {
		t.Fatalf("Rollback tx4: %v", err)
	}
	if got := tx4.Stats().SlabPeakBytes; got != 0 {
		t.Errorf("SlabPeakBytes post-rollback = %d, want 0", got)
	}

	// --- Index maintenance: inserts, unique probes, deletes ---
	tx5, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx5: %v", err)
	}
	decl := testDecl("by_b", "b")
	decl.Extract = firstByteExtract
	decl.Unique = true
	ks5, err := tx5.CreateKeyspace("idxks", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace idx: %v", err)
	}
	const m = 200 // distinct first-byte values ⇒ no unique conflicts
	for i := range m {
		if err := ks5.Put([]byte{byte(i)}, []byte{byte(i)}); err != nil {
			t.Fatalf("indexed Put %d: %v", i, err)
		}
	}
	s5 := tx5.Stats()
	if s5.IndexEntriesInserted != m {
		t.Errorf("IndexEntriesInserted = %d, want %d", s5.IndexEntriesInserted, m)
	}
	if s5.IndexUniqueProbes == 0 {
		t.Error("IndexUniqueProbes = 0, want > 0 (unique index probes on insert)")
	}
	for i := range 50 {
		if err := ks5.Delete([]byte{byte(i)}); err != nil {
			t.Fatalf("indexed Delete %d: %v", i, err)
		}
	}
	if got := tx5.Stats().IndexEntriesDeleted; got != 50 {
		t.Errorf("IndexEntriesDeleted = %d, want 50", got)
	}
	_ = tx5.Rollback()
}

// TestTxStatsChild is the H1 regression: a child transaction's
// Stats().Duration must be its own sane window (BeginChild stamps
// startTime, commit/rollbackChild stamp endTime) — not time.Since(zero),
// which overflows. Its counters are the cumulative parent+child totals
// (shared pager).
func TestTxStatsChild(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("ks")
		if err != nil {
			return err
		}
		if err := ks.Put([]byte("a"), []byte("1")); err != nil {
			return err
		}
		child, err := tx.BeginChild()
		if err != nil {
			return err
		}
		cks, err := child.OpenKeyspace("ks")
		if err != nil {
			return err
		}
		if err := cks.Put([]byte("b"), []byte("2")); err != nil {
			return err
		}
		s := child.Stats()
		if s.Duration < 0 || s.Duration > time.Hour {
			t.Errorf("child Duration = %v, want a small sane value (not time.Since(zero))", s.Duration)
		}
		if s.Puts < 2 {
			t.Errorf("child Puts = %d, want >= 2 (cumulative parent+child)", s.Puts)
		}
		return child.Commit()
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}
