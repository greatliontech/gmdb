package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"testing"
)

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// seqOf adapts a kv slice to the BulkLoad input iterator.
func seqOf(kvs []kv) iter.Seq2[[]byte, []byte] {
	return func(yield func(k, v []byte) bool) {
		for _, e := range kvs {
			if !yield(e.k, e.v) {
				return
			}
		}
	}
}

// openWith opens (or reopens) the database at path with opts, failing the
// test on error.
func openWith(t *testing.T, ctx context.Context, path string, opts Options) *DB {
	t.Helper()
	db, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

func TestKeyspaceBulkLoadRoundTrip(t *testing.T) {
	for _, csum := range []bool{false, true} {
		t.Run("csum="+boolStr(csum), func(t *testing.T) {
			ctx := context.Background()
			path := tmpPath(t)
			opts := Options{PageSize: 4096, PageChecksum: csum, MinSize: 16, MaxSize: 16384}

			kvs := genKVs(3000, 48) // multiple leaves + branch levels

			db := openWith(t, ctx, path, opts)
			tx, err := db.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			ks, err := tx.CreateKeyspace("users")
			if err != nil {
				t.Fatalf("CreateKeyspace: %v", err)
			}
			n, err := ks.BulkLoad(seqOf(kvs))
			if err != nil {
				t.Fatalf("BulkLoad: %v", err)
			}
			if n != uint64(len(kvs)) {
				t.Errorf("BulkLoad returned %d, want %d", n, len(kvs))
			}
			if ks.desc.Count != uint64(len(kvs)) {
				t.Errorf("Count = %d, want %d", ks.desc.Count, len(kvs))
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// Reopen and verify durability via the public read API.
			db2 := openWith(t, ctx, path, opts)
			defer db2.Close()
			tx2, err := db2.Begin(ctx)
			if err != nil {
				t.Fatalf("re-Begin: %v", err)
			}
			defer tx2.Rollback()
			ks2, err := tx2.OpenKeyspace("users")
			if err != nil {
				t.Fatalf("re-OpenKeyspace: %v", err)
			}
			for _, e := range kvs {
				got, err := ks2.Get(e.k)
				if err != nil {
					t.Fatalf("Get(%q): %v", e.k, err)
				}
				if !bytes.Equal(got, e.v) {
					t.Fatalf("Get(%q) = %q, want %q", e.k, got, e.v)
				}
			}
			// Full ordered cursor scan.
			c := ks2.Cursor()
			i := 0
			for k, v := c.First(); k != nil; k, v = c.Next() {
				if i >= len(kvs) || !bytes.Equal(k, kvs[i].k) || !bytes.Equal(v, kvs[i].v) {
					t.Fatalf("scan[%d] = (%q,%q)", i, k, v)
				}
				i++
			}
			if err := c.Err(); err != nil {
				t.Fatalf("cursor Err: %v", err)
			}
			if i != len(kvs) {
				t.Errorf("scan yielded %d entries, want %d", i, len(kvs))
			}
		})
	}
}

// TestKeyspaceBulkLoadOverflowValues exercises the streaming overflow-chain
// writer: values larger than a leaf page are stored as overflow chains and
// must reassemble byte-identically through the public Get after reopen.
// Both checksum modes (footer interaction with the overflow tail).
func TestKeyspaceBulkLoadOverflowValues(t *testing.T) {
	for _, csum := range []bool{false, true} {
		t.Run("csum="+boolStr(csum), func(t *testing.T) {
			ctx := context.Background()
			path := tmpPath(t)
			opts := Options{PageSize: 4096, PageChecksum: csum, MinSize: 16, MaxSize: 16384}

			// Mix of inline and overflow-sized values (1, 2, 3-page runs)
			// in sorted key order.
			kvs := []kv{
				{[]byte("k01"), bytes.Repeat([]byte("a"), 10)},     // inline
				{[]byte("k02"), bytes.Repeat([]byte("B"), 5000)},   // 2-page overflow
				{[]byte("k03"), []byte("small")},                   // inline
				{[]byte("k04"), bytes.Repeat([]byte{0x7f}, 12000)}, // 3-page overflow
				{[]byte("k05"), bytes.Repeat([]byte("z"), 4050)},   // ~1-page boundary
				{[]byte("k06"), bytes.Repeat([]byte("Q"), 40000)},  // ~10-page overflow
			}

			db := openWith(t, ctx, path, opts)
			tx, err := db.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			ks, err := tx.CreateKeyspace("blobs")
			if err != nil {
				t.Fatalf("CreateKeyspace: %v", err)
			}
			if _, err := ks.BulkLoad(seqOf(kvs)); err != nil {
				t.Fatalf("BulkLoad: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			db.Close()

			db2 := openWith(t, ctx, path, opts)
			defer db2.Close()
			tx2, err := db2.Begin(ctx)
			if err != nil {
				t.Fatalf("re-Begin: %v", err)
			}
			defer tx2.Rollback()
			ks2, err := tx2.OpenKeyspace("blobs")
			if err != nil {
				t.Fatalf("re-OpenKeyspace: %v", err)
			}
			for _, e := range kvs {
				got, err := ks2.Get(e.k)
				if err != nil {
					t.Fatalf("Get(%q): %v", e.k, err)
				}
				if !bytes.Equal(got, e.v) {
					t.Fatalf("Get(%q): got %d bytes, want %d (overflow reassembly)", e.k, len(got), len(e.v))
				}
			}
		})
	}
}

func TestKeyspaceBulkLoadNonEmpty(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("users")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := ks.BulkLoad(seqOf([]kv{{[]byte("b"), []byte("2")}})); !errors.Is(err, ErrBulkLoadNonEmpty) {
		t.Errorf("BulkLoad into non-empty = %v, want ErrBulkLoadNonEmpty", err)
	}
}

func TestKeyspaceBulkLoadOutOfOrder(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("users")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	bad := []kv{{[]byte("b"), []byte("1")}, {[]byte("a"), []byte("2")}}
	if _, err := ks.BulkLoad(seqOf(bad)); !errors.Is(err, ErrBulkLoadOutOfOrder) {
		t.Errorf("BulkLoad out-of-order = %v, want ErrBulkLoadOutOfOrder", err)
	}
}

func TestKeyspaceBulkLoadEmptyKey(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("users")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if _, err := ks.BulkLoad(seqOf([]kv{{nil, []byte("v")}})); !errors.Is(err, ErrKeyEmpty) {
		t.Errorf("BulkLoad empty key = %v, want ErrKeyEmpty", err)
	}
}

func TestKeyspaceBulkLoadReadOnlyHandle(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	opts := Options{PageSize: 4096, MinSize: 16, MaxSize: 128}
	// Create the keyspace first so a read-only open can find it.
	db := openWith(t, ctx, path, opts)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.CreateKeyspace("users"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	tx2, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin2: %v", err)
	}
	defer tx2.Rollback()
	ks, err := tx2.OpenKeyspaceReadOnly("users")
	if err != nil {
		t.Fatalf("OpenKeyspaceReadOnly: %v", err)
	}
	if _, err := ks.BulkLoad(seqOf([]kv{{[]byte("a"), []byte("1")}})); !errors.Is(err, ErrReadOnly) {
		t.Errorf("BulkLoad on read-only handle = %v, want ErrReadOnly", err)
	}
	db.Close()
}

// TestKeyspaceBulkLoadAbortLeavesPreState verifies a rolled-back BulkLoad
// leaves the committed (empty) keyspace intact — the bounded-leakage /
// atomicity contract at the Keyspace layer.
func TestKeyspaceBulkLoadAbortLeavesPreState(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	opts := Options{PageSize: 4096, MinSize: 16, MaxSize: 16384}

	// tx1: create the keyspace empty, commit.
	db := openWith(t, ctx, path, opts)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.CreateKeyspace("users"); err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// tx2: BulkLoad a lot, then ROLLBACK.
	tx2, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin2: %v", err)
	}
	ks, err := tx2.OpenKeyspace("users")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	// Include an overflow-sized value so the rollback also exercises
	// reclamation of directly-written overflow-chain pages, not just
	// leaf/branch pages.
	abortKVs := append(genKVs(2000, 64), kv{[]byte("zzz_overflow"), bytes.Repeat([]byte("X"), 30000)})
	if _, err := ks.BulkLoad(seqOf(abortKVs)); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	db.Close()

	// Reopen: the keyspace exists and is still empty.
	db2 := openWith(t, ctx, path, opts)
	defer db2.Close()
	tx3, err := db2.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin3: %v", err)
	}
	defer tx3.Rollback()
	ks3, err := tx3.OpenKeyspace("users")
	if err != nil {
		t.Fatalf("OpenKeyspace after rollback: %v", err)
	}
	if ks3.desc.Count != 0 {
		t.Errorf("Count after rolled-back BulkLoad = %d, want 0", ks3.desc.Count)
	}
	if ks3.desc.Root != 0 {
		t.Errorf("Root after rolled-back BulkLoad = %d, want 0", ks3.desc.Root)
	}
}

func TestKeyspaceBulkLoadEmptyStream(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("users")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	n, err := ks.BulkLoad(seqOf(nil))
	if err != nil {
		t.Fatalf("BulkLoad empty: %v", err)
	}
	if n != 0 || ks.desc.Count != 0 || ks.desc.Root != 0 {
		t.Errorf("empty BulkLoad: n=%d Count=%d Root=%d, want all 0", n, ks.desc.Count, ks.desc.Root)
	}
}

// Indexed Keyspace.BulkLoad is covered comprehensively in
// bulkload_indexed_test.go (round-trip, Put-parity, unique-violation,
// spill, abort-leaves-pre-state). The chunk-8.4 interim "indexed ⇒ error"
// stub it asserted was deliberately replaced by the real path in 8.6.

// TestKeyspaceBulkLoadReusedKeyBuffer verifies the iter.Seq2 contract: a
// yield that reuses a single key (and value) buffer across iterations must
// still produce a correct tree (the builder clones what it retains).
func TestKeyspaceBulkLoadReusedKeyBuffer(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	opts := Options{PageSize: 4096, MinSize: 16, MaxSize: 16384}

	const n = 2000
	db := openWith(t, ctx, path, opts)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("reuse")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// A single reused backing buffer for both key and value.
	keyBuf := make([]byte, 0, 16)
	valBuf := make([]byte, 0, 16)
	reuse := func(yield func(k, v []byte) bool) {
		for i := range n {
			keyBuf = append(keyBuf[:0], fmt.Sprintf("rk%08d", i)...)
			valBuf = append(valBuf[:0], fmt.Sprintf("v%d", i)...)
			if !yield(keyBuf, valBuf) {
				return
			}
		}
	}
	if _, err := ks.BulkLoad(reuse); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	db.Close()

	db2 := openWith(t, ctx, path, opts)
	defer db2.Close()
	tx2, err := db2.Begin(ctx)
	if err != nil {
		t.Fatalf("re-Begin: %v", err)
	}
	defer tx2.Rollback()
	ks2, err := tx2.OpenKeyspace("reuse")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	for i := range n {
		k := fmt.Appendf(nil, "rk%08d", i)
		want := fmt.Appendf(nil, "v%d", i)
		got, err := ks2.Get(k)
		if err != nil {
			t.Fatalf("Get(%q): %v", k, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Get(%q) = %q, want %q", k, got, want)
		}
	}
}

// TestKeyspaceBulkLoadKeyTooLarge verifies a key too large for even an
// overflow-reference entry surfaces the public gmdb.ErrKeyTooLarge
// sentinel (the internal btree.ErrKeyTooLarge is translated by
// mapBtreeErr at the BulkLoad boundary).
func TestKeyspaceBulkLoadKeyTooLarge(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("users")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	// A key larger than the leaf content area can't fit even as an
	// overflow reference.
	bigKey := bytes.Repeat([]byte("K"), 4096)
	if _, err := ks.BulkLoad(seqOf([]kv{{bigKey, []byte("v")}})); !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("BulkLoad oversize key = %v, want gmdb.ErrKeyTooLarge", err)
	}
}
