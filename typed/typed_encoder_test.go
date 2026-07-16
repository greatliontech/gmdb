package typed

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
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
	assertRoundTrip(t, Uint64Encoder{}, []uint64{0, 1, math.MaxUint64}, eqU64)
	assertRoundTrip(t, Uint32Encoder{}, []uint32{0, 1, math.MaxUint32}, eqU32)
	assertRoundTrip(t, Int64Encoder{}, []int64{math.MinInt64, -1, 0, 1, math.MaxInt64}, eqI64)
	assertRoundTrip(t, Int32Encoder{}, []int32{math.MinInt32, -1, 0, 1, math.MaxInt32}, eqI32)
	assertRoundTrip(t, TimeEncoder{}, []time.Time{
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
	assertLexOrder(t, Uint64Encoder{}, []uint64{0, 1, 256, 1 << 32, math.MaxUint64})
	assertLexOrder(t, Uint32Encoder{}, []uint32{0, 1, 256, math.MaxUint32})
	assertLexOrder(t, Int64Encoder{}, []int64{math.MinInt64, -1 << 32, -256, -1, 0, 1, 256, 1 << 32, math.MaxInt64})
	assertLexOrder(t, Int32Encoder{}, []int32{math.MinInt32, -65536, -1, 0, 1, 65536, math.MaxInt32})
	assertLexOrder(t, TimeEncoder{}, []time.Time{
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
		{Uint64Encoder{}.ID(), "gmdb/be-uint64"},
		{Uint32Encoder{}.ID(), "gmdb/be-uint32"},
		{Int64Encoder{}.ID(), "gmdb/be-int64"},
		{Int32Encoder{}.ID(), "gmdb/be-int32"},
		{TimeEncoder{}.ID(), "gmdb/be-time-nanos"},
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
	if _, err := (Uint64Encoder{}).Decode([]byte{1, 2, 3}); err == nil {
		t.Error("Uint64Encoder.Decode(3 bytes) = nil err, want error")
	}
	if _, err := (Uint32Encoder{}).Decode([]byte{1, 2, 3, 4, 5}); err == nil {
		t.Error("Uint32Encoder.Decode(5 bytes) = nil err, want error")
	}
	if _, err := (Int64Encoder{}).Decode(nil); err == nil {
		t.Error("Int64Encoder.Decode(nil) = nil err, want error")
	}
	if _, err := (TimeEncoder{}).Decode([]byte{1}); err == nil {
		t.Error("TimeEncoder.Decode(1 byte) = nil err, want error")
	}
	if _, err := (UUIDv4Encoder{}).Decode(make([]byte, 15)); err == nil {
		t.Error("UUIDv4 Decode(15 bytes) = nil err, want error")
	}
}

// TestTimeEncoderRange verifies in-range times encode and out-of-
// range times (beyond ~year 2262 / before ~1678) are rejected rather
// than silently wrapping to a wrong lex position (Inv-T1).
func TestTimeEncoderRange(t *testing.T) {
	enc := TimeEncoder{}
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

// JSONValue (typed-keyspaces.md §Engine-Provided Canonical Encoders,
// value encoders): generic round-trip for JSON-representable types,
// type-distinct stable IDs, loud failure on unrepresentable values.

type jsonProbe struct {
	Name    string
	Age     int64
	Scores  []float64
	Tags    map[string]string
	Blob    []byte
	When    time.Time
	Nested  *jsonProbe
	private int // invisible to JSON by design — never set in tests
}

func TestJSONValueRoundTrip(t *testing.T) {
	enc := JSONValue[jsonProbe]()
	v := jsonProbe{
		Name:   "α-probe",
		Age:    -42,
		Scores: []float64{0, 1.5, -2.25},
		Tags:   map[string]string{"a": "1", "b": ""},
		Blob:   []byte{0x00, 0xFF, 0x7F},
		When:   time.Date(2026, 7, 17, 1, 2, 3, 456789000, time.UTC),
		Nested: &jsonProbe{Name: "inner"},
	}
	b, err := enc.AppendEncode(nil, v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := enc.Decode(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != v.Name || got.Age != v.Age || !got.When.Equal(v.When) ||
		len(got.Scores) != 3 || got.Scores[2] != -2.25 ||
		got.Tags["a"] != "1" || string(got.Blob) != string(v.Blob) ||
		got.Nested == nil || got.Nested.Name != "inner" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// AppendEncode appends (the dst contract).
	pre := []byte("prefix")
	b2, err := enc.AppendEncode(pre, v)
	if err != nil {
		t.Fatal(err)
	}
	if string(b2[:6]) != "prefix" {
		t.Fatal("AppendEncode did not append to dst")
	}
}

func TestJSONValueRoundTripProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		enc := JSONValue[map[string][]int64]()
		v := rapid.MapOf(
			rapid.String(),
			rapid.SliceOfN(rapid.Int64(), 0, 8),
		).Draw(t, "v")
		b, err := enc.AppendEncode(nil, v)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		got, err := enc.Decode(b)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got) != len(v) {
			t.Fatalf("size mismatch: %d != %d", len(got), len(v))
		}
		for k, xs := range v {
			ys, ok := got[k]
			if !ok || len(ys) != len(xs) {
				t.Fatalf("key %q mismatch", k)
			}
			for i := range xs {
				if xs[i] != ys[i] {
					t.Fatalf("key %q[%d]: %d != %d", k, i, xs[i], ys[i])
				}
			}
		}
	})
}

func TestJSONValueIDsDistinctAndStable(t *testing.T) {
	a := JSONValue[jsonProbe]().ID()
	b := JSONValue[map[string]int]().ID()
	c := JSONValue[jsonProbe]().ID()
	if a == b {
		t.Fatalf("distinct types share an ID: %q", a)
	}
	if a != c {
		t.Fatalf("ID not stable: %q vs %q", a, c)
	}
	if a == "" || b == "" {
		t.Fatal("empty ID")
	}
}

// TestJSONValueIDsUseFullPackagePaths: wrapper shapes (*T, []T,
// map[...]T) and anonymous structs must embed FULL package paths —
// reflect's String() prints short names, so two same-basename
// packages' types would otherwise share a schema fingerprint and a
// value-type swap would evade ErrIndexFingerprintMismatch (silent
// misdecode).
func TestJSONValueIDsUseFullPackagePaths(t *testing.T) {
	const full = "github.com/greatliontech/gmdb/typed.jsonProbe"
	cases := map[string]string{
		"ptr":   JSONValue[*jsonProbe]().ID(),
		"slice": JSONValue[[]jsonProbe]().ID(),
		"map":   JSONValue[map[string]jsonProbe]().ID(),
		"anon": JSONValue[struct {
			P jsonProbe
			N int
		}]().ID(),
	}
	for name, id := range cases {
		if !strings.Contains(id, full) {
			t.Errorf("%s ID lacks the full package path: %q", name, id)
		}
	}
	// Wrapper distinctness across shapes.
	if cases["ptr"] == cases["slice"] {
		t.Error("*T and []T share an ID")
	}
	// Interface fields encode (dynamic value / null), so their
	// method signatures carry full paths too.
	ifaceID := JSONValue[struct {
		F interface{ M() jsonProbe }
	}]().ID()
	if !strings.Contains(ifaceID, full) {
		t.Errorf("interface-field ID lacks the full package path: %q", ifaceID)
	}
	// Anonymous structs with different field TAGS are different JSON
	// schemas — distinct IDs.
	tagA := JSONValue[struct {
		X int `json:"x"`
	}]().ID()
	tagB := JSONValue[struct {
		X int `json:"y"`
	}]().ID()
	if tagA == tagB {
		t.Error("anonymous structs with different JSON tags share an ID")
	}
}

func TestJSONValueRejectsUnrepresentable(t *testing.T) {
	if _, err := JSONValue[float64]().AppendEncode(nil, math.NaN()); err == nil {
		t.Fatal("NaN encoded without error")
	}
	if _, err := JSONValue[float64]().AppendEncode(nil, math.Inf(1)); err == nil {
		t.Fatal("+Inf encoded without error")
	}
	if _, err := JSONValue[chan int]().AppendEncode(nil, make(chan int)); err == nil {
		t.Fatal("chan encoded without error")
	}
	if _, err := JSONValue[int]().Decode([]byte("not-json")); err == nil {
		t.Fatal("malformed input decoded without error")
	}
}

// TestJSONValueInKeyspace: the encoder drives a real typed keyspace
// end to end as the VALUE side, with a canonical key encoder.
func TestJSONValueInKeyspace(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewKeyspace[string, jsonProbe]("j", StringEncoder{}, JSONValue[jsonProbe]())
	ks, err := tks.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := jsonProbe{Name: "stored", Age: 7, Tags: map[string]string{"k": "v"}}
	if err := ks.Put("row", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := ks.Get("row")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "stored" || got.Age != 7 || got.Tags["k"] != "v" {
		t.Fatalf("stored round-trip mismatch: %+v", got)
	}
}
