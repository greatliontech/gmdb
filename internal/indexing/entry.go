package indexing

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// On-disk index-entry shape per indexing.md §Storage Layout:
//
//	Unique index:     key = EncodeKey(cols)
//	                  value = uvarint(len(pk)) || pk_bytes || encoded_covering
//	Non-unique index: key = EncodeKey(cols || pk)
//	                  value = encoded_covering (empty if no Covering)
//
// encoded_covering = EncodeKey(coverColumns) when the declaration
// declares covering columns; otherwise empty bytes.
// The uvarint(len(pk)) length prefix on the unique value
// delimits the PK from the optional covering blob — without it,
// the decoder cannot distinguish where pk_bytes ends and
// encoded_covering begins. Non-unique indexes carry the PK in the
// key, so no length prefix is needed in the value.

// Entry is one row's contribution to an index, produced by the
// extractor. Cols holds the per-column lex-safe byte encoding;
// Cover holds the per-covering-column bytes (omit when the
// declaration has no covering columns). Per indexing.md §Overview.
// The engine's public IndexEntry is an alias of this type.
type Entry struct {
	Cols  [][]byte
	Cover [][]byte
}

// EntryKey returns the on-disk index-tree key for a single
// extractor-produced Entry on a row whose primary key is pk. For a
// SetKeyspace the pk argument is the compound
// `escape(setKey) || 0x00 0x01 || escape(setValue)`.
func EntryKey(entry Entry, pk []byte, unique bool) []byte {
	if unique {
		return EncodeKey(entry.Cols)
	}
	// Append the PK as an extra "column" so it gets escaped +
	// terminated by the key encoder, matching the spec grammar.
	withPK := make([][]byte, 0, len(entry.Cols)+1)
	withPK = append(withPK, entry.Cols...)
	withPK = append(withPK, pk)
	return EncodeKey(withPK)
}

// EntryValue returns the on-disk index-tree value for entry on a
// row whose PK is pk. Per the value-format godoc above:
//
//	Unique:     uvarint(len(pk)) || pk_bytes || encoded_covering
//	Non-unique: encoded_covering
//
// encoded_covering = EncodeKey(entry.Cover) when the declaration
// has covering columns and the extractor produced Cover bytes;
// otherwise empty.
func EntryValue(entry Entry, pk []byte, unique bool, hasCovering bool) []byte {
	var covering []byte
	if hasCovering && len(entry.Cover) > 0 {
		covering = EncodeKey(entry.Cover)
	}
	if unique {
		// uvarint(len(pk)) + pk + covering
		var lenBuf [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(lenBuf[:], uint64(len(pk)))
		out := make([]byte, 0, n+len(pk)+len(covering))
		out = append(out, lenBuf[:n]...)
		out = append(out, pk...)
		out = append(out, covering...)
		return out
	}
	// Non-unique: just the covering (empty if none).
	if covering == nil {
		return []byte{}
	}
	return covering
}

// ErrValueMalformed marks a malformed index entry value (truncated
// uvarint, pk-length past end). Wrapped in ErrCorrupted at the
// engine's boundary; index entries are engine-internal so a
// malformed value signals on-disk corruption.
var ErrValueMalformed = errors.New("index entry value malformed")

// DecodeUniqueValue unpacks the unique-index entry value produced
// by EntryValue (unique=true) into the row PK and the encoded
// covering bytes (which may be empty).
//
// Returns ErrValueMalformed wrapped in ErrCorrupted at the
// engine's boundary on malformed input.
func DecodeUniqueValue(value []byte) (pk, encodedCovering []byte, err error) {
	pkLen, n := binary.Uvarint(value)
	if n <= 0 {
		return nil, nil, fmt.Errorf("%w: bad uvarint pk-length prefix", ErrValueMalformed)
	}
	if uint64(len(value)-n) < pkLen {
		return nil, nil, fmt.Errorf("%w: pk length %d exceeds remaining %d bytes",
			ErrValueMalformed, pkLen, len(value)-n)
	}
	pk = value[n : n+int(pkLen)]
	encodedCovering = value[n+int(pkLen):]
	return pk, encodedCovering, nil
}

// DiffEntrySets diffs one index's old and new entry sets into
// sorted delete / insert / update key lists: dels = keys in olds
// absent from news; ins = keys in news absent from olds; upds =
// keys present in both whose stored VALUE differs — computed only
// when coverRewrite is set. The covering payload is extracted from
// the ROW VALUE, which a replace operation changes even when every
// index key stays the same (indexing.md §Covering Indexes);
// without the rewrite, lookups serve the stale covering forever
// while the checker reports FingerprintDrift. pkFn is called
// lazily, at most once, only when a value comparison is needed —
// the pure-insert path never pays for it.
func DiffEntrySets(olds, news map[string]Entry, unique, coverRewrite bool, pkFn func() []byte) (dels, ins, upds []string) {
	var pk []byte
	pkLoaded := false
	for k := range olds {
		if _, ok := news[k]; !ok {
			dels = append(dels, k)
		}
	}
	for k, ne := range news {
		oe, ok := olds[k]
		if !ok {
			ins = append(ins, k)
			continue
		}
		if coverRewrite {
			if !pkLoaded {
				pk = pkFn()
				pkLoaded = true
			}
			if !bytes.Equal(EntryValue(oe, pk, unique, true),
				EntryValue(ne, pk, unique, true)) {
				upds = append(upds, k)
			}
		}
	}
	sort.Strings(dels)
	sort.Strings(ins)
	sort.Strings(upds)
	return dels, ins, upds
}
