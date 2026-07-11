package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/pager"
)

// firstByteDecl indexes rows on the first byte of their value — a small
// reusable IndexDecl for the copy tests.
func firstByteDecl() *IndexDecl {
	return &IndexDecl{
		Name:    "by_first",
		Columns: []IndexColumn{{Name: "first"}},
		Extract: func(_, value []byte) []IndexEntry {
			if len(value) == 0 {
				return nil
			}
			return []IndexEntry{{Cols: [][]byte{{value[0]}}}}
		},
	}
}

// TestCopyToVerbatimRoundTrip: a verbatim copy of a populated database
// (indexed keyspace + plain keyspace + set keyspace with nested-tree
// promotion) reopens clean, every row/member survives, and CheckIndexes
// reports no drift — the index trees were copied faithfully.
func TestCopyToVerbatimRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := tmpPath(t)
	dst := tmpPath(t)
	db, err := Open(ctx, src, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const nItems, nPlain, nSet = 800, 100, 200
	tx, _ := db.Begin(ctx)
	ks, err := tx.CreateKeyspace("items", firstByteDecl())
	if err != nil {
		t.Fatalf("CreateKeyspace items: %v", err)
	}
	for i := range nItems {
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "%c-val-%05d", byte('a'+i%26), i)); err != nil {
			t.Fatalf("Put items %d: %v", i, err)
		}
	}
	plain, _ := tx.CreateKeyspace("plain")
	for i := range nPlain {
		_ = plain.Put(fmt.Appendf(nil, "p%05d", i), []byte("v"))
	}
	set, _ := tx.CreateSetKeyspace("set", nil)
	for i := range nSet {
		if _, err := set.Put(fmt.Appendf(nil, "s%03d", i%10), fmt.Appendf(nil, "member%05d", i)); err != nil {
			t.Fatalf("set Put: %v", err)
		}
	}
	// Force a nested-tree promotion: one set key with enough members to
	// blow past the ~2 KB subpage threshold (exercises collectReachable's
	// NestedRoot recursion — the load-bearing reason copy uses btree.Walk).
	const nHuge = 400
	for i := range nHuge {
		if _, err := set.Put([]byte("huge"), fmt.Appendf(nil, "m%05d", i)); err != nil {
			t.Fatalf("set huge Put: %v", err)
		}
	}
	// Force overflow chains: values larger than a leaf can inline
	// (exercises the overflow-run recursion in the copy walk).
	bigVal := bytes.Repeat([]byte("X"), 6000)
	big, _ := tx.CreateKeyspace("big")
	const nBig = 5
	for i := range nBig {
		if err := big.Put(fmt.Appendf(nil, "big%02d", i), bigVal); err != nil {
			t.Fatalf("big Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := db.CopyTo(dst, false); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close source: %v", err)
	}

	cp, err := Open(ctx, dst, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open copy: %v", err)
	}
	defer cp.Close()

	// Structural Check + CheckIndexes are clean on the copy.
	for _, iss := range collectIssues(cp.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"items": {firstByteDecl()}},
	})) {
		if iss.Severity != CheckWarning {
			t.Errorf("copy Check error: code=%s ks=%s idx=%s page=%d msg=%s", iss.Code, iss.Keyspace, iss.Index, iss.PageID, iss.Message)
		}
		if iss.Code == "BitmapLeak" || iss.Code == "CheckIndexes.FingerprintDrift" {
			t.Errorf("copy Check unexpected: code=%s page=%d idx=%s", iss.Code, iss.PageID, iss.Index)
		}
	}

	// Every row + member round-trips.
	rtx, _ := cp.Begin(ctx)
	defer rtx.Rollback()
	rks, err := rtx.OpenKeyspace("items", firstByteDecl())
	if err != nil {
		t.Fatalf("copy OpenKeyspace items: %v", err)
	}
	for i := range nItems {
		got, err := rks.Get(fmt.Appendf(nil, "key%05d", i))
		if err != nil {
			t.Fatalf("copy Get items key%05d: %v", i, err)
		}
		if want := fmt.Sprintf("%c-val-%05d", byte('a'+i%26), i); string(got) != want {
			t.Fatalf("copy items key%05d = %q, want %q", i, got, want)
		}
	}
	rplain, err := rtx.OpenKeyspace("plain")
	if err != nil {
		t.Fatalf("copy OpenKeyspace plain: %v", err)
	}
	for i := range nPlain {
		if _, err := rplain.Get(fmt.Appendf(nil, "p%05d", i)); err != nil {
			t.Fatalf("copy Get plain p%05d: %v", i, err)
		}
	}
	rset, err := rtx.OpenSetKeyspace("set")
	if err != nil {
		t.Fatalf("copy OpenSetKeyspace set: %v", err)
	}
	for i := range nSet {
		ok, err := rset.HasValue(fmt.Appendf(nil, "s%03d", i%10), fmt.Appendf(nil, "member%05d", i))
		if err != nil {
			t.Fatalf("copy set Has: %v", err)
		}
		if !ok {
			t.Fatalf("copy set missing member%05d under s%03d", i, i%10)
		}
	}
	// Nested-tree key survived verbatim (count + membership).
	if cnt, err := rset.CountValues([]byte("huge")); err != nil || cnt != nHuge {
		t.Fatalf("copy set CountValues(huge) = %d, %v; want %d", cnt, err, nHuge)
	}
	for i := range nHuge {
		ok, err := rset.HasValue([]byte("huge"), fmt.Appendf(nil, "m%05d", i))
		if err != nil || !ok {
			t.Fatalf("copy set missing huge member m%05d (ok=%v err=%v)", i, ok, err)
		}
	}
	// Overflow values survived verbatim.
	rbig, err := rtx.OpenKeyspace("big")
	if err != nil {
		t.Fatalf("copy OpenKeyspace big: %v", err)
	}
	for i := range nBig {
		got, err := rbig.Get(fmt.Appendf(nil, "big%02d", i))
		if err != nil {
			t.Fatalf("copy Get big%02d: %v", i, err)
		}
		if !bytes.Equal(got, bigVal) {
			t.Fatalf("copy big%02d value mismatch (len %d, want %d)", i, len(got), len(bigVal))
		}
	}
}

// TestCopyToVerbatimDropsLeak: a leaked page in the source is NOT carried
// into the copy — the rebuilt bitmap marks it free, so the copy's Check
// reports no BitmapLeak.
func TestCopyToVerbatimDropsLeak(t *testing.T) {
	ctx := context.Background()
	dst := tmpPath(t)
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	leaked := buildKeyspaceWithLeak(t, db, 50)

	// Source has the leak.
	var srcLeak bool
	for _, iss := range collectIssues(db.Check()) {
		if iss.Code == "BitmapLeak" && iss.PageID == leaked {
			srcLeak = true
		}
	}
	if !srcLeak {
		t.Fatal("source should report the leak")
	}

	if err := db.CopyTo(dst, false); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	cp, err := Open(ctx, dst, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open copy: %v", err)
	}
	defer cp.Close()
	for _, iss := range collectIssues(cp.Check()) {
		if iss.Code == "BitmapLeak" {
			t.Errorf("copy carried a BitmapLeak (page %d); bitmap rebuild should have dropped it", iss.PageID)
		}
		if iss.Severity != CheckWarning {
			t.Errorf("copy Check error: code=%s msg=%s", iss.Code, iss.Message)
		}
	}
	// And the keyspace data still round-trips.
	rtx, _ := cp.Begin(ctx)
	defer rtx.Rollback()
	ks, _ := rtx.OpenKeyspace("k")
	for i := range 50 {
		if _, err := ks.Get(fmt.Appendf(nil, "key%05d", i)); err != nil {
			t.Fatalf("copy Get key%05d: %v", i, err)
		}
	}
}

// TestCopyToFreshUUID: a copy is a distinct database identity.
func TestCopyToFreshUUID(t *testing.T) {
	ctx := context.Background()
	dst := tmpPath(t)
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	_ = ks.Put([]byte("a"), []byte("b"))
	tx.Commit()
	srcUUID := db.currentMeta.UUID

	if err := db.CopyTo(dst, false); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	db.Close()

	cp, err := Open(ctx, dst, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open copy: %v", err)
	}
	defer cp.Close()
	if cp.currentMeta.UUID == srcUUID {
		t.Errorf("copy UUID equals source UUID %x; expected a fresh identity", srcUUID)
	}
}

// TestCopyToTargetExists: CopyTo refuses to clobber an existing path.
func TestCopyToTargetExists(t *testing.T) {
	ctx := context.Background()
	dst := tmpPath(t)
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	_ = ks.Put([]byte("a"), []byte("b"))
	tx.Commit()

	// Pre-create the destination.
	if err := db.CopyTo(dst, false); err != nil {
		t.Fatalf("first CopyTo: %v", err)
	}
	before, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if err := db.CopyTo(dst, false); err == nil {
		t.Errorf("CopyTo to an existing path should fail")
	}
	// The pre-existing file must be untouched (not clobbered or removed).
	after, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("dst removed/clobbered by failed CopyTo: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("failed CopyTo modified the pre-existing destination")
	}
}

// TestCopyToEmptyDatabase: a copy of a DB with no keyspaces opens clean.
func TestCopyToEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	dst := tmpPath(t)
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.CopyTo(dst, false); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	db.Close()
	cp, err := Open(ctx, dst, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open copy: %v", err)
	}
	defer cp.Close()
	for _, iss := range collectIssues(cp.Check()) {
		t.Errorf("empty-DB copy Check issue: code=%s sev=%d msg=%s", iss.Code, iss.Severity, iss.Message)
	}
	vtx, _ := cp.Begin(ctx)
	if names, _ := vtx.ListKeyspaces(); len(names) != 0 {
		t.Errorf("empty-DB copy has keyspaces: %v", names)
	}
	vtx.Rollback()
}

// TestCopyToPageChecksum: a verbatim copy preserves each page's xxhash
// footer (copied to the same id), so a PageChecksum-on copy verifies
// clean — no BadPageChecksum.
func TestCopyToPageChecksum(t *testing.T) {
	ctx := context.Background()
	dst := tmpPath(t)
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 400 { // multi-level tree
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "val%05d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := db.CopyTo(dst, false); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	db.Close()

	cp, err := Open(ctx, dst, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open copy: %v", err)
	}
	defer cp.Close()
	for _, iss := range collectIssues(cp.Check()) {
		if iss.Code == "BadPageChecksum" {
			t.Errorf("copy reported BadPageChecksum at page %d", iss.PageID)
		}
		if iss.Severity != CheckWarning {
			t.Errorf("checksum copy Check error: code=%s msg=%s", iss.Code, iss.Message)
		}
	}
	rtx, _ := cp.Begin(ctx)
	defer rtx.Rollback()
	rks, _ := rtx.OpenKeyspace("k")
	for i := range 400 {
		if _, err := rks.Get(fmt.Appendf(nil, "key%05d", i)); err != nil {
			t.Fatalf("copy checksum Get key%05d: %v", i, err)
		}
	}
}

// TestCopyToConcurrentWriter (the "writers not blocked" guarantee): a
// writer churning a separate keyspace (allocating then freeing pages in
// the live data region) commits repeatedly while CopyTo runs. The
// snapshot's reader slot pins its pages against reuse, so the copy of the
// stable baseline keyspace must round-trip and Check clean regardless of
// the concurrent commits. Run under -race.
func TestCopyToConcurrentWriter(t *testing.T) {
	ctx := context.Background()
	dst := tmpPath(t)
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	const nBase = 600
	tx, _ := db.Begin(ctx)
	base, _ := tx.CreateKeyspace("base")
	for i := range nBase {
		if err := base.Put(fmt.Appendf(nil, "base%05d", i), fmt.Appendf(nil, "v%05d", i)); err != nil {
			t.Fatalf("base Put: %v", err)
		}
	}
	if _, err := tx.CreateKeyspace("churn"); err != nil {
		t.Fatalf("CreateKeyspace churn: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var stop atomic.Bool
	churnVal := bytes.Repeat([]byte("Y"), 3000) // big enough to alloc several pages
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; !stop.Load(); i++ {
			wtx, err := db.Begin(ctx)
			if err != nil {
				return
			}
			ks, err := wtx.OpenKeyspace("churn")
			if err != nil {
				wtx.Rollback()
				return
			}
			key := fmt.Appendf(nil, "c%08d", i)
			_ = ks.Put(key, churnVal) // allocate pages
			_ = ks.Delete(key)        // free them (RPL-retired; pinned by the copy's reader)
			if err := wtx.Commit(); err != nil {
				return
			}
		}
	}()

	// Run several copies so the churn reliably overlaps a copy.
	for r := range 3 {
		rdst := fmt.Sprintf("%s.%d", dst, r)
		if err := db.CopyTo(rdst, false); err != nil {
			stop.Store(true)
			<-done
			t.Fatalf("CopyTo round %d: %v", r, err)
		}
	}
	stop.Store(true)
	<-done

	// Verify each copy: baseline intact + structurally clean.
	for r := range 3 {
		rdst := fmt.Sprintf("%s.%d", dst, r)
		cp, err := Open(ctx, rdst, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
		if err != nil {
			t.Fatalf("Open copy %d: %v", r, err)
		}
		for _, iss := range collectIssues(cp.Check()) {
			if iss.Severity != CheckWarning {
				t.Errorf("concurrent copy %d Check error: code=%s msg=%s", r, iss.Code, iss.Message)
			}
		}
		rtx, _ := cp.Begin(ctx)
		rks, err := rtx.OpenKeyspace("base")
		if err != nil {
			t.Fatalf("copy %d OpenKeyspace base: %v", r, err)
		}
		for i := range nBase {
			got, err := rks.Get(fmt.Appendf(nil, "base%05d", i))
			if err != nil {
				t.Fatalf("copy %d Get base%05d: %v", r, i, err)
			}
			if want := fmt.Sprintf("v%05d", i); string(got) != want {
				t.Fatalf("copy %d base%05d = %q, want %q", r, i, got, want)
			}
		}
		rtx.Rollback()
		cp.Close()
	}
}

// metaOf opens path read-only-ish (a write tx, rolled back) and returns its
// active meta — used to compare HighWaterMark / NumFreePages across copies.
func metaOf(t *testing.T, path string) pager.Meta {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open %q: %v", path, err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	return tx.prevMeta
}

// TestCopyToCompactRoundTrip: a compacting copy of a populated DB (indexed
// keyspace + set keyspace with nested-tree promotion + overflow values)
// reopens clean, every row/member/value survives, CheckIndexes reports no
// drift (index trees rebuilt structurally), and the copy has zero free
// pages.
func TestCopyToCompactRoundTrip(t *testing.T) {
	ctx := context.Background()
	dst := tmpPath(t)
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const nItems, nHuge, nBig = 800, 400, 5
	bigVal := bytes.Repeat([]byte("Z"), 6000)
	tx, _ := db.Begin(ctx)
	ks, err := tx.CreateKeyspace("items", firstByteDecl())
	if err != nil {
		t.Fatalf("CreateKeyspace items: %v", err)
	}
	for i := range nItems {
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "%c-val-%05d", byte('a'+i%26), i)); err != nil {
			t.Fatalf("Put items %d: %v", i, err)
		}
	}
	big, _ := tx.CreateKeyspace("big")
	for i := range nBig {
		if err := big.Put(fmt.Appendf(nil, "big%02d", i), bigVal); err != nil {
			t.Fatalf("big Put: %v", err)
		}
	}
	set, _ := tx.CreateSetKeyspace("set", nil)
	for i := range nHuge {
		if _, err := set.Put([]byte("huge"), fmt.Appendf(nil, "m%05d", i)); err != nil {
			t.Fatalf("set huge Put: %v", err)
		}
	}
	for i := range 50 { // a few small-set keys (subpage path)
		if _, err := set.Put(fmt.Appendf(nil, "s%02d", i%5), fmt.Appendf(nil, "v%05d", i)); err != nil {
			t.Fatalf("set small Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := db.CopyTo(dst, true); err != nil {
		t.Fatalf("CopyTo(compact): %v", err)
	}
	db.Close()

	cp, err := Open(ctx, dst, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open compact copy: %v", err)
	}
	defer cp.Close()
	for _, iss := range collectIssues(cp.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"items": {firstByteDecl()}},
	})) {
		if iss.Severity != CheckWarning {
			t.Errorf("compact copy Check error: code=%s ks=%s idx=%s page=%d msg=%s", iss.Code, iss.Keyspace, iss.Index, iss.PageID, iss.Message)
		}
		if iss.Code == "BitmapLeak" || iss.Code == "CheckIndexes.FingerprintDrift" {
			t.Errorf("compact copy Check unexpected: code=%s page=%d idx=%s", iss.Code, iss.PageID, iss.Index)
		}
	}

	rtx, _ := cp.Begin(ctx)
	defer rtx.Rollback()
	rks, err := rtx.OpenKeyspace("items", firstByteDecl())
	if err != nil {
		t.Fatalf("compact OpenKeyspace items: %v", err)
	}
	for i := range nItems {
		got, err := rks.Get(fmt.Appendf(nil, "key%05d", i))
		if err != nil {
			t.Fatalf("compact Get items key%05d: %v", i, err)
		}
		if want := fmt.Sprintf("%c-val-%05d", byte('a'+i%26), i); string(got) != want {
			t.Fatalf("compact items key%05d = %q, want %q", i, got, want)
		}
	}
	rbig, _ := rtx.OpenKeyspace("big")
	for i := range nBig {
		got, err := rbig.Get(fmt.Appendf(nil, "big%02d", i))
		if err != nil || !bytes.Equal(got, bigVal) {
			t.Fatalf("compact big%02d mismatch (err=%v len=%d)", i, err, len(got))
		}
	}
	rset, _ := rtx.OpenSetKeyspace("set")
	if cnt, err := rset.CountValues([]byte("huge")); err != nil || cnt != nHuge {
		t.Fatalf("compact CountValues(huge) = %d, %v; want %d", cnt, err, nHuge)
	}
	for i := range nHuge {
		ok, err := rset.HasValue([]byte("huge"), fmt.Appendf(nil, "m%05d", i))
		if err != nil || !ok {
			t.Fatalf("compact set missing huge m%05d (ok=%v err=%v)", i, ok, err)
		}
	}

	// A compacted copy has no free pages (read from the open copy's tx;
	// re-Opening the same path here would deadlock on its lock file).
	if nf := rtx.prevMeta.NumFreePages; nf != 0 {
		t.Errorf("compact copy NumFreePages = %d, want 0", nf)
	}
}

// TestCopyToCompactEmpty: a compacting copy of a DB with no keyspaces
// opens clean and empty.
func TestCopyToCompactEmpty(t *testing.T) {
	ctx := context.Background()
	dst := tmpPath(t)
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.CopyTo(dst, true); err != nil {
		t.Fatalf("CopyTo(compact): %v", err)
	}
	db.Close()
	cp, err := Open(ctx, dst, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open compact copy: %v", err)
	}
	defer cp.Close()
	for _, iss := range collectIssues(cp.Check()) {
		t.Errorf("empty compact copy Check issue: code=%s sev=%d msg=%s", iss.Code, iss.Severity, iss.Message)
	}
	vtx, _ := cp.Begin(ctx)
	if names, _ := vtx.ListKeyspaces(); len(names) != 0 {
		t.Errorf("empty compact copy has keyspaces: %v", names)
	}
	vtx.Rollback()
}

// TestCopyToCompactPageChecksum: a compacting copy with PageChecksum on
// stamps fresh footers on every rebuilt page, so the copy verifies clean.
func TestCopyToCompactPageChecksum(t *testing.T) {
	ctx := context.Background()
	dst := tmpPath(t)
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 400 {
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "val%05d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := db.CopyTo(dst, true); err != nil {
		t.Fatalf("CopyTo(compact): %v", err)
	}
	db.Close()
	cp, err := Open(ctx, dst, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open compact copy: %v", err)
	}
	defer cp.Close()
	for _, iss := range collectIssues(cp.Check()) {
		if iss.Code == "BadPageChecksum" {
			t.Errorf("compact copy BadPageChecksum at page %d", iss.PageID)
		}
		if iss.Severity != CheckWarning {
			t.Errorf("compact checksum copy Check error: code=%s msg=%s", iss.Code, iss.Message)
		}
	}
	rtx, _ := cp.Begin(ctx)
	defer rtx.Rollback()
	rks, _ := rtx.OpenKeyspace("k")
	for i := range 400 {
		if _, err := rks.Get(fmt.Appendf(nil, "key%05d", i)); err != nil {
			t.Fatalf("compact checksum Get key%05d: %v", i, err)
		}
	}
}

// TestCopyToCompactDefragments: a source with many freed pages (churned via
// DeleteRange) yields a compact copy with a strictly smaller HighWaterMark
// than a verbatim copy of the same snapshot — free pages are reclaimed.
func TestCopyToCompactDefragments(t *testing.T) {
	ctx := context.Background()
	verbatimDst := tmpPath(t)
	compactDst := tmpPath(t)
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Build a large keyspace, then delete most of it — leaving many free
	// pages below a high HighWaterMark (fragmentation).
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 3000 {
		if err := ks.Put(fmt.Appendf(nil, "key%06d", i), bytes.Repeat([]byte("v"), 200)); err != nil {
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

	if err := db.CopyTo(verbatimDst, false); err != nil {
		t.Fatalf("CopyTo verbatim: %v", err)
	}
	if err := db.CopyTo(compactDst, true); err != nil {
		t.Fatalf("CopyTo compact: %v", err)
	}

	vHWM := metaOf(t, verbatimDst).HighWaterMark
	cMeta := metaOf(t, compactDst)
	if cMeta.HighWaterMark >= vHWM {
		t.Errorf("compact HighWaterMark %d not smaller than verbatim %d (no defragmentation)", cMeta.HighWaterMark, vHWM)
	}
	if cMeta.NumFreePages != 0 {
		t.Errorf("compact copy NumFreePages = %d, want 0", cMeta.NumFreePages)
	}

	// Surviving keys round-trip in the compact copy.
	cp, err := Open(ctx, compactDst, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open compact: %v", err)
	}
	defer cp.Close()
	for _, iss := range collectIssues(cp.Check()) {
		if iss.Severity != CheckWarning {
			t.Errorf("compact copy Check error: code=%s msg=%s", iss.Code, iss.Message)
		}
	}
	rtx, _ := cp.Begin(ctx)
	defer rtx.Rollback()
	rks, _ := rtx.OpenKeyspace("k")
	for _, i := range []int{0, 50, 99, 2900, 2999} { // boundary survivors
		if _, err := rks.Get(fmt.Appendf(nil, "key%06d", i)); err != nil {
			t.Errorf("compact Get key%06d: %v", i, err)
		}
	}
	if _, err := rks.Get(fmt.Appendf(nil, "key%06d", 1500)); err == nil {
		t.Errorf("deleted key%06d present in compact copy", 1500)
	}
}

// TestCompactAbortsOnBitrotRatherThanLaundering pins checksums.md
// §Verification for the compact rebuild path. CopyTo(compact=true) DECODES
// each source page and re-encodes it into a fresh page with a new footer.
// An unverified read would therefore launder a bitrotted-but-decodable
// source page into a copy carrying a fresh VALID footer — converting a
// detectable ErrBadPageChecksum into a permanent silent wrong value that
// Check() reports clean. The rebuild must instead verify each source
// footer and ABORT on mismatch (the half-built copy is removed by the
// !committed defer), leaving the original detectably corrupt.
func TestCompactAbortsOnBitrotRatherThanLaundering(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	dst := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 800 { // multi-level tree
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "val%05d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	rtx, _ := db.Begin(ctx)
	rks, _ := rtx.OpenKeyspace("k")
	root := rks.desc.Root
	rtx.Rollback()
	if root == 0 {
		t.Fatal("data-tree root is 0")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Flip a byte inside the data-tree root page's footer: the page still
	// decodes structurally, but its xxhash64 footer no longer matches — the
	// exact silent-bitrot class checksums exist to catch.
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
		t.Fatalf("corrupt write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close after corruption: %v", err)
	}

	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	// Precondition: the corruption is detectable on the normal read path.
	rtx2, _ := db2.Begin(ctx)
	rks2, _ := rtx2.OpenKeyspace("k")
	if _, err := rks2.Get([]byte("key00000")); !errors.Is(err, ErrBadPageChecksum) {
		t.Fatalf("Get on bitrotted DB = %v, want ErrBadPageChecksum", err)
	}
	rtx2.Rollback()

	// The fix: compaction must ABORT on the bad footer, not launder it.
	err = db2.CopyTo(dst, true)
	if !errors.Is(err, ErrBadPageChecksum) {
		t.Fatalf("CopyTo(compact) over bitrot = %v, want ErrBadPageChecksum (launder guard)", err)
	}
	// The half-built copy is rolled back — no laundered file survives.
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("compact copy %q exists after aborted compaction (stat err=%v)", dst, statErr)
	}
	// The original is untouched and still detectably corrupt.
	rtx3, _ := db2.Begin(ctx)
	rks3, _ := rtx3.OpenKeyspace("k")
	if _, err := rks3.Get([]byte("key00000")); !errors.Is(err, ErrBadPageChecksum) {
		t.Errorf("original after aborted compaction = %v, want still ErrBadPageChecksum", err)
	}
	rtx3.Rollback()
}
