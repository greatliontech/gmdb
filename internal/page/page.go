package page

import (
	"encoding/binary"
	"fmt"

	"github.com/zeebo/xxh3"
)

var le = binary.LittleEndian

// Page type constants stored in the Type field of the page header.
// TypeLeaf is the prefix-compressed leaf variant (variable-size restart
// groups per page-formats.md §Compressed Leaf); TypeLeafUncompressed is the
// variant selected by RestartGroupTarget == 1 (full keys + positional
// offset table per §Uncompressed Leaf). Both share the 8-byte common
// header and place entry data at offset 12.
const (
	TypeBranch           uint8 = 1
	TypeLeaf             uint8 = 2
	TypeOverflow         uint8 = 3
	TypeRPLSegment       uint8 = 4
	TypeLeafUncompressed uint8 = 5
)

// IsLeafType reports whether typ is any leaf variant (compressed or
// uncompressed). The btree dispatcher uses this to gate descent on a
// page's type byte without committing to a specific encoding.
func IsLeafType(typ uint8) bool {
	return typ == TypeLeaf || typ == TypeLeafUncompressed
}

// Magic identifies a gmdb file. LE encoding produces bytes
// [0x67, 0x6D, 0x64, 0x62] = "gmdb" readable in hex dumps.
const Magic uint32 = 0x62646D67

// FormatVersion is the current on-disk format version. It is NOT bumped for
// routine pre-v1 format changes (e.g. the within-page branch
// prefix-truncation format, page-formats.md §Branch Page): with no installed
// base, a version discriminator would be backcompat scaffolding for files
// that do not exist — the clean break is to change the format and
// regenerate. It IS bumped when a change must partition a MIXED-BINARY
// FLEET on one machine: a binary that cannot correctly read or coexist
// with the other's on-disk or lock-file state must strict-reject at the
// version gate rather than misdiagnose. Two partition causes so far: the
// lock-file layout invariant (cross-process.md §Lock File Lifecycle —
// a binary with a different lock-file layout must not open the data
// file, else the size-mismatch stale arm removes a live peer's lock
// file, split brain), and a persisted-digest family change (a binary
// hashing with the other family reads every checksummed page as corrupt
// — ErrVersionMismatch is the honest error, ErrCorrupted a misdiagnosis).
// Version 2 = the boot-epoch/seqlock/slot-generation lock-file layout.
// Version 3 = XXH3-64 persisted digests (page footers, meta checksum,
// index schema fingerprint) + overflow-key cells (page-formats.md
// §Overflow-Key Cells).
const FormatVersion uint32 = 3

// Supported page-size range. PageSize is set at database creation, persisted
// on the meta page, and immutable.
const (
	MinPageSize uint32 = 4096
	MaxPageSize uint32 = 65536
)

// HeaderSize is the byte length of the common page header carried by every
// non-meta, non-bitmap page.
const HeaderSize = 8

// FooterSize is the byte length of the XXH3-64 footer when PageChecksum is
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

// DefaultRestartGroupTarget is the engine default restart-group target
// applied when Config.RestartGroupTarget == 0. Mirrors the spec at
// api-surface.md Options.RestartGroupTarget default.
const DefaultRestartGroupTarget uint16 = 16

// MaxRestartGroupTarget is the hard physical cap on RestartGroupTarget: the
// compressed-leaf restart-table entry's Count field is uint8 (per
// page-formats.md §Compressed Leaf), so groups can hold at most 255 entries.
// Config.Validate rejects values above this; callers translate to
// gmdb.ErrInvalidOptions at the public surface.
const MaxRestartGroupTarget uint16 = 255

// Config bundles the page-size-dependent values that callers thread through
// the package. Built once per database Open from the meta page (PageSize,
// PageChecksum) and threaded with the per-keyspace RestartGroupTarget when
// building or reading leaf pages. Validate before use — every consumer
// (encoders, decoders, checksum helpers, RPLEntriesPerSegment) assumes a
// valid Config, and the data file's PageSize invariant (`file-layout.md`)
// is encoded against that assumption.
//
// RestartGroupTarget bounds (per page-formats.md):
//   - 0   ⇒ engine default (DefaultRestartGroupTarget = 16).
//   - 1   ⇒ uncompressed-leaf variant (TypeLeafUncompressed).
//   - 2.. ⇒ compressed-leaf variant (TypeLeaf) with target as the maximum
//     group entry count.
//   - >255 ⇒ rejected by Validate (restart-table Count field is uint8).
type Config struct {
	PageSize           uint32
	PageChecksum       bool
	RestartGroupTarget uint16
}

// EffectiveRestartGroupTarget returns the effective target — the configured
// value if non-zero, otherwise DefaultRestartGroupTarget. Used by the leaf
// builder to decide which variant to produce and how many entries to pack
// per compressed group.
func (c Config) EffectiveRestartGroupTarget() uint16 {
	if c.RestartGroupTarget == 0 {
		return DefaultRestartGroupTarget
	}
	return c.RestartGroupTarget
}

// Validate reports whether c describes a supported page configuration.
// Returns an error when PageSize is not a power of two in [MinPageSize,
// MaxPageSize], or when RestartGroupTarget exceeds MaxRestartGroupTarget;
// PageChecksum is unconstrained (either bool is legal). Boundary consumers
// (Open, pager construction) Validate at entry; the rest of the package
// panics on a Config it knows to be invalid since reaching it indicates a
// programming error upstream.
func (c Config) Validate() error {
	if !ValidPageSize(c.PageSize) {
		return fmt.Errorf("page: invalid PageSize %d (must be a power of two in [%d, %d])",
			c.PageSize, MinPageSize, MaxPageSize)
	}
	if c.RestartGroupTarget > MaxRestartGroupTarget {
		return fmt.Errorf("page: RestartGroupTarget %d exceeds MaxRestartGroupTarget %d (Count field is uint8)",
			c.RestartGroupTarget, MaxRestartGroupTarget)
	}
	return nil
}

// MustValidate panics with the Validate error. Used at format
// boundaries — this package's own and the pager-domain formats built
// on the shared base — where a Config reaching the function with an
// invalid PageSize signals a caller bug.
func (c Config) MustValidate() {
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

// InlineThreshold returns T — the largest key length stored wholly
// inline; longer keys take the overflow-key cell form (page-formats.md
// §Overflow-Key Cells). T is the largest value such that a branch page
// holds TWO overflow-key cells at PrefixLen == 0, the split-feasibility
// floor:
//
//	content = ContentEnd - 8 (header) - 8 (leftmost ptr)
//	          - 4 (PrefixLen + Reserved)                    = ContentEnd - 20
//	percell = 4 (directory) + T (inline) + 12 (extent ref) + 8 (child)
//	2 × percell <= content  ⇒  T = (ContentEnd - 68) / 2
//
// which is (PageSize-76)/2 with checksums, (PageSize-68)/2 without —
// the exact constants pinned in limits.md §Maximum Key Size and by
// TestInlineThresholdValues. A pure function of (PageSize,
// PageChecksum): the deterministic-encoding invariant depends on every
// encoder deriving the same T.
func (c Config) InlineThreshold() int {
	return (c.ContentEnd() - 68) / 2
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

// ComputePageChecksum returns the XXH3-64 of bytes 0 through
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
	return xxh3.Hash(buf[:int(pageSize)-FooterSize])
}

// WritePageFooter computes the XXH3-64 footer of buf and writes it into
// the last FooterSize bytes of the page region. Used at commit time on
// each dirty slab buffer, before pwrite. Panics under the same condition
// as ComputePageChecksum.
func WritePageFooter(buf []byte, pageSize uint32) {
	c := ComputePageChecksum(buf, pageSize)
	le.PutUint64(buf[int(pageSize)-FooterSize:int(pageSize)], c)
}

// VerifyPageFooter recomputes the XXH3-64 footer of buf and compares it
// to the stored last 8 bytes. Returns true on match.
func VerifyPageFooter(buf []byte, pageSize uint32) bool {
	want := ComputePageChecksum(buf, pageSize)
	got := le.Uint64(buf[int(pageSize)-FooterSize : int(pageSize)])
	return want == got
}
