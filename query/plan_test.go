package query_test

import (
	"context"
	"encoding/binary"
	"iter"
	"math/rand"
	"path/filepath"
	"slices"
	"testing"

	"github.com/thegrumpylion/gmdb"
	"github.com/thegrumpylion/gmdb/query"
	"github.com/thegrumpylion/gmdb/typed"
)

func ci(name string, cols []typed.AnyColumn[uint64, row], opts typed.ColumnIndexOpts[uint64, row]) typed.AnyIndex[uint64, row] {
	return typed.NewColumnIndex(name, cols, opts)
}

func anyCols(cols ...typed.AnyColumn[uint64, row]) []typed.AnyColumn[uint64, row] { return cols }

// Rule 2's shape mapping and rule 6's fallback, pinned via Explain
// (query-builder.md §Planning rules).
func TestPlannerShapeSelection(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 30, rng,
		ci("gn", anyCols(colGrp, colName), typed.ColumnIndexOpts[uint64, row]{}),
		ci("n", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
		ci("t", anyCols(colTags), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()

	for _, tc := range []struct {
		name      string
		build     func() *query.Query[uint64, row]
		wantKind  string
		wantIndex string
	}{
		{"all-EQ is a seek", func() *query.Query[uint64, row] {
			return query.New(h).Where(colGrp.Eq(3), colName.Eq("a"))
		}, "seek", "gn"},
		{"leading EQ only is a prefix", func() *query.Query[uint64, row] {
			return query.New(h).Where(colGrp.Eq(3))
		}, "prefix", "gn"},
		{"EQ prefix + trailing bound is a range", func() *query.Query[uint64, row] {
			return query.New(h).Where(colGrp.Eq(3), colName.Lt("m"))
		}, "range", "gn"},
		{"empty prefix + bound on the first column is a range", func() *query.Query[uint64, row] {
			return query.New(h).Where(colName.Lt("m"))
		}, "range", "n"},
		{"Contains is EQ-shaped", func() *query.Query[uint64, row] {
			return query.New(h).Where(colTags.Contains("go"))
		}, "seek", "t"},
		{"no terms falls back to scan", func() *query.Query[uint64, row] {
			return query.New(h)
		}, "scan", ""},
		{"top-level Or with pushable groups becomes a Union", func() *query.Query[uint64, row] {
			return query.New(h).Where(typed.Or([]typed.Term[uint64, row]{colGrp.Eq(1)}))
		}, "union", ""},
		{"Or with an unpushable group degrades whole to scan", func() *query.Query[uint64, row] {
			return query.New(h).Where(typed.Or(
				[]typed.Term[uint64, row]{colGrp.Eq(1)},
				[]typed.Term[uint64, row]{colID.Eq(9)}, // id undeclared: group unpushable
			))
		}, "scan", ""},
		{"undeclared column falls back to scan", func() *query.Query[uint64, row] {
			return query.New(h).Where(colID.Eq(4))
		}, "scan", ""},
	} {
		kind, index := planLeafOf(tc.build().Explain())
		if kind != tc.wantKind || index != tc.wantIndex {
			t.Errorf("%s: plan = %s over %q, want %s over %q", tc.name, kind, index, tc.wantKind, tc.wantIndex)
		}
	}

	// The second range-shaped term on the bound column stays
	// residual: exactly ONE trailing bound is consumed.
	q := query.New(h).Where(colName.Gte("b"), colName.Lt("x"))
	p := q.Explain()
	rf, ok := p.Root.(query.ResidualFilter)
	if !ok || rf.Terms != 1 {
		t.Fatalf("two bounds on one column: plan = %s, want ResidualFilter(terms=1) over a range", p)
	}
	if r, ok := rf.Input.(query.IndexRange); !ok || r.Index != "n" || r.PrefixLen != 0 {
		t.Fatalf("two bounds on one column: leaf = %s, want IndexRange(n, prefix=0)", rf.Input)
	}
}

// Rule 3's tie-break ladder: (a) most terms consumed, (b)
// covering, (c) unique, (d) name.
func TestPlannerScoringTieBreaks(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	t.Run("most-consumed beats shorter", func(t *testing.T) {
		h, _, cleanup := openQueryDB(t, 10, rng,
			ci("aa", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
			ci("zz", anyCols(colGrp, colName), typed.ColumnIndexOpts[uint64, row]{}),
		)
		defer cleanup()
		if _, idx := planLeafOf(query.New(h).Where(colGrp.Eq(1), colName.Eq("a")).Explain()); idx != "zz" {
			t.Fatalf("chose %q, want zz (2 terms consumed beat 1 despite name order)", idx)
		}
	})
	t.Run("covering breaks the consumed tie", func(t *testing.T) {
		h, _, cleanup := openQueryDB(t, 10, rng,
			ci("aa", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
			ci("zz", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{Covering: []typed.AnySingleColumn[uint64, row]{colName}}),
		)
		defer cleanup()
		// The query touches grp and name; only zz carries both.
		if _, idx := planLeafOf(query.New(h).Where(colGrp.Eq(1), colName.Eq("a")).Explain()); idx != "zz" {
			t.Fatalf("chose %q, want zz (covering beats name order)", idx)
		}
	})
	t.Run("unique breaks the covering tie", func(t *testing.T) {
		h, _, cleanup := openQueryDB(t, 10, rng,
			ci("aa", anyCols(colID), typed.ColumnIndexOpts[uint64, row]{}),
			ci("zz", anyCols(colID), typed.ColumnIndexOpts[uint64, row]{Unique: true}),
		)
		defer cleanup()
		if _, idx := planLeafOf(query.New(h).Where(colID.Eq(4)).Explain()); idx != "zz" {
			t.Fatalf("chose %q, want zz (unique beats name order)", idx)
		}
	})
	t.Run("name is the final tie-break", func(t *testing.T) {
		h, _, cleanup := openQueryDB(t, 10, rng,
			ci("bb", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
			ci("aa", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
		)
		defer cleanup()
		if _, idx := planLeafOf(query.New(h).Where(colGrp.Eq(1)).Explain()); idx != "aa" {
			t.Fatalf("chose %q, want aa (deterministic name order)", idx)
		}
	})
}

// Rule 7: a Where-partial index is never planner-eligible even
// when it is the ONLY structural match — the plan degrades to a
// scan and the results include rows the partial index excludes.
func TestPlannerPartialIndexExcluded(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("p", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{
			Where: func(_ uint64, v row) bool { return v.Grp%2 == 0 },
		}),
	)
	defer cleanup()
	_ = h.Put(1, row{Grp: 2})
	_ = h.Put(2, row{Grp: 3}) // excluded from the partial index's entries
	_ = h.Put(3, row{Grp: 3})

	q := query.New(h).Where(colGrp.Eq(3))
	if kind, idx := planLeafOf(q.Explain()); kind != "scan" || idx != "" {
		t.Fatalf("plan = %s over %q, want scan (partial index ineligible)", kind, idx)
	}
	var keys []uint64
	for k := range q.Keys() {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if q.Err() != nil || !slices.Equal(keys, []uint64{2, 3}) {
		t.Fatalf("keys = %v (err %v), want [2 3] — rows the partial index does not carry", keys, q.Err())
	}
}

// Inv-QB4 + the multi-column eligibility rule: a range over a
// multi column dedups its per-element expansion, and an index
// whose multi column the query does NOT consume is no access path
// at all — a row with an empty element slice has no entries there
// (typed-columns.md Inv-TC4), so choosing it would silently lose
// rows (Inv-QB1).
func TestQueryMultiColumnRangeDedup(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("t", anyCols(colTags), typed.ColumnIndexOpts[uint64, row]{}),
		ci("gt", anyCols(colGrp, colTags), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	_ = h.Put(1, row{Grp: 1, Tags: []string{"b", "c"}})
	_ = h.Put(2, row{Grp: 1, Tags: []string{"a"}})
	_ = h.Put(3, row{Grp: 2, Tags: []string{"d", "e", "f"}})
	_ = h.Put(4, row{Grp: 2}) // no tags: unindexed by t and gt

	q := query.New(h).Where(colTags.ContainsRange("a", "z"))
	if kind, idx := planLeafOf(q.Explain()); kind != "range" || idx != "t" {
		t.Fatalf("plan = %s over %q, want range over t", kind, idx)
	}
	var keys []uint64
	for k := range q.Keys() {
		keys = append(keys, k)
	}
	if q.Err() != nil {
		t.Fatalf("Err: %v", q.Err())
	}
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	if len(keys) != 3 || !slices.Equal(sorted, []uint64{1, 2, 3}) {
		t.Fatalf("ContainsRange yielded %v, want each of [1 2 3] exactly once", keys)
	}

	// grp.Eq(2) structurally matches gt's leading column, but gt
	// carries an unentailed multi column: row 4 (no tags) has no
	// gt entries, so gt is ineligible and the plan degrades to a
	// scan that returns BOTH grp=2 rows.
	q2 := query.New(h).Where(colGrp.Eq(2))
	if kind, idx := planLeafOf(q2.Explain()); kind != "scan" || idx != "" {
		t.Fatalf("plan = %s over %q, want scan (unentailed multi column makes gt row-partial)", kind, idx)
	}
	var keys2 []uint64
	for k := range q2.Keys() {
		keys2 = append(keys2, k)
	}
	slices.Sort(keys2)
	if q2.Err() != nil || !slices.Equal(keys2, []uint64{3, 4}) {
		t.Fatalf("grp=2 rows = %v (err %v), want [3 4] — the tagless row must not vanish", keys2, q2.Err())
	}

	// A RESIDUAL top-level Contains/ContainsRange term entails
	// element existence, so gt becomes eligible: grp bound
	// consumed, tags entailed residually, per-element expansion
	// deduped (row 3 carries three in-range elements).
	q3 := query.New(h).Where(colGrp.Gte(1), colTags.ContainsRange("a", "z"))
	if kind, idx := planLeafOf(q3.Explain()); kind != "range" || idx != "gt" {
		t.Fatalf("plan = %s over %q, want range over gt (residual entailment)", kind, idx)
	}
	var keys3 []uint64
	for k := range q3.Keys() {
		keys3 = append(keys3, k)
	}
	slices.Sort(keys3)
	if q3.Err() != nil || !slices.Equal(keys3, []uint64{1, 2, 3}) {
		t.Fatalf("entailed-multi range = %v (err %v), want [1 2 3] each once", keys3, q3.Err())
	}

	// An Or-nested Contains does NOT entail: a row matching the
	// OTHER disjunct may have no elements (row 4 matches via
	// name.Eq yet has no gt entries) — the plan must stay a scan.
	q4 := query.New(h).Where(colGrp.Eq(2), typed.Or(
		[]typed.Term[uint64, row]{colTags.Contains("d")},
		[]typed.Term[uint64, row]{colName.Eq("")},
	))
	if kind, idx := planLeafOf(q4.Explain()); kind != "scan" || idx != "" {
		t.Fatalf("plan = %s over %q, want scan (Or-nested Contains must not entail)", kind, idx)
	}
	var keys4 []uint64
	for k := range q4.Keys() {
		keys4 = append(keys4, k)
	}
	slices.Sort(keys4)
	if q4.Err() != nil || !slices.Equal(keys4, []uint64{3, 4}) {
		t.Fatalf("Or-disjunct rows = %v (err %v), want [3 4] — row 4 matches the other disjunct", keys4, q4.Err())
	}
}

// The value-level bound construction (query-builder.md
// §Byte-surface requirements): boundary-equal values, embedded
// 0x00 bytes (the successor trick's exact edge), all-0xFF prefix
// literals, empty prefixes, and EQ-group closure — each compared
// against an independently computed reference.
func TestQueryRangeBoundAnchors(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("n", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
		ci("gn", anyCols(colGrp, colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()

	names := []string{"", "a", "a\x00", "a\x00b", "ab", "b", "\xff", "\xff\xff"}
	rows := map[uint64]row{}
	var k uint64
	for _, grp := range []uint32{1, 2} {
		for _, nm := range names {
			k++
			rows[k] = row{Grp: grp, Name: nm}
			if err := h.Put(k, rows[k]); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
	}

	run := func(t *testing.T, q *query.Query[uint64, row], wantKind, wantIndex string, ref func(row) bool) {
		t.Helper()
		if kind, idx := planLeafOf(q.Explain()); kind != wantKind || idx != wantIndex {
			t.Fatalf("plan = %s over %q, want %s over %q", kind, idx, wantKind, wantIndex)
		}
		got := map[uint64]bool{}
		for key := range q.Keys() {
			if got[key] {
				t.Fatalf("duplicate key %d", key)
			}
			got[key] = true
		}
		if err := q.Err(); err != nil {
			t.Fatalf("Err: %v", err)
		}
		for key, v := range rows {
			if ref(v) != got[key] {
				t.Fatalf("key %d (grp=%d name=%q): got %v, want %v", key, v.Grp, v.Name, got[key], ref(v))
			}
		}
	}

	for _, tc := range []struct {
		name string
		term typed.Term[uint64, row]
		ref  func(row) bool
	}{
		{"Gt at a stored value includes its 0x00-successor", colName.Gt("a"),
			func(v row) bool { return v.Name > "a" }},
		{"Gte at a stored value", colName.Gte("a"),
			func(v row) bool { return v.Name >= "a" }},
		{"Lt at a 0x00-embedding value", colName.Lt("a\x00"),
			func(v row) bool { return v.Name < "a\x00" }},
		{"Lte at a 0x00-embedding value", colName.Lte("a\x00"),
			func(v row) bool { return v.Name <= "a\x00" }},
		{"Between half-open", colName.Between("a", "b"),
			func(v row) bool { return v.Name >= "a" && v.Name < "b" }},
		{"HasPrefix spans embedded 0x00", colName.HasPrefix("a"),
			func(v row) bool { return len(v.Name) >= 1 && v.Name[0] == 'a' }},
		{"HasPrefix all-0xFF has no successor", colName.HasPrefix("\xff"),
			func(v row) bool { return len(v.Name) >= 1 && v.Name[0] == '\xff' }},
		{"HasPrefix empty matches everything", colName.HasPrefix(""),
			func(row) bool { return true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run(t, query.New(h).Where(tc.term), "range", "n", tc.ref)
		})
	}

	// EQ-group closure: the same bounds inside a fixed grp group
	// must never leak the other group's rows.
	for _, tc := range []struct {
		name string
		term typed.Term[uint64, row]
		ref  func(string) bool
	}{
		{"group-closed Gt", colName.Gt("a"), func(n string) bool { return n > "a" }},
		{"group-closed Gte", colName.Gte(""), func(string) bool { return true }},
		{"group-closed all-0xFF HasPrefix", colName.HasPrefix("\xff"),
			func(n string) bool { return len(n) >= 1 && n[0] == '\xff' }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run(t, query.New(h).Where(colGrp.Eq(1), tc.term), "range", "gn",
				func(v row) bool { return v.Grp == 1 && tc.ref(v.Name) })
		})
	}
}

// Handle discipline: each execution obtains a FRESH byte handle,
// so two interleaved drains over the same typed handle never
// clobber each other's per-handle Err state (query-builder.md
// §Plan nodes).
func TestQueryFreshHandlePerExecution(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	for i := uint64(1); i <= 20; i++ {
		_ = h.Put(i, row{Grp: uint32(i % 2)})
	}

	q1 := query.New(h).Where(colGrp.Eq(0))
	q2 := query.New(h).Where(colGrp.Eq(1))
	next1, stop1 := iter.Pull2(q1.All())
	next2, stop2 := iter.Pull2(q2.All())
	defer stop1()
	defer stop2()
	var n1, n2 int
	for {
		_, _, ok1 := next1()
		_, _, ok2 := next2()
		if ok1 {
			n1++
		}
		if ok2 {
			n2++
		}
		if !ok1 && !ok2 {
			break
		}
	}
	if q1.Err() != nil || q2.Err() != nil {
		t.Fatalf("interleaved drains: err1=%v err2=%v, want nil", q1.Err(), q2.Err())
	}
	if n1 != 10 || n2 != 10 {
		t.Fatalf("interleaved drains: %d + %d rows, want 10 + 10", n1, n2)
	}
}

// synthName replicates the documented synthesized column-name
// grammar (typed-columns.md §Synthesized column-name grammar) so a
// byte-API declaration can address the same column a typed
// ColumnIndex lowered.
func synthName(formPrefix, userName, encoderID string) string {
	var buf [binary.MaxVarintLen64]byte
	out := []byte(formPrefix)
	n := binary.PutUvarint(buf[:], uint64(len(userName)))
	out = append(out, buf[:n]...)
	out = append(out, userName...)
	n = binary.PutUvarint(buf[:], uint64(len(encoderID)))
	out = append(out, buf[:n]...)
	out = append(out, encoderID...)
	return string(out)
}

// Inv-QB3's LIVE-declaration clause: a same-tx Rebuild that ADDS
// covering to the chosen index changes the meaning of entry value
// bytes from row bytes to a covering tuple. The executor must
// follow the live declaration (probed on its fresh handle), not
// the typed handle's open-time planner snapshot — a snapshot-led
// read decodes the covering tuple as row bytes.
func TestQueryIndexExecFollowsLiveCoveringShape(t *testing.T) {
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
	h, err := tks.Create(tx, ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := map[uint64]row{
		1: {Grp: 5, Name: "x", Tags: []string{"t"}},
		2: {Grp: 5, Name: "y"},
	}
	for k, v := range want {
		if err := h.Put(k, v); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	// Rebuild the same index name with a covering column added —
	// the sanctioned shape-changing recovery (indexing.md
	// §Rebuild). The byte decl addresses the typed columns by
	// their documented synthesized names.
	grpCol := synthName("gmdb/col/", "grp", "gmdb/be-uint32")
	nameCol := synthName("gmdb/col/", "name", "gmdb/string")
	err = tx.Indexes().Rebuild("rows", &gmdb.IndexDecl{
		Name:     "g",
		Columns:  []gmdb.IndexColumn{{Name: grpCol}},
		Covering: []gmdb.IndexCoveringColumn{{Name: nameCol}},
		Extract: func(kb, vb []byte) []gmdb.IndexEntry {
			v, err := rowCodec{}.Decode(vb)
			if err != nil {
				panic(err)
			}
			grp := []byte{byte(v.Grp >> 24), byte(v.Grp >> 16), byte(v.Grp >> 8), byte(v.Grp)}
			return []gmdb.IndexEntry{{Cols: [][]byte{grp}, Cover: [][]byte{[]byte(v.Name)}}}
		},
	})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	q := query.New(h).Where(colGrp.Eq(5))
	if kind, idx := planLeafOf(q.Explain()); kind != "seek" || idx != "g" {
		t.Fatalf("plan = %s over %q, want seek over g", kind, idx)
	}
	got := map[uint64]row{}
	for k, v := range q.All() {
		got[k] = v
	}
	if q.Err() != nil {
		t.Fatalf("Err: %v (covering tuple decoded as row bytes?)", q.Err())
	}
	if len(got) != len(want) {
		t.Fatalf("%d rows, want %d", len(got), len(want))
	}
	for k, w := range want {
		g := got[k]
		if g.Grp != w.Grp || g.Name != w.Name || !slices.Equal(g.Tags, w.Tags) {
			t.Fatalf("key %d: row %+v, want %+v (stale covering snapshot?)", k, g, w)
		}
	}
}

// A covering declaration's entry bytes are a covering tuple, not
// row bytes: the executor must take the back-lookup route and
// return true rows (the Inv-QB3 interpretation trap, at this
// stage's value-acquisition slice).
func TestQueryCoveringIndexBackLookup(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, tc := range []struct {
		name string
		opts typed.ColumnIndexOpts[uint64, row]
	}{
		{"projection covering", typed.ColumnIndexOpts[uint64, row]{Covering: []typed.AnySingleColumn[uint64, row]{colName}}},
		{"full-row CoverValue", typed.ColumnIndexOpts[uint64, row]{CoverValue: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, cleanup := openQueryDB(t, 0, rng, ci("c", anyCols(colGrp), tc.opts))
			defer cleanup()
			want := map[uint64]row{
				1: {Grp: 7, Name: "x", Tags: []string{"t1"}},
				2: {Grp: 7, Name: "y"},
			}
			for k, v := range want {
				if err := h.Put(k, v); err != nil {
					t.Fatalf("Put: %v", err)
				}
			}
			q := query.New(h).Where(colGrp.Eq(7))
			if kind, idx := planLeafOf(q.Explain()); kind != "seek" || idx != "c" {
				t.Fatalf("plan = %s over %q, want seek over c", kind, idx)
			}
			got := map[uint64]row{}
			for k, v := range q.All() {
				got[k] = v
			}
			if q.Err() != nil {
				t.Fatalf("Err: %v", q.Err())
			}
			if len(got) != len(want) {
				t.Fatalf("%d rows, want %d", len(got), len(want))
			}
			for k, w := range want {
				g := got[k]
				if g.Grp != w.Grp || g.Name != w.Name || !slices.Equal(g.Tags, w.Tags) {
					t.Fatalf("key %d: row %+v, want %+v (covering tuple misread as row bytes?)", k, g, w)
				}
			}
		})
	}
}
