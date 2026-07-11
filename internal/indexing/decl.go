package indexing

// The index declaration family. The engine's public IndexDecl /
// IndexColumn / IndexCoveringColumn / IndexExtractor names are
// aliases of these types; the concrete types live here beside the
// entry codec, key encoder, and registry codec they feed.

// Decl describes one secondary index on a byte-oriented keyspace.
// Supplied to Tx.OpenKeyspace / Tx.CreateKeyspace (and the SetKeyspace
// mirrors) at every open of an indexed keyspace; every transaction that
// opens the same keyspace for write must supply matching Decls.
// Per indexing.md §Index Declaration.
type Decl struct {
	// Name is the index's logical identifier; unique within the
	// keyspace. Contributes to the schema-hash fingerprint, so
	// renaming an index forces a RebuildIndex.
	Name string

	// Columns is the ordered list of indexed columns. Concatenated
	// lex-safely via the NUL-escape encoding (see indexing.md
	// §Column Encoding + page-formats.md §NUL-escape encoding).
	Columns []Column

	// Covering optionally pins columns to be carried in the index
	// entry value. Lookup returns covering bytes directly when the
	// caller's query is satisfied by the covering set.
	Covering []CoveringColumn

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

	// Extract produces zero or more Entry values per row. A
	// nil or zero-length slice signals "do not index this row"
	// (partial-index semantics; the two are equivalent per
	// indexing.md §Partial Indexes).
	Extract Extractor
}

// Column names a positional column in an index's column tuple.
// The Name is a semantic anchor that contributes to the schema-hash
// fingerprint; column storage itself is purely positional. Renaming
// a column changes the schema hash and forces RebuildIndex. Reusing
// a name for a column whose semantic content has changed is the
// caller's responsibility — bump Version in that case (see
// indexing.md §Covering Indexes for the rename-pair example).
type Column struct {
	Name string
}

// CoveringColumn names a positional column in an index's covering
// tuple. Same semantics as Column.Name. Adding / removing /
// reordering covering columns triggers ErrIndexFingerprintMismatch.
type CoveringColumn struct {
	Name string
}

// Extractor produces zero or more Entry values for a row.
// Returning a nil slice or a zero-length slice both signal "do not
// index this row" (partial-index semantics) and are equivalent.
// Per indexing.md §Overview.
type Extractor func(key, value []byte) []Entry
