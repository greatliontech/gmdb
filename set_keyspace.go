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
	tx   *Tx
	name uniqueNameHandle

	// desc is the in-tx view of the keyspace's descriptor (Kind=1).
	// Mutated in place by Put / Delete / DeleteValue data-op paths
	// (descriptor.Root + descriptor.Count). Persisted to the
	// keyspace B+tree at Tx.Commit's flushKeyspaces walk per the
	// chunk-5.6 deferred-flush refactor.
	desc page.KeyspaceDescriptor

	// state controls how Tx.Commit's flushKeyspaces walk treats this
	// handle. Same semantics as Keyspace.state: Created or Dirty
	// triggers a btree.Put on the keyspace B+tree; Clean is skipped.
	state keyspaceState

	// dead is set by Tx.DeleteKeyspace on every SetKeyspace handle
	// returned against this name in this tx. Once dead, every
	// SetKeyspace op returns ErrKeyspaceClosed; re-creating the
	// same name does NOT reactivate. Per api-surface.md §Keyspace
	// API DeleteKeyspace.
	dead bool

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

// Name returns the keyspace's name.
func (ks *SetKeyspace) Name() string { return ks.name.Value() }

// FixedValueSize returns the keyspace's declared fixed-value stride
// (0 = variable-size values). Immutable after creation per
// keyspaces.md invariant #5.
func (ks *SetKeyspace) FixedValueSize() int { return int(ks.desc.FixedValueSize) }

// builderCfg returns the page.Config to pass to btree.* calls for
// this keyspace. Mirrors Keyspace.builderCfg — the per-keyspace
// RestartGroupTarget (when set via SetKeyspaceConfig) overrides the
// engine default for newly-written leaves.
func (ks *SetKeyspace) builderCfg() page.Config {
	cfg := ks.tx.pgr.Config()
	if ks.desc.RestartGroupTarget != 0 {
		cfg.RestartGroupTarget = ks.desc.RestartGroupTarget
	}
	return cfg
}

// markDirty transitions the handle's state to Dirty unless it is
// already Created. Centralised so every mutation routes through one
// code path.
func (ks *SetKeyspace) markDirty() {
	if ks.state == keyspaceStateCreated {
		return
	}
	ks.state = keyspaceStateDirty
}

// descriptor returns the in-tx descriptor pointer. Used by the
// chunk-7.3 registry-CRUD helpers (index_codec.go) to satisfy the
// descriptorOwner interface — see *Keyspace.descriptor godoc.
// Unexported.
func (ks *SetKeyspace) descriptor() *page.KeyspaceDescriptor {
	return &ks.desc
}

// markSetCursorsStale invokes MarkStale on every SetCursor
// registered on this keyspace AND refreshes their outer-cursor's
// tracked rootID to the keyspace's current desc.Root. Also sets
// each SetCursor's own `stale` flag so the value-bounded ops
// (NextValue / PrevValue / Current) surface stale even though
// they short-circuit on the materialized values slice. Called by
// Put / Delete / DeleteValue after a successful mutation. Stale
// cursors are not unregistered — the caller may re-position via
// First/Last/Seek/SeekGE (which clears the stale flag).
func (ks *SetKeyspace) markSetCursorsStale() {
	for _, c := range ks.openSetCursors {
		c.outerCursor.MarkStale()
		c.outerCursor.SetRootID(ks.desc.Root)
		c.stale = true
	}
}


// OpenSetKeyspace opens an existing Kind=1 keyspace (SetKeyspace) for
// read+write. Returns ErrNotFound if the named keyspace does not
// exist; ErrKeyspaceKindMismatch if the stored descriptor's Kind is 0
// (Keyspace — use OpenKeyspace); ErrKeyspaceReserved if the name
// resolves to an engine-internal keyspace (Kind=2); ErrCorrupted on
// a malformed descriptor.
//
// Signature deferral: api-surface.md specifies
// `OpenSetKeyspace(name string, indexes ...*IndexDecl)`. The
// variadic IndexDecls land at chunk 7 alongside indexing
// implementation; adding a variadic argument is a non-breaking Go-
// language extension, so chunk-6.6 callers do not need source
// changes when chunk 7 lands.
func (tx *Tx) OpenSetKeyspace(name string) (*SetKeyspace, error) {
	if err := tx.requireOpen(true); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, ErrKeyEmpty
	}
	handle := unique.Make(name)
	if sks, ok := tx.openSetKeyspaces[handle]; ok {
		return sks, nil
	}
	// Reject if a Kind=0 *Keyspace handle is already cached for this
	// name (a same-tx OpenKeyspace then OpenSetKeyspace on the same
	// name violates Kind-immutability; the on-disk descriptor's
	// Kind would surface the mismatch on a fresh resolve, but the
	// cached *Keyspace path would otherwise short-circuit before
	// hitting the descriptor check).
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
	delete(tx.dirtyDescriptors, name)
	return tx.cacheOpenSetKeyspace(handle, desc, keyspaceStateClean), nil
}

// OpenSetKeyspaceReadOnly opens an existing Kind=1 keyspace on a
// read transaction (or on a write tx where the caller wants
// read-only access). Same Kind / Reserved checks as
// OpenSetKeyspace; the returned handle's mutating methods return
// ErrReadOnly when invoked on a read tx (enforced by
// requireOpen(true)).
//
// Chunk-6.6 caveat: the read-tx surface (gmdb.ReadTx) is the
// chunk-3 *ReadTx type; this Tx-bound method is the write-tx
// no-IndexDecls form. The read-tx surface lands as part of the
// chunk-9 / chunk-11 read-tx wiring.
func (tx *Tx) OpenSetKeyspaceReadOnly(name string) (*SetKeyspace, error) {
	if err := tx.requireOpen(false); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, ErrKeyEmpty
	}
	handle := unique.Make(name)
	if sks, ok := tx.openSetKeyspaces[handle]; ok {
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
	delete(tx.dirtyDescriptors, name)
	return tx.cacheOpenSetKeyspace(handle, desc, keyspaceStateClean), nil
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
func (tx *Tx) CreateSetKeyspace(name string, opts *SetKeyspaceOptions) (*SetKeyspace, error) {
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
	return tx.cacheOpenSetKeyspace(handle, desc, keyspaceStateCreated), nil
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
func (tx *Tx) CreateSetKeyspaceIfNotExists(name string, opts *SetKeyspaceOptions) (*SetKeyspace, error) {
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
	handle := unique.Make(name)
	if sks, ok := tx.openSetKeyspaces[handle]; ok {
		// Existing handle — Kind already matched at open time.
		// Verify FixedValueSize parity.
		if sks.desc.FixedValueSize != fvs {
			return nil, fmt.Errorf("%w: existing FixedValueSize=%d, opts.FixedValueSize=%d",
				ErrFixedValueSizeMismatch, sks.desc.FixedValueSize, fvs)
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
		return tx.cacheOpenSetKeyspace(handle, desc, keyspaceStateCreated), nil
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
		delete(tx.dirtyDescriptors, name)
		return tx.cacheOpenSetKeyspace(handle, desc, keyspaceStateClean), nil
	}
	desc = page.KeyspaceDescriptor{
		Kind:           page.KeyspaceKindSetKeyspace,
		FixedValueSize: fvs,
	}
	tx.numKeyspaces++
	return tx.cacheOpenSetKeyspace(handle, desc, keyspaceStateCreated), nil
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
	sks := &SetKeyspace{tx: tx, name: handle, desc: desc, state: state}
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
	if err := ks.tx.requireOpen(false); err != nil {
		return false, err
	}
	if ks.dead {
		return false, ErrKeyspaceClosed
	}
	if len(key) == 0 {
		return false, ErrKeyEmpty
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
	if err := ks.tx.requireOpen(false); err != nil {
		return false, err
	}
	if ks.dead {
		return false, ErrKeyspaceClosed
	}
	if len(key) == 0 {
		return false, ErrKeyEmpty
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
	if err := ks.tx.requireOpen(false); err != nil {
		return 0, err
	}
	if ks.dead {
		return 0, ErrKeyspaceClosed
	}
	if len(key) == 0 {
		return 0, ErrKeyEmpty
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
// tree via btree.Put; updates the parent cell's NestedCount.
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
	if err := ks.tx.requireOpen(true); err != nil {
		return false, err
	}
	if ks.dead {
		return false, ErrKeyspaceClosed
	}
	if len(key) == 0 {
		return false, ErrKeyEmpty
	}
	fvs := ks.desc.FixedValueSize
	if fvs != 0 && len(value) != int(fvs) {
		return false, fmt.Errorf("%w: value len %d, keyspace FixedValueSize %d", ErrValueSizeMismatch, len(value), fvs)
	}
	if value == nil {
		value = []byte{}
	}
	cfg := ks.builderCfg()

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
// Inserts the value into the nested tree via btree.Put; on
// duplicate returns (false, nil) without mutation. On success,
// updates the parent cell's NestedRoot + NestedCount.
func (ks *SetKeyspace) putIntoNestedTree(cfg page.Config, key, value []byte, e page.LeafEntry) (bool, error) {
	// Duplicate check via btree.Has BEFORE the Put.
	exists, err := btree.Has(ks.tx.pgr, cfg, e.NestedRoot, value)
	if err != nil {
		return false, mapBtreeErr(err)
	}
	if exists {
		return false, nil
	}
	newNestedRoot, err := btree.Put(ks.tx.pgr, cfg, e.NestedRoot, value, nil)
	if err != nil {
		return false, mapBtreeErr(err)
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
func (ks *SetKeyspace) Delete(key []byte) error {
	if err := ks.tx.requireOpen(true); err != nil {
		return err
	}
	if ks.dead {
		return ErrKeyspaceClosed
	}
	if len(key) == 0 {
		return ErrKeyEmpty
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
func (ks *SetKeyspace) DeleteValue(key, value []byte) error {
	if err := ks.tx.requireOpen(true); err != nil {
		return err
	}
	if ks.dead {
		return ErrKeyspaceClosed
	}
	if len(key) == 0 {
		return ErrKeyEmpty
	}
	fvs := ks.desc.FixedValueSize
	if fvs != 0 && len(value) != int(fvs) {
		return fmt.Errorf("%w: value len %d, keyspace FixedValueSize %d", ErrValueSizeMismatch, len(value), fvs)
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

// DeleteRange deletes every (key, value) pair whose KEY falls in
// [start, end) from the SetKeyspace. Returns the count of VALUES
// deleted (E2 accounting — desc.Count delta), NOT the count of keys.
// Returns (0, nil) for an empty range (start == end, start > end,
// nil/nil on an empty keyspace, or no keys matching).
//
// Boundary semantics (api-surface.md §SetKeyspace.DeleteRange):
//   - nil = open-boundary sentinel. nil start = "from the
//     beginning"; nil end = "through the last key"; (nil, nil) =
//     every key.
//   - Non-nil zero-length ([]byte{}) is rejected with ErrKeyEmpty.
//
// For a key with a nested-tree cell, bulk-frees the nested subtree
// via SetKeyspace.Delete (which uses chunk-6.5's FreeSubtree
// extension). For a key with a subpage cell, the cell + its
// inline subpage are removed via btree.Delete on the parent tree.
//
// Implementation strategy (v1): snapshot keys in [start, end) via
// a read cursor, then call SetKeyspace.Delete on each. Cost is
// O(K log N) (K = keys in range, N = total parent-tree size).
// The chunk-5.7 btree.DeleteRange three-phase algorithm is faster
// but does not free nested-tree subtrees per cell — adapting it to
// be SetKeyspace-aware is a perf-driven follow-up
// (docs/issues/setkeyspace-delete-range-bulk-walker.md).
//
// **Partial-progress semantic (chunk-6.8 user-locked, distinct
// from chunk-5.7 Keyspace.DeleteRange).** Chunk-5.7's atomic
// btree.DeleteRange returns (0, err) on failure with descriptor
// state untouched. Chunk-6.8's per-key Delete loop is NOT
// atomic: on error at iteration i, iterations 0..i-1 have
// already completed and their effects (desc.Count delta,
// desc.Root advance, sibling-cursor MarkStale, on-disk page
// retirements via the pager) ARE reflected in the in-memory
// state. The function returns (deleted_so_far, err) so the
// caller sees the actual scope of state change; Inv-1 / E1 / E2
// hold for each successful per-key Delete (state is
// consistent-but-partial). The only safe recovery is
// Tx.Rollback() — which restores the pre-tx state via the
// pager's bitmap snapshot. The future O(K+logN) bulk-walker
// rewrite (filed follow-up) will honor the same
// (deleted_so_far, err) contract.
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
//   - Every open SetCursor on this keyspace is MarkStale'd
//     (each per-key Delete call invalidates them).
//
// Indexed-keyspace fallback (chunk 7) is not yet implemented;
// chunk-6.8 operates on indexed-or-not Kind=1 keyspaces uniformly,
// matching the chunk-5.7 Keyspace.DeleteRange deferral.
func (ks *SetKeyspace) DeleteRange(start, end []byte) (uint64, error) {
	if err := ks.tx.requireOpen(true); err != nil {
		return 0, err
	}
	if ks.dead {
		return 0, ErrKeyspaceClosed
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
	cfg := ks.builderCfg()

	// Phase 1: snapshot keys in [start, end) via a read cursor.
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

	// Phase 2: per-key Delete. Each call computes the cell's value
	// count, bulk-frees the nested tree if applicable, removes the
	// parent cell, and decrements desc.Count by the right value
	// count. We accumulate the delta via the desc.Count snapshot
	// so partial-progress on error surfaces honestly to the caller
	// per the user-locked contract above.
	before := ks.desc.Count
	for _, k := range keys {
		if err := ks.Delete(k); err != nil {
			// Partial-progress error: iterations 0..i-1 have
			// completed; their effects on desc.Count + desc.Root
			// + sibling cursors + on-disk page retirements stand.
			// Return (deleted_so_far, err) so the caller observes
			// the real scope of state change. The only safe
			// recovery is Tx.Rollback().
			return before - ks.desc.Count, err
		}
	}
	after := ks.desc.Count
	// before - after is the total values freed across all per-key
	// Delete calls (each decremented by the cell's value count).
	return before - after, nil
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
