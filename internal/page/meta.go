package page

import (
	"fmt"

	"github.com/cespare/xxhash/v2"
)

// Meta-page Flags bit assignments. Per file-layout.md §Meta Page:
//   - Bit 0 (PageChecksum) is immutable across the file's lifetime.
//   - Bit 1 (Checkpoint)   is mutable per commit.
//   - Bits 2..31 are reserved; Open() rejects unknown bits set.
const (
	MetaFlagPageChecksum uint32 = 1 << 0
	MetaFlagCheckpoint   uint32 = 1 << 1

	// MetaFlagKnownMask is the union of currently-defined meta flag bits.
	// Open() must reject metas where (Flags &^ MetaFlagKnownMask) != 0.
	MetaFlagKnownMask = MetaFlagPageChecksum | MetaFlagCheckpoint
)

// MetaPayloadSize is the fixed on-disk byte length of the meta-page payload:
// 4×4 + 4 (padding) + 16 (UUID) + 13×8 = 144 bytes. Fits in any supported
// page size.
const MetaPayloadSize = 144

// Meta-page field offsets within the 144-byte payload. Meta pages do not
// carry the common page header.
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
	RPLTailPage     uint64
	RPLEntryCount   uint64
	NumFreePages    uint64
	KeyspaceRoot    uint64
	NumKeyspaces    uint64
	TxnID           uint64
	Checksum        uint64
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
	m.RPLTailPage = le.Uint64(buf[metaOffRPLTailPage:])
	m.RPLEntryCount = le.Uint64(buf[metaOffRPLEntryCount:])
	m.NumFreePages = le.Uint64(buf[metaOffNumFreePages:])
	m.KeyspaceRoot = le.Uint64(buf[metaOffKeyspaceRoot:])
	m.NumKeyspaces = le.Uint64(buf[metaOffNumKeyspaces:])
	m.TxnID = le.Uint64(buf[metaOffTxnID:])
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
	le.PutUint64(buf[metaOffRPLTailPage:], m.RPLTailPage)
	le.PutUint64(buf[metaOffRPLEntryCount:], m.RPLEntryCount)
	le.PutUint64(buf[metaOffNumFreePages:], m.NumFreePages)
	le.PutUint64(buf[metaOffKeyspaceRoot:], m.KeyspaceRoot)
	le.PutUint64(buf[metaOffNumKeyspaces:], m.NumKeyspaces)
	le.PutUint64(buf[metaOffTxnID:], m.TxnID)
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

// ValidateMeta reports whether m is a well-formed meta-page payload as
// observed after decoding from disk. Magic and Version are checked
// against the package constants; PageSize must satisfy ValidPageSize;
// Flags must not carry any bits outside MetaFlagKnownMask
// (file-layout.md §Meta Page: "Open() must reject databases where any
// unknown flag bit is set").
//
// Does NOT verify the checksum — that requires the encoded byte buffer
// and is exposed via VerifyMeta. Callers typically VerifyMeta first to
// detect torn writes, then ValidateMeta on the decoded struct to detect
// out-of-band fields.
func ValidateMeta(m Meta) error {
	if m.Magic != Magic {
		return fmt.Errorf("page: meta Magic mismatch: got 0x%08x, want 0x%08x", m.Magic, Magic)
	}
	if m.Version != FormatVersion {
		return fmt.Errorf("page: meta Version mismatch: got %d, want %d", m.Version, FormatVersion)
	}
	if !ValidPageSize(m.PageSize) {
		return fmt.Errorf("page: meta PageSize invalid: %d", m.PageSize)
	}
	if unknown := m.Flags &^ MetaFlagKnownMask; unknown != 0 {
		return fmt.Errorf("page: meta Flags has unknown bits set: 0x%x", unknown)
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

// ActiveMetaCheckpointPreferring implements durability.md §Recovery:
//
//  1. Discard metas with invalid xxhash64 checksum.
//  2. Of the valid metas, select the one with the highest TxnID
//     whose MetaFlagCheckpoint flag is **set**.
//  3. If neither valid meta has Checkpoint set (SyncLazy-only DB
//     never Checkpoint()'d), select the higher-TxnID valid meta
//     and return noCheckpoint=true so the caller can warn.
//  4. Neither valid → ok=false.
//
// The promotion of the durability.md §Recovery rule; the
// raw highest-TxnID selector (ActiveMeta) is preserved for tests and
// callers who want the pre-checkpoint behaviour.
//
// Tie-break rules within (2): identical to ActiveMeta — equal
// non-zero TxnIDs is a commit-protocol violation (ok=false).
func ActiveMetaCheckpointPreferring(meta0, meta1 []byte) (active int, noCheckpoint bool, ok bool) {
	ok0 := VerifyMeta(meta0)
	ok1 := VerifyMeta(meta1)
	switch {
	case ok0 && ok1:
		txn0 := le.Uint64(meta0[metaOffTxnID:])
		txn1 := le.Uint64(meta1[metaOffTxnID:])
		flags0 := le.Uint32(meta0[metaOffFlags:])
		flags1 := le.Uint32(meta1[metaOffFlags:])
		cp0 := flags0&MetaFlagCheckpoint != 0
		cp1 := flags1&MetaFlagCheckpoint != 0
		switch {
		case cp0 && cp1:
			// Both have Checkpoint; highest TxnID wins, tie-break
			// matches ActiveMeta (zero-tie → 0; equal-non-zero →
			// protocol violation).
			switch {
			case txn1 > txn0:
				return 1, false, true
			case txn0 > txn1:
				return 0, false, true
			case txn0 == 0:
				return 0, false, true
			default:
				return 0, false, false
			}
		case cp0:
			return 0, false, true
		case cp1:
			return 1, false, true
		default:
			// Neither checkpointed — fall back to highest-TxnID
			// selection per durability.md step 3, but signal
			// noCheckpoint so the caller logs a warning.
			switch {
			case txn1 > txn0:
				return 1, true, true
			case txn0 > txn1:
				return 0, true, true
			case txn0 == 0:
				return 0, true, true
			default:
				return 0, true, false
			}
		}
	case ok0:
		// Only one valid meta — its Checkpoint flag is informational;
		// we still surface noCheckpoint=true if it's clear so the
		// caller can warn that recovery accepted a non-checkpoint
		// meta.
		flags0 := le.Uint32(meta0[metaOffFlags:])
		return 0, flags0&MetaFlagCheckpoint == 0, true
	case ok1:
		flags1 := le.Uint32(meta1[metaOffFlags:])
		return 1, flags1&MetaFlagCheckpoint == 0, true
	default:
		return 0, false, false
	}
}

// ActiveMeta selects the active meta page given the two candidate
// payloads. Per file-layout.md §Meta Page and the entailed dual-meta
// invariant: the active meta is the one with the highest TxnID whose
// checksum is valid.
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
//
// The checkpoint-flag precedence rule from durability.md is layered on
// top by the SyncMode-aware caller; this function returns the
// raw highest-valid-TxnID winner.
func ActiveMeta(meta0, meta1 []byte) (active int, ok bool) {
	ok0 := VerifyMeta(meta0)
	ok1 := VerifyMeta(meta1)
	switch {
	case ok0 && ok1:
		txn0 := le.Uint64(meta0[metaOffTxnID:])
		txn1 := le.Uint64(meta1[metaOffTxnID:])
		switch {
		case txn1 > txn0:
			return 1, true
		case txn0 > txn1:
			return 0, true
		case txn0 == 0:
			return 0, true
		default:
			// Equal non-zero TxnIDs — commit-protocol violation.
			return 0, false
		}
	case ok0:
		return 0, true
	case ok1:
		return 1, true
	default:
		return 0, false
	}
}
