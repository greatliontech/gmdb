package gmdb

import (
	"errors"
	"fmt"
	"sort"
	"unique"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// uniqueNameHandle is the interned form of a keyspace name. unique.Make
// (Go 1.23+) maps equal strings to equal handles per-process, so
// repeated lookups against the same name within a tx (or across txs)
// hit the same cache entry without per-call byte comparison. Per
// keyspaces.md §Keyspace Name Interning.
type uniqueNameHandle = unique.Handle[string]

// keyspaceState tracks a *Keyspace's pending-flush status within the
// owning write tx (chunk-5.6 deferred-flush refactor). The keyspace
// descriptor's on-disk state propagates to the keyspace B+tree at
// Tx.Commit's flushKeyspaces walk — not per data-op.
type keyspaceState uint8

const (
	// keyspaceStateClean: descriptor matches the on-disk state for
	// this name; no flush needed unless a future mutation transitions
	// the state to dirty. Set by OpenKeyspace / OpenKeyspaceReadOnly
	// and by the flush walk itself after a successful btree.Put.
	keyspaceStateClean keyspaceState = iota

	// keyspaceStateCreated: the descriptor was created in this tx
	// (CreateKeyspace / CreateKeyspaceIfNotExists's create path) and
	// has never been persisted. The flush walk does btree.Put; the
	// DeleteKeyspace path skips adding the name to pendingDeletes
	// (there is nothing on disk to remove).
	keyspaceStateCreated

	// keyspaceStateDirty: the descriptor was loaded from disk but
	// has been mutated since (Keyspace.Put / Delete / Cursor.Delete /
	// SetKeyspaceConfig). The flush walk does btree.Put.
	keyspaceStateDirty
)

// Keyspace is a handle to a named single-value keyspace within a write
// transaction. Returned by Tx.OpenKeyspace / Tx.CreateKeyspace /
// Tx.CreateKeyspaceIfNotExists.
//
// A handle is valid for the lifetime of the owning transaction. Per
// api-surface.md §Keyspace API, DeleteKeyspace invalidates every
// handle previously opened on the named keyspace within the same
// tx; subsequent operations on those handles return
// ErrKeyspaceClosed. Re-creating the same name via CreateKeyspace
// in the same tx does NOT reactivate the old handle — the new
// CreateKeyspace returns a fresh *Keyspace while the old handle
// stays dead until the caller drops it (chunk-5.6 Inv-D).
type Keyspace struct {
	tx   *Tx
	name uniqueNameHandle

	// desc is the in-tx view of the keyspace's descriptor. Mutated
	// in place by Put / Delete data-op paths (descriptor.Root +
	// descriptor.Count). The chunk-5.6 deferred-flush refactor
	// promotes the in-memory desc to the on-disk keyspace B+tree at
	// Tx.Commit's flushKeyspaces walk — not per data op.
	desc page.KeyspaceDescriptor

	// state controls how Tx.Commit's flushKeyspaces walk treats this
	// handle. Created and Dirty cause a btree.Put on the keyspace
	// B+tree; Clean is skipped. See keyspaceState godocs.
	state keyspaceState

	// dead is set by Tx.DeleteKeyspace on every handle returned
	// against this name in this tx (the deleted *Keyspace itself
	// plus the openKeyspaces cache entry, which DeleteKeyspace
	// removes from the map AND adds to tx.deadKeyspaces with
	// dead=true). Once dead, every Keyspace/Cursor op returns
	// ErrKeyspaceClosed; re-creating the same name via
	// CreateKeyspace does NOT clear dead on the old handle (a fresh
	// *Keyspace is allocated; the old stays dead). Per
	// api-surface.md §Keyspace API DeleteKeyspace.
	dead bool

	// openCursors tracks every *Cursor returned by Keyspace.Cursor()
	// in this tx so Put / Delete can MarkStale them — sibling
	// mutations to the keyspace's B+tree invalidate cursor state
	// because curKey / iter alias leaf-buffer slices that the
	// mutation may CoW or free, so a stale cursor must clear those
	// fields (see btree.Cursor.MarkStale) to avoid returning
	// dangling references on a subsequent unguarded access.
	openCursors []*Cursor

	// indexes carries the pinned per-index state for this tx, keyed
	// by IndexDecl.Name. Populated by OpenKeyspace / CreateKeyspace
	// / CreateKeyspaceIfNotExists at chunk-7.5 with the validated
	// supplied IndexDecls. First-Extract-wins per indexing.md
	// §Re-opening: a same-tx second open with structurally identical
	// hashable inputs but a different Extract function silently keeps
	// the first call's Extract. Each pinnedIndex carries the user's
	// IndexDecl (for the Extract function), the schema-hash (cached
	// from chunk-7.2 schemaHash), and the index's data-tree
	// root+count (populated from the on-disk registry on Open,
	// initialized to 0 on Create, updated by chunk-7.6 atomic Put +
	// chunk-7.8 RebuildIndex/DropIndex).
	//
	// nil for keyspaces with no declared indexes.
	indexes map[string]*pinnedIndex

	// readOnly is true when this handle was opened via
	// OpenKeyspaceReadOnly. Used by the chunk-7.5 same-tx re-open
	// idempotence check to surface ErrKeyspaceAlreadyOpen when a
	// caller mixes OpenKeyspace and OpenKeyspaceReadOnly for the
	// same name within one tx per indexing.md §Re-opening.
	readOnly bool
}

// Name returns the keyspace's name (the unique-interned identity).
// Allocations: returns the underlying string from the interned handle
// without copying.
func (ks *Keyspace) Name() string { return ks.name.Value() }

// OpenKeyspace opens an existing single-value keyspace (Kind=0) for
// read+write. Returns ErrNotFound if the named keyspace does not
// exist; ErrKeyspaceKindMismatch if the stored descriptor's Kind is 1
// (SetKeyspace — use OpenSetKeyspace); ErrKeyspaceReserved if the
// name resolves to an engine-internal keyspace (Kind=2); ErrCorrupted
// (wrapping the codec validate error) if the descriptor fails
// ValidateKeyspaceDescriptor.
//
// IndexDecl handling (chunk 7.5): every declared index on the
// keyspace must be supplied with a matching IndexDecl. Missing
// decls return ErrIndexExtractorRequired; extras return
// ErrIndexUnknown; schema-hash or Version drift returns
// ErrIndexFingerprintMismatch wrapped in *IndexFingerprintError
// (indexing.md §Open Semantics + §Drift Guard).
//
// Same-tx re-open (indexing.md §Re-opening): a second OpenKeyspace
// for the same name returns the cached handle iff every hashable
// input matches the first call (names + Unique flags + schema
// hashes + Versions). Otherwise returns ErrKeyspaceAlreadyOpen.
// First-Extract-wins: structurally identical IndexDecls with
// different Extract functions yield the cached handle (the first
// call's Extract is pinned for the tx).
//
// Mixing OpenKeyspace and OpenKeyspaceReadOnly for the same name in
// one tx also returns ErrKeyspaceAlreadyOpen.
func (tx *Tx) OpenKeyspace(name string, indexes ...*IndexDecl) (*Keyspace, error) {
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
	if ks, ok := tx.openKeyspaces[handle]; ok {
		// Cache hit. Same-tx re-open: compare hashable inputs.
		if ks.readOnly {
			return nil, ErrKeyspaceAlreadyOpen
		}
		if !indexesEqualByHashableInputs(ks.indexes, pinned) {
			return nil, ErrKeyspaceAlreadyOpen
		}
		// First-Extract-wins: keep the pinned Extract from the
		// original open call; discard the second call's pinned set.
		return ks, nil
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
	if err := checkKeyspaceKind(desc.Kind, page.KeyspaceKindKeyspace); err != nil {
		return nil, err
	}
	// Cache the handle BEFORE validation so validatePinned* can
	// resolve `ks.descriptor()`, but defer the dirtyDescriptors
	// removal until validation succeeds — a fingerprint mismatch on
	// open of a name that has an in-flight SetKeyspaceConfig
	// mutation in dirtyDescriptors must not silently drop that
	// mutation (chunk-7.5 Round-1 M-2 fix).
	ks := tx.cacheOpenKeyspace(handle, desc, keyspaceStateClean)
	if err := tx.validatePinnedAgainstRegistry(ks, name, pinned); err != nil {
		delete(tx.openKeyspaces, handle)
		return nil, err
	}
	delete(tx.dirtyDescriptors, name)
	ks.indexes = pinned
	return ks, nil
}

// OpenKeyspaceReadOnly opens an existing Kind=0 keyspace for reads
// only. No IndexDecls are accepted (and none are required) per
// indexing.md §Open Semantics: index lookups still work on a
// read-only handle by reading stored index entries directly,
// without needing the extractor. Same-tx-mixed
// OpenKeyspace+OpenKeyspaceReadOnly on a single name returns
// ErrKeyspaceAlreadyOpen.
//
// The returned handle's mutating methods return ErrReadOnly when
// invoked on a read tx.
func (tx *Tx) OpenKeyspaceReadOnly(name string) (*Keyspace, error) {
	if err := tx.requireOpen(false); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, ErrKeyEmpty
	}
	handle := unique.Make(name)
	if ks, ok := tx.openKeyspaces[handle]; ok {
		if !ks.readOnly {
			return nil, ErrKeyspaceAlreadyOpen
		}
		return ks, nil
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
	if err := checkKeyspaceKind(desc.Kind, page.KeyspaceKindKeyspace); err != nil {
		return nil, err
	}
	ks := tx.cacheOpenKeyspace(handle, desc, keyspaceStateClean)
	ks.readOnly = true
	delete(tx.dirtyDescriptors, name)
	return ks, nil
}

// CreateKeyspace creates a new single-value keyspace (Kind=0) with
// the supplied IndexDecls. Returns ErrKeyExists if a keyspace with
// the supplied name already exists (use CreateKeyspaceIfNotExists
// for the open-or-create shape).
//
// The keyspace descriptor is created in memory with state=Created
// and persisted to the keyspace B+tree at Tx.Commit's flushKeyspaces
// walk. numKeyspaces is incremented eagerly so same-tx
// ListKeyspaces / NumKeyspaces reflect the new entry immediately.
//
// For each supplied IndexDecl, a fresh registry entry is written to
// the new keyspace's index registry sub-tree (allocated lazily on
// the first registryPut). Each entry starts with Root=0 (empty
// index data tree) — chunk-7.6 atomic Put populates entries as
// rows are written.
//
// Delete-then-Create in the same tx is permitted; any previously-
// opened *Keyspace handle for the same name stays dead per
// api-surface.md §Keyspace API DeleteKeyspace.
func (tx *Tx) CreateKeyspace(name string, indexes ...*IndexDecl) (*Keyspace, error) {
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
	if _, ok := tx.openKeyspaces[handle]; ok {
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
		Kind: page.KeyspaceKindKeyspace,
	}
	tx.numKeyspaces++
	ks := tx.cacheOpenKeyspace(handle, desc, keyspaceStateCreated)
	if len(pinned) > 0 {
		if err := tx.writeNewIndexRegistry(ks, pinned); err != nil {
			// Roll back the in-memory cache insertion. The
			// numKeyspaces++ was eager; symmetric decrement here.
			// If we cleared a pending-delete entry above (pendingDelete
			// was true), restore it so the original on-disk descriptor
			// still gets removed at Commit (chunk-7.5 Round-1 M-1 fix).
			delete(tx.openKeyspaces, handle)
			tx.numKeyspaces--
			if pendingDelete {
				tx.pendingDeletes[name] = struct{}{}
			}
			return nil, err
		}
	}
	ks.indexes = pinned
	return ks, nil
}

// CreateKeyspaceIfNotExists opens the keyspace if it exists (Kind=0
// check + supplied IndexDecls validated against the stored registry)
// or creates it with the supplied IndexDecls. Same-tx re-open
// idempotence applies on the open branch (indexing.md §Re-opening).
func (tx *Tx) CreateKeyspaceIfNotExists(name string, indexes ...*IndexDecl) (*Keyspace, error) {
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
	if ks, ok := tx.openKeyspaces[handle]; ok {
		if ks.readOnly {
			return nil, ErrKeyspaceAlreadyOpen
		}
		if !indexesEqualByHashableInputs(ks.indexes, pinned) {
			return nil, ErrKeyspaceAlreadyOpen
		}
		return ks, nil
	}
	if _, deleted := tx.pendingDeletes[name]; deleted {
		delete(tx.pendingDeletes, name)
		desc := page.KeyspaceDescriptor{Kind: page.KeyspaceKindKeyspace}
		tx.numKeyspaces++
		ks := tx.cacheOpenKeyspace(handle, desc, keyspaceStateCreated)
		if len(pinned) > 0 {
			if err := tx.writeNewIndexRegistry(ks, pinned); err != nil {
				// Restore pending-delete state (M-1 fix).
				delete(tx.openKeyspaces, handle)
				tx.numKeyspaces--
				tx.pendingDeletes[name] = struct{}{}
				return nil, err
			}
		}
		ks.indexes = pinned
		return ks, nil
	}
	desc, found, err := tx.lookupDescriptor(name)
	if err != nil {
		return nil, err
	}
	if found {
		if err := checkKeyspaceKind(desc.Kind, page.KeyspaceKindKeyspace); err != nil {
			return nil, err
		}
		ks := tx.cacheOpenKeyspace(handle, desc, keyspaceStateClean)
		if err := tx.validatePinnedAgainstRegistry(ks, name, pinned); err != nil {
			delete(tx.openKeyspaces, handle)
			return nil, err
		}
		delete(tx.dirtyDescriptors, name)
		ks.indexes = pinned
		return ks, nil
	}
	desc = page.KeyspaceDescriptor{Kind: page.KeyspaceKindKeyspace}
	tx.numKeyspaces++
	ks := tx.cacheOpenKeyspace(handle, desc, keyspaceStateCreated)
	if len(pinned) > 0 {
		if err := tx.writeNewIndexRegistry(ks, pinned); err != nil {
			delete(tx.openKeyspaces, handle)
			tx.numKeyspaces--
			return nil, err
		}
	}
	ks.indexes = pinned
	return ks, nil
}

// ListKeyspaces returns the names of all user keyspaces (Kind=0 or
// Kind=1). Engine-internal index keyspaces (Kind=2) are filtered out
// per keyspaces.md invariant #4 — they are addressable only via their
// parent keyspace's index registry, not by name. Names are returned
// in sorted byte order.
//
// Iteration uses a chunk-4 cursor against the keyspace B+tree's
// current in-tx root, then merges:
//   - in-memory openKeyspaces entries with state=Created (created in
//     this tx, not yet persisted),
//   - excludes any name in pendingDeletes (deleted in this tx),
//   - the on-disk entries surface their persisted Kind (so a forged
//     Kind=2 descriptor is filtered).
func (tx *Tx) ListKeyspaces() ([]string, error) {
	if err := tx.requireOpen(false); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	if tx.keyspaceRoot != 0 {
		cfg := tx.pgr.Config()
		c := btree.NewReadCursor(tx.pgr, cfg, tx.keyspaceRoot)
		for k, v := c.First(); k != nil; k, v = c.Next() {
			name := string(k)
			if _, deleted := tx.pendingDeletes[name]; deleted {
				continue
			}
			if len(v) != page.KeyspaceDescriptorSize {
				return nil, fmt.Errorf("%w: keyspace descriptor value length %d != %d",
					ErrCorrupted, len(v), page.KeyspaceDescriptorSize)
			}
			desc := page.DecodeKeyspaceDescriptor(v)
			if err := page.ValidateKeyspaceDescriptor(v, desc); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrCorrupted, err)
			}
			if desc.Kind == page.KeyspaceKindIndexInternal {
				continue
			}
			seen[name] = struct{}{}
		}
		if err := c.Err(); err != nil {
			return nil, mapBtreeErr(err)
		}
	}
	// Merge in created-this-tx names from openKeyspaces. dirty-state
	// entries are already on disk (their names show up in the cursor
	// walk above). Created entries are NOT on disk yet.
	for _, ks := range tx.openKeyspaces {
		if ks.state != keyspaceStateCreated {
			continue
		}
		if ks.desc.Kind == page.KeyspaceKindIndexInternal {
			continue
		}
		seen[ks.name.Value()] = struct{}{}
	}
	// Kind=1 partner: same-tx-created SetKeyspaces are also user-
	// visible and must surface in ListKeyspaces.
	for _, sks := range tx.openSetKeyspaces {
		if sks.state != keyspaceStateCreated {
			continue
		}
		// Kind=1 by construction; Kind=2 cannot reach this map.
		seen[sks.name.Value()] = struct{}{}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// lookupDescriptor resolves the effective descriptor for name in this
// tx. Lookup order: openKeyspaces cache (the *Keyspace.desc carries
// the latest in-memory state), then dirtyDescriptors (config-only
// mutations on uncached names), then the on-disk keyspace B+tree.
// pendingDeletes is NOT consulted — the caller decides what an
// in-flight delete means (OpenKeyspace returns ErrNotFound;
// CreateKeyspace clears the entry).
//
// Returns (desc, true, nil) on hit; (zero, false, nil) when the name
// is absent; (zero, false, err) on btree/codec failure.
func (tx *Tx) lookupDescriptor(name string) (page.KeyspaceDescriptor, bool, error) {
	handle := unique.Make(name)
	if ks, ok := tx.openKeyspaces[handle]; ok {
		return ks.desc, true, nil
	}
	if sks, ok := tx.openSetKeyspaces[handle]; ok {
		// Kind=1 partner: a same-tx-created SetKeyspace must
		// surface its descriptor here so a subsequent OpenKeyspace
		// on the same name observes Kind=1 and returns
		// ErrKeyspaceKindMismatch (rather than ErrNotFound, which
		// would suggest a never-seen name).
		return sks.desc, true, nil
	}
	if desc, ok := tx.dirtyDescriptors[name]; ok {
		return desc, true, nil
	}
	return tx.loadDescriptor(name)
}

// loadDescriptor reads the descriptor for name directly from the
// on-disk keyspace B+tree. Bypasses openKeyspaces / dirtyDescriptors /
// pendingDeletes — used by lookupDescriptor as the disk-tier fallback
// and by tests that need to inspect the persisted state regardless
// of in-flight mutations.
func (tx *Tx) loadDescriptor(name string) (page.KeyspaceDescriptor, bool, error) {
	if tx.keyspaceRoot == 0 {
		return page.KeyspaceDescriptor{}, false, nil
	}
	cfg := tx.pgr.Config()
	value, found, err := btree.Get(tx.pgr, cfg, tx.keyspaceRoot, []byte(name))
	if err != nil {
		return page.KeyspaceDescriptor{}, false, mapBtreeErr(err)
	}
	if !found {
		return page.KeyspaceDescriptor{}, false, nil
	}
	if len(value) != page.KeyspaceDescriptorSize {
		return page.KeyspaceDescriptor{}, false, fmt.Errorf("%w: keyspace descriptor value length %d != %d",
			ErrCorrupted, len(value), page.KeyspaceDescriptorSize)
	}
	desc := page.DecodeKeyspaceDescriptor(value)
	if err := page.ValidateKeyspaceDescriptor(value, desc); err != nil {
		return page.KeyspaceDescriptor{}, false, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	return desc, true, nil
}

// storeDescriptor encodes desc and writes it directly into the
// on-disk keyspace B+tree under name. Mutates tx.keyspaceRoot to the
// new root. The chunk-5.6 deferred-flush refactor moved every
// production caller to in-memory state + Tx.Commit's flushKeyspaces
// walk; storeDescriptor remains as an internal helper that
// keyspace-machinery tests (Kind-mismatch forging, Kind-reserved
// forging) use to inject descriptors that the public surface cannot
// produce.
func (tx *Tx) storeDescriptor(name string, desc page.KeyspaceDescriptor) error {
	buf := make([]byte, page.KeyspaceDescriptorSize)
	page.EncodeKeyspaceDescriptor(buf, desc)
	cfg := tx.pgr.Config()
	newRoot, err := btree.Put(tx.pgr, cfg, tx.keyspaceRoot, []byte(name), buf)
	if err != nil {
		return mapBtreeErr(err)
	}
	tx.keyspaceRoot = newRoot
	return nil
}

// cacheOpenKeyspace constructs the *Keyspace and registers it in the
// tx's per-name cache with the supplied initial state (Clean for
// Open paths, Created for Create paths). All Open and Create paths
// route through here.
func (tx *Tx) cacheOpenKeyspace(handle uniqueNameHandle, desc page.KeyspaceDescriptor, state keyspaceState) *Keyspace {
	ks := &Keyspace{tx: tx, name: handle, desc: desc, state: state}
	if tx.openKeyspaces == nil {
		tx.openKeyspaces = make(map[uniqueNameHandle]*Keyspace)
	}
	tx.openKeyspaces[handle] = ks
	return ks
}

// checkKeyspaceKind verifies the stored Kind matches the API used.
// Kind=2 (engine-internal) routes to ErrKeyspaceReserved per
// keyspaces.md invariant #4; mismatched user Kinds (Kind=0 vs
// Kind=1) route to ErrKeyspaceKindMismatch per invariant #3.
func checkKeyspaceKind(stored, requested uint8) error {
	if stored == requested {
		return nil
	}
	if stored == page.KeyspaceKindIndexInternal {
		return ErrKeyspaceReserved
	}
	return ErrKeyspaceKindMismatch
}

// builderCfg returns the page.Config to pass to btree.* calls for
// this keyspace. When the per-keyspace RestartGroupTarget is set
// (via SetKeyspaceConfig per keyspaces.md invariant #6), it
// overrides the engine default — newly written leaves use the
// per-keyspace target. Decoding ignores RestartGroupTarget so the
// override is safe on Get-side too.
func (ks *Keyspace) builderCfg() page.Config {
	cfg := ks.tx.pgr.Config()
	if ks.desc.RestartGroupTarget != 0 {
		cfg.RestartGroupTarget = ks.desc.RestartGroupTarget
	}
	return cfg
}

// Get returns the value stored under key in the keyspace. Returns
// (nil, ErrNotFound) if the key is absent. Returns ([]byte{}, nil)
// for a key whose stored value is empty. Per api-surface.md
// §Invariants, the returned slice is a borrowed reference valid
// until transaction close. ErrKeyEmpty if key is nil or empty.
// ErrKeyspaceClosed if this handle was invalidated by a same-tx
// DeleteKeyspace.
func (ks *Keyspace) Get(key []byte) ([]byte, error) {
	if err := ks.tx.requireOpen(false); err != nil {
		return nil, err
	}
	if ks.dead {
		return nil, ErrKeyspaceClosed
	}
	if len(key) == 0 {
		return nil, ErrKeyEmpty
	}
	if ks.desc.Root == 0 {
		return nil, ErrNotFound
	}
	cfg := ks.builderCfg()
	value, found, err := btree.Get(ks.tx.pgr, cfg, ks.desc.Root, key)
	if err != nil {
		return nil, mapBtreeErr(err)
	}
	if !found {
		return nil, ErrNotFound
	}
	return value, nil
}

// Put inserts or replaces (key, value) in the keyspace. nil values are
// treated as empty (api-surface.md §Invariants — nil-value-as-empty).
// ErrKeyEmpty if key is nil or empty. ErrKeyspaceClosed if the handle
// was invalidated by a same-tx DeleteKeyspace. Other errors map from
// the btree layer (ErrKeyTooLarge for oversize keys, ErrTxTooLarge
// for slab-budget exhaustion).
//
// Side effects on success (all in-memory; persisted at Tx.Commit's
// flushKeyspaces walk per chunk-5.6 deferred-flush refactor):
//   - descriptor.Root is updated to the new btree root.
//   - descriptor.Count is incremented iff the key did not previously
//     exist in the keyspace.
//   - The handle's state transitions to Dirty (unless already Created,
//     which stays Created — both will write at flush).
//   - Every open Cursor on this keyspace is MarkStale'd.
func (ks *Keyspace) Put(key, value []byte) error {
	if err := ks.tx.requireOpen(true); err != nil {
		return err
	}
	if ks.dead {
		return ErrKeyspaceClosed
	}
	if ks.readOnly {
		return ErrReadOnly
	}
	if len(key) == 0 {
		return ErrKeyEmpty
	}
	cfg := ks.builderCfg()
	if value == nil {
		value = []byte{}
	}
	existed := false
	if ks.desc.Root != 0 {
		exists, err := btree.Has(ks.tx.pgr, cfg, ks.desc.Root, key)
		if err != nil {
			return mapBtreeErr(err)
		}
		existed = exists
	}
	newRoot, err := btree.Put(ks.tx.pgr, cfg, ks.desc.Root, key, value)
	if err != nil {
		return mapBtreeErr(err)
	}
	ks.desc.Root = newRoot
	if !existed {
		ks.desc.Count++
	}
	ks.markDirty()
	ks.markCursorsStale()
	return nil
}

// Delete removes the entry under key. Returns ErrNotFound if the key
// does not exist (api-surface.md §Invariants — keyed-removal returns
// ErrNotFound on miss; chunk-5.1 user-locked decision). ErrKeyEmpty
// if key is nil or empty. ErrKeyspaceClosed if the handle was
// invalidated by a same-tx DeleteKeyspace.
//
// Side effects on success (all in-memory; persisted at flush):
//   - descriptor.Root reflects the new btree root (0 when the
//     keyspace is emptied).
//   - descriptor.Count is decremented.
//   - state transitions to Dirty unless already Created.
//   - Every open Cursor on this keyspace is MarkStale'd.
func (ks *Keyspace) Delete(key []byte) error {
	if err := ks.tx.requireOpen(true); err != nil {
		return err
	}
	if ks.dead {
		return ErrKeyspaceClosed
	}
	if ks.readOnly {
		return ErrReadOnly
	}
	if len(key) == 0 {
		return ErrKeyEmpty
	}
	if ks.desc.Root == 0 {
		return ErrNotFound
	}
	cfg := ks.builderCfg()
	mergeThreshold := ks.tx.db.opts.MergeThreshold
	newRoot, err := btree.Delete(ks.tx.pgr, cfg, ks.desc.Root, mergeThreshold, key)
	if err != nil {
		if errors.Is(err, btree.ErrNotFound) {
			return ErrNotFound
		}
		return mapBtreeErr(err)
	}
	ks.desc.Root = newRoot
	ks.desc.Count--
	ks.markDirty()
	ks.markCursorsStale()
	return nil
}

// markDirty transitions the handle's state to Dirty unless it is
// already Created (Created stays Created — both flush variants do
// btree.Put). Centralized so Put / Delete / Cursor.Delete /
// SetKeyspaceConfig route through one code path.
func (ks *Keyspace) markDirty() {
	if ks.state == keyspaceStateCreated {
		return
	}
	ks.state = keyspaceStateDirty
}

// descriptor returns the in-tx descriptor pointer. Used by the
// chunk-7.3 registry-CRUD helpers (index_codec.go) to satisfy the
// descriptorOwner interface — registryPut / registryDelete mutate
// the descriptor in place AND call markDirty() so the chunk-5.6
// flushKeyspaces walk persists the mutation. Unexported.
func (ks *Keyspace) descriptor() *page.KeyspaceDescriptor {
	return &ks.desc
}

// DeleteRange deletes every key k with start <= k < end from the
// keyspace per range-delete.md §Algorithm. Returns the count of
// entries deleted; empty range (start == end OR start > end) returns
// (0, nil) without mutating the tree per the chunk-5.1 user-locked
// "bulk operations report rows-affected, not membership" decision.
//
// Boundary semantics (range-delete.md §Invariants #1):
//   - nil start = open-left ("from the first key").
//   - nil end = open-right ("through the last key").
//   - (nil, nil) deletes every key.
//
// Errors:
//   - ErrKeyspaceClosed if the handle was invalidated by a same-tx
//     DeleteKeyspace.
//   - ErrTxClosed / ErrReadOnly via Tx.requireOpen.
//   - Pager errors (ErrTxTooLarge, ErrDBFull) pass through.
//   - ErrCorrupted (wrapped) on a structural anomaly observed by the
//     three-phase walk.
//
// Side effects on success (in-memory; persisted at Tx.Commit's
// flushKeyspaces walk per chunk-5.6 deferred-flush refactor):
//   - desc.Root reflects the new btree root (0 if the keyspace was
//     emptied).
//   - desc.Count decrements by the returned count.
//   - state transitions to Dirty unless already Created.
//   - Every open Cursor on this keyspace is MarkStale'd.
//
// Indexed-keyspace fallback (chunk 7) is not yet implemented;
// DeleteRange operates on Kind=0 keyspaces only at chunk 5.7. The
// chunk-7 surface will reroute indexed keyspaces through a per-row
// cursor walk per range-delete.md §Indexed-keyspace fallback.
func (ks *Keyspace) DeleteRange(start, end []byte) (uint64, error) {
	if err := ks.tx.requireOpen(true); err != nil {
		return 0, err
	}
	if ks.dead {
		return 0, ErrKeyspaceClosed
	}
	if ks.readOnly {
		return 0, ErrReadOnly
	}
	// Reject empty-but-non-nil bounds per the chunk-5.1 user-locked
	// empty-key policy + the chunk-5.7 spec-amend on DeleteRange
	// boundary semantics (nil = open, []byte{} = invalid). Treats
	// both bounds independently so the caller's malformed shape
	// surfaces at the originating arg.
	if start != nil && len(start) == 0 {
		return 0, ErrKeyEmpty
	}
	if end != nil && len(end) == 0 {
		return 0, ErrKeyEmpty
	}
	if ks.desc.Root == 0 {
		return 0, nil
	}
	cfg := ks.builderCfg()
	mergeThreshold := ks.tx.db.opts.MergeThreshold
	count, newRoot, err := btree.DeleteRange(ks.tx.pgr, cfg, ks.desc.Root, mergeThreshold, start, end)
	if err != nil {
		return 0, mapBtreeErr(err)
	}
	if count == 0 {
		// No-op — no Cursor invalidation, no state transition.
		return 0, nil
	}
	// Defense-in-depth: a count > desc.Count return from btree.DeleteRange
	// would indicate corruption (a divergence between the in-memory
	// Count and the on-disk leaf-entry count). Surface as ErrCorrupted
	// rather than wrapping desc.Count under uint64 arithmetic.
	if count > ks.desc.Count {
		return 0, fmt.Errorf("%w: DeleteRange count=%d exceeds desc.Count=%d for keyspace %q",
			ErrCorrupted, count, ks.desc.Count, ks.name.Value())
	}
	ks.desc.Root = newRoot
	ks.desc.Count -= count
	ks.markDirty()
	ks.markCursorsStale()
	return count, nil
}

// Cursor returns a new cursor for iterating over this keyspace's
// (key, value) pairs. The cursor starts Unpositioned — call First /
// Last / Seek / SeekGE before reading. Per transactions.md §Cursor
// State Machine.
//
// Calling Cursor() on a handle invalidated by a same-tx
// DeleteKeyspace is permitted; the returned cursor's methods
// (including Err()) all surface ErrKeyspaceClosed because the
// cursor's requireOpen and Err paths probe ks.dead before any
// underlying btree-cursor state.
//
// Sibling-mutation contract: Keyspace.Put / Delete on this keyspace
// MarkStale's every Cursor returned by this method that is still
// reachable; subsequent non-repositioning cursor ops surface
// ErrCursorStale. The caller re-positions via First / Last / Seek /
// SeekGE and continues.
func (ks *Keyspace) Cursor() *Cursor {
	cfg := ks.builderCfg()
	var inner *btree.Cursor
	if ks.tx.writable {
		inner = btree.NewCursor(ks.tx.pgr, cfg, ks.desc.Root, ks.tx.db.opts.MergeThreshold)
	} else {
		inner = btree.NewReadCursor(ks.tx.pgr, cfg, ks.desc.Root)
	}
	c := &Cursor{inner: inner, tx: ks.tx, ks: ks}
	ks.openCursors = append(ks.openCursors, c)
	return c
}

// markCursorsStale invokes MarkStale on every cursor registered on
// this keyspace AND refreshes their tracked rootID to the keyspace's
// current desc.Root. Called by Put / Delete / DeleteRange after a
// successful mutation. Stale cursors are not unregistered — the
// caller may re-position them via First/Last/Seek/SeekGE without
// needing a fresh Keyspace.Cursor() call; the rootID refresh
// guarantees the re-position descends from the live tree, not the
// pre-mutation (now-retired) root.
func (ks *Keyspace) markCursorsStale() {
	for _, c := range ks.openCursors {
		c.inner.MarkStale()
		c.inner.SetRootID(ks.desc.Root)
	}
}

// KeyspaceConfig is the mutable per-keyspace configuration surface
// for Tx.SetKeyspaceConfig.
type KeyspaceConfig struct {
	// RestartGroupTarget sets the leaf-builder restart-group target
	// for leaves written AFTER this call (existing leaves keep
	// their stored group structure per keyspaces.md invariant #6).
	// 0 = leave unchanged (the sentinel distinct from the on-disk
	// descriptor's "0 = engine default" semantic).
	// [1, 255] = the new builder hint.
	// > 255 returns ErrInvalidOptions.
	RestartGroupTarget uint16
}

// SetKeyspaceConfig updates mutable per-keyspace settings on the named
// keyspace. Kind-agnostic at the descriptor layer (the descriptor's
// RestartGroupTarget field lives independently of Kind). Returns:
//
//   - ErrNotFound if the named keyspace does not exist (consistent
//     with Tx.DeleteKeyspace and the chunk-5.1 Delete-on-miss
//     invariant family per api-surface.md §Invariants).
//   - ErrKeyEmpty for an empty name.
//   - ErrInvalidOptions when cfg.RestartGroupTarget > 255.
//
// cfg.RestartGroupTarget == 0 is the "leave unchanged" sentinel — the
// existing descriptor's RestartGroupTarget is preserved. A no-op call
// (every field zero) is harmless and does not write the descriptor.
func (tx *Tx) SetKeyspaceConfig(name string, cfg KeyspaceConfig) error {
	if err := tx.requireOpen(true); err != nil {
		return err
	}
	if name == "" {
		return ErrKeyEmpty
	}
	if cfg.RestartGroupTarget > page.MaxRestartGroupTarget {
		return fmt.Errorf("%w: RestartGroupTarget %d exceeds max %d",
			ErrInvalidOptions, cfg.RestartGroupTarget, page.MaxRestartGroupTarget)
	}
	if _, deleted := tx.pendingDeletes[name]; deleted {
		return ErrNotFound
	}
	// 0 = leave unchanged. No other fields are configurable today.
	if cfg.RestartGroupTarget == 0 {
		// Existence still needs to be verified — the user-locked
		// chunk-5.5 contract requires ErrNotFound for a missing
		// name even on a no-op call.
		_, found, err := tx.lookupDescriptor(name)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		return nil
	}
	handle := unique.Make(name)
	if ks, ok := tx.openKeyspaces[handle]; ok {
		if ks.dead {
			// Defensive — dead handles shouldn't appear in
			// openKeyspaces (DeleteKeyspace removes them) but guard
			// anyway so a future refactor can't silently mutate a
			// dead handle.
			return ErrNotFound
		}
		if ks.desc.RestartGroupTarget == cfg.RestartGroupTarget {
			return nil
		}
		ks.desc.RestartGroupTarget = cfg.RestartGroupTarget
		ks.markDirty()
		return nil
	}
	// Kind=1 partner of the Kind=0 cached-handle branch above. Per
	// keyspaces.md inv #6, RestartGroupTarget is kind-agnostic: the
	// descriptor field is mutable for any Kind via SetKeyspaceConfig.
	// Without this branch a same-tx CreateSetKeyspace +
	// SetKeyspaceConfig silently returns ErrNotFound (the cached
	// SetKeyspace's desc never gets updated, and the on-disk lookup
	// misses because the descriptor was never persisted).
	if sks, ok := tx.openSetKeyspaces[handle]; ok {
		if sks.dead {
			return ErrNotFound
		}
		if sks.desc.RestartGroupTarget == cfg.RestartGroupTarget {
			return nil
		}
		sks.desc.RestartGroupTarget = cfg.RestartGroupTarget
		sks.markDirty()
		return nil
	}
	if desc, ok := tx.dirtyDescriptors[name]; ok {
		if desc.RestartGroupTarget == cfg.RestartGroupTarget {
			return nil
		}
		desc.RestartGroupTarget = cfg.RestartGroupTarget
		tx.dirtyDescriptors[name] = desc
		return nil
	}
	desc, found, err := tx.loadDescriptor(name)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	if desc.RestartGroupTarget == cfg.RestartGroupTarget {
		return nil
	}
	desc.RestartGroupTarget = cfg.RestartGroupTarget
	if tx.dirtyDescriptors == nil {
		tx.dirtyDescriptors = make(map[string]page.KeyspaceDescriptor)
	}
	tx.dirtyDescriptors[name] = desc
	return nil
}

// Cursor is a public read-and-(when on a write tx)-mutate cursor over
// a single keyspace's (key, value) pairs. State-machine semantics per
// transactions.md §Cursor State Machine: starts Unpositioned;
// First/Last/Seek/SeekGE transition to Positioned or End-of-iteration;
// Next/Prev advance; Delete (on a write tx) removes the current entry
// and advances to the post-delete successor (or End-of-iteration).
//
// Stale handling: a sibling Put/Delete on the same keyspace
// invalidates this cursor — Current/Next/Prev/Delete surface
// ErrCursorStale until the caller re-positions.
//
// Tx lifecycle: every navigation/mutation method first checks that
// the parent tx is still open. After tx.Commit() or tx.Rollback() the
// cursor returns (nil, nil) and Err() reports ErrTxClosed — guarding
// against reads of pool-recycled slab buffers or post-Close munmap'd
// mmap pages.
type Cursor struct {
	inner    *btree.Cursor
	tx       *Tx
	ks       *Keyspace
	closeErr error
}

func (c *Cursor) requireOpen(needsWrite bool) bool {
	if c.closeErr != nil {
		return false
	}
	if err := c.tx.requireOpen(needsWrite); err != nil {
		c.closeErr = err
		return false
	}
	if c.ks.dead {
		c.closeErr = ErrKeyspaceClosed
		return false
	}
	if needsWrite && c.ks.readOnly {
		c.closeErr = ErrReadOnly
		return false
	}
	return true
}

// First positions the cursor at the leftmost entry. Returns
// (nil, nil) on an empty keyspace.
func (c *Cursor) First() (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	return c.inner.First()
}

// Last positions at the rightmost entry. Returns (nil, nil) on empty.
func (c *Cursor) Last() (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	return c.inner.Last()
}

// Next advances by one entry. Unpositioned ⇒ First. End-of-iteration
// ⇒ (nil, nil). Sets Err to ErrCursorStale when a sibling mutation
// has invalidated the cursor.
func (c *Cursor) Next() (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	return c.inner.Next()
}

// Prev steps backward by one entry. Unpositioned ⇒ Last.
func (c *Cursor) Prev() (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	return c.inner.Prev()
}

// Seek positions at the exact key. On miss returns (nil, nil) with
// End-of-iteration state and Err == nil.
func (c *Cursor) Seek(target []byte) (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	return c.inner.Seek(target)
}

// SeekGE positions at the smallest key >= target. Returns (nil, nil)
// when no such key exists.
func (c *Cursor) SeekGE(target []byte) (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	return c.inner.SeekGE(target)
}

// Current returns the current (key, value) without advancing.
// (nil, nil) at End-of-iteration or Unpositioned.
func (c *Cursor) Current() (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	return c.inner.Current()
}

// Delete removes the current entry. Cursor must be Positioned;
// otherwise returns ErrCursorUnpositioned. After delete, advances to
// the next entry or transitions to End-of-iteration. On a read-only
// transaction returns ErrReadOnly.
func (c *Cursor) Delete() error {
	if !c.requireOpen(true) {
		return c.closeErr
	}
	if err := c.inner.Delete(); err != nil {
		if errors.Is(err, btree.ErrCursorUnpositioned) {
			return ErrCursorUnpositioned
		}
		if errors.Is(err, btree.ErrReadOnly) {
			return ErrReadOnly
		}
		if errors.Is(err, btree.ErrCursorStale) {
			return ErrCursorStale
		}
		return mapBtreeErr(err)
	}
	// Cursor.Delete mutated the keyspace's B+tree. Update the
	// in-memory descriptor (mirrors Keyspace.Delete's post-
	// conditions). The descriptor is persisted at Tx.Commit's
	// flushKeyspaces walk per chunk-5.6 deferred-flush refactor.
	c.ks.desc.Root = c.inner.RootID()
	c.ks.desc.Count--
	c.ks.markDirty()
	// Mark stale on every OTHER cursor (this cursor self-recovered
	// via its internal SeekGE in btree.Cursor.Delete). Refresh
	// rootID alongside the MarkStale so a caller re-positioning
	// the sibling descends from the live tree.
	for _, sibling := range c.ks.openCursors {
		if sibling != c {
			sibling.inner.MarkStale()
			sibling.inner.SetRootID(c.ks.desc.Root)
		}
	}
	return nil
}

// Err returns the sticky error captured by the most recent op on this
// cursor. The sticky-error contract surfaces:
//
//   - ErrKeyspaceClosed when the parent keyspace was DeleteKeyspace'd
//     in this tx (api-surface.md §Keyspace API DeleteKeyspace
//     permanent-invalidation clause — every method on a dead handle
//     reports ErrKeyspaceClosed, including Err() called before any
//     nav op latches closeErr).
//   - ErrTxClosed when the parent tx has closed.
//   - ErrCursorStale on a sibling-mutation invalidation that the
//     caller has not yet recovered via First / Last / Seek / SeekGE.
//   - ErrCorrupted (wrapped) on a structural fault surfaced by the
//     underlying btree cursor.
func (c *Cursor) Err() error {
	if c.closeErr != nil {
		return c.closeErr
	}
	if c.ks.dead {
		return ErrKeyspaceClosed
	}
	if err := c.inner.Err(); err != nil {
		if errors.Is(err, btree.ErrCursorStale) {
			return ErrCursorStale
		}
		if errors.Is(err, btree.ErrCorrupted) {
			return fmt.Errorf("%w: %v", ErrCorrupted, err)
		}
		return err
	}
	return nil
}

// DeleteKeyspace removes a keyspace and bulk-frees its data B+tree
// per api-surface.md §Keyspace API. Chunk-5.6 implements the first
// of the three subtree-retirement steps documented in the spec:
//
//  1. The keyspace's own B+tree (this implementation).
//  2. Engine-internal index keyspaces — chunk 7.
//  3. The per-keyspace index registry sub-tree — chunk 7.
//
// SetKeyspace nested-tree retirement (set members promoted to nested
// B+trees per set-keyspace.md) lands at chunk 6.
//
// Errors:
//   - ErrKeyEmpty on an empty name.
//   - ErrNotFound when name does not resolve to any keyspace in this
//     tx (neither in openKeyspaces, dirtyDescriptors, nor on disk).
//   - ErrKeyspaceReserved when the resolved descriptor's Kind is 2
//     (engine-internal index keyspace, not user-deletable).
//
// Side effects on success (atomic on commit; aborted cleanly via
// AbortTx on Tx.Rollback or flush-walk failure):
//   - Every page reachable from desc.Root retires into loosePages
//     (same-tx allocations) or retiredPages (prior-tx pages, RPL'd
//     at commit) per chunk-5.6 Inv-B.
//   - The name is added to tx.pendingDeletes (or, when the *Keyspace
//     was Created in this tx, the entry is simply dropped — there is
//     no on-disk descriptor to btree.Delete at flush).
//   - The *Keyspace handle (if any) is removed from openKeyspaces,
//     added to tx.deadKeyspaces, and marked dead=true; every open
//     *Cursor on that handle observes ErrKeyspaceClosed on next op.
//   - numKeyspaces decrements.
//
// Re-creation in same tx: a subsequent CreateKeyspace with the same
// name allocates a fresh *Keyspace; the old handle stays dead per
// Inv-D (api-surface.md §Keyspace API DeleteKeyspace permanent-
// invalidation clause).
func (tx *Tx) DeleteKeyspace(name string) error {
	if err := tx.requireOpen(true); err != nil {
		return err
	}
	if name == "" {
		return ErrKeyEmpty
	}
	if _, deleted := tx.pendingDeletes[name]; deleted {
		// Already deleted in this tx.
		return ErrNotFound
	}
	handle := unique.Make(name)

	var (
		desc             page.KeyspaceDescriptor
		existingKS       *Keyspace
		existingSKS      *SetKeyspace
		needsBtreeDelete bool // true when an on-disk descriptor entry must be removed via btree.Delete at flush; false when the name lives only in-memory (state=Created)
	)
	if ks, ok := tx.openKeyspaces[handle]; ok && !ks.dead {
		existingKS = ks
		desc = ks.desc
		needsBtreeDelete = ks.state != keyspaceStateCreated
	} else if sks, ok := tx.openSetKeyspaces[handle]; ok && !sks.dead {
		// Kind=1 partner of the Kind=0 cached-handle branch above.
		// Same lifecycle: needsBtreeDelete iff the descriptor was
		// already persisted (state != Created).
		existingSKS = sks
		desc = sks.desc
		needsBtreeDelete = sks.state != keyspaceStateCreated
	} else if d, ok := tx.dirtyDescriptors[name]; ok {
		// dirtyDescriptors-only entries (from SetKeyspaceConfig on
		// an uncached name) reflect an on-disk descriptor with a
		// pending config change — the on-disk entry must still be
		// removed at flush.
		desc = d
		needsBtreeDelete = true
	} else {
		d, found, err := tx.loadDescriptor(name)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		desc = d
		needsBtreeDelete = true
	}
	if desc.Kind == page.KeyspaceKindIndexInternal {
		return ErrKeyspaceReserved
	}
	// Pre-flight assertion: chunk-5.6 implements only step 1 of the
	// three-subtree retirement (api-surface.md §Keyspace API
	// DeleteKeyspace). Index-keyspace + index-registry retire lands
	// at chunk 7. No chunk-5 path produces a non-zero
	// IndexRegistryRoot, so reaching this branch means a corrupted
	// disk surface or a forged descriptor. Check BEFORE the
	// FreeSubtree call so an error here does not leave the data
	// subtree half-retired (caller's Rollback / AbortTx cleans up
	// either way, but a Commit-after-ignoring-the-error path must
	// not publish a partial state).
	if desc.IndexRegistryRoot != 0 {
		return fmt.Errorf("%w: DeleteKeyspace %q: IndexRegistryRoot=%d is non-zero; chunk 7 implements index-registry retirement, this should be unreachable at chunk 5",
			ErrCorrupted, name, desc.IndexRegistryRoot)
	}

	// Bulk-free the data subtree. The chunk-5.6 scope is the Kind=0
	// case; the Kind=1 (SetKeyspace) nested-tree pages reachable via
	// promoted-set leaf entries are NOT walked here (they don't exist
	// in chunk 5 — the SetKeyspace surface lands at chunk 6 with its
	// own bulk-free extension). For Kind=1 descriptors that reach
	// here only via test-forging, the data subtree (if any) is a
	// plain B+tree from the data-tree's perspective; FreeSubtree
	// retires the top-level pages but cannot reach nested
	// SetKeyspace promotions. This is acceptable because no
	// non-forging path produces Kind=1 at chunk 5.
	cfg := tx.pgr.Config()
	if _, err := btree.FreeSubtree(tx.pgr, cfg, desc.Root); err != nil {
		return fmt.Errorf("DeleteKeyspace %q: %w", name, mapBtreeErr(err))
	}

	// Invalidate the in-memory state.
	if existingKS != nil {
		delete(tx.openKeyspaces, handle)
		existingKS.dead = true
		tx.deadKeyspaces = append(tx.deadKeyspaces, existingKS)
		// Mark every Cursor on this *Keyspace stale so a Cursor
		// method called from now on can short-circuit via the dead
		// check on requireOpen without dereferencing a freed leaf
		// buffer (the subtree has been retired into loose/retired
		// pages — same-tx reuse via AllocPage may re-issue the same
		// page IDs as new data on the keyspace-B+tree CoW path).
		for _, c := range existingKS.openCursors {
			c.inner.MarkStale()
		}
	}
	if existingSKS != nil {
		// Kind=1 partner: same dead-marking + cache eviction. No
		// open-cursor invalidation here yet — SetCursor lands at
		// chunk 6.7; that chunk wires the openSetCursors slice and
		// the markStale loop.
		delete(tx.openSetKeyspaces, handle)
		existingSKS.dead = true
		tx.deadSetKeyspaces = append(tx.deadSetKeyspaces, existingSKS)
	}
	delete(tx.dirtyDescriptors, name)

	if needsBtreeDelete {
		if tx.pendingDeletes == nil {
			tx.pendingDeletes = make(map[string]struct{})
		}
		tx.pendingDeletes[name] = struct{}{}
	}
	tx.numKeyspaces--
	return nil
}

// mapBtreeErr translates internal btree sentinels into public gmdb
// sentinels. btree.ErrCorrupted → ErrCorrupted; other btree errors
// pass through unwrapped.
//
// Chunk-5.4 caveat: btree.ErrKeyTooLarge is reachable through
// storeDescriptor if a future caller supplies a pathologically long
// keyspace name. Chunk 5.4 does not impose a name-length cap (api-
// surface.md is silent on it). Chunk 5.5's data-op surface should add
// the cap and the public ErrKeyTooLarge translation before exposing
// user-controlled keys via Keyspace.Put.
func mapBtreeErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, btree.ErrCorrupted) {
		return fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	return err
}
