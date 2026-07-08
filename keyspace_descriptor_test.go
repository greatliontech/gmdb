package gmdb

import (
	"bytes"
	"github.com/thegrumpylion/gmdb/internal/page"
	"strings"
	"testing"
)

// Chunk-5.3 tests promote four spec-tier invariants from keyspaces.md
// to enforcement at the codec layer:
//
//   #1 (40-byte fixed format + field order/sizes) →
//      TestKeyspaceDescriptorRoundTripAllFields + the explicit
//      per-offset tests below.
//   #2 (Kind ∈ {0, 1, 2}) →
//      TestKeyspaceDescriptorValidateRejectsUnknownKind.
//   #5 (FixedValueSize ≠ 0 ⇒ Kind == 1) →
//      TestKeyspaceDescriptorValidateRejectsFixedValueSizeOnNonSet.
//   Reserved-bytes-zero clause-explicit →
//      TestKeyspaceDescriptorValidateRejectsNonZeroReserved.
//
// The codec is the first code able to violate these invariants; per the
// chunk-5.1 enforcement schedule, the codec round-trip and validate
// rejections are the load-bearing promotions. The chunk-5.4 API surface
// inherits via callsite use of these primitives.

func TestKeyspaceDescriptorSize(t *testing.T) {
	if keyspaceDescriptorSize != 40 {
		t.Fatalf("keyspaceDescriptorSize = %d, want 40 (8+8+1+2+8+2+8+3)",
			keyspaceDescriptorSize)
	}
}

// TestKeyspaceDescriptorRoundTripAllFields promotes invariant #1 — the
// 40-byte fixed format with the exact field order from keyspaces.md
// §Keyspace Descriptor. Encode then Decode with distinguishable values
// per field; assert every field round-trips and the encoded form has the
// expected byte layout (each field's offset and width).
func TestKeyspaceDescriptorRoundTripAllFields(t *testing.T) {
	d := keyspaceDescriptor{
		Root:               0x0123456789ABCDEF,
		Count:              0xFEDCBA9876543210,
		Kind:               keyspaceKindSetKeyspace,
		FixedValueSize:     0x1234,
		NextSeq:            0xAABBCCDDEEFF0011,
		RestartGroupTarget: 32,
		IndexRegistryRoot:  0x1122334455667788,
	}

	buf := make([]byte, keyspaceDescriptorSize)
	encodeKeyspaceDescriptor(buf, d)

	got := decodeKeyspaceDescriptor(buf)
	if got != d {
		t.Errorf("round-trip mismatch:\n got=%+v\nwant=%+v", got, d)
	}

	// Field-by-field byte-layout assertions. Each line pins one
	// field's offset and width — a reordering or resizing of any
	// field in keyspaces.md §Keyspace Descriptor would fail here
	// before silently misrouting reads.
	checks := []struct {
		name      string
		offset    int
		width     int
		littleEnd uint64
	}{
		{"Root", 0, 8, 0x0123456789ABCDEF},
		{"Count", 8, 8, 0xFEDCBA9876543210},
		{"Kind", 16, 1, uint64(keyspaceKindSetKeyspace)},
		{"FixedValueSize", 17, 2, 0x1234},
		{"NextSeq", 19, 8, 0xAABBCCDDEEFF0011},
		{"RestartGroupTarget", 27, 2, 32},
		{"IndexRegistryRoot", 29, 8, 0x1122334455667788},
	}
	for _, c := range checks {
		var got uint64
		switch c.width {
		case 1:
			got = uint64(buf[c.offset])
		case 2:
			got = uint64(le.Uint16(buf[c.offset:]))
		case 8:
			got = le.Uint64(buf[c.offset:])
		}
		if got != c.littleEnd {
			t.Errorf("field %s at offset %d width %d: got 0x%x, want 0x%x",
				c.name, c.offset, c.width, got, c.littleEnd)
		}
	}
	// Reserved bytes [37..40) must be zero after Encode.
	for i := 37; i < 40; i++ {
		if buf[i] != 0 {
			t.Errorf("reserved byte %d = 0x%02x, want 0", i, buf[i])
		}
	}

	// Canonical-form determinism: Encode is a pure function of d, so
	// Encode → Decode → Encode produces byte-identical output. Pins
	// against a future change that introduces nondeterminism (e.g. a
	// reserved-byte slot used for a hash or counter).
	buf2 := make([]byte, keyspaceDescriptorSize)
	encodeKeyspaceDescriptor(buf2, got)
	if !bytes.Equal(buf, buf2) {
		t.Errorf("Encode is non-deterministic:\n first=%x\nsecond=%x", buf, buf2)
	}
}

func TestKeyspaceDescriptorRoundTripZero(t *testing.T) {
	var d keyspaceDescriptor
	buf := make([]byte, keyspaceDescriptorSize)
	encodeKeyspaceDescriptor(buf, d)
	if !bytes.Equal(buf, make([]byte, keyspaceDescriptorSize)) {
		t.Errorf("encoded zero descriptor = %x, want all zero", buf)
	}
	got := decodeKeyspaceDescriptor(buf)
	if got != (keyspaceDescriptor{}) {
		t.Errorf("decode of all-zero buf = %+v, want zero struct", got)
	}
	if err := validateKeyspaceDescriptor(buf, got); err != nil {
		t.Errorf("validate of all-zero (Kind=0, empty keyspace): %v", err)
	}
}

func TestKeyspaceDescriptorEncodeOverwritesReservedBytes(t *testing.T) {
	// A buffer pre-filled with non-zero bytes must come out clean in
	// the reserved region after Encode — Encode owns the canonical
	// 40 bytes.
	buf := make([]byte, keyspaceDescriptorSize)
	for i := range buf {
		buf[i] = 0xAB
	}
	encodeKeyspaceDescriptor(buf, keyspaceDescriptor{Kind: keyspaceKindKeyspace})
	for i := 37; i < 40; i++ {
		if buf[i] != 0 {
			t.Errorf("reserved byte %d not cleared by Encode: 0x%02x", i, buf[i])
		}
	}
}

// TestKeyspaceDescriptorValidateRejectsUnknownKind promotes invariant #2
// (Kind ∈ {0, 1, 2}).
func TestKeyspaceDescriptorValidateRejectsUnknownKind(t *testing.T) {
	// Forge a descriptor with Kind = 3 (out of {0,1,2}) at the codec
	// level. CreateKeyspace's API surface (chunk 5.4) does not expose
	// a Kind setter, so the violation path is on-disk corruption +
	// reload, which is exactly the §Invariants violation case.
	buf := make([]byte, keyspaceDescriptorSize)
	encodeKeyspaceDescriptor(buf, keyspaceDescriptor{Kind: keyspaceKindKeyspace})
	buf[16] = 3 // overwrite Kind byte directly

	d := decodeKeyspaceDescriptor(buf)
	err := validateKeyspaceDescriptor(buf, d)
	if err == nil {
		t.Fatal("validateKeyspaceDescriptor with Kind=3: want error, got nil")
	}
	if !strings.Contains(err.Error(), "Kind") {
		t.Errorf("error doesn't mention Kind: %v", err)
	}

	// Also verify each accepted Kind passes.
	for _, k := range []uint8{
		keyspaceKindKeyspace,
		keyspaceKindSetKeyspace,
		keyspaceKindIndexInternal,
	} {
		var d2 keyspaceDescriptor
		d2.Kind = k
		buf2 := make([]byte, keyspaceDescriptorSize)
		encodeKeyspaceDescriptor(buf2, d2)
		if err := validateKeyspaceDescriptor(buf2, decodeKeyspaceDescriptor(buf2)); err != nil {
			t.Errorf("validateKeyspaceDescriptor Kind=%d: %v", k, err)
		}
	}
}

// TestKeyspaceDescriptorValidateRejectsFixedValueSizeOnNonSet promotes
// invariant #5: FixedValueSize meaningful only when Kind == 1.
// Specifically guards the keyspaces.md violation case: "a mutable or
// wrong-kind FixedValueSize silently re-interprets the on-disk subpage
// entry stride."
func TestKeyspaceDescriptorValidateRejectsFixedValueSizeOnNonSet(t *testing.T) {
	for _, kind := range []uint8{
		keyspaceKindKeyspace,
		keyspaceKindIndexInternal,
	} {
		d := keyspaceDescriptor{Kind: kind, FixedValueSize: 8}
		buf := make([]byte, keyspaceDescriptorSize)
		encodeKeyspaceDescriptor(buf, d)
		err := validateKeyspaceDescriptor(buf, decodeKeyspaceDescriptor(buf))
		if err == nil {
			t.Errorf("validateKeyspaceDescriptor Kind=%d FixedValueSize=8: want error, got nil",
				kind)
			continue
		}
		if !strings.Contains(err.Error(), "FixedValueSize") {
			t.Errorf("error doesn't mention FixedValueSize: %v", err)
		}
	}

	// Kind == SetKeyspace with FixedValueSize > 0 is the canonical
	// fixed-size set keyspace — must pass.
	d := keyspaceDescriptor{Kind: keyspaceKindSetKeyspace, FixedValueSize: 8}
	buf := make([]byte, keyspaceDescriptorSize)
	encodeKeyspaceDescriptor(buf, d)
	if err := validateKeyspaceDescriptor(buf, decodeKeyspaceDescriptor(buf)); err != nil {
		t.Errorf("validateKeyspaceDescriptor Kind=Set FixedValueSize=8: %v", err)
	}

	// Kind == SetKeyspace with FixedValueSize == 0 is the
	// variable-size set keyspace — must also pass.
	d2 := keyspaceDescriptor{Kind: keyspaceKindSetKeyspace, FixedValueSize: 0}
	buf2 := make([]byte, keyspaceDescriptorSize)
	encodeKeyspaceDescriptor(buf2, d2)
	if err := validateKeyspaceDescriptor(buf2, decodeKeyspaceDescriptor(buf2)); err != nil {
		t.Errorf("validateKeyspaceDescriptor Kind=Set FixedValueSize=0: %v", err)
	}
}

// TestKeyspaceDescriptorValidateRejectsNonZeroReserved promotes the
// reserved-bytes-zero clause from keyspaces.md §Keyspace Descriptor.
func TestKeyspaceDescriptorValidateRejectsNonZeroReserved(t *testing.T) {
	for off := 37; off < 40; off++ {
		buf := make([]byte, keyspaceDescriptorSize)
		encodeKeyspaceDescriptor(buf, keyspaceDescriptor{Kind: keyspaceKindKeyspace})
		buf[off] = 0xFF

		err := validateKeyspaceDescriptor(buf, decodeKeyspaceDescriptor(buf))
		if err == nil {
			t.Errorf("validateKeyspaceDescriptor with reserved[%d]=0xFF: want error, got nil",
				off-37)
			continue
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Errorf("error doesn't mention reserved: %v", err)
		}
	}
}

// TestKeyspaceDescriptorValidateRejectsRestartGroupTargetOverflow
// promotes the clause-explicit cap from keyspaces.md §Keyspace
// Descriptor: "Values > 255 are rejected by Open() and
// Tx.SetKeyspaceConfig() with ErrInvalidOptions — the compressed-leaf
// restart-table Count field is uint8."
func TestKeyspaceDescriptorValidateRejectsRestartGroupTargetOverflow(t *testing.T) {
	// 256 is the smallest invalid value.
	d := keyspaceDescriptor{Kind: keyspaceKindKeyspace, RestartGroupTarget: 256}
	buf := make([]byte, keyspaceDescriptorSize)
	encodeKeyspaceDescriptor(buf, d)
	err := validateKeyspaceDescriptor(buf, decodeKeyspaceDescriptor(buf))
	if err == nil {
		t.Fatal("validateKeyspaceDescriptor RestartGroupTarget=256: want error, got nil")
	}
	if !strings.Contains(err.Error(), "RestartGroupTarget") {
		t.Errorf("error doesn't mention RestartGroupTarget: %v", err)
	}

	// 255 is the largest valid value (the physical cap).
	d.RestartGroupTarget = page.MaxRestartGroupTarget
	encodeKeyspaceDescriptor(buf, d)
	if err := validateKeyspaceDescriptor(buf, decodeKeyspaceDescriptor(buf)); err != nil {
		t.Errorf("validateKeyspaceDescriptor RestartGroupTarget=255: %v", err)
	}

	// 0 (engine default) is valid.
	d.RestartGroupTarget = 0
	encodeKeyspaceDescriptor(buf, d)
	if err := validateKeyspaceDescriptor(buf, decodeKeyspaceDescriptor(buf)); err != nil {
		t.Errorf("validateKeyspaceDescriptor RestartGroupTarget=0: %v", err)
	}

	// 1 (uncompressed leaf selector) is valid.
	d.RestartGroupTarget = 1
	encodeKeyspaceDescriptor(buf, d)
	if err := validateKeyspaceDescriptor(buf, decodeKeyspaceDescriptor(buf)); err != nil {
		t.Errorf("validateKeyspaceDescriptor RestartGroupTarget=1: %v", err)
	}
}

// TestKeyspaceDescriptorEncodeDecodeBufferTooSmall pins the bounds-check
// behavior — a smaller buffer must panic deterministically (slice OOR)
// so a sizing bug surfaces at the test layer rather than via a heap
// stomp on disk. Matches DecodeMeta's bounds-check pattern.
func TestKeyspaceDescriptorEncodeBufferTooSmall(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("encodeKeyspaceDescriptor on 39-byte buf: want panic, got none")
		}
	}()
	buf := make([]byte, keyspaceDescriptorSize-1)
	encodeKeyspaceDescriptor(buf, keyspaceDescriptor{})
}

func TestKeyspaceDescriptorDecodeBufferTooSmall(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("decodeKeyspaceDescriptor on 39-byte buf: want panic, got none")
		}
	}()
	buf := make([]byte, keyspaceDescriptorSize-1)
	_ = decodeKeyspaceDescriptor(buf)
}

func TestKeyspaceDescriptorValidateBufferTooSmall(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("validateKeyspaceDescriptor on 39-byte buf: want panic, got none")
		}
	}()
	buf := make([]byte, keyspaceDescriptorSize-1)
	// The upfront `_ = buf[keyspaceDescriptorSize-1]` slice-index
	// triggers an out-of-range panic on a 39-byte slice regardless of
	// the descriptor's Kind. A valid Kind is supplied so the test would
	// not pass for any reason other than the bounds-check.
	_ = validateKeyspaceDescriptor(buf, keyspaceDescriptor{Kind: keyspaceKindKeyspace})
}
