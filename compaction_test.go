package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"
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
	tx, err := db.Begin(ctx, true)
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
	tx, _ := db.Begin(ctx, true)
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
	tx2, _ := db.Begin(ctx, true)
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
		rtx, _ := db.Begin(ctx, true)
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
	rtx, _ := db.Begin(ctx, true)
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

	tx, _ := db.Begin(ctx, true)
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

	rtx, _ := db.Begin(ctx, true)
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

	tx, _ := db.Begin(ctx, true)
	moved, err := tx.compactForest(func(uint64) bool { return true }, 1000)
	if err != nil || moved != 0 {
		t.Errorf("empty forest: moved=%d err=%v, want 0,nil", moved, err)
	}
	_ = tx.Rollback()

	// Non-empty forest, zero budget ⇒ no-op.
	tx2, _ := db.Begin(ctx, true)
	ks, _ := tx2.CreateKeyspace("k")
	_ = ks.Put([]byte("a"), []byte("b"))
	moved, err = tx2.compactForest(func(uint64) bool { return true }, 0)
	if err != nil || moved != 0 {
		t.Errorf("zero budget: moved=%d err=%v, want 0,nil", moved, err)
	}
	_ = tx2.Rollback()
}
