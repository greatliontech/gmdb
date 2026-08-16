package gmdb

import (
	"context"
	"fmt"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// TestCompactPreservesBuilderDefaults pins that the engine-wide builder
// defaults (Options.RestartGroupTarget / LeafLayout / BranchLayout —
// per-open configuration, options.go) survive Compact's writer-pager
// reopen: the swapped-in pager must carry the same page-build
// configuration the handle was opened with, not the engine defaults.
func TestCompactPreservesBuilderDefaults(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 4096,
		RestartGroupTarget: 7,
		LeafLayout:         LeafLayoutInterleaved,
		BranchLayout:       BranchLayoutPlain,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	buildChurnedDB(t, db)

	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	cfg := db.pgr.Config()
	if cfg.RestartGroupTarget != 7 {
		t.Errorf("post-Compact RestartGroupTarget = %d, want 7", cfg.RestartGroupTarget)
	}
	if cfg.LeafLayout != page.LeafLayout(LeafLayoutInterleaved) {
		t.Errorf("post-Compact LeafLayout = %d, want %d (interleaved)",
			cfg.LeafLayout, LeafLayoutInterleaved)
	}
	if cfg.BranchLayout != page.BranchLayout(BranchLayoutPlain) {
		t.Errorf("post-Compact BranchLayout = %d, want %d (plain)",
			cfg.BranchLayout, BranchLayoutPlain)
	}
}

// TestCompactKeepsUncompressedLeafDefault pins the behavioral
// consequence: with Options.RestartGroupTarget = 1 (uncompressed leaf
// variant), leaves re-encoded by writes AFTER a Compact must still be
// built uncompressed — a post-Compact pager that reverted to the
// engine default would re-encode them compressed at target 6.
func TestCompactKeepsUncompressedLeafDefault(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{
		PageSize: 4096, MinSize: 16, MaxSize: 4096,
		RestartGroupTarget: 1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := tx.CreateKeyspace("ks")
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Put([]byte("a"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if got := tx.pgr.PageRaw(ks.desc.Root)[0]; got != page.TypeLeafUncompressed {
		t.Fatalf("pre-Compact leaf type = %d, want %d (uncompressed) — "+
			"engine-wide RGT=1 not reaching the builder at all", got, page.TypeLeafUncompressed)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := db.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	tx2, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback()
	ks2, err := tx2.OpenKeyspace("ks")
	if err != nil {
		t.Fatal(err)
	}
	// BEFORE any write: the compacted image itself must honor the
	// engine-wide default — the rebuild builds pages and must not
	// revert to the engine defaults.
	if got := tx2.pgr.PageRaw(ks2.desc.Root)[0]; got != page.TypeLeafUncompressed {
		t.Errorf("compacted-image leaf type = %d, want %d (uncompressed) — "+
			"builder defaults lost in the Compact rebuild", got, page.TypeLeafUncompressed)
	}
	// The Put CoWs and re-encodes the root leaf with the post-Compact
	// pager's builder config.
	if err := ks2.Put([]byte("b"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if got := tx2.pgr.PageRaw(ks2.desc.Root)[0]; got != page.TypeLeafUncompressed {
		t.Errorf("post-Compact leaf type = %d, want %d (uncompressed) — "+
			"builder defaults lost across Compact's pager reopen", got, page.TypeLeafUncompressed)
	}
}

// TestCompactImageHonorsLayoutDefaults pins the remaining two
// engine-wide defaults in the compacted image: with
// Options.LeafLayout = interleaved a single-leaf keyspace must come
// out of Compact as TypeLeaf (interleaved compressed), and with
// Options.BranchLayout = plain a multi-level keyspace's root branch
// must come out as TypeBranch (plain) — not the segregated engine
// defaults.
func TestCompactImageHonorsLayoutDefaults(t *testing.T) {
	ctx := context.Background()

	t.Run("leaf", func(t *testing.T) {
		db, err := Open(ctx, tmpPath(t), Options{
			PageSize: 4096, MinSize: 16, MaxSize: 4096,
			LeafLayout: LeafLayoutInterleaved,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ks, err := tx.CreateKeyspace("ks")
		if err != nil {
			t.Fatal(err)
		}
		if err := ks.Put([]byte("a"), []byte("v")); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := db.Compact(); err != nil {
			t.Fatalf("Compact: %v", err)
		}
		tx2, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx2.Rollback()
		ks2, err := tx2.OpenKeyspace("ks")
		if err != nil {
			t.Fatal(err)
		}
		if got := tx2.pgr.PageRaw(ks2.desc.Root)[0]; got != page.TypeLeaf {
			t.Errorf("compacted-image leaf type = %d, want %d (interleaved)",
				got, page.TypeLeaf)
		}
	})

	t.Run("branch", func(t *testing.T) {
		db, err := Open(ctx, tmpPath(t), Options{
			PageSize: 4096, MinSize: 16, MaxSize: 4096,
			BranchLayout: BranchLayoutPlain,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		// Enough surviving keys that the COMPACTED tree still needs a
		// branch root (buildChurnedDB deletes down to a single leaf).
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ks, err := tx.CreateKeyspace("k")
		if err != nil {
			t.Fatal(err)
		}
		for i := range 3000 {
			if err := ks.Put(fmt.Appendf(nil, "key%06d", i), fmt.Appendf(nil, "val%06d", i)); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := db.Compact(); err != nil {
			t.Fatalf("Compact: %v", err)
		}
		tx2, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx2.Rollback()
		ks2, err := tx2.OpenKeyspace("k")
		if err != nil {
			t.Fatal(err)
		}
		if got := tx2.pgr.PageRaw(ks2.desc.Root)[0]; got != page.TypeBranch {
			t.Errorf("compacted-image root branch type = %d, want %d (plain)",
				got, page.TypeBranch)
		}
	})
}
