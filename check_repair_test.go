package gmdb

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
)

// buildKeyspaceWithLeak creates a keyspace "k" with n round-trippable
// rows plus one deliberately leaked page (allocated + committed but
// linked into no tree). Returns the leaked page id. Used by the Repair
// tests as a reproducible BitmapLeak.
func buildKeyspaceWithLeak(t *testing.T, db *DB, n int) uint64 {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for i := range n {
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "val%05d", i)); err != nil {
			t.Fatalf("Put %d: %v", i, err)
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

// assertKeyspaceIntact re-reads every key%05d..n-1 from keyspace "k" and
// fails if any is missing or has the wrong value — the Inv-C5 "free ONLY
// unreachable pages, never a live one" guarantee.
func assertKeyspaceIntact(t *testing.T, db *DB, n int) {
	t.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (verify): %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.OpenKeyspace("k")
	if err != nil {
		t.Fatalf("OpenKeyspace (verify): %v", err)
	}
	for i := range n {
		got, err := ks.Get(fmt.Appendf(nil, "key%05d", i))
		if err != nil {
			t.Fatalf("Get key%05d after repair: %v", i, err)
		}
		want := fmt.Sprintf("val%05d", i)
		if string(got) != want {
			t.Fatalf("key%05d = %q, want %q after repair", i, got, want)
		}
	}
}

func numFreePages(t *testing.T, db *DB) uint64 {
	t.Helper()
	tx, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin (numFree): %v", err)
	}
	defer tx.Rollback()
	return tx.prevMeta.NumFreePages
}

// TestRepairReclaimsLeak (Inv-C5 happy path): on a structurally clean,
// quiescent database, Repair frees the leaked page (Repaired=true), a
// subsequent plain Check reports no leak, and every live row survives —
// across a close/re-open (atomicity via the commit pipeline).
func TestRepairReclaimsLeak(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	const n = 800
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	leaked := buildKeyspaceWithLeak(t, db, n)

	issues := collectIssues(db.CheckWithOptions(&CheckOptions{Repair: true}))
	var reclaimed bool
	for _, iss := range issues {
		if iss.Severity == CheckError || iss.Severity == CheckFatal {
			t.Errorf("unexpected error/fatal during repair of clean DB: code=%s msg=%s", iss.Code, iss.Message)
		}
		if iss.Code == "BitmapLeak" && iss.PageID == leaked {
			if !iss.Repaired {
				t.Errorf("leaked page %d reported with Repaired=false", leaked)
			}
			reclaimed = true
		}
	}
	if !reclaimed {
		t.Fatalf("leaked page %d not reported as reclaimed; issues=%v", leaked, issuesByCode(issues))
	}

	// Leak is gone: a plain read-only Check finds no BitmapLeak.
	for _, iss := range collectIssues(db.Check()) {
		if iss.Code == "BitmapLeak" {
			t.Errorf("BitmapLeak still present after repair: page=%d", iss.PageID)
		}
		if iss.Severity != CheckWarning {
			t.Errorf("post-repair Check error: code=%s msg=%s", iss.Code, iss.Message)
		}
	}
	assertKeyspaceIntact(t, db, n)

	// Atomicity: the freed bitmap survives close + re-open, and the DB is
	// still structurally clean with all data intact.
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	for _, iss := range collectIssues(db2.Check()) {
		if iss.Code == "BitmapLeak" {
			t.Errorf("BitmapLeak reappeared after re-open: page=%d", iss.PageID)
		}
		if iss.Severity != CheckWarning {
			t.Errorf("post-reopen Check error: code=%s msg=%s", iss.Code, iss.Message)
		}
	}
	assertKeyspaceIntact(t, db2, n)
}

// TestRepairRefusesWithActiveReader (Inv-C5 clause-explicit exclusivity):
// with a read transaction live, Repair emits Repair.ReadersActive and
// reclaims nothing — the leak survives for a later, exclusive repair.
func TestRepairRefusesWithActiveReader(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	leaked := buildKeyspaceWithLeak(t, db, 50)

	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}

	issues := collectIssues(db.CheckWithOptions(&CheckOptions{Repair: true}))
	var sawReadersActive, sawReclaim bool
	for _, iss := range issues {
		if iss.Code == "Repair.ReadersActive" {
			sawReadersActive = true
		}
		if iss.Code == "BitmapLeak" && iss.Repaired {
			sawReclaim = true
		}
	}
	if !sawReadersActive {
		t.Errorf("Repair.ReadersActive not emitted with reader active; issues=%v", issuesByCode(issues))
	}
	if sawReclaim {
		t.Errorf("Repair reclaimed a page despite an active reader")
	}
	rtx.Rollback()

	// The leak is intact: an exclusive repair now reclaims it.
	var reclaimed bool
	for _, iss := range collectIssues(db.CheckWithOptions(&CheckOptions{Repair: true})) {
		if iss.Code == "BitmapLeak" && iss.PageID == leaked && iss.Repaired {
			reclaimed = true
		}
	}
	if !reclaimed {
		t.Errorf("leak %d not reclaimed by the subsequent exclusive repair", leaked)
	}
}

// TestRepairSkipsUnderCorruption (Inv-C5 completeness gate — the strongest
// case): a forged data-tree root makes keyspace "k"'s walk abort, so its
// LIVE pages never enter the reachable set and would look leaked. Repair
// MUST free nothing (it would otherwise reclaim live data); it reports the
// would-be leaks with Repaired=false plus Repair.Skipped, and the meta's
// free count is unchanged.
func TestRepairSkipsUnderCorruption(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Force a multi-level tree so the data-tree root is a branch.
	buildKeyspaceWithLeak(t, db, 800)
	tx, _ := db.Begin(ctx)
	ks, _ := tx.OpenKeyspace("k")
	root := ks.desc.Root
	tx.Rollback()
	if root == 0 {
		t.Fatal("keyspace root is 0")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Corrupt the data-tree root's first cell-directory entry offset to
	// 0xFFFF (same forge as TestCheckForgedBranchNoPanic): the subtree
	// walk aborts, leaving k's live pages unvisited.
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xFF, 0xFF}, int64(root)*4096+16); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	f.Close()

	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()

	freeBefore := numFreePages(t, db2)
	issues := collectIssues(db2.CheckWithOptions(&CheckOptions{Repair: true}))
	var sawReclaim, sawCorruption, sawUnrepairedLeak bool
	for _, iss := range issues {
		if iss.Code == "BitmapLeak" && iss.Repaired {
			sawReclaim = true
		}
		if iss.Code == "BitmapLeak" && !iss.Repaired {
			sawUnrepairedLeak = true
		}
		if iss.Code == "TreeCorrupted" || iss.Code == "KeyspaceWalkFailed" || iss.Code == "Repair.Skipped" {
			sawCorruption = true
		}
	}
	if sawReclaim {
		t.Errorf("Repair reclaimed a page on a corrupt DB (live pages at risk); issues=%v", issuesByCode(issues))
	}
	if !sawCorruption {
		t.Errorf("corruption not surfaced during repair; issues=%v", issuesByCode(issues))
	}
	// The gate's load-bearing proof: the forged root makes k's LIVE
	// subtree pages unreachable, so they ARE collected into the would-be
	// -leak set (reported Repaired=false) — and would be freed without the
	// gate. Their presence + zero reclamation is the demonstration.
	if !sawUnrepairedLeak {
		t.Errorf("expected k's unvisited live pages reported as unrepaired BitmapLeaks; issues=%v", issuesByCode(issues))
	}
	if got := numFreePages(t, db2); got != freeBefore {
		t.Errorf("NumFreePages changed under corruption repair: before=%d after=%d (Repair committed)", freeBefore, got)
	}
}

// TestRepairWriteTxUnavailable: Repair on a closed DB cannot open the
// exclusive write tx and surfaces a single CheckFatal Repair.
// WriteTxUnavailable.
func TestRepairWriteTxUnavailable(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	issues := collectIssues(db.CheckWithOptions(&CheckOptions{Repair: true}))
	if len(issues) != 1 || issues[0].Code != "Repair.WriteTxUnavailable" || issues[0].Severity != CheckFatal {
		t.Fatalf("closed-DB repair: got %v, want single CheckFatal Repair.WriteTxUnavailable", issues)
	}
}

// TestRepairCommitFailedReclaimsNothing: a commit-pipeline failure during
// the repair commit yields a CheckFatal Repair.CommitFailed and reclaims
// no page (Repaired=true never appears). Atomicity: the freed bits never
// reach disk because the meta-swap failed.
func TestRepairCommitFailedReclaimsNothing(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	buildKeyspaceWithLeak(t, db, 50)

	// Inject a step-4 (meta fdatasync) commit failure.
	db.pgr.SetCommitStep4HookForTest(func() error { return io.ErrUnexpectedEOF })

	issues := collectIssues(db.CheckWithOptions(&CheckOptions{Repair: true}))
	var sawCommitFailed, sawReclaim bool
	for _, iss := range issues {
		if iss.Code == "Repair.CommitFailed" && iss.Severity == CheckFatal {
			sawCommitFailed = true
		}
		if iss.Code == "BitmapLeak" && iss.Repaired {
			sawReclaim = true
		}
	}
	if !sawCommitFailed {
		t.Errorf("Repair.CommitFailed not emitted; issues=%v", issuesByCode(issues))
	}
	if sawReclaim {
		t.Errorf("Repair reported a reclaimed page despite commit failure")
	}
	db.pgr.SetCommitStep4HookForTest(nil)
}

// TestRepairCleanDBNoChange: Repair on a structurally clean DB with no
// leaks yields no issues and commits nothing (free count unchanged).
func TestRepairCleanDBNoChange(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 100 {
		_ = ks.Put(fmt.Appendf(nil, "key%05d", i), []byte("v"))
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	freeBefore := numFreePages(t, db)
	for _, iss := range collectIssues(db.CheckWithOptions(&CheckOptions{Repair: true})) {
		t.Errorf("unexpected issue on clean-DB repair: code=%s sev=%d msg=%s", iss.Code, iss.Severity, iss.Message)
	}
	if got := numFreePages(t, db); got != freeBefore {
		t.Errorf("NumFreePages changed on clean-DB repair: before=%d after=%d", freeBefore, got)
	}
}
