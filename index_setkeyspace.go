package gmdb

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// Compound-PK codec for SetKeyspace indexes (chunk 7.9), per
// set-keyspace.md §Indexes on SetKeyspaces.
//
// The "primary key" for a SetKeyspace index entry is the
// `(setKey, setValue)` pair — neither alone identifies the set
// member. The on-disk encoding:
//
//	escapedPK := escape(setKey) || 0x00 0x01 || escape(setValue)
//
// 0x00 0x01 is **lex-distinct** from the NUL-escape column
// terminator 0x00 0x00 and from the escape sequence 0x00 0xFF.
// Inside escape(setKey) and escape(setValue), every literal 0x00
// is already escaped to 0x00 0xFF — so the only raw 0x00 0x01 in
// the compound PK is the separator. (set-keyspace.md Inv-6,
// promoted to enforced tests at chunk 7.9.)
//
// The full SetKeyspace non-unique index key is:
//
//	indexKey := encodeIndexKey(cols) || escapedPK || 0x00 0x00
//
// For unique indexes the PK is in the value, not the key:
// indexKey := encodeIndexKey(cols), exactly as for Keyspace
// indexes. The unique value format remains uvarint(len(pk)) ||
// pk_bytes || encoded_covering — where pk_bytes is the FULL
// compound PK above.

// errCompoundPKMalformed marks a decode failure (no 0x00 0x01
// separator found, or the surrounding escapeColumn decode failed).
// Wrapped in ErrCorrupted at the caller's boundary.
var errCompoundPKMalformed = errors.New("SetKeyspace compound PK malformed")

// encodeSetKeyspaceCompoundPK builds the compound PK bytes for a
// SetKeyspace index entry: escape(setKey) || 0x00 0x01 ||
// escape(setValue). The result contains exactly one literal
// 0x00 0x01 sequence (the separator) and no other 0x00 0x01
// pattern (because escapeColumn turns every 0x00 in its input
// into 0x00 0xFF).
func encodeSetKeyspaceCompoundPK(setKey, setValue []byte) []byte {
	escapedKey := escapeColumn(setKey)
	escapedValue := escapeColumn(setValue)
	out := make([]byte, 0, len(escapedKey)+2+len(escapedValue))
	out = append(out, escapedKey...)
	out = append(out, 0x00, 0x01)
	out = append(out, escapedValue...)
	return out
}

// decodeSetKeyspaceCompoundPK reverses encodeSetKeyspaceCompoundPK.
// Splits on the first literal 0x00 0x01 sequence (the separator
// is unique within the compound by Inv-6: every 0x00 inside an
// escaped half is followed by 0xFF, never 0x01).
//
// Returns errCompoundPKMalformed wrapped in ErrCorrupted at the
// caller's boundary if no 0x00 0x01 separator is found OR if
// either escaped half fails to unescape.
func decodeSetKeyspaceCompoundPK(encoded []byte) (setKey, setValue []byte, err error) {
	// Scan for the first 0x00 0x01 — Inv-6 ensures this is the
	// separator, since every other 0x00 in the compound is part of
	// an 0x00 0xFF escape pair.
	for i := 0; i < len(encoded)-1; i++ {
		if encoded[i] == 0x00 && encoded[i+1] == 0x01 {
			// Found separator at offset i.
			setKey, err = unescapeColumn(encoded[:i])
			if err != nil {
				return nil, nil, fmt.Errorf("%w: setKey half: %w", errCompoundPKMalformed, err)
			}
			setValue, err = unescapeColumn(encoded[i+2:])
			if err != nil {
				return nil, nil, fmt.Errorf("%w: setValue half: %w", errCompoundPKMalformed, err)
			}
			return setKey, setValue, nil
		}
		// Skip past a 0x00 0xFF escape pair so we don't mis-parse
		// a 0x00 0xFF as separator-ish.
		if encoded[i] == 0x00 && encoded[i+1] == 0xFF {
			i++
		}
	}
	return nil, nil, fmt.Errorf("%w: no 0x00 0x01 separator found in %d-byte compound PK",
		errCompoundPKMalformed, len(encoded))
}

// encodeSetKeyspaceIndexKey assembles the full on-disk index key
// for a SetKeyspace non-unique index. For unique SetKeyspace
// indexes, the key is just encodeIndexKey(cols) — the compound
// PK goes in the value (uvarint-prefixed pk_bytes).
//
// For non-unique:
//
//	indexKey := encodeIndexKey(cols) || encodeSetKeyspaceCompoundPK(setKey, setValue) || 0x00 0x00
//
// The trailing 0x00 0x00 terminates the PK component, matching
// the spec grammar.
func encodeSetKeyspaceIndexKey(cols [][]byte, setKey, setValue []byte, unique bool) []byte {
	colBytes := encodeIndexKey(cols)
	if unique {
		return colBytes
	}
	compoundPK := encodeSetKeyspaceCompoundPK(setKey, setValue)
	out := make([]byte, 0, len(colBytes)+len(compoundPK)+2)
	out = append(out, colBytes...)
	out = append(out, compoundPK...)
	out = append(out, 0x00, 0x00)
	return out
}

// extractSetKeyspaceCompoundPKFromIndexKey extracts the compound
// PK (escapedPK bytes — still in escaped form, separator literal)
// from a non-unique SetKeyspace index key, given the number of
// columns the index declares.
//
// Walks the encoded key counting REAL 0x00 0x00 column
// terminators (skipping 0x00 0xFF escape pairs and 0x00 0x01
// separators). The Nth terminator marks the start of the
// escapedPK; everything up to (but not including) the (N+1)th
// terminator is the compound PK.
//
// Returns the escapedPK bytes (caller can then call
// decodeSetKeyspaceCompoundPK to split into setKey/setValue).
// Returns errCompoundPKMalformed if the key has fewer than N+1
// real terminators (malformed).
func extractSetKeyspaceCompoundPKFromIndexKey(indexKey []byte, numColumns int) ([]byte, error) {
	terminators := 0
	pkStart := -1
	for i := 0; i < len(indexKey)-1; i++ {
		if indexKey[i] != 0x00 {
			continue
		}
		next := indexKey[i+1]
		switch next {
		case 0xFF:
			// Escape pair; skip the 0xFF.
			i++
		case 0x01:
			// Separator inside the compound PK; not a terminator.
			i++
		case 0x00:
			// Real column terminator.
			terminators++
			if terminators == numColumns {
				// Next byte after this terminator is the start of
				// escapedPK.
				pkStart = i + 2
			} else if terminators == numColumns+1 {
				// This is the terminator AFTER the escapedPK.
				if pkStart < 0 {
					return nil, fmt.Errorf("%w: extra terminator before PK", errCompoundPKMalformed)
				}
				return indexKey[pkStart:i], nil
			}
			i++
		default:
			return nil, fmt.Errorf("%w: 0x00 followed by 0x%02x at offset %d (want 0x00, 0x01, or 0xFF)",
				errCompoundPKMalformed, next, i)
		}
	}
	return nil, fmt.Errorf("%w: index key has %d real terminators, want %d+1",
		errCompoundPKMalformed, terminators, numColumns)
}

// compoundPKHasPrefix reports whether a SetKeyspace index key
// begins with the supplied encodeIndexKey-encoded column prefix.
// Equivalent to bytes.HasPrefix but factored for readability at
// call sites (clarifies that the prefix is column-tuple bytes).
func compoundPKHasPrefix(indexKey, encodedColPrefix []byte) bool {
	return bytes.HasPrefix(indexKey, encodedColPrefix)
}

// setKeyspaceExtractEntries runs the extractor on (setKey,
// setValue) and dedupes into a key-set. Mirrors chunk-7.6's
// extractEntriesAsKeySet but uses the SetKeyspace-aware index
// entry key builder (encodeSetKeyspaceIndexKey with the compound
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
		k := string(encodeSetKeyspaceIndexKey(e.Cols, setKey, setValue, decl.Unique))
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
	names := sortedIndexNames(ks.indexes)
	cfg := ks.tx.pgr.Config()

	// Extract per-index new entry set.
	type perIndex struct {
		p   *pinnedIndex
		ins []string // encoded index keys to insert
		news map[string]IndexEntry
	}
	plans := make([]*perIndex, 0, len(names))
	for _, name := range names {
		p := ks.indexes[name]
		news, err := setKeyspaceExtractEntries(p.decl, setKey, setValue)
		if err != nil {
			return err
		}
		if len(news) == 0 {
			plans = append(plans, &perIndex{p: p})
			continue
		}
		ins := make([]string, 0, len(news))
		for k := range news {
			ins = append(ins, k)
		}
		sort.Strings(ins)
		plans = append(plans, &perIndex{p: p, ins: ins, news: news})
	}

	// Step 1: unique-index probes (against on-disk).
	for _, pl := range plans {
		if !pl.p.decl.Unique {
			continue
		}
		for _, k := range pl.ins {
			if pl.p.root == 0 {
				continue
			}
			_, found, err := btree.Get(ks.tx.pgr, cfg, pl.p.root, []byte(k))
			if err != nil {
				return mapBtreeErr(err)
			}
			if found {
				return fmt.Errorf("%w: index %q on SetKeyspace %q: key %x (setKey=%x setValue=%x)",
					ErrIndexUniqueViolation, pl.p.decl.Name, ks.name.Value(), []byte(k), setKey, setValue)
			}
		}
	}

	// Step 2: inserts. opIdx exposes per-btree.Put progress to
	// indexMaintenanceFailHookForTest for the regression test that
	// pins the caller-site savepoint rollback.
	compoundPK := encodeSetKeyspaceCompoundPK(setKey, setValue)
	opIdx := 0
	for _, pl := range plans {
		hasCovering := len(pl.p.decl.Covering) > 0
		for _, k := range pl.ins {
			entry := pl.news[k]
			val := indexEntryValue(entry, compoundPK, pl.p.decl.Unique, hasCovering)
			newRoot, err := btree.Put(ks.tx.pgr, cfg, pl.p.root, []byte(k), val)
			if err != nil {
				return mapBtreeErr(err)
			}
			pl.p.root = newRoot
			pl.p.count++
			if err := fireIndexMaintenanceFailHookForTest(opIdx); err != nil {
				return err
			}
			opIdx++
		}
	}
	return nil
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
		c := btree.NewCursor(ks.tx.pgr, cfg, e.NestedRoot, mergeThreshold)
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
	names := sortedIndexNames(ks.indexes)
	cfg := ks.tx.pgr.Config()
	mergeThreshold := ks.tx.db.opts.MergeThreshold

	opIdx := 0
	for _, name := range names {
		p := ks.indexes[name]
		olds, err := setKeyspaceExtractEntries(p.decl, setKey, setValue)
		if err != nil {
			return err
		}
		if len(olds) == 0 {
			continue
		}
		keys := make([]string, 0, len(olds))
		for k := range olds {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if p.root == 0 {
				return fmt.Errorf("%w: SetKeyspace index %q: delete of %x but root is 0",
					ErrCorrupted, p.decl.Name, []byte(k))
			}
			newRoot, err := btree.Delete(ks.tx.pgr, cfg, p.root, mergeThreshold, []byte(k))
			if err != nil {
				if errors.Is(err, btree.ErrNotFound) {
					return fmt.Errorf("%w: SetKeyspace index %q: delete of %x missed (row/index divergence)",
						ErrCorrupted, p.decl.Name, []byte(k))
				}
				return mapBtreeErr(err)
			}
			p.root = newRoot
			p.count--
			if err := fireIndexMaintenanceFailHookForTest(opIdx); err != nil {
				return err
			}
			opIdx++
		}
	}
	return nil
}
