package query_test

import (
	"context"
	"math/rand"
	"path/filepath"
	"slices"
	"testing"

	"github.com/greatliontech/gmdb"
	"github.com/greatliontech/gmdb/query"
	"github.com/greatliontech/gmdb/typed"
)

// unionArm unwraps a Union root and reports its arm.
func unionArm(t *testing.T, p query.Plan) (merge bool, branches int) {
	t.Helper()
	n := p.Root
	if pr, ok := n.(query.Project); ok {
		n = pr.Input
	}
	if so, ok := n.(query.Sort); ok {
		n = so.Input
	}
	if tk, ok := n.(query.TopK); ok {
		n = tk.Input
	}
	if rf, ok := n.(query.ResidualFilter); ok {
		n = rf.Input
	}
	u, ok := n.(query.Union)
	if !ok {
		t.Fatalf("plan = %s, want a Union root", p)
	}
	return u.Merge, len(u.Branches)
}

// Inv-QB4's Or-branch overlap anchor (testing contract): a row
// matching BOTH branches yields once — on the streaming merge arm
// (all seek branches) and on the hash arm alike.
func TestQueryUnionOverlapDedup(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
		ci("n", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	_ = h.Put(1, row{Grp: 1, Name: "x"}) // matches both branches
	_ = h.Put(2, row{Grp: 1, Name: "y"}) // grp branch only
	_ = h.Put(3, row{Grp: 2, Name: "x"}) // name branch only
	_ = h.Put(4, row{Grp: 3, Name: "z"}) // neither

	t.Run("merge arm", func(t *testing.T) {
		q := query.New(h).Where(typed.Or(
			[]typed.Term[uint64, row]{colGrp.Eq(1)},
			[]typed.Term[uint64, row]{colName.Eq("x")},
		))
		if merge, n := unionArm(t, q.Explain()); !merge || n != 2 {
			t.Fatalf("plan = %s, want merge Union of 2 seeks", q.Explain())
		}
		var keys []uint64
		for k := range q.Keys() {
			keys = append(keys, k)
		}
		if q.Err() != nil || !slices.Equal(keys, []uint64{1, 2, 3}) {
			t.Fatalf("keys = %v (err %v), want [1 2 3] in PK order, overlap row once", keys, q.Err())
		}
	})
	t.Run("hash arm", func(t *testing.T) {
		// The name branch is a range (Lt) — merge unsound, hash arm.
		q := query.New(h).Where(typed.Or(
			[]typed.Term[uint64, row]{colGrp.Eq(1)},
			[]typed.Term[uint64, row]{colName.Lt("y")},
		))
		if merge, n := unionArm(t, q.Explain()); merge || n != 2 {
			t.Fatalf("plan = %s, want hash Union of 2 branches", q.Explain())
		}
		got := map[uint64]int{}
		for k := range q.Keys() {
			got[k]++
		}
		if q.Err() != nil {
			t.Fatalf("Err: %v", q.Err())
		}
		for k, c := range got {
			if c != 1 {
				t.Fatalf("key %d yielded %d times", k, c)
			}
		}
		var keys []uint64
		for k := range got {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []uint64{1, 2, 3}) {
			t.Fatalf("keys = %v, want [1 2 3]", keys)
		}
	})
}

// Branch residuals: a group term the branch leaf does not consume
// evaluates on that branch's rows ONLY — never leaks onto the
// other branch.
func TestQueryUnionBranchResiduals(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
		ci("n", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	_ = h.Put(1, row{Grp: 1, Name: "ax"})
	_ = h.Put(2, row{Grp: 1, Name: "bx"}) // fails group 1's name residual
	_ = h.Put(3, row{Grp: 2, Name: "zz"}) // matches group 2 (name branch)

	// Merge arm: both groups plan as seeks; group 1's HasPrefix is
	// its branch residual.
	q := query.New(h).Where(typed.Or(
		[]typed.Term[uint64, row]{colGrp.Eq(1), colName.HasPrefix("a")},
		[]typed.Term[uint64, row]{colName.Eq("zz")},
	))
	if merge, _ := unionArm(t, q.Explain()); !merge {
		t.Fatalf("plan = %s, want the merge arm", q.Explain())
	}
	var keys []uint64
	for k := range q.Keys() {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if q.Err() != nil || !slices.Equal(keys, []uint64{1, 3}) {
		t.Fatalf("merge arm keys = %v (err %v), want [1 3] — row 2 fails its group's residual", keys, q.Err())
	}

	// Hash arm: group 2 becomes a range, so the union hashes;
	// group 1's residual must still bind to its own branch.
	q2 := query.New(h).Where(typed.Or(
		[]typed.Term[uint64, row]{colGrp.Eq(1), colName.HasPrefix("a")},
		[]typed.Term[uint64, row]{colName.Gte("zz")},
	))
	if merge, _ := unionArm(t, q2.Explain()); merge {
		t.Fatalf("plan = %s, want the hash arm", q2.Explain())
	}
	keys = nil
	for k := range q2.Keys() {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if q2.Err() != nil || !slices.Equal(keys, []uint64{1, 3}) {
		t.Fatalf("hash arm keys = %v (err %v), want [1 3] — row 2 fails its group's residual", keys, q2.Err())
	}
}

// Rule 5: two disjoint EQ-shaped seeks on different indexes plan
// as Intersect when together they consume more than any single
// candidate; an index consuming both terms suppresses the upgrade.
func TestQueryIntersect(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
		ci("n", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	_ = h.Put(1, row{Grp: 1, Name: "x"})
	_ = h.Put(2, row{Grp: 1, Name: "y"})
	_ = h.Put(3, row{Grp: 2, Name: "x"})

	q := query.New(h).Where(colGrp.Eq(1), colName.Eq("x"))
	if kind, _, _ := planLeafRoute(q.Explain()); kind != "intersect" {
		t.Fatalf("plan = %s, want Intersect", q.Explain())
	}
	var keys []uint64
	for k := range q.Keys() {
		keys = append(keys, k)
	}
	if q.Err() != nil || !slices.Equal(keys, []uint64{1}) {
		t.Fatalf("keys = %v (err %v), want [1]", keys, q.Err())
	}

	// Residual + filter on the intersection's rows.
	q2 := query.New(h).Where(colGrp.Eq(1), colName.Eq("x")).
		Filter(func(k uint64, _ row) bool { return k != 1 })
	n := 0
	for range q2.Keys() {
		n++
	}
	if q2.Err() != nil || n != 0 {
		t.Fatalf("filtered intersect: %d rows (err %v), want 0", n, q2.Err())
	}
}

// The Intersect's OUTER residual terms narrow the intersection —
// an unconsumed term must never be dropped (Inv-QB1/Inv-QB2), and
// the probe's PK order is preserved across multiple surviving
// rows; Rows() over an Intersect projects route-3.
func TestQueryIntersectResidualAndOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
		ci("n", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	_ = h.Put(1, row{Grp: 1, Name: "x", Tags: []string{"go"}})
	_ = h.Put(2, row{Grp: 1, Name: "x"}) // fails the residual Contains
	_ = h.Put(3, row{Grp: 1, Name: "x", Tags: []string{"go"}})
	_ = h.Put(4, row{Grp: 2, Name: "x", Tags: []string{"go"}})

	q := query.New(h).Where(colGrp.Eq(1), colName.Eq("x"), colTags.Contains("go"))
	if kind, _, _ := planLeafRoute(q.Explain()); kind != "intersect" {
		t.Fatalf("plan = %s, want Intersect", q.Explain())
	}
	var keys []uint64
	for k := range q.Keys() {
		keys = append(keys, k)
	}
	// Probe order preserved: the probe seek yields PK-ascending.
	if q.Err() != nil || !slices.Equal(keys, []uint64{1, 3}) {
		t.Fatalf("keys = %v (err %v), want [1 3] in probe order — residual dropped?", keys, q.Err())
	}

	q2 := query.New(h).Where(colGrp.Eq(1), colName.Eq("x"), colTags.Contains("go")).Select(colName)
	got := map[uint64]string{}
	for k, p := range q2.Rows() {
		nm, err := colName.From(p)
		if err != nil {
			t.Fatalf("From: %v", err)
		}
		got[k] = nm
	}
	if q2.Err() != nil || len(got) != 2 || got[1] != "x" || got[3] != "x" {
		t.Fatalf("Rows over intersect = %v (err %v), want {1:x 3:x}", got, q2.Err())
	}
}

// Rule 5's disjointness: two candidates consuming the SAME term
// never pair — an intersect of two seeks over one term is the
// term itself, never an upgrade.
func TestQueryIntersectRequiresDisjointTerms(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("n1", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
		ci("n2", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	_ = h.Put(1, row{Name: "x"})
	q := query.New(h).Where(colName.Eq("x"))
	if kind, idx, _ := planLeafRoute(q.Explain()); kind != "seek" || idx != "n1" {
		t.Fatalf("plan = %s, want seek over n1 (same term must not intersect with itself)", q.Explain())
	}
}

func TestQueryIntersectSuppressedByCoveringIndex(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
		ci("n", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
		ci("gn", anyCols(colGrp, colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	_ = h.Put(1, row{Grp: 1, Name: "x"})
	q := query.New(h).Where(colGrp.Eq(1), colName.Eq("x"))
	if kind, idx, _ := planLeafRoute(q.Explain()); kind != "seek" || idx != "gn" {
		t.Fatalf("plan = %s, want seek over gn (single index consumes both)", q.Explain())
	}
}

// Rows over a Union: projections serve row-materialized (route 3)
// with correct slots and PK dedup.
func TestQueryRowsOverUnion(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
		ci("n", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	_ = h.Put(1, row{Grp: 1, Name: "x"})
	_ = h.Put(2, row{Grp: 2, Name: "x"})

	q := query.New(h).Where(typed.Or(
		[]typed.Term[uint64, row]{colGrp.Eq(1)},
		[]typed.Term[uint64, row]{colName.Eq("x")},
	)).Select(colName)
	got := map[uint64]string{}
	for k, p := range q.Rows() {
		nm, err := colName.From(p)
		if err != nil {
			t.Fatalf("From: %v", err)
		}
		if _, dup := got[k]; dup {
			t.Fatalf("duplicate key %d", k)
		}
		got[k] = nm
	}
	if q.Err() != nil || len(got) != 2 || got[1] != "x" || got[2] != "x" {
		t.Fatalf("rows = %v (err %v), want {1:x 2:x}", got, q.Err())
	}
}

// A limit-complete Union stops mid-merge without error (the
// consumer-break path through the pull iterators).
func TestQueryUnionLimitStopsClean(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
		ci("n", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	for i := uint64(1); i <= 20; i++ {
		_ = h.Put(i, row{Grp: uint32(i % 2), Name: "x"})
	}
	q := query.New(h).Where(typed.Or(
		[]typed.Term[uint64, row]{colGrp.Eq(0)},
		[]typed.Term[uint64, row]{colName.Eq("x")},
	)).Limit(3)
	seen := map[uint64]bool{}
	for k := range q.Keys() {
		if seen[k] {
			t.Fatalf("duplicate key %d", k)
		}
		seen[k] = true
	}
	if q.Err() != nil || len(seen) != 3 {
		t.Fatalf("rows=%d err=%v, want 3 nil", len(seen), q.Err())
	}
}

// The hash arm stops cleanly at a limit-complete result and on a
// consumer break — the early-stop must propagate across the
// sequential branch drains, never over-yield.
func TestQueryUnionHashArmLimitStopsClean(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
		ci("n", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
	)
	defer cleanup()
	for i := uint64(1); i <= 10; i++ {
		_ = h.Put(i, row{Grp: uint32(i % 2), Name: "x"})
	}
	// The name branch is a range — hash arm.
	q := query.New(h).Where(typed.Or(
		[]typed.Term[uint64, row]{colGrp.Eq(0)},
		[]typed.Term[uint64, row]{colName.Lt("y")},
	)).Limit(2)
	if merge, _ := unionArm(t, q.Explain()); merge {
		t.Fatalf("plan = %s, want the hash arm", q.Explain())
	}
	n := 0
	for range q.Keys() {
		n++
	}
	if q.Err() != nil || n != 2 {
		t.Fatalf("rows=%d err=%v, want exactly 2 (limit-complete)", n, q.Err())
	}
	// Consumer break mid-drain.
	q2 := query.New(h).Where(typed.Or(
		[]typed.Term[uint64, row]{colGrp.Eq(0)},
		[]typed.Term[uint64, row]{colName.Lt("y")},
	))
	for range q2.Keys() {
		break
	}
	if q2.Err() != nil {
		t.Fatalf("consumer break: err=%v, want nil", q2.Err())
	}
}

// A same-tx Rebuild reordering ONE branch's index degrades the
// whole Union to a scan with correct results (Inv-QB1 under every
// live shape).
func TestQueryUnionFallsBackOnLiveTupleChange(t *testing.T) {
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
	h, err := tks.Create(tx,
		ci("g", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
		ci("nn", anyCols(colName, colGrp), typed.ColumnIndexOpts[uint64, row]{}),
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_ = h.Put(1, row{Grp: 1, Name: "x"})
	_ = h.Put(2, row{Grp: 2, Name: "x"})
	grpCol := synthName("gmdb/col/", "grp", "gmdb/be-uint32")
	nameCol := synthName("gmdb/col/", "name", "gmdb/string")
	// Reverse nn's tuple: (name, grp) → (grp, name).
	err = tx.Indexes().Rebuild("rows", &gmdb.IndexDecl{
		Name:    "nn",
		Columns: []gmdb.IndexColumn{{Name: grpCol}, {Name: nameCol}},
		Extract: func(kb, vb []byte) []gmdb.IndexEntry {
			v, err := rowCodec{}.Decode(vb)
			if err != nil {
				panic(err)
			}
			grp := []byte{byte(v.Grp >> 24), byte(v.Grp >> 16), byte(v.Grp >> 8), byte(v.Grp)}
			return []gmdb.IndexEntry{{Cols: [][]byte{grp, []byte(v.Name)}}}
		},
	})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	q := query.New(h).Where(typed.Or(
		[]typed.Term[uint64, row]{colGrp.Eq(1)},
		[]typed.Term[uint64, row]{colName.Eq("x")}, // nn branch: live tuple changed
	))
	var keys []uint64
	for k := range q.Keys() {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if q.Err() != nil || !slices.Equal(keys, []uint64{1, 2}) {
		t.Fatalf("keys = %v (err %v), want [1 2] via the scan fallback", keys, q.Err())
	}
}
