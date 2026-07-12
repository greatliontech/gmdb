package gmdb

import (
	"bytes"
	"errors"
	"fmt"
	"sync/atomic"
	"unique"

	"github.com/thegrumpylion/gmdb/internal/descriptor"
	"github.com/thegrumpylion/gmdb/internal/indexing"

	"github.com/thegrumpylion/gmdb/internal/btree"
)

// descAdapterValue implements descriptorOwner for code paths that
// work directly on a *descriptor.Keyspace without a *Keyspace /
// *SetKeyspace handle (RebuildIndex / DropIndex on a
// keyspace not currently cached in tx.openKeyspaces, per
// indexing.md §Recovery pattern after ErrIndexFingerprintMismatch
// where OpenKeyspace fails BEFORE caching). The dirty flag is
// observed by the caller to decide whether to push the mutated
// descriptor into tx.dirtyDescriptors at the end of the op.
type descAdapterValue struct {
	desc  descriptor.Keyspace
	dirty bool
}

func (a *descAdapterValue) descriptor() *descriptor.Keyspace { return &a.desc }
func (a *descAdapterValue) markDirty()                       { a.dirty = true }

// resolveKeyspaceForIndexOp loads the keyspace's descriptor for a
// RebuildIndex / DropIndex operation. Returns:
//   - owner: a descriptorOwner the caller passes to registry CRUD
//     helpers. For a currently-open Kind=0 keyspace, the cached
//     *Keyspace handle. For a currently-open Kind=1 SetKeyspace
//     the cached *SetKeyspace. Otherwise,
//     a descAdapterValue that the caller propagates to
//     tx.dirtyDescriptors at the end of the op.
//   - cachedKS / cachedSKS: non-nil when the keyspace is cached
//     (used by RebuildIndex's cached-path which also updates
//     ks.indexes[decl.Name]).
//   - desc: the descriptor (for Kind check + Root + Count).
//
// Errors:
//   - ErrNotFound if the keyspace does not exist on disk or in tx.
//   - ErrKeyspaceReserved if the resolved descriptor is Kind=2.
func (tx *Tx) resolveKeyspaceForIndexOp(name string) (owner descriptorOwner, cachedKS *Keyspace, cachedSKS *SetKeyspace, desc descriptor.Keyspace, err error) {
	if _, deleted := tx.pendingDeletes[name]; deleted {
		return nil, nil, nil, descriptor.Keyspace{}, ErrNotFound
	}
	handle := unique.Make(name)
	if ks, ok := tx.openKeyspaces[handle]; ok && !ks.dead {
		return ks, ks, nil, ks.desc, nil
	}
	if sks, ok := tx.openSetKeyspaces[handle]; ok && !sks.dead {
		return sks, nil, sks, sks.desc, nil
	}
	d, found, err := tx.lookupDescriptor(name)
	if err != nil {
		return nil, nil, nil, descriptor.Keyspace{}, err
	}
	if !found {
		return nil, nil, nil, descriptor.Keyspace{}, ErrNotFound
	}
	if d.Kind == descriptor.KindIndexInternal {
		return nil, nil, nil, descriptor.Keyspace{}, ErrKeyspaceReserved
	}
	// Not-cached path: build an adapter the caller propagates.
	// The Kind=1 gate is removed now that
	// RebuildIndex's row-walk is kind-aware.
	adapter := &descAdapterValue{desc: d}
	return adapter, nil, nil, d, nil
}

// propagateNotCachedDescChange writes the adapter's mutated
// descriptor back to tx.dirtyDescriptors when the adapter saw a
// mark-dirty call. No-op for the cached path (cachedKS/cachedSKS
// already carries the mutation in their .desc field). The staged
// entry joins the commit-flush reserve via recalcFlushReserve so the
// flush write is always affordable at Commit.
func (tx *Tx) propagateNotCachedDescChange(name string, owner descriptorOwner) error {
	a, ok := owner.(*descAdapterValue)
	if !ok || !a.dirty {
		return nil
	}
	if err := tx.ensureKeyspacePathLen(); err != nil {
		return err
	}
	if tx.dirtyDescriptors == nil {
		tx.dirtyDescriptors = make(map[string]descriptor.Keyspace)
	}
	tx.dirtyDescriptors[name] = a.desc
	tx.recalcFlushReserve()
	// Obligation-edge admission: unstage on rejection — the caller's
	// savepoint machinery then unwinds the operation itself.
	if err := tx.checkReserveAffordable(); err != nil {
		delete(tx.dirtyDescriptors, name)
		tx.recalcFlushReserve()
		return err
	}
	return nil
}

// liveIndexRoot resolves the index data-tree root to free for a
// registry-DDL operation: the cached handle's pinned root when one
// exists — live same-tx growth updates pinned.root in memory and
// syncs the registry only at flush, so the registry's Root lags it —
// otherwise the registry entry's Root (an uncached keyspace has no
// in-memory drift). Freeing the stale registry root orphans every
// page the live tree gained this tx (reproduced: create indexed
// keyspace, Put rows, Drop the index, Commit → BitmapLeak).
func liveIndexRoot(cachedKS *Keyspace, cachedSKS *SetKeyspace, indexName string, registryRoot uint64) uint64 {
	if cachedKS != nil {
		if p, ok := cachedKS.indexes[indexName]; ok {
			return p.root
		}
	}
	if cachedSKS != nil {
		if p, ok := cachedSKS.indexes[indexName]; ok {
			return p.root
		}
	}
	return registryRoot
}

// ownerFlushState snapshots the cached handle's flush state (state +
// reserveCharged) for the registry-DDL unwind; zero-values when the
// operation runs on the uncached adapter path.
func ownerFlushState(cachedKS *Keyspace, cachedSKS *SetKeyspace) (keyspaceState, bool) {
	switch {
	case cachedKS != nil:
		return cachedKS.state, cachedKS.reserveCharged
	case cachedSKS != nil:
		return cachedSKS.state, cachedSKS.reserveCharged
	}
	return keyspaceStateClean, false
}

// restoreOwnerFlushState reverts the cached handle's flush state to a
// snapshot taken before a registry DDL operation and re-prices the
// reserve. No-op on the adapter path.
func restoreOwnerFlushState(tx *Tx, cachedKS *Keyspace, cachedSKS *SetKeyspace, st keyspaceState, charged bool) {
	switch {
	case cachedKS != nil:
		cachedKS.state, cachedKS.reserveCharged = st, charged
	case cachedSKS != nil:
		cachedSKS.state, cachedSKS.reserveCharged = st, charged
	default:
		return
	}
	tx.recalcFlushReserve()
}

// remeasureRegistryDepth refreshes the cached registry path length on
// the cached handle (if any) after registry DDL restructured the
// sub-tree, then recomputes the commit-flush reserve. The uncached
// (adapter) path has no handle and its staged entry is charged for
// the descriptor write only — flushIndexRegistry runs only for open
// handles — so there is nothing to re-measure there.
func (tx *Tx) remeasureRegistryDepth(cachedKS *Keyspace, cachedSKS *SetKeyspace) error {
	switch {
	case cachedKS != nil:
		if err := tx.measureRegPathLen(&cachedKS.keyspaceCore); err != nil {
			return err
		}
	case cachedSKS != nil:
		if err := tx.measureRegPathLen(&cachedSKS.keyspaceCore); err != nil {
			return err
		}
	default:
		return nil
	}
	tx.recalcFlushReserve()
	// Obligation-edge admission: a deepened registry raises the
	// reserve; rejection fails the operation inside its savepoint
	// window (the cached regPathLen keeps the higher value — a safe
	// overcharge for the reverted tree).
	return tx.checkReserveAffordable()
}

// rebuildIndex implements TxIndexes.Rebuild: it drops and re-populates
// the named index using the supplied IndexDecl. The full caller-facing
// contract (error set, recovery semantics after
// ErrIndexFingerprintMismatch) lives on TxIndexes.Rebuild in
// index_admin.go.
func (tx *Tx) rebuildIndex(keyspace string, decl *IndexDecl) (retErr error) {
	if err := tx.requireOpen(true); err != nil {
		return err
	}
	if keyspace == "" {
		return ErrKeyEmpty
	}
	if decl == nil {
		return fmt.Errorf("gmdb: RebuildIndex: nil decl: %w", ErrInvalidOptions)
	}
	if decl.Name == "" {
		return ErrKeyEmpty
	}
	if decl.Kind != IndexKindComposite {
		return fmt.Errorf("gmdb: index %q kind %d: %w", decl.Name, decl.Kind, ErrIndexKindUnknown)
	}
	if decl.Extract == nil {
		return ErrIndexExtractorRequired
	}
	owner, cachedKS, cachedSKS, desc, err := tx.resolveKeyspaceForIndexOp(keyspace)
	if err != nil {
		return err
	}
	// Load the existing registry entry (must exist).
	existing, err := tx.registryGet(owner, decl.Name)
	if err != nil {
		return err
	}
	// Stored-entry kind gate (indexing.md §Rebuild / §Removing an
	// Index): rebuilding a foreign-kind index with a composite decl
	// would silently convert it — the very outcome the open path's
	// gate ordering exists to prevent, via the documented recovery
	// verb itself.
	if existing.Kind != indexing.KindComposite {
		return fmt.Errorf("gmdb: stored index %q kind %d: %w", decl.Name, existing.Kind, ErrIndexKindUnknown)
	}

	// Atomicity wrap (transactions.md §Write-helper error contract):
	// the rebuild allocates pages for the new index data tree
	// (processPair's btree.Put loop), advances the registry to point
	// at the new root (registryPut), then frees the old data tree
	// (FreeSubtree). A mid-build btree.Put failure leaves the partial
	// new tree allocated; a FreeSubtree failure after the publish-
	// then-retire registryPut leaves the partially-freed OLD tree
	// allocated and unreferenced — either shape orphans pages on
	// Tx.Commit (the rest-of-tx-continues path). Bracket the body in
	// a nested savepoint so any error after this point reverts every
	// page allocation/free AND restores the descriptor's pre-call
	// IndexRegistryRoot (the savepoint owns pager state, not caller-
	// descriptor fields). Nested kind (not shallow) is correct here:
	// RebuildIndex is one-shot per call, not per-row, so suspending
	// loose-page reuse for the duration is not a cost concern.
	ownerDesc := owner.descriptor()
	prevRegRoot := ownerDesc.IndexRegistryRoot
	prevState, prevCharged := ownerFlushState(cachedKS, cachedSKS)
	sp := tx.pgr.BeginSavepoint()
	completed := false
	defer func() {
		// completed distinguishes the success path from BOTH failure
		// modes: retErr != nil (ordinary error) and a PANIC unwinding
		// through this frame (retErr stays nil then — branching on it
		// alone released the savepoint and permanently leaked the
		// partial new-tree allocations when a recovering caller
		// committed after an extractor panic).
		if retErr != nil || !completed {
			tx.pgr.RestoreSavepoint(sp)
			ownerDesc.IndexRegistryRoot = prevRegRoot
			// Revert the flush-state flip registryPut's markDirty
			// made on the cached handle: the savepoint restored the
			// pages, so the obligation it priced must not persist —
			// in particular when this very error IS the obligation
			// rejection (remeasureRegistryDepth's affordability
			// check).
			restoreOwnerFlushState(tx, cachedKS, cachedSKS, prevState, prevCharged)
			return
		}
		tx.pgr.ReleaseSavepoint(sp)
	}()

	// Open a row-cursor on the parent keyspace.
	cfg := tx.pgr.Config()
	if desc.Root == 0 {
		// Empty parent → rebuilt index is empty. Publish-then-retire
		// (registry-first ordering): write the registry entry first, then free
		// the old data tree.
		newEntry := &indexing.RegistryEntry{
			SchemaHash:  schemaHash(decl),
			Unique:      decl.Unique,
			Kind:        decl.Kind,
			Root:        0,
			Count:       0,
			UserVersion: decl.Version,
			// A rebuild replaces the index wholesale from the
			// supplied decl: the OLD entry's per-kind payload does
			// not carry over — a non-composite kind's rebuild
			// produces its own fresh payload.
		}
		for _, c := range decl.Columns {
			newEntry.Columns = append(newEntry.Columns, c.Name)
		}
		for _, c := range decl.Covering {
			newEntry.Covering = append(newEntry.Covering, c.Name)
		}
		if err := tx.registryPut(owner, decl.Name, newEntry); err != nil {
			return err
		}
		if oldRoot := liveIndexRoot(cachedKS, cachedSKS, decl.Name, existing.Root); oldRoot != 0 {
			if _, err := btree.FreeSubtree(btreeWriter{tx.pgr}, cfg, oldRoot); err != nil {
				return fmt.Errorf("RebuildIndex %q.%q: free old subtree: %w", keyspace, decl.Name, mapBtreeErr(err))
			}
		}
		if err := tx.propagateNotCachedDescChange(keyspace, owner); err != nil {
			return err
		}
		if err := tx.remeasureRegistryDepth(cachedKS, cachedSKS); err != nil {
			return err
		}
		tx.syncRebuildToCachedPinned(cachedKS, cachedSKS, decl, 0, 0)
		// Inv-IHS1: any in-flight *IndexHandle iter on this name walks the
		// just-FreeSubtree'd old root. MarkStale every such cursor
		// (by-name — other declared indexes were not touched) and
		// refresh the cursor's tracked rootID to the new (0, empty)
		// root so a re-position yields nothing rather than reading
		// freed pages.
		if cachedKS != nil {
			cachedKS.markIndexHandleStaleByName(decl.Name)
		}
		if cachedSKS != nil {
			cachedSKS.markIndexHandleStaleByName(decl.Name)
		}
		completed = true // empty-parent SUCCESS: the defer must Release, not Restore
		return nil
	}

	mergeThreshold := tx.db.opts.MergeThreshold
	// Rebuild into a fresh tree (root=0; first btree.Put will
	// allocate).
	newRoot := uint64(0)
	newCount := uint64(0)
	hasCovering := len(decl.Covering) > 0
	isSetKeyspace := desc.Kind == descriptor.KindSetKeyspace

	// processPair builds and writes index entries for one extractor
	// input. For Keyspace: k1=rowKey, k2=rowValue. For SetKeyspace: k1=setKey, k2=setValue — per-(setKey, setValue)
	// extractor invocation per indexing.md §Indexes on SetKeyspaces.
	processPair := func(k1, k2 []byte) error {
		entries := decl.Extract(k1, k2)
		if len(entries) == 0 {
			return nil
		}
		seen := make(map[string]IndexEntry, len(entries))
		for _, e := range entries {
			var ik string
			if isSetKeyspace {
				ik = string(indexing.EncodeSetEntryKey(e.Cols, k1, k2, decl.Unique))
			} else {
				ik = string(indexing.EntryKey(e, k1, decl.Unique))
			}
			if _, dup := seen[ik]; dup {
				if decl.Unique {
					return fmt.Errorf("%w: index %q during rebuild: candidate-set duplicate for row %x",
						ErrIndexUniqueViolation, decl.Name, k1)
				}
				// LAST-wins overwrite — the same set semantic as the
				// live path's extractEntriesAsKeySet, so a rebuilt
				// index is byte-identical to a live-maintained one
				// (first-wins here diverged the covering payload and
				// produced FingerprintDrift false positives).
			}
			seen[ik] = e
		}
		for ik, entry := range seen {
			ikBytes := []byte(ik)
			if decl.Unique && newRoot != 0 {
				_, found, err := btree.Get(tx.pgr, cfg, newRoot, ikBytes)
				if err != nil {
					return mapBtreeErr(err)
				}
				if found {
					return fmt.Errorf("%w: index %q during rebuild: duplicate key (new extractor produces collision across rows)",
						ErrIndexUniqueViolation, decl.Name)
				}
			}
			var pkForValue []byte
			if isSetKeyspace {
				pkForValue = indexing.EncodeSetCompoundPK(k1, k2)
			} else {
				pkForValue = k1
			}
			val := indexing.EntryValue(entry, pkForValue, decl.Unique, hasCovering)
			updated, err := btree.Put(btreeWriter{tx.pgr}, cfg, newRoot, ikBytes, val)
			if err != nil {
				return fmt.Errorf("RebuildIndex %q.%q: btree.Put: %w", keyspace, decl.Name, mapBtreeErr(err))
			}
			newRoot = updated
			newCount++
		}
		return nil
	}

	if isSetKeyspace {
		// Use the cached *SetKeyspace if present; otherwise construct
		// a transient handle for the SetCursor walk (read-only). The
		// transient handle is NOT registered in tx.openSetKeyspaces
		// so the spec's recovery-after-fingerprint-mismatch pattern
		// (RebuildIndex on a not-cached keyspace) works unchanged.
		// readOnly=true on the transient since we only read.
		sks := cachedSKS
		if sks == nil {
			sks = &SetKeyspace{keyspaceCore: keyspaceCore{tx: tx, name: unique.Make(keyspace), desc: desc, readOnly: true}}
		}
		// newInternalSetCursor bypasses openSetCursors registration
		// so repeated RebuildIndex calls don't leak entries into the
		// per-tx slice.
		sc := newInternalSetCursor(sks)
		for sk, sv := sc.First(); sk != nil; sk, sv = sc.Next() {
			skCopy := bytes.Clone(sk)
			svCopy := bytes.Clone(sv)
			if err := processPair(skCopy, svCopy); err != nil {
				return err
			}
		}
		if err := sc.Err(); err != nil {
			return fmt.Errorf("RebuildIndex %q.%q: set cursor: %w", keyspace, decl.Name, mapBtreeErr(err))
		}
	} else {
		rowCursor := btree.NewCursor(btreeWriter{tx.pgr}, cfg, desc.Root, mergeThreshold)
		for rowKey, rowValue := rowCursor.First(); rowKey != nil; rowKey, rowValue = rowCursor.Next() {
			kCopy := bytes.Clone(rowKey)
			vCopy := bytes.Clone(rowValue)
			if err := processPair(kCopy, vCopy); err != nil {
				return err
			}
		}
		if err := rowCursor.Err(); err != nil {
			return fmt.Errorf("RebuildIndex %q.%q: row cursor: %w", keyspace, decl.Name, mapBtreeErr(err))
		}
	}

	// Publish-then-retire ordering:
	// registryPut the new entry FIRST so a registryPut failure
	// leaves the old data tree intact. Only after the registry
	// points at the new root do we FreeSubtree the old root —
	// a FreeSubtree failure after that point leaks the old tree
	// (recoverable via Rollback) but cannot leave the registry
	// pointing at freed pages.
	newEntry := &indexing.RegistryEntry{
		SchemaHash:  schemaHash(decl),
		Unique:      decl.Unique,
		Kind:        decl.Kind,
		Root:        newRoot,
		Count:       newCount,
		UserVersion: decl.Version,
	}
	for _, c := range decl.Columns {
		newEntry.Columns = append(newEntry.Columns, c.Name)
	}
	for _, c := range decl.Covering {
		newEntry.Covering = append(newEntry.Covering, c.Name)
	}
	if err := tx.registryPut(owner, decl.Name, newEntry); err != nil {
		return err
	}
	// Test-only failure-injection seam: simulates a FreeSubtree
	// failure after the publish-then-retire registryPut succeeded.
	// Exercises the savepoint-backed restore that must revert both
	// pager state and the descriptor's IndexRegistryRoot.
	if hook := rebuildIndexFailHookForTest.Load(); hook != nil {
		if err := (*hook)(); err != nil {
			return err
		}
	}
	// Free the OLD index data tree only after the registry has
	// been atomically advanced. Freed at its LIVE root (see
	// liveIndexRoot) — the registry's Root lags same-tx growth.
	if oldRoot := liveIndexRoot(cachedKS, cachedSKS, decl.Name, existing.Root); oldRoot != 0 {
		if _, err := btree.FreeSubtree(btreeWriter{tx.pgr}, cfg, oldRoot); err != nil {
			return fmt.Errorf("RebuildIndex %q.%q: free old subtree: %w", keyspace, decl.Name, mapBtreeErr(err))
		}
	}
	if err := tx.propagateNotCachedDescChange(keyspace, owner); err != nil {
		return err
	}
	if err := tx.remeasureRegistryDepth(cachedKS, cachedSKS); err != nil {
		return err
	}
	tx.syncRebuildToCachedPinned(cachedKS, cachedSKS, decl, newRoot, newCount)
	// Inv-IHS1: see the empty-parent branch above. The pinned root
	// was just swapped to newRoot and the OLD tree FreeSubtree'd —
	// any in-flight *IndexHandle iter cursor on this name is now walking
	// freed pages. MarkStale by-name (only this index, not siblings)
	// and refresh the cursor's rootID to newRoot.
	if cachedKS != nil {
		cachedKS.markIndexHandleStaleByName(decl.Name)
	}
	if cachedSKS != nil {
		cachedSKS.markIndexHandleStaleByName(decl.Name)
	}
	completed = true
	return nil
}

// syncRebuildToCachedPinned updates ks.indexes / sks.indexes (when
// the keyspace was already cached at RebuildIndex entry) with the
// new decl + root + count + schemaHash. No-op on the not-cached
// path. Required so a subsequent Lookup / Put within the same tx
// observes the rebuilt index.
func (tx *Tx) syncRebuildToCachedPinned(cachedKS *Keyspace, cachedSKS *SetKeyspace, decl *IndexDecl, newRoot, newCount uint64) {
	if cachedKS == nil && cachedSKS == nil {
		return
	}
	var pinnedMap map[string]*pinnedIndex
	if cachedKS != nil {
		pinnedMap = cachedKS.indexes
	} else {
		pinnedMap = cachedSKS.indexes
	}
	if pinnedMap == nil {
		// The cached keyspace had no indexes in its pinned set
		// (e.g. opened with no IndexDecls, then RebuildIndex
		// supplied one). The rebuild updated the on-disk registry
		// via registryPut, but the cached handle isn't aware. The
		// caller's subsequent re-OpenKeyspace with the rebuilt
		// IndexDecl will fail with ErrKeyspaceAlreadyOpen (cache
		// hit + index-set mismatch). That's expected per the
		// recovery loop pattern — the caller exits the loop after
		// successful rebuild.
		return
	}
	p, ok := pinnedMap[decl.Name]
	if !ok {
		// The cached pinnedIndex doesn't track decl.Name. The
		// caller's prior OpenKeyspace must not have declared this
		// index (otherwise it would be in pinnedMap). The on-disk
		// registry is now in a state inconsistent with the cached
		// handle's pinned set — subsequent ops via the cached
		// handle don't know about the rebuilt index, which is
		// acceptable (the caller will re-open).
		return
	}
	p.root = newRoot
	p.count = newCount
	p.schemaHash = schemaHash(decl)
	p.decl = decl
	// The rebuild wrote a fresh, payload-less entry; the pinned
	// copy must match or Commit's flushIndexRegistry would
	// resurrect the OLD kind's payload over it (a non-composite
	// kind's rebuild installs its own payload here).
	p.kindPayload = nil
}

// retireIndexRegistry implements steps 2+3 of the three-subtree
// retirement in Keyspace.DeleteKeyspace (api-surface.md §Keyspace
// API DeleteKeyspace). For each index entry
// in the registry sub-tree:
//
//  1. Decode the entry to read its Root (the index data tree).
//  2. FreeSubtree the index data tree (when Root != 0; an
//     empty index's data tree was never allocated).
//  3. After all per-index data trees are retired, FreeSubtree
//     the registry sub-tree itself.
//
// On a mid-walk failure: in-flight FreeSubtree calls have returned
// pages to the loose pool, but the descriptor itself is still
// reachable from the caller's tx state (DeleteKeyspace's removal
// of the descriptor from the keyspace B+tree happens at the
// flushKeyspaces Step 1, which runs at Commit). Tx
// Rollback restores via AbortTx; Commit-after-error leaks (same
// rest-of-tx-continues contract).
func (tx *Tx) retireIndexRegistry(keyspaceName string, registryRoot uint64) error {
	cfg := tx.pgr.Config()
	mergeThreshold := tx.db.opts.MergeThreshold
	// Walk the registry sub-tree with a cursor; collect each
	// entry's Root for FreeSubtree.
	cur := btree.NewCursor(btreeWriter{tx.pgr}, cfg, registryRoot, mergeThreshold)
	i := 0
	for k, v := cur.First(); k != nil; k, v = cur.Next() {
		// Copy v because the next cursor op may invalidate.
		valCopy := make([]byte, len(v))
		copy(valCopy, v)
		entry, err := indexing.DecodeRegistryEntry(valCopy)
		if err != nil {
			return fmt.Errorf("%w: DeleteKeyspace %q: registry entry %q decode: %w",
				ErrCorrupted, keyspaceName, string(k), err)
		}
		if entry.Root != 0 {
			if _, err := btree.FreeSubtree(btreeWriter{tx.pgr}, cfg, entry.Root); err != nil {
				return fmt.Errorf("DeleteKeyspace %q: free index %q data subtree: %w",
					keyspaceName, string(k), mapBtreeErr(err))
			}
		}
		// Test-only failure-injection seam: simulates a partial-walk
		// failure (e.g. decode of a later entry, or a FreeSubtree
		// error). Exercises the savepoint-backed restore in
		// Keyspace.DeleteKeyspace that covers both this walk and the
		// preceding step-1 data-subtree FreeSubtree.
		if hook := retireIndexRegistryFailHookForTest.Load(); hook != nil {
			if err := (*hook)(i); err != nil {
				return err
			}
		}
		i++
	}
	if err := cur.Err(); err != nil {
		return fmt.Errorf("DeleteKeyspace %q: registry walk: %w", keyspaceName, mapBtreeErr(err))
	}
	// Step 3: free the registry sub-tree itself.
	if _, err := btree.FreeSubtree(btreeWriter{tx.pgr}, cfg, registryRoot); err != nil {
		return fmt.Errorf("DeleteKeyspace %q: free registry subtree: %w", keyspaceName, mapBtreeErr(err))
	}
	return nil
}

// dropIndex implements TxIndexes.Drop: it removes the named index,
// retiring the index's Kind=2 data sub-tree pages (via
// btree.FreeSubtree) and the registry entry. If the dropped index was
// the keyspace's last, registryDelete's btree.Delete returns root=0,
// structurally satisfying the indexing.md entailed invariant on
// empty-registry canonical-at-zero. The full caller-facing contract
// lives on TxIndexes.Drop in index_admin.go.
func (tx *Tx) dropIndex(keyspace, indexName string) (retErr error) {
	if err := tx.requireOpen(true); err != nil {
		return err
	}
	if keyspace == "" || indexName == "" {
		return ErrKeyEmpty
	}
	owner, cachedKS, cachedSKS, _, err := tx.resolveKeyspaceForIndexOp(keyspace)
	if err != nil {
		return err
	}
	// Load the registry entry to capture its Root (the index data
	// tree to retire).
	existing, err := tx.registryGet(owner, indexName)
	if err != nil {
		return err
	}
	// Stored-entry kind gate: a foreign kind's payload may reference
	// state beyond Root that a composite FreeSubtree cannot see —
	// this engine version must not retire what it cannot interpret
	// (indexing.md §Removing an Index).
	if existing.Kind != indexing.KindComposite {
		return fmt.Errorf("gmdb: stored index %q kind %d: %w", indexName, existing.Kind, ErrIndexKindUnknown)
	}
	cfg := tx.pgr.Config()

	// Atomicity wrap (transactions.md §Write-helper error contract):
	// the drop advances the registry off the entry (registryDelete,
	// which CoWs the registry tree) and then frees the OLD data tree
	// (FreeSubtree). A FreeSubtree mid-walk failure after the publish-
	// then-retire registryDelete orphans the partially-freed data
	// tree (the registry no longer references it) on Tx.Commit (the
	// rest-of-tx-continues path). Bracket the body in a nested
	// savepoint so any error after this point reverts the page
	// allocations/frees AND restores the descriptor's pre-call
	// IndexRegistryRoot.
	ownerDesc := owner.descriptor()
	prevRegRoot := ownerDesc.IndexRegistryRoot
	prevState, prevCharged := ownerFlushState(cachedKS, cachedSKS)
	sp := tx.pgr.BeginSavepoint()
	defer func() {
		if retErr != nil {
			tx.pgr.RestoreSavepoint(sp)
			ownerDesc.IndexRegistryRoot = prevRegRoot
			// See rebuildIndex's twin defer: revert the markDirty
			// flip so a rejected obligation does not persist.
			restoreOwnerFlushState(tx, cachedKS, cachedSKS, prevState, prevCharged)
			return
		}
		tx.pgr.ReleaseSavepoint(sp)
	}()

	// Publish-then-retire ordering:
	// remove the registry entry FIRST so any failure leaves the
	// data tree intact (and recoverable). Only after the registry
	// is updated do we FreeSubtree the data tree pages.
	if err := tx.registryDelete(owner, indexName); err != nil {
		if errors.Is(err, ErrIndexNotFound) {
			return err
		}
		return fmt.Errorf("DropIndex %q.%q: registry delete: %w", keyspace, indexName, err)
	}
	// Test-only failure-injection seam: simulates a FreeSubtree
	// failure after the publish-then-retire registryDelete advanced
	// the registry. Exercises the savepoint-backed restore.
	if hook := dropIndexFailHookForTest.Load(); hook != nil {
		if err := (*hook)(); err != nil {
			return err
		}
	}
	// Freed at the LIVE root (see liveIndexRoot) — the registry's
	// Root lags same-tx growth.
	if oldRoot := liveIndexRoot(cachedKS, cachedSKS, indexName, existing.Root); oldRoot != 0 {
		if _, err := btree.FreeSubtree(btreeWriter{tx.pgr}, cfg, oldRoot); err != nil {
			return fmt.Errorf("DropIndex %q.%q: free data subtree: %w", keyspace, indexName, mapBtreeErr(err))
		}
	}
	if err := tx.propagateNotCachedDescChange(keyspace, owner); err != nil {
		return err
	}
	if err := tx.remeasureRegistryDepth(cachedKS, cachedSKS); err != nil {
		return err
	}
	// Drop the pinned entry from the cached keyspace, if any.
	if cachedKS != nil {
		delete(cachedKS.indexes, indexName)
	}
	if cachedSKS != nil {
		delete(cachedSKS.indexes, indexName)
	}
	// Inv-IHS2 + Inv-IHS1: any cached *IndexHandle whose pinned
	// matches this (ks, name) pair must now reject further use —
	// the on-disk registry entry is gone and the data tree pages
	// have been FreeSubtree'd. markIndexHandleDead poisons the
	// handle (dead=true → ErrIndexNotFound on subsequent
	// Lookup / LookupKeys / Range / Prefix / Get / Stats) AND
	// MarkStales every in-flight cursor so any iter mid-loop
	// terminates with ErrCursorStale instead of walking freed
	// leaves.
	if cachedKS != nil {
		cachedKS.markIndexHandleDead(indexName)
	}
	if cachedSKS != nil {
		cachedSKS.markIndexHandleDead(indexName)
	}
	return nil
}

// rebuildIndexFailHookForTest, when set, is invoked once inside
// rebuildIndex after the publish-then-retire registryPut succeeded
// but before FreeSubtree of the OLD index data tree runs. A non-nil
// return injects a failure that exercises the savepoint-backed restore
// (revert pager state + restore IndexRegistryRoot). Test-only;
// installed via setRebuildIndexFailHookForTest and cleared via
// t.Cleanup. The hook is global state — tests that set it must NOT
// call t.Parallel(). Same caveat for dropIndexFailHookForTest and
// retireIndexRegistryFailHookForTest below.
var rebuildIndexFailHookForTest atomic.Pointer[func() error]

func setRebuildIndexFailHookForTest(hook func() error) {
	if hook == nil {
		rebuildIndexFailHookForTest.Store(nil)
		return
	}
	rebuildIndexFailHookForTest.Store(&hook)
}

// dropIndexFailHookForTest, when set, is invoked once inside
// dropIndex after the publish-then-retire registryDelete succeeded
// but before FreeSubtree of the OLD data tree runs. Test-only.
var dropIndexFailHookForTest atomic.Pointer[func() error]

func setDropIndexFailHookForTest(hook func() error) {
	if hook == nil {
		dropIndexFailHookForTest.Store(nil)
		return
	}
	dropIndexFailHookForTest.Store(&hook)
}

// retireIndexRegistryFailHookForTest, when set, is invoked after each
// registry entry's processing inside retireIndexRegistry's walk (the
// per-entry FreeSubtree runs only when entry.Root != 0; the hook
// still fires for an empty-Root entry). A non-nil return injects a
// partial-walk failure that exercises the savepoint-backed restore in
// Keyspace.DeleteKeyspace (which covers both step-1 data-subtree
// FreeSubtree and the registry retirement). Test-only.
var retireIndexRegistryFailHookForTest atomic.Pointer[func(i int) error]

func setRetireIndexRegistryFailHookForTest(hook func(i int) error) {
	if hook == nil {
		retireIndexRegistryFailHookForTest.Store(nil)
		return
	}
	retireIndexRegistryFailHookForTest.Store(&hook)
}
