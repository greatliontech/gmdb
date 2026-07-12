package query_test

import (
	"errors"
	"math/rand"
	"slices"
	"testing"

	"github.com/thegrumpylion/gmdb/query"
	"github.com/thegrumpylion/gmdb/typed"
)

// twoKeyspaces builds the same corpus twice: once with the given
// indexes, once bare — the cross-plan canonicality fixture
// (Inv-QB5: an ordered sequence is identical across plan choices).
func twoKeyspaces(t *testing.T, rows map[uint64]row, indexes ...typed.AnyIndex[uint64, row]) (indexed, bare *typed.KeyspaceHandle[uint64, row], cleanup func()) {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	h1, _, c1 := openQueryDB(t, 0, rng, indexes...)
	h2, _, c2 := openQueryDB(t, 0, rng)
	for k, v := range rows {
		if err := h1.Put(k, v); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := h2.Put(k, v); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	return h1, h2, func() { c1(); c2() }
}

func orderedKeys(t *testing.T, q *query.Query[uint64, row]) []uint64 {
	t.Helper()
	var keys []uint64
	for k := range q.Keys() {
		keys = append(keys, k)
	}
	if q.Err() != nil {
		t.Fatalf("Err: %v (plan %s)", q.Err(), q.Explain())
	}
	return keys
}

// Streaming ordered execution: an OrderBy the chosen index's entry
// order realizes runs with NO materializing node — ascending
// forward, descending via reverse iteration — and the sequence
// matches the scan-sorted twin exactly (Inv-QB5 cross-plan).
func TestQueryOrderByStreams(t *testing.T) {
	rows := map[uint64]row{
		1: {Grp: 1, Name: "b"},
		2: {Grp: 1, Name: "a"},
		3: {Grp: 1, Name: "a"}, // equal-key run: PK tie-break
		4: {Grp: 1, Name: "c"},
		5: {Grp: 2, Name: "a"},
	}
	h, bare, cleanup := twoKeyspaces(t, rows,
		ci("gn", anyCols(colGrp, colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()

	t.Run("ascending", func(t *testing.T) {
		q := query.New(h).Where(colGrp.Eq(1)).OrderBy(colName.Asc())
		if shape := orderedShape(q.Explain()); shape != "stream" {
			t.Fatalf("plan = %s, want streaming (no Sort/TopK node)", q.Explain())
		}
		got := orderedKeys(t, q)
		want := orderedKeys(t, query.New(bare).Where(colGrp.Eq(1)).OrderBy(colName.Asc()))
		if shape := orderedShape(query.New(bare).Where(colGrp.Eq(1)).OrderBy(colName.Asc()).Explain()); shape != "sort" {
			t.Fatalf("bare plan should sort, got %s", shape)
		}
		// asc name, asc PK ties: a(2,3), b(1), c(4).
		if !slices.Equal(got, []uint64{2, 3, 1, 4}) || !slices.Equal(got, want) {
			t.Fatalf("stream = %v, scan-sort = %v, want [2 3 1 4] both (Inv-QB5 cross-plan)", got, want)
		}
	})
	t.Run("descending streams via reverse", func(t *testing.T) {
		q := query.New(h).Where(colGrp.Eq(1)).OrderBy(colName.Desc())
		p := q.Explain()
		if shape := orderedShape(p); shape != "stream" {
			t.Fatalf("plan = %s, want streaming desc", p)
		}
		if _, _, _ = planLeafRoute(p); !planHasReverseLeaf(p) {
			t.Fatalf("plan = %s, want the Reverse leaf marker", p)
		}
		got := orderedKeys(t, q)
		want := orderedKeys(t, query.New(bare).Where(colGrp.Eq(1)).OrderBy(colName.Desc()))
		// desc name, DESC PK ties (Inv-QB5 direction clause): c(4), b(1), a(3,2).
		if !slices.Equal(got, []uint64{4, 1, 3, 2}) || !slices.Equal(got, want) {
			t.Fatalf("stream = %v, scan-sort = %v, want [4 1 3 2] both", got, want)
		}
	})
	t.Run("mixed directions materialize", func(t *testing.T) {
		// A three-column index where BOTH order keys match the
		// column run exactly — only the uniform-direction rule
		// stands between this and a wrong-order stream.
		rng := rand.New(rand.NewSource(1))
		h3, bare3, c3 := func() (*typed.KeyspaceHandle[uint64, row], *typed.KeyspaceHandle[uint64, row], func()) {
			a, _, ca := openQueryDB(t, 0, rng,
				ci("gni", anyCols(colGrp, colName, colID), typed.ColumnIndexOpts[uint64, row]{}))
			b, _, cb := openQueryDB(t, 0, rng)
			return a, b, func() { ca(); cb() }
		}()
		defer c3()
		for i := uint64(1); i <= 6; i++ {
			v := row{Grp: 1, Name: []string{"a", "a", "b"}[i%3]}
			_ = h3.Put(i, v)
			_ = bare3.Put(i, v)
		}
		q := query.New(h3).Where(colGrp.Eq(1)).OrderBy(colName.Asc(), colID.Desc())
		if shape := orderedShape(q.Explain()); shape != "sort" {
			t.Fatalf("plan = %s, want Sort (mixed directions cannot stream)", q.Explain())
		}
		got := orderedKeys(t, q)
		want := orderedKeys(t, query.New(bare3).Where(colGrp.Eq(1)).OrderBy(colName.Asc(), colID.Desc()))
		if !slices.Equal(got, want) {
			t.Fatalf("mixed-direction sequence = %v, scan twin = %v", got, want)
		}
	})
	t.Run("unmatched tail column materializes", func(t *testing.T) {
		// Ordering by grp alone over (grp, name): name would
		// tie-break before the PK — must not stream.
		q := query.New(h).OrderBy(colGrp.Asc())
		if shape := orderedShape(q.Explain()); shape == "stream" {
			t.Fatalf("plan = %s, want a materializing node (tail column breaks the PK tie-break)", q.Explain())
		}
	})
}

func planHasReverseLeaf(p query.Plan) bool {
	n := p.Root
	if pr, ok := n.(query.Project); ok {
		n = pr.Input
	}
	if rf, ok := n.(query.ResidualFilter); ok {
		n = rf.Input
	}
	switch l := n.(type) {
	case query.IndexPrefix:
		return l.Reverse
	case query.IndexRange:
		return l.Reverse
	}
	return false
}

// The equal-key Limit boundary (Inv-QB5 named anchor): a Limit
// cutting inside an equal-key run yields the identical sequence
// across executions AND across plan choices — the directional PK
// tie-break is what makes the cut deterministic.
func TestQueryOrderByEqualKeyLimitBoundary(t *testing.T) {
	rows := map[uint64]row{}
	for i := uint64(1); i <= 9; i++ {
		rows[i] = row{Grp: 1, Name: "same"}
	}
	h, bare, cleanup := twoKeyspaces(t, rows,
		ci("gn", anyCols(colGrp, colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()

	build := func(hh *typed.KeyspaceHandle[uint64, row], desc bool) *query.Query[uint64, row] {
		key := colName.Asc()
		if desc {
			key = colName.Desc()
		}
		return query.New(hh).Where(colGrp.Eq(1)).OrderBy(key).Limit(4)
	}
	for _, desc := range []bool{false, true} {
		want := []uint64{1, 2, 3, 4}
		if desc {
			want = []uint64{9, 8, 7, 6}
		}
		a := orderedKeys(t, build(h, desc))
		b := orderedKeys(t, build(bare, desc))
		c := orderedKeys(t, build(h, desc)) // repeat execution
		if !slices.Equal(a, want) || !slices.Equal(b, want) || !slices.Equal(c, want) {
			t.Fatalf("desc=%v: streamed=%v scan=%v repeat=%v, want %v (equal-key cut, directional PK ties)", desc, a, b, c, want)
		}
	}
	// The bare plan is a TopK (limit set, no index route).
	if shape := orderedShape(build(bare, false).Explain()); shape != "topk" {
		t.Fatalf("bare limited plan = %s, want TopK", build(bare, false).Explain())
	}
}

// TopK retains exactly limit+offset rows and its emission equals
// the full Sort's bounded emission.
func TestQueryOrderByTopKOffset(t *testing.T) {
	rows := map[uint64]row{}
	for i := uint64(1); i <= 20; i++ {
		rows[i] = row{Grp: uint32(i % 3), Name: string(rune('a' + i%7))}
	}
	h, bare, cleanup := twoKeyspaces(t, rows)
	defer cleanup()
	_ = bare
	q := query.New(h).OrderBy(colName.Asc()).Offset(3).Limit(5)
	p := q.Explain()
	tk, ok := p.Root.(query.TopK)
	if !ok || tk.K != 8 {
		t.Fatalf("plan = %s, want TopK(8)", p)
	}
	got := orderedKeys(t, q)
	// Reference: full sort then bounds.
	full := orderedKeys(t, query.New(h).OrderBy(colName.Asc()))
	want := full[3:8]
	if !slices.Equal(got, want) {
		t.Fatalf("topk+offset = %v, want %v (sorted[3:8])", got, want)
	}
}

// Inv-QB6: a set materialization budget fails the iteration with
// ErrQueryMaterializeLimit — never a silently truncated result —
// while pure streams are unaffected by any budget.
func TestQueryMaterializeBudget(t *testing.T) {
	rows := map[uint64]row{}
	for i := uint64(1); i <= 50; i++ {
		rows[i] = row{Grp: 1, Name: "nnnnnnnnnn"}
	}
	h, bare, cleanup := twoKeyspaces(t, rows,
		ci("gn", anyCols(colGrp, colName), typed.ColumnIndexOpts[uint64, row]{}),
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
		ci("n", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()

	t.Run("sort trips", func(t *testing.T) {
		q := query.New(bare).OrderBy(colName.Asc()).WithMaterializeLimit(64)
		n := 0
		for range q.All() {
			n++
		}
		if !errors.Is(q.Err(), query.ErrQueryMaterializeLimit) {
			t.Fatalf("rows=%d err=%v, want ErrQueryMaterializeLimit", n, q.Err())
		}
		if n != 0 {
			t.Fatalf("budget-tripped sort yielded %d rows — silent truncation forbidden", n)
		}
	})
	t.Run("topk trips", func(t *testing.T) {
		q := query.New(bare).OrderBy(colName.Asc()).Limit(30).WithMaterializeLimit(64)
		for range q.All() {
		}
		if !errors.Is(q.Err(), query.ErrQueryMaterializeLimit) {
			t.Fatalf("err=%v, want ErrQueryMaterializeLimit", q.Err())
		}
	})
	t.Run("streamed ordering ignores the budget", func(t *testing.T) {
		q := query.New(h).Where(colGrp.Eq(1)).OrderBy(colName.Asc()).WithMaterializeLimit(1)
		if shape := orderedShape(q.Explain()); shape != "stream" {
			t.Fatalf("plan = %s, want stream", q.Explain())
		}
		n := 0
		for range q.All() {
			n++
		}
		if q.Err() != nil || n != 50 {
			t.Fatalf("rows=%d err=%v, want 50 nil (pure streams buffer nothing)", n, q.Err())
		}
	})
	t.Run("hash union charges", func(t *testing.T) {
		q := query.New(h).Where(typed.Or(
			[]typed.Term[uint64, row]{colGrp.Eq(1)},
			[]typed.Term[uint64, row]{colName.Lt("z")},
		)).WithMaterializeLimit(16)
		if merge, _ := unionArm(t, q.Explain()); merge {
			t.Fatalf("want hash arm")
		}
		for range q.All() {
		}
		if !errors.Is(q.Err(), query.ErrQueryMaterializeLimit) {
			t.Fatalf("err=%v, want ErrQueryMaterializeLimit", q.Err())
		}
	})
	t.Run("intersect build charges", func(t *testing.T) {
		rng := rand.New(rand.NewSource(1))
		h2, _, cleanup2 := openQueryDB(t, 0, rng,
			ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
			ci("n", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
		)
		defer cleanup2()
		for i := uint64(1); i <= 50; i++ {
			_ = h2.Put(i, row{Grp: 1, Name: "nnnnnnnnnn"})
		}
		q := query.New(h2).Where(colGrp.Eq(1), colName.Eq("nnnnnnnnnn")).WithMaterializeLimit(16)
		if kind, _, _ := planLeafRoute(q.Explain()); kind != "intersect" {
			t.Fatalf("plan = %s, want intersect", q.Explain())
		}
		for range q.All() {
		}
		if !errors.Is(q.Err(), query.ErrQueryMaterializeLimit) {
			t.Fatalf("err=%v, want ErrQueryMaterializeLimit", q.Err())
		}
	})
	t.Run("sufficient budget matches unbounded", func(t *testing.T) {
		a := orderedKeys(t, query.New(bare).OrderBy(colName.Asc()).WithMaterializeLimit(1<<20))
		b := orderedKeys(t, query.New(bare).OrderBy(colName.Asc()))
		if !slices.Equal(a, b) {
			t.Fatalf("budgeted = %v, unbounded = %v", a, b)
		}
	})
}

// Count: the Inv-QB1 cardinality via the cheapest route — the
// value-free entry count when no residual work exists (proven by
// an armed row-decode poison), the match-counting drive otherwise,
// with the offset/limit formula applied arithmetically.
func TestQueryCount(t *testing.T) {
	h, armed, cleanup := openPoisonedDB(t,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
		ci("t", anyCols(colTags), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	for i := uint64(1); i <= 10; i++ {
		v := row{Grp: uint32(i % 2), Name: "x"}
		if i <= 4 {
			v.Tags = []string{"a", "b"} // multi expansion: dedup counting
		}
		if err := h.Put(i, v); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	*armed = true // the fast path must not decode rows

	n, err := query.New(h).Where(colGrp.Eq(1)).Count()
	if err != nil || n != 5 {
		t.Fatalf("Count = %d (err %v), want 5 — value-free path decoded rows?", n, err)
	}
	// Dedup counting over a multi range.
	n, err = query.New(h).Where(colTags.ContainsRange("a", "z")).Count()
	if err != nil || n != 4 {
		t.Fatalf("multi Count = %d (err %v), want 4 distinct rows", n, err)
	}
	// Offset/limit formula.
	for _, tc := range []struct {
		limit, offset int
		hasLim        bool
		want          uint64
	}{
		{0, 2, false, 3},
		{2, 0, true, 2},
		{2, 4, true, 1},
		{2, 9, true, 0},
	} {
		q := query.New(h).Where(colGrp.Eq(1)).Offset(tc.offset)
		if tc.hasLim {
			q.Limit(tc.limit)
		}
		n, err := q.Count()
		if err != nil || n != tc.want {
			t.Fatalf("limit=%d(%v) offset=%d: Count = %d (err %v), want %d", tc.limit, tc.hasLim, tc.offset, n, err, tc.want)
		}
	}
	// Residual work forces the counting drive (decodes rows —
	// disarm the poison first) and agrees with len(All()).
	*armed = false
	q := query.New(h).Where(colGrp.Eq(1)).Filter(func(k uint64, _ row) bool { return k > 3 })
	n, err = q.Count()
	all := 0
	for range q.All() {
		all++
	}
	if err != nil || q.Err() != nil || n != uint64(all) || n != 3 {
		t.Fatalf("filtered Count = %d (err %v), All = %d, want 3", n, err, all)
	}
}

// Ordered Rows: streamed index-only projections (asc and desc) and
// the materialized ordered projection agree with the row order.
func TestQueryRowsOrdered(t *testing.T) {
	rows := map[uint64]row{
		1: {Grp: 1, Name: "b"},
		2: {Grp: 1, Name: "a"},
		3: {Grp: 1, Name: "c"},
	}
	h, _, cleanup := twoKeyspaces(t, rows,
		ci("gn", anyCols(colGrp, colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()

	collect := func(q *query.Query[uint64, row]) (keys []uint64, names []string) {
		t.Helper()
		for k, p := range q.Rows() {
			nm, err := colName.From(p)
			if err != nil {
				t.Fatalf("From: %v", err)
			}
			keys = append(keys, k)
			names = append(names, nm)
		}
		if q.Err() != nil {
			t.Fatalf("Err: %v", q.Err())
		}
		return keys, names
	}

	// Streamed index-only (entry route), ascending.
	q := query.New(h).Where(colGrp.Eq(1)).OrderBy(colName.Asc()).Select(colName)
	if _, _, route := planLeafRoute(q.Explain()); route != query.ValuesEntry {
		t.Fatalf("plan = %s, want entry route", q.Explain())
	}
	if shape := orderedShape(q.Explain()); shape != "stream" {
		t.Fatalf("plan = %s, want stream", q.Explain())
	}
	keys, names := collect(q)
	if !slices.Equal(keys, []uint64{2, 1, 3}) || !slices.Equal(names, []string{"a", "b", "c"}) {
		t.Fatalf("ordered index-only rows = %v %v, want [2 1 3] [a b c]", keys, names)
	}
	// Descending.
	keys, names = collect(query.New(h).Where(colGrp.Eq(1)).OrderBy(colName.Desc()).Select(colName))
	if !slices.Equal(keys, []uint64{3, 1, 2}) || !slices.Equal(names, []string{"c", "b", "a"}) {
		t.Fatalf("desc rows = %v %v, want [3 1 2] [c b a]", keys, names)
	}
	// Materialized ordered projection (order key not realizable:
	// order by id over the gn index).
	q2 := query.New(h).Where(colGrp.Eq(1)).OrderBy(colID.Desc()).Select(colName)
	if shape := orderedShape(q2.Explain()); shape == "stream" {
		t.Fatalf("plan = %s, want materializing", q2.Explain())
	}
	keys, _ = collect(q2)
	if !slices.Equal(keys, []uint64{3, 2, 1}) {
		t.Fatalf("materialized ordered rows = %v, want [3 2 1]", keys)
	}
}

// A negative Offset skips nothing — normalized at the builder, so
// the TopK cap, the Count formula, and the bound stage all read a
// sane value (the raw value once panicked the heap and silently
// shrank ordered results).
func TestQueryNegativeOffset(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng)
	defer cleanup()
	for i := uint64(1); i <= 10; i++ {
		_ = h.Put(i, row{Grp: 1, Name: string(rune('a' + i))})
	}
	q := query.New(h).OrderBy(colName.Asc()).Limit(5).Offset(-3)
	keys := orderedKeys(t, q)
	if len(keys) != 5 {
		t.Fatalf("ordered negative-offset rows = %v, want 5 (silent TopK shrink?)", keys)
	}
	q2 := query.New(h).OrderBy(colName.Asc()).Limit(2).Offset(-2)
	keys = orderedKeys(t, q2) // once panicked on an empty heap
	if len(keys) != 2 {
		t.Fatalf("rows = %v, want 2", keys)
	}
	n, err := query.New(h).Offset(-3).Count()
	if err != nil || n != 10 {
		t.Fatalf("Count with negative offset = %d (err %v), want 10", n, err)
	}
}

// One budget spans ALL of an execution's buffering nodes: a
// hash-union feeding a Sort must trip on their SUM even when each
// node alone would fit.
func TestQueryBudgetSpansExecution(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
		ci("n", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	for i := uint64(1); i <= 40; i++ {
		_ = h.Put(i, row{Grp: 1, Name: "x"})
	}
	// Hash union (range branch) into a Sort: PK set ≈ 40*8 = 320
	// bytes, sort buffer ≈ 40*(1+8) = 360 bytes. A 500-byte budget
	// fits either node alone but not both.
	q := query.New(h).Where(typed.Or(
		[]typed.Term[uint64, row]{colGrp.Eq(1)},
		[]typed.Term[uint64, row]{colName.Lt("y")},
	)).OrderBy(colName.Asc()).WithMaterializeLimit(500)
	if merge, _ := unionArm(t, q.Explain()); merge {
		t.Fatalf("want hash arm")
	}
	for range q.All() {
	}
	if !errors.Is(q.Err(), query.ErrQueryMaterializeLimit) {
		t.Fatalf("err=%v, want ErrQueryMaterializeLimit on the SUM across nodes", q.Err())
	}
	// Either node alone fits a budget that covers it.
	q2 := query.New(h).Where(typed.Or(
		[]typed.Term[uint64, row]{colGrp.Eq(1)},
		[]typed.Term[uint64, row]{colName.Lt("y")},
	)).WithMaterializeLimit(500)
	n := 0
	for range q2.All() {
		n++
	}
	if q2.Err() != nil || n != 40 {
		t.Fatalf("union alone: rows=%d err=%v, want 40 nil", n, q2.Err())
	}
}

// Leaf-level multi-expansion dedup is hash dedup and charges the
// budget (Inv-QB6) — on All, Rows (index-only), and Count alike.
func TestQueryBudgetChargesLeafDedup(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("t", anyCols(colTags), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	for i := uint64(1); i <= 30; i++ {
		_ = h.Put(i, row{Tags: []string{"a", "b"}})
	}
	q := query.New(h).Where(colTags.ContainsRange("a", "z")).WithMaterializeLimit(16)
	for range q.All() {
	}
	if !errors.Is(q.Err(), query.ErrQueryMaterializeLimit) {
		t.Fatalf("All err=%v, want ErrQueryMaterializeLimit", q.Err())
	}
	if _, err := query.New(h).Where(colTags.ContainsRange("a", "z")).WithMaterializeLimit(16).Count(); !errors.Is(err, query.ErrQueryMaterializeLimit) {
		t.Fatalf("Count err=%v, want ErrQueryMaterializeLimit", err)
	}
}

// A zero-value OrderKey fails at iteration start, like zero Terms.
func TestQueryOrderByZeroKeyRejected(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 3, rng)
	defer cleanup()
	var zero typed.OrderKey[uint64, row]
	q := query.New(h).OrderBy(zero)
	for range q.All() {
		t.Fatal("zero OrderKey yielded")
	}
	if q.Err() == nil {
		t.Fatal("zero OrderKey: Err is nil")
	}
}
