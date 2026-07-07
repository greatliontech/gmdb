package gmdb

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// checkpointStepHookForTest injects a failure after Checkpoint's
// step-2/3/4 syscall succeeded, simulating the syscall failing —
// the publication-phase poison contract's test seam.
var checkpointStepHookForTest atomic.Pointer[func(step int) error]

// Checkpoint flushes all outstanding writes to stable storage and
// stamps the active meta with MetaFlagCheckpoint so subsequent
// recovery will accept it as a checkpoint per durability.md
// §Recovery.
//
// In SyncLazy mode this is the explicit-checkpoint hook: prior
// SyncLazy commits skipped both step-2 and step-4 fsync, so recovery
// would roll back to the last checkpoint-flagged meta on crash. A
// successful Checkpoint() makes every commit at or before the active
// meta's TxnID durable.
//
// In SyncDurable / SyncDataOnly, prior commits already issued the
// step-2 fsync, so the data is durable; Checkpoint is still useful
// for SyncDataOnly (where step-4 was skipped — the active meta may
// not be on stable storage; Checkpoint re-pwrites and fsyncs it).
//
// Mechanics (durability.md §Checkpoint mechanics):
//
//  1. Acquire the write lock via the flock goroutine — serialises
//     against any concurrent write tx and against Compact. Concurrent reads are unaffected. Honours
//     ctx for the wait; once the lock is granted, ctx is not
//     checked further (the fsync + pwrite sequence completes
//     unconditionally, bounded and short relative to a Compact
//     wait).
//  2. fdatasync the file to flush prior pwrites from the OS page
//     cache.
//  3. Read the active meta, set MetaFlagCheckpoint, recompute
//     xxhash64, pwrite back to the same slot. The TxnID is unchanged
//     — Checkpoint records that the already-committed state is
//     durable, not a new transaction.
//  4. fdatasync again so the flag set itself reaches stable storage.
//  5. Release the write lock.
//
// Returns:
//   - context.Cause(ctx) if ctx fires before the lock is granted.
//   - ErrClosed if the DB is closed (or closes during the wait).
//   - ErrPoisoned if the handle is poisoned.
//   - Any pwrite/fdatasync error wrapped under "gmdb: checkpoint".
func (db *DB) Checkpoint(ctx context.Context) error {
	// Shared write-grant preamble (fast gates, acquisition, post-grant
	// poison/close/generation re-checks). For Checkpoint specifically:
	// the poison re-check closes the fsyncgate window (a concurrent
	// commit's publication failure consumed the kernel's fsync error
	// state; proceeding would stamp the checkpoint flag over
	// non-durable data), and the generation check prevents flagging a
	// meta on an unlinked inode no other process can see.
	grant, _, err := db.acquireWriteGrant(ctx)
	if err != nil {
		return err
	}
	defer grant.Release()

	// Re-sync before flagging: a peer's commit while we waited leaves
	// db.currentMeta stale, and re-flagging that stale meta in its
	// stale slot (step 3 below) would overwrite the peer's newer meta
	// with an older one — silent lost update. Resync also rebuilds the
	// writer pager's bitmap/RPL so the NEXT Begin (which sees a
	// matching TxnID and skips its own Resync) starts from a
	// consistent pager.
	db.mu.Lock()
	_, file, err := db.resyncOnGrantLocked()
	if err != nil {
		db.mu.Unlock()
		return err
	}
	pageSize := db.currentMeta.PageSize
	meta := db.currentMeta
	activeIdx := db.activeMetaIdx
	db.mu.Unlock()

	// Steps 2–4 are the publication phase: any failure poisons the
	// handle (Close + re-Open is the only recovery), mirroring
	// Tx.Commit's publication contract. Two failure modes make
	// continuing unsafe rather than merely unsuccessful:
	//
	//   - A failed fdatasync (steps 2/4) consumes the kernel's error
	//     state while marking pages clean; a retried Checkpoint's
	//     fsync then succeeds trivially and stamps MetaFlagCheckpoint
	//     over data that never reached disk — recovery selects the
	//     checkpoint and traverses unwritten pages.
	//   - A torn step-3 WriteAt leaves the ONLY on-disk copy of the
	//     active meta checksum-invalid while this handle keeps
	//     serving it from memory; a peer's Resync then selects the
	//     other (older) slot and commits over this tree — split
	//     brain, page aliasing.
	//
	// Re-Open re-reads the actual disk state, so a poisoned handle
	// converges instead of compounding. (durability.md §Checkpoint
	// failure semantics.)
	failStep := func(step int, err error) error {
		db.poisoned.Store(true)
		db.logger.Error("gmdb: checkpoint publication failure; handle poisoned — Close and re-Open",
			"step", step, "err", err)
		return fmt.Errorf("gmdb: checkpoint step %d: %w (handle poisoned)", step, err)
	}
	stepErr := func(step int) error {
		if hook := checkpointStepHookForTest.Load(); hook != nil {
			return (*hook)(step)
		}
		return nil
	}

	// Step 2 — fdatasync to flush prior SyncLazy pwrites.
	if err := file.Sync(); err != nil {
		return failStep(2, err)
	}
	if err := stepErr(2); err != nil {
		return failStep(2, err)
	}

	// Step 3 — set MetaFlagCheckpoint on the active meta, recompute
	// checksum, pwrite back to the SAME slot. The single-meta-slot
	// pwrite is atomic within one page per durability.md (an
	// unaligned tear cannot affect a single contiguous sub-page
	// region, and the xxhash64 checksum catches partial writes —
	// recovery falls back to the other slot).
	if meta.HasFlag(page.MetaFlagCheckpoint) {
		// Already checkpointed — step 2's fdatasync is the only
		// useful work. Skip the pwrite (idempotent) but DO issue
		// step 4 to ensure the previously-written flag bit is on
		// stable storage even if the prior commit was in
		// SyncDataOnly (which skipped step 4).
	} else {
		meta.Flags |= page.MetaFlagCheckpoint
		buf := make([]byte, pageSize)
		page.EncodeMeta(buf, &meta)
		off := int64(activeIdx) * int64(pageSize)
		if _, err := file.WriteAt(buf, off); err != nil {
			return failStep(3, err)
		}
		if err := stepErr(3); err != nil {
			return failStep(3, err)
		}
	}

	// Step 4 — fdatasync so the flag-set is durable. (Even if step
	// 3 was skipped above, step 4 still flushes the OS page cache
	// for any in-flight meta pwrite — defense in depth for the
	// SyncDataOnly case.)
	if err := file.Sync(); err != nil {
		return failStep(4, err)
	}
	if err := stepErr(4); err != nil {
		return failStep(4, err)
	}

	// Publish the updated meta to db.currentMeta so subsequent
	// callers observe the now-checkpointed flag.
	db.mu.Lock()
	if db.currentMeta.TxnID == meta.TxnID {
		// The active meta is now checkpoint-flagged → it is the last
		// checkpoint, so it bounds RPL reclamation (free-space.md §RPL
		// Reclamation). We hold the write grant, so no commit advanced
		// currentMeta past meta while we worked; the slot index is
		// unchanged (the flag pwrite targeted the same slot).
		db.setMetaState(meta, db.activeMetaIdx, meta.TxnID)
	}
	db.mu.Unlock()
	return nil
}
