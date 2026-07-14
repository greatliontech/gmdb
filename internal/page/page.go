package page

import (
	"encoding/binary"
	"fmt"

	"github.com/zeebo/xxh3"
)

var le = binary.LittleEndian

// Page type constants stored in the Type field of the page header.
// TypeLeaf is the interleaved prefix-compressed leaf variant
// (variable-size restart groups per page-formats.md §Compressed Leaf,
// value bytes following each entry's key bytes); TypeLeafSegregated is
// the segregated prefix-compressed variant (§Segregated Leaf — pure
// headers+keys entry stream, value bytes in a separate region located
// by per-entry VOff); TypeLeafUncompressed is the variant selected by
// RestartGroupTarget == 1 (full keys + positional offset table per
// §Uncompressed Leaf). All share the 8-byte common header; entry data
// starts at offset 12 (segregated: 14 — one extra ValueEnd u16 header
// field). Readers dispatch a page's layout by its type byte alone,
// never by keyspace configuration (page-formats.md §Invariants).
const (
	TypeBranch           uint8 = 1
	TypeLeaf             uint8 = 2
	TypeOverflow         uint8 = 3
	TypeRPLSegment       uint8 = 4
	TypeLeafUncompressed uint8 = 5
	TypeLeafSegregated   uint8 = 6
)

// IsLeafType reports whether typ is any leaf variant. The btree
// dispatcher uses this to gate descent on a page's type byte without
// committing to a specific encoding.
func IsLeafType(typ uint8) bool {
	return typ == TypeLeaf || typ == TypeLeafUncompressed || typ == TypeLeafSegregated
}

// Magic identifies a gmdb file. LE encoding produces bytes
// [0x67, 0x6D, 0x64, 0x62] = "gmdb" readable in hex dumps.
const Magic uint32 = 0x62646D67

// FormatVersion is the current on-disk format version. It is NOT bumped for
// routine pre-v1 format changes (e.g. the branch-layout replacement,
// page-formats.md §Plain Branch): with no installed
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
const DefaultRestartGroupTarget uint16 = 6

// LeafLayout selects the compressed-leaf layout variant a builder
// produces (page-formats.md §Leaf Page). Readers always dispatch by
// the page's type byte — the config value never affects decoding.
type LeafLayout uint8

const (
	// LeafLayoutDefault defers to the engine default (segregated).
	LeafLayoutDefault LeafLayout = 0
	// LeafLayoutInterleaved builds TypeLeaf pages: each entry's value
	// bytes follow its key bytes in one stream.
	LeafLayoutInterleaved LeafLayout = 1
	// LeafLayoutSegregated builds TypeLeafSegregated pages: pure
	// headers+keys entry stream; value bytes in a separate region.
	LeafLayoutSegregated LeafLayout = 2
)

// Valid reports whether l is a defined LeafLayout value.
func (l LeafLayout) Valid() bool { return l <= LeafLayoutSegregated }

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

	// LeafLayout selects the compressed-leaf variant the builders
	// produce (keyspaces.md §Per-Keyspace Configuration NodeLayouts;
	// LeafLayoutDefault resolves to segregated). Ignored when
	// RestartGroupTarget == 1 (the uncompressed variant overrides).
	// Decode-side machinery never consults it — readers dispatch on
	// the page type byte.
	LeafLayout LeafLayout
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

// EffectiveLeafType returns the page type the leaf builders produce
// under this config: TypeLeafUncompressed at RestartGroupTarget == 1,
// otherwise the declared compressed layout (LeafLayoutDefault ⇒
// segregated, the engine default per keyspaces.md).
func (c Config) EffectiveLeafType() uint8 {
	if c.EffectiveRestartGroupTarget() == 1 {
		return TypeLeafUncompressed
	}
	if c.LeafLayout == LeafLayoutInterleaved {
		return TypeLeaf
	}
	return TypeLeafSegregated
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
	if !c.LeafLayout.Valid() {
		return fmt.Errorf("page: unknown LeafLayout %d", c.LeafLayout)
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
// §Overflow-Key Cells). T is one shared constant across every layout
// variant — (PageSize-76)/2 with checksums, (PageSize-68)/2 without —
// chosen so every BRANCH layout holds TWO worst-case overflow-key
// cells per page and every LEAF layout holds one maximal-form entry
// (the per-layout split-feasibility floors, page-formats.md §The
// inline threshold T; the plain branch needs 2T+72 <= PageSize with
// checksums, the tightest current budget is the segregated branch at
// 2T+74). The exact constants are pinned in limits.md §Maximum Key
// Size and by TestInlineThresholdValues; the floors by
// TestLeafFloorOneMaximalEntryEveryLayout and the branch round-trip
// floor fixtures. A pure function of (PageSize, PageChecksum): the
// deterministic-encoding invariant depends on every encoder deriving
// the same T, and a per-layout T would move the extent cut when a key
// crosses layout boundaries.
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
