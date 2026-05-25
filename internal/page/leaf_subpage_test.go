package page

// Leaf integration tests for SetKeyspace subpage cells (chunk 6.3).
// Subpage cells carry CellFlagMultiValue && !CellFlagNestedTree —
// per set-keyspace.md §Subpage Format — and use the same on-disk
// shape as a plain inline cell (`[Flags][KeyLen][ValueLen][Key][Value]`)
// with the value half holding raw subpage bytes. Chunk 4's
// LeafBuilder.AddEntry must preserve the CellFlagMultiValue bit so
// every leaf rebuild path (split, merge, delete-then-rebuild,
// in-place-then-rebuild) carries the flag through; otherwise the
// chunk-6.6 SetKeyspace surface would observe its cells silently
// demoted to plain inline cells the next time the parent leaf got
// rewritten.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// makeSubpage builds a small subpage byte slice for use as a leaf
// cell's value half. The 6.2 codec is the producer; this is a thin
// helper that delegates and fails the test on a codec error.
func makeSubpage(t *testing.T, values [][]byte, fvs uint16) []byte {
	t.Helper()
	buf, err := EncodeSubpage(values, fvs)
	if err != nil {
		t.Fatalf("EncodeSubpage: %v", err)
	}
	return buf
}

func TestLeafBuilderAddSubpageRoundTripUncompressed(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1}
	subpage := makeSubpage(t, [][]byte{[]byte("alpha"), []byte("beta")}, 0)
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	if !b.AddSubpage([]byte("topic-1"), subpage) {
		t.Fatalf("AddSubpage returned false (page full)")
	}
	b.Finish()

	r := NewLeafReader(buf, cfg)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if r.Count() != 1 {
		t.Fatalf("Count=%d, want 1", r.Count())
	}
	idx, entry, found := r.SearchLeaf([]byte("topic-1"))
	if !found || idx != 0 {
		t.Fatalf("SearchLeaf: idx=%d found=%v, want (0,true)", idx, found)
	}
	if entry.Flags&CellFlagMultiValue == 0 {
		t.Errorf("Flags=0x%x: CellFlagMultiValue not set after round-trip", entry.Flags)
	}
	if entry.Flags&CellFlagNestedTree != 0 {
		t.Errorf("Flags=0x%x: CellFlagNestedTree leaked through round-trip", entry.Flags)
	}
	// SearchLeaf nils entry.Key on a match — re-fetch via EntryAt for key.
	got, _ := r.EntryAt(0, nil)
	if !bytes.Equal(got.Key, []byte("topic-1")) {
		t.Errorf("EntryAt(0).Key=%q, want %q", got.Key, "topic-1")
	}
	if !bytes.Equal(got.Value, subpage) {
		t.Errorf("EntryAt(0).Value=%x, want %x", got.Value, subpage)
	}
}

func TestLeafBuilderAddSubpageRoundTripCompressedRestart(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	subpage := makeSubpage(t, [][]byte{[]byte("a"), []byte("b"), []byte("c")}, 0)
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	if !b.AddSubpage([]byte("topic-1"), subpage) {
		t.Fatalf("AddSubpage returned false")
	}
	b.Finish()

	r := NewLeafReader(buf, cfg)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got, _ := r.EntryAt(0, nil)
	if got.Flags&CellFlagMultiValue == 0 {
		t.Errorf("Flags=0x%x: CellFlagMultiValue missing on compressed-restart cell", got.Flags)
	}
	if !bytes.Equal(got.Value, subpage) {
		t.Errorf("compressed-restart subpage round-trip Value mismatch")
	}
}

func TestLeafBuilderAddSubpageDelta(t *testing.T) {
	// Two subpage cells in the same restart group — the second is
	// encoded as a delta entry sharing the "topic-" prefix.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	sp1 := makeSubpage(t, [][]byte{[]byte("v1"), []byte("v2")}, 0)
	sp2 := makeSubpage(t, [][]byte{[]byte("v3"), []byte("v4")}, 0)
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	if !b.AddSubpage([]byte("topic-1"), sp1) {
		t.Fatalf("AddSubpage(topic-1) returned false")
	}
	if !b.AddSubpage([]byte("topic-2"), sp2) {
		t.Fatalf("AddSubpage(topic-2) returned false")
	}
	b.Finish()

	r := NewLeafReader(buf, cfg)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got0, _ := r.EntryAt(0, nil)
	got1, _ := r.EntryAt(1, nil)
	if got0.Flags&CellFlagMultiValue == 0 || got1.Flags&CellFlagMultiValue == 0 {
		t.Errorf("MultiValue lost on delta entry: got0=0x%x got1=0x%x", got0.Flags, got1.Flags)
	}
	if !bytes.Equal(got0.Value, sp1) {
		t.Errorf("EntryAt(0).Value mismatch")
	}
	if !bytes.Equal(got1.Value, sp2) {
		t.Errorf("EntryAt(1).Value mismatch")
	}
	if !bytes.Equal(got0.Key, []byte("topic-1")) {
		t.Errorf("EntryAt(0).Key=%q, want topic-1", got0.Key)
	}
	if !bytes.Equal(got1.Key, []byte("topic-2")) {
		t.Errorf("EntryAt(1).Key=%q, want topic-2 (delta reconstruction)", got1.Key)
	}
}

func TestLeafBuilderAddEntryPreservesMultiValue(t *testing.T) {
	// Simulates the chunk-4 leaf rebuild path: decode → modify list →
	// re-encode via AddEntry. The MultiValue flag MUST round-trip;
	// before the chunk-6.3 fix, AddEntry dropped non-Overflow flags
	// and demoted subpage cells to plain inline cells.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	sp := makeSubpage(t, [][]byte{[]byte("alpha"), []byte("beta")}, 0)

	// Build initial leaf with mixed cells (keys in sorted order).
	src := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(src, cfg)
	b.AddInline([]byte("aaa"), []byte("plain-a"))
	b.AddSubpage([]byte("bbb"), sp)
	b.AddInline([]byte("ccc"), []byte("plain-c"))
	b.Finish()

	// Decode all entries via LeafIter (mirrors chunk-4 rebuild paths).
	r := NewLeafReader(src, cfg)
	it := r.IterForReuse(nil, nil, nil)
	var entries []LeafEntry
	for e, ok := it.Next(); ok; e, ok = it.Next() {
		dup := LeafEntry{
			Flags:        e.Flags,
			Key:          append([]byte(nil), e.Key...),
			Value:        append([]byte(nil), e.Value...),
			OverflowPage: e.OverflowPage,
			TotalLen:     e.TotalLen,
		}
		entries = append(entries, dup)
	}

	// Rebuild via AddEntry (the chunk-4 rebuild contract).
	dst := make([]byte, cfg.PageSize)
	b2 := NewLeafBuilder(dst, cfg)
	for _, e := range entries {
		if !b2.AddEntry(e) {
			t.Fatalf("AddEntry returned false during rebuild")
		}
	}
	b2.Finish()

	r2 := NewLeafReader(dst, cfg)
	if err := r2.Validate(); err != nil {
		t.Fatalf("rebuilt leaf Validate: %v", err)
	}
	got0, _ := r2.EntryAt(0, nil)
	got1, _ := r2.EntryAt(1, nil)
	got2, _ := r2.EntryAt(2, nil)
	if got0.Flags != 0 {
		t.Errorf("rebuilt entry 0 Flags=0x%x, want 0 (plain inline)", got0.Flags)
	}
	if got1.Flags&CellFlagMultiValue == 0 {
		t.Errorf("rebuilt entry 1 Flags=0x%x, MultiValue MISSING after rebuild (chunk 6.3 regression)", got1.Flags)
	}
	if got1.Flags&CellFlagNestedTree != 0 {
		t.Errorf("rebuilt entry 1 Flags=0x%x, NestedTree LEAKED on rebuild", got1.Flags)
	}
	if got2.Flags != 0 {
		t.Errorf("rebuilt entry 2 Flags=0x%x, want 0", got2.Flags)
	}
	if !bytes.Equal(got1.Value, sp) {
		t.Errorf("rebuilt entry 1 Value mismatch (subpage bytes corrupted)")
	}
}

// Chunk-6.3 had a TestLeafBuilderAddEntryPanicsOnNestedTree that
// asserted AddEntry panicked on NestedTree cells; chunk 6.4 retires
// the panic by wiring the actual encoding. The round-trip is now
// covered by TestLeafBuilderAddNestedTreeRefRoundTrip below.

func TestLeafBuilderAddNestedTreeRefRoundTripUncompressed(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1}
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	if !b.AddNestedTreeRef([]byte("topic-1"), 42, 1000) {
		t.Fatalf("AddNestedTreeRef returned false")
	}
	b.Finish()

	r := NewLeafReader(buf, cfg)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got, _ := r.EntryAt(0, nil)
	if !got.IsNestedTree() {
		t.Errorf("IsNestedTree=false; Flags=0x%x", got.Flags)
	}
	if got.NestedRoot != 42 {
		t.Errorf("NestedRoot=%d, want 42", got.NestedRoot)
	}
	if got.NestedCount != 1000 {
		t.Errorf("NestedCount=%d, want 1000", got.NestedCount)
	}
	if string(got.Key) != "topic-1" {
		t.Errorf("Key=%q, want topic-1", got.Key)
	}
	if len(got.Value) != 0 {
		t.Errorf("Value=%x, want empty (NestedTree cells have no inline value)", got.Value)
	}
}

func TestLeafBuilderAddNestedTreeRefRoundTripCompressed(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	if !b.AddNestedTreeRef([]byte("topic-1"), 100, 500) {
		t.Fatalf("AddNestedTreeRef(topic-1): false")
	}
	if !b.AddNestedTreeRef([]byte("topic-2"), 200, 800) {
		t.Fatalf("AddNestedTreeRef(topic-2): false")
	}
	b.Finish()

	r := NewLeafReader(buf, cfg)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got0, _ := r.EntryAt(0, nil)
	got1, _ := r.EntryAt(1, nil)
	if !got0.IsNestedTree() || !got1.IsNestedTree() {
		t.Errorf("flags lost: got0=0x%x got1=0x%x", got0.Flags, got1.Flags)
	}
	if got0.NestedRoot != 100 || got0.NestedCount != 500 {
		t.Errorf("entry 0 Root/Count = %d/%d, want 100/500", got0.NestedRoot, got0.NestedCount)
	}
	if got1.NestedRoot != 200 || got1.NestedCount != 800 {
		t.Errorf("entry 1 Root/Count = %d/%d, want 200/800 (delta-encoded)", got1.NestedRoot, got1.NestedCount)
	}
	if string(got1.Key) != "topic-2" {
		t.Errorf("entry 1 Key=%q, want topic-2 (delta reconstruction)", got1.Key)
	}
}

func TestLeafBuilderAddEntryPreservesNestedTree(t *testing.T) {
	// Decode → modify → re-encode via AddEntry must preserve the
	// NestedTree flag + (NestedRoot, NestedCount) fields. Mirrors
	// TestLeafBuilderAddEntryPreservesMultiValue but exercises the
	// 16-byte-trailer write path.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	src := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(src, cfg)
	b.AddInline([]byte("aaa"), []byte("plain-a"))
	b.AddNestedTreeRef([]byte("bbb"), 42, 100)
	b.AddInline([]byte("ccc"), []byte("plain-c"))
	b.Finish()

	r := NewLeafReader(src, cfg)
	it := r.IterForReuse(nil, nil, nil)
	var entries []LeafEntry
	for e, ok := it.Next(); ok; e, ok = it.Next() {
		entries = append(entries, LeafEntry{
			Flags:        e.Flags,
			Key:          append([]byte(nil), e.Key...),
			Value:        append([]byte(nil), e.Value...),
			OverflowPage: e.OverflowPage,
			TotalLen:     e.TotalLen,
			NestedRoot:   e.NestedRoot,
			NestedCount:  e.NestedCount,
		})
	}

	dst := make([]byte, cfg.PageSize)
	b2 := NewLeafBuilder(dst, cfg)
	for _, e := range entries {
		if !b2.AddEntry(e) {
			t.Fatalf("AddEntry returned false during rebuild")
		}
	}
	b2.Finish()

	r2 := NewLeafReader(dst, cfg)
	if err := r2.Validate(); err != nil {
		t.Fatalf("rebuilt Validate: %v", err)
	}
	got1, _ := r2.EntryAt(1, nil)
	if !got1.IsNestedTree() {
		t.Errorf("rebuilt entry 1 lost NestedTree: Flags=0x%x", got1.Flags)
	}
	if got1.NestedRoot != 42 || got1.NestedCount != 100 {
		t.Errorf("rebuilt entry 1 (Root,Count)=(%d,%d), want (42,100)",
			got1.NestedRoot, got1.NestedCount)
	}
}

func TestLeafValidateAcceptsNestedTreeCells(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	b.AddNestedTreeRef([]byte("k1"), 10, 20)
	b.AddNestedTreeRef([]byte("k2"), 30, 40)
	b.Finish()
	r := NewLeafReader(buf, cfg)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate rejected a valid nested-tree-cell leaf: %v", err)
	}
}

func TestLeafBuilderAddEntryPanicsOnOverflowMultiValue(t *testing.T) {
	// CellFlagOverflow | CellFlagMultiValue is declared mutually
	// exclusive by page-formats.md §CellFlags but the chunk-4
	// AddOverflow + AddSubpage paths each encode only one of the
	// two flag bits, so the combination has no defined on-disk
	// encoding. AddEntry rejects it explicitly so a caller's flag-
	// construction bug fails loud rather than silently writing a
	// half-encoded cell.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("AddEntry on Overflow|MultiValue cell did not panic")
			return
		}
		if !strings.Contains(fmt.Sprint(r), "mutually exclusive") {
			t.Errorf("panic message missing 'mutually exclusive' marker: %v", r)
		}
	}()
	b.AddEntry(LeafEntry{
		Flags:        CellFlagOverflow | CellFlagMultiValue,
		Key:          []byte("k"),
		OverflowPage: 42,
		TotalLen:     1000,
	})
}

func TestLeafSplitPreservesSubpageCells(t *testing.T) {
	// End-to-end: build a leaf full of mixed plain + subpage cells,
	// simulate a chunk-4 split by decoding all entries and rebuilding
	// two leaves (left half / right half), then verify every subpage
	// cell survives with its Flags + bytes intact.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 8}
	sp := makeSubpage(t, [][]byte{[]byte("v1"), []byte("v2"), []byte("v3")}, 0)

	src := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(src, cfg)
	// Interleave plain + subpage cells. Keep keys in sorted order.
	type kind int
	const (
		plain kind = iota
		subp
	)
	mix := []struct {
		key  string
		kind kind
	}{
		{"alpha-01", plain},
		{"alpha-02", subp},
		{"alpha-03", plain},
		{"alpha-04", subp},
		{"alpha-05", plain},
		{"alpha-06", subp},
	}
	for _, e := range mix {
		switch e.kind {
		case plain:
			if !b.AddInline([]byte(e.key), []byte("plain-value-"+e.key)) {
				t.Fatalf("AddInline(%q) returned false", e.key)
			}
		case subp:
			if !b.AddSubpage([]byte(e.key), sp) {
				t.Fatalf("AddSubpage(%q) returned false", e.key)
			}
		}
	}
	b.Finish()

	// Decode entries.
	r := NewLeafReader(src, cfg)
	it := r.IterForReuse(nil, nil, nil)
	var entries []LeafEntry
	for e, ok := it.Next(); ok; e, ok = it.Next() {
		entries = append(entries, LeafEntry{
			Flags: e.Flags,
			Key:   append([]byte(nil), e.Key...),
			Value: append([]byte(nil), e.Value...),
		})
	}

	// Simulate a midpoint split.
	mid := len(entries) / 2
	leftEntries := entries[:mid]
	rightEntries := entries[mid:]

	leftBuf := make([]byte, cfg.PageSize)
	lb := NewLeafBuilder(leftBuf, cfg)
	for _, e := range leftEntries {
		if !lb.AddEntry(e) {
			t.Fatalf("left AddEntry returned false")
		}
	}
	lb.Finish()

	rightBuf := make([]byte, cfg.PageSize)
	rb := NewLeafBuilder(rightBuf, cfg)
	for _, e := range rightEntries {
		if !rb.AddEntry(e) {
			t.Fatalf("right AddEntry returned false")
		}
	}
	rb.Finish()

	// Verify both halves: every subpage cell still has MultiValue and
	// its bytes match.
	for half, halfBuf := range map[string][]byte{"left": leftBuf, "right": rightBuf} {
		rr := NewLeafReader(halfBuf, cfg)
		if err := rr.Validate(); err != nil {
			t.Fatalf("%s Validate: %v", half, err)
		}
		var halfEnts []LeafEntry
		switch half {
		case "left":
			halfEnts = leftEntries
		case "right":
			halfEnts = rightEntries
		}
		for i, want := range halfEnts {
			got, _ := rr.EntryAt(i, nil)
			if got.Flags != want.Flags {
				t.Errorf("%s entry %d Flags=0x%x, want 0x%x", half, i, got.Flags, want.Flags)
			}
			if !bytes.Equal(got.Value, want.Value) {
				t.Errorf("%s entry %d Value diverged after split-and-rebuild", half, i)
			}
		}
	}
}

func TestLeafValidateRejectsIllegalFlagCombos(t *testing.T) {
	// Validate is the trust boundary the chunk-4 rebuild paths depend
	// on — every decoded LeafEntry feeding LeafBuilder.AddEntry must
	// carry a flag combination the builder has an encoding for, OR
	// Validate must surface ErrCorrupted at the boundary so callers
	// can map to a recoverable error rather than panic-the-process
	// mid-rebuild.
	//
	// Two combinations are spec-illegal and Validate must reject:
	//   - Overflow | MultiValue: mutually exclusive per
	//     page-formats.md §Leaf Page (CellFlags bit layout).
	//   - NestedTree without MultiValue: NestedTree is defined as
	//     meaningful only when MultiValue is also set.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1}

	t.Run("Overflow|MultiValue UC entry", func(t *testing.T) {
		// Hand-craft an UC leaf with one entry carrying both flags.
		buf := make([]byte, cfg.PageSize)
		WriteHeader(buf, TypeLeafUncompressed, 1, 0)
		off := leafEntryStart
		buf[off] = CellFlagOverflow | CellFlagMultiValue
		off++
		le.PutUint16(buf[off:], 1) // KeyLen
		off += 2
		buf[off] = 'a'
		off++
		le.PutUint64(buf[off:], 42) // OvflPage
		off += 8
		le.PutUint64(buf[off:], 100) // TotalLen
		off += 8
		le.PutUint16(buf[ucLeafOffDataEnd:], uint16(off))
		// Offset table at the page tail.
		contentEnd := cfg.ContentEnd()
		tableOff := contentEnd - ucOffsetEntrySize
		le.PutUint16(buf[tableOff:], uint16(leafEntryStart))
		r := NewLeafReader(buf, cfg)
		err := r.Validate()
		if err == nil {
			t.Errorf("Validate accepted Overflow|MultiValue; want ErrCorrupted")
		} else if !strings.Contains(err.Error(), "mutually exclusive") {
			t.Errorf("Validate err=%v, want substring 'mutually exclusive'", err)
		}
	})

	t.Run("NestedTree without MultiValue UC entry", func(t *testing.T) {
		buf := make([]byte, cfg.PageSize)
		WriteHeader(buf, TypeLeafUncompressed, 1, 0)
		off := leafEntryStart
		buf[off] = CellFlagNestedTree // illegal: NestedTree without MultiValue
		off++
		le.PutUint16(buf[off:], 1) // KeyLen
		off += 2
		le.PutUint32(buf[off:], 0) // ValueLen (inline path)
		off += 4
		buf[off] = 'a'
		off++
		le.PutUint16(buf[ucLeafOffDataEnd:], uint16(off))
		contentEnd := cfg.ContentEnd()
		tableOff := contentEnd - ucOffsetEntrySize
		le.PutUint16(buf[tableOff:], uint16(leafEntryStart))
		r := NewLeafReader(buf, cfg)
		err := r.Validate()
		if err == nil {
			t.Errorf("Validate accepted NestedTree-without-MultiValue; want ErrCorrupted")
		} else if !strings.Contains(err.Error(), "only valid when MultiValue") {
			t.Errorf("Validate err=%v, want substring 'only valid when MultiValue'", err)
		}
	})
}

func TestLeafValidateAcceptsSubpageCells(t *testing.T) {
	// Defense-in-depth: a leaf with MultiValue cells passes Validate
	// (the cellFlagKnownMask already includes CellFlagMultiValue but
	// pin a regression test so a future cellFlagKnownMask narrowing
	// would fail loudly).
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	sp := makeSubpage(t, [][]byte{[]byte("x"), []byte("y")}, 0)
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	b.AddSubpage([]byte("k1"), sp)
	b.AddSubpage([]byte("k2"), sp)
	b.Finish()
	r := NewLeafReader(buf, cfg)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate rejected a valid subpage-cell leaf: %v", err)
	}
}

func TestLeafIterPreservesSubpageFlagAcrossNext(t *testing.T) {
	// LeafIter is the chunk-4 cursor's leaf-walking primitive. The
	// MultiValue flag must survive forward streaming (compressed
	// delta-decode) so the chunk-6.7 SetCursor can dispatch on
	// CellFlags to know whether the cell is a subpage or a regular
	// inline cell.
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	sp := makeSubpage(t, [][]byte{[]byte("v1"), []byte("v2")}, 0)
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	b.AddInline([]byte("a"), []byte("av"))
	b.AddSubpage([]byte("b"), sp)
	b.AddInline([]byte("c"), []byte("cv"))
	b.AddSubpage([]byte("d"), sp)
	b.Finish()

	r := NewLeafReader(buf, cfg)
	it := r.IterForReuse(nil, nil, nil)
	wantFlags := []uint8{0, CellFlagMultiValue, 0, CellFlagMultiValue}
	wantKeys := []string{"a", "b", "c", "d"}
	for i := range 4 {
		e, ok := it.Next()
		if !ok {
			t.Fatalf("iter exhausted at %d", i)
		}
		if e.Flags != wantFlags[i] {
			t.Errorf("entry %d Flags=0x%x, want 0x%x", i, e.Flags, wantFlags[i])
		}
		if string(e.Key) != wantKeys[i] {
			t.Errorf("entry %d Key=%q, want %q", i, e.Key, wantKeys[i])
		}
	}
}

func TestLeafIterPreservesSubpageFlagAcrossPrev(t *testing.T) {
	// Same as above but for backward streaming via Prev (which on
	// compressed pages triggers buffered-mode group decode).
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	sp := makeSubpage(t, [][]byte{[]byte("v1"), []byte("v2")}, 0)
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	b.AddInline([]byte("a"), []byte("av"))
	b.AddSubpage([]byte("b"), sp)
	b.AddInline([]byte("c"), []byte("cv"))
	b.AddSubpage([]byte("d"), sp)
	b.Finish()

	r := NewLeafReader(buf, cfg)
	// Position at the last entry via IterAtForReuse(count) + At(last),
	// matching the existing TestLeafIter_BackwardBuffered pattern. At
	// returns the entry at idx AND sets it.idx = idx+1, so subsequent
	// Prev steps backward.
	it := r.IterAtForReuse(r.Count(), nil, nil, nil)
	last, ok := it.At(r.Count() - 1)
	if !ok {
		t.Fatalf("At(last): !ok")
	}
	if last.Flags != CellFlagMultiValue {
		t.Errorf("At(last) Flags=0x%x, want CellFlagMultiValue", last.Flags)
	}
	if string(last.Key) != "d" {
		t.Errorf("At(last) Key=%q, want d", last.Key)
	}
	// Walk backward: Prev returns c, b, a (in that order).
	wantFlags := []uint8{0, CellFlagMultiValue, 0}
	wantKeys := []string{"c", "b", "a"}
	for i := range 3 {
		e, ok := it.Prev()
		if !ok {
			t.Fatalf("Prev exhausted at %d", i)
		}
		if e.Flags != wantFlags[i] {
			t.Errorf("Prev %d Flags=0x%x, want 0x%x", i, e.Flags, wantFlags[i])
		}
		if string(e.Key) != wantKeys[i] {
			t.Errorf("Prev %d Key=%q, want %q", i, e.Key, wantKeys[i])
		}
	}
}
