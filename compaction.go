package gmdb

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
	"github.com/thegrumpylion/gmdb/internal/pager"
)

// compactForest relocates every page selected by shouldRelocate across all
// B+trees reachable from this write transaction's keyspace forest, returning
// the count of pages relocated. It is the in-place engine behind online
// incremental compaction (background-maintenance.md §Incremental
// Compaction); the orchestration layer supplies a high-watermark predicate
// (id >= evacFloor) and a budget, then commits.
//
// budget bounds the total relocations (btree.RelocatePages' maxMoves, shared
// — and decremented — across every tree in the forest). When it is exhausted
// mid-forest the remaining keyspaces are left untouched this pass; the
// orchestration's resumable cursor picks them up next pass. budget <= 0 or an
// empty forest (keyspaceRoot == 0) is a no-op.
//
// Precondition: the transaction must have NO open *Keyspace / *SetKeyspace
// handles. compactForest stages relocated descriptors via tx.dirtyDescriptors,
// which the tx field invariant requires be disjoint from openKeyspaces; an
// open handle for a name compactForest also restages would let the handle's
// stale-root descriptor and the relocated one collide at flushKeyspaces. The
// maintenance pass opens a dedicated bare write tx, so this holds by
// construction.
//
// Re-rooting reuses the transaction's existing persistence machinery, so a
// compacted forest is byte-indistinguishable from a Put-built one and lands
// atomically at Commit:
//   - an index data tree whose root moved → its registry entry is rewritten
//     (btree.Put into the registry sub-tree);
//   - a keyspace whose data root or index-registry root moved → the updated
//     descriptor is staged in tx.dirtyDescriptors (re-Put into the keyspace
//     descriptor tree by flushKeyspaces at Commit);
//   - the keyspace descriptor tree itself is relocated last and assigned to
//     tx.keyspaceRoot (→ meta.KeyspaceRoot at Commit) — last, so the
//     descriptor re-Puts in flushKeyspaces land on the relocated tree.
//
// cfg discipline mirrors copyCompact: a keyspace's data tree (and its nested
// set-keyspace trees, which RelocatePages recurses into) uses the
// keyspace-overridden RestartGroupTarget; the index registry sub-tree and
// index data trees use the base pager cfg, matching how the runtime
// (index_codec.go / index_maintain.go) maintains them.
//
// RPL segment pages are never relocated, and not because of any predicate
// term: they hang off meta.RPLHeadPage on a chain that the keyspace forest
// walk never reaches, so RelocatePages never offers one to shouldRelocate.
// They drain via reclamation and new segments self-place low (the deferred
// out-of-band-relocation case lives in git history).
//
// A relocation that would overrun MaxTxBufferBytes surfaces ErrTxTooLarge,
// which compactForest returns to the caller for rollback (Inv-M4: the
// maintenance orchestration catches it and reduces the batch — it is never
// user-visible). Partial work already applied to the slab is discarded by
// the caller's rollback; nothing is committed.
func (tx *Tx) compactForest(shouldRelocate func(uint64) bool, budget int) (int, error) {
	if tx.keyspaceRoot == 0 || budget <= 0 {
		return 0, nil
	}
	pw := tx.pgr
	baseCfg := pw.Config()
	hwm := pw.HighWaterMark()
	remaining := budget
	moved := 0

	// 1. Snapshot the keyspace roster. WalkKV borrows key/value into page
	//    buffers that later relocations mutate, so clone the names and decode
	//    the descriptors up front.
	type ksEntry struct {
		name []byte
		desc page.KeyspaceDescriptor
	}
	var roster []ksEntry
	if err := btree.WalkKV(pw, baseCfg, tx.keyspaceRoot, hwm, func(k, v []byte) error {
		if len(v) != page.KeyspaceDescriptorSize {
			return fmt.Errorf("%w: keyspace %q descriptor size %d", btree.ErrCorrupted, string(k), len(v))
		}
		roster = append(roster, ksEntry{name: bytes.Clone(k), desc: page.DecodeKeyspaceDescriptor(v)})
		return nil
	}); err != nil {
		return 0, mapCompactErr(err)
	}

	// 2. Relocate each keyspace's data tree + index trees.
	for i := range roster {
		if remaining <= 0 {
			break
		}
		ks := &roster[i]
		dataCfg := baseCfg
		if ks.desc.RestartGroupTarget != 0 {
			dataCfg.RestartGroupTarget = ks.desc.RestartGroupTarget
		}
		dirty := false

		if ks.desc.Root != 0 {
			nr, m, err := btree.RelocatePages(pw, dataCfg, ks.desc.Root, shouldRelocate, remaining)
			if err != nil {
				return 0, mapCompactErr(err)
			}
			remaining -= m
			moved += m
			if nr != ks.desc.Root {
				ks.desc.Root = nr
				dirty = true
			}
		}

		if ks.desc.IndexRegistryRoot != 0 && remaining > 0 {
			newReg, m, err := tx.compactIndexRegistry(ks.desc.IndexRegistryRoot, shouldRelocate, baseCfg, hwm, &remaining)
			if err != nil {
				return 0, err // already mapped
			}
			moved += m
			if newReg != ks.desc.IndexRegistryRoot {
				ks.desc.IndexRegistryRoot = newReg
				dirty = true
			}
		}

		if dirty {
			if tx.dirtyDescriptors == nil {
				tx.dirtyDescriptors = make(map[string]page.KeyspaceDescriptor)
			}
			tx.dirtyDescriptors[string(ks.name)] = ks.desc
		}
	}

	// 3. Relocate the keyspace descriptor tree itself, last (base cfg —
	//    matching how copyCompact and CreateKeyspace build it). flushKeyspaces
	//    will re-Put the dirty descriptors into the relocated tree at Commit.
	if remaining > 0 {
		nkr, m, err := btree.RelocatePages(pw, baseCfg, tx.keyspaceRoot, shouldRelocate, remaining)
		if err != nil {
			return 0, mapCompactErr(err)
		}
		remaining -= m
		moved += m
		tx.keyspaceRoot = nkr
	}

	return moved, nil
}

// compactIndexRegistry relocates a keyspace's index registry sub-tree and
// every index data tree it points at, returning the (possibly new) registry
// root and the count of pages relocated. An index data tree whose root moves
// has its registry entry rewritten in place (btree.Put), then the registry
// tree itself is relocated. remaining is the shared forest budget, decremented
// as pages move. Uses cfg (the base pager cfg) for both the registry tree and
// the index data trees, matching the runtime's maintenance of them.
func (tx *Tx) compactIndexRegistry(regRoot uint64, shouldRelocate func(uint64) bool, cfg page.Config, hwm uint64, remaining *int) (uint64, int, error) {
	pw := tx.pgr
	moved := 0

	// Snapshot the registry entries (name + decoded entry) before mutating.
	type idxEntry struct {
		name  []byte
		entry *indexRegistryEntry
	}
	var entries []idxEntry
	if err := btree.WalkKV(pw, cfg, regRoot, hwm, func(k, v []byte) error {
		e, derr := decodeRegistryEntry(v)
		if derr != nil {
			return fmt.Errorf("%w: index %q registry entry: %v", btree.ErrCorrupted, string(k), derr)
		}
		entries = append(entries, idxEntry{name: bytes.Clone(k), entry: e})
		return nil
	}); err != nil {
		return 0, 0, mapCompactErr(err)
	}

	curReg := regRoot
	for _, ie := range entries {
		if *remaining <= 0 {
			break
		}
		if ie.entry.Root == 0 {
			continue
		}
		nr, m, err := btree.RelocatePages(pw, cfg, ie.entry.Root, shouldRelocate, *remaining)
		if err != nil {
			return 0, 0, mapCompactErr(err)
		}
		*remaining -= m
		moved += m
		if nr != ie.entry.Root {
			ie.entry.Root = nr
			nv, eerr := encodeRegistryEntry(ie.entry)
			if eerr != nil {
				// Unreachable in-spec (we re-encode an entry we just decoded,
				// changing only the scalar Root), but keep the contract that
				// this function returns mapped/contextualised errors.
				return 0, 0, fmt.Errorf("gmdb: compaction: index %q registry re-encode: %w", string(ie.name), eerr)
			}
			nrg, perr := btree.Put(pw, cfg, curReg, ie.name, nv)
			if perr != nil {
				return 0, 0, mapCompactErr(perr)
			}
			curReg = nrg
		}
	}

	// Relocate the registry tree itself, after the entry rewrites.
	if *remaining > 0 {
		nr, m, err := btree.RelocatePages(pw, cfg, curReg, shouldRelocate, *remaining)
		if err != nil {
			return 0, 0, mapCompactErr(err)
		}
		*remaining -= m
		moved += m
		curReg = nr
	}

	return curReg, moved, nil
}

// mapCompactErr maps the btree + pager error surfaces that the relocation
// engine spans onto the gmdb public sentinels, so the orchestration layer can
// match ErrTxTooLarge (Inv-M4) and callers see consistent errors.
func mapCompactErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, pager.ErrTxTooLarge):
		return ErrTxTooLarge
	case errors.Is(err, pager.ErrDBFull):
		return ErrDBFull
	case errors.Is(err, pager.ErrBadPageChecksum):
		return fmt.Errorf("%w: %w", ErrBadPageChecksum, err)
	case errors.Is(err, btree.ErrCorrupted), errors.Is(err, pager.ErrCorrupted), errors.Is(err, btree.ErrTreeTooDeep):
		return fmt.Errorf("%w: %w", ErrCorrupted, err)
	}
	return fmt.Errorf("gmdb: compaction: %w", err)
}
