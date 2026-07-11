package gmdb

import (
	"context"
	"fmt"
	"testing"
)

// An extractor panic during Delete's index maintenance must escape
// BEFORE any index mutation (extract-all-then-mutate) AND resolve the
// caller's shallow savepoint on the way out — a recovering caller
// that commits must leave NO partial index state (indexing.md §Write
// Path; Check(CheckIndexes) clean) and no unresolved savepoint (the
// pager's all-resolved-before-Commit assumption).
func TestExtractorPanicOnDeleteLeavesNoPartialIndexState(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	panicOn := false
	declA := &IndexDecl{
		Name:    "a",
		Columns: []IndexColumn{{Name: "c"}},
		Extract: func(key, value []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{value[:1]}}}
		},
	}
	declB := &IndexDecl{
		Name:    "b",
		Columns: []IndexColumn{{Name: "c"}},
		Extract: func(key, value []byte) []IndexEntry {
			if panicOn {
				panic("extractor exploded")
			}
			return []IndexEntry{{Cols: [][]byte{value[1:2]}}}
		},
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("k", declA, declB)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("row"), []byte("xy")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// The recovering caller: Delete panics in index "b"'s extractor
	// AFTER "a" would (pre-fix) have had its entries deleted.
	panicOn = true
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("extractor panic did not propagate")
			}
		}()
		_ = ks.Delete([]byte("row"))
	}()
	panicOn = false
	// The escaped panic resolved the caller's shallow savepoint on
	// the way out (the pager's all-resolved-before-Commit
	// assumption).
	if d := tx.ActiveSavepointDepthForTest(); d != 0 {
		t.Fatalf("%d unresolved savepoints after the recovered panic", d)
	}

	// The recovering caller commits.
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after recovered panic: %v", err)
	}

	// No partial index state: the row survives WITH all its index
	// entries — CheckIndexes is clean.
	issues := collectIssues(db.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"k": {declA, declB}},
	}))
	for _, iss := range issues {
		if iss.Severity == CheckError || iss.Severity == CheckFatal {
			t.Errorf("partial index state committed: %+v", iss)
		}
	}
}

// The SetKeyspace BULK key delete interleaves per-member
// extract→mutate, so the panic-restore wrapper's PINNED-STATE half is
// load-bearing there: member 2's extractor panic lands after member
// 1's index deletions mutated pinned {root,count} — a recovering
// caller's commit must still leave Check clean (no double-reference,
// no drift, no leak) and the savepoint resolved.
func TestExtractorPanicOnBulkKeyDeleteLeavesNoPartialState(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	panicOn := false
	decl := &IndexDecl{
		Name:    "m",
		Columns: []IndexColumn{{Name: "c"}},
		Extract: func(key, value []byte) []IndexEntry {
			if panicOn && string(value) == "member2" {
				panic("extractor exploded")
			}
			return []IndexEntry{{Cols: [][]byte{value}}}
		},
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	sks, err := tx.CreateSetKeyspace("s", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if _, err := sks.Put([]byte("k"), []byte("member1")); err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	if _, err := sks.Put([]byte("k"), []byte("member2")); err != nil {
		t.Fatalf("Put 2: %v", err)
	}

	panicOn = true
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("extractor panic did not propagate")
			}
		}()
		_ = sks.Delete([]byte("k"))
	}()
	panicOn = false
	if d := tx.ActiveSavepointDepthForTest(); d != 0 {
		t.Fatalf("%d unresolved savepoints after the recovered panic", d)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	issues := collectIssues(db.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"s": {decl}},
	}))
	for _, iss := range issues {
		if iss.Severity == CheckError || iss.Severity == CheckFatal {
			t.Errorf("partial state committed: %+v", iss)
		}
	}
}

// An extractor panic mid-Rebuild must RESTORE the rebuild savepoint —
// branching on the error alone released it during panic unwind,
// permanently leaking the partial new-tree allocations when a
// recovering caller committed (Check: BitmapLeak).
func TestExtractorPanicMidRebuildLeaksNothing(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 512})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	plain := &IndexDecl{
		Name:    "r",
		Columns: []IndexColumn{{Name: "c"}},
		Extract: func(key, value []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{value}}}
		},
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("k", plain)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for i := range 50 {
		if err := ks.Put(fmt.Appendf(nil, "key%03d", i), fmt.Appendf(nil, "val%03d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	n := 0
	bomb := &IndexDecl{
		Name:    "r",
		Columns: []IndexColumn{{Name: "c"}},
		Extract: func(key, value []byte) []IndexEntry {
			if n++; n == 26 {
				panic("rebuild extractor exploded")
			}
			return []IndexEntry{{Cols: [][]byte{value}}}
		},
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("rebuild panic did not propagate")
			}
		}()
		_ = tx.Indexes().Rebuild("k", bomb)
	}()
	if d := tx.ActiveSavepointDepthForTest(); d != 0 {
		t.Fatalf("%d unresolved savepoints after the recovered rebuild panic", d)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for iss := range db.Check() {
		if iss.Code == "BitmapLeak" {
			t.Errorf("rebuild panic leaked pages: %+v", iss)
		}
		if iss.Severity == CheckError || iss.Severity == CheckFatal {
			t.Errorf("unexpected: %+v", iss)
		}
	}
}

// An EMPTY-parent Rebuild is a success: its registry write must
// persist (the panic-restore defer must Release, not Restore, on this
// early success return). The not-cached path is the sharp edge — the
// staged descriptor carries the new registry root, and a spurious
// restore leaves it dangling over reverted pages: the index registry
// becomes unreadable at the next open.
func TestEmptyParentRebuildPersists(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	declV1 := &IndexDecl{
		Name:    "i",
		Columns: []IndexColumn{{Name: "c"}},
		Extract: func(key, value []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{value}}}
		},
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.CreateKeyspace("k", declV1)
		return e
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	declV2 := &IndexDecl{
		Name:    "i",
		Columns: []IndexColumn{{Name: "c"}},
		Unique:  true,
		Extract: func(key, value []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{value}}}
		},
	}
	// Rebuild WITHOUT opening the keyspace: the not-cached path.
	if err := db.Update(ctx, func(tx *Tx) error {
		return tx.Indexes().Rebuild("k", declV2)
	}); err != nil {
		t.Fatalf("empty-parent Rebuild: %v", err)
	}
	// The registry must be readable and carry declV2.
	if err := db.Update(ctx, func(tx *Tx) error {
		_, e := tx.OpenKeyspace("k", declV2)
		return e
	}); err != nil {
		t.Fatalf("post-rebuild open: %v (registry dangling after a spuriously-restored empty-parent rebuild)", err)
	}
}
