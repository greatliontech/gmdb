package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// These tests promote four data-op invariants:
//
//   Inv-A:  Put-then-Get round-trips the value.
//   Inv-B:  descriptor CoW → meta.KeyspaceRoot across commit;
//           reopen finds the data.
//   Inv-C:  descriptor.Count tracks leaf-entry count.
//   Inv-D:  Sibling cursor MarkStale'd by Put/Delete (curKey/curValue
//           nilled; subsequent ops return ErrCursorStale).
//
// Plus the API-level Delete-on-miss invariant (Kind=0 portion).

func TestKeyspacePutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	ks, err := tx.CreateKeyspace("ks")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("hello"), []byte("world")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := ks.Get([]byte("hello"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, []byte("world")) {
		t.Errorf("Get = %q, want %q", got, "world")
	}
}

func TestKeyspaceGetMissingReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()

	ks, _ := tx.CreateKeyspace("ks")
	if _, err := ks.Get([]byte("missing")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing: got %v, want ErrNotFound", err)
	}
}

// TestKeyspaceDeleteMissingReturnsErrNotFound promotes the
// Delete-on-miss invariant at the Keyspace.Delete surface.
func TestKeyspaceDeleteMissingReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	if err := ks.Delete([]byte("missing")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing: got %v, want ErrNotFound", err)
	}
}

func TestKeyspaceEmptyKeyReturnsErrKeyEmpty(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	for _, k := range [][]byte{nil, {}} {
		if _, err := ks.Get(k); !errors.Is(err, ErrKeyEmpty) {
			t.Errorf("Get(empty): got %v", err)
		}
		if err := ks.Put(k, []byte("v")); !errors.Is(err, ErrKeyEmpty) {
			t.Errorf("Put(empty): got %v", err)
		}
		if err := ks.Delete(k); !errors.Is(err, ErrKeyEmpty) {
			t.Errorf("Delete(empty): got %v", err)
		}
	}
}

// Inv-C: descriptor.Count tracks leaf-entry count.
func TestKeyspaceCountTracksLeafEntries(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")

	if ks.desc.Count != 0 {
		t.Fatalf("initial Count = %d, want 0", ks.desc.Count)
	}
	for i := 0; i < 5; i++ {
		_ = ks.Put([]byte{byte('a' + i)}, []byte("v"))
	}
	if ks.desc.Count != 5 {
		t.Errorf("Count after 5 Puts = %d, want 5", ks.desc.Count)
	}
	// Put-replace does not bump Count.
	_ = ks.Put([]byte("a"), []byte("v2"))
	if ks.desc.Count != 5 {
		t.Errorf("Count after replace = %d, want 5", ks.desc.Count)
	}
	// Delete decrements.
	_ = ks.Delete([]byte("c"))
	if ks.desc.Count != 4 {
		t.Errorf("Count after Delete = %d, want 4", ks.desc.Count)
	}
}

// Inv-B: CoW propagation across commit + reopen.
func TestKeyspacePutCommitReopenRetrieves(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("ks")
	if err := ks.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	_ = db.Close()

	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	tx2, _ := db2.Begin(ctx)
	defer tx2.Rollback()
	ks2, err := tx2.OpenKeyspace("ks")
	if err != nil {
		t.Fatalf("OpenKeyspace after reopen: %v", err)
	}
	got, err := ks2.Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if !bytes.Equal(got, []byte("v")) {
		t.Errorf("Get after reopen = %q, want %q", got, "v")
	}
}

// Inv-D: sibling cursor MarkStale'd by Put/Delete.
func TestCursorMarkStaleAfterSiblingPut(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	_ = ks.Put([]byte("a"), []byte("1"))
	_ = ks.Put([]byte("b"), []byte("2"))

	c := ks.Cursor()
	k, _ := c.First()
	if !bytes.Equal(k, []byte("a")) {
		t.Fatalf("First() = %q, want %q", k, "a")
	}

	// Sibling Put — should MarkStale c.
	if err := ks.Put([]byte("c"), []byte("3")); err != nil {
		t.Fatalf("sibling Put: %v", err)
	}
	// Next on the stale cursor must surface ErrCursorStale.
	k, _ = c.Next()
	if k != nil {
		t.Errorf("Next on stale cursor = %q, want nil", k)
	}
	if !errors.Is(c.Err(), ErrCursorStale) {
		t.Errorf("Err on stale cursor = %v, want ErrCursorStale", c.Err())
	}
	// Re-position recovers — and observes the sibling-Put'd "c".
	// Prior to the cursor.SetRootID +
	// markCursorsStale-refresh pair, the re-position would have
	// descended from the now-retired old root and silently read
	// stale (or corrupted post-loose-pop) data.
	k, _ = c.First()
	if !bytes.Equal(k, []byte("a")) {
		t.Errorf("Re-First after sibling Put = %q, want a", k)
	}
	if err := c.Err(); err != nil {
		t.Errorf("Err after re-position = %v, want nil", err)
	}
	// Walk to confirm the Put'd "c" is visible.
	saw := map[string]struct{}{}
	for k != nil {
		saw[string(k)] = struct{}{}
		k, _ = c.Next()
	}
	for _, want := range []string{"a", "b", "c"} {
		if _, ok := saw[want]; !ok {
			t.Errorf("cursor walk missing %q after sibling-Put + re-position", want)
		}
	}
}

// SetKeyspaceConfig: Inv-E.
func TestSetKeyspaceConfigUpdatesRestartGroupTarget(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	if ks.desc.RestartGroupTarget != 0 {
		t.Fatalf("initial RestartGroupTarget = %d, want 0", ks.desc.RestartGroupTarget)
	}
	if err := tx.SetKeyspaceConfig("ks", KeyspaceConfig{RestartGroupTarget: 32}); err != nil {
		t.Fatalf("SetKeyspaceConfig: %v", err)
	}
	// Lookup via the in-memory-aware path — the deferred-flush
	// machinery moves descriptor persistence to Commit time, so
	// loadDescriptor (disk-only) returns the pre-config state until
	// the flush walk runs. lookupDescriptor consults openKeyspaces
	// and dirtyDescriptors first per the deferred-flush contract.
	desc, found, err := tx.lookupDescriptor("ks")
	if err != nil || !found {
		t.Fatalf("lookupDescriptor: found=%v err=%v", found, err)
	}
	if desc.RestartGroupTarget != 32 {
		t.Errorf("descriptor RestartGroupTarget = %d, want 32", desc.RestartGroupTarget)
	}
	// Cache also reflects.
	if ks.desc.RestartGroupTarget != 32 {
		t.Errorf("cached handle RestartGroupTarget = %d, want 32", ks.desc.RestartGroupTarget)
	}
}

func TestSetKeyspaceConfigZeroIsNoOp(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	_, _ = tx.CreateKeyspace("ks")
	desc, _, _ := tx.loadDescriptor("ks")
	rootBefore := tx.keyspaceRoot
	_ = desc

	if err := tx.SetKeyspaceConfig("ks", KeyspaceConfig{}); err != nil {
		t.Fatalf("SetKeyspaceConfig zero: %v", err)
	}
	if tx.keyspaceRoot != rootBefore {
		t.Errorf("zero-config SetKeyspaceConfig CoW'd the keyspace B+tree")
	}
}

func TestSetKeyspaceConfigOversizeReturnsErrInvalidOptions(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	_, _ = tx.CreateKeyspace("ks")
	if err := tx.SetKeyspaceConfig("ks", KeyspaceConfig{RestartGroupTarget: 256}); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("RestartGroupTarget=256: got %v, want ErrInvalidOptions", err)
	}
}

func TestSetKeyspaceConfigMissingReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	if err := tx.SetKeyspaceConfig("missing", KeyspaceConfig{RestartGroupTarget: 32}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing keyspace: got %v, want ErrNotFound", err)
	}
}

// Cursor walk: round-trip via cursor.
func TestCursorWalkRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	for _, k := range []string{"c", "a", "b"} {
		_ = ks.Put([]byte(k), []byte("v-"+k))
	}
	c := ks.Cursor()
	var seen []string
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		seen = append(seen, string(k))
	}
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("cursor walk = %v, want %v", seen, want)
	}
}

func TestCursorDeleteAdvancesToSuccessor(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	for _, k := range []string{"a", "b", "c"} {
		_ = ks.Put([]byte(k), []byte("v"))
	}
	c := ks.Cursor()
	c.First()
	k, _ := c.Current()
	if !bytes.Equal(k, []byte("a")) {
		t.Fatalf("Current = %q, want a", k)
	}
	if err := c.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Post-delete: cursor at successor "b".
	k, _ = c.Current()
	if !bytes.Equal(k, []byte("b")) {
		t.Errorf("post-Delete Current = %q, want b", k)
	}
	// Descriptor Count decremented.
	if ks.desc.Count != 2 {
		t.Errorf("Count after cursor Delete = %d, want 2", ks.desc.Count)
	}
}

// Options.Logger plumb-through smoke test: a non-nil logger captures
// the cleanup warning.
func TestOptionsLoggerCapturesLeakWarning(t *testing.T) {
	// We can't easily intercept the cleanup since it fires on GC,
	// but we can verify Options.Logger is captured at Open and
	// available via the cleanup-info plumbing. The semantic
	// pinning lives in the existing leak-detection tests; this
	// test just confirms the field is set.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 128,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if db.logger == nil {
		t.Error("db.logger is nil; Open did not install discard fallback")
	}
}

// TestCursorAfterRollbackReturnsErrTxClosed pins H2 fix: Cursor
// methods refuse to read after the parent tx has closed.
func TestCursorAfterRollbackReturnsErrTxClosed(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("ks")
	_ = ks.Put([]byte("a"), []byte("v"))
	c := ks.Cursor()
	c.First()

	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if k, _ := c.Next(); k != nil {
		t.Errorf("Next on closed tx returned key %q, want nil", k)
	}
	if err := c.Err(); !errors.Is(err, ErrTxClosed) {
		t.Errorf("Err on closed tx = %v, want ErrTxClosed", err)
	}
	// Mutating ops also refuse.
	if err := c.Delete(); !errors.Is(err, ErrTxClosed) {
		t.Errorf("Delete on closed tx = %v, want ErrTxClosed", err)
	}
}

// TestKeyspacePutHonorsPerKeyspaceRestartGroupTarget pins H3 fix:
// SetKeyspaceConfig's RestartGroupTarget value reaches the leaf
// builder. Verify by observing the leaf-page Type byte: target=1
// produces TypeLeafUncompressed; target>=2 produces TypeLeaf.
func TestKeyspacePutHonorsPerKeyspaceRestartGroupTarget(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")

	// Default RGT=0 (engine default = compressed leaf in the default
	// layout — segregated).
	_ = ks.Put([]byte("a"), []byte("v"))
	leafBuf := tx.pgr.PageRaw(ks.desc.Root)
	if !page.IsLeafType(leafBuf[0]) {
		t.Fatalf("root is not a leaf: type=%d", leafBuf[0])
	}
	if leafBuf[0] != page.TypeLeafSegregated {
		t.Errorf("default RGT: leaf type = %d, want %d (TypeLeafSegregated compressed)",
			leafBuf[0], page.TypeLeafSegregated)
	}

	// Set RGT=1 → next leaf should be uncompressed.
	if err := tx.SetKeyspaceConfig("ks", KeyspaceConfig{RestartGroupTarget: 1}); err != nil {
		t.Fatalf("SetKeyspaceConfig: %v", err)
	}
	// Write enough to force a split or rewrite — Put on an existing
	// leaf CoWs it and re-encodes with the new builder cfg.
	_ = ks.Put([]byte("b"), []byte("v"))
	leafBuf = tx.pgr.PageRaw(ks.desc.Root)
	if leafBuf[0] != page.TypeLeafUncompressed {
		t.Errorf("after SetKeyspaceConfig(RGT=1): leaf type = %d, "+
			"want %d (TypeLeafUncompressed) — per-keyspace RGT not "+
			"reaching the builder", leafBuf[0], page.TypeLeafUncompressed)
	}
}

// TestKeyspaceLayoutDeclaration pins the NodeLayouts flow (keyspaces.md
// §Per-Keyspace Configuration): SetKeyspaceConfig's LeafLayout reaches
// the builder for pages written after the call, existing pages keep
// their on-disk variant (readers dispatch by type byte, never config —
// page-formats.md §Invariants), the declaration persists across reopen,
// and invalid values reject.
func TestKeyspaceLayoutDeclaration(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("ks")

	// Default layout: segregated. Fill several leaves so a later
	// declaration flip leaves genuinely MIXED-variant pages behind
	// (untouched leaves keep their type byte).
	put := func(k string) {
		if err := ks.Put([]byte(k), []byte("v")); err != nil {
			t.Fatalf("Put(%q): %v", k, err)
		}
	}
	var keys []string
	for i := range 40 {
		k := fmt.Sprintf("seg-%03d-%s", i, strings.Repeat("x", 200))
		keys = append(keys, k)
		put(k)
	}
	countLeafTypes := func() (seg, il int) {
		var walk func(id uint64)
		walk = func(id uint64) {
			b := tx.pgr.PageRaw(id)
			switch ty, _, _, _ := page.ReadHeader(b); ty {
			case page.TypeBranch, page.TypeBranchSegregated:
				lm, cells := page.DecodeBranch(b, tx.pgr.Config())
				walk(lm)
				for _, c := range cells {
					walk(c.Child)
				}
			case page.TypeLeafSegregated:
				seg++
			case page.TypeLeaf:
				il++
			}
		}
		walk(ks.desc.Root)
		return seg, il
	}
	if seg, il := countLeafTypes(); seg < 2 || il != 0 {
		t.Fatalf("default layout: leaves seg=%d il=%d, want several segregated only", seg, il)
	}

	// Declare interleaved: pages written after the call use it; the
	// untouched leaves keep their segregated type — the tree holds
	// BOTH variants at once, and every read dispatches by the page's
	// type byte (page-formats.md §Invariants).
	if err := tx.SetKeyspaceConfig("ks", KeyspaceConfig{LeafLayout: LeafLayoutInterleaved}); err != nil {
		t.Fatalf("SetKeyspaceConfig(LeafLayout): %v", err)
	}
	for i := range 40 {
		k := fmt.Sprintf("il--%03d-%s", i, strings.Repeat("y", 200))
		keys = append(keys, k)
		put(k)
	}
	if seg, il := countLeafTypes(); seg == 0 || il == 0 {
		t.Fatalf("after flip: leaves seg=%d il=%d, want both variants coexisting", seg, il)
	}
	// Every key readable through the mixed tree.
	for _, k := range keys {
		if v, err := ks.Get([]byte(k)); err != nil || string(v) != "v" {
			t.Fatalf("Get(%q) on mixed tree: %q, %v", k, v, err)
		}
	}
	put("b")
	keys = append(keys, "b")
	put("a")
	keys = append(keys, "a")

	// Unknown values reject.
	if err := tx.SetKeyspaceConfig("ks", KeyspaceConfig{LeafLayout: 3}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("LeafLayout=3: err = %v, want ErrInvalidOptions", err)
	}
	// Branch layout declarations flow the same way. Build a separate
	// keyspace whose LONG shared-prefix keys make separators ~800
	// bytes — branch fanout ~4 — so the tree carries SEVERAL branch
	// pages; a declaration flip then re-encodes only the branches on
	// later write paths, leaving genuinely mixed branch variants.
	ksb, err := tx.CreateKeyspace("ksb")
	if err != nil {
		t.Fatal(err)
	}
	// CLUSTERED long keys: within a cluster, adjacent keys share an
	// ~800-byte prefix (long separators); across clusters they diverge
	// at byte 0. A segregated branch spanning clusters therefore has a
	// zero page prefix and each ~800-byte separator costs full bytes —
	// fanout ~5 — so the tree carries several branch pages (the same
	// shape as the unreachable-floor fixtures).
	longP := strings.Repeat("P", 800)
	bigVal := []byte(strings.Repeat("v", 1200)) // ~2 entries per leaf
	var bkeys []string
	putB := func(k string) {
		if err := ksb.Put([]byte(k), bigVal); err != nil {
			t.Fatalf("ksb.Put(%q): %v", k, err)
		}
	}
	for c := range 12 {
		for j := range 4 {
			k := fmt.Sprintf("%c%s-%03d", 'A'+c, longP, j)
			bkeys = append(bkeys, k)
			putB(k)
		}
	}
	countBranchTypes := func() (seg, plain int) {
		var walk func(id uint64)
		walk = func(id uint64) {
			b := tx.pgr.PageRaw(id)
			switch ty, _, _, _ := page.ReadHeader(b); ty {
			case page.TypeBranchSegregated:
				seg++
			case page.TypeBranch:
				plain++
			default:
				return
			}
			lm, cells := page.DecodeBranch(b, tx.pgr.Config())
			walk(lm)
			for _, c := range cells {
				walk(c.Child)
			}
		}
		walk(ksb.desc.Root)
		return seg, plain
	}
	if seg, plain := countBranchTypes(); seg < 2 || plain != 0 {
		t.Fatalf("pre-declaration branches: seg=%d plain=%d, want several segregated only", seg, plain)
	}
	if err := tx.SetKeyspaceConfig("ksb", KeyspaceConfig{BranchLayout: BranchLayoutPlain}); err != nil {
		t.Fatalf("SetKeyspaceConfig(BranchLayout=plain): %v", err)
	}
	// Insert into only the first few clusters — the touched write
	// paths re-encode their branches to plain, the rest keep the
	// segregated type byte.
	for c := range 3 {
		k := fmt.Sprintf("%c%s-%03d", 'A'+c, longP, 4)
		bkeys = append(bkeys, k)
		putB(k)
	}
	if seg, plain := countBranchTypes(); plain == 0 || seg == 0 {
		t.Fatalf("after declaring plain and splitting: seg=%d plain=%d, want both variants coexisting", seg, plain)
	}
	for _, k := range bkeys {
		if v, err := ksb.Get([]byte(k)); err != nil || !bytes.Equal(v, bigVal) {
			t.Fatalf("ksb.Get(%q) with mixed branch layouts: %d bytes, %v", k, len(v), err)
		}
	}
	if err := tx.SetKeyspaceConfig("ks", KeyspaceConfig{BranchLayout: 3}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("BranchLayout=3: err = %v, want ErrInvalidOptions", err)
	}
	// The leaf-side keyspace also declares plain branches so the
	// reopened-descriptor assertion below covers both bit fields.
	if err := tx.SetKeyspaceConfig("ks", KeyspaceConfig{BranchLayout: BranchLayoutPlain}); err != nil {
		t.Fatalf("SetKeyspaceConfig(ks, BranchLayout=plain): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the declaration persisted; both keys readable (the root
	// page keeps its interleaved variant; type-byte dispatch reads it
	// regardless of any config).
	db2, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	tx2, _ := db2.Begin(ctx)
	defer tx2.Rollback()
	ks2, err := tx2.OpenKeyspace("ks")
	if err != nil {
		t.Fatal(err)
	}
	if ks2.desc.LeafLayoutBits() != uint8(LeafLayoutInterleaved) {
		t.Fatalf("reopened descriptor LeafLayoutBits = %d, want %d", ks2.desc.LeafLayoutBits(), LeafLayoutInterleaved)
	}
	if ks2.desc.BranchLayoutBits() != uint8(BranchLayoutPlain) {
		t.Fatalf("reopened descriptor BranchLayoutBits = %d, want %d", ks2.desc.BranchLayoutBits(), BranchLayoutPlain)
	}
	for _, k := range []string{"a", "b"} {
		v, err := ks2.Get([]byte(k))
		if err != nil || string(v) != "v" {
			t.Fatalf("Get(%q) after reopen: %q, %v", k, v, err)
		}
	}
	// Flip back to segregated: new pages use it, old pages still read.
	if err := tx2.SetKeyspaceConfig("ks", KeyspaceConfig{LeafLayout: LeafLayoutSegregated}); err != nil {
		t.Fatal(err)
	}
	_ = ks2.Put([]byte("c"), []byte("v"))
	for _, k := range []string{"a", "b", "c"} {
		v, err := ks2.Get([]byte(k))
		if err != nil || string(v) != "v" {
			t.Fatalf("Get(%q) after flip back: %q, %v", k, v, err)
		}
	}
}

// TestOptionsBranchLayoutReachesBuilder pins the ENGINE-WIDE
// Options.BranchLayout flow (keyspaces.md: descriptor 0 defers to the
// opening process's engine default): with no per-keyspace declaration,
// branches build in the Options-selected layout.
func TestOptionsBranchLayoutReachesBuilder(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 256,
		BranchLayout: BranchLayoutPlain,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("ks")
	for i := range 60 {
		if err := ks.Put([]byte(fmt.Sprintf("k-%03d-%s", i, strings.Repeat("x", 200))), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	buf := tx.pgr.PageRaw(ks.desc.Root)
	typ, _, _, _ := page.ReadHeader(buf)
	if typ != page.TypeBranch {
		t.Fatalf("root type = %d, want TypeBranch (engine-wide Options.BranchLayout=plain ignored)", typ)
	}
}
