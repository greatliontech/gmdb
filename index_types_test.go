package gmdb

import (
	"errors"
	"strings"
	"testing"
)

// TestSchemaHashDeterministic verifies that two structurally identical
// IndexDecls produce the same schema-hash. Per indexing.md §Drift
// Guard schema-hash determinism (clause-explicit invariant).
func TestSchemaHashDeterministic(t *testing.T) {
	a := &IndexDecl{
		Name:    "by_owner",
		Columns: []IndexColumn{{Name: "owner"}, {Name: "repo"}},
		Unique:  false,
		Version: "v1",
	}
	b := &IndexDecl{
		Name:    "by_owner",
		Columns: []IndexColumn{{Name: "owner"}, {Name: "repo"}},
		Unique:  false,
		Version: "v1-OTHER", // Version is NOT a schema-hash input.
	}
	if schemaHash(a) != schemaHash(b) {
		t.Fatalf("schemaHash differs for structurally identical decls (Version is not a hash input): a=%016x b=%016x",
			schemaHash(a), schemaHash(b))
	}
}

// TestSchemaHashSensitiveToColumnAdd verifies that adding a column
// changes the schema-hash. Per indexing.md §Drift Guard "structural
// drift" (column add/remove/reorder).
func TestSchemaHashSensitiveToColumnAdd(t *testing.T) {
	a := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "owner"}}}
	b := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "owner"}, {Name: "repo"}}}
	if schemaHash(a) == schemaHash(b) {
		t.Fatalf("schemaHash collides after column add: %016x", schemaHash(a))
	}
}

// TestSchemaHashSensitiveToColumnRemove verifies that removing a
// column changes the schema-hash.
func TestSchemaHashSensitiveToColumnRemove(t *testing.T) {
	a := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "owner"}, {Name: "repo"}}}
	b := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "owner"}}}
	if schemaHash(a) == schemaHash(b) {
		t.Fatalf("schemaHash collides after column remove: %016x", schemaHash(a))
	}
}

// TestSchemaHashSensitiveToColumnReorder verifies that reordering
// columns changes the schema-hash — column storage is positional, so
// reorder MUST change the hash. Per indexing.md §Drift Guard.
func TestSchemaHashSensitiveToColumnReorder(t *testing.T) {
	a := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "owner"}, {Name: "repo"}}}
	b := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "repo"}, {Name: "owner"}}}
	if schemaHash(a) == schemaHash(b) {
		t.Fatalf("schemaHash collides after column reorder: %016x", schemaHash(a))
	}
}

// TestSchemaHashSensitiveToColumnRename verifies that renaming a
// column changes the schema-hash. Per indexing.md §Drift Guard +
// §Column Encoding "Names are semantic anchors."
func TestSchemaHashSensitiveToColumnRename(t *testing.T) {
	a := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "owner"}}}
	b := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "owner2"}}}
	if schemaHash(a) == schemaHash(b) {
		t.Fatalf("schemaHash collides after column rename: %016x", schemaHash(a))
	}
}

// TestSchemaHashSensitiveToUniqueFlip verifies that flipping the
// Unique flag changes the schema-hash. Per indexing.md §Drift Guard.
func TestSchemaHashSensitiveToUniqueFlip(t *testing.T) {
	a := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "owner"}}, Unique: false}
	b := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "owner"}}, Unique: true}
	if schemaHash(a) == schemaHash(b) {
		t.Fatalf("schemaHash collides after Unique flip: %016x", schemaHash(a))
	}
}

// TestSchemaHashSensitiveToCoveringAdd verifies that adding a
// covering column changes the schema-hash. Per indexing.md
// §Covering Indexes.
func TestSchemaHashSensitiveToCoveringAdd(t *testing.T) {
	a := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "owner"}}}
	b := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "owner"}}, Covering: []IndexCoveringColumn{{Name: "size"}}}
	if schemaHash(a) == schemaHash(b) {
		t.Fatalf("schemaHash collides after Covering add: %016x", schemaHash(a))
	}
}

// TestSchemaHashSensitiveToIndexNameChange verifies that the index
// Name participates in the schema-hash. Per indexing.md §Drift
// Guard (the Name is the leading hash input).
func TestSchemaHashSensitiveToIndexNameChange(t *testing.T) {
	a := &IndexDecl{Name: "by_owner", Columns: []IndexColumn{{Name: "owner"}}}
	b := &IndexDecl{Name: "by_repo", Columns: []IndexColumn{{Name: "owner"}}}
	if schemaHash(a) == schemaHash(b) {
		t.Fatalf("schemaHash collides after index Name change: %016x", schemaHash(a))
	}
}

// TestSchemaHashLengthPrefixingDisambiguates verifies that
// uvarint-prefixed encoding prevents the classic ambiguous
// concatenation: ({"a", "bc"}) MUST NOT hash-collide with
// ({"ab", "c"}). Without the length prefix the byte-stream
// `a` || `bc` is indistinguishable from `ab` || `c`. Per
// indexing.md §Drift Guard "uvarint length prefixes."
func TestSchemaHashLengthPrefixingDisambiguates(t *testing.T) {
	a := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "a"}, {Name: "bc"}}}
	b := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "ab"}, {Name: "c"}}}
	if schemaHash(a) == schemaHash(b) {
		t.Fatalf("schemaHash collides on length-disambiguation case — uvarint prefixing failed: %016x", schemaHash(a))
	}
}

// TestSchemaHashColumnVsCoveringDisambiguated verifies that putting
// the same name in Columns vs Covering produces different hashes.
// Per indexing.md §Drift Guard (Columns and Covering are independent
// uvarint-counted sections).
func TestSchemaHashColumnVsCoveringDisambiguated(t *testing.T) {
	a := &IndexDecl{Name: "x", Columns: []IndexColumn{{Name: "owner"}}}
	b := &IndexDecl{Name: "x", Covering: []IndexCoveringColumn{{Name: "owner"}}}
	if schemaHash(a) == schemaHash(b) {
		t.Fatalf("schemaHash collides between Columns and Covering placement: %016x", schemaHash(a))
	}
}

// TestSchemaHashNamePrefixPreventsCollision verifies the specific
// collision case that motivates uvarint-prefixing index.Name in the
// chunk-7.2 spec amendment. Without the Name prefix:
//
//	A: Name="ab",     Columns=[{Name:""}], Covering=[{Name:""}], Unique=true
//	B: Name="ab\x01", Columns=[],          Covering=[{Name:""}], Unique=true
//
// both encode to `61 62 01 00 01 00 01`. With the prefix, A leads
// with `uvarint(2) || "ab"` = `02 61 62`, while B leads with
// `uvarint(3) || "ab\x01"` = `03 61 62 01` — the streams diverge at
// byte 0, so the hashes differ. Per indexing.md §Drift Guard
// (chunk-7.2 amendment).
func TestSchemaHashNamePrefixPreventsCollision(t *testing.T) {
	a := &IndexDecl{
		Name:     "ab",
		Columns:  []IndexColumn{{Name: ""}},
		Covering: []IndexCoveringColumn{{Name: ""}},
		Unique:   true,
	}
	b := &IndexDecl{
		Name:     "ab\x01",
		Columns:  []IndexColumn{},
		Covering: []IndexCoveringColumn{{Name: ""}},
		Unique:   true,
	}
	if schemaHash(a) == schemaHash(b) {
		t.Fatalf("schemaHash collides on Name-vs-Columns-boundary case — Name prefix missing: %016x", schemaHash(a))
	}
}

// TestValidateIndexDeclsEmpty verifies that an empty IndexDecl slice
// is accepted (a keyspace with no indexes is the default — every
// existing chunk-5/6 test passes this).
func TestValidateIndexDeclsEmpty(t *testing.T) {
	if err := validateIndexDecls(nil); err != nil {
		t.Fatalf("nil slice rejected: %v", err)
	}
	if err := validateIndexDecls([]*IndexDecl{}); err != nil {
		t.Fatalf("empty slice rejected: %v", err)
	}
}

// TestValidateIndexDeclsAcceptsDistinctNames verifies the happy path.
func TestValidateIndexDeclsAcceptsDistinctNames(t *testing.T) {
	decls := []*IndexDecl{
		{Name: "by_owner", Columns: []IndexColumn{{Name: "owner"}}},
		{Name: "by_repo", Columns: []IndexColumn{{Name: "repo"}}},
	}
	if err := validateIndexDecls(decls); err != nil {
		t.Fatalf("distinct names rejected: %v", err)
	}
}

// TestValidateIndexDeclsRejectsDuplicateName verifies that two decls
// with the same Name return ErrIndexExists. Per indexing.md §Index
// Declaration "Duplicate IndexDecl.Name values ... rejected with
// ErrIndexExists naming the offending duplicate."
func TestValidateIndexDeclsRejectsDuplicateName(t *testing.T) {
	decls := []*IndexDecl{
		{Name: "by_owner", Columns: []IndexColumn{{Name: "owner"}}},
		{Name: "by_owner", Columns: []IndexColumn{{Name: "owner"}, {Name: "repo"}}},
	}
	err := validateIndexDecls(decls)
	if !errors.Is(err, ErrIndexExists) {
		t.Fatalf("expected ErrIndexExists, got %v", err)
	}
	if !strings.Contains(err.Error(), "by_owner") {
		t.Fatalf("error message does not name the duplicate index: %v", err)
	}
}

// TestValidateIndexDeclsRejectsNilEntry verifies that a nil decl
// inside the variadic slice is rejected with a wrapped
// ErrInvalidOptions. A nil decl is a caller bug, distinct from
// missing/extra-decl errors (which apply to non-nil decls whose
// Name does not match the registry).
func TestValidateIndexDeclsRejectsNilEntry(t *testing.T) {
	decls := []*IndexDecl{
		{Name: "by_owner", Columns: []IndexColumn{{Name: "owner"}}},
		nil,
	}
	err := validateIndexDecls(decls)
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("expected ErrInvalidOptions for nil decl, got %v", err)
	}
	if !strings.Contains(err.Error(), "position 1") {
		t.Fatalf("error does not name the nil position: %v", err)
	}
}

// TestValidateIndexDeclsRejectsEmptyName verifies that an empty
// IndexDecl.Name is rejected with ErrKeyEmpty. The Name is keyed
// in the registry and in the schema-hash inputs — empty is
// unrepresentable. The sentinel matches the chunk-7.1
// api-surface.md Tx.RebuildIndex godoc ("ErrKeyEmpty if … decl.Name
// is empty") so callers see the same error at every variadic-
// IndexDecl call site.
func TestValidateIndexDeclsRejectsEmptyName(t *testing.T) {
	decls := []*IndexDecl{
		{Name: "", Columns: []IndexColumn{{Name: "owner"}}},
	}
	err := validateIndexDecls(decls)
	if !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("expected ErrKeyEmpty for empty Name, got %v", err)
	}
}

// TestValidateIndexDeclsRejectsZeroColumns verifies the chunk-7.10
// rejection rule: a zero-column IndexDecl is rejected with
// ErrInvalidOptions at the variadic-IndexDecl entry points. The
// non-unique decoder (extractSetKeyspaceCompoundPKFromIndexKey +
// extractPKAndValue) needs at least one column terminator to bound
// the PK component; a zero-column index would surface
// errIndexKeyMalformed at decode time. Rejecting at construction
// gives a clear sentinel.
func TestValidateIndexDeclsRejectsZeroColumns(t *testing.T) {
	decls := []*IndexDecl{
		{Name: "by_nothing", Columns: nil},
	}
	err := validateIndexDecls(decls)
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("expected ErrInvalidOptions for zero-column IndexDecl, got %v", err)
	}
	// Empty-slice variant — same shape.
	decls = []*IndexDecl{
		{Name: "by_nothing", Columns: []IndexColumn{}},
	}
	err = validateIndexDecls(decls)
	if !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("expected ErrInvalidOptions for empty-slice Columns, got %v", err)
	}
}

// TestIndexFingerprintErrorSchemaHashFormat verifies the formatted
// error message for the schema-hash discriminant. Per
// api-surface.md §IndexFingerprintError + indexing.md §Drift Guard
// example.
func TestIndexFingerprintErrorSchemaHashFormat(t *testing.T) {
	e := &IndexFingerprintError{
		Keyspace:     "workspaces",
		IndexName:    "by_repository",
		Field:        "schema-hash",
		StoredHash:   0x3f2a000000000000,
		SuppliedHash: 0xc104000000000000,
	}
	msg := e.Error()
	for _, want := range []string{
		`index "by_repository"`,
		`keyspace "workspaces"`,
		"schema-hash",
		"stored=0x3f2a000000000000",
		"supplied=0xc104000000000000",
		"RebuildIndex",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error message to contain %q; got %q", want, msg)
		}
	}
}

// TestIndexFingerprintErrorVersionFormat verifies the formatted error
// message for the version discriminant. Stored/SuppliedVersion
// surface as quoted strings; Stored/SuppliedHash are zero-valued
// sentinels and MUST NOT appear in the message (per
// api-surface.md §IndexFingerprintError — the zero values are NOT
// real hash values).
func TestIndexFingerprintErrorVersionFormat(t *testing.T) {
	e := &IndexFingerprintError{
		Keyspace:        "workspaces",
		IndexName:       "by_repository",
		Field:           "version",
		StoredVersion:   "v1",
		SuppliedVersion: "v2",
	}
	msg := e.Error()
	for _, want := range []string{
		`index "by_repository"`,
		`keyspace "workspaces"`,
		"version",
		`stored="v1"`,
		`supplied="v2"`,
		"RebuildIndex",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error message to contain %q; got %q", want, msg)
		}
	}
	// Zero-valued hash sentinels must not be formatted into the
	// version message (would be misleading per the spec).
	if strings.Contains(msg, "0x0000000000000000") {
		t.Errorf("version-field error must not format the zero-valued hash sentinels: %q", msg)
	}
}

// TestIndexFingerprintErrorUnwrap verifies errors.Is dispatch through
// the wrapper to the sentinel — the supported recovery dispatch per
// indexing.md §Recovery pattern after ErrIndexFingerprintMismatch.
func TestIndexFingerprintErrorUnwrap(t *testing.T) {
	e := &IndexFingerprintError{Field: "schema-hash"}
	if !errors.Is(e, ErrIndexFingerprintMismatch) {
		t.Fatalf("errors.Is failed to unwrap to ErrIndexFingerprintMismatch")
	}
	var target *IndexFingerprintError
	wrapped := error(e)
	if !errors.As(wrapped, &target) {
		t.Fatalf("errors.As failed to extract *IndexFingerprintError")
	}
	if target.Field != "schema-hash" {
		t.Fatalf("errors.As extracted struct with wrong Field: %q", target.Field)
	}
}
