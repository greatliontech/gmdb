package page

// Leaf-page formats per page-formats.md §Leaf Page. Three variants share
// the 8-byte common header and the "entries forward / lookup table
// backward" layout; only the per-entry encoding, value placement, and
// lookup machinery differ:
//
//   - TypeLeaf (interleaved compressed): variable-size restart groups,
//     prefix-compressed delta entries, value bytes following each
//     entry's key bytes. Format details in leaf_compressed.go.
//   - TypeLeafSegregated (segregated compressed, the default): the same
//     restart-group key compression over a pure headers+keys entry
//     stream; value bytes in a separate end-anchored region located by
//     per-entry VOff. Format details in leaf_segregated.go.
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

	// CellFlagEmptyValue marks the compact inline form for an empty
	// value: [Flags][KeyLen][Key] (full-key) /
	// [Flags][SharedLen][UnsharedLen][UnsharedKey] (delta) — no
	// ValueLen field, no value bytes. The encoders emit it for every
	// plain cell whose value is empty (nested-tree members, the
	// set-of-keys pattern); decoders also accept the legacy
	// zero-ValueLen inline form (page-formats.md §Leaf Page,
	// empty-value cell).
	CellFlagEmptyValue uint8 = 1 << 3

	// CellFlagOverflowKey marks an overflow-key cell (page-formats.md
	// §Overflow-Key Cells): the key half stores exactly the first
	// T(cfg) bytes of the full key inline (KeyLen == T), followed by
	// a 12-byte key-extent reference — KeyExtPage(u64), the first
	// page of an overflow run holding key[T:], and KeyTotalLen(u32),
	// the FULL key length. The bit modifies only the key half; the
	// value half per bits 0–3 follows the extent reference unchanged
	// in form. Restart/uncompressed entries only — a delta entry
	// never carries this bit, and a compressed overflow-key entry is
	// always a singleton restart group.
	CellFlagOverflowKey uint8 = 1 << 4

	// cellFlagKnownMask is the union of currently-defined cell flag
	// bits. The strict-reject rule from file-layout.md §Reserved-byte
	// policy is enforced via LeafReader.Validate (not in the hot-path
	// decoders, which assume well-formed input); see Validate's doc
	// for the boundary discipline.
	cellFlagKnownMask = CellFlagOverflow | CellFlagMultiValue | CellFlagNestedTree | CellFlagEmptyValue | CellFlagOverflowKey
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

	// Overflow-key fields (valid iff Flags has CellFlagOverflowKey
	// set; zero otherwise). Key then holds only the RESIDENT first
	// T(cfg) bytes of the full key; KeyExtPage is the first page of
	// the overflow run holding key[T:]; KeyTotalLen is the full key
	// length (page-formats.md §Overflow-Key Cells). Comparisons that
	// tie through the resident bytes against a longer probe must
	// consult the extent — the page layer never reads it (pure over
	// one page); the btree layer chases via its PageReader.
	KeyExtPage  uint64
	KeyTotalLen uint32
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

// IsOverflowKey reports whether the entry's key exceeds the inline
// threshold and is stored as resident-first-T bytes plus a key extent
// (page-formats.md §Overflow-Key Cells). e.Key then holds only
// key[0:T]; key[T:] lives in the run starting at e.KeyExtPage and the
// full length is e.KeyTotalLen.
func (e LeafEntry) IsOverflowKey() bool { return e.Flags&CellFlagOverflowKey != 0 }

// TailCompare resolves the order of probe against the FULL key of an
// overflow-key cell whose first-T bytes tie with probe's (page-formats.md
// §Overflow-Key Cells, Comparison). Precondition established by the
// caller: len(probe) > T and probe[0:T] equals the cell's resident
// bytes; the implementation compares probe[T:] against the extent run
// at extPage (totalLen is the stored key's full length). Returns
// bytes.Compare semantics for probe vs the stored full key. The page
// package never reads pages other than the one it decodes — the btree
// layer supplies this over its PageReader.
type TailCompare func(probe []byte, extPage uint64, totalLen uint32) (int, error)

// NoExtentTail is a TailCompare for callers that guarantee the searched
// pages contain no overflow-key cells (unit tests over hand-built
// pages; surfaces whose keys are structurally bounded under the inline
// threshold). Invoking it reports the broken guarantee as an error
// rather than a wrong comparison.
var NoExtentTail TailCompare = func([]byte, uint64, uint32) (int, error) {
	return 0, fmt.Errorf("%w: overflow-key extent comparison required but caller supplied NoExtentTail", ErrCorrupted)
}

// compareEntryKey returns bytes.Compare(storedFullKey, target) for an
// entry that may be overflow-key, reading the extent (via tail) exactly
// when the spec's comparison rule requires it: target longer than the
// resident portion AND tying through all of it. For ordinary entries it
// is bytes.Compare(e.Key, target).
func compareEntryKey(e LeafEntry, target []byte, tail TailCompare) (int, error) {
	if !e.IsOverflowKey() {
		return bytes.Compare(e.Key, target), nil
	}
	t := len(e.Key) // resident length == InlineThreshold by Validate
	k := min(len(target), t)
	if c := bytes.Compare(e.Key[:k], target[:k]); c != 0 {
		return c, nil
	}
	if len(target) <= t {
		// target is a (possibly full-length) prefix of the resident
		// bytes; the stored key is strictly longer (KeyTotalLen > T by
		// Validate) — stored > target, no extent read.
		return 1, nil
	}
	c, err := tail(target, e.KeyExtPage, e.KeyTotalLen)
	return -c, err
}

// LeafReader provides read access over all leaf page variants.
// Constructed once per Page-resolution boundary in the btree descent;
// cheap by value, holds only a slice header into the page buffer plus
// precomputed extents.
type LeafReader struct {
	buf     []byte
	cfg     Config
	count   int
	dataEnd int
	variant uint8 // TypeLeaf | TypeLeafSegregated | TypeLeafUncompressed
	// Restart-group variants (interleaved + segregated) only:
	rt restartTable
	// Segregated variant only:
	valueEnd int
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
	switch typ {
	case TypeLeafUncompressed:
		return LeafReader{
			buf:        buf,
			cfg:        cfg,
			count:      int(count),
			dataEnd:    int(le.Uint16(buf[ucLeafOffDataEnd:])),
			variant:    typ,
			ucTableOff: contentEnd - int(count)*ucOffsetEntrySize,
		}
	case TypeLeafSegregated:
		rc := int(le.Uint16(buf[leafOffRestartCount:]))
		return LeafReader{
			buf:      buf,
			cfg:      cfg,
			count:    int(count),
			dataEnd:  int(le.Uint16(buf[leafOffDataEnd:])),
			variant:  typ,
			valueEnd: int(le.Uint16(buf[segLeafOffValueEnd:])),
			rt:       newRestartTable(buf, rc, contentEnd),
		}
	default: // TypeLeaf
		rc := int(le.Uint16(buf[leafOffRestartCount:]))
		return LeafReader{
			buf:     buf,
			cfg:     cfg,
			count:   int(count),
			dataEnd: int(le.Uint16(buf[leafOffDataEnd:])),
			variant: typ,
			rt:      newRestartTable(buf, rc, contentEnd),
		}
	}
}

// Buf returns the underlying page buffer for callers that need to perform
// further page-level inspection (e.g. ReadHeader for type-byte checks in a
// generic walker). The returned slice is borrowed; do not retain past the
// reader's lifetime.
func (r LeafReader) Buf() []byte { return r.buf }

// Compressed reports whether this leaf is a restart-group compressed
// page (interleaved OR segregated). False ⇒ uncompressed (positional
// offset table, full keys).
func (r LeafReader) Compressed() bool { return r.variant != TypeLeafUncompressed }

// Variant returns the page's leaf type byte (TypeLeaf,
// TypeLeafSegregated, or TypeLeafUncompressed) — the read-side
// dispatch authority per page-formats.md §Invariants.
func (r LeafReader) Variant() uint8 { return r.variant }

// seg reports the segregated variant; uc the uncompressed one. The
// interleaved variant is the residual case.
func (r LeafReader) seg() bool { return r.variant == TypeLeafSegregated }
func (r LeafReader) uc() bool  { return r.variant == TypeLeafUncompressed }

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
	if r.uc() {
		return r.count
	}
	return r.rt.RestartCount()
}

// GroupEntryCount returns the number of entries in the i-th restart group.
// For uncompressed leaves every group has exactly 1 entry.
func (r LeafReader) GroupEntryCount(i int) int {
	if r.uc() {
		return 1
	}
	return r.rt.GroupEntryCount(i)
}

// SearchLeaf performs a key lookup against the leaf. Returns the entry's
// absolute index, the entry (with Key always nil — callers either had the
// target already or don't need the key bytes back), and whether the key
// was found. On miss, index is the insertion point (the index of the
// smallest key strictly greater than target, or Count() if every entry is
// less than target). SearchLeaf uses the unchecked hot-path decoders and
// assumes structural validity — first-resolve callers must gate the page
// through Validate (see the Validate doc for the trust boundary).
//
// tail resolves overflow-key first-T-byte ties against the key extent
// (page-formats.md §Overflow-Key Cells, Comparison); its page reads are
// the only error source.
func (r LeafReader) SearchLeaf(target []byte, tail TailCompare) (index int, entry LeafEntry, found bool, err error) {
	if r.count == 0 {
		return 0, LeafEntry{}, false, nil
	}
	switch r.variant {
	case TypeLeafUncompressed:
		return r.ucSearchLeaf(target, tail)
	case TypeLeafSegregated:
		return r.segSearchLeaf(target, tail)
	default:
		return r.compressedSearchLeaf(target, tail)
	}
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
func (r LeafReader) SearchLeafIter(target, keyBuf, bufKeys []byte, bufEnts []LeafEntry, tail TailCompare) (int, LeafEntry, bool, LeafIter, error) {
	if r.count == 0 {
		return 0, LeafEntry{}, false, LeafIter{
			r:       r,
			variant: r.variant,
			keyBuf:  keyBuf[:0],
			bufKeys: bufKeys[:0],
			bufEnts: bufEnts[:0],
		}, nil
	}
	switch r.variant {
	case TypeLeafUncompressed:
		return r.ucSearchLeafIter(target, keyBuf, bufKeys, bufEnts, tail)
	case TypeLeafSegregated:
		return r.segSearchLeafIter(target, keyBuf, bufKeys, bufEnts, tail)
	default:
		return r.compressedSearchLeafIter(target, keyBuf, bufKeys, bufEnts, tail)
	}
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
	switch r.variant {
	case TypeLeafUncompressed:
		e, _ := r.decodeFullKeyEntry(r.ucOffset(idx))
		return e, keyBuf
	case TypeLeafSegregated:
		return r.segEntryAt(idx, keyBuf)
	default:
		return r.compressedEntryAt(idx, keyBuf)
	}
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
	switch r.variant {
	case TypeLeafUncompressed:
		off := r.ucOffset(r.count - 1)
		flags := r.buf[off]
		off++
		keyLen := int(le.Uint16(r.buf[off:]))
		off += 2
		off += cellPreKeySkip(flags)
		return r.buf[off : off+keyLen], keyBuf
	case TypeLeafSegregated:
		return r.segLastKey(keyBuf)
	default:
		return r.compressedLastKey(keyBuf)
	}
}

// cellHasTrailerOnly reports whether the entry's value half is the
// 16-byte trailer form (overflow run reference OR nested-tree
// reference) — both shapes elide the `ValueLen u32` prefix. Centralised
// here so FirstKey / LastKey and the splice helpers don't have to
// re-derive the condition.
func cellHasTrailerOnly(flags uint8) bool {
	return flags&CellFlagOverflow != 0 || flags&CellFlagNestedTree != 0
}

// cellPreKeySkip returns the byte count between a full-key cell's
// KeyLen field and its key bytes: 4 (ValueLen u32) for inline/subpage
// cells, 0 for trailer-only and empty-value forms. The single
// skip-math helper for the manual key readers (FirstKey / LastKey) —
// hand-rolled `+= 4` here misread every empty-value cell as its key
// shifted by four bytes.
func cellPreKeySkip(flags uint8) int {
	if cellHasTrailerOnly(flags) || flags&CellFlagEmptyValue != 0 {
		return 0
	}
	return 4
}

// FirstKey returns the key of the first entry. Every variant stores the
// first entry's full key inline (compressed: at the first restart point;
// uncompressed: at offset 0 of the entry-data region; segregated: at
// the fixed 5-byte header offset), so this is a constant-time slice
// into the page buffer regardless of variant.
func (r LeafReader) FirstKey() []byte {
	if r.count == 0 {
		return nil
	}
	if r.seg() {
		_, key, _, _ := r.decodeSegRestart(segLeafEntryStart)
		return key
	}
	flags := r.buf[leafEntryStart]
	off := leafEntryStart
	off++ // skip CellFlags
	keyLen := int(le.Uint16(r.buf[off:]))
	off += 2
	// Trailer-only and empty-value forms place the key immediately
	// after KeyLen; inline cells (plain + subpage) carry ValueLen
	// between KeyLen and Key.
	off += cellPreKeySkip(flags)
	return r.buf[off : off+keyLen]
}

// FreeSpace returns the byte count of unused space between the
// entry-data tail and the lookup-table start. Used by the in-place
// splice helpers to fit-check appends and inserts. Negative values are
// not reachable from a well-formed page; the helpers gate their use on
// the returned value being nonnegative.
func (r LeafReader) FreeSpace() int {
	switch r.variant {
	case TypeLeafUncompressed:
		return r.cfg.ContentEnd() - r.dataEnd - r.count*ucOffsetEntrySize
	case TypeLeafSegregated:
		// Two disjoint free regions: the middle gap between the entry
		// stream and the value region's first byte, plus any gap
		// between ValueEnd and the restart table (left by table
		// shrinks; zero in canonical form).
		voff0 := r.valueEnd
		if r.count > 0 {
			voff0 = segReadVOff(r.buf, segLeafEntryStart, true)
		}
		tableBase := r.cfg.ContentEnd() - r.rt.RestartCount()*restartTableEntrySize
		return (voff0 - r.dataEnd) + (tableBase - r.valueEnd)
	default:
		return r.cfg.ContentEnd() - r.dataEnd - r.rt.RestartCount()*restartTableEntrySize
	}
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
	if r.seg() {
		return r.segValidate()
	}
	contentEnd := r.cfg.ContentEnd()
	if r.dataEnd < leafEntryStart || r.dataEnd > contentEnd {
		return fmt.Errorf("%w: leaf DataEnd %d outside [%d, %d]", ErrCorrupted, r.dataEnd, leafEntryStart, contentEnd)
	}
	if r.variant == TypeLeaf {
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
	if r.uc() {
		expected := leafEntryStart
		for i := range r.count {
			off := r.ucOffset(i)
			if off != expected {
				return fmt.Errorf("%w: uncompressed leaf offset[%d]=%d != entry stream position %d", ErrCorrupted, i, off, expected)
			}
			next, _, _, err := r.validateFullKeyEntry(off)
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
				var flags uint8
				next, prevKeyLen, flags, err = r.validateFullKeyEntry(off)
				// Singleton-restart-group rule (page-formats.md
				// §Overflow-Key Cells): an overflow-key entry is
				// always the sole entry of its group — delta
				// reconstruction never chains through an
				// extent-resident key.
				if err == nil && flags&CellFlagOverflowKey != 0 && gc != 1 {
					err = fmt.Errorf("overflow-key restart entry in group with Count=%d (must be a singleton group)", gc)
				}
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
	// ValueLen (inline / subpage forms only) sits in the header BEFORE
	// the key bytes — the decode-speed field ordering (page-formats.md
	// §Compressed Leaf). Trailer-only and empty-value forms have no
	// ValueLen.
	valLen := -1
	if !cellHasTrailerOnly(e.Flags) && e.Flags&CellFlagEmptyValue == 0 {
		valLen = int(le.Uint32(r.buf[off:]))
		off += 4
	}
	e.Key = r.buf[off : off+keyLen]
	off += keyLen
	if e.Flags&CellFlagOverflowKey != 0 {
		// Key half is resident-first-T bytes + 12-byte key-extent
		// reference; the value half follows unchanged in form
		// (page-formats.md §Overflow-Key Cells).
		e.KeyExtPage = le.Uint64(r.buf[off:])
		off += 8
		e.KeyTotalLen = le.Uint32(r.buf[off:])
		off += 4
	}
	switch {
	case e.Flags&CellFlagOverflow != 0:
		// [... Key half ...][OvflPage uint64][TotalLen uint64]
		e.OverflowPage = le.Uint64(r.buf[off:])
		off += 8
		e.TotalLen = le.Uint64(r.buf[off:])
		off += 8
	case e.IsNestedTree():
		// [... Key half ...][Root uint64][Count uint64] — same wire
		// shape as overflow; different decoded-view fields. Per
		// set-keyspace.md §Nested B+tree Reference Cell.
		e.NestedRoot = le.Uint64(r.buf[off:])
		off += 8
		e.NestedCount = le.Uint64(r.buf[off:])
		off += 8
	case e.Flags&CellFlagEmptyValue != 0:
		// Compact empty-value form; the value half is absent. Value
		// decodes to a non-nil zero-length slice, matching the legacy
		// zero-ValueLen inline decode.
		e.Value = r.buf[off:off]
	default:
		// Inline / subpage: [ValueLen already read][Value]
		e.Value = r.buf[off : off+valLen]
		off += valLen
	}
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
	if flags&CellFlagEmptyValue != 0 && flags&^(CellFlagEmptyValue|CellFlagOverflowKey) != 0 {
		return fmt.Errorf("CellFlags 0x%x sets EmptyValue alongside other value-form flags (EmptyValue is exclusive among bits 0-2: trailer and subpage forms carry their own value halves; OverflowKey composes — it modifies only the key half)", flags)
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
func (r LeafReader) validateFullKeyEntry(off int) (next, keyLen int, flags uint8, err error) {
	if err := r.ensureBytes(off, 1); err != nil {
		return 0, 0, 0, err
	}
	flags = r.buf[off]
	if flags&^cellFlagKnownMask != 0 {
		return 0, 0, 0, fmt.Errorf("unknown CellFlags 0x%x", flags&^cellFlagKnownMask)
	}
	if err := validateCellFlagsCombo(flags); err != nil {
		return 0, 0, 0, err
	}
	off++
	if err := r.ensureBytes(off, 2); err != nil {
		return 0, 0, 0, err
	}
	keyLen = int(le.Uint16(r.buf[off:]))
	off += 2
	// Key-half tail: 12-byte key-extent reference when OverflowKey is
	// set. The derivable-length read policy (page-formats.md
	// §Overflow-Key Cells): the resident length is EXACTLY the inline
	// threshold, and the stored full length strictly exceeds it —
	// divergence is structural corruption, never a trusted field.
	keyTail := 0
	if flags&CellFlagOverflowKey != 0 {
		t := r.cfg.InlineThreshold()
		if keyLen != t {
			return 0, 0, 0, fmt.Errorf("overflow-key resident length %d != inline threshold %d", keyLen, t)
		}
		keyTail = 12
	}
	if cellHasTrailerOnly(flags) {
		// Overflow: [.. Key half ..][OvflPage u64][TotalLen u64].
		// NestedTree: [.. Key half ..][Root u64][Count u64].
		// Identical wire shape; the trailer is always 16 bytes.
		if err := r.ensureBytes(off, keyLen+keyTail+16); err != nil {
			return 0, 0, 0, fmt.Errorf("full-key trailer body: %w", err)
		}
		if err := r.validateKeyExt(flags, off+keyLen, keyLen); err != nil {
			return 0, 0, 0, err
		}
		return off + keyLen + keyTail + 16, keyLen, flags, nil
	}
	if flags&CellFlagEmptyValue != 0 {
		// [Flags][KeyLen][Key half] — no value half.
		if err := r.ensureBytes(off, keyLen+keyTail); err != nil {
			return 0, 0, 0, fmt.Errorf("full-key empty-value body: %w", err)
		}
		if err := r.validateKeyExt(flags, off+keyLen, keyLen); err != nil {
			return 0, 0, 0, err
		}
		return off + keyLen + keyTail, keyLen, flags, nil
	}
	// [Flags][KeyLen][ValueLen][Key half][Value]
	if err := r.ensureBytes(off, 4); err != nil {
		return 0, 0, 0, err
	}
	valLen := int(le.Uint32(r.buf[off:]))
	off += 4
	if err := r.ensureBytes(off, keyLen+keyTail+valLen); err != nil {
		return 0, 0, 0, fmt.Errorf("full-key inline body keyLen=%d valLen=%d: %w", keyLen, valLen, err)
	}
	if err := r.validateKeyExt(flags, off+keyLen, keyLen); err != nil {
		return 0, 0, 0, err
	}
	return off + keyLen + keyTail + valLen, keyLen, flags, nil
}

// validateKeyExt checks the 12-byte key-extent reference of an
// overflow-key cell whose reference begins at extOff (immediately after
// the resident key bytes). residentLen is the already-validated
// resident length (== InlineThreshold). No-op for cells without
// CellFlagOverflowKey. Bounds were established by the caller's
// ensureBytes over the full body.
func (r LeafReader) validateKeyExt(flags uint8, extOff, residentLen int) error {
	if flags&CellFlagOverflowKey == 0 {
		return nil
	}
	extPage := le.Uint64(r.buf[extOff:])
	totalLen := int(le.Uint32(r.buf[extOff+8:]))
	if extPage == 0 {
		return fmt.Errorf("overflow-key extent page is 0")
	}
	if totalLen <= residentLen {
		return fmt.Errorf("overflow-key KeyTotalLen %d does not exceed resident length %d", totalLen, residentLen)
	}
	return nil
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
	if flags&CellFlagOverflowKey != 0 {
		// page-formats.md §Overflow-Key Cells: a delta entry never
		// carries an overflow key (singleton-restart-group rule).
		return 0, 0, fmt.Errorf("delta entry carries CellFlagOverflowKey (overflow-key entries are restart-only singleton groups)")
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
	if flags&CellFlagEmptyValue != 0 {
		// [Flags][SharedLen][UnsharedLen][UnsharedKey] — no value half.
		if err := r.ensureBytes(off, unsharedLen); err != nil {
			return 0, 0, fmt.Errorf("delta empty-value body: %w", err)
		}
		return off + unsharedLen, keyLen, nil
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
