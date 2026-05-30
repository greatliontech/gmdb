package gmdb

import "github.com/thegrumpylion/gmdb/internal/page"

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
	// descriptor.Count); the chunk-5.6 deferred-flush refactor promotes
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
	// DeleteKeyspace (chunk-5.6 Inv-D).
	dead bool

	// openIndexHandles tracks every *Index returned by Index(name) in
	// this tx. Atomic Put / Delete / Cursor.Delete mutate index trees
	// (applyIndexMaintenanceOn*); Tx.RebuildIndex / Tx.DropIndex replace
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
	// method. Used by the chunk-7.5 same-tx re-open idempotence check to
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

// descriptor returns the in-tx descriptor pointer. Used by the
// chunk-7.3 registry-CRUD helpers (index_codec.go) to satisfy the
// descriptorOwner interface — registryPut / registryDelete mutate the
// descriptor in place AND call markDirty() so the chunk-5.6
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
// the named index only. Called by Tx.RebuildIndex after
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
// Called by Tx.DropIndex after registryDelete + FreeSubtree succeed:
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
