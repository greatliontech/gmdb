package gmdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/thegrumpylion/gmdb/internal/btree"
)

// indexRegistryEntry is the in-memory form of one row in a keyspace's
// per-keyspace index registry sub-tree. Encoded layout per
// indexing.md §Storage Layout:
//
//	+----------------+----------------------------------+
//	| SchemaHash     | uint64                           |
//	| Unique         | uint8                            |
//	| Padding        | [7]byte                          |
//	| Root           | uint64    (index B+tree root)    |
//	| Count          | uint64    (entries in the index) |
//	| UserVersionLen | uint16                           |
//	| UserVersion    | bytes                            |
//	| ColumnCount    | uint16                           |
//	| For each col: NameLen u16 || Name bytes           |
//	| CoveringCount  | uint16                           |
//	| For each col: NameLen u16 || Name bytes           |
//	+----------------+----------------------------------+
//
// Padding after Unique aligns the subsequent Root / Count uint64s
// at file-relative offsets 16 and 24.
type indexRegistryEntry struct {
	SchemaHash  uint64
	Unique      bool
	Root        uint64 // root page ID of the index's Kind=2 data sub-tree
	Count       uint64 // entries in the index
	UserVersion string
	Columns     []string // ordered; positional
	Covering    []string // ordered; positional; may be empty
}

// indexRegistryEntryFixedPrefixSize is the byte length of the
// fixed-size prefix (SchemaHash u64 + Unique u8 + Padding [7]byte +
// Root u64 + Count u64) = 32 bytes.
const indexRegistryEntryFixedPrefixSize = 32

// errRegistryEntryShort marks a registry-entry decode that ran out
// of bytes mid-field. Wrapped in ErrCorrupted at the caller's
// boundary (the registry sub-tree is engine-internal; a short
// entry value means the on-disk registry is malformed).
var errRegistryEntryShort = errors.New("registry entry truncated")

// encodeRegistryEntry serializes e to a fresh byte slice. Returns an
// error if any uint16-counted field overflows (UserVersion >
// 65535 bytes, column-name > 65535 bytes, ColumnCount > 65535,
// CoveringCount > 65535). The format is little-endian per the
// engine's file-layout.md §Byte order convention.
func encodeRegistryEntry(e *indexRegistryEntry) ([]byte, error) {
	if len(e.UserVersion) > math.MaxUint16 {
		return nil, fmt.Errorf("gmdb: IndexDecl.Version length %d exceeds uint16 max: %w",
			len(e.UserVersion), ErrInvalidOptions)
	}
	if len(e.Columns) > math.MaxUint16 {
		return nil, fmt.Errorf("gmdb: IndexDecl.Columns length %d exceeds uint16 max: %w",
			len(e.Columns), ErrInvalidOptions)
	}
	if len(e.Covering) > math.MaxUint16 {
		return nil, fmt.Errorf("gmdb: IndexDecl.Covering length %d exceeds uint16 max: %w",
			len(e.Covering), ErrInvalidOptions)
	}
	for i, c := range e.Columns {
		if len(c) > math.MaxUint16 {
			return nil, fmt.Errorf("gmdb: IndexDecl.Columns[%d].Name length %d exceeds uint16 max: %w",
				i, len(c), ErrInvalidOptions)
		}
	}
	for i, c := range e.Covering {
		if len(c) > math.MaxUint16 {
			return nil, fmt.Errorf("gmdb: IndexDecl.Covering[%d].Name length %d exceeds uint16 max: %w",
				i, len(c), ErrInvalidOptions)
		}
	}

	// Compute size for one allocation.
	size := indexRegistryEntryFixedPrefixSize
	size += 2 + len(e.UserVersion)
	size += 2
	for _, c := range e.Columns {
		size += 2 + len(c)
	}
	size += 2
	for _, c := range e.Covering {
		size += 2 + len(c)
	}

	buf := make([]byte, size)
	off := 0

	binary.LittleEndian.PutUint64(buf[off:], e.SchemaHash)
	off += 8

	if e.Unique {
		buf[off] = 1
	}
	off += 1
	// Padding [7]byte is zero — already implicit in make.
	off += 7

	binary.LittleEndian.PutUint64(buf[off:], e.Root)
	off += 8
	binary.LittleEndian.PutUint64(buf[off:], e.Count)
	off += 8

	binary.LittleEndian.PutUint16(buf[off:], uint16(len(e.UserVersion)))
	off += 2
	copy(buf[off:], e.UserVersion)
	off += len(e.UserVersion)

	binary.LittleEndian.PutUint16(buf[off:], uint16(len(e.Columns)))
	off += 2
	for _, c := range e.Columns {
		binary.LittleEndian.PutUint16(buf[off:], uint16(len(c)))
		off += 2
		copy(buf[off:], c)
		off += len(c)
	}

	binary.LittleEndian.PutUint16(buf[off:], uint16(len(e.Covering)))
	off += 2
	for _, c := range e.Covering {
		binary.LittleEndian.PutUint16(buf[off:], uint16(len(c)))
		off += 2
		copy(buf[off:], c)
		off += len(c)
	}

	return buf, nil
}

// decodeRegistryEntry deserializes data into a fresh indexRegistryEntry.
// Returns errRegistryEntryShort (wrapped in ErrCorrupted at the caller)
// if any field runs past the end of data. Padding bytes after Unique
// are NOT validated — on-disk values MUST be zero per
// indexing.md §Storage Layout, but the decoder is tolerant; the
// strict integrity walk asserts the zero requirement.
func decodeRegistryEntry(data []byte) (*indexRegistryEntry, error) {
	if len(data) < indexRegistryEntryFixedPrefixSize {
		return nil, fmt.Errorf("%w: fixed prefix needs %d bytes, got %d",
			errRegistryEntryShort, indexRegistryEntryFixedPrefixSize, len(data))
	}
	e := &indexRegistryEntry{}
	off := 0

	e.SchemaHash = binary.LittleEndian.Uint64(data[off:])
	off += 8

	e.Unique = data[off] != 0
	off += 1
	off += 7 // Padding

	e.Root = binary.LittleEndian.Uint64(data[off:])
	off += 8
	e.Count = binary.LittleEndian.Uint64(data[off:])
	off += 8

	if off+2 > len(data) {
		return nil, fmt.Errorf("%w: UserVersionLen u16 past end at offset %d", errRegistryEntryShort, off)
	}
	uvLen := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	if off+uvLen > len(data) {
		return nil, fmt.Errorf("%w: UserVersion(%d) past end at offset %d", errRegistryEntryShort, uvLen, off)
	}
	if uvLen > 0 {
		// Copy out of the (potentially mmap-borrowed) data slice so the
		// decoded struct outlives the borrow window. Per
		// api-surface.md §Byte Slice Ownership.
		e.UserVersion = string(data[off : off+uvLen])
	}
	off += uvLen

	if off+2 > len(data) {
		return nil, fmt.Errorf("%w: ColumnCount u16 past end at offset %d", errRegistryEntryShort, off)
	}
	colCount := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	// Forged-length bound (checksums.md §Structural and Allocation Bounds): before allocating the slice, verify the remaining bytes can
	// hold at least one 2-byte NameLen per column. A forged ColumnCount on
	// a truncated on-disk entry would otherwise force a multi-MB make()
	// before the per-iteration bounds check trips.
	if colCount*2 > len(data)-off {
		return nil, fmt.Errorf("%w: ColumnCount %d needs ≥%d bytes, %d remain at offset %d",
			errRegistryEntryShort, colCount, colCount*2, len(data)-off, off)
	}
	if colCount > 0 {
		e.Columns = make([]string, colCount)
		for i := range colCount {
			if off+2 > len(data) {
				return nil, fmt.Errorf("%w: Columns[%d] NameLen past end at offset %d",
					errRegistryEntryShort, i, off)
			}
			nLen := int(binary.LittleEndian.Uint16(data[off:]))
			off += 2
			if off+nLen > len(data) {
				return nil, fmt.Errorf("%w: Columns[%d] Name(%d) past end at offset %d",
					errRegistryEntryShort, i, nLen, off)
			}
			e.Columns[i] = string(data[off : off+nLen])
			off += nLen
		}
	}

	if off+2 > len(data) {
		return nil, fmt.Errorf("%w: CoveringCount u16 past end at offset %d", errRegistryEntryShort, off)
	}
	covCount := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	// Same forged-length pre-allocation bound as ColumnCount above (checksums.md §Structural and Allocation Bounds).
	if covCount*2 > len(data)-off {
		return nil, fmt.Errorf("%w: CoveringCount %d needs ≥%d bytes, %d remain at offset %d",
			errRegistryEntryShort, covCount, covCount*2, len(data)-off, off)
	}
	if covCount > 0 {
		e.Covering = make([]string, covCount)
		for i := range covCount {
			if off+2 > len(data) {
				return nil, fmt.Errorf("%w: Covering[%d] NameLen past end at offset %d",
					errRegistryEntryShort, i, off)
			}
			nLen := int(binary.LittleEndian.Uint16(data[off:]))
			off += 2
			if off+nLen > len(data) {
				return nil, fmt.Errorf("%w: Covering[%d] Name(%d) past end at offset %d",
					errRegistryEntryShort, i, nLen, off)
			}
			e.Covering[i] = string(data[off : off+nLen])
			off += nLen
		}
	}

	if off != len(data) {
		return nil, fmt.Errorf("%w: %d trailing bytes after registry entry", errRegistryEntryShort, len(data)-off)
	}

	return e, nil
}

// descriptorOwner is the contract a *Keyspace or *SetKeyspace
// satisfies for the registry-CRUD helpers: read+mutate the parent
// keyspace's descriptor, AND transition the owning handle's
// flush-state so the flushKeyspaces walk persists the
// mutation at Commit. registryPut / registryDelete take this
// interface (not a raw *keyspaceDescriptor) so that calling
// the helper without also marking the parent dirty is structurally
// impossible — closes a silent-data-loss path (registry mutation
// without the parent dirty-mark never reaches Commit's flush).
//
// *Keyspace.markDirty (keyspace.go) and *SetKeyspace.markDirty
// (set_keyspace.go) both preserve a Created state (Created stays
// Created) so the descriptor still flushes via the Created arm of
// flushKeyspaces.
type descriptorOwner interface {
	descriptor() *keyspaceDescriptor
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
func (tx *Tx) registryGet(owner descriptorOwner, name string) (*indexRegistryEntry, error) {
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
	e, err := decodeRegistryEntry(val)
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
func (tx *Tx) registryPut(owner descriptorOwner, name string, entry *indexRegistryEntry) error {
	if name == "" {
		return ErrKeyEmpty
	}
	encoded, err := encodeRegistryEntry(entry)
	if err != nil {
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
