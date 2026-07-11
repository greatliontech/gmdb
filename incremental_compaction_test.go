package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
	"github.com/thegrumpylion/gmdb/internal/pager"
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
	for budget >= 1 {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin(compact): %v", err)
		}
		moved, err := tx.compactForest(floor, budget)
		if errors.Is(err, errCompactionSpaceExhausted) {
			// Mirror the production driver: the batch outran the
			// below-floor capacity — roll back and retry smaller.
			_ = tx.Rollback()
			budget /= 2
			continue
		}
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("compactForest: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(compact): %v", err)
		}
		return moved
	}
	return 0
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
	// Advance the reclamation bound past the deletes so the relocation
	// pass's eager reclaim can return their pages as below-floor
	// capacity (the consolidating allocator never extends the file).
	txnudge, _ := db.Begin(ctx)
	dn, _ := txnudge.OpenKeyspace("tiny")
	_ = dn.Put([]byte("nudge"), []byte("x"))
	if err := txnudge.Commit(); err != nil {
		t.Fatalf("Commit nudge: %v", err)
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
	// Free holes + bound advance: the consolidating allocator needs
	// existing below-bound capacity (it never extends the file). The
	// pad keyspace's create/delete churn supplies it via the CoW
	// retirees the delete + nudge commits release (DeleteKeyspace
	// itself defers the tree teardown to maintenance, disabled here)
	// — without touching the probed rows.
	txp, _ := db.Begin(ctx)
	pad, _ := txp.CreateKeyspace("pad")
	for i := range 400 {
		_ = pad.Put(fmt.Appendf(nil, "pad%06d", i), bytes.Repeat([]byte{'p'}, 200))
	}
	if err := txp.Commit(); err != nil {
		t.Fatalf("Commit pad: %v", err)
	}
	txd, _ := db.Begin(ctx)
	if err := txd.DeleteKeyspace("pad"); err != nil {
		t.Fatalf("DeleteKeyspace pad: %v", err)
	}
	if err := txd.Commit(); err != nil {
		t.Fatalf("Commit delete: %v", err)
	}
	txn, _ := db.Begin(ctx)
	ksn, _ := txn.OpenKeyspace("k")
	_ = ksn.Put([]byte("nudge"), []byte("x"))
	if err := txn.Commit(); err != nil {
		t.Fatalf("Commit nudge: %v", err)
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
	moved, err := tx.compactForest(0, 1000)
	if err != nil || moved != 0 {
		t.Errorf("empty forest: moved=%d err=%v, want 0,nil", moved, err)
	}
	_ = tx.Rollback()

	// Non-empty forest, zero budget ⇒ no-op.
	tx2, _ := db.Begin(ctx)
	ks, _ := tx2.CreateKeyspace("k")
	_ = ks.Put([]byte("a"), []byte("b"))
	moved, err = tx2.compactForest(0, 0)
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
		MaxTxBufferBytes: 480 << 10, // 120 pages of slab — fixture batches fit; a full-band relocation does not
		Maintenance:      MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	buildOversizeForest(t, db)
	// LOW capacity: deleting the first eight batches (one tx each, to
	// fit the reduced slab) releases their overflow chains and leaves
	// inline (row deletes free chains immediately; a DeleteKeyspace
	// defers the tree teardown to maintenance, which is disabled
	// here) — enough below-floor holes that the surviving batches'
	// relocation cascade overruns the slab before the capacity runs
	// out.
	for batch := range 8 {
		txd, _ := db.Begin(ctx)
		ksd, _ := txd.OpenKeyspace("k")
		for i := range 30 {
			_ = ksd.Delete(fmt.Appendf(nil, "ovf%03d-%03d", batch, i))
		}
		if err := txd.Commit(); err != nil {
			t.Fatalf("Commit delete %d: %v", batch, err)
		}
	}
	txn, _ := db.Begin(ctx)
	ksn, _ := txn.OpenKeyspace("k")
	_ = ksn.Put([]byte("nudge"), []byte("x"))
	if err := txn.Commit(); err != nil {
		t.Fatalf("Commit nudge: %v", err)
	}

	// Huge budget ⇒ the feasibility floor drops deep into the forest ⇒
	// the relocation cascade overruns MaxTxBufferBytes.
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
	// LOW capacity first: a twin chain + filler rows created BEFORE
	// the target chain occupy low ids; deleting them afterwards leaves
	// a contiguous below-bound run (overflow chains are contiguous by
	// construction) plus leaf holes BELOW the corrupt chain — trailing
	// frees would just tail-refund away, and the relocation pass draws
	// targets from existing capacity only.
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("ovf")
	if err := ks.Put([]byte("big2"), bytes.Repeat([]byte{0xDD}, 9000)); err != nil {
		t.Fatalf("Put big2: %v", err)
	}
	for i := range 200 {
		_ = ks.Put(fmt.Appendf(nil, "s%03d", i), bytes.Repeat([]byte{'v'}, 64))
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	tx1b, _ := db.Begin(ctx)
	ks1b, _ := tx1b.OpenKeyspace("ovf")
	if err := ks1b.Put([]byte("big"), bytes.Repeat([]byte{0xCC}, 9000)); err != nil {
		t.Fatalf("Put big: %v", err)
	}
	if err := tx1b.Commit(); err != nil {
		t.Fatalf("Commit big: %v", err)
	}
	txd, _ := db.Begin(ctx)
	ksd, _ := txd.OpenKeyspace("ovf")
	if err := ksd.Delete([]byte("big2")); err != nil {
		t.Fatalf("Delete big2: %v", err)
	}
	for i := range 150 {
		_ = ksd.Delete(fmt.Appendf(nil, "s%03d", i))
	}
	if err := txd.Commit(); err != nil {
		t.Fatalf("Commit delete: %v", err)
	}
	txn, _ := db.Begin(ctx)
	ksn, _ := txn.OpenKeyspace("ovf")
	_ = ksn.Put([]byte("nudge"), []byte("x"))
	if err := txn.Commit(); err != nil {
		t.Fatalf("Commit nudge: %v", err)
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

	// Corrupt the follower's content IN PLACE (leaving its stored
	// footer) → footer verification mismatches on read. The DB stays
	// open: a clean Close's checkpoint would consume the free capacity
	// the fixture just arranged, and the mmap observes the external
	// write through the shared page cache.
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xDE, 0xAD, 0xBE, 0xEF}, int64(follower)*4096+100); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	f.Close()

	// Relocate everything (floor=firstData): relocating the chain reads the
	// corrupt follower via pw.Page → ErrBadPageChecksum, rolled back.
	tx2, _ := db.Begin(ctx)
	_, err = tx2.compactForest(0, 1<<20)
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

// TestCompactionPassRelocatesRPLSegments pins the pass-level wiring
// of the RPL chain-prefix relocation (free-space.md §RPL segment
// relocation) in the scenario the mechanism exists for: a lagging
// reader pins the reclamation bound, so in-band segments cannot drain
// on their own and must be MOVED. The fixture builds a dense low
// region with scattered holes (below-floor homes), holds a read
// transaction, churns to stack RPL segments near the high-water mark,
// then runs passes and asserts the pre-existing segments leave the
// band while the reader is still live.
func TestCompactionPassRelocatesRPLSegments(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 2048,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// DURABLE low holes: fill an early region A whose leaf pages sit
	// below the later region B's, then delete A whole — the freed
	// pages are NON-trailing (B's live pages sit above them), so the
	// tail refund cannot take them and they survive as below-floor
	// capacity for the prefix relocation's homes.
	tx, _ := db.Begin(ctx)
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for i := range 900 {
		if err := ks.Put(fmt.Appendf(nil, "a%06d", i), make([]byte, 256)); err != nil {
			t.Fatalf("Put a: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit fill A: %v", err)
	}
	txb, _ := db.Begin(ctx)
	ksb, _ := txb.OpenKeyspace("k")
	for i := range 300 {
		if err := ksb.Put(fmt.Appendf(nil, "b%06d", i), make([]byte, 256)); err != nil {
			t.Fatalf("Put b: %v", err)
		}
	}
	if err := txb.Commit(); err != nil {
		t.Fatalf("commit fill B: %v", err)
	}
	txd, _ := db.Begin(ctx)
	ksd, _ := txd.OpenKeyspace("k")
	if _, err := ksd.DeleteRange([]byte("a"), []byte("b")); err != nil {
		t.Fatalf("DeleteRange A: %v", err)
	}
	if err := txd.Commit(); err != nil {
		t.Fatalf("commit holes: %v", err)
	}
	txnudge, _ := db.Begin(ctx)
	ksnudge, _ := txnudge.OpenKeyspace("k")
	_ = ksnudge.Put([]byte("nudge"), []byte("x"))
	if err := txnudge.Commit(); err != nil {
		t.Fatalf("commit nudge: %v", err)
	}
	// One reclaim-only commit returns the holes to the bitmap before
	// the reader pins the bound — a full pass would consume them as
	// relocation targets, leaving the prefix relocation no homes.
	if !db.reclaimOrAdvanceCommit(ctx) {
		t.Fatal("pre-reclaim did not commit")
	}

	// Lagging reader: pins the reclamation bound from here on.
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer rtx.Rollback()

	// Churn under the pin: every commit's retirements stack RPL
	// segments the bound cannot drain, at pages near the HWM.
	for round := range 6 {
		txc, _ := db.Begin(ctx)
		ksc, _ := txc.OpenKeyspace("k")
		for i := range 10 {
			if err := ksc.Put(fmt.Appendf(nil, "churn%02d-%04d", round, i), make([]byte, 256)); err != nil {
				t.Fatalf("churn put: %v", err)
			}
		}
		if err := txc.Commit(); err != nil {
			t.Fatalf("churn commit: %v", err)
		}
	}

	db.mu.Lock()
	pgr := db.pgr
	db.mu.Unlock()
	preChain := pgr.RPLChain()
	if len(preChain) < 2 {
		t.Fatalf("fixture: chain too short (%d)", len(preChain))
	}
	prePages := map[uint64]bool{}
	for _, r := range preChain {
		prePages[r.PageID] = true
	}

	relocatedSome := false
	for pass := range 20 {
		if _, err := db.compactionPass(ctx, 64); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		db.mu.Lock()
		firstData := uint64(2) + uint64(db.currentMeta.BitmapPages)
		db.mu.Unlock()
		floor, ok := pgr.EvacuationFloor(firstData, 64, uint64(pgr.RPLRelocationPrefixLen(firstData))+2)
		if !ok {
			break
		}
		// Success: no PRE-EXISTING segment page remains at/above the
		// floor (the passes' own fresh heads are self-healing and
		// exempt from the assertion).
		stuck := 0
		for _, r := range pgr.RPLChain() {
			if prePages[r.PageID] && r.PageID >= floor {
				stuck++
			}
		}
		if stuck == 0 {
			relocatedSome = true
			break
		}
	}
	if !relocatedSome {
		t.Fatal("pre-existing RPL segments never left the evacuation band across 20 passes (reader still pinning)")
	}
}

// Compaction passes must CONVERGE: relocated pages receive below-floor
// ids (the consolidating allocator), so successive passes drain the
// band and the trigger quiesces with the file near its live size.
// Pre-fix, relocations drew from the LIFO hint — which each pass's
// eager reclaim had just pointed INTO the band — so the band refilled
// every pass: moved stayed positive forever and the HWM plateaued far
// above the live size (background-maintenance.md §Incremental
// Compaction step 2 + convergence clause).
func TestCompactionConvergesToQuiescence(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 1 << 20,
		ShrinkThreshold: 1,
		Maintenance:     MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// 12000 rows, then delete a large LOW range: the live set is
	// top-heavy and dense, global free density is low, so
	// the evacuation floor lands well above the first data page (the STRICT
	// regime) with a band population far above the per-pass budget —
	// exactly the partial-evacuation steady state where pre-fix
	// relocations re-landed in the band (the LIFO hint pointed at the
	// holes the pass's own eager reclaim just opened there) and the
	// tail refund could never pass the refilled band.
	// An index rides along so the index forest (registry sub-tree +
	// index data tree) is part of the relocation surface — it shares
	// the pass's consolidating writer.
	decl := &IndexDecl{
		Name:    "byval",
		Columns: []IndexColumn{{Name: "v"}},
		Extract: func(key, value []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{value}}}
		},
	}
	tx, _ := db.Begin(ctx)
	ks, err := tx.CreateKeyspace("k", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for i := range 12000 {
		if err := ks.Put(fmt.Appendf(nil, "key%06d", i), fmt.Appendf(nil, "val%06d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	tx.Commit()
	txd, _ := db.Begin(ctx)
	ksd, _ := txd.OpenKeyspace("k", decl)
	if _, err := ksd.DeleteRange(fmt.Appendf(nil, "key%06d", 100), fmt.Appendf(nil, "key%06d", 8000)); err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	txd.Commit()
	txn, _ := db.Begin(ctx)
	ksn, _ := txn.OpenKeyspace("k", decl)
	_ = ksn.Put([]byte("nudge"), []byte("x"))
	txn.Commit()

	initialHWM := db.Meta().HighWaterMark
	firstData := uint64(2) + uint64(db.Meta().BitmapPages)

	// ~200 tiny surviving rows fit in a handful of pages; the bound is
	// generous yet unmistakably below any band-refill plateau (pre-fix
	// the plateau sat far above it: relocations re-landed in the band,
	// so the tail refund could never pass it). Passes may keep
	// repacking a tiny dense file (the whole-region regime; the
	// fragmentation trigger governs invocation in production), so the
	// convergence pin is the HWM reaching the live-size bound and
	// never regressing — not moved hitting zero.
	bound := firstData + 100 // ~4000 tiny live rows ≈ 80 pages + slack
	reachedAt := -1
	prev := initialHWM
	for pass := range 60 {
		// The production driver: handles mid-pass space exhaustion by
		// landing the reclaim/bound-advance commit and retrying.
		db.runCompaction(ctx)
		hwm := db.Meta().HighWaterMark
		if hwm > prev {
			t.Errorf("pass %d: HWM grew %d -> %d", pass, prev, hwm)
		}
		prev = hwm
		if hwm <= bound {
			reachedAt = pass
			break
		}
	}
	if reachedAt < 0 {
		t.Fatalf("HWM plateaued at %d after 60 passes (initial %d, live-size bound %d): the passes are not converging (band refill, or a frozen reclamation bound)", prev, initialHWM, bound)
	}

	// Full drain: two more passes land the trailing retirees — the
	// declining pass's bound-advance commit is what makes the LAST
	// batch reclaimable (without it the final retirees stay pending
	// forever and the file holds their pages).
	db.runCompaction(ctx)
	db.runCompaction(ctx)
	if rplE := db.Meta().RPLEntryCount; rplE != 0 {
		t.Errorf("RPL not drained after convergence: %d entries still pending (the declining pass never advances the reclamation bound)", rplE)
	}

	// Integrity: survivors intact, deleted gone, Check clean.
	rtx, _ := db.Begin(ctx)
	rks, _ := rtx.OpenKeyspace("k", decl)
	for _, i := range []int{0, 99, 8000, 11999} {
		if _, err := rks.Get(fmt.Appendf(nil, "key%06d", i)); err != nil {
			t.Errorf("survivor key%06d missing: %v", i, err)
		}
	}
	if _, err := rks.Get(fmt.Appendf(nil, "key%06d", 1500)); err == nil {
		t.Errorf("deleted key present")
	}
	rtx.Rollback()
	assertCheckClean(t, db, "post-convergence")
}

// compactionWriter is the consolidating allocator's wiring: allocations
// draw the LOWEST free hole strictly below allocBound regardless of the
// LIFO hint (which eager reclaim points INTO the band being drained),
// count against the pass-wide allowance, and abort with the space
// sentinel rather than fall back to the base allocator's hint/extension
// tiers.
func TestCompactionWriterAllocPolicy(t *testing.T) {
	dir := t.TempDir()
	f, err := os.OpenFile(filepath.Join(dir, "db.gmdb"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	const pages = 64
	if err := f.Truncate(int64(pages) * 4096); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	pool := pager.NewBufPool(4096)
	p, err := pager.NewWriter(f, page.Config{PageSize: 4096}, int64(pages)*4096, pool, 16<<20)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer p.Close()
	bm := bitmap.New(make([]byte, 4096), 4096, 1, pages)
	p.AttachBitmap(bm)
	p.SetCommitState(50, pages, 0) // HWM 50
	first := bm.FirstDataPage()

	// Holes below the bound (low) AND above it (in-band, near HWM);
	// the hint points at the in-band ones — the base allocator would
	// take those and refill the band.
	low := []uint64{first + 4, first + 5, first + 6, first + 9}
	high := []uint64{first + 40, first + 41, first + 42}
	for _, id := range append(append([]uint64{}, low...), high...) {
		bm.Set(id)
	}
	bm.SetHint(first + 40)

	bound := first + 20
	allowance := uint64(3)
	w := compactionWriter{btreeWriter{p}, bound, &allowance}

	id, err := w.AllocPage()
	if err != nil || id != first+4 {
		t.Fatalf("AllocPage = (%d, %v), want the lowest below-bound hole %d", id, err, first+4)
	}
	// Contiguous: the below-bound pair, never the in-band run.
	id, err = w.AllocContiguous(2)
	if err != nil {
		t.Fatalf("AllocContiguous: %v", err)
	}
	if id >= bound {
		t.Fatalf("AllocContiguous = %d — an at/above-bound target (band refill)", id)
	}
	// Allowance exhausted (3 pages consumed): the sentinel, never a
	// fallback into the in-band holes.
	if _, err := w.AllocPage(); !errors.Is(err, errCompactionSpaceExhausted) {
		t.Fatalf("AllocPage past the allowance = %v, want errCompactionSpaceExhausted", err)
	}
	// Below-bound space exhausted with allowance remaining: sentinel
	// too (the in-band holes are never targets). One below-bound hole
	// (first+9) remains — consume it, then require the sentinel.
	allowance = 5
	if id, err := w.AllocPage(); err != nil || id != first+9 {
		t.Fatalf("AllocPage = (%d, %v), want the last below-bound hole %d", id, err, first+9)
	}
	if _, err := w.AllocPage(); !errors.Is(err, errCompactionSpaceExhausted) {
		t.Fatalf("AllocPage past below-bound capacity = %v, want errCompactionSpaceExhausted", err)
	}
	// Contiguous exhaustion below the bound: the in-band 3-run at
	// first+40 must never be the fallback.
	if _, err := w.AllocContiguous(2); !errors.Is(err, errCompactionSpaceExhausted) {
		t.Fatalf("AllocContiguous past below-bound capacity = %v, want errCompactionSpaceExhausted", err)
	}
	if used := bm.IsSet(first + 40); !used {
		t.Fatal("an in-band hole was consumed (base-allocator fallback)")
	}
}

// runCompaction must TERMINATE when below-floor capacity is
// unsatisfiable and the reclamation bound is frozen: a pinned reader
// stops the bound regardless of how many commits land, so an
// unconditional advance-commit-and-retry spins the maintenance
// goroutine at ~1000 empty commits/s until the reader ends (the
// once-per-invocation advance cap is the termination bound). The
// fixture makes the floor feasible by COUNT while a 12-page contiguous
// chain in the band cannot find a below-floor run → the sentinel path,
// with the RPL held non-empty by post-pin churn.
func TestRunCompactionTerminatesOnFrozenBound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 1 << 20,
		ShrinkThreshold: 1,
		Maintenance:     MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// One-page rows alternating across two keyspaces → page ownership
	// interleaves, so deleting keyspace "del" leaves strictly
	// SINGLE-PAGE holes: the floor is feasible by COUNT, but the
	// 12-page chain relocation finds no contiguous below-floor run —
	// the sentinel path, every pass.
	tx, _ := db.Begin(ctx)
	ka, _ := tx.CreateKeyspace("keep")
	kb, _ := tx.CreateKeyspace("del")
	for i := range 400 {
		if err := ka.Put(fmt.Appendf(nil, "a%06d", i), bytes.Repeat([]byte{'a'}, 3500)); err != nil {
			t.Fatalf("Put a: %v", err)
		}
		if err := kb.Put(fmt.Appendf(nil, "b%06d", i), bytes.Repeat([]byte{'b'}, 3500)); err != nil {
			t.Fatalf("Put b: %v", err)
		}
	}
	tx.Commit()
	txd, _ := db.Begin(ctx)
	kd, _ := txd.OpenKeyspace("del")
	if _, err := kd.DeleteRange([]byte("b"), []byte("c")); err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	txd.Commit()
	txe, _ := db.Begin(ctx)
	ke, _ := txe.OpenKeyspace("keep")
	_ = ke.Put([]byte("nudge0"), []byte("x"))
	txe.Commit()
	// The large contiguous chain lands high (the band).
	txc, _ := db.Begin(ctx)
	ksc, _ := txc.OpenKeyspace("keep")
	if err := ksc.Put([]byte("zzz-big"), bytes.Repeat([]byte{'B'}, 48<<10)); err != nil {
		t.Fatalf("Put big: %v", err)
	}
	txc.Commit()

	// Pin the bound, then churn so the RPL stays non-empty.
	pin, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	defer pin.Rollback()
	txn, _ := db.Begin(ctx)
	ksn, _ := txn.OpenKeyspace("keep")
	_ = ksn.Put([]byte("nudge"), []byte("x"))
	txn.Commit()

	before := db.Meta().TxnID
	done := make(chan struct{})
	go func() {
		db.runCompaction(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runCompaction did not return within 30s — the sentinel path is spinning against the frozen bound")
	}
	if delta := db.Meta().TxnID - before; delta > 16 {
		t.Fatalf("runCompaction committed %d transactions in one invocation (want a small bounded count) — empty-commit churn against the frozen bound", delta)
	}
}
