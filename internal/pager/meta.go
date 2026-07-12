package pager

import (
	"encoding/binary"
	"fmt"
	"github.com/greatliontech/gmdb/internal/page"

	"github.com/cespare/xxhash/v2"
)

// le is the on-disk byte order for the pager-domain formats (meta,
// RPL segment) — little-endian, matching the node formats in
// internal/page.
var le = binary.LittleEndian

// Meta-page Flags bit assignments. Per file-layout.md §Meta Page:
//   - Bit 0 (PageChecksum) is immutable across the file's lifetime.
//   - Bits 1..31 are reserved and must be zero; Open() rejects unknown
//     bits set. (Bit 1 previously held the retired Checkpoint flag —
//     the durable sub-record supersedes it.)
const (
	MetaFlagPageChecksum uint32 = 1 << 0

	// MetaFlagKnownMask is the union of currently-defined meta flag bits.
	// Open() must reject metas where (Flags &^ MetaFlagKnownMask) != 0.
	MetaFlagKnownMask = MetaFlagPageChecksum
)

// MetaPayloadSize is the fixed on-disk byte length of the meta-page payload:
// 4×4 + 4 (padding) + 16 (UUID) + 24×8 = 232 bytes. Fits in any supported
// page size.
const MetaPayloadSize = 232

// Meta-page field offsets within the 232-byte payload. Meta pages do not
// carry the common page header.
const (
	metaOffMagic                = 0
	metaOffVersion              = 4
	metaOffPageSize             = 8
	metaOffFlags                = 12
	metaOffBitmapPages          = 16
	metaOffPadding              = 20
	metaOffUUID                 = 24
	metaOffMinSize              = 40
	metaOffMaxSize              = 48
	metaOffGrowStep             = 56
	metaOffShrinkThreshold      = 64
	metaOffHighWaterMark        = 72
	metaOffRPLHeadPage          = 80
	metaOffRPLHeadTxnID         = 88
	metaOffRPLTailPage          = 96
	metaOffRPLEntryCount        = 104
	metaOffNumFreePages         = 112
	metaOffKeyspaceRoot         = 120
	metaOffNumKeyspaces         = 128
	metaOffTxnID                = 136
	metaOffDurableTxnID         = 144
	metaOffAnchoredDurableTxnID = 152
	metaOffDurableHighWaterMark = 160
	metaOffDurableRPLHeadPage   = 168
	metaOffDurableRPLHeadTxnID  = 176
	metaOffDurableRPLTailPage   = 184
	metaOffDurableRPLEntryCount = 192
	metaOffDurableNumFreePages  = 200
	metaOffDurableKeyspaceRoot  = 208
	metaOffDurableNumKeyspaces  = 216
	metaOffChecksum             = 224
)

// DurableSubRecord is the durable epoch's state-bearing projection,
// carried inside every meta (durability.md §Checkpoints and the
// durable sub-record). Crash recovery adopts these fields, never the
// carrying meta's live tree. On a self-durable meta
// (TxnID == DurableSubRecord.TxnID) every field equals its live
// counterpart.
type DurableSubRecord struct {
	// TxnID is the durable epoch — the newest transaction whose data
	// pages are confirmed on stable storage.
	TxnID uint64
	// AnchoredTxnID is the newest DurableTxnID assertion whose
	// carrying meta pwrite a COMPLETED fdatasync covered
	// (durability.md §Anchoring). Bounds RPL reclamation; never a
	// forward promise about an fsync still in flight.
	AnchoredTxnID uint64
	HighWaterMark uint64
	RPLHeadPage   uint64
	RPLHeadTxnID  uint64
	RPLTailPage   uint64
	RPLEntryCount uint64
	NumFreePages  uint64
	KeyspaceRoot  uint64
	NumKeyspaces  uint64
}

// Meta is the decoded view of one meta page. Field order matches the on-disk
// layout in file-layout.md §Meta Page.
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
	// RPLHeadTxnID is the TxnID of the segment RPLHeadPage names (0 on
	// an empty chain). Persisted so the recovery chain walk can
	// classify a head-segment read failure without trusting the —
	// possibly reclaimed-and-reused — head page itself (free-space.md
	// §Head classification requires the persisted head TxnID).
	RPLHeadTxnID  uint64
	RPLTailPage   uint64
	RPLEntryCount uint64
	NumFreePages  uint64
	KeyspaceRoot  uint64
	NumKeyspaces  uint64
	TxnID         uint64
	Durable       DurableSubRecord
	Checksum      uint64
}

// SelfDurable reports whether the meta's own commit is the durable
// epoch (its data was confirmed durable when it was written), i.e. the
// live and durable projections coincide.
func (m Meta) SelfDurable() bool { return m.Durable.TxnID == m.TxnID }

// LiveSubRecord returns the meta's LIVE state in sub-record shape —
// what a self-asserting commit or a Checkpoint bump writes as the new
// durable sub-record (AnchoredTxnID is NOT derived here; the caller
// supplies it per durability.md §Anchoring's no-forward-promise rule).
func (m Meta) LiveSubRecord() DurableSubRecord {
	return DurableSubRecord{
		TxnID:         m.TxnID,
		HighWaterMark: m.HighWaterMark,
		RPLHeadPage:   m.RPLHeadPage,
		RPLHeadTxnID:  m.RPLHeadTxnID,
		RPLTailPage:   m.RPLTailPage,
		RPLEntryCount: m.RPLEntryCount,
		NumFreePages:  m.NumFreePages,
		KeyspaceRoot:  m.KeyspaceRoot,
		NumKeyspaces:  m.NumKeyspaces,
	}
}

// HasFlag reports whether all bits in mask are set.
func (m Meta) HasFlag(mask uint32) bool { return m.Flags&mask == mask }

// DecodeMeta reads a Meta from the first MetaPayloadSize bytes of buf. Does
// not verify the checksum; use VerifyMeta separately.
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
	m.RPLHeadTxnID = le.Uint64(buf[metaOffRPLHeadTxnID:])
	m.RPLTailPage = le.Uint64(buf[metaOffRPLTailPage:])
	m.RPLEntryCount = le.Uint64(buf[metaOffRPLEntryCount:])
	m.NumFreePages = le.Uint64(buf[metaOffNumFreePages:])
	m.KeyspaceRoot = le.Uint64(buf[metaOffKeyspaceRoot:])
	m.NumKeyspaces = le.Uint64(buf[metaOffNumKeyspaces:])
	m.TxnID = le.Uint64(buf[metaOffTxnID:])
	m.Durable.TxnID = le.Uint64(buf[metaOffDurableTxnID:])
	m.Durable.AnchoredTxnID = le.Uint64(buf[metaOffAnchoredDurableTxnID:])
	m.Durable.HighWaterMark = le.Uint64(buf[metaOffDurableHighWaterMark:])
	m.Durable.RPLHeadPage = le.Uint64(buf[metaOffDurableRPLHeadPage:])
	m.Durable.RPLHeadTxnID = le.Uint64(buf[metaOffDurableRPLHeadTxnID:])
	m.Durable.RPLTailPage = le.Uint64(buf[metaOffDurableRPLTailPage:])
	m.Durable.RPLEntryCount = le.Uint64(buf[metaOffDurableRPLEntryCount:])
	m.Durable.NumFreePages = le.Uint64(buf[metaOffDurableNumFreePages:])
	m.Durable.KeyspaceRoot = le.Uint64(buf[metaOffDurableKeyspaceRoot:])
	m.Durable.NumKeyspaces = le.Uint64(buf[metaOffDurableNumKeyspaces:])
	m.Checksum = le.Uint64(buf[metaOffChecksum:])
	return m
}

// EncodeMeta writes m into the first MetaPayloadSize bytes of buf and
// stores the xxhash64 checksum of the preceding fields into the Checksum
// slot. Padding bytes are zeroed. m.Checksum is updated in place to the
// value written.
func EncodeMeta(buf []byte, m *Meta) {
	_ = buf[MetaPayloadSize-1] // bounds check
	le.PutUint32(buf[metaOffMagic:], m.Magic)
	le.PutUint32(buf[metaOffVersion:], m.Version)
	le.PutUint32(buf[metaOffPageSize:], m.PageSize)
	le.PutUint32(buf[metaOffFlags:], m.Flags)
	le.PutUint32(buf[metaOffBitmapPages:], m.BitmapPages)
	clear(buf[metaOffPadding : metaOffPadding+4])
	copy(buf[metaOffUUID:], m.UUID[:])
	le.PutUint64(buf[metaOffMinSize:], m.MinSize)
	le.PutUint64(buf[metaOffMaxSize:], m.MaxSize)
	le.PutUint64(buf[metaOffGrowStep:], m.GrowStep)
	le.PutUint64(buf[metaOffShrinkThreshold:], m.ShrinkThreshold)
	le.PutUint64(buf[metaOffHighWaterMark:], m.HighWaterMark)
	le.PutUint64(buf[metaOffRPLHeadPage:], m.RPLHeadPage)
	le.PutUint64(buf[metaOffRPLHeadTxnID:], m.RPLHeadTxnID)
	le.PutUint64(buf[metaOffRPLTailPage:], m.RPLTailPage)
	le.PutUint64(buf[metaOffRPLEntryCount:], m.RPLEntryCount)
	le.PutUint64(buf[metaOffNumFreePages:], m.NumFreePages)
	le.PutUint64(buf[metaOffKeyspaceRoot:], m.KeyspaceRoot)
	le.PutUint64(buf[metaOffNumKeyspaces:], m.NumKeyspaces)
	le.PutUint64(buf[metaOffTxnID:], m.TxnID)
	le.PutUint64(buf[metaOffDurableTxnID:], m.Durable.TxnID)
	le.PutUint64(buf[metaOffAnchoredDurableTxnID:], m.Durable.AnchoredTxnID)
	le.PutUint64(buf[metaOffDurableHighWaterMark:], m.Durable.HighWaterMark)
	le.PutUint64(buf[metaOffDurableRPLHeadPage:], m.Durable.RPLHeadPage)
	le.PutUint64(buf[metaOffDurableRPLHeadTxnID:], m.Durable.RPLHeadTxnID)
	le.PutUint64(buf[metaOffDurableRPLTailPage:], m.Durable.RPLTailPage)
	le.PutUint64(buf[metaOffDurableRPLEntryCount:], m.Durable.RPLEntryCount)
	le.PutUint64(buf[metaOffDurableNumFreePages:], m.Durable.NumFreePages)
	le.PutUint64(buf[metaOffDurableKeyspaceRoot:], m.Durable.KeyspaceRoot)
	le.PutUint64(buf[metaOffDurableNumKeyspaces:], m.Durable.NumKeyspaces)
	m.Checksum = ComputeMetaChecksum(buf)
	le.PutUint64(buf[metaOffChecksum:], m.Checksum)
}

// ComputeMetaChecksum returns the xxhash64 of the fields preceding the
// Checksum slot: bytes 0 through metaOffChecksum-1. The meta-page Magic /
// Version / PageSize / Flags / ... are all covered; the trailing 8 bytes
// are the checksum itself.
func ComputeMetaChecksum(buf []byte) uint64 {
	return xxhash.Sum64(buf[:metaOffChecksum])
}

// MetaChecksumOffsetForTest exposes the byte offset of the meta checksum
// slot (== the length of the checksummed prefix) so the crash-consistency
// harness can synthesize torn meta writes that land within the checksummed
// payload — the only tear that a valid old meta cannot absorb. Test-only.
func MetaChecksumOffsetForTest() int { return metaOffChecksum }

// ValidateMeta reports whether m is a well-formed meta-page payload as
// observed after decoding from disk. Magic and Version are checked
// against the package constants; PageSize must satisfy page.ValidPageSize;
// Flags must not carry any bits outside MetaFlagKnownMask
// (file-layout.md §Meta Page: "Open() must reject databases where any
// unknown flag bit is set").
//
// Does NOT verify the checksum — that requires the encoded byte buffer
// and is exposed via VerifyMeta. Callers typically VerifyMeta first to
// detect torn writes, then ValidateMeta on the decoded struct to detect
// out-of-band fields.
func ValidateMeta(m Meta) error {
	if m.Magic != page.Magic {
		return fmt.Errorf("pager: meta Magic mismatch: got 0x%08x, want 0x%08x", m.Magic, page.Magic)
	}
	if m.Version != page.FormatVersion {
		return fmt.Errorf("pager: meta Version mismatch: got %d, want %d", m.Version, page.FormatVersion)
	}
	if !page.ValidPageSize(m.PageSize) {
		return fmt.Errorf("pager: meta PageSize invalid: %d", m.PageSize)
	}
	if unknown := m.Flags &^ MetaFlagKnownMask; unknown != 0 {
		return fmt.Errorf("pager: meta Flags has unknown bits set: 0x%x", unknown)
	}
	return nil
}

// VerifyMeta recomputes the xxhash64 of the meta-page prefix and compares
// it against the stored Checksum field. Returns true on match.
func VerifyMeta(buf []byte) bool {
	_ = buf[MetaPayloadSize-1] // bounds check
	want := ComputeMetaChecksum(buf)
	got := le.Uint64(buf[metaOffChecksum:])
	return want == got
}

// ActiveMeta selects the active meta page given the two candidate
// payloads — the ONE selection every consumer uses (durability.md
// §One selection, two projections): live paths use the winner's live
// fields, crash recovery adopts its durable sub-record. Per
// file-layout.md §Meta Page and the entailed dual-meta invariant: the
// active meta is the one with the highest TxnID whose checksum is
// valid.
//
// Tie-break rules:
//   - One meta valid, one not → the valid one wins.
//   - Both valid, distinct TxnIDs → the higher wins.
//   - Both valid, TxnIDs both zero → meta 0 wins. Tie-at-zero is the
//     expected immediately-post-initialisation state, when both metas
//     are byte-identical.
//   - Both valid, equal non-zero TxnIDs → ok=false (corruption). The
//     commit protocol writes a strictly-greater TxnID per commit, so
//     observing two metas with equal non-zero TxnIDs means the protocol
//     was violated; active-meta selection is undefined and the caller
//     must surface this rather than guess.
//   - Neither valid → ok=false.
func ActiveMeta(meta0, meta1 []byte) (active int, ok bool) {
	ok0 := VerifyMeta(meta0)
	ok1 := VerifyMeta(meta1)
	switch {
	case ok0 && ok1:
		return pickHighestTxnID(le.Uint64(meta0[metaOffTxnID:]), le.Uint64(meta1[metaOffTxnID:]))
	case ok0:
		return 0, true
	case ok1:
		return 1, true
	default:
		return 0, false
	}
}

// pickHighestTxnID is the dual-meta TxnID tie-break over two VALID
// metas (file-layout.md §Meta Page, entailed dual-meta invariant):
// higher wins; a tie at zero is the legitimate immediately-post-
// initialisation state (meta 0 wins, both images byte-identical); an
// equal NON-zero pair is a commit-protocol violation (the protocol
// writes a strictly-greater TxnID per commit), so selection is
// undefined and ok=false — the caller surfaces corruption rather than
// guessing.
func pickHighestTxnID(txn0, txn1 uint64) (active int, ok bool) {
	switch {
	case txn1 > txn0:
		return 1, true
	case txn0 > txn1:
		return 0, true
	case txn0 == 0:
		return 0, true
	default:
		return 0, false
	}
}
