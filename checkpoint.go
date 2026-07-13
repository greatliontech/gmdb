package gmdb

import (
	"context"
	"fmt"
	"github.com/greatliontech/gmdb/internal/pager"
	"sync/atomic"
)

// checkpointStepHookForTest injects a failure after Checkpoint's
// step-2/3/4 syscall succeeded, simulating the syscall failing —
// the publication-phase poison contract's test seam.
var checkpointStepHookForTest atomic.Pointer[func(step int) error]

// Checkpoint flushes all outstanding writes to stable storage and
// bumps the active meta's durable sub-record to its own live state, so
// subsequent recovery adopts it (durability.md §Recovery).
//
// In SyncLazy mode this is the explicit-checkpoint hook: prior
// SyncLazy commits skipped both step-2 and step-4 fsync, so recovery
// would roll back to the durable epoch on crash. A successful
// Checkpoint() makes every commit at or before the active meta's
// TxnID durable — and anchors the new epoch (durability.md
// §Anchoring), advancing the RPL reclamation bound.
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
//  3. Read the active meta, bump its durable sub-record to its own
//     live state, recompute XXH3-64, pwrite back to the same slot.
//     The TxnID is unchanged — Checkpoint records that the
//     already-committed state is durable, not a new transaction.
//  4. fdatasync again so the sub-record bump reaches stable storage.
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
	// state; proceeding would stamp a durable sub-record over
	// non-durable data), and the generation check prevents bumping a
	// meta on an unlinked inode no other process can see.
	grant, _, err := db.acquireWriteGrant(ctx)
	if err != nil {
		return err
	}
	defer grant.Release()
	return db.checkpointUnderGrant()
}

// checkpointUnderGrant runs Checkpoint steps 2–4 + the meta publish.
// The caller MUST hold the write grant and have passed the poison and
// generation gates (acquireWriteGrant, or Close's shutdown path which
// runs its own reduced preamble — the close gate is deliberately NOT
// consulted here, because the shutdown checkpoint runs after Close
// wins the close CAS but before teardown).
func (db *DB) checkpointUnderGrant() error {
	// Re-sync before bumping: a peer's commit while we waited leaves
	// db.currentMeta stale, and re-bumping that stale meta in its
	// stale slot (step 3 below) would overwrite the peer's newer meta
	// with an older one — silent lost update. Resync also rebuilds the
	// writer pager's bitmap/RPL so the NEXT Begin (which sees a
	// matching TxnID and skips its own Resync) starts from a
	// consistent pager.
	db.mu.Lock()
	pgr, _, err := db.resyncPagerLocked()
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
	//     fsync then succeeds trivially and stamps a durable
	//     sub-record over data that never reached disk — recovery
	//     adopts it and traverses unwritten pages.
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
	if err := pgr.SyncData(); err != nil {
		return failStep(2, err)
	}
	if err := stepErr(2); err != nil {
		return failStep(2, err)
	}
	// The completed step-2 fsync anchors the pre-bump meta's own
	// durable assertion — its carrying pwrite preceded the fsync
	// (durability.md §Anchoring; the commit path's step 2 makes the
	// same advance). In-process knowledge only: whether step 3 may
	// PERSIST it depends on which slot carries the assertion — see
	// the sole-carrier constraint on the skip branch below.
	pgr.AdvanceAnchoredEpoch(meta.Durable.TxnID)

	// Step 3 — bump the active meta's durable sub-record to its own
	// live state, set AnchoredDurableTxnID to the PRE-bump anchored
	// value (step 2's completed fsync anchors the pre-bump assertion;
	// the bump's own assertion is anchored by step 4 and persisted by
	// the NEXT meta write — durability.md §Anchoring,
	// no-forward-promise), recompute the checksum, pwrite back to the
	// SAME slot. The single-meta-slot pwrite is atomic within one page
	// per durability.md (an unaligned tear cannot affect a single
	// contiguous sub-page region, and the XXH3-64 checksum catches
	// partial writes — recovery falls back to the other slot).
	if meta.SelfDurable() {
		// Already at its own durable epoch — step 2's fdatasync is
		// the only useful work. Skip the pwrite but DO issue step 4
		// so the previously-written sub-record is on stable storage
		// even if the prior commit was in SyncDataOnly (which skipped
		// step 4). The skip is LOAD-BEARING, not just an idempotence
		// elision: a self-durable meta is the SOLE durable carrier of
		// its own assertion (the other slot's sub-record predates
		// it), and pwriting it in place — even only to persist the
		// step-2 anchor advance — risks a torn step-4 fsync (the
		// kernel consumes the writeback error and marks the page
		// clean) destroying the assertion on disk while the intact
		// page-cache copy keeps feeding peer reclamation bounds:
		// after a crash, recovery falls back to the other, OLDER
		// slot, whose tree references pages the bound let a peer
		// reuse. A non-self-durable bump has no such hazard — its
		// sub-record is carried in BOTH slots. The persisted anchor
		// therefore deliberately trails the in-process one in pure
		// SyncDataOnly use (delayed peer reclamation, never
		// unsafety); peers close the gap through the tear-safe
		// anchor persist channel (durability.md §Anchoring), never
		// through a changed-bytes rewrite of this carrier.
	} else {
		meta.Durable = meta.LiveSubRecord()
		meta.Durable.AnchoredTxnID = pgr.AnchoredEpoch()
		buf := make([]byte, pageSize)
		pager.EncodeMeta(buf, &meta)
		off := int64(activeIdx) * int64(pageSize)
		if _, err := pgr.WriteMetaPage(buf, off); err != nil {
			return failStep(3, err)
		}
		if err := stepErr(3); err != nil {
			return failStep(3, err)
		}
		// The bump rewrote the active slot with CHANGED bytes: refresh
		// the adopted-meta cache so the tear-safe anchor gate's
		// byte-identity premise (cache == on-disk slot) stays true —
		// the bumped meta is exactly what a peer would now adopt.
		pgr.NoteAdoptedMeta(meta, activeIdx)
	}

	// Step 4 — fdatasync so the sub-record bump is durable. (Even if
	// step 3 was skipped above, step 4 still flushes the OS page cache
	// for any in-flight meta pwrite — defense in depth for the
	// SyncDataOnly case.)
	if err := pgr.SyncData(); err != nil {
		return failStep(4, err)
	}
	if err := stepErr(4); err != nil {
		return failStep(4, err)
	}
	// The completed step-4 fsync anchors the bump's own assertion
	// (durability.md §Anchoring) — the bound may now trust it.
	pgr.AdvanceAnchoredEpoch(meta.TxnID)

	// Publish the updated meta to db.currentMeta so subsequent
	// callers observe the bumped sub-record.
	db.mu.Lock()
	if db.currentMeta.TxnID == meta.TxnID {
		// We hold the write grant, so no commit advanced currentMeta
		// past meta while we worked; the slot index is unchanged (the
		// bump pwrite targeted the same slot).
		db.setMetaState(meta, db.activeMetaIdx)
	}
	db.mu.Unlock()
	return nil
}
