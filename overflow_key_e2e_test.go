package gmdb

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// e2eOvkKey builds an over-threshold key with a cluster-shared prefix
// past the inline threshold at 4 KiB pages, so adjacent keys tie
// through their resident bytes (page-formats.md §Overflow-Key Cells).
func e2eOvkKey(i int) []byte {
	k := bytes.Repeat([]byte{'k'}, 2100)
	return append(k, fmt.Sprintf("-%06d", i)...)
}

// TestOverflowKeyEndToEnd drives over-threshold keys through the full
// public surface: transactional writes, reads, cursor iteration,
// Check, Compact, CopyTo, and a close/reopen cycle — the durability
// and maintenance paths a real consumer exercises.
func TestOverflowKeyEndToEnd(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const n = 40

	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("big")
		if err != nil {
			return err
		}
		for i := range n {
			if err := ks.Put(e2eOvkKey(i), fmt.Appendf(nil, "v%d", i)); err != nil {
				return fmt.Errorf("Put %d: %w", i, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("populate: %v", err)
	}

	verify := func(d *DB, label string) {
		t.Helper()
		if err := d.View(ctx, func(rtx *ReadTx) error {
			ks, err := rtx.OpenKeyspaceReadOnly("big")
			if err != nil {
				return err
			}
			for i := range n {
				v, err := ks.Get(e2eOvkKey(i))
				if err != nil {
					return fmt.Errorf("Get %d: %w", i, err)
				}
				if !bytes.Equal(v, fmt.Appendf(nil, "v%d", i)) {
					return fmt.Errorf("Get %d = %q", i, v)
				}
			}
			// Full-key iteration order and materialization.
			i := 0
			for k := range ks.All() {
				if !bytes.Equal(k, e2eOvkKey(i)) {
					return fmt.Errorf("iter %d: key len %d tail %q", i, len(k), k[max(0, len(k)-8):])
				}
				i++
			}
			if i != n {
				return fmt.Errorf("iterated %d keys, want %d", i, n)
			}
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	verify(db, "post-populate")

	checkClean := func(d *DB, label string) {
		t.Helper()
		for issue := range d.Check() {
			t.Errorf("%s: Check issue: %+v", label, issue)
		}
	}
	checkClean(db, "post-populate")

	// Compact: rebuilds the file in place; overflow-key cells and their
	// extents must relocate coherently.
	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	verify(db, "post-compact")
	checkClean(db, "post-compact")

	// CopyTo: the compacting rebuild re-encodes every tree bottom-up
	// through the bulk builders (over-threshold separators included).
	copyPath := tmpPath(t)
	if err := db.CopyTo(copyPath, true); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	cdb, err := Open(ctx, copyPath, Options{PageSize: 4096})
	if err != nil {
		t.Fatalf("Open copy: %v", err)
	}
	verify(cdb, "copy")
	checkClean(cdb, "copy")
	if err := cdb.Close(); err != nil {
		t.Fatalf("Close copy: %v", err)
	}

	// Delete half through the public surface; extents retire.
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.OpenKeyspace("big")
		if err != nil {
			return err
		}
		for i := 0; i < n; i += 2 {
			if err := ks.Delete(e2eOvkKey(i)); err != nil {
				return fmt.Errorf("Delete %d: %w", i, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("delete-half: %v", err)
	}
	checkClean(db, "post-delete")

	// Close/reopen: durability across restart (SyncDurable default).
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db, err = Open(ctx, path, Options{PageSize: 4096})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	if err := db.View(ctx, func(rtx *ReadTx) error {
		ks, err := rtx.OpenKeyspaceReadOnly("big")
		if err != nil {
			return err
		}
		for i := range n {
			_, err := ks.Get(e2eOvkKey(i))
			if i%2 == 0 {
				if err == nil {
					return fmt.Errorf("deleted key %d still present after reopen", i)
				}
				continue
			}
			if err != nil {
				return fmt.Errorf("surviving key %d after reopen: %w", i, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("post-reopen: %v", err)
	}
	checkClean(db, "post-reopen")
}

// TestOverflowKeySetReplaceRetiresDisplacedExtent pins the PutEntry
// displaced-key-extent retirement (page-formats.md §Overflow-Key
// Cells, lifecycle): every set-cell rewrite under an over-threshold
// set key retires the displaced entry's key extent — repeated value
// puts must not grow Check-reported leakage.
func TestOverflowKeySetReplaceRetiresDisplacedExtent(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 512})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	key := bytes.Repeat([]byte{'k'}, 2100) // > T
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateSetKeyspace("s", nil)
		if err != nil {
			return err
		}
		for i := range 8 { // every put rewrites the set cell
			if _, err := ks.Put(key, fmt.Appendf(nil, "v%d", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("puts: %v", err)
	}
	for issue := range db.Check() {
		t.Errorf("Check issue after set-cell rewrites: %+v", issue)
	}
}

// TestOverflowKeyDeleteRangeClearRetiresSeparatorExtents pins the
// whole-branch retirement path: DeleteRange(nil, nil) over a tree
// whose branch separators carry key extents retires every extent —
// the documented clear idiom must leave zero leakage.
func TestOverflowKeyDeleteRangeClearRetiresSeparatorExtents(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("big")
		if err != nil {
			return err
		}
		for i := range 64 {
			if err := ks.Put(e2eOvkKey(i), []byte("v")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("populate: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.OpenKeyspace("big")
		if err != nil {
			return err
		}
		n, err := ks.DeleteRange(nil, nil)
		if err != nil {
			return err
		}
		if n != 64 {
			return fmt.Errorf("DeleteRange(nil, nil) = %d, want 64", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	for issue := range db.Check() {
		t.Errorf("Check issue after clear: %+v", issue)
	}
}
