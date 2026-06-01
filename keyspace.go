package gmdb

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"unique"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
	"github.com/thegrumpylion/gmdb/internal/pager"
)

// uniqueNameHandle is the interned form of a keyspace name. unique.Make
// (Go 1.23+) maps equal strings to equal handles per-process, so
// repeated lookups against the same name within a tx (or across txs)
// hit the same cache entry without per-call byte comparison. Per
// keyspaces.md §Keyspace Name Interning.
type uniqueNameHandle = unique.Handle[string]

// keyspaceState tracks a *Keyspace's pending-flush status within the
// owning write tx (deferred-flush refactor). The keyspace
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
// stays dead until the caller drops it (api-surface.md §Keyspace API DeleteKeyspace).
type Keyspace struct {
	keyspaceCore

	// openCursors tracks every *Cursor returned by Keyspace.Cursor()
	// in this tx so Put / Delete can MarkStale them — sibling
	// mutations to the keyspace's B+tree invalidate cursor state
	// because curKey / iter alias leaf-buffer slices that the
	// mutation may CoW or free, so a stale cursor must clear those
	// fields (see btree.Cursor.MarkStale) to avoid returning
	// dangling references on a subsequent unguarded access.
	openCursors []*Cursor
}

// OpenKeyspace opens an existing single-value keyspace (Kind=0) for
// read+write. Returns ErrNotFound if the named keyspace does not
// exist; ErrKeyspaceKindMismatch if the stored descriptor's Kind is 1
// (SetKeyspace — use OpenSetKeyspace); ErrKeyspaceReserved if the
// name resolves to an engine-internal keyspace (Kind=2); ErrCorrupted
// (wrapping the codec validate error) if the descriptor fails
// ValidateKeyspaceDescriptor.
//
// IndexDecl handling: every declared index on the
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
	// mutation.
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
// index data tree) — atomic Put populates entries as
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
			// still gets removed at Commit.
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
// Iteration uses a cursor against the keyspace B+tree's
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
// new root. The deferred-flush refactor moved every
// production caller to in-memory state + Tx.Commit's flushKeyspaces
// walk; storeDescriptor remains as an internal helper that
// keyspace-machinery tests (Kind-mismatch forging, Kind-reserved
// forging) use to inject descriptors that the public surface cannot
// produce.
func (tx *Tx) storeDescriptor(name string, desc page.KeyspaceDescriptor) error {
	buf := make([]byte, page.KeyspaceDescriptorSize)
	page.EncodeKeyspaceDescriptor(buf, desc)
	cfg := tx.pgr.Config()
	newRoot, err := btree.Put(btreeWriter{tx.pgr}, cfg, tx.keyspaceRoot, []byte(name), buf)
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
	ks := &Keyspace{keyspaceCore: keyspaceCore{tx: tx, name: handle, desc: desc, state: state}}
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

// Get returns the value stored under key in the keyspace. Returns
// (nil, ErrNotFound) if the key is absent. Returns ([]byte{}, nil)
// for a key whose stored value is empty. Per api-surface.md
// §Invariants, the returned slice is a borrowed reference valid
// until transaction close. ErrKeyEmpty if key is nil or empty.
// ErrKeyspaceClosed if this handle was invalidated by a same-tx
// DeleteKeyspace.
func (ks *Keyspace) Get(key []byte) ([]byte, error) {
	if err := ks.checkReadable(key); err != nil {
		return nil, err
	}
	ks.tx.pgr.RecordGet() // TxStats.Gets
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
// flushKeyspaces walk per the deferred-flush refactor):
//   - descriptor.Root is updated to the new btree root.
//   - descriptor.Count is incremented iff the key did not previously
//     exist in the keyspace.
//   - The handle's state transitions to Dirty (unless already Created,
//     which stays Created — both will write at flush).
//   - Every open Cursor on this keyspace is MarkStale'd.
func (ks *Keyspace) Put(key, value []byte) error {
	if err := ks.checkWritable(key); err != nil {
		return err
	}
	ks.tx.pgr.RecordPut() // TxStats.Puts
	cfg := ks.builderCfg()
	if value == nil {
		value = []byte{}
	}
	// Indexed-keyspace path: read the old value (if
	// any), apply per-index maintenance BEFORE the row write so a
	// unique-probe failure aborts cleanly without partial state.
	var oldValue []byte
	existed := false
	indexed := len(ks.indexes) > 0
	if ks.desc.Root != 0 && indexed {
		// Indexed keyspaces need the OLD value (not just existence) to
		// diff index entries, so they read it via btree.Get; existence
		// falls out of that read.
		v, found, err := btree.Get(ks.tx.pgr, cfg, ks.desc.Root, key)
		if err != nil {
			return mapBtreeErr(err)
		}
		existed = found
		if found {
			// Copy out of the (potentially mmap-borrowed) value
			// slice so the extractor's view stays valid across
			// subsequent btree mutations.
			oldValue = make([]byte, len(v))
			copy(oldValue, v)
		}
	}
	// For an un-indexed keyspace `existed` is not known yet — the single
	// btree.PutReportExisting descent below reports it, collapsing the
	// redundant btree.Has probe + btree.Put into one descent.
	// Index maintenance is a no-op with no indexes, so invoking it with
	// the provisional existed=false on that path is harmless.
	//
	// Two atomicity layers protect the indexed path against a per-op
	// error followed by Tx.Commit (the rest-of-tx-continues contract):
	//
	//   (a) rowSnap + restoreIndexes covers in-memory pinnedIndex.
	//       {root,count} so flushIndexRegistry at Commit-after-error
	//       does not write a half-mutated registry entry pointing at
	//       a row that was never written.
	//
	//   (b) BeginShallowSavepoint / ReleaseSavepoint(success) /
	//       RestoreSavepoint(error) covers on-disk page allocations
	//       the maintenance helper's btree.Put / btree.Delete (and a
	//       subsequent row btree.Put) made — without it those pages
	//       are unreachable from the active meta yet have bitmap bit
	//       clear on Commit-after-error (the engine's rest-of-tx-
	//       continues per-op-error path), violating free-space.md's
	//       entailed bitmap-consistency invariant ("every page below
	//       HighWaterMark with bit clear is reachable from the active
	//       meta, in the RPL, or is a meta/bitmap page"). The SHALLOW
	//       kind preserves intra-tx loose-page recycling so an indexed
	//       bulk-Put workload does not grow the file O(N·depth);
	//       nested-kind savepoint would suspend loose-pop across every
	//       Put and exhaust MaxSize for moderate batches.
	//
	// The un-indexed branch needs neither layer (no index pages
	// allocated; the row PutReportExisting is the only mutation, and
	// its own internal CoW failure mode is tracked separately).
	var sp *pager.Savepoint
	if indexed {
		sp = ks.tx.pgr.BeginShallowSavepoint()
	}
	rowSnap := snapshotIndexes(ks.indexes)
	if err := ks.applyIndexMaintenanceOnPut(key, oldValue, value, existed); err != nil {
		// The helper does not snapshot pinned state — see its godoc.
		// rowSnap is the sole atomicity-rollback for in-memory pinned
		// state, covering both this helper's failure and the row
		// btree.Put failure below.
		restoreIndexes(ks.indexes, rowSnap)
		if indexed {
			ks.tx.pgr.RestoreSavepoint(sp)
		}
		return err
	}
	var newRoot uint64
	var err error
	if indexed {
		newRoot, err = btree.Put(btreeWriter{ks.tx.pgr}, cfg, ks.desc.Root, key, value)
	} else {
		newRoot, existed, err = btree.PutReportExisting(btreeWriter{ks.tx.pgr}, cfg, ks.desc.Root, key, value)
	}
	if err != nil {
		restoreIndexes(ks.indexes, rowSnap)
		if indexed {
			ks.tx.pgr.RestoreSavepoint(sp)
		}
		return mapBtreeErr(err)
	}
	if indexed {
		ks.tx.pgr.ReleaseSavepoint(sp)
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
// ErrNotFound on miss). ErrKeyEmpty
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
	if err := ks.checkWritable(key); err != nil {
		return err
	}
	ks.tx.pgr.RecordDelete() // TxStats.Deletes
	if ks.desc.Root == 0 {
		return ErrNotFound
	}
	cfg := ks.builderCfg()
	mergeThreshold := ks.tx.db.opts.MergeThreshold
	// Indexed-keyspace path: fetch the old value first
	// so the extractor can compute the index entries to delete;
	// then apply index maintenance; then delete the row.
	//
	// Atomicity has two layers on the indexed path (see Keyspace.Put
	// godoc for the full rationale): (a) rowSnap + restoreIndexes for
	// in-memory pinnedIndex.{root,count}; (b) Pager.BeginShallowSavepoint
	// / Release|Restore for on-disk page allocations the maintenance
	// helper's btree.Delete (and the row btree.Delete below) made,
	// so a per-op error followed by Tx.Commit does not orphan the
	// in-flight allocations. The SHALLOW kind preserves intra-tx
	// loose-page recycling so an indexed bulk-Delete workload does
	// not grow the file O(N·depth).
	var rowSnap indexSnapshot
	var sp *pager.Savepoint
	indexed := len(ks.indexes) > 0
	if indexed {
		v, found, err := btree.Get(ks.tx.pgr, cfg, ks.desc.Root, key)
		if err != nil {
			return mapBtreeErr(err)
		}
		if !found {
			return ErrNotFound
		}
		oldValue := make([]byte, len(v))
		copy(oldValue, v)
		sp = ks.tx.pgr.BeginShallowSavepoint()
		rowSnap = snapshotIndexes(ks.indexes)
		if err := ks.applyIndexMaintenanceOnDelete(key, oldValue); err != nil {
			// The helper does not snapshot pinned state — see its godoc.
			// rowSnap is the sole atomicity-rollback for in-memory
			// pinned state, covering both this helper's failure and
			// the row btree.Delete failure below.
			restoreIndexes(ks.indexes, rowSnap)
			ks.tx.pgr.RestoreSavepoint(sp)
			return err
		}
	}
	newRoot, err := btree.Delete(btreeWriter{ks.tx.pgr}, cfg, ks.desc.Root, mergeThreshold, key)
	if err != nil {
		restoreIndexes(ks.indexes, rowSnap)
		if indexed {
			ks.tx.pgr.RestoreSavepoint(sp)
		}
		if errors.Is(err, btree.ErrNotFound) {
			return ErrNotFound
		}
		return mapBtreeErr(err)
	}
	if indexed {
		ks.tx.pgr.ReleaseSavepoint(sp)
	}
	ks.desc.Root = newRoot
	ks.desc.Count--
	ks.markDirty()
	ks.markCursorsStale()
	return nil
}

// DeleteRange deletes every key k with start <= k < end from the
// keyspace per range-delete.md §Algorithm. Returns the count of
// entries deleted; empty range (start == end OR start > end) returns
// (0, nil) without mutating the tree per the
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
// flushKeyspaces walk per the deferred-flush refactor):
//   - desc.Root reflects the new btree root (0 if the keyspace was
//     emptied).
//   - desc.Count decrements by the returned count.
//   - state transitions to Dirty unless already Created.
//   - Every open Cursor on this keyspace is MarkStale'd.
//
// Indexed-keyspace fallback is not yet implemented;
// DeleteRange operates on Kind=0 keyspaces only. The
// surface will reroute indexed keyspaces through a per-row
// cursor walk per range-delete.md §Indexed-keyspace fallback.
// keyspaceCellFree is the per-cell free callback Keyspace.DeleteRange
// passes to btree.DeleteRange. Per range-delete.md §Algorithm, Kind=0
// (plain key→value) cells carry at most one overflow chain and
// contribute exactly 1 to the values count — no subpage or nested-
// tree cases. Mirrors the prior in-place
// freeOverflowChainIfPresent + uint64(len(deleted)) shape; the count
// semantics are preserved exactly (count of cells == count of values
// for Kind=0).
func keyspaceCellFree(pw btree.PageWriter, cfg page.Config, e page.LeafEntry) (uint64, error) {
	if e.IsOverflow() {
		runLen := page.OverflowRunLength(cfg, e.TotalLen)
		if err := pw.FreeRun(e.OverflowPage, runLen); err != nil {
			return 0, fmt.Errorf("btree: keyspace DeleteRange free overflow chain at %d (run=%d): %w",
				e.OverflowPage, runLen, err)
		}
	}
	return 1, nil
}

func (ks *Keyspace) DeleteRange(start, end []byte) (uint64, error) {
	if err := ks.requireWritable(); err != nil {
		return 0, err
	}
	// Reject empty-but-non-nil bounds per the
	// empty-key policy + the DeleteRange
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
	// Indexed-keyspace fallback per range-delete.md §Indexed-keyspace
	// fallback: when the keyspace has declared indexes,
	// the O(pages) subtree-retirement fast path is unsafe because
	// the extractor needs each row's value to compute the prior
	// index keys. Cursor-walk + Cursor.Delete per row instead. Cost
	// is O(entries × (indexes + extractor)) — same as a SQL engine
	// with secondary indexes.
	if len(ks.indexes) > 0 {
		return ks.deleteRangeIndexed(start, end)
	}
	return ks.deleteRangeUnindexed(start, end, keyspaceCellFree, ks.markCursorsStale)
}

// deleteRangeIndexed is the indexed-keyspace fallback
// for Keyspace.DeleteRange per range-delete.md §Indexed-keyspace
// fallback. Walks the [start, end) range with a cursor, calling
// Cursor.Delete per row. Each Cursor.Delete invokes the
// atomic index maintenance to clear the row's index entries
// before removing the row, so post-condition: row + all its index
// entries are gone, atomically per row.
//
// Returns (count_deleted, err). On a per-row delete error, the
// loop stops and returns (count_so_far, err) — the
// SetKeyspace.DeleteRange partial-progress contract applies here
// too: the atomic contract of Keyspace.DeleteRange is
// replaced by per-row atomicity when indexes force the cursor
// walker.
func (ks *Keyspace) deleteRangeIndexed(start, end []byte) (uint64, error) {
	// Internal cursor — bypass ks.Cursor() registration in
	// ks.openCursors so repeated DeleteRange calls don't grow
	// the slice unboundedly. The
	// internal cursor is the sole mutator during this loop, so
	// it self-recovers via btree.Cursor.Delete's internal SeekGE
	// without needing the sibling-stale broadcast that
	// ks.markCursorsStale relies on.
	c := newInternalCursor(ks)
	var k []byte
	if start != nil {
		k, _ = c.SeekGE(start)
	} else {
		k, _ = c.First()
	}
	var count uint64
	for k != nil {
		if end != nil && bytes.Compare(k, end) >= 0 {
			break
		}
		if err := c.Delete(); err != nil {
			return count, err
		}
		count++
		// Cursor.Delete advanced the cursor to the next entry; read
		// via Current (Next would skip).
		k, _ = c.Current()
	}
	if err := c.Err(); err != nil {
		return count, err
	}
	return count, nil
}

// newInternalCursor returns a *Cursor on this keyspace WITHOUT
// registering in ks.openCursors. Used by internal
// helpers (deleteRangeIndexed) where the cursor's lifetime is
// scoped to a single helper call and registration would leak
// entries into the per-tx openCursors slice. The non-registered
// cursor doesn't receive markStale from sibling mutations — fine
// when the cursor itself is the only mutator during its lifetime.
func newInternalCursor(ks *Keyspace) *Cursor {
	return &Cursor{inner: ks.newRootCursor(), tx: ks.tx, ks: ks}
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
	c := &Cursor{inner: ks.newRootCursor(), tx: ks.tx, ks: ks}
	// Only register cursors on live handles. A dead keyspace's cursors
	// are rejected by requireOpen anyway (ErrKeyspaceClosed); appending
	// them would let a pathological caller (`for { ks.Cursor() }` after
	// DeleteKeyspace) grow openCursors unboundedly, since
	// markCursorsStale walks every entry on every sibling mutation.
	// Mirrors SetKeyspace.Cursor.
	if !ks.dead {
		ks.openCursors = append(ks.openCursors, c)
	}
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
//
// Also delegates to markIndexHandlesStale (Inv-IHS1): every site
// that stales row cursors here is post-mutation and, on the indexed
// path, has just CoW'd index trees via applyIndexMaintenanceOn{Put,
// Delete}; the in-flight *Index iter cursors must MarkStale or read
// CoW'd-then-released leaf pages. Centralized here so every existing
// markCursorsStale caller (Put / Delete / DeleteRange non-indexed
// fast path) gets the index-handle invalidation for free without
// re-touching each site. No-op on non-indexed keyspaces
// (openIndexHandles stays empty — Keyspace.Index rejects).
func (ks *Keyspace) markCursorsStale() {
	for _, c := range ks.openCursors {
		c.inner.MarkStale()
		c.inner.SetRootID(ks.desc.Root)
	}
	ks.markIndexHandlesStale()
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
//     with Tx.DeleteKeyspace and the Delete-on-miss
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
		// Existence still needs to be verified — the
		// contract requires ErrNotFound for a missing
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
		// ErrChildActive is transient — the parent-freeze lifts when the
		// active child resolves (transactions.md §Nested Transactions).
		// Do NOT stick it in closeErr, or a parent cursor merely touched
		// during the freeze would stay dead afterward. Terminal errors
		// (ErrTxClosed / ErrReadOnly / ErrClosed) still stick.
		if !errors.Is(err, ErrChildActive) {
			c.closeErr = err
		}
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
	// Indexed-keyspace path: apply per-index
	// maintenance BEFORE the row delete, using the cursor's
	// current (key, value). Copy out because c.inner.Delete may
	// CoW or free the underlying mmap-borrowed slices.
	//
	// Atomicity has two layers on the indexed path (see Keyspace.Put
	// godoc for the full rationale): rowSnap + restoreIndexes covers
	// in-memory pinnedIndex; BeginShallowSavepoint / Release|Restore
	// covers on-disk page allocations the maintenance helper's
	// btree.Delete (and the row c.inner.Delete below) made — so a
	// per-op error followed by Tx.Commit does not orphan the in-
	// flight allocations. The SHALLOW kind preserves intra-tx
	// loose-page recycling across the cursor-driven delete loop.
	var rowSnap indexSnapshot
	var sp *pager.Savepoint
	indexed := len(c.ks.indexes) > 0
	if indexed {
		curKey, curValue := c.inner.Current()
		if curKey == nil {
			// Distinguish stale-cursor from unpositioned per
			// transactions.md §Cursor State Machine — the inner
			// cursor's Err() reports ErrCursorStale when a sibling
			// mutation invalidated state; nil-from-Current with
			// no inner error is the Unpositioned state. Without
			// this branch, the indexed path would translate stale to
			// ErrCursorUnpositioned, regressing the
			// state machine contract that the non-indexed path
			// preserves via btree.ErrCursorStale at the
			// inner.Delete error path below.
			if errors.Is(c.inner.Err(), btree.ErrCursorStale) {
				return ErrCursorStale
			}
			return ErrCursorUnpositioned
		}
		keyCopy := make([]byte, len(curKey))
		copy(keyCopy, curKey)
		valueCopy := make([]byte, len(curValue))
		copy(valueCopy, curValue)
		sp = c.ks.tx.pgr.BeginShallowSavepoint()
		rowSnap = snapshotIndexes(c.ks.indexes)
		if err := c.ks.applyIndexMaintenanceOnDelete(keyCopy, valueCopy); err != nil {
			// The helper does not snapshot pinned state — see its godoc.
			// rowSnap is the sole atomicity-rollback for in-memory
			// pinned state, covering both this helper's failure and
			// the row inner.Delete failure below.
			restoreIndexes(c.ks.indexes, rowSnap)
			c.ks.tx.pgr.RestoreSavepoint(sp)
			return err
		}
	}
	if err := c.inner.Delete(); err != nil {
		// Atomicity: revert pinned state on row-write
		// failure so flushIndexRegistry doesn't commit partial-
		// state indexes pointing at a still-existing row.
		restoreIndexes(c.ks.indexes, rowSnap)
		if indexed {
			c.ks.tx.pgr.RestoreSavepoint(sp)
		}
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
	if indexed {
		c.ks.tx.pgr.ReleaseSavepoint(sp)
	}
	// Cursor.Delete mutated the keyspace's B+tree. Update the
	// in-memory descriptor (mirrors Keyspace.Delete's post-
	// conditions). The descriptor is persisted at Tx.Commit's
	// flushKeyspaces walk per the deferred-flush refactor.
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
	// Inv-IHS1: indexed Cursor.Delete ran applyIndexMaintenanceOnDelete
	// which CoW'd index trees. Stale every in-flight *Index iter cursor
	// on this keyspace. (Open-coded here rather than via the
	// markCursorsStale helper because Cursor.Delete sibling-stales only
	// OTHER row cursors — not all — so the row-cursor stale loop is
	// above; the index-handle path has no analogous self-recovery.)
	c.ks.markIndexHandlesStale()
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
	// Transient: report the parent-freeze without sticking it, so Err
	// clears once the active child resolves.
	if c.ks.tx.activeChild != nil {
		return ErrChildActive
	}
	if err := c.inner.Err(); err != nil {
		if errors.Is(err, btree.ErrCursorStale) {
			return ErrCursorStale
		}
		// Translate the internal Unpositioned sentinel to the public one:
		// the btree 3-state machine returns btree.ErrCursorUnpositioned for
		// the Unpositioned state (and nil at End-of-iteration), which is the
		// discriminator transactions.md §Cursor State Machine requires — but
		// callers errors.Is against gmdb.ErrCursorUnpositioned, so the
		// internal sentinel must not leak across the public boundary (same
		// reason ErrCursorStale is translated just above). End-of-iteration
		// stays nil because Err() only sees a non-nil value here.
		if errors.Is(err, btree.ErrCursorUnpositioned) {
			return ErrCursorUnpositioned
		}
		// mapBtreeErr covers btree.ErrCorrupted AND the pager sentinels
		// (ErrBadPageChecksum / ErrCorrupted) now reachable through a
		// cursor read via the verifying Page (checksums.md §Verification + checksums.md §Structural and Allocation Bounds); other errors
		// pass through unwrapped, preserving the prior behaviour.
		return mapBtreeErr(err)
	}
	return nil
}

// DeleteKeyspace removes a keyspace and bulk-frees its data B+tree
// per api-surface.md §Keyspace API. Chunk-5.6 implements the first
// of the three subtree-retirement steps documented in the spec:
//
//  1. The keyspace's own B+tree (this implementation).
//  2. Engine-internal index keyspaces.
//  3. The per-keyspace index registry sub-tree.
//
// SetKeyspace nested-tree retirement (set members promoted to nested
// B+trees per set-keyspace.md).
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
//     at commit) per free-space.md §Retired Page Log (RPL).
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
// api-surface.md §Keyspace API DeleteKeyspace (permanent-
// invalidation clause).
func (tx *Tx) DeleteKeyspace(name string) (retErr error) {
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
	cfg := tx.pgr.Config()

	// Three-subtree retirement per api-surface.md §Keyspace API
	// DeleteKeyspace (wires steps 2 + 3; step 1 already
	// existed):
	//
	//   1. Data subtree FreeSubtree.
	//   2. Per-index Kind=2 data tree FreeSubtree (walk registry).
	//   3. Index registry sub-tree FreeSubtree.
	//
	// All three happen in the same write tx; on Commit the meta
	// swap publishes the descriptor removal atomically.
	//
	// Atomicity wrap (transactions.md §Write-helper error contract):
	// any of the three FreeSubtree calls (step 1's data subtree, the
	// per-index data trees in step 2, or step 3's registry root) can
	// fail mid-walk, leaving the bitmap with some pages of the
	// keyspace's structures freed and others still allocated. The
	// cached *Keyspace's descriptor still points at the partially-
	// freed structures, and the in-memory invalidation (eviction +
	// pendingDeletes + numKeyspaces--) happens AFTER the retirement
	// returns successfully — so a Tx.Commit on the rest-of-tx-continues
	// path would publish a bitmap that contradicts a still-live
	// descriptor, violating free-space.md's bitmap-consistency
	// invariant (structurally similar to RebuildIndex / DropIndex's
	// partial-failure shapes; the consequence — a future tx that
	// re-allocates the bitmap-freed pages overwrites still-referenced
	// data — is the same overwrite hazard each of the four DDL sites
	// must avoid). Bracket the whole retirement in a nested savepoint
	// so a mid-walk failure restores every FreeSubtree, leaving the
	// keyspace structurally intact. No descriptor-field restore is
	// needed here: the local `desc` is a value-copy of the cached
	// descriptor, and the retirement does not mutate the cached
	// descriptor's fields. Defer-based dispatch (Restore on retErr !=
	// nil, Release on success) mirrors RebuildIndex / DropIndex and
	// is robust to future additions of early-return paths inside the
	// retirement window.
	sp := tx.pgr.BeginSavepoint()
	defer func() {
		if retErr != nil {
			tx.pgr.RestoreSavepoint(sp)
			return
		}
		tx.pgr.ReleaseSavepoint(sp)
	}()
	if _, err := btree.FreeSubtree(btreeWriter{tx.pgr}, cfg, desc.Root); err != nil {
		return fmt.Errorf("DeleteKeyspace %q: data subtree: %w", name, mapBtreeErr(err))
	}

	// Steps 2 + 3: index retirement. Skip when no registry exists.
	if desc.IndexRegistryRoot != 0 {
		if err := tx.retireIndexRegistry(name, desc.IndexRegistryRoot); err != nil {
			return err
		}
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
		// Inv-IHS3 (indexing.md §Handle Invalidation): MarkStale every
		// in-flight *btree.Cursor opened by an *Index iter closure on
		// this keyspace's handles. retireIndexRegistry above
		// FreeSubtree'd every declared index's data tree, so any
		// cursor still iterating idx.pinned.root would walk loose
		// (now-reusable) pages. The closure's mapCursorErr translates
		// the resulting btree.ErrCursorStale to ErrKeyspaceClosed
		// because keyspaceDead() is now true.
		existingKS.markIndexHandlesStale()
	}
	if existingSKS != nil {
		// Kind=1 partner: same dead-marking + cache eviction. No
		// openSetCursors walk is needed here: SetCursor's
		// requireOpen probes c.ks.dead at every entry and sticks
		// closeErr = ErrKeyspaceClosed before touching outerCursor,
		// so a freed-leaf dereference is impossible (set_cursor.go
		// requireOpen).
		delete(tx.openSetKeyspaces, handle)
		existingSKS.dead = true
		tx.deadSetKeyspaces = append(tx.deadSetKeyspaces, existingSKS)
		// Inv-IHS3 mirror on Kind=1: stale every in-flight *Index
		// iter cursor opened from sks-side handles, same rationale
		// as the Keyspace branch above. SetCursor's per-method
		// dead-check is insufficient here — the iter closure runs
		// btree.Cursor.Next directly without an outer ks.dead
		// probe, so the MarkStale is the canonical mechanism.
		existingSKS.markIndexHandlesStale()
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
// The btree read path resolves pages through pager.Page (the verifying
// accessor), so a read can now surface pager.ErrBadPageChecksum (a
// bitrotted footer, checksums.md §Verification) or pager.ErrCorrupted (a content-derived id
// past the file-resident extent, checksums.md §Structural and Allocation Bounds). Both are mapped to their
// public gmdb sentinels here so callers' errors.Is checks work.
//
// btree.ErrKeyTooLarge — a key too large even for an overflow-reference
// leaf entry (limits.md §Maximum Key Size) — is translated to the public
// gmdb.ErrKeyTooLarge sentinel here so a caller's
// errors.Is(err, ErrKeyTooLarge) works through Keyspace.Put / Delete /
// Get and (via the bulkLeafEntry → mapBtreeErr path) BulkLoad.
func mapBtreeErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, pager.ErrBadPageChecksum):
		return fmt.Errorf("%w: %v", ErrBadPageChecksum, err)
	case errors.Is(err, btree.ErrCorrupted), errors.Is(err, pager.ErrCorrupted):
		return fmt.Errorf("%w: %v", ErrCorrupted, err)
	case errors.Is(err, btree.ErrKeyTooLarge):
		return fmt.Errorf("%w: %v", ErrKeyTooLarge, err)
	}
	return err
}
