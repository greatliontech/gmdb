package page

import (
	"bytes"
	"fmt"
	"testing"
)

// Tests for the no-decode compressed-leaf group split (leaf_split.go). The split
// is CANONICAL: each half is byte-identical to a LeafBuilder rebuild of its entry
// subset (we carve at a builder-produced group boundary). The oracle is that
// byte-identity, plus: SplitLeafRightHalf is READ-ONLY on src (the decline-safety
// the put fast path relies on — a truncated src would corrupt the decode-split
// fallback), free space stays zeroed, and the two halves partition the entries.

// assertSplitMatchesRebuild builds a page from entries, splits it at splitGroup,
// and asserts: SplitLeafRightHalf leaves src byte-unchanged and yields a right
// half byte-identical to a rebuild of entries[leftCount:]; TruncateLeafToGroups
// then yields a left half byte-identical to a rebuild of entries[:leftCount];
// both halves pass Validate with zeroed free space; counts partition the input.
func assertSplitMatchesRebuild(t *testing.T, cfg Config, entries []LeafEntry, splitGroup int) {
	t.Helper()
	orig, ok := tryBuild(cfg, entries)
	if !ok {
		t.Fatalf("base (%d entries) did not fit", len(entries))
	}
	origCopy := bytes.Clone(orig)
	src := bytes.Clone(orig)
	dst := make([]byte, cfg.PageSize)

	leftCount, rightCount := SplitLeafRightHalf(src, dst, cfg, splitGroup)
	if !bytes.Equal(src, origCopy) {
		t.Fatalf("SplitLeafRightHalf mutated src (must be read-only): splitGroup=%d", splitGroup)
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

	if tlc := TruncateLeafToGroups(src, cfg, splitGroup); tlc != leftCount {
		t.Fatalf("TruncateLeafToGroups leftCount=%d, want %d", tlc, leftCount)
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

func TestSplitLeafAtGroup_MatchesRebuild(t *testing.T) {
	mk := func(k, v string) LeafEntry { return LeafEntry{Key: []byte(k), Value: []byte(v)} }

	t.Run("shared-prefix groups (RGT=4)", func(t *testing.T) {
		cfg := Config{PageSize: 4096, RestartGroupTarget: 4, LeafLayout: LeafLayoutInterleaved}
		var entries []LeafEntry
		for i := range 12 { // 3 groups of 4 (all share "key-")
			entries = append(entries, mk(fmt.Sprintf("key-%04d", i), "v"))
		}
		rc := NewLeafReader(mustBuild(t, cfg, entries), cfg).RestartCount()
		if rc != 3 {
			t.Fatalf("RestartCount = %d, want 3", rc)
		}
		for g := 1; g < rc; g++ {
			assertSplitMatchesRebuild(t, cfg, entries, g)
		}
	})

	t.Run("natural-break single-entry groups", func(t *testing.T) {
		cfg := Config{PageSize: 4096, RestartGroupTarget: 16, LeafLayout: LeafLayoutInterleaved}
		// No shared prefixes → every entry its own group.
		entries := []LeafEntry{mk("aaa", "1"), mk("bbb", "2"), mk("ccc", "3"), mk("ddd", "4")}
		buf := mustBuild(t, cfg, entries)
		if rc := NewLeafReader(buf, cfg).RestartCount(); rc != 4 {
			t.Fatalf("RestartCount = %d, want 4", rc)
		}
		for g := 1; g < 4; g++ {
			assertSplitMatchesRebuild(t, cfg, entries, g)
		}
	})

	t.Run("mixed cell kinds", func(t *testing.T) {
		cfg := Config{PageSize: 4096, RestartGroupTarget: 4, LeafLayout: LeafLayoutInterleaved}
		entries := []LeafEntry{
			mk("key-0", "v"), mk("key-1", "v"), mk("key-2", "v"), mk("key-3", "v"),
			{Flags: CellFlagOverflow, Key: []byte("key-4"), OverflowPage: 7, TotalLen: 99},
			mk("key-5", "v"), mk("key-6", "v"),
			{Flags: CellFlagMultiValue | CellFlagNestedTree, Key: []byte("key-7"), NestedRoot: 3, NestedCount: 4},
		}
		rc := NewLeafReader(mustBuild(t, cfg, entries), cfg).RestartCount()
		for g := 1; g < rc; g++ {
			assertSplitMatchesRebuild(t, cfg, entries, g)
		}
	})
}

func TestFindSplitGroup(t *testing.T) {
	mk := func(k, v string) LeafEntry { return LeafEntry{Key: []byte(k), Value: []byte(v)} }
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4, LeafLayout: LeafLayoutInterleaved}

	// Uniform entries → 4 groups of 4; the ~50% byte boundary is the middle
	// group index. The result must be in [1, rc) and split the bytes near half.
	var entries []LeafEntry
	for i := range 16 {
		entries = append(entries, mk(fmt.Sprintf("key-%04d", i), "val"))
	}
	buf := mustBuild(t, cfg, entries)
	r := NewLeafReader(buf, cfg)
	rc := r.RestartCount()
	if rc != 4 {
		t.Fatalf("RestartCount = %d, want 4", rc)
	}
	g := FindSplitGroup(buf, cfg)
	if g < 1 || g >= rc {
		t.Fatalf("FindSplitGroup = %d, want in [1, %d)", g, rc)
	}
	// Uniform groups → the closest-to-50%% boundary is group 2 (offset table:
	// groups 0..3 each ~equal; the boundary nearest half the data is index 2).
	if g != 2 {
		t.Errorf("FindSplitGroup = %d, want 2 for uniform 4-group page", g)
	}
	// Determinism: a pure function of the bytes.
	if g2 := FindSplitGroup(buf, cfg); g2 != g {
		t.Errorf("FindSplitGroup not deterministic: %d then %d", g, g2)
	}
}

// FuzzSplitLeafAtGroup asserts the canonical/byte-identity + read-only-src
// invariants over random multi-group pages (mixed cell kinds) and every valid
// split boundary.
func FuzzSplitLeafAtGroup(f *testing.F) {
	f.Add(uint64(1), uint64(0))
	f.Add(uint64(42), uint64(1))
	f.Add(uint64(0xDEADBEEF), uint64(2))

	f.Fuzz(func(t *testing.T, leafSeed, groupSeed uint64) {
		cfg := Config{PageSize: 4096, RestartGroupTarget: 4, LeafLayout: LeafLayoutInterleaved} // smaller RGT → more groups
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
			return // need a multi-group page to split
		}
		splitGroup := 1 + int(groupSeed%uint64(rc-1)) // [1, rc)
		assertSplitMatchesRebuild(t, cfg, entries, splitGroup)
	})
}

// ---------------------------------------------------------------------------
// Uncompressed entry split (byte-carve) — same canonical / read-only-src /
// free-space-zeroed oracle as the compressed group split, at RGT=1.
// ---------------------------------------------------------------------------

func assertUCSplitMatchesRebuild(t *testing.T, cfg Config, entries []LeafEntry, splitIdx int) {
	t.Helper()
	orig, ok := tryBuild(cfg, entries)
	if !ok {
		t.Fatalf("base (%d entries) did not fit", len(entries))
	}
	origCopy := bytes.Clone(orig)
	src := bytes.Clone(orig)
	dst := make([]byte, cfg.PageSize)

	leftCount, rightCount := SplitUCRightHalf(src, dst, cfg, splitIdx)
	if !bytes.Equal(src, origCopy) {
		t.Fatalf("SplitUCRightHalf mutated src (must be read-only): splitIdx=%d", splitIdx)
	}
	if leftCount != splitIdx || leftCount+rightCount != len(entries) || rightCount < 1 {
		t.Fatalf("counts (%d,%d) wrong for splitIdx=%d n=%d", leftCount, rightCount, splitIdx, len(entries))
	}
	wantRight, _ := tryBuild(cfg, entries[splitIdx:])
	if !bytes.Equal(dst, wantRight) {
		t.Fatalf("UC right half != rebuild(entries[%d:]) (must be canonical)", splitIdx)
	}
	assertFreeSpaceZeroed(t, dst, cfg)
	if err := NewLeafReader(dst, cfg).Validate(); err != nil {
		t.Fatalf("UC right half fails Validate: %v", err)
	}

	if tlc := TruncateUCToEntries(src, cfg, splitIdx); tlc != splitIdx {
		t.Fatalf("TruncateUCToEntries = %d, want %d", tlc, splitIdx)
	}
	wantLeft, _ := tryBuild(cfg, entries[:splitIdx])
	if !bytes.Equal(src, wantLeft) {
		t.Fatalf("UC left half != rebuild(entries[:%d]) (must be canonical)", splitIdx)
	}
	assertFreeSpaceZeroed(t, src, cfg)
	if err := NewLeafReader(src, cfg).Validate(); err != nil {
		t.Fatalf("UC left half fails Validate: %v", err)
	}
}

func TestUCSplitLeafAt_MatchesRebuild(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1} // uncompressed
	mk := func(k, v string) LeafEntry { return LeafEntry{Key: []byte(k), Value: []byte(v)} }
	t.Run("inline", func(t *testing.T) {
		var entries []LeafEntry
		for i := range 10 {
			entries = append(entries, mk(fmt.Sprintf("k%04d", i), "val"))
		}
		for s := 1; s < len(entries); s++ {
			assertUCSplitMatchesRebuild(t, cfg, entries, s)
		}
	})
	t.Run("mixed cell kinds", func(t *testing.T) {
		entries := []LeafEntry{
			mk("k0", "v"),
			{Flags: CellFlagOverflow, Key: []byte("k1"), OverflowPage: 7, TotalLen: 99},
			mk("k2", "v"),
			{Flags: CellFlagMultiValue | CellFlagNestedTree, Key: []byte("k3"), NestedRoot: 3, NestedCount: 4},
			mk("k4", "v"), mk("k5", "v"),
		}
		for s := 1; s < len(entries); s++ {
			assertUCSplitMatchesRebuild(t, cfg, entries, s)
		}
	})
}

func TestFindUCSplitIndex(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1}
	mk := func(k, v string) LeafEntry { return LeafEntry{Key: []byte(k), Value: []byte(v)} }
	var entries []LeafEntry
	for i := range 8 { // uniform entries → 50% boundary is the middle index
		entries = append(entries, mk(fmt.Sprintf("k%04d", i), "val"))
	}
	buf := mustBuild(t, cfg, entries)
	count := NewLeafReader(buf, cfg).Count()
	s := FindUCSplitIndex(buf, cfg)
	if s < 1 || s >= count {
		t.Fatalf("FindUCSplitIndex = %d, want in [1, %d)", s, count)
	}
	if s != count/2 {
		t.Errorf("FindUCSplitIndex = %d, want %d for uniform entries", s, count/2)
	}
	if s2 := FindUCSplitIndex(buf, cfg); s2 != s {
		t.Errorf("FindUCSplitIndex not deterministic: %d then %d", s, s2)
	}
}

// FuzzUCSplitLeafAt asserts the canonical/byte-identity + read-only-src
// invariants over random uncompressed pages (mixed cell kinds) and every valid
// entry boundary.
func FuzzUCSplitLeafAt(f *testing.F) {
	f.Add(uint64(1), uint64(0))
	f.Add(uint64(42), uint64(1))
	f.Add(uint64(0xDEADBEEF), uint64(2))

	f.Fuzz(func(t *testing.T, leafSeed, idxSeed uint64) {
		cfg := Config{PageSize: 4096, RestartGroupTarget: 1} // uncompressed
		entries := randomFittingMixed(leafSeed, cfg)
		if len(entries) < 2 {
			return
		}
		splitIdx := 1 + int(idxSeed%uint64(len(entries)-1)) // [1, len)
		assertUCSplitMatchesRebuild(t, cfg, entries, splitIdx)
	})
}

// BenchmarkSplitLeafAtGroup measures the no-decode carve (right-half copy +
// left truncate) — in-place, zero-alloc — vs the decode/re-encode split it
// replaces on the append-overflow hot path.
func BenchmarkSplitLeafAtGroup(b *testing.B) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 16, LeafLayout: LeafLayoutInterleaved}
	var entries []LeafEntry
	for i := 0; ; i++ {
		entries = append(entries, LeafEntry{Key: fmt.Appendf(nil, "key-%05d", i), Value: bytes.Repeat([]byte("v"), 40)})
		if _, ok := tryBuild(cfg, entries); !ok {
			entries = entries[:len(entries)-1]
			break
		}
	}
	prebuilt, _ := tryBuild(cfg, entries)
	splitGroup := NewLeafReader(prebuilt, cfg).RestartCount() / 2
	src := make([]byte, cfg.PageSize)
	dst := make([]byte, cfg.PageSize)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		copy(src, prebuilt)
		clear(dst)
		SplitLeafRightHalf(src, dst, cfg, splitGroup)
		TruncateLeafToGroups(src, cfg, splitGroup)
	}
}

// mustBuild builds a leaf from entries, failing the test if they don't fit.
func mustBuild(t *testing.T, cfg Config, entries []LeafEntry) []byte {
	t.Helper()
	buf, ok := tryBuild(cfg, entries)
	if !ok {
		t.Fatalf("entries (%d) did not fit a page", len(entries))
	}
	return buf
}
