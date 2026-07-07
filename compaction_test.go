package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// evacFloorAt returns a high-watermark evacuation floor at the midpoint of
// the current data region — the band [floor, HighWaterMark) is what a
// compaction pass evacuates.
func evacFloorAt(db *DB) uint64 {
	m := db.Meta()
	firstData := uint64(2) + uint64(m.BitmapPages)
	return firstData + (m.HighWaterMark-firstData)/2
}

// runCompactForest opens a write tx, runs the forest-relocation engine with a
// high-watermark predicate and the given budget, commits, and returns the
// number of pages relocated.
func runCompactForest(t *testing.T, db *DB, floor uint64, budget int) int {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin(compact): %v", err)
	}
	moved, err := tx.compactForest(func(id uint64) bool { return id >= floor }, budget)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("compactForest: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(compact): %v", err)
	}
	return moved
}

func assertCheckClean(t *testing.T, db *DB, when string) {
	t.Helper()
	for _, iss := range collectIssues(db.Check()) {
		if iss.Severity == CheckError || iss.Severity == CheckFatal {
			t.Errorf("%s: Check error code=%s ks=%s idx=%s page=%d msg=%s",
				when, iss.Code, iss.Keyspace, iss.Index, iss.PageID, iss.Message)
		}
		if iss.Code == "BitmapLeak" {
			t.Errorf("%s: BitmapLeak at page %d", when, iss.PageID)
		}
	}
}

// TestCompactForestPreservesForest builds a forest with several keyspaces —
// plain keys, large overflow values, and a secondary index — fragments it,
// then relocates every page above the evacuation floor in one pass. It pins
// the engine's whole-forest contract: every surviving key→value round-trips,
// index lookups are unchanged, the re-rooting cascade reaches meta
// (KeyspaceRoot changes), relocation actually happened, and the database
// Checks structurally clean (no dangling roots, double-allocs, or leaks).
func TestCompactForestPreservesForest(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 8192})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Deterministic data so every key can be checked after compaction.
	docKeys := func() [][]byte {
		var ks [][]byte
		for i := range 1500 {
			ks = append(ks, fmt.Appendf(nil, "doc%06d", i))
		}
		return ks
	}()
	docVal := func(i int) []byte {
		if i%50 == 0 { // every 50th value overflows (> one page)
			return bytes.Repeat([]byte{byte('A' + i%26)}, 6000)
		}
		return fmt.Appendf(nil, "value-for-doc-%06d", i)
	}

	// 1. Populate: a plain keyspace, an indexed keyspace, a tiny keyspace.
	tx, _ := db.Begin(ctx)
	docs, err := tx.CreateKeyspace("docs")
	if err != nil {
		t.Fatalf("CreateKeyspace docs: %v", err)
	}
	for i, k := range docKeys {
		if err := docs.Put(k, docVal(i)); err != nil {
			t.Fatalf("Put docs %q: %v", k, err)
		}
	}
	decl := testDecl("by_first", "first")
	decl.Extract = firstByteExtract
	idxKs, err := tx.CreateKeyspace("indexed", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace indexed: %v", err)
	}
	for i := range 400 {
		// value's first byte (the index key) cycles through a small alphabet.
		if err := idxKs.Put(fmt.Appendf(nil, "row%05d", i), []byte{byte('a' + i%8), 'x', 'y'}); err != nil {
			t.Fatalf("Put indexed %d: %v", i, err)
		}
	}
	tiny, err := tx.CreateKeyspace("tiny")
	if err != nil {
		t.Fatalf("CreateKeyspace tiny: %v", err)
	}
	if err := tiny.Put([]byte("only"), []byte("one")); err != nil {
		t.Fatalf("Put tiny: %v", err)
	}
	// A set keyspace: "hot" holds enough members to promote to a nested
	// B+tree (exercising nested-tree relocation through the forest); "cold"
	// stays a small inline subpage.
	subs, err := tx.CreateSetKeyspace("subs", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	const hotMembers = 1000 // ~9 KB of members ⇒ cannot be an inline subpage
	for i := range hotMembers {
		if _, err := subs.Put([]byte("hot"), fmt.Appendf(nil, "member%06d", i)); err != nil {
			t.Fatalf("Put subs hot %d: %v", i, err)
		}
	}
	for _, m := range []string{"x", "y", "z"} {
		if _, err := subs.Put([]byte("cold"), []byte(m)); err != nil {
			t.Fatalf("Put subs cold %q: %v", m, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit populate: %v", err)
	}

	// 2. Fragment: delete a middle band of docs, freeing low pages.
	tx2, _ := db.Begin(ctx)
	d2, _ := tx2.OpenKeyspace("docs")
	if _, err := d2.DeleteRange(fmt.Appendf(nil, "doc%06d", 300), fmt.Appendf(nil, "doc%06d", 1100)); err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit delete: %v", err)
	}
	survives := func(i int) bool { return i < 300 || i >= 1100 }

	// 3. Capture the index result set before compaction (per index key).
	lookupKeys := func(t *testing.T, db *DB, idxKey byte) []string {
		t.Helper()
		rtx, _ := db.Begin(ctx)
		defer rtx.Rollback()
		ks, _ := rtx.OpenKeyspace("indexed", decl)
		idx, err := ks.Index("by_first")
		if err != nil {
			t.Fatalf("Index: %v", err)
		}
		var got []string
		for pk := range idx.LookupKeys([]byte{idxKey}) {
			got = append(got, string(pk))
		}
		if err := idx.Err(); err != nil {
			t.Fatalf("idx.Err: %v", err)
		}
		slices.Sort(got)
		return got
	}
	beforeIdx := map[byte][]string{}
	for c := byte('a'); c < 'a'+8; c++ {
		beforeIdx[c] = lookupKeys(t, db, c)
	}

	assertCheckClean(t, db, "pre-compaction")
	beforeRoot := db.Meta().KeyspaceRoot

	// 4. Compact: relocate everything above the floor in one pass.
	floor := evacFloorAt(db)
	moved := runCompactForest(t, db, floor, 1<<20)
	if moved == 0 {
		t.Fatal("compactForest relocated nothing; expected pages above the floor")
	}

	// 5. Verify. Re-rooting cascade reached meta.
	if afterRoot := db.Meta().KeyspaceRoot; afterRoot == beforeRoot {
		t.Errorf("KeyspaceRoot unchanged (%d) after relocating %d pages", afterRoot, moved)
	}
	assertCheckClean(t, db, "post-compaction")

	// Every surviving doc round-trips; deleted docs stay gone.
	rtx, _ := db.Begin(ctx)
	rdocs, _ := rtx.OpenKeyspace("docs")
	for i, k := range docKeys {
		got, err := rdocs.Get(k)
		if survives(i) {
			if err != nil {
				t.Fatalf("survivor %q missing post-compaction: %v", k, err)
			}
			if !bytes.Equal(got, docVal(i)) {
				t.Errorf("doc %q value mismatch (len got=%d want=%d)", k, len(got), len(docVal(i)))
			}
		} else if err == nil {
			t.Errorf("deleted doc %q present post-compaction", k)
		}
	}
	rtiny, _ := rtx.OpenKeyspace("tiny")
	if got, err := rtiny.Get([]byte("only")); err != nil || string(got) != "one" {
		t.Errorf("tiny survivor: got=%q err=%v", got, err)
	}
	// Set keyspace: the nested-tree-backed "hot" set and inline "cold" set
	// both survive relocation with full membership.
	rsubs, _ := rtx.OpenSetKeyspace("subs")
	if n, err := rsubs.CountValues([]byte("hot")); err != nil || n != hotMembers {
		t.Errorf("subs hot CountValues: got=%d err=%v want=%d", n, err, hotMembers)
	}
	if n, err := rsubs.CountValues([]byte("cold")); err != nil || n != 3 {
		t.Errorf("subs cold CountValues: got=%d err=%v want=3", n, err)
	}
	for _, probe := range []string{"member000000", "member000500", "member000999"} {
		if ok, err := rsubs.HasValue([]byte("hot"), []byte(probe)); err != nil || !ok {
			t.Errorf("subs hot HasValue %q: ok=%v err=%v", probe, ok, err)
		}
	}
	_ = rtx.Rollback()

	// Index result sets unchanged after the index registry + data tree moved.
	for c := byte('a'); c < 'a'+8; c++ {
		after := lookupKeys(t, db, c)
		if !slices.Equal(beforeIdx[c], after) {
			t.Errorf("index key %q result set changed: before=%v after=%v", c, beforeIdx[c], after)
		}
	}
}

// TestCompactForestBudgetBound: a small budget caps the relocations while
// leaving the forest fully consistent and every value intact.
func TestCompactForestBudgetBound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 8192})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 1200 {
		if err := ks.Put(fmt.Appendf(nil, "key%06d", i), fmt.Appendf(nil, "v%06d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	floor := uint64(2) + uint64(db.Meta().BitmapPages) // floor = firstData ⇒ whole forest eligible
	const budget = 5
	moved := runCompactForest(t, db, floor, budget)
	if moved == 0 {
		t.Error("moved=0; expected partial relocation")
	}
	if moved > budget {
		t.Errorf("moved=%d exceeds budget=%d", moved, budget)
	}
	assertCheckClean(t, db, "post-bounded-compaction")

	rtx, _ := db.Begin(ctx)
	rks, _ := rtx.OpenKeyspace("k")
	for i := range 1200 {
		k := fmt.Appendf(nil, "key%06d", i)
		got, err := rks.Get(k)
		if err != nil || !bytes.Equal(got, fmt.Appendf(nil, "v%06d", i)) {
			t.Fatalf("key %q after bounded compaction: got=%q err=%v", k, got, err)
		}
	}
	_ = rtx.Rollback()
}

// TestCompactForestEmpty: an empty database (no keyspaces) is a no-op, and a
// zero/negative budget never relocates.
func TestCompactForestEmpty(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, _ := db.Begin(ctx)
	moved, err := tx.compactForest(func(uint64) bool { return true }, 1000)
	if err != nil || moved != 0 {
		t.Errorf("empty forest: moved=%d err=%v, want 0,nil", moved, err)
	}
	_ = tx.Rollback()

	// Non-empty forest, zero budget ⇒ no-op.
	tx2, _ := db.Begin(ctx)
	ks, _ := tx2.CreateKeyspace("k")
	_ = ks.Put([]byte("a"), []byte("b"))
	moved, err = tx2.compactForest(func(uint64) bool { return true }, 0)
	if err != nil || moved != 0 {
		t.Errorf("zero budget: moved=%d err=%v, want 0,nil", moved, err)
	}
	_ = tx2.Rollback()
}

// --- 12.5b-3b: orchestration unit tests ---------------------------------

func TestCompactionTriggered(t *testing.T) {
	cases := []struct {
		name      string
		attempts  uint64
		fragFails uint64
		threshold float64
		want      bool
	}{
		{"no signal", 0, 0, 0.5, false},
		{"no signal nonzero threshold", 0, 5, 0.0, false},
		{"above threshold", 100, 60, 0.5, true},
		{"exactly at threshold not triggered", 100, 50, 0.5, false},
		{"below threshold", 100, 40, 0.5, false},
		{"saturated", 10, 10, 0.99, true},
		{"tiny threshold tiny rate", 1000, 1, 0.0005, true},
		{"never (threshold 1)", 100, 100, 1.0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := compactionTriggered(c.attempts, c.fragFails, c.threshold); got != c.want {
				t.Errorf("compactionTriggered(%d,%d,%g)=%v, want %v", c.attempts, c.fragFails, c.threshold, got, c.want)
			}
		})
	}
}

func TestEvacuationFloor(t *testing.T) {
	const fd, hwm = uint64(10), uint64(1010) // dataSpan 1000
	cases := []struct {
		name      string
		firstData uint64
		hwm       uint64
		free      uint64
		budget    int
		wantFloor uint64
		wantOK    bool
	}{
		{"no data region", fd, fd, 0, 100, 0, false},
		{"hwm below firstData", fd, 5, 0, 100, 0, false},
		{"zero budget", fd, hwm, 0, 0, 0, false},
		{"entirely free", fd, hwm, 1000, 100, 0, false},
		{"dense region thin band", fd, hwm, 0, 100, 910, true},     // density 1.0, band 100
		{"sparse region wide band", fd, hwm, 900, 100, fd, true},   // density 0.1, band 1000 -> whole
		{"mid density", fd, hwm, 500, 100, 810, true},              // density 0.5, band 200
		{"budget covers whole region", fd, hwm, 0, 2000, fd, true}, // band 2000 -> whole
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			floor, ok := evacuationFloor(c.firstData, c.hwm, c.free, c.budget)
			if ok != c.wantOK || (ok && floor != c.wantFloor) {
				t.Errorf("evacuationFloor(%d,%d,%d,%d)=(%d,%v), want (%d,%v)",
					c.firstData, c.hwm, c.free, c.budget, floor, ok, c.wantFloor, c.wantOK)
			}
		})
	}
}

// --- 12.5b-3b: shrink (SA3) ---------------------------------------------

// TestCompactionShrinksFileMonotonically pins the spec's headline benefit:
// over successive passes the file shrinks (HighWaterMark strictly decreases
// net) and never grows (monotone non-increasing), with eager reclamation
// making consolidation immediate. Models steady state — a commit between the
// fragmenting deletes and the first pass advances the reclamation bound past
// the frees, so there is no bootstrap growth.
func TestCompactionShrinksFileMonotonically(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 1 << 20,
		ShrinkThreshold: 1, // shrink the file aggressively so HWM tracks live size
		Maintenance:     MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// 3000 rows, delete a wide middle band → high HWM held by RPL'd dead pages.
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 3000 {
		if err := ks.Put(fmt.Appendf(nil, "key%06d", i), fmt.Appendf(nil, "val%06d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	tx.Commit()
	txd, _ := db.Begin(ctx)
	ksd, _ := txd.OpenKeyspace("k")
	if _, err := ksd.DeleteRange(fmt.Appendf(nil, "key%06d", 100), fmt.Appendf(nil, "key%06d", 2900)); err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	txd.Commit()
	// Intervening commit: advances prevMeta.TxnID past the deletes so their
	// freed pages are reclaim-eligible at the first pass (no bootstrap growth).
	txn, _ := db.Begin(ctx)
	ksn, _ := txn.OpenKeyspace("k")
	_ = ksn.Put([]byte("nudge"), []byte("x"))
	txn.Commit()

	initialHWM := db.Meta().HighWaterMark
	prev := initialHWM
	for pass := range 15 {
		if _, err := db.compactionPass(ctx, 256); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		hwm := db.Meta().HighWaterMark
		if hwm > prev {
			t.Errorf("pass %d: HWM grew %d -> %d (compaction must not grow the file in steady state)", pass, prev, hwm)
		}
		prev = hwm
	}
	if prev >= initialHWM {
		t.Errorf("HWM did not shrink: initial=%d final=%d", initialHWM, prev)
	}

	// Survivors intact, deleted gone, Check clean.
	rtx, _ := db.Begin(ctx)
	rks, _ := rtx.OpenKeyspace("k")
	for _, i := range []int{0, 50, 99, 2900, 2999} {
		if _, err := rks.Get(fmt.Appendf(nil, "key%06d", i)); err != nil {
			t.Errorf("survivor key%06d missing: %v", i, err)
		}
	}
	if _, err := rks.Get(fmt.Appendf(nil, "key%06d", 1500)); err == nil {
		t.Errorf("deleted key%06d present", 1500)
	}
	rtx.Rollback()
	assertCheckClean(t, db, "post-shrink")
}

// --- 12.5b-3b: Inv-M4 (never surface ErrTxTooLarge) ---------------------

// buildOversizeForest builds a forest whose full relocation exceeds maxBuf
// (in slab pages), in per-tx batches small enough to commit under maxBuf.
func buildOversizeForest(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	big := bytes.Repeat([]byte{0x5A}, 6000) // overflow value (~2 follower pages)
	for batch := range 12 {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin batch %d: %v", batch, err)
		}
		ks, err := tx.OpenKeyspace("k")
		if err != nil {
			ks, err = tx.CreateKeyspace("k")
			if err != nil {
				t.Fatalf("Create/Open k: %v", err)
			}
		}
		for i := range 25 {
			if err := ks.Put(fmt.Appendf(nil, "ovf%03d-%03d", batch, i), big); err != nil {
				t.Fatalf("Put ovf %d-%d: %v", batch, i, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit batch %d: %v", batch, err)
		}
	}
}

// TestCompactionPassReturnsTxTooLargeAndRollsBack: a full-forest relocation
// that overruns MaxTxBufferBytes surfaces ErrTxTooLarge to compactionPass and
// rolls back cleanly (the foundation of runCompaction's halving — Inv-M4).
func TestCompactionPassReturnsTxTooLargeAndRollsBack(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 1 << 20,
		MaxTxBufferBytes: 1 << 20, // 256 pages — smaller than the forest
		Maintenance:      MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	buildOversizeForest(t, db)

	// Huge budget ⇒ floor=firstData ⇒ relocate the whole (oversize) forest.
	_, err = db.compactionPass(ctx, 1<<20)
	if !errors.Is(err, ErrTxTooLarge) {
		t.Fatalf("compactionPass err = %v, want ErrTxTooLarge", err)
	}
	assertCheckClean(t, db, "post-rolled-back-pass")
}

// TestRunCompactionNeverSurfacesTxTooLarge: runCompaction halves the budget
// past an over-large batch and lands a committed pass without surfacing the
// error; the database stays consistent and every value survives (Inv-M4).
func TestRunCompactionNeverSurfacesTxTooLarge(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 1 << 20,
		MaxTxBufferBytes: 1 << 20,
		Maintenance:      MaintenanceOptions{Disable: true, CompactionBatchSize: 1 << 20},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	buildOversizeForest(t, db)

	// runCompaction returns nothing and must not panic / surface ErrTxTooLarge.
	db.runCompaction(ctx)
	assertCheckClean(t, db, "post-runCompaction")

	// Every value still readable.
	rtx, _ := db.Begin(ctx)
	rks, _ := rtx.OpenKeyspace("k")
	big := bytes.Repeat([]byte{0x5A}, 6000)
	for batch := range 12 {
		for i := range 25 {
			got, err := rks.Get(fmt.Appendf(nil, "ovf%03d-%03d", batch, i))
			if err != nil || !bytes.Equal(got, big) {
				t.Fatalf("ovf%03d-%03d after runCompaction: err=%v len=%d", batch, i, err, len(got))
			}
		}
	}
	rtx.Rollback()
}

// --- 12.5b-3b: corrupt-follower checksum (deferred from 12.5b-2) ---------

// TestCompactionOverflowFollowerChecksum: relocating an overflow chain whose
// follower page is bitrotted aborts with ErrBadPageChecksum (the follower is
// footer-verified by pw.Page), rather than silently propagating corruption.
func TestCompactionOverflowFollowerChecksum(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 1 << 20,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// One overflow value (multi-page chain) plus filler so the tree has shape.
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("ovf")
	if err := ks.Put([]byte("big"), bytes.Repeat([]byte{0xCC}, 9000)); err != nil {
		t.Fatalf("Put big: %v", err)
	}
	for i := range 20 {
		_ = ks.Put(fmt.Appendf(nil, "s%03d", i), []byte("v"))
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Locate the overflow chain's first page, then a FOLLOWER (first+1).
	rtx, _ := db.Begin(ctx)
	rks, _ := rtx.OpenKeyspace("ovf")
	root := rks.desc.Root
	var first uint64
	if err := btree.WalkLeafEntries(rawPageReader{db.pgr}, db.pgr.Config(), root, db.pgr.HighWaterMark(), func(e page.LeafEntry) error {
		if e.IsOverflow() && first == 0 {
			first = e.OverflowPage
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkLeafEntries: %v", err)
	}
	rtx.Rollback()
	if first == 0 {
		t.Fatal("no overflow chain found")
	}
	follower := first + 1

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Corrupt the follower's content (leaving its stored footer) → footer
	// verification will mismatch on read.
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xDE, 0xAD, 0xBE, 0xEF}, int64(follower)*4096+100); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	f.Close()

	db2, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 1 << 20,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()

	// Relocate everything (floor=firstData): relocating the chain reads the
	// corrupt follower via pw.Page → ErrBadPageChecksum, rolled back.
	tx2, _ := db2.Begin(ctx)
	_, err = tx2.compactForest(func(uint64) bool { return true }, 1<<20)
	tx2.Rollback()
	if !errors.Is(err, ErrBadPageChecksum) {
		t.Fatalf("compactForest over a bitrotted follower err = %v, want ErrBadPageChecksum", err)
	}
}

// --- 12.5b-3b: disable gate ---------------------------------------------

// TestMaintCompactDisabledIsNoOp: DisableCompaction skips Task 4 entirely —
// maintCompact returns at the disable guard BEFORE consuming the fragmentation
// stats. The test isolates the guard (not merely "nothing happened"): writing
// an overflow value issues a multi-page AllocContiguous, leaving the contig
// counters non-zero; if the guard gates, maintCompact does not consume them,
// so a subsequent ConsumeContiguousAllocStats still observes attempts>0.
// (Without the guard, maintCompact would consume the stats → attempts==0.)
func TestMaintCompactDisabledIsNoOp(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 1 << 20,
		Maintenance: MaintenanceOptions{Disable: true, DisableCompaction: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 50 {
		_ = ks.Put(fmt.Appendf(nil, "k%05d", i), fmt.Appendf(nil, "v%05d", i))
	}
	// An overflow value (> one page) forces AllocContiguous(n>1), bumping the
	// contiguous-allocation attempt counter the trigger reads.
	if err := ks.Put([]byte("big"), bytes.Repeat([]byte{0x5A}, 9000)); err != nil {
		t.Fatalf("Put big: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	before := db.Meta().KeyspaceRoot

	db.maintCompact(ctx) // DisableCompaction ⇒ returns at the guard, before consuming stats

	if after := db.Meta().KeyspaceRoot; after != before {
		t.Errorf("DisableCompaction did not prevent compaction: KeyspaceRoot %d -> %d", before, after)
	}
	// The guard returned before the consume, so the stats are intact.
	if attempts, _ := db.pgr.ConsumeContiguousAllocStats(); attempts == 0 {
		t.Errorf("maintCompact consumed the fragmentation stats despite DisableCompaction (guard not gating)")
	}
}
