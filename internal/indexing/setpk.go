package indexing

import (
	"errors"
	"fmt"
)

// Compound-PK codec for SetKeyspace indexes, per
// set-keyspace.md §Indexes on SetKeyspaces.
//
// The "primary key" for a SetKeyspace index entry is the
// `(setKey, setValue)` pair — neither alone identifies the set
// member. The on-disk encoding:
//
//	escapedPK := escape(setKey) || 0x00 0x01 || escape(setValue)
//
// 0x00 0x01 is **lex-distinct** from the NUL-escape column
// terminator 0x00 0x00 and from the escape sequence 0x00 0xFF.
// Inside escape(setKey) and escape(setValue), every literal 0x00
// is already escaped to 0x00 0xFF — so the only raw 0x00 0x01 in
// the compound PK is the separator. (set-keyspace.md §Compound-PK encoding,
// promoted to enforced tests.)
//
// The full SetKeyspace non-unique index key is:
//
//	indexKey := EncodeKey(cols) || escapedPK || 0x00 0x00
//
// For unique indexes the PK is in the value, not the key:
// indexKey := EncodeKey(cols), exactly as for Keyspace
// indexes. The unique value format remains uvarint(len(pk)) ||
// pk_bytes || encoded_covering — where pk_bytes is the FULL
// compound PK above.

// ErrCompoundPKMalformed marks a decode failure (no 0x00 0x01
// separator found, or the surrounding escape decode failed).
// Wrapped in ErrCorrupted at the engine's boundary.
var ErrCompoundPKMalformed = errors.New("SetKeyspace compound PK malformed")

// EncodeSetCompoundPK builds the compound PK bytes for a
// SetKeyspace index entry: escape(setKey) || 0x00 0x01 ||
// escape(setValue). The result contains exactly one literal
// 0x00 0x01 sequence (the separator) and no other 0x00 0x01
// pattern (because EscapeColumn turns every 0x00 in its input
// into 0x00 0xFF).
func EncodeSetCompoundPK(setKey, setValue []byte) []byte {
	escapedKey := EscapeColumn(setKey)
	escapedValue := EscapeColumn(setValue)
	out := make([]byte, 0, len(escapedKey)+2+len(escapedValue))
	out = append(out, escapedKey...)
	out = append(out, 0x00, 0x01)
	out = append(out, escapedValue...)
	return out
}

// DecodeSetCompoundPK reverses EncodeSetCompoundPK.
// Splits on the first literal 0x00 0x01 sequence (the separator
// is unique within the compound per set-keyspace.md §Compound-PK encoding: every 0x00 inside an
// escaped half is followed by 0xFF, never 0x01).
//
// Returns ErrCompoundPKMalformed wrapped in ErrCorrupted at the
// engine's boundary if no 0x00 0x01 separator is found OR if
// either escaped half fails to unescape.
func DecodeSetCompoundPK(encoded []byte) (setKey, setValue []byte, err error) {
	// Scan for the first 0x00 0x01 — set-keyspace.md §Compound-PK encoding ensures this is the
	// separator, since every other 0x00 in the compound is part of
	// an 0x00 0xFF escape pair.
	for i := 0; i < len(encoded)-1; i++ {
		if encoded[i] == 0x00 && encoded[i+1] == 0x01 {
			// Found separator at offset i.
			setKey, err = UnescapeColumn(encoded[:i])
			if err != nil {
				return nil, nil, fmt.Errorf("%w: setKey half: %w", ErrCompoundPKMalformed, err)
			}
			setValue, err = UnescapeColumn(encoded[i+2:])
			if err != nil {
				return nil, nil, fmt.Errorf("%w: setValue half: %w", ErrCompoundPKMalformed, err)
			}
			return setKey, setValue, nil
		}
		// Skip past a 0x00 0xFF escape pair so we don't mis-parse
		// a 0x00 0xFF as separator-ish.
		if encoded[i] == 0x00 && encoded[i+1] == 0xFF {
			i++
		}
	}
	return nil, nil, fmt.Errorf("%w: no 0x00 0x01 separator found in %d-byte compound PK",
		ErrCompoundPKMalformed, len(encoded))
}

// EncodeSetEntryKey assembles the full on-disk index key
// for a SetKeyspace non-unique index. For unique SetKeyspace
// indexes, the key is just EncodeKey(cols) — the compound
// PK goes in the value (uvarint-prefixed pk_bytes).
//
// For non-unique:
//
//	indexKey := EncodeKey(cols) || EncodeSetCompoundPK(setKey, setValue) || 0x00 0x00
//
// The trailing 0x00 0x00 terminates the PK component, matching
// the spec grammar.
func EncodeSetEntryKey(cols [][]byte, setKey, setValue []byte, unique bool) []byte {
	colBytes := EncodeKey(cols)
	if unique {
		return colBytes
	}
	compoundPK := EncodeSetCompoundPK(setKey, setValue)
	out := make([]byte, 0, len(colBytes)+len(compoundPK)+2)
	out = append(out, colBytes...)
	out = append(out, compoundPK...)
	out = append(out, 0x00, 0x00)
	return out
}

// ExtractSetCompoundPK extracts the compound
// PK (escapedPK bytes — still in escaped form, separator literal)
// from a non-unique SetKeyspace index key, given the number of
// columns the index declares.
//
// Walks the encoded key counting REAL 0x00 0x00 column
// terminators (skipping 0x00 0xFF escape pairs and 0x00 0x01
// separators). The Nth terminator marks the start of the
// escapedPK; everything up to (but not including) the (N+1)th
// terminator is the compound PK.
//
// Returns the escapedPK bytes (caller can then call
// DecodeSetCompoundPK to split into setKey/setValue).
// Returns ErrCompoundPKMalformed if the key has fewer than N+1
// real terminators (malformed).
func ExtractSetCompoundPK(indexKey []byte, numColumns int) ([]byte, error) {
	terminators := 0
	pkStart := -1
	for i := 0; i < len(indexKey)-1; i++ {
		if indexKey[i] != 0x00 {
			continue
		}
		next := indexKey[i+1]
		switch next {
		case 0xFF:
			// Escape pair; skip the 0xFF.
			i++
		case 0x01:
			// Separator inside the compound PK; not a terminator.
			i++
		case 0x00:
			// Real column terminator.
			terminators++
			if terminators == numColumns {
				// Next byte after this terminator is the start of
				// escapedPK.
				pkStart = i + 2
			} else if terminators == numColumns+1 {
				// This is the terminator AFTER the escapedPK.
				if pkStart < 0 {
					return nil, fmt.Errorf("%w: extra terminator before PK", ErrCompoundPKMalformed)
				}
				return indexKey[pkStart:i], nil
			}
			i++
		default:
			return nil, fmt.Errorf("%w: 0x00 followed by 0x%02x at offset %d (want 0x00, 0x01, or 0xFF)",
				ErrCompoundPKMalformed, next, i)
		}
	}
	return nil, fmt.Errorf("%w: index key has %d real terminators, want %d+1",
		ErrCompoundPKMalformed, terminators, numColumns)
}
