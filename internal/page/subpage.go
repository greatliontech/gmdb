package page

import "bytes"

// Subpage is an inline sorted value list embedded in a set keyspace leaf cell.
//
// Byte layout:
//
//	0..1:  Count     uint16   number of entries
//	2..3:  DataSize  uint16   total byte size of all entries
//	4..:   Entries
//
// Variable-size entry: ValueLen uint16 + Val bytes
// Fixed-size entry:    Val bytes (size = FixedValueSize, no length prefix)

// Subpage header size.
const subpageHeaderSize = 4 // Count(2) + DataSize(2)

// SubpageReader provides read access to an inline subpage.
type SubpageReader struct {
	data           []byte
	fixedValueSize int // 0 = variable-size
}

// NewSubpageReader wraps data as a subpage reader. fixedValueSize is 0 for
// variable-size value sets.
func NewSubpageReader(data []byte, fixedValueSize int) SubpageReader {
	return SubpageReader{data: data, fixedValueSize: fixedValueSize}
}

// Count returns the number of entries.
func (r SubpageReader) Count() int {
	return int(le.Uint16(r.data[0:]))
}

// DataSize returns the total data size field.
func (r SubpageReader) DataSize() int {
	return int(le.Uint16(r.data[2:]))
}

// TotalSize returns the total encoded size of this subpage (header + data).
func (r SubpageReader) TotalSize() int {
	return subpageHeaderSize + r.DataSize()
}

// Value returns the i-th value. For fixed-size, uses direct offset calculation.
// For variable-size, scans from the start — callers should iterate forward.
// The returned slice is borrowed from the underlying data.
func (r SubpageReader) Value(i int) []byte {
	if r.fixedValueSize > 0 {
		off := subpageHeaderSize + i*r.fixedValueSize
		return r.data[off : off+r.fixedValueSize]
	}
	// Variable-size: scan from start.
	off := subpageHeaderSize
	for range i {
		vlen := int(le.Uint16(r.data[off:]))
		off += 2 + vlen
	}
	vlen := int(le.Uint16(r.data[off:]))
	return r.data[off+2 : off+2+vlen]
}

// Search performs binary search for target in the subpage. Returns the index
// and whether an exact match was found. For fixed-size values, uses direct
// offset calculation for O(log N). For variable-size, uses a hybrid approach.
func (r SubpageReader) Search(target []byte) (int, bool) {
	count := r.Count()
	if r.fixedValueSize > 0 {
		return r.searchFixed(target, count)
	}
	return r.searchVariable(target, count)
}

func (r SubpageReader) searchFixed(target []byte, count int) (int, bool) {
	lo, hi := 0, count
	for lo < hi {
		mid := lo + (hi-lo)/2
		off := subpageHeaderSize + mid*r.fixedValueSize
		v := r.data[off : off+r.fixedValueSize]
		switch bytes.Compare(v, target) {
		case -1:
			lo = mid + 1
		case 0:
			return mid, true
		case 1:
			hi = mid
		}
	}
	return lo, false
}

func (r SubpageReader) searchVariable(target []byte, count int) (int, bool) {
	// Build offset index for binary search.
	offsets := make([]int, count)
	off := subpageHeaderSize
	for i := range count {
		offsets[i] = off
		vlen := int(le.Uint16(r.data[off:]))
		off += 2 + vlen
	}

	lo, hi := 0, count
	for lo < hi {
		mid := lo + (hi-lo)/2
		moff := offsets[mid]
		vlen := int(le.Uint16(r.data[moff:]))
		v := r.data[moff+2 : moff+2+vlen]
		switch bytes.Compare(v, target) {
		case -1:
			lo = mid + 1
		case 0:
			return mid, true
		case 1:
			hi = mid
		}
	}
	return lo, false
}

// SubpageBuilder constructs subpage data in a buffer.
type SubpageBuilder struct {
	buf            []byte
	fixedValueSize int
	count          int
	pos            int // current write position (after header)
}

// NewSubpageBuilder initializes a builder. buf is the target buffer.
// fixedValueSize is 0 for variable-size value sets.
func NewSubpageBuilder(buf []byte, fixedValueSize int) *SubpageBuilder {
	return &SubpageBuilder{
		buf:            buf,
		fixedValueSize: fixedValueSize,
		pos:            subpageHeaderSize,
	}
}

// AddValue appends a value (must be in sorted order). Returns false if no space.
func (b *SubpageBuilder) AddValue(value []byte) bool {
	needed := len(value)
	if b.fixedValueSize == 0 {
		needed += 2 // ValueLen prefix
	}
	if b.pos+needed > len(b.buf) {
		return false
	}
	if b.fixedValueSize == 0 {
		le.PutUint16(b.buf[b.pos:], uint16(len(value)))
		b.pos += 2
	}
	copy(b.buf[b.pos:], value)
	b.pos += len(value)
	b.count++
	return true
}

// Finish writes the subpage header (Count, DataSize) and returns the total
// bytes used (header + data).
func (b *SubpageBuilder) Finish() int {
	dataSize := b.pos - subpageHeaderSize
	le.PutUint16(b.buf[0:], uint16(b.count))
	le.PutUint16(b.buf[2:], uint16(dataSize))
	return b.pos
}

// SubpageSize computes the total encoded size for the given values.
func SubpageSize(values [][]byte, fixedValueSize int) int {
	size := subpageHeaderSize
	for _, v := range values {
		if fixedValueSize == 0 {
			size += 2 // ValueLen prefix
		}
		size += len(v)
	}
	return size
}
