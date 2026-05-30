package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// secondByteExtract indexes the value's SECOND byte — used to simulate
// extractor drift against the stored index built with firstByteExtract.
func secondByteExtract(_, value []byte) []IndexEntry {
	if len(value) < 2 {
		return nil
	}
	return []IndexEntry{{Cols: [][]byte{{value[1]}}}}
}

// setupIndexedDB creates a committed "items" keyspace with a non-unique
// "by_first" index (firstByteExtract over value[0]) and three rows whose
// first and second bytes are all distinct, so firstByteExtract and
// secondByteExtract produce disjoint index-key sets.
func setupIndexedDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, PageChecksum: true, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	decl := testDecl("by_first", "b0")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	rows := []struct {
		k string
		v []byte
	}{
		{"k1", []byte{0x01, 0x02}},
		{"k2", []byte{0x03, 0x04}},
		{"k3", []byte{0x05, 0x06}},
	}
	for _, r := range rows {
		if err := ks.Put([]byte(r.k), r.v); err != nil {
			t.Fatalf("Put %s: %v", r.k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return db
}

func firstIssueWithCode(issues []CheckIssue, code string) (CheckIssue, bool) {
	for _, iss := range issues {
		if iss.Code == code {
			return iss, true
		}
	}
	return CheckIssue{}, false
}

// byFirstDecl returns a fresh IndexDecl matching the stored "by_first"
// index (firstByteExtract).
func byFirstDecl() *IndexDecl {
	d := testDecl("by_first", "b0")
	d.Extract = firstByteExtract
	return d
}

// TestCheckIndexesCleanPasses: an index matching its supplied extractor
// produces no FingerprintDrift (and no error/fatal).
func TestCheckIndexesCleanPasses(t *testing.T) {
	db := setupIndexedDB(t)
	defer db.Close()
	issues := collectIssues(db.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"items": {byFirstDecl()}},
	}))
	for _, iss := range issues {
		if iss.Severity == CheckError || iss.Severity == CheckFatal {
			t.Errorf("unexpected error/fatal on clean indexed DB: %+v", iss)
		}
	}
	if _, ok := firstIssueWithCode(issues, "CheckIndexes.FingerprintDrift"); ok {
		t.Errorf("clean index reported drift; issues=%v", issuesByCode(issues))
	}
	if _, ok := firstIssueWithCode(issues, "CheckIndexes.KeyspaceNotSupplied"); ok {
		t.Errorf("supplied keyspace reported NotSupplied; issues=%v", issuesByCode(issues))
	}
}

// TestCheckIndexesDetectsDrift: supplying an extractor that disagrees with
// the stored index yields a FingerprintDrift CheckError for that index.
func TestCheckIndexesDetectsDrift(t *testing.T) {
	db := setupIndexedDB(t)
	defer db.Close()
	drifted := testDecl("by_first", "b0")
	drifted.Extract = secondByteExtract // stored index used value[0]
	issues := collectIssues(db.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"items": {drifted}},
	}))
	iss, ok := firstIssueWithCode(issues, "CheckIndexes.FingerprintDrift")
	if !ok {
		t.Fatalf("no FingerprintDrift reported; issues=%v", issuesByCode(issues))
	}
	if iss.Keyspace != "items" || iss.Index != "by_first" {
		t.Errorf("drift ks/idx = %q/%q, want items/by_first", iss.Keyspace, iss.Index)
	}
	if iss.Severity != CheckError {
		t.Errorf("drift severity = %d, want CheckError(%d)", iss.Severity, CheckError)
	}
}

// TestCheckIndexesKeyspaceNotSupplied: an indexed keyspace absent from
// opts.Indexes is a KeyspaceNotSupplied warning (no Indexes map at all).
func TestCheckIndexesKeyspaceNotSupplied(t *testing.T) {
	db := setupIndexedDB(t)
	defer db.Close()
	issues := collectIssues(db.CheckWithOptions(&CheckOptions{CheckIndexes: true}))
	iss, ok := firstIssueWithCode(issues, "CheckIndexes.KeyspaceNotSupplied")
	if !ok || iss.Keyspace != "items" || iss.Severity != CheckWarning {
		t.Fatalf("want KeyspaceNotSupplied warning for items; issues=%v", issuesByCode(issues))
	}
}

// TestCheckIndexesKeyspaceNotFound: a supplied keyspace name absent from
// the database is a KeyspaceNotFound warning.
func TestCheckIndexesKeyspaceNotFound(t *testing.T) {
	db := setupIndexedDB(t)
	defer db.Close()
	issues := collectIssues(db.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"items": {byFirstDecl()}, "ghost": {byFirstDecl()}},
	}))
	iss, ok := firstIssueWithCode(issues, "CheckIndexes.KeyspaceNotFound")
	if !ok || iss.Keyspace != "ghost" || iss.Severity != CheckWarning {
		t.Fatalf("want KeyspaceNotFound warning for ghost; issues=%v", issuesByCode(issues))
	}
}

// TestCheckIndexesIndexNotInRegistry: a supplied IndexDecl whose Name is
// not registered on an existing keyspace is an IndexNotInRegistry warning.
func TestCheckIndexesIndexNotInRegistry(t *testing.T) {
	db := setupIndexedDB(t)
	defer db.Close()
	wrong := testDecl("not_an_index", "b0")
	wrong.Extract = firstByteExtract
	issues := collectIssues(db.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"items": {wrong}},
	}))
	iss, ok := firstIssueWithCode(issues, "CheckIndexes.IndexNotInRegistry")
	if !ok || iss.Index != "not_an_index" || iss.Severity != CheckWarning {
		t.Fatalf("want IndexNotInRegistry warning for not_an_index; issues=%v", issuesByCode(issues))
	}
}

// coverFirstA: key = value[0], covering = value[1:].
func coverFirstA(_, value []byte) []IndexEntry {
	if len(value) == 0 {
		return nil
	}
	return []IndexEntry{{Cols: [][]byte{{value[0]}}, Cover: [][]byte{value[1:]}}}
}

// coverFirstB: same key (value[0]) but DIFFERENT covering bytes — used to
// drift only the covering value, which a key-only comparison would miss.
func coverFirstB(_, value []byte) []IndexEntry {
	if len(value) == 0 {
		return nil
	}
	return []IndexEntry{{Cols: [][]byte{{value[0]}}, Cover: [][]byte{append([]byte{0xFF}, value[1:]...)}}}
}

func coveringDecl(extract IndexExtractor) *IndexDecl {
	d := testDecl("cov", "b0")
	d.Covering = []CoveringColumn{{Name: "rest"}}
	d.Extract = extract
	return d
}

func setupCoveringDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, PageChecksum: true, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("items", coveringDecl(coverFirstA))
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for _, r := range []struct {
		k string
		v []byte
	}{{"k1", []byte{0x01, 0x02, 0x03}}, {"k2", []byte{0x04, 0x05, 0x06}}} {
		if err := ks.Put([]byte(r.k), r.v); err != nil {
			t.Fatalf("Put %s: %v", r.k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return db
}

// TestCheckIndexesCoveringCleanPasses: a covering index verified with its
// own extractor produces no drift — validates that the check reproduces
// the stored (key, value) encoding exactly (no false positive on the
// covering value).
func TestCheckIndexesCoveringCleanPasses(t *testing.T) {
	db := setupCoveringDB(t)
	defer db.Close()
	issues := collectIssues(db.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"items": {coveringDecl(coverFirstA)}},
	}))
	if _, ok := firstIssueWithCode(issues, "CheckIndexes.FingerprintDrift"); ok {
		t.Errorf("clean covering index reported drift; issues=%v", issuesByCode(issues))
	}
}

// TestCheckIndexesDetectsCoveringDrift: a covering-only drift (same index
// keys, different covering bytes) is caught by the full-entry comparison —
// a key-only check would miss it.
func TestCheckIndexesDetectsCoveringDrift(t *testing.T) {
	db := setupCoveringDB(t)
	defer db.Close()
	issues := collectIssues(db.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"items": {coveringDecl(coverFirstB)}},
	}))
	iss, ok := firstIssueWithCode(issues, "CheckIndexes.FingerprintDrift")
	if !ok || iss.Index != "cov" {
		t.Fatalf("want FingerprintDrift for cov (covering-only drift); issues=%v", issuesByCode(issues))
	}
}

// --- SetKeyspace index verification (M-1) -------------------------

// setKeyspaceSecondByteExtract indexes the member's SECOND byte — drift
// against setKeyspaceFirstByteExtract.
func setKeyspaceSecondByteExtract(_, setValue []byte) []IndexEntry {
	if len(setValue) < 2 {
		return nil
	}
	return []IndexEntry{{Cols: [][]byte{{setValue[1]}}}}
}

func setMemberDecl(extract IndexExtractor) *IndexDecl {
	d := testDecl("by_member", "m0")
	d.Extract = extract
	return d
}

// setupIndexedSetKeyspace creates a committed SetKeyspace "subs" indexed by
// the member's first byte, with one key holding two members (a small
// subpage set) and a second key holding one member.
func setupIndexedSetKeyspace(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, PageChecksum: true, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	sks, err := tx.CreateSetKeyspace("subs", nil, setMemberDecl(setKeyspaceFirstByteExtract))
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	for _, p := range [][2]string{{"u1", "alpha"}, {"u1", "beta"}, {"u2", "gamma"}} {
		if _, err := sks.Put([]byte(p[0]), []byte(p[1])); err != nil {
			t.Fatalf("Put(%s,%s): %v", p[0], p[1], err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return db
}

// TestCheckIndexesSetKeyspaceCleanPasses (M-1): an indexed SetKeyspace
// verified with its own extractor produces no drift AND none of the
// corruption-flavored warnings (RowsUnreadable / KeyspaceKindUnsupported)
// the pre-fix key-only path emitted on healthy set data.
func TestCheckIndexesSetKeyspaceCleanPasses(t *testing.T) {
	db := setupIndexedSetKeyspace(t)
	defer db.Close()
	issues := collectIssues(db.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"subs": {setMemberDecl(setKeyspaceFirstByteExtract)}},
	}))
	for _, code := range []string{"CheckIndexes.FingerprintDrift", "CheckIndexes.RowsUnreadable", "CheckIndexes.KeyspaceKindUnsupported"} {
		if _, ok := firstIssueWithCode(issues, code); ok {
			t.Errorf("clean indexed SetKeyspace reported %s; issues=%v", code, issuesByCode(issues))
		}
	}
}

// TestCheckIndexesSetKeyspaceDetectsDrift (M-1): a drifted extractor on an
// indexed SetKeyspace yields FingerprintDrift via the SetKeyspace codec.
func TestCheckIndexesSetKeyspaceDetectsDrift(t *testing.T) {
	db := setupIndexedSetKeyspace(t)
	defer db.Close()
	issues := collectIssues(db.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"subs": {setMemberDecl(setKeyspaceSecondByteExtract)}},
	}))
	if iss, ok := firstIssueWithCode(issues, "CheckIndexes.FingerprintDrift"); !ok || iss.Keyspace != "subs" {
		t.Fatalf("want FingerprintDrift for subs (SetKeyspace drift); issues=%v", issuesByCode(issues))
	}
}

// TestCheckIndexesSetKeyspaceNestedTreeCleanPasses (M-1): a set key with
// enough members to spill into a nested B+tree verifies clean — exercises
// the nested-tree member-enumeration branch (vs the subpage branch).
func TestCheckIndexesSetKeyspaceNestedTreeCleanPasses(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, PageChecksum: true, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	sks, err := tx.CreateSetKeyspace("subs", nil, setMemberDecl(setKeyspaceFirstByteExtract))
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	for i := range 600 { // force "big" into a nested B+tree
		if _, err := sks.Put([]byte("big"), fmt.Appendf(nil, "m%05d", i)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	issues := collectIssues(db.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"subs": {setMemberDecl(setKeyspaceFirstByteExtract)}},
	}))
	for _, code := range []string{"CheckIndexes.FingerprintDrift", "CheckIndexes.RowsUnreadable"} {
		if _, ok := firstIssueWithCode(issues, code); ok {
			t.Errorf("clean nested-tree SetKeyspace reported %s; issues=%v", code, issuesByCode(issues))
		}
	}
}

// --- plain-Keyspace coverage gaps (unique / partial / multi-column) ---

func evenFirstByteExtract(_, value []byte) []IndexEntry {
	if len(value) == 0 || value[0]%2 != 0 { // partial: index only even first bytes
		return nil
	}
	return []IndexEntry{{Cols: [][]byte{{value[0]}}}}
}

func twoColExtract(_, value []byte) []IndexEntry {
	if len(value) < 2 {
		return nil
	}
	return []IndexEntry{{Cols: [][]byte{{value[0]}, {value[1]}}}}
}

// makeIndexedKeyspace creates+commits "items" indexed by decl over rows.
func makeIndexedKeyspace(t *testing.T, decl *IndexDecl, rows [][2]string) *DB {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, PageChecksum: true, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for _, r := range rows {
		if err := ks.Put([]byte(r[0]), []byte(r[1])); err != nil {
			t.Fatalf("Put %s: %v", r[0], err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return db
}

func assertNoDrift(t *testing.T, db *DB, name string, verify *IndexDecl) {
	t.Helper()
	issues := collectIssues(db.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"items": {verify}},
	}))
	for _, code := range []string{"CheckIndexes.FingerprintDrift", "CheckIndexes.RowsUnreadable", "CheckIndexes.IndexUnreadable", "CheckIndexes.ExtractorError"} {
		if _, ok := firstIssueWithCode(issues, code); ok {
			t.Errorf("%s: clean index reported %s; issues=%v", name, code, issuesByCode(issues))
		}
	}
}

// TestCheckIndexesUniqueCleanPasses pins the unique-index value encoding
// (uvarint(len(pk))||pk||covering) — a divergence there would false-drift
// every unique index.
func TestCheckIndexesUniqueCleanPasses(t *testing.T) {
	decl := testDecl("u", "b0")
	decl.Unique = true
	decl.Extract = firstByteExtract
	db := makeIndexedKeyspace(t, decl, [][2]string{{"k1", "\x01x"}, {"k2", "\x03y"}, {"k3", "\x05z"}})
	defer db.Close()
	v := testDecl("u", "b0")
	v.Unique = true
	v.Extract = firstByteExtract
	assertNoDrift(t, db, "unique", v)
}

// TestCheckIndexesPartialCleanPasses: rows the extractor skips (returns
// nil) are absent from both expected and stored — no drift.
func TestCheckIndexesPartialCleanPasses(t *testing.T) {
	decl := testDecl("p", "b0")
	decl.Extract = evenFirstByteExtract
	db := makeIndexedKeyspace(t, decl, [][2]string{{"k1", "\x02a"}, {"k2", "\x03b"}, {"k3", "\x04c"}})
	defer db.Close()
	v := testDecl("p", "b0")
	v.Extract = evenFirstByteExtract
	assertNoDrift(t, db, "partial", v)
}

// TestCheckIndexesMultiColumnCleanPasses pins the multi-column key
// encoding.
func TestCheckIndexesMultiColumnCleanPasses(t *testing.T) {
	decl := testDecl("two", "a", "b")
	decl.Extract = twoColExtract
	db := makeIndexedKeyspace(t, decl, [][2]string{{"k1", "\x01\x02"}, {"k2", "\x03\x04"}})
	defer db.Close()
	v := testDecl("two", "a", "b")
	v.Extract = twoColExtract
	assertNoDrift(t, db, "multicol", v)
}

// TestCheckIndexesSetKeyspaceForgedSubpageNoPanic (H-1 regression): a
// forged SetKeyspace subpage (bad internal Count) must surface as a
// CheckIndexes.RowsUnreadable warning, never a panic — the SetKeyspace
// index pass validates the raw subpage before decoding it, upholding the
// chunk-11 "Check never panics on a forged page" contract.
func TestCheckIndexesSetKeyspaceForgedSubpageNoPanic(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, PageChecksum: true, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	sks, err := tx.CreateSetKeyspace("subs", nil, setMemberDecl(setKeyspaceFirstByteExtract))
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	for _, m := range []string{"alpha", "beta", "gamma"} {
		if _, err := sks.Put([]byte("u1"), []byte(m)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	rtx, _ := db.Begin(ctx)
	rsks, err := rtx.OpenSetKeyspace("subs", setMemberDecl(setKeyspaceFirstByteExtract))
	if err != nil {
		t.Fatalf("OpenSetKeyspace: %v", err)
	}
	root := rsks.desc.Root
	rtx.Rollback()
	if root == 0 {
		t.Fatal("set data-tree root is 0")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Locate the "u1" subpage value in the data-tree root leaf and forge
	// its internal Count to 0xFFFF — the leaf stays structurally valid
	// (only value bytes change), but the subpage would over-read on decode
	// without the guard.
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	pageBuf := make([]byte, 4096)
	if _, err := f.ReadAt(pageBuf, int64(root)*4096); err != nil {
		t.Fatalf("read root leaf: %v", err)
	}
	cfg := page.Config{PageSize: 4096, PageChecksum: true}
	it := page.NewLeafReader(pageBuf, cfg).IterForReuse(nil, nil, nil)
	e, ok := it.Next()
	if !ok || !e.IsSubpage() {
		t.Fatalf("first data-tree entry is not a subpage (ok=%v, flags=0x%x)", ok, e.Flags)
	}
	off := bytes.Index(pageBuf, e.Value) // subpage Count is the first 2 bytes
	if off < 0 {
		t.Fatal("could not locate subpage value in the leaf page")
	}
	if _, err := f.WriteAt([]byte{0xFF, 0xFF}, int64(root)*4096+int64(off)); err != nil {
		t.Fatalf("forge write: %v", err)
	}
	f.Close()

	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	// Must not panic (a panic crashes this range); the forged subpage is
	// reported as RowsUnreadable.
	issues := collectIssues(db2.CheckWithOptions(&CheckOptions{
		CheckIndexes: true,
		Indexes:      map[string][]*IndexDecl{"subs": {setMemberDecl(setKeyspaceFirstByteExtract)}},
	}))
	if _, ok := firstIssueWithCode(issues, "CheckIndexes.RowsUnreadable"); !ok {
		t.Fatalf("want RowsUnreadable for forged subpage; issues=%v", issuesByCode(issues))
	}
}
