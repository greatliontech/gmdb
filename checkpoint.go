package gmdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/lock"
	"github.com/thegrumpylion/gmdb/internal/page"
)

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
// In SyncUnsafe, Checkpoint syncs the file but does not retroactively
// fix the ordering of prior commits' pwrites — those may already be
// out-of-order on disk. The flag is set anyway; the documentation
// for SyncUnsafe warns this is benchmarks-and-ephemeral-data only.
//
// Mechanics (durability.md §Checkpoint mechanics):
//
//  1. Acquire the write lock via the flock goroutine — serialises
//     against any concurrent write tx and against Compact (when
//     chunk 11 wires it). Concurrent reads are unaffected. Honours
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
	if db.closeGate.IsClosed() {
		return ErrClosed
	}
	if db.poisoned.Load() {
		return ErrPoisoned
	}
	db.mu.Lock()
	coord := db.coord
	file := db.file
	pageSize := db.currentMeta.PageSize
	db.mu.Unlock()
	if coord == nil || file == nil {
		return ErrClosed
	}
	grant, err := coord.AcquireWriter(ctx)
	if err != nil {
		if errors.Is(err, lock.ErrClosed) {
			return ErrClosed
		}
		return err
	}
	defer grant.Release()

	// Re-snapshot under db.mu to pick up any concurrent commit
	// that may have published a new meta while we were waiting for
	// the grant. Subsequent steps run against THIS snapshot.
	db.mu.Lock()
	if db.closeGate.IsClosed() {
		db.mu.Unlock()
		return ErrClosed
	}
	meta := db.currentMeta
	activeIdx := db.activeMetaIdx
	db.mu.Unlock()

	// Step 2 — fdatasync to flush prior SyncLazy pwrites.
	if err := file.Sync(); err != nil {
		return fmt.Errorf("gmdb: checkpoint step 2 fdatasync: %w", err)
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
			return fmt.Errorf("gmdb: checkpoint step 3 write meta: %w", err)
		}
	}

	// Step 4 — fdatasync so the flag-set is durable. (Even if step
	// 3 was skipped above, step 4 still flushes the OS page cache
	// for any in-flight meta pwrite — defense in depth for the
	// SyncDataOnly case.)
	if err := file.Sync(); err != nil {
		return fmt.Errorf("gmdb: checkpoint step 4 fdatasync meta: %w", err)
	}

	// Publish the updated meta to db.currentMeta so subsequent
	// callers observe the now-checkpointed flag.
	db.mu.Lock()
	if db.currentMeta.TxnID == meta.TxnID {
		db.currentMeta = meta
		// The active meta is now checkpoint-flagged → it is the last
		// checkpoint, so it bounds RPL reclamation (free-space.md §RPL
		// Reclamation). We hold the write grant, so no commit advanced
		// currentMeta past meta while we worked.
		db.lastCheckpointTxnID = meta.TxnID
	}
	db.mu.Unlock()
	return nil
}
