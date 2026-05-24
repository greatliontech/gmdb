package page

import (
	"bytes"
	"fmt"
)

// Leaf-page layout per page-formats.md §Leaf Page:
//
//	+-----------------------+ offset 0
//	| Page Header (8 bytes) | Type=TypeLeaf, Count=N (entry count)
//	+-----------------------+ offset 8
//	| RestartInterval u16   | per-keyspace target (default 16)
//	| RestartCount    u16   | number of restart points
//	+-----------------------+ offset 12
//	| Entry 0 (restart)     | entries forward-packed from offset 12
//	| Entry 1 (delta)       |
//	| ...                   |
//	+-----------------------+ grows forward
//	|       free space      |
//	+-----------------------+
//	| Restart Table         | RestartCount × 2 bytes, packed at
//	|                       | content end
//	+-----------------------+ ContentEnd (PageSize - optional 8-byte footer)

const (
	// leafHeaderEnd is the byte offset where entries begin: after
	// the common header (8) + RestartInterval (2) + RestartCount
	// (2) = 12.
	leafHeaderEnd = 12

	// leafRestartTableEntrySize is the byte length of one restart
	// table entry — a uint16 offset into the page.
	leafRestartTableEntrySize = 2

	// leafRestartIntervalOff / leafRestartCountOff are the offsets
	// of the per-page leaf-header fields.
	leafRestartIntervalOff = HeaderSize
	leafRestartCountOff    = HeaderSize + 2
)

// CellFlags bit assignments per page-formats.md §Leaf Page.
const (
	CellFlagOverflow   uint8 = 1 << 0
	CellFlagMultiValue uint8 = 1 << 1
	CellFlagNestedTree uint8 = 1 << 2

	// cellFlagKnownMask is the union of currently-defined cell
	// flag bits. Decoders reject any flag outside this mask.
	cellFlagKnownMask = CellFlagOverflow | CellFlagMultiValue | CellFlagNestedTree
)

// LeafEntry is the decoded form of one leaf cell. Key and Value
// slices borrow from the page buffer (and, for delta entries, the
// Key field is materialised by reconstruction — see LeafCursor).
// Callers MUST NOT modify or retain past page-buffer lifetime.
//
// For overflow references, Value is nil and OverflowPage / TotalLen
// are populated; the caller is responsible for assembling the
// large value by walking the overflow run.
type LeafEntry struct {
	Flags        uint8
	Key          []byte
	Value        []byte
	OverflowPage uint64
	TotalLen     uint64
}

// IsOverflow reports whether the entry's value lives in an overflow
// run rather than inline on the leaf.
func (e LeafEntry) IsOverflow() bool { return e.Flags&CellFlagOverflow != 0 }

// LeafRestartInterval / LeafRestartCount expose the per-page leaf
// header fields. RestartInterval is the per-keyspace target K;
// RestartCount is the number of restart points = ceil(N / K).
func LeafRestartInterval(buf []byte) uint16 {
	_ = buf[leafRestartIntervalOff+1]
	return le.Uint16(buf[leafRestartIntervalOff:])
}

func LeafRestartCount(buf []byte) uint16 {
	_ = buf[leafRestartCountOff+1]
	return le.Uint16(buf[leafRestartCountOff:])
}

// LeafEntryCount returns the total entry count N from the page
// header. Wraps ReadHeader for type-safety on leaf pages.
func LeafEntryCount(buf []byte) uint16 {
	typ, _, count, _ := ReadHeader(buf)
	if typ != TypeLeaf {
		panic(fmt.Sprintf("page: LeafEntryCount on type %d (want %d)", typ, TypeLeaf))
	}
	return count
}

// leafRestartTableStart returns the byte offset where the restart
// table begins. Per page-formats.md the table sits immediately
// before the optional checksum footer; given RestartCount restart
// points each of 2 bytes, the table occupies
// [ContentEnd - RestartCount*2, ContentEnd).
func leafRestartTableStart(cfg Config, restartCount uint16) int {
	return cfg.ContentEnd() - int(restartCount)*leafRestartTableEntrySize
}

// LeafRestartOffset returns the byte offset of the i-th restart
// entry, read from the restart table. Used by leaf lookup's
// binary-search phase.
func LeafRestartOffset(buf []byte, cfg Config, i uint16) uint16 {
	cfg.mustValidate()
	rc := LeafRestartCount(buf)
	if i >= rc {
		panic(fmt.Sprintf("page: LeafRestartOffset(%d) out of range [0, %d)", i, rc))
	}
	tableStart := leafRestartTableStart(cfg, rc)
	return le.Uint16(buf[tableStart+int(i)*leafRestartTableEntrySize:])
}

// EncodedEntry is one entry to feed into EncodeLeaf. The caller
// supplies the full (Flags, Key, Value/OverflowPage+TotalLen) for
// every entry in the page's logical order; the codec decides which
// entries land at restart positions and applies delta compression
// to the rest.
//
// For inline values, set Flags=0 (or just CellFlagMultiValue for
// subpage cells) and Value to the inline bytes. For overflow
// references, set Flags|=CellFlagOverflow and OverflowPage +
// TotalLen; Value must be nil.
type EncodedEntry struct {
	Flags        uint8
	Key          []byte
	Value        []byte
	OverflowPage uint64
	TotalLen     uint64
}

// EncodeLeaf writes entries into buf with restart-grouping at
// intervals of `interval` (the leaf's RestartInterval, persisted
// per-page; see page-formats.md). Entries MUST be in ascending Key
// order (the codec verifies and errors on violation rather than
// silently mis-encoding).
//
// Returns an error if the entries don't fit. Caller can check
// fit-ahead with LeafEncodedSize.
//
// Encoding format per page-formats.md §Leaf Page:
//
//   - Restart entry (index 0, K, 2K, ...): full key.
//     Inline:    [Flags u8][KeyLen u16][ValueLen u32][Key][Value]
//     Overflow:  [Flags u8|=Overflow][KeyLen u16][Key][OvflPage u64][TotalLen u64]
//
//   - Delta entry (other indices): shared prefix omitted.
//     Inline:    [Flags u8][SharedLen u16][UnsharedLen u16][ValueLen u32][UnsharedKey][Value]
//     Overflow:  [Flags u8|=Overflow][SharedLen u16][UnsharedLen u16][UnsharedKey][OvflPage u64][TotalLen u64]
func EncodeLeaf(buf []byte, cfg Config, interval uint16, entries []EncodedEntry) error {
	cfg.mustValidate()
	if len(buf) != int(cfg.PageSize) {
		return fmt.Errorf("page: EncodeLeaf buf len %d != PageSize %d", len(buf), cfg.PageSize)
	}
	if interval == 0 {
		return fmt.Errorf("page: EncodeLeaf RestartInterval must be > 0")
	}
	for i := 1; i < len(entries); i++ {
		if bytes.Compare(entries[i-1].Key, entries[i].Key) >= 0 {
			return fmt.Errorf("page: EncodeLeaf entries not strictly ascending at index %d", i)
		}
	}
	for i, e := range entries {
		if e.Flags&^cellFlagKnownMask != 0 {
			return fmt.Errorf("page: EncodeLeaf entry %d: unknown flag bits 0x%x", i, e.Flags&^cellFlagKnownMask)
		}
		if e.Flags&CellFlagOverflow != 0 && len(e.Value) != 0 {
			return fmt.Errorf("page: EncodeLeaf entry %d: Overflow flag with non-empty Value", i)
		}
		// MultiValue/NestedTree cells land in chunk 6 (SetKeyspace);
		// for chunk 4 we permit the bits but the encoder treats
		// them as opaque inline values. EncodeLeaf neither knows
		// nor cares about subpage internal structure.
	}

	clear(buf)
	WriteHeader(buf, TypeLeaf, uint16(len(entries)), 0)
	le.PutUint16(buf[leafRestartIntervalOff:], interval)

	// Phase 1: figure out per-entry sizes + which entries are
	// restart points, then verify the encoded size fits before
	// touching buf.
	restartCount := uint16(0)
	for i := range entries {
		if uint16(i)%interval == 0 {
			restartCount++
		}
	}
	totalEntryBytes := 0
	for i, e := range entries {
		isRestart := uint16(i)%interval == 0
		var prev []byte
		if !isRestart {
			prev = entries[i-1].Key
		}
		totalEntryBytes += leafEntrySize(e, isRestart, prev)
	}
	need := leafHeaderEnd + totalEntryBytes + int(restartCount)*leafRestartTableEntrySize
	if need > cfg.ContentEnd() {
		return fmt.Errorf("page: EncodeLeaf %d entries need %d bytes, page content area is %d",
			len(entries), need, cfg.ContentEnd())
	}
	le.PutUint16(buf[leafRestartCountOff:], restartCount)

	// Phase 2: emit entries forward; record restart-point offsets.
	pos := leafHeaderEnd
	restartTable := make([]uint16, 0, restartCount)
	for i, e := range entries {
		isRestart := uint16(i)%interval == 0
		var prev []byte
		if !isRestart {
			prev = entries[i-1].Key
		}
		if isRestart {
			restartTable = append(restartTable, uint16(pos))
		}
		pos += writeLeafEntry(buf[pos:], e, isRestart, prev)
	}
	// Restart table: written at [ContentEnd - restartCount*2, ContentEnd).
	tableStart := leafRestartTableStart(cfg, restartCount)
	for i, off := range restartTable {
		le.PutUint16(buf[tableStart+i*leafRestartTableEntrySize:], off)
	}
	return nil
}

// leafEntrySize computes the on-disk byte size of an entry. Pure
// function for the fit-ahead pass.
func leafEntrySize(e EncodedEntry, isRestart bool, prev []byte) int {
	if isRestart {
		if e.Flags&CellFlagOverflow != 0 {
			// [Flags u8][KeyLen u16][Key][OvflPage u64][TotalLen u64]
			return 1 + 2 + len(e.Key) + 8 + 8
		}
		// [Flags u8][KeyLen u16][ValueLen u32][Key][Value]
		return 1 + 2 + 4 + len(e.Key) + len(e.Value)
	}
	shared := commonPrefixLen(prev, e.Key)
	unshared := len(e.Key) - shared
	if e.Flags&CellFlagOverflow != 0 {
		// [Flags u8][SharedLen u16][UnsharedLen u16][UnsharedKey][OvflPage u64][TotalLen u64]
		return 1 + 2 + 2 + unshared + 8 + 8
	}
	// [Flags u8][SharedLen u16][UnsharedLen u16][ValueLen u32][UnsharedKey][Value]
	return 1 + 2 + 2 + 4 + unshared + len(e.Value)
}

// writeLeafEntry writes one entry at dst[0:]. Returns the number of
// bytes written. Must agree with leafEntrySize.
func writeLeafEntry(dst []byte, e EncodedEntry, isRestart bool, prev []byte) int {
	flags := e.Flags
	if isRestart {
		if flags&CellFlagOverflow != 0 {
			dst[0] = flags
			le.PutUint16(dst[1:], uint16(len(e.Key)))
			copy(dst[3:], e.Key)
			off := 3 + len(e.Key)
			le.PutUint64(dst[off:], e.OverflowPage)
			le.PutUint64(dst[off+8:], e.TotalLen)
			return off + 16
		}
		dst[0] = flags
		le.PutUint16(dst[1:], uint16(len(e.Key)))
		le.PutUint32(dst[3:], uint32(len(e.Value)))
		copy(dst[7:], e.Key)
		off := 7 + len(e.Key)
		copy(dst[off:], e.Value)
		return off + len(e.Value)
	}
	shared := commonPrefixLen(prev, e.Key)
	unshared := e.Key[shared:]
	if flags&CellFlagOverflow != 0 {
		dst[0] = flags
		le.PutUint16(dst[1:], uint16(shared))
		le.PutUint16(dst[3:], uint16(len(unshared)))
		copy(dst[5:], unshared)
		off := 5 + len(unshared)
		le.PutUint64(dst[off:], e.OverflowPage)
		le.PutUint64(dst[off+8:], e.TotalLen)
		return off + 16
	}
	dst[0] = flags
	le.PutUint16(dst[1:], uint16(shared))
	le.PutUint16(dst[3:], uint16(len(unshared)))
	le.PutUint32(dst[5:], uint32(len(e.Value)))
	copy(dst[9:], unshared)
	off := 9 + len(unshared)
	copy(dst[off:], e.Value)
	return off + len(e.Value)
}

// LeafEncodedSize computes the byte size a leaf with the given
// entries + interval would occupy. Used by the splitter / insert
// hot path to decide when a leaf must split.
func LeafEncodedSize(cfg Config, interval uint16, entries []EncodedEntry) int {
	total := leafHeaderEnd
	restartCount := 0
	for i, e := range entries {
		isRestart := uint16(i)%interval == 0
		var prev []byte
		if !isRestart {
			prev = entries[i-1].Key
		}
		if isRestart {
			restartCount++
		}
		total += leafEntrySize(e, isRestart, prev)
	}
	return total + restartCount*leafRestartTableEntrySize
}

// DecodeLeaf returns all entries from a leaf page with full keys
// reconstructed. Used by tests + tree-walk consumers; hot-path
// cursor uses incremental decode via the LeafCursor type (chunk
// 4.6).
//
// For overflow references, the entry's Value is nil and
// OverflowPage / TotalLen are populated.
func DecodeLeaf(buf []byte, cfg Config) ([]LeafEntry, error) {
	cfg.mustValidate()
	if len(buf) != int(cfg.PageSize) {
		return nil, fmt.Errorf("page: DecodeLeaf buf len %d != PageSize %d", len(buf), cfg.PageSize)
	}
	n := LeafEntryCount(buf)
	if n == 0 {
		return nil, nil
	}
	interval := LeafRestartInterval(buf)
	if interval == 0 {
		return nil, fmt.Errorf("page: DecodeLeaf RestartInterval=0")
	}
	// M3 cross-check (chunk-4.2 close-out): the per-page
	// RestartCount field MUST equal ceil(n / interval). Spec
	// invariant 3 — a forged RestartCount mislocates the restart
	// table and lets LeafRestartOffset return offsets into the
	// entries area, producing silently-wrong lookup results.
	expectRC := uint16((uint32(n) + uint32(interval) - 1) / uint32(interval))
	rc := LeafRestartCount(buf)
	if rc != expectRC {
		return nil, fmt.Errorf("page: DecodeLeaf RestartCount %d != ceil(%d/%d)=%d (corrupt header)",
			rc, n, interval, expectRC)
	}
	// The restart table lives at [contentEnd - rc*2, contentEnd);
	// no entry may extend past contentEnd - rc*2 or we'd overlap
	// the table. Pass that as the per-entry upper bound to the
	// decoder so a forged page that runs entries into the table
	// surfaces as an error rather than wrong-data.
	tableStart := leafRestartTableStart(cfg, rc)
	out := make([]LeafEntry, 0, n)
	pos := leafHeaderEnd
	var prevKey []byte
	for i := uint16(0); i < n; i++ {
		isRestart := i%interval == 0
		e, next, err := decodeLeafEntry(buf, pos, tableStart, isRestart, prevKey)
		if err != nil {
			return nil, fmt.Errorf("page: DecodeLeaf entry %d: %w", i, err)
		}
		out = append(out, e)
		prevKey = e.Key
		pos = next
	}
	return out, nil
}

// decodeLeafEntry decodes one entry starting at buf[off]. Returns
// the decoded entry, the byte offset of the next entry, and an
// error on malformed flags / out-of-range lengths.
//
// Decoder robustness contract (chunk-4.2 close-out): the decoder is
// **total** over its input — for any byte sequence within `buf`,
// decodeLeafEntry either returns a valid (LeafEntry, nextOff, nil)
// or an error. It MUST NOT panic on slice-out-of-range. This is the
// load-bearing property for Check() (chunk 11), which feeds
// arbitrary on-disk pages through DecodeLeaf to enumerate
// corruption findings.
//
// The returned entry's Key:
//   - for an inline restart: borrows from buf (zero-copy).
//   - for a delta with shared > 0: a fresh allocation
//     (`shared || unshared` cannot alias both sources in one slice).
//   - for an inline restart / delta with shared=0: borrows from buf.
//
// `contentEnd` is the upper bound on entry-area bytes — caller
// passes cfg.ContentEnd() so the bounds checks see the same horizon
// the restart table lives below.
func decodeLeafEntry(buf []byte, off, contentEnd int, isRestart bool, prev []byte) (LeafEntry, int, error) {
	// Per-section bound helper: ensures off+n <= contentEnd before
	// the caller reads `n` bytes at off.
	bound := func(o, n int) error {
		if o < 0 || n < 0 || o+n > contentEnd {
			return fmt.Errorf("length out of range: off=%d n=%d contentEnd=%d", o, n, contentEnd)
		}
		return nil
	}
	if err := bound(off, 1); err != nil {
		return LeafEntry{}, 0, err
	}
	flags := buf[off]
	if flags&^cellFlagKnownMask != 0 {
		return LeafEntry{}, 0, fmt.Errorf("unknown flag bits 0x%x", flags&^cellFlagKnownMask)
	}
	if isRestart {
		if flags&CellFlagOverflow != 0 {
			if err := bound(off+1, 2); err != nil {
				return LeafEntry{}, 0, err
			}
			keyLen := int(le.Uint16(buf[off+1:]))
			keyStart := off + 3
			if err := bound(keyStart, keyLen+16); err != nil {
				return LeafEntry{}, 0, err
			}
			ovflStart := keyStart + keyLen
			return LeafEntry{
				Flags:        flags,
				Key:          buf[keyStart : keyStart+keyLen],
				OverflowPage: le.Uint64(buf[ovflStart:]),
				TotalLen:     le.Uint64(buf[ovflStart+8:]),
			}, ovflStart + 16, nil
		}
		if err := bound(off+1, 6); err != nil {
			return LeafEntry{}, 0, err
		}
		keyLen := int(le.Uint16(buf[off+1:]))
		valLen := int(le.Uint32(buf[off+3:]))
		keyStart := off + 7
		if err := bound(keyStart, keyLen+valLen); err != nil {
			return LeafEntry{}, 0, err
		}
		valStart := keyStart + keyLen
		return LeafEntry{
			Flags: flags,
			Key:   buf[keyStart : keyStart+keyLen],
			Value: buf[valStart : valStart+valLen],
		}, valStart + valLen, nil
	}
	// Delta entry.
	if err := bound(off+1, 4); err != nil {
		return LeafEntry{}, 0, err
	}
	shared := int(le.Uint16(buf[off+1:]))
	unsharedLen := int(le.Uint16(buf[off+3:]))
	if shared > len(prev) {
		return LeafEntry{}, 0, fmt.Errorf("SharedLen %d > previous key len %d", shared, len(prev))
	}
	if flags&CellFlagOverflow != 0 {
		unsharedStart := off + 5
		if err := bound(unsharedStart, unsharedLen+16); err != nil {
			return LeafEntry{}, 0, err
		}
		ovflStart := unsharedStart + unsharedLen
		var key []byte
		if shared == 0 {
			key = buf[unsharedStart : unsharedStart+unsharedLen]
		} else {
			key = make([]byte, shared+unsharedLen)
			copy(key, prev[:shared])
			copy(key[shared:], buf[unsharedStart:unsharedStart+unsharedLen])
		}
		return LeafEntry{
			Flags:        flags,
			Key:          key,
			OverflowPage: le.Uint64(buf[ovflStart:]),
			TotalLen:     le.Uint64(buf[ovflStart+8:]),
		}, ovflStart + 16, nil
	}
	if err := bound(off+5, 4); err != nil {
		return LeafEntry{}, 0, err
	}
	valLen := int(le.Uint32(buf[off+5:]))
	unsharedStart := off + 9
	if err := bound(unsharedStart, unsharedLen+valLen); err != nil {
		return LeafEntry{}, 0, err
	}
	valStart := unsharedStart + unsharedLen
	var key []byte
	if shared == 0 {
		key = buf[unsharedStart : unsharedStart+unsharedLen]
	} else {
		key = make([]byte, shared+unsharedLen)
		copy(key, prev[:shared])
		copy(key[shared:], buf[unsharedStart:unsharedStart+unsharedLen])
	}
	return LeafEntry{
		Flags: flags,
		Key:   key,
		Value: buf[valStart : valStart+valLen],
	}, valStart + valLen, nil
}

// commonPrefixLen returns the length of the shared prefix between
// a and b.
func commonPrefixLen(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
