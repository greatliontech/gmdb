package query_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thegrumpylion/gmdb"
	"github.com/thegrumpylion/gmdb/query"
	"github.com/thegrumpylion/gmdb/typed"
)

// The Inv-QB1 equivalence harness, scan-only stage: query results
// must equal an INDEPENDENT reference evaluation of the same
// terms over the same corpus (set comparison — no OrderBy exists
// yet, order is plan-defined). The generator grammar (schemas,
// corpora, term lists incl. Or nesting and opaque filters) is
// extended alongside every extension of the query surface;
// index-backed plans reuse this harness unchanged.

type row struct {
	Grp  uint32
	Name string
	Tags []string
}

type rowCodec struct{}

func (rowCodec) ID() string { return "test/row" }
func (rowCodec) AppendEncode(dst []byte, v row) ([]byte, error) {
	dst = append(dst, byte(v.Grp>>24), byte(v.Grp>>16), byte(v.Grp>>8), byte(v.Grp))
	dst = append(dst, byte(len(v.Name)))
	dst = append(dst, v.Name...)
	dst = append(dst, byte(len(v.Tags)))
	for _, t := range v.Tags {
		dst = append(dst, byte(len(t)))
		dst = append(dst, t...)
	}
	return dst, nil
}
func (rowCodec) Decode(src []byte) (row, error) {
	if len(src) < 5 {
		return row{}, fmt.Errorf("short row")
	}
	v := row{Grp: uint32(src[0])<<24 | uint32(src[1])<<16 | uint32(src[2])<<8 | uint32(src[3])}
	src = src[4:]
	nl := int(src[0])
	v.Name = string(src[1 : 1+nl])
	src = src[1+nl:]
	tn := int(src[0])
	src = src[1:]
	for i := 0; i < tn; i++ {
		l := int(src[0])
		v.Tags = append(v.Tags, string(src[1:1+l]))
		src = src[1+l:]
	}
	return v, nil
}

var (
	colGrp  = typed.NewColumn("grp", typed.Uint32Encoder{}, func(_ uint64, v row) uint32 { return v.Grp })
	colName = typed.NewColumn("name", typed.StringEncoder{}, func(_ uint64, v row) string { return v.Name })
	colTags = typed.NewMultiColumn("tag", typed.StringEncoder{}, func(_ uint64, v row) []string { return v.Tags })
	colID   = typed.NewColumn("id", typed.Uint64Encoder{}, func(k uint64, _ row) uint64 { return k })
)

func openQueryDB(t *testing.T, n int, rng *rand.Rand, indexes ...typed.AnyIndex[uint64, row]) (*typed.KeyspaceHandle[uint64, row], map[uint64]row, func()) {
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
	tks := typed.NewKeyspace[uint64, row]("rows", typed.Uint64Encoder{}, rowCodec{})
	h, err := tks.Create(tx, indexes...)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	names := []string{"alpha", "beta", "Alpha", "b", ""}
	tagPool := []string{"go", "db", "x", ""}
	corpus := make(map[uint64]row, n)
	for i := 0; i < n; i++ {
		v := row{Grp: uint32(rng.Intn(6)), Name: names[rng.Intn(len(names))]}
		for j := rng.Intn(3); j > 0; j-- {
			v.Tags = append(v.Tags, tagPool[rng.Intn(len(tagPool))])
		}
		k := uint64(i)
		corpus[k] = v
		if err := h.Put(k, v); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	return h, corpus, func() { _ = tx.Rollback(); _ = db.Close() }
}

// refTerm is the reference evaluator's own term shape — evaluated
// over decoded rows with byte-level comparison semantics,
// implemented independently of the builder's evaluation path.
type refTerm struct {
	eval func(k uint64, v row) bool
	term typed.Term[uint64, row]
}

func encU32(v uint32) []byte { b, _ := (typed.Uint32Encoder{}).AppendEncode(nil, v); return b }
func encStr(s string) []byte { b, _ := (typed.StringEncoder{}).AppendEncode(nil, s); return b }
func cmpU32(a, b uint32) int { return slices.Compare(encU32(a), encU32(b)) }
func cmpStr(a, b string) int { return slices.Compare(encStr(a), encStr(b)) }

func randTerm(rng *rand.Rand, depth int) refTerm {
	switch rng.Intn(12) {
	case 10:
		id := uint64(rng.Intn(200))
		return refTerm{term: colID.Eq(id), eval: func(k uint64, _ row) bool { return k == id }}
	case 11:
		nm := []string{"alpha", "beta", "Alpha", "b", ""}[rng.Intn(5)]
		return refTerm{term: colName.Eq(nm), eval: func(_ uint64, v row) bool { return v.Name == nm }}
	case 0:
		g := uint32(rng.Intn(6))
		return refTerm{term: colGrp.Eq(g), eval: func(_ uint64, v row) bool { return cmpU32(v.Grp, g) == 0 }}
	case 1:
		g := uint32(rng.Intn(6))
		return refTerm{term: colGrp.Lt(g), eval: func(_ uint64, v row) bool { return cmpU32(v.Grp, g) < 0 }}
	case 2:
		g := uint32(rng.Intn(6))
		return refTerm{term: colGrp.Gte(g), eval: func(_ uint64, v row) bool { return cmpU32(v.Grp, g) >= 0 }}
	case 3:
		lo, hi := uint32(rng.Intn(4)), uint32(rng.Intn(4)+2)
		return refTerm{term: colGrp.Between(lo, hi),
			eval: func(_ uint64, v row) bool { return cmpU32(v.Grp, lo) >= 0 && cmpU32(v.Grp, hi) < 0 }}
	case 4:
		p := []string{"a", "Al", "b", ""}[rng.Intn(4)]
		return refTerm{term: colName.HasPrefix(p),
			eval: func(_ uint64, v row) bool { return strings.HasPrefix(v.Name, p) }} // StringEncoder is identity: byte prefix == string prefix
	case 5:
		tag := []string{"go", "db", "x", ""}[rng.Intn(4)]
		return refTerm{term: colTags.Contains(tag),
			eval: func(_ uint64, v row) bool { return slices.Contains(v.Tags, tag) }}
	case 8:
		g := uint32(rng.Intn(6))
		return refTerm{term: colGrp.Lte(g), eval: func(_ uint64, v row) bool { return cmpU32(v.Grp, g) <= 0 }}
	case 9:
		g := uint32(rng.Intn(6))
		return refTerm{term: colGrp.Gt(g), eval: func(_ uint64, v row) bool { return cmpU32(v.Grp, g) > 0 }}
	case 6:
		lo, hi := "d", "y"
		return refTerm{term: colTags.ContainsRange(lo, hi),
			eval: func(_ uint64, v row) bool {
				for _, t := range v.Tags {
					if cmpStr(t, lo) >= 0 && cmpStr(t, hi) < 0 {
						return true
					}
				}
				return false
			}}
	default:
		if depth >= 2 {
			g := uint32(rng.Intn(6))
			return refTerm{term: colGrp.Eq(g), eval: func(_ uint64, v row) bool { return cmpU32(v.Grp, g) == 0 }}
		}
		// Or of two conjunction groups.
		g1a, g1b := randTerm(rng, depth+1), randTerm(rng, depth+1)
		g2 := randTerm(rng, depth+1)
		return refTerm{
			term: typed.Or([]typed.Term[uint64, row]{g1a.term, g1b.term}, []typed.Term[uint64, row]{g2.term}),
			eval: func(k uint64, v row) bool {
				return (g1a.eval(k, v) && g1b.eval(k, v)) || g2.eval(k, v)
			},
		}
	}
}

// randIndexes generates the schema arm of the grammar: 0–3
// ColumnIndexes over random subsets/orders of the declared
// columns, with unique (only when the injective id column is
// present), Where-partial (rule 7's exclusion arm), CoverValue,
// and covering mixes. Returns the declarations plus the set of
// partial index names — the planner must never choose those.
func randIndexes(rng *rand.Rand) (idxs []typed.AnyIndex[uint64, row], partial map[string]bool) {
	scalar := []typed.AnyColumn[uint64, row]{colGrp, colName, colID}
	all := []typed.AnyColumn[uint64, row]{colGrp, colName, colTags, colID}
	partial = map[string]bool{}
	n := 1 + rng.Intn(3)
	for i := 0; i < n; i++ {
		// Bias toward short scalar-led indexes: an index with an
		// unconsumed MultiColumn is never an access path (rule 2's
		// eligibility clause), so an all-arms-equal draw starves
		// the planner and the census below trips.
		pool := scalar
		if rng.Intn(3) == 0 {
			pool = all
		}
		perm := rng.Perm(len(pool))
		take := 1 + rng.Intn(2)
		if rng.Intn(4) == 0 {
			take = 3
		}
		var cols []typed.AnyColumn[uint64, row]
		hasID, hasMulti := false, false
		for _, p := range perm[:take] {
			cols = append(cols, pool[p])
			if pool[p] == typed.AnyColumn[uint64, row](colID) {
				hasID = true
			}
			if pool[p] == typed.AnyColumn[uint64, row](colTags) {
				hasMulti = true
			}
		}
		name := fmt.Sprintf("ix%d", i)
		opts := typed.ColumnIndexOpts[uint64, row]{}
		// Unique needs tuple injectivity: the id column makes the
		// tuple injective per row, but a multi column re-introduces
		// intra-row candidate-set duplicates (a row's tag list may
		// repeat an element), so unique stays multi-free.
		if hasID && !hasMulti && rng.Intn(2) == 0 {
			opts.Unique = true
		}
		switch rng.Intn(4) {
		case 0:
			opts.Where = func(_ uint64, v row) bool { return v.Grp%2 == 0 }
			partial[name] = true
		case 1:
			opts.CoverValue = true
		case 2:
			opts.Covering = []typed.AnySingleColumn[uint64, row]{colName}
		}
		idxs = append(idxs, typed.NewColumnIndex(name, cols, opts))
	}
	// The intersect arm: a deterministic pair of single-column
	// scalar indexes makes rule 5's trigger (two disjoint EQ seeks
	// on different indexes with no superset candidate) reachable
	// from grp.Eq + name.Eq draws — census-floored below.
	if rng.Intn(3) == 0 {
		idxs = append(idxs,
			typed.NewColumnIndex("sg", []typed.AnyColumn[uint64, row]{colGrp}, typed.ColumnIndexOpts[uint64, row]{}),
			typed.NewColumnIndex("sn", []typed.AnyColumn[uint64, row]{colName}, typed.ColumnIndexOpts[uint64, row]{}),
		)
	}
	return idxs, partial
}

// selArm pairs a selectable column with its reference verifier:
// the projected slot must decode to the same value the accessor
// computes from the corpus row.
type selArm struct {
	col    typed.AnySingleColumn[uint64, row]
	verify func(k uint64, v row, p typed.Projection) error
}

func selPool() []selArm {
	return []selArm{
		{colGrp, func(_ uint64, v row, p typed.Projection) error {
			g, err := colGrp.From(p)
			if err != nil {
				return fmt.Errorf("grp From: %w", err)
			}
			if g != v.Grp {
				return fmt.Errorf("grp slot = %d, want %d", g, v.Grp)
			}
			return nil
		}},
		{colName, func(_ uint64, v row, p typed.Projection) error {
			n, err := colName.From(p)
			if err != nil {
				return fmt.Errorf("name From: %w", err)
			}
			if n != v.Name {
				return fmt.Errorf("name slot = %q, want %q", n, v.Name)
			}
			return nil
		}},
		{colID, func(k uint64, _ row, p typed.Projection) error {
			id, err := colID.From(p)
			if err != nil {
				return fmt.Errorf("id From: %w", err)
			}
			if id != k {
				return fmt.Errorf("id slot = %d, want %d", id, k)
			}
			return nil
		}},
	}
}

// planLeafOf unwraps a plan to its leaf kind and index name.
func planLeafOf(p query.Plan) (kind, index string) {
	kind, index, _ = planLeafRoute(p)
	return kind, index
}

// planLeafRoute additionally reports the leaf's value route.
// Combiner roots report their kind with no single index/route.
func planLeafRoute(p query.Plan) (kind, index string, route query.ValueRoute) {
	n := p.Root
	if pr, ok := n.(query.Project); ok {
		n = pr.Input
	}
	if rf, ok := n.(query.ResidualFilter); ok {
		n = rf.Input
	}
	switch l := n.(type) {
	case query.Scan:
		return "scan", "", query.ValuesRowBytes
	case query.IndexSeek:
		return "seek", l.Index, l.Values
	case query.IndexPrefix:
		return "prefix", l.Index, l.Values
	case query.IndexRange:
		return "range", l.Index, l.Values
	case query.Union:
		return "union", "", query.ValuesRowBytes
	case query.Intersect:
		return "intersect", "", query.ValuesRowBytes
	}
	return "?", "", query.ValuesRowBytes
}

// The Inv-QB1 property live: for every generated schema (indexes
// included), corpus, and query, the planned execution equals the
// independent reference evaluation; Where-partial indexes are
// never chosen (rule 7) while results stay correct. The census
// asserted at the end keeps the property non-vacuous: every
// landed leaf kind must actually be sampled, and the rule-7
// assertion must run against generated partial indexes — a
// generator drift that starves the planner would otherwise leave
// this green while testing scan-vs-scan only (a silent coverage
// cap).
func TestQueryPlanScanEquivalence(t *testing.T) {
	census := map[string]int{}
	partialSeeds, seedsRun := 0, 0
	for seed := int64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			seedsRun++
			rng := rand.New(rand.NewSource(seed))
			idxs, partial := randIndexes(rng)
			if seed == 8 {
				// The intersect seed: exactly the two single-column
				// indexes, so the fixed final round's EQ pair has no
				// superset candidate and rule 5 fires (census floor).
				idxs = []typed.AnyIndex[uint64, row]{
					ci("sg", anyCols(colGrp), typed.ColumnIndexOpts[uint64, row]{}),
					ci("sn", anyCols(colName), typed.ColumnIndexOpts[uint64, row]{}),
				}
				partial = map[string]bool{}
			}
			if len(partial) > 0 {
				partialSeeds++
			}
			h, corpus, cleanup := openQueryDB(t, 60+rng.Intn(120), rng, idxs...)
			defer cleanup()

			for round := 0; round < 31; round++ {
				var terms []refTerm
				if round == 30 {
					// Fixed final round: the EQ pair every schema can
					// serve — on the intersect seed it exercises rule 5.
					terms = []refTerm{
						{term: colGrp.Eq(2), eval: func(_ uint64, v row) bool { return v.Grp == 2 }},
						{term: colName.Eq("beta"), eval: func(_ uint64, v row) bool { return v.Name == "beta" }},
					}
				} else {
					nTerms := rng.Intn(4)
					for i := 0; i < nTerms; i++ {
						terms = append(terms, randTerm(rng, 0))
					}
				}
				useFilter := rng.Intn(3) == 0

				q := query.New(h)
				for _, rt := range terms {
					q.Where(rt.term)
				}
				filter := func(k uint64, v row) bool { return k%3 != 0 }
				if useFilter {
					q.Filter(filter)
				}
				// The Select arm: a random subset of the scalar
				// columns, verified against the reference row via
				// Column.From on every projection.
				var sel []selArm
				if rng.Intn(2) == 0 {
					pool := selPool()
					for _, i := range rng.Perm(len(pool))[:1+rng.Intn(2)] {
						sel = append(sel, pool[i])
					}
					cols := make([]typed.AnySingleColumn[uint64, row], len(sel))
					for i, s := range sel {
						cols[i] = s.col
					}
					q.Select(cols...)
				}

				// Rule 7: a Where-partial index is never chosen,
				// no matter how well its columns match.
				leafKind, leafIdx, leafRoute := planLeafRoute(q.Explain())
				census[leafKind]++
				if len(sel) > 0 {
					census["values-"+leafRoute.String()]++
				}
				if partial[leafIdx] {
					t.Fatalf("round %d: planner chose partial index %q: %s", round, leafIdx, q.Explain())
				}

				got := map[uint64]row{}
				for k, v := range q.All() {
					// Distinct-by-PK (Inv-QB4): multi-column entry
					// expansion must never yield a row twice.
					if _, dup := got[k]; dup {
						t.Fatalf("round %d: duplicate key %d (plan %s)", round, k, q.Explain())
					}
					got[k] = v
				}
				if err := q.Err(); err != nil {
					t.Fatalf("round %d: Err: %v (plan %s)", round, err, q.Explain())
				}

				want := map[uint64]row{}
				for k, v := range corpus {
					ok := true
					for _, rt := range terms {
						if !rt.eval(k, v) {
							ok = false
							break
						}
					}
					if ok && useFilter && !filter(k, v) {
						ok = false
					}
					if ok {
						want[k] = v
					}
				}
				if len(got) != len(want) {
					t.Fatalf("round %d: %d rows, want %d (plan %s)", round, len(got), len(want), q.Explain())
				}
				for k := range want {
					if _, ok := got[k]; !ok {
						t.Fatalf("round %d: missing key %d (plan %s)", round, k, q.Explain())
					}
				}

				// Repeat-execution determinism (Inv-QB5, no-OrderBy
				// regime): the same query yields the identical
				// SEQUENCE on a second run.
				var seq1, seq2 []uint64
				for k := range q.All() {
					seq1 = append(seq1, k)
				}
				for k := range q.All() {
					seq2 = append(seq2, k)
				}
				if !slices.Equal(seq1, seq2) {
					t.Fatalf("round %d: repeat execution diverged", round)
				}

				// Rows(): the same matched key set, every selected
				// slot equal to the reference accessor's value
				// (Inv-QB1/Inv-QB3 across all value routes).
				if len(sel) > 0 {
					rowsGot := map[uint64]bool{}
					for k, p := range q.Rows() {
						if rowsGot[k] {
							t.Fatalf("round %d: Rows duplicate key %d (plan %s)", round, k, q.Explain())
						}
						rowsGot[k] = true
						for _, s := range sel {
							if err := s.verify(k, corpus[k], p); err != nil {
								t.Fatalf("round %d: key %d: %v (plan %s)", round, k, err, q.Explain())
							}
						}
					}
					if err := q.Err(); err != nil {
						t.Fatalf("round %d: Rows Err: %v (plan %s)", round, err, q.Explain())
					}
					if len(rowsGot) != len(want) {
						t.Fatalf("round %d: Rows %d rows, want %d (plan %s)", round, len(rowsGot), len(want), q.Explain())
					}
					for k := range want {
						if !rowsGot[k] {
							t.Fatalf("round %d: Rows missing key %d (plan %s)", round, k, q.Explain())
						}
					}
				}
			}
		})
	}
	// Non-vacuity census (see the doc comment): every landed leaf
	// kind sampled several times, and the rule-7 assertion backed
	// by generated partial indexes in more than one seed. Only
	// meaningful over the full seed set — `-run .../seed=N`
	// filtering must not trip it spuriously.
	if seedsRun < 8 {
		return
	}
	if census["seek"] < 3 || census["prefix"] < 3 || census["range"] < 3 || census["scan"] < 3 {
		t.Fatalf("leaf census too thin: %v — the generator no longer exercises the planner", census)
	}
	if partialSeeds < 2 {
		t.Fatalf("only %d seed(s) generated a partial index — rule 7's property arm is near-vacuous", partialSeeds)
	}
	// The Select arm must sample the index-only route (route 1) —
	// the covering-execution property is otherwise scan-projected
	// only.
	if census["values-entry"] < 1 {
		t.Fatalf("no index-only Rows plan sampled: %v", census)
	}
	// Combiners must be sampled: Or pushdown (rule 4) and the
	// intersect arm (rule 5, reachable via the deterministic
	// single-column index pair).
	if census["union"] < 1 {
		t.Fatalf("no Union plan sampled: %v", census)
	}
	if census["intersect"] < 1 {
		t.Fatalf("no Intersect plan sampled: %v", census)
	}
}

// Inv-QB2 anchor: a folding encoder makes Go-value comparison and
// byte comparison diverge — the builder must follow the BYTES.
func TestQueryEncodedComparisonAnchor(t *testing.T) {
	fold := typed.FuncEncoder[string]{
		EncodeFunc: func(dst []byte, v string) ([]byte, error) {
			return append(dst, strings.ToUpper(v)...), nil
		},
		DecodeFunc: func(src []byte) (string, error) { return string(src), nil },
		EncoderID:  "test/fold",
	}
	colFold := typed.NewColumn("fname", fold, func(_ uint64, v row) string { return v.Name })

	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng)
	defer cleanup()
	_ = h.Put(1, row{Name: "alpha"})
	_ = h.Put(2, row{Name: "Alpha"})
	_ = h.Put(3, row{Name: "beta"})

	var keys []uint64
	q := query.New(h).Where(colFold.Eq("ALPHA"))
	for k := range q.Keys() {
		keys = append(keys, k)
	}
	if err := q.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []uint64{1, 2}) {
		t.Fatalf("folding-encoder Eq matched %v, want [1 2] (byte semantics, not Go ==)", keys)
	}
}

// A term carrying a literal-encode error fails the query at
// iteration start — no rows, Err set (query-builder.md §Terms).
func TestQueryTermEncodeErrorFailsAtStart(t *testing.T) {
	sentinel := errors.New("boom")
	// Fails ONLY for the literal "x": row values encode fine, so a
	// builder that loses the carried error cannot fall back on an
	// eval-time failure — it would return wrong results with a nil
	// Err, which is exactly what fail-at-start prevents.
	bad := typed.FuncEncoder[string]{
		EncodeFunc: func(dst []byte, v string) ([]byte, error) {
			if v == "x" {
				return nil, sentinel
			}
			return append(dst, v...), nil
		},
		DecodeFunc: func(src []byte) (string, error) { return string(src), nil },
		EncoderID:  "test/bad",
	}
	colBad := typed.NewColumn("bad", bad, func(_ uint64, v row) string { return v.Name })

	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 5, rng)
	defer cleanup()

	q := query.New(h).Where(colBad.Eq("x"))
	n := 0
	for range q.All() {
		n++
	}
	if n != 0 || !errors.Is(q.Err(), sentinel) {
		t.Fatalf("rows=%d err=%v, want 0 rows + carried encode error", n, q.Err())
	}

	// The same error carried inside an Or disjunct also surfaces.
	q2 := query.New(h).Where(typed.Or([]typed.Term[uint64, row]{colBad.Eq("x")}))
	n2 := 0
	for range q2.All() {
		n2++
	}
	if n2 != 0 || !errors.Is(q2.Err(), sentinel) {
		t.Fatalf("Or-carried encode error: rows=%d err=%v, want 0 rows + carried sentinel at iteration START", n2, q2.Err())
	}

	// Fail-at-start precedes every short-circuit: a carried encode
	// error surfaces even when Limit(0) means no row would ever be
	// yielded.
	q3 := query.New(h).Where(colBad.Eq("x")).Limit(0)
	for range q3.All() {
	}
	if !errors.Is(q3.Err(), sentinel) {
		t.Fatalf("Limit(0) + carried encode error: err=%v, want the carried sentinel", q3.Err())
	}
}

// Offset/Limit apply to the final sequence with the documented
// cardinality (Inv-QB1 third regime).
func TestQueryLimitOffsetCardinality(t *testing.T) {
	// Both regimes of the same contract: the scan plan (no
	// indexes) and an IndexRange plan over the selective term's
	// column must satisfy the identical cardinality formula.
	for _, arm := range []struct {
		name string
		idxs []typed.AnyIndex[uint64, row]
	}{
		{"scan", nil},
		{"indexed", []typed.AnyIndex[uint64, row]{
			typed.NewColumnIndex("ixgrp", []typed.AnyColumn[uint64, row]{colGrp}, typed.ColumnIndexOpts[uint64, row]{}),
		}},
	} {
		t.Run(arm.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(2))
			h, corpus, cleanup := openQueryDB(t, 40, rng, arm.idxs...)
			defer cleanup()

			sel := colGrp.Lt(4) // selective: subset semantics observable
			matched := 0
			inRef := map[uint64]bool{}
			for k, v := range corpus {
				if v.Grp < 4 {
					matched++
					inRef[k] = true
				}
			}
			if arm.idxs != nil {
				if kind, idx := planLeafOf(query.New(h).Where(sel).Explain()); kind != "range" || idx != "ixgrp" {
					t.Fatalf("plan = %s %s, want range over ixgrp", kind, idx)
				}
			}
			for _, tc := range []struct{ limit, offset int }{
				{10, 0},
				{10, matched - 5},
				{10, matched + 60},
				{0, 7}, // offset only (limit unset below)
			} {
				q := query.New(h).Where(sel).Offset(tc.offset)
				if tc.limit > 0 {
					q.Limit(tc.limit)
				}
				n := 0
				for k := range q.Keys() {
					if !inRef[k] {
						t.Fatalf("limit=%d offset=%d: key %d outside the matched set", tc.limit, tc.offset, k)
					}
					n++
				}
				// Inv-QB1 third regime: max(0, min(limit, matched − offset)),
				// unset limit = ∞.
				want := matched - tc.offset
				if tc.limit > 0 && tc.limit < want {
					want = tc.limit
				}
				if want < 0 {
					want = 0
				}
				if n != want {
					t.Fatalf("limit=%d offset=%d: %d rows, want %d (matched=%d)", tc.limit, tc.offset, n, want, matched)
				}
			}
		})
	}
}

// Inv-QB2 anchor, float bits: NaN != NaN under Go comparison but
// its encoded bits are byte-equal — the builder follows the bytes.
func TestQueryNaNSafeFloatAnchor(t *testing.T) {
	bits := typed.FuncEncoder[float64]{
		EncodeFunc: func(dst []byte, v float64) ([]byte, error) {
			b := math.Float64bits(v)
			return append(dst, byte(b>>56), byte(b>>48), byte(b>>40), byte(b>>32),
				byte(b>>24), byte(b>>16), byte(b>>8), byte(b)), nil
		},
		DecodeFunc: func(src []byte) (float64, error) { return 0, nil },
		EncoderID:  "test/f64bits",
	}
	nanCol := typed.NewColumn("score", bits, func(k uint64, v row) float64 {
		if k == 1 {
			return math.NaN()
		}
		return 1.0
	})
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 0, rng)
	defer cleanup()
	_ = h.Put(1, row{Name: "nan"})
	_ = h.Put(2, row{Name: "one"})

	var keys []uint64
	q := query.New(h).Where(nanCol.Eq(math.NaN()))
	for k := range q.Keys() {
		keys = append(keys, k)
	}
	if err := q.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if !slices.Equal(keys, []uint64{1}) {
		t.Fatalf("Eq(NaN) matched %v, want [1] (byte semantics; Go NaN==NaN is false)", keys)
	}
}

// A mid-scan value-decode failure SURFACES via Err — a truncated
// result is never silently indistinguishable from a small one.
func TestQueryScanDecodeErrorSurfaces(t *testing.T) {
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
	poison := errors.New("decode poison")
	codec := typed.FuncEncoder[string]{
		EncodeFunc: func(dst []byte, v string) ([]byte, error) { return append(dst, v...), nil },
		DecodeFunc: func(src []byte) (string, error) {
			if string(src) == "BAD" {
				return "", poison
			}
			return string(src), nil
		},
		EncoderID: "test/poison",
	}
	tks := typed.NewKeyspace[uint64, string]("p", typed.Uint64Encoder{}, codec)
	h, err := tks.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for k, v := range map[uint64]string{1: "a", 2: "BAD", 3: "c"} {
		if err := h.Put(k, v); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	q := query.New(h)
	n := 0
	for range q.All() {
		n++
	}
	if err := q.Err(); !errors.Is(err, poison) {
		t.Fatalf("mid-scan decode failure: rows=%d Err=%v, want the decode error surfaced", n, err)
	}

	// A limit-complete result ends the scan at the cap: rows past
	// it are outside the observable sequence (Inv-QB1's cardinality
	// formula), so their decode failures must NOT surface — a
	// correct, complete result would otherwise be discarded by any
	// caller honoring Err.
	q = query.New(h).Limit(1)
	var got []uint64
	for k := range q.All() {
		got = append(got, k)
	}
	if q.Err() != nil || len(got) != 1 || got[0] != 1 {
		t.Fatalf("Limit(1) over {1:ok, 2:poison, 3:ok}: rows=%v Err=%v, want [1] with nil Err", got, q.Err())
	}
	// A limit NOT yet reached when the poison row arrives still
	// surfaces the failure — the truncation stays observable.
	q = query.New(h).Limit(2)
	for range q.All() {
	}
	if err := q.Err(); !errors.Is(err, poison) {
		t.Fatalf("Limit(2) reaches the poison row: Err=%v, want the decode error surfaced", err)
	}
	// Limit(0) is an empty observable sequence: no rows, no error.
	q = query.New(h).Limit(0)
	for range q.All() {
		t.Fatal("Limit(0) yielded a row")
	}
	if q.Err() != nil {
		t.Fatalf("Limit(0): Err=%v, want nil", q.Err())
	}
}

// A zero-value Term or nil Filter fails the query at iteration
// start with a diagnosable error, never an anonymous panic.
func TestQueryZeroValuePredicatesRejected(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	h, _, cleanup := openQueryDB(t, 3, rng)
	defer cleanup()

	var zero typed.Term[uint64, row]
	q := query.New(h).Where(zero)
	for range q.All() {
	}
	if q.Err() == nil {
		t.Fatal("zero-value Term: Err is nil, want a construction error")
	}
	q2 := query.New(h).Filter(nil)
	for range q2.All() {
	}
	if q2.Err() == nil {
		t.Fatal("nil Filter: Err is nil, want a construction error")
	}
}
