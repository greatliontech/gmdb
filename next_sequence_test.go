package gmdb

import (
	"context"
	"errors"
	"testing"
)

// TestNextSequenceKeyspace covers the monotonic per-keyspace counter:
// in-tx monotonicity from 1, persistence across commits and a Close/reopen,
// rollback-discards-the-increment, and read-only rejection.
func TestNextSequenceKeyspace(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	opts := Options{PageSize: 4096, MinSize: 16, MaxSize: 256}
	db, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("ks")
		if e != nil {
			return e
		}
		for want := uint64(1); want <= 3; want++ {
			got, e := ks.NextSequence()
			if e != nil {
				return e
			}
			if got != want {
				t.Fatalf("NextSequence = %d, want %d", got, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Persisted by the previous commit → continues monotonically.
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.OpenKeyspace("ks")
		if e != nil {
			return e
		}
		if got, e := ks.NextSequence(); e != nil || got != 4 {
			t.Fatalf("cross-commit NextSequence = %d, %v; want 4, nil", got, e)
		}
		return nil
	}); err != nil {
		t.Fatalf("Update 2: %v", err)
	}

	// Survives Close + reopen.
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db, err = Open(ctx, path, opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.OpenKeyspace("ks")
		if e != nil {
			return e
		}
		if got, e := ks.NextSequence(); e != nil || got != 5 {
			t.Fatalf("post-reopen NextSequence = %d, %v; want 5, nil", got, e)
		}
		return nil
	}); err != nil {
		t.Fatalf("Update 3: %v", err)
	}

	// Rollback discards the increment (the on-disk counter stays at 5).
	sentinel := errors.New("rollback")
	_ = db.Update(ctx, func(tx *Tx) error {
		ks, _ := tx.OpenKeyspace("ks")
		_, _ = ks.NextSequence() // would be 6, but the tx rolls back
		return sentinel
	})
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, e := tx.OpenKeyspace("ks")
		if e != nil {
			return e
		}
		if got, e := ks.NextSequence(); e != nil || got != 6 {
			t.Fatalf("post-rollback NextSequence = %d, %v; want 6, nil (rollback must discard)", got, e)
		}
		return nil
	}); err != nil {
		t.Fatalf("Update 4: %v", err)
	}

	// Read-only handle rejects.
	if err := db.View(ctx, func(rtx *ReadTx) error {
		ks, e := rtx.OpenKeyspaceReadOnly("ks")
		if e != nil {
			return e
		}
		if _, e := ks.NextSequence(); !errors.Is(e, ErrReadOnly) {
			t.Fatalf("read-only NextSequence err = %v, want ErrReadOnly", e)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestNextSequenceSetKeyspace confirms the promoted method also works on the
// Kind=1 (set) keyspace.
func TestNextSequenceSetKeyspace(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx *Tx) error {
		sks, e := tx.CreateSetKeyspace("set", nil)
		if e != nil {
			return e
		}
		for want := uint64(1); want <= 3; want++ {
			got, e := sks.NextSequence()
			if e != nil {
				return e
			}
			if got != want {
				t.Fatalf("SetKeyspace.NextSequence = %d, want %d", got, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}
