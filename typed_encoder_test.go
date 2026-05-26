package gmdb

import (
	"bytes"
	"math"
	"testing"
	"time"
)

// assertRoundTrip encodes each value, decodes it, and asserts equality
// via eq (Inv-T6: Decode(AppendEncode(v)) == v).
func assertRoundTrip[T any](t *testing.T, enc Encoder[T], vals []T, eq func(a, b T) bool) {
	t.Helper()
	for _, v := range vals {
		b, err := enc.AppendEncode(nil, v)
		if err != nil {
			t.Fatalf("%s AppendEncode(%v): %v", enc.ID(), v, err)
		}
		got, err := enc.Decode(b)
		if err != nil {
			t.Fatalf("%s Decode(%x): %v", enc.ID(), b, err)
		}
		if !eq(got, v) {
			t.Errorf("%s round-trip: got %v, want %v", enc.ID(), got, v)
		}
	}
}

// assertLexOrder asserts that encoding an already-sorted slice yields
// byte sequences in strictly-ascending lex order (Inv-T1: encoder lex
// order matches the intended key order). vals MUST be sorted ascending
// and distinct.
func assertLexOrder[T any](t *testing.T, enc Encoder[T], vals []T) {
	t.Helper()
	var prev []byte
	for i, v := range vals {
		b, err := enc.AppendEncode(nil, v)
		if err != nil {
			t.Fatalf("%s AppendEncode(%v): %v", enc.ID(), v, err)
		}
		if i > 0 && bytes.Compare(prev, b) >= 0 {
			t.Errorf("%s lex order: vals[%d] encodes to %x which is not > prev %x", enc.ID(), i, b, prev)
		}
		prev = b
	}
}

func TestEncoderRoundTrip(t *testing.T) {
	eqBytes := func(a, b []byte) bool { return bytes.Equal(a, b) }
	eqStr := func(a, b string) bool { return a == b }
	eqU64 := func(a, b uint64) bool { return a == b }
	eqU32 := func(a, b uint32) bool { return a == b }
	eqI64 := func(a, b int64) bool { return a == b }
	eqI32 := func(a, b int32) bool { return a == b }
	eqTime := func(a, b time.Time) bool { return a.Equal(b) }
	eqUUID := func(a, b [16]byte) bool { return a == b }

	assertRoundTrip(t, StringEncoder{}, []string{"", "a", "hello", "\x00\xff"}, eqStr)
	assertRoundTrip(t, BytesEncoder{}, [][]byte{{}, {0x00}, {0xff, 0x00, 0x01}}, eqBytes)
	assertRoundTrip(t, BEUint64Encoder{}, []uint64{0, 1, math.MaxUint64}, eqU64)
	assertRoundTrip(t, BEUint32Encoder{}, []uint32{0, 1, math.MaxUint32}, eqU32)
	assertRoundTrip(t, BEInt64Encoder{}, []int64{math.MinInt64, -1, 0, 1, math.MaxInt64}, eqI64)
	assertRoundTrip(t, BEInt32Encoder{}, []int32{math.MinInt32, -1, 0, 1, math.MaxInt32}, eqI32)
	assertRoundTrip(t, BENanosEncoder{}, []time.Time{
		time.Unix(0, -1_000_000_000).UTC(), time.Unix(0, 0).UTC(), time.Unix(1_700_000_000, 123).UTC(),
	}, eqTime)
	assertRoundTrip(t, UUIDv4Encoder{}, [][16]byte{{}, {1: 0xaa}, {15: 0xff}}, eqUUID)
	assertRoundTrip(t, UUIDv7Encoder{}, [][16]byte{{}, {0: 0x01}, {15: 0xff}}, eqUUID)
}

// TestEncoderLexOrder is the load-bearing Inv-T1 check: the signed and
// time encoders must order negatives below positives in big-endian lex
// (the sign-bit-XOR transform), not the raw two's-complement order
// (which would sort negatives ABOVE positives).
func TestEncoderLexOrder(t *testing.T) {
	assertLexOrder(t, StringEncoder{}, []string{"", "a", "aa", "ab", "b", "z"})
	assertLexOrder(t, BytesEncoder{}, [][]byte{{}, {0x00}, {0x00, 0x01}, {0x01}, {0xff}})
	assertLexOrder(t, BEUint64Encoder{}, []uint64{0, 1, 256, 1 << 32, math.MaxUint64})
	assertLexOrder(t, BEUint32Encoder{}, []uint32{0, 1, 256, math.MaxUint32})
	assertLexOrder(t, BEInt64Encoder{}, []int64{math.MinInt64, -1 << 32, -256, -1, 0, 1, 256, 1 << 32, math.MaxInt64})
	assertLexOrder(t, BEInt32Encoder{}, []int32{math.MinInt32, -65536, -1, 0, 1, 65536, math.MaxInt32})
	assertLexOrder(t, BENanosEncoder{}, []time.Time{
		time.Unix(-1_000_000, 0).UTC(), // pre-epoch
		time.Unix(0, -1).UTC(),
		time.Unix(0, 0).UTC(),
		time.Unix(0, 1).UTC(),
		time.Unix(1_700_000_000, 0).UTC(),
	})
	// UUID raw byte order.
	assertLexOrder(t, UUIDv7Encoder{}, [][16]byte{{}, {0: 0x01}, {0: 0x01, 15: 0x01}, {0: 0xff}})
}

// TestCanonicalEncoderIDs locks the canonical ID strings (Inv-T4:
// forever immutable). A change here is a deliberate, breaking decision
// (ship a new encoder + ID instead).
func TestCanonicalEncoderIDs(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{StringEncoder{}.ID(), "gmdb/string"},
		{BytesEncoder{}.ID(), "gmdb/bytes"},
		{BEUint64Encoder{}.ID(), "gmdb/be-uint64"},
		{BEUint32Encoder{}.ID(), "gmdb/be-uint32"},
		{BEInt64Encoder{}.ID(), "gmdb/be-int64"},
		{BEInt32Encoder{}.ID(), "gmdb/be-int32"},
		{BENanosEncoder{}.ID(), "gmdb/be-time-nanos"},
		{UUIDv4Encoder{}.ID(), "gmdb/uuid-v4"},
		{UUIDv7Encoder{}.ID(), "gmdb/uuid-v7"},
	}
	seen := make(map[string]struct{}, len(cases))
	for _, c := range cases {
		if c.id != c.want {
			t.Errorf("canonical encoder ID = %q, want %q (canonical IDs are immutable)", c.id, c.want)
		}
		if c.id == "" {
			t.Errorf("canonical encoder ID is empty (Inv-T2)")
		}
		if _, dup := seen[c.id]; dup {
			t.Errorf("duplicate canonical encoder ID %q (Inv-T2: distinct IDs)", c.id)
		}
		seen[c.id] = struct{}{}
	}
}

// TestEncoderDecodeLengthErrors verifies fixed-width encoders reject a
// wrong-length src rather than panicking (Encoder.Decode contract).
func TestEncoderDecodeLengthErrors(t *testing.T) {
	if _, err := (BEUint64Encoder{}).Decode([]byte{1, 2, 3}); err == nil {
		t.Error("BEUint64 Decode(3 bytes) = nil err, want error")
	}
	if _, err := (BEUint32Encoder{}).Decode([]byte{1, 2, 3, 4, 5}); err == nil {
		t.Error("BEUint32 Decode(5 bytes) = nil err, want error")
	}
	if _, err := (BEInt64Encoder{}).Decode(nil); err == nil {
		t.Error("BEInt64 Decode(nil) = nil err, want error")
	}
	if _, err := (BENanosEncoder{}).Decode([]byte{1}); err == nil {
		t.Error("BENanos Decode(1 byte) = nil err, want error")
	}
	if _, err := (UUIDv4Encoder{}).Decode(make([]byte, 15)); err == nil {
		t.Error("UUIDv4 Decode(15 bytes) = nil err, want error")
	}
}

// TestBENanosEncoderRange verifies in-range times encode and out-of-
// range times (beyond ~year 2262 / before ~1678) are rejected rather
// than silently wrapping to a wrong lex position (Inv-T1).
func TestBENanosEncoderRange(t *testing.T) {
	enc := BENanosEncoder{}
	// In range: a far-future-but-representable time round-trips.
	ok := time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := enc.AppendEncode(nil, ok); err != nil {
		t.Errorf("AppendEncode(year 2200) = %v, want nil (in range)", err)
	}
	// Out of range: year 3000 overflows int64 nanos.
	for _, bad := range []time.Time{
		time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		if _, err := enc.AppendEncode(nil, bad); err == nil {
			t.Errorf("AppendEncode(%v) = nil err, want out-of-range error", bad)
		}
	}

	// Exact int64-nanosecond boundaries: the MaxInt64 and MinInt64
	// instants are representable (accepted) and one nanosecond beyond
	// each is rejected. These pin the symmetric guard — the earlier
	// asymmetric lower bound wrongly rejected the MinInt64 instant.
	maxT := time.Unix(0, math.MaxInt64).UTC()
	minT := time.Unix(0, math.MinInt64).UTC()
	if _, err := enc.AppendEncode(nil, maxT); err != nil {
		t.Errorf("AppendEncode(MaxInt64 instant) = %v, want nil (representable boundary)", err)
	}
	if _, err := enc.AppendEncode(nil, minT); err != nil {
		t.Errorf("AppendEncode(MinInt64 instant) = %v, want nil (representable boundary)", err)
	}
	if _, err := enc.AppendEncode(nil, maxT.Add(time.Nanosecond)); err == nil {
		t.Error("AppendEncode(MaxInt64 + 1ns) = nil err, want out-of-range (would wrap)")
	}
	if _, err := enc.AppendEncode(nil, minT.Add(-time.Nanosecond)); err == nil {
		t.Error("AppendEncode(MinInt64 − 1ns) = nil err, want out-of-range (would wrap)")
	}
	// And the MinInt64 instant must round-trip (not just encode).
	b, err := enc.AppendEncode(nil, minT)
	if err != nil {
		t.Fatalf("AppendEncode(MinInt64 instant): %v", err)
	}
	if got, err := enc.Decode(b); err != nil || !got.Equal(minT) {
		t.Errorf("MinInt64-instant round-trip = (%v, %v), want (%v, nil)", got, err, minT)
	}
}

// TestBytesEncoderDecodeCopies verifies BytesEncoder.Decode returns a
// value that does not alias src (the Encoder.Decode "may not retain
// src" contract — the byte layer hands borrowed buffers).
func TestBytesEncoderDecodeCopies(t *testing.T) {
	src := []byte{1, 2, 3}
	got, err := BytesEncoder{}.Decode(src)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	src[0] = 0xff // mutate the source after decode
	if got[0] != 1 {
		t.Errorf("BytesEncoder.Decode aliases src: got[0]=%d after src mutation, want 1", got[0])
	}
}

// TestFuncEncoder exercises the FuncEncoder adapter round-trip + ID.
func TestFuncEncoder(t *testing.T) {
	enc := FuncEncoder[uint16]{
		EncodeFunc: func(dst []byte, v uint16) ([]byte, error) {
			return append(dst, byte(v>>8), byte(v)), nil
		},
		DecodeFunc: func(src []byte) (uint16, error) {
			if len(src) != 2 {
				return 0, errDecode("test/u16", len(src), 2)
			}
			return uint16(src[0])<<8 | uint16(src[1]), nil
		},
		EncoderID: "test/u16-be",
	}
	assertRoundTrip(t, enc, []uint16{0, 1, 258, math.MaxUint16}, func(a, b uint16) bool { return a == b })
	assertLexOrder(t, enc, []uint16{0, 1, 258, math.MaxUint16})
	if enc.ID() != "test/u16-be" {
		t.Errorf("FuncEncoder ID = %q, want test/u16-be", enc.ID())
	}
}
