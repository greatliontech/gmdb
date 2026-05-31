package gmdb

import (
	"context"
	"errors"
	"testing"
)

// TestSetFileFormat: the mutable file-format params are persisted (set →
// commit → reopen → read back), MaxSize is rejected as immutable, and
// non-page-multiple sizes are rejected.
func TestSetFileFormat(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	const ps = 4096
	opts := Options{PageSize: ps, MinSize: 64, MaxSize: 4096, GrowStep: 64, ShrinkThreshold: 128}
	db, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	newFF := FileFormat{Lower: 32 * ps, Upper: 4096 * ps, GrowStep: 16 * ps, ShrinkThreshold: 256 * ps}
	if err := db.Update(ctx, func(tx *Tx) error { return tx.SetFileFormat(newFF) }); err != nil {
		t.Fatalf("SetFileFormat: %v", err)
	}
	// Persisted into the committed meta (white-box; in pages).
	if m := db.currentMeta; m.MinSize != 32 || m.GrowStep != 16 || m.ShrinkThreshold != 256 {
		t.Fatalf("meta after SetFileFormat: MinSize=%d GrowStep=%d Shrink=%d; want 32/16/256", m.MinSize, m.GrowStep, m.ShrinkThreshold)
	}
	if db.currentMeta.MaxSize != 4096 {
		t.Fatalf("MaxSize = %d, must stay 4096 (immutable)", db.currentMeta.MaxSize)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Survives reopen — the persisted (not the Options) values win.
	db2, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if m := db2.currentMeta; m.MinSize != 32 || m.GrowStep != 16 || m.ShrinkThreshold != 256 {
		t.Fatalf("post-reopen meta: MinSize=%d GrowStep=%d Shrink=%d; want 32/16/256", m.MinSize, m.GrowStep, m.ShrinkThreshold)
	}

	// MaxSize is immutable.
	if err := db2.Update(ctx, func(tx *Tx) error {
		return tx.SetFileFormat(FileFormat{Lower: 32 * ps, Upper: 2048 * ps, GrowStep: 16 * ps, ShrinkThreshold: 256 * ps})
	}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("SetFileFormat with changed Upper: err = %v, want ErrInvalidOptions", err)
	}

	// Non-page-multiple is rejected.
	if err := db2.Update(ctx, func(tx *Tx) error {
		return tx.SetFileFormat(FileFormat{Lower: 32*ps + 1, Upper: 4096 * ps, GrowStep: 16 * ps, ShrinkThreshold: 256 * ps})
	}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("SetFileFormat with non-page-multiple Lower: err = %v, want ErrInvalidOptions", err)
	}

	// Lower below the metas+bitmap floor is rejected.
	if err := db2.Update(ctx, func(tx *Tx) error {
		return tx.SetFileFormat(FileFormat{Lower: 1 * ps, Upper: 4096 * ps, GrowStep: 16 * ps, ShrinkThreshold: 256 * ps})
	}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("SetFileFormat with sub-floor Lower: err = %v, want ErrInvalidOptions", err)
	}

	// A SetFileFormat whose tx rolls back is discarded (meta unchanged).
	sentinel := errors.New("rollback")
	_ = db2.Update(ctx, func(tx *Tx) error {
		_ = tx.SetFileFormat(FileFormat{Lower: 48 * ps, Upper: 4096 * ps, GrowStep: 8 * ps, ShrinkThreshold: 64 * ps})
		return sentinel
	})
	if m := db2.currentMeta; m.MinSize != 32 || m.GrowStep != 16 || m.ShrinkThreshold != 256 {
		t.Fatalf("after rolled-back SetFileFormat: MinSize=%d GrowStep=%d Shrink=%d; want 32/16/256 (rollback must discard)", m.MinSize, m.GrowStep, m.ShrinkThreshold)
	}
}
