// Package page implements serialization and deserialization for all gmdb
// on-disk page formats. It operates on []byte slices with no I/O or OS
// dependencies. All multi-byte integers use little-endian byte order via
// encoding/binary.
package page

import "encoding/binary"

var le = binary.LittleEndian

// Page type constants (stored in the Type field of the page header).
const (
	TypeBranch     uint8 = 1
	TypeLeaf       uint8 = 2
	TypeOverflow   uint8 = 3
	TypeRPLSegment uint8 = 4
)

// CellFlags bit masks for leaf entry cell flags.
const (
	CellFlagOverflow   uint8 = 1 << 0 // value stored in overflow pages
	CellFlagMultiValue uint8 = 1 << 1 // multi-value (subpage or nested B+tree)
	CellFlagNestedTree uint8 = 1 << 2 // nested B+tree (only valid when MultiValue is set)
)

// Keyspace descriptor Kind values.
const (
	KindKeyspace    uint8 = 0
	KindSetKeyspace uint8 = 1
)

// Meta flag bit masks.
const (
	MetaFlagPageChecksum uint32 = 1 << 0
	MetaFlagCheckpoint   uint32 = 1 << 1
)

// Size constants.
const (
	HeaderSize       = 8  // common page header
	Ptr0Size         = 8  // leftmost child pointer in branch pages
	CellDirEntrySize = 4  // branch cell directory entry: Offset(2) + KeyLen(2)
	ChildPtrSize     = 8  // child pointer in branch cell data
	CRC32Size        = 4  // CRC32C footer
	MetaPayloadSize  = 144 // meta page payload (Magic through Checksum)
	KeyspaceDescSize       = 32 // keyspace descriptor

	RestartInterval = 16 // leaf prefix compression restart interval
)

// Header field offsets within a page.
const (
	offType            = 0
	offFlags           = 1
	offCount           = 2
	offAdditionalPages = 4
)

// Supported page size range.
const (
	MinPageSize = 4096
	MaxPageSize = 65536
)

// Magic number identifying a gmdb file. LE encoding produces bytes
// [0x67, 0x6D, 0x64, 0x62] = "gmdb" readable in hex dumps.
const Magic uint32 = 0x62646D67

// FormatVersion is the current database format version.
const FormatVersion uint32 = 1

// PageConfig holds page-size-dependent constants. Created once per database
// open and threaded through all reader/builder constructors.
type PageConfig struct {
	PageSize     uint32
	PageChecksum bool
}

// UsableSpace returns the number of content bytes available in a standard
// data page (PageSize minus header, minus optional CRC32C footer).
func (c PageConfig) UsableSpace() int {
	n := int(c.PageSize) - HeaderSize
	if c.PageChecksum {
		n -= CRC32Size
	}
	return n
}

// ContentEnd returns the byte offset where content ends in a page
// (PageSize, or PageSize - CRC32Size when checksums are enabled).
func (c PageConfig) ContentEnd() int {
	if c.PageChecksum {
		return int(c.PageSize) - CRC32Size
	}
	return int(c.PageSize)
}

// MaxKeySize returns the maximum key size determined by branch page
// constraints: a branch must fit at least 2 keys. Each key needs a
// 4-byte cell directory entry, the key bytes, and an 8-byte child pointer.
func (c PageConfig) MaxKeySize() int {
	usable := c.UsableSpace()
	// Branch layout after header: Ptr0(8) + N*(CellDirEntry(4)) + N*(Key + ChildPtr(8))
	// Minimum N=2: usable >= 8 + 2*4 + 2*(keyLen + 8) = 32 + 2*keyLen
	return (usable - Ptr0Size - 2*CellDirEntrySize - 2*ChildPtrSize) / 2
}

// BitmapPages returns the number of bitmap pages required for the given
// MaxSize (in pages).
func (c PageConfig) BitmapPages(maxSizePages uint64) uint32 {
	bitsPerPage := uint64(c.PageSize) * 8
	totalPages := maxSizePages
	return uint32((totalPages + bitsPerPage - 1) / bitsPerPage)
}

// FirstDataPage returns the first data page ID (after meta + bitmap pages).
func (c PageConfig) FirstDataPage(bitmapPages uint32) uint64 {
	return 2 + uint64(bitmapPages)
}

// ReadHeader reads the common 8-byte page header from buf.
func ReadHeader(buf []byte) (typ uint8, flags uint8, count uint16, additional uint32) {
	_ = buf[7] // bounds check
	typ = buf[offType]
	flags = buf[offFlags]
	count = le.Uint16(buf[offCount:])
	additional = le.Uint32(buf[offAdditionalPages:])
	return
}

// WriteHeader writes the common 8-byte page header into buf.
func WriteHeader(buf []byte, typ uint8, flags uint8, count uint16, additional uint32) {
	_ = buf[7] // bounds check
	buf[offType] = typ
	buf[offFlags] = flags
	le.PutUint16(buf[offCount:], count)
	le.PutUint32(buf[offAdditionalPages:], additional)
}

// ValidPageSize returns true if size is a supported page size
// (power of 2 between MinPageSize and MaxPageSize).
func ValidPageSize(size uint32) bool {
	if size < MinPageSize || size > MaxPageSize {
		return false
	}
	return size&(size-1) == 0
}
