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
// a debug assertion (panic in tests; the keyspace layer pre-sorts
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
	rgt     restartGroupTracker // compressed modes only
	buf     []byte
	cfg     Config
	count   int
	dataPos int
	variant uint8 // TypeLeaf | TypeLeafSegregated | TypeLeafUncompressed

	// Compressed-mode running state (interleaved + segregated).
	prevKey    []byte
	prevKeyBuf [512]byte

	// Uncompressed-mode positional offset accumulator.
	ucOffsets    []uint16
	ucOffsetsBuf [512]uint16

	// Segregated-mode accumulators: value-region content in add order
	// (copied into place at Finish, when the restart table's size —
	// and so the region's end-anchored position — is known), plus the
	// buf position of each entry's VOff field and its content's
	// relative start for the Finish-time patch.
	segVals         []byte
	segValsBuf      [1024]byte
	segVOffSlots    []int32
	segVOffSlotsBuf [512]int32
	segRel          []uint32
	segRelBuf       [512]uint32

	// Debug: previous key for sort-order assertion (shared across
	// modes). Initialized lazily.
	lastAddedKey []byte

	// forceRestart forces the next compressed entry to open a new
	// restart group: set after writing an overflow-key entry so its
	// group stays a singleton (page-formats.md §Overflow-Key Cells,
	// restart-group rule).
	forceRestart bool
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
	cfg.MustValidate()
	if len(buf) != int(cfg.PageSize) {
		panic(fmt.Sprintf("page: LeafBuilder buf len %d != PageSize %d", len(buf), cfg.PageSize))
	}
	b.buf = buf
	b.cfg = cfg
	b.count = 0
	b.variant = cfg.EffectiveLeafType()
	b.dataPos = leafEntryStart
	b.lastAddedKey = nil
	b.forceRestart = false
	switch b.variant {
	case TypeLeafUncompressed:
		b.ucOffsets = b.ucOffsetsBuf[:0]
	case TypeLeafSegregated:
		b.dataPos = segLeafEntryStart
		b.rgt.init()
		b.prevKey = b.prevKeyBuf[:0]
		b.segVals = b.segValsBuf[:0]
		b.segVOffSlots = b.segVOffSlotsBuf[:0]
		b.segRel = b.segRelBuf[:0]
	default:
		b.rgt.init()
		b.prevKey = b.prevKeyBuf[:0]
	}
}

// AddInline appends an inline (key, value) entry (CellFlags = 0).
// Returns false if the page is full (caller decides to split or
// finish + start a new page). Panics on out-of-order key (debug
// assertion — pre-sorting is the caller's responsibility).
func (b *LeafBuilder) AddInline(key, value []byte) bool {
	return b.addEntry(key, 0, value, 0, 0, 0, 0)
}

// AddOverflow appends an overflow-reference entry. ovflPage is the first
// page ID of the overflow run; totalLen is the assembled value size.
// Returns false on page-full.
func (b *LeafBuilder) AddOverflow(key []byte, ovflPage, totalLen uint64) bool {
	return b.addEntry(key, CellFlagOverflow, nil, ovflPage, totalLen, 0, 0)
}

// AddSubpage appends a SetKeyspace subpage cell (CellFlagMultiValue
// set, CellFlagNestedTree clear). The subpage parameter holds the
// raw subpage bytes (header + entries, per set-keyspace.md §Subpage
// Format) produced by internal/page.SubpageReader / EncodeSubpage /
// Insert / Delete. The leaf carries the bytes opaque-through;
// per-subpage validation lives at the SetKeyspace surface,
// which has the keyspace's FixedValueSize.
//
// On-disk encoding is the same shape as AddInline (the cell flag is
// the only byte that differs): [Flags][KeyLen][ValueLen][Key][Subpage].
// Returns false on page-full.
//
// The subpage's 50%-of-leaf promotion threshold is the SetKeyspace
// layer's responsibility — this builder does not enforce it and will
// happily build a leaf containing an over-threshold subpage if asked.
func (b *LeafBuilder) AddSubpage(key, subpage []byte) bool {
	return b.addEntry(key, CellFlagMultiValue, subpage, 0, 0, 0, 0)
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
	return b.addEntry(key, CellFlagMultiValue|CellFlagNestedTree, nil, root, count, 0, 0)
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
		return b.addEntry(e.Key, e.Flags, nil, e.OverflowPage, e.TotalLen, e.KeyExtPage, e.KeyTotalLen)
	case e.IsNestedTree():
		return b.addEntry(e.Key, e.Flags, nil, e.NestedRoot, e.NestedCount, e.KeyExtPage, e.KeyTotalLen)
	default:
		// Subpage and plain inline share the inline wire form; the
		// flags pass through so subpage cells (and the OverflowKey
		// bit on any form) survive rebuilds without demotion.
		return b.addEntry(e.Key, e.Flags, e.Value, 0, 0, e.KeyExtPage, e.KeyTotalLen)
	}
}

func (b *LeafBuilder) addEntry(key []byte, flags uint8, value []byte, ovflPage, totalLen uint64, keyExtPage uint64, keyTotalLen uint32) bool {
	if flags&^cellFlagKnownMask != 0 {
		panic(fmt.Sprintf("page: LeafBuilder.AddEntry unknown CellFlags bits 0x%x", flags&^cellFlagKnownMask))
	}
	ovk := flags&CellFlagOverflowKey != 0
	if b.lastAddedKey != nil {
		// Ordering assertion over the bytes the builder can see. For
		// overflow-key entries `key` is the RESIDENT first-T prefix, so
		// a resident tie is legal exactly when the INCOMING entry is
		// overflow-key: its full key strictly exceeds the tied resident
		// bytes, so it is strictly greater than a non-overflow
		// predecessor equal to them (a key of exactly T bytes), and
		// against an overflow predecessor the order lives in the
		// extents, which the builder cannot read — the caller's
		// contract. Every other equality (a duplicate, or a
		// non-overflow entry tying an overflow predecessor whose full
		// key exceeds it) or inversion is a caller bug.
		c := bytes.Compare(b.lastAddedKey, key)
		if c > 0 || (c == 0 && !ovk) {
			panic(fmt.Sprintf("page: LeafBuilder keys out of order — last %q, next %q", b.lastAddedKey, key))
		}
	}
	var ok bool
	switch b.variant {
	case TypeLeafUncompressed:
		ok = b.addUCEntry(key, flags, value, ovflPage, totalLen, keyExtPage, keyTotalLen)
	case TypeLeafSegregated:
		ok = b.addSegEntry(key, flags, value, ovflPage, totalLen, keyExtPage, keyTotalLen)
	default:
		ok = b.addCompressedEntry(key, flags, value, ovflPage, totalLen, keyExtPage, keyTotalLen)
	}
	if ok {
		// Stash the key we just wrote for the next ordering check.
		// Borrow the on-page bytes — they live for the builder's
		// lifetime and don't get re-encoded later.
		b.lastAddedKey = key
	}
	return ok
}

// addUCEntry writes an uncompressed entry via the shared writeFullKeyEntry encoder
// (single source of truth for the uncompressed byte layout, shared with the
// uncompressed in-place splice helpers).
func (b *LeafBuilder) addUCEntry(key []byte, flags uint8, value []byte, ovflPage, totalLen uint64, keyExtPage uint64, keyTotalLen uint32) bool {
	entrySize := 1 + keyPartSize(flags, key) + valuePartSize(flags, value)
	newTableSize := (b.count + 1) * ucOffsetEntrySize
	if b.dataPos+entrySize+newTableSize > b.cfg.ContentEnd() {
		return false
	}
	newPos := writeFullKeyEntry(b.buf, b.dataPos, flags, key, value, ovflPage, totalLen, keyExtPage, keyTotalLen)
	b.ucOffsets = append(b.ucOffsets, uint16(b.dataPos))
	b.dataPos = newPos
	b.count++
	return true
}

// writeFullKeyEntry writes one full-key entry at off and returns the
// offset just past it. The uncompressed entry and the compressed
// RESTART entry share this exact wire layout (page-formats.md
// §Uncompressed Leaf: "identical to the compressed variant") — gmdb's
// [Flags][KeyLen][ValueLen][Key][Value] order (ValueLen before the
// key), with the 16-byte trailer form for overflow / nested-tree
// cells (no ValueLen prefix). ovflPage / totalLen are the generic
// trailer pair — (OverflowPage, TotalLen) for overflow, (NestedRoot,
// NestedCount) for nested-tree (mirrors LeafBuilder.AddEntry's
// dispatch).
//
// The ONE encoder for both variants, shared by LeafBuilder and the
// in-place splice helpers; do not duplicate the layout. (Only DELTA
// entries have a distinct layout — writeDeltaEntry.)
func writeFullKeyEntry(buf []byte, off int, flags uint8, key, value []byte, ovflPage, totalLen uint64, keyExtPage uint64, keyTotalLen uint32) int {
	flags = effectiveCellFlags(flags, value)
	buf[off] = flags
	off++
	le.PutUint16(buf[off:], uint16(len(key)))
	off += 2
	// ValueLen (inline / subpage forms) precedes the key bytes — the
	// decode-speed field ordering. The key half then ends with the
	// 12-byte key-extent reference for overflow-key cells
	// (page-formats.md §Overflow-Key Cells); the value half follows
	// unchanged in form.
	hasValueLen := !cellHasTrailerOnly(flags) && flags&CellFlagEmptyValue == 0
	if hasValueLen {
		le.PutUint32(buf[off:], uint32(len(value)))
		off += 4
	}
	copy(buf[off:], key)
	off += len(key)
	if flags&CellFlagOverflowKey != 0 {
		le.PutUint64(buf[off:], keyExtPage)
		off += 8
		le.PutUint32(buf[off:], keyTotalLen)
		off += 4
	}
	switch {
	case cellHasTrailerOnly(flags):
		le.PutUint64(buf[off:], ovflPage)
		off += 8
		le.PutUint64(buf[off:], totalLen)
		off += 8
	case flags&CellFlagEmptyValue != 0:
		// no value half
	default:
		copy(buf[off:], value)
		off += len(value)
	}
	return off
}

// segValueContentSize returns the value-region byte size of an entry's
// content: the 12-byte key-extent reference (overflow-key cells), then
// the 16-byte trailer (overflow / nested-tree) or the raw value bytes
// (inline / subpage; empty ⇒ zero — no EmptyValue form in the
// segregated layout, emptiness is derived).
func segValueContentSize(flags uint8, value []byte) int {
	n := 0
	if flags&CellFlagOverflowKey != 0 {
		n += 12
	}
	if cellHasTrailerOnly(flags) {
		return n + 16
	}
	return n + len(value)
}

// addSegEntry writes a segregated entry: the stream half (headers +
// key bytes) into buf at dataPos with a placeholder VOff, the value
// content into the segVals accumulator. The group decision — restart
// at target / natural break / overflow-key singleton — is IDENTICAL to
// addCompressedEntry's, so the two compressed layouts group the same
// input identically.
func (b *LeafBuilder) addSegEntry(key []byte, flags uint8, value []byte, ovflPage, totalLen uint64, keyExtPage uint64, keyTotalLen uint32) bool {
	// The segregated layout has no EmptyValue form (page-formats.md
	// §Segregated Leaf) — normalize entries decoded from interleaved
	// pages (variant migration) rather than reject them.
	flags &^= CellFlagEmptyValue
	target := int(b.cfg.EffectiveRestartGroupTarget())
	ovk := flags&CellFlagOverflowKey != 0

	atTarget := b.rgt.IsRestart(b.count, target)
	naturalBreak := false
	if !atTarget && !ovk && !b.forceRestart && b.count > 0 && b.rgt.CurGroupCount() > 0 {
		if sharedPrefixLen(b.prevKey, key) == 0 {
			naturalBreak = true
		}
	}
	isRestart := atTarget || naturalBreak || ovk || b.forceRestart
	if isRestart && b.rgt.CurGroupCount() > 0 {
		b.rgt.FinalizeCurrentGroup()
	}

	var sharedLen int
	unsharedKey := key
	if !isRestart {
		sharedLen = sharedPrefixLen(b.prevKey, key)
		unsharedKey = key[sharedLen:]
	}

	// Stream size: restart [Flags][KeyLen][VOff][Key] = 5 + len(key);
	// delta [Flags][Shared][Unshared][VOff][UnsharedKey] = 7 + unshared.
	var entrySize int
	if isRestart {
		entrySize = 5 + len(key)
	} else {
		entrySize = 7 + len(unsharedKey)
	}
	valSize := segValueContentSize(flags, value)

	extraRestart := 0
	if isRestart {
		extraRestart = 1
	}
	tableSize := b.rgt.TableSize(extraRestart)
	// Regions must not collide: stream forward from dataPos, value
	// region + table backward from ContentEnd.
	if b.dataPos+entrySize+len(b.segVals)+valSize+tableSize > b.cfg.ContentEnd() {
		return false
	}

	if isRestart {
		b.rgt.StartGroup(b.dataPos)
	}

	// Stream half. VOff is a placeholder patched at Finish (the
	// end-anchored region position needs the final table size).
	off := b.dataPos
	b.buf[off] = flags
	off++
	if isRestart {
		le.PutUint16(b.buf[off:], uint16(len(key)))
		off += 2
		b.segVOffSlots = append(b.segVOffSlots, int32(off))
		le.PutUint16(b.buf[off:], 0)
		off += 2
		copy(b.buf[off:], key)
		off += len(key)
	} else {
		le.PutUint16(b.buf[off:], uint16(sharedLen))
		off += 2
		le.PutUint16(b.buf[off:], uint16(len(unsharedKey)))
		off += 2
		b.segVOffSlots = append(b.segVOffSlots, int32(off))
		le.PutUint16(b.buf[off:], 0)
		off += 2
		copy(b.buf[off:], unsharedKey)
		off += len(unsharedKey)
	}

	// Value content, in add order.
	b.segRel = append(b.segRel, uint32(len(b.segVals)))
	if flags&CellFlagOverflowKey != 0 {
		var ref [12]byte
		le.PutUint64(ref[:], keyExtPage)
		le.PutUint32(ref[8:], keyTotalLen)
		b.segVals = append(b.segVals, ref[:]...)
	}
	if cellHasTrailerOnly(flags) {
		var tr [16]byte
		le.PutUint64(tr[:], ovflPage)
		le.PutUint64(tr[8:], totalLen)
		b.segVals = append(b.segVals, tr[:]...)
	} else {
		b.segVals = append(b.segVals, value...)
	}

	b.dataPos = off
	b.prevKey = append(b.prevKey[:0], key...)
	b.count++
	b.rgt.IncrCount()
	b.forceRestart = ovk
	return true
}

// addCompressedEntry writes a compressed entry — restart or delta as
// determined by the restart-group tracker. Variable-group natural-break
// heuristic: when SharedLen would be zero with the previous key (no
// compression benefit) and the current group is non-empty, force a new
// group early so the delta-header overhead doesn't accrue on entries
// that gain nothing from sharing. This is the "natural break" policy
// described in page-formats.md §Compressed Leaf.
func (b *LeafBuilder) addCompressedEntry(key []byte, flags uint8, value []byte, ovflPage, totalLen uint64, keyExtPage uint64, keyTotalLen uint32) bool {
	target := int(b.cfg.EffectiveRestartGroupTarget())
	ovk := flags&CellFlagOverflowKey != 0

	// Decide if this entry must start a new group. Overflow-key
	// entries are ALWAYS restart entries in singleton groups
	// (page-formats.md §Overflow-Key Cells): the entry itself forces
	// a restart, and forceRestart (set below) makes the FOLLOWING
	// entry restart too, so no delta ever chains through an
	// extent-resident key.
	atTarget := b.rgt.IsRestart(b.count, target)
	naturalBreak := false
	if !atTarget && !ovk && !b.forceRestart && b.count > 0 && b.rgt.CurGroupCount() > 0 {
		// Compute SharedLen against the previous key cheaply. If
		// it's zero, force a new group (avoid spending 2 extra
		// bytes per delta entry on no-shared-prefix keys when we
		// could pay 0 extra by starting fresh).
		if sharedPrefixLen(b.prevKey, key) == 0 {
			naturalBreak = true
		}
	}
	isRestart := atTarget || naturalBreak || ovk || b.forceRestart

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
		headerSize += keyPartSize(flags, key) // KeyLen + Key (+ key-extent ref)
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
		off = writeFullKeyEntry(b.buf, b.dataPos, flags, key, value, ovflPage, totalLen, keyExtPage, keyTotalLen)
	} else {
		off = writeCompressedDeltaEntry(b.buf, b.dataPos, flags, sharedLen, unsharedKey, value, ovflPage, totalLen)
	}

	b.dataPos = off
	b.prevKey = append(b.prevKey[:0], key...)
	b.count++
	b.rgt.IncrCount()
	// Seal the singleton: the next entry must open a fresh group so
	// it never deltas against this entry's resident-prefix key.
	b.forceRestart = ovk
	return true
}

// writeCompressedDeltaEntry writes a delta (prefix-compressed) compressed
// entry at off and returns the offset just past it. Byte layout per
// page-formats.md §Compressed Leaf delta entry — gmdb's
// [Flags][SharedLen][UnsharedLen][ValueLen][UnsharedKey][Value] order
// (ValueLen before the unshared key), with the 16-byte trailer form for
// overflow / nested-tree cells. ovflPage / totalLen are the generic
// trailer pair as in writeFullKeyEntry.
//
// Shared by LeafBuilder.addCompressedEntry and the in-place splice helpers.
func writeCompressedDeltaEntry(buf []byte, off int, flags uint8, sharedLen int, unsharedKey, value []byte, ovflPage, totalLen uint64) int {
	flags = effectiveCellFlags(flags, value)
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
	if flags&CellFlagEmptyValue != 0 {
		copy(buf[off:], unsharedKey)
		return off + len(unsharedKey)
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
	switch b.variant {
	case TypeLeafUncompressed:
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

	case TypeLeafSegregated:
		// Restart table first (fixes the table base), then the value
		// region packed flush against it (the canonical end-anchored
		// form: ValueEnd == table base; empty page: ValueEnd == entry
		// start), then the VOff patch pass over the recorded slots.
		contentEnd := b.cfg.ContentEnd()
		rc := b.rgt.WriteTable(b.buf, contentEnd)
		valueEnd := contentEnd - rc*restartTableEntrySize
		if b.count == 0 {
			valueEnd = segLeafEntryStart
		}
		valueStart := valueEnd - len(b.segVals)
		copy(b.buf[valueStart:valueEnd], b.segVals)
		for i, slot := range b.segVOffSlots {
			le.PutUint16(b.buf[slot:], uint16(valueStart+int(b.segRel[i])))
		}
		zeroFreeSpace(b.buf, b.dataPos, valueStart)
		WriteHeader(b.buf, TypeLeafSegregated, count, 0)
		le.PutUint16(b.buf[leafOffRestartCount:], uint16(rc))
		le.PutUint16(b.buf[leafOffDataEnd:], uint16(b.dataPos))
		le.PutUint16(b.buf[segLeafOffValueEnd:], uint16(valueEnd))
		return count

	default:
		// Interleaved compressed: write restart table first, then
		// header (so RestartCount is known and stored).
		rc := b.rgt.WriteTable(b.buf, b.cfg.ContentEnd())
		zeroFreeSpace(b.buf, b.dataPos, b.cfg.ContentEnd()-rc*restartTableEntrySize)
		WriteHeader(b.buf, TypeLeaf, count, 0)
		le.PutUint16(b.buf[leafOffRestartCount:], uint16(rc))
		le.PutUint16(b.buf[leafOffDataEnd:], uint16(b.dataPos))
		return count
	}
}

// Count returns the number of entries added so far.
func (b *LeafBuilder) Count() int { return b.count }

// FreeSpace returns the remaining usable bytes available for one more
// entry. Used by bulkload + split heuristics to decide when to close a
// page.
func (b *LeafBuilder) FreeSpace() int {
	switch b.variant {
	case TypeLeafUncompressed:
		return b.cfg.ContentEnd() - b.dataPos - b.count*ucOffsetEntrySize
	case TypeLeafSegregated:
		return b.cfg.ContentEnd() - b.dataPos - len(b.segVals) - b.rgt.TableSize(0)
	default:
		return b.cfg.ContentEnd() - b.dataPos - b.rgt.TableSize(0)
	}
}

// effectiveCellFlags upgrades a plain inline cell with an empty value
// to the compact empty-value form (page-formats.md §Leaf Page,
// empty-value cell). Applied identically by the entry writers and the
// size projection, so a projected size always matches the bytes
// written. Flagged cells (trailer, subpage) keep their own forms.
func effectiveCellFlags(flags uint8, value []byte) uint8 {
	if flags&^CellFlagOverflowKey == 0 && len(value) == 0 {
		// Plain inline cell (with or without the key-half OverflowKey
		// bit) and an empty value → compact empty-value form.
		return flags | CellFlagEmptyValue
	}
	if flags&CellFlagEmptyValue != 0 && len(value) != 0 {
		// Writing value bytes under the no-value-half form would drop
		// them silently — same misuse class as Overflow|MultiValue,
		// same response.
		panic(fmt.Sprintf("page: CellFlagEmptyValue with %d value bytes — the empty-value form carries no value half", len(value)))
	}
	return flags
}

// valuePartSize returns the on-page byte size of an entry's value half.
// Both overflow references and nested-tree references use the 16-byte
// trailer form (no ValueLen prefix) per page-formats.md §Leaf Page;
// inline and subpage cells carry [ValueLen u32][bytes]; empty-value
// cells carry nothing.
func valuePartSize(flags uint8, value []byte) int {
	flags = effectiveCellFlags(flags, value)
	if cellHasTrailerOnly(flags) {
		return 8 + 8 // u64 + u64 (OvflPage|Root + TotalLen|Count)
	}
	if flags&CellFlagEmptyValue != 0 {
		return 0
	}
	return 4 + len(value) // ValueLen uint32 + value bytes
}

// keyPartSize returns the on-page byte size of a full-key entry's key
// half past the CellFlags byte: KeyLen(u16) + key bytes, plus the
// 12-byte key-extent reference for overflow-key cells (page-formats.md
// §Overflow-Key Cells). Delta entries have their own key-half math and
// never carry an overflow key.
func keyPartSize(flags uint8, key []byte) int {
	n := 2 + len(key)
	if flags&CellFlagOverflowKey != 0 {
		n += 12
	}
	return n
}

// zeroFreeSpace clears bytes in [lo, hi). Mirrors the
// "clear(buf)-before-encode" behavior so two encoders for the same input
// produce byte-identical pages — the deterministic-encoding invariant
// per page-formats.md §Leaf Split.
func zeroFreeSpace(buf []byte, lo, hi int) {
	if lo < hi {
		clear(buf[lo:hi])
	}
}
