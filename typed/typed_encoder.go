package typed

import (
	"encoding/binary"
	"fmt"
	"time"
)

// Typed-layer encoders (typed-keyspaces.md §Encoder interface +
// §Engine-Provided Canonical Encoders). The typed API serialises Go
// values to bytes through an Encoder[T]; the byte layer stores the
// bytes. Two contracts make the encoders load-bearing:
//
//   - Key ordering (typed-keyspaces.md §Invariants): a key Encoder MUST produce byte sequences
//     whose lex order matches the desired key order, so range / prefix
//     queries route correctly. The canonical integer encoders use
//     big-endian with a sign-bit XOR for signed types — NOT zigzag,
//     which is not lex-preserving for big-endian byte order.
//   - ID stability (typed-keyspaces.md §Invariants): ID() is a stable, non-empty, unique
//     string hashed into a typed index's schema fingerprint. Canonical
//     engine IDs are forever immutable — a bug fix ships under a NEW id
//     + type (e.g. "gmdb/be-int64/v2"), never by mutating an existing
//     encoding under its old id.

// Encoder handles serialization between a Go type and byte slices.
//
// AppendEncode appends the encoded form of v to dst and returns the
// extended buffer (the standard Go append pattern, so callers can pass
// a reusable dst[:0] to eliminate per-call allocations). It returns an
// error to reject values that cannot be represented.
//
// Decode deserializes src into a value of type T, returning an error to
// surface malformed or truncated data rather than panicking. Decode
// must return a value independent of src (it may not retain src), since
// the byte layer hands it borrowed buffers (api-surface.md §Byte Slice
// Ownership).
//
// ID returns a stable, non-empty string identifier for this encoder
// type, hashed into the schema fingerprint of any typed index that uses
// it. Two distinct encoders MUST NOT share an ID. Recommended naming:
// "<pkg>/<type>[/<version>]".
type Encoder[T any] interface {
	AppendEncode(dst []byte, v T) ([]byte, error)
	Decode(src []byte) (T, error)
	ID() string
}

// FuncEncoder adapts plain functions into the Encoder interface for
// simple stateless cases. EncoderID must be a stable non-empty string
// when the encoder is referenced by a typed index (see Encoder.ID).
type FuncEncoder[T any] struct {
	EncodeFunc func(dst []byte, v T) ([]byte, error)
	DecodeFunc func(src []byte) (T, error)
	EncoderID  string
}

func (f FuncEncoder[T]) AppendEncode(dst []byte, v T) ([]byte, error) { return f.EncodeFunc(dst, v) }
func (f FuncEncoder[T]) Decode(src []byte) (T, error)                 { return f.DecodeFunc(src) }
func (f FuncEncoder[T]) ID() string                                   { return f.EncoderID }

// errDecode reports a malformed/truncated byte sequence for a canonical
// encoder (wrong length). Surfaced to the typed caller, not wrapped in
// gmdb.ErrCorrupted: a decode failure on user data usually means the wrong
// encoder was supplied, not engine corruption.
func errDecode(enc string, got, want int) error {
	return fmt.Errorf("gmdb: %s decode: got %d bytes, want %d", enc, got, want)
}

// --- string / bytes ----------------------------------------------

// StringEncoder encodes Go strings as their raw UTF-8 bytes; lex order
// of the bytes is natural string order (no normalization).
type StringEncoder struct{}

func (StringEncoder) AppendEncode(dst []byte, v string) ([]byte, error) {
	return append(dst, v...), nil
}
func (StringEncoder) Decode(src []byte) (string, error) { return string(src), nil }
func (StringEncoder) ID() string                        { return "gmdb/string" }

// BytesEncoder is the identity encoder for []byte; lex order is natural
// byte order. Decode returns a copy so the result does not alias the
// byte layer's borrowed buffer (Encoder.Decode "may not retain src").
type BytesEncoder struct{}

func (BytesEncoder) AppendEncode(dst []byte, v []byte) ([]byte, error) {
	return append(dst, v...), nil
}
func (BytesEncoder) Decode(src []byte) ([]byte, error) {
	out := make([]byte, len(src))
	copy(out, src)
	return out, nil
}
func (BytesEncoder) ID() string { return "gmdb/bytes" }

// --- unsigned integers --------------------------------------------

// Uint64Encoder encodes uint64 as 8-byte big-endian; lex order =
// natural uint64 order.
type Uint64Encoder struct{}

func (Uint64Encoder) AppendEncode(dst []byte, v uint64) ([]byte, error) {
	return binary.BigEndian.AppendUint64(dst, v), nil
}
func (Uint64Encoder) Decode(src []byte) (uint64, error) {
	if len(src) != 8 {
		return 0, errDecode("gmdb/be-uint64", len(src), 8)
	}
	return binary.BigEndian.Uint64(src), nil
}
func (Uint64Encoder) ID() string { return "gmdb/be-uint64" }

// Uint32Encoder encodes uint32 as 4-byte big-endian; lex order =
// natural uint32 order.
type Uint32Encoder struct{}

func (Uint32Encoder) AppendEncode(dst []byte, v uint32) ([]byte, error) {
	return binary.BigEndian.AppendUint32(dst, v), nil
}
func (Uint32Encoder) Decode(src []byte) (uint32, error) {
	if len(src) != 4 {
		return 0, errDecode("gmdb/be-uint32", len(src), 4)
	}
	return binary.BigEndian.Uint32(src), nil
}
func (Uint32Encoder) ID() string { return "gmdb/be-uint32" }

// --- signed integers (sign-bit XOR, lex-preserving) ---------------

// signBit64 / signBit32 are the top-bit masks XOR'd into the big-endian
// encoding of a signed integer so that two's-complement order maps to
// unsigned lex order: INT_MIN encodes to 0x00.., −1 to 0x7f.., 0 to
// 0x80.., INT_MAX to 0xff.. — monotonic in the unsigned big-endian
// comparison the B+tree uses.
const (
	signBit64 uint64 = 1 << 63
	signBit32 uint32 = 1 << 31
)

// Int64Encoder encodes int64 as 8-byte big-endian with the sign bit
// XOR'd (NOT zigzag); lex order = natural int64 order.
type Int64Encoder struct{}

func (Int64Encoder) AppendEncode(dst []byte, v int64) ([]byte, error) {
	return binary.BigEndian.AppendUint64(dst, uint64(v)^signBit64), nil
}
func (Int64Encoder) Decode(src []byte) (int64, error) {
	if len(src) != 8 {
		return 0, errDecode("gmdb/be-int64", len(src), 8)
	}
	return int64(binary.BigEndian.Uint64(src) ^ signBit64), nil
}
func (Int64Encoder) ID() string { return "gmdb/be-int64" }

// Int32Encoder encodes int32 as 4-byte big-endian with the sign bit
// XOR'd; lex order = natural int32 order.
type Int32Encoder struct{}

func (Int32Encoder) AppendEncode(dst []byte, v int32) ([]byte, error) {
	return binary.BigEndian.AppendUint32(dst, uint32(v)^signBit32), nil
}
func (Int32Encoder) Decode(src []byte) (int32, error) {
	if len(src) != 4 {
		return 0, errDecode("gmdb/be-int32", len(src), 4)
	}
	return int32(binary.BigEndian.Uint32(src) ^ signBit32), nil
}
func (Int32Encoder) ID() string { return "gmdb/be-int32" }

// --- time ----------------------------------------------------------

// nanos{Min,Max}{Sec,Rem} bound the times representable as int64
// nanoseconds since the epoch. time.Time.UnixNano() is undefined
// (silently wraps) outside ~[1678, 2262]; a wrapped value would sort in
// the wrong lex position, violating the lex=time invariant — so
// TimeEncoder rejects out-of-range times rather than corrupting
// order. time.Time.Unix() floors toward −∞ and Nanosecond() is always
// in [0, 1e9), so the representable extremes are:
//
//	MaxInt64 = nanosMaxSec·1e9 + nanosMaxRem  (sec 9223372036, nsec 854775807)
//	MinInt64 = nanosMinSec·1e9 + nanosMinRem  (sec −9223372037, nsec 145224192)
//
// A time is in range iff it is ≥ the MinInt64 instant and ≤ the
// MaxInt64 instant; the guard below is symmetric in both bounds (the
// earlier asymmetric form spuriously rejected the ~0.855 s band just
// above MinInt64, whose Unix() second is nanosMinSec, not nanosMinSec+1).
const (
	nanosMaxSec int64 = 9223372036  // floor(math.MaxInt64 / 1e9)
	nanosMaxRem int64 = 854775807   // math.MaxInt64 − nanosMaxSec·1e9
	nanosMinSec int64 = -9223372037 // floor(math.MinInt64 / 1e9), toward −∞
	nanosMinRem int64 = 145224192   // math.MinInt64 − nanosMinSec·1e9
)

// TimeEncoder encodes time.Time as int64 nanoseconds since the Unix
// epoch with the same sign-bit-XOR transform as Int64Encoder; lex
// order = natural time order. Decode returns a UTC time; the monotonic
// clock reading and location are NOT preserved (only the instant).
// AppendEncode rejects times outside the int64-nanoseconds range with
// an error rather than silently wrapping.
type TimeEncoder struct{}

func (TimeEncoder) AppendEncode(dst []byte, v time.Time) ([]byte, error) {
	sec, nsec := v.Unix(), int64(v.Nanosecond())
	if sec < nanosMinSec || (sec == nanosMinSec && nsec < nanosMinRem) ||
		sec > nanosMaxSec || (sec == nanosMaxSec && nsec > nanosMaxRem) {
		return nil, fmt.Errorf("gmdb/be-time-nanos: %s out of int64-nanoseconds range (~year 1678–2262)", v.UTC().Format(time.RFC3339))
	}
	return binary.BigEndian.AppendUint64(dst, uint64(v.UnixNano())^signBit64), nil
}
func (TimeEncoder) Decode(src []byte) (time.Time, error) {
	if len(src) != 8 {
		return time.Time{}, errDecode("gmdb/be-time-nanos", len(src), 8)
	}
	nanos := int64(binary.BigEndian.Uint64(src) ^ signBit64)
	return time.Unix(0, nanos).UTC(), nil
}
func (TimeEncoder) ID() string { return "gmdb/be-time-nanos" }

// --- UUID (raw 16 bytes) ------------------------------------------

// UUIDv4Encoder encodes a 16-byte UUID raw; lex order is the raw byte
// order (random for v4 — no time ordering). The Go type is [16]byte.
type UUIDv4Encoder struct{}

func (UUIDv4Encoder) AppendEncode(dst []byte, v [16]byte) ([]byte, error) {
	return append(dst, v[:]...), nil
}
func (UUIDv4Encoder) Decode(src []byte) ([16]byte, error) {
	var u [16]byte
	if len(src) != 16 {
		return u, errDecode("gmdb/uuid-v4", len(src), 16)
	}
	copy(u[:], src)
	return u, nil
}
func (UUIDv4Encoder) ID() string { return "gmdb/uuid-v4" }

// UUIDv7Encoder encodes a 16-byte UUID raw; identical wire form to
// UUIDv4Encoder but a distinct ID, since a v7 UUID's leading timestamp
// makes its raw lex order equal time order. The Go type is [16]byte.
type UUIDv7Encoder struct{}

func (UUIDv7Encoder) AppendEncode(dst []byte, v [16]byte) ([]byte, error) {
	return append(dst, v[:]...), nil
}
func (UUIDv7Encoder) Decode(src []byte) ([16]byte, error) {
	var u [16]byte
	if len(src) != 16 {
		return u, errDecode("gmdb/uuid-v7", len(src), 16)
	}
	copy(u[:], src)
	return u, nil
}
func (UUIDv7Encoder) ID() string { return "gmdb/uuid-v7" }

// Compile-time assertions that each canonical encoder satisfies
// Encoder[T] for its element type.
var (
	_ Encoder[string]    = StringEncoder{}
	_ Encoder[[]byte]    = BytesEncoder{}
	_ Encoder[uint64]    = Uint64Encoder{}
	_ Encoder[uint32]    = Uint32Encoder{}
	_ Encoder[int64]     = Int64Encoder{}
	_ Encoder[int32]     = Int32Encoder{}
	_ Encoder[time.Time] = TimeEncoder{}
	_ Encoder[[16]byte]  = UUIDv4Encoder{}
	_ Encoder[[16]byte]  = UUIDv7Encoder{}
)
