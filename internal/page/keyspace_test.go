package page

import "testing"

func TestKsDescRoundTrip(t *testing.T) {
	orig := KsDesc{
		Root:           12345,
		Count:          67890,
		Kind:           KindSetKeyspace,
		FixedValueSize: 8,
		NextSeq:        42,
	}

	buf := make([]byte, KsDescSize)
	EncodeKsDesc(buf, &orig)
	got := DecodeKsDesc(buf)

	if got != orig {
		t.Errorf("round-trip mismatch:\n got: %+v\nwant: %+v", got, orig)
	}
}

func TestKsDescZeroReserved(t *testing.T) {
	d := KsDesc{Root: 1, Count: 2, Kind: KindKeyspace, NextSeq: 3}
	buf := make([]byte, KsDescSize)
	// Pre-fill with non-zero to verify reserved bytes are zeroed.
	for i := range buf {
		buf[i] = 0xFF
	}
	EncodeKsDesc(buf, &d)

	// Check reserved bytes (27..31) are zero.
	for i := ksOffReserved; i < KsDescSize; i++ {
		if buf[i] != 0 {
			t.Errorf("reserved byte at offset %d = 0x%02x, want 0x00", i, buf[i])
		}
	}
}

func TestKsDescKeyspaceType(t *testing.T) {
	d := KsDesc{Kind: KindKeyspace, FixedValueSize: 0}
	buf := make([]byte, KsDescSize)
	EncodeKsDesc(buf, &d)
	got := DecodeKsDesc(buf)
	if got.Kind != KindKeyspace {
		t.Errorf("Kind = %d, want %d", got.Kind, KindKeyspace)
	}
	if got.FixedValueSize != 0 {
		t.Errorf("FixedValueSize = %d, want 0", got.FixedValueSize)
	}
}
