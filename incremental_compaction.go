package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/descriptor"
	"github.com/thegrumpylion/gmdb/internal/indexing"
	"github.com/thegrumpylion/gmdb/internal/page"
	"github.com/thegrumpylion/gmdb/internal/pager"
)

// errCompactionSpaceExhausted aborts a compaction pass when the
// consolidating allocator finds free space ONLY at or above the
// evacuation floor: relocating into the band being drained is the
// no-progress pathology the below-floor policy exists to prevent, so
// the pass rolls back and the driver retries with a halved budget
// (earlier relocations may fit the below-floor capacity) until it
// declines outright.
var errCompactionSpaceExhausted = errors.New("gmdb: compaction: free space exhausted below the evacuation floor")

// compactionReserve is the below-floor hole count a compaction pass
// must NOT consume itself: homes for the RPL chain-prefix relocation
// (the full prefix from the deepest at-or-above-floor segment to the
// head) plus the commit's own head-segment append. One formula, used
// by both the floor feasibility scan and the pass's allocation
// allowance — a divergence between the two would let relocations eat
// the prefix homes the floor was chosen to protect.
func compactionReserve(pgr *pager.Pager, floor uint64) uint64 {
	return uint64(pgr.RPLRelocationPrefixLen(floor)) + 2
}

// compactionWriter is the relocation pass's PageWriter: allocations
// draw from the LOWEST free hole below allocBound (the consolidating
// allocator, background-maintenance.md §Incremental Compaction step 2
// — btree.PageWriter's AllocPage contract makes the allocation source
// the writer's concern). Two regimes:
//
//   - strict (the evacuation floor sits above the first data page, so
//     a below-floor region exists): allocBound = floor.
//   - whole-region (floor at the first data page — the band covers
//     everything, so there is no "below the band" to preserve):
//     allocBound = HighWaterMark; lowest-hole-first packing still
//     consolidates.
//
// There is NO fallback tier: exhaustion aborts the pass with
// errCompactionSpaceExhausted. The base allocator's extension tier is
// never a relocation target — extending places LIVE pages at the file
// top, re-creating the band the pass is draining (observed as a
// permanent HWM limit cycle) — and its in-band holes are the refill
// pathology itself. The bound-advance the lazy-shrink clause needs in
// the nothing-reader-safe state comes from the driver's reclaim/
// bound-advance commit (reclaimOrAdvanceCommit), not from relocating
// into extensions.
type compactionWriter struct {
	btreeWriter
	allocBound uint64
	// allowance is the below-bound hole budget for the WHOLE pass —
	// decremented by every allocation (relocated leaves AND their CoW
	// cascades alike; a leaf-count budget alone undercounts and would
	// eat the holes reserved for the RPL prefix relocation's homes).
	allowance *uint64
}

func (w compactionWriter) AllocPage() (uint64, error) {
	if *w.allowance == 0 {
		return 0, errCompactionSpaceExhausted
	}
	id, ok, err := w.Pager.AllocPageBelow(w.allocBound)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errCompactionSpaceExhausted
	}
	*w.allowance--
	return id, nil
}

func (w compactionWriter) AllocContiguous(n uint32) (uint64, error) {
	if *w.allowance < uint64(n) {
		return 0, errCompactionSpaceExhausted
	}
	id, ok, err := w.Pager.AllocContiguousBelow(n, w.allocBound)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errCompactionSpaceExhausted
	}
	*w.allowance -= uint64(n)
	return id, nil
}

// compactForest relocates every page at or above floor across all
// B+trees reachable from this write transaction's keyspace forest, returning
// the count of pages relocated. It is the in-place engine behind online
// incremental compaction (background-maintenance.md §Incremental
// Compaction); the orchestration layer supplies the evacuation floor
// (relocation predicate: id >= floor) and a budget, then commits.
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
func (tx *Tx) compactForest(floor uint64, budget int) (int, error) {
	if tx.keyspaceRoot == 0 || budget <= 0 {
		return 0, nil
	}
	shouldRelocate := func(id uint64) bool { return id >= floor }
	firstData := uint64(2) + uint64(tx.prevMeta.BitmapPages)
	// Eager reclaim: return already-eligible freed pages to the bitmap
	// so the capacity measured below is real (idempotent when the
	// caller already reclaimed).
	tx.pgr.ReclaimFreeSpace()
	// Capacity accounting: the pass may consume below-bound holes for
	// its relocations and cascades, MINUS a reserve for the RPL
	// chain-prefix relocation's commit-time homes (the full prefix —
	// free-space.md §RPL segment relocation copies newer below-floor
	// segments too) and the commit's own head-segment append. A pass
	// that consumed every hole would force the prefix relocation to
	// decline each pass, and the re-appended head segment then lands
	// in the freed top holes and pins the tail refund — a permanent
	// HWM limit cycle (observed: rplHead re-appearing at the band top
	// every pass).
	allocBound := floor
	if floor <= firstData {
		// TEST-ONLY surface: production floors come from
		// EvacuationFloor, which never returns floor <= firstData
		// (feasibility needs free capacity strictly below the floor).
		// Direct engine tests pass floor=0 for relocate-everything
		// semantics; targets then pack toward the lowest holes below
		// the HWM with no reserve — outside the amended spec's
		// strictly-downward regime, acceptable for single-shot
		// engine-level exercise.
		allocBound = tx.pgr.HighWaterMark()
	}
	capacity := tx.pgr.FreePagesBelow(allocBound)
	reserve := uint64(0)
	if floor > firstData {
		reserve = compactionReserve(tx.pgr, floor)
	}
	allowance := uint64(0)
	if capacity > reserve {
		allowance = capacity - reserve
	}
	pw := compactionWriter{btreeWriter: btreeWriter{tx.pgr}, allocBound: allocBound, allowance: &allowance}
	baseCfg := pw.Config()
	hwm := pw.HighWaterMark()
	remaining := budget
	moved := 0

	// 1. Snapshot the keyspace roster. WalkKV borrows key/value into page
	//    buffers that later relocations mutate, so clone the names and decode
	//    the descriptors up front.
	type ksEntry struct {
		name []byte
		desc descriptor.Keyspace
	}
	var roster []ksEntry
	if err := btree.WalkKV(pw, baseCfg, tx.keyspaceRoot, hwm, func(k, v []byte) error {
		if len(v) != descriptor.Size {
			return fmt.Errorf("%w: keyspace %q descriptor size %d", btree.ErrCorrupted, string(k), len(v))
		}
		roster = append(roster, ksEntry{name: bytes.Clone(k), desc: descriptor.Decode(v)})
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
			newReg, m, err := tx.compactIndexRegistry(pw, ks.desc.IndexRegistryRoot, shouldRelocate, baseCfg, hwm, &remaining)
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
				tx.dirtyDescriptors = make(map[string]descriptor.Keyspace)
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
func (tx *Tx) compactIndexRegistry(pw btree.PageWriter, regRoot uint64, shouldRelocate func(uint64) bool, cfg page.Config, hwm uint64, remaining *int) (uint64, int, error) {
	moved := 0

	// Snapshot the registry entries (name + decoded entry) before mutating.
	type idxEntry struct {
		name  []byte
		entry *indexing.RegistryEntry
	}
	var entries []idxEntry
	if err := btree.WalkKV(pw, cfg, regRoot, hwm, func(k, v []byte) error {
		e, derr := indexing.DecodeRegistryEntry(v)
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
			// Decode→mutate(Root/Count)→re-encode round-trip: the
			// uint16-bounded fields came off disk within bound and are
			// not mutated, so ErrFieldTooLarge is unreachable; if ever
			// reached (memory corruption) the raw internal error is the
			// right class — no ErrInvalidOptions mapping here.
			nv, eerr := indexing.EncodeRegistryEntry(ie.entry)
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
	spaceExhausted := false
	advanced := false
	for budget >= 1 {
		moved, err := db.compactionPass(ctx, budget)
		switch {
		case errors.Is(err, ErrTxTooLarge):
			budget /= 2 // batch too large for MaxTxBufferBytes — halve and retry
			continue
		case errors.Is(err, errCompactionSpaceExhausted):
			// Free space exists only at or above the floor. The failed
			// pass rolled back — including its eager reclaim — so land
			// that progress in a reclaim/bound-advance commit ONCE per
			// runCompaction call, then retry; every further sentinel
			// halves the batch (a smaller batch may fit the remaining
			// below-floor capacity); at budget 0, decline — the region
			// is unsatisfiable until churn frees low holes. The
			// once-per-call cap is the termination argument: an
			// advance commit cannot help twice in a row (the bound
			// moves at most once per commit, and a READER-pinned bound
			// does not move at all — retrying the same budget against
			// a frozen bound spun this loop at ~1000 empty commits/s),
			// so the loop runs at most 1 + log2(budget) iterations.
			spaceExhausted = true
			if !advanced && db.reclaimOrAdvanceCommit(ctx) {
				advanced = true
				continue
			}
			budget /= 2
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
	if spaceExhausted {
		db.logger.Info("gmdb: maintenance compaction declined — no free space below the evacuation floor")
		return
	}
	db.logger.Warn("gmdb: maintenance compaction could not fit a single page relocation in MaxTxBufferBytes")
}

// reclaimOrAdvanceCommit opens a write transaction, eagerly reclaims
// the RPL, and commits when that freed pages OR the RPL is non-empty:
// a rolled-back compaction pass discards its own eager reclaim (the
// tail refund happens only at commit), and pages retired by the LAST
// commit need one further commit before the reclamation bound covers
// them — this is the lazy-shrink clause's bound-advancing commit,
// WITHOUT relocating anything into extensions. Returns whether a
// commit landed; false (RPL empty and nothing reclaimed) means no
// amount of committing will free more space.
func (db *DB) reclaimOrAdvanceCommit(ctx context.Context) bool {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if tx.pgr.ReclaimFreeSpace() == 0 && tx.pgr.RPLSegmentsAtOrAbove(0) == 0 {
		return false
	}
	if err := tx.Commit(); err != nil {
		return false
	}
	committed = true
	return true
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
	reclaimed := tx.pgr.ReclaimFreeSpace()
	// A decline below must still COMMIT when the reclaim freed pages —
	// the tail refund happens only at commit, so a rolled-back decline
	// would strand the freed band — and ALSO when the RPL is non-empty
	// with nothing freed: the last pass's retirees become eligible only
	// once a LATER commit advances the reclamation bound past theirs,
	// so a declining pass that never commits freezes the bound and
	// every later pass sees the same infeasible state (both observed
	// as permanent HWM plateaus). Self-limiting: one tiny commit per
	// declined pass, and the condition clears once the RPL drains.
	commitReclaim := func() (int, error) {
		if reclaimed == 0 && tx.pgr.RPLSegmentsAtOrAbove(0) == 0 {
			return 0, nil // defer rolls back — nothing to land or advance
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		committed = true
		return 0, nil
	}

	// Size the evacuation band against the post-reclaim free count + the
	// current high-water mark (reclamation frees pages but does not lower the
	// HWM — that happens via tail refund at commit).
	firstData := uint64(2) + uint64(tx.prevMeta.BitmapPages)
	// Exact feasibility scan (replaces the former density ESTIMATE,
	// which let a budget large relative to the region collapse the
	// floor to the first data page — allocation targets and sources
	// then interleaved and passes shuffled the same pages between hole
	// sets forever, a permanent HWM plateau). reserve covers the RPL
	// chain-prefix relocation's commit-time homes (the full prefix)
	// plus the head-segment append.
	floor, ok := tx.pgr.EvacuationFloor(firstData, uint64(budget), compactionReserve(tx.pgr, firstData))
	if !ok {
		return commitReclaim() // nothing feasible to evacuate
	}
	// RPL segment pages in the band cannot be relocated out-of-band —
	// they go through the in-pipeline chain-prefix relocation, whose
	// below-floor HOMES are probed at commit time. Compute the band's
	// segment count FIRST and budget the tree relocations against the
	// remaining below-floor capacity: a pass that consumed every hole
	// itself would force the prefix relocation to decline each pass,
	// and the re-appended head segment then lands in the freed top
	// holes and pins the tail refund — a permanent HWM limit cycle
	// (observed: rplHead re-appearing at the band top every pass).
	// The +2 covers the commit's own head-segment append. Arm only
	// when a below-floor region EXISTS (floor > firstData): a
	// density-sized band covering the whole data region has nowhere to
	// relocate to, and the request would be a guaranteed decline plus
	// a warn every pass.
	rplInBand := 0
	if floor > firstData {
		rplInBand = tx.pgr.RPLSegmentsAtOrAbove(floor)
	}
	moved, err := tx.compactForest(floor, budget)
	if err != nil {
		return 0, err
	}
	if rplInBand > 0 {
		tx.pgr.RequestRPLRelocation(floor)
	}
	if moved == 0 && rplInBand == 0 {
		return commitReclaim() // nothing above the floor to move
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

// mapCompactErr maps the btree + pager error surfaces that the relocation
// engine spans onto the gmdb public sentinels, so the orchestration layer can
// match ErrTxTooLarge (background-maintenance.md §Invariants) and callers see consistent errors.
func mapCompactErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, errCompactionSpaceExhausted):
		return errCompactionSpaceExhausted
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
