package indexing

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// le is the on-disk byte order (file-layout.md §Byte order).
var le = binary.LittleEndian

// ErrFieldTooLarge marks a registry-entry field exceeding its
// uint16-counted encoding bound. The root package wraps it into its
// public ErrFieldTooLarge at the Tx boundary.
var ErrFieldTooLarge = errors.New("registry entry field exceeds uint16 encoding bound")

// RegistryEntry is the in-memory form of one row in a keyspace's
// per-keyspace index registry sub-tree. Encoded layout per
// indexing.md §Storage Layout:
//
//	+----------------+----------------------------------+
//	| SchemaHash     | uint64                           |
//	| Unique         | uint8                            |
//	| Kind           | uint8                            |
//	| Padding        | [6]byte                          |
//	| Root           | uint64    (index B+tree root)    |
//	| Count          | uint64    (entries in the index) |
//	| UserVersionLen | uint16                           |
//	| UserVersion    | bytes                            |
//	| ColumnCount    | uint16                           |
//	| For each col: NameLen u16 || Name bytes           |
//	| CoveringCount  | uint16                           |
//	| For each col: NameLen u16 || Name bytes           |
//	| KindPayloadLen | uint32                           |
//	| KindPayload    | bytes                            |
//	+----------------+----------------------------------+
//
// Kind plus the padding after it align the subsequent Root / Count
// uint64s at offsets 16 and 24. KindPayload is the per-kind
// metadata tail (indexing.md §Storage Layout): a future kind's
// extra state lives there behind its length prefix; the composite
// kind's canonical form is an EMPTY payload, enforced on both
// encode and decode — a stray payload would decode under a future
// kind's reader as that kind's metadata.
type RegistryEntry struct {
	SchemaHash  uint64
	Unique      bool
	Kind        Kind
	Root        uint64 // root page ID of the index's Kind=2 data sub-tree
	Count       uint64 // entries in the index
	UserVersion string
	Columns     []string // ordered; positional
	Covering    []string // ordered; positional; may be empty
	KindPayload []byte   // per-kind metadata; empty for KindComposite
}

// RegistryEntryFixedPrefixSize is the byte length of the
// fixed-size prefix (SchemaHash u64 + Unique u8 + Kind u8 +
// Padding [6]byte + Root u64 + Count u64) = 32 bytes.
const RegistryEntryFixedPrefixSize = 32

// ErrRegistryEntryInvalid marks a structurally complete but
// ill-formed registry entry — a composite entry carrying a kind
// payload. Wrapped in ErrCorrupted at the caller's boundary.
var ErrRegistryEntryInvalid = errors.New("registry entry invalid")

// ErrRegistryEntryShort marks a registry-entry decode that ran out
// of bytes mid-field. Wrapped in ErrCorrupted at the caller's
// boundary (the registry sub-tree is engine-internal; a short
// entry value means the on-disk registry is malformed).
var ErrRegistryEntryShort = errors.New("registry entry truncated")

// EncodeRegistryEntry serializes e to a fresh byte slice. Returns an
// error if any uint16-counted field overflows (UserVersion >
// 65535 bytes, column-name > 65535 bytes, ColumnCount > 65535,
// CoveringCount > 65535). The format is little-endian per the
// engine's file-layout.md §Byte order convention.
func EncodeRegistryEntry(e *RegistryEntry) ([]byte, error) {
	if e.Kind == KindComposite && len(e.KindPayload) > 0 {
		return nil, fmt.Errorf("indexing: composite entry with %d payload bytes: %w",
			len(e.KindPayload), ErrRegistryEntryInvalid)
	}
	if uint64(len(e.KindPayload)) > math.MaxUint32 {
		return nil, fmt.Errorf("indexing: KindPayload length %d exceeds uint32 max: %w",
			len(e.KindPayload), ErrFieldTooLarge)
	}
	if len(e.UserVersion) > math.MaxUint16 {
		return nil, fmt.Errorf("indexing: Version length %d exceeds uint16 max: %w",
			len(e.UserVersion), ErrFieldTooLarge)
	}
	if len(e.Columns) > math.MaxUint16 {
		return nil, fmt.Errorf("indexing: Columns length %d exceeds uint16 max: %w",
			len(e.Columns), ErrFieldTooLarge)
	}
	if len(e.Covering) > math.MaxUint16 {
		return nil, fmt.Errorf("indexing: Covering length %d exceeds uint16 max: %w",
			len(e.Covering), ErrFieldTooLarge)
	}
	for i, c := range e.Columns {
		if len(c) > math.MaxUint16 {
			return nil, fmt.Errorf("indexing: Columns[%d].Name length %d exceeds uint16 max: %w",
				i, len(c), ErrFieldTooLarge)
		}
	}
	for i, c := range e.Covering {
		if len(c) > math.MaxUint16 {
			return nil, fmt.Errorf("indexing: Covering[%d].Name length %d exceeds uint16 max: %w",
				i, len(c), ErrFieldTooLarge)
		}
	}

	// Compute size for one allocation.
	size := RegistryEntryFixedPrefixSize
	size += 2 + len(e.UserVersion)
	size += 2
	for _, c := range e.Columns {
		size += 2 + len(c)
	}
	size += 2
	for _, c := range e.Covering {
		size += 2 + len(c)
	}
	size += 4 + len(e.KindPayload)

	buf := make([]byte, size)
	off := 0

	binary.LittleEndian.PutUint64(buf[off:], e.SchemaHash)
	off += 8

	if e.Unique {
		buf[off] = 1
	}
	off += 1
	buf[off] = byte(e.Kind)
	off += 1
	// Padding [6]byte is zero — already implicit in make.
	off += 6

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

	binary.LittleEndian.PutUint32(buf[off:], uint32(len(e.KindPayload)))
	off += 4
	copy(buf[off:], e.KindPayload)

	return buf, nil
}

// DecodeRegistryEntry deserializes data into a fresh RegistryEntry.
// Returns ErrRegistryEntryShort (wrapped in ErrCorrupted at the caller)
// if any field runs past the end of data. Padding bytes after Unique
// are NOT validated — on-disk values MUST be zero per
// indexing.md §Storage Layout, but the decoder is tolerant; the
// strict integrity walk asserts the zero requirement.
func DecodeRegistryEntry(data []byte) (*RegistryEntry, error) {
	if len(data) < RegistryEntryFixedPrefixSize {
		return nil, fmt.Errorf("%w: fixed prefix needs %d bytes, got %d",
			ErrRegistryEntryShort, RegistryEntryFixedPrefixSize, len(data))
	}
	e := &RegistryEntry{}
	off := 0

	e.SchemaHash = binary.LittleEndian.Uint64(data[off:])
	off += 8

	e.Unique = data[off] != 0
	off += 1
	e.Kind = Kind(data[off])
	off += 1
	off += 6 // Padding

	e.Root = binary.LittleEndian.Uint64(data[off:])
	off += 8
	e.Count = binary.LittleEndian.Uint64(data[off:])
	off += 8

	if off+2 > len(data) {
		return nil, fmt.Errorf("%w: UserVersionLen u16 past end at offset %d", ErrRegistryEntryShort, off)
	}
	uvLen := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	if off+uvLen > len(data) {
		return nil, fmt.Errorf("%w: UserVersion(%d) past end at offset %d", ErrRegistryEntryShort, uvLen, off)
	}
	if uvLen > 0 {
		// Copy out of the (potentially mmap-borrowed) data slice so the
		// decoded struct outlives the borrow window. Per
		// api-surface.md §Byte Slice Ownership.
		e.UserVersion = string(data[off : off+uvLen])
	}
	off += uvLen

	if off+2 > len(data) {
		return nil, fmt.Errorf("%w: ColumnCount u16 past end at offset %d", ErrRegistryEntryShort, off)
	}
	colCount := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	// Forged-length bound (checksums.md §Structural and Allocation Bounds): before allocating the slice, verify the remaining bytes can
	// hold at least one 2-byte NameLen per column. A forged ColumnCount on
	// a truncated on-disk entry would otherwise force a multi-MB make()
	// before the per-iteration bounds check trips.
	if colCount*2 > len(data)-off {
		return nil, fmt.Errorf("%w: ColumnCount %d needs ≥%d bytes, %d remain at offset %d",
			ErrRegistryEntryShort, colCount, colCount*2, len(data)-off, off)
	}
	if colCount > 0 {
		e.Columns = make([]string, colCount)
		for i := range colCount {
			if off+2 > len(data) {
				return nil, fmt.Errorf("%w: Columns[%d] NameLen past end at offset %d",
					ErrRegistryEntryShort, i, off)
			}
			nLen := int(binary.LittleEndian.Uint16(data[off:]))
			off += 2
			if off+nLen > len(data) {
				return nil, fmt.Errorf("%w: Columns[%d] Name(%d) past end at offset %d",
					ErrRegistryEntryShort, i, nLen, off)
			}
			e.Columns[i] = string(data[off : off+nLen])
			off += nLen
		}
	}

	if off+2 > len(data) {
		return nil, fmt.Errorf("%w: CoveringCount u16 past end at offset %d", ErrRegistryEntryShort, off)
	}
	covCount := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	// Same forged-length pre-allocation bound as ColumnCount above (checksums.md §Structural and Allocation Bounds).
	if covCount*2 > len(data)-off {
		return nil, fmt.Errorf("%w: CoveringCount %d needs ≥%d bytes, %d remain at offset %d",
			ErrRegistryEntryShort, covCount, covCount*2, len(data)-off, off)
	}
	if covCount > 0 {
		e.Covering = make([]string, covCount)
		for i := range covCount {
			if off+2 > len(data) {
				return nil, fmt.Errorf("%w: Covering[%d] NameLen past end at offset %d",
					ErrRegistryEntryShort, i, off)
			}
			nLen := int(binary.LittleEndian.Uint16(data[off:]))
			off += 2
			if off+nLen > len(data) {
				return nil, fmt.Errorf("%w: Covering[%d] Name(%d) past end at offset %d",
					ErrRegistryEntryShort, i, nLen, off)
			}
			e.Covering[i] = string(data[off : off+nLen])
			off += nLen
		}
	}

	if off+4 > len(data) {
		return nil, fmt.Errorf("%w: KindPayloadLen u32 past end at offset %d", ErrRegistryEntryShort, off)
	}
	payloadLen := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	// Forged-length bound (checksums.md §Structural and Allocation
	// Bounds): verify the remaining bytes hold the declared payload
	// before allocating.
	if payloadLen > len(data)-off {
		return nil, fmt.Errorf("%w: KindPayload(%d) past end at offset %d",
			ErrRegistryEntryShort, payloadLen, off)
	}
	if e.Kind == KindComposite && payloadLen > 0 {
		return nil, fmt.Errorf("%w: composite entry with %d payload bytes",
			ErrRegistryEntryInvalid, payloadLen)
	}
	if payloadLen > 0 {
		// Copy out of the (potentially mmap-borrowed) data slice, as
		// for UserVersion above.
		e.KindPayload = append([]byte(nil), data[off:off+payloadLen]...)
	}
	off += payloadLen

	if off != len(data) {
		return nil, fmt.Errorf("%w: %d trailing bytes after registry entry", ErrRegistryEntryShort, len(data)-off)
	}

	return e, nil
}
