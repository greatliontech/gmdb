# Encoder set: inconsistent `BE` prefix, mis-named time encoder, root-namespace bloat

**Lands:** condition — renames / a namespace move are breaking
(exported symbols), so land before the first tagged release while
`development: true`.

## Problem

The built-in `Encoder[T]` implementations (`typed_encoder.go`) are a
sensible, fairly orthogonal canonical set (string, bytes,
{u}int{32,64}, time, two UUID versions) with a clean custom-encoder
story (`FuncEncoder[T]`). Three surface issues:

1. **`BE` prefix is applied inconsistently.** `BEInt32Encoder`,
   `BEInt64Encoder`, `BEUint32Encoder`, `BEUint64Encoder`,
   `BENanosEncoder` carry it; `StringEncoder`, `BytesEncoder`,
   `UUIDv4Encoder`, `UUIDv7Encoder` do not — yet those are equally
   order-sensitive. The prefix communicates nothing a user acts on
   (there is no little-endian variant offered) and just adds noise.

2. **`BENanosEncoder` names the encoding, not the type.** Every sibling
   is named by its Go type (`BEInt64Encoder` ↔ `int64`); the
   `time.Time` encoder breaks the pattern. Should be `TimeEncoder` (or
   `BETimeEncoder` if the prefix is kept).

3. **Root-namespace bloat.** 11 encoder symbols sit in the root
   `gmdb` package. A `gmdb/encoders` sub-package (`encoders.Int64`,
   `encoders.UUIDv7`, `encoders.Func[T]`) would cut root-namespace
   breadth materially and read better at call sites.

## Resolution

Decide the prefix policy (drop `BE` everywhere, or apply it uniformly),
rename the time encoder to type-named, and decide whether the encoder
set moves to a `gmdb/encoders` sub-package. All three are mechanical
renames/moves; do them together since each is breaking.

## Notes

Surfaced during the 2026-05-30 architecture/factoring audit (public API
surface pass). The *set* is fine — this is naming/placement, not
missing or redundant encoders.
