package gmdb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/thegrumpylion/gmdb/internal/lock"
	"github.com/thegrumpylion/gmdb/internal/pager"
)

// compactDrainPoll is the spin interval while waiting for in-process
// readers to drain.
const compactDrainPoll = 2 * time.Millisecond

// Compact rebuilds the database file in place: a compacting copy
// (CopyTo(compact=true), preserving the UUID) is written to a temp file in
// the same directory, then atomically renamed over the original, and the
// handle's pager is reopened against the new inode. It reclaims leaked
// pages, defragments, and shrinks the file. See api-surface.md §Compact.
//
// Coordination (Compact is the most invasive single-process operation; the
// caller need not ensure no transactions are open — Compact arranges it):
//
//  1. Acquire the cross-process write lock (blocking concurrent writers +
//     Checkpoint for the duration).
//  2. Wait up to Options.CompactDrainTimeout for active IN-PROCESS read
//     transactions to finish. If any remain, abort with
//     ErrCompactReadersActive — no temp file, no rename. (Cross-process
//     readers are NOT drained; they keep working against the original
//     inode, which stays alive until their mappings drop.)
//  3. Write the compacted copy to a temp file (UUID preserved) and fsync.
//  4. Atomic rename over the original; fsync the directory; reopen the
//     pager against the new inode; release the write lock.
//
// On ErrCompactReadersActive, fall back to CopyTo(path, compact=true) to
// produce an offline compacted copy without draining in-process readers.
//
// Requires free disk space for the temp copy (up to the live data size) on
// the SAME filesystem as the database file (otherwise the rename is not
// atomic).
//
// Compact MUST NOT be called from a goroutine that currently holds an open
// write transaction on this DB: Compact acquires the cross-process write
// lock and would block forever behind the grant the caller already holds.
// (Compact does not need the caller to ensure OTHER transactions are
// closed — it serialises them via the lock — but it cannot wait on the
// caller's own grant.) If the reopen after the rename fails, the handle is
// poisoned (subsequent ops return ErrPoisoned); Close and re-Open recovers
// against the renamed file.
func (db *DB) Compact() error {
	if db.closeGate.IsClosed() {
		return ErrClosed
	}
	if db.poisoned.Load() {
		return ErrPoisoned
	}
	db.mu.Lock()
	coord := db.coord
	db.mu.Unlock()
	if coord == nil {
		return ErrClosed
	}

	// 1. Acquire the cross-process write lock. Blocks behind any in-flight
	// writer (including the batch coordinator's tx) until it commits.
	grant, err := coord.AcquireWriter(context.Background())
	if err != nil {
		if errors.Is(err, lock.ErrClosed) {
			return ErrClosed
		}
		return err
	}
	defer grant.Release()

	// Re-check after acquiring the grant — a concurrent commit could have
	// poisoned the handle, or Close could have run while we waited.
	if db.poisoned.Load() {
		return ErrPoisoned
	}
	if db.closeGate.IsClosed() {
		return ErrClosed
	}

	// 2. Drain in-process readers (bounded by CompactDrainTimeout).
	deadline := time.Now().Add(db.opts.CompactDrainTimeout)
	for coord.ActiveReaderSlots() > 0 {
		if time.Now().After(deadline) {
			return ErrCompactReadersActive
		}
		time.Sleep(compactDrainPoll)
	}

	// 3. Write the compacted copy to a temp file beside the original,
	// preserving the UUID (the renamed file IS this database).
	dir := filepath.Dir(db.path)
	base := filepath.Base(db.path)
	tmpPath := db.path + ".compact"
	_ = os.Remove(tmpPath) // clear any stale temp from a crashed Compact

	rtx, err := db.BeginRead(context.Background())
	if err != nil {
		return err
	}
	srcUUID := rtx.Meta().UUID
	cerr := copyCompact(rtx, tmpPath, srcUUID)
	_ = rtx.Rollback()
	if cerr != nil {
		return fmt.Errorf("gmdb: Compact copy: %w", cerr)
	}

	// 4. Atomic rename over the original, then make the rename durable, then
	// reopen the handle's pager against the new inode.
	if err := os.Rename(tmpPath, db.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("gmdb: Compact rename: %w", err)
	}
	syncDir(dir, db.logger)

	if err := db.reopenAfterCompact(base); err != nil {
		return err
	}
	return nil
}

// reopenAfterCompact swaps the handle's data-file fd + writer pager to the
// post-rename inode, keeping the (unrenamed) lock file + Coord + write
// grant alive. Called only under the write grant with readers drained.
func (db *DB) reopenAfterCompact(base string) error {
	// Open the new inode through the same os.Root as Open (symlink guard).
	// A failure here is AFTER the rename: db.path is the new inode but this
	// handle still maps the old (now-unlinked) one. Poison the handle so
	// every subsequent op fails with ErrPoisoned rather than silently
	// serving the stale inode while other processes see the new one (the
	// split-brain the api-surface.md §Compact all-or-nothing invariant
	// forbids). Close + re-Open recovers against the new inode.
	newFile, err := db.root.OpenFile(base, os.O_RDWR, 0o600)
	if err != nil {
		db.poisoned.Store(true)
		return fmt.Errorf("gmdb: Compact reopen file (handle poisoned — Close and re-Open): %w", err)
	}
	opened, err := pager.Open(newFile, pager.OpenParams{
		Pool:             db.pool, // PageSize is preserved across compaction
		MaxTxBufferBytes: db.opts.MaxTxBufferBytes,
	})
	if err != nil {
		_ = newFile.Close()
		db.poisoned.Store(true)
		return fmt.Errorf("gmdb: Compact reopen pager (handle poisoned — Close and re-Open): %w", mapPagerErr(err))
	}

	// Swap the live fields atomically vs. a concurrent Begin/BeginRead
	// (which snapshot db.pgr/db.file/db.currentMeta under db.mu).
	db.mu.Lock()
	oldFile := db.file
	oldPgr := db.pgr
	db.file = newFile
	db.pgr = opened.Pager
	db.currentMeta = opened.Meta
	db.activeMetaIdx = opened.ActiveMetaIdx
	db.mu.Unlock()

	// Re-point the DB leak-detection cleanup at the new pager + file (the
	// old cleanup info captured the now-closed ones). coord / lockFile /
	// root / gate are unchanged.
	db.cleanup.Stop()
	db.cleanup = runtime.AddCleanup(db, dbCleanupFn, dbCleanupInfo{
		gate:      db.closeGate,
		coord:     db.coord,
		lockFile:  db.lockFile,
		pgr:       opened.Pager,
		file:      newFile,
		root:      db.root,
		logger:    db.logger,
		originPCs: captureOriginPCs(),
	})

	// Release the old mapping + fd. The old inode (now unlinked by the
	// rename) is freed once the last mapping drops — any cross-process
	// reader still on it keeps it alive in their address space, never here.
	_ = oldPgr.Close()
	_ = oldFile.Close()
	return nil
}

// syncDir fsyncs the directory so a rename survives a crash (POSIX:
// renaming is durable only after the parent directory is fsynced).
// Best-effort: a failure is logged, not fatal — the rename has already
// occurred in the page cache.
func syncDir(dir string, logger *slog.Logger) {
	d, err := os.Open(dir)
	if err != nil {
		logger.Warn("gmdb: Compact could not open dir for fsync", "dir", dir, "err", err)
		return
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		logger.Warn("gmdb: Compact dir fsync failed", "dir", dir, "err", err)
	}
}
