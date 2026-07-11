package gmdb

import (
	"errors"
	"fmt"
	"sort"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/indexing"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// indexPlan is the per-index unit of work the maintenance engine
// computes once and then applies: the entries to delete from and
// insert into one index's data tree. `news` is the insert source —
// indexing.EntryValue reads the IndexEntry behind each key in `ins`.
type indexPlan struct {
	p    *pinnedIndex
	news map[string]IndexEntry // insert/update source: index key -> entry
	dels []string              // sorted index keys to delete (old \ new)
	ins  []string              // sorted index keys to insert (new \ old)
	// upds: sorted index keys present in BOTH sets whose stored value
	// (covering payload) changed — rewritten in place by applyInserts.
	// Skipped by the unique probe: the on-disk hit at such a key is
	// this row's own old entry, and the overwrite is benign. Only
	// covering-bearing indexes can populate this (without covering,
	// the value is a function of the unchanged key + row PK).
	upds []string
}

// indexMaintainer is the kind-agnostic secondary-index maintenance
// engine shared by Keyspace and SetKeyspace (indexing.md §Write Path:
// Atomic Index Maintenance). It owns the diff, the pre-mutation
// unique probe, and the delete/insert loops. The two keyspace kinds
// differ only in how an extractor IndexEntry maps to an on-disk index
// key, what primary-key bytes the index value carries, and the label
// used in error context — captured in the fields below and supplied by
// (*Keyspace).newIndexMaintainer / (*SetKeyspace).newIndexMaintainer.
//
// One maintainer is constructed per mutation. Callers invoke onReplace
// (Put / SetKeyspace add-of-a-new-member) or onDelete (Delete /
// SetKeyspace remove-member). The atomicity contract — the caller's
// rowSnap + Pager savepoint that make this engine plus the subsequent
// row write all-or-nothing across Commit — is documented on
// (*Keyspace).applyIndexMaintenanceOnPut, which every apply* wrapper
// references.
type indexMaintainer struct {
	tx             *Tx
	indexes        map[string]*pinnedIndex
	cfg            page.Config
	mergeThreshold uint8

	// extractKey is the first extractor operand (the row key for a
	// Keyspace; the set key for a SetKeyspace). onReplace/onDelete
	// supply the second operand (the value being added/removed).
	extractKey []byte
	// extractFn runs the extractor and dedupes its output into a
	// key-set under the kind's on-disk index-key encoding. It is the
	// existing free function (extractEntriesAsKeySet /
	// setKeyspaceExtractEntries), shared with the bulk-build and
	// Check paths, so candidate-set collision detection lives there.
	extractFn func(decl *IndexDecl, key, value []byte) (map[string]IndexEntry, error)
	// valuePK returns the primary-key bytes stored in each index value
	// (the row key for a Keyspace; the compound (setKey,setValue) PK
	// for a SetKeyspace). Lazy so the delete path never pays for it.
	valuePK func() []byte

	// Error context (no allocation unless an error actually fires).
	kind             string // "keyspace" | "SetKeyspace"
	ksName           string
	setKey, setValue []byte // SetKeyspace operands for probe context; nil for Keyspace
}

// indexLabel is the keyspace-kind-qualified noun for ErrCorrupted
// row/index-divergence messages.
func (m *indexMaintainer) indexLabel() string {
	if m.kind == "SetKeyspace" {
		return "SetKeyspace index"
	}
	return "index"
}

// opSuffix is the trailing operand context for a unique-violation
// message: empty for a Keyspace (the key already identifies the row),
// the (setKey,setValue) pair for a SetKeyspace (whose unique-index key
// is just the column tuple — the PK lives in the value).
func (m *indexMaintainer) opSuffix() string {
	if m.kind == "SetKeyspace" {
		return fmt.Sprintf(" (setKey=%x setValue=%x)", m.setKey, m.setValue)
	}
	return ""
}

func (m *indexMaintainer) extract(decl *IndexDecl, value []byte) (map[string]IndexEntry, error) {
	return m.extractFn(decl, m.extractKey, value)
}

// onReplace runs full maintenance for a value replacement (Keyspace
// Put; SetKeyspace add-of-a-new-member when hadOld is false): diff the
// old and new extractor output per index, probe uniques, then apply
// deletes followed by inserts (indexing.md §Write Path, Put steps
// 2-7). hadOld=false skips the old-value extract — the insert-only
// case (a genesis Put with no prior row, or a fresh set member).
func (m *indexMaintainer) onReplace(oldValue, newValue []byte, hadOld bool) error {
	if len(m.indexes) == 0 {
		return nil
	}
	plans, err := m.buildReplacePlans(oldValue, newValue, hadOld)
	if err != nil {
		return err
	}
	if err := m.probeUnique(plans); err != nil {
		return err
	}
	// opIdx is the per-call monotonic index of successful
	// btree.Put/Delete operations exposed to the maintenance fail hook
	// so regression tests can deterministically inject a mid-loop
	// failure that exercises the caller's savepoint rollback. Deletes
	// precede inserts and share the counter.
	opIdx := 0
	if err := m.applyDeletes(plans, &opIdx); err != nil {
		return err
	}
	return m.applyInserts(plans, &opIdx)
}

// onDelete runs delete-only maintenance (Keyspace Delete /
// Cursor.Delete; SetKeyspace remove-member): extract the removed
// value's entries per index and delete each from its index tree
// (indexing.md §Write Path, Delete steps 2-3). No unique probe — a
// delete cannot introduce a duplicate.
func (m *indexMaintainer) onDelete(value []byte) error {
	if len(m.indexes) == 0 {
		return nil
	}
	// Extract EVERY index's entries BEFORE any mutation — the
	// onReplace shape (buildReplacePlans): extractors panic by design
	// (the typed layer, and user extractors may), and interleaving
	// extraction with per-index deletion would let a panic while
	// extracting for index "b" escape AFTER index "a"'s entries were
	// already deleted — committed-visible partial index state if a
	// recovering caller commits (indexing.md §Write Path).
	names := sortedIndexNames(m.indexes)
	perIndex := make([][]string, len(names))
	for i, name := range names {
		p := m.indexes[name]
		olds, err := m.extract(p.decl, value)
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
		perIndex[i] = keys
	}
	opIdx := 0
	for i, name := range names {
		if len(perIndex[i]) == 0 {
			continue
		}
		if err := m.deleteKeys(m.indexes[name], perIndex[i], &opIdx); err != nil {
			return err
		}
	}
	return nil
}

// buildReplacePlans extracts old/new entry sets per index and diffs
// them into delete/insert key lists. hadOld=false leaves olds empty
// (every new entry is an insert, nothing to delete).
func (m *indexMaintainer) buildReplacePlans(oldValue, newValue []byte, hadOld bool) ([]indexPlan, error) {
	names := sortedIndexNames(m.indexes)
	plans := make([]indexPlan, 0, len(names))
	for _, name := range names {
		p := m.indexes[name]
		var olds map[string]IndexEntry
		if hadOld {
			var err error
			olds, err = m.extract(p.decl, oldValue)
			if err != nil {
				return nil, err
			}
		}
		news, err := m.extract(p.decl, newValue)
		if err != nil {
			return nil, err
		}
		// Key-unchanged entries can still need a value rewrite: the
		// covering payload is extracted from the ROW VALUE, which
		// this operation is replacing (indexing.md §Covering
		// Indexes) — indexing.DiffEntrySets owns the diff + that
		// value comparison (lazy PK).
		hasCovering := len(p.decl.Covering) > 0
		dels, ins, upds := indexing.DiffEntrySets(olds, news, p.decl.Unique,
			hadOld && hasCovering, m.valuePK)
		plans = append(plans, indexPlan{p: p, news: news, dels: dels, ins: ins, upds: upds})
	}
	return plans, nil
}

// probeUnique implements the pre-mutation unique check (indexing.md
// §Unique Indexes): for every insert key on a unique index, probe the
// on-disk index; a hit aborts with ErrIndexUniqueViolation before any
// row or index write happens. No mutation occurs here.
func (m *indexMaintainer) probeUnique(plans []indexPlan) error {
	for _, pl := range plans {
		if !pl.p.decl.Unique {
			continue
		}
		for _, k := range pl.ins {
			if pl.p.root == 0 {
				continue // empty index, no possible conflict
			}
			m.tx.pgr.RecordIndexProbe() // TxStats.IndexUniqueProbes
			_, found, err := btree.Get(m.tx.pgr, m.cfg, pl.p.root, []byte(k))
			if err != nil {
				return mapBtreeErr(err)
			}
			if found {
				return fmt.Errorf("%w: index %q on %s %q: key %x%s",
					ErrIndexUniqueViolation, pl.p.decl.Name, m.kind, m.ksName, []byte(k), m.opSuffix())
			}
		}
	}
	return nil
}

func (m *indexMaintainer) applyDeletes(plans []indexPlan, opIdx *int) error {
	for _, pl := range plans {
		if err := m.deleteKeys(pl.p, pl.dels, opIdx); err != nil {
			return err
		}
	}
	return nil
}

// deleteKeys retires `keys` from index p's data tree, advancing opIdx
// and recording TxStats per delete. A zero root or a missing key
// signals row/index divergence (the extractor said the key was present
// for the old value but the index disagrees) and surfaces ErrCorrupted
// — the same fault Check(CheckIndexes) would catch.
func (m *indexMaintainer) deleteKeys(p *pinnedIndex, keys []string, opIdx *int) error {
	for _, k := range keys {
		if p.root == 0 {
			return fmt.Errorf("%w: %s %q: delete of %x but index root is 0",
				ErrCorrupted, m.indexLabel(), p.decl.Name, []byte(k))
		}
		newRoot, err := btree.Delete(btreeWriter{m.tx.pgr}, m.cfg, p.root, m.mergeThreshold, []byte(k))
		if err != nil {
			if errors.Is(err, btree.ErrNotFound) {
				return fmt.Errorf("%w: %s %q: delete of %x missed (row/index divergence)",
					ErrCorrupted, m.indexLabel(), p.decl.Name, []byte(k))
			}
			return mapBtreeErr(err)
		}
		p.root = newRoot
		p.count--
		m.tx.pgr.AddIndexDeleted(1) // TxStats.IndexEntriesDeleted
		if err := fireIndexMaintenanceFailHookForTest(*opIdx); err != nil {
			return err
		}
		*opIdx++
	}
	return nil
}

func (m *indexMaintainer) applyInserts(plans []indexPlan, opIdx *int) error {
	pk := m.valuePK()
	for _, pl := range plans {
		hasCovering := len(pl.p.decl.Covering) > 0
		for _, k := range pl.ins {
			entry := pl.news[k]
			val := indexing.EntryValue(entry, pk, pl.p.decl.Unique, hasCovering)
			newRoot, err := btree.Put(btreeWriter{m.tx.pgr}, m.cfg, pl.p.root, []byte(k), val)
			if err != nil {
				return mapBtreeErr(err)
			}
			pl.p.root = newRoot
			pl.p.count++
			m.tx.pgr.AddIndexInserted(1) // TxStats.IndexEntriesInserted
			if err := fireIndexMaintenanceFailHookForTest(*opIdx); err != nil {
				return err
			}
			*opIdx++
		}
		// Covering-value rewrites: same key, changed payload. The Put
		// overwrites in place — entry count unchanged, but the write
		// is a real index mutation for stats and the failure hook.
		for _, k := range pl.upds {
			entry := pl.news[k]
			val := indexing.EntryValue(entry, pk, pl.p.decl.Unique, hasCovering)
			newRoot, err := btree.Put(btreeWriter{m.tx.pgr}, m.cfg, pl.p.root, []byte(k), val)
			if err != nil {
				return mapBtreeErr(err)
			}
			pl.p.root = newRoot
			m.tx.pgr.AddIndexInserted(1) // TxStats.IndexEntriesInserted (value rewrite)
			if err := fireIndexMaintenanceFailHookForTest(*opIdx); err != nil {
				return err
			}
			*opIdx++
		}
	}
	return nil
}

// newIndexMaintainer builds the engine for an indexed Keyspace
// mutation. The index value carries the row key as its PK and index
// keys encode via indexing.EntryKey.
func (ks *Keyspace) newIndexMaintainer(key []byte) *indexMaintainer {
	return &indexMaintainer{
		tx:             ks.tx,
		indexes:        ks.indexes,
		cfg:            ks.tx.pgr.Config(),
		mergeThreshold: ks.tx.db.opts.MergeThreshold,
		extractKey:     key,
		extractFn:      extractEntriesAsKeySet,
		valuePK:        func() []byte { return key },
		kind:           "keyspace",
		ksName:         ks.name.Value(),
	}
}

// newIndexMaintainer builds the engine for an indexed SetKeyspace
// mutation. The index value carries the compound (setKey,setValue) PK
// and index keys encode via indexing.EncodeSetEntryKey.
func (ks *SetKeyspace) newIndexMaintainer(setKey, setValue []byte) *indexMaintainer {
	return &indexMaintainer{
		tx:             ks.tx,
		indexes:        ks.indexes,
		cfg:            ks.tx.pgr.Config(),
		mergeThreshold: ks.tx.db.opts.MergeThreshold,
		extractKey:     setKey,
		extractFn:      setKeyspaceExtractEntries,
		valuePK:        func() []byte { return indexing.EncodeSetCompoundPK(setKey, setValue) },
		kind:           "SetKeyspace",
		ksName:         ks.name.Value(),
		setKey:         setKey,
		setValue:       setValue,
	}
}
