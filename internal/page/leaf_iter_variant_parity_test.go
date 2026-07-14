package page

import (
	"bytes"
	"fmt"
	"testing"
)

// The uncompressed variant must expose the same LeafIter semantics as the
// compressed variant (page-formats.md §Cursor Iteration — the modes are
// per-variant machinery, not per-variant behavior). These tests pin the
// behaviors where the uncompressed positioning previously diverged:
// exact-match seek of the last entry (offset-table read past the last
// slot), and Prev/At repositioning followed by Next.

// Exact-match SearchLeafIter on every entry of an uncompressed leaf with
// checksums DISABLED: no footer slack exists past the offset table, so
// any past-end table read faults. The last entry is the interesting case;
// the loop pins all of them plus the returned iterator's Next.
// TestSearchLeafIter_Miss_Uncompressed below pins the two miss arms.
func TestSearchLeafIter_ExactMatch_Uncompressed_NoChecksum(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}
	entries := [][2]string{
		{"apple", "A"}, {"banana", "B"}, {"cherry", "C"}, {"date", "D"},
	}
	buf := buildLeaf(t, cfg, entries)
	r := NewLeafReader(buf, cfg)
	if r.Compressed() {
		t.Fatal("expected uncompressed leaf")
	}

	for i, e := range entries {
		idx, ent, found, it, _ := r.SearchLeafIter([]byte(e[0]), nil, nil, nil, NoExtentTail)
		if !found || idx != i {
			t.Fatalf("SearchLeafIter(%q): found=%v idx=%d, want true/%d", e[0], found, idx, i)
		}
		if !bytes.Equal(ent.Value, []byte(e[1])) {
			t.Fatalf("SearchLeafIter(%q): value=%q want %q", e[0], ent.Value, e[1])
		}
		nxt, ok := it.Next()
		if i == len(entries)-1 {
			if ok {
				t.Fatalf("SearchLeafIter(%q): Next past last entry ok=true, key=%q", e[0], nxt.Key)
			}
		} else {
			if !ok || !bytes.Equal(nxt.Key, []byte(entries[i+1][0])) {
				t.Fatalf("SearchLeafIter(%q): Next=%q/%v, want %q", e[0], nxt.Key, ok, entries[i+1][0])
			}
		}
	}
}

// Forward iteration, step back, resume: Next,Next,Next,Prev,Next on
// [a b c d] must yield a,b,c,b,c on both variants. The uncompressed
// variant previously skipped c and then fabricated entries from the free
// region.
func TestLeafIter_PrevThenNextResumes_BothVariants(t *testing.T) {
	entries := [][2]string{
		{"aaa", "1"}, {"bbb", "2"}, {"ccc", "3"}, {"ddd", "4"},
	}
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"uncompressed", Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}},
		{"interleaved", Config{PageSize: 4096, RestartGroupTarget: 2, PageChecksum: false, LeafLayout: LeafLayoutInterleaved}},
		{"segregated", Config{PageSize: 4096, RestartGroupTarget: 2, PageChecksum: false, LeafLayout: LeafLayoutSegregated}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := buildLeaf(t, tc.cfg, entries)
			r := NewLeafReader(buf, tc.cfg)
			it := r.IterForReuse(nil, nil, nil)

			step := func(op string, got LeafEntry, ok bool, wantKey string) {
				t.Helper()
				if !ok || !bytes.Equal(got.Key, []byte(wantKey)) {
					t.Fatalf("%s: key=%q ok=%v, want %q", op, got.Key, ok, wantKey)
				}
			}
			e, ok := it.Next()
			step("Next#1", e, ok, "aaa")
			e, ok = it.Next()
			step("Next#2", e, ok, "bbb")
			e, ok = it.Next()
			step("Next#3", e, ok, "ccc")
			e, ok = it.Prev()
			step("Prev", e, ok, "bbb")
			e, ok = it.Next()
			step("Next#4", e, ok, "ccc")
			e, ok = it.Next()
			step("Next#5", e, ok, "ddd")
			if e, ok = it.Next(); ok {
				t.Fatalf("Next past end: ok=true key=%q", e.Key)
			}
		})
	}
}

// The cursor's Last() setup: a past-end iter (IterAtForReuse(count)),
// position at the last entry via At, walk backward, then resume forward.
// The uncompressed variant previously decoded the page header as an
// entry after this repositioning.
func TestLeafIter_PastEndAtPrevNext_BothVariants(t *testing.T) {
	entries := [][2]string{
		{"aaa", "1"}, {"bbb", "2"}, {"ccc", "3"}, {"ddd", "4"},
	}
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"uncompressed", Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}},
		{"interleaved", Config{PageSize: 4096, RestartGroupTarget: 2, PageChecksum: false, LeafLayout: LeafLayoutInterleaved}},
		{"segregated", Config{PageSize: 4096, RestartGroupTarget: 2, PageChecksum: false, LeafLayout: LeafLayoutSegregated}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := buildLeaf(t, tc.cfg, entries)
			r := NewLeafReader(buf, tc.cfg)
			it := r.IterAtForReuse(r.Count(), nil, nil, nil)

			if e, ok := it.Next(); ok {
				t.Fatalf("Next on past-end iter: ok=true key=%q", e.Key)
			}
			e, ok := it.At(r.Count() - 1)
			if !ok || !bytes.Equal(e.Key, []byte("ddd")) {
				t.Fatalf("At(last): key=%q ok=%v, want ddd", e.Key, ok)
			}
			e, ok = it.Prev()
			if !ok || !bytes.Equal(e.Key, []byte("ccc")) {
				t.Fatalf("Prev: key=%q ok=%v, want ccc", e.Key, ok)
			}
			e, ok = it.Next()
			if !ok || !bytes.Equal(e.Key, []byte("ddd")) {
				t.Fatalf("Next after Prev: key=%q ok=%v, want ddd", e.Key, ok)
			}
		})
	}
}

// Miss-path SearchLeafIter on an uncompressed leaf: the returned
// iterator must be positioned past the successor (spec §Leaf Lookup —
// "positioned past the result, ready to stream forward without
// re-emitting"), and a past-end miss must yield an immediately-exhausted
// iterator.
func TestSearchLeafIter_Miss_Uncompressed(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}
	entries := [][2]string{
		{"aaa", "1"}, {"ccc", "3"}, {"eee", "5"}, {"ggg", "7"},
	}
	buf := buildLeaf(t, cfg, entries)
	r := NewLeafReader(buf, cfg)

	// Gap misses: successor returned, iterator streams from successor+1.
	for _, tc := range []struct {
		target  string
		wantIdx int // successor index
	}{
		{"a", 0}, {"bbb", 1}, {"ddd", 2}, {"fff", 3},
	} {
		idx, ent, found, it, _ := r.SearchLeafIter([]byte(tc.target), nil, nil, nil, NoExtentTail)
		if found || idx != tc.wantIdx {
			t.Fatalf("SearchLeafIter(%q): found=%v idx=%d, want false/%d", tc.target, found, idx, tc.wantIdx)
		}
		if !bytes.Equal(ent.Key, []byte(entries[tc.wantIdx][0])) {
			t.Fatalf("SearchLeafIter(%q): successor key=%q, want %q", tc.target, ent.Key, entries[tc.wantIdx][0])
		}
		nxt, ok := it.Next()
		if tc.wantIdx == len(entries)-1 {
			if ok {
				t.Fatalf("SearchLeafIter(%q): Next past last successor ok=true key=%q", tc.target, nxt.Key)
			}
		} else if !ok || !bytes.Equal(nxt.Key, []byte(entries[tc.wantIdx+1][0])) {
			t.Fatalf("SearchLeafIter(%q): Next=%q/%v, want %q (no re-emit, no skip)", tc.target, nxt.Key, ok, entries[tc.wantIdx+1][0])
		}
	}

	// Past-end miss: idx == count, iterator positioned exactly at count
	// (a mispositioned iter would satisfy Next→!ok for any idx ≥ count
	// but step Prev to the wrong entry) and immediately exhausted.
	idx, _, found, it, _ := r.SearchLeafIter([]byte("zzz"), nil, nil, nil, NoExtentTail)
	if found || idx != r.Count() {
		t.Fatalf("SearchLeafIter(zzz): found=%v idx=%d, want false/%d", found, idx, r.Count())
	}
	if it.Idx() != r.Count() {
		t.Fatalf("SearchLeafIter(zzz): iter Idx=%d, want %d", it.Idx(), r.Count())
	}
	if e, ok := it.Next(); ok {
		t.Fatalf("SearchLeafIter(zzz): Next ok=true key=%q, want exhausted", e.Key)
	}
}

// Exhaustive variant-parity sweep: every op sequence of length ≤ 5 drawn
// from {Next, Prev, At(0..n-1)} must produce byte-identical (key, value,
// ok) streams on the compressed and uncompressed encodings of the same
// entry set. This pins the whole behavior class, not just the sequences
// above.
func TestLeafIter_VariantParity_Exhaustive(t *testing.T) {
	entries := [][2]string{
		{"aa", "1"}, {"ab", "2"}, {"ba", "3"}, {"bb", "4"}, {"ca", "5"},
	}
	ucCfg := Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}
	coCfg := Config{PageSize: 4096, RestartGroupTarget: 2, PageChecksum: false}
	ucBuf := buildLeaf(t, ucCfg, entries)
	coBuf := buildLeaf(t, coCfg, entries)
	uc := NewLeafReader(ucBuf, ucCfg)
	co := NewLeafReader(coBuf, coCfg)

	// Ops: 0 = Next, 1 = Prev, 2+k = At(k) — k == len(entries) sweeps
	// the out-of-range rejection for parity too.
	nOps := 2 + len(entries) + 1
	apply := func(it *LeafIter, op int) string {
		var e LeafEntry
		var ok bool
		switch {
		case op == 0:
			e, ok = it.Next()
		case op == 1:
			e, ok = it.Prev()
		default:
			e, ok = it.At(op - 2)
		}
		if !ok {
			return "!ok"
		}
		return fmt.Sprintf("%q=%q", e.Key, e.Value)
	}

	const maxLen = 5
	var seq []int
	var walk func()
	walk = func() {
		if len(seq) > 0 {
			// Fresh iterators per sequence, for every start position
			// (fresh-forward and past-end, the two cursor entry points).
			for _, start := range []int{-1, len(entries)} {
				var itU, itC LeafIter
				if start < 0 {
					itU = uc.IterForReuse(nil, nil, nil)
					itC = co.IterForReuse(nil, nil, nil)
				} else {
					itU = uc.IterAtForReuse(start, nil, nil, nil)
					itC = co.IterAtForReuse(start, nil, nil, nil)
				}
				for i, op := range seq {
					gotU := apply(&itU, op)
					gotC := apply(&itC, op)
					if gotU != gotC {
						t.Fatalf("seq=%v start=%d step=%d: uncompressed=%s compressed=%s",
							seq, start, i, gotU, gotC)
					}
				}
			}
		}
		if len(seq) == maxLen {
			return
		}
		for op := 0; op < nOps; op++ {
			seq = append(seq, op)
			walk()
			seq = seq[:len(seq)-1]
		}
	}
	walk()
}
