package gmdb

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"testing"
)

// buildReverseCorpus creates an indexed keyspace with n rows over a
// two-column non-unique index ("grp", "sub") whose group column
// takes few distinct values (dup-heavy — reverse must get PK order
// within equal-key runs right) and, when covering is set, carries
// the row value as a covering column.
func buildReverseCorpus(t *testing.T, rng *rand.Rand, n int, covering bool) (*Tx, *IndexHandle, [2]byte, func()) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	decl := &IndexDecl{
		Name:    "byGrpSub",
		Columns: []IndexColumn{{Name: "grp"}, {Name: "sub"}},
		Extract: func(key, value []byte) []IndexEntry {
			// grp = first byte of value, sub = second byte.
			e := IndexEntry{Cols: [][]byte{{value[0]}, {value[1]}}}
			if covering {
				e.Cover = [][]byte{value}
			}
			return []IndexEntry{e}
		},
	}
	if covering {
		decl.Covering = []IndexCoveringColumn{{Name: "val"}}
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		db.Close()
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("rows", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	var sample [2]byte
	for i := 0; i < n; i++ {
		key := fmt.Appendf(nil, "k%04d", i)
		val := []byte{byte(rng.Intn(4)), byte(rng.Intn(6)), byte(i)}
		if err := ks.Put(key, val); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if i == 0 {
			sample = [2]byte{val[0], val[1]}
		}
	}
	idx, err := ks.Index("byGrpSub")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	return tx, idx, sample, func() { _ = tx.Rollback(); _ = db.Close() }
}

func collectSeq2(t *testing.T, idx *IndexHandle, seq func(func([]byte, []byte) bool)) []string {
	t.Helper()
	var out []string
	for k, v := range seq {
		out = append(out, fmt.Sprintf("%x|%x", k, v))
	}
	if err := idx.Err(); err != nil {
		t.Fatalf("Err after iteration: %v", err)
	}
	return out
}

func collectSeq1(t *testing.T, idx *IndexHandle, seq func(func([]byte) bool)) []string {
	t.Helper()
	var out []string
	for k := range seq {
		out = append(out, fmt.Sprintf("%x", k))
	}
	if err := idx.Err(); err != nil {
		t.Fatalf("Err after iteration: %v", err)
	}
	return out
}

// Reverse yields the element-wise reversal of the forward sequence
// over the same snapshot, on every iteration surface and bound
// shape (indexing.md §Lookup API). Randomized corpora, dup-heavy
// group columns, covering and back-lookup variants.
func TestIndexIterationReverseIsExactReversal(t *testing.T) {
	for seed := int64(1); seed <= 6; seed++ {
		for _, covering := range []bool{false, true} {
			t.Run(fmt.Sprintf("seed=%d/covering=%v", seed, covering), func(t *testing.T) {
				rng := rand.New(rand.NewSource(seed))
				n := 20 + rng.Intn(120)
				_, idx, sample, cleanup := buildReverseCorpus(t, rng, n, covering)
				defer cleanup()

				// Non-vacuity floor: every row emits exactly one
				// entry, so the zero-column Prefix scan must see all
				// n — and the reversal checks below cannot all pass
				// on empty sequences.
				if got := len(collectSeq2(t, idx, idx.Prefix(nil))); got != n {
					t.Fatalf("zero-col Prefix yielded %d entries, want n=%d", got, n)
				}

				check2 := func(what string, fwd, rev []string) {
					t.Helper()
					slices.Reverse(rev)
					if !slices.Equal(fwd, rev) {
						t.Fatalf("%s: reverse is not the exact reversal\nfwd=%v\nrev(reversed)=%v", what, fwd, rev)
					}
				}

				// Lookup on a dup-heavy exact tuple, taken from a real
				// row so the group is provably non-empty.
				g, u := []byte{sample[0]}, []byte{sample[1]}
				fwdGroup := collectSeq2(t, idx, idx.Lookup([][]byte{g, u}))
				if len(fwdGroup) == 0 {
					t.Fatal("sampled Lookup group empty — corpus sampling broken")
				}
				check2("Lookup", fwdGroup,
					collectSeq2(t, idx, idx.Lookup([][]byte{g, u}, Reverse())))

				// LookupKeys on the same tuple.
				fk := collectSeq1(t, idx, idx.LookupKeys([][]byte{g, u}))
				rk := collectSeq1(t, idx, idx.LookupKeys([][]byte{g, u}, Reverse()))
				slices.Reverse(rk)
				if !slices.Equal(fk, rk) {
					t.Fatalf("LookupKeys: reverse is not the exact reversal")
				}

				// Prefix: one leading column, and the zero-column full scan.
				check2("Prefix(g)",
					collectSeq2(t, idx, idx.Prefix([][]byte{g})),
					collectSeq2(t, idx, idx.Prefix([][]byte{g}, Reverse())))
				check2("Prefix()",
					collectSeq2(t, idx, idx.Prefix(nil)),
					collectSeq2(t, idx, idx.Prefix(nil, Reverse())))

				// Range: open/open, half-open both sides, closed, empty.
				bounds := [][2][][]byte{
					{nil, nil},
					{[][]byte{{1}}, nil},
					{nil, [][]byte{{2}}},
					{[][]byte{{1}}, [][]byte{{3}}},
					{[][]byte{{2}, {3}}, [][]byte{{3}, {1}}},
					{[][]byte{{3}}, [][]byte{{3}}}, // empty range
				}
				for i, b := range bounds {
					check2(fmt.Sprintf("Range#%d", i),
						collectSeq2(t, idx, idx.Range(b[0], b[1])),
						collectSeq2(t, idx, idx.Range(b[0], b[1], Reverse())))
				}
			})
		}
	}
}

// Reverse on a unique index's single-row surfaces is a no-op, and
// an empty index yields nothing in either direction.
func TestIndexReverseUniqueAndEmptyAnchors(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	decl := &IndexDecl{
		Name: "u", Unique: true,
		Columns: []IndexColumn{{Name: "c"}},
		Extract: func(key, value []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{value}}}
		},
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("rows", decl)
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	idx, err := ks.Index("u")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	// Empty, both directions, all surfaces.
	if got := collectSeq2(t, idx, idx.Range(nil, nil, Reverse())); len(got) != 0 {
		t.Fatalf("empty index reverse Range yielded %v", got)
	}
	if got := collectSeq2(t, idx, idx.Prefix(nil, Reverse())); len(got) != 0 {
		t.Fatalf("empty index reverse Prefix yielded %v", got)
	}

	if err := ks.Put([]byte("k"), []byte("c1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fwd := collectSeq2(t, idx, idx.Lookup([][]byte{[]byte("c1")}))
	rev := collectSeq2(t, idx, idx.Lookup([][]byte{[]byte("c1")}, Reverse()))
	if !slices.Equal(fwd, rev) || len(fwd) != 1 {
		t.Fatalf("unique Lookup reverse: fwd=%v rev=%v", fwd, rev)
	}
	fkeys := collectSeq1(t, idx, idx.LookupKeys([][]byte{[]byte("c1")}))
	rkeys := collectSeq1(t, idx, idx.LookupKeys([][]byte{[]byte("c1")}, Reverse()))
	if !slices.Equal(fkeys, rkeys) || len(fkeys) != 1 {
		t.Fatalf("unique LookupKeys reverse (must be a no-op): fwd=%v rev=%v", fkeys, rkeys)
	}

	// Inclusive-start boundary, reverse: only a UNIQUE index stores
	// keys that can EQUAL a full-tuple start bound (non-unique keys
	// always extend the tuple with the PK), so this is the one
	// place the reverse walk's >= start check is observable.
	for _, kv := range [][2]string{{"k2", "c2"}, {"k3", "c3"}} {
		if err := ks.Put([]byte(kv[0]), []byte(kv[1])); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	start := [][]byte{[]byte("c2")}
	f := collectSeq2(t, idx, idx.Range(start, nil))
	r := collectSeq2(t, idx, idx.Range(start, nil, Reverse()))
	slices.Reverse(r)
	if len(f) != 2 || !slices.Equal(f, r) {
		t.Fatalf("unique reverse Range inclusive-start boundary: fwd=%v rev(reversed)=%v", f, r)
	}
}

// A reverse walk stales identically to a forward one (Inv-IHS1
// applies as written): a same-tx Put on the parent keyspace during
// reverse iteration surfaces ErrCursorStale via Err().
func TestIndexReverseIterationStalesOnParentMutation(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	tx, idx, _, cleanup := buildReverseCorpus(t, rng, 50, false)
	defer cleanup()
	ks, err := tx.OpenKeyspace("rows", idx.Decl())
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	seen := 0
	for range idx.Range(nil, nil, Reverse()) {
		seen++
		if seen == 3 {
			if err := ks.Put([]byte("zz-new"), []byte{1, 2, 99}); err != nil {
				t.Fatalf("Put mid-iter: %v", err)
			}
		}
	}
	if err := idx.Err(); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("Err after mid-reverse-iter mutation = %v, want ErrCursorStale", err)
	}
}

// prefixSuccessor pins: increment-and-truncate, 0xFF carry, and
// the open-bound cases (empty and all-0xFF inputs).
func TestPrefixSuccessor(t *testing.T) {
	cases := []struct{ in, want []byte }{
		{[]byte{0x01}, []byte{0x02}},
		{[]byte{0x01, 0xFF}, []byte{0x02}},
		{[]byte{0x01, 0xFE, 0xFF, 0xFF}, []byte{0x01, 0xFF}},
		{[]byte{0xFF, 0xFF}, nil},
		{nil, nil},
		{[]byte{}, nil},
	}
	for _, c := range cases {
		if got := prefixSuccessor(c.in); !slices.Equal(got, c.want) {
			t.Errorf("prefixSuccessor(%x) = %x, want %x", c.in, got, c.want)
		}
	}
}
