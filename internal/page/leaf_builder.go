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

// AddInline appends an inline (key, value) entry. Returns false if the
// page is full (caller decides to split or finish + start a new page).
// Panics on out-of-order key (debug assertion — pre-sorting is the
// caller's responsibility).
func (b *LeafBuilder) AddInline(key, value []byte) bool {
	return b.addEntry(key, 0, value, 0, 0)
}

// AddOverflow appends an overflow-reference entry. ovflPage is the first
// page ID of the overflow run; totalLen is the assembled value size.
// Returns false on page-full.
func (b *LeafBuilder) AddOverflow(key []byte, ovflPage, totalLen uint64) bool {
	return b.addEntry(key, CellFlagOverflow, nil, ovflPage, totalLen)
}

// AddEntry dispatches by cellFlags. Convenience for callers that already
// hold a LeafEntry (e.g., the split-helper that copies entries between
// pages).
func (b *LeafBuilder) AddEntry(e LeafEntry) bool {
	if e.Flags&CellFlagOverflow != 0 {
		return b.AddOverflow(e.Key, e.OverflowPage, e.TotalLen)
	}
	return b.AddInline(e.Key, e.Value)
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
	if flags&CellFlagOverflow != 0 {
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

	// Write the entry.
	off := b.dataPos
	b.buf[off] = flags
	off++

	if isRestart {
		le.PutUint16(b.buf[off:], uint16(len(key)))
		off += 2
		if flags&CellFlagOverflow != 0 {
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
	} else {
		le.PutUint16(b.buf[off:], uint16(sharedLen))
		off += 2
		le.PutUint16(b.buf[off:], uint16(len(unsharedKey)))
		off += 2
		if flags&CellFlagOverflow != 0 {
			copy(b.buf[off:], unsharedKey)
			off += len(unsharedKey)
			le.PutUint64(b.buf[off:], ovflPage)
			off += 8
			le.PutUint64(b.buf[off:], totalLen)
			off += 8
		} else {
			le.PutUint32(b.buf[off:], uint32(len(value)))
			off += 4
			copy(b.buf[off:], unsharedKey)
			off += len(unsharedKey)
			copy(b.buf[off:], value)
			off += len(value)
		}
	}

	b.dataPos = off
	b.prevKey = append(b.prevKey[:0], key...)
	b.count++
	b.rgt.IncrCount()
	return true
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
func valuePartSize(flags uint8, value []byte) int {
	if flags&CellFlagOverflow != 0 {
		return 8 + 8 // OvflPage + TotalLen
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
