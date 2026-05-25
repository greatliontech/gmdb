package page

// Leaf-page formats per page-formats.md §Leaf Page. Two variants share the
// 8-byte common header and the "entries forward / lookup table backward"
// layout; only the per-entry encoding and lookup machinery differ:
//
//   - TypeLeaf (compressed): variable-size restart groups, prefix-compressed
//     delta entries within each group. Format details in leaf_compressed.go.
//   - TypeLeafUncompressed: full keys per entry, positional offset table.
//     Format details in leaf_uncompressed.go.
//
// The dispatch surface is LeafReader (read-side) and LeafBuilder
// (write-side). Both inspect the page's Type byte and route to the
// variant-specific implementation. The btree consumer uses LeafReader /
// LeafBuilder / LeafIter exclusively — never the variant-specific helpers
// directly.

import (
	"bytes"
	"errors"
	"fmt"
)

// Common header offsets for both leaf variants. The 4 bytes after the
// 8-byte common page header are variant-specific (DataEnd + (RestartCount
// or Reserved)), but the entry-data region always starts at offset 12.
const (
	// leafEntryStart is the byte offset where leaf entries begin in
	// either variant. Holding this constant across variants lets the
	// split helpers + Check() walk per-entry without per-variant offset
	// branches.
	leafEntryStart = HeaderSize + 4 // 12

	// Per-variant header field offsets (within the 4 bytes after the
	// common 8-byte header).
	leafOffRestartCount = HeaderSize     // compressed: RestartCount uint16
	leafOffDataEnd      = HeaderSize + 2 // both:       DataEnd uint16
	ucLeafOffDataEnd    = HeaderSize     // uncompressed: DataEnd uint16 (no RestartCount)

	// Offset-table entry size for uncompressed leaves (positional table).
	ucOffsetEntrySize = 2
)

// CellFlags bit assignments per page-formats.md §Leaf Page.
const (
	CellFlagOverflow   uint8 = 1 << 0
	CellFlagMultiValue uint8 = 1 << 1
	CellFlagNestedTree uint8 = 1 << 2

	// cellFlagKnownMask is the union of currently-defined cell flag
	// bits. The strict-reject rule from file-layout.md §Reserved-byte
	// policy is enforced via LeafReader.Validate (not in the hot-path
	// decoders, which assume well-formed input); see Validate's doc
	// for the boundary discipline.
	cellFlagKnownMask = CellFlagOverflow | CellFlagMultiValue | CellFlagNestedTree
)

// ErrCorrupted is the leaf-decoder corruption sentinel. Wraps a
// human-readable description of the structural fault (out-of-bounds
// length, unknown flag bit, restart-table Count=0, etc.). The btree
// caller maps to btree.ErrCorrupted at its boundary.
var ErrCorrupted = errors.New("page: leaf structural corruption")

// LeafEntry holds the decoded fields of one leaf entry. Used by both the
// compressed and uncompressed variants and by the Iter / Search return
// types. Slice ownership:
//
//   - Key: backed by the page buffer for restart entries in compressed
//     leaves and for all entries in uncompressed leaves; backed by the
//     iterator's keyBuf for compressed delta entries (since the full key
//     must be reconstructed). Caller MUST NOT retain past the next
//     iterator move or leaf transition.
//   - Value: backed by the page buffer for inline (Flags == 0) entries
//     and for subpage cells (Flags == CellFlagMultiValue, where Value
//     holds the raw subpage bytes); nil for overflow and nested-tree
//     reference entries (consult OverflowPage + TotalLen or
//     NestedRoot + NestedCount respectively).
//
// On-disk vs in-memory: Overflow and NestedTree share the same wire
// shape (`[Flags][KeyLen][Key][u64][u64]` for restart/uncompressed;
// `[Flags][SharedLen][UnsharedLen][UnsharedKey][u64][u64]` for delta) —
// the SetKeyspace nested-tree reference (`set-keyspace.md §Nested
// B+tree Reference Cell`) intentionally reuses the overflow wire
// format because both have a 16-byte trailer and no `ValueLen` prefix.
// The decoded view splits the trailer into distinct field pairs by
// CellFlags so callers reading e.OverflowPage on a NestedTree cell
// (or vice versa) get a zero value rather than a silent
// misinterpretation.
type LeafEntry struct {
	Flags uint8
	Key   []byte
	Value []byte

	// Overflow-cell fields (valid iff Flags has CellFlagOverflow set
	// and CellFlagMultiValue clear). For other flag combinations
	// these are zero.
	OverflowPage uint64
	TotalLen     uint64

	// NestedTree-cell fields (valid iff Flags has both
	// CellFlagMultiValue and CellFlagNestedTree set). NestedRoot is
	// the page ID of the nested B+tree's root; NestedCount is the
	// number of values in the set (the O(1) Count surfaced by
	// SetKeyspace.CountValues). For other flag combinations these
	// are zero.
	NestedRoot  uint64
	NestedCount uint64
}

// IsOverflow reports whether the entry's value lives in an overflow run
// rather than inline on the leaf. Mutually exclusive with IsSubpage
// and IsNestedTree at the spec level (page-formats.md §Leaf Page
// CellFlags bit layout); validateCellFlagsCombo enforces this at the
// Validate boundary.
func (e LeafEntry) IsOverflow() bool { return e.Flags&CellFlagOverflow != 0 }

// IsSubpage reports whether the entry holds an inline SetKeyspace
// subpage (CellFlagMultiValue set, CellFlagNestedTree clear). The
// subpage bytes occupy e.Value.
func (e LeafEntry) IsSubpage() bool {
	return e.Flags&CellFlagMultiValue != 0 && e.Flags&CellFlagNestedTree == 0
}

// IsNestedTree reports whether the entry is a SetKeyspace nested-tree
// reference (both CellFlagMultiValue and CellFlagNestedTree set).
// The nested tree's root page ID is e.NestedRoot; the cached member
// count is e.NestedCount.
func (e LeafEntry) IsNestedTree() bool {
	return e.Flags&CellFlagMultiValue != 0 && e.Flags&CellFlagNestedTree != 0
}

// LeafReader provides read access over both compressed and uncompressed
// leaf pages. Constructed once per Page-resolution boundary in the btree
// descent; cheap by value, holds only a slice header into the page buffer
// plus precomputed extents.
type LeafReader struct {
	buf        []byte
	cfg        Config
	count      int
	dataEnd    int
	compressed bool
	// Compressed variant only:
	rt restartTable
	// Uncompressed variant only:
	ucTableOff int // byte offset of the positional offset table
}

// NewLeafReader wraps buf as a leaf page reader. It dispatches on the
// page's Type byte and initializes variant-specific state. Panics on a
// non-leaf type (programming error at the call site — callers gate via
// IsLeafType or read the header themselves).
func NewLeafReader(buf []byte, cfg Config) LeafReader {
	cfg.mustValidate()
	if len(buf) != int(cfg.PageSize) {
		panic(fmt.Sprintf("page: NewLeafReader buf len %d != PageSize %d", len(buf), cfg.PageSize))
	}
	typ, _, count, _ := ReadHeader(buf)
	if !IsLeafType(typ) {
		panic(fmt.Sprintf("page: NewLeafReader on non-leaf type %d", typ))
	}
	contentEnd := cfg.ContentEnd()
	if typ == TypeLeafUncompressed {
		return LeafReader{
			buf:        buf,
			cfg:        cfg,
			count:      int(count),
			dataEnd:    int(le.Uint16(buf[ucLeafOffDataEnd:])),
			compressed: false,
			ucTableOff: contentEnd - int(count)*ucOffsetEntrySize,
		}
	}
	rc := int(le.Uint16(buf[leafOffRestartCount:]))
	return LeafReader{
		buf:        buf,
		cfg:        cfg,
		count:      int(count),
		dataEnd:    int(le.Uint16(buf[leafOffDataEnd:])),
		compressed: true,
		rt:         newRestartTable(buf, rc, contentEnd),
	}
}

// Buf returns the underlying page buffer for callers that need to perform
// further page-level inspection (e.g. ReadHeader for type-byte checks in a
// generic walker). The returned slice is borrowed; do not retain past the
// reader's lifetime.
func (r LeafReader) Buf() []byte { return r.buf }

// Compressed reports whether this leaf is a compressed (variable-restart-
// groups, delta-encoded) page. False ⇒ uncompressed (positional offset
// table, full keys).
func (r LeafReader) Compressed() bool { return r.compressed }

// Count returns the number of entries in the leaf.
func (r LeafReader) Count() int { return r.count }

// DataEnd returns the byte offset after the last entry's data. Useful for
// fit checks and free-space computation in the in-place splice helpers.
func (r LeafReader) DataEnd() int { return r.dataEnd }

// RestartCount returns the number of restart groups. For uncompressed
// leaves every entry is its own "group" (a positional slot), so this
// returns Count(); the abstraction lets generic walkers iterate
// group-by-group regardless of variant.
func (r LeafReader) RestartCount() int {
	if !r.compressed {
		return r.count
	}
	return r.rt.RestartCount()
}

// GroupEntryCount returns the number of entries in the i-th restart group.
// For uncompressed leaves every group has exactly 1 entry.
func (r LeafReader) GroupEntryCount(i int) int {
	if !r.compressed {
		return 1
	}
	return r.rt.GroupEntryCount(i)
}

// SearchLeaf performs a key lookup against the leaf. Returns the entry's
// absolute index, the entry (with Key always nil — callers either had the
// target already or don't need the key bytes back), and whether the key
// was found. On miss, index is the insertion point (the index of the
// smallest key strictly greater than target, or Count() if every entry is
// less than target). Same robustness contract as the per-variant
// decoders: total over input; errors flow through return-error variants
// (TODO: chunk-4.6γ will wire an err-returning sibling).
func (r LeafReader) SearchLeaf(target []byte) (index int, entry LeafEntry, found bool) {
	if r.count == 0 {
		return 0, LeafEntry{}, false
	}
	if !r.compressed {
		return r.ucSearchLeaf(target)
	}
	return r.compressedSearchLeaf(target)
}

// SearchLeafIter is the cursor-friendly form of SearchLeaf: returns the
// lookup result plus a LeafIter whose next Next() call returns the entry
// immediately AFTER the returned found/successor entry (i.e. positioned
// past the result, ready to stream forward). Carries delta-decode state
// accumulated during the scan so the cursor's Seek/SeekGE avoid a second
// group walk per page-formats.md §Leaf Lookup.
//
// keyBuf / bufKeys / bufEnts are caller-supplied scratch buffers reused by
// the iter; pass the cursor's previously-returned KeyBuf/BufKeys/BufEnts
// so allocation amortizes to zero across leaf transitions in the steady-
// state cursor loop.
func (r LeafReader) SearchLeafIter(target, keyBuf, bufKeys []byte, bufEnts []LeafEntry) (int, LeafEntry, bool, LeafIter) {
	if r.count == 0 {
		return 0, LeafEntry{}, false, LeafIter{
			r:       r,
			keyBuf:  keyBuf[:0],
			bufKeys: bufKeys[:0],
			bufEnts: bufEnts[:0],
		}
	}
	if !r.compressed {
		return r.ucSearchLeafIter(target, keyBuf, bufKeys, bufEnts)
	}
	return r.compressedSearchLeafIter(target, keyBuf, bufKeys, bufEnts)
}

// EntryAt decodes the entry at absolute index idx. For compressed leaves
// this walks the entry's containing group from its restart point (O(K));
// hot callers should use a LeafIter and call At through that to amortize
// across multiple in-group hits. keyBuf is scratch storage for the
// reconstructed key (compressed only); pass the cursor's keyBuf to avoid
// per-call allocation. The returned entry's Key may alias keyBuf or the
// page buffer.
func (r LeafReader) EntryAt(idx int, keyBuf []byte) (LeafEntry, []byte) {
	if idx < 0 || idx >= r.count {
		panic(fmt.Sprintf("page: LeafReader.EntryAt %d out of range [0, %d)", idx, r.count))
	}
	if !r.compressed {
		e, _ := r.ucDecodeEntry(r.ucOffset(idx))
		return e, keyBuf
	}
	return r.compressedEntryAt(idx, keyBuf)
}

// LastKey returns the key of the last entry in the leaf, doing the
// minimum decoding required (skip values; reconstruct deltas only for
// the final group on compressed pages). Useful for the split-time
// boundary-key reconstruction described in page-formats.md §Leaf Split.
// keyBuf is scratch for delta-key reconstruction (compressed only); the
// returned key may alias keyBuf or the page buffer.
func (r LeafReader) LastKey(keyBuf []byte) ([]byte, []byte) {
	if r.count == 0 {
		return nil, keyBuf
	}
	if !r.compressed {
		off := r.ucOffset(r.count - 1)
		flags := r.buf[off]
		off++
		keyLen := int(le.Uint16(r.buf[off:]))
		off += 2
		if !cellHasTrailerOnly(flags) {
			off += 4 // skip ValueLen uint32 — Key follows
		}
		return r.buf[off : off+keyLen], keyBuf
	}
	return r.compressedLastKey(keyBuf)
}

// cellHasTrailerOnly reports whether the entry's value half is the
// 16-byte trailer form (overflow run reference OR nested-tree
// reference) — both shapes elide the `ValueLen u32` prefix. Centralised
// here so FirstKey / LastKey and the splice helpers don't have to
// re-derive the condition.
func cellHasTrailerOnly(flags uint8) bool {
	return flags&CellFlagOverflow != 0 || flags&CellFlagNestedTree != 0
}

// FirstKey returns the key of the first entry. Both variants store the
// first entry's full key inline (compressed: at the first restart point;
// uncompressed: at offset 0 of the entry-data region), so this is a
// constant-time slice into the page buffer regardless of variant.
func (r LeafReader) FirstKey() []byte {
	if r.count == 0 {
		return nil
	}
	flags := r.buf[leafEntryStart]
	off := leafEntryStart
	off++ // skip CellFlags
	keyLen := int(le.Uint16(r.buf[off:]))
	off += 2
	// Trailer-only forms (overflow + nested-tree) elide the
	// ValueLen u32 prefix and place the key immediately after KeyLen;
	// inline cells (plain + subpage) carry ValueLen between KeyLen
	// and Key.
	if !cellHasTrailerOnly(flags) {
		off += 4
	}
	return r.buf[off : off+keyLen]
}

// FreeSpace returns the byte count of unused space between the
// entry-data tail and the lookup-table start. Used by the in-place
// splice helpers to fit-check appends and inserts. Negative values are
// not reachable from a well-formed page; the helpers gate their use on
// the returned value being nonnegative.
func (r LeafReader) FreeSpace() int {
	if !r.compressed {
		return r.cfg.ContentEnd() - r.dataEnd - r.count*ucOffsetEntrySize
	}
	return r.cfg.ContentEnd() - r.dataEnd - r.rt.RestartCount()*restartTableEntrySize
}

// Validate walks the page's structural surface and returns ErrCorrupted
// (wrapped with context) on any spec invariant violation. Total over
// its input: any byte sequence within `r.buf` either returns a clean
// ErrCorrupted or nil — never panics on slice-out-of-range. This is
// the load-bearing contract for the btree-level pager.Page boundary
// and Check(), both of which feed arbitrary on-disk pages here.
//
// Checks performed:
//
//   - Compressed: restart-table fits within the content area; every
//     restart-table entry has `Count >= 1` (per page-formats.md
//     §Compressed Leaf — `Count == 0` leaves the next group's start
//     ambiguous since variable-group reads sum counts to derive group
//     ranges); the sum of group counts equals the page header's Count.
//   - Both variants: per-entry walk with bounds-checked field reads;
//     CellFlags has no bits outside cellFlagKnownMask (per
//     file-layout.md §Reserved-byte policy — strict-reject for flag
//     bits); declared lengths fit within the entry data region
//     `[leafEntryStart, dataEnd)`.
//
// NewLeafReader is intentionally NOT a validation boundary: it
// initializes per-variant metadata cheaply (O(1)) so cursor hot-path
// re-construction doesn't pay a per-page walk. Callers that resolve a
// page from disk for the first time (the btree's pager.Page boundary,
// or Check()) should call Validate to surface corruption before
// invoking the read paths (SearchLeaf, Iter, EntryAt), which assume
// structural validity and either panic or decode garbage on malformed
// input. The internal decoder helpers (decodeRestartEntry,
// decodeDeltaEntry, ucDecodeEntry) are NOT bounds-checked because
// they're on the hot lookup path; Validate's own walk uses checked
// sibling helpers (validateRestartEntry, validateDeltaEntry,
// validateUCEntry) defined below.
func (r LeafReader) Validate() error {
	contentEnd := r.cfg.ContentEnd()
	if r.dataEnd < leafEntryStart || r.dataEnd > contentEnd {
		return fmt.Errorf("%w: leaf DataEnd %d outside [%d, %d]", ErrCorrupted, r.dataEnd, leafEntryStart, contentEnd)
	}
	if r.compressed {
		rc := r.rt.RestartCount()
		// Restart table must fit between DataEnd and ContentEnd.
		tableBytes := rc * restartTableEntrySize
		if r.dataEnd+tableBytes > contentEnd {
			return fmt.Errorf("%w: compressed leaf restart table (RestartCount=%d, %d bytes) overlaps DataEnd %d / ContentEnd %d",
				ErrCorrupted, rc, tableBytes, r.dataEnd, contentEnd)
		}
		if rc == 0 && r.count > 0 {
			return fmt.Errorf("%w: compressed leaf has %d entries but 0 restart groups", ErrCorrupted, r.count)
		}
		// Count == 0 forbidden; sum-of-Counts must equal r.count.
		sum := 0
		for g := range rc {
			c := r.rt.GroupEntryCount(g)
			if c == 0 {
				return fmt.Errorf("%w: compressed leaf restart group %d has Count=0 (spec invariant)", ErrCorrupted, g)
			}
			sum += c
			// Restart-table Offset must point within the entry-data
			// region.
			off := r.rt.Offset(g)
			if off < leafEntryStart || off >= r.dataEnd {
				return fmt.Errorf("%w: compressed leaf restart[%d] offset %d outside [%d, %d)", ErrCorrupted, g, off, leafEntryStart, r.dataEnd)
			}
		}
		if sum != r.count {
			return fmt.Errorf("%w: compressed leaf sum-of-group-counts %d != header Count %d", ErrCorrupted, sum, r.count)
		}
	} else {
		// Uncompressed: offset table must fit; check below in the
		// per-entry walk validates each offset is in-range.
		tableBytes := r.count * ucOffsetEntrySize
		if r.dataEnd+tableBytes > contentEnd {
			return fmt.Errorf("%w: uncompressed leaf offset table (Count=%d, %d bytes) overlaps DataEnd %d / ContentEnd %d",
				ErrCorrupted, r.count, tableBytes, r.dataEnd, contentEnd)
		}
	}

	// Per-entry walks. Both branches use bounds-checked helpers and
	// surface ErrCorrupted on any over-read.
	if r.count == 0 {
		return nil
	}
	if !r.compressed {
		for i := range r.count {
			off := r.ucOffset(i)
			if off < leafEntryStart || off >= r.dataEnd {
				return fmt.Errorf("%w: uncompressed leaf offset[%d]=%d outside [%d, %d)", ErrCorrupted, i, off, leafEntryStart, r.dataEnd)
			}
			if err := r.validateUCEntry(off); err != nil {
				return fmt.Errorf("%w: uncompressed leaf entry %d: %w", ErrCorrupted, i, err)
			}
		}
		return nil
	}
	// Compressed: walk by restart groups using validated decoders.
	for g := range r.rt.RestartCount() {
		gc := r.rt.GroupEntryCount(g)
		off := r.rt.Offset(g)
		for i := range gc {
			if off > r.dataEnd {
				return fmt.Errorf("%w: compressed leaf entry walk ran past DataEnd at group %d entry %d", ErrCorrupted, g, i)
			}
			var next int
			var err error
			if i == 0 {
				next, err = r.validateRestartEntry(off)
			} else {
				next, err = r.validateDeltaEntry(off)
			}
			if err != nil {
				return fmt.Errorf("%w: compressed leaf group %d entry %d: %w", ErrCorrupted, g, i, err)
			}
			off = next
		}
	}
	return nil
}

// validateCellFlagsCombo rejects flag combinations that have no defined
// on-disk encoding (the cellFlagKnownMask check only catches unknown
// bits, not illegal combinations of known bits):
//
//   - CellFlagOverflow | CellFlagMultiValue: `page-formats.md §Leaf
//     Page (CellFlags bit layout)` declares these mutually exclusive
//     in practice; no encoding exists for the combination.
//   - CellFlagNestedTree without CellFlagMultiValue: the NestedTree
//     bit is only meaningful when MultiValue is also set
//     (see this file's `CellFlags bit layout` doc: NestedTree is
//     defined as "only when Bit 1 set: 0 = subpage, 1 = nested
//     B+tree"). NestedTree alone is structurally invalid.
//
// Caller wraps with the per-variant structural context. Centralised
// here so all three Validate paths (restart / delta / uncompressed)
// enforce the same combination contract, keeping `Validate` as the
// trust boundary that `LeafBuilder.AddEntry` and downstream callers
// rely on (`AddEntry` panics on these same combinations; without
// this Validate gate, a corrupted on-disk page passing Validate
// would panic-the-process mid-rebuild instead of returning
// ErrCorrupted at the boundary).
func validateCellFlagsCombo(flags uint8) error {
	if flags&CellFlagOverflow != 0 && flags&CellFlagMultiValue != 0 {
		return fmt.Errorf("CellFlags 0x%x sets both Overflow and MultiValue (mutually exclusive)", flags)
	}
	if flags&CellFlagNestedTree != 0 && flags&CellFlagMultiValue == 0 {
		return fmt.Errorf("CellFlags 0x%x sets NestedTree without MultiValue (only valid when MultiValue is set)", flags)
	}
	return nil
}

// validateRestartEntry checks a restart entry at off and returns the
// offset of the next entry. Returns a non-nil error (without
// ErrCorrupted wrap — the caller wraps with structural context) on any
// bounds violation or unknown CellFlags.
func (r LeafReader) validateRestartEntry(off int) (int, error) {
	if err := r.ensureBytes(off, 1); err != nil {
		return 0, err
	}
	flags := r.buf[off]
	if flags&^cellFlagKnownMask != 0 {
		return 0, fmt.Errorf("unknown CellFlags 0x%x", flags&^cellFlagKnownMask)
	}
	if err := validateCellFlagsCombo(flags); err != nil {
		return 0, err
	}
	off++
	if err := r.ensureBytes(off, 2); err != nil {
		return 0, err
	}
	keyLen := int(le.Uint16(r.buf[off:]))
	off += 2
	if cellHasTrailerOnly(flags) {
		// Overflow: [Flags][KeyLen][Key][OvflPage u64][TotalLen u64].
		// NestedTree: [Flags][KeyLen][Key][Root u64][Count u64].
		// Identical wire shape; the trailer is always 16 bytes.
		if err := r.ensureBytes(off, keyLen+16); err != nil {
			return 0, fmt.Errorf("restart trailer body: %w", err)
		}
		return off + keyLen + 16, nil
	}
	// [Flags][KeyLen][ValueLen][Key][Value]
	if err := r.ensureBytes(off, 4); err != nil {
		return 0, err
	}
	valLen := int(le.Uint32(r.buf[off:]))
	off += 4
	if err := r.ensureBytes(off, keyLen+valLen); err != nil {
		return 0, fmt.Errorf("restart inline body keyLen=%d valLen=%d: %w", keyLen, valLen, err)
	}
	return off + keyLen + valLen, nil
}

// validateDeltaEntry mirrors validateRestartEntry for delta entries.
func (r LeafReader) validateDeltaEntry(off int) (int, error) {
	if err := r.ensureBytes(off, 1); err != nil {
		return 0, err
	}
	flags := r.buf[off]
	if flags&^cellFlagKnownMask != 0 {
		return 0, fmt.Errorf("unknown CellFlags 0x%x", flags&^cellFlagKnownMask)
	}
	if err := validateCellFlagsCombo(flags); err != nil {
		return 0, err
	}
	off++
	if err := r.ensureBytes(off, 4); err != nil {
		return 0, err
	}
	// sharedLen at off, unsharedLen at off+2. We don't validate
	// SharedLen against the previous key's actual length here (a
	// separate semantic check); just bounds-check the unsharedLen
	// region.
	off += 2
	unsharedLen := int(le.Uint16(r.buf[off:]))
	off += 2
	if cellHasTrailerOnly(flags) {
		// Overflow: [Flags][SharedLen][UnsharedLen][UnsharedKey][OvflPage][TotalLen].
		// NestedTree: [Flags][SharedLen][UnsharedLen][UnsharedKey][Root][Count].
		// Identical wire shape; trailer is always 16 bytes.
		if err := r.ensureBytes(off, unsharedLen+16); err != nil {
			return 0, fmt.Errorf("delta trailer body: %w", err)
		}
		return off + unsharedLen + 16, nil
	}
	// [Flags][SharedLen][UnsharedLen][ValueLen][UnsharedKey][Value]
	if err := r.ensureBytes(off, 4); err != nil {
		return 0, err
	}
	valLen := int(le.Uint32(r.buf[off:]))
	off += 4
	if err := r.ensureBytes(off, unsharedLen+valLen); err != nil {
		return 0, fmt.Errorf("delta inline body unsharedLen=%d valLen=%d: %w", unsharedLen, valLen, err)
	}
	return off + unsharedLen + valLen, nil
}

// validateUCEntry checks an uncompressed entry at off. Returns nil on
// success; non-nil error (without ErrCorrupted wrap) on any bounds
// violation or unknown CellFlags.
func (r LeafReader) validateUCEntry(off int) error {
	if err := r.ensureBytes(off, 1); err != nil {
		return err
	}
	flags := r.buf[off]
	if flags&^cellFlagKnownMask != 0 {
		return fmt.Errorf("unknown CellFlags 0x%x", flags&^cellFlagKnownMask)
	}
	if err := validateCellFlagsCombo(flags); err != nil {
		return err
	}
	off++
	if err := r.ensureBytes(off, 2); err != nil {
		return err
	}
	keyLen := int(le.Uint16(r.buf[off:]))
	off += 2
	if cellHasTrailerOnly(flags) {
		// Overflow OR NestedTree — both have a 16-byte trailer after
		// the key, no ValueLen prefix.
		if err := r.ensureBytes(off, keyLen+16); err != nil {
			return fmt.Errorf("uc trailer body: %w", err)
		}
		return nil
	}
	if err := r.ensureBytes(off, 4); err != nil {
		return err
	}
	valLen := int(le.Uint32(r.buf[off:]))
	off += 4
	if err := r.ensureBytes(off, keyLen+valLen); err != nil {
		return fmt.Errorf("uc inline body keyLen=%d valLen=%d: %w", keyLen, valLen, err)
	}
	return nil
}

// ensureBytes verifies r.buf[off : off+n] is within the entry-data
// region [leafEntryStart, dataEnd). The bound is dataEnd, not
// ContentEnd, so the per-entry validator catches entries that overrun
// into the lookup table — even though raw r.buf has bytes past
// dataEnd, the leaf-page invariant is that entry data ends at
// dataEnd. Returns a context-free error; callers wrap with field
// context.
func (r LeafReader) ensureBytes(off, n int) error {
	if off < leafEntryStart {
		return fmt.Errorf("read at offset %d (n=%d) precedes entry-data start %d", off, n, leafEntryStart)
	}
	if n < 0 {
		return fmt.Errorf("read length %d negative", n)
	}
	if off+n > r.dataEnd {
		return fmt.Errorf("read at offset %d (n=%d) exceeds DataEnd %d", off, n, r.dataEnd)
	}
	return nil
}

// (commonPrefixLen alias removed — sharedPrefixLen in restart.go is the
// canonical implementation; chunk-4.2's external callers have been
// retired in the 4.6β rewrite.)

var _ = bytes.Equal // keep bytes imported for future use
