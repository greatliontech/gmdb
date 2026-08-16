package gmdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/greatliontech/gmdb/internal/pager"
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
//     readers are NOT drained; on unix they keep working against the
//     original inode, which stays alive until their mappings drop. On
//     windows any peer mapping makes the rename below fail cleanly —
//     the kernel-enforced sole-mapper gate, api-surface.md §Check,
//     CopyTo, Compact.)
//  3. Write the compacted copy to a temp file (UUID preserved) and fsync.
//  4. Teardown-before-rename: close this handle's own pager (releasing
//     its mapping), atomically rename over the original via the
//     confined os.Root form, fsync the directory, reopen the pager
//     against the new inode, release the write lock. A refused rename
//     (windows sole-mapper gate) restores the pager against the
//     original file and returns a clean, retryable error.
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
	tmpBase := base + ".compact"
	// One namespace for the whole publish: the temp is created,
	// removed, and renamed through db.root, so a re-pointed path
	// component after Open cannot make the rename pick up a different
	// file than the copy wrote (the same symlink-guard rationale as
	// the data-file open).
	_ = db.root.Remove(tmpBase) // clear any stale temp from a crashed Compact

	rtx, err := db.BeginRead(context.Background())
	if err != nil {
		return err
	}
	srcUUID := rtx.meta.UUID
	cerr := publicChecksumErr(copyCompact(rtx,
		func() (*os.File, error) {
			return db.root.OpenFile(tmpBase, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		},
		func() { _ = db.root.Remove(tmpBase) },
		srcUUID))
	_ = rtx.Rollback()
	if cerr != nil {
		return fmt.Errorf("gmdb: Compact copy: %w", cerr)
	}

	// 4. Teardown-before-rename (api-surface.md §Check, CopyTo,
	// Compact): close our own writer pager FIRST — its mapping is what
	// makes the kernel refuse the replace-rename on windows, and under
	// the grant with readers drained the teardown is equally sound
	// everywhere. The nil-pager window is the same state DB.Close
	// establishes; every db.pgr reader tolerates it. The data-file fd
	// stays open — an unmapped open handle with share-delete (os.Root
	// opens) does not block a POSIX-semantics rename.
	db.mu.Lock()
	oldPgr := db.pgr
	db.pgr = nil
	db.mu.Unlock()
	_ = oldPgr.Close()

	// Atomic rename over the original — os.Root's confined form
	// (symlink guard; POSIX semantics on windows). On windows the
	// kernel refuses while ANY other mapping of the file exists (a
	// peer handle maps at Open; a read snapshot that began after the
	// drain counts too) — the sole-mapper gate. The refusal is clean
	// and retryable: restore the handle against the original file,
	// which the failed rename left in place under its unchanged name.
	renameFn := db.root.Rename
	if hook := compactRenameHookForTest.Load(); hook != nil {
		renameFn = *hook
	}
	if err := renameFn(tmpBase, base); err != nil {
		_ = db.root.Remove(tmpBase)
		if rerr := db.installCompactPager(db.file, "restore"); rerr != nil {
			return fmt.Errorf("gmdb: Compact rename: %w (%v)", err, rerr)
		}
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
	if err := syncDir(db.root, base, db.opts.NoFullFsync); err != nil {
		db.poisoned.Store(true)
		return fmt.Errorf("gmdb: Compact dir fsync (handle poisoned — Close and re-Open): %w", err)
	}

	// Reopen against the new inode through the same os.Root (symlink
	// guard) and install it as the live pager.
	newFile, err := db.root.OpenFile(base, os.O_RDWR, 0o600)
	if err != nil {
		db.poisoned.Store(true)
		return fmt.Errorf("gmdb: Compact reopen file (handle poisoned — Close and re-Open): %w", err)
	}
	return db.installCompactPager(newFile, "reopen")
}

// compactRenameHookForTest, when set, replaces the publish rename —
// the seam for exercising the rename-refusal restore path (the
// windows sole-mapper gate) on any platform. Same non-parallel rule
// as the other compact seams.
var compactRenameHookForTest atomic.Pointer[func(oldname, newname string) error]

// installCompactPager (re)builds the writer pager over file and
// installs it as the handle's live pager — the shared tail of
// Compact's publish reopen and its rename-refusal restore. file is
// either a freshly opened fd for the renamed inode (reopen) or
// db.file itself (restore: the original inode, still named by path —
// no reopen needed or wanted). The previous pager is already closed
// by the pre-rename teardown; the previous fd is closed iff replaced.
// Called only under the write grant with readers drained.
//
// Any failure poisons the handle: a live handle without a pager
// cannot serve the api-surface.md §Compact all-or-nothing contract
// (on the reopen path it would otherwise serve the stale unlinked
// inode while peers see the new one — split brain). Close + re-Open
// recovers.
func (db *DB) installCompactPager(file *os.File, op string) error {
	// The same per-open parameter set Open derives — via the single
	// derivation point, so the installed pager cannot silently diverge
	// from the handle's configuration (db.pool's PageSize is preserved
	// across compaction).
	opened, err := pager.Open(file, pagerOpenParamsFrom(db.pool, db.opts))
	if err != nil {
		if file != db.file {
			_ = file.Close()
		}
		db.poisoned.Store(true)
		return fmt.Errorf("gmdb: Compact %s pager (handle poisoned — Close and re-Open): %w", op, mapPagerErr(err))
	}
	// Attach under the grant Compact already holds. Reopen: the
	// compacted copy is self-durable at TxnID 0 (copy.go), so the live
	// projection is the durable one and no recovery gate applies.
	// Restore: the original image — whose latest meta may be a dead
	// peer's uncovered lineage; the covered-through gate below handles
	// exactly that.
	if m, idx, aerr := opened.Pager.AttachLatest(file); aerr != nil {
		_ = opened.Pager.Close()
		if file != db.file {
			_ = file.Close()
		}
		db.poisoned.Store(true)
		return fmt.Errorf("gmdb: Compact %s attach (handle poisoned — Close and re-Open): %w", op, mapPagerErr(aerr))
	} else {
		opened.Meta, opened.ActiveMetaIdx = m, idx
	}

	// Restore attaches the ORIGINAL image, whose latest meta may
	// reference a dead peer's uncovered writeback lineage (a failed
	// barrier's dropped pwrites) — the state Open's live-join arm
	// covers. Run the same gate here, BEFORE the takeover-sequence
	// cache below arms itself past the bump this Compact's own
	// acquisition made (durability.md §Anchoring). The reopen path
	// must NOT: it attaches the freshly written, fully fsynced copy —
	// barrier-covered by construction — and redirtying old-lineage
	// page names against the new image would be wrong.
	if file == db.file {
		if rerr := coverDroppedWritebackLineage(db.coord, opened.Pager, opened.Meta); rerr != nil {
			_ = opened.Pager.Close()
			db.poisoned.Store(true)
			return fmt.Errorf("gmdb: Compact %s lineage cover (handle poisoned — Close and re-Open): %w", op, mapPagerErr(rerr))
		}
	}

	// Swap the live fields atomically vs. a concurrent Begin/BeginRead
	// (which snapshot db.pgr/db.file/db.currentMeta under db.mu).
	db.mu.Lock()
	oldFile := db.file
	db.file = file
	db.pgr = opened.Pager
	// Meta baseline mirrors Open (adoptOpened).
	db.adoptOpened(opened)
	// The adopted state is a full rebuild from the on-disk image;
	// refresh the takeover-sequence cache like Open's attach arms do
	// (Compact holds the grant, so the read is stable).
	if db.coord != nil {
		db.takeoverSeqSeen = db.coord.TakeoverSeq()
	}
	db.mu.Unlock()

	// Re-point the DB leak-detection cleanup at the new pager + file (the
	// old cleanup info captured the closed ones). coord / lockFile /
	// root / gate are unchanged.
	db.cleanup.Stop()
	db.cleanup = runtime.AddCleanup(db, dbCleanupFn, dbCleanupInfo{
		gate:      db.closeGate,
		coord:     db.coord,
		lockFile:  db.lockFile,
		pgr:       opened.Pager,
		file:      file,
		root:      db.root,
		logger:    db.logger,
		originPCs: captureOriginPCs(),
	})

	// Release the replaced fd (reopen path only). The old inode (now
	// unlinked by the rename) is freed once the last mapping drops —
	// any cross-process reader still on it keeps it alive in their
	// address space, never here.
	if oldFile != file {
		_ = oldFile.Close()
	}
	return nil
}

// syncDir / syncDirPath — the dirent-durability barrier, per-platform
// in dirent_unix.go (parent-directory fsync) and dirent_windows.go
// (named-file flush; directory handles refuse write access there).
// An unsynced dirent means a crash can lose the file (create) or
// resurrect the replaced inode (Compact rename) despite acked
// SyncDurable commits — callers treat failure as fatal.

// syncDirHookForTest observes/injects after syncDir's real barrier
// succeeded — the dirent-durability contract's test seam.
var syncDirHookForTest atomic.Pointer[func(dir string) error]
