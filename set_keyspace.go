package gmdb

import (
	"bytes"
	"errors"
	"fmt"
	"unique"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// SetKeyspaceOptions configures a SetKeyspace at creation time. Per
// api-surface.md §SetKeyspace API + keyspaces.md §Keyspace Descriptor.
type SetKeyspaceOptions struct {
	// FixedValueSize, when non-zero, pins every value in the
	// keyspace to exactly this many bytes. Enables the no-per-value-
	// length-prefix encoding in subpages (flat-array binary search)
	// and the compact-nested-leaf optimisation per set-keyspace.md
	// §Fixed-Size Value Sets.
	//
	// 0 = variable-size values (the default). Immutable after
	// creation (keyspaces.md invariant #5). Must be ≤ 65535 (uint16
	// on disk).
	FixedValueSize int
}

// SetKeyspace is a handle to a named Kind=1 keyspace within a write
// transaction. Returned by Tx.OpenSetKeyspace / Tx.CreateSetKeyspace /
// Tx.CreateSetKeyspaceIfNotExists. Mirrors *Keyspace's lifecycle:
//
//   - A handle is valid for the lifetime of the owning transaction.
//   - DeleteKeyspace invalidates the handle (and every SetCursor
//     opened against it); subsequent operations return
//     ErrKeyspaceClosed.
//   - Re-creating the same name via CreateSetKeyspace in the same tx
//     does NOT reactivate the old handle (chunk-5.6 Inv-D).
//
// All values for a key are stored as either:
//   - An inline subpage cell (CellFlagMultiValue, NestedTree clear)
//     when the value-set fits below SubpagePromotionThreshold.
//   - A nested-B+tree reference cell (MultiValue|NestedTree) when
//     larger, with desc-cell-recorded `NestedCount` for O(1)
//     CountValues.
//
// Per set-keyspace.md §Storage Strategy. The promotion / demotion is
// engine-managed (chunk 6.4 / 6.5); SetKeyspace.Put / DeleteValue
// transparently dispatch between subpage in-place edits and nested-
// tree mutations.
type SetKeyspace struct {
	keyspaceCore

	// openSetCursors tracks every *SetCursor returned by
	// SetKeyspace.Cursor() in this tx so Put / Delete / DeleteValue
	// can MarkStale them. Sibling mutations to the keyspace's
	// B+tree (parent OR a cell's nested tree) invalidate cursor
	// state because the cursor's outer btree.Cursor and inner
	// materialized values may both be stale post-mutation. Same
	// pattern as Keyspace.openCursors (chunk-5 markCursorsStale
	// + SetRootID).
	openSetCursors []*SetCursor
}

// FixedValueSize returns the keyspace's declared fixed-value stride
// (0 = variable-size values). Immutable after creation per
// keyspaces.md invariant #5.
func (ks *SetKeyspace) FixedValueSize() int { return int(ks.desc.FixedValueSize) }

// markSetCursorsStale invokes MarkStale on every SetCursor
// registered on this keyspace AND refreshes their outer-cursor's
// tracked rootID to the keyspace's current desc.Root. Also sets
// each SetCursor's own `stale` flag so the value-bounded ops
// (NextValue / PrevValue / Current) surface stale even though
// they short-circuit on the materialized values slice. Called by
// Put / Delete / DeleteValue after a successful mutation. Stale
// cursors are not unregistered — the caller may re-position via
// First/Last/Seek/SeekGE (which clears the stale flag).
//
// Also delegates to markIndexHandlesStale (Inv-IHS1): every existing
// caller (Put / Delete / DeleteValue / SetCursor.Delete / BulkLoad
// success paths) post-dates the mutation and, on the indexed path,
// has just CoW'd index trees via applyIndexMaintenanceOn{AddValue,
// RemoveValue} or finalizeIndexBuild. The in-flight *Index iter
// cursors must MarkStale or read CoW'd/freed leaves. Centralized
// here so every existing call site gets index-handle invalidation
// for free; no-op when openIndexHandles is empty (no
// SetKeyspace.Index(name) call has been made — or the keyspace has
// no declared indexes).
func (ks *SetKeyspace) markSetCursorsStale() {
	for _, c := range ks.openSetCursors {
		c.outerCursor.MarkStale()
		c.outerCursor.SetRootID(ks.desc.Root)
		c.stale = true
	}
	ks.markIndexHandlesStale()
}

// OpenSetKeyspace opens an existing Kind=1 keyspace (SetKeyspace) for
// read+write with the supplied IndexDecls (chunk-7.5 indexing wiring).
// Same shape as OpenKeyspace: validation against the stored registry
// returns ErrIndexExtractorRequired / ErrIndexUnknown /
// ErrIndexFingerprintMismatch as appropriate; same-tx re-open returns
// the cached handle iff hashable inputs match (otherwise
// ErrKeyspaceAlreadyOpen).
func (tx *Tx) OpenSetKeyspace(name string, indexes ...*IndexDecl) (*SetKeyspace, error) {
	if err := tx.requireOpen(true); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, ErrKeyEmpty
	}
	pinned, err := buildPinnedIndexMap(indexes)
	if err != nil {
		return nil, err
	}
	handle := unique.Make(name)
	if sks, ok := tx.openSetKeyspaces[handle]; ok {
		if sks.readOnly {
			return nil, ErrKeyspaceAlreadyOpen
		}
		if !indexesEqualByHashableInputs(sks.indexes, pinned) {
			return nil, ErrKeyspaceAlreadyOpen
		}
		return sks, nil
	}
	if _, ok := tx.openKeyspaces[handle]; ok {
		return nil, ErrKeyspaceKindMismatch
	}
	if _, deleted := tx.pendingDeletes[name]; deleted {
		return nil, ErrNotFound
	}
	desc, found, err := tx.lookupDescriptor(name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	if err := checkKeyspaceKind(desc.Kind, page.KeyspaceKindSetKeyspace); err != nil {
		return nil, err
	}
	// Defer dirtyDescriptors removal until validation succeeds —
	// chunk-7.5 Round-1 M-2 fix.
	sks := tx.cacheOpenSetKeyspace(handle, desc, keyspaceStateClean)
	if err := tx.validatePinnedAgainstRegistry(sks, name, pinned); err != nil {
		delete(tx.openSetKeyspaces, handle)
		return nil, err
	}
	delete(tx.dirtyDescriptors, name)
	sks.indexes = pinned
	return sks, nil
}

// OpenSetKeyspaceReadOnly opens an existing Kind=1 keyspace for
// reads only. No IndexDecls accepted/required (index lookups work
// from stored entries directly per indexing.md §Open Semantics).
// Same-tx-mixed Open* / OpenReadOnly on a single name returns
// ErrKeyspaceAlreadyOpen.
func (tx *Tx) OpenSetKeyspaceReadOnly(name string) (*SetKeyspace, error) {
	if err := tx.requireOpen(false); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, ErrKeyEmpty
	}
	handle := unique.Make(name)
	if sks, ok := tx.openSetKeyspaces[handle]; ok {
		if !sks.readOnly {
			return nil, ErrKeyspaceAlreadyOpen
		}
		return sks, nil
	}
	if _, ok := tx.openKeyspaces[handle]; ok {
		return nil, ErrKeyspaceKindMismatch
	}
	if _, deleted := tx.pendingDeletes[name]; deleted {
		return nil, ErrNotFound
	}
	desc, found, err := tx.lookupDescriptor(name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	if err := checkKeyspaceKind(desc.Kind, page.KeyspaceKindSetKeyspace); err != nil {
		return nil, err
	}
	sks := tx.cacheOpenSetKeyspace(handle, desc, keyspaceStateClean)
	sks.readOnly = true
	delete(tx.dirtyDescriptors, name)
	return sks, nil
}

// CreateSetKeyspace creates a new Kind=1 keyspace with the supplied
// options. Returns ErrKeyExists if a keyspace with the supplied name
// already exists. Returns ErrInvalidOptions when
// opts.FixedValueSize is out of range (< 0 or > 65535).
//
// The descriptor is created in memory with state=Created and
// persisted to the keyspace B+tree at Tx.Commit's flushKeyspaces
// walk. numKeyspaces is incremented eagerly so same-tx
// ListKeyspaces / NumKeyspaces reflect the new entry immediately.
//
// Delete-then-Create in the same tx (where the name was previously
// in pendingDeletes) is permitted; the pendingDeletes entry is
// cleared and a fresh *SetKeyspace is returned. Any previously-
// opened handle for the same name stays dead.
//
// Chunk-6.6 caveat: opts == nil is treated as
// SetKeyspaceOptions{FixedValueSize: 0} (variable-size values).
func (tx *Tx) CreateSetKeyspace(name string, opts *SetKeyspaceOptions, indexes ...*IndexDecl) (*SetKeyspace, error) {
	if err := tx.requireOpen(true); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, ErrKeyEmpty
	}
	fvs, err := validateSetOpts(opts)
	if err != nil {
		return nil, err
	}
	pinned, err := buildPinnedIndexMap(indexes)
	if err != nil {
		return nil, err
	}
	handle := unique.Make(name)
	if _, ok := tx.openKeyspaces[handle]; ok {
		return nil, ErrKeyExists
	}
	if _, ok := tx.openSetKeyspaces[handle]; ok {
		return nil, ErrKeyExists
	}
	_, pendingDelete := tx.pendingDeletes[name]
	if !pendingDelete {
		if _, found, err := tx.lookupDescriptor(name); err != nil {
			return nil, err
		} else if found {
			return nil, ErrKeyExists
		}
	} else {
		delete(tx.pendingDeletes, name)
	}
	desc := page.KeyspaceDescriptor{
		Kind:           page.KeyspaceKindSetKeyspace,
		FixedValueSize: fvs,
	}
	tx.numKeyspaces++
	sks := tx.cacheOpenSetKeyspace(handle, desc, keyspaceStateCreated)
	if len(pinned) > 0 {
		if err := tx.writeNewIndexRegistry(sks, pinned); err != nil {
			// chunk-7.5 Round-1 M-1 fix: restore pendingDeletes
			// state if we cleared it above.
			delete(tx.openSetKeyspaces, handle)
			tx.numKeyspaces--
			if pendingDelete {
				tx.pendingDeletes[name] = struct{}{}
			}
			return nil, err
		}
	}
	sks.indexes = pinned
	return sks, nil
}

// CreateSetKeyspaceIfNotExists opens the keyspace if it exists OR
// creates it. If the keyspace exists with a different FixedValueSize
// than opts, returns ErrFixedValueSizeMismatch — FixedValueSize is
// immutable after creation (keyspaces.md invariant #5), so the API
// cannot silently coerce the caller's opts to the existing value.
// Returns ErrKeyspaceKindMismatch if the existing keyspace is Kind=0.
//
// opts == nil is treated as FixedValueSize=0 — the same equality
// check applies.
func (tx *Tx) CreateSetKeyspaceIfNotExists(name string, opts *SetKeyspaceOptions, indexes ...*IndexDecl) (*SetKeyspace, error) {
	if err := tx.requireOpen(true); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, ErrKeyEmpty
	}
	fvs, err := validateSetOpts(opts)
	if err != nil {
		return nil, err
	}
	pinned, err := buildPinnedIndexMap(indexes)
	if err != nil {
		return nil, err
	}
	handle := unique.Make(name)
	if sks, ok := tx.openSetKeyspaces[handle]; ok {
		if sks.readOnly {
			return nil, ErrKeyspaceAlreadyOpen
		}
		if sks.desc.FixedValueSize != fvs {
			return nil, fmt.Errorf("%w: existing FixedValueSize=%d, opts.FixedValueSize=%d",
				ErrFixedValueSizeMismatch, sks.desc.FixedValueSize, fvs)
		}
		if !indexesEqualByHashableInputs(sks.indexes, pinned) {
			return nil, ErrKeyspaceAlreadyOpen
		}
		return sks, nil
	}
	if _, ok := tx.openKeyspaces[handle]; ok {
		return nil, ErrKeyspaceKindMismatch
	}
	if _, deleted := tx.pendingDeletes[name]; deleted {
		delete(tx.pendingDeletes, name)
		desc := page.KeyspaceDescriptor{
			Kind:           page.KeyspaceKindSetKeyspace,
			FixedValueSize: fvs,
		}
		tx.numKeyspaces++
		sks := tx.cacheOpenSetKeyspace(handle, desc, keyspaceStateCreated)
		if len(pinned) > 0 {
			if err := tx.writeNewIndexRegistry(sks, pinned); err != nil {
				// Restore pending-delete (M-1 fix).
				delete(tx.openSetKeyspaces, handle)
				tx.numKeyspaces--
				tx.pendingDeletes[name] = struct{}{}
				return nil, err
			}
		}
		sks.indexes = pinned
		return sks, nil
	}
	desc, found, err := tx.lookupDescriptor(name)
	if err != nil {
		return nil, err
	}
	if found {
		if err := checkKeyspaceKind(desc.Kind, page.KeyspaceKindSetKeyspace); err != nil {
			return nil, err
		}
		if desc.FixedValueSize != fvs {
			return nil, fmt.Errorf("%w: existing FixedValueSize=%d, opts.FixedValueSize=%d",
				ErrFixedValueSizeMismatch, desc.FixedValueSize, fvs)
		}
		sks := tx.cacheOpenSetKeyspace(handle, desc, keyspaceStateClean)
		if err := tx.validatePinnedAgainstRegistry(sks, name, pinned); err != nil {
			delete(tx.openSetKeyspaces, handle)
			return nil, err
		}
		delete(tx.dirtyDescriptors, name)
		sks.indexes = pinned
		return sks, nil
	}
	desc = page.KeyspaceDescriptor{
		Kind:           page.KeyspaceKindSetKeyspace,
		FixedValueSize: fvs,
	}
	tx.numKeyspaces++
	sks := tx.cacheOpenSetKeyspace(handle, desc, keyspaceStateCreated)
	if len(pinned) > 0 {
		if err := tx.writeNewIndexRegistry(sks, pinned); err != nil {
			delete(tx.openSetKeyspaces, handle)
			tx.numKeyspaces--
			return nil, err
		}
	}
	sks.indexes = pinned
	return sks, nil
}

// validateSetOpts normalises a *SetKeyspaceOptions into the uint16
// fixedValueSize stored in the descriptor. Returns ErrInvalidOptions
// for negative or > 65535 values; nil opts → 0.
func validateSetOpts(opts *SetKeyspaceOptions) (uint16, error) {
	if opts == nil {
		return 0, nil
	}
	if opts.FixedValueSize < 0 || opts.FixedValueSize > 0xFFFF {
		return 0, fmt.Errorf("%w: SetKeyspaceOptions.FixedValueSize %d out of range [0, 65535]",
			ErrInvalidOptions, opts.FixedValueSize)
	}
	return uint16(opts.FixedValueSize), nil
}

// cacheOpenSetKeyspace constructs the *SetKeyspace and registers it
// in the tx's per-name cache. All Open / Create paths route through
// here.
func (tx *Tx) cacheOpenSetKeyspace(handle uniqueNameHandle, desc page.KeyspaceDescriptor, state keyspaceState) *SetKeyspace {
	sks := &SetKeyspace{keyspaceCore: keyspaceCore{tx: tx, name: handle, desc: desc, state: state}}
	if tx.openSetKeyspaces == nil {
		tx.openSetKeyspaces = make(map[uniqueNameHandle]*SetKeyspace)
	}
	tx.openSetKeyspaces[handle] = sks
	return sks
}

// Has reports whether key has any values stored (i.e., the
// SetKeyspace contains an entry for this key). For a key with no
// values, returns (false, nil) — empty sets do not persist per
// set-keyspace.md invariant #1, so "has key" and "has any value
// for key" are equivalent.
//
// Errors: ErrKeyEmpty (nil/empty key), ErrKeyspaceClosed (handle
// invalidated), ErrCorrupted (wrapped) on structural fault.
func (ks *SetKeyspace) Has(key []byte) (bool, error) {
	if err := ks.checkReadable(key); err != nil {
		return false, err
	}
	if ks.desc.Root == 0 {
		return false, nil
	}
	cfg := ks.builderCfg()
	exists, err := btree.Has(ks.tx.pgr, cfg, ks.desc.Root, key)
	if err != nil {
		return false, mapBtreeErr(err)
	}
	return exists, nil
}

// HasValue reports whether (key, value) is a member of the keyspace.
// Returns false (not error) when the key is absent OR when the key
// is present but value is not in its set.
//
// Errors: ErrKeyEmpty (nil/empty key — value may be empty per
// set-keyspace.md which allows zero-length values),
// ErrKeyspaceClosed, ErrCorrupted (wrapped).
func (ks *SetKeyspace) HasValue(key, value []byte) (bool, error) {
	if err := ks.checkReadable(key); err != nil {
		return false, err
	}
	if ks.desc.Root == 0 {
		return false, nil
	}
	cfg := ks.builderCfg()
	e, found, err := btree.GetEntry(ks.tx.pgr, cfg, ks.desc.Root, key)
	if err != nil {
		return false, mapBtreeErr(err)
	}
	if !found {
		return false, nil
	}
	return ks.cellHasValue(cfg, e, value)
}

// cellHasValue dispatches HasValue on a decoded leaf cell.
func (ks *SetKeyspace) cellHasValue(cfg page.Config, e page.LeafEntry, value []byte) (bool, error) {
	switch {
	case e.IsSubpage():
		sp := page.NewSubpageReader(e.Value, ks.desc.FixedValueSize)
		_, found := sp.Search(value)
		return found, nil
	case e.IsNestedTree():
		exists, err := btree.Has(ks.tx.pgr, cfg, e.NestedRoot, value)
		if err != nil {
			return false, mapBtreeErr(err)
		}
		return exists, nil
	default:
		// A Kind=1 cell that is neither Subpage nor NestedTree is
		// structural corruption (every SetKeyspace cell must be one
		// of the two per set-keyspace.md §Storage Strategy).
		return false, fmt.Errorf("%w: SetKeyspace cell at key %q has unexpected CellFlags 0x%x (expected Subpage or NestedTree)",
			ErrCorrupted, e.Key, e.Flags)
	}
}

// CountValues returns the number of values stored under key. Returns
// (0, nil) when the key is absent (per Inv-1: empty sets don't
// persist, so a missing key has zero values).
//
// O(1) for nested-tree cells (read from cell's NestedCount field);
// O(1) for subpage cells (read from subpage's Count header).
//
// Errors: ErrKeyEmpty, ErrKeyspaceClosed, ErrCorrupted (wrapped).
func (ks *SetKeyspace) CountValues(key []byte) (uint64, error) {
	if err := ks.checkReadable(key); err != nil {
		return 0, err
	}
	if ks.desc.Root == 0 {
		return 0, nil
	}
	cfg := ks.builderCfg()
	e, found, err := btree.GetEntry(ks.tx.pgr, cfg, ks.desc.Root, key)
	if err != nil {
		return 0, mapBtreeErr(err)
	}
	if !found {
		return 0, nil
	}
	switch {
	case e.IsSubpage():
		sp := page.NewSubpageReader(e.Value, ks.desc.FixedValueSize)
		return uint64(sp.Count()), nil
	case e.IsNestedTree():
		return e.NestedCount, nil
	default:
		return 0, fmt.Errorf("%w: SetKeyspace cell at key %q has unexpected CellFlags 0x%x",
			ErrCorrupted, e.Key, e.Flags)
	}
}

// Put inserts value into the set stored under key. added reports
// whether the set actually grew (false iff (key, value) was already
// present — the call is a no-op in that case). Per the chunk-6.1
// user-locked SetKeyspace.Put signature.
//
// On a missing key: creates a new subpage cell with just (value).
// On an existing key with subpage cell: inserts into the subpage; if
// the result exceeds SubpagePromotionThreshold, promotes to a
// nested B+tree (chunk 6.4 PromoteSubpageToNestedTree).
// On an existing key with nested-tree cell: inserts into the nested
// tree via btree.InsertIfAbsent (one descent, a no-op if the value is
// already a member); on insert, updates the parent cell's NestedCount.
//
// Fixed-size SetKeyspace: a wrong-length value returns
// ErrValueSizeMismatch BEFORE any tree mutation.
//
// Errors: ErrKeyEmpty (nil/empty key), ErrKeyspaceClosed,
// ErrValueSizeMismatch, ErrReadOnly (read-tx — caught by
// requireOpen(true)), ErrTxTooLarge / ErrDBFull (allocation),
// ErrCorrupted (structural).
//
// Side effects on success:
//   - desc.Root reflects the new btree root.
//   - desc.Count increments by 1 iff added=true.
//   - State transitions to Dirty (unless already Created).
func (ks *SetKeyspace) Put(key, value []byte) (added bool, err error) {
	if err := ks.checkWritable(key); err != nil {
		return false, err
	}
	fvs := ks.desc.FixedValueSize
	if fvs != 0 && len(value) != int(fvs) {
		return false, fmt.Errorf("%w: value len %d, keyspace FixedValueSize %d", ErrValueSizeMismatch, len(value), fvs)
	}
	if value == nil {
		value = []byte{}
	}
	cfg := ks.builderCfg()

	// Indexed-keyspace path (chunk 7.9): pre-probe the membership
	// to determine if this Put is an add or a no-op. The spec at
	// indexing.md §Write Path requires maintenance BEFORE the row
	// write so a unique-probe failure aborts cleanly; for
	// SetKeyspace there's no "old value to diff" — Put is either
	// a no-op (pair already present) or an insert. The pre-probe
	// duplicates the membership check the subsequent dispatch
	// would perform; the redundancy is acceptable as the cost of
	// the abort-before-mutation contract.
	if len(ks.indexes) > 0 {
		already, hvErr := ks.HasValue(key, value)
		if hvErr != nil {
			return false, hvErr
		}
		if already {
			return false, nil // no-op; no maintenance fires
		}
		// Two atomicity layers cover the indexed Put against a per-op
		// error followed by Tx.Commit (see Keyspace.Put godoc for the
		// full rationale): rowSnap + restoreIndexes covers in-memory
		// pinnedIndex; psp (pager savepoint) covers on-disk page
		// allocations that applyIndexMaintenanceOnAddValue and the
		// subsequent dispatched btree mutation (genesis subpage,
		// putIntoSubpage, putIntoNestedTree) made — so a per-op error
		// followed by Tx.Commit does not orphan the in-flight
		// allocations.
		psp := ks.tx.pgr.BeginShallowSavepoint()
		rowSnap := snapshotIndexes(ks.indexes)
		if mErr := ks.applyIndexMaintenanceOnAddValue(key, value); mErr != nil {
			// The helper does not snapshot pinned state — see its godoc.
			// rowSnap is the sole atomicity-rollback for in-memory
			// pinned state, covering both this helper's failure and
			// the dispatched btree.Put failure further below.
			restoreIndexes(ks.indexes, rowSnap)
			ks.tx.pgr.RestoreSavepoint(psp)
			return false, mErr
		}
		// Revert pinned state on any failure OR on the (currently
		// unreachable in single-writer-tx) case where the dispatch
		// returns added=false despite our pre-probe saying not-
		// present. The contract per indexing.md is "row write
		// happened" = (err==nil && added==true); any other outcome
		// means our maintenance mutated pinned without a matching
		// row mutation, and we must restore. (Chunk-7.9 Round-1
		// M-1 fix.)
		defer func() {
			if err != nil || !added {
				restoreIndexes(ks.indexes, rowSnap)
				ks.tx.pgr.RestoreSavepoint(psp)
			} else {
				ks.tx.pgr.ReleaseSavepoint(psp)
			}
		}()
	}

	if ks.desc.Root == 0 {
		// Genesis: build a single-entry subpage + insert as the
		// keyspace's root cell.
		sp, err := page.EncodeSubpage([][]byte{value}, fvs)
		if err != nil {
			return false, fmt.Errorf("SetKeyspace.Put: encode genesis subpage: %w", err)
		}
		newRoot, _, err := btree.PutEntry(ks.tx.pgr, cfg, ks.desc.Root, page.LeafEntry{
			Flags: page.CellFlagMultiValue,
			Key:   key,
			Value: sp,
		})
		if err != nil {
			return false, mapBtreeErr(err)
		}
		ks.desc.Root = newRoot
		ks.desc.Count++
		ks.markDirty()
		ks.markSetCursorsStale()
		return true, nil
	}

	e, found, err := btree.GetEntry(ks.tx.pgr, cfg, ks.desc.Root, key)
	if err != nil {
		return false, mapBtreeErr(err)
	}
	if !found {
		// New key in a non-empty tree.
		sp, err := page.EncodeSubpage([][]byte{value}, fvs)
		if err != nil {
			return false, fmt.Errorf("SetKeyspace.Put: encode new-key subpage: %w", err)
		}
		newRoot, _, err := btree.PutEntry(ks.tx.pgr, cfg, ks.desc.Root, page.LeafEntry{
			Flags: page.CellFlagMultiValue,
			Key:   key,
			Value: sp,
		})
		if err != nil {
			return false, mapBtreeErr(err)
		}
		ks.desc.Root = newRoot
		ks.desc.Count++
		ks.markDirty()
		ks.markSetCursorsStale()
		return true, nil
	}

	// Cell exists — dispatch by type.
	switch {
	case e.IsSubpage():
		return ks.putIntoSubpage(cfg, key, value, e)
	case e.IsNestedTree():
		return ks.putIntoNestedTree(cfg, key, value, e)
	default:
		return false, fmt.Errorf("%w: SetKeyspace.Put: cell at key %q has unexpected CellFlags 0x%x",
			ErrCorrupted, key, e.Flags)
	}
}

// putIntoSubpage handles Put when the cell is a subpage. Inserts the
// value into the subpage; if the result still fits, replaces the
// cell with the new subpage. If the result exceeds the promotion
// threshold, calls PromoteSubpageToNestedTree and installs a
// nested-tree-ref cell.
func (ks *SetKeyspace) putIntoSubpage(cfg page.Config, key, value []byte, e page.LeafEntry) (bool, error) {
	fvs := ks.desc.FixedValueSize
	sp := page.NewSubpageReader(e.Value, fvs)
	newSubpage, added, err := sp.Insert(value)
	if err != nil {
		return false, fmt.Errorf("SetKeyspace.Put: subpage Insert: %w", err)
	}
	if !added {
		// Duplicate value — no-op.
		return false, nil
	}
	threshold := page.SubpagePromotionThreshold(cfg)
	if len(newSubpage) <= threshold {
		// Fits — update the subpage cell in place via PutEntry.
		newRoot, _, err := btree.PutEntry(ks.tx.pgr, cfg, ks.desc.Root, page.LeafEntry{
			Flags: page.CellFlagMultiValue,
			Key:   key,
			Value: newSubpage,
		})
		if err != nil {
			return false, mapBtreeErr(err)
		}
		ks.desc.Root = newRoot
		ks.desc.Count++
		ks.markDirty()
		ks.markSetCursorsStale()
		return true, nil
	}
	// Promote: build the nested tree from the EXISTING subpage +
	// the new value (PromoteSubpageToNestedTree handles the 4-step
	// algorithm using the pre-insert subpage bytes).
	root, count, err := btree.PromoteSubpageToNestedTree(ks.tx.pgr, cfg, e.Value, fvs, value)
	if err != nil {
		return false, mapBtreeErr(err)
	}
	newRoot, _, err := btree.PutEntry(ks.tx.pgr, cfg, ks.desc.Root, page.LeafEntry{
		Flags:       page.CellFlagMultiValue | page.CellFlagNestedTree,
		Key:         key,
		NestedRoot:  root,
		NestedCount: count,
	})
	if err != nil {
		return false, mapBtreeErr(err)
	}
	ks.desc.Root = newRoot
	ks.desc.Count++
	ks.markDirty()
	ks.markSetCursorsStale()
	return true, nil
}

// putIntoNestedTree handles Put when the cell is a nested-tree-ref.
// Inserts the value into the nested tree via btree.InsertIfAbsent (a
// single descent that is a true no-op on an existing member); on
// duplicate returns (false, nil) without mutation. On success,
// updates the parent cell's NestedRoot + NestedCount.
func (ks *SetKeyspace) putIntoNestedTree(cfg page.Config, key, value []byte, e page.LeafEntry) (bool, error) {
	// Single-descent insert-if-absent: btree.InsertIfAbsent reports
	// whether the value was newly added and, when already present, is a
	// true no-op (no CoW, no alloc) — so a duplicate set-insert neither
	// pays a second descent (the old btree.Has + btree.Put) nor orphans
	// rewritten pages.
	newNestedRoot, added, err := btree.InsertIfAbsent(ks.tx.pgr, cfg, e.NestedRoot, value, nil)
	if err != nil {
		return false, mapBtreeErr(err)
	}
	if !added {
		return false, nil
	}
	newCell := page.LeafEntry{
		Flags:       page.CellFlagMultiValue | page.CellFlagNestedTree,
		Key:         key,
		NestedRoot:  newNestedRoot,
		NestedCount: e.NestedCount + 1,
	}
	newRoot, _, err := btree.PutEntry(ks.tx.pgr, cfg, ks.desc.Root, newCell)
	if err != nil {
		return false, mapBtreeErr(err)
	}
	ks.desc.Root = newRoot
	ks.desc.Count++
	ks.markDirty()
	ks.markSetCursorsStale()
	return true, nil
}

// Delete removes the key and ALL its values from the SetKeyspace.
// Returns ErrNotFound when the key does not exist (per the chunk-5.1
// user-locked Delete-on-miss invariant for keyed-removal APIs —
// api-surface.md §Invariants).
//
// For a key with a nested-tree cell, bulk-frees the nested subtree
// via btree.FreeSubtree (O(pages-in-nested-tree), not O(values) —
// per set-keyspace.md §Bulk Free).
//
// Errors: ErrKeyEmpty, ErrKeyspaceClosed, ErrNotFound (Delete-on-
// miss), ErrCorrupted, pager allocation errors.
func (ks *SetKeyspace) Delete(key []byte) (err error) {
	if err := ks.checkWritable(key); err != nil {
		return err
	}
	if ks.desc.Root == 0 {
		return ErrNotFound
	}
	cfg := ks.builderCfg()
	e, found, err := btree.GetEntry(ks.tx.pgr, cfg, ks.desc.Root, key)
	if err != nil {
		return mapBtreeErr(err)
	}
	if !found {
		return ErrNotFound
	}

	// Indexed-keyspace path (chunk 7.9 + indexing.md §Indexes on
	// SetKeyspaces): bulk-key Delete on an indexed SetKeyspace
	// cannot use the bulk-free fast path because each (key, value)
	// pair must have its index entries removed via the extractor.
	// Walk every set member, invoke applyIndexMaintenanceOnRemoveValue
	// per pair, then drop the row's leaf cell.
	//
	// Two atomicity layers cover the indexed bulk-key Delete against
	// a per-op error followed by Tx.Commit (see Keyspace.Put godoc):
	// rowSnap + restoreIndexes covers in-memory pinnedIndex; psp
	// (pager savepoint) covers on-disk page allocations the bulk
	// per-member maintenance walk AND the subsequent FreeSubtree /
	// btree.Delete row drop made — so a per-op error followed by
	// Tx.Commit does not orphan the in-flight allocations.
	if len(ks.indexes) > 0 {
		psp := ks.tx.pgr.BeginShallowSavepoint()
		rowSnap := snapshotIndexes(ks.indexes)
		if err := ks.applyIndexMaintenanceOnBulkKeyDelete(cfg, key, e); err != nil {
			restoreIndexes(ks.indexes, rowSnap)
			ks.tx.pgr.RestoreSavepoint(psp)
			return err
		}
		defer func() {
			if err != nil {
				restoreIndexes(ks.indexes, rowSnap)
				ks.tx.pgr.RestoreSavepoint(psp)
			} else {
				ks.tx.pgr.ReleaseSavepoint(psp)
			}
		}()
	}

	// Determine the value count contributed by this cell (for
	// desc.Count delta) BEFORE freeing any pages.
	var valuesFreed uint64
	switch {
	case e.IsSubpage():
		sp := page.NewSubpageReader(e.Value, ks.desc.FixedValueSize)
		valuesFreed = uint64(sp.Count())
	case e.IsNestedTree():
		valuesFreed = e.NestedCount
		// Bulk-free the nested subtree first (the parent cell still
		// references it). FreeSubtree's count is sanity-checkable
		// against e.NestedCount per entailed invariant E1.
		freed, err := btree.FreeSubtree(ks.tx.pgr, cfg, e.NestedRoot)
		if err != nil {
			return mapBtreeErr(err)
		}
		if freed != e.NestedCount {
			// E1 violation surfaces as corruption.
			return fmt.Errorf("%w: SetKeyspace.Delete %q: FreeSubtree freed %d values, cell NestedCount=%d",
				ErrCorrupted, key, freed, e.NestedCount)
		}
	default:
		return fmt.Errorf("%w: SetKeyspace.Delete %q: unexpected CellFlags 0x%x",
			ErrCorrupted, key, e.Flags)
	}

	// Remove the cell from the parent tree.
	mergeThreshold := ks.tx.db.opts.MergeThreshold
	newRoot, err := btree.Delete(ks.tx.pgr, cfg, ks.desc.Root, mergeThreshold, key)
	if err != nil {
		if errors.Is(err, btree.ErrNotFound) {
			// Race-impossible (we just observed the cell via
			// GetEntry); treat as corruption.
			return fmt.Errorf("%w: SetKeyspace.Delete %q: btree.Delete reports ErrNotFound after GetEntry hit",
				ErrCorrupted, key)
		}
		return mapBtreeErr(err)
	}
	ks.desc.Root = newRoot
	ks.desc.Count -= valuesFreed
	ks.markDirty()
	ks.markSetCursorsStale()
	return nil
}

// DeleteValue removes a single (key, value) pair. Returns
// ErrNotFound when the key does not exist OR when the key exists but
// value is not in its set (per the chunk-5.1 Delete-on-miss
// invariant — api-surface.md §Invariants).
//
// On a subpage cell: removes the value from the subpage; if the
// subpage now has zero values, removes the parent cell entirely
// (Inv-1: empty sets must not persist).
// On a nested-tree cell: removes the value via btree.Delete; on
// success, checks DemoteNestedTreeIfFits and replaces the parent
// cell with a subpage when the nested tree shrinks below the
// promotion threshold.
//
// Errors: ErrKeyEmpty, ErrKeyspaceClosed, ErrValueSizeMismatch
// (fixed-size keyspace wrong-length value), ErrNotFound (Delete-on-
// miss), ErrCorrupted, pager allocation errors.
func (ks *SetKeyspace) DeleteValue(key, value []byte) (err error) {
	if err := ks.checkWritable(key); err != nil {
		return err
	}
	fvs := ks.desc.FixedValueSize
	if fvs != 0 && len(value) != int(fvs) {
		return fmt.Errorf("%w: value len %d, keyspace FixedValueSize %d", ErrValueSizeMismatch, len(value), fvs)
	}
	if ks.desc.Root == 0 {
		return ErrNotFound
	}
	cfg := ks.builderCfg()

	// Indexed-keyspace path (chunk 7.9): pre-probe that the
	// (key, value) pair exists, then run index maintenance BEFORE
	// the actual subpage / nested-tree delete. On any subsequent
	// failure, restore pinned state.
	//
	// Two atomicity layers cover the indexed DeleteValue against a
	// per-op error followed by Tx.Commit (see Keyspace.Put godoc):
	// rowSnap + restoreIndexes covers in-memory pinnedIndex; psp
	// (pager savepoint) covers on-disk page allocations
	// applyIndexMaintenanceOnRemoveValue and the subsequent
	// subpage / nested-tree delete made — so a per-op error followed
	// by Tx.Commit does not orphan the in-flight allocations.
	if len(ks.indexes) > 0 {
		present, hvErr := ks.HasValue(key, value)
		if hvErr != nil {
			return hvErr
		}
		if !present {
			return ErrNotFound
		}
		psp := ks.tx.pgr.BeginShallowSavepoint()
		rowSnap := snapshotIndexes(ks.indexes)
		if mErr := ks.applyIndexMaintenanceOnRemoveValue(key, value); mErr != nil {
			// The helper does not snapshot pinned state — see its godoc.
			// rowSnap is the sole atomicity-rollback for in-memory
			// pinned state, covering both this helper's failure and
			// the dispatched subpage / nested-tree delete failure
			// further below.
			restoreIndexes(ks.indexes, rowSnap)
			ks.tx.pgr.RestoreSavepoint(psp)
			return mErr
		}
		defer func() {
			if err != nil {
				restoreIndexes(ks.indexes, rowSnap)
				ks.tx.pgr.RestoreSavepoint(psp)
			} else {
				ks.tx.pgr.ReleaseSavepoint(psp)
			}
		}()
	}

	e, found, err := btree.GetEntry(ks.tx.pgr, cfg, ks.desc.Root, key)
	if err != nil {
		return mapBtreeErr(err)
	}
	if !found {
		return ErrNotFound
	}
	switch {
	case e.IsSubpage():
		return ks.deleteValueFromSubpage(cfg, key, value, e)
	case e.IsNestedTree():
		return ks.deleteValueFromNestedTree(cfg, key, value, e)
	default:
		return fmt.Errorf("%w: SetKeyspace.DeleteValue %q: unexpected CellFlags 0x%x",
			ErrCorrupted, key, e.Flags)
	}
}

// deleteValueFromSubpage handles DeleteValue when the cell is a
// subpage. If the value is not present, returns ErrNotFound (Delete-
// on-miss). If the deletion would leave the subpage empty (Count=0),
// removes the parent cell entirely (Inv-1: empty sets must not
// persist).
func (ks *SetKeyspace) deleteValueFromSubpage(cfg page.Config, key, value []byte, e page.LeafEntry) error {
	fvs := ks.desc.FixedValueSize
	sp := page.NewSubpageReader(e.Value, fvs)
	newSubpage, deleted, err := sp.Delete(value)
	if err != nil {
		return fmt.Errorf("SetKeyspace.DeleteValue: subpage Delete: %w", err)
	}
	if !deleted {
		return ErrNotFound
	}
	newReader := page.NewSubpageReader(newSubpage, fvs)
	if newReader.Count() == 0 {
		// Removed the last value — drop the parent cell.
		mergeThreshold := ks.tx.db.opts.MergeThreshold
		newRoot, err := btree.Delete(ks.tx.pgr, cfg, ks.desc.Root, mergeThreshold, key)
		if err != nil {
			return mapBtreeErr(err)
		}
		ks.desc.Root = newRoot
		ks.desc.Count--
		ks.markDirty()
		ks.markSetCursorsStale()
		return nil
	}
	// Replace cell with shrunk subpage.
	newRoot, _, err := btree.PutEntry(ks.tx.pgr, cfg, ks.desc.Root, page.LeafEntry{
		Flags: page.CellFlagMultiValue,
		Key:   key,
		Value: newSubpage,
	})
	if err != nil {
		return mapBtreeErr(err)
	}
	ks.desc.Root = newRoot
	ks.desc.Count--
	ks.markDirty()
	ks.markSetCursorsStale()
	return nil
}

// setKeyspaceCellFree is the per-cell free callback
// SetKeyspace.DeleteRange (un-indexed path) passes to
// btree.DeleteRange. SetKeyspace cells split three ways per
// set-keyspace.md §Storage Strategy + §Nested B+tree Reference Cell:
//
//   - Nested-tree cell (CellFlagMultiValue|CellFlagNestedTree):
//     recursively retires the nested B+tree via btree.FreeSubtree;
//     contributes NestedCount values (per set-keyspace.md Inv-2
//     entailed: the cell's Count field equals the number of leaf
//     entries reachable from Root). FreeSubtree's returned count
//     IS that NestedCount by construction — it walks the nested
//     tree and tallies entries; an on-disk mismatch surfaces as
//     ErrCorrupted on the next sanity check.
//   - Subpage cell (CellFlagMultiValue, no NestedTree): the
//     inline subpage bytes go away with the parent leaf entry; no
//     extra page retire. Contributes Subpage.Count values
//     (decoded from the 2-byte Count header at offset 0 of
//     e.Value; matches btree.FreeSubtree's subpage handling for
//     consistency).
//   - Plain cell (no MultiValue flag): contributes 1 value. If
//     CellFlagOverflow is set, the overflow chain is retired via
//     pw.FreeRun. Reachable for SetKeyspace only when the user
//     stores a single inline value above the subpage promotion
//     threshold (rare but in-spec).
//
// Mirrors the cell-type-aware retire + count logic in
// btree.freeSubtreeAt (subtree.go §Count semantics) used for
// interior subtrees in the same DeleteRange call. The two paths
// must agree on count semantics so the (interior + boundary)
// total returned by DeleteRange equals desc.Count's per-cell
// accounting (set-keyspace.md Inv-2 entailed + Inv entailed E2).
func setKeyspaceCellFree(pw btree.PageWriter, cfg page.Config, e page.LeafEntry) (uint64, error) {
	switch {
	case e.IsNestedTree():
		if e.NestedRoot == 0 {
			return 0, fmt.Errorf("%w: SetKeyspace DeleteRange: nested-tree cell has NestedRoot=0", btree.ErrCorrupted)
		}
		freed, err := btree.FreeSubtree(pw, cfg, e.NestedRoot)
		if err != nil {
			return 0, err
		}
		// Defense-in-depth (mirrors SetKeyspace.Delete's bulk-free
		// path at set_keyspace.go's deleteFromNestedTree branch): the
		// cell's NestedCount field MUST equal the walked-leaf tally
		// per set-keyspace.md §Invariants Inv-2 entailed ("the
		// nested-tree reference cell's Count field equals the number
		// of leaf entries reachable from Root"). A divergence is
		// on-disk corruption — surface as ErrCorrupted rather than
		// silently letting desc.Count drift (the parent-level
		// `count > ks.desc.Count` defense at DeleteRange's tail can't
		// catch an undercount; only an overcount).
		if freed != uint64(e.NestedCount) {
			return 0, fmt.Errorf("%w: SetKeyspace DeleteRange: FreeSubtree freed %d values, cell NestedCount=%d",
				btree.ErrCorrupted, freed, e.NestedCount)
		}
		return freed, nil
	case e.IsSubpage():
		// Inline subpage — no extra page retire; Count semantic
		// matches freeSubtreeAt: read the 2-byte Count header
		// directly via SubpageReader with fixedValueSize=0
		// (Count is independent of variable/fixed mode).
		sp := page.NewSubpageReader(e.Value, 0)
		return uint64(sp.Count()), nil
	case e.IsOverflow():
		runLen := page.OverflowRunLength(cfg, e.TotalLen)
		if err := pw.FreeRun(e.OverflowPage, runLen); err != nil {
			return 0, fmt.Errorf("btree: SetKeyspace DeleteRange free overflow chain at %d (run=%d): %w",
				e.OverflowPage, runLen, err)
		}
		return 1, nil
	default:
		return 1, nil
	}
}

// DeleteRange deletes every (key, value) pair whose KEY falls in
// [start, end) from the SetKeyspace. Returns the count of VALUES
// deleted (entailed E2 accounting — desc.Count delta), NOT the
// count of keys. Returns (0, nil) for an empty range (start ==
// end, start > end, nil/nil on an empty keyspace, or no keys
// matching).
//
// Boundary semantics (api-surface.md §SetKeyspace.DeleteRange):
//   - nil = open-boundary sentinel. nil start = "from the
//     beginning"; nil end = "through the last key"; (nil, nil) =
//     every key.
//   - Non-nil zero-length ([]byte{}) is rejected with ErrKeyEmpty.
//
// Mechanism splits on whether the SetKeyspace has secondary
// indexes declared (per range-delete.md §Indexed-keyspace
// fallback chunk-7.10 amendment — the same dispatch shape
// Keyspace.DeleteRange uses):
//
//   - **Un-indexed (len(ks.indexes) == 0)**: dispatches to
//     btree.DeleteRange with setKeyspaceCellFree. The walker
//     descends once, retires interior subtrees via FreeSubtree
//     (which handles SetKeyspace cell types — subpage / nested-
//     tree / overflow — and tallies values), and at boundary
//     leaves invokes setKeyspaceCellFree per deleted entry.
//     Cost: O(P + depth²) walker descent (range-delete.md
//     §Complexity worked example) — single tree walk vs. v1's
//     O(K log N) per-key descents. **Atomic on error**: returns
//     (0, err) with no observable mutations (tx-level Rollback
//     restores via pager bitmap snapshot per pager-slab.md), the
//     same all-or-nothing contract as Keyspace.DeleteRange. The
//     chunk-6.8 per-row partial-progress contract no longer
//     applies on this path; an error means nothing was deleted.
//
//   - **Indexed (len(ks.indexes) > 0)**: dispatches to the per-
//     key Delete loop helper (deleteRangePerKey). Each
//     SetKeyspace.Delete invokes chunk-7.9's
//     applyIndexMaintenanceOnBulkKeyDelete, walking every
//     (setKey, setValue) pair and clearing index entries via the
//     extractor. **Per-row atomic on error**: returns
//     (deleted_so_far, err) — iterations 0..i-1 are committed
//     in-memory; the failing iteration and remainder are
//     untouched. Cost: O(K × M × (indexes + extractor)) where K
//     = keys in range, M = average set size per key.
//
// The atomicity-contract split between the two paths mirrors
// Keyspace.DeleteRange's chunk-7.10 split (atomic walker for
// Kind=0 un-indexed; per-row cursor walk for Kind=0 indexed).
//
// Errors:
//   - ErrKeyspaceClosed (handle invalidated by same-tx
//     DeleteKeyspace).
//   - ErrTxClosed / ErrReadOnly via Tx.requireOpen.
//   - ErrKeyEmpty for non-nil zero-length bounds.
//   - Pager errors (ErrTxTooLarge, ErrDBFull) propagate.
//   - ErrCorrupted (wrapped) on structural fault.
//
// Side effects on success (in-memory; persisted at Tx.Commit's
// flushKeyspaces walk):
//   - desc.Root reflects the post-delete root.
//   - desc.Count decrements by the returned value count.
//   - state transitions to Dirty unless already Created.
//   - Every open SetCursor on this keyspace is MarkStale'd.
func (ks *SetKeyspace) DeleteRange(start, end []byte) (uint64, error) {
	if err := ks.requireWritable(); err != nil {
		return 0, err
	}
	if start != nil && len(start) == 0 {
		return 0, ErrKeyEmpty
	}
	if end != nil && len(end) == 0 {
		return 0, ErrKeyEmpty
	}
	if ks.desc.Root == 0 {
		return 0, nil
	}
	if start != nil && end != nil && bytes.Compare(start, end) >= 0 {
		return 0, nil
	}
	if len(ks.indexes) > 0 {
		// Indexed path: per-key Delete loop preserves chunk-7.9's
		// applyIndexMaintenanceOnBulkKeyDelete per-row contract +
		// chunk-6.8 per-row-atomic partial-progress semantic.
		cfg := ks.builderCfg()
		return ks.deleteRangePerKey(cfg, start, end)
	}

	// Un-indexed path: atomic walker. setKeyspaceCellFree handles
	// subpage / nested-tree / overflow cells and tallies values, so the
	// returned count is values-correct (set-keyspace.md Inv-2 entailed
	// E2); desc.Count is kept in the same value unit.
	return ks.deleteRangeUnindexed(start, end, setKeyspaceCellFree, ks.markSetCursorsStale)
}

// deleteRangePerKey is the indexed-SetKeyspace path for DeleteRange.
// Snapshots keys in [start, end) and calls ks.Delete(k) per row.
// Per-row atomic via SetKeyspace.Delete's existing per-row index
// maintenance contract; returns (deleted_so_far, err) on a per-row
// error. The walker shape (btree.DeleteRange) is unsafe here
// because the extractor needs each (setKey, setValue) pair's value
// to compute the prior index keys, which subtree retirement does
// not visit.
//
// Count accumulation uses a per-iteration wrap-immune shape:
// capture desc.Count BEFORE each Delete, verify it did not
// INCREASE (a corruption signal — set-keyspace.md §Invariants
// entailed E2 has desc.Count strictly decrease per successful
// Delete on a non-empty cell), and accumulate the per-row delta.
// Compared to the chunk-6.8 v1 pattern (`before - ks.desc.Count`
// once at end) the per-iteration shape is wrap-immune even under
// a corrupt on-disk state where Delete might leave desc.Count
// above its pre-call value. Cannot use Keyspace.deleteRangeIndexed's
// `count++` per-row pattern because SetKeyspace cells may carry
// N>1 values (subpage Count, nested-tree NestedCount); the delta
// must be computed from desc.Count, not assumed to be 1.
func (ks *SetKeyspace) deleteRangePerKey(cfg page.Config, start, end []byte) (uint64, error) {
	// Snapshot up front so the per-key Delete calls don't
	// invalidate the iteration (each Delete mutates the parent
	// tree's root, which would stale a cursor mid-walk).
	keys, err := ks.snapshotKeysInRange(cfg, start, end)
	if err != nil {
		return 0, err
	}
	if len(keys) == 0 {
		return 0, nil
	}
	var count uint64
	for _, k := range keys {
		before := ks.desc.Count
		if err := ks.Delete(k); err != nil {
			// Partial-progress error: iterations 0..i-1 have
			// completed; their effects on desc.Count + desc.Root +
			// sibling cursors + on-disk page retirements stand.
			// Return (deleted_so_far, err); the only safe recovery
			// is Tx.Rollback (pager bitmap snapshot per
			// pager-slab.md).
			return count, err
		}
		if ks.desc.Count > before {
			// Defense-in-depth (mirrors the un-indexed walker's
			// `count > ks.desc.Count` check at the parent
			// DeleteRange tail): a successful Delete must not
			// INCREASE desc.Count; an increase is on-disk
			// corruption (set-keyspace.md §Invariants entailed E2
			// — desc.Count equals the sum of stored values; Delete
			// decrements by the cell's value count).
			return count, fmt.Errorf("%w: SetKeyspace.Delete(%q) raised desc.Count from %d to %d",
				ErrCorrupted, k, before, ks.desc.Count)
		}
		count += before - ks.desc.Count
	}
	return count, nil
}

// snapshotKeysInRange returns a sorted, deep-copied list of keys
// in [start, end). Used by DeleteRange to materialize the
// per-key-delete worklist before any mutation. Read-cursor only —
// no MarkStale side effects, no descriptor mutation.
func (ks *SetKeyspace) snapshotKeysInRange(cfg page.Config, start, end []byte) ([][]byte, error) {
	cur := btree.NewReadCursor(ks.tx.pgr, cfg, ks.desc.Root)
	var k []byte
	if start == nil {
		k, _ = cur.First()
	} else {
		k, _ = cur.SeekGE(start)
	}
	var keys [][]byte
	for ; k != nil; k, _ = cur.Next() {
		if end != nil && bytes.Compare(k, end) >= 0 {
			break
		}
		keys = append(keys, bytes.Clone(k))
	}
	if err := cur.Err(); err != nil {
		return nil, mapBtreeErr(err)
	}
	return keys, nil
}

// deleteValueFromNestedTree handles DeleteValue when the cell is a
// nested-tree ref. Calls btree.Delete on the nested tree, then
// checks DemoteNestedTreeIfFits to potentially shrink back to a
// subpage. Updates the parent cell.
func (ks *SetKeyspace) deleteValueFromNestedTree(cfg page.Config, key, value []byte, e page.LeafEntry) error {
	fvs := ks.desc.FixedValueSize
	mergeThreshold := ks.tx.db.opts.MergeThreshold
	newNestedRoot, err := btree.Delete(ks.tx.pgr, cfg, e.NestedRoot, mergeThreshold, value)
	if err != nil {
		if errors.Is(err, btree.ErrNotFound) {
			return ErrNotFound
		}
		return mapBtreeErr(err)
	}
	newCount := e.NestedCount - 1
	if newCount == 0 {
		// Last value removed; drop the parent cell entirely.
		// (A NestedTree with 1 value at the start of DeleteValue
		// would already be a candidate for demote-then-delete at
		// the SetKeyspace surface; defensively handle the edge.)
		newRoot, err := btree.Delete(ks.tx.pgr, cfg, ks.desc.Root, mergeThreshold, key)
		if err != nil {
			return mapBtreeErr(err)
		}
		ks.desc.Root = newRoot
		ks.desc.Count--
		ks.markDirty()
		ks.markSetCursorsStale()
		return nil
	}
	// Try to demote.
	subpageBytes, demoted, err := btree.DemoteNestedTreeIfFits(ks.tx.pgr, cfg, fvs, newNestedRoot)
	if err != nil {
		return mapBtreeErr(err)
	}
	var newCell page.LeafEntry
	if demoted {
		newCell = page.LeafEntry{
			Flags: page.CellFlagMultiValue,
			Key:   key,
			Value: subpageBytes,
		}
	} else {
		newCell = page.LeafEntry{
			Flags:       page.CellFlagMultiValue | page.CellFlagNestedTree,
			Key:         key,
			NestedRoot:  newNestedRoot,
			NestedCount: newCount,
		}
	}
	newRoot, _, err := btree.PutEntry(ks.tx.pgr, cfg, ks.desc.Root, newCell)
	if err != nil {
		return mapBtreeErr(err)
	}
	ks.desc.Root = newRoot
	ks.desc.Count--
	ks.markDirty()
	ks.markSetCursorsStale()
	return nil
}
