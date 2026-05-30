package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// TestPutNearFullInlineValueIsStored is the regression for the limits.md
// §Maximum Value Size storability guarantee: a near-full-page inline value
// inserted between existing entries leaves a leaf with no feasible
// two-page split, which previously returned a spurious ErrKeyTooLarge.
// On-demand overflow promotion (put.go store loop) now stores it —
// limits.md promises any value bounded by disk/MaxSize is storable.
func TestPutNearFullInlineValueIsStored(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 1024})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}

	// A leaf of 20 small entries (k00..k19), then a near-full-page inline
	// value whose key (k095) sorts in the middle (index 10 of 21). The
	// value cannot share a page with the small entries on either side, so
	// no two-page split fits and the Put must promote it to overflow, not
	// reject it. Arithmetic (4 KB page, ~4080-byte content area): the
	// 3900-byte value inlines (~3913 bytes ≤ threshold), and with ~10
	// small entries (~430 bytes) forced onto whichever half holds the big
	// value, neither half fits — every contiguous split overflows a page.
	// Keep the big value near-full and mid-sorted if these sizes change.
	want := map[string][]byte{}
	put := func(k string, v []byte) {
		t.Helper()
		if err := ks.Put([]byte(k), v); err != nil {
			t.Fatalf("Put %q (len %d): %v", k, len(v), err)
		}
		want[k] = v
	}
	small := bytes.Repeat([]byte("s"), 30)
	for i := 0; i < 20; i++ {
		put(fmt.Sprintf("k%02d", i), small)
	}
	put("k095", bytes.Repeat([]byte{0xAB}, 3900)) // strict-fit inline, but unsplittable here

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	verifyAll(t, db, "k", want)
}

// TestPutManyNearFullValuesSucceed stresses on-demand promotion: many
// near-full-page inline values interleaved with small entries, every Put
// succeeding and reading back intact.
func TestPutManyNearFullValuesSucceed(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}

	want := map[string][]byte{}
	for i := 0; i < 120; i++ {
		k := fmt.Sprintf("k%04d", i)
		var v []byte
		if i%3 == 0 {
			v = bytes.Repeat([]byte{byte(i)}, 3600) // near-full inline
		} else {
			v = []byte(fmt.Sprintf("v%d", i))
		}
		if err := ks.Put([]byte(k), v); err != nil {
			t.Fatalf("Put %q (len %d): %v", k, len(v), err)
		}
		want[k] = v
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	verifyAll(t, db, "k", want)
}

func verifyAll(t *testing.T, db *DB, ksName string, want map[string][]byte) {
	t.Helper()
	tx, err := db.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin (verify): %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.OpenKeyspace(ksName)
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	for k, v := range want {
		got, err := ks.Get([]byte(k))
		if err != nil {
			t.Fatalf("Get %q: %v", k, err)
		}
		if !bytes.Equal(got, v) {
			t.Errorf("Get %q: value mismatch (got %d bytes, want %d)", k, len(got), len(v))
		}
	}
}
