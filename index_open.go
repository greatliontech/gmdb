package gmdb

import (
	"fmt"
	"sort"
	"sync/atomic"
)

// pinnedIndex carries the per-index state that survives the open-time
// validation walk: the user-supplied IndexDecl (for the pinned
// Extract function — first-Extract-wins per indexing.md §Re-opening)
// + the schema-hash computed once per open. registryEntry carries
// the on-disk state read from the parent's index registry: Root
// (the index data B+tree root) + Count (entries in the index).
//
// pinnedIndex is held on *Keyspace.indexes (keyed by IndexDecl.Name)
// and *SetKeyspace.indexes for the lifetime of the tx.
type pinnedIndex struct {
	// decl is the IndexDecl whose Extract is pinned for this tx.
	// First-Extract-wins: a second OpenKeyspace with structurally
	// identical IndexDecl but a different Extract function gets a
	// cached *Keyspace whose maintenance still uses this decl's
	// Extract.
	decl *IndexDecl

	// schemaHash is the chunk-7.2 schema-hash computed from decl
	// at open time. Cached so same-tx re-open comparison doesn't
	// re-hash.
	schemaHash uint64

	// root is the index data B+tree root page ID (the registry
	// entry's Root field). Empty index → root == 0. Updated by
	// chunk-7.6 atomic Put / chunk-7.8 RebuildIndex / chunk-7.8
	// DropIndex.
	root uint64

	// count is the number of entries in the index data tree
	// (registry entry's Count field). Updated by chunk-7.6.
	count uint64
}

// indexesEqualByHashableInputs reports whether two pinned-index maps
// are equivalent for the chunk-7.5 same-tx re-open idempotence check
// (indexing.md §Re-opening). Equality is by **hashable inputs only**:
// the set of index names, each index's schema-hash, each index's
// Version. The Extract function pointer is NOT a hashable input —
// per the spec "Go function values are not comparable, so the
// Extract function pointer is NOT part of the hashable-inputs
// comparison." Encoder IDs (typed indexes) land at chunk 9.
func indexesEqualByHashableInputs(a, b map[string]*pinnedIndex) bool {
	if len(a) != len(b) {
		return false
	}
	for name, pa := range a {
		pb, ok := b[name]
		if !ok {
			return false
		}
		if pa.schemaHash != pb.schemaHash {
			return false
		}
		if pa.decl.Version != pb.decl.Version {
			return false
		}
	}
	return true
}

// buildPinnedIndexMap precomputes the per-decl schemaHash so the
// caller can compare against the cached set or write to the
// registry in one pass. Returns an error if validateIndexDecls
// rejects the slice (duplicate Name, nil entry, empty Name).
func buildPinnedIndexMap(decls []*IndexDecl) (map[string]*pinnedIndex, error) {
	if err := validateIndexDecls(decls); err != nil {
		return nil, err
	}
	if len(decls) == 0 {
		return nil, nil
	}
	out := make(map[string]*pinnedIndex, len(decls))
	for _, d := range decls {
		out[d.Name] = &pinnedIndex{
			decl:       d,
			schemaHash: schemaHash(d),
		}
	}
	return out, nil
}

// validatePinnedAgainstRegistry compares the supplied pinned-index
// set against the on-disk registry stored under owner. Implements
// the chunk-7.5 open-time validation per indexing.md §Open
// Semantics:
//
//   - Each registry entry must have a matching supplied IndexDecl
//     (by Name); missing decl returns ErrIndexExtractorRequired
//     naming the registry's index.
//   - Each supplied IndexDecl must have a matching registry entry
//     (by Name); extra decl returns ErrIndexUnknown naming the
//     supplied IndexDecl.
//   - Matching pairs must agree on schemaHash and Version; drift
//     returns ErrIndexFingerprintMismatch wrapped in
//     *IndexFingerprintError naming Keyspace + IndexName + Field +
//     Stored* + Supplied* per the api-surface.md
//     §IndexFingerprintError contract.
//
// On success, pinned[name].root and pinned[name].count are
// populated from the on-disk registry entry. The caller's pinned
// map is mutated in place.
//
// Drift detection is alphabetical-by-IndexName to keep the surfaced
// error deterministic across runs (api-surface.md surfaces drift on
// the first mismatch encountered per the §Recovery pattern after
// ErrIndexFingerprintMismatch).
func (tx *Tx) validatePinnedAgainstRegistry(
	owner descriptorOwner,
	keyspaceName string,
	pinned map[string]*pinnedIndex,
) error {
	storedNames, err := tx.registryList(owner)
	if err != nil {
		return err
	}
	storedSet := make(map[string]struct{}, len(storedNames))
	for _, n := range storedNames {
		storedSet[n] = struct{}{}
	}

	// Extra decl check (supplied name not in registry).
	// Sort the pinned keys for deterministic error surfacing.
	pinnedNames := make([]string, 0, len(pinned))
	for name := range pinned {
		pinnedNames = append(pinnedNames, name)
	}
	sort.Strings(pinnedNames)
	for _, name := range pinnedNames {
		if _, ok := storedSet[name]; !ok {
			return fmt.Errorf("gmdb: IndexDecl %q on keyspace %q: %w",
				name, keyspaceName, ErrIndexUnknown)
		}
	}

	// Missing decl check (registry name not supplied).
	// Walk the alphabetical stored list so the surfaced error is
	// deterministic.
	for _, name := range storedNames {
		if _, ok := pinned[name]; !ok {
			return fmt.Errorf("gmdb: index %q on keyspace %q: %w",
				name, keyspaceName, ErrIndexExtractorRequired)
		}
	}

	// Fingerprint check + populate root/count for each pinned
	// index. Walk alphabetical so a multi-drift surfaces
	// deterministically (caller's RebuildIndex recovery loop per
	// indexing.md §Recovery pattern after
	// ErrIndexFingerprintMismatch iterates one at a time).
	for _, name := range storedNames {
		entry, err := tx.registryGet(owner, name)
		if err != nil {
			return err
		}
		p := pinned[name]
		if entry.SchemaHash != p.schemaHash {
			return &IndexFingerprintError{
				Keyspace:     keyspaceName,
				IndexName:    name,
				Field:        "schema-hash",
				StoredHash:   entry.SchemaHash,
				SuppliedHash: p.schemaHash,
			}
		}
		if entry.UserVersion != p.decl.Version {
			return &IndexFingerprintError{
				Keyspace:        keyspaceName,
				IndexName:       name,
				Field:           "version",
				StoredVersion:   entry.UserVersion,
				SuppliedVersion: p.decl.Version,
			}
		}
		// Fingerprints agree — populate runtime fields from the
		// on-disk entry.
		p.root = entry.Root
		p.count = entry.Count
	}
	return nil
}

// writeNewIndexRegistry writes a fresh registry entry per pinned
// index to the parent keyspace's registry sub-tree. Used by
// CreateKeyspace / CreateSetKeyspace at chunk-7.5: the parent
// descriptor is newly-created with IndexRegistryRoot=0, so the
// first registryPut allocates the registry sub-tree; subsequent
// entries grow it. Each new entry starts with Root=0 and Count=0
// (empty index data tree — populated by chunk-7.6 atomic Put as
// rows are written).
//
// The pinned-index map's root/count fields are left at zero (they
// already reflect the empty state).
func (tx *Tx) writeNewIndexRegistry(
	owner descriptorOwner,
	pinned map[string]*pinnedIndex,
) error {
	names := make([]string, 0, len(pinned))
	for name := range pinned {
		names = append(names, name)
	}
	sort.Strings(names)

	// Atomicity (transactions.md §Write-helper error contract): each
	// registryPut allocates pages and threads the new registry-tree root
	// through desc.IndexRegistryRoot. If iteration k+1 fails after 0..k
	// succeeded, iterations 0..k's pages are reachable only via the
	// descriptor the failing caller is about to drop from the open-
	// keyspace cache — Tx.Rollback's bitmap snapshot reclaims them, but
	// Tx.Commit (the rest-of-tx-continues path) would orphan them as a
	// bitmap leak. Bracket the loop in a savepoint so a mid-loop failure
	// frees every in-flight page and restores the descriptor's pre-loop
	// registry root: the helper is all-or-nothing regardless of whether
	// the caller commits or rolls back.
	desc := owner.descriptor()
	prevRegRoot := desc.IndexRegistryRoot
	sp := tx.pgr.BeginSavepoint()
	var loopErr error
	for i, name := range names {
		p := pinned[name]
		entry := &indexRegistryEntry{
			SchemaHash:  p.schemaHash,
			Unique:      p.decl.Unique,
			Root:        0, // empty index data tree
			Count:       0,
			UserVersion: p.decl.Version,
		}
		for _, c := range p.decl.Columns {
			entry.Columns = append(entry.Columns, c.Name)
		}
		for _, c := range p.decl.Covering {
			entry.Covering = append(entry.Covering, c.Name)
		}
		if loopErr = tx.registryPut(owner, name, entry); loopErr != nil {
			break
		}
		if hook := writeRegistryFailHookForTest.Load(); hook != nil {
			if loopErr = (*hook)(i); loopErr != nil {
				break
			}
		}
	}
	if loopErr != nil {
		tx.pgr.RestoreSavepoint(sp)
		desc.IndexRegistryRoot = prevRegRoot
		return loopErr
	}
	tx.pgr.ReleaseSavepoint(sp)
	return nil
}

// writeRegistryFailHookForTest, when set, is invoked after each
// successful registryPut in writeNewIndexRegistry with the iteration
// index; a non-nil return injects a failure that exercises the
// savepoint-backed rollback above. Test-only; installed via
// setWriteRegistryFailHookForTest and cleared via t.Cleanup.
var writeRegistryFailHookForTest atomic.Pointer[func(i int) error]

func setWriteRegistryFailHookForTest(hook func(i int) error) {
	if hook == nil {
		writeRegistryFailHookForTest.Store(nil)
		return
	}
	writeRegistryFailHookForTest.Store(&hook)
}

