package page

import (
	"github.com/cespare/xxhash/v2"
)

// Meta holds deserialized meta page fields. The meta page has its own layout
// (no standard page header). Two meta pages occupy pages 0 and 1.
//
// Byte layout (144 bytes):
//
//	  0..3:   Magic            uint32
//	  4..7:   Version          uint32
//	  8..11:  PageSize         uint32
//	 12..15:  Flags            uint32
//	 16..19:  BitmapPages      uint32
//	 20..23:  Padding          (4 bytes)
//	 24..39:  UUID             [16]byte
//	 40..47:  MinSize          uint64
//	 48..55:  MaxSize          uint64
//	 56..63:  GrowStep         uint64
//	 64..71:  ShrinkThreshold  uint64
//	 72..79:  HighWaterMark    uint64
//	 80..87:  RPLHeadPage      uint64
//	 88..95:  RPLTailPage      uint64
//	 96..103: RPLEntryCount    uint64
//	104..111: NumFreePages     uint64
//	112..119: KeyspaceRoot     uint64
//	120..127: NumKeyspaces     uint64
//	128..135: TxnID            uint64
//	136..143: Checksum         uint64 (xxhash64 of bytes 0..135)
type Meta struct {
	Magic           uint32
	Version         uint32
	PageSize        uint32
	Flags           uint32
	BitmapPages     uint32
	UUID            [16]byte
	MinSize         uint64
	MaxSize         uint64
	GrowStep        uint64
	ShrinkThreshold uint64
	HighWaterMark   uint64
	RPLHeadPage     uint64
	RPLTailPage     uint64
	RPLEntryCount   uint64
	NumFreePages    uint64
	KeyspaceRoot    uint64
	NumKeyspaces    uint64
	TxnID           uint64
	Checksum        uint64
}

// Meta field offsets.
const (
	metaOffMagic           = 0
	metaOffVersion         = 4
	metaOffPageSize        = 8
	metaOffFlags           = 12
	metaOffBitmapPages     = 16
	metaOffPadding         = 20
	metaOffUUID            = 24
	metaOffMinSize         = 40
	metaOffMaxSize         = 48
	metaOffGrowStep        = 56
	metaOffShrinkThreshold = 64
	metaOffHighWaterMark   = 72
	metaOffRPLHeadPage     = 80
	metaOffRPLTailPage     = 88
	metaOffRPLEntryCount   = 96
	metaOffNumFreePages    = 104
	metaOffKeyspaceRoot    = 112
	metaOffNumKeyspaces    = 120
	metaOffTxnID           = 128
	metaOffChecksum        = 136
)

// DecodeMeta reads a meta page from buf into a Meta struct.
// Does NOT verify the checksum — use VerifyMeta for that.
func DecodeMeta(buf []byte) Meta {
	_ = buf[MetaPayloadSize-1] // bounds check
	var m Meta
	m.Magic = le.Uint32(buf[metaOffMagic:])
	m.Version = le.Uint32(buf[metaOffVersion:])
	m.PageSize = le.Uint32(buf[metaOffPageSize:])
	m.Flags = le.Uint32(buf[metaOffFlags:])
	m.BitmapPages = le.Uint32(buf[metaOffBitmapPages:])
	copy(m.UUID[:], buf[metaOffUUID:metaOffUUID+16])
	m.MinSize = le.Uint64(buf[metaOffMinSize:])
	m.MaxSize = le.Uint64(buf[metaOffMaxSize:])
	m.GrowStep = le.Uint64(buf[metaOffGrowStep:])
	m.ShrinkThreshold = le.Uint64(buf[metaOffShrinkThreshold:])
	m.HighWaterMark = le.Uint64(buf[metaOffHighWaterMark:])
	m.RPLHeadPage = le.Uint64(buf[metaOffRPLHeadPage:])
	m.RPLTailPage = le.Uint64(buf[metaOffRPLTailPage:])
	m.RPLEntryCount = le.Uint64(buf[metaOffRPLEntryCount:])
	m.NumFreePages = le.Uint64(buf[metaOffNumFreePages:])
	m.KeyspaceRoot = le.Uint64(buf[metaOffKeyspaceRoot:])
	m.NumKeyspaces = le.Uint64(buf[metaOffNumKeyspaces:])
	m.TxnID = le.Uint64(buf[metaOffTxnID:])
	m.Checksum = le.Uint64(buf[metaOffChecksum:])
	return m
}

// EncodeMeta writes the Meta fields into buf (must be >= MetaPayloadSize bytes),
// computes and stores the xxhash64 checksum.
func EncodeMeta(buf []byte, m *Meta) {
	_ = buf[MetaPayloadSize-1] // bounds check
	le.PutUint32(buf[metaOffMagic:], m.Magic)
	le.PutUint32(buf[metaOffVersion:], m.Version)
	le.PutUint32(buf[metaOffPageSize:], m.PageSize)
	le.PutUint32(buf[metaOffFlags:], m.Flags)
	le.PutUint32(buf[metaOffBitmapPages:], m.BitmapPages)
	// Zero padding bytes.
	clear(buf[metaOffPadding : metaOffPadding+4])
	copy(buf[metaOffUUID:], m.UUID[:])
	le.PutUint64(buf[metaOffMinSize:], m.MinSize)
	le.PutUint64(buf[metaOffMaxSize:], m.MaxSize)
	le.PutUint64(buf[metaOffGrowStep:], m.GrowStep)
	le.PutUint64(buf[metaOffShrinkThreshold:], m.ShrinkThreshold)
	le.PutUint64(buf[metaOffHighWaterMark:], m.HighWaterMark)
	le.PutUint64(buf[metaOffRPLHeadPage:], m.RPLHeadPage)
	le.PutUint64(buf[metaOffRPLTailPage:], m.RPLTailPage)
	le.PutUint64(buf[metaOffRPLEntryCount:], m.RPLEntryCount)
	le.PutUint64(buf[metaOffNumFreePages:], m.NumFreePages)
	le.PutUint64(buf[metaOffKeyspaceRoot:], m.KeyspaceRoot)
	le.PutUint64(buf[metaOffNumKeyspaces:], m.NumKeyspaces)
	le.PutUint64(buf[metaOffTxnID:], m.TxnID)
	// Compute and store checksum.
	m.Checksum = ComputeMetaChecksum(buf)
	le.PutUint64(buf[metaOffChecksum:], m.Checksum)
}

// VerifyMeta checks the xxhash64 checksum of a meta page. Returns true if valid.
func VerifyMeta(buf []byte) bool {
	_ = buf[MetaPayloadSize-1] // bounds check
	expected := ComputeMetaChecksum(buf)
	stored := le.Uint64(buf[metaOffChecksum:])
	return expected == stored
}

// ComputeMetaChecksum returns the xxhash64 of bytes 0 through metaOffChecksum-1
// (all fields preceding the Checksum field).
func ComputeMetaChecksum(buf []byte) uint64 {
	return xxhash.Sum64(buf[:metaOffChecksum])
}

// HasFlag returns true if the meta flags have the given flag bit set.
func (m Meta) HasFlag(flag uint32) bool {
	return m.Flags&flag != 0
}
