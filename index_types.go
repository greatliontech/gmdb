package gmdb

import (
	"fmt"

	"github.com/greatliontech/gmdb/internal/indexing"
)

// IndexDecl describes one secondary index on a byte-oriented keyspace.
// Declared at OpenKeyspace / CreateKeyspace; every write transaction
// opening the keyspace must supply a matching set (indexing.md §Index
// Declaration). Fields: Name (schema-hash input; renaming forces
// RebuildIndex), Columns / Covering (positional, lex-safe bytes),
// Unique, Version (extractor-logic drift tag), Extract.
//
// The concrete type lives in internal/indexing beside the entry and
// registry codecs; this alias is the public surface.
type IndexDecl = indexing.Decl

// IndexColumn names a positional column in an index's column tuple.
// The Name is a semantic anchor that contributes to the schema-hash
// fingerprint; column storage itself is purely positional. Renaming
// a column changes the schema hash and forces RebuildIndex. Reusing
// a name for a column whose semantic content has changed is the
// caller's responsibility — bump Version in that case (see
// indexing.md §Covering Indexes for the rename-pair example).
type IndexColumn = indexing.Column

// IndexCoveringColumn names a positional column in an index's covering
// tuple. Same semantics as IndexColumn.Name. Adding / removing /
// reordering covering columns triggers ErrIndexFingerprintMismatch.
type IndexCoveringColumn = indexing.CoveringColumn

// IndexKind discriminates the index's data-structure family
// (indexing.md §Overview). The zero value, IndexKindComposite, is
// the composite-key lex-ordered B+tree — the only kind this engine
// version accepts; OpenKeyspace / CreateKeyspace / RebuildIndex
// reject any other value with ErrIndexKindUnknown, before any
// work. The concrete type lives in internal/indexing.
type IndexKind = indexing.Kind

// IndexKindComposite is the composite-key lex-ordered B+tree kind.
const IndexKindComposite = indexing.KindComposite

// IndexExtractor produces zero or more IndexEntry values for a row.
// Returning a nil slice or a zero-length slice both signal "do not
// index this row" (partial-index semantics) and are equivalent.
// Per indexing.md §Overview.
type IndexExtractor = indexing.Extractor

// IndexEntry is one row's contribution to an index, produced by the
// IndexExtractor. Cols holds the per-IndexColumn lex-safe byte
// encoding; Cover holds the per-IndexCoveringColumn bytes (omit when
// the IndexDecl declares no Covering). Per indexing.md §Overview.
//
// The concrete type lives in internal/indexing beside the entry
// codec; this alias is the public surface.
type IndexEntry = indexing.Entry

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
// per indexing.md §Drift Guard. The hash core (grammar, injectivity
// rationale) lives in internal/indexing (SchemaHash); this adapter
// projects the decl's hashable inputs — Name, column names, covering
// names, Unique. Version is NOT a hash input: it is stored and
// compared independently because it captures extractor-logic drift
// the engine cannot inspect (per the spec).
func schemaHash(decl *IndexDecl) uint64 {
	cols := make([]string, len(decl.Columns))
	for i, c := range decl.Columns {
		cols[i] = c.Name
	}
	cov := make([]string, len(decl.Covering))
	for i, c := range decl.Covering {
		cov[i] = c.Name
	}
	return indexing.SchemaHash(decl.Name, cols, cov, decl.Unique, decl.Kind, nil)
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
		if d.Kind != IndexKindComposite {
			return fmt.Errorf("gmdb: IndexDecl %q has kind %d: %w", d.Name, d.Kind, ErrIndexKindUnknown)
		}
		// Zero-column IndexDecls are unsupported: the non-unique
		// decoder (indexing.ExtractSetCompoundPK +
		// extractPKAndValue) needs at least one column terminator
		// to bound the PK component; a zero-column index would
		// surface indexing.ErrKeyMalformed at decode time. Reject at
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
