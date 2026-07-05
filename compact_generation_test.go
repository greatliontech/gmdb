package gmdb

import (
	"context"
	"errors"
	"testing"
)

// TestPeerCompactPoisonsStaleHandles pins the data-file generation
// contract (cross-process.md §Data-file generation): after handle A
// compacts, handle B — still mapping the replaced inode — must be
// refused with ErrPoisoned on its next write, read, and checkpoint,
// never allowed to commit to the unlinked file (writes invisible to
// every other process). Close + re-Open converges B on the new inode.
func TestPeerCompactPoisonsStaleHandles(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	open := func() *DB {
		db, err := Open(ctx, path, Options{
			PageSize: 4096, MinSize: 16, MaxSize: 256,
			Maintenance: MaintenanceOptions{Disable: true},
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return db
	}
	a := open()
	defer a.Close()
	if err := a.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("k")
		if err != nil {
			return err
		}
		return ks.Put([]byte("seed"), []byte("v0"))
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	b := open() // peer handle on the same inode
	defer b.Close()
	c := open() // second peer: its FIRST post-compact op is a read
	defer c.Close()
	d := open() // third peer: its FIRST post-compact op is a checkpoint
	defer d.Close()
	e := open() // fourth peer: its FIRST post-compact op is a compact
	defer e.Close()

	if err := a.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	// Each entry point's OWN generation check must fire on a handle
	// nothing has poisoned yet (an earlier refused op would mask the
	// site under test via the poison fast-path).
	if err := d.Checkpoint(ctx); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("fresh peer Checkpoint after Compact: %v, want ErrPoisoned", err)
	}
	if err := e.Compact(); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("fresh peer Compact after Compact: %v, want ErrPoisoned", err)
	}

	// The read path's own generation check must fire on a handle no
	// prior write has poisoned (the write path would otherwise mask
	// it — a surviving mutation caught exactly that).
	if _, err := c.BeginRead(ctx); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("fresh peer BeginRead after Compact: %v, want ErrPoisoned", err)
	}

	// B's next transaction-opening operations are refused.
	if err := b.Update(ctx, func(tx *Tx) error {
		ks, err := tx.OpenKeyspace("k")
		if err != nil {
			return err
		}
		return ks.Put([]byte("lost"), []byte("x"))
	}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("peer Update after Compact: %v, want ErrPoisoned", err)
	}
	if _, err := b.BeginRead(ctx); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("peer BeginRead after Compact: %v, want ErrPoisoned", err)
	}
	if err := b.Checkpoint(ctx); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("peer Checkpoint after Compact: %v, want ErrPoisoned", err)
	}

	// A keeps working (its cache advanced with its own bump).
	if err := a.Update(ctx, func(tx *Tx) error {
		ks, err := tx.OpenKeyspace("k")
		if err != nil {
			return err
		}
		return ks.Put([]byte("after"), []byte("v1"))
	}); err != nil {
		t.Fatalf("compacting handle Update: %v", err)
	}

	// B converges via Close + re-Open: sees A's post-compact write,
	// and no trace of the refused write.
	if err := b.Close(); err != nil {
		t.Fatalf("Close(B): %v", err)
	}
	b2 := open()
	defer b2.Close()
	if err := b2.View(ctx, func(rtx *ReadTx) error {
		ks, err := rtx.OpenKeyspaceReadOnly("k")
		if err != nil {
			return err
		}
		if _, err := ks.Get([]byte("after")); err != nil {
			return err
		}
		if _, err := ks.Get([]byte("lost")); !errors.Is(err, ErrNotFound) {
			t.Errorf("refused write visible after reopen: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("reopened peer view: %v", err)
	}
}


// TestOpenRacingPeerCompactRetries pins the Open-time inode
// verification: an Open whose data fd was acquired just before a
// peer's Compact renamed the file would cache the post-bump
// generation while mapping the replaced inode — every per-op check
// passing forever. openAttempt must detect fd-vs-path divergence and
// Open must retry onto the new inode.
func TestOpenRacingPeerCompactRetries(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	a, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	if err != nil {
		t.Fatalf("Open(a): %v", err)
	}
	defer a.Close()
	if err := a.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("k")
		if err != nil {
			return err
		}
		return ks.Put([]byte("seed"), []byte("v0"))
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Simulate the race deterministically: compact AFTER the racing
	// Open's fd would have been acquired. The syncDir hook fires
	// during b's Open (creation-free reopen doesn't create, but every
	// writable Open dir-syncs — use that as the mid-Open seam BEFORE
	// the lock/generation stage).
	compacted := false
	restore := SetSyncDirHookForTest(func(string) error {
		if !compacted {
			compacted = true
			if err := a.Compact(); err != nil {
				t.Errorf("mid-open Compact: %v", err)
			}
		}
		return nil
	})
	defer restore()
	b, err := Open(ctx, path, Options{
		PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true},
	})
	restore()
	if err != nil {
		t.Fatalf("Open(b) racing Compact: %v", err)
	}
	defer b.Close()
	if !compacted {
		t.Fatalf("fixture: the mid-open Compact never ran")
	}

	// b must be usable and coherent with a — proof it landed on the
	// post-compact inode, not the unlinked one.
	if err := b.Update(ctx, func(tx *Tx) error {
		ks, err := tx.OpenKeyspace("k")
		if err != nil {
			return err
		}
		return ks.Put([]byte("from-b"), []byte("v1"))
	}); err != nil {
		t.Fatalf("b.Update: %v", err)
	}
	if err := a.View(ctx, func(rtx *ReadTx) error {
		ks, err := rtx.OpenKeyspaceReadOnly("k")
		if err != nil {
			return err
		}
		_, err = ks.Get([]byte("from-b"))
		return err
	}); err != nil {
		t.Fatalf("a cannot see b's write — fork: %v", err)
	}
}
