// Package indexing holds the pure byte-level codecs of the index
// machinery: the NUL-escape composite-key codec, the registry-entry
// row codec, and the declaration schema hash. It operates strictly on
// primitives and []byte — no engine types, no I/O — so the root
// package's index surface (declarations, handles, Tx glue) composes
// on top without a dependency cycle. Per the package-composition
// audit, this is the largest clean extraction the index machinery's
// coupling permits: the surrounding state (pinned indexes, handles)
// lives on the root's keyspace layer by design.
package indexing

import (
	"errors"
	"fmt"
	"slices"
)

// NUL-escape codec for composite index keys, per
// page-formats.md §NUL-escape encoding + indexing.md §Column
// Encoding.
//
// Grammar — the shape is statically determined by `IndexDecl.Unique`,
// NOT by runtime data (so "optional" below means "present iff the
// index is non-unique"):
//
//	uniqueIndexKey    := (escapedCol 0x00 0x00)+
//	nonUniqueIndexKey := (escapedCol 0x00 0x00)+ escapedPK 0x00 0x00
//	escapedCol        := bytes-of-col with every 0x00 → 0x00 0xFF
//
// The encoder (EncodeKey) produces the column-tuple portion
// (escaped columns + 0x00 0x00 terminators). For non-unique
// indexes the caller appends the escaped primary key + a final
// 0x00 0x00 terminator itself — for plain Keyspace indexes that's
// `EscapeColumn(pk)` plus 0x00 0x00; for SetKeyspace indexes
// that's the compound `escape(setKey) || 0x00 0x01 ||
// escape(setValue)` + final 0x00 0x00 per set-keyspace.md
// §Indexes on SetKeyspaces.
//
// The encoding is **prefix-free** (page-formats.md §Invariants
// clause-explicit invariant): after escaping, no encoded column
// is a prefix of another's encoded form, and the column
// terminator 0x00 0x00 never appears inside an escaped column.

// ErrKeyMalformed marks a decode failure: a 0x00 byte
// followed by a byte other than 0x00 (terminator) or 0xFF (escape).
// Wrapped in ErrCorrupted at the caller's boundary (index-tree
// data is engine-internal; a malformed index key signals on-disk
// corruption).
var ErrKeyMalformed = errors.New("index key malformed")

// EscapeColumn returns col's NUL-escaped form: every 0x00 byte
// becomes 0x00 0xFF; other bytes pass through. The result contains
// no raw 0x00-then-anything-but-0xFF sequence, which is what makes
// the column terminator 0x00 0x00 prefix-free w.r.t. column
// content.
//
// Allocates a fresh slice; the input is not modified.
func EscapeColumn(col []byte) []byte {
	// Count 0x00 bytes to size the output buffer once.
	zeros := 0
	for _, b := range col {
		if b == 0x00 {
			zeros++
		}
	}
	if zeros == 0 {
		// Fast path: no escape needed. Still allocate fresh per
		// the "Allocates a fresh slice" function contract.
		out := make([]byte, len(col))
		copy(out, col)
		return out
	}
	out := make([]byte, 0, len(col)+zeros)
	for _, b := range col {
		if b == 0x00 {
			out = append(out, 0x00, 0xFF)
		} else {
			out = append(out, b)
		}
	}
	return out
}

// UnescapeColumn reverses EscapeColumn over an already-extracted
// escaped column (without its 0x00 0x00 terminator). Returns
// ErrKeyMalformed if a 0x00 byte appears not followed by
// 0xFF — the only legal 0x00-prefix in column content is the
// escape sequence (terminators are stripped before unescaping).
//
// Allocates a fresh slice.
func UnescapeColumn(escaped []byte) ([]byte, error) {
	// Fast path: no 0x00 bytes → input is already raw.
	if !slices.Contains(escaped, byte(0x00)) {
		out := make([]byte, len(escaped))
		copy(out, escaped)
		return out, nil
	}
	out := make([]byte, 0, len(escaped))
	for i := 0; i < len(escaped); i++ {
		b := escaped[i]
		if b != 0x00 {
			out = append(out, b)
			continue
		}
		if i+1 >= len(escaped) {
			return nil, fmt.Errorf("%w: lone 0x00 at end of escaped column", ErrKeyMalformed)
		}
		next := escaped[i+1]
		if next != 0xFF {
			return nil, fmt.Errorf("%w: 0x00 followed by 0x%02x (want 0xFF) at offset %d",
				ErrKeyMalformed, next, i)
		}
		out = append(out, 0x00)
		i++ // consume the 0xFF
	}
	return out, nil
}

// EncodeKey encodes a column tuple into the NUL-escape format:
// each column is escaped + followed by a 0x00 0x00 terminator. The
// result is suitable as a unique-index key directly; for non-unique
// indexes the caller appends the (escaped) primary key + a final
// 0x00 0x00 terminator.
//
// An empty cols slice produces an empty result (length 0). Empty
// columns (zero-length byte slices) are valid; they encode to a
// bare 0x00 0x00 terminator (no escaped bytes precede the
// terminator), which is the prefix-free encoding of the empty
// column.
//
// Allocates a fresh slice; input slices are not modified.
func EncodeKey(cols [][]byte) []byte {
	// Size pass.
	size := 0
	for _, c := range cols {
		size += len(c) + 2 // worst case under-counts by zeros count
		for _, b := range c {
			if b == 0x00 {
				size++ // +1 per escape (0x00 → 0x00 0xFF)
			}
		}
	}
	out := make([]byte, 0, size)
	for _, c := range cols {
		for _, b := range c {
			if b == 0x00 {
				out = append(out, 0x00, 0xFF)
			} else {
				out = append(out, b)
			}
		}
		out = append(out, 0x00, 0x00)
	}
	return out
}

// DecodeKey decodes an encoded column tuple into the per-column
// byte slices. The decoder is **strict**: every 0x00 inside an
// encoded column must be followed by 0xFF (escape) or 0x00
// (terminator); any other 0x00-prefix returns ErrKeyMalformed
// wrapped in ErrCorrupted.
//
// A SetKeyspace compound-PK component (escape(setKey) || 0x00 0x01
// || escape(setValue)) contains a literal 0x00 0x01 that this
// strict decoder rejects — SetKeyspace lookup uses an
// ad-hoc PK-component decoder that allows the 0x00 0x01 separator,
// not this strict function.
//
// Returns the decoded columns in order. Each returned slice is a
// fresh allocation; the input is not aliased.
func DecodeKey(key []byte) ([][]byte, error) {
	if len(key) == 0 {
		return nil, nil
	}
	var cols [][]byte
	colStart := 0
	i := 0
	for i < len(key) {
		b := key[i]
		if b != 0x00 {
			i++
			continue
		}
		// 0x00 prefix: must be followed by 0xFF (escape) or 0x00 (terminator).
		if i+1 >= len(key) {
			return nil, fmt.Errorf("%w: lone 0x00 at end of index key", ErrKeyMalformed)
		}
		next := key[i+1]
		switch next {
		case 0xFF:
			i += 2 // skip the escape pair; column continues
		case 0x00:
			// Column terminator — extract + unescape.
			escaped := key[colStart:i]
			col, err := UnescapeColumn(escaped)
			if err != nil {
				return nil, err
			}
			cols = append(cols, col)
			i += 2
			colStart = i
		default:
			return nil, fmt.Errorf("%w: 0x00 followed by 0x%02x (want 0x00 or 0xFF) at offset %d",
				ErrKeyMalformed, next, i)
		}
	}
	if colStart != len(key) {
		// Trailing bytes past the last terminator — the key was
		// not terminated.
		return nil, fmt.Errorf("%w: %d trailing bytes after last terminator",
			ErrKeyMalformed, len(key)-colStart)
	}
	return cols, nil
}
