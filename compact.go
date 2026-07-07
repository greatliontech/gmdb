package gmdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

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
	// 1. Shared write-grant preamble (fast gates, acquisition,
	// post-grant poison/close/generation re-checks). Blocks behind any
	// in-flight writer (including the batch coordinator's tx) until it
	// commits. For Compact the generation check is fail-fast only:
	// even without it the copy phase's own BeginRead fires the
	// read-path generation check before anything is renamed (the
	// peer-Compact test pins that outcome); the earlier exit just
	// skips the reader-drain wait.
	grant, coord, err := db.acquireWriteGrant(context.Background())
	if err != nil {
		return err
	}
	defer grant.Release()

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
	base := filepath.Base(db.path)
	tmpPath := db.path + ".compact"
	_ = os.Remove(tmpPath) // clear any stale temp from a crashed Compact

	rtx, err := db.BeginRead(context.Background())
	if err != nil {
		return err
	}
	srcUUID := rtx.meta.UUID
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
	// Publish the replacement to every peer handle (cross-process.md
	// §Data-file generation) IMMEDIATELY after the rename: the live
	// directory entry already changed, so peers must converge from
	// this instant regardless of what the fsync below does — a
	// dir-fsync failure poisons only this handle, never un-renames.
	// Bumped under the grant, before our own reopen (even if that
	// fails and poisons us, peers still observe the replacement). Our
	// cache updates in the same step so this handle's later checks
	// pass.
	db.dataGeneration.Store(coord.BumpDataGeneration())
	// The rename is durable only once the parent directory is fsynced;
	// on failure the on-disk outcome is unknowable across a crash (the
	// old inode may resurrect) — same failure class as the reopen path
	// below: poison, Close + re-Open recovers.
	if err := syncDir(db.root); err != nil {
		db.poisoned.Store(true)
		return fmt.Errorf("gmdb: Compact dir fsync (handle poisoned — Close and re-Open): %w", err)
	}

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
	// Meta baseline mirrors Open (adoptOpened): a compacted file is
	// fully durable with a checkpoint meta; the NoCheckpoint guard is
	// robustness only.
	db.adoptOpened(opened)
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

// syncDir fsyncs the directory so a created or renamed entry survives
// a crash (POSIX: dirents are durable only after the parent directory
// is fsynced — durability.md §Directory-entry durability). Callers
// treat failure as fatal for their operation: an unsynced dirent means
// a crash can lose the file (create) or resurrect the replaced inode
// (Compact rename) despite acked SyncDurable commits.
// syncDir fsyncs through the handle's pinned os.Root, so the fsync
// hits the SAME directory the data file was opened under even if a
// path component was re-pointed since (the symlink-guard rationale on
// DB.root).
func syncDir(root *os.Root) error {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	if hook := syncDirHookForTest.Load(); hook != nil {
		return (*hook)(root.Name())
	}
	return nil
}

// syncDirPath is the path-addressed variant for targets with no
// pinned root (CopyTo's freshly-created output file).
func syncDirPath(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	if hook := syncDirHookForTest.Load(); hook != nil {
		return (*hook)(dir)
	}
	return nil
}

// syncDirHookForTest observes/injects after syncDir's real fsync
// succeeded — the dirent-durability contract's test seam.
var syncDirHookForTest atomic.Pointer[func(dir string) error]
