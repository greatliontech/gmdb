package gmdb

import (
	"encoding/binary"
	"fmt"

	"github.com/cespare/xxhash/v2"
)

// IndexDecl describes one secondary index on a byte-oriented keyspace.
// Supplied to Tx.OpenKeyspace / Tx.CreateKeyspace (and the SetKeyspace
// mirrors) at every open of an indexed keyspace; every transaction that
// opens the same keyspace for write must supply matching IndexDecls.
// Per indexing.md §Index Declaration.
type IndexDecl struct {
	// Name is the index's logical identifier; unique within the
	// keyspace. Contributes to the schema-hash fingerprint, so
	// renaming an index forces a RebuildIndex.
	Name string

	// Columns is the ordered list of indexed columns. Concatenated
	// lex-safely via the NUL-escape encoding (see indexing.md
	// §Column Encoding + page-formats.md §NUL-escape encoding).
	Columns []IndexColumn

	// Covering optionally pins columns to be carried in the index
	// entry value. Lookup returns covering bytes directly when the
	// caller's query is satisfied by the covering set.
	Covering []IndexCoveringColumn

	// Unique rejects extractor-produced duplicate index keys
	// (ErrIndexUniqueViolation on Put). Detection runs against both
	// the on-disk index and the candidate-set produced by a single
	// extractor invocation. Per indexing.md §Unique Indexes.
	Unique bool

	// Version is a user-supplied tag bumped after extractor-logic
	// changes the engine cannot inspect (e.g. masking a column,
	// changing partial-index predicate, reordering output). Stored
	// alongside the schema-hash; mismatch returns
	// ErrIndexFingerprintMismatch. Per indexing.md §Drift Guard.
	Version string

	// Extract produces zero or more IndexEntry values per row. A
	// nil or zero-length slice signals "do not index this row"
	// (partial-index semantics; the two are equivalent per
	// indexing.md §Partial Indexes).
	Extract IndexExtractor
}

// IndexColumn names a positional column in an index's column tuple.
// The Name is a semantic anchor that contributes to the schema-hash
// fingerprint; column storage itself is purely positional. Renaming
// a column changes the schema hash and forces RebuildIndex. Reusing
// a name for a column whose semantic content has changed is the
// caller's responsibility — bump Version in that case (see
// indexing.md §Covering Indexes for the rename-pair example).
type IndexColumn struct {
	Name string
}

// IndexCoveringColumn names a positional column in an index's covering
// tuple. Same semantics as IndexColumn.Name. Adding / removing /
// reordering covering columns triggers ErrIndexFingerprintMismatch.
type IndexCoveringColumn struct {
	Name string
}

// IndexEntry is one row's contribution to an index, produced by the
// IndexExtractor. Cols holds the per-IndexColumn lex-safe byte
// encoding; Cover holds the per-IndexCoveringColumn bytes (omit when
// the IndexDecl declares no Covering). Per indexing.md §Overview.
type IndexEntry struct {
	Cols  [][]byte
	Cover [][]byte
}

// IndexExtractor produces zero or more IndexEntry values for a row.
// Returning a nil slice or a zero-length slice both signal "do not
// index this row" (partial-index semantics) and are equivalent.
// Per indexing.md §Overview.
type IndexExtractor func(key, value []byte) []IndexEntry

// IndexFingerprintError wraps ErrIndexFingerprintMismatch with the
// drifted index's identity and the specific field that differs.
//
// Field is the discriminant; callers MUST inspect Field before
// reading the corresponding pair:
//   - Field == "schema-hash" → StoredHash and SuppliedHash are
//     valid; StoredVersion and SuppliedVersion are empty strings
//     (sentinel placeholders, NOT meaningful values).
//   - Field == "version" → StoredVersion and SuppliedVersion are
//     valid; StoredHash and SuppliedHash are zero (sentinel
//     placeholders, NOT a real hash collision).
//
// Per api-surface.md §IndexFingerprintError.
type IndexFingerprintError struct {
	Keyspace        string
	IndexName       string
	Field           string // "schema-hash" or "version"
	StoredHash      uint64 // valid when Field == "schema-hash"
	SuppliedHash    uint64 // valid when Field == "schema-hash"
	StoredVersion   string // valid when Field == "version"
	SuppliedVersion string // valid when Field == "version"
}

func (e *IndexFingerprintError) Error() string {
	switch e.Field {
	case "schema-hash":
		return fmt.Sprintf("gmdb: index %q on keyspace %q fingerprint mismatch (schema-hash): stored=0x%016x supplied=0x%016x — caller must RebuildIndex",
			e.IndexName, e.Keyspace, e.StoredHash, e.SuppliedHash)
	case "version":
		return fmt.Sprintf("gmdb: index %q on keyspace %q fingerprint mismatch (version): stored=%q supplied=%q — caller must RebuildIndex",
			e.IndexName, e.Keyspace, e.StoredVersion, e.SuppliedVersion)
	default:
		return fmt.Sprintf("gmdb: index %q on keyspace %q fingerprint mismatch (%s) — caller must RebuildIndex",
			e.IndexName, e.Keyspace, e.Field)
	}
}

func (e *IndexFingerprintError) Unwrap() error { return ErrIndexFingerprintMismatch }

// schemaHash computes the deterministic schema-hash for an IndexDecl
// per indexing.md §Drift Guard:
//
//	xxhash64(
//	  Name ||
//	  uvarint(len(Columns)) || for each col: uvarint(len(Name)) || Name ||
//	  uvarint(len(Covering)) || for each col: uvarint(len(Name)) || Name ||
//	  uint8(Unique)
//	)
//
// Inputs are exclusively byte sequences with explicit uvarint
// length prefixes — no gob, no JSON, no struct layout — so the
// hash is deterministic across Go versions, build flags, and host
// architectures (clause-explicit invariant: indexing.md §Drift
// Guard schema-hash determinism).
//
// Version is NOT part of the schema-hash inputs: it is stored and
// compared independently because it captures extractor-logic drift
// the engine cannot inspect (per the spec).
func schemaHash(decl *IndexDecl) uint64 {
	h := xxhash.New()
	// All string inputs (Name, column names, covering names) are
	// uvarint-length-prefixed for injectivity. Without a prefix on
	// Name, two distinct decls can collide: Name="ab" +
	// Columns=[{Name:""}] + Covering=[{Name:""}] + Unique=true
	// encodes to the same 7 bytes (61 62 01 00 01 00 01) as
	// Name="ab\x01" + Columns=[] + Covering=[{Name:""}] +
	// Unique=true — the boundary between Name and
	// uvarint(len(Columns)) is undetectable when Name's trailing
	// bytes mimic a uvarint length. Uniform uvarint-prefixing is
	// the minimal injective encoding consistent with the Drift-
	// Guard clause-explicit invariant.
	var buf [binary.MaxVarintLen64]byte
	writeLenPrefixedString(h, buf[:], decl.Name)

	n := binary.PutUvarint(buf[:], uint64(len(decl.Columns)))
	_, _ = h.Write(buf[:n])
	for _, c := range decl.Columns {
		writeLenPrefixedString(h, buf[:], c.Name)
	}

	n = binary.PutUvarint(buf[:], uint64(len(decl.Covering)))
	_, _ = h.Write(buf[:n])
	for _, c := range decl.Covering {
		writeLenPrefixedString(h, buf[:], c.Name)
	}

	var uniqueByte [1]byte
	if decl.Unique {
		uniqueByte[0] = 1
	}
	_, _ = h.Write(uniqueByte[:])

	return h.Sum64()
}

// writeLenPrefixedString writes uvarint(len(s)) || s to h. Reusable
// buf must have capacity binary.MaxVarintLen64.
func writeLenPrefixedString(h *xxhash.Digest, buf []byte, s string) {
	n := binary.PutUvarint(buf, uint64(len(s)))
	_, _ = h.Write(buf[:n])
	_, _ = h.Write([]byte(s))
}

// validateIndexDecls rejects a variadic IndexDecl slice that contains
// two entries with the same Name. Used by Tx.OpenKeyspace /
// Tx.CreateKeyspace / Tx.OpenSetKeyspace / Tx.CreateSetKeyspace
// to satisfy indexing.md §Index Declaration:
//
//	Duplicate IndexDecl.Name values in one OpenKeyspace call's
//	variadic slice are rejected with ErrIndexExists naming the
//	offending duplicate.
//
// Rejections:
//   - nil decl in the slice → ErrInvalidOptions (caller bug; the
//     slice itself is malformed).
//   - empty decl.Name → ErrKeyEmpty (consistent with
//     api-surface.md §Keyspace API TxIndexes.Rebuild
//     godoc: "ErrKeyEmpty if … decl.Name is empty"; both call sites
//     surface the same sentinel for the same fault).
//   - duplicate Name → ErrIndexExists naming the offending name.
func validateIndexDecls(decls []*IndexDecl) error {
	if len(decls) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(decls))
	for i, d := range decls {
		if d == nil {
			return fmt.Errorf("gmdb: IndexDecl at position %d is nil: %w", i, ErrInvalidOptions)
		}
		if d.Name == "" {
			return fmt.Errorf("gmdb: IndexDecl at position %d has empty Name: %w", i, ErrKeyEmpty)
		}
		// Zero-column IndexDecls are unsupported: the non-unique
		// decoder (extractSetKeyspaceCompoundPKFromIndexKey +
		// extractPKAndValue) needs at least one column terminator
		// to bound the PK component; a zero-column index would
		// surface errIndexKeyMalformed at decode time. Reject at
		// construction with a clear sentinel.
		if len(d.Columns) == 0 {
			return fmt.Errorf("gmdb: IndexDecl %q has zero columns (index must declare at least one column): %w",
				d.Name, ErrInvalidOptions)
		}
		if _, dup := seen[d.Name]; dup {
			return fmt.Errorf("gmdb: duplicate IndexDecl name %q: %w", d.Name, ErrIndexExists)
		}
		seen[d.Name] = struct{}{}
	}
	return nil
}
