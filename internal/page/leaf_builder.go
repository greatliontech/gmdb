package page

import (
	"bytes"
	"fmt"
)

// LeafBuilder constructs a leaf page incrementally — one AddInline or
// AddOverflow call per entry, then Finish to write the page header and
// lookup table. Dispatches on Config.RestartGroupTarget to produce
// either a compressed leaf (target ≥ 2, the default) or an uncompressed
// leaf (target == 1).
//
// Entries MUST be added in ascending key order. The builder verifies via
// a debug assertion (panic in tests; the chunk-5 keyspace layer pre-sorts
// for BulkLoad and the btree splice helpers always insert in-order).
//
// Inline backing arrays avoid heap allocation in the common case:
//   - prevKeyBuf: 512 bytes for the running previous key (compressed mode).
//   - ucOffsets: 512-entry inline offset table (uncompressed mode).
//   - restartGroupTracker.restartsBuf: 128 groups inline.
//
// Workloads with >512 entries per page (random tiny keys at 16 KB+
// PageSize) fall back to heap allocation transparently — the builder
// grows the relevant slice without changing the public surface.
type LeafBuilder struct {
	rgt        restartGroupTracker // compressed mode only
	buf        []byte
	cfg        Config
	count      int
	dataPos    int
	compressed bool

	// Compressed-mode running state.
	prevKey    []byte
	prevKeyBuf [512]byte

	// Uncompressed-mode positional offset accumulator.
	ucOffsets    []uint16
	ucOffsetsBuf [512]uint16

	// Debug: previous key for sort-order assertion (shared across
	// modes). Initialized lazily.
	lastAddedKey []byte
}

// NewLeafBuilder initializes a builder writing into buf. Caller must
// ensure len(buf) == cfg.PageSize; for the btree write path, this is the
// freshly-allocated slab buffer.
func NewLeafBuilder(buf []byte, cfg Config) *LeafBuilder {
	b := &LeafBuilder{}
	b.Reset(buf, cfg)
	return b
}

// Reset reinitializes the builder for reuse with a new buffer, preserving
// the inline backing arrays so a pooled LeafBuilder doesn't re-allocate.
func (b *LeafBuilder) Reset(buf []byte, cfg Config) {
	cfg.mustValidate()
	if len(buf) != int(cfg.PageSize) {
		panic(fmt.Sprintf("page: LeafBuilder buf len %d != PageSize %d", len(buf), cfg.PageSize))
	}
	b.buf = buf
	b.cfg = cfg
	b.count = 0
	b.compressed = cfg.EffectiveRestartGroupTarget() != 1
	b.dataPos = leafEntryStart
	b.lastAddedKey = nil
	if b.compressed {
		b.rgt.init()
		b.prevKey = b.prevKeyBuf[:0]
	} else {
		b.ucOffsets = b.ucOffsetsBuf[:0]
	}
}

// AddInline appends an inline (key, value) entry (CellFlags = 0).
// Returns false if the page is full (caller decides to split or
// finish + start a new page). Panics on out-of-order key (debug
// assertion — pre-sorting is the caller's responsibility).
func (b *LeafBuilder) AddInline(key, value []byte) bool {
	return b.addEntry(key, 0, value, 0, 0)
}

// AddOverflow appends an overflow-reference entry. ovflPage is the first
// page ID of the overflow run; totalLen is the assembled value size.
// Returns false on page-full.
func (b *LeafBuilder) AddOverflow(key []byte, ovflPage, totalLen uint64) bool {
	return b.addEntry(key, CellFlagOverflow, nil, ovflPage, totalLen)
}

// AddSubpage appends a SetKeyspace subpage cell (CellFlagMultiValue
// set, CellFlagNestedTree clear). The subpage parameter holds the
// raw subpage bytes (header + entries, per set-keyspace.md §Subpage
// Format) produced by internal/page.SubpageReader / EncodeSubpage /
// Insert / Delete. The leaf carries the bytes opaque-through;
// per-subpage validation lives at the SetKeyspace surface (chunk
// 6.6) which has the keyspace's FixedValueSize.
//
// On-disk encoding is the same shape as AddInline (the cell flag is
// the only byte that differs): [Flags][KeyLen][ValueLen][Key][Subpage].
// Returns false on page-full.
//
// The subpage's 50%-of-leaf promotion threshold is the SetKeyspace
// layer's responsibility — this builder does not enforce it and will
// happily build a leaf containing an over-threshold subpage if asked.
func (b *LeafBuilder) AddSubpage(key, subpage []byte) bool {
	return b.addEntry(key, CellFlagMultiValue, subpage, 0, 0)
}

// AddNestedTreeRef appends a SetKeyspace nested-B+tree reference cell
// (both CellFlagMultiValue and CellFlagNestedTree set). root is the
// page ID of the nested B+tree's root; count is the cached member
// count (the O(1) value surfaced by SetKeyspace.CountValues per
// set-keyspace.md §Nested B+tree Reference Cell).
//
// On-disk encoding reuses the overflow wire form
// (`[Flags][KeyLen][Key][u64][u64]` for restart/uncompressed;
// `[Flags][SharedLen][UnsharedLen][UnsharedKey][u64][u64]` for delta)
// — no `ValueLen` prefix; the two u64 fields hold (Root, Count)
// instead of (OvflPage, TotalLen). Returns false on page-full.
//
// The keyspace's Count-equality contract (set-keyspace.md entailed
// invariant E1: nested-cell Count equals the number of leaf entries
// reachable from Root) is the SetKeyspace surface's responsibility;
// the builder writes whatever (root, count) the caller supplies.
func (b *LeafBuilder) AddNestedTreeRef(key []byte, root, count uint64) bool {
	return b.addEntry(key, CellFlagMultiValue|CellFlagNestedTree, nil, root, count)
}

// AddEntry dispatches by e.Flags. Convenience for callers that
// already hold a LeafEntry — e.g., the split / merge / rebuild
// helpers in internal/btree that decode a leaf, mutate the entry
// list, and re-encode. Preserves the cell flags through the
// round-trip so SetKeyspace subpage cells survive every leaf
// rebuild without silent demotion to a plain inline cell.
//
// Recognised dispatches:
//   - CellFlagOverflow (alone) → AddOverflow (overflow reference
//     value half).
//   - CellFlagMultiValue && !CellFlagNestedTree → AddSubpage (subpage).
//   - flags == 0 → AddInline (plain key → value).
//
// Panics on `CellFlagOverflow | CellFlagMultiValue` — `page-formats.md
// §Leaf Page (CellFlags bit layout)` declares these mutually exclusive
// in practice; no encoding exists for the combination, so the caller
// has constructed an unrepresentable cell. Validate enforces the same
// rejection at the read-side trust boundary.
func (b *LeafBuilder) AddEntry(e LeafEntry) bool {
	switch {
	case e.Flags&CellFlagOverflow != 0 && e.Flags&CellFlagMultiValue != 0:
		panic(fmt.Sprintf("page: LeafBuilder.AddEntry on CellFlagOverflow|CellFlagMultiValue cell (flags=0x%x) — these bits are mutually exclusive per page-formats.md §Leaf Page (CellFlags bit layout)", e.Flags))
	case e.Flags&CellFlagOverflow != 0:
		return b.AddOverflow(e.Key, e.OverflowPage, e.TotalLen)
	case e.IsNestedTree():
		return b.AddNestedTreeRef(e.Key, e.NestedRoot, e.NestedCount)
	case e.IsSubpage():
		return b.AddSubpage(e.Key, e.Value)
	default:
		return b.AddInline(e.Key, e.Value)
	}
}

func (b *LeafBuilder) addEntry(key []byte, flags uint8, value []byte, ovflPage, totalLen uint64) bool {
	if flags&^cellFlagKnownMask != 0 {
		panic(fmt.Sprintf("page: LeafBuilder.AddEntry unknown CellFlags bits 0x%x", flags&^cellFlagKnownMask))
	}
	if b.lastAddedKey != nil && bytes.Compare(b.lastAddedKey, key) >= 0 {
		panic(fmt.Sprintf("page: LeafBuilder keys out of order — last %q, next %q", b.lastAddedKey, key))
	}
	var ok bool
	if !b.compressed {
		ok = b.addUCEntry(key, flags, value, ovflPage, totalLen)
	} else {
		ok = b.addCompressedEntry(key, flags, value, ovflPage, totalLen)
	}
	if ok {
		// Stash the key we just wrote for the next ordering check.
		// Borrow the on-page bytes — they live for the builder's
		// lifetime and don't get re-encoded later.
		b.lastAddedKey = key
	}
	return ok
}

// addUCEntry writes an uncompressed entry: [Flags][KeyLen][ValueLen|Ovfl][Key][Value|OvflPage+TotalLen].
func (b *LeafBuilder) addUCEntry(key []byte, flags uint8, value []byte, ovflPage, totalLen uint64) bool {
	entrySize := 1 + 2 + len(key) + valuePartSize(flags, value)
	newTableSize := (b.count + 1) * ucOffsetEntrySize
	if b.dataPos+entrySize+newTableSize > b.cfg.ContentEnd() {
		return false
	}

	off := b.dataPos
	b.buf[off] = flags
	off++
	le.PutUint16(b.buf[off:], uint16(len(key)))
	off += 2
	if cellHasTrailerOnly(flags) {
		// Overflow OR NestedTree — both lay [Key][u64][u64]. The
		// caller passes the trailer u64s as (ovflPage, totalLen);
		// AddNestedTreeRef threads (root, count) through these
		// same parameters.
		copy(b.buf[off:], key)
		off += len(key)
		le.PutUint64(b.buf[off:], ovflPage)
		off += 8
		le.PutUint64(b.buf[off:], totalLen)
		off += 8
	} else {
		le.PutUint32(b.buf[off:], uint32(len(value)))
		off += 4
		copy(b.buf[off:], key)
		off += len(key)
		copy(b.buf[off:], value)
		off += len(value)
	}

	b.ucOffsets = append(b.ucOffsets, uint16(b.dataPos))
	b.dataPos = off
	b.count++
	return true
}

// addCompressedEntry writes a compressed entry — restart or delta as
// determined by the restart-group tracker. Variable-group natural-break
// heuristic: when SharedLen would be zero with the previous key (no
// compression benefit) and the current group is non-empty, force a new
// group early so the delta-header overhead doesn't accrue on entries
// that gain nothing from sharing. This is the "natural break" policy
// described in page-formats.md §Compressed Leaf.
func (b *LeafBuilder) addCompressedEntry(key []byte, flags uint8, value []byte, ovflPage, totalLen uint64) bool {
	target := int(b.cfg.EffectiveRestartGroupTarget())

	// Decide if this entry must start a new group.
	atTarget := b.rgt.IsRestart(b.count, target)
	naturalBreak := false
	if !atTarget && b.count > 0 && b.rgt.CurGroupCount() > 0 {
		// Compute SharedLen against the previous key cheaply. If
		// it's zero, force a new group (avoid spending 2 extra
		// bytes per delta entry on no-shared-prefix keys when we
		// could pay 0 extra by starting fresh).
		if sharedPrefixLen(b.prevKey, key) == 0 {
			naturalBreak = true
		}
	}
	isRestart := atTarget || naturalBreak

	// Finalize the in-progress group before opening a new one.
	if isRestart && b.rgt.CurGroupCount() > 0 {
		b.rgt.FinalizeCurrentGroup()
	}

	var sharedLen int
	unsharedKey := key
	if !isRestart {
		sharedLen = sharedPrefixLen(b.prevKey, key)
		unsharedKey = key[sharedLen:]
	}

	// Compute the entry's on-page size.
	headerSize := 1 // CellFlags
	if isRestart {
		headerSize += 2 + len(key) // KeyLen + Key
	} else {
		headerSize += 2 + 2 + len(unsharedKey) // SharedLen + UnsharedLen + UnsharedKey
	}
	entrySize := headerSize + valuePartSize(flags, value)

	// Fit check: account for the table slot we'd add if this is a new group.
	extraRestart := 0
	if isRestart {
		extraRestart = 1
	}
	tableSize := b.rgt.TableSize(extraRestart)
	if b.dataPos+entrySize+tableSize > b.cfg.ContentEnd() {
		return false
	}

	if isRestart {
		b.rgt.StartGroup(b.dataPos)
	}

	// Write the entry. The byte layout — gmdb's ValueLen-before-key order —
	// is produced by the shared writeCompressed{Restart,Delta}Entry helpers
	// so the builder and the in-place splice helpers (leaf_splice.go) encode
	// entries identically; that single source of truth is what lets a
	// spliced page stay byte-identical to a decode-re-encode (page-formats.md
	// §Leaf Split deterministic-encoding invariant).
	var off int
	if isRestart {
		off = writeCompressedRestartEntry(b.buf, b.dataPos, flags, key, value, ovflPage, totalLen)
	} else {
		off = writeCompressedDeltaEntry(b.buf, b.dataPos, flags, sharedLen, unsharedKey, value, ovflPage, totalLen)
	}

	b.dataPos = off
	b.prevKey = append(b.prevKey[:0], key...)
	b.count++
	b.rgt.IncrCount()
	return true
}

// writeCompressedRestartEntry writes a restart (full-key) compressed entry
// at off and returns the offset just past it. Byte layout per
// page-formats.md §Compressed Leaf restart entry — gmdb's
// [Flags][KeyLen][ValueLen][Key][Value] order (ValueLen before the key),
// with the 16-byte trailer form for overflow / nested-tree cells (no
// ValueLen prefix). ovflPage / totalLen are the generic trailer pair —
// (OverflowPage, TotalLen) for overflow cells, (NestedRoot, NestedCount)
// for nested-tree cells (mirrors LeafBuilder.AddEntry's dispatch).
//
// Shared by LeafBuilder.addCompressedEntry and the in-place splice helpers
// so both encoders are byte-identical; do not duplicate the layout.
func writeCompressedRestartEntry(buf []byte, off int, flags uint8, key, value []byte, ovflPage, totalLen uint64) int {
	buf[off] = flags
	off++
	le.PutUint16(buf[off:], uint16(len(key)))
	off += 2
	if cellHasTrailerOnly(flags) {
		copy(buf[off:], key)
		off += len(key)
		le.PutUint64(buf[off:], ovflPage)
		off += 8
		le.PutUint64(buf[off:], totalLen)
		off += 8
		return off
	}
	le.PutUint32(buf[off:], uint32(len(value)))
	off += 4
	copy(buf[off:], key)
	off += len(key)
	copy(buf[off:], value)
	off += len(value)
	return off
}

// writeCompressedDeltaEntry writes a delta (prefix-compressed) compressed
// entry at off and returns the offset just past it. Byte layout per
// page-formats.md §Compressed Leaf delta entry — gmdb's
// [Flags][SharedLen][UnsharedLen][ValueLen][UnsharedKey][Value] order
// (ValueLen before the unshared key), with the 16-byte trailer form for
// overflow / nested-tree cells. ovflPage / totalLen are the generic
// trailer pair as in writeCompressedRestartEntry.
//
// Shared by LeafBuilder.addCompressedEntry and the in-place splice helpers.
func writeCompressedDeltaEntry(buf []byte, off int, flags uint8, sharedLen int, unsharedKey, value []byte, ovflPage, totalLen uint64) int {
	buf[off] = flags
	off++
	le.PutUint16(buf[off:], uint16(sharedLen))
	off += 2
	le.PutUint16(buf[off:], uint16(len(unsharedKey)))
	off += 2
	if cellHasTrailerOnly(flags) {
		copy(buf[off:], unsharedKey)
		off += len(unsharedKey)
		le.PutUint64(buf[off:], ovflPage)
		off += 8
		le.PutUint64(buf[off:], totalLen)
		off += 8
		return off
	}
	le.PutUint32(buf[off:], uint32(len(value)))
	off += 4
	copy(buf[off:], unsharedKey)
	off += len(unsharedKey)
	copy(buf[off:], value)
	off += len(value)
	return off
}

// Finish writes the page header and the lookup table. Returns the entry
// count for caller convenience (matches what was written to the header).
// After Finish the builder is single-use; call Reset to write another
// page into the same builder.
func (b *LeafBuilder) Finish() uint16 {
	count := uint16(b.count)
	if !b.compressed {
		// Zero the free-space region for determinism (matches behavior
		// of EncodeLeaf which cleared the whole buffer before writing).
		zeroFreeSpace(b.buf, b.dataPos, b.cfg.ContentEnd()-int(count)*ucOffsetEntrySize)
		WriteHeader(b.buf, TypeLeafUncompressed, count, 0)
		le.PutUint16(b.buf[ucLeafOffDataEnd:], uint16(b.dataPos))
		le.PutUint16(b.buf[ucLeafOffDataEnd+2:], 0) // reserved

		// Write the positional offset table.
		contentEnd := b.cfg.ContentEnd()
		tableStart := contentEnd - b.count*ucOffsetEntrySize
		for i := range b.count {
			off := tableStart + i*ucOffsetEntrySize
			le.PutUint16(b.buf[off:], b.ucOffsets[i])
		}
		return count
	}

	// Compressed: write restart table first, then header (so RestartCount
	// is known and stored).
	rc := b.rgt.WriteTable(b.buf, b.cfg.ContentEnd())
	zeroFreeSpace(b.buf, b.dataPos, b.cfg.ContentEnd()-rc*restartTableEntrySize)
	WriteHeader(b.buf, TypeLeaf, count, 0)
	le.PutUint16(b.buf[leafOffRestartCount:], uint16(rc))
	le.PutUint16(b.buf[leafOffDataEnd:], uint16(b.dataPos))
	return count
}

// Count returns the number of entries added so far.
func (b *LeafBuilder) Count() int { return b.count }

// FreeSpace returns the remaining usable bytes available for one more
// entry. Used by bulkload + split heuristics to decide when to close a
// page.
func (b *LeafBuilder) FreeSpace() int {
	if !b.compressed {
		return b.cfg.ContentEnd() - b.dataPos - b.count*ucOffsetEntrySize
	}
	return b.cfg.ContentEnd() - b.dataPos - b.rgt.TableSize(0)
}

// valuePartSize returns the on-page byte size of an entry's value half.
// Both overflow references and nested-tree references use the 16-byte
// trailer form (no ValueLen prefix) per page-formats.md §Leaf Page;
// inline and subpage cells carry [ValueLen u32][bytes].
func valuePartSize(flags uint8, value []byte) int {
	if cellHasTrailerOnly(flags) {
		return 8 + 8 // u64 + u64 (OvflPage|Root + TotalLen|Count)
	}
	return 4 + len(value) // ValueLen uint32 + value bytes
}

// zeroFreeSpace clears bytes in [lo, hi). Mirrors the chunk-4.2
// "clear(buf)-before-encode" behavior so two encoders for the same input
// produce byte-identical pages — the deterministic-encoding invariant
// per page-formats.md §Leaf Split.
func zeroFreeSpace(buf []byte, lo, hi int) {
	if lo < hi {
		clear(buf[lo:hi])
	}
}
