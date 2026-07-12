package gmdb

import (
	"fmt"

	"github.com/greatliontech/gmdb/internal/indexing"

	"github.com/greatliontech/gmdb/internal/btree"
	"github.com/greatliontech/gmdb/internal/page"
)

// The compound-PK codec (EncodeSetCompoundPK / DecodeSetCompoundPK /
// EncodeSetEntryKey / ExtractSetCompoundPK) lives in
// internal/indexing (setpk.go), per set-keyspace.md §Indexes on
// SetKeyspaces; its sentinel is indexing.ErrCompoundPKMalformed.

// setKeyspaceExtractEntries runs the extractor on (setKey,
// setValue) and dedupes into a key-set. Mirrors
// extractEntriesAsKeySet but uses the SetKeyspace-aware index
// entry key builder (indexing.EncodeSetEntryKey with the compound
// PK appended for non-unique). Candidate-set collisions on unique
// indexes return ErrIndexUniqueViolation.
func setKeyspaceExtractEntries(decl *IndexDecl, setKey, setValue []byte) (map[string]IndexEntry, error) {
	if decl.Extract == nil {
		return nil, fmt.Errorf("%w: extractor nil for index %q at maintenance time",
			ErrCorrupted, decl.Name)
	}
	raw := decl.Extract(setKey, setValue)
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]IndexEntry, len(raw))
	for _, e := range raw {
		k := string(indexing.EncodeSetEntryKey(e.Cols, setKey, setValue, decl.Unique))
		if existing, ok := out[k]; ok {
			if decl.Unique {
				return nil, fmt.Errorf("%w: index %q candidate-set duplicate (setKey=%x setValue=%x existing Cols=%v)",
					ErrIndexUniqueViolation, decl.Name, setKey, setValue, existing.Cols)
			}
		}
		out[k] = e
	}
	return out, nil
}

// applyIndexMaintenanceOnAddValue is the SetKeyspace analogue of
// Keyspace.applyIndexMaintenanceOnPut. Called BEFORE the actual
// btree.Put when a NEW set member is being added (Put where
// (setKey, setValue) was not already in the set).
//
// Atomicity: the caller owns `rowSnap` and the pager savepoint — see
// applyIndexMaintenanceOnPut godoc for the full two-layer contract.
// This helper does NOT snapshot pinned state itself; a single caller-
// side `rowSnap` covers both this helper's error path and the
// subsequent dispatched btree mutation (genesis subpage, putIntoSubpage,
// putIntoNestedTree) failing.
func (ks *SetKeyspace) applyIndexMaintenanceOnAddValue(setKey, setValue []byte) error {
	if len(ks.indexes) == 0 {
		return nil
	}
	return ks.newIndexMaintainer(setKey, setValue).onReplace(nil, setValue, false)
}

// applyIndexMaintenanceOnBulkKeyDelete iterates every value in the
// key's set (subpage or nested-tree storage) and applies index
// maintenance per (setKey, setValue) pair. Used by
// SetKeyspace.Delete on an indexed keyspace where the bulk-free
// fast path is unsafe (the extractor needs to see every member's
// value to compute the prior index keys). Per indexing.md §Indexes
// on SetKeyspaces: "Bulk-free of a key's nested B+tree (via
// Delete(key)) reverts to a per-member walk when the SetKeyspace
// has indexes."
//
// Atomicity: this routine threads errors out of
// applyIndexMaintenanceOnRemoveValue per member; the outer
// SetKeyspace.Delete caller's `rowSnap` covers a mid-loop failure
// by restoring to pre-loop state (the per-member helper is
// snapshot-less by design — see its godoc).
func (ks *SetKeyspace) applyIndexMaintenanceOnBulkKeyDelete(cfg page.Config, key []byte, e page.LeafEntry) error {
	fvs := ks.desc.FixedValueSize
	switch {
	case e.IsSubpage():
		sp := page.NewSubpageReader(e.Value, fvs)
		var walkErr error
		sp.AllValues(func(value []byte) bool {
			// Copy because future iterations may invalidate.
			valueCopy := make([]byte, len(value))
			copy(valueCopy, value)
			if err := ks.applyIndexMaintenanceOnRemoveValue(key, valueCopy); err != nil {
				walkErr = err
				return false
			}
			return true
		})
		return walkErr
	case e.IsNestedTree():
		mergeThreshold := ks.tx.db.opts.MergeThreshold
		c := btree.NewCursor(btreeWriter{ks.tx.pgr}, cfg, e.NestedRoot, mergeThreshold)
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			// Nested-tree value is the key in the inner tree (per
			// set-keyspace.md §Storage Strategy: each value becomes
			// a key in the nested B+tree with empty values).
			valueCopy := make([]byte, len(k))
			copy(valueCopy, k)
			if err := ks.applyIndexMaintenanceOnRemoveValue(key, valueCopy); err != nil {
				return err
			}
		}
		if err := c.Err(); err != nil {
			return fmt.Errorf("SetKeyspace bulk-key index maintenance: nested-tree walk: %w",
				mapBtreeErr(err))
		}
		return nil
	default:
		return fmt.Errorf("%w: SetKeyspace bulk-key index maintenance: unexpected CellFlags 0x%x",
			ErrCorrupted, e.Flags)
	}
}

// applyIndexMaintenanceOnRemoveValue is the SetKeyspace analogue of
// Keyspace.applyIndexMaintenanceOnDelete. Called BEFORE the actual
// btree.Delete (or value-specific removal) for the given
// (setKey, setValue) pair.
//
// Atomicity: the caller owns `rowSnap` and the pager savepoint — see
// applyIndexMaintenanceOnPut godoc for the full two-layer contract.
// This helper does NOT snapshot pinned state itself; a single caller-
// side `rowSnap` covers both this helper's error path and the
// subsequent dispatched btree mutation failing. In the bulk-key
// delete case (applyIndexMaintenanceOnBulkKeyDelete loop), one outer
// `rowSnap` at SetKeyspace.Delete covers EVERY per-member call: a
// failure on member k leaves the caller to restore to pre-loop state,
// not per-iteration intermediate state.
func (ks *SetKeyspace) applyIndexMaintenanceOnRemoveValue(setKey, setValue []byte) error {
	if len(ks.indexes) == 0 {
		return nil
	}
	return ks.newIndexMaintainer(setKey, setValue).onDelete(setValue)
}
