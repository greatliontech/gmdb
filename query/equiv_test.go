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
)

func openQueryDB(t *testing.T, n int, rng *rand.Rand) (*typed.KeyspaceHandle[uint64, row], map[uint64]row, func()) {
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
	h, err := tks.Create(tx)
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
	switch rng.Intn(10) {
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

func TestQueryScanEquivalence(t *testing.T) {
	for seed := int64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			h, corpus, cleanup := openQueryDB(t, 60+rng.Intn(120), rng)
			defer cleanup()

			for round := 0; round < 10; round++ {
				nTerms := rng.Intn(3)
				var terms []refTerm
				for i := 0; i < nTerms; i++ {
					terms = append(terms, randTerm(rng, 0))
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

				got := map[uint64]row{}
				for k, v := range q.All() {
					got[k] = v
				}
				if err := q.Err(); err != nil {
					t.Fatalf("round %d: Err: %v", round, err)
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
					t.Fatalf("round %d: %d rows, want %d", round, len(got), len(want))
				}
				for k := range want {
					if _, ok := got[k]; !ok {
						t.Fatalf("round %d: missing key %d", round, k)
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
			}
		})
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
	rng := rand.New(rand.NewSource(2))
	h, corpus, cleanup := openQueryDB(t, 40, rng)
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
