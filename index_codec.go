package gmdb

import (
	"errors"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/descriptor"
	"github.com/thegrumpylion/gmdb/internal/indexing"
)

// descriptorOwner is the contract a *Keyspace or *SetKeyspace
// satisfies for the registry-CRUD helpers: read+mutate the parent
// keyspace's descriptor, AND transition the owning handle's
// flush-state so the flushKeyspaces walk persists the
// mutation at Commit. registryPut / registryDelete take this
// interface (not a raw *descriptor.Keyspace) so that calling
// the helper without also marking the parent dirty is structurally
// impossible — closes a silent-data-loss path (registry mutation
// without the parent dirty-mark never reaches Commit's flush).
//
// *Keyspace.markDirty (keyspace.go) and *SetKeyspace.markDirty
// (set_keyspace.go) both preserve a Created state (Created stays
// Created) so the descriptor still flushes via the Created arm of
// flushKeyspaces.
type descriptorOwner interface {
	descriptor() *descriptor.Keyspace
	markDirty()
}

// registryGet looks up the index registry entry for name in the
// keyspace owned by owner. Returns ErrIndexNotFound when
// IndexRegistryRoot == 0 (no indexes declared) or when the name is
// absent from the registry sub-tree. Wraps registry-entry decode
// failures with ErrCorrupted. Read-only — does NOT mutate the
// descriptor or transition the owner's flush state.
//
// Returns ErrKeyEmpty if name is "" (defense-in-depth against
// internal callers bypassing validateIndexDecls; matches the
// api-surface.md TxIndexes.Rebuild / Drop empty-
// IndexDecl.Name sentinel).
func (tx *Tx) registryGet(owner descriptorOwner, name string) (*indexing.RegistryEntry, error) {
	if name == "" {
		return nil, ErrKeyEmpty
	}
	desc := owner.descriptor()
	if desc.IndexRegistryRoot == 0 {
		return nil, ErrIndexNotFound
	}
	val, found, err := btree.Get(tx.pgr, tx.pgr.Config(), desc.IndexRegistryRoot, []byte(name))
	if err != nil {
		return nil, fmt.Errorf("registryGet %q: %w", name, mapBtreeErr(err))
	}
	if !found {
		return nil, ErrIndexNotFound
	}
	e, err := indexing.DecodeRegistryEntry(val)
	if err != nil {
		return nil, fmt.Errorf("%w: registryGet %q: %w", ErrCorrupted, name, err)
	}
	return e, nil
}

// registryPut writes entry under name in the keyspace's registry
// sub-tree, allocating the sub-tree on first index, bumping the
// descriptor's IndexRegistryRoot to the post-Put root, AND
// transitioning the owner's flush state via markDirty() so the
// flushKeyspaces walk persists the new root at Commit.
//
// Mutates owner.descriptor() and calls owner.markDirty() only on
// success — a btree.Put error leaves both untouched. Idempotent
// w.r.t. the same (name, entry) pair: a second Put with identical
// encoded bytes is a btree replace-in-place + a (state-preserving)
// markDirty.
//
// Returns ErrKeyEmpty if name is "".
func (tx *Tx) registryPut(owner descriptorOwner, name string, entry *indexing.RegistryEntry) error {
	if name == "" {
		return ErrKeyEmpty
	}
	encoded, err := indexing.EncodeRegistryEntry(entry)
	if err != nil {
		// The codec's field-bound sentinel maps to the public
		// options-validation class at this boundary (the fields come
		// from a user IndexDecl).
		if errors.Is(err, indexing.ErrFieldTooLarge) {
			return fmt.Errorf("%w: %w", ErrInvalidOptions, err)
		}
		return err
	}
	desc := owner.descriptor()
	newRoot, err := btree.Put(btreeWriter{tx.pgr}, tx.pgr.Config(), desc.IndexRegistryRoot, []byte(name), encoded)
	if err != nil {
		return fmt.Errorf("registryPut %q: %w", name, mapBtreeErr(err))
	}
	desc.IndexRegistryRoot = newRoot
	owner.markDirty()
	return nil
}

// registryDelete removes the index entry for name from the
// keyspace's registry sub-tree, bumping the descriptor's
// IndexRegistryRoot and transitioning the owner's flush state.
//
// If the deleted entry was the last (registry sub-tree shrinks to
// empty), btree.Delete returns newRoot == 0; IndexRegistryRoot then
// reflects the empty-registry canonical-at-zero representation per
// the indexing.md entailed invariant. The btree layer
// frees the (now-orphan) registry leaf via the pager's free list,
// so the "retire the registry sub-tree pages" portion of the
// invariant is structurally satisfied.
//
// Returns ErrIndexNotFound when IndexRegistryRoot == 0 or name is
// absent. Returns ErrKeyEmpty if name is "". Mutates descriptor +
// owner state only on success.
func (tx *Tx) registryDelete(owner descriptorOwner, name string) error {
	if name == "" {
		return ErrKeyEmpty
	}
	desc := owner.descriptor()
	if desc.IndexRegistryRoot == 0 {
		return ErrIndexNotFound
	}
	newRoot, err := btree.Delete(btreeWriter{tx.pgr}, tx.pgr.Config(), desc.IndexRegistryRoot,
		tx.db.opts.MergeThreshold, []byte(name))
	if err != nil {
		if errors.Is(err, btree.ErrNotFound) {
			return ErrIndexNotFound
		}
		return fmt.Errorf("registryDelete %q: %w", name, mapBtreeErr(err))
	}
	desc.IndexRegistryRoot = newRoot
	owner.markDirty()
	return nil
}

// registryList returns the index names declared on the keyspace
// owned by owner, in lex order. Returns nil for a keyspace with
// no declared indexes (IndexRegistryRoot == 0). Read-only.
//
// On a corrupt registry tree the result is bounded by construction: the
// cursor reads through the verifying pager.Page (checksums.md §Structural and Allocation Bounds, file-resident
// bound + branch validation), so it cannot yield more entries than the
// finite registry tree holds, and cur.Err() surfaces ErrCorrupted /
// ErrBadPageChecksum. No separate output cap is therefore needed — the
// allocation is bounded by the file. (decodeRegistryEntry bounds the
// per-entry count fields; see the forged-length bound, checksums.md §Structural and Allocation Bounds.)
func (tx *Tx) registryList(owner descriptorOwner) ([]string, error) {
	desc := owner.descriptor()
	if desc.IndexRegistryRoot == 0 {
		return nil, nil
	}
	cur := btree.NewCursor(btreeWriter{tx.pgr}, tx.pgr.Config(), desc.IndexRegistryRoot, tx.db.opts.MergeThreshold)
	var names []string
	k, _ := cur.First()
	for k != nil {
		names = append(names, string(k))
		k, _ = cur.Next()
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("registryList: %w", mapBtreeErr(err))
	}
	return names, nil
}
