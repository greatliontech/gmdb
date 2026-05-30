package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestPutSizeSkewedLeafSplitNoSpuriousError reproduces the headline
// byte-split fault (btree-byte-balanced-split) through the public API:
// Put of valid size-skewed data — many small entries interleaved with
// occasional large inline values, the directory/metadata shape — must
// not return a spurious ErrKeyTooLarge, and every value must read back
// intact. The prior entry-count midpoint leaf split clustered large
// inline values on one half and rejected an ordinary Put with
// "btree: key too large" though every key is tiny. page-formats.md
// §Leaf Split.
func TestPutSizeSkewedLeafSplitNoSpuriousError(t *testing.T) {
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
	ks, err := tx.CreateKeyspace("dir")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}

	want := map[string][]byte{}
	put := func(k string, v []byte) {
		t.Helper()
		if err := ks.Put([]byte(k), v); err != nil {
			t.Fatalf("Put %q (value len %d): %v", k, len(v), err)
		}
		want[k] = v
	}
	// Four tiny entries, then four ~1300-byte inline values with keys
	// sorting after them. Inserting the fourth large value overflows the
	// leaf [a0..a3, b0..b3]; the entry-count midpoint (idx 4) puts all
	// four large values on the right half (~5.2 KB > a 4 KB page) and the
	// old split rejected the Put with a spurious ErrKeyTooLarge, although
	// the byte-balanced boundary fits both halves.
	for i := 0; i < 4; i++ {
		put(fmt.Sprintf("a%d", i), []byte("x"))
	}
	for i := 0; i < 4; i++ {
		put(fmt.Sprintf("b%d", i), bytes.Repeat([]byte{byte('A' + i)}, 1300))
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Read every value back in a fresh transaction — the split must have
	// preserved all data, not merely avoided the error.
	rtx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (read-back): %v", err)
	}
	defer rtx.Rollback()
	rks, err := rtx.OpenKeyspace("dir")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	for k, v := range want {
		got, err := rks.Get([]byte(k))
		if err != nil {
			t.Fatalf("Get %q: %v", k, err)
		}
		if !bytes.Equal(got, v) {
			t.Errorf("Get %q: value mismatch (got len %d, want len %d)", k, len(got), len(v))
		}
	}
}

// TestDeleteRangeSizeSkewedNoSpuriousCorrupt exercises the delete-side
// byte-balanced redistribute: deleting a large middle range from a
// size-skewed multi-leaf keyspace (MergeThreshold at the max 50, so
// boundary leaves go underfull and merge with full siblings, forcing the
// merge→overflow→redistribute path) must not return a spurious
// ErrCorrupted, and the surviving keys must remain intact.
func TestDeleteRangeSizeSkewedNoSpuriousCorrupt(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 1024, MergeThreshold: 50})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	key := func(i int) []byte { return []byte(fmt.Sprintf("k%05d", i)) }

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("dir")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for i := 0; i < 300; i++ {
		var v []byte
		if i%5 == 0 {
			v = bytes.Repeat([]byte{byte(i)}, 1300)
		} else {
			v = []byte(fmt.Sprintf("v%d", i))
		}
		if err := ks.Put(key(i), v); err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	tx2, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (delete): %v", err)
	}
	ks2, err := tx2.OpenKeyspace("dir")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	// Delete [k00100, k00200): 100 keys spanning several leaves.
	n, err := ks2.DeleteRange(key(100), key(200))
	if errors.Is(err, ErrCorrupted) {
		t.Fatalf("DeleteRange returned spurious ErrCorrupted: %v", err)
	}
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if n != 100 {
		t.Errorf("DeleteRange removed %d entries, want 100", n)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit (delete): %v", err)
	}

	// Surviving keys intact; deleted keys gone.
	tx3, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin (verify): %v", err)
	}
	defer tx3.Rollback()
	ks3, err := tx3.OpenKeyspace("dir")
	if err != nil {
		t.Fatalf("OpenKeyspace (verify): %v", err)
	}
	for i := 0; i < 300; i++ {
		got, err := ks3.Get(key(i))
		deleted := i >= 100 && i < 200
		switch {
		case deleted && !errors.Is(err, ErrNotFound):
			t.Errorf("Get #%d: expected ErrNotFound (deleted), got value=%v err=%v", i, got != nil, err)
		case !deleted && err != nil:
			t.Errorf("Get #%d: surviving key returned err %v", i, err)
		}
	}
}
