package gmdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/thegrumpylion/gmdb/internal/btree"
)

// Atomic index maintenance for chunk-7.6 Keyspace.Put / Delete /
// Cursor.Delete. Implements indexing.md §Write Path: Atomic Index
// Maintenance + §Unique Indexes.
//
// On-disk index-entry shape per indexing.md §Storage Layout:
//
//	Unique index:     key = encodeIndexKey(cols)
//	                  value = uvarint(len(pk)) || pk_bytes || encoded_covering
//	Non-unique index: key = encodeIndexKey(cols || pk)
//	                  value = encoded_covering (empty if no Covering)
//
// encoded_covering = encodeIndexKey(coverColumns) when the
// IndexDecl declares Covering; otherwise empty bytes.
// The uvarint(len(pk)) length prefix on the unique value
// delimits the PK from the optional covering blob — without it,
// the decoder cannot distinguish where pk_bytes ends and
// encoded_covering begins. Non-unique indexes carry the PK in the
// key, so no length prefix is needed in the value.

// indexEntryKey returns the on-disk index-tree key for a single
// extractor-produced IndexEntry on a Keyspace row (the PK is the
// row's key). For SetKeyspace at chunk 7.9 the pk argument is the
// compound `escape(setKey) || 0x00 0x01 || escape(setValue)`.
func indexEntryKey(entry IndexEntry, pk []byte, unique bool) []byte {
	if unique {
		return encodeIndexKey(entry.Cols)
	}
	// Append the PK as an extra "column" so it gets escaped +
	// terminated by encodeIndexKey, matching the spec grammar.
	withPK := make([][]byte, 0, len(entry.Cols)+1)
	withPK = append(withPK, entry.Cols...)
	withPK = append(withPK, pk)
	return encodeIndexKey(withPK)
}

// indexEntryValue returns the on-disk index-tree value for entry on
// a row whose PK is pk. Per the value-format godoc above:
//
//	Unique:     uvarint(len(pk)) || pk_bytes || encoded_covering
//	Non-unique: encoded_covering
//
// encoded_covering = encodeIndexKey(entry.Cover) when the IndexDecl
// declares Covering and the extractor produced Cover bytes;
// otherwise empty.
func indexEntryValue(entry IndexEntry, pk []byte, unique bool, hasCovering bool) []byte {
	var covering []byte
	if hasCovering && len(entry.Cover) > 0 {
		covering = encodeIndexKey(entry.Cover)
	}
	if unique {
		// uvarint(len(pk)) + pk + covering
		var lenBuf [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(lenBuf[:], uint64(len(pk)))
		out := make([]byte, 0, n+len(pk)+len(covering))
		out = append(out, lenBuf[:n]...)
		out = append(out, pk...)
		out = append(out, covering...)
		return out
	}
	// Non-unique: just the covering (empty if none).
	if covering == nil {
		return []byte{}
	}
	return covering
}

// decodeUniqueIndexValue unpacks the unique-index entry value
// produced by indexEntryValue (unique=true) into the row PK and
// the encoded covering bytes (which may be empty).
//
// Returns errIndexValueShort wrapped in ErrCorrupted at the
// caller's boundary on malformed input.
func decodeUniqueIndexValue(value []byte) (pk, encodedCovering []byte, err error) {
	pkLen, n := binary.Uvarint(value)
	if n <= 0 {
		return nil, nil, fmt.Errorf("%w: bad uvarint pk-length prefix", errIndexValueShort)
	}
	if uint64(len(value)-n) < pkLen {
		return nil, nil, fmt.Errorf("%w: pk length %d exceeds remaining %d bytes",
			errIndexValueShort, pkLen, len(value)-n)
	}
	pk = value[n : n+int(pkLen)]
	encodedCovering = value[n+int(pkLen):]
	return pk, encodedCovering, nil
}

// errIndexValueShort marks a malformed index entry value (truncated
// uvarint, pk-length past end). Wrapped in ErrCorrupted at the
// caller boundary; index entries are engine-internal so a
// malformed value signals on-disk corruption.
var errIndexValueShort = errors.New("index entry value malformed")

// extractEntriesAsKeySet runs the extractor and returns a
// map[string]IndexEntry keyed by the encoded index-tree key. The
// set semantic (not multiset) collapses duplicate IndexEntry
// outputs from a single extractor invocation per the chunk-7.1
// entailed invariant on atomic index maintenance.
//
// extractor==nil or returning nil/empty slice: returns nil (no
// entries; partial-index semantics).
func extractEntriesAsKeySet(decl *IndexDecl, key, value []byte) (map[string]IndexEntry, error) {
	if decl.Extract == nil {
		// Per indexing.md the Extract is required at OpenKeyspace
		// time (ErrIndexExtractorRequired); reaching here with a
		// nil Extract is an internal invariant violation.
		return nil, fmt.Errorf("%w: extractor nil for index %q at maintenance time",
			ErrCorrupted, decl.Name)
	}
	raw := decl.Extract(key, value)
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]IndexEntry, len(raw))
	for _, e := range raw {
		k := string(indexEntryKey(e, key, decl.Unique))
		// Set semantic: a second entry with the same encoded key
		// overwrites (the user's two extractor outputs share the
		// same on-disk slot — equivalent for non-unique; for
		// unique it's a candidate-set collision detected below).
		if existing, ok := out[k]; ok {
			if decl.Unique {
				// Candidate-set collision on a unique index.
				return nil, fmt.Errorf("%w: index %q candidate-set duplicate for key produced by extractor: %x (existing entry has Cols=%v)",
					ErrIndexUniqueViolation, decl.Name, []byte(k), existing.Cols)
			}
		}
		out[k] = e
	}
	return out, nil
}

// applyIndexMaintenanceOnPut wraps the per-index pre-Put/post-Put
// work for an indexed Keyspace. Caller has already validated the
// keyspace handle (not dead, not readOnly) and the key (non-empty).
//
// Sequence per indexing.md §Write Path:
//  1. For each index: extract(key, oldValue) → old entry set.
//  2. For each index: extract(key, newValue) → new entry set.
//  3. For each unique index: probe each new key against the index;
//     ErrIndexUniqueViolation on conflict (no row write happens).
//  4. For each index: apply deletes (old \ new).
//  5. For each index: apply inserts (new \ old).
//  6. Update pinnedIndex.root + .count in memory; registry entry
//     sync deferred to Tx.Commit's flushKeyspaces walk.
//
// Caller writes the row AFTER this function returns nil.
//
// Atomicity. Two layers cooperate to keep the helper + the subsequent
// row write all-or-nothing across Tx.Commit:
//
//   - In-memory pinned state (chunk-7.6 Round-1 H-2 fix): on any
//     failure during the sequence, pinnedIndex.root / .count are
//     restored to the pre-call snapshot so flushIndexRegistry at
//     Commit-after-error never writes half-mutated pinned values
//     to the on-disk registry.
//
//   - On-disk page allocations: the caller (Keyspace.Put / Delete /
//     Cursor.Delete) brackets the maintenance call AND its subsequent
//     row btree write in a Pager.BeginShallowSavepoint /
//     ReleaseSavepoint(success) / RestoreSavepoint(error) pair. The
//     savepoint's bitmap.Snapshot is an undo-log marker (free-space.md
//     / transactions.md §Nested Transactions cost contract —
//     O(this-window-flips), not O(MaxSize)), and the SHALLOW kind
//     preserves intra-tx loose-page recycling so per-row wrapping is
//     correct-by-design without growing the file O(N·depth) across
//     an indexed bulk workload: a mid-loop failure (or a row-btree-
//     mutation failure after the helper returned nil) frees every
//     in-flight index-data-tree page allocation regardless of
//     whether the caller eventually commits or rolls back.
//
// Together the two layers close free-space.md's entailed
// bitmap-consistency invariant ("every page below HighWaterMark
// with bit clear is reachable from the active meta, in the RPL,
// or is a meta/bitmap page") against the per-op-error path that
// the engine's rest-of-tx-continues contract allows.
func (ks *Keyspace) applyIndexMaintenanceOnPut(key, oldValue, newValue []byte, existedBefore bool) error {
	snap := snapshotIndexes(ks.indexes)
	if err := ks.applyIndexMaintenanceOnPutInner(key, oldValue, newValue, existedBefore); err != nil {
		restoreIndexes(ks.indexes, snap)
		return err
	}
	return nil
}

func (ks *Keyspace) applyIndexMaintenanceOnPutInner(key, oldValue, newValue []byte, existedBefore bool) error {
	if len(ks.indexes) == 0 {
		return nil
	}
	names := sortedIndexNames(ks.indexes)
	cfg := ks.tx.pgr.Config()
	mergeThreshold := ks.tx.db.opts.MergeThreshold

	// Steps 1-2: extract per-index old/new sets.
	type perIndex struct {
		p     *pinnedIndex
		olds  map[string]IndexEntry
		news  map[string]IndexEntry
		dels  []string // keys to delete (old \ new)
		ins   []string // keys to insert (new \ old)
	}
	plans := make([]*perIndex, 0, len(names))
	for _, name := range names {
		p := ks.indexes[name]
		var olds map[string]IndexEntry
		if existedBefore {
			var err error
			olds, err = extractEntriesAsKeySet(p.decl, key, oldValue)
			if err != nil {
				return err
			}
		}
		news, err := extractEntriesAsKeySet(p.decl, key, newValue)
		if err != nil {
			return err
		}
		// Diff: dels = olds \ news; ins = news \ olds.
		var dels, ins []string
		for k := range olds {
			if _, ok := news[k]; !ok {
				dels = append(dels, k)
			}
		}
		for k := range news {
			if _, ok := olds[k]; !ok {
				ins = append(ins, k)
			}
		}
		sort.Strings(dels)
		sort.Strings(ins)
		plans = append(plans, &perIndex{p: p, olds: olds, news: news, dels: dels, ins: ins})
	}

	// Step 3: unique-index probes (BEFORE any mutation).
	for _, pl := range plans {
		if !pl.p.decl.Unique {
			continue
		}
		for _, k := range pl.ins {
			// Skip if this insert's key was in olds — that's an
			// in-place same-PK overwrite (the row's key didn't
			// change), handled by the diff. Actually if k ∈ ins
			// then k ∉ olds by construction. Probe against the
			// on-disk index.
			if pl.p.root == 0 {
				continue // empty index, no possible conflict
			}
			_, found, err := btree.Get(ks.tx.pgr, cfg, pl.p.root, []byte(k))
			if err != nil {
				return mapBtreeErr(err)
			}
			if found {
				return fmt.Errorf("%w: index %q on keyspace %q: key %x",
					ErrIndexUniqueViolation, pl.p.decl.Name, ks.name.Value(), []byte(k))
			}
		}
	}

	// Steps 4-5 mutate index data trees. The opIdx counter is the
	// per-call monotonic index of successful btree.Put/Delete
	// operations exposed to indexMaintenanceFailHookForTest so
	// regression tests can deterministically inject a mid-loop
	// failure that exercises the caller's savepoint rollback.
	opIdx := 0

	// Step 4: apply deletes.
	for _, pl := range plans {
		for _, k := range pl.dels {
			if pl.p.root == 0 {
				// Stale old extractor output: index was empty when
				// we computed olds. This would be a logic error
				// since we only added k to olds because extract
				// emitted it. Surface as ErrCorrupted.
				return fmt.Errorf("%w: index %q: delete of %x but index root is 0",
					ErrCorrupted, pl.p.decl.Name, []byte(k))
			}
			newRoot, err := btree.Delete(ks.tx.pgr, cfg, pl.p.root, mergeThreshold, []byte(k))
			if err != nil {
				if errors.Is(err, btree.ErrNotFound) {
					// Stale old extractor output: extractor said this
					// key exists in the index for the old value, but
					// it's not actually there. Surface as ErrCorrupted
					// (row/index divergence — chunk-11 Check would
					// catch this normally).
					return fmt.Errorf("%w: index %q: delete of %x missed (row/index divergence)",
						ErrCorrupted, pl.p.decl.Name, []byte(k))
				}
				return mapBtreeErr(err)
			}
			pl.p.root = newRoot
			pl.p.count--
			if err := fireIndexMaintenanceFailHookForTest(opIdx); err != nil {
				return err
			}
			opIdx++
		}
	}

	// Step 5: apply inserts.
	for _, pl := range plans {
		hasCovering := len(pl.p.decl.Covering) > 0
		for _, k := range pl.ins {
			entry := pl.news[k]
			val := indexEntryValue(entry, key, pl.p.decl.Unique, hasCovering)
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

// applyIndexMaintenanceOnDelete wraps the per-index work for an
// indexed Keyspace.Delete or Cursor.Delete. Caller has the existing
// row's (key, value) and has validated the handle.
//
// Sequence per indexing.md §Write Path:
//  1. For each index: extract(key, oldValue) → old entry set.
//  2. For each index: delete every entry in the old set.
//  3. Decrement each affected index's count.
//
// The row delete itself happens AFTER this function returns nil.
//
// Atomicity (chunk-7.6 Round-1 H-2 / M-1 fix): on any failure,
// pinnedIndex state is restored from the pre-call snapshot — see
// applyIndexMaintenanceOnPut godoc.
func (ks *Keyspace) applyIndexMaintenanceOnDelete(key, oldValue []byte) error {
	snap := snapshotIndexes(ks.indexes)
	if err := ks.applyIndexMaintenanceOnDeleteInner(key, oldValue); err != nil {
		restoreIndexes(ks.indexes, snap)
		return err
	}
	return nil
}

func (ks *Keyspace) applyIndexMaintenanceOnDeleteInner(key, oldValue []byte) error {
	if len(ks.indexes) == 0 {
		return nil
	}
	names := sortedIndexNames(ks.indexes)
	cfg := ks.tx.pgr.Config()
	mergeThreshold := ks.tx.db.opts.MergeThreshold

	opIdx := 0
	for _, name := range names {
		p := ks.indexes[name]
		olds, err := extractEntriesAsKeySet(p.decl, key, oldValue)
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
				return fmt.Errorf("%w: index %q: delete of %x but index root is 0",
					ErrCorrupted, p.decl.Name, []byte(k))
			}
			newRoot, err := btree.Delete(ks.tx.pgr, cfg, p.root, mergeThreshold, []byte(k))
			if err != nil {
				if errors.Is(err, btree.ErrNotFound) {
					return fmt.Errorf("%w: index %q: delete of %x missed (row/index divergence)",
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

// sortedIndexNames returns the index names in lex order so the
// maintenance sequence is deterministic across runs (and so any
// surfaced error reports the same offending index regardless of
// map-iteration order).
func sortedIndexNames(indexes map[string]*pinnedIndex) []string {
	names := make([]string, 0, len(indexes))
	for name := range indexes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// flushIndexRegistry syncs each pinnedIndex's in-memory (root, count)
// back to the on-disk registry entry. Called by Tx.flushKeyspaces
// BEFORE the parent descriptor is written, so the descriptor's
// IndexRegistryRoot reflects the post-sync root of the registry
// sub-tree.
//
// No-op when ks has no indexes or none have been mutated since
// open. (Chunk 7.6 syncs all entries unconditionally — a perf
// optimization to track per-pinnedIndex dirty bits is deferred.)
func (tx *Tx) flushIndexRegistry(owner descriptorOwner, indexes map[string]*pinnedIndex) error {
	if len(indexes) == 0 {
		return nil
	}
	names := sortedIndexNames(indexes)
	for _, name := range names {
		p := indexes[name]
		entry := &indexRegistryEntry{
			SchemaHash:  p.schemaHash,
			Unique:      p.decl.Unique,
			Root:        p.root,
			Count:       p.count,
			UserVersion: p.decl.Version,
		}
		for _, c := range p.decl.Columns {
			entry.Columns = append(entry.Columns, c.Name)
		}
		for _, c := range p.decl.Covering {
			entry.Covering = append(entry.Covering, c.Name)
		}
		if err := tx.registryPut(owner, name, entry); err != nil {
			return fmt.Errorf("flushIndexRegistry %q: %w", name, err)
		}
	}
	return nil
}

// indexSnapshot is a per-index (root, count) pair captured before
// a mutation, used by the chunk-7.6 atomicity-rollback path
// (Round-1 H-2 fix). On any failure during the
// applyIndexMaintenanceOn{Put,Delete} sequence — including a
// failure of the row btree.Put/Delete that runs AFTER maintenance
// — the caller restores the snapshot to keep pinnedIndex's
// (root, count) consistent with the not-yet-written row, so
// flushIndexRegistry at Commit-after-error never writes
// partial-state pinned values to the on-disk registry. The
// newly-allocated index data-tree pages still leak under the
// engine's rest-of-tx-continues contract (Tx.Rollback reclaims;
// Commit-after-error orphans), but the registry stays consistent.
type indexSnapshot map[string]struct {
	root  uint64
	count uint64
}

func snapshotIndexes(indexes map[string]*pinnedIndex) indexSnapshot {
	if len(indexes) == 0 {
		return nil
	}
	out := make(indexSnapshot, len(indexes))
	for name, p := range indexes {
		out[name] = struct {
			root  uint64
			count uint64
		}{root: p.root, count: p.count}
	}
	return out
}

func restoreIndexes(indexes map[string]*pinnedIndex, snap indexSnapshot) {
	if snap == nil {
		return
	}
	for name, s := range snap {
		if p, ok := indexes[name]; ok {
			p.root = s.root
			p.count = s.count
		}
	}
}

// indexMaintenanceFailHookForTest, when set, is invoked after each
// successful btree.Put / btree.Delete on an index data tree inside
// applyIndexMaintenanceOn{Put,Delete} (Keyspace) and
// applyIndexMaintenanceOn{AddValue,RemoveValue} (SetKeyspace), with
// a monotonic per-call iteration index. A non-nil return aborts the
// loop with that error so a regression test can deterministically
// exercise the caller-site savepoint-backed rollback (the per-row
// case of the page-orphan-on-Commit-after-error contract). The hook
// signature mirrors writeRegistryFailHookForTest (chunk-7.5 sibling
// in index_open.go). Test-only; installed via
// setIndexMaintenanceFailHookForTest and cleared via t.Cleanup.
var indexMaintenanceFailHookForTest atomic.Pointer[func(i int) error]

func setIndexMaintenanceFailHookForTest(hook func(i int) error) {
	if hook == nil {
		indexMaintenanceFailHookForTest.Store(nil)
		return
	}
	indexMaintenanceFailHookForTest.Store(&hook)
}

// fireIndexMaintenanceFailHookForTest dispatches the hook (if
// installed) with the just-completed op index. Returns nil when no
// hook is set or the hook returns nil; otherwise the hook's error
// flows through the helper's failure path so the caller-site
// savepoint reverts the partial allocations.
func fireIndexMaintenanceFailHookForTest(i int) error {
	hook := indexMaintenanceFailHookForTest.Load()
	if hook == nil {
		return nil
	}
	return (*hook)(i)
}

