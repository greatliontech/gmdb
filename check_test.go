package gmdb

import (
	"context"
	"fmt"
	"os"
	"testing"
)

func collectIssues(seq func(func(CheckIssue) bool)) []CheckIssue {
	var out []CheckIssue
	for iss := range seq {
		out = append(out, iss)
	}
	return out
}

func issuesByCode(issues []CheckIssue) map[string]int {
	m := make(map[string]int)
	for _, iss := range issues {
		m[iss.Code]++
	}
	return m
}

// TestCheckCleanPopulatedDB: a freshly built, quiescent database with a
// keyspace + an index reports no CheckError / CheckFatal issues, and no
// spurious leak / free-count warnings.
func TestCheckCleanPopulatedDB(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	decl := &IndexDecl{
		Name:    "by_first",
		Columns: []IndexColumn{{Name: "first"}},
		Extract: func(_, value []byte) []IndexEntry {
			if len(value) == 0 {
				return nil
			}
			return []IndexEntry{{Cols: [][]byte{{value[0]}}}}
		},
	}
	tx, _ := db.Begin(ctx, true)
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for i := range 800 {
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "%c-val-%05d", byte('a'+i%26), i)); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	// A second, index-free keyspace (IndexRegistryRoot==0 path).
	plain, err := tx.CreateKeyspace("plain")
	if err != nil {
		t.Fatalf("CreateKeyspace plain: %v", err)
	}
	for i := range 100 {
		_ = plain.Put(fmt.Appendf(nil, "p%05d", i), []byte("v"))
	}
	// A SetKeyspace with enough members per key to promote nested B+trees
	// (exercises the nested-tree walk).
	set, err := tx.CreateSetKeyspace("set", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	for i := range 200 {
		key := fmt.Appendf(nil, "s%03d", i%10)
		if _, err := set.Put(key, fmt.Appendf(nil, "member%05d", i)); err != nil {
			t.Fatalf("set Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	issues := collectIssues(db.Check())
	for _, iss := range issues {
		if iss.Severity != CheckWarning {
			t.Errorf("unexpected issue: sev=%d code=%s ks=%s idx=%s page=%d msg=%s",
				iss.Severity, iss.Code, iss.Keyspace, iss.Index, iss.PageID, iss.Message)
		} else {
			t.Errorf("unexpected warning on clean DB: code=%s page=%d msg=%s", iss.Code, iss.PageID, iss.Message)
		}
	}
}

// TestCheckDetectsBitmapLeak (Inv-C2): an allocated-but-unreferenced
// page (a bare AllocPage+AllocSlab committed without being linked into
// any tree) is reported as a BitmapLeak.
func TestCheckDetectsBitmapLeak(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, _ := db.Begin(ctx, true)
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

	issues := collectIssues(db.Check())
	var found bool
	for _, iss := range issues {
		if iss.Code == "BitmapLeak" && iss.PageID == leaked {
			found = true
		}
		if iss.Severity == CheckFatal {
			t.Errorf("unexpected fatal: %s", iss.Message)
		}
	}
	if !found {
		t.Errorf("BitmapLeak for page %d not reported; issues=%v", leaked, issuesByCode(issues))
	}
}

// TestCheckDetectsBadChecksum (Inv-C1 surface): corrupting a reachable
// tree page on disk is reported as BadPageChecksum after re-open.
func TestCheckDetectsBadChecksum(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, PageChecksum: true, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx, true)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 50 {
		if err := ks.Put(fmt.Appendf(nil, "key%03d", i), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Capture the keyspace data-tree root (same-package field access).
	tx2, _ := db.Begin(ctx, true)
	ks2, _ := tx2.OpenKeyspace("k")
	root := ks2.desc.Root
	tx2.Rollback()
	if root == 0 {
		t.Fatal("keyspace root is 0")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Flip a byte in the root page's xxhash64 footer (last 8 bytes), so
	// the page structure still validates but the checksum no longer
	// matches — isolating a BadPageChecksum from a structural error.
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	off := int64(root)*4096 + 4096 - 4
	one := make([]byte, 1)
	if _, err := f.ReadAt(one, off); err != nil {
		t.Fatalf("read: %v", err)
	}
	one[0] ^= 0xFF
	if _, err := f.WriteAt(one, off); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()

	issues := collectIssues(db2.Check())
	var found bool
	for _, iss := range issues {
		if iss.Code == "BadPageChecksum" && iss.PageID == root {
			found = true
		}
	}
	if !found {
		t.Errorf("BadPageChecksum for root page %d not reported; issues=%v", root, issuesByCode(issues))
	}
}

// TestCheckEarlyBreakReleasesReaderSlot: breaking out of the Check range
// loop releases the reader slot, so a subsequent Check can acquire one
// even with MaxReaders=1.
func TestCheckEarlyBreakReleasesReaderSlot(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256, MaxReaders: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx, true)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 50 {
		_ = ks.Put(fmt.Appendf(nil, "k%03d", i), []byte("v"))
	}
	// Create a leak so Check is guaranteed to yield at least one issue,
	// ensuring the break below is actually taken (exercises the
	// early-abandon release path, not the normal-exhaustion path).
	leaked, _ := tx.AllocPage()
	_, _ = tx.AllocSlab(leaked)
	tx.Commit()

	// First Check: break on the first issue.
	var iterated int
	for range db.Check() {
		iterated++
		break
	}
	if iterated == 0 {
		t.Fatal("first Check yielded no issue; early-break path not exercised")
	}

	// Second Check must still acquire the single reader slot.
	for iss := range db.Check() {
		if iss.Code == "ReadTxUnavailable" {
			t.Fatalf("reader slot leaked by early-broken Check: %s", iss.Message)
		}
	}
}

// TestCheckForgedBranchNoPanic (Inv-C1, pins the 11.2 review H-1 fix): a
// corrupt branch page in a real on-disk keyspace tree is reported as a
// structural issue, NOT panicked on — Check enumerates via the guarded
// WalkKV, never the unguarded read cursor.
func TestCheckForgedBranchNoPanic(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx, true)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 800 { // force a multi-level tree (root is a branch)
		_ = ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "val%05d", i))
	}
	tx.Commit()
	tx2, _ := db.Begin(ctx, true)
	ks2, _ := tx2.OpenKeyspace("k")
	root := ks2.desc.Root
	tx2.Rollback()
	db.Close()

	// Corrupt the keyspace data-tree root's first cell-directory entry
	// offset to 0xFFFF (past content end) — BranchCellAt would panic;
	// ValidateBranch must reject and Check must report, not crash.
	f, _ := os.OpenFile(path, os.O_RDWR, 0o600)
	bad := []byte{0xFF, 0xFF}
	if _, err := f.WriteAt(bad, int64(root)*4096+16); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	f.Close()

	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()

	// Must not panic; must report a structural issue for keyspace "k".
	issues := collectIssues(db2.Check()) // panics here pre-fix
	var sawTreeIssue bool
	for _, iss := range issues {
		if iss.Keyspace == "k" && (iss.Code == "TreeCorrupted" || iss.Code == "KeyspaceWalkFailed") {
			sawTreeIssue = true
		}
	}
	if !sawTreeIssue {
		t.Errorf("no structural issue reported for corrupt keyspace; issues=%v", issuesByCode(issues))
	}
}

// TestCheckFatalIsLast (pins the 11.2 round-2 H-1 fix): when enumeration
// of the top-level keyspace tree fails fatally, the CheckFatal is the
// LAST issue yielded — no spurious accounting warnings follow it.
func TestCheckFatalIsLast(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx, true)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 800 {
		_ = ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "val%05d", i))
	}
	tx.Commit()
	// Capture the top-level keyspace B+tree root.
	meta := db.Meta()
	ksRoot := meta.KeyspaceRoot
	db.Close()

	// Corrupt the keyspace-tree root branch directory.
	f, _ := os.OpenFile(path, os.O_RDWR, 0o600)
	if _, err := f.WriteAt([]byte{0xFF, 0xFF}, int64(ksRoot)*4096+16); err != nil {
		t.Fatalf("corrupt write: %v", err)
	}
	f.Close()

	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()

	issues := collectIssues(db2.Check())
	if len(issues) == 0 {
		t.Fatal("expected at least one issue on a corrupt keyspace root")
	}
	// A CheckFatal must appear, and it must be the last issue.
	var fatalIdx = -1
	for i, iss := range issues {
		if iss.Severity == CheckFatal {
			fatalIdx = i
		}
	}
	if fatalIdx == -1 {
		t.Fatalf("no CheckFatal yielded; issues=%v", issuesByCode(issues))
	}
	if fatalIdx != len(issues)-1 {
		t.Errorf("CheckFatal at index %d, not last (%d issues): %v", fatalIdx, len(issues), issuesByCode(issues))
	}
}
