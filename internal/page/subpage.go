package page

// Subpage encode/decode/search/insert/delete per `set-keyspace.md
// §Subpage Format`. A subpage stores a key's sorted set of values
// inline in the leaf cell's value area when the set is small enough
// to fit below the 50%-of-leaf promotion threshold
// (`set-keyspace.md §Subpage Promotion Threshold` — enforced at the
// SetKeyspace surface in chunk 6.4, not by this codec).
//
// Layout (`set-keyspace.md §Subpage Format`):
//
//	+----------+----------+---------+---------+-----+
//	| Count    | DataSize | Entry 0 | Entry 1 | ... |
//	| uint16   | uint16   |         |         |     |
//	+----------+----------+---------+---------+-----+
//
// Variable-size entry: [ValueLen uint16][Val bytes].
// Fixed-size entry:    [Val bytes] (no length prefix; uniform stride).
//
// Invariants the codec maintains and Validate checks
// (`set-keyspace.md §Invariants`):
//
//   - Inv-2 (sorted-order): values are stored in sorted (lex) order.
//   - Inv-3 (fixed-size stride): when fixedValueSize ≠ 0 every entry
//     is exactly fixedValueSize bytes; no entry carries a ValueLen
//     prefix.
//
// Empty-set policy (Inv-1): a Count=0 subpage is a structurally valid
// transient state for in-place operations (e.g. Delete that empties
// the subpage; the SetKeyspace surface at chunk 6.5 then removes the
// parent cell entirely so an empty subpage never persists). This
// codec accepts and produces Count=0 subpages; the persistence ban is
// enforced one layer up.

import (
	"bytes"
	"errors"
	"fmt"
)

// SubpageHeaderSize is the byte length of a subpage's fixed header
// (Count uint16 + DataSize uint16). The full subpage size is
// SubpageHeaderSize + DataSize.
const SubpageHeaderSize = 4

// Subpage field offsets within the 4-byte header.
const (
	subOffCount    = 0
	subOffDataSize = 2
)

// MaxSubpageDataSize is the maximum DataSize a single subpage may
// carry. The on-disk DataSize field is uint16 — at fill, the
// per-keyspace 50%-of-leaf promotion threshold enforces a much
// tighter bound (chunk 6.4). The codec rejects DataSize values that
// exceed uint16-max so a malformed buf returns ErrSubpageCorrupted
// from Validate rather than corrupting downstream reads.
const MaxSubpageDataSize = (1 << 16) - 1

// ErrSubpageCorrupted is returned by Validate / decode helpers when
// a structural invariant is violated (Count/DataSize disagree with
// enumerated entries, ValueLen overruns DataSize, sorted-order
// violated, fixed-size value with wrong length). Wrapped with a
// human-readable description at the call site.
var ErrSubpageCorrupted = errors.New("page: subpage structural corruption")

// ErrSubpageValueSize is returned by Insert / EncodeSubpage when a
// caller-supplied value's length disagrees with the keyspace's
// declared fixedValueSize (Inv-3). The SetKeyspace surface at chunk
// 6.6 maps this to the public gmdb.ErrValueSizeMismatch sentinel.
var ErrSubpageValueSize = errors.New("page: subpage value-size mismatch")

// SubpageReader provides bounded read access over a single subpage.
// Holds only a borrowed slice into the caller-supplied buf plus
// pre-decoded header fields.
//
// Caller responsibilities:
//   - buf MUST be exactly SubpageHeaderSize + DataSize bytes — slice
//     the leaf cell's value area to the subpage's stored extent
//     before passing here. NewSubpageReader bounds-checks the buf
//     length against the header's DataSize.
//   - fixedValueSize is the SetKeyspace's per-descriptor stride (0
//     for variable-size). Mis-supplying the value silently decodes
//     garbage entries (Inv-3 violation); the leaf-level caller pulls
//     this from the keyspace descriptor and has no source for an
//     inconsistent value.
type SubpageReader struct {
	buf            []byte
	count          int
	dataSize       int
	fixedValueSize uint16
}

// NewSubpageReader wraps buf as a subpage reader. Reads the 4-byte
// header to extract Count + DataSize; panics on a buf shorter than
// SubpageHeaderSize (programming error at the call site — leaf
// machinery gates buf length before invoking). Does NOT validate
// per-entry structure; callers consuming externally-sourced subpages
// MUST call Validate before any decode op.
func NewSubpageReader(buf []byte, fixedValueSize uint16) SubpageReader {
	if len(buf) < SubpageHeaderSize {
		panic(fmt.Sprintf("page: NewSubpageReader buf len %d < SubpageHeaderSize %d", len(buf), SubpageHeaderSize))
	}
	return SubpageReader{
		buf:            buf,
		count:          int(le.Uint16(buf[subOffCount:])),
		dataSize:       int(le.Uint16(buf[subOffDataSize:])),
		fixedValueSize: fixedValueSize,
	}
}

// Count returns the number of entries in the subpage.
func (r SubpageReader) Count() int { return r.count }

// DataSize returns the byte size of the entry-data region (excludes
// the 4-byte header).
func (r SubpageReader) DataSize() int { return r.dataSize }

// SizeBytes returns the total subpage byte size (header + entries).
func (r SubpageReader) SizeBytes() int { return SubpageHeaderSize + r.dataSize }

// Buf returns the underlying subpage buffer (header + entries) for
// callers that need to copy or splice the subpage at a higher layer
// (the leaf-cell builder at chunk 6.3).
func (r SubpageReader) Buf() []byte { return r.buf[:r.SizeBytes()] }

// FixedValueSize returns the per-keyspace stride this reader was
// initialised with (0 for variable-size).
func (r SubpageReader) FixedValueSize() uint16 { return r.fixedValueSize }

// Validate walks the subpage's structural surface and returns
// ErrSubpageCorrupted (wrapped with context) on any invariant
// violation. Total over its input — any byte sequence within
// SubpageHeaderSize+DataSize either returns a clean ErrSubpageCorrupted
// or nil; never panics on slice-out-of-range.
//
// Checks performed:
//
//   - Buf length ≥ SubpageHeaderSize + DataSize.
//   - DataSize ≤ MaxSubpageDataSize (uint16 hard cap).
//   - Per-entry walk: variable-size entries' ValueLen does not overrun
//     DataSize; fixed-size entries sum to exactly DataSize and Count is
//     consistent with DataSize / fixedValueSize.
//   - Entries are stored in sorted (Inv-2) order; duplicates are
//     forbidden (a SetKeyspace cannot contain the same value twice
//     per the set semantics in keyspaces.md §API split).
//
// NewSubpageReader is intentionally NOT a validation boundary: it
// only initialises header state. Callers resolving a subpage from
// disk (the chunk 6.3 leaf-walker) MUST call Validate before any
// Search / ValueAt / Insert / Delete op; in-memory subpages
// produced by this package's own Insert / Delete / EncodeSubpage
// helpers do not need re-validation because the producers
// maintain the invariants by construction.
func (r SubpageReader) Validate() error {
	if len(r.buf) < r.SizeBytes() {
		return fmt.Errorf("%w: buf len %d < header(%d) + dataSize(%d)",
			ErrSubpageCorrupted, len(r.buf), SubpageHeaderSize, r.dataSize)
	}
	if r.dataSize > MaxSubpageDataSize {
		return fmt.Errorf("%w: DataSize %d exceeds max %d",
			ErrSubpageCorrupted, r.dataSize, MaxSubpageDataSize)
	}
	if r.count == 0 {
		// Count=0 short-circuit: a structurally valid Count=0 subpage
		// MUST have DataSize=0 regardless of fixedValueSize (both
		// "zero variable entries summing to zero bytes" and "zero
		// fixed entries with zero stride contribution" land here).
		// The early return is intentional, not branch-ordering luck:
		// every malformed Count=0 layout has the inconsistency in
		// the header field pair, not in entry-region structure that
		// the per-Count-≥1 branches walk.
		if r.dataSize != 0 {
			return fmt.Errorf("%w: Count=0 but DataSize=%d (header inconsistent)",
				ErrSubpageCorrupted, r.dataSize)
		}
		return nil
	}
	if r.fixedValueSize != 0 {
		// Fixed-size: total bytes = Count * fixedValueSize, no per-entry
		// length prefix.
		expect := r.count * int(r.fixedValueSize)
		if expect != r.dataSize {
			return fmt.Errorf("%w: fixed-size DataSize %d != Count(%d) * fixedValueSize(%d) = %d",
				ErrSubpageCorrupted, r.dataSize, r.count, r.fixedValueSize, expect)
		}
		// Per-entry sorted check.
		stride := int(r.fixedValueSize)
		for i := 1; i < r.count; i++ {
			prev := r.buf[SubpageHeaderSize+(i-1)*stride : SubpageHeaderSize+i*stride]
			cur := r.buf[SubpageHeaderSize+i*stride : SubpageHeaderSize+(i+1)*stride]
			cmp := bytes.Compare(prev, cur)
			if cmp == 0 {
				return fmt.Errorf("%w: duplicate value at index %d (fixed-size)",
					ErrSubpageCorrupted, i)
			}
			if cmp > 0 {
				return fmt.Errorf("%w: out-of-order entry at index %d (fixed-size)",
					ErrSubpageCorrupted, i)
			}
		}
		return nil
	}
	// Variable-size: walk per-entry headers; each entry is
	// [ValueLen uint16][Val bytes]; sum to exactly DataSize.
	off := SubpageHeaderSize
	end := SubpageHeaderSize + r.dataSize
	var prev []byte
	for i := range r.count {
		if off+2 > end {
			return fmt.Errorf("%w: variable-size entry %d ValueLen header overruns DataSize",
				ErrSubpageCorrupted, i)
		}
		vl := int(le.Uint16(r.buf[off:]))
		bodyStart := off + 2
		bodyEnd := bodyStart + vl
		if bodyEnd > end {
			return fmt.Errorf("%w: variable-size entry %d body (ValueLen=%d) overruns DataSize",
				ErrSubpageCorrupted, i, vl)
		}
		cur := r.buf[bodyStart:bodyEnd]
		if i > 0 {
			cmp := bytes.Compare(prev, cur)
			if cmp == 0 {
				return fmt.Errorf("%w: duplicate value at index %d (variable-size)",
					ErrSubpageCorrupted, i)
			}
			if cmp > 0 {
				return fmt.Errorf("%w: out-of-order entry at index %d (variable-size)",
					ErrSubpageCorrupted, i)
			}
		}
		prev = cur
		off = bodyEnd
	}
	if off != end {
		return fmt.Errorf("%w: enumerated entries end at offset %d but DataSize implies end at %d",
			ErrSubpageCorrupted, off, end)
	}
	return nil
}

// ValueAt returns the value at index idx as a slice borrowed from the
// underlying buf. Panics on out-of-range idx (programming error;
// callers gate via Count / Search). For variable-size subpages this
// walks the entries from offset 0 in O(idx) — callers iterating
// every value should use AllValues for an amortised O(N) walk.
func (r SubpageReader) ValueAt(idx int) []byte {
	if idx < 0 || idx >= r.count {
		panic(fmt.Sprintf("page: SubpageReader.ValueAt %d out of range [0, %d)", idx, r.count))
	}
	if r.fixedValueSize != 0 {
		stride := int(r.fixedValueSize)
		off := SubpageHeaderSize + idx*stride
		return r.buf[off : off+stride]
	}
	off := SubpageHeaderSize
	for range idx {
		vl := int(le.Uint16(r.buf[off:]))
		off += 2 + vl
	}
	vl := int(le.Uint16(r.buf[off:]))
	return r.buf[off+2 : off+2+vl]
}

// AllValues invokes yield for every value in sorted order; returns
// when yield returns false or after the last value. The yielded
// slices are borrowed from the underlying buf; callers must not
// retain them past the next mutation of the subpage.
func (r SubpageReader) AllValues(yield func(value []byte) bool) {
	if r.fixedValueSize != 0 {
		stride := int(r.fixedValueSize)
		for i := range r.count {
			off := SubpageHeaderSize + i*stride
			if !yield(r.buf[off : off+stride]) {
				return
			}
		}
		return
	}
	off := SubpageHeaderSize
	for range r.count {
		vl := int(le.Uint16(r.buf[off:]))
		bodyStart := off + 2
		bodyEnd := bodyStart + vl
		if !yield(r.buf[bodyStart:bodyEnd]) {
			return
		}
		off = bodyEnd
	}
}

// Search locates target in the subpage. Returns (idx, found): if
// found, target == ValueAt(idx); if not found, idx is the insertion
// point (the position where target would be inserted to maintain
// sorted order; idx ∈ [0, Count]).
//
//   - Fixed-size: O(log N) binary search via direct offset
//     arithmetic. Caller is NOT required to pre-validate target's
//     length — a wrong-length target is compared via bytes.Compare
//     and necessarily mismatches every stored entry, so the call
//     returns (insertion-point, false) without panic. The SetKeyspace
//     surface at chunk 6.6 rejects wrong-length values at the Put
//     boundary with gmdb.ErrValueSizeMismatch BEFORE reaching here;
//     this codec's contract is "given a target, where does it go" —
//     not "is the target well-formed for this keyspace".
//   - Variable-size: O(N) linear scan. Subpages are bounded well
//     below the 50% promotion threshold, so the constant factor is
//     small in practice; the design pays linear cost here to avoid
//     a per-subpage offset table.
func (r SubpageReader) Search(target []byte) (idx int, found bool) {
	if r.count == 0 {
		return 0, false
	}
	if r.fixedValueSize != 0 {
		stride := int(r.fixedValueSize)
		lo, hi := 0, r.count
		for lo < hi {
			mid := int(uint(lo+hi) >> 1)
			off := SubpageHeaderSize + mid*stride
			cmp := bytes.Compare(r.buf[off:off+stride], target)
			if cmp == 0 {
				return mid, true
			}
			if cmp < 0 {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		return lo, false
	}
	off := SubpageHeaderSize
	for i := range r.count {
		vl := int(le.Uint16(r.buf[off:]))
		bodyStart := off + 2
		bodyEnd := bodyStart + vl
		cur := r.buf[bodyStart:bodyEnd]
		cmp := bytes.Compare(cur, target)
		if cmp == 0 {
			return i, true
		}
		if cmp > 0 {
			return i, false
		}
		off = bodyEnd
	}
	return r.count, false
}

// Insert produces a fresh subpage byte slice with value inserted in
// sorted position. Returns:
//
//   - (r.buf[:r.SizeBytes()], false, nil) if value is already present
//     (Set semantics: duplicate is a no-op; SetKeyspace.Put's
//     `added` return is false). The returned slice is the original
//     buf — caller must not mutate.
//   - (newBuf, true, nil) on a successful insert. newBuf is a freshly
//     allocated slice of length r.SizeBytes() + entrySize(value).
//   - (_, false, ErrSubpageValueSize) for a fixed-size subpage when
//     len(value) ≠ fixedValueSize.
//
// Caller-facing length budget: the caller (chunk 6.4) is responsible
// for the 50%-of-leaf promotion-threshold check BEFORE calling
// Insert. This codec does not have visibility into leaf usable space
// and will happily produce a subpage that exceeds the threshold.
func (r SubpageReader) Insert(value []byte) ([]byte, bool, error) {
	if r.fixedValueSize != 0 && len(value) != int(r.fixedValueSize) {
		return nil, false, fmt.Errorf("%w: value len %d != fixedValueSize %d",
			ErrSubpageValueSize, len(value), r.fixedValueSize)
	}
	idx, found := r.Search(value)
	if found {
		return r.buf[:r.SizeBytes()], false, nil
	}
	// Variable-size dual-walk note: Insert calls Search (O(N) linear
	// scan computing idx) then entryOffset(idx) (a second O(N) walk
	// to byte-offset). Acceptable because N is hard-capped by the
	// 50% promotion threshold to ~200 max for 4 KB pages (typically
	// far less), and the slice splice cost below is itself O(subpage
	// bytes) which dominates two short scans. If profiling later
	// shows variable-size Insert as a hot path, the optimization is
	// to have Search return (idx, byteOff, found) so entryOffset is
	// elided — same on-disk format, internal codec change only.
	entryBytes := subpageEntrySize(value, r.fixedValueSize)
	// Header-field overflow guard: Count and DataSize are uint16 on
	// disk. The 50%-of-leaf promotion-threshold check at chunk 6.4
	// keeps a well-behaved caller well below these caps, but the
	// codec's contract (godoc line ~50: "rejects DataSize values
	// that exceed uint16-max so a malformed buf returns
	// ErrSubpageCorrupted") requires that a misbehaving caller does
	// NOT silently produce a header whose declared Count / DataSize
	// truncates mod 2^16. Detect-and-reject here so the corrupted
	// bytes never escape this call.
	if r.count+1 > 0xFFFF {
		return nil, false, fmt.Errorf("%w: Insert would push Count past uint16 max (current=%d)",
			ErrSubpageCorrupted, r.count)
	}
	if r.dataSize+entryBytes > MaxSubpageDataSize {
		return nil, false, fmt.Errorf("%w: Insert would push DataSize to %d, exceeds max %d",
			ErrSubpageCorrupted, r.dataSize+entryBytes, MaxSubpageDataSize)
	}
	newBuf := make([]byte, r.SizeBytes()+entryBytes)
	insertOff := r.entryOffset(idx)
	// Header + entries [0..idx-1].
	copy(newBuf[:insertOff], r.buf[:insertOff])
	// New entry.
	writeSubpageEntry(newBuf[insertOff:], value, r.fixedValueSize)
	// Entries [idx..count-1] shifted by entryBytes.
	copy(newBuf[insertOff+entryBytes:], r.buf[insertOff:r.SizeBytes()])
	// Update header (Count + DataSize). Guarded above against
	// uint16 truncation.
	le.PutUint16(newBuf[subOffCount:], uint16(r.count+1))
	le.PutUint16(newBuf[subOffDataSize:], uint16(r.dataSize+entryBytes))
	return newBuf, true, nil
}

// Delete produces a fresh subpage byte slice with value removed.
// Returns:
//
//   - (r.buf[:r.SizeBytes()], false, nil) if value is not present
//     (the SetKeyspace surface at chunk 6.6 maps this to
//     gmdb.ErrNotFound per api-surface.md §Invariants Delete-on-miss).
//     The returned slice is the original buf — caller must not
//     mutate.
//   - (newBuf, true, nil) on a successful delete. newBuf is a freshly
//     allocated slice of length r.SizeBytes() - entrySize(removed).
//     For Count=1 deletions, newBuf is a Count=0 / DataSize=0
//     subpage (a 4-byte header); the SetKeyspace surface at chunk
//     6.5 owns the empty-set ban — empty subpages must not persist
//     and the surface removes the parent cell instead of storing
//     the 4-byte header.
//   - (_, false, ErrSubpageValueSize) for a fixed-size subpage when
//     len(value) ≠ fixedValueSize.
func (r SubpageReader) Delete(value []byte) ([]byte, bool, error) {
	if r.fixedValueSize != 0 && len(value) != int(r.fixedValueSize) {
		return nil, false, fmt.Errorf("%w: value len %d != fixedValueSize %d",
			ErrSubpageValueSize, len(value), r.fixedValueSize)
	}
	idx, found := r.Search(value)
	if !found {
		return r.buf[:r.SizeBytes()], false, nil
	}
	entryBytes := subpageEntrySize(value, r.fixedValueSize)
	removeOff := r.entryOffset(idx)
	newBuf := make([]byte, r.SizeBytes()-entryBytes)
	copy(newBuf[:removeOff], r.buf[:removeOff])
	copy(newBuf[removeOff:], r.buf[removeOff+entryBytes:r.SizeBytes()])
	le.PutUint16(newBuf[subOffCount:], uint16(r.count-1))
	le.PutUint16(newBuf[subOffDataSize:], uint16(r.dataSize-entryBytes))
	return newBuf, true, nil
}

// entryOffset returns the byte offset of the idx-th entry's first
// byte (the ValueLen field for variable-size, the value's first byte
// for fixed-size). For idx == r.count, returns the offset where a
// new entry appended at the end would start (i.e. SizeBytes()).
func (r SubpageReader) entryOffset(idx int) int {
	if r.fixedValueSize != 0 {
		return SubpageHeaderSize + idx*int(r.fixedValueSize)
	}
	off := SubpageHeaderSize
	for range idx {
		vl := int(le.Uint16(r.buf[off:]))
		off += 2 + vl
	}
	return off
}

// subpageEntrySize reports the byte size one entry would consume in
// a subpage with the given fixedValueSize.
func subpageEntrySize(value []byte, fixedValueSize uint16) int {
	if fixedValueSize != 0 {
		return int(fixedValueSize)
	}
	return 2 + len(value)
}

// writeSubpageEntry writes one entry to buf starting at offset 0.
// Caller is responsible for buf having ≥ subpageEntrySize(value,
// fixedValueSize) bytes.
func writeSubpageEntry(buf, value []byte, fixedValueSize uint16) {
	if fixedValueSize != 0 {
		copy(buf, value)
		return
	}
	le.PutUint16(buf, uint16(len(value)))
	copy(buf[2:], value)
}

// SubpagePromotionThreshold returns the byte size at which a subpage
// must be promoted to a nested B+tree per set-keyspace.md §Subpage
// Promotion Threshold ("50% of the leaf page's usable space").
//
// Usable space derivation (spec: PageSize minus header, per-page
// metadata, restart-table overhead, optional checksum footer):
//
//	ContentEnd       = PageSize - footer_if_checksums (cfg.ContentEnd())
//	per-page header  = HeaderSize (8 bytes)
//	per-page metadata = 4 bytes (RestartCount+DataEnd for compressed
//	                    OR DataEnd+Reserved for uncompressed)
//	restart-table allowance = 4 bytes (one restart-table slot — the
//	                          minimum a leaf carries; pages with more
//	                          groups have slightly less usable space)
//
// The 4-byte restart-table allowance is conservative: pages with
// multiple restart groups have a smaller true usable space, so the
// returned threshold slightly OVER-estimates capacity and promotion
// may fire one entry later than the strictest spec reading would.
// The trade-off is determinism — the threshold is a function of cfg
// only, not of the current leaf's group layout — which keeps the
// SetKeyspace.Put fast-path (subpage vs promotion check) free of
// per-page state inspection.
//
// Returned threshold is the maximum subpage byte size (header +
// entries) that may remain inline; a subpage whose size would
// exceed this value after a new insert must be promoted by the
// caller per the 4-step algorithm in §Subpage Promotion Threshold.
func SubpagePromotionThreshold(cfg Config) int {
	cfg.mustValidate()
	usable := cfg.ContentEnd() - HeaderSize - 4 - 4
	if usable < 0 {
		return 0
	}
	return usable / 2
}

// EncodeSubpage builds a subpage from the supplied sorted, deduped
// values. Returns ErrSubpageCorrupted if values is out-of-order or
// contains duplicates; ErrSubpageValueSize if any value's length
// disagrees with fixedValueSize on a fixed-size keyspace.
//
// Use cases:
//   - Demotion at chunk 6.5: a nested B+tree that shrinks to a
//     single-leaf-fits-as-subpage is rebuilt as a subpage from the
//     leaf's enumerated entries.
//   - Test fixtures and the chunk 6.4 promotion's inverse for
//     round-trip property tests.
//
// Returns a Count=0 subpage (4-byte header, all zeroes) for an
// empty values input; the empty-set ban (Inv-1) is enforced at the
// SetKeyspace surface, not here.
func EncodeSubpage(values [][]byte, fixedValueSize uint16) ([]byte, error) {
	dataSize := 0
	for i, v := range values {
		if fixedValueSize != 0 && len(v) != int(fixedValueSize) {
			return nil, fmt.Errorf("%w: values[%d] len %d != fixedValueSize %d",
				ErrSubpageValueSize, i, len(v), fixedValueSize)
		}
		if i > 0 {
			cmp := bytes.Compare(values[i-1], v)
			if cmp == 0 {
				return nil, fmt.Errorf("%w: values[%d] is duplicate of values[%d]",
					ErrSubpageCorrupted, i, i-1)
			}
			if cmp > 0 {
				return nil, fmt.Errorf("%w: values[%d] sorts before values[%d]",
					ErrSubpageCorrupted, i, i-1)
			}
		}
		dataSize += subpageEntrySize(v, fixedValueSize)
	}
	if dataSize > MaxSubpageDataSize {
		return nil, fmt.Errorf("%w: encoded DataSize %d exceeds uint16 max %d",
			ErrSubpageCorrupted, dataSize, MaxSubpageDataSize)
	}
	// Symmetric Count uint16 cap guard, mirroring the Insert path's
	// overflow guard. No reachable in-spec input today reaches this
	// case (any 65536+ entry set would necessarily exceed the
	// DataSize cap above, since the smallest entry is 1 byte fixed
	// or 2 bytes variable), but the guard preserves the codec's
	// contract that a header field can never silently truncate
	// mod 2^16 — defensive completion at the encode boundary.
	if len(values) > 0xFFFF {
		return nil, fmt.Errorf("%w: encoded Count %d exceeds uint16 max %d",
			ErrSubpageCorrupted, len(values), 0xFFFF)
	}
	buf := make([]byte, SubpageHeaderSize+dataSize)
	le.PutUint16(buf[subOffCount:], uint16(len(values)))
	le.PutUint16(buf[subOffDataSize:], uint16(dataSize))
	off := SubpageHeaderSize
	for _, v := range values {
		writeSubpageEntry(buf[off:], v, fixedValueSize)
		off += subpageEntrySize(v, fixedValueSize)
	}
	return buf, nil
}
