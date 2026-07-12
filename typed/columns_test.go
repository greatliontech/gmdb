package typed

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/thegrumpylion/gmdb"
)

// rowVal is the test row type for column-tier tests.
type rowVal struct {
	Grp  uint8
	Tags []string
}

// rowEnc is a deliberately simple lex-safe encoder for rowVal:
// grp byte || count byte || (len byte || tag)*.
type rowEnc struct{}

func (rowEnc) ID() string { return "test/rowVal" }
func (rowEnc) AppendEncode(dst []byte, v rowVal) ([]byte, error) {
	dst = append(dst, v.Grp, byte(len(v.Tags)))
	for _, t := range v.Tags {
		dst = append(dst, byte(len(t)))
		dst = append(dst, t...)
	}
	return dst, nil
}
func (rowEnc) Decode(src []byte) (rowVal, error) {
	if len(src) < 2 {
		return rowVal{}, fmt.Errorf("short rowVal")
	}
	v := rowVal{Grp: src[0]}
	n := int(src[1])
	src = src[2:]
	for i := 0; i < n; i++ {
		if len(src) < 1 || len(src) < 1+int(src[0]) {
			return rowVal{}, fmt.Errorf("short tag")
		}
		l := int(src[0])
		v.Tags = append(v.Tags, string(src[1:1+l]))
		src = src[1+l:]
	}
	return v, nil
}

func colGrp() *Column[uint64, rowVal, uint32] {
	return NewColumn("grp", Uint32Encoder{}, func(_ uint64, v rowVal) uint32 { return uint32(v.Grp) })
}

func colTags() *MultiColumn[uint64, rowVal, string] {
	return NewMultiColumn("tag", StringEncoder{}, func(_ uint64, v rowVal) []string { return v.Tags })
}

func openColumnsDB(t *testing.T, decls ...AnyIndex[uint64, rowVal]) (*KeyspaceHandle[uint64, rowVal], *gmdb.Tx, func()) {
	t.Helper()
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	tx, err := db.Begin(ctx)
	if err != nil {
		db.Close()
		t.Fatalf("Begin: %v", err)
	}
	tks := NewKeyspace[uint64, rowVal]("rows", Uint64Encoder{}, rowEnc{})
	h, err := tks.Create(tx, decls...)
	if err != nil {
		tx.Rollback()
		db.Close()
		t.Fatalf("Create: %v", err)
	}
	return h, tx, func() { _ = tx.Rollback(); _ = db.Close() }
}

// Inv-TC4 (compilation equivalence): the lowered extractor's
// output equals the rule — Where gate, then the Cartesian product
// of per-column encoded sequences as a MULTISET in element order,
// rightmost fastest, no tier-side dedup. Compared against an
// independent reference implementation over randomized rows.
func TestColumnIndexExtractorMatchesRule(t *testing.T) {
	keyEnc := Uint64Encoder{}
	valEnc := rowEnc{}
	ci := NewColumnIndex("byGrpTag",
		[]AnyColumn[uint64, rowVal]{colGrp(), colTags()},
		ColumnIndexOpts[uint64, rowVal]{
			Where: func(_ uint64, v rowVal) bool { return v.Grp != 9 },
		})
	decl, err := ci.indexDecl(keyEnc, valEnc)
	if err != nil {
		t.Fatalf("indexDecl: %v", err)
	}

	reference := func(k uint64, v rowVal) [][]string {
		if v.Grp == 9 {
			return nil
		}
		grpB, _ := Uint32Encoder{}.AppendEncode(nil, uint32(v.Grp))
		var out [][]string
		for _, tag := range v.Tags { // rightmost (tags) fastest under 2 cols
			tagB, _ := StringEncoder{}.AppendEncode(nil, tag)
			out = append(out, []string{string(grpB), string(tagB)})
		}
		return out
	}

	rng := rand.New(rand.NewSource(7))
	tagPool := []string{"go", "db", "go", "x", ""}
	for i := 0; i < 200; i++ {
		k := rng.Uint64()
		v := rowVal{Grp: uint8(rng.Intn(11))}
		for j := rng.Intn(4); j > 0; j-- {
			v.Tags = append(v.Tags, tagPool[rng.Intn(len(tagPool))])
		}
		kb, _ := keyEnc.AppendEncode(nil, k)
		vb, _ := valEnc.AppendEncode(nil, v)

		got := decl.Extract(kb, vb)
		want := reference(k, v)
		if len(got) != len(want) {
			t.Fatalf("row %d (%+v): %d entries, want %d", i, v, len(got), len(want))
		}
		for e := range got {
			if len(got[e].Cols) != 2 ||
				string(got[e].Cols[0]) != want[e][0] ||
				string(got[e].Cols[1]) != want[e][1] {
				t.Fatalf("row %d entry %d: got %q want %q", i, e, got[e].Cols, want[e])
			}
		}
	}
}

// Inv-TC2 (fingerprint distinctness): a Column and a MultiColumn
// with the SAME user name and encoder ID synthesize different
// byte column names — a stored index built under one form fails
// the drift guard when opened with the other.
func TestColumnFormsAreFingerprintDistinct(t *testing.T) {
	single := NewColumn("f", StringEncoder{}, func(_ uint64, v rowVal) string { return "" })
	multi := NewMultiColumn("f", StringEncoder{}, func(_ uint64, v rowVal) []string { return nil })
	if single.columnName() == multi.columnName() {
		t.Fatal("Column and MultiColumn with identical (name, encoder ID) synthesized the same byte column name")
	}

	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()
	tks := NewKeyspace[uint64, rowVal]("rows", Uint64Encoder{}, rowEnc{})

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ciSingle := NewColumnIndex("i", []AnyColumn[uint64, rowVal]{single}, ColumnIndexOpts[uint64, rowVal]{})
	if _, err := tks.Create(tx, ciSingle); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	tx, err = db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ciMulti := NewColumnIndex("i", []AnyColumn[uint64, rowVal]{multi}, ColumnIndexOpts[uint64, rowVal]{})
	_, err = tks.Open(tx, ciMulti)
	if !errors.Is(err, gmdb.ErrIndexFingerprintMismatch) {
		t.Fatalf("open with the other column form = %v, want ErrIndexFingerprintMismatch", err)
	}
}

// The reserved column namespace is not mintable (Inv-TC2 cross-
// form disjointness): encoder IDs inside gmdb/col/, gmdb/multicol/
// or gmdb/cover-value/ are rejected at open — on ColumnIndex
// columns AND on Index's IK encoder (whose synthesized name is
// the raw ID).
func TestReservedEncoderIDNamespaceRejected(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	badEnc := FuncEncoder[string]{
		EncodeFunc: func(dst []byte, v string) ([]byte, error) { return append(dst, v...), nil },
		DecodeFunc: func(src []byte) (string, error) { return string(src), nil },
		EncoderID:  "gmdb/col/evil",
	}
	tks := NewKeyspace[uint64, rowVal]("rows", Uint64Encoder{}, rowEnc{})

	ci := NewColumnIndex("i",
		[]AnyColumn[uint64, rowVal]{NewColumn("c", badEnc, func(_ uint64, _ rowVal) string { return "" })},
		ColumnIndexOpts[uint64, rowVal]{})
	if _, err := tks.Create(tx, ci); !errors.Is(err, gmdb.ErrIndexEncoderIDReserved) {
		t.Fatalf("ColumnIndex with reserved encoder ID = %v, want ErrIndexEncoderIDReserved", err)
	}

	badEnc.EncoderID = "gmdb/multicol/evil"
	ti := &Index[uint64, rowVal, string]{
		Name: "j", IKEnc: badEnc,
		Extract: func(_ uint64, _ rowVal) []string { return nil },
	}
	if _, err := tks.Create(tx, ti); !errors.Is(err, gmdb.ErrIndexEncoderIDReserved) {
		t.Fatalf("Index with reserved IK encoder ID = %v, want ErrIndexEncoderIDReserved", err)
	}
}

// Unique x MultiColumn composes at ELEMENT granularity: an
// intra-row duplicate element is a candidate-set collision; a
// cross-row duplicate element hits the on-disk probe. On a
// NON-unique index the intra-row duplicate collapses last-wins to
// a single entry.
func TestColumnIndexUniqueMultiElementSemantics(t *testing.T) {
	// Non-unique: dup elements collapse; lookup yields the row once.
	h, _, cleanup := openColumnsDB(t,
		NewColumnIndex("byTag", []AnyColumn[uint64, rowVal]{colTags()}, ColumnIndexOpts[uint64, rowVal]{}))
	defer cleanup()
	if err := h.Put(1, rowVal{Grp: 1, Tags: []string{"go", "go"}}); err != nil {
		t.Fatalf("Put dup-tag row (non-unique): %v", err)
	}
	idx, err := h.Index("byTag")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	goB, _ := StringEncoder{}.AppendEncode(nil, "go")
	n := 0
	for range idx.idx.Lookup([][]byte{goB}) {
		n++
	}
	if n != 1 {
		t.Fatalf("dup element under non-unique: %d entries, want 1 (last-wins collapse)", n)
	}

	// Unique: intra-row duplicate = candidate-set violation.
	hu, _, cleanupU := openColumnsDB(t,
		NewColumnIndex("uTag", []AnyColumn[uint64, rowVal]{colTags()}, ColumnIndexOpts[uint64, rowVal]{Unique: true}))
	defer cleanupU()
	if err := hu.Put(1, rowVal{Tags: []string{"go", "go"}}); !errors.Is(err, gmdb.ErrIndexUniqueViolation) {
		t.Fatalf("intra-row dup element under unique = %v, want ErrIndexUniqueViolation", err)
	}
	// Cross-row duplicate element = on-disk probe hit.
	if err := hu.Put(1, rowVal{Tags: []string{"go"}}); err != nil {
		t.Fatalf("Put row1: %v", err)
	}
	if err := hu.Put(2, rowVal{Tags: []string{"db", "go"}}); !errors.Is(err, gmdb.ErrIndexUniqueViolation) {
		t.Fatalf("cross-row dup element under unique = %v, want ErrIndexUniqueViolation", err)
	}
}

// Where gates whole rows, and updates diff element sets exactly
// (adds insert, removals delete) — indexing.md §Partial Indexes
// through the column tier.
func TestColumnIndexWhereGatingAndUpdateDiff(t *testing.T) {
	h, _, cleanup := openColumnsDB(t,
		NewColumnIndex("byTag", []AnyColumn[uint64, rowVal]{colTags()},
			ColumnIndexOpts[uint64, rowVal]{Where: func(_ uint64, v rowVal) bool { return v.Grp != 0 }}))
	defer cleanup()
	idx, err := h.Index("byTag")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	count := func(tag string) int {
		t.Helper()
		b, _ := StringEncoder{}.AppendEncode(nil, tag)
		n := 0
		for range idx.idx.Lookup([][]byte{b}) {
			n++
		}
		if err := idx.idx.Err(); err != nil {
			t.Fatalf("Err: %v", err)
		}
		return n
	}

	if err := h.Put(1, rowVal{Grp: 0, Tags: []string{"go"}}); err != nil { // gated out
		t.Fatalf("Put: %v", err)
	}
	if count("go") != 0 {
		t.Fatal("Where-gated row produced entries")
	}
	if err := h.Put(1, rowVal{Grp: 1, Tags: []string{"go", "db"}}); err != nil { // now indexed
		t.Fatalf("Put: %v", err)
	}
	if count("go") != 1 || count("db") != 1 {
		t.Fatal("un-gated update did not insert element entries")
	}
	if err := h.Put(1, rowVal{Grp: 1, Tags: []string{"db"}}); err != nil { // element removed
		t.Fatalf("Put: %v", err)
	}
	if count("go") != 0 || count("db") != 1 {
		t.Fatal("element-set diff on update wrong")
	}
}

// Declaration-shape rejections: zero columns, nil column, empty
// encoder ID.
func TestColumnIndexDeclarationRejections(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	tks := NewKeyspace[uint64, rowVal]("rows", Uint64Encoder{}, rowEnc{})

	if _, err := tks.Create(tx,
		NewColumnIndex[uint64, rowVal]("z", nil, ColumnIndexOpts[uint64, rowVal]{})); !errors.Is(err, gmdb.ErrInvalidOptions) {
		t.Fatalf("zero columns = %v, want ErrInvalidOptions", err)
	}
	if _, err := tks.Create(tx,
		NewColumnIndex("n", []AnyColumn[uint64, rowVal]{nil}, ColumnIndexOpts[uint64, rowVal]{})); !errors.Is(err, gmdb.ErrInvalidOptions) {
		t.Fatalf("nil column = %v, want ErrInvalidOptions", err)
	}
	empty := FuncEncoder[string]{
		EncodeFunc: func(dst []byte, v string) ([]byte, error) { return append(dst, v...), nil },
		DecodeFunc: func(src []byte) (string, error) { return string(src), nil },
	}
	if _, err := tks.Create(tx,
		NewColumnIndex("e",
			[]AnyColumn[uint64, rowVal]{NewColumn("c", empty, func(_ uint64, _ rowVal) string { return "" })},
			ColumnIndexOpts[uint64, rowVal]{})); !errors.Is(err, gmdb.ErrIndexEncoderIDEmpty) {
		t.Fatalf("empty encoder ID = %v, want ErrIndexEncoderIDEmpty", err)
	}
}

// The synthesized-name grammar is injective over (form, name,
// encoder ID) — shifting bytes between the two length-prefixed
// halves changes the name.
func TestSynthesizedColumnNameInjective(t *testing.T) {
	a := synthesizeColumnName(columnNamePrefix, "ab", "cd")
	b := synthesizeColumnName(columnNamePrefix, "abc", "d")
	c := synthesizeColumnName(columnNamePrefix, "a", "bcd")
	if a == b || b == c || a == c {
		t.Fatalf("grammar not injective: %q %q %q", a, b, c)
	}
	if !slices.Contains([]byte(a), byte(2)) { // uvarint(2) prefix present
		t.Fatalf("expected uvarint length prefixes in %q", a)
	}
}

// Inv-TC4's ordering clause is engine-observable (chunk-independent:
// last-wins collapse keys off encounter order): with TWO
// multi-columns the product must enumerate rightmost-fastest in
// element order — ax, ay, bx, by.
func TestColumnIndexProductOrderRightmostFastest(t *testing.T) {
	first := NewMultiColumn("f", StringEncoder{}, func(_ uint64, v rowVal) []string { return []string{"a", "b"} })
	second := NewMultiColumn("s", StringEncoder{}, func(_ uint64, v rowVal) []string { return []string{"x", "y"} })
	ci := NewColumnIndex("p", []AnyColumn[uint64, rowVal]{first, second}, ColumnIndexOpts[uint64, rowVal]{})
	decl, err := ci.indexDecl(Uint64Encoder{}, rowEnc{})
	if err != nil {
		t.Fatalf("indexDecl: %v", err)
	}
	kb, _ := Uint64Encoder{}.AppendEncode(nil, 1)
	vb, _ := rowEnc{}.AppendEncode(nil, rowVal{})
	got := decl.Extract(kb, vb)
	want := [][2]string{{"a", "x"}, {"a", "y"}, {"b", "x"}, {"b", "y"}}
	if len(got) != len(want) {
		t.Fatalf("product size %d, want %d", len(got), len(want))
	}
	dec := func(b []byte) string { s, _ := (StringEncoder{}).Decode(b); return s }
	for i, e := range got {
		if dec(e.Cols[0]) != want[i][0] || dec(e.Cols[1]) != want[i][1] {
			t.Fatalf("entry %d = (%s,%s), want %v (rightmost-fastest element order)",
				i, dec(e.Cols[0]), dec(e.Cols[1]), want[i])
		}
	}
}

// Declaring the same column twice in one ColumnIndex is rejected —
// positions would be ambiguous for per-column consumers.
func TestColumnIndexDuplicateColumnRejected(t *testing.T) {
	c := colGrp()
	ci := NewColumnIndex("d", []AnyColumn[uint64, rowVal]{c, c}, ColumnIndexOpts[uint64, rowVal]{})
	_, err := ci.indexDecl(Uint64Encoder{}, rowEnc{})
	if !errors.Is(err, gmdb.ErrInvalidOptions) {
		t.Fatalf("duplicate column = %v, want ErrInvalidOptions", err)
	}
}
