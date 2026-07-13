package indexing

import (
	"encoding/binary"

	"github.com/zeebo/xxh3"
)

// SchemaHash computes the deterministic schema-hash for an index
// declaration per indexing.md §Drift Guard:
//
//	XXH3-64(
//	  uvarint(len(name)) || name ||
//	  uvarint(len(columns)) || for each col: uvarint(len(col)) || col ||
//	  uvarint(len(covering)) || for each col: uvarint(len(col)) || col ||
//	  uint8(unique) ||
//	  uint8(kind) || uvarint(len(kindParams)) || kindParams
//	)
//
// Inputs are exclusively byte sequences with explicit uvarint
// length prefixes — no gob, no JSON, no struct layout — so the
// hash is deterministic across Go versions, build flags, and host
// architectures (clause-explicit invariant: indexing.md §Drift
// Guard schema-hash determinism).
//
// All string inputs (name, column names, covering names) are
// uvarint-length-prefixed for injectivity. Without a prefix on
// name, two distinct declarations can collide: name="ab" +
// columns=[""] + covering=[""] + unique=true encodes to the same
// 7 bytes (61 62 01 00 01 00 01) as name="ab\x01" + columns=[] +
// covering=[""] + unique=true — the boundary between name and
// uvarint(len(columns)) is undetectable when name's trailing
// bytes mimic a uvarint length. Uniform uvarint-prefixing is
// the minimal injective encoding consistent with the Drift-
// Guard clause-explicit invariant.
//
// The user Version tag is NOT part of the schema-hash inputs: it
// is stored and compared independently because it captures
// extractor-logic drift the engine cannot inspect (per the spec).
// kind and kindParams (empty for the composite kind) are
// fingerprint inputs per indexing.md §Drift Guard: a kind change
// under an unchanged shape must fail the guard, or stored entries
// would be read under the wrong kind's semantics.
func SchemaHash(name string, columns, covering []string, unique bool, kind Kind, kindParams []byte) uint64 {
	h := xxh3.New()
	var buf [binary.MaxVarintLen64]byte
	writeLenPrefixedString(h, buf[:], name)

	n := binary.PutUvarint(buf[:], uint64(len(columns)))
	_, _ = h.Write(buf[:n])
	for _, c := range columns {
		writeLenPrefixedString(h, buf[:], c)
	}

	n = binary.PutUvarint(buf[:], uint64(len(covering)))
	_, _ = h.Write(buf[:n])
	for _, c := range covering {
		writeLenPrefixedString(h, buf[:], c)
	}

	var uniqueByte [1]byte
	if unique {
		uniqueByte[0] = 1
	}
	_, _ = h.Write(uniqueByte[:])

	kindByte := [1]byte{byte(kind)}
	_, _ = h.Write(kindByte[:])
	n = binary.PutUvarint(buf[:], uint64(len(kindParams)))
	_, _ = h.Write(buf[:n])
	_, _ = h.Write(kindParams)

	return h.Sum64()
}

// writeLenPrefixedString writes uvarint(len(s)) || s to h. Reusable
// buf must have capacity binary.MaxVarintLen64.
func writeLenPrefixedString(h *xxh3.Hasher, buf []byte, s string) {
	n := binary.PutUvarint(buf, uint64(len(s)))
	_, _ = h.Write(buf[:n])
	_, _ = h.Write([]byte(s))
}
