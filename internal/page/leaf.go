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
	cfg.MustValidate()
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
// (TODO: an err-returning sibling could be added).
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
		e, _ := r.decodeFullKeyEntry(r.ucOffset(idx))
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
//   - Both variants: the entry data is one contiguous stream starting
//     at leafEntryStart and ending exactly at DataEnd, with every
//     lookup-table offset equal to its entry's stream position (per
//     page-formats.md §Leaf Page — contiguous entry stream). The
//     streaming readers decode by continuation and never re-consult
//     the tables mid-stream; the splice writers append at DataEnd.
//   - Compressed: every delta entry's `SharedLen` is bounded by the
//     previous entry's full-key length (per page-formats.md §Leaf
//     Page delta reconstruction) — decodeDeltaEntry slices
//     `prevKey[:SharedLen]` unguarded on the hot path, so an
//     unbounded SharedLen would panic or fabricate keys from
//     adjacent page bytes.
//
// NewLeafReader is intentionally NOT a validation boundary: it
// initializes per-variant metadata cheaply (O(1)) so cursor hot-path
// re-construction doesn't pay a per-page walk. Callers that resolve a
// page from disk for the first time (the btree's pager.Page boundary,
// or Check()) should call Validate to surface corruption before
// invoking the read paths (SearchLeaf, Iter, EntryAt), which assume
// structural validity and either panic or decode garbage on malformed
// input. The internal decoder helpers (decodeFullKeyEntry,
// decodeDeltaEntry) are NOT bounds-checked because
// they're on the hot lookup path; Validate's own walk uses checked
// sibling helpers (validateFullKeyEntry, validateDeltaEntry)
// defined below.
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
		// (Per-group Offsets are checked against the entry stream in
		// the walk below — exact-position matching, which subsumes a
		// range check.)
		sum := 0
		for g := range rc {
			c := r.rt.GroupEntryCount(g)
			if c == 0 {
				return fmt.Errorf("%w: compressed leaf restart group %d has Count=0 (spec invariant)", ErrCorrupted, g)
			}
			sum += c
		}
		if sum != r.count {
			return fmt.Errorf("%w: compressed leaf sum-of-group-counts %d != header Count %d", ErrCorrupted, sum, r.count)
		}
	} else {
		// Uncompressed: offset table must fit; the per-entry walk
		// below matches each table slot against the exact entry
		// stream position.
		tableBytes := r.count * ucOffsetEntrySize
		if r.dataEnd+tableBytes > contentEnd {
			return fmt.Errorf("%w: uncompressed leaf offset table (Count=%d, %d bytes) overlaps DataEnd %d / ContentEnd %d",
				ErrCorrupted, r.count, tableBytes, r.dataEnd, contentEnd)
		}
	}

	// Per-entry walks. Both branches use bounds-checked helpers and
	// surface ErrCorrupted on any over-read.
	if r.count == 0 {
		// No entries ⇒ the stream is empty and DataEnd must sit at
		// the entry-data start (same DataEnd==stream-end rule the
		// walks below enforce for count > 0).
		if r.dataEnd != leafEntryStart {
			return fmt.Errorf("%w: empty leaf DataEnd %d != entry-data start %d", ErrCorrupted, r.dataEnd, leafEntryStart)
		}
		return nil
	}
	// Both walks below verify the entry data forms one CONTIGUOUS
	// stream starting at leafEntryStart and ending exactly at
	// DataEnd, with every lookup-table offset equal to its entry's
	// stream position. The streaming readers (LeafIter.Next,
	// FirstKey) decode by continuation from leafEntryStart with the
	// unchecked hot-path decoders and never re-consult the tables
	// mid-stream — a table that passes range checks but doesn't
	// match the stream would send them into bytes this walk never
	// examined. DataEnd==stream-end matters because the splice paths
	// Validate first and then write the next entry at DataEnd:
	// trailing slack would put the new entry outside the stream.
	if !r.compressed {
		expected := leafEntryStart
		for i := range r.count {
			off := r.ucOffset(i)
			if off != expected {
				return fmt.Errorf("%w: uncompressed leaf offset[%d]=%d != entry stream position %d", ErrCorrupted, i, off, expected)
			}
			next, _, err := r.validateFullKeyEntry(off)
			if err != nil {
				return fmt.Errorf("%w: uncompressed leaf entry %d: %w", ErrCorrupted, i, err)
			}
			expected = next
		}
		if expected != r.dataEnd {
			return fmt.Errorf("%w: uncompressed leaf entry stream ends at %d, DataEnd %d", ErrCorrupted, expected, r.dataEnd)
		}
		return nil
	}
	// Compressed: walk by restart groups using validated decoders,
	// threading each entry's full-key length so every delta's
	// SharedLen is bounded by its predecessor's reconstructed key.
	expected := leafEntryStart
	for g := range r.rt.RestartCount() {
		gc := r.rt.GroupEntryCount(g)
		off := r.rt.Offset(g)
		if off != expected {
			return fmt.Errorf("%w: compressed leaf restart[%d] offset %d != entry stream position %d", ErrCorrupted, g, off, expected)
		}
		prevKeyLen := 0
		for i := range gc {
			var next int
			var err error
			if i == 0 {
				next, prevKeyLen, err = r.validateFullKeyEntry(off)
			} else {
				next, prevKeyLen, err = r.validateDeltaEntry(off, prevKeyLen)
			}
			if err != nil {
				return fmt.Errorf("%w: compressed leaf group %d entry %d: %w", ErrCorrupted, g, i, err)
			}
			off = next
		}
		expected = off
	}
	if expected != r.dataEnd {
		return fmt.Errorf("%w: compressed leaf entry stream ends at %d, DataEnd %d", ErrCorrupted, expected, r.dataEnd)
	}
	return nil
}

// decodeFullKeyEntry decodes a full-key entry at the given byte
// offset — the ONE decoder for the uncompressed entry and the
// compressed RESTART entry, which share this exact wire layout
// (page-formats.md §Uncompressed Leaf; only DELTA entries differ —
// decodeDeltaEntry). Returns the entry and the offset of the next
// entry's first byte. Key and Value (for inline values) borrow from
// the page buffer. NOT bounds-checked (see the Validate contract
// above): callers run only on validated pages.
func (r LeafReader) decodeFullKeyEntry(off int) (LeafEntry, int) {
	var e LeafEntry
	e.Flags = r.buf[off]
	off++
	keyLen := int(le.Uint16(r.buf[off:]))
	off += 2
	if e.Flags&CellFlagOverflow != 0 {
		// [Flags][KeyLen][Key][OvflPage uint64][TotalLen uint64]
		e.Key = r.buf[off : off+keyLen]
		off += keyLen
		e.OverflowPage = le.Uint64(r.buf[off:])
		off += 8
		e.TotalLen = le.Uint64(r.buf[off:])
		off += 8
		return e, off
	}
	if e.IsNestedTree() {
		// [Flags][KeyLen][Key][Root uint64][Count uint64] — same wire
		// shape as overflow; different decoded-view fields. Per
		// set-keyspace.md §Nested B+tree Reference Cell.
		e.Key = r.buf[off : off+keyLen]
		off += keyLen
		e.NestedRoot = le.Uint64(r.buf[off:])
		off += 8
		e.NestedCount = le.Uint64(r.buf[off:])
		off += 8
		return e, off
	}
	// [Flags][KeyLen][ValueLen][Key][Value]
	valLen := int(le.Uint32(r.buf[off:]))
	off += 4
	e.Key = r.buf[off : off+keyLen]
	off += keyLen
	e.Value = r.buf[off : off+valLen]
	off += valLen
	return e, off
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

// validateFullKeyEntry checks a full-key entry — the shared wire
// layout of the uncompressed entry and the compressed restart entry
// (see decodeFullKeyEntry) — at off and returns the
// offset of the next entry plus the entry's full-key length (the
// group's delta chain reconstructs keys from it). Returns a non-nil
// error (without ErrCorrupted wrap — the caller wraps with structural
// context) on any bounds violation or unknown CellFlags.
func (r LeafReader) validateFullKeyEntry(off int) (next, keyLen int, err error) {
	if err := r.ensureBytes(off, 1); err != nil {
		return 0, 0, err
	}
	flags := r.buf[off]
	if flags&^cellFlagKnownMask != 0 {
		return 0, 0, fmt.Errorf("unknown CellFlags 0x%x", flags&^cellFlagKnownMask)
	}
	if err := validateCellFlagsCombo(flags); err != nil {
		return 0, 0, err
	}
	off++
	if err := r.ensureBytes(off, 2); err != nil {
		return 0, 0, err
	}
	keyLen = int(le.Uint16(r.buf[off:]))
	off += 2
	if cellHasTrailerOnly(flags) {
		// Overflow: [Flags][KeyLen][Key][OvflPage u64][TotalLen u64].
		// NestedTree: [Flags][KeyLen][Key][Root u64][Count u64].
		// Identical wire shape; the trailer is always 16 bytes.
		if err := r.ensureBytes(off, keyLen+16); err != nil {
			return 0, 0, fmt.Errorf("full-key trailer body: %w", err)
		}
		return off + keyLen + 16, keyLen, nil
	}
	// [Flags][KeyLen][ValueLen][Key][Value]
	if err := r.ensureBytes(off, 4); err != nil {
		return 0, 0, err
	}
	valLen := int(le.Uint32(r.buf[off:]))
	off += 4
	if err := r.ensureBytes(off, keyLen+valLen); err != nil {
		return 0, 0, fmt.Errorf("full-key inline body keyLen=%d valLen=%d: %w", keyLen, valLen, err)
	}
	return off + keyLen + valLen, keyLen, nil
}

// validateDeltaEntry mirrors validateFullKeyEntry for delta entries.
// prevKeyLen is the previous entry's full-key length; SharedLen must
// not exceed it, or decodeDeltaEntry's `prevKey[:sharedLen]` either
// panics (keyBuf-backed prevKey) or silently prepends adjacent page
// bytes to the reconstructed key (page-buffer-backed prevKey) — the
// reconstruction invariant in page-formats.md §Leaf Page (Delta
// entry). Returns the entry's own full-key length for the next link
// in the chain.
func (r LeafReader) validateDeltaEntry(off, prevKeyLen int) (next, keyLen int, err error) {
	if err := r.ensureBytes(off, 1); err != nil {
		return 0, 0, err
	}
	flags := r.buf[off]
	if flags&^cellFlagKnownMask != 0 {
		return 0, 0, fmt.Errorf("unknown CellFlags 0x%x", flags&^cellFlagKnownMask)
	}
	if err := validateCellFlagsCombo(flags); err != nil {
		return 0, 0, err
	}
	off++
	if err := r.ensureBytes(off, 4); err != nil {
		return 0, 0, err
	}
	sharedLen := int(le.Uint16(r.buf[off:]))
	off += 2
	unsharedLen := int(le.Uint16(r.buf[off:]))
	off += 2
	if sharedLen > prevKeyLen {
		return 0, 0, fmt.Errorf("delta SharedLen %d exceeds previous full-key length %d", sharedLen, prevKeyLen)
	}
	keyLen = sharedLen + unsharedLen
	if cellHasTrailerOnly(flags) {
		// Overflow: [Flags][SharedLen][UnsharedLen][UnsharedKey][OvflPage][TotalLen].
		// NestedTree: [Flags][SharedLen][UnsharedLen][UnsharedKey][Root][Count].
		// Identical wire shape; trailer is always 16 bytes.
		if err := r.ensureBytes(off, unsharedLen+16); err != nil {
			return 0, 0, fmt.Errorf("delta trailer body: %w", err)
		}
		return off + unsharedLen + 16, keyLen, nil
	}
	// [Flags][SharedLen][UnsharedLen][ValueLen][UnsharedKey][Value]
	if err := r.ensureBytes(off, 4); err != nil {
		return 0, 0, err
	}
	valLen := int(le.Uint32(r.buf[off:]))
	off += 4
	if err := r.ensureBytes(off, unsharedLen+valLen); err != nil {
		return 0, 0, fmt.Errorf("delta inline body unsharedLen=%d valLen=%d: %w", unsharedLen, valLen, err)
	}
	return off + unsharedLen + valLen, keyLen, nil
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
// canonical implementation; its external callers were
// retired in the rewrite.)

var _ = bytes.Equal // keep bytes imported for future use
