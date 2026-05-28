package pager

import (
	"maps"
	"slices"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
)

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
type Savepoint struct {
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
}

// BeginSavepoint captures the current tx-scoped state and pushes a
// savepoint nesting level. While the level is non-zero (a child or any
// descendant is active), AllocPage suspends loose-page reuse so a child
// can never hand out a page id whose slab buffer an ancestor's tree
// still references — the transactions.md §Invariants clause-explicit
// "child never mutates a parent's slab buffer in place" guarantee
// (Inv-N1). Returns nil on a read-only pager.
//
// The returned *Savepoint is owned by the caller; pass it back to
// exactly one of RestoreSavepoint (child rollback) or ReleaseSavepoint
// (child commit). Savepoints are strictly nested (LIFO): a child must
// resolve before its parent, which the root package enforces via the
// parent-freeze rule (ErrChildActive).
func (p *Pager) BeginSavepoint() *Savepoint {
	if p.readOnly {
		return nil
	}
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
	p.savepointDepth++
	return sp
}

// RestoreSavepoint rolls the pager back to sp (child rollback). It is
// the savepoint analogue of AbortTx, scoped to one nesting level:
//
//   - bitmap, HighWaterMark, RPL chain, pendingAllocs, pendingFrees, and
//     loosePages are reverted to their capture-time values;
//   - retiredPages and detachedBufs are truncated to their captured
//     lengths (any detached buffer beyond the cut is pool-returned —
//     none is expected while a savepoint is active);
//   - every slab buffer added since the savepoint (a child CoW /
//     AllocSlab) is returned to the pool and removed from p.dirty;
//   - dirtyBytes is restored.
//
// No buffer-content restoration is needed: the child's modifications all
// live at fresh page IDs in fresh slab buffers (loose-pop suspended), so
// dropping those buffers drops the modification and the ancestor's
// buffers were never touched (transactions.md §Why this is cheap).
//
// sp is consumed — its cloned maps are adopted as the pager's live maps,
// so do not reuse sp afterward.
func (p *Pager) RestoreSavepoint(sp *Savepoint) {
	if p.readOnly || sp == nil {
		return
	}
	if sp.bitmap != nil && p.bitmap != nil {
		p.bitmap.Restore(sp.bitmap)
	}
	p.highWaterMark = sp.highWaterMark
	p.rplSegments = sp.rplSegments
	// Release child-added slab buffers before adopting the captured
	// pending sets so accounting stays consistent if the pool panics.
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
	for i := sp.detachedLen; i < len(p.detachedBufs); i++ {
		p.bufPool.Put(p.detachedBufs[i])
	}
	if len(p.detachedBufs) > sp.detachedLen {
		p.detachedBufs = p.detachedBufs[:sp.detachedLen]
	}
	p.dirtyBytes = sp.dirtyBytes
	if p.savepointDepth > 0 {
		p.savepointDepth--
	}
}

// ReleaseSavepoint discards sp without restoring (child commit): the
// child's allocations, frees, and slab buffers remain in the parent's
// pager state, to be published at the top-level Commit. Popping the
// nesting level re-enables loose-page reuse once the stack empties.
//
// Also releases the bitmap's per-Snapshot undo-log tracking for sp:
// any flips appended during the child window stay in the parent's
// (still-open) outer Snapshot's revert range, so an outer Restore
// (top-level AbortTx) still undoes them.
func (p *Pager) ReleaseSavepoint(sp *Savepoint) {
	if p.readOnly || sp == nil {
		return
	}
	if sp.bitmap != nil && p.bitmap != nil {
		p.bitmap.Discard(sp.bitmap)
	}
	if p.savepointDepth > 0 {
		p.savepointDepth--
	}
}

// SavepointDepth reports the number of active (unresolved) savepoints.
// Zero on a top-level transaction with no open child. Used by tests and
// by the root package's parent-freeze guard.
func (p *Pager) SavepointDepth() int { return p.savepointDepth }
