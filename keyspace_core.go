package gmdb

import (
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// keyspaceCore is the state and behavior shared by *Keyspace and
// *SetKeyspace. Both embed it. The only per-kind difference is the
// open-cursor slice (Keyspace.openCursors []*Cursor vs
// SetKeyspace.openSetCursors []*SetCursor), which stays on each outer
// type; everything that touches only shared state — the in-tx
// descriptor and flush state, the index-handle registry, and the
// builder config — lives here once and is promoted to both embedders.
type keyspaceCore struct {
	tx   *Tx
	name uniqueNameHandle

	// desc is the in-tx view of the keyspace's descriptor. Mutated in
	// place by the Put / Delete data-op paths (descriptor.Root +
	// descriptor.Count); the deferred-flush refactor promotes
	// it to the on-disk keyspace B+tree at Tx.Commit's flushKeyspaces
	// walk, not per data op.
	desc page.KeyspaceDescriptor

	// state controls how Tx.Commit's flushKeyspaces walk treats this
	// handle: Created and Dirty cause a btree.Put on the keyspace
	// B+tree; Clean is skipped. See keyspaceState godocs.
	state keyspaceState

	// dead is set by Tx.DeleteKeyspace on every handle returned against
	// this name in this tx. Once dead, every handle/cursor op returns
	// ErrKeyspaceClosed; re-creating the same name via a Create* call
	// does NOT clear dead on the old handle (a fresh handle is
	// allocated; the old stays dead). Per api-surface.md §Keyspace API
	// DeleteKeyspace.
	dead bool

	// openIndexHandles tracks every *Index returned by Index(name) in
	// this tx. Atomic Put / Delete / Cursor.Delete mutate index trees
	// (applyIndexMaintenanceOn*); TxIndexes.Rebuild / TxIndexes.Drop replace
	// or free a per-index data tree wholesale. Each of those CoWs or
	// frees pages reached by in-flight *btree.Cursor instances opened
	// from these handles' iter closures, so markIndexHandlesStale (and
	// the by-name / dead variants) walk this slice to MarkStale every
	// such cursor (Inv-IHS1). Mirrors the per-kind openCursors /
	// openSetCursors structurally; see indexing.md §Handle Invalidation.
	openIndexHandles []*Index

	// indexes carries the pinned per-index state for this tx, keyed by
	// IndexDecl.Name. Populated by the Open* / Create* keyspace methods
	// with the validated supplied IndexDecls (first-Extract-wins per
	// indexing.md §Re-opening). Each pinnedIndex carries the user's
	// IndexDecl, the cached schema-hash, and the index's data-tree
	// root+count. nil for keyspaces with no declared indexes.
	indexes map[string]*pinnedIndex

	// readOnly is true when the handle was opened via an *ReadOnly
	// method. Used by the same-tx re-open idempotence check to
	// surface ErrKeyspaceAlreadyOpen when a caller mixes a read-write
	// and a read-only open of the same name within one tx (indexing.md
	// §Re-opening).
	readOnly bool
}

// Name returns the keyspace's name (the unique-interned identity).
// Allocations: returns the underlying string from the interned handle
// without copying.
func (ks *keyspaceCore) Name() string { return ks.name.Value() }

// builderCfg returns the page.Config to pass to btree.* calls for this
// keyspace. When a per-keyspace RestartGroupTarget is set (via
// SetKeyspaceConfig per keyspaces.md invariant #6) it overrides the
// engine default — newly written leaves use the per-keyspace target.
// Decoding ignores RestartGroupTarget so the override is safe on the
// Get side too.
func (ks *keyspaceCore) builderCfg() page.Config {
	cfg := ks.tx.pgr.Config()
	if ks.desc.RestartGroupTarget != 0 {
		cfg.RestartGroupTarget = ks.desc.RestartGroupTarget
	}
	return cfg
}

// newRootCursor builds a *btree.Cursor positioned at the keyspace's
// current root: a writable cursor (carrying the engine merge threshold)
// on a write transaction, else a read cursor. The per-kind Cursor /
// SetCursor wrappers attach their own state — and their own
// openCursors / openSetCursors registration — around the returned
// cursor.
func (ks *keyspaceCore) newRootCursor() *btree.Cursor {
	cfg := ks.builderCfg()
	if ks.tx.writable {
		return btree.NewCursor(btreeWriter{ks.tx.pgr}, cfg, ks.desc.Root, ks.tx.db.opts.MergeThreshold)
	}
	return btree.NewReadCursor(ks.tx.pgr, cfg, ks.desc.Root)
}

// requireWritable verifies the handle is open on a live, writable
// keyspace: an open non-closed tx, not DeleteKeyspace'd, and not opened
// read-only. It is the state guard shared by every mutator. Key-taking
// mutators add the non-empty-key check via checkWritable; key-less and
// range mutators (BulkLoad, DeleteRange — which permits nil bounds)
// call this directly and do their own bound/state checks. Returns
// ErrKeyspaceClosed / ErrReadOnly (or the tx-closed error) on failure.
func (ks *keyspaceCore) requireWritable() error {
	if err := ks.tx.requireOpen(true); err != nil {
		return err
	}
	if ks.dead {
		return ErrKeyspaceClosed
	}
	if ks.readOnly {
		return ErrReadOnly
	}
	return nil
}

// checkWritable is requireWritable plus the non-empty-key check that
// every key-taking mutator (Keyspace.Put / Delete, SetKeyspace.Put /
// Delete / DeleteValue) runs before touching the tree. Returns
// ErrKeyEmpty for a nil/empty key.
func (ks *keyspaceCore) checkWritable(key []byte) error {
	if err := ks.requireWritable(); err != nil {
		return err
	}
	if len(key) == 0 {
		return ErrKeyEmpty
	}
	return nil
}

// checkReadable is the guard every key-taking reader (Keyspace.Get,
// SetKeyspace.Has / HasValue / CountValues) runs: the handle must be
// open and live and key non-empty. Unlike checkWritable it permits a
// read-only handle and does not require a write tx.
func (ks *keyspaceCore) checkReadable(key []byte) error {
	if err := ks.tx.requireOpen(false); err != nil {
		return err
	}
	if ks.dead {
		return ErrKeyspaceClosed
	}
	if len(key) == 0 {
		return ErrKeyEmpty
	}
	return nil
}

// deleteRangeUnindexed runs the atomic three-phase btree.DeleteRange
// walker over [start, end) on an un-indexed keyspace, then updates
// desc.Root / desc.Count and broadcasts cursor invalidation via
// markStale. Shared by the un-indexed paths of Keyspace.DeleteRange and
// SetKeyspace.DeleteRange; the only per-kind inputs are cellFree (the
// per-cell release callback — single-value for a Keyspace; subpage /
// nested-tree / overflow plus value-tally for a SetKeyspace) and the
// cursor-stale broadcast (markCursorsStale / markSetCursorsStale).
//
// The returned count and desc.Count are kept in the same unit per kind
// (entries for a Keyspace, values for a SetKeyspace — cellFree tallies
// values), so the desc.Count subtraction is consistent for both. A
// count exceeding desc.Count is impossible absent corruption and is
// surfaced as ErrCorrupted rather than underflowing.
func (ks *keyspaceCore) deleteRangeUnindexed(start, end []byte, cellFree btree.PerCellFreeFn, markStale func()) (uint64, error) {
	cfg := ks.builderCfg()
	mergeThreshold := ks.tx.db.opts.MergeThreshold
	count, newRoot, err := btree.DeleteRange(btreeWriter{ks.tx.pgr}, cfg, ks.desc.Root, mergeThreshold, start, end, cellFree)
	if err != nil {
		return 0, mapBtreeErr(err)
	}
	if count == 0 {
		// No-op — no descriptor or cursor invalidation, no transition.
		return 0, nil
	}
	if count > ks.desc.Count {
		return 0, fmt.Errorf("%w: DeleteRange count=%d exceeds desc.Count=%d for keyspace %q",
			ErrCorrupted, count, ks.desc.Count, ks.name.Value())
	}
	ks.desc.Root = newRoot
	ks.desc.Count -= count
	ks.markDirty()
	markStale()
	return count, nil
}

// markDirty transitions the handle's state to Dirty unless it is
// already Created (Created stays Created — both flush variants do
// btree.Put). Centralized so Put / Delete / Cursor.Delete /
// SetKeyspaceConfig route through one code path.
func (ks *keyspaceCore) markDirty() {
	if ks.state == keyspaceStateCreated {
		return
	}
	ks.state = keyspaceStateDirty
}

// NextSequence increments the keyspace's persisted sequence counter and
// returns the new value — a monotonically increasing, per-keyspace number
// (the first call on a fresh keyspace returns 1), à la bbolt's
// Bucket.NextSequence. The increment rides the enclosing write transaction:
// it is persisted atomically at Commit (via the descriptor flush) and
// discarded on Rollback, and the counter survives reopen. Defined on
// keyspaceCore so both Keyspace and SetKeyspace expose it (api-surface.md
// §Keyspace API / §Set API). Returns ErrReadOnly on a read-only handle,
// ErrKeyspaceClosed if the keyspace was deleted in this tx, or the tx-closed
// error.
func (ks *keyspaceCore) NextSequence() (uint64, error) {
	if err := ks.requireWritable(); err != nil {
		return 0, err
	}
	ks.desc.NextSeq++
	ks.markDirty()
	return ks.desc.NextSeq, nil
}

// descriptor returns the in-tx descriptor pointer. Used by the
// registry-CRUD helpers (index_codec.go) to satisfy the
// descriptorOwner interface — registryPut / registryDelete mutate the
// descriptor in place AND call markDirty() so the
// flushKeyspaces walk persists the mutation. Unexported.
func (ks *keyspaceCore) descriptor() *page.KeyspaceDescriptor {
	return &ks.desc
}

// markIndexHandlesStale invokes MarkStale on every in-flight
// *btree.Cursor opened by an *Index handle from this keyspace, and
// refreshes the cursor's tracked rootID to the (possibly mutated)
// pinnedIndex.root. Called by Put / Delete / Cursor.Delete after the
// atomic-maintenance step that mutates index trees:
// applyIndexMaintenanceOn{Put,Delete} runs btree.Put / btree.Delete on
// each declared index's data tree → CoWs pages reachable from in-flight
// iter cursors → those cursors must MarkStale or the next c.Next()
// reads CoW'd-then-released leaf pages (Inv-IHS1).
//
// Also called by Tx.DeleteKeyspace (Inv-IHS3). Caveat: in that path,
// idx.pinned.root has already been FreeSubtree'd by
// retireIndexRegistry, so SetRootID stores a FREED pageID into every
// stale cursor's tracked-root. This is safe under the current API
// because every *Index entry method (Stats / Lookup / LookupKeys /
// Range / Prefix / Get / Err) short-circuits via keyspaceDead() before
// issuing a fresh descent, so the freed-root is never dereferenced. If
// a future API exposes the underlying *btree.Cursor or weakens the
// entry-method short-circuit, this SetRootID(freed) store on the
// DeleteKeyspace path becomes a use-after-free hazard and the caller
// must skip the SetRootID under ks.dead.
//
// Conservative-mark-all (mirrors the per-kind markCursorsStale): an
// extractor may emit entries for any subset of declared indexes per
// Put, and the cheapest correct policy is to stale every in-flight
// index cursor on any successful row mutation. Dead handles are skipped
// (their cursors have already been staled by markIndexHandleDead).
func (ks *keyspaceCore) markIndexHandlesStale() {
	for _, idx := range ks.openIndexHandles {
		if idx.pinned == nil || idx.dead {
			continue
		}
		newRoot := idx.pinned.root
		for _, c := range idx.openCursors {
			c.MarkStale()
			c.SetRootID(newRoot)
		}
	}
}

// markIndexHandleStaleByName invokes MarkStale on in-flight cursors for
// the named index only. Called by TxIndexes.Rebuild after
// syncRebuildToCachedPinned: only the rebuilt index's tree was replaced
// (FreeSubtree of the old root + new root published into pinned.root);
// other indexes' handles must NOT be invalidated. Refreshes the
// cursor's tracked rootID so a re-position descends from the new root.
func (ks *keyspaceCore) markIndexHandleStaleByName(name string) {
	for _, idx := range ks.openIndexHandles {
		if idx.pinned == nil || idx.dead {
			continue
		}
		if idx.pinned.decl.Name != name {
			continue
		}
		newRoot := idx.pinned.root
		for _, c := range idx.openCursors {
			c.MarkStale()
			c.SetRootID(newRoot)
		}
	}
}

// markIndexHandleDead marks every *Index handle for the named index as
// dead (Inv-IHS2): subsequent Lookup / LookupKeys / Range / Prefix /
// Get / Stats on the cached handle return ErrIndexNotFound. Also
// MarkStale's in-flight cursors so any iter mid-loop terminates
// (ErrCursorStale on Err()) instead of walking the FreeSubtree'd data
// tree pages.
//
// Called by TxIndexes.Drop after registryDelete + FreeSubtree succeed:
// the on-disk registry no longer references this index and the data
// tree pages have been returned to the loose-page pool, so any cached
// handle pointing at the stale pinnedIndex must reject further use.
func (ks *keyspaceCore) markIndexHandleDead(name string) {
	for _, idx := range ks.openIndexHandles {
		if idx.pinned == nil || idx.dead {
			continue
		}
		if idx.pinned.decl.Name != name {
			continue
		}
		idx.dead = true
		for _, c := range idx.openCursors {
			c.MarkStale()
		}
	}
}
