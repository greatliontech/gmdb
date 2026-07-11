package pager

import (
	"slices"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
)

// SavepointKind selects the savepoint flavour. The Nested kind is the
// canonical user-facing primitive (transactions.md §Nested
// Transactions, BeginChild semantics) that suspends loose-pop for the
// duration so a child can never hand out a page id whose slab buffer
// the parent's tree still references (pager-slab.md §Slab Lifecycle Across Nested Transactions). The Shallow kind is
// the internal-helper flavour used by every keyspace-layer row
// mutation, indexed or not (Keyspace.Put / Delete / Cursor.Delete;
// SetKeyspace.Put / Delete / DeleteValue) and by the un-indexed
// DeleteRange walk, to make the whole per-op mutation — index
// maintenance, row btree op, and the retiredPages/allocation state
// btree mutations accrue before their last fallible step — atomic at
// the page-persistence layer without giving up across-Put loose-page
// recycling. Loose-pop stays enabled under a Shallow
// savepoint; each loose-pop event appends a (id, original-buffer)
// entry to the savepoint's loosePopLog so Restore can re-attach the
// detached buffer and re-add id to loosePages.
type SavepointKind int

const (
	// SavepointNested is the user-facing nested-transaction savepoint
	// flavour (BeginSavepoint). Increments savepointDepth → loose-pop
	// suspended for the duration.
	SavepointNested SavepointKind = iota

	// SavepointShallow is the internal-helper savepoint flavour
	// (BeginShallowSavepoint). Loose-pop remains enabled; the
	// Savepoint's loosePopLog captures each loose-pop event so
	// Restore can re-attach the detached buffer to p.dirty.
	SavepointShallow
)

// loosePopEntry records a single loose-pop event observed during a
// Shallow savepoint window. The (id, buf) pair is exactly what
// AllocPage's loose-pop branch did: id was removed from loosePages,
// buf was severed from p.dirty[id] into p.detachedBufs, and id was
// added to pendingAllocs. The loosePages / pendingAllocs / pendingFrees
// side effects are tracked via savepointUndoLog independently and need
// no special handling here.
//
// wasPreWindow distinguishes whether the detached buf was already in
// p.dirty at this Savepoint's Begin time (true → restore by re-
// attaching buf to dirty[id]) or was added to p.dirty *during* the
// window via CoW/AllocSlab/AllocSlabRun (false → the in-window
// installer's `(fieldDirty, id, false)` undo entry already deleted
// dirty[id] during the Restore step-3 replay, so the loose-pop replay
// must drop buf back to the pool instead of installing it — installing
// would re-introduce an in-window buffer post-Restore, leaking it into
// the parent tx's slab state with no corresponding dirtyBytes account
// reading, breaking pager-slab.md byte-slice ownership). The
// flag is set at loose-pop time (freespace.go: AllocPage loose-pop
// branch) by scanning the savepoint's window slice of savepointUndoLog
// for any prior `(fieldDirty, id, false)` entry — O(this-savepoint-
// window-mutations) per loose-pop event, bounded by the cost contract.
type loosePopEntry struct {
	id           uint64
	buf          *[]byte
	wasPreWindow bool
}

// savepointUndoField identifies which tx-scoped map a savepoint undo
// entry targets. Each field has a per-mutation-site producer that
// appends an entry when at least one savepoint is open (see Pager.
// recordSavepointUndo).
type savepointUndoField uint8

const (
	fieldPendingAllocs savepointUndoField = iota
	fieldPendingFrees
	fieldLoosePages
	// fieldDirty tracks additions to p.dirty (CoW / AllocSlab /
	// AllocSlabRun). Pre-op state is always "absent" because the slab
	// installers return early when dirty[id] is already present
	// (idempotent CoW), so the loggable case is uniformly wasPresent=
	// false. RestoreSavepoint deletes dirty[id] and pool-Puts the buffer
	// presently there (the in-window installation). loose-pop's
	// dirty-detach is tracked separately in Savepoint.loosePopLog (the
	// detach needs the original buffer pointer for re-attach, which the
	// uniform key/wasPresent shape here cannot carry).
	fieldDirty
)

// savepointUndoEntry records one observed mutation of a tx-scoped map
// during a window in which at least one savepoint was open. For the
// three set-valued fields (pendingAllocs/pendingFrees/loosePages),
// wasPresent is the pre-op map[key] state — RestoreSavepoint replays in
// reverse and sets map[key] = wasPresent (delete to remove, struct{} to
// add). For fieldDirty, wasPresent is always false (see fieldDirty
// godoc); replay deletes p.dirty[key] and pool-Puts the buffer.
//
// Only state-changing mutations append an entry — idempotent no-ops
// (delete on absent, add on present) skip the append at the call site
// (mirrors bitmap.recordFlip skipping when no flip actually occurred).
// This keeps the log proportional to *real* mutations during the
// savepoint window, which is what transactions.md §Nested Transactions's
// cost clause requires.
type savepointUndoEntry struct {
	field      savepointUndoField
	key        uint64
	wasPresent bool
}

// Savepoint captures the pager's tx-scoped mutable state at a child-
// transaction boundary (transactions.md §Nested Transactions). A child
// transaction shares the parent's pager — there is exactly one *Pager
// per top-level write transaction and children are sub-transactions of
// it — so the savepoint, not a separate pager, is what makes a child
// independently rollback-able.
//
// On child rollback, RestoreSavepoint reverts every field below to the
// value it held at BeginSavepoint and releases the slab buffers the
// child added. On child commit, ReleaseSavepoint discards the capture
// (the child's mutations stay in the parent's pager state). The two are
// the savepoint analogue of AbortTx (top-level rollback) and
// discardTxSnapshot (top-level commit).
//
// Cost contract (transactions.md §Nested Transactions §Nesting depth
// "Cost is proportional to pages modified since the outermost open
// savepoint, plus O(bitmap-pages currently dirty) ... plus
// O(rplSegments chain length) ..."): every field is O(per-window
// mutations) — bitmap.Snapshot is an undo-log marker (O(flips since
// this Snapshot opened) + O(bitmap-pages-dirty ≤ ~2048 = ~16 KiB));
// pendingAllocs/Frees/loosePages/dirty additions are tracked via the
// per-pager Pager.savepointUndoLog with a per-Savepoint marker
// (undoLogPos), so Begin is O(1) and Restore replays only this
// savepoint's window-mutations; rplSegments is the chain inherited
// from the active meta and clone-captured at Begin — O(chain length),
// workload-dependent at cross-tx granularity (a stuck reclamation
// bound from a lagging reader lets the chain accumulate across
// commits; see transactions.md §Why this is cheap). No field scales
// with MaxSize; the within-tx cumulative-state scaling closed by
// 43ac8df is gone for all four undo-logged fields; only rplSegments
// retains an across-writer-commits scaling that is orthogonal to the
// within-tx case.
//
// Kind=SavepointShallow additionally carries loosePopLog: a per-event
// record of every loose-pop AllocPage performed during the savepoint
// window. Kind=SavepointNested leaves loosePopLog nil — loose-pop is
// suspended for nested savepoints (savepointDepth > 0), so the field
// is unreachable.
type Savepoint struct {
	kind SavepointKind
	snapshotCore
	// undoLogPos is the marker into Pager.savepointUndoLog at this
	// savepoint's Begin time. Restore replays log[undoLogPos:end] in
	// reverse to undo every observed mutation of pendingAllocs/
	// pendingFrees/loosePages/dirty during the window. The marker is
	// the savepoint analogue of bitmap.Snapshot.logPos.
	undoLogPos int
	// retiredPages and detachedBufs are append-only over a transaction
	// body (FreePage appends prior-tx frees; AllocPage's loose-pop is the
	// only producer of detachedBufs, and it is suspended while a NESTED
	// savepoint is active — see AllocPage). Truncating to the captured
	// length is therefore a complete restore.
	retiredLen  int
	detachedLen int
	dirtyBytes  int

	// loosePopLog is the per-event record of loose-pops that AllocPage
	// performed during a SHALLOW savepoint window. Each entry is the
	// (id, original-buffer) pair the loose-pop branch in AllocPage
	// captured before detaching it. RestoreSavepoint replays the log
	// in reverse: re-attach buf to p.dirty[id], pool-Put any post-pop
	// buffer at that id. The loose-pop's bookkeeping side effects
	// (loosePages delete, pendingAllocs add, pendingFrees defensive
	// delete) are tracked via savepointUndoLog independently. Nil for
	// SavepointNested (loose-pop suspended, no events to log).
	loosePopLog []loosePopEntry
}

// snapshotCore is the restorable state both snapshot flavours capture —
// the top-level transaction snapshot (BeginTx / AbortTx / Commit) and
// per-level Savepoints. The flavours share this one CAPTURE but
// deliberately not one RESTORE policy: a savepoint restores
// incrementally (undo-log replay covering only its window's mutations,
// paid only while a savepoint is open), while top-level abort resets
// wholesale (clear the tx-scoped maps, release every slab buffer —
// abort drops everything, so nothing finer is needed). Making the top
// level an implicit depth-0 savepoint would keep p.activeSavepoints
// non-empty for the whole transaction, defeating recordSavepointUndo's
// no-savepoint fast path and putting every ordinary write transaction's
// map mutations on the undo log — the cost the fast path exists to
// avoid (transactions.md §Nested Transactions, the Nesting-depth cost paragraph).
type snapshotCore struct {
	bitmap        *bitmap.Snapshot
	highWaterMark uint64
	rplSegments   []RPLSegmentRef
}

// captureCore snapshots the core restorable state: the bitmap's
// undo-log marker (nil when no bitmap is attached), HighWaterMark, and
// a clone of the in-memory RPL chain.
func (p *Pager) captureCore() snapshotCore {
	c := snapshotCore{
		highWaterMark: p.highWaterMark,
		rplSegments:   slices.Clone(p.rplSegments),
	}
	if p.bitmap != nil {
		c.bitmap = p.bitmap.Snapshot()
	}
	return c
}

// captureSavepointState builds a Savepoint capture of the pager's
// current tx-scoped state — shared between BeginSavepoint and
// BeginShallowSavepoint. The kind field is set by the caller, which
// also pushes the savepoint onto p.activeSavepoints (so the undo log
// recording machinery sees this savepoint as open from its first
// post-capture mutation onward).
//
// The capture is intentionally O(1) per call for the three pending
// sets and dirty additions: it records only the marker
// (undoLogPos = len(p.savepointUndoLog)) and the bitmap.Snapshot
// (which itself is an undo-log marker post-0893be5). The rplSegments
// slice is still clone-captured because mid-tx mutations to it are
// rare (only reclaimRPL tail-trim, which monotonically shrinks the
// chain). The clone is O(chain length) — independent of MaxSize and
// of per-tx mutation count, but workload-dependent at cross-tx
// granularity (a stuck reclamationBound from a lagging reader lets
// the chain accumulate across commits; see transactions.md §Why this
// is cheap for the chain-length workload profile). Scalars
// (highWaterMark, retiredLen, detachedLen, dirtyBytes) are straight
// value copies.
func (p *Pager) captureSavepointState() *Savepoint {
	return &Savepoint{
		snapshotCore: p.captureCore(),
		undoLogPos:   len(p.savepointUndoLog),
		retiredLen:   len(p.retiredPages),
		detachedLen:  len(p.detachedBufs),
		dirtyBytes:   p.dirtyBytes,
	}
}

// recordSavepointUndo appends one undo entry to p.savepointUndoLog when
// at least one savepoint (of either kind) is open. The (field, key,
// wasPresent) trio is sufficient for RestoreSavepoint to undo the
// caller's mutation — see savepointUndoEntry godoc.
//
// Only state-changing mutations should call this — callers check
// wasPresent against the new state and skip the call for no-ops. The
// "skip the append when no savepoint is open" guard mirrors
// bitmap.recordFlip's openSnapshots == 0 fast-path; without it the log
// would grow during ordinary mid-tx work that no savepoint ever sees.
func (p *Pager) recordSavepointUndo(field savepointUndoField, key uint64, wasPresent bool) {
	if len(p.activeSavepoints) == 0 {
		return
	}
	p.savepointUndoLog = append(p.savepointUndoLog, savepointUndoEntry{
		field: field, key: key, wasPresent: wasPresent,
	})
}

// BeginSavepoint captures the current tx-scoped state and pushes a
// NESTED savepoint nesting level. While the level is non-zero (a child
// or any descendant is active), AllocPage suspends loose-page reuse so
// a child can never hand out a page id whose slab buffer an ancestor's
// tree still references — the transactions.md §Invariants clause-
// explicit "child never mutates a parent's slab buffer in place"
// guarantee (pager-slab.md §Slab Lifecycle Across Nested Transactions). Returns nil on a read-only pager.
//
// The returned *Savepoint is owned by the caller; pass it back to
// exactly one of RestoreSavepoint (child rollback) or ReleaseSavepoint
// (child commit). Savepoints are strictly nested (LIFO): a child must
// resolve before its parent, which the root package enforces via the
// parent-freeze rule (Commit frozen; Rollback cascades deepest-first,
// preserving LIFO).
//
// For internal-helper atomicity (the per-op row mutations, indexed or
// not), use BeginShallowSavepoint instead: it preserves loose-pop so
// across-Put loose-page recycling stays bounded.
func (p *Pager) BeginSavepoint() *Savepoint {
	if p.readOnly {
		return nil
	}
	sp := p.captureSavepointState()
	sp.kind = SavepointNested
	p.activeSavepoints = append(p.activeSavepoints, sp)
	p.savepointDepth++
	return sp
}

// BeginShallowSavepoint captures the pager's tx-scoped state as a
// SHALLOW savepoint. Unlike BeginSavepoint, it does NOT increment
// savepointDepth — loose-pop remains enabled during the savepoint
// window so multiple shallow savepoints across many indexed Put /
// Delete calls in one transaction can still reuse same-tx loose
// pages (otherwise file growth would be O(N·depth) instead of
// bounded). Every loose-pop event during the window is recorded in
// the savepoint's loosePopLog (with the original detached buffer) so
// RestoreSavepoint can faithfully reverse the detach.
//
// Same lifecycle as BeginSavepoint: pass the returned handle to
// exactly one of RestoreSavepoint (rollback) or ReleaseSavepoint
// (success). Returns nil on a read-only pager.
//
// Shallow savepoints make a per-row mutation atomic at the
// page-persistence layer: a per-op error inside Keyspace.{Put,Delete}
// / Cursor.Delete / SetKeyspace.{Put,Delete,DeleteValue} (indexed or
// not) or the un-indexed DeleteRange walk, followed by Tx.Commit (the
// engine's rest-of-tx-continues contract), neither orphans the
// in-flight allocations nor publishes still-referenced retired pages
// to the RPL — see free-space.md's entailed bitmap-consistency
// invariant.
//
// Single-active per pager. At most one SHALLOW savepoint may be
// active on the pager at any moment (transactions.md §Nested
// Transactions / §Write-helper error contract). A second concurrent
// SHALLOW would alias the same loose-popped *[]byte across both
// savepoints' loosePopLogs (freespace.go's loose-pop branch walks
// every active SHALLOW and appends the SAME buf pointer to each),
// so on Restore the outer would pool-Put the buffer the inner just
// re-installed — leaving the buf simultaneously in the pool free
// list and in p.dirty[id], a use-after-free in user-visible page
// data on the next bufPool.Get(). The per-op production callers
// (the keyspace-layer row mutations — indexed or not — and the
// un-indexed DeleteRange walk; the only legitimate users of this
// primitive) each open-and-resolve exactly one shallow per call and
// do not nest; the panic surfaces a programming-discipline violation
// at the API surface rather than allowing the buffer alias to form.
//
// SHALLOW-inside-NESTED is allowed (NESTED suspends loose-pop via
// savepointDepth > 0, so no loose-pop event fires inside the
// SHALLOW window → no alias can form). Likewise NESTED-inside-
// SHALLOW: NESTED's Begin establishes the suspension (depth 0 →
// 1) for its own window, so no new loose-pop fires during it; the
// outer SHALLOW's pre-NESTED loosePopLog entries (if any) are not
// aliased because there is still only one SHALLOW on the stack.
func (p *Pager) BeginShallowSavepoint() *Savepoint {
	if p.readOnly {
		return nil
	}
	// Single-active SHALLOW guard. Check BEFORE captureSavepointState
	// so a panic leaves no bitmap.Snapshot leaked into openSnapshots
	// and no partial mutation of activeSavepoints. Scan the full stack
	// (not just the topmost) so the rule holds regardless of NESTED
	// interleaving below: [SHALLOW, NESTED, … attempt SHALLOW] must
	// still panic — two SHALLOWs could still alias on a loose-pop
	// fired after the inner NESTED resolves but before the outer
	// SHALLOW does.
	for _, sp := range p.activeSavepoints {
		if sp.kind == SavepointShallow {
			panic("pager: BeginShallowSavepoint: shallow savepoint already active (single-active per pager)")
		}
	}
	sp := p.captureSavepointState()
	sp.kind = SavepointShallow
	p.activeSavepoints = append(p.activeSavepoints, sp)
	return sp
}

// RestoreSavepoint rolls the pager back to sp (child rollback). It is
// the savepoint analogue of AbortTx, scoped to one nesting level. Order
// matters; the steps run as:
//
//  1. Strict-LIFO precondition check — sp must be the topmost entry in
//     p.activeSavepoints; out-of-order resolution panics (same
//     discipline as bitmap.openSnapshots).
//
//  2. Pop sp from p.activeSavepoints. (Done before the log replay so
//     mutations performed during the replay — there are none today,
//     but the invariant is intentional — would not append fresh
//     undo entries.)
//
//  3. Replay p.savepointUndoLog[sp.undoLogPos:end] in reverse: each
//     entry undoes one observed mutation of pendingAllocs/Frees/
//     loosePages (set map[key] = wasPresent) or dirty additions
//     (delete + pool-Put). Then truncate the log to sp.undoLogPos.
//     This MUST precede the Shallow loose-pop replay below; a Shallow
//     savepoint window whose dirty-add was on a freshly loose-popped
//     id has both a savepointUndoLog (fieldDirty) entry for the in-
//     window CoW and a loosePopLog entry for the detach. Replaying
//     savepointUndoLog first deletes the in-window-installed buffer,
//     leaving dirty[id] absent — exactly the pre-loose-pop-replay
//     state — so the subsequent loose-pop replay can cleanly re-
//     install the original buffer without overwriting anything.
//
//  4. (Shallow only) Replay sp.loosePopLog in reverse: re-attach each
//     captured (id, original-buffer) to p.dirty[id]; if dirty[id]
//     somehow still holds a buffer (a post-pop CoW that the
//     savepointUndoLog replay did not delete because no fieldDirty
//     entry recorded it — defensive, currently unreachable), pool-Put
//     it before installing the original.
//
//  5. bitmap.Restore (per-Snapshot undo-log replay), then scalar
//     restores for HighWaterMark, rplSegments, retiredPages length,
//     detachedBufs length, dirtyBytes.
//
//  6. (Nested only) Decrement savepointDepth so loose-pop re-enables
//     when the stack empties.
//
// No buffer-content restoration is needed under Nested: the child's
// modifications all live at fresh page IDs in fresh slab buffers
// (loose-pop suspended), so dropping those buffers drops the
// modification and the ancestor's buffers were never touched
// (transactions.md §Why this is cheap). Under Shallow the loose-pop
// replay restores buffer content at popped IDs explicitly.
//
// sp is consumed — do not reuse it afterward.
func (p *Pager) RestoreSavepoint(sp *Savepoint) {
	if p.readOnly || sp == nil {
		return
	}
	// Strict-LIFO: panic on out-of-order RestoreSavepoint, mirroring
	// the bitmap layer's openSnapshots LIFO discipline (and
	// transactions.md §Nested Transactions's "out-of-order Restore or
	// Discard panics rather than silently corrupt state"). A best-
	// effort no-op would leave an outer savepoint dangling in
	// activeSavepoints with a stale bitmap snapshot, surfacing as a
	// delayed panic inside bitmap.Restore / Discard at its eventual
	// resolution.
	//
	// Before panicking, Discard sp's bitmap snapshot so a caller that
	// recovers from the panic and proceeds to commit (via
	// discardTxSnapshot) does not leave sp.bitmap dangling in
	// bitmap.openSnapshots across the tx boundary — the undoLog would
	// otherwise survive into the next tx and a future Snapshot's
	// logPos would index against entries from the prior tx. Recovering
	// from this panic is unsupported (programmer error), but defensive
	// cleanup keeps the bitmap state consistent if a test or
	// supervisory layer does recover.
	n := len(p.activeSavepoints)
	if n == 0 || p.activeSavepoints[n-1] != sp {
		if sp.bitmap != nil && p.bitmap != nil {
			p.bitmap.Discard(sp.bitmap)
		}
		panic("pager: RestoreSavepoint: out-of-order savepoint resolution")
	}
	p.activeSavepoints = p.activeSavepoints[:n-1]

	// Step 3: replay savepointUndoLog in reverse, then truncate.
	for i := len(p.savepointUndoLog) - 1; i >= sp.undoLogPos; i-- {
		e := p.savepointUndoLog[i]
		switch e.field {
		case fieldPendingAllocs:
			if e.wasPresent {
				p.pendingAllocs[e.key] = struct{}{}
			} else {
				delete(p.pendingAllocs, e.key)
			}
		case fieldPendingFrees:
			if e.wasPresent {
				p.pendingFrees[e.key] = struct{}{}
			} else {
				delete(p.pendingFrees, e.key)
			}
		case fieldLoosePages:
			if e.wasPresent {
				p.loosePages[e.key] = struct{}{}
			} else {
				delete(p.loosePages, e.key)
			}
		case fieldDirty:
			// Delete the buffer the in-window installer placed at this
			// id and pool-Put it. wasPresent is always false for
			// fieldDirty (the installers no-op on present); the buffer
			// presently at dirty[id] is the one this entry's mutation
			// produced.
			if buf, ok := p.dirty[e.key]; ok {
				p.bufPool.Put(buf)
				delete(p.dirty, e.key)
			}
		}
	}
	p.savepointUndoLog = p.savepointUndoLog[:sp.undoLogPos]

	// Step 4: Shallow loose-pop replay (Nested has no loose-pop events
	// because savepointDepth > 0 suspended the allocator's loose-pop
	// branch for the window).
	//
	// Two cases per entry, distinguished by entry.wasPreWindow:
	//
	//   - wasPreWindow=true: dirty[entry.id] was present at this
	//     savepoint's Begin time. The loose-pop detached it in-window;
	//     Restore re-attaches it. Any subsequent in-window CoW that
	//     re-installed at id appended a (fieldDirty, id, false) undo
	//     entry; the step-3 replay above already deleted it, so
	//     dirty[entry.id] is absent here in the common case. The
	//     defensive pool-Put on a non-absent dirty[entry.id] covers any
	//     future code path that mutates dirty without going through
	//     the recorded installers. The single-active SHALLOW invariant
	//     (BeginShallowSavepoint's panic guard; transactions.md §Nested
	//     Transactions §Why this is cheap "single-owner contract")
	//     guarantees buf is owned by exactly one loosePopLog entry, so
	//     the pool-Put never aliases another savepoint's still-live
	//     entry.buf at re-install time.
	//
	//   - wasPreWindow=false: dirty[entry.id] was added in-window
	//     (CoW/AllocSlab/AllocSlabRun) and then loose-popped, also
	//     in-window. The step-3 replay deleted the post-pop buffer (if
	//     any) but cannot undo the original in-window add — that
	//     buffer is held by entry.buf alone (detached by the loose-
	//     pop). Restore must drop it to the pool and NOT re-install:
	//     pre-window dirty[entry.id] was absent. Re-installing would
	//     leak an in-window buffer into the post-Restore dirty map
	//     (dirtyBytes accounting desync; pager-
	//     slab.md byte-slice-ownership invariant violation if anyone
	//     held a []byte from the in-window CoW).
	if sp.kind == SavepointShallow {
		for i := len(sp.loosePopLog) - 1; i >= 0; i-- {
			entry := sp.loosePopLog[i]
			if entry.wasPreWindow {
				if cur, ok := p.dirty[entry.id]; ok {
					p.bufPool.Put(cur)
				}
				p.dirty[entry.id] = entry.buf
			} else {
				p.bufPool.Put(entry.buf)
			}
		}
	}

	// Step 5: bitmap.Restore + scalar restores.
	if sp.bitmap != nil && p.bitmap != nil {
		p.bitmap.Restore(sp.bitmap)
	}
	p.highWaterMark = sp.highWaterMark
	p.rplSegments = sp.rplSegments
	if len(p.retiredPages) > sp.retiredLen {
		p.retiredPages = p.retiredPages[:sp.retiredLen]
	}
	// Detached-buf cleanup. For Nested, loose-pop was suspended so
	// detachedBufs[detachedLen:] is empty in practice — the pool-Put
	// loop is defensive. For Shallow, the entries past detachedLen
	// are the loose-pop log captures; step 4 above either re-attached
	// each to dirty[id] (wasPreWindow=true) or pool-Put it
	// (wasPreWindow=false). In both cases we must NOT pool-Put them
	// again here (double-Put would corrupt the pool's free list).
	// Just truncate.
	if sp.kind != SavepointShallow {
		for i := sp.detachedLen; i < len(p.detachedBufs); i++ {
			p.bufPool.Put(p.detachedBufs[i])
		}
	}
	if len(p.detachedBufs) > sp.detachedLen {
		p.detachedBufs = p.detachedBufs[:sp.detachedLen]
	}
	p.dirtyBytes = sp.dirtyBytes

	// Step 6: Nested loose-pop re-enable.
	if sp.kind == SavepointNested && p.savepointDepth > 0 {
		p.savepointDepth--
	}
}

// ReleaseSavepoint discards sp without restoring (child commit): the
// child's allocations, frees, and slab buffers remain in the parent's
// pager state, to be published at the top-level Commit. For Nested
// kind, popping the nesting level re-enables loose-page reuse once
// the stack empties.
//
// The savepointUndoLog entries past sp.undoLogPos are NOT truncated
// here when an outer savepoint is still open — those entries are this
// savepoint's window-mutations and must remain replayable through any
// still-open outer Restore (transactions.md §Why this is cheap, applied
// at savepoint granularity). When sp's release leaves p.activeSavepoints
// empty, the log truncates to length 0 so memory does not survive
// across savepoint windows (mirrors bitmap.Discard's truncation when
// openSnapshots becomes empty).
//
// Also releases the bitmap's per-Snapshot undo-log tracking for sp:
// any flips appended during the child window stay in the parent's
// (still-open) outer Snapshot's revert range, so an outer Restore
// (top-level AbortTx) still undoes them.
//
// For Shallow kind, the loose-pop log is dropped (the captures stay
// in detachedBufs to be pool-Put at tx-end, exactly as if no
// savepoint had wrapped the loose-pops).
func (p *Pager) ReleaseSavepoint(sp *Savepoint) {
	if p.readOnly || sp == nil {
		return
	}
	// Strict-LIFO mirror of RestoreSavepoint's guard. Pre-panic
	// bitmap.Discard for the same defensive reason — recover + commit
	// must not leak sp.bitmap into the next tx's openSnapshots.
	n := len(p.activeSavepoints)
	if n == 0 || p.activeSavepoints[n-1] != sp {
		if sp.bitmap != nil && p.bitmap != nil {
			p.bitmap.Discard(sp.bitmap)
		}
		panic("pager: ReleaseSavepoint: out-of-order savepoint resolution")
	}
	p.activeSavepoints = p.activeSavepoints[:n-1]
	if len(p.activeSavepoints) == 0 {
		p.savepointUndoLog = p.savepointUndoLog[:0]
	}
	if sp.bitmap != nil && p.bitmap != nil {
		p.bitmap.Discard(sp.bitmap)
	}
	if sp.kind == SavepointNested && p.savepointDepth > 0 {
		p.savepointDepth--
	}
}

// SavepointDepth reports the number of active (unresolved) NESTED
// savepoints. Zero on a top-level transaction with no open child or
// when only SHALLOW savepoints are active. Used by tests and by the
// root package's parent-freeze guard (which is interested in
// user-facing nested-tx semantics, not internal-helper savepoints).
func (p *Pager) SavepointDepth() int { return p.savepointDepth }

// ActiveSavepointCount reports how many savepoints are currently
// unresolved — the all-resolved-before-Commit assumption's observable
// (commit.go's buffer-discard sweep and the loose-pop guard both rely
// on it).
func (p *Pager) ActiveSavepointCount() int { return len(p.activeSavepoints) }
