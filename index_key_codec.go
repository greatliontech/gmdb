package gmdb

import (
	"errors"
	"fmt"

	"github.com/greatliontech/gmdb/internal/indexing"
)

// ErrCoveringTupleMalformed marks a decode failure in
// DecodeCoveringTuple — the bytes did not parse as a NUL-escape
// column tuple (truncated terminator, unescaped 0x00 byte, or
// trailing bytes after the last terminator).
//
// Deliberately NOT wrapped in ErrCorrupted: the caller decides
// whether the bytes came from a covering-Lookup return (engine
// corruption if malformed — Check() is the authoritative
// diagnostic) or from caller misuse (e.g. applying the decoder to
// non-covering Lookup bytes). At the byte-stream level these are
// indistinguishable, so the public surface stays neutral.
var ErrCoveringTupleMalformed = errors.New("gmdb: covering tuple malformed")

// DecodeCoveringTuple decodes the byte slice returned by `Lookup` /
// `Get` / `Range` / `Prefix` on an index whose `IndexDecl.Covering`
// is non-empty (the byte-API covering return contract — see
// indexing.md §Covering Indexes and api-surface.md §Index Lookup
// API). The returned `[][]byte` has one entry per declared
// `IndexCoveringColumn` in declaration order, each carrying the
// extractor's `IndexEntry.Cover[i]` bytes verbatim.
//
// Returns an error wrapping `ErrCoveringTupleMalformed` if the
// input does not parse as a NUL-escape column tuple. The wrap is
// neutral by design — see ErrCoveringTupleMalformed's godoc: an
// on-disk-corruption diagnosis goes through Check(), not this
// decoder's error class.
//
// The bytes returned by `Lookup` for a non-covering index are the
// row's stored value (back-lookup) — `DecodeCoveringTuple` is not
// the right decoder there; the caller knows whether their index
// declared `Covering`.
func DecodeCoveringTuple(value []byte) ([][]byte, error) {
	cols, err := indexing.DecodeKey(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCoveringTupleMalformed, err)
	}
	return cols, nil
}
