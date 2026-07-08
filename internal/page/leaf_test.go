package page

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// Helper: build a leaf page from a sorted (key, value) sequence using the
// supplied Config. Returns the page bytes. Fails the test on builder
// fit error — tests should keep their inputs small enough to fit a page.
func buildLeaf(t *testing.T, cfg Config, entries [][2]string) []byte {
	t.Helper()
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	for _, e := range entries {
		if !b.AddInline([]byte(e[0]), []byte(e[1])) {
			t.Fatalf("buildLeaf: AddInline(%q) returned false (page full)", e[0])
		}
	}
	b.Finish()
	return buf
}

// ---------------------------------------------------------------------------
// Reader basics — both variants
// ---------------------------------------------------------------------------

func TestLeafReader_Compressed_RoundTrip(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	entries := [][2]string{
		{"apple", "A"}, {"apricot", "A2"}, {"banana", "B"}, {"blueberry", "B2"},
		{"cherry", "C"}, {"date", "D"}, {"durian", "D2"}, {"elderberry", "E"},
	}
	buf := buildLeaf(t, cfg, entries)

	typ, _, count, _ := ReadHeader(buf)
	if typ != TypeLeaf {
		t.Fatalf("type = %d, want TypeLeaf=%d", typ, TypeLeaf)
	}
	if int(count) != len(entries) {
		t.Fatalf("count = %d, want %d", count, len(entries))
	}

	r := NewLeafReader(buf, cfg)
	if !r.Compressed() {
		t.Error("Compressed() = false, want true")
	}
	if r.Count() != len(entries) {
		t.Errorf("Count = %d, want %d", r.Count(), len(entries))
	}
	if got := r.RestartCount(); got < 2 {
		t.Errorf("RestartCount = %d, want ≥ 2 (target=4, %d entries)", got, len(entries))
	}

	// Every key must be findable; every value must round-trip.
	for i, e := range entries {
		idx, ent, found := r.SearchLeaf([]byte(e[0]))
		if !found {
			t.Errorf("SearchLeaf(%q): not found", e[0])
			continue
		}
		if idx != i {
			t.Errorf("SearchLeaf(%q): idx = %d, want %d", e[0], idx, i)
		}
		if !bytes.Equal(ent.Value, []byte(e[1])) {
			t.Errorf("SearchLeaf(%q): value = %q, want %q", e[0], ent.Value, e[1])
		}
	}

	// Misses on either side and gaps.
	for _, miss := range []string{"", "aa", "carrot", "elderberryX", "zzzzz"} {
		_, _, found := r.SearchLeaf([]byte(miss))
		if found {
			t.Errorf("SearchLeaf(%q): unexpectedly found", miss)
		}
	}
}

func TestLeafReader_Uncompressed_RoundTrip(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1}
	entries := [][2]string{
		{"alpha", "1"}, {"beta", "2"}, {"gamma", "3"}, {"delta", "4"},
	}
	// Builder accepts only sorted entries — pre-sort.
	sorted := [][2]string{{"alpha", "1"}, {"beta", "2"}, {"delta", "4"}, {"gamma", "3"}}
	buf := buildLeaf(t, cfg, sorted)

	typ, _, count, _ := ReadHeader(buf)
	if typ != TypeLeafUncompressed {
		t.Fatalf("type = %d, want TypeLeafUncompressed=%d", typ, TypeLeafUncompressed)
	}
	if int(count) != len(entries) {
		t.Fatalf("count = %d, want %d", count, len(entries))
	}

	r := NewLeafReader(buf, cfg)
	if r.Compressed() {
		t.Error("Compressed() = true, want false for uncompressed leaf")
	}
	if r.RestartCount() != len(entries) {
		// uncompressed: every entry is its own "group"
		t.Errorf("RestartCount = %d, want %d (one group per entry)", r.RestartCount(), len(entries))
	}

	for _, e := range sorted {
		_, ent, found := r.SearchLeaf([]byte(e[0]))
		if !found {
			t.Errorf("SearchLeaf(%q): not found", e[0])
			continue
		}
		if !bytes.Equal(ent.Value, []byte(e[1])) {
			t.Errorf("SearchLeaf(%q): value = %q, want %q", e[0], ent.Value, e[1])
		}
	}
	for _, miss := range []string{"", "alphabet", "z"} {
		_, _, found := r.SearchLeaf([]byte(miss))
		if found {
			t.Errorf("SearchLeaf(%q): unexpectedly found", miss)
		}
	}
}

// ---------------------------------------------------------------------------
// Variable-size restart groups
// ---------------------------------------------------------------------------

func TestLeafBuilder_NaturalBreakStartsNewGroup(t *testing.T) {
	// Keys that share zero prefix with each other should trigger the
	// natural-break heuristic — each new key starts a fresh group
	// rather than accruing 2-byte delta-header overhead with no
	// SharedLen recoup.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 16}
	entries := [][2]string{
		{"aaa-1", "v"}, {"aaa-2", "v"}, // shared prefix → same group
		{"bbb-1", "v"}, {"bbb-2", "v"}, // new prefix → natural break
		{"ccc-1", "v"}, {"ccc-2", "v"}, // another natural break
	}
	buf := buildLeaf(t, cfg, entries)
	r := NewLeafReader(buf, cfg)
	if r.RestartCount() < 3 {
		t.Errorf("RestartCount = %d, want ≥ 3 (natural breaks should split the 3 prefix runs)", r.RestartCount())
	}
	// All keys still findable.
	for _, e := range entries {
		_, _, found := r.SearchLeaf([]byte(e[0]))
		if !found {
			t.Errorf("SearchLeaf(%q): not found", e[0])
		}
	}
}

func TestLeafBuilder_GroupTargetCap(t *testing.T) {
	// With RestartGroupTarget=4 and 12 prefix-sharing entries, expect
	// at least 3 groups (12/4) so no group exceeds the cap.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	entries := make([][2]string, 12)
	for i := range entries {
		entries[i] = [2]string{fmt.Sprintf("prefix-key-%02d", i), "v"}
	}
	buf := buildLeaf(t, cfg, entries)
	r := NewLeafReader(buf, cfg)
	if r.RestartCount() < 3 {
		t.Errorf("RestartCount = %d, want ≥ 3 (target=4, 12 entries)", r.RestartCount())
	}
	for g := range r.RestartCount() {
		if cnt := r.GroupEntryCount(g); cnt > 4 {
			t.Errorf("group %d has %d entries, exceeds target 4", g, cnt)
		}
	}
}

// ---------------------------------------------------------------------------
// Iter — forward + backward
// ---------------------------------------------------------------------------

func TestLeafIter_ForwardStreaming_Compressed(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	entries := make([][2]string, 20)
	for i := range entries {
		entries[i] = [2]string{fmt.Sprintf("prefix-key-%03d", i), fmt.Sprintf("v%03d", i)}
	}
	buf := buildLeaf(t, cfg, entries)
	r := NewLeafReader(buf, cfg)
	it := r.IterForReuse(nil, nil, nil)
	got := 0
	for e, ok := it.Next(); ok; e, ok = it.Next() {
		want := entries[got]
		if !bytes.Equal(e.Key, []byte(want[0])) {
			t.Errorf("Next[%d]: key = %q, want %q", got, e.Key, want[0])
		}
		if !bytes.Equal(e.Value, []byte(want[1])) {
			t.Errorf("Next[%d]: value = %q, want %q", got, e.Value, want[1])
		}
		got++
	}
	if got != len(entries) {
		t.Errorf("Next yielded %d entries, want %d", got, len(entries))
	}
}

func TestLeafIter_ForwardStreaming_Uncompressed(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1}
	entries := make([][2]string, 10)
	for i := range entries {
		entries[i] = [2]string{fmt.Sprintf("k%02d", i), fmt.Sprintf("v%02d", i)}
	}
	buf := buildLeaf(t, cfg, entries)
	r := NewLeafReader(buf, cfg)
	it := r.IterForReuse(nil, nil, nil)
	got := 0
	for e, ok := it.Next(); ok; e, ok = it.Next() {
		want := entries[got]
		if !bytes.Equal(e.Key, []byte(want[0])) {
			t.Errorf("Next[%d]: key=%q want=%q", got, e.Key, want[0])
		}
		if !bytes.Equal(e.Value, []byte(want[1])) {
			t.Errorf("Next[%d]: value=%q want=%q", got, e.Value, want[1])
		}
		got++
	}
	if got != len(entries) {
		t.Errorf("yield = %d want %d", got, len(entries))
	}
}

func TestLeafIter_BackwardBuffered_Compressed(t *testing.T) {
	// Walk a compressed leaf forward to the last entry, then backward
	// to entry 0. Pin: every Prev returns the structurally-correct
	// entry in reverse order, the buffered-mode transition handles
	// group-boundary crossings, and the same key bytes round-trip.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	entries := make([][2]string, 16)
	for i := range entries {
		entries[i] = [2]string{fmt.Sprintf("shared-prefix-%03d", i), fmt.Sprintf("v%03d", i)}
	}
	buf := buildLeaf(t, cfg, entries)
	r := NewLeafReader(buf, cfg)

	// Start at the last entry via IterAtForReuse(count) + At(last).
	it := r.IterAtForReuse(len(entries), nil, nil, nil)
	last, ok := it.At(len(entries) - 1)
	if !ok {
		t.Fatalf("At(last): !ok")
	}
	if !bytes.Equal(last.Key, []byte(entries[len(entries)-1][0])) {
		t.Errorf("At(last) key = %q, want %q", last.Key, entries[len(entries)-1][0])
	}

	// Walk backward.
	for i := len(entries) - 1; i > 0; i-- {
		e, ok := it.Prev()
		if !ok {
			t.Fatalf("Prev at index %d: !ok", i-1)
		}
		want := entries[i-1]
		if !bytes.Equal(e.Key, []byte(want[0])) {
			t.Errorf("Prev[%d]: key=%q want=%q", i-1, e.Key, want[0])
		}
		if !bytes.Equal(e.Value, []byte(want[1])) {
			t.Errorf("Prev[%d]: value=%q want=%q", i-1, e.Value, want[1])
		}
	}

	// One more Prev should fail (at first entry).
	if _, ok := it.Prev(); ok {
		t.Error("Prev past first entry: returned ok=true; want false")
	}
}

func TestLeafIter_BackwardBuffered_Uncompressed(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1}
	entries := make([][2]string, 10)
	for i := range entries {
		entries[i] = [2]string{fmt.Sprintf("k%02d", i), fmt.Sprintf("v%02d", i)}
	}
	buf := buildLeaf(t, cfg, entries)
	r := NewLeafReader(buf, cfg)
	it := r.IterAtForReuse(len(entries), nil, nil, nil)
	if _, ok := it.At(len(entries) - 1); !ok {
		t.Fatalf("At(last): !ok")
	}
	for i := len(entries) - 1; i > 0; i-- {
		e, ok := it.Prev()
		if !ok {
			t.Fatalf("Prev at %d: !ok", i-1)
		}
		want := entries[i-1]
		if !bytes.Equal(e.Key, []byte(want[0])) {
			t.Errorf("Prev[%d]: key=%q want=%q", i-1, e.Key, want[0])
		}
	}
	if _, ok := it.Prev(); ok {
		t.Error("Prev past first entry returned ok=true")
	}
}

// ---------------------------------------------------------------------------
// SearchLeafIter — cursor seed
// ---------------------------------------------------------------------------

func TestSearchLeafIter_ExactMatch_Compressed(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	entries := make([][2]string, 16)
	for i := range entries {
		entries[i] = [2]string{fmt.Sprintf("k-%03d", i), fmt.Sprintf("v%03d", i)}
	}
	buf := buildLeaf(t, cfg, entries)
	r := NewLeafReader(buf, cfg)

	for i, e := range entries {
		idx, ent, found, it := r.SearchLeafIter([]byte(e[0]), nil, nil, nil)
		if !found {
			t.Errorf("SearchLeafIter(%q): not found", e[0])
			continue
		}
		if idx != i {
			t.Errorf("SearchLeafIter(%q): idx=%d want=%d", e[0], idx, i)
		}
		if !bytes.Equal(ent.Value, []byte(e[1])) {
			t.Errorf("SearchLeafIter(%q): value=%q want=%q", e[0], ent.Value, e[1])
		}
		// The iter should be positioned past idx — next Next is idx+1.
		if i+1 < len(entries) {
			nxt, ok := it.Next()
			if !ok {
				t.Errorf("SearchLeafIter(%q): Next after match: !ok", e[0])
				continue
			}
			wantNext := entries[i+1]
			if !bytes.Equal(nxt.Key, []byte(wantNext[0])) {
				t.Errorf("SearchLeafIter(%q): Next.Key=%q want=%q", e[0], nxt.Key, wantNext[0])
			}
		}
	}
}

func TestSearchLeafIter_Successor_Compressed(t *testing.T) {
	// Seek to a key that doesn't exist; iter should land on the
	// successor's index and the returned entry should be the successor.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	entries := make([][2]string, 16)
	for i := range entries {
		// even numbers only — odd-numbered targets are misses with a
		// well-defined successor.
		entries[i] = [2]string{fmt.Sprintf("k-%03d", i*2), fmt.Sprintf("v%03d", i)}
	}
	buf := buildLeaf(t, cfg, entries)
	r := NewLeafReader(buf, cfg)
	for i := range entries {
		target := fmt.Sprintf("k-%03d", i*2+1) // between entries[i] and entries[i+1]
		idx, ent, found, _ := r.SearchLeafIter([]byte(target), nil, nil, nil)
		if found {
			t.Errorf("SearchLeafIter(%q): unexpectedly found", target)
			continue
		}
		wantIdx := i + 1
		if wantIdx >= len(entries) {
			if idx != len(entries) {
				t.Errorf("SearchLeafIter(%q): idx=%d want=%d (past end)", target, idx, len(entries))
			}
			continue
		}
		if idx != wantIdx {
			t.Errorf("SearchLeafIter(%q): idx=%d want=%d", target, idx, wantIdx)
			continue
		}
		if !bytes.Equal(ent.Key, []byte(entries[wantIdx][0])) {
			t.Errorf("SearchLeafIter(%q): ent.Key=%q want=%q", target, ent.Key, entries[wantIdx][0])
		}
	}
}

func TestSearchLeafIter_Empty(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	b.Finish()
	r := NewLeafReader(buf, cfg)
	idx, _, found, _ := r.SearchLeafIter([]byte("k"), nil, nil, nil)
	if found || idx != 0 {
		t.Errorf("SearchLeafIter on empty: idx=%d found=%v want=(0,false)", idx, found)
	}
}

// ---------------------------------------------------------------------------
// Overflow entries
// ---------------------------------------------------------------------------

func TestLeafBuilder_OverflowEntry_Compressed(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	b.AddInline([]byte("a"), []byte("inline-A"))
	b.AddOverflow([]byte("b-big"), 42, 100000)
	b.AddInline([]byte("c"), []byte("inline-C"))
	b.Finish()

	r := NewLeafReader(buf, cfg)
	_, ent, found := r.SearchLeaf([]byte("b-big"))
	if !found {
		t.Fatal("SearchLeaf(b-big): not found")
	}
	if !ent.IsOverflow() {
		t.Errorf("ent.IsOverflow() = false; want true")
	}
	if ent.OverflowPage != 42 || ent.TotalLen != 100000 {
		t.Errorf("overflow fields: page=%d totalLen=%d want=(42, 100000)", ent.OverflowPage, ent.TotalLen)
	}
}

// ---------------------------------------------------------------------------
// LastKey + FirstKey
// ---------------------------------------------------------------------------

func TestLeafReader_FirstLast_Compressed(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	entries := [][2]string{
		{"alpha", "1"}, {"beta", "2"}, {"gamma", "3"}, {"omega", "4"}, {"zeta", "5"},
	}
	buf := buildLeaf(t, cfg, entries)
	r := NewLeafReader(buf, cfg)
	if got := r.FirstKey(); !bytes.Equal(got, []byte("alpha")) {
		t.Errorf("FirstKey = %q, want alpha", got)
	}
	last, _ := r.LastKey(nil)
	if !bytes.Equal(last, []byte("zeta")) {
		t.Errorf("LastKey = %q, want zeta", last)
	}
}

func TestLeafReader_FirstLast_Uncompressed(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1}
	entries := [][2]string{
		{"a", "1"}, {"b", "2"}, {"c", "3"}, {"d", "4"},
	}
	buf := buildLeaf(t, cfg, entries)
	r := NewLeafReader(buf, cfg)
	if got := r.FirstKey(); !bytes.Equal(got, []byte("a")) {
		t.Errorf("FirstKey = %q, want a", got)
	}
	last, _ := r.LastKey(nil)
	if !bytes.Equal(last, []byte("d")) {
		t.Errorf("LastKey = %q, want d", last)
	}
}

// ---------------------------------------------------------------------------
// Determinism — same input, same bytes
// ---------------------------------------------------------------------------

func TestLeafBuilder_DeterministicEncoding(t *testing.T) {
	// page-formats.md §Leaf Split deterministic-encoding invariant:
	// same encoder version + same input + same Config → byte-identical
	// pages. Cover both shared-prefix (compression dense) and
	// mixed-prefix (natural-break heuristic fires) inputs to pin the
	// invariant across the builder's branches.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}

	sharedPrefix := make([][2]string, 30)
	for i := range sharedPrefix {
		sharedPrefix[i] = [2]string{fmt.Sprintf("k-%05d", i), fmt.Sprintf("v-%05d", i)}
	}
	t.Run("shared-prefix", func(t *testing.T) {
		a := buildLeaf(t, cfg, sharedPrefix)
		b := buildLeaf(t, cfg, sharedPrefix)
		if !bytes.Equal(a, b) {
			t.Errorf("non-deterministic encoding on shared-prefix input")
		}
	})

	// Mixed prefixes — exercises the natural-break code path
	// (sharedPrefixLen == 0 between adjacent keys forces a fresh
	// group). If the builder's group-split decision were
	// nondeterministic (e.g., depended on a map iteration order or
	// time), this test would catch it.
	mixedPrefix := [][2]string{
		{"aaa-1", "v1"}, {"aaa-2", "v2"},
		{"bbb-1", "v3"}, {"bbb-2", "v4"},
		{"ccc-1", "v5"}, {"ccc-2", "v6"},
		{"ddd-1", "v7"}, {"ddd-2", "v8"},
	}
	t.Run("mixed-prefix-natural-breaks", func(t *testing.T) {
		a := buildLeaf(t, cfg, mixedPrefix)
		b := buildLeaf(t, cfg, mixedPrefix)
		if !bytes.Equal(a, b) {
			t.Errorf("non-deterministic encoding on mixed-prefix input")
		}
	})

	// Uncompressed variant determinism.
	t.Run("uncompressed", func(t *testing.T) {
		ucCfg := Config{PageSize: 4096, RestartGroupTarget: 1}
		a := buildLeaf(t, ucCfg, sharedPrefix)
		b := buildLeaf(t, ucCfg, sharedPrefix)
		if !bytes.Equal(a, b) {
			t.Errorf("non-deterministic encoding on uncompressed leaf")
		}
	})
}

// ---------------------------------------------------------------------------
// Validate — spec invariants on read
// ---------------------------------------------------------------------------

func TestLeafReader_Validate_AcceptsWellFormed(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		keys [][2]string
	}{
		{"compressed-shared", Config{PageSize: 4096, RestartGroupTarget: 4},
			[][2]string{{"aaa-1", "v"}, {"aaa-2", "v"}, {"aaa-3", "v"}}},
		{"compressed-natural-breaks", Config{PageSize: 4096, RestartGroupTarget: 16},
			[][2]string{{"aaa", "v"}, {"bbb", "v"}, {"ccc", "v"}}},
		{"uncompressed", Config{PageSize: 4096, RestartGroupTarget: 1},
			[][2]string{{"a", "1"}, {"b", "2"}, {"c", "3"}}},
		{"empty-compressed", Config{PageSize: 4096, RestartGroupTarget: 4}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := make([]byte, tc.cfg.PageSize)
			b := NewLeafBuilder(buf, tc.cfg)
			for _, e := range tc.keys {
				b.AddInline([]byte(e[0]), []byte(e[1]))
			}
			b.Finish()
			r := NewLeafReader(buf, tc.cfg)
			if err := r.Validate(); err != nil {
				t.Errorf("Validate on well-formed %s: %v", tc.name, err)
			}
		})
	}
}

func TestLeafReader_Validate_RejectsCountZero(t *testing.T) {
	// Forge a restart-table Count byte to 0 on a compressed leaf —
	// per page-formats.md §Compressed Leaf invariant, this is
	// structural corruption.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	entries := [][2]string{{"a-1", "v"}, {"a-2", "v"}, {"b-1", "v"}, {"b-2", "v"}}
	buf := buildLeaf(t, cfg, entries)
	r := NewLeafReader(buf, cfg)
	// Restart table starts at ContentEnd - RestartCount*4. Zero the
	// Count byte of group 0 (which sits at table[0] + offset 2).
	tableStart := cfg.ContentEnd() - r.rt.RestartCount()*restartTableEntrySize
	buf[tableStart+2] = 0
	r2 := NewLeafReader(buf, cfg)
	err := r2.Validate()
	if err == nil {
		t.Fatal("Validate accepted Count==0 restart-table entry; want ErrCorrupted")
	}
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("Validate returned %v; want errors.Is(ErrCorrupted)", err)
	}
}

func TestLeafReader_Validate_RejectsUnknownCellFlags(t *testing.T) {
	// Forge a CellFlags byte to set bit 7 (reserved). Per
	// file-layout.md §Reserved-byte policy + page-formats.md §Leaf
	// Page, unknown CellFlags bits are strict-reject.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1}
	buf := buildLeaf(t, cfg, [][2]string{{"alpha", "1"}, {"beta", "2"}})
	r := NewLeafReader(buf, cfg)
	off := r.ucOffset(0)
	buf[off] = 0x80 // bit 7 — outside cellFlagKnownMask
	err := NewLeafReader(buf, cfg).Validate()
	if err == nil {
		t.Fatal("Validate accepted unknown CellFlags; want ErrCorrupted")
	}
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("Validate returned %v; want errors.Is(ErrCorrupted)", err)
	}
}

func TestLeafReader_Validate_RejectsCompressedCellFlags(t *testing.T) {
	// Same as the uncompressed case, but on a compressed leaf — both
	// restart and delta entries are walked by Validate. Forge the
	// restart entry's flags.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	buf := buildLeaf(t, cfg, [][2]string{{"k-1", "v"}, {"k-2", "v"}, {"k-3", "v"}})
	r := NewLeafReader(buf, cfg)
	off := r.rt.Offset(0)
	buf[off] = 0x40 // arbitrary reserved bit
	err := NewLeafReader(buf, cfg).Validate()
	if err == nil {
		t.Fatal("Validate accepted unknown CellFlags in compressed restart; want ErrCorrupted")
	}
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("Validate returned %v; want errors.Is(ErrCorrupted)", err)
	}
}

// TestLeafReader_Validate_TotalOverInput is the load-bearing
// "Validate is total over arbitrary input" probe — for each forged
// length field, Validate must return ErrCorrupted, NOT panic with a
// slice-out-of-range. The corresponding panics in the prior
// (decoder-using) implementation were the Round-2 H finding; this
// test pins the regression boundary.
func TestLeafReader_Validate_TotalOverInput(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	mk := func() []byte {
		// Build a small compressed leaf as the base; tests mutate it.
		return buildLeaf(t, cfg, [][2]string{
			{"alpha", "v1"}, {"beta", "v2"}, {"gamma", "v3"},
		})
	}

	t.Run("compressed-restart-KeyLen-oversize", func(t *testing.T) {
		buf := mk()
		r := NewLeafReader(buf, cfg)
		off := r.rt.Offset(0)
		// Restart-entry KeyLen is at off+1 (after Flags). Forge to 0xFFFF.
		le.PutUint16(buf[off+1:], 0xFFFF)
		err := NewLeafReader(buf, cfg).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("forged restart KeyLen=0xFFFF: err=%v; want ErrCorrupted", err)
		}
	})

	// mkDelta builds a leaf whose first restart group genuinely
	// contains delta entries (shared-prefix keys defeat the
	// natural-break heuristic, which would otherwise give every key
	// its own restart group and leave nothing delta-encoded — a
	// forgery aimed at a "delta" would then corrupt a restart entry
	// and pass for the wrong reason). Returns the page, the first
	// delta's byte offset, and the restart entry's full key.
	mkDelta := func(t *testing.T) ([]byte, int, []byte) {
		t.Helper()
		buf := buildLeaf(t, cfg, [][2]string{
			{"aaa-1", "v"}, {"aaa-2", "v"}, {"aaa-3", "v"},
		})
		r := NewLeafReader(buf, cfg)
		if gc := r.rt.GroupEntryCount(0); gc < 2 {
			t.Fatalf("fixture: group 0 has %d entries; need >= 2 for a real delta", gc)
		}
		restartOff := r.rt.Offset(0)
		re, deltaOff := r.decodeFullKeyEntry(restartOff)
		return buf, deltaOff, re.Key
	}

	t.Run("compressed-delta-UnsharedLen-oversize", func(t *testing.T) {
		buf, deltaOff, _ := mkDelta(t)
		// Delta entry layout: [Flags][SharedLen u16][UnsharedLen u16]...
		// UnsharedLen at deltaOff+3.
		le.PutUint16(buf[deltaOff+3:], 0xFFFF)
		err := NewLeafReader(buf, cfg).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("forged delta UnsharedLen=0xFFFF: err=%v; want ErrCorrupted", err)
		}
	})

	t.Run("compressed-delta-SharedLen-oversize", func(t *testing.T) {
		// SharedLen far beyond any key: decodeDeltaEntry would panic
		// slicing a keyBuf-backed prevKey. Validate must reject, not
		// let read paths hit that panic.
		buf, deltaOff, _ := mkDelta(t)
		// SharedLen at deltaOff+1.
		le.PutUint16(buf[deltaOff+1:], 0xFFFF)
		err := NewLeafReader(buf, cfg).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("forged delta SharedLen=0xFFFF: err=%v; want ErrCorrupted", err)
		}
	})

	// shiftStream moves buf[from:dataEnd) forward by two bytes,
	// bumps DataEnd, and repoints every restart-table offset >= from
	// at its shifted position. Every entry remains VALID at its
	// table-declared offset — only the stream-contiguity rule is
	// broken (the two bytes left behind at `from` are exactly what a
	// streaming reader would decode). Forgeries that land mid-entry
	// get rejected by the CellFlags/bounds checks instead and would
	// leave the contiguity guard untested (a surviving mutation in
	// review round 1 caught precisely that).
	shiftStream := func(t *testing.T, buf []byte, from int) {
		t.Helper()
		r := NewLeafReader(buf, cfg)
		dataEnd := int(le.Uint16(buf[leafOffDataEnd:]))
		tmp := append([]byte(nil), buf[from:dataEnd]...)
		copy(buf[from+2:dataEnd+2], tmp)
		le.PutUint16(buf[leafOffDataEnd:], uint16(dataEnd+2))
		tableStart := cfg.ContentEnd() - r.rt.RestartCount()*restartTableEntrySize
		for g := range r.rt.RestartCount() {
			if off := r.rt.Offset(g); off >= from {
				le.PutUint16(buf[tableStart+g*restartTableEntrySize:], uint16(off+2))
			}
		}
	}

	t.Run("compressed-restart-offset-not-stream-start", func(t *testing.T) {
		// Restart-table Offset(0) must equal the entry-data start:
		// the streaming iterator and FirstKey decode from offset 12
		// unconditionally, so a table pointing at a valid entry
		// deeper in the page leaves the leading bytes (which the
		// stream WILL decode) completely unvalidated.
		buf, _, _ := mkDelta(t)
		shiftStream(t, buf, leafEntryStart)
		err := NewLeafReader(buf, cfg).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("valid entries shifted off stream start: err=%v; want ErrCorrupted", err)
		}
	})

	t.Run("compressed-group-gap", func(t *testing.T) {
		// A second group whose table offset does not equal the end of
		// the first group's walk (gap) must be rejected — the
		// streaming iterator crosses group boundaries by
		// continuation, never via the table.
		buf := buildLeaf(t, cfg, [][2]string{
			{"aaa-1", "v"}, {"aaa-2", "v"},
			{"bbb-1", "v"}, {"bbb-2", "v"}, // natural break → group 1
		})
		r := NewLeafReader(buf, cfg)
		if r.rt.RestartCount() < 2 {
			t.Fatalf("fixture: RestartCount=%d, need >= 2", r.rt.RestartCount())
		}
		shiftStream(t, buf, r.rt.Offset(1))
		err := NewLeafReader(buf, cfg).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("valid group 1 shifted off stream (gap): err=%v; want ErrCorrupted", err)
		}
	})

	t.Run("compressed-DataEnd-trailing-slack", func(t *testing.T) {
		// DataEnd past the stream end must be rejected: the splice
		// paths validate and then APPEND at DataEnd, so slack would
		// place the new entry outside the stream readers decode.
		buf, _, _ := mkDelta(t)
		dataEnd := int(le.Uint16(buf[leafOffDataEnd:]))
		le.PutUint16(buf[leafOffDataEnd:], uint16(dataEnd+9))
		err := NewLeafReader(buf, cfg).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("forged DataEnd+9 (trailing slack): err=%v; want ErrCorrupted", err)
		}
	})

	t.Run("uncompressed-DataEnd-trailing-slack", func(t *testing.T) {
		cfgU := Config{PageSize: 4096, RestartGroupTarget: 1}
		buf := buildLeaf(t, cfgU, [][2]string{{"alpha", "1"}, {"beta", "2"}})
		dataEnd := int(le.Uint16(buf[ucLeafOffDataEnd:]))
		le.PutUint16(buf[ucLeafOffDataEnd:], uint16(dataEnd+9))
		err := NewLeafReader(buf, cfgU).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("forged uc DataEnd+9 (trailing slack): err=%v; want ErrCorrupted", err)
		}
	})

	t.Run("empty-leaf-DataEnd-slack", func(t *testing.T) {
		buf := buildLeaf(t, cfg, nil)
		dataEnd := int(le.Uint16(buf[leafOffDataEnd:]))
		if dataEnd != leafEntryStart {
			t.Fatalf("fixture: empty leaf DataEnd=%d, want %d", dataEnd, leafEntryStart)
		}
		le.PutUint16(buf[leafOffDataEnd:], uint16(leafEntryStart+9))
		err := NewLeafReader(buf, cfg).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("forged empty-leaf DataEnd (slack): err=%v; want ErrCorrupted", err)
		}
	})

	t.Run("uncompressed-offset-not-stream-position", func(t *testing.T) {
		// The uncompressed offset table is positional AND must match
		// the sequential stream: Next() streams from offset 12 by
		// continuation. Point entry 1's slot at entry 0 (valid
		// range, wrong position).
		cfgU := Config{PageSize: 4096, RestartGroupTarget: 1}
		buf := buildLeaf(t, cfgU, [][2]string{{"alpha", "1"}, {"beta", "2"}})
		r := NewLeafReader(buf, cfgU)
		tableStart := cfgU.ContentEnd() - r.count*ucOffsetEntrySize
		le.PutUint16(buf[tableStart+ucOffsetEntrySize:], uint16(r.ucOffset(0)))
		err := NewLeafReader(buf, cfgU).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("forged uc offset[1] -> offset[0]: err=%v; want ErrCorrupted", err)
		}
	})

	t.Run("compressed-delta-SharedLen-exceeds-prev-key", func(t *testing.T) {
		// The silent variant: SharedLen only slightly beyond the
		// previous full key. A page-buffer-backed prevKey has spare
		// capacity, so decode would not panic — it would fabricate a
		// key from adjacent page bytes. Validate must reject per the
		// page-formats.md delta-reconstruction invariant.
		buf, deltaOff, restartKey := mkDelta(t)
		le.PutUint16(buf[deltaOff+1:], uint16(len(restartKey)+1))
		err := NewLeafReader(buf, cfg).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("forged delta SharedLen=len(prevKey)+1: err=%v; want ErrCorrupted", err)
		}
	})

	t.Run("compressed-header-RestartCount-oversize", func(t *testing.T) {
		buf := mk()
		// RestartCount is at offset 8 (leafOffRestartCount). Forge to
		// 0xFFFF — newRestartTable's tableOff computation would go
		// negative; Validate must catch and return ErrCorrupted.
		le.PutUint16(buf[leafOffRestartCount:], 0xFFFF)
		err := NewLeafReader(buf, cfg).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("forged RestartCount=0xFFFF: err=%v; want ErrCorrupted", err)
		}
	})

	t.Run("uncompressed-KeyLen-oversize", func(t *testing.T) {
		ucCfg := Config{PageSize: 4096, RestartGroupTarget: 1}
		buf := buildLeaf(t, ucCfg, [][2]string{{"alpha", "v1"}, {"beta", "v2"}})
		r := NewLeafReader(buf, ucCfg)
		off := r.ucOffset(0)
		// UC entry: [Flags][KeyLen u16]... so KeyLen at off+1.
		le.PutUint16(buf[off+1:], 0xFFFF)
		err := NewLeafReader(buf, ucCfg).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("forged UC KeyLen=0xFFFF: err=%v; want ErrCorrupted", err)
		}
	})

	t.Run("uncompressed-ValueLen-oversize", func(t *testing.T) {
		ucCfg := Config{PageSize: 4096, RestartGroupTarget: 1}
		buf := buildLeaf(t, ucCfg, [][2]string{{"alpha", "v1"}, {"beta", "v2"}})
		r := NewLeafReader(buf, ucCfg)
		off := r.ucOffset(0)
		// UC inline entry: [Flags][KeyLen u16][ValueLen u32]...
		// ValueLen at off+3.
		le.PutUint32(buf[off+3:], 0xFFFFFFFF)
		err := NewLeafReader(buf, ucCfg).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("forged UC ValueLen=0xFFFFFFFF: err=%v; want ErrCorrupted", err)
		}
	})

	t.Run("compressed-DataEnd-out-of-range", func(t *testing.T) {
		buf := mk()
		// DataEnd at offset 10 (leafOffDataEnd). Forge to 0xFFFF.
		le.PutUint16(buf[leafOffDataEnd:], 0xFFFF)
		err := NewLeafReader(buf, cfg).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("forged DataEnd=0xFFFF: err=%v; want ErrCorrupted", err)
		}
	})

	t.Run("compressed-sum-counts-mismatch", func(t *testing.T) {
		buf := mk()
		r := NewLeafReader(buf, cfg)
		// Bump every group's Count by 1 — sum will exceed header
		// Count, triggering the sum-of-counts check.
		tableStart := cfg.ContentEnd() - r.rt.RestartCount()*restartTableEntrySize
		for g := range r.rt.RestartCount() {
			off := tableStart + g*restartTableEntrySize + 2
			buf[off] = buf[off] + 1
		}
		err := NewLeafReader(buf, cfg).Validate()
		if err == nil || !errors.Is(err, ErrCorrupted) {
			t.Errorf("forged sum-of-counts: err=%v; want ErrCorrupted", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Config validation
// ---------------------------------------------------------------------------

func TestConfig_ValidateRejectsBadRestartGroupTarget(t *testing.T) {
	for _, bad := range []uint16{256, 1000, 65535} {
		cfg := Config{PageSize: 4096, RestartGroupTarget: bad}
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate(RestartGroupTarget=%d): expected error", bad)
		}
	}
	// Valid range.
	for _, ok := range []uint16{0, 1, 2, 16, 255} {
		cfg := Config{PageSize: 4096, RestartGroupTarget: ok}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(RestartGroupTarget=%d): %v; want nil", ok, err)
		}
	}
}

// FuzzLeafValidateTotal pins the trust-boundary contract stated on
// Validate: over arbitrary page bytes, Validate never panics, and any
// page Validate accepts can be fully read (Iter walk + SearchLeaf)
// without panicking. The read paths deliberately skip bounds checks
// (hot path), so this property is exactly what makes Validate a
// sufficient gate for pages resolved from disk.
func FuzzLeafValidateTotal(f *testing.F) {
	cfgC := Config{PageSize: 4096, RestartGroupTarget: 4}
	cfgU := Config{PageSize: 4096, RestartGroupTarget: 1}
	f.Add(buildLeafF(cfgC, [][2]string{{"aaa-1", "v"}, {"aaa-2", "v"}, {"aaa-3", "v"}}), byte(0), uint16(0))
	f.Add(buildLeafF(cfgU, [][2]string{{"alpha", "1"}, {"beta", "2"}}), byte(0), uint16(0))
	f.Fuzz(func(t *testing.T, page []byte, mutByte byte, mutOff uint16) {
		if len(page) == 0 {
			return
		}
		buf := make([]byte, cfgC.PageSize)
		copy(buf, page)
		// One targeted mutation beyond whatever the fuzzer did to the
		// raw bytes — biases the search toward near-valid pages.
		buf[int(mutOff)%len(buf)] ^= mutByte
		// Mirror the production boundary: callers gate on the Type
		// byte before constructing a LeafReader (which panics on
		// non-leaf types by documented contract).
		if typ, _, _, _ := ReadHeader(buf); !IsLeafType(typ) {
			return
		}
		// One cfg suffices: LeafReader never reads RestartGroupTarget
		// (builder-only), and ContentEnd depends only on
		// PageSize/PageChecksum — identical across the seed configs.
		r := NewLeafReader(buf, cfgC)
		if err := r.Validate(); err != nil {
			return
		}
		it := r.IterForReuse(nil, nil, nil)
		for {
			e, ok := it.Next()
			if !ok {
				break
			}
			// Every accepted entry's key must be searchable
			// without panicking (found or not — ordering is not
			// Validate's contract, totality is).
			r.SearchLeaf(e.Key)
		}
	})
}

// buildLeafF is buildLeaf without the testing.T dependency (fuzz seed
// construction runs outside a test context).
func buildLeafF(cfg Config, entries [][2]string) []byte {
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	for _, e := range entries {
		b.AddInline([]byte(e[0]), []byte(e[1]))
	}
	b.Finish()
	return buf
}
