package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/greatliontech/gmdb/internal/extsort"
)

// --- shared indexed-BulkLoad test helpers -------------------------

// wholeValueExtract emits one IndexEntry whose single column is the whole
// value (distinct per row when values are distinct — usable for a unique
// index).
func wholeValueExtract(_, value []byte) []IndexEntry {
	if len(value) == 0 {
		return nil
	}
	return []IndexEntry{{Cols: [][]byte{value}}}
}

// dupColExtract returns TWO entries with the same column for any row — a
// candidate-set collision for a unique index.
func dupColExtract(_, _ []byte) []IndexEntry {
	return []IndexEntry{{Cols: [][]byte{{0x01}}}, {Cols: [][]byte{{0x01}}}}
}

// collectAllIndexPairs walks the whole index via Range(nil,nil) and returns
// sorted "pk=value" strings — a canonical snapshot of the index's
// (pk, rowValue) contents for cross-checking BulkLoad vs Put.
func collectAllIndexPairs(t *testing.T, idx *IndexHandle) []string {
	t.Helper()
	var out []string
	for pk, v := range idx.Range(nil, nil) {
		out = append(out, string(pk)+"="+string(v))
	}
	if err := idx.Err(); err != nil {
		t.Fatalf("idx.Err: %v", err)
	}
	sort.Strings(out)
	return out
}

// lookupKeysSorted returns the sorted PK list for a non-unique exact match.
func lookupKeysSorted(t *testing.T, idx *IndexHandle, cols ...[]byte) []string {
	t.Helper()
	var out []string
	for pk := range idx.LookupKeys(cols) {
		out = append(out, string(pk))
	}
	if err := idx.Err(); err != nil {
		t.Fatalf("idx.Err: %v", err)
	}
	sort.Strings(out)
	return out
}

func countScratchRuns(t *testing.T, dir string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "gmdb-bulkidx-*.run"))
	if err != nil {
		t.Fatalf("glob scratch dir: %v", err)
	}
	return len(matches)
}

// --- Keyspace indexed BulkLoad ------------------------------------

// TestKeyspaceBulkLoadIndexedRoundTrip bulk-loads an indexed keyspace with
// one non-unique and one unique index, then verifies index lookups and
// counts resolve correctly through the bulk-built index trees.
func TestKeyspaceBulkLoadIndexedRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	byFirst := testDecl("by_first", "first")
	byFirst.Extract = firstByteExtract
	byValue := testDecl("by_value", "value")
	byValue.Unique = true
	byValue.Extract = wholeValueExtract

	ks, err := tx.CreateKeyspace("items", byFirst, byValue)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// Pre-state: a freshly-created indexed keyspace has every index data
	// root at 0, so the defensive retire-after-publish step's FreeSubtree(0)
	// is a no-op and cannot error (nit-2: the retire error path is
	// structurally unreachable for a BulkLoad-eligible empty keyspace).
	for _, n := range []string{"by_first", "by_value"} {
		if p := ks.indexes[n]; p.root != 0 {
			t.Fatalf("pre-BulkLoad pinned index %q root=%d, want 0", n, p.root)
		}
	}
	rows := []kv{
		{[]byte("k0"), []byte("apple")},
		{[]byte("k1"), []byte("apricot")},
		{[]byte("k2"), []byte("banana")},
		{[]byte("k3"), []byte("blueberry")},
		{[]byte("k4"), []byte("cherry")},
		{[]byte("k5"), []byte("citrus")},
	}
	n, err := ks.BulkLoad(seqOf(rows))
	if err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	if n != 6 {
		t.Errorf("BulkLoad returned %d, want 6", n)
	}

	idxFirst, err := ks.Index("by_first")
	if err != nil {
		t.Fatalf("Index(by_first): %v", err)
	}
	if got := lookupKeysSorted(t, idxFirst, []byte("a")); len(got) != 2 || got[0] != "k0" || got[1] != "k1" {
		t.Errorf("by_first['a'] = %v, want [k0 k1]", got)
	}
	if got := lookupKeysSorted(t, idxFirst, []byte("b")); len(got) != 2 || got[0] != "k2" || got[1] != "k3" {
		t.Errorf("by_first['b'] = %v, want [k2 k3]", got)
	}
	if st, err := idxFirst.Stats(); err != nil || st.Entries != 6 {
		t.Errorf("by_first Stats = %+v err=%v, want Count 6", st, err)
	}

	idxValue, err := ks.Index("by_value")
	if err != nil {
		t.Fatalf("Index(by_value): %v", err)
	}
	pk, val, err := idxValue.Get([]byte("banana"))
	if err != nil {
		t.Fatalf("by_value.Get(banana): %v", err)
	}
	if string(pk) != "k2" || string(val) != "banana" {
		t.Errorf("by_value.Get(banana) = (%s, %s), want (k2, banana)", pk, val)
	}
	if st, err := idxValue.Stats(); err != nil || st.Entries != 6 {
		t.Errorf("by_value Stats = %+v err=%v, want Count 6", st, err)
	}
}

// TestKeyspaceBulkLoadIndexedMatchesPut builds the same indexed dataset two
// ways — per-row Put (top-down) and BulkLoad (bottom-up) — and verifies the
// index contents are byte-identical (Inv-IdxBulk-2).
func TestKeyspaceBulkLoadIndexedMatchesPut(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	// 400 rows; values cycle 26 first-bytes (non-unique multiplicity) but
	// are globally distinct (suffix), so the unique index is satisfiable.
	rows := make([]kv, 400)
	for i := range rows {
		rows[i] = kv{
			k: fmt.Appendf(nil, "row%05d", i),
			v: fmt.Appendf(nil, "%c-val-%05d", byte('a'+i%26), i),
		}
	}

	mkDecls := func() []*IndexDecl {
		byFirst := testDecl("by_first", "first")
		byFirst.Extract = firstByteExtract
		byValue := testDecl("by_value", "value")
		byValue.Unique = true
		byValue.Extract = wholeValueExtract
		return []*IndexDecl{byFirst, byValue}
	}

	ksPut, err := tx.CreateKeyspace("viaput", mkDecls()...)
	if err != nil {
		t.Fatalf("CreateKeyspace viaput: %v", err)
	}
	for _, r := range rows {
		if err := ksPut.Put(r.k, r.v); err != nil {
			t.Fatalf("Put(%s): %v", r.k, err)
		}
	}

	ksBulk, err := tx.CreateKeyspace("viabulk", mkDecls()...)
	if err != nil {
		t.Fatalf("CreateKeyspace viabulk: %v", err)
	}
	if _, err := ksBulk.BulkLoad(seqOf(rows)); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}

	for _, name := range []string{"by_first", "by_value"} {
		ip, err := ksPut.Index(name)
		if err != nil {
			t.Fatalf("viaput Index(%s): %v", name, err)
		}
		ib, err := ksBulk.Index(name)
		if err != nil {
			t.Fatalf("viabulk Index(%s): %v", name, err)
		}
		putPairs := collectAllIndexPairs(t, ip)
		bulkPairs := collectAllIndexPairs(t, ib)
		if len(putPairs) != len(rows) {
			t.Errorf("index %q: Put produced %d pairs, want %d", name, len(putPairs), len(rows))
		}
		if !slicesEqualStr(putPairs, bulkPairs) {
			t.Errorf("index %q: BulkLoad pairs differ from Put pairs\n put=%d\n bulk=%d", name, len(putPairs), len(bulkPairs))
		}
		sp, _ := ip.Stats()
		sb, _ := ib.Stats()
		if sp.Entries != sb.Entries {
			t.Errorf("index %q: Put Count=%d, BulkLoad Count=%d", name, sp.Entries, sb.Entries)
		}
	}
}

// TestKeyspaceBulkLoadIndexedUniqueViolationCrossRow verifies two rows
// producing the same unique-index column abort BulkLoad with
// ErrIndexUniqueViolation and publish nothing (Inv-IdxBulk-1/3).
func TestKeyspaceBulkLoadIndexedUniqueViolationCrossRow(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	uniq := testDecl("by_first", "first")
	uniq.Unique = true
	uniq.Extract = firstByteExtract // both rows below share first byte 'a'

	ks, err := tx.CreateKeyspace("items", uniq)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	rows := []kv{{[]byte("k0"), []byte("apple")}, {[]byte("k1"), []byte("avocado")}}
	_, err = ks.BulkLoad(seqOf(rows))
	if !errors.Is(err, ErrIndexUniqueViolation) {
		t.Fatalf("BulkLoad = %v, want ErrIndexUniqueViolation", err)
	}
	if ks.desc.Count != 0 || ks.desc.Root != 0 {
		t.Errorf("after violation: Count=%d Root=%d, want both 0 (nothing published)", ks.desc.Count, ks.desc.Root)
	}
	if p := ks.indexes["by_first"]; p.root != 0 || p.count != 0 {
		t.Errorf("after violation: pinned index root=%d count=%d, want 0/0", p.root, p.count)
	}
}

// TestKeyspaceBulkLoadIndexedUniqueViolationCandidateSet verifies a single
// row whose extractor returns two same-column entries aborts a unique-index
// BulkLoad (the candidate-set collision is caught during the stream).
func TestKeyspaceBulkLoadIndexedUniqueViolationCandidateSet(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	uniq := testDecl("by_const", "const")
	uniq.Unique = true
	uniq.Extract = dupColExtract

	ks, err := tx.CreateKeyspace("items", uniq)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	_, err = ks.BulkLoad(seqOf([]kv{{[]byte("k0"), []byte("v")}}))
	if !errors.Is(err, ErrIndexUniqueViolation) {
		t.Fatalf("BulkLoad = %v, want ErrIndexUniqueViolation", err)
	}
	if ks.desc.Count != 0 || ks.desc.Root != 0 {
		t.Errorf("after candidate-set violation: Count=%d Root=%d, want both 0", ks.desc.Count, ks.desc.Root)
	}
}

// TestKeyspaceBulkLoadIndexedPartialIndex verifies an extractor that emits
// nothing for some rows indexes only the rows it does emit for.
func TestKeyspaceBulkLoadIndexedPartialIndex(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	// firstByteExtract emits nothing for empty values; index only non-empty.
	decl := testDecl("by_first", "first")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	rows := []kv{
		{[]byte("k0"), []byte("")},     // not indexed
		{[]byte("k1"), []byte("xray")}, // indexed under 'x'
		{[]byte("k2"), []byte("")},     // not indexed
	}
	if _, err := ks.BulkLoad(seqOf(rows)); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	idx, err := ks.Index("by_first")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if st, _ := idx.Stats(); st.Entries != 1 {
		t.Errorf("partial index Count=%d, want 1", st.Entries)
	}
	if got := lookupKeysSorted(t, idx, []byte("x")); len(got) != 1 || got[0] != "k1" {
		t.Errorf("by_first['x'] = %v, want [k1]", got)
	}
}

// TestKeyspaceBulkLoadIndexedSpills forces the external sort to spill (tiny
// MaxTxBufferBytes) and verifies the merge path still produces a correct,
// complete index — and that scratch run files are cleaned up afterward
// (Inv-IdxBulk-4/5).
func TestKeyspaceBulkLoadIndexedSpills(t *testing.T) {
	ctx := context.Background()
	scratch := t.TempDir()
	db := openWith(t, ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 4096,
		// 48 KiB: < dataset → still spills, with room for the indexed
		// create's eager writes + the commit reserve.
		MaxTxBufferBytes: 48 << 10,
		ScratchDir:       scratch,
	})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	decl := testDecl("by_first", "first")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	const nrows = 1800
	rows := genKVs(nrows, 12) // values cycle 26 first-bytes
	if _, err := ks.BulkLoad(seqOf(rows)); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}

	idx, err := ks.Index("by_first")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if st, _ := idx.Stats(); st.Entries != nrows {
		t.Errorf("spilled index Count=%d, want %d", st.Entries, nrows)
	}
	// Every entry present and back-lookups resolve: a full Range yields nrows.
	if pairs := collectAllIndexPairs(t, idx); len(pairs) != nrows {
		t.Errorf("Range over spilled index yielded %d pairs, want %d", len(pairs), nrows)
	}
	// Spot-check one first-byte bucket: byte 'a' appears at i%26==0.
	wantA := 0
	for i := range rows {
		if rows[i].v[0] == 'a' {
			wantA++
		}
	}
	if got := lookupKeysSorted(t, idx, []byte("a")); len(got) != wantA {
		t.Errorf("by_first['a'] yielded %d keys, want %d", len(got), wantA)
	}
	// Cleanup ran: no leftover scratch run files.
	if got := countScratchRuns(t, scratch); got != 0 {
		t.Errorf("after BulkLoad: %d leftover scratch run files, want 0", got)
	}
}

// TestKeyspaceBulkLoadIndexedSpillWriteError verifies a spill write failure
// (ScratchDir does not exist) aborts BulkLoad with the wrapped I/O error and
// publishes nothing (Inv-IdxBulk-5).
func TestKeyspaceBulkLoadIndexedSpillWriteError(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	db := openWith(t, ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 4096,
		MaxTxBufferBytes: 48 << 10,
		ScratchDir:       missing,
	})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	decl := testDecl("by_first", "first")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	rows := genKVs(1800, 12)
	_, err = ks.BulkLoad(seqOf(rows))
	if err == nil {
		t.Fatal("BulkLoad with nonexistent ScratchDir returned nil, want a spill I/O error")
	}
	// The error must reference the spill failure (the create-scratch-file
	// wrap names the missing directory).
	if !strings.Contains(err.Error(), "scratch") || !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not reference the spill failure in %q", err, missing)
	}
	if ks.desc.Count != 0 || ks.desc.Root != 0 {
		t.Errorf("after spill error: Count=%d Root=%d, want both 0", ks.desc.Count, ks.desc.Root)
	}
}

// TestKeyspaceBulkLoadIndexedMergeCascadeBoundsFanIn forces the sorter to
// spill more runs than maxMergeFanIn (via extsort.SetMaxMergeFanInForTest + a
// small MaxTxBufferBytes) and verifies the cascade reduces the final
// merge fan-in to <= maxMergeFanIn while preserving end-to-end
// correctness. Pins the bulkload.md §Interaction with Indexes
// "Merge fan-in cap" invariant: the merger holds at most maxMergeFanIn
// run files open concurrently regardless of #runs.
//
// Neuter: removing the cascadeRuns call inside extsort.Cascade
// (or replacing it with a no-op) leaves postRuns == preRuns > cap=2,
// failing the postRuns assertion below. Removing the whole
// s.Cascade() call in buildIndexFromSorter instead fails earlier,
// at the cascadeFired check.
//
// The probe assertion (preRuns > cap) defends against silent test-
// workload shrinkage — if a future edit reduces nrows below the spill
// threshold, the cascade path never fires and the test would pass for
// the wrong reason.
func TestKeyspaceBulkLoadIndexedMergeCascadeBoundsFanIn(t *testing.T) {
	// cap=2 forces multi-pass cascade with any preRuns >= 2*cap+1 (=5):
	// pass 1 reduces preRuns → ceil(preRuns/cap) intermediates; pass 2
	// reduces those further; etc. The preRuns >= 2*testCap+1 probe
	// below structurally guards multi-pass exercise regardless of the
	// exact record-encoding size (which depends on the index encoder's
	// non-unique key shape + extsort's internal recordMemOverhead) — if a future edit
	// changes encoder sizing, the probe surfaces it.
	const testCap = 2
	restoreCap := extsort.SetMaxMergeFanInForTest(testCap)
	defer restoreCap()
	var preRuns, postRuns int
	var cascadeFired bool
	extsort.SetMergeCascadeHookForTest(func(pre, post int) {
		cascadeFired = true
		preRuns, postRuns = pre, post
	})
	defer extsort.SetMergeCascadeHookForTest(nil)

	ctx := context.Background()
	scratch := t.TempDir()
	db := openWith(t, ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 4096,
		// 32 KiB / 1 index → ~540 records/run. Sized so the indexed
		// CreateKeyspace's eager descriptor+registry writes plus the
		// commit reserve fit (a few pages), while the sorter still
		// spills often enough to exceed the merge fan-in below.
		MaxTxBufferBytes: 32 << 10,
		ScratchDir:       scratch,
	})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	decl := testDecl("by_first", "first")
	decl.Extract = firstByteExtract
	ks, err := tx.CreateKeyspace("items", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}

	// 900 rows at 8 KiB budget empirically produces ~6 spilled runs
	// (the encoded non-unique index key + extsort's internal recordMemOverhead per
	// record fits ~130 records per spill); the preRuns probe below
	// enforces the multi-pass minimum without depending on the exact
	// encoding constant.
	const nrows = 3600
	rows := genKVs(nrows, 12)
	if _, err := ks.BulkLoad(seqOf(rows)); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}

	if !cascadeFired {
		t.Fatal("merge-cascade hook did not fire; buildIndexFromSorter did not enter the spilled branch")
	}
	if preRuns <= testCap {
		t.Fatalf("preRuns=%d not greater than maxMergeFanIn=%d; test workload too small to exercise cascade — increase nrows or shrink MaxTxBufferBytes",
			preRuns, testCap)
	}
	// Probe a multi-pass cascade so a future workload shrinkage that
	// silently reduces to a single pass surfaces immediately. With
	// cap=2 and 6 spills, the first pass produces 3 intermediates
	// (still > cap=2), so a correct cascade runs a SECOND pass to land
	// at 2. Anything that would land at preRuns=3 indicates the multi-
	// pass loop short-circuited.
	if preRuns < 2*testCap+1 {
		t.Fatalf("preRuns=%d not >= 2*testCap+1=%d; workload too small to exercise multi-pass cascade",
			preRuns, 2*testCap+1)
	}
	if postRuns > testCap {
		t.Errorf("postRuns=%d > maxMergeFanIn=%d; cascade did not bound the fan-in",
			postRuns, testCap)
	}

	// End-to-end correctness: every row indexed and back-lookups
	// resolve, identical to the un-cascaded spill path.
	idx, err := ks.Index("by_first")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if st, _ := idx.Stats(); st.Entries != nrows {
		t.Errorf("cascaded index Count=%d, want %d", st.Entries, nrows)
	}
	if pairs := collectAllIndexPairs(t, idx); len(pairs) != nrows {
		t.Errorf("Range over cascaded index yielded %d pairs, want %d", len(pairs), nrows)
	}
	wantA := 0
	for i := range rows {
		if rows[i].v[0] == 'a' {
			wantA++
		}
	}
	if got := lookupKeysSorted(t, idx, []byte("a")); len(got) != wantA {
		t.Errorf("by_first['a'] yielded %d keys, want %d", len(got), wantA)
	}
	// Cleanup ran: no leftover scratch files (original spills + cascade
	// intermediates both match the gmdb-bulkidx-* glob).
	if got := countScratchRuns(t, scratch); got != 0 {
		t.Errorf("after cascading BulkLoad: %d leftover scratch run files, want 0", got)
	}
}

// TestKeyspaceBulkLoadIndexedAbortReopen verifies that after a unique
// violation + Rollback, a fresh transaction sees the keyspace and its
// indexes exactly at their pre-BulkLoad (empty) state — nothing leaked into
// committed state (Inv-IdxBulk-1).
func TestKeyspaceBulkLoadIndexedAbortReopen(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db := openWith(t, ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()

	mkDecl := func() *IndexDecl {
		d := testDecl("by_first", "first")
		d.Unique = true
		d.Extract = firstByteExtract
		return d
	}

	// tx1: create empty indexed keyspace, commit.
	tx1, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx1: %v", err)
	}
	if _, err := tx1.CreateKeyspace("items", mkDecl()); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatalf("Commit tx1: %v", err)
	}

	// tx2: BulkLoad with a cross-row unique dup → error → Rollback.
	tx2, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx2: %v", err)
	}
	ks2, err := tx2.OpenKeyspace("items", mkDecl())
	if err != nil {
		t.Fatalf("OpenKeyspace tx2: %v", err)
	}
	rows := []kv{{[]byte("k0"), []byte("apple")}, {[]byte("k1"), []byte("avocado")}}
	if _, err := ks2.BulkLoad(seqOf(rows)); !errors.Is(err, ErrIndexUniqueViolation) {
		t.Fatalf("BulkLoad = %v, want ErrIndexUniqueViolation", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("Rollback tx2: %v", err)
	}

	// tx3: reopen — must be empty.
	tx3, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin tx3: %v", err)
	}
	defer tx3.Rollback()
	ks3, err := tx3.OpenKeyspace("items", mkDecl())
	if err != nil {
		t.Fatalf("OpenKeyspace tx3: %v", err)
	}
	if ks3.desc.Count != 0 {
		t.Errorf("after abort+reopen: Count=%d, want 0", ks3.desc.Count)
	}
	idx, err := ks3.Index("by_first")
	if err != nil {
		t.Fatalf("Index tx3: %v", err)
	}
	if st, _ := idx.Stats(); st.Entries != 0 {
		t.Errorf("after abort+reopen: index Count=%d, want 0", st.Entries)
	}
}

// --- SetKeyspace indexed BulkLoad ---------------------------------

// TestSetKeyspaceBulkLoadIndexedRoundTrip bulk-loads an indexed SetKeyspace
// and verifies the per-member index resolves (setKey, setValue) pairs.
func TestSetKeyspaceBulkLoadIndexedRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	byFirst := testDecl("by_first", "first")
	byFirst.Extract = firstByteExtract // first byte of the set member value
	sks, err := tx.CreateSetKeyspace("subs", nil, byFirst)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	// (key, value) members in ascending (key, value) order.
	rows := []kv{
		{[]byte("u1"), []byte("alice")},
		{[]byte("u1"), []byte("amy")},
		{[]byte("u1"), []byte("bob")},
		{[]byte("u2"), []byte("ann")},
	}
	n, err := sks.BulkLoad(seqOf(rows))
	if err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	if n != 4 {
		t.Errorf("BulkLoad returned %d, want 4", n)
	}

	idx, err := sks.Index("by_first")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	// 'a' → (u1,alice),(u1,amy),(u2,ann); the iter.Seq2 yields (setKey, setValue).
	var aPairs []string
	for sk, sv := range idx.Lookup([][]byte{[]byte("a")}) {
		aPairs = append(aPairs, string(sk)+"/"+string(sv))
	}
	if err := idx.Err(); err != nil {
		t.Fatalf("idx.Err: %v", err)
	}
	sort.Strings(aPairs)
	want := []string{"u1/alice", "u1/amy", "u2/ann"}
	if !slicesEqualStr(aPairs, want) {
		t.Errorf("by_first['a'] = %v, want %v", aPairs, want)
	}
	if st, _ := idx.Stats(); st.Entries != 4 {
		t.Errorf("Stats Count=%d, want 4", st.Entries)
	}
}

// TestSetKeyspaceBulkLoadIndexedMatchesPut cross-checks an indexed
// SetKeyspace built via per-member Put vs BulkLoad.
func TestSetKeyspaceBulkLoadIndexedMatchesPut(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	// 30 keys × ~10 members each, ascending (key, value).
	var rows []kv
	for k := range 30 {
		key := fmt.Appendf(nil, "key%03d", k)
		for m := range 10 {
			rows = append(rows, kv{key, fmt.Appendf(nil, "%c-mem-%03d", byte('a'+m%26), m)})
		}
	}

	mkDecl := func() *IndexDecl {
		d := testDecl("by_first", "first")
		d.Extract = firstByteExtract
		return d
	}

	put, err := tx.CreateSetKeyspace("viaput", nil, mkDecl())
	if err != nil {
		t.Fatalf("CreateSetKeyspace viaput: %v", err)
	}
	for _, r := range rows {
		if _, err := put.Put(r.k, r.v); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	bulk, err := tx.CreateSetKeyspace("viabulk", nil, mkDecl())
	if err != nil {
		t.Fatalf("CreateSetKeyspace viabulk: %v", err)
	}
	if _, err := bulk.BulkLoad(seqOf(rows)); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}

	ip, err := put.Index("by_first")
	if err != nil {
		t.Fatalf("viaput Index: %v", err)
	}
	ib, err := bulk.Index("by_first")
	if err != nil {
		t.Fatalf("viabulk Index: %v", err)
	}
	putPairs := collectAllIndexPairs(t, ip)
	bulkPairs := collectAllIndexPairs(t, ib)
	if len(putPairs) != len(rows) {
		t.Errorf("Put produced %d index pairs, want %d", len(putPairs), len(rows))
	}
	if !slicesEqualStr(putPairs, bulkPairs) {
		t.Errorf("SetKeyspace BulkLoad index pairs differ from Put (put=%d bulk=%d)", len(putPairs), len(bulkPairs))
	}
}

// TestSetKeyspaceBulkLoadIndexedUniqueViolation verifies a unique index on a
// SetKeyspace aborts on two members producing the same column.
func TestSetKeyspaceBulkLoadIndexedUniqueViolation(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	uniq := testDecl("by_first", "first")
	uniq.Unique = true
	uniq.Extract = firstByteExtract
	sks, err := tx.CreateSetKeyspace("subs", nil, uniq)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	// Two distinct members sharing first byte 'a' → unique violation.
	rows := []kv{{[]byte("u1"), []byte("alice")}, {[]byte("u1"), []byte("amy")}}
	_, err = sks.BulkLoad(seqOf(rows))
	if !errors.Is(err, ErrIndexUniqueViolation) {
		t.Fatalf("BulkLoad = %v, want ErrIndexUniqueViolation", err)
	}
	if sks.desc.Count != 0 || sks.desc.Root != 0 {
		t.Errorf("after violation: Count=%d Root=%d, want both 0", sks.desc.Count, sks.desc.Root)
	}
}

// TestSetKeyspaceBulkLoadIndexedSpills forces a spill on an indexed
// SetKeyspace and verifies correctness + scratch cleanup.
func TestSetKeyspaceBulkLoadIndexedSpills(t *testing.T) {
	ctx := context.Background()
	scratch := t.TempDir()
	db := openWith(t, ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 4096,
		MaxTxBufferBytes: 48 << 10,
		ScratchDir:       scratch,
	})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	decl := testDecl("by_first", "first")
	decl.Extract = firstByteExtract
	sks, err := tx.CreateSetKeyspace("subs", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	var rows []kv
	for k := range 120 {
		key := fmt.Appendf(nil, "key%03d", k)
		for m := range 15 {
			rows = append(rows, kv{key, fmt.Appendf(nil, "%c-mem-%03d", byte('a'+m%26), m)})
		}
	}
	n, err := sks.BulkLoad(seqOf(rows))
	if err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	if int(n) != len(rows) {
		t.Errorf("BulkLoad returned %d, want %d", n, len(rows))
	}
	idx, err := sks.Index("by_first")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if pairs := collectAllIndexPairs(t, idx); len(pairs) != len(rows) {
		t.Errorf("Range over spilled SetKeyspace index yielded %d, want %d", len(pairs), len(rows))
	}
	if got := countScratchRuns(t, scratch); got != 0 {
		t.Errorf("after BulkLoad: %d leftover scratch run files, want 0", got)
	}
}

// TestKeyspaceBulkLoadIndexedUniqueViolationSpilled exercises the spilling
// (merge-output) unique-violation branch (bulkload.md spilling-sort bullet):
// a unique index with enough records to spill, and a duplicate value planted
// at two well-separated row positions. The duplicate lands in different
// sorted runs, so it is caught at the k-way merge, not the in-memory
// pre-scan. (Inv-IdxBulk-3; the cross-run detection path.)
func TestKeyspaceBulkLoadIndexedUniqueViolationSpilled(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 4096,
		MaxTxBufferBytes: 48 << 10, // forces spilling well before 1800 rows
		ScratchDir:       t.TempDir(),
	})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	uniq := testDecl("by_value", "value")
	uniq.Unique = true
	uniq.Extract = wholeValueExtract
	ks, err := tx.CreateKeyspace("items", uniq)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// 1800 rows, ascending keys, globally-distinct values EXCEPT two rows
	// that share the value "DUPLICATE" → a cross-row unique collision.
	const nrows = 1800
	rows := make([]kv, nrows)
	for i := range rows {
		rows[i] = kv{k: fmt.Appendf(nil, "row%05d", i), v: fmt.Appendf(nil, "val-%05d", i)}
	}
	rows[100].v = []byte("DUPLICATE")
	rows[500].v = []byte("DUPLICATE")

	_, err = ks.BulkLoad(seqOf(rows))
	if !errors.Is(err, ErrIndexUniqueViolation) {
		t.Fatalf("BulkLoad = %v, want ErrIndexUniqueViolation (via spilled merge output)", err)
	}
	if ks.desc.Count != 0 || ks.desc.Root != 0 {
		t.Errorf("after spilled violation: Count=%d Root=%d, want both 0", ks.desc.Count, ks.desc.Root)
	}
	if p := ks.indexes["by_value"]; p.root != 0 || p.count != 0 {
		t.Errorf("after spilled violation: pinned root=%d count=%d, want 0/0", p.root, p.count)
	}
}

// reuseBufSeq yields each pair through a SINGLE shared key buffer and a
// single shared value buffer, reused across iterations — the adversarial
// form of the iter.Seq2 input contract (callers may reuse buffers after
// yield returns). BulkLoad must clone everything it retains, including what
// the extractor's index entries alias.
func reuseBufSeq(pairs []kv) func(func([]byte, []byte) bool) {
	return func(yield func([]byte, []byte) bool) {
		kbuf := make([]byte, 0, 64)
		vbuf := make([]byte, 0, 64)
		for _, p := range pairs {
			kbuf = append(kbuf[:0], p.k...)
			vbuf = append(vbuf[:0], p.v...)
			if !yield(kbuf, vbuf) {
				return
			}
		}
	}
}

// TestKeyspaceBulkLoadIndexedReusedKeyBuffer verifies the index side honours
// the iter.Seq2 buffer-reuse contract: a yield reusing one key + one value
// buffer across iterations must still produce correct index entries, even
// across a spill+merge round-trip. The extractor's Cols/Cover may
// alias the reused buffers, so the emit helpers must copy before retaining.
func TestKeyspaceBulkLoadIndexedReusedKeyBuffer(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 4096,
		// 48 KiB: forces spill (→ exercise the merge path too) while
		// leaving room for the two-index CreateKeyspace's eager
		// writes plus the commit reserve.
		MaxTxBufferBytes: 48 << 10,
		ScratchDir:       t.TempDir(),
	})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	byFirst := testDecl("by_first", "first")
	byFirst.Extract = firstByteExtract
	byValue := testDecl("by_value", "value")
	byValue.Unique = true
	byValue.Extract = wholeValueExtract
	ks, err := tx.CreateKeyspace("items", byFirst, byValue)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}

	const nrows = 1800
	rows := make([]kv, nrows)
	for i := range rows {
		rows[i] = kv{k: fmt.Appendf(nil, "row%05d", i), v: fmt.Appendf(nil, "%c-val-%05d", byte('a'+i%26), i)}
	}
	n, err := ks.BulkLoad(reuseBufSeq(rows))
	if err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	if int(n) != nrows {
		t.Fatalf("BulkLoad returned %d, want %d", n, nrows)
	}

	// Unique index: every distinct value resolves to its exact PK — proves
	// the index keys/values were not corrupted by buffer reuse.
	idxValue, err := ks.Index("by_value")
	if err != nil {
		t.Fatalf("Index(by_value): %v", err)
	}
	for i := 0; i < nrows; i += 53 { // spot-check a spread of rows
		pk, val, err := idxValue.Get(rows[i].v)
		if err != nil {
			t.Fatalf("Get(%s): %v", rows[i].v, err)
		}
		if !bytes.Equal(pk, rows[i].k) || !bytes.Equal(val, rows[i].v) {
			t.Fatalf("Get(%s) = (%s,%s), want (%s,%s)", rows[i].v, pk, val, rows[i].k, rows[i].v)
		}
	}
	if st, _ := idxValue.Stats(); st.Entries != nrows {
		t.Errorf("by_value Count=%d, want %d", st.Entries, nrows)
	}
	// Non-unique index intact: total entries == nrows.
	idxFirst, err := ks.Index("by_first")
	if err != nil {
		t.Fatalf("Index(by_first): %v", err)
	}
	if pairs := collectAllIndexPairs(t, idxFirst); len(pairs) != nrows {
		t.Errorf("by_first Range yielded %d, want %d", len(pairs), nrows)
	}
}

// --- small local helpers ------------------------------------------

func slicesEqualStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The bulk index build accepts the SAME keys as the online path
// (limits.md — one threshold, no drift): an extractor-produced index
// key over the inline threshold stores as an overflow-key cell
// through BOTH paths, and the index lookup resolves it to the row.
func TestBulkLoadIndexKeyGateParity(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 512})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	bigKey := &IndexDecl{
		Name:    "big",
		Columns: []IndexColumn{{Name: "c"}},
		Extract: func(key, value []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{make([]byte, 1500)}}}
		},
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("k", bigKey)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// Online path: the 1500-byte all-zero column NUL-escapes to ~3000
	// bytes — over the inline threshold — and stores as an
	// overflow-key index cell.
	if err := ks.Put([]byte("row"), []byte("v")); err != nil {
		t.Fatalf("online Put with over-threshold index key: %v", err)
	}
	lookupRow := func(ks *Keyspace, label string) {
		idx, err := ks.Index("big")
		if err != nil {
			t.Fatalf("%s Index: %v", label, err)
		}
		var pks [][]byte
		for pk := range idx.LookupKeys([][]byte{make([]byte, 1500)}) {
			pks = append(pks, bytes.Clone(pk))
		}
		if err := idx.Err(); err != nil {
			t.Fatalf("%s LookupKeys: %v", label, err)
		}
		if len(pks) != 1 || !bytes.Equal(pks[0], []byte("row")) {
			t.Fatalf("%s lookup = %q, want [row]", label, pks)
		}
	}
	lookupRow(ks, "online")
	// BulkLoad accepts it identically (fresh keyspace) and the lookup
	// resolves through the bulk-built index tree.
	ks2, err := tx.CreateKeyspace("k2", bigKey)
	if err != nil {
		t.Fatalf("CreateKeyspace k2: %v", err)
	}
	if _, err = ks2.BulkLoad(func(yield func(k, v []byte) bool) {
		yield([]byte("row"), []byte("v"))
	}); err != nil {
		t.Fatalf("BulkLoad with over-threshold index key: %v", err)
	}
	lookupRow(ks2, "bulk")
}

// The SPILLED merge path shares the acceptance contract: enough rows
// to exceed the per-index sort budget forces the external-merge
// branch, whose bulkLeafEntry call stores the same over-threshold
// index keys as overflow-key cells.
func TestBulkLoadIndexKeyGateParitySpilled(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 4096,
		MaxTxBufferBytes: 1 << 20, // shrink the sort budget to force spill
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	bigKey := &IndexDecl{
		Name:    "big",
		Columns: []IndexColumn{{Name: "c"}},
		Extract: func(key, value []byte) []IndexEntry {
			col := make([]byte, 1500)
			copy(col, key) // distinct per row
			return []IndexEntry{{Cols: [][]byte{col}}}
		},
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("k", bigKey)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// The hook fires only on the spilled branch (after the run
	// cascade, before the merge loop where the gate rejects) —
	// asserting it fired pins that this test exercises the merge
	// path, not a silent duplicate of the in-memory case after a
	// future budget change. Global hook: no t.Parallel().
	spilled := false
	extsort.SetMergeCascadeHookForTest(func(pre, post int) { spilled = true })
	defer extsort.SetMergeCascadeHookForTest(nil)
	if _, err = ks.BulkLoad(func(yield func(k, v []byte) bool) {
		for i := range 1200 { // ~1.8 MB of index entries > 1 MB budget
			if !yield(fmt.Appendf(nil, "row%05d", i), []byte("v")) {
				return
			}
		}
	}); err != nil {
		t.Fatalf("spilled BulkLoad with over-threshold index keys: %v", err)
	}
	if !spilled {
		t.Fatal("index sorter never spilled — the test degraded to the in-memory case (raise the row count or lower MaxTxBufferBytes)")
	}
	// Spot-check a lookup through the spill-built index tree.
	idx, err := ks.Index("big")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	probe := make([]byte, 1500)
	copy(probe, []byte("row00042"))
	var pks [][]byte
	for pk := range idx.LookupKeys([][]byte{probe}) {
		pks = append(pks, bytes.Clone(pk))
	}
	if err := idx.Err(); err != nil {
		t.Fatalf("LookupKeys: %v", err)
	}
	if len(pks) != 1 || string(pks[0]) != "row00042" {
		t.Fatalf("spilled lookup = %q, want [row00042]", pks)
	}
}

// A covering index value the per-op path stores by overflow promotion
// must round-trip through BulkLoad too — pre-fix the bulk build
// aborted with a misleading ErrKeyTooLarge on data Put accepts. Both
// index-value layouts are pinned CONTENT-verified: the non-unique
// layout resolves the overflow-promoted value through the cursor's
// eager assembly, the unique layout through btree.Get +
// indexing.DecodeUniqueValue — a reassembly bug in either would
// otherwise pass a match-count-only check.
func TestBulkLoadCoveringLargeValueRoundTrips(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		unique bool
	}{{"non_unique", false}, {"unique", true}} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 1024})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer db.Close()
			bigVal := make([]byte, 5000)
			for i := range bigVal {
				bigVal[i] = byte(i)
			}
			cover := &IndexDecl{
				Name:     "cov",
				Unique:   tc.unique,
				Columns:  []IndexColumn{{Name: "c"}},
				Covering: []IndexCoveringColumn{{Name: "v"}},
				Extract: func(key, value []byte) []IndexEntry {
					return []IndexEntry{{Cols: [][]byte{key}, Cover: [][]byte{value}}}
				},
			}
			tx, err := db.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			ks, err := tx.CreateKeyspace("k", cover)
			if err != nil {
				t.Fatalf("CreateKeyspace: %v", err)
			}
			if _, err := ks.BulkLoad(func(yield func(k, v []byte) bool) {
				yield([]byte("row"), bigVal)
			}); err != nil {
				t.Fatalf("BulkLoad = %v (large covering value rejected; the per-op path stores it)", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			// The covering entry round-trips through the index read
			// path, content included.
			if err := db.Update(ctx, func(tx *Tx) error {
				ks, e := tx.OpenKeyspace("k", cover)
				if e != nil {
					return e
				}
				h, e := ks.Index("cov")
				if e != nil {
					return e
				}
				n := 0
				for pk, v := range h.Lookup([][]byte{[]byte("row")}) {
					n++
					if string(pk) != "row" {
						t.Errorf("pk = %q", pk)
					}
					cols, e := DecodeCoveringTuple(v)
					if e != nil {
						t.Errorf("DecodeCoveringTuple: %v", e)
						continue
					}
					if len(cols) != 1 {
						t.Errorf("covering tuple cols = %d, want 1", len(cols))
					} else if !bytes.Equal(cols[0], bigVal) {
						t.Errorf("covering column does not round-trip (%d bytes, want %d)", len(cols[0]), len(bigVal))
					}
				}
				if e := h.Err(); e != nil {
					return e
				}
				if n != 1 {
					t.Errorf("Lookup matches = %d, want 1", n)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Bulk-built index trees are encoded with the BASE page config —
// byte-identical to the online maintenance path — even when the
// keyspace's row tree uses a custom RestartGroupTarget. Covers BOTH
// keyspace kinds: the Keyspace and SetKeyspace BulkLoad variants
// build their index trees through separate finalizeIndexBuild call
// sites, so each needs its own pin.
func TestBulkLoadIndexTreeConfigParity(t *testing.T) {
	ctx := context.Background()
	decl := func() *IndexDecl {
		return &IndexDecl{
			Name:    "i",
			Columns: []IndexColumn{{Name: "c"}},
			Extract: func(key, value []byte) []IndexEntry {
				return []IndexEntry{{Cols: [][]byte{value}}}
			},
		}
	}
	build := func(t *testing.T, set, bulk bool) []byte {
		db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer db.Close()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer tx.Rollback()
		var (
			put      func(k, v []byte) error
			bulkLoad func(iter.Seq2[[]byte, []byte]) (uint64, error)
			indexes  map[string]*pinnedIndex
		)
		if set {
			ks, err := tx.CreateSetKeyspace("k", nil, decl())
			if err != nil {
				t.Fatalf("CreateSetKeyspace: %v", err)
			}
			put = func(k, v []byte) error { _, e := ks.Put(k, v); return e }
			bulkLoad = ks.BulkLoad
			indexes = ks.indexes
		} else {
			ks, err := tx.CreateKeyspace("k", decl())
			if err != nil {
				t.Fatalf("CreateKeyspace: %v", err)
			}
			put = ks.Put
			bulkLoad = ks.BulkLoad
			indexes = ks.indexes
		}
		if err := tx.SetKeyspaceConfig("k", KeyspaceConfig{RestartGroupTarget: 2}); err != nil {
			t.Fatalf("SetKeyspaceConfig: %v", err)
		}
		if bulk {
			if _, err := bulkLoad(func(yield func(k, v []byte) bool) {
				yield([]byte("a"), []byte("v1"))
				yield([]byte("b"), []byte("v2"))
				yield([]byte("c"), []byte("v3"))
			}); err != nil {
				t.Fatalf("BulkLoad: %v", err)
			}
		} else {
			for _, kv := range [][2]string{{"a", "v1"}, {"b", "v2"}, {"c", "v3"}} {
				if err := put([]byte(kv[0]), []byte(kv[1])); err != nil {
					t.Fatalf("Put: %v", err)
				}
			}
		}
		root := indexes["i"].root
		// Compare COMMITTED bytes: online slab pages receive their
		// checksum footer at commit-time pwrite, bulk pages at
		// WriteDirect — pre-commit the footers legitimately differ.
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		rtx, err := db.BeginRead(ctx)
		if err != nil {
			t.Fatalf("BeginRead: %v", err)
		}
		defer rtx.Rollback()
		buf, err := rtx.Page(root)
		if err != nil {
			t.Fatalf("Page: %v", err)
		}
		return append([]byte(nil), buf...)
	}
	for _, tc := range []struct {
		name string
		set  bool
	}{{"keyspace", false}, {"set_keyspace", true}} {
		t.Run(tc.name, func(t *testing.T) {
			bulkLeaf, onlineLeaf := build(t, tc.set, true), build(t, tc.set, false)
			if !bytes.Equal(bulkLeaf, onlineLeaf) {
				for i := range bulkLeaf {
					if bulkLeaf[i] != onlineLeaf[i] {
						t.Logf("first diff at offset %d: bulk=%02x online=%02x", i, bulkLeaf[i], onlineLeaf[i])
						break
					}
				}
				t.Fatal("bulk-built index leaf differs from the online-built one — config parity broken")
			}
		})
	}
}
