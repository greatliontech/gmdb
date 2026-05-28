package pager

import (
	"maps"
	"slices"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
)

// SavepointKind selects the savepoint flavour. The Nested kind is the
// canonical user-facing primitive (transactions.md §Nested
// Transactions, BeginChild semantics) that suspends loose-pop for the
// duration so a child can never hand out a page id whose slab buffer
// the parent's tree still references (Inv-N1). The Shallow kind is
// the internal-helper flavour used by per-row indexed maintenance
// (Keyspace.Put / Delete / Cursor.Delete; SetKeyspace.Put / Delete /
// DeleteValue) to make the helper + the subsequent row btree mutation
// atomic at the page-persistence layer without giving up across-Put
// loose-page recycling. Loose-pop stays enabled under a Shallow
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
// added to pendingAllocs. Restoring this event undoes all three.
type loosePopEntry struct {
	id  uint64
	buf *[]byte
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
// The capture is by value/clone (not by reference into the live pager
// maps) so the live maps can keep mutating while the savepoint is held.
//
// Cost contract (transactions.md §Nested Transactions "Cost is
// proportional to pages modified at that level, not total database
// size"): every field is O(this-level changes) — bitmap.Snapshot is an
// undo-log marker (O(flips since BeginSavepoint), not O(MaxSize));
// pendingAllocs/Frees/loosePages/dirtyKeys are clones of maps whose
// cardinality is bounded by this-tx mutations; rplSegments is the chain
// inherited from the active meta (bounded). No field scales with
// MaxSize.
//
// Kind=SavepointShallow additionally carries loosePopLog: a per-event
// record of every loose-pop AllocPage performed during the savepoint
// window. Kind=SavepointNested leaves loosePopLog nil — loose-pop is
// suspended for nested savepoints, so the field is unreachable.
type Savepoint struct {
	kind          SavepointKind
	bitmap        *bitmap.Snapshot
	highWaterMark uint64
	rplSegments   []RPLSegmentRef
	pendingAllocs map[uint64]struct{}
	pendingFrees  map[uint64]struct{}
	loosePages    map[uint64]struct{}
	// retiredPages and detachedBufs are append-only over a transaction
	// body (FreePage appends prior-tx frees; AllocPage's loose-pop is the
	// only producer of detachedBufs, and it is suspended while a savepoint
	// is active — see AllocPage). Truncating to the captured length is
	// therefore a complete restore.
	retiredLen  int
	detachedLen int
	// dirtyKeys is the set of slab page IDs live at capture time. With
	// loose-pop suspended (savepointDepth > 0), p.dirty only grows during
	// a child, so any id absent from this set on restore is a child
	// addition whose buffer is released back to the pool.
	dirtyKeys  map[uint64]struct{}
	dirtyBytes int

	// loosePopLog is the per-event record of loose-pops that AllocPage
	// performed during a SHALLOW savepoint window. Each entry is the
	// (id, original-buffer) pair the loose-pop branch in AllocPage
	// captured before detaching it. RestoreSavepoint replays the log
	// in reverse: re-attach buf to p.dirty[id], pool-Put any
	// post-pop buffer at that id, and rely on the clone-restore to
	// re-add id to loosePages / remove from pendingAllocs. Nil for
	// SavepointNested (loose-pop suspended, no events to log).
	loosePopLog []loosePopEntry
}

// captureSavepointState builds a Savepoint capture of the pager's
// current tx-scoped state — shared between BeginSavepoint and
// BeginShallowSavepoint. The kind field is set by the caller.
func (p *Pager) captureSavepointState() *Savepoint {
	sp := &Savepoint{
		highWaterMark: p.highWaterMark,
		rplSegments:   slices.Clone(p.rplSegments),
		pendingAllocs: maps.Clone(p.pendingAllocs),
		pendingFrees:  maps.Clone(p.pendingFrees),
		loosePages:    maps.Clone(p.loosePages),
		retiredLen:    len(p.retiredPages),
		detachedLen:   len(p.detachedBufs),
		dirtyKeys:     make(map[uint64]struct{}, len(p.dirty)),
		dirtyBytes:    p.dirtyBytes,
	}
	if p.bitmap != nil {
		sp.bitmap = p.bitmap.Snapshot()
	}
	for id := range p.dirty {
		sp.dirtyKeys[id] = struct{}{}
	}
	return sp
}

// BeginSavepoint captures the current tx-scoped state and pushes a
// NESTED savepoint nesting level. While the level is non-zero (a child
// or any descendant is active), AllocPage suspends loose-page reuse so
// a child can never hand out a page id whose slab buffer an ancestor's
// tree still references — the transactions.md §Invariants clause-
// explicit "child never mutates a parent's slab buffer in place"
// guarantee (Inv-N1). Returns nil on a read-only pager.
//
// The returned *Savepoint is owned by the caller; pass it back to
// exactly one of RestoreSavepoint (child rollback) or ReleaseSavepoint
// (child commit). Savepoints are strictly nested (LIFO): a child must
// resolve before its parent, which the root package enforces via the
// parent-freeze rule (ErrChildActive).
//
// For internal-helper atomicity (per-row indexed maintenance), use
// BeginShallowSavepoint instead: it preserves loose-pop so across-Put
// loose-page recycling stays bounded.
func (p *Pager) BeginSavepoint() *Savepoint {
	if p.readOnly {
		return nil
	}
	sp := p.captureSavepointState()
	sp.kind = SavepointNested
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
// Shallow savepoints make a per-row indexed-maintenance helper
// invocation atomic at the page-persistence layer: a per-op error
// inside an indexed Keyspace.{Put,Delete} / Cursor.Delete /
// SetKeyspace.{Put,Delete,DeleteValue} followed by Tx.Commit (the
// engine's rest-of-tx-continues contract) does not orphan the
// in-flight allocations — see free-space.md's entailed bitmap-
// consistency invariant.
func (p *Pager) BeginShallowSavepoint() *Savepoint {
	if p.readOnly {
		return nil
	}
	sp := p.captureSavepointState()
	sp.kind = SavepointShallow
	p.activeShallowSavepoints = append(p.activeShallowSavepoints, sp)
	return sp
}

// RestoreSavepoint rolls the pager back to sp (child rollback). It is
// the savepoint analogue of AbortTx, scoped to one nesting level:
//
//   - For Shallow kind: replay the loosePopLog in reverse FIRST so
//     each loose-pop event is undone (re-attach the original buffer
//     to p.dirty[id], pool-Put any post-pop CoW buffer). Then proceed
//     with the steps below.
//   - bitmap, HighWaterMark, RPL chain, pendingAllocs, pendingFrees, and
//     loosePages are reverted to their capture-time values;
//   - retiredPages and detachedBufs are truncated to their captured
//     lengths (for Nested any detached buffer beyond the cut is
//     pool-returned — none is expected because loose-pop was
//     suspended; for Shallow the truncated range was already re-
//     attached by the loose-pop replay above, so the truncate is a
//     pure length adjustment with no pool-Put);
//   - every slab buffer added since the savepoint (a child CoW /
//     AllocSlab) is returned to the pool and removed from p.dirty;
//   - dirtyBytes is restored.
//
// No buffer-content restoration is needed under Nested: the child's
// modifications all live at fresh page IDs in fresh slab buffers
// (loose-pop suspended), so dropping those buffers drops the
// modification and the ancestor's buffers were never touched
// (transactions.md §Why this is cheap). Under Shallow the loose-pop
// replay restores buffer content at popped IDs explicitly.
//
// sp is consumed — its cloned maps are adopted as the pager's live maps,
// so do not reuse sp afterward.
func (p *Pager) RestoreSavepoint(sp *Savepoint) {
	if p.readOnly || sp == nil {
		return
	}
	if sp.kind == SavepointShallow {
		// Strict-LIFO: panic on out-of-order RestoreSavepoint, mirroring
		// the bitmap layer's openSnapshots LIFO discipline (and
		// transactions.md §Nested Transactions's "out-of-order Restore
		// or Discard panics rather than silently corrupt state"). A
		// best-effort no-op would leave an outer shallow savepoint
		// dangling in activeShallowSavepoints with a stale bitmap
		// snapshot, surfacing as a delayed panic inside bitmap.Restore
		// / Discard at its eventual resolution.
		//
		// Before panicking, Discard sp's bitmap snapshot so a caller
		// that recovers from the panic and proceeds to commit (via
		// discardTxSnapshot) does not leave sp.bitmap dangling in
		// bitmap.openSnapshots across the tx boundary — the undoLog
		// would otherwise survive into the next tx and a future
		// Snapshot's logPos would index against entries from the prior
		// tx. Recovering from this panic is unsupported (programmer
		// error), but defensive cleanup keeps the bitmap state
		// consistent if a test or supervisory layer does recover.
		n := len(p.activeShallowSavepoints)
		if n == 0 || p.activeShallowSavepoints[n-1] != sp {
			if sp.bitmap != nil && p.bitmap != nil {
				p.bitmap.Discard(sp.bitmap)
			}
			panic("pager: RestoreSavepoint: out-of-order shallow savepoint resolution")
		}
		p.activeShallowSavepoints = p.activeShallowSavepoints[:n-1]
		// Replay loose-pops in reverse: at each event, dirty[id]
		// presently holds either bufB (a post-pop CoW buffer) or
		// nothing (popped but never re-CoW'd in this window). Pool-
		// Put bufB; install the original bufA the loose-pop detached.
		// The clone-restore steps below then re-add id to loosePages
		// (it was in the loosePages clone pre-savepoint) and remove
		// id from pendingAllocs (the loose-pop added it; the clone
		// has the pre-savepoint state without it).
		for i := len(sp.loosePopLog) - 1; i >= 0; i-- {
			entry := sp.loosePopLog[i]
			// AllocPage's loose-pop detached entry.buf from dirty[id]
			// before returning, so anything subsequently in
			// dirty[id] is a post-pop CoW/AllocSlab fresh buffer (or
			// a second loose-pop's detach + CoW). It cannot equal
			// entry.buf — bufPool.Get hands out fresh allocations,
			// never the buffer still alive in detachedBufs. Pool-Put
			// the post-pop buffer (if any) and restore the original.
			if cur, ok := p.dirty[entry.id]; ok {
				p.bufPool.Put(cur)
			}
			p.dirty[entry.id] = entry.buf
		}
	}
	if sp.bitmap != nil && p.bitmap != nil {
		p.bitmap.Restore(sp.bitmap)
	}
	p.highWaterMark = sp.highWaterMark
	p.rplSegments = sp.rplSegments
	// Release child-added slab buffers before adopting the captured
	// pending sets so accounting stays consistent if the pool panics.
	//
	// Loose-pop interaction (Shallow kind only): the replay loop
	// above already restored dirty[id] for every (id, buf) the
	// loose-pop log captured. Each such id falls into one of two
	// cases here:
	//
	//   - id WAS in p.dirty pre-savepoint (the common case: a
	//     prior-tx page CoW'd into a same-tx-alloc that later went
	//     loose) → id ∈ sp.dirtyKeys → kept, dirty[id] now holds the
	//     original buffer the replay re-installed.
	//   - id was NOT in p.dirty pre-savepoint (the alloc-bitmap →
	//     CoW → FreePage(loose) → AllocPage(loose-pop) chain inside
	//     the savepoint window) → id ∉ sp.dirtyKeys → dropped here,
	//     buffer the replay installed is pool-Put'd. Correct: the
	//     alloc that originated the chain is also being reverted by
	//     the pendingAllocs clone-restore, so dirty[id] should
	//     mirror the pre-savepoint "absent" state.
	for id, buf := range p.dirty {
		if _, existed := sp.dirtyKeys[id]; !existed {
			p.bufPool.Put(buf)
			delete(p.dirty, id)
		}
	}
	p.pendingAllocs = sp.pendingAllocs
	p.pendingFrees = sp.pendingFrees
	p.loosePages = sp.loosePages
	if len(p.retiredPages) > sp.retiredLen {
		p.retiredPages = p.retiredPages[:sp.retiredLen]
	}
	// Detached-buf cleanup. For Nested, loose-pop was suspended so
	// detachedBufs[detachedLen:] is empty in practice — the pool-Put
	// loop is defensive. For Shallow, the entries past detachedLen
	// are exactly the loose-pop log captures; the replay above
	// re-installed each as dirty[id], so we must NOT pool-Put them
	// again (double-Put would corrupt the pool's free list). Just
	// truncate.
	if sp.kind != SavepointShallow {
		for i := sp.detachedLen; i < len(p.detachedBufs); i++ {
			p.bufPool.Put(p.detachedBufs[i])
		}
	}
	if len(p.detachedBufs) > sp.detachedLen {
		p.detachedBufs = p.detachedBufs[:sp.detachedLen]
	}
	p.dirtyBytes = sp.dirtyBytes
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
	if sp.kind == SavepointShallow {
		// Strict-LIFO mirror of RestoreSavepoint's guard. Pre-panic
		// bitmap.Discard for the same defensive reason — recover +
		// commit must not leak sp.bitmap into the next tx's
		// openSnapshots.
		n := len(p.activeShallowSavepoints)
		if n == 0 || p.activeShallowSavepoints[n-1] != sp {
			if sp.bitmap != nil && p.bitmap != nil {
				p.bitmap.Discard(sp.bitmap)
			}
			panic("pager: ReleaseSavepoint: out-of-order shallow savepoint resolution")
		}
		p.activeShallowSavepoints = p.activeShallowSavepoints[:n-1]
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
