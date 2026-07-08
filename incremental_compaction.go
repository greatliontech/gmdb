package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
	"github.com/thegrumpylion/gmdb/internal/pager"
)

// compactForest relocates every page selected by shouldRelocate across all
// B+trees reachable from this write transaction's keyspace forest, returning
// the count of pages relocated. It is the in-place engine behind online
// incremental compaction (background-maintenance.md §Incremental
// Compaction); the orchestration layer supplies a high-watermark predicate
// (id >= evacFloor) and a budget, then commits.
//
// budget bounds the total relocations (btree.RelocatePages' maxMoves, shared
// — and decremented — across every tree in the forest). When it is exhausted
// mid-forest the remaining keyspaces are left untouched this pass; the
// orchestration's resumable cursor picks them up next pass. budget <= 0 or an
// empty forest (keyspaceRoot == 0) is a no-op.
//
// Precondition: the transaction must have NO open *Keyspace / *SetKeyspace
// handles. compactForest stages relocated descriptors via tx.dirtyDescriptors,
// which the tx field invariant requires be disjoint from openKeyspaces; an
// open handle for a name compactForest also restages would let the handle's
// stale-root descriptor and the relocated one collide at flushKeyspaces. The
// maintenance pass opens a dedicated bare write tx, so this holds by
// construction.
//
// Re-rooting reuses the transaction's existing persistence machinery, so a
// compacted forest is byte-indistinguishable from a Put-built one and lands
// atomically at Commit:
//   - an index data tree whose root moved → its registry entry is rewritten
//     (btree.Put into the registry sub-tree);
//   - a keyspace whose data root or index-registry root moved → the updated
//     descriptor is staged in tx.dirtyDescriptors (re-Put into the keyspace
//     descriptor tree by flushKeyspaces at Commit);
//   - the keyspace descriptor tree itself is relocated last and assigned to
//     tx.keyspaceRoot (→ meta.KeyspaceRoot at Commit) — last, so the
//     descriptor re-Puts in flushKeyspaces land on the relocated tree.
//
// cfg discipline mirrors copyCompact: a keyspace's data tree (and its nested
// set-keyspace trees, which RelocatePages recurses into) uses the
// keyspace-overridden RestartGroupTarget; the index registry sub-tree and
// index data trees use the base pager cfg, matching how the runtime
// (index_codec.go / index_maintain.go) maintains them.
//
// RPL segment pages are never relocated, and not because of any predicate
// term: they hang off meta.RPLHeadPage on a chain that the keyspace forest
// walk never reaches, so RelocatePages never offers one to shouldRelocate.
// They drain via reclamation and new segments self-place low (the deferred
// out-of-band-relocation case lives in git history).
//
// A relocation that would overrun MaxTxBufferBytes surfaces ErrTxTooLarge,
// which compactForest returns to the caller for rollback (background-maintenance.md §Invariants: the
// maintenance orchestration catches it and reduces the batch — it is never
// user-visible). Partial work already applied to the slab is discarded by
// the caller's rollback; nothing is committed.
func (tx *Tx) compactForest(shouldRelocate func(uint64) bool, budget int) (int, error) {
	if tx.keyspaceRoot == 0 || budget <= 0 {
		return 0, nil
	}
	pw := btreeWriter{tx.pgr}
	baseCfg := pw.Config()
	hwm := pw.HighWaterMark()
	remaining := budget
	moved := 0

	// 1. Snapshot the keyspace roster. WalkKV borrows key/value into page
	//    buffers that later relocations mutate, so clone the names and decode
	//    the descriptors up front.
	type ksEntry struct {
		name []byte
		desc keyspaceDescriptor
	}
	var roster []ksEntry
	if err := btree.WalkKV(pw, baseCfg, tx.keyspaceRoot, hwm, func(k, v []byte) error {
		if len(v) != keyspaceDescriptorSize {
			return fmt.Errorf("%w: keyspace %q descriptor size %d", btree.ErrCorrupted, string(k), len(v))
		}
		roster = append(roster, ksEntry{name: bytes.Clone(k), desc: decodeKeyspaceDescriptor(v)})
		return nil
	}); err != nil {
		return 0, mapCompactErr(err)
	}

	// 2. Relocate each keyspace's data tree + index trees.
	for i := range roster {
		if remaining <= 0 {
			break
		}
		ks := &roster[i]
		dataCfg := baseCfg
		if ks.desc.RestartGroupTarget != 0 {
			dataCfg.RestartGroupTarget = ks.desc.RestartGroupTarget
		}
		dirty := false

		if ks.desc.Root != 0 {
			nr, m, err := btree.RelocatePages(pw, dataCfg, ks.desc.Root, shouldRelocate, remaining)
			if err != nil {
				return 0, mapCompactErr(err)
			}
			remaining -= m
			moved += m
			if nr != ks.desc.Root {
				ks.desc.Root = nr
				dirty = true
			}
		}

		if ks.desc.IndexRegistryRoot != 0 && remaining > 0 {
			newReg, m, err := tx.compactIndexRegistry(ks.desc.IndexRegistryRoot, shouldRelocate, baseCfg, hwm, &remaining)
			if err != nil {
				return 0, err // already mapped
			}
			moved += m
			if newReg != ks.desc.IndexRegistryRoot {
				ks.desc.IndexRegistryRoot = newReg
				dirty = true
			}
		}

		if dirty {
			if err := tx.ensureKeyspacePathLen(); err != nil {
				return 0, err
			}
			if tx.dirtyDescriptors == nil {
				tx.dirtyDescriptors = make(map[string]keyspaceDescriptor)
			}
			tx.dirtyDescriptors[string(ks.name)] = ks.desc
			tx.recalcFlushReserve()
			// Obligation-edge admission; any error discards the whole
			// batch via the caller's rollback.
			if err := tx.checkReserveAffordable(); err != nil {
				return 0, err
			}
		}
	}

	// 3. Relocate the keyspace descriptor tree itself, last (base cfg —
	//    matching how copyCompact and CreateKeyspace build it). flushKeyspaces
	//    will re-Put the dirty descriptors into the relocated tree at Commit.
	if remaining > 0 {
		nkr, m, err := btree.RelocatePages(pw, baseCfg, tx.keyspaceRoot, shouldRelocate, remaining)
		if err != nil {
			return 0, mapCompactErr(err)
		}
		remaining -= m
		moved += m
		tx.keyspaceRoot = nkr
	}

	return moved, nil
}

// compactIndexRegistry relocates a keyspace's index registry sub-tree and
// every index data tree it points at, returning the (possibly new) registry
// root and the count of pages relocated. An index data tree whose root moves
// has its registry entry rewritten in place (btree.Put), then the registry
// tree itself is relocated. remaining is the shared forest budget, decremented
// as pages move. Uses cfg (the base pager cfg) for both the registry tree and
// the index data trees, matching the runtime's maintenance of them.
func (tx *Tx) compactIndexRegistry(regRoot uint64, shouldRelocate func(uint64) bool, cfg page.Config, hwm uint64, remaining *int) (uint64, int, error) {
	pw := btreeWriter{tx.pgr}
	moved := 0

	// Snapshot the registry entries (name + decoded entry) before mutating.
	type idxEntry struct {
		name  []byte
		entry *indexRegistryEntry
	}
	var entries []idxEntry
	if err := btree.WalkKV(pw, cfg, regRoot, hwm, func(k, v []byte) error {
		e, derr := decodeRegistryEntry(v)
		if derr != nil {
			return fmt.Errorf("%w: index %q registry entry: %v", btree.ErrCorrupted, string(k), derr)
		}
		entries = append(entries, idxEntry{name: bytes.Clone(k), entry: e})
		return nil
	}); err != nil {
		return 0, 0, mapCompactErr(err)
	}

	curReg := regRoot
	for _, ie := range entries {
		if *remaining <= 0 {
			break
		}
		if ie.entry.Root == 0 {
			continue
		}
		nr, m, err := btree.RelocatePages(pw, cfg, ie.entry.Root, shouldRelocate, *remaining)
		if err != nil {
			return 0, 0, mapCompactErr(err)
		}
		*remaining -= m
		moved += m
		if nr != ie.entry.Root {
			ie.entry.Root = nr
			nv, eerr := encodeRegistryEntry(ie.entry)
			if eerr != nil {
				// Unreachable in-spec (we re-encode an entry we just decoded,
				// changing only the scalar Root), but keep the contract that
				// this function returns mapped/contextualised errors.
				return 0, 0, fmt.Errorf("gmdb: compaction: index %q registry re-encode: %w", string(ie.name), eerr)
			}
			nrg, perr := btree.Put(pw, cfg, curReg, ie.name, nv)
			if perr != nil {
				return 0, 0, mapCompactErr(perr)
			}
			curReg = nrg
		}
	}

	// Relocate the registry tree itself, after the entry rewrites.
	if *remaining > 0 {
		nr, m, err := btree.RelocatePages(pw, cfg, curReg, shouldRelocate, *remaining)
		if err != nil {
			return 0, 0, mapCompactErr(err)
		}
		*remaining -= m
		moved += m
		curReg = nr
	}

	return curReg, moved, nil
}

// maintCompact runs Task 4 of a maintenance pass: incremental compaction
// (background-maintenance.md §Incremental Compaction). It consumes the
// contiguous-allocation fragmentation rate the allocator has tracked since the
// last pass and, when that rate exceeds CompactionThreshold (and compaction is
// enabled), runs a budgeted high-watermark relocation batch to consolidate
// free space and let the file shrink.
//
// Per background-maintenance.md §Invariants, the pass never surfaces ErrTxTooLarge. runCompaction halves the
// batch and retries when a relocation would exceed MaxTxBufferBytes, and gives
// up (logs) if not even one page's cascade fits — the user never sees a
// maintenance-induced ErrTxTooLarge.
func (db *DB) maintCompact(ctx context.Context) {
	if db.opts.Maintenance.DisableCompaction {
		return
	}
	db.mu.Lock()
	pgr := db.pgr
	db.mu.Unlock()
	if pgr == nil {
		return // closing
	}
	// Consume (read-and-reset) the fragmentation counters. Doing this even
	// when the trigger does not fire keeps the rate scoped to the most recent
	// interval (spec §Incremental Compaction Trigger).
	attempts, fragFails := pgr.ConsumeContiguousAllocStats()
	if !compactionTriggered(attempts, fragFails, db.opts.Maintenance.CompactionThreshold) {
		return
	}
	db.runCompaction(ctx)
}

// compactionTriggered reports whether the contiguous-allocation failure rate
// (fragFails/attempts) over the last interval exceeds threshold. A pass with no
// multi-page allocations (attempts == 0) has no signal and never triggers.
func compactionTriggered(attempts, fragFails uint64, threshold float64) bool {
	if attempts == 0 {
		return false
	}
	return float64(fragFails)/float64(attempts) > threshold
}

// runCompaction runs compaction batches until one succeeds (or there is
// nothing to do), halving the budget on ErrTxTooLarge so a too-large batch is
// reduced rather than surfaced (background-maintenance.md §Invariants). Benign tx-open failures (closing /
// cancelled / poisoned) are silent; other errors are logged. Each successful
// batch is one committed transaction.
func (db *DB) runCompaction(ctx context.Context) {
	budget := db.opts.Maintenance.CompactionBatchSize
	for budget >= 1 {
		moved, err := db.compactionPass(ctx, budget)
		switch {
		case errors.Is(err, ErrTxTooLarge):
			budget /= 2 // batch too large for MaxTxBufferBytes — halve and retry
			continue
		case err != nil:
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
				!errors.Is(err, ErrClosed) && !errors.Is(err, ErrPoisoned) {
				db.logger.Warn("gmdb: maintenance compaction skipped", "err", err)
			}
			return
		default:
			if moved > 0 {
				db.logger.Info("gmdb: maintenance compacted pages", "count", moved)
			}
			return
		}
	}
	db.logger.Warn("gmdb: maintenance compaction could not fit a single page relocation in MaxTxBufferBytes")
}

// compactionPass runs one compaction transaction: derive the high-watermark
// evacuation floor from the snapshot meta, relocate up to budget pages at or
// above it across the forest, and commit. Returns the count of pages
// relocated. On ErrTxTooLarge (or any error) the tx is rolled back and the
// error returned; the caller retries with a smaller budget or aborts.
func (db *DB) compactionPass(ctx context.Context, budget int) (int, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err // closing / cancelled / poisoned
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Eagerly reclaim the RPL before relocating: returns already-eligible
	// freed pages to the bitmap so relocations consolidate into them (rather
	// than extending the file), and so the previous pass's relocated-from
	// pages — now committed below the reclamation bound — are freed and a
	// trailing run of them tail-refunds at this commit. This is what makes
	// the file shrink monotonically across passes (background-maintenance.md
	// §Incremental Compaction); without it reclamation is lazy and the file
	// shrinks only stepwise after bitmap exhaustion.
	tx.pgr.ReclaimFreeSpace()

	// Size the evacuation band against the post-reclaim free count + the
	// current high-water mark (reclamation frees pages but does not lower the
	// HWM — that happens via tail refund at commit).
	firstData := uint64(2) + uint64(tx.prevMeta.BitmapPages)
	floor, ok := evacuationFloor(firstData, tx.pgr.HighWaterMark(), tx.pgr.NumFreePages(), budget)
	if !ok {
		return 0, nil // no data region / nothing allocated — defer rolls back
	}
	moved, err := tx.compactForest(func(id uint64) bool { return id >= floor }, budget)
	if err != nil {
		return 0, err
	}
	// RPL segment pages in the band cannot be relocated out-of-band —
	// arm the in-pipeline chain-prefix relocation, which this commit
	// executes or declines (free-space.md §RPL segment relocation).
	// Arm only when a below-floor region EXISTS (floor > firstData):
	// a density-sized band covering the whole data region has nowhere
	// to relocate to, and the request would be a guaranteed decline
	// plus a warn every pass.
	rplInBand := 0
	if floor > firstData {
		rplInBand = tx.pgr.RPLSegmentsAtOrAbove(floor)
	}
	if rplInBand > 0 {
		tx.pgr.RequestRPLRelocation(floor)
	}
	if moved == 0 && rplInBand == 0 {
		return 0, nil // nothing above the floor to move — defer rolls back
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	committed = true
	if rplInBand > 0 && tx.pgr.RPLRelocationDeclined() {
		// Decline-and-report per the spec: no below-floor homes for
		// every copy, or the prefix exceeded the commit budget. The
		// region stays pinned this pass; a later pass re-requests
		// against the then-current chain.
		db.logger.Warn("gmdb: RPL chain-prefix relocation declined; evacuation region unsatisfiable this pass",
			"floor", floor, "segmentsInBand", rplInBand)
	}
	return moved, nil
}

// evacuationFloor computes the high-watermark evacuation floor: the lowest page
// id such that the trailing band [floor, hwm) holds roughly budget allocated
// pages, estimated from the free-page density. Relocating that band lets it
// drain into a contiguous free run so the file can shrink. Returns ok=false
// when there is no data region or nothing allocated to relocate.
//
// Sizing the band to ~budget allocated pages (rather than fixing it at the very
// top) keeps each pass's relocation count near the budget regardless of how
// sparse the region is: a near-full region uses a thin band, a sparse one a
// wider band.
func evacuationFloor(firstData, hwm, numFreePages uint64, budget int) (uint64, bool) {
	if budget <= 0 || hwm <= firstData {
		return 0, false
	}
	dataSpan := hwm - firstData
	freeInData := min(numFreePages, dataSpan)
	allocInData := dataSpan - freeInData
	if allocInData == 0 {
		return 0, false // region is entirely free — nothing to relocate
	}
	density := float64(allocInData) / float64(dataSpan) // in (0, 1]
	bandSpan := uint64(float64(budget) / density)
	if bandSpan >= dataSpan {
		return firstData, true // budget covers the whole region
	}
	return hwm - bandSpan, true
}

// mapCompactErr maps the btree + pager error surfaces that the relocation
// engine spans onto the gmdb public sentinels, so the orchestration layer can
// match ErrTxTooLarge (background-maintenance.md §Invariants) and callers see consistent errors.
func mapCompactErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, pager.ErrTxTooLarge):
		return ErrTxTooLarge
	case errors.Is(err, pager.ErrDBFull):
		return ErrDBFull
	case errors.Is(err, pager.ErrBadPageChecksum):
		return fmt.Errorf("%w: %w", ErrBadPageChecksum, err)
	case errors.Is(err, btree.ErrCorrupted), errors.Is(err, pager.ErrCorrupted), errors.Is(err, btree.ErrTreeTooDeep):
		return fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	return fmt.Errorf("gmdb: compaction: %w", err)
}
