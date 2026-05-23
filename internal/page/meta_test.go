package page

import (
	"bytes"
	"testing"
)

func sampleMeta(txnID uint64) Meta {
	return Meta{
		Magic:           Magic,
		Version:         FormatVersion,
		PageSize:        4096,
		Flags:           MetaFlagPageChecksum,
		BitmapPages:     1,
		UUID:            [16]byte{0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE, 0xBA, 0xBE},
		MinSize:         16,
		MaxSize:         4096,
		GrowStep:        16,
		ShrinkThreshold: 4,
		HighWaterMark:   3,
		RPLHeadPage:     0,
		RPLTailPage:     0,
		RPLEntryCount:   0,
		NumFreePages:    8192 - 3,
		KeyspaceRoot:    0,
		NumKeyspaces:    0,
		TxnID:           txnID,
	}
}

func TestMetaRoundTrip(t *testing.T) {
	buf := make([]byte, MetaPayloadSize)
	m := sampleMeta(42)
	EncodeMeta(buf, &m)
	got := DecodeMeta(buf)
	// Encode populated m.Checksum; the decoded copy should match exactly.
	if got != m {
		t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", got, m)
	}
	if !VerifyMeta(buf) {
		t.Fatal("VerifyMeta returned false on freshly encoded buffer")
	}
}

func TestMetaChecksumTamper(t *testing.T) {
	buf := make([]byte, MetaPayloadSize)
	m := sampleMeta(7)
	EncodeMeta(buf, &m)
	if !VerifyMeta(buf) {
		t.Fatal("baseline verify failed")
	}
	// Tamper with the TxnID; verify must fail.
	buf[metaOffTxnID] ^= 0x01
	if VerifyMeta(buf) {
		t.Fatal("verify passed despite TxnID tamper")
	}
	buf[metaOffTxnID] ^= 0x01
	// Tamper with the checksum field itself; verify must fail.
	buf[metaOffChecksum] ^= 0xFF
	if VerifyMeta(buf) {
		t.Fatal("verify passed despite checksum-slot tamper")
	}
}

func TestEncodeMetaZeroesPadding(t *testing.T) {
	buf := bytes.Repeat([]byte{0xAA}, MetaPayloadSize)
	m := sampleMeta(1)
	EncodeMeta(buf, &m)
	pad := buf[metaOffPadding : metaOffPadding+4]
	for i, b := range pad {
		if b != 0 {
			t.Fatalf("padding byte %d not zeroed: %x", i, b)
		}
	}
}

func TestActiveMetaSelection(t *testing.T) {
	makeBuf := func(m Meta) []byte {
		b := make([]byte, MetaPayloadSize)
		EncodeMeta(b, &m)
		return b
	}
	corrupt := func(b []byte) []byte {
		c := append([]byte(nil), b...)
		c[metaOffTxnID] ^= 1
		return c
	}

	m0 := sampleMeta(10)
	m1 := sampleMeta(11)

	// Both valid, higher TxnID wins.
	active, ok := ActiveMeta(makeBuf(m0), makeBuf(m1))
	if !ok || active != 1 {
		t.Errorf("both valid: active=%d ok=%v, want 1 true", active, ok)
	}
	// Both valid, meta 0 higher.
	active, ok = ActiveMeta(makeBuf(m1), makeBuf(m0))
	if !ok || active != 0 {
		t.Errorf("both valid (m0 higher): active=%d ok=%v, want 0 true", active, ok)
	}
	// Both valid, tie at TxnID=0 (the only legitimate tie, immediately
	// post-initialisation) → meta 0.
	mInit := sampleMeta(0)
	active, ok = ActiveMeta(makeBuf(mInit), makeBuf(mInit))
	if !ok || active != 0 {
		t.Errorf("tie at zero: active=%d ok=%v, want 0 true", active, ok)
	}
	// Only meta 0 valid.
	active, ok = ActiveMeta(makeBuf(m1), corrupt(makeBuf(m0)))
	if !ok || active != 0 {
		t.Errorf("only m0 valid: active=%d ok=%v, want 0 true", active, ok)
	}
	// Only meta 1 valid.
	active, ok = ActiveMeta(corrupt(makeBuf(m0)), makeBuf(m1))
	if !ok || active != 1 {
		t.Errorf("only m1 valid: active=%d ok=%v, want 1 true", active, ok)
	}
	// Neither valid.
	active, ok = ActiveMeta(corrupt(makeBuf(m0)), corrupt(makeBuf(m1)))
	if ok {
		t.Errorf("neither valid: active=%d ok=%v, want false", active, ok)
	}
}

func TestActiveMetaNonZeroTieRejected(t *testing.T) {
	makeBuf := func(m Meta) []byte {
		b := make([]byte, MetaPayloadSize)
		EncodeMeta(b, &m)
		return b
	}
	m := sampleMeta(7) // non-zero TxnID
	active, ok := ActiveMeta(makeBuf(m), makeBuf(m))
	if ok {
		t.Errorf("non-zero tie: active=%d ok=%v, want false (corruption)", active, ok)
	}
}

func TestValidateMeta(t *testing.T) {
	good := sampleMeta(1)
	if err := ValidateMeta(good); err != nil {
		t.Fatalf("Validate(good) = %v", err)
	}
	bad := good
	bad.Magic = 0xCAFEBABE
	if err := ValidateMeta(bad); err == nil {
		t.Error("Validate accepted wrong Magic")
	}
	bad = good
	bad.Version = FormatVersion + 1
	if err := ValidateMeta(bad); err == nil {
		t.Error("Validate accepted wrong Version")
	}
	bad = good
	bad.PageSize = 3000
	if err := ValidateMeta(bad); err == nil {
		t.Error("Validate accepted non-power-of-two PageSize")
	}
	bad = good
	bad.Flags |= 1 << 5 // unknown bit
	if err := ValidateMeta(bad); err == nil {
		t.Error("Validate accepted unknown Flags bit")
	}
}

func TestEncodeMetaGoldenBytes(t *testing.T) {
	// Hand-derived golden bytes: a meta with chosen fields encoded into
	// exactly MetaPayloadSize bytes, checksum included.
	m := Meta{
		Magic:           Magic,
		Version:         FormatVersion,
		PageSize:        4096,
		Flags:           MetaFlagPageChecksum | MetaFlagCheckpoint,
		BitmapPages:     1,
		UUID:            [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10},
		MinSize:         0x1100,
		MaxSize:         0x1200,
		GrowStep:        0x1300,
		ShrinkThreshold: 0x1400,
		HighWaterMark:   0x1500,
		RPLHeadPage:     0x1600,
		RPLTailPage:     0x1700,
		RPLEntryCount:   0x1800,
		NumFreePages:    0x1900,
		KeyspaceRoot:    0x1A00,
		NumKeyspaces:    0x1B00,
		TxnID:           0x1C00,
	}
	buf := make([]byte, MetaPayloadSize)
	EncodeMeta(buf, &m)
	// Assert key field positions directly against the LE encoding.
	if le.Uint32(buf[0:]) != Magic {
		t.Errorf("Magic @0 = 0x%x", le.Uint32(buf[0:]))
	}
	if le.Uint32(buf[4:]) != FormatVersion {
		t.Errorf("Version @4 = %d", le.Uint32(buf[4:]))
	}
	if le.Uint32(buf[8:]) != 4096 {
		t.Errorf("PageSize @8 = %d", le.Uint32(buf[8:]))
	}
	if le.Uint32(buf[12:]) != MetaFlagPageChecksum|MetaFlagCheckpoint {
		t.Errorf("Flags @12 = 0x%x", le.Uint32(buf[12:]))
	}
	if le.Uint32(buf[16:]) != 1 {
		t.Errorf("BitmapPages @16 = %d", le.Uint32(buf[16:]))
	}
	// Padding @20..23 must be zeroed.
	for i := 20; i < 24; i++ {
		if buf[i] != 0 {
			t.Errorf("padding @%d = %x", i, buf[i])
		}
	}
	if buf[24] != 0x01 || buf[39] != 0x10 {
		t.Errorf("UUID first/last = %x %x", buf[24], buf[39])
	}
	if le.Uint64(buf[40:]) != 0x1100 {
		t.Errorf("MinSize @40 = 0x%x", le.Uint64(buf[40:]))
	}
	if le.Uint64(buf[128:]) != 0x1C00 {
		t.Errorf("TxnID @128 = 0x%x", le.Uint64(buf[128:]))
	}
	// Checksum @136 must verify.
	if !VerifyMeta(buf) {
		t.Error("VerifyMeta failed on golden bytes")
	}
}

func TestMetaHasFlag(t *testing.T) {
	m := Meta{Flags: MetaFlagPageChecksum | MetaFlagCheckpoint}
	if !m.HasFlag(MetaFlagPageChecksum) {
		t.Error("missing PageChecksum")
	}
	if !m.HasFlag(MetaFlagCheckpoint) {
		t.Error("missing Checkpoint")
	}
	if !m.HasFlag(MetaFlagPageChecksum | MetaFlagCheckpoint) {
		t.Error("missing combined")
	}
	m2 := Meta{Flags: MetaFlagPageChecksum}
	if m2.HasFlag(MetaFlagCheckpoint) {
		t.Error("false positive Checkpoint")
	}
}
