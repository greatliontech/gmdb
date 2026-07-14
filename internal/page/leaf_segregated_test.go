package page

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"
)

// segCfg is the segregated-leaf test config: explicit layout so the
// tests stay pinned if the engine default ever changes.
func segCfg() Config {
	return Config{PageSize: 4096, RestartGroupTarget: 4, LeafLayout: LeafLayoutSegregated}
}

// buildSegLeaf builds a segregated leaf from string pairs via the
// public builder, failing the test if any entry doesn't fit.
func buildSegLeaf(t *testing.T, cfg Config, entries [][2]string) []byte {
	t.Helper()
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	for _, e := range entries {
		if !b.AddInline([]byte(e[0]), []byte(e[1])) {
			t.Fatalf("AddInline(%q) did not fit", e[0])
		}
	}
	b.Finish()
	return buf
}

// segCheck validates the page and walks it front to back, returning
// the decoded (key, value) pairs.
func segCheck(t *testing.T, buf []byte, cfg Config) [][2]string {
	t.Helper()
	r := NewLeafReader(buf, cfg)
	if r.Variant() != TypeLeafSegregated {
		t.Fatalf("variant = %d, want TypeLeafSegregated", r.Variant())
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	var out [][2]string
	it := r.IterForReuse(nil, nil, nil)
	for e, ok := it.Next(); ok; e, ok = it.Next() {
		out = append(out, [2]string{string(e.Key), string(e.Value)})
	}
	if len(out) != r.Count() {
		t.Fatalf("walked %d entries, header Count %d", len(out), r.Count())
	}
	return out
}

func TestSegLeafRoundTrip(t *testing.T) {
	cfg := segCfg()
	entries := [][2]string{
		{"apple", "A"}, {"apricot", ""}, {"banana", "B"}, {"blueberry", "B2"},
		{"cherry", "C"}, {"date", ""}, {"durian", "D2"}, {"elderberry", "E"},
		{"fig", "F"}, {"grape", "G"},
	}
	buf := buildSegLeaf(t, cfg, entries)
	got := segCheck(t, buf, cfg)
	for i, e := range entries {
		if got[i][0] != e[0] || got[i][1] != e[1] {
			t.Fatalf("entry %d = %q/%q, want %q/%q", i, got[i][0], got[i][1], e[0], e[1])
		}
	}

	// Point lookups: every present key found with its value; a probe
	// between keys reports the successor index.
	r := NewLeafReader(buf, cfg)
	for i, e := range entries {
		idx, ent, found, err := r.SearchLeaf([]byte(e[0]), NoExtentTail)
		if err != nil || !found || idx != i || string(ent.Value) != e[1] {
			t.Fatalf("SearchLeaf(%q) = idx %d found %v val %q err %v; want %d true %q",
				e[0], idx, found, ent.Value, err, i, e[1])
		}
	}
	if idx, _, found, _ := r.SearchLeaf([]byte("cat"), NoExtentTail); found || idx != 4 {
		t.Fatalf("SearchLeaf(cat) = %d/%v, want 4/false (successor cherry)", idx, found)
	}
	// EmptyValue bit never set on-disk (derived emptiness).
	for i := range entries {
		e, _ := r.EntryAt(i, nil)
		if e.Flags&CellFlagEmptyValue != 0 {
			t.Fatalf("entry %d carries CellFlagEmptyValue on a segregated page", i)
		}
		if e.Value == nil {
			t.Fatalf("entry %d decoded nil Value", i)
		}
	}
}

// The VOff entry-order invariant (page-formats.md §Invariants): with
// zero-length values, adjacent entries share a VOff and decode must
// still attribute lengths by entry order. Alternating empty/non-empty
// values across group boundaries is the aliasing-heavy shape.
func TestSegLeafZeroLengthValueAliasing(t *testing.T) {
	cfg := segCfg()
	var entries [][2]string
	for i := range 40 {
		v := ""
		if i%3 == 0 {
			v = fmt.Sprintf("val-%d", i)
		}
		entries = append(entries, [2]string{fmt.Sprintf("key-%02d", i), v})
	}
	buf := buildSegLeaf(t, cfg, entries)
	got := segCheck(t, buf, cfg)
	for i, e := range entries {
		if got[i][1] != e[1] {
			t.Fatalf("entry %d value = %q, want %q (zero-length aliasing)", i, got[i][1], e[1])
		}
	}
}

// Validate rejects the EmptyValue bit on a segregated entry.
func TestSegValidateRejectsEmptyValueFlag(t *testing.T) {
	cfg := segCfg()
	buf := buildSegLeaf(t, cfg, [][2]string{{"aaa", ""}, {"bbb", "x"}})
	// Set the EmptyValue bit on entry 0's flags (restart at stream start).
	buf[segLeafEntryStart] |= CellFlagEmptyValue
	err := NewLeafReader(buf, cfg).Validate()
	if err == nil {
		t.Fatal("Validate accepted CellFlagEmptyValue on a segregated entry")
	}
}

// Validate rejects non-monotone VOffs.
func TestSegValidateRejectsVOffRegression(t *testing.T) {
	cfg := segCfg()
	buf := buildSegLeaf(t, cfg, [][2]string{{"aaa", "1"}, {"aab", "2"}, {"aac", "3"}})
	r := NewLeafReader(buf, cfg)
	// Forge entry 1's VOff (a delta: VOff at +5) to point past entry 2's.
	off := r.rt.Offset(0)
	keyLen := int(le.Uint16(buf[off+1:]))
	deltaOff := off + 5 + keyLen
	le.PutUint16(buf[deltaOff+5:], uint16(r.valueEnd))
	if err := NewLeafReader(buf, cfg).Validate(); err == nil {
		t.Fatal("Validate accepted a non-monotone VOff")
	}
}

// segOracle rebuilds the same entries through the builder and demands
// byte identity — the deterministic-encoding check used by the split
// and append tests.
func segOracle(t *testing.T, cfg Config, buf []byte, entries [][2]string, ctx string) {
	t.Helper()
	want := buildSegLeaf(t, cfg, entries)
	if !bytes.Equal(buf, want) {
		t.Fatalf("%s: page != canonical rebuild", ctx)
	}
}

func segPairs(prefix string, n int) [][2]string {
	var out [][2]string
	for i := range n {
		out = append(out, [2]string{fmt.Sprintf("%s-%03d", prefix, i), fmt.Sprintf("value-%03d", i)})
	}
	return out
}

// Group split carve: both halves must validate, decode to the exact
// entry subsets, and be byte-identical to a canonical rebuild.
func TestSegSplitMatchesRebuild(t *testing.T) {
	cfg := segCfg()
	entries := segPairs("key", 20)
	src := buildSegLeaf(t, cfg, entries)

	boundary := FindSegSplitGroup(src, cfg)
	dst := make([]byte, cfg.PageSize)
	leftCount, rightCount := SplitSegRightHalf(src, dst, cfg, boundary)
	if leftCount+rightCount != len(entries) {
		t.Fatalf("split counts %d+%d != %d", leftCount, rightCount, len(entries))
	}
	segCheck(t, dst, cfg)
	segOracle(t, cfg, dst, entries[leftCount:], "right half")

	TruncateSegToGroups(src, cfg, boundary)
	segCheck(t, src, cfg)
	segOracle(t, cfg, src, entries[:leftCount], "left half")
}

// TrySegAppend: appending in key order must keep the page valid,
// decode correctly, and stay byte-identical to a rebuild (append is
// the canonical splice, mirroring the interleaved contract).
func TestSegTryAppendCanonical(t *testing.T) {
	cfg := segCfg()
	entries := segPairs("key", 12)
	buf := buildSegLeaf(t, cfg, entries[:1])
	for i := 1; i < len(entries); i++ {
		prev := []byte(entries[i-1][0])
		e := LeafEntry{Key: []byte(entries[i][0]), Value: []byte(entries[i][1])}
		if !TryAppend(buf, cfg, e, prev) {
			t.Fatalf("TryAppend(%q) declined", entries[i][0])
		}
		segCheck(t, buf, cfg)
	}
	segOracle(t, cfg, buf, entries, "after appends")
}

// TrySegAppend with empty values (zero-length spans at the region top).
func TestSegTryAppendEmptyValues(t *testing.T) {
	cfg := segCfg()
	buf := buildSegLeaf(t, cfg, [][2]string{{"aaa", "v"}})
	for _, k := range []string{"bbb", "ccc", "ddd"} {
		e := LeafEntry{Key: []byte(k)}
		prev, _ := NewLeafReader(buf, cfg).LastKey(nil)
		if !TryAppend(buf, cfg, e, bytes.Clone(prev)) {
			t.Fatalf("TryAppend(%q) declined", k)
		}
		segCheck(t, buf, cfg)
	}
	got := segCheck(t, buf, cfg)
	want := [][2]string{{"aaa", "v"}, {"bbb", ""}, {"ccc", ""}, {"ddd", ""}}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TrySegInsertAt across the I-B / I-C / I-D positions: the spliced
// page must validate and decode to the expected sequence (the insert
// splice is localized, not canonical — mirror of the interleaved
// contract).
func TestSegTryInsertAtPositions(t *testing.T) {
	mk := func() ([][2]string, []byte) {
		cfg := segCfg()
		entries := [][2]string{
			{"key-00", "v0"}, {"key-02", "v2"}, {"key-04", "v4"},
			{"key-06", "v6"}, {"key-08", "v8"}, {"key-10", "v10"},
		}
		return entries, buildSegLeaf(t, cfg, entries)
	}
	insert := func(t *testing.T, buf []byte, idx int, k, v string) {
		t.Helper()
		if !TryInsertAt(buf, segCfg(), idx, LeafEntry{Key: []byte(k), Value: []byte(v)}) {
			t.Fatalf("TryInsertAt(%d, %q) declined", idx, k)
		}
	}
	t.Run("I-B front", func(t *testing.T) {
		entries, buf := mk()
		insert(t, buf, 0, "key-!!", "front")
		got := segCheck(t, buf, segCfg())
		if got[0] != [2]string{"key-!!", "front"} || got[1] != entries[0] {
			t.Fatalf("front insert: got[0..1] = %v %v", got[0], got[1])
		}
	})
	t.Run("I-C interior", func(t *testing.T) {
		entries, buf := mk()
		insert(t, buf, 2, "key-03", "mid")
		got := segCheck(t, buf, segCfg())
		if got[2] != [2]string{"key-03", "mid"} || got[3] != entries[2] {
			t.Fatalf("interior insert: got[2..3] = %v %v", got[2], got[3])
		}
	})
	t.Run("I-C empty value", func(t *testing.T) {
		entries, buf := mk()
		insert(t, buf, 2, "key-03", "")
		got := segCheck(t, buf, segCfg())
		if got[2] != [2]string{"key-03", ""} || got[3] != entries[2] {
			t.Fatalf("empty-value insert: got[2..3] = %v %v", got[2], got[3])
		}
	})
	t.Run("I-D group boundary", func(t *testing.T) {
		_, buf := mk()
		// RestartGroupTarget 4 → group 0 holds entries 0..3; index 4 is
		// the boundary routed into group 0 as p == gc.
		insert(t, buf, 4, "key-07", "tail")
		got := segCheck(t, buf, segCfg())
		if got[4] != [2]string{"key-07", "tail"} {
			t.Fatalf("boundary insert: got[4] = %v", got[4])
		}
	})
}

// TrySegDeleteAt across the D-A / D-B / D-C / D-D positions.
func TestSegTryDeleteAtPositions(t *testing.T) {
	cfg := segCfg()
	entries := segPairs("key", 13) // groups of 4: [0..3][4..7][8..11][12]
	mk := func() []byte { return buildSegLeaf(t, cfg, entries) }
	del := func(t *testing.T, buf []byte, idx int) {
		t.Helper()
		if !TryDeleteAt(buf, cfg, idx) {
			t.Fatalf("TryDeleteAt(%d) declined", idx)
		}
	}
	expectWithout := func(t *testing.T, buf []byte, missing ...int) {
		t.Helper()
		got := segCheck(t, buf, cfg)
		skip := map[int]bool{}
		for _, m := range missing {
			skip[m] = true
		}
		var want [][2]string
		for i, e := range entries {
			if !skip[i] {
				want = append(want, e)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("count %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("entry %d = %v, want %v", i, got[i], want[i])
			}
		}
	}
	t.Run("D-A single-entry group", func(t *testing.T) {
		buf := mk()
		del(t, buf, 12)
		expectWithout(t, buf, 12)
	})
	t.Run("D-B group front", func(t *testing.T) {
		buf := mk()
		del(t, buf, 4)
		expectWithout(t, buf, 4)
	})
	t.Run("D-C interior", func(t *testing.T) {
		buf := mk()
		del(t, buf, 5)
		expectWithout(t, buf, 5)
	})
	t.Run("D-D group tail", func(t *testing.T) {
		buf := mk()
		del(t, buf, 3)
		expectWithout(t, buf, 3)
	})
	t.Run("drain to one", func(t *testing.T) {
		buf := mk()
		for NewLeafReader(buf, cfg).Count() > 1 {
			del(t, buf, 0)
			segCheck(t, buf, cfg)
		}
	})
}

// Special value forms round-trip through the segregated builder and
// splices: overflow values, nested-tree references, subpages, and
// overflow keys with each of those halves.
func TestSegSpecialFormsRoundTrip(t *testing.T) {
	cfg := segCfg()
	tt := cfg.InlineThreshold()
	longKey := bytes.Repeat([]byte("z"), tt)
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	if !b.AddInline([]byte("aaa"), []byte("v")) {
		t.Fatal("AddInline declined")
	}
	if !b.AddOverflow([]byte("bbb"), 77, 123456) {
		t.Fatal("AddOverflow declined")
	}
	if !b.AddNestedTreeRef([]byte("ccc"), 88, 42) {
		t.Fatal("AddNestedTreeRef declined")
	}
	if !b.AddSubpage([]byte("ddd"), []byte("subpage-bytes")) {
		t.Fatal("AddSubpage declined")
	}
	if !b.AddEntry(LeafEntry{
		Flags: CellFlagOverflowKey, Key: longKey,
		KeyExtPage: 99, KeyTotalLen: uint32(tt + 50), Value: []byte("ovk-val"),
	}) {
		t.Fatal("AddEntry(overflow-key) declined")
	}
	b.Finish()

	r := NewLeafReader(buf, cfg)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	e1, _ := r.EntryAt(1, nil)
	if !e1.IsOverflow() || e1.OverflowPage != 77 || e1.TotalLen != 123456 {
		t.Fatalf("overflow entry = %+v", e1)
	}
	e2, _ := r.EntryAt(2, nil)
	if !e2.IsNestedTree() || e2.NestedRoot != 88 || e2.NestedCount != 42 {
		t.Fatalf("nested entry = %+v", e2)
	}
	e3, _ := r.EntryAt(3, nil)
	if !e3.IsSubpage() || string(e3.Value) != "subpage-bytes" {
		t.Fatalf("subpage entry = %+v", e3)
	}
	e4, _ := r.EntryAt(4, nil)
	if !e4.IsOverflowKey() || e4.KeyExtPage != 99 || e4.KeyTotalLen != uint32(tt+50) ||
		string(e4.Value) != "ovk-val" || len(e4.Key) != tt {
		t.Fatalf("overflow-key entry = %+v", e4)
	}
	// Singleton-group rule: the overflow-key entry is alone in its group.
	if gc := r.GroupEntryCount(r.RestartCount() - 1); gc != 1 {
		t.Fatalf("overflow-key group has %d entries, want singleton", gc)
	}

	// PatchRefs / PatchKeyExtRefs rewrite the region-resident refs.
	r.PatchRefs(func(idx int, e LeafEntry) uint64 {
		if e.IsOverflow() {
			return 770
		}
		return 880
	})
	r.PatchKeyExtRefs(func(idx int, e LeafEntry) uint64 { return 990 })
	r2 := NewLeafReader(buf, cfg)
	if err := r2.Validate(); err != nil {
		t.Fatalf("Validate after patch: %v", err)
	}
	e1, _ = r2.EntryAt(1, nil)
	e2, _ = r2.EntryAt(2, nil)
	e4, _ = r2.EntryAt(4, nil)
	if e1.OverflowPage != 770 || e2.NestedRoot != 880 || e4.KeyExtPage != 990 {
		t.Fatalf("patched refs = %d/%d/%d, want 770/880/990", e1.OverflowPage, e2.NestedRoot, e4.KeyExtPage)
	}
	if e4.KeyTotalLen != uint32(tt+50) || e2.NestedCount != 42 {
		t.Fatal("patch mutated an immutable second field")
	}
}

// ---------------------------------------------------------------------------
// Segregated fuzz targets — the same oracles as the interleaved fuzzers
// (byte-identity vs rebuild for the canonical append and split carve;
// semantic + structural for the localized insert/delete splices), run
// over the segregated layout. Keeps the fuzz grammar in step with the
// input surface (a lagging grammar is a silent coverage cap).
// ---------------------------------------------------------------------------

func segFuzzCfg() Config {
	return Config{PageSize: 4096, RestartGroupTarget: 6, LeafLayout: LeafLayoutSegregated}
}

func FuzzSegTryAppend(f *testing.F) {
	f.Add(uint64(1), uint64(2))
	f.Add(uint64(42), uint64(7))
	f.Add(uint64(0xDEADBEEF), uint64(0xCAFEBABE))

	f.Fuzz(func(t *testing.T, leafSeed, appendSeed uint64) {
		cfg := segFuzzCfg()
		base := randomFittingEntries(leafSeed, cfg)
		if len(base) == 0 {
			return
		}
		appendKey := keyAfterSeed(base[len(base)-1].Key, appendSeed)
		val := make([]byte, int(appendSeed%48))
		for i := range val {
			val[i] = byte(appendSeed >> uint(8*(i%8)))
		}
		assertAppendMatchesRebuild(t, cfg, base, LeafEntry{Key: appendKey, Value: val})
	})
}

func FuzzSegTryInsertAt(f *testing.F) {
	f.Add(uint64(1), uint64(2), uint64(0))
	f.Add(uint64(42), uint64(7), uint64(3))
	f.Add(uint64(0xDEADBEEF), uint64(0xCAFEBABE), uint64(5))

	f.Fuzz(func(t *testing.T, leafSeed, keySeed, posSeed uint64) {
		cfg := segFuzzCfg()
		base := randomFittingMixed(leafSeed, cfg)
		if len(base) == 0 {
			return
		}
		insertIdx := int(posSeed % uint64(len(base)))
		var lo []byte
		if insertIdx > 0 {
			lo = base[insertIdx-1].Key
		}
		newKey, ok := keyBetween(lo, base[insertIdx].Key, keySeed)
		if !ok {
			return
		}
		e := randomLeafEntry(rand.New(rand.NewPCG(keySeed, keySeed^0xABCD1234EF567890)), newKey)

		buf, fit := tryBuild(cfg, base)
		if !fit {
			return
		}
		before := bytes.Clone(buf)
		if !TryInsertAt(buf, cfg, insertIdx, e) {
			if !bytes.Equal(buf, before) {
				t.Fatal("TryInsertAt declined but mutated the page")
			}
			return
		}
		assertLeafDecodesTo(t, buf, cfg, insertExpected(base, e, insertIdx))
		assertFreeSpaceZeroed(t, buf, cfg)
	})
}

func FuzzSegTryDeleteAt(f *testing.F) {
	f.Add(uint64(1), uint64(0))
	f.Add(uint64(42), uint64(3))
	f.Add(uint64(0xDEADBEEF), uint64(5))

	f.Fuzz(func(t *testing.T, leafSeed, posSeed uint64) {
		cfg := segFuzzCfg()
		base := randomFittingMixed(leafSeed, cfg)
		if len(base) == 0 {
			return
		}
		buf, fit := tryBuild(cfg, base)
		if !fit {
			return
		}
		deleteIdx := int(posSeed % uint64(len(base)))
		before := bytes.Clone(buf)
		if len(base) == 1 {
			if TryDeleteAt(buf, cfg, deleteIdx) {
				t.Fatal("TryDeleteAt should decline a count==1 page")
			}
			if !bytes.Equal(buf, before) {
				t.Fatal("TryDeleteAt mutated a count==1 page on decline")
			}
			return
		}
		if !TryDeleteAt(buf, cfg, deleteIdx) {
			t.Fatalf("TryDeleteAt declined for count=%d idx=%d (delete always shrinks)", len(base), deleteIdx)
		}
		assertLeafDecodesTo(t, buf, cfg, deleteExpected(base, deleteIdx))
		assertFreeSpaceZeroed(t, buf, cfg)
	})
}

// assertSegSplitMatchesRebuild is the segregated mirror of
// assertSplitMatchesRebuild: read-only right carve, canonical halves,
// zeroed free space, Validate-clean.
func assertSegSplitMatchesRebuild(t *testing.T, cfg Config, entries []LeafEntry, splitGroup int) {
	t.Helper()
	orig, ok := tryBuild(cfg, entries)
	if !ok {
		t.Fatalf("base (%d entries) did not fit", len(entries))
	}
	origCopy := bytes.Clone(orig)
	src := bytes.Clone(orig)
	dst := make([]byte, cfg.PageSize)

	leftCount, rightCount := SplitSegRightHalf(src, dst, cfg, splitGroup)
	if !bytes.Equal(src, origCopy) {
		t.Fatalf("SplitSegRightHalf mutated src (must be read-only): splitGroup=%d", splitGroup)
	}
	if leftCount+rightCount != len(entries) || leftCount < 1 || rightCount < 1 {
		t.Fatalf("counts (%d,%d) don't partition %d entries", leftCount, rightCount, len(entries))
	}
	wantRight, _ := tryBuild(cfg, entries[leftCount:])
	if !bytes.Equal(dst, wantRight) {
		t.Fatalf("right half != rebuild(entries[%d:]) (must be canonical): splitGroup=%d", leftCount, splitGroup)
	}
	assertFreeSpaceZeroed(t, dst, cfg)
	if err := NewLeafReader(dst, cfg).Validate(); err != nil {
		t.Fatalf("right half fails Validate: %v", err)
	}

	if tlc := TruncateSegToGroups(src, cfg, splitGroup); tlc != leftCount {
		t.Fatalf("TruncateSegToGroups leftCount=%d, want %d", tlc, leftCount)
	}
	wantLeft, _ := tryBuild(cfg, entries[:leftCount])
	if !bytes.Equal(src, wantLeft) {
		t.Fatalf("left half != rebuild(entries[:%d]) (must be canonical): splitGroup=%d", leftCount, splitGroup)
	}
	assertFreeSpaceZeroed(t, src, cfg)
	if err := NewLeafReader(src, cfg).Validate(); err != nil {
		t.Fatalf("left half fails Validate: %v", err)
	}
}

func FuzzSegSplitLeafAtGroup(f *testing.F) {
	f.Add(uint64(1), uint64(0))
	f.Add(uint64(42), uint64(1))
	f.Add(uint64(0xDEADBEEF), uint64(2))

	f.Fuzz(func(t *testing.T, leafSeed, groupSeed uint64) {
		cfg := Config{PageSize: 4096, RestartGroupTarget: 4, LeafLayout: LeafLayoutSegregated} // smaller RGT → more groups
		entries := randomFittingMixed(leafSeed, cfg)
		if len(entries) == 0 {
			return
		}
		buf, ok := tryBuild(cfg, entries)
		if !ok {
			return
		}
		rc := NewLeafReader(buf, cfg).RestartCount()
		if rc < 2 {
			return
		}
		splitGroup := 1 + int(groupSeed%uint64(rc-1))
		assertSegSplitMatchesRebuild(t, cfg, entries, splitGroup)
	})
}

// Every leaf layout must hold ONE maximal-form entry (overflow key +
// overflow value) at every page size and checksum mode — the leaf half
// of the per-layout split-feasibility floor (page-formats.md
// §Invariants; the branch half lands with the branch variants).
func TestLeafFloorOneMaximalEntryEveryLayout(t *testing.T) {
	for _, ps := range []uint32{4096, 8192, 16384, 65536} {
		for _, ck := range []bool{false, true} {
			for _, layout := range []struct {
				name string
				rgt  uint16
				ll   LeafLayout
			}{
				{"interleaved", 6, LeafLayoutInterleaved},
				{"segregated", 6, LeafLayoutSegregated},
				{"uncompressed", 1, LeafLayoutDefault},
			} {
				cfg := Config{PageSize: ps, PageChecksum: ck, RestartGroupTarget: layout.rgt, LeafLayout: layout.ll}
				tt := cfg.InlineThreshold()
				buf := make([]byte, ps)
				b := NewLeafBuilder(buf, cfg)
				e := LeafEntry{
					Flags:        CellFlagOverflowKey | CellFlagOverflow,
					Key:          bytes.Repeat([]byte("k"), tt),
					KeyExtPage:   7,
					KeyTotalLen:  uint32(tt + 100),
					OverflowPage: 9,
					TotalLen:     1 << 20,
				}
				if !b.AddEntry(e) {
					t.Errorf("%s ps=%d ck=%v: one maximal-form entry does not fit (floor violated)", layout.name, ps, ck)
					continue
				}
				b.Finish()
				if err := NewLeafReader(buf, cfg).Validate(); err != nil {
					t.Errorf("%s ps=%d ck=%v: maximal-entry page fails Validate: %v", layout.name, ps, ck, err)
				}
			}
		}
	}
}

// TryDeleteAtNative splices in the PAGE's own variant regardless of the
// configured one — the delete path's removal-monotone fallback
// (page-formats.md §Insert and Delete). A segregated page under an
// interleaved-configured keyspace (mid-migration) must still native-
// splice rather than decline.
func TestSegTryDeleteAtNativeIgnoresConfiguredVariant(t *testing.T) {
	buildCfg := segCfg()
	entries := segPairs("key", 9)
	buf := buildSegLeaf(t, buildCfg, entries)

	// Configured variant differs (interleaved): TryDeleteAt declines
	// (migration gate), TryDeleteAtNative splices natively.
	delCfg := Config{PageSize: 4096, RestartGroupTarget: 4, LeafLayout: LeafLayoutInterleaved}
	before := bytes.Clone(buf)
	if TryDeleteAt(buf, delCfg, 3) {
		t.Fatal("TryDeleteAt should decline on a variant mismatch")
	}
	if !bytes.Equal(buf, before) {
		t.Fatal("TryDeleteAt mutated the page on decline")
	}
	if !TryDeleteAtNative(buf, delCfg, 3) {
		t.Fatal("TryDeleteAtNative declined a count>1 segregated page")
	}
	got := segCheck(t, buf, buildCfg)
	want := append(append([][2]string{}, entries[:3]...), entries[4:]...)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
}
