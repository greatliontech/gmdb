package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/greatliontech/gmdb/internal/lock"
)

// seedDB creates a writable DB at path, commits one keyspace
// "ks" with hello→world, and closes it. Shared setup for the
// read-only reopen tests.
func seedDB(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("ks")
		if err != nil {
			return err
		}
		return ks.Put([]byte("hello"), []byte("world"))
	}); err != nil {
		t.Fatalf("seed Update: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}
}

// readHello opens a read snapshot and asserts ks/hello == world.
func readHello(t *testing.T, db *DB) {
	t.Helper()
	if err := db.View(context.Background(), func(rtx *ReadTx) error {
		ks, err := rtx.OpenKeyspaceReadOnly("ks")
		if err != nil {
			return err
		}
		got, err := ks.Get([]byte("hello"))
		if err != nil {
			return err
		}
		if !bytes.Equal(got, []byte("world")) {
			return fmt.Errorf("Get = %q, want %q", got, "world")
		}
		return nil
	}); err != nil {
		t.Fatalf("read-only View: %v", err)
	}
}

// TestReadOnlyReadsWorkWritesRejected is the core acceptance: a
// read-only reopen serves reads from the committed snapshot and
// rejects every write entry point with ErrDatabaseReadOnly.
func TestReadOnlyReadsWorkWritesRejected(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	seedDB(t, path)

	db, err := Open(ctx, path, Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only Open: %v", err)
	}
	defer db.Close()

	// Reads work.
	readHello(t, db)

	// Every write entry point rejects with ErrDatabaseReadOnly.
	if _, err := db.Begin(ctx); !errors.Is(err, ErrDatabaseReadOnly) {
		t.Errorf("Begin: got %v, want ErrDatabaseReadOnly", err)
	}
	if err := db.Update(ctx, func(*Tx) error { return nil }); !errors.Is(err, ErrDatabaseReadOnly) {
		t.Errorf("Update: got %v, want ErrDatabaseReadOnly", err)
	}
	if err := db.Batch(ctx, func(*Tx) error { return nil }); !errors.Is(err, ErrDatabaseReadOnly) {
		t.Errorf("Batch: got %v, want ErrDatabaseReadOnly", err)
	}
	if err := db.Compact(); !errors.Is(err, ErrDatabaseReadOnly) {
		t.Errorf("Compact: got %v, want ErrDatabaseReadOnly", err)
	}
	if err := db.Checkpoint(ctx); !errors.Is(err, ErrDatabaseReadOnly) {
		t.Errorf("Checkpoint: got %v, want ErrDatabaseReadOnly", err)
	}
}

// TestReadOnlyMissingFileNotCreated confirms a read-only Open of a
// nonexistent path returns os.ErrNotExist (wrapped) and never creates
// the file — read-only must not bring a database into existence.
func TestReadOnlyMissingFileNotCreated(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)

	_, err := Open(ctx, path, Options{ReadOnly: true})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only Open of missing file: got %v, want os.ErrNotExist", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("read-only Open created %q (stat err = %v); it must not", path, statErr)
	}
	// The sibling lock file must not have been created either.
	lockPath := filepath.Join(filepath.Dir(path), lock.BaseFor(filepath.Base(path)))
	if _, statErr := os.Stat(lockPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("read-only Open created lock file %q; it must not", lockPath)
	}
}

// TestReadOnlyPinsReaderSlot is the Posture-B safety property: when the
// lock file is writable, a read-only handle still opens it and read
// transactions acquire reader slots so a concurrent cross-process
// writer cannot reclaim under them. White-box via the unexported coord.
func TestReadOnlyPinsReaderSlot(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	seedDB(t, path)

	db, err := Open(ctx, path, Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only Open: %v", err)
	}
	defer db.Close()

	if db.coord == nil {
		t.Fatal("read-only Open on writable media left coord nil; expected lock-file participation")
	}
	if n := db.coord.ActiveReaderSlots(); n != 0 {
		t.Fatalf("ActiveReaderSlots before BeginRead = %d, want 0", n)
	}
	rtx, err := db.BeginRead(ctx)
	if err != nil {
		t.Fatalf("BeginRead: %v", err)
	}
	if n := db.coord.ActiveReaderSlots(); n != 1 {
		t.Errorf("ActiveReaderSlots with one read tx = %d, want 1 (slot not pinned)", n)
	}
	if err := rtx.Commit(); err != nil {
		t.Fatalf("read tx Commit: %v", err)
	}
	if n := db.coord.ActiveReaderSlots(); n != 0 {
		t.Errorf("ActiveReaderSlots after close = %d, want 0 (slot not released)", n)
	}
}

// TestReadOnlyLockFreeFallback exercises the read-only-media fallback:
// when the lock file cannot be opened read-write, Open proceeds
// lock-free (coord == nil) and reads still work. Simulated by making
// the lock file unwritable. Skipped as root, which bypasses file
// permissions.
func TestReadOnlyLockFreeFallback(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions; cannot force a lock-file open failure")
	}
	ctx := context.Background()
	path := tmpPath(t)
	seedDB(t, path)

	// Make the existing lock file unwritable so lock.Open's O_RDWR open
	// fails with EACCES, forcing the read-only lock-free fallback.
	lockPath := filepath.Join(filepath.Dir(path), lock.BaseFor(filepath.Base(path)))
	if err := os.Chmod(lockPath, 0o444); err != nil {
		t.Fatalf("chmod lock file: %v", err)
	}
	// Restore before TempDir cleanup (defensive; unlink only needs dir
	// write, but keep perms sane).
	t.Cleanup(func() { _ = os.Chmod(lockPath, 0o644) })

	db, err := Open(ctx, path, Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only Open (fallback): %v", err)
	}
	defer db.Close()

	if db.coord != nil {
		t.Error("expected lock-free fallback (coord == nil) when lock file is unwritable")
	}
	// Reads must still work without a coord / reader slot.
	readHello(t, db)
}
