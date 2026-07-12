package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/greatliontech/gmdb/internal/btree"
	"github.com/greatliontech/gmdb/internal/page"
)

// bulkTestTx opens a fresh DB, begins a write transaction, and returns the
// tx plus a cleanup. The builder writes directly through tx.pgr; the test
// reads its own writes back through the same pager (mmap coherence with
// the WriteDirect pwrites) without committing.
func bulkTestTx(t *testing.T) (*Tx, func()) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 16384})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		_ = db.Close()
		t.Fatalf("Begin: %v", err)
	}
	return tx, func() {
		_ = tx.Rollback()
		_ = db.Close()
	}
}

type kv struct{ k, v []byte }

// buildBulkTree drives the bottom-up builder over kvs (assumed sorted) and
// returns (rootID, count). cfg selects the leaf encoding.
func buildBulkTree(t *testing.T, pw bulkPageWriter, cfg page.Config, kvs []kv) (uint64, uint64) {
	t.Helper()
	b := newBulkBuilder(pw, cfg)
	for _, e := range kvs {
		if err := b.add(page.LeafEntry{Key: e.k, Value: e.v}); err != nil {
			t.Fatalf("add(%q): %v", e.k, err)
		}
	}
	root, count, err := b.finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	return root, count
}

// verifyBulkTree checks every key resolves to its value via btree.Get and
// that a full cursor scan reproduces kvs in order.
func verifyBulkTree(t *testing.T, pr btree.PageReader, cfg page.Config, root uint64, kvs []kv) {
	t.Helper()
	for _, e := range kvs {
		got, found, err := btree.Get(pr, cfg, root, e.k)
		if err != nil {
			t.Fatalf("Get(%q): %v", e.k, err)
		}
		if !found {
			t.Fatalf("Get(%q): not found", e.k)
		}
		if !bytes.Equal(got, e.v) {
			t.Fatalf("Get(%q) = %q, want %q", e.k, got, e.v)
		}
	}
	// Full ordered scan.
	c := btree.NewReadCursor(pr, cfg, root)
	i := 0
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if i >= len(kvs) {
			t.Fatalf("cursor yielded more than %d entries", len(kvs))
		}
		if !bytes.Equal(k, kvs[i].k) {
			t.Fatalf("scan[%d] key = %q, want %q", i, k, kvs[i].k)
		}
		if !bytes.Equal(v, kvs[i].v) {
			t.Fatalf("scan[%d] value = %q, want %q", i, v, kvs[i].v)
		}
		i++
	}
	if err := c.Err(); err != nil {
		t.Fatalf("cursor Err: %v", err)
	}
	if i != len(kvs) {
		t.Fatalf("cursor yielded %d entries, want %d", i, len(kvs))
	}
}

// genKVs produces n sorted unique entries with valueLen-byte values.
func genKVs(n, valueLen int) []kv {
	kvs := make([]kv, n)
	for i := range n {
		k := fmt.Appendf(nil, "key%08d", i)
		v := bytes.Repeat([]byte{byte('a' + i%26)}, valueLen)
		kvs[i] = kv{k, v}
	}
	return kvs
}

func TestBulkBuilderRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		n        int
		valueLen int
		rgt      uint16 // 0 = engine default (compressed), 1 = uncompressed
	}{
		{"single-entry", 1, 8, 0},
		{"single-leaf", 50, 8, 0},
		{"multi-leaf-compressed", 500, 64, 0},
		{"multi-leaf-uncompressed", 500, 64, 1},
		{"deep-large-values", 4000, 400, 0},
		{"deep-uncompressed", 4000, 400, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, cleanup := bulkTestTx(t)
			defer cleanup()

			cfg := tx.pgr.Config()
			cfg.RestartGroupTarget = tc.rgt

			kvs := genKVs(tc.n, tc.valueLen)
			root, count := buildBulkTree(t, tx.pgr, cfg, kvs)

			if count != uint64(tc.n) {
				t.Errorf("count = %d, want %d", count, tc.n)
			}
			if root == 0 {
				t.Fatal("root = 0 for non-empty input")
			}
			verifyBulkTree(t, tx.pgr, cfg, root, kvs)
		})
	}
}

// TestBulkBuilderEmpty verifies zero entries yield no tree.
func TestBulkBuilderEmpty(t *testing.T) {
	tx, cleanup := bulkTestTx(t)
	defer cleanup()

	cfg := tx.pgr.Config()
	root, count := buildBulkTree(t, tx.pgr, cfg, nil)
	if root != 0 || count != 0 {
		t.Errorf("empty build = (root %d, count %d), want (0, 0)", root, count)
	}
}

// TestBulkBuilderRootTypeProgression verifies the root is a leaf for a
// single-leaf tree and a branch once the entries span multiple leaves —
// confirming the builder actually constructs branch levels.
func TestBulkBuilderRootTypeProgression(t *testing.T) {
	tx, cleanup := bulkTestTx(t)
	defer cleanup()
	cfg := tx.pgr.Config()

	// Few entries → single leaf root.
	smallRoot, _ := buildBulkTree(t, tx.pgr, cfg, genKVs(10, 8))
	typ, _, _, _ := page.ReadHeader(tx.pgr.PageRaw(smallRoot))
	if typ != page.TypeLeaf {
		t.Errorf("small-tree root type = %d, want TypeLeaf(%d)", typ, page.TypeLeaf)
	}

	// Many large-valued entries → branch root (multiple leaves).
	bigRoot, _ := buildBulkTree(t, tx.pgr, cfg, genKVs(2000, 400))
	btyp, _, _, _ := page.ReadHeader(tx.pgr.PageRaw(bigRoot))
	if btyp != page.TypeBranch {
		t.Errorf("big-tree root type = %d, want TypeBranch(%d)", btyp, page.TypeBranch)
	}
}

// genSkewedSeparatorKVs produces sorted entries that drive the bulk branch
// builder with LARGE, variable-size separators: keys within a cluster share
// a long prefix (deep common prefix → long ShortestSeparator at leaf
// boundaries), cluster transitions diverge at byte 0 (tiny separators), and
// large values keep each leaf small so a cluster spans many leaves.
func genSkewedSeparatorKVs(clusters, per, prefixLen, valueLen int) []kv {
	var kvs []kv
	for c := range clusters {
		prefix := append([]byte{byte('A' + c)}, bytes.Repeat([]byte("p"), prefixLen)...)
		for j := range per {
			k := append(append([]byte(nil), prefix...), fmt.Appendf(nil, "%06d", j)...)
			v := bytes.Repeat([]byte{byte('a' + j%26)}, valueLen)
			kvs = append(kvs, kv{k, v})
		}
	}
	return kvs
}

// TestBulkBuilderLargeSeparatorsByteDriven covers the bulk branch builder
// under size-skewed separators (page-formats.md §Prefix-Truncated Branch
// Keys). The
// bottom-up builder is fill-driven by construction — addLink appends a cell
// only while the page's BranchEncodedSizeOf stays <= ContentEnd (bulkload.go) — so unlike
// the top-down Put/Delete split it never chose a count midpoint and cannot
// overflow a branch page. This pins that property end-to-end: ~1400-byte
// separators build a multi-branch-level tree that round-trips intact.
func TestBulkBuilderLargeSeparatorsByteDriven(t *testing.T) {
	tx, cleanup := bulkTestTx(t)
	defer cleanup()
	cfg := tx.pgr.Config()

	kvs := genSkewedSeparatorKVs(6, 20, 1400, 1300)
	root, count := buildBulkTree(t, tx.pgr, cfg, kvs)
	if count != uint64(len(kvs)) {
		t.Errorf("count = %d, want %d", count, len(kvs))
	}
	// Large keys + values force many leaves and at least one branch level.
	if typ, _, _, _ := page.ReadHeader(tx.pgr.PageRaw(root)); typ != page.TypeBranch {
		t.Fatalf("root type = %d, want TypeBranch (no branch level built)", typ)
	}
	verifyBulkTree(t, tx.pgr, cfg, root, kvs)
}

// TestBulkBuilderOutOfOrder verifies a non-ascending key is rejected with
// ErrBulkLoadOutOfOrder (not a LeafBuilder panic).
func TestBulkBuilderOutOfOrder(t *testing.T) {
	tx, cleanup := bulkTestTx(t)
	defer cleanup()
	cfg := tx.pgr.Config()

	b := newBulkBuilder(tx.pgr, cfg)
	if err := b.add(page.LeafEntry{Key: []byte("b"), Value: []byte("1")}); err != nil {
		t.Fatalf("add b: %v", err)
	}
	// Equal key (not strictly greater) is out of order.
	if err := b.add(page.LeafEntry{Key: []byte("b"), Value: []byte("2")}); !errors.Is(err, ErrBulkLoadOutOfOrder) {
		t.Errorf("add equal key = %v, want ErrBulkLoadOutOfOrder", err)
	}
	// Smaller key is out of order.
	if err := b.add(page.LeafEntry{Key: []byte("a"), Value: []byte("3")}); !errors.Is(err, ErrBulkLoadOutOfOrder) {
		t.Errorf("add smaller key = %v, want ErrBulkLoadOutOfOrder", err)
	}
}

// TestBulkBuilderRandomKeys builds from random variable-length unique keys
// (sorted), exercising ShortestSeparator at diverse byte boundaries — a
// stronger separator-correctness probe than the shared-prefix sequential
// keys. Round-trips and cross-checks against the top-down tree.
func TestBulkBuilderRandomKeys(t *testing.T) {
	rng := rand.New(rand.NewPCG(0x9e3779b97f4a7c15, 0xb5))
	seen := make(map[string]struct{}, 3000)
	var kvs []kv
	for len(kvs) < 3000 {
		klen := 1 + rng.IntN(48)
		k := make([]byte, klen)
		for i := range k {
			k[i] = byte(rng.IntN(256))
		}
		if _, dup := seen[string(k)]; dup {
			continue
		}
		seen[string(k)] = struct{}{}
		v := fmt.Appendf(nil, "v=%x", k)
		kvs = append(kvs, kv{k, v})
	}
	slices.SortFunc(kvs, func(a, b kv) int { return bytes.Compare(a.k, b.k) })

	tx, cleanup := bulkTestTx(t)
	defer cleanup()
	cfg := tx.pgr.Config()

	root, count := buildBulkTree(t, tx.pgr, cfg, kvs)
	if count != uint64(len(kvs)) {
		t.Fatalf("count = %d, want %d", count, len(kvs))
	}
	verifyBulkTree(t, tx.pgr, cfg, root, kvs)

	// Cross-check: a key NOT present must not be found (separator routing
	// must not falsely land a miss on some leaf).
	for range 200 {
		probe := make([]byte, 1+rng.IntN(48))
		for i := range probe {
			probe[i] = byte(rng.IntN(256))
		}
		_, present := seen[string(probe)]
		_, found, err := btree.Get(tx.pgr, cfg, root, probe)
		if err != nil {
			t.Fatalf("Get(probe %x): %v", probe, err)
		}
		if found != present {
			t.Fatalf("Get(probe %x): found=%v, want %v", probe, found, present)
		}
	}
}

// TestBulkBuilderBranchSizeAccounting locks the bulk builder's incremental
// branch-size tracking against a full BranchEncodedSize recompute. Branch
// sizing is NON-additive (page-formats.md §Branch Page): the page-wide shared
// prefix is stored once and shrinks as separators that share less prefix are
// appended, lengthening every existing cell's suffix — so there is no fixed
// per-cell cost. addLink tracks (keyLenSum, prefix) and sizes via
// BranchEncodedSizeOf; this mirrors that exact accounting and asserts it
// equals the authoritative BranchEncodedSize after every append, across a
// shared-prefix run, a distinct-prefix run, and a run where the page prefix
// collapses mid-build.
func TestBulkBuilderBranchSizeAccounting(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	withPrefix := func(p []byte, suffix string) []byte {
		return append(append([]byte(nil), p...), suffix...)
	}
	pfx := bytes.Repeat([]byte("shared/"), 40) // ~280-byte shared prefix
	cases := map[string][][]byte{
		"shared-prefix": {
			withPrefix(pfx, "0001"), withPrefix(pfx, "0002"),
			withPrefix(pfx, "0500"), withPrefix(pfx, "9999"),
		},
		"distinct-prefix": {
			bytes.Repeat([]byte{'a'}, 1), bytes.Repeat([]byte{'b'}, 4),
			bytes.Repeat([]byte{'c'}, 13), bytes.Repeat([]byte{'d'}, 250),
		},
		"prefix-collapses-midway": {
			withPrefix(pfx, "01"), withPrefix(pfx, "02"),
			[]byte("zzz-different-cluster"), // shrinks the page prefix to nothing
		},
	}
	for name, keys := range cases {
		t.Run(name, func(t *testing.T) {
			var cells []page.BranchCell
			keyLenSum := 0
			for i, k := range keys {
				// Mirror addLink: prefix = commonPrefix(cells[0], new key)
				// (cells arrive ascending, so the new key is the largest and
				// commonPrefix(first,last) is the whole-set prefix).
				prefixLen := len(k)
				if len(cells) > 0 {
					prefixLen = commonPrefixLen(cells[0].Key, k)
				}
				incr := page.BranchEncodedSizeOf(len(cells)+1, keyLenSum+len(k), prefixLen)
				cells = append(cells, page.BranchCell{Key: k, Child: uint64(i + 1)})
				keyLenSum += len(k)
				if got := page.BranchEncodedSize(cfg, cells); got != incr {
					t.Fatalf("after %d cells: incremental size %d != BranchEncodedSize %d", i+1, incr, got)
				}
			}
		})
	}
}

// TestBulkBuilderMatchesTopDownGet cross-checks the bulk-built tree against
// an independent top-down btree.Put tree: every key Get-resolves to the
// same value in both. Guards against a separator bug that routes Get to the
// wrong leaf in a way a self-consistent scan might miss.
func TestBulkBuilderMatchesTopDownGet(t *testing.T) {
	tx, cleanup := bulkTestTx(t)
	defer cleanup()
	cfg := tx.pgr.Config()

	kvs := genKVs(1500, 32)

	// Bulk tree.
	bulkRoot, _ := buildBulkTree(t, tx.pgr, cfg, kvs)

	// Top-down tree built via btree.Put (the production insert path).
	var topRoot uint64
	for _, e := range kvs {
		nr, err := btree.Put(btreeWriter{tx.pgr}, cfg, topRoot, e.k, e.v)
		if err != nil {
			t.Fatalf("btree.Put(%q): %v", e.k, err)
		}
		topRoot = nr
	}

	for _, e := range kvs {
		bv, bfound, err := btree.Get(tx.pgr, cfg, bulkRoot, e.k)
		if err != nil || !bfound {
			t.Fatalf("bulk Get(%q): found=%v err=%v", e.k, bfound, err)
		}
		tv, tfound, err := btree.Get(tx.pgr, cfg, topRoot, e.k)
		if err != nil || !tfound {
			t.Fatalf("topdown Get(%q): found=%v err=%v", e.k, tfound, err)
		}
		if !bytes.Equal(bv, tv) {
			t.Fatalf("Get(%q): bulk=%q topdown=%q", e.k, bv, tv)
		}
	}
}
