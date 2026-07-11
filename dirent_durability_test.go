package gmdb

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestOpenSyncsParentDir pins the Open and CopyTo halves of
// durability.md §Directory-entry durability: every writable Open
// fsyncs the parent directory (creation and reopen alike — the
// existing-file path must re-establish the obligation), read-only
// opens skip it, a dir-fsync failure fails the Open rather than
// handing back a handle whose file can vanish in a crash, and CopyTo
// makes its output's dirent durable.
func TestOpenSyncsParentDir(t *testing.T) {
	ctx := context.Background()

	t.Run("create-syncs-dir", func(t *testing.T) {
		path := tmpPath(t)
		var synced []string
		restore := SetSyncDirHookForTest(func(dir string) error {
			synced = append(synced, dir)
			return nil
		})
		defer restore()
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer db.Close()
		want := filepath.Dir(path)
		if len(synced) == 0 || synced[0] != want {
			t.Fatalf("syncDir calls = %v, want first call on %q", synced, want)
		}
	})

	t.Run("dir-sync-failure-fails-open", func(t *testing.T) {
		path := tmpPath(t)
		injected := errors.New("injected dir fsync failure")
		restore := SetSyncDirHookForTest(func(string) error { return injected })
		defer restore()
		if db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128}); !errors.Is(err, injected) {
			if err == nil {
				db.Close()
			}
			t.Fatalf("Open = %v, want the injected dir-fsync failure", err)
		}
	})

	t.Run("every-writable-open-syncs-dir", func(t *testing.T) {
		// Not creation-only: the create-retry after a failed dir
		// fsync and an Open racing a crashed creator both land on the
		// EEXIST-fallback path, so every writable Open re-establishes
		// the dirent obligation.
		path := tmpPath(t)
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open(create): %v", err)
		}
		db.Close()
		var synced int
		restore := SetSyncDirHookForTest(func(string) error { synced++; return nil })
		defer restore()
		db, err = Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open(reopen): %v", err)
		}
		db.Close()
		if synced != 1 {
			t.Errorf("writable reopen synced the dir %d times, want 1", synced)
		}
		// Read-only opens skip the sync (nothing to make durable;
		// read-only media would EROFS).
		synced = 0
		db, err = Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128, ReadOnly: true})
		if err != nil {
			t.Fatalf("Open(read-only): %v", err)
		}
		defer db.Close()
		if synced != 0 {
			t.Errorf("read-only open synced the dir %d times, want 0", synced)
		}
	})

	t.Run("copyto-syncs-target-dir", func(t *testing.T) {
		path := tmpPath(t)
		db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer db.Close()
		target := filepath.Join(t.TempDir(), "copy.gmdb")
		var synced []string
		restore := SetSyncDirHookForTest(func(dir string) error {
			synced = append(synced, dir)
			return nil
		})
		defer restore()
		if err := db.CopyTo(target, true); err != nil {
			t.Fatalf("CopyTo: %v", err)
		}
		want := filepath.Dir(target)
		found := false
		for _, d := range synced {
			if d == want {
				found = true
			}
		}
		if !found {
			t.Errorf("CopyTo synced %v; want the target parent %q among them", synced, want)
		}
	})
}

// TestCompactDirSyncFailurePoisons pins the rename half: a failed
// post-rename directory fsync leaves the on-disk outcome unknowable,
// so Compact must poison the handle (Close + re-Open recovers).
func TestCompactDirSyncFailurePoisons(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("k")
		if err != nil {
			return err
		}
		return ks.Put([]byte("a"), []byte("v"))
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Compact's only dir sync is the post-rename one this test targets
	// (the tmp copy's dirent needs no durability — a crash pre-rename
	// leaves the original intact plus an inert temp).
	injected := errors.New("injected dir fsync failure")
	calls := 0
	restore := SetSyncDirHookForTest(func(string) error {
		calls++
		return injected
	})
	err = db.Compact()
	restore()
	if !errors.Is(err, injected) {
		t.Fatalf("Compact = %v (sync calls=%d), want the injected post-rename dir-fsync failure", err, calls)
	}
	if _, err := db.Begin(ctx); !errors.Is(err, ErrPoisoned) {
		t.Errorf("Begin after failed Compact dir fsync: %v, want ErrPoisoned", err)
	}

	// Close + re-Open converges (the rename either survived or not;
	// both are consistent single-inode states).
	db.Close()
	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	if err := db2.View(ctx, func(rtx *ReadTx) error {
		ks, err := rtx.OpenKeyspaceReadOnly("k")
		if err != nil {
			return err
		}
		_, err = ks.Get([]byte("a"))
		return err
	}); err != nil {
		t.Fatalf("read after re-Open: %v", err)
	}
}
