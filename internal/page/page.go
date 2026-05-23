package page

import (
	"encoding/binary"
	"fmt"

	"github.com/cespare/xxhash/v2"
)

var le = binary.LittleEndian

// Page type constants stored in the Type field of the page header.
const (
	TypeBranch     uint8 = 1
	TypeLeaf       uint8 = 2
	TypeOverflow   uint8 = 3
	TypeRPLSegment uint8 = 4
)

// Magic identifies a gmdb file. LE encoding produces bytes
// [0x67, 0x6D, 0x64, 0x62] = "gmdb" readable in hex dumps.
const Magic uint32 = 0x62646D67

// FormatVersion is the current on-disk format version.
const FormatVersion uint32 = 1

// Supported page-size range. PageSize is set at database creation, persisted
// on the meta page, and immutable.
const (
	MinPageSize uint32 = 4096
	MaxPageSize uint32 = 65536
)

// HeaderSize is the byte length of the common page header carried by every
// non-meta, non-bitmap page.
const HeaderSize = 8

// FooterSize is the byte length of the xxhash64 footer when PageChecksum is
// enabled. Footer occupies the last FooterSize bytes of the page.
const FooterSize = 8

// Page header field offsets within the 8-byte header.
const (
	offType            = 0
	offFlags           = 1
	offCount           = 2
	offAdditionalPages = 4
)

// ValidPageSize reports whether size is a supported page size: a power of
// two within [MinPageSize, MaxPageSize].
func ValidPageSize(size uint32) bool {
	if size < MinPageSize || size > MaxPageSize {
		return false
	}
	return size&(size-1) == 0
}

// Config bundles the page-size-dependent values that callers thread through
// the package. Built once per database Open from the meta page. Validate
// before use — every consumer (encoders, decoders, checksum helpers,
// RPLEntriesPerSegment) assumes a valid Config, and the data file's
// PageSize invariant (`file-layout.md`) is encoded against that assumption.
type Config struct {
	PageSize     uint32
	PageChecksum bool
}

// Validate reports whether c describes a supported page configuration.
// Returns an error when PageSize is not a power of two in [MinPageSize,
// MaxPageSize]; PageChecksum is unconstrained (either bool is legal).
// Boundary consumers (Open, pager construction) Validate at entry; the
// rest of the package panics on a Config it knows to be invalid since
// reaching it indicates a programming error upstream.
func (c Config) Validate() error {
	if !ValidPageSize(c.PageSize) {
		return fmt.Errorf("page: invalid PageSize %d (must be a power of two in [%d, %d])",
			c.PageSize, MinPageSize, MaxPageSize)
	}
	return nil
}

// mustValidate panics with the Validate error. Used at the package's
// internal boundaries where Config reaching the function with an invalid
// PageSize signals a caller bug.
func (c Config) mustValidate() {
	if err := c.Validate(); err != nil {
		panic(err)
	}
}

// ContentEnd returns the byte offset where in-page content ends: PageSize
// when checksums are disabled, PageSize - FooterSize when enabled. Callers
// use this as the end of the encoded payload (the footer, if any, lives
// after).
func (c Config) ContentEnd() int {
	if c.PageChecksum {
		return int(c.PageSize) - FooterSize
	}
	return int(c.PageSize)
}

// UsableSpace returns the number of content bytes between the header and
// the (optional) footer on a standard data page.
func (c Config) UsableSpace() int {
	return c.ContentEnd() - HeaderSize
}

// ReadHeader decodes the 8-byte page header at the start of buf.
func ReadHeader(buf []byte) (typ uint8, flags uint8, count uint16, additional uint32) {
	_ = buf[HeaderSize-1] // bounds check
	typ = buf[offType]
	flags = buf[offFlags]
	count = le.Uint16(buf[offCount:])
	additional = le.Uint32(buf[offAdditionalPages:])
	return
}

// WriteHeader encodes the 8-byte page header at the start of buf. The
// Flags byte is written as zero per file-layout.md §Page Header ("Must be
// zero on write"); future flag bits will land via a separate setter so
// the common write path can't accidentally encode a non-zero value.
func WriteHeader(buf []byte, typ uint8, count uint16, additional uint32) {
	_ = buf[HeaderSize-1] // bounds check
	buf[offType] = typ
	buf[offFlags] = 0
	le.PutUint16(buf[offCount:], count)
	le.PutUint32(buf[offAdditionalPages:], additional)
}

// ComputePageChecksum returns the xxhash64 of bytes 0 through
// pageSize-FooterSize-1 inclusive (the spec-mandated coverage region per
// checksums.md §Storage). buf must be exactly pageSize bytes — passing a
// larger or smaller slice panics.
//
// The pageSize parameter (rather than relying on len(buf)) is the
// load-bearing choice: it pins the coverage region to the configured
// PageSize even if the caller hands over a backing buffer of unintended
// size, which is the exact failure mode silent-bitrot detection must not
// have.
func ComputePageChecksum(buf []byte, pageSize uint32) uint64 {
	if len(buf) != int(pageSize) {
		panic(fmt.Sprintf("page: ComputePageChecksum buf len %d != PageSize %d", len(buf), pageSize))
	}
	return xxhash.Sum64(buf[:int(pageSize)-FooterSize])
}

// WritePageFooter computes the xxhash64 footer of buf and writes it into
// the last FooterSize bytes of the page region. Used at commit time on
// each dirty slab buffer, before pwrite. Panics under the same condition
// as ComputePageChecksum.
func WritePageFooter(buf []byte, pageSize uint32) {
	c := ComputePageChecksum(buf, pageSize)
	le.PutUint64(buf[int(pageSize)-FooterSize:int(pageSize)], c)
}

// VerifyPageFooter recomputes the xxhash64 footer of buf and compares it
// to the stored last 8 bytes. Returns true on match.
func VerifyPageFooter(buf []byte, pageSize uint32) bool {
	want := ComputePageChecksum(buf, pageSize)
	got := le.Uint64(buf[int(pageSize)-FooterSize : int(pageSize)])
	return want == got
}
