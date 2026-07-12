// Package descriptor holds the fixed 40-byte on-disk codec for
// keyspace descriptors (keyspaces.md §Keyspace Descriptor).
package descriptor

import (
	"encoding/binary"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// le is the on-disk byte order for the keyspace-descriptor row
// format — little-endian, matching every other persisted format.
var le = binary.LittleEndian

// Keyspace is the 40-byte struct stored as the value for a
// keyspace's entry in the keyspace B+tree. Layout per
// keyspaces.md §Keyspace Descriptor:
//
//	+----------+----------+----------+----------------+----------+----------+--------------------+----------+
//	| Root     | Count    | Kind     | FixedValueSize | NextSeq  | RestartGT| IndexRegistryRoot  | Reserved |
//	| uint64   | uint64   | uint8    | uint16         | uint64   | uint16   | uint64             | [3]byte  |
//	+----------+----------+----------+----------------+----------+----------+--------------------+----------+
//	  8 + 8 + 1 + 2 + 8 + 2 + 8 + 3 = 40 bytes
//
// Field order and sizes are part of the on-disk contract — invariant #1
// in keyspaces.md (an extra byte shifts every IndexRegistryRoot into the
// reserved space and silently disconnects every index registry from its
// keyspace). The codec round-trip test
// (TestKeyspaceDescriptorRoundTripAllFields in
// descriptor_test.go) is the load-bearing enforcement.
type Keyspace struct {
	// Root is the page ID of this keyspace's B+tree root. Zero ⇒ empty
	// keyspace.
	Root uint64

	// Count is the number of key-value pairs in the keyspace. For a
	// SetKeyspace, the total pairs across all value sets.
	Count uint64

	// Kind is one of KindKeyspace / KindSetKeyspace /
	// KindIndexInternal. Set at creation, immutable after.
	Kind uint8

	// FixedValueSize is meaningful only when Kind ==
	// KindSetKeyspace: the fixed value size in bytes for set
	// members (0 ⇒ variable). Must be 0 when Kind != SetKeyspace
	// (validated by Validate).
	FixedValueSize uint16

	// NextSeq is the next sequence number for Keyspace.NextSequence().
	// First call returns 1.
	NextSeq uint64

	// RestartGroupTarget is the per-keyspace target leaf restart-group
	// size: 0 ⇒ engine default, 1 ⇒ uncompressed-leaf variant
	// (TypeLeafUncompressed), [2, 255] ⇒ compressed-leaf variant with
	// the value as the group target. Values > 255 are rejected by
	// Validate (the compressed-leaf restart-table
	// Count field is uint8 — 255 is the physical cap per
	// page-formats.md §Compressed Leaf). uint16 on disk reserves bits
	// 8..15 for future use.
	RestartGroupTarget uint16

	// IndexRegistryRoot is the page ID of this keyspace's per-keyspace
	// index registry sub-tree. Zero ⇒ no indexes declared on this
	// keyspace.
	IndexRegistryRoot uint64
}

// Keyspace Kind values. Stored in Keyspace.Kind.
const (
	// KindKeyspace is a key→value keyspace.
	KindKeyspace uint8 = 0
	// KindSetKeyspace is a key→sorted-set keyspace.
	KindSetKeyspace uint8 = 1
	// KindIndexInternal is an engine-internal index keyspace —
	// not directly openable by user code (filtered out of
	// ListKeyspaces; OpenKeyspace returns ErrKeyspaceReserved per
	// keyspaces.md invariant #4).
	KindIndexInternal uint8 = 2
)

// Size is the fixed on-disk byte length of one
// keyspace descriptor (8+8+1+2+8+2+8+3). The
// Keyspace.RestartGroupTarget cap is page.MaxRestartGroupTarget
// (defined alongside Config.RestartGroupTarget); the same uint8 physical
// cap from page-formats.md §Compressed Leaf applies here.
const Size = 40

// Keyspace descriptor field offsets within the 40-byte buffer.
const (
	ksdOffRoot               = 0
	ksdOffCount              = 8
	ksdOffKind               = 16
	ksdOffFixedValueSize     = 17
	ksdOffNextSeq            = 19
	ksdOffRestartGroupTarget = 27
	ksdOffIndexRegistryRoot  = 29
	ksdOffReserved           = 37
	ksdReservedLen           = 3
)

// Decode reads a Keyspace from the first
// Size bytes of buf. Does not validate field-level
// invariants; use Validate to detect malformed
// descriptors (unknown Kind, FixedValueSize set on Kind != 1,
// RestartGroupTarget > 255, non-zero reserved bytes).
func Decode(buf []byte) Keyspace {
	_ = buf[Size-1] // bounds check
	return Keyspace{
		Root:               le.Uint64(buf[ksdOffRoot:]),
		Count:              le.Uint64(buf[ksdOffCount:]),
		Kind:               buf[ksdOffKind],
		FixedValueSize:     le.Uint16(buf[ksdOffFixedValueSize:]),
		NextSeq:            le.Uint64(buf[ksdOffNextSeq:]),
		RestartGroupTarget: le.Uint16(buf[ksdOffRestartGroupTarget:]),
		IndexRegistryRoot:  le.Uint64(buf[ksdOffIndexRegistryRoot:]),
	}
}

// Encode writes d into the first
// Size bytes of buf. The 3 reserved bytes are
// zeroed. The encoded form is the canonical representation — a
// subsequent Decode + Encode round-trip is byte-identical (test
// pinned).
func Encode(buf []byte, d Keyspace) {
	_ = buf[Size-1] // bounds check
	le.PutUint64(buf[ksdOffRoot:], d.Root)
	le.PutUint64(buf[ksdOffCount:], d.Count)
	buf[ksdOffKind] = d.Kind
	le.PutUint16(buf[ksdOffFixedValueSize:], d.FixedValueSize)
	le.PutUint64(buf[ksdOffNextSeq:], d.NextSeq)
	le.PutUint16(buf[ksdOffRestartGroupTarget:], d.RestartGroupTarget)
	le.PutUint64(buf[ksdOffIndexRegistryRoot:], d.IndexRegistryRoot)
	clear(buf[ksdOffReserved : ksdOffReserved+ksdReservedLen])
}

// Validate reports whether d is a well-formed
// descriptor as observed after decoding from disk. Reads
// Kind/FixedValueSize/RestartGroupTarget from d and the reserved
// bytes from buf (decode does not surface reserved into the struct).
//
// Intended call shape:
//
//	d := Decode(buf)
//	if err := Validate(buf, d); err != nil { ... }
//
// Passing a mutated d alongside a stale buf produces well-defined but
// possibly surprising results — Kind/FixedValueSize/RestartGroupTarget
// reflect the new intent; reserved bytes reflect the original buffer.
//
// Enforces:
//   - Kind ∈ {0, 1, 2} (keyspaces.md invariant #2).
//   - FixedValueSize == 0 OR Kind == KindSetKeyspace
//     (invariant #5: FixedValueSize meaningful only for Kind == 1).
//   - RestartGroupTarget ≤ page.MaxRestartGroupTarget (255) — the
//     compressed-leaf restart-table Count is uint8.
//   - Reserved bytes are all zero (keyspaces.md §Keyspace Descriptor:
//     "Open() rejects descriptors with non-zero reserved bytes").
func Validate(buf []byte, d Keyspace) error {
	_ = buf[Size-1]
	switch d.Kind {
	case KindKeyspace, KindSetKeyspace, KindIndexInternal:
	default:
		return fmt.Errorf("descriptor: keyspace descriptor Kind %d not in {0, 1, 2}", d.Kind)
	}
	if d.FixedValueSize != 0 && d.Kind != KindSetKeyspace {
		return fmt.Errorf("descriptor: keyspace descriptor FixedValueSize %d set on Kind %d (only valid for Kind=%d SetKeyspace)",
			d.FixedValueSize, d.Kind, KindSetKeyspace)
	}
	if d.RestartGroupTarget > page.MaxRestartGroupTarget {
		return fmt.Errorf("descriptor: keyspace descriptor RestartGroupTarget %d exceeds max %d",
			d.RestartGroupTarget, page.MaxRestartGroupTarget)
	}
	for i := 0; i < ksdReservedLen; i++ {
		if b := buf[ksdOffReserved+i]; b != 0 {
			return fmt.Errorf("descriptor: keyspace descriptor reserved byte %d is 0x%02x, want 0", i, b)
		}
	}
	return nil
}
