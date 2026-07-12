package query_test

import (
	"context"
	"errors"
	"math/rand"
	"path/filepath"
	"slices"
	"testing"

	"github.com/thegrumpylion/gmdb"
	"github.com/thegrumpylion/gmdb/query"
	"github.com/thegrumpylion/gmdb/typed"
)

// poisonRowCodec wraps rowCodec with a switchable Decode failure:
// the corpus loads (and extractors run) with the poison OFF, then
// the test arms it — any subsequent ROW-BYTES decode fails loudly.
// Index-only plans never decode row bytes, so they survive an
// armed poison; every row-materializing route trips it. This is
// the behavioral pin for "the row keyspace is never read"
// (query-builder.md §Covering-aware execution route 1).
type poisonRowCodec struct{ armed *bool }

func (poisonRowCodec) ID() string { return "test/row" } // same stored schema as rowCodec
func (poisonRowCodec) AppendEncode(dst []byte, v row) ([]byte, error) {
	return rowCodec{}.AppendEncode(dst, v)
}
func (p poisonRowCodec) Decode(src []byte) (row, error) {
	if *p.armed {
		return row{}, errors.New("row bytes decoded while poison armed")
	}
	return rowCodec{}.Decode(src)
}

// openPoisonedDB builds a keyspace whose row decodes can be armed
// to fail. Columns must be re-declared against the poison codec's
// identity (same encoder ID keeps the synthesized names equal to
// the shared fixtures).
func openPoisonedDB(t *testing.T, indexes ...typed.AnyIndex[uint64, row]) (*typed.KeyspaceHandle[uint64, row], *bool, func()) {
	t.Helper()
	ctx := context.Background()
	db, err := gmdb.Open(ctx, filepath.Join(t.TempDir(), "db.gmdb"),
		gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 512})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		db.Close()
		t.Fatalf("Begin: %v", err)
	}
	armed := false
	tks := typed.NewKeyspace[uint64, row]("rows", typed.Uint64Encoder{}, poisonRowCodec{armed: &armed})
	h, err := tks.Create(tx, indexes...)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return h, &armed, func() { _ = tx.Rollback(); _ = db.Close() }
}

// Route 1, covering slots: Select satisfied by a covering column
// ⇒ index-only; the armed poison proves no row bytes decode
// (Inv-QB3's landed slice + the route-1 "never read" clause).
func TestQueryRowsIndexOnlyFromCovering(t *testing.T) {
	h, armed, cleanup := openPoisonedDB(t,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{
			Covering: []typed.AnySingleColumn[uint64, row]{colName},
		}),
	)
	defer cleanup()
	want := map[uint64]string{1: "x", 2: "y", 3: ""}
	for k, nm := range want {
		if err := h.Put(k, row{Grp: 7, Name: nm}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// A row OUTSIDE the seek group: the index-only interval must
	// close the EQ group exactly.
	if err := h.Put(4, row{Grp: 8, Name: "other"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	*armed = true

	q := query.New(h).Where(colGrp.Eq(7)).Select(colName)
	if kind, idx, route := planLeafRoute(q.Explain()); kind != "seek" || idx != "g" || route != query.ValuesEntry {
		t.Fatalf("plan = %s over %q values=%s, want seek over g values=entry", kind, idx, route)
	}
	got := map[uint64]string{}
	for k, p := range q.Rows() {
		nm, err := colName.From(p)
		if err != nil {
			t.Fatalf("From: %v", err)
		}
		got[k] = nm
	}
	if q.Err() != nil {
		t.Fatalf("Err: %v (row keyspace read on an index-only plan?)", q.Err())
	}
	if len(got) != len(want) {
		t.Fatalf("%d rows, want %d", len(got), len(want))
	}
	for k, nm := range want {
		if got[k] != nm {
			t.Fatalf("key %d: name %q, want %q", k, got[k], nm)
		}
	}
	// A column the plan does not carry errors at From (never a
	// zero value).
	q2 := query.New(h).Where(colGrp.Eq(7)).Select(colName)
	for _, p := range q2.Rows() {
		if _, err := colGrp.From(p); !errors.Is(err, typed.ErrColumnAbsent) {
			t.Fatalf("From(unselected) = %v, want ErrColumnAbsent", err)
		}
		break
	}
}

// Route 1 with MULTIPLE covering columns: slots resolve by their
// covering-tuple POSITION — selecting the second covering column
// must serve its own slot, not the first.
func TestQueryRowsIndexOnlyCoveringSlotPositions(t *testing.T) {
	h, armed, cleanup := openPoisonedDB(t,
		ci("i", anyCols(colID), typed.ColumnIndexOpts[uint64, row]{
			Covering: []typed.AnySingleColumn[uint64, row]{colName, colGrp},
		}),
	)
	defer cleanup()
	if err := h.Put(9, row{Grp: 42, Name: "n9"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	*armed = true
	q := query.New(h).Where(colID.Eq(9)).Select(colGrp, colName)
	if kind, idx, route := planLeafRoute(q.Explain()); kind != "seek" || idx != "i" || route != query.ValuesEntry {
		t.Fatalf("plan = %s over %q values=%s, want seek over i values=entry", kind, idx, route)
	}
	n := 0
	for k, p := range q.Rows() {
		g, err := colGrp.From(p)
		if err != nil || g != 42 {
			t.Fatalf("grp slot = (%d, %v), want (42, nil) — wrong covering position?", g, err)
		}
		nm, err := colName.From(p)
		if err != nil || nm != "n9" {
			t.Fatalf("name slot = (%q, %v), want (n9, nil)", nm, err)
		}
		if k != 9 {
			t.Fatalf("key = %d, want 9", k)
		}
		n++
	}
	if q.Err() != nil || n != 1 {
		t.Fatalf("rows=%d err=%v, want 1 nil", n, q.Err())
	}
}

// Route 1, key-column slots: the selected column is a KEY column
// past the EQ prefix — served from RangeEntries' decoded tuple,
// still no row read.
func TestQueryRowsIndexOnlyFromKeyColumns(t *testing.T) {
	h, armed, cleanup := openPoisonedDB(t,
		ci("gn", anyCols(colGrp, colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	rows := map[uint64]row{
		1: {Grp: 1, Name: "alpha"},
		2: {Grp: 1, Name: "beta"},
		3: {Grp: 2, Name: "gamma"},
		4: {Grp: 1, Name: "zz"}, // excluded ONLY by the residual bound
	}
	for k, v := range rows {
		if err := h.Put(k, v); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	*armed = true

	// Prefix leaf + residual bound on the carried name column:
	// residual evaluates over entry slot bytes too.
	q := query.New(h).Where(colGrp.Eq(1), colName.Gte("a"), colName.Lt("z")).Select(colName)
	if kind, idx, route := planLeafRoute(q.Explain()); kind != "range" || idx != "gn" || route != query.ValuesEntry {
		t.Fatalf("plan = %s over %q values=%s, want range over gn values=entry", kind, idx, route)
	}
	got := map[uint64]string{}
	for k, p := range q.Rows() {
		nm, err := colName.From(p)
		if err != nil {
			t.Fatalf("From: %v", err)
		}
		got[k] = nm
	}
	if q.Err() != nil {
		t.Fatalf("Err: %v (row keyspace read on an index-only plan?)", q.Err())
	}
	if len(got) != 2 || got[1] != "alpha" || got[2] != "beta" {
		t.Fatalf("rows = %v, want {1:alpha 2:beta}", got)
	}
}

// Route 2: All() on a CoverValue index decodes V from the entry's
// full-row covering bytes; plan pinning via the value route
// (behaviorally identical to back-lookup by design — the route is
// a read strategy).
func TestQueryAllCoverValueRoute(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("cv", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{CoverValue: true}),
	)
	defer cleanup()
	want := map[uint64]row{
		1: {Grp: 3, Name: "x", Tags: []string{"a", "b"}},
		2: {Grp: 3, Name: "y"},
	}
	for k, v := range want {
		if err := h.Put(k, v); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	q := query.New(h).Where(colGrp.Eq(3))
	if kind, idx, route := planLeafRoute(q.Explain()); kind != "seek" || idx != "cv" || route != query.ValuesCoverValue {
		t.Fatalf("plan = %s over %q values=%s, want seek over cv values=cover-value", kind, idx, route)
	}
	got := map[uint64]row{}
	for k, v := range q.All() {
		got[k] = v
	}
	if q.Err() != nil || len(got) != len(want) {
		t.Fatalf("rows=%d err=%v, want %d rows nil err", len(got), q.Err(), len(want))
	}
	for k, w := range want {
		g := got[k]
		if g.Grp != w.Grp || g.Name != w.Name || !slices.Equal(g.Tags, w.Tags) {
			t.Fatalf("key %d: %+v, want %+v", k, g, w)
		}
	}
	// Rows() on a CoverValue index materializes V from the entry
	// (route 2) and projects — an opaque filter stays legal there
	// (Inv-QB7: full-row covering IS the whole row).
	q2 := query.New(h).Where(colGrp.Eq(3)).Select(colName).
		Filter(func(_ uint64, v row) bool { return v.Name == "x" && v.Grp == 3 })
	if kind, _, route := planLeafRoute(q2.Explain()); kind != "seek" || route != query.ValuesCoverValue {
		t.Fatalf("plan = %s values=%s, want seek values=cover-value", kind, route)
	}
	n := 0
	for k, p := range q2.Rows() {
		nm, err := colName.From(p)
		if err != nil || k != 1 || nm != "x" {
			t.Fatalf("row = (%d, %q, %v), want (1, x, nil)", k, nm, err)
		}
		n++
	}
	if q2.Err() != nil || n != 1 {
		t.Fatalf("rows=%d err=%v, want 1 nil", n, q2.Err())
	}
}

// Route 1 with a residual Or: disjunction trees evaluate over
// entry slot bytes with the same semantics as the row path.
func TestQueryRowsIndexOnlyResidualOr(t *testing.T) {
	h, armed, cleanup := openPoisonedDB(t,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{
			Covering: []typed.AnySingleColumn[uint64, row]{colName},
		}),
	)
	defer cleanup()
	for k, nm := range map[uint64]string{1: "x", 2: "y", 3: "z"} {
		if err := h.Put(k, row{Grp: 1, Name: nm}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	*armed = true
	q := query.New(h).Where(colGrp.Eq(1), typed.Or(
		[]typed.Term[uint64, row]{colName.Eq("x")},
		[]typed.Term[uint64, row]{colName.Eq("y")},
	)).Select(colName)
	if kind, _, route := planLeafRoute(q.Explain()); kind != "seek" || route != query.ValuesEntry {
		t.Fatalf("plan = %s values=%s, want seek values=entry", kind, route)
	}
	var keys []uint64
	for k := range q.Rows() {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if q.Err() != nil || !slices.Equal(keys, []uint64{1, 2}) {
		t.Fatalf("Or over entry slots = %v (err %v), want [1 2]", keys, q.Err())
	}
}

// The index-only route's live revalidation, per sub-guard: a
// same-tx Rebuild that keeps the key tuple but changes the
// covering set must not serve stale positions — the executor
// falls back to the row-materialized scan (which the disarmed
// poison observes) rather than decoding the wrong slot's bytes.
func TestQueryRowsEntryExecRevalidatesLiveCovering(t *testing.T) {
	ctx := context.Background()
	db, err := gmdb.Open(ctx, filepath.Join(t.TempDir(), "db.gmdb"),
		gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	tks := typed.NewKeyspace[uint64, row]("rows", typed.Uint64Encoder{}, rowCodec{})
	h, err := tks.Create(tx, ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{
		Covering: []typed.AnySingleColumn[uint64, row]{colName},
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.Put(1, row{Grp: 1, Name: "real"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Same key tuple, covering REPLACED by the id column: the
	// snapshot's name slot no longer exists in live entries.
	grpCol := synthName("gmdb/col/", "grp", "gmdb/be-uint32")
	idCol := synthName("gmdb/col/", "id", "gmdb/be-uint64")
	err = tx.Indexes().Rebuild("rows", &gmdb.IndexDecl{
		Name:     "g",
		Columns:  []gmdb.IndexColumn{{Name: grpCol}},
		Covering: []gmdb.IndexCoveringColumn{{Name: idCol}},
		Extract: func(kb, vb []byte) []gmdb.IndexEntry {
			v, err := rowCodec{}.Decode(vb)
			if err != nil {
				panic(err)
			}
			grp := []byte{byte(v.Grp >> 24), byte(v.Grp >> 16), byte(v.Grp >> 8), byte(v.Grp)}
			kc := make([]byte, len(kb))
			copy(kc, kb)
			return []gmdb.IndexEntry{{Cols: [][]byte{grp}, Cover: [][]byte{kc}}}
		},
	})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// Selected column vanished from live covering.
	q := query.New(h).Where(colGrp.Eq(1)).Select(colName)
	n := 0
	for k, p := range q.Rows() {
		nm, err := colName.From(p)
		if err != nil || k != 1 || nm != "real" {
			t.Fatalf("row = (%d, %q, %v), want (1, real, nil) — stale covering slot served?", k, nm, err)
		}
		n++
	}
	if q.Err() != nil || n != 1 {
		t.Fatalf("rows=%d err=%v, want 1 nil", n, q.Err())
	}

	// Residual-term column vanished from live covering: grp
	// (selected) stays resolvable via the key tuple, so only the
	// residual-resolvability guard stands between the executor and
	// evaluating name against a nonexistent slot. Snapshot-side
	// the query is index-only eligible (both columns carried).
	q2 := query.New(h).Where(colGrp.Eq(1), colName.HasPrefix("re")).Select(colGrp)
	if _, _, route := planLeafRoute(q2.Explain()); route != query.ValuesEntry {
		t.Fatalf("snapshot route = %s, want entry (the guard must be what intervenes)", route)
	}
	n2 := 0
	for k, p := range q2.Rows() {
		g, err := colGrp.From(p)
		if err != nil || k != 1 || g != 1 {
			t.Fatalf("row = (%d, %d, %v), want (1, 1, nil)", k, g, err)
		}
		n2++
	}
	if q2.Err() != nil || n2 != 1 {
		t.Fatalf("residual-guard rows=%d err=%v, want 1 nil", n2, q2.Err())
	}
}

// modeCover trusts the live sentinel only when it embeds THIS
// handle's value-encoder ID: a rebuild installing a FOREIGN
// encoder's cover-value sentinel routes to back-lookup instead of
// decoding foreign bytes as V.
func TestQueryCoverValueForeignEncoderFallsBack(t *testing.T) {
	ctx := context.Background()
	db, err := gmdb.Open(ctx, filepath.Join(t.TempDir(), "db.gmdb"),
		gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	tks := typed.NewKeyspace[uint64, row]("rows", typed.Uint64Encoder{}, rowCodec{})
	h, err := tks.Create(tx, ci("cv", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := row{Grp: 3, Name: "true-row", Tags: []string{"t"}}
	if err := h.Put(1, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Install a cover-value sentinel for a DIFFERENT encoder whose
	// covering bytes are NOT encode(V) under this handle's codec.
	grpCol := synthName("gmdb/col/", "grp", "gmdb/be-uint32")
	err = tx.Indexes().Rebuild("rows", &gmdb.IndexDecl{
		Name:     "cv",
		Columns:  []gmdb.IndexColumn{{Name: grpCol}},
		Covering: []gmdb.IndexCoveringColumn{{Name: "gmdb/cover-value/other/codec"}},
		Extract: func(kb, vb []byte) []gmdb.IndexEntry {
			v, err := rowCodec{}.Decode(vb)
			if err != nil {
				panic(err)
			}
			grp := []byte{byte(v.Grp >> 24), byte(v.Grp >> 16), byte(v.Grp >> 8), byte(v.Grp)}
			return []gmdb.IndexEntry{{Cols: [][]byte{grp}, Cover: [][]byte{[]byte("foreign-bytes")}}}
		},
	})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	q := query.New(h).Where(colGrp.Eq(3))
	n := 0
	for k, v := range q.All() {
		if k != 1 || v.Name != want.Name || !slices.Equal(v.Tags, want.Tags) {
			t.Fatalf("row = (%d, %+v), want the true row (foreign covering decoded as V?)", k, v)
		}
		n++
	}
	if q.Err() != nil || n != 1 {
		t.Fatalf("rows=%d err=%v, want 1 nil", n, q.Err())
	}
}

// Inv-QB7: an opaque Filter forces whole-row materialization —
// the plan is never index-only, and the filter observes the FULL
// decoded V (fields outside the index included).
func TestQueryFilterForcesWholeRows(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{
			Covering: []typed.AnySingleColumn[uint64, row]{colName},
		}),
	)
	defer cleanup()
	_ = h.Put(1, row{Grp: 1, Name: "x", Tags: []string{"whole"}})
	_ = h.Put(2, row{Grp: 1, Name: "y"})

	q := query.New(h).Where(colGrp.Eq(1)).Select(colName).
		Filter(func(_ uint64, v row) bool { return slices.Contains(v.Tags, "whole") })
	if _, _, route := planLeafRoute(q.Explain()); route == query.ValuesEntry {
		t.Fatalf("filter query planned index-only: %s", q.Explain())
	}
	var keys []uint64
	for k := range q.Rows() {
		keys = append(keys, k)
	}
	if q.Err() != nil || !slices.Equal(keys, []uint64{1}) {
		t.Fatalf("keys = %v (err %v), want [1] — the filter must see Tags", keys, q.Err())
	}
}

// Inv-QB3 + Inv-TC5 through the query surface: covering slots
// reflect the row's CURRENT value after an update.
func TestQueryRowsCoveringFreshAfterUpdate(t *testing.T) {
	h, armed, cleanup := openPoisonedDB(t,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{
			Covering: []typed.AnySingleColumn[uint64, row]{colName},
		}),
	)
	defer cleanup()
	if err := h.Put(1, row{Grp: 5, Name: "old"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := h.Put(1, row{Grp: 5, Name: "new"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	*armed = true
	q := query.New(h).Where(colGrp.Eq(5)).Select(colName)
	n := 0
	for _, p := range q.Rows() {
		nm, err := colName.From(p)
		if err != nil || nm != "new" {
			t.Fatalf("slot = (%q, %v), want the updated value", nm, err)
		}
		n++
	}
	if q.Err() != nil || n != 1 {
		t.Fatalf("rows=%d err=%v, want exactly the updated row", n, q.Err())
	}
}

// Rows construction errors: no Select, nil Select column.
func TestQueryRowsConstructionErrors(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 3, rng)
	defer cleanup()

	q := query.New(h)
	for range q.Rows() {
		t.Fatal("Rows without Select yielded")
	}
	if q.Err() == nil {
		t.Fatal("Rows without Select: Err is nil")
	}
	q2 := query.New(h).Select(nil)
	for range q2.Rows() {
		t.Fatal("nil Select column yielded")
	}
	if q2.Err() == nil {
		t.Fatal("nil Select column: Err is nil")
	}
}

// A same-tx Rebuild that REORDERS the index's columns invalidates
// the plan's literals; the executor detects the live-tuple
// mismatch and falls back to a scan with correct results —
// Inv-QB1 under every declaration shape.
func TestQueryIndexExecFallsBackOnLiveTupleChange(t *testing.T) {
	ctx := context.Background()
	db, err := gmdb.Open(ctx, filepath.Join(t.TempDir(), "db.gmdb"),
		gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	tks := typed.NewKeyspace[uint64, row]("rows", typed.Uint64Encoder{}, rowCodec{})
	h, err := tks.Create(tx, ci("gn", anyCols(colGrp, colName), typed.ColumnIndexOpts[uint64, row]{}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rows := map[uint64]row{
		1: {Grp: 1, Name: "a"},
		2: {Grp: 1, Name: "b"},
		3: {Grp: 2, Name: "a"},
	}
	for k, v := range rows {
		if err := h.Put(k, v); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Rebuild the same name with the column tuple REVERSED
	// ((name, grp) instead of (grp, name)).
	grpCol := synthName("gmdb/col/", "grp", "gmdb/be-uint32")
	nameCol := synthName("gmdb/col/", "name", "gmdb/string")
	err = tx.Indexes().Rebuild("rows", &gmdb.IndexDecl{
		Name:    "gn",
		Columns: []gmdb.IndexColumn{{Name: nameCol}, {Name: grpCol}},
		Extract: func(kb, vb []byte) []gmdb.IndexEntry {
			v, err := rowCodec{}.Decode(vb)
			if err != nil {
				panic(err)
			}
			grp := []byte{byte(v.Grp >> 24), byte(v.Grp >> 16), byte(v.Grp >> 8), byte(v.Grp)}
			return []gmdb.IndexEntry{{Cols: [][]byte{[]byte(v.Name), grp}}}
		},
	})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	q := query.New(h).Where(colGrp.Eq(1))
	var keys []uint64
	for k := range q.Keys() {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if q.Err() != nil || !slices.Equal(keys, []uint64{1, 2}) {
		t.Fatalf("keys = %v (err %v), want [1 2] — reordered live tuple must fall back to scan", keys, q.Err())
	}

	// The index-only route falls back the same way.
	q2 := query.New(h).Where(colGrp.Eq(1)).Select(colName)
	got := map[uint64]string{}
	for k, p := range q2.Rows() {
		nm, err := colName.From(p)
		if err != nil {
			t.Fatalf("From: %v", err)
		}
		got[k] = nm
	}
	if q2.Err() != nil || len(got) != 2 || got[1] != "a" || got[2] != "b" {
		t.Fatalf("rows = %v (err %v), want {1:a 2:b}", got, q2.Err())
	}
}
