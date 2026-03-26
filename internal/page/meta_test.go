package page

import (
	"crypto/rand"
	"testing"
)

func testMeta() Meta {
	var uuid [16]byte
	rand.Read(uuid[:])
	return Meta{
		Magic:           Magic,
		Version:         FormatVersion,
		PageSize:        4096,
		Flags:           MetaFlagPageChecksum,
		BitmapPages:     2048,
		UUID:            uuid,
		MinSize:         256,
		MaxSize:         67108864,
		GrowStep:        1024,
		ShrinkThreshold: 4096,
		HighWaterMark:   3000,
		RPLHeadPage:     100,
		RPLTailPage:     50,
		RPLEntryCount:   500,
		NumFreePages:    1000,
		KeyspaceRoot:    2050,
		NumKeyspaces:    3,
		TxnID:           42,
	}
}

func TestMetaRoundTrip(t *testing.T) {
	orig := testMeta()
	buf := make([]byte, MetaPayloadSize)
	EncodeMeta(buf, &orig)

	got := DecodeMeta(buf)
	// Checksum is set by EncodeMeta.
	if got != orig {
		t.Errorf("round-trip mismatch:\n got: %+v\nwant: %+v", got, orig)
	}
}

func TestMetaVerify(t *testing.T) {
	m := testMeta()
	buf := make([]byte, MetaPayloadSize)
	EncodeMeta(buf, &m)

	if !VerifyMeta(buf) {
		t.Fatal("VerifyMeta failed on valid meta")
	}
}

func TestMetaDetectsCorruption(t *testing.T) {
	m := testMeta()
	buf := make([]byte, MetaPayloadSize)
	EncodeMeta(buf, &m)

	// Flip a bit in the TxnID field.
	buf[metaOffTxnID] ^= 0x01

	if VerifyMeta(buf) {
		t.Fatal("VerifyMeta passed on corrupted meta")
	}
}

func TestMetaDetectsMagicCorruption(t *testing.T) {
	m := testMeta()
	buf := make([]byte, MetaPayloadSize)
	EncodeMeta(buf, &m)

	buf[0] ^= 0xFF

	if VerifyMeta(buf) {
		t.Fatal("VerifyMeta passed with corrupted magic")
	}
}

func TestMetaHasFlag(t *testing.T) {
	m := Meta{Flags: MetaFlagPageChecksum | MetaFlagCheckpoint}
	if !m.HasFlag(MetaFlagPageChecksum) {
		t.Error("HasFlag(PageChecksum) = false, want true")
	}
	if !m.HasFlag(MetaFlagCheckpoint) {
		t.Error("HasFlag(Checkpoint) = false, want true")
	}
	if m.HasFlag(1 << 2) {
		t.Error("HasFlag(bit2) = true, want false")
	}
}

func TestMetaPaddingZeroed(t *testing.T) {
	m := testMeta()
	buf := make([]byte, MetaPayloadSize)
	// Pre-fill with non-zero.
	for i := range buf {
		buf[i] = 0xFF
	}
	EncodeMeta(buf, &m)

	for i := metaOffPadding; i < metaOffPadding+4; i++ {
		if buf[i] != 0 {
			t.Errorf("padding byte at offset %d = 0x%02x, want 0x00", i, buf[i])
		}
	}
}
