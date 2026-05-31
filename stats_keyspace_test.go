package gmdb

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestKeyspaceStats covers the data-tree walk: empty keyspace, a split
// (branch + multiple leaves, depth >= 2), an overflow run, the index
// count, and the post-DeleteKeyspace guard.
func TestKeyspaceStats(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("ks")
		if err != nil {
			return err
		}

		// Empty keyspace: everything zero.
		if s, err := ks.Stats(); err != nil {
			t.Fatalf("empty Stats: %v", err)
		} else if s.Entries != 0 || s.Depth != 0 || s.LeafPages != 0 || s.BranchPages != 0 || s.OverflowPages != 0 || s.IndexCount != 0 {
			t.Errorf("empty keyspace stats = %+v, want all zero", s)
		}

		// Enough small entries to force at least one split, plus one
		// oversized value to force an overflow run.
		const n = 2000
		for i := range n {
			if err := ks.Put([]byte{byte(i >> 8), byte(i)}, []byte("v")); err != nil {
				return err
			}
		}
		if err := ks.Put([]byte("\xff\xffbig"), bytes.Repeat([]byte("x"), 4096*3)); err != nil {
			return err
		}

		s, err := ks.Stats()
		if err != nil {
			return err
		}
		if s.Entries != n+1 {
			t.Errorf("Entries = %d, want %d", s.Entries, n+1)
		}
		if s.Depth < 2 {
			t.Errorf("Depth = %d, want >= 2 (a split occurred)", s.Depth)
		}
		if s.BranchPages < 1 {
			t.Errorf("BranchPages = %d, want >= 1", s.BranchPages)
		}
		if s.LeafPages < 2 {
			t.Errorf("LeafPages = %d, want >= 2", s.LeafPages)
		}
		if s.OverflowPages < 1 {
			t.Errorf("OverflowPages = %d, want >= 1 (oversized value)", s.OverflowPages)
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// IndexCount reflects registered indexes, and the dead-handle guard.
	if err := db.Update(ctx, func(tx *Tx) error {
		decl := testDecl("by_b", "b")
		decl.Extract = firstByteExtract
		ks, err := tx.CreateKeyspace("indexed", decl)
		if err != nil {
			return err
		}
		if err := ks.Put([]byte("k"), []byte{0x7}); err != nil {
			return err
		}
		if s, err := ks.Stats(); err != nil {
			t.Fatalf("indexed Stats: %v", err)
		} else if s.IndexCount != 1 {
			t.Errorf("IndexCount = %d, want 1", s.IndexCount)
		}
		// DeleteKeyspace invalidates the handle.
		if err := tx.DeleteKeyspace("indexed"); err != nil {
			return err
		}
		if _, err := ks.Stats(); !errors.Is(err, ErrKeyspaceClosed) {
			t.Errorf("Stats on a deleted keyspace: got %v, want ErrKeyspaceClosed", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("indexed Update: %v", err)
	}
}

// TestSetKeyspaceStats confirms the embedded keyspaceCore.Stats() is
// reachable on a *SetKeyspace and returns sane values for its nested
// outer/member tree.
func TestSetKeyspaceStats(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("set", nil)
		if err != nil {
			return err
		}
		for i := range 20 {
			if _, err := sks.Put([]byte("key"), []byte{byte(i)}); err != nil {
				return err
			}
		}
		s, err := sks.Stats()
		if err != nil {
			return err
		}
		if s.Entries != 20 {
			t.Errorf("SetKeyspace Entries = %d, want 20", s.Entries)
		}
		if s.Depth < 1 || s.LeafPages < 1 {
			t.Errorf("SetKeyspace stats = %+v, want Depth>=1, LeafPages>=1", s)
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

// TestIndexStats covers the reshaped 7-field IndexStats: entry count,
// Unique/Covering flags, and the data-tree walk (depth/page/size).
func TestIndexStats(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.Update(ctx, func(tx *Tx) error {
		decl := testDecl("by_b", "b")
		decl.Extract = firstByteExtract
		decl.Unique = false
		ks, err := tx.CreateKeyspace("items", decl)
		if err != nil {
			return err
		}
		idx, err := ks.Index("by_b")
		if err != nil {
			return err
		}
		// Empty index.
		if s, err := idx.Stats(); err != nil {
			t.Fatalf("empty index Stats: %v", err)
		} else if s.Entries != 0 || s.Depth != 0 || s.SizeBytes != 0 {
			t.Errorf("empty index stats = %+v, want zero", s)
		}

		// Distinct extracted keys so each Put adds an index entry.
		const n = 64
		for i := range n {
			if err := ks.Put([]byte{byte(i)}, []byte{byte(i)}); err != nil {
				return err
			}
		}
		s, err := idx.Stats()
		if err != nil {
			return err
		}
		if s.Entries != n {
			t.Errorf("IndexStats.Entries = %d, want %d", s.Entries, n)
		}
		if s.Unique {
			t.Error("Unique = true, want false (decl.Unique = false)")
		}
		if s.Covering {
			t.Error("Covering = true, want false (no covering columns)")
		}
		if s.Depth < 1 || s.LeafPages < 1 {
			t.Errorf("index tree stats = %+v, want Depth>=1, LeafPages>=1", s)
		}
		if want := (s.BranchPages + s.LeafPages) * 4096; s.SizeBytes < want {
			t.Errorf("SizeBytes = %d, want >= %d (branch+leaf pages * PageSize)", s.SizeBytes, want)
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

// TestKeyspaceStatsReadOnlyTx exercises the read-transaction stats path
// (statsHWM sourced from the snapshot meta, not the live pager).
func TestKeyspaceStatsReadOnlyTx(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("ks")
		if err != nil {
			return err
		}
		for i := range 100 {
			if err := ks.Put([]byte{byte(i)}, []byte("v")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := db.View(ctx, func(rtx *ReadTx) error {
		ks, err := rtx.OpenKeyspaceReadOnly("ks")
		if err != nil {
			return err
		}
		s, err := ks.Stats()
		if err != nil {
			return err
		}
		if s.Entries != 100 {
			t.Errorf("read-tx KeyspaceStats.Entries = %d, want 100", s.Entries)
		}
		if s.Depth < 1 || s.LeafPages < 1 {
			t.Errorf("read-tx stats = %+v, want Depth>=1, LeafPages>=1", s)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}
