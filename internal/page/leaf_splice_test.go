package page

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
)

// Tests for the in-place leaf splice helpers (leaf_splice.go). The central
// safety net is the determinism/parity invariant (page-formats.md §Leaf
// Split): a successful splice must be byte-identical to a LeafBuilder rebuild
// of the same logical entries, and a declined splice must leave the page
// byte-unchanged. The LeafBuilder rebuild is the oracle — no frozen copy to
// drift out of sync.

// tryBuild builds a leaf from entries via LeafBuilder. Returns the page bytes
// and whether every entry fit (false the moment an AddEntry returns
// page-full). entries must be sorted by key.
func tryBuild(cfg Config, entries []LeafEntry) ([]byte, bool) {
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	for _, e := range entries {
		if !b.AddEntry(e) {
			return nil, false
		}
	}
	b.Finish()
	return buf, true
}

// assertAppendMatchesRebuild is the parity oracle for TryAppend. It builds a
// leaf from base, appends e via TryAppend on a copy, and asserts:
//   - if a LeafBuilder rebuild of base+e fits: TryAppend succeeds, the
//     spliced page is byte-identical to that rebuild, and it passes Validate.
//   - if the rebuild overflows: TryAppend declines and leaves the page
//     byte-unchanged (fit-check parity).
//
// base must be sorted and non-empty; e.Key must sort after base's last key.
// Returns the spliced page and whether the append landed in place.
func assertAppendMatchesRebuild(t *testing.T, cfg Config, base []LeafEntry, e LeafEntry) ([]byte, bool) {
	t.Helper()
	orig, fit := tryBuild(cfg, base)
	if !fit {
		t.Fatalf("assertAppendMatchesRebuild: base (%d entries) did not fit a page", len(base))
	}

	all := append(append([]LeafEntry(nil), base...), e)
	expected, expectedFits := tryBuild(cfg, all)

	actual := bytes.Clone(orig)
	prevKey, _ := NewLeafReader(orig, cfg).LastKey(nil)
	prevKey = bytes.Clone(prevKey)
	ok := TryAppend(actual, cfg, e, prevKey)

	if !expectedFits {
		if ok {
			t.Fatalf("TryAppend succeeded but a LeafBuilder rebuild overflowed (fit-check divergence): base=%d e.Key=%q", len(base), e.Key)
		}
		if !bytes.Equal(actual, orig) {
			t.Fatalf("TryAppend declined but mutated the page: base=%d e.Key=%q", len(base), e.Key)
		}
		return nil, false
	}
	if !ok {
		t.Fatalf("TryAppend declined but a LeafBuilder rebuild fit (fit-check divergence): base=%d e.Key=%q", len(base), e.Key)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("spliced page != LeafBuilder rebuild (determinism invariant)\nbase=%d e.Key=%q\nactual DataEnd=%d RestartCount=%d / expected DataEnd=%d RestartCount=%d",
			len(base), e.Key,
			le.Uint16(actual[leafOffDataEnd:]), le.Uint16(actual[leafOffRestartCount:]),
			le.Uint16(expected[leafOffDataEnd:]), le.Uint16(expected[leafOffRestartCount:]))
	}
	if err := NewLeafReader(actual, cfg).Validate(); err != nil {
		t.Fatalf("spliced page fails Validate: %v", err)
	}
	return actual, true
}

func TestTryAppendCompressed_MatchesRebuild(t *testing.T) {
	mk := func(k, v string) LeafEntry { return LeafEntry{Key: []byte(k), Value: []byte(v)} }
	cases := []struct {
		name         string
		cfg          Config
		base         []LeafEntry
		e            LeafEntry
		wantNewGroup bool // RestartCount must increase by exactly 1
	}{
		{
			name: "delta-extends-group",
			cfg:  Config{PageSize: 4096, RestartGroupTarget: 16},
			base: []LeafEntry{mk("key-0001", "v1"), mk("key-0002", "v2")},
			e:    mk("key-0003", "v3"),
		},
		{
			name:         "natural-break-new-group",
			cfg:          Config{PageSize: 4096, RestartGroupTarget: 16},
			base:         []LeafEntry{mk("aaa-1", "v1"), mk("aaa-2", "v2")},
			e:            mk("zzz", "v3"), // sharedPrefixLen("aaa-2","zzz")==0
			wantNewGroup: true,
		},
		{
			name:         "target-cap-new-group",
			cfg:          Config{PageSize: 4096, RestartGroupTarget: 4},
			base:         []LeafEntry{mk("k-00", "v"), mk("k-01", "v"), mk("k-02", "v"), mk("k-03", "v")},
			e:            mk("k-04", "v"),
			wantNewGroup: true,
		},
		{
			name:         "overflow-entry",
			cfg:          Config{PageSize: 4096, RestartGroupTarget: 16},
			base:         []LeafEntry{mk("aaa", "v1")},
			e:            LeafEntry{Flags: CellFlagOverflow, Key: []byte("bbb"), OverflowPage: 42, TotalLen: 100000},
			wantNewGroup: true, // sharedPrefixLen("aaa","bbb")==0 → natural break
		},
		{
			name:         "overflow-entry-shared-prefix",
			cfg:          Config{PageSize: 4096, RestartGroupTarget: 16},
			base:         []LeafEntry{mk("key-1", "v1"), mk("key-2", "v2")},
			e:            LeafEntry{Flags: CellFlagOverflow, Key: []byte("key-3"), OverflowPage: 7, TotalLen: 99},
			wantNewGroup: false, // shares "key-" → delta extends the group
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rcBefore := func() int {
				orig, ok := tryBuild(tc.cfg, tc.base)
				if !ok {
					t.Fatalf("base did not fit")
				}
				return NewLeafReader(orig, tc.cfg).RestartCount()
			}()

			actual, spliced := assertAppendMatchesRebuild(t, tc.cfg, tc.base, tc.e)
			if !spliced {
				t.Fatal("expected TryAppend to land in place")
			}
			rcAfter := NewLeafReader(actual, tc.cfg).RestartCount()
			switch {
			case tc.wantNewGroup && rcAfter != rcBefore+1:
				t.Errorf("RestartCount = %d, want %d (append should open a new group)", rcAfter, rcBefore+1)
			case !tc.wantNewGroup && rcAfter != rcBefore:
				t.Errorf("RestartCount = %d, want %d (append should extend the last group)", rcAfter, rcBefore)
			}

			// The appended entry must be the new last entry, intact.
			r := NewLeafReader(actual, tc.cfg)
			got, _ := r.EntryAt(r.Count()-1, nil)
			if !bytes.Equal(got.Key, tc.e.Key) {
				t.Errorf("last key = %q, want %q", got.Key, tc.e.Key)
			}
			if tc.e.Flags == 0 && !bytes.Equal(got.Value, tc.e.Value) {
				t.Errorf("last value = %q, want %q", got.Value, tc.e.Value)
			}
		})
	}
}

// TestTryAppendCompressed_SequentialMatchesRebuild appends many entries one at
// a time, asserting byte-identity to a fresh rebuild after EACH append. This
// exercises all three restart triggers (delta, target-cap, natural-break) and
// directly guards the free-space-zeroed invariant across repeated splices — a
// stale non-zero byte left by an earlier splice would diverge from the
// rebuild on a later one.
func TestTryAppendCompressed_SequentialMatchesRebuild(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	var entries []LeafEntry
	for _, pfx := range []string{"alpha", "beta", "gamma"} {
		for i := range 6 {
			entries = append(entries, LeafEntry{
				Key:   fmt.Appendf(nil, "%s-%03d", pfx, i),
				Value: fmt.Appendf(nil, "val-%s-%d", pfx, i),
			})
		}
	}

	buf, ok := tryBuild(cfg, entries[:1])
	if !ok {
		t.Fatal("first entry did not fit")
	}
	for i := 1; i < len(entries); i++ {
		prevKey, _ := NewLeafReader(buf, cfg).LastKey(nil)
		prevKey = bytes.Clone(prevKey)
		if !TryAppend(buf, cfg, entries[i], prevKey) {
			t.Fatalf("append %d (%q) declined unexpectedly", i, entries[i].Key)
		}
		want, wok := tryBuild(cfg, entries[:i+1])
		if !wok {
			t.Fatalf("rebuild of %d entries did not fit", i+1)
		}
		if !bytes.Equal(buf, want) {
			t.Fatalf("after append %d (%q): spliced page != rebuild", i, entries[i].Key)
		}
		if err := NewLeafReader(buf, cfg).Validate(); err != nil {
			t.Fatalf("after append %d: spliced page fails Validate: %v", i, err)
		}
	}
}

func TestTryAppendCompressed_PageFull(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 16}
	// Fill a page to capacity with 10-byte values, then drop the entry that
	// overflowed so base is a packed-but-valid page.
	var base []LeafEntry
	for i := 0; ; i++ {
		base = append(base, LeafEntry{Key: fmt.Appendf(nil, "key-%05d", i), Value: bytes.Repeat([]byte("v"), 10)})
		if _, ok := tryBuild(cfg, base); !ok {
			base = base[:len(base)-1]
			break
		}
	}
	if len(base) == 0 {
		t.Fatal("page too small to hold any entry")
	}
	// A large append the page cannot absorb — the rebuild overflows, so
	// assertAppendMatchesRebuild verifies TryAppend declines and leaves the
	// page byte-unchanged.
	e := LeafEntry{Key: []byte("zzz-99999"), Value: bytes.Repeat([]byte("x"), 4000)}
	if _, spliced := assertAppendMatchesRebuild(t, cfg, base, e); spliced {
		t.Fatal("expected TryAppend to decline on a full page")
	}
}

func TestTryAppendOnEmptyPage(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 16}
	buf, _ := tryBuild(cfg, nil) // count==0 leaf
	before := bytes.Clone(buf)
	if TryAppend(buf, cfg, LeafEntry{Key: []byte("a"), Value: []byte("1")}, nil) {
		t.Fatal("TryAppend should return false on an empty page (count==0)")
	}
	if !bytes.Equal(buf, before) {
		t.Fatal("TryAppend mutated an empty page on decline")
	}
}

// TestUCTryAppend_MatchesRebuild: an append to an uncompressed leaf (RGT=1)
// splices in place and is byte-identical to a LeafBuilder rebuild — uncompressed
// appends are CANONICAL (a sorted, packed entry array + sorted offset table, so
// an append is exactly the builder's last action). Reuses the TryAppend byte-
// identity oracle with an uncompressed config.
func TestUCTryAppend_MatchesRebuild(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1} // uncompressed variant
	mk := func(k, v string) LeafEntry { return LeafEntry{Key: []byte(k), Value: []byte(v)} }
	cases := []struct {
		name string
		base []LeafEntry
		e    LeafEntry
	}{
		{"inline", []LeafEntry{mk("aaa", "1"), mk("bbb", "2")}, mk("ccc", "3")},
		{"single-entry base", []LeafEntry{mk("aaa", "1")}, mk("bbb", "2")},
		{"overflow", []LeafEntry{mk("k1", "v")}, LeafEntry{Flags: CellFlagOverflow, Key: []byte("k2"), OverflowPage: 42, TotalLen: 100000}},
		{"nested", []LeafEntry{mk("k1", "v")}, LeafEntry{Flags: CellFlagMultiValue | CellFlagNestedTree, Key: []byte("k2"), NestedRoot: 7, NestedCount: 9}},
		{"subpage", []LeafEntry{mk("k1", "v")}, LeafEntry{Flags: CellFlagMultiValue, Key: []byte("k2"), Value: []byte("sub")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, spliced := assertAppendMatchesRebuild(t, cfg, tc.base, tc.e); !spliced {
				t.Fatal("expected uncompressed TryAppend to land in place")
			}
		})
	}
}

// TestTryAppendVariantMismatchDeclines: a page whose on-disk variant differs
// from the configured one must decline (false, byte-unchanged) so the leaf
// migrates via rebuild. The compressed-page + RGT==1 direction is covered by
// TestTryAppendCompressedDeclinesWhenConfiguredUncompressed; this covers the
// uncompressed-page + RGT>=2 direction.
func TestTryAppendVariantMismatchDeclines(t *testing.T) {
	base := []LeafEntry{{Key: []byte("aaa"), Value: []byte("1")}, {Key: []byte("bbb"), Value: []byte("2")}}
	buf, ok := tryBuild(Config{PageSize: 4096, RestartGroupTarget: 1}, base) // uncompressed page
	if !ok {
		t.Fatal("base did not fit")
	}
	if buf[0] != TypeLeafUncompressed {
		t.Fatalf("base is not an uncompressed leaf: type=%d", buf[0])
	}
	before := bytes.Clone(buf)
	// Same uncompressed page, but the keyspace is now configured compressed.
	if TryAppend(buf, Config{PageSize: 4096, RestartGroupTarget: 16}, LeafEntry{Key: []byte("ccc"), Value: []byte("3")}, []byte("bbb")) {
		t.Fatal("TryAppend should decline an uncompressed page when RGT>=2 (migrate to compressed via rebuild)")
	}
	if !bytes.Equal(buf, before) {
		t.Fatal("TryAppend mutated the page on variant-decline")
	}
}

// TestTryAppendCompressedDeclinesWhenConfiguredUncompressed: a compressed leaf
// whose keyspace was reconfigured to RGT=1 (the uncompressed variant) must NOT
// be spliced — it has to migrate to uncompressed via the decode/rebuild path
// (keyspaces.md: existing leaves migrate when next split/merged/rebuilt). The
// dispatcher declines (false, page unchanged) so the caller's fallback runs.
// This is the page-level analog of the gmdb-level
// TestKeyspacePutHonorsPerKeyspaceRestartGroupTarget.
func TestTryAppendCompressedDeclinesWhenConfiguredUncompressed(t *testing.T) {
	buildCfg := Config{PageSize: 4096, RestartGroupTarget: 16} // compressed page
	base := []LeafEntry{{Key: []byte("aaa"), Value: []byte("1")}, {Key: []byte("bbb"), Value: []byte("2")}}
	buf, ok := tryBuild(buildCfg, base)
	if !ok {
		t.Fatal("base did not fit")
	}
	if buf[0] != TypeLeaf {
		t.Fatalf("base is not a compressed leaf: type=%d", buf[0])
	}
	before := bytes.Clone(buf)

	// Same compressed page, but the keyspace is now configured uncompressed.
	ucCfg := Config{PageSize: 4096, RestartGroupTarget: 1}
	prevKey, _ := NewLeafReader(buf, ucCfg).LastKey(nil)
	if TryAppend(buf, ucCfg, LeafEntry{Key: []byte("ccc"), Value: []byte("3")}, prevKey) {
		t.Fatal("TryAppend should decline a compressed page when RGT=1 (must migrate via rebuild)")
	}
	if !bytes.Equal(buf, before) {
		t.Fatal("TryAppend mutated the page on decline")
	}
}

// FuzzTryAppendCompressed asserts the determinism/parity invariant over random
// leaves and random append keys (both shared- and zero-shared-prefix
// successors, so all restart branches are exercised).
func FuzzTryAppendCompressed(f *testing.F) {
	f.Add(uint64(1), uint64(2))
	f.Add(uint64(42), uint64(7))
	f.Add(uint64(0xDEADBEEF), uint64(0xCAFEBABE))

	f.Fuzz(func(t *testing.T, leafSeed, appendSeed uint64) {
		cfg := Config{PageSize: 4096, RestartGroupTarget: 16}
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

// FuzzUCTryAppend asserts the byte-identity invariant for UNCOMPRESSED appends
// (RGT=1) over random leaves + random appended entries of mixed cell kinds
// (overflow / nested / subpage / inline). Uncompressed appends are canonical, so
// the oracle is the same byte-identical-to-rebuild check as the compressed
// append.
func FuzzUCTryAppend(f *testing.F) {
	f.Add(uint64(1), uint64(2))
	f.Add(uint64(42), uint64(7))
	f.Add(uint64(0xDEADBEEF), uint64(0xCAFEBABE))

	f.Fuzz(func(t *testing.T, leafSeed, appendSeed uint64) {
		cfg := Config{PageSize: 4096, RestartGroupTarget: 1} // uncompressed variant
		base := randomFittingMixed(leafSeed, cfg)
		if len(base) == 0 {
			return
		}
		appendKey := keyAfterSeed(base[len(base)-1].Key, appendSeed)
		e := randomLeafEntry(rand.New(rand.NewPCG(appendSeed, appendSeed^0xABCD1234EF567890)), appendKey)
		assertAppendMatchesRebuild(t, cfg, base, e)
	})
}

// ---------------------------------------------------------------------------
// TryInsertAt — localized (non-canonical) splice; oracle is semantic +
// structural (decode → exact entry sequence; Validate; free-space zeroed), not
// byte-identity to a rebuild (a mid-group insert grows the group past target).
// ---------------------------------------------------------------------------

// assertLeafDecodesTo decodes buf entry-by-entry and asserts it equals expected
// (key, flags, and value/trailer), and that it passes Validate.
func assertLeafDecodesTo(t *testing.T, buf []byte, cfg Config, expected []LeafEntry) {
	t.Helper()
	r := NewLeafReader(buf, cfg)
	if err := r.Validate(); err != nil {
		t.Fatalf("spliced page fails Validate: %v", err)
	}
	if r.Count() != len(expected) {
		t.Fatalf("Count = %d, want %d", r.Count(), len(expected))
	}
	var keyBuf []byte
	for i, want := range expected {
		got, kb := r.EntryAt(i, keyBuf)
		keyBuf = kb
		if !bytes.Equal(got.Key, want.Key) {
			t.Fatalf("entry %d: key %q, want %q", i, got.Key, want.Key)
		}
		if got.Flags != want.Flags {
			t.Fatalf("entry %d: flags 0x%x, want 0x%x", i, got.Flags, want.Flags)
		}
		switch {
		case want.IsOverflow():
			if got.OverflowPage != want.OverflowPage || got.TotalLen != want.TotalLen {
				t.Fatalf("entry %d: overflow=(%d,%d), want (%d,%d)", i, got.OverflowPage, got.TotalLen, want.OverflowPage, want.TotalLen)
			}
		case want.IsNestedTree():
			if got.NestedRoot != want.NestedRoot || got.NestedCount != want.NestedCount {
				t.Fatalf("entry %d: nested=(%d,%d), want (%d,%d)", i, got.NestedRoot, got.NestedCount, want.NestedRoot, want.NestedCount)
			}
		default:
			if !bytes.Equal(got.Value, want.Value) {
				t.Fatalf("entry %d: value %q, want %q", i, got.Value, want.Value)
			}
		}
	}
}

// assertFreeSpaceZeroed asserts [DataEnd, restart-table-start) is all zero.
func assertFreeSpaceZeroed(t *testing.T, buf []byte, cfg Config) {
	t.Helper()
	r := NewLeafReader(buf, cfg)
	dataEnd := r.DataEnd()
	tableStart := cfg.ContentEnd() - r.RestartCount()*restartTableEntrySize
	for i := dataEnd; i < tableStart; i++ {
		if buf[i] != 0 {
			t.Fatalf("free-space byte at %d = 0x%x, want 0 (DataEnd=%d tableStart=%d)", i, buf[i], dataEnd, tableStart)
		}
	}
}

func insertExpected(base []LeafEntry, e LeafEntry, idx int) []LeafEntry {
	out := make([]LeafEntry, 0, len(base)+1)
	out = append(out, base[:idx]...)
	out = append(out, e)
	out = append(out, base[idx:]...)
	return out
}

// checkInsert builds base, splices e at idx (asserting success), and verifies
// the result decodes to the expected sequence with zeroed free space. Returns
// the spliced page for further assertions (e.g. group counts).
func checkInsert(t *testing.T, cfg Config, base []LeafEntry, idx int, e LeafEntry) []byte {
	t.Helper()
	buf, ok := tryBuild(cfg, base)
	if !ok {
		t.Fatalf("base (%d entries) did not fit", len(base))
	}
	if !TryInsertAt(buf, cfg, idx, e) {
		t.Fatalf("TryInsertAt declined unexpectedly (base=%d idx=%d)", len(base), idx)
	}
	assertLeafDecodesTo(t, buf, cfg, insertExpected(base, e, idx))
	assertFreeSpaceZeroed(t, buf, cfg)
	return buf
}

func TestTryInsertAtCompressed_Positions(t *testing.T) {
	mk := func(k, v string) LeafEntry { return LeafEntry{Key: []byte(k), Value: []byte(v)} }
	ovf := func(k string, pg, tl uint64) LeafEntry {
		return LeafEntry{Flags: CellFlagOverflow, Key: []byte(k), OverflowPage: pg, TotalLen: tl}
	}
	t.Run("I-B front", func(t *testing.T) {
		checkInsert(t, Config{PageSize: 4096, RestartGroupTarget: 16},
			[]LeafEntry{mk("bbb", "1"), mk("ccc", "2")}, 0, mk("aaa", "x"))
	})
	t.Run("I-C interior shared-prefix", func(t *testing.T) {
		checkInsert(t, Config{PageSize: 4096, RestartGroupTarget: 16},
			[]LeafEntry{mk("key-1", "1"), mk("key-3", "2")}, 1, mk("key-2", "x"))
	})
	t.Run("I-C interior prefix-break", func(t *testing.T) {
		// New key shares no prefix with either neighbour — still valid (sharedLen 0).
		checkInsert(t, Config{PageSize: 4096, RestartGroupTarget: 16},
			[]LeafEntry{mk("aaa", "1"), mk("ccc", "2")}, 1, mk("bbb", "x"))
	})
	t.Run("I-D group boundary (multi-group)", func(t *testing.T) {
		// RGT=4 → base of 8 makes two full groups [k0..k3][k4..k7]. Insert at
		// idx 4 (the boundary) routes into group 0 as p==gc (I-D).
		base := []LeafEntry{mk("key-0", "0"), mk("key-1", "1"), mk("key-2", "2"), mk("key-3", "3"),
			mk("key-4", "4"), mk("key-5", "5"), mk("key-6", "6"), mk("key-7", "7")}
		buf := checkInsert(t, Config{PageSize: 4096, RestartGroupTarget: 4}, base, 4, mk("key-35", "x"))
		// Group 0 grew to 5; group 1 unchanged at 4; RestartCount unchanged.
		r := NewLeafReader(buf, Config{PageSize: 4096, RestartGroupTarget: 4})
		if r.RestartCount() != 2 {
			t.Fatalf("RestartCount = %d, want 2 (insert grows a group, never adds one)", r.RestartCount())
		}
		if r.GroupEntryCount(0) != 5 || r.GroupEntryCount(1) != 4 {
			t.Fatalf("group counts = (%d,%d), want (5,4)", r.GroupEntryCount(0), r.GroupEntryCount(1))
		}
	})
	t.Run("overflow successor", func(t *testing.T) {
		checkInsert(t, Config{PageSize: 4096, RestartGroupTarget: 16},
			[]LeafEntry{mk("key-1", "1"), ovf("key-3", 99, 100000)}, 1, mk("key-2", "x"))
	})
	t.Run("new overflow entry interior", func(t *testing.T) {
		checkInsert(t, Config{PageSize: 4096, RestartGroupTarget: 16},
			[]LeafEntry{mk("key-1", "1"), mk("key-3", "2")}, 1, ovf("key-2", 7, 50000))
	})
}

func TestTryInsertAtCompressed_GrowsThenCapDeclines(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4} // maxGroup = 2*4 = 8
	// One group of 4 prefix-sharing keys (even-numbered, so odd keys slot in).
	base := []LeafEntry{
		{Key: []byte("key-00"), Value: []byte("v")},
		{Key: []byte("key-02"), Value: []byte("v")},
		{Key: []byte("key-04"), Value: []byte("v")},
		{Key: []byte("key-06"), Value: []byte("v")},
	}
	buf, ok := tryBuild(cfg, base)
	if !ok {
		t.Fatal("base did not fit")
	}
	// Insert into group 0 until it reaches the 2T cap (8). Inserts of odd keys
	// at the front of the group (idx 1) keep landing in group 0.
	entries := append([]LeafEntry(nil), base...)
	for _, k := range []string{"key-01", "key-03", "key-05", "key-07"} {
		e := LeafEntry{Key: []byte(k), Value: []byte("v")}
		idx := sortedInsertIdx(entries, e.Key)
		if !TryInsertAt(buf, cfg, idx, e) {
			t.Fatalf("insert %q declined before reaching the cap (count now %d)", k, len(entries))
		}
		entries = insertExpected(entries, e, idx)
		assertLeafDecodesTo(t, buf, cfg, entries)
		assertFreeSpaceZeroed(t, buf, cfg)
	}
	// Group 0 is now at 8 (== 2T). The next insert into it must decline.
	if r := NewLeafReader(buf, cfg); r.GroupEntryCount(0) != 8 {
		t.Fatalf("group 0 count = %d, want 8 (2T)", r.GroupEntryCount(0))
	}
	e := LeafEntry{Key: []byte("key-035"), Value: []byte("v")} // between key-03 and key-04 → group 0
	idx := sortedInsertIdx(entries, e.Key)
	before := bytes.Clone(buf)
	if TryInsertAt(buf, cfg, idx, e) {
		t.Fatal("TryInsertAt should decline when the group is at the 2T cap")
	}
	if !bytes.Equal(buf, before) {
		t.Fatal("TryInsertAt mutated the page on cap-decline")
	}
}

func TestTryInsertAtCompressed_PageFull(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 16}
	var base []LeafEntry
	for i := 0; ; i++ {
		base = append(base, LeafEntry{Key: fmt.Appendf(nil, "key-%05d", i*2), Value: bytes.Repeat([]byte("v"), 10)})
		if _, ok := tryBuild(cfg, base); !ok {
			base = base[:len(base)-1]
			break
		}
	}
	buf, _ := tryBuild(cfg, base)
	before := bytes.Clone(buf)
	// A large interior insert that cannot fit.
	e := LeafEntry{Key: []byte("key-00001"), Value: bytes.Repeat([]byte("x"), 4000)}
	idx := sortedInsertIdx(base, e.Key)
	if TryInsertAt(buf, cfg, idx, e) {
		t.Fatal("TryInsertAt should decline on a full page")
	}
	if !bytes.Equal(buf, before) {
		t.Fatal("TryInsertAt mutated the page on decline")
	}
}

func TestTryInsertAtCompressed_VariantDeclines(t *testing.T) {
	base := []LeafEntry{{Key: []byte("aaa"), Value: []byte("1")}, {Key: []byte("ccc"), Value: []byte("2")}}
	buf, ok := tryBuild(Config{PageSize: 4096, RestartGroupTarget: 16}, base)
	if !ok {
		t.Fatal("base did not fit")
	}
	before := bytes.Clone(buf)
	// Compressed page, but the keyspace is now configured uncompressed (RGT=1).
	if TryInsertAt(buf, Config{PageSize: 4096, RestartGroupTarget: 1}, 1, LeafEntry{Key: []byte("bbb"), Value: []byte("x")}) {
		t.Fatal("TryInsertAt should decline a compressed page when RGT=1 (migrate via rebuild)")
	}
	if !bytes.Equal(buf, before) {
		t.Fatal("TryInsertAt mutated the page on variant-decline")
	}
}

// FuzzTryInsertAtCompressed asserts the semantic + structural invariant over
// random leaves and random interior insert positions/keys: a successful splice
// decodes to the exact expected entry sequence, passes Validate, and leaves
// free space zeroed; a declined splice leaves the page byte-unchanged.
func FuzzTryInsertAtCompressed(f *testing.F) {
	f.Add(uint64(1), uint64(2), uint64(0))
	f.Add(uint64(42), uint64(7), uint64(3))
	f.Add(uint64(0xDEADBEEF), uint64(0xCAFEBABE), uint64(5))

	f.Fuzz(func(t *testing.T, leafSeed, keySeed, posSeed uint64) {
		cfg := Config{PageSize: 4096, RestartGroupTarget: 16}
		// Mixed cell kinds (inline / overflow / subpage / nested-tree) so the
		// successor-stash + re-encode paths are fuzzed for every cell form, not
		// just inline.
		base := randomFittingMixed(leafSeed, cfg)
		if len(base) == 0 {
			return
		}
		insertIdx := int(posSeed % uint64(len(base))) // [0, len): interior/front, not append
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

// sortedInsertIdx returns the index at which key belongs in the sorted entries.
func sortedInsertIdx(entries []LeafEntry, key []byte) int {
	return sort.Search(len(entries), func(i int) bool { return bytes.Compare(entries[i].Key, key) >= 0 })
}

// keyBetween returns a key strictly greater than lo (or any key if lo == nil)
// and strictly less than hi, or false if none is easily constructed.
func keyBetween(lo, hi []byte, seed uint64) ([]byte, bool) {
	if lo == nil {
		// Insert at front: a single byte one less than hi[0].
		if len(hi) > 0 && hi[0] > 0 {
			return []byte{hi[0] - 1}, true
		}
		return nil, false
	}
	// lo + [b] is always > lo (longer); pick b making it < hi.
	for _, b := range []byte{0, 1, byte(seed), 0x80, 0xfe, 0xff} {
		k := append(bytes.Clone(lo), b)
		if bytes.Compare(k, hi) < 0 {
			return k, true
		}
	}
	return nil, false
}

// BenchmarkTryInsertAtCompressed measures the per-call cost of a mid-group
// insert at various group fullness and value sizes (clone + splice per
// iteration). Compare against the decode/re-encode fallback it replaces.
func BenchmarkTryInsertAtCompressed(b *testing.B) {
	for _, gc := range []int{4, 8, 16} {
		for _, valSize := range []int{8, 100} {
			b.Run(fmt.Sprintf("gc=%d/val=%d", gc, valSize), func(b *testing.B) {
				cfg := Config{PageSize: 4096, RestartGroupTarget: 16}
				val := bytes.Repeat([]byte("v"), valSize)
				var base []LeafEntry
				for i := range gc {
					base = append(base, LeafEntry{Key: fmt.Appendf(nil, "key-%04d", i*2), Value: val})
				}
				prebuilt, ok := tryBuild(cfg, base)
				if !ok {
					b.Fatalf("base did not fit (gc=%d val=%d)", gc, valSize)
				}
				work := make([]byte, cfg.PageSize)
				insertIdx := gc / 2
				e := LeafEntry{Key: fmt.Appendf(nil, "key-%04d", insertIdx*2-1), Value: val}
				b.ResetTimer()
				b.ReportAllocs()
				for range b.N {
					copy(work, prebuilt)
					TryInsertAt(work, cfg, insertIdx, e)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// TryDeleteAt — localized (non-canonical) splice; same semantic + structural
// oracle as TryInsertAt (decode → exact entry sequence; Validate; free-space
// zeroed). A delete ALWAYS shrinks the page (never declines for space), proven
// by the shared-prefix triangle inequality — _AlwaysShrinks / _Reencoding-
// GrowthIsBounded pin that bound.
// ---------------------------------------------------------------------------

func deleteExpected(base []LeafEntry, idx int) []LeafEntry {
	out := make([]LeafEntry, 0, len(base)-1)
	out = append(out, base[:idx]...)
	out = append(out, base[idx+1:]...)
	return out
}

// checkDelete builds base, splices out the entry at idx (asserting success), and
// verifies the result decodes to the expected sequence with zeroed free space.
// Returns the spliced page for further assertions (e.g. RestartCount).
func checkDelete(t *testing.T, cfg Config, base []LeafEntry, idx int) []byte {
	t.Helper()
	buf, ok := tryBuild(cfg, base)
	if !ok {
		t.Fatalf("base (%d entries) did not fit", len(base))
	}
	if !TryDeleteAt(buf, cfg, idx) {
		t.Fatalf("TryDeleteAt declined unexpectedly (base=%d idx=%d)", len(base), idx)
	}
	assertLeafDecodesTo(t, buf, cfg, deleteExpected(base, idx))
	assertFreeSpaceZeroed(t, buf, cfg)
	return buf
}

func TestTryDeleteAtCompressed_Positions(t *testing.T) {
	mk := func(k, v string) LeafEntry { return LeafEntry{Key: []byte(k), Value: []byte(v)} }
	ovf := func(k string, pg, tl uint64) LeafEntry {
		return LeafEntry{Flags: CellFlagOverflow, Key: []byte(k), OverflowPage: pg, TotalLen: tl}
	}
	cfg16 := Config{PageSize: 4096, RestartGroupTarget: 16}

	// One group of three (all share "key-") exercises D-B / D-C / D-D; the group
	// survives so RestartCount stays 1.
	grp3 := []LeafEntry{mk("key-1", "1"), mk("key-2", "2"), mk("key-3", "3")}

	t.Run("D-B front (E[1] promoted to restart)", func(t *testing.T) {
		buf := checkDelete(t, cfg16, grp3, 0)
		if r := NewLeafReader(buf, cfg16); r.RestartCount() != 1 {
			t.Fatalf("RestartCount = %d, want 1 (group survives)", r.RestartCount())
		}
	})
	t.Run("D-C interior (successor re-deltaed vs E[p-1])", func(t *testing.T) {
		checkDelete(t, cfg16, grp3, 1)
	})
	t.Run("D-D group tail (no successor)", func(t *testing.T) {
		checkDelete(t, cfg16, grp3, 2)
	})

	// Three single-entry groups (no shared prefix → natural breaks) exercise
	// D-A at the first / middle / last group; each removal drops RestartCount.
	t.Run("D-A first group", func(t *testing.T) {
		buf := checkDelete(t, cfg16, []LeafEntry{mk("aaa", "1"), mk("bbb", "2"), mk("ccc", "3")}, 0)
		if r := NewLeafReader(buf, cfg16); r.RestartCount() != 2 {
			t.Fatalf("RestartCount = %d, want 2 (single-entry group removed)", r.RestartCount())
		}
	})
	t.Run("D-A middle group", func(t *testing.T) {
		buf := checkDelete(t, cfg16, []LeafEntry{mk("aaa", "1"), mk("bbb", "2"), mk("ccc", "3")}, 1)
		if r := NewLeafReader(buf, cfg16); r.RestartCount() != 2 {
			t.Fatalf("RestartCount = %d, want 2", r.RestartCount())
		}
	})
	t.Run("D-A last group", func(t *testing.T) {
		buf := checkDelete(t, cfg16, []LeafEntry{mk("aaa", "1"), mk("bbb", "2"), mk("ccc", "3")}, 2)
		if r := NewLeafReader(buf, cfg16); r.RestartCount() != 2 {
			t.Fatalf("RestartCount = %d, want 2", r.RestartCount())
		}
	})

	t.Run("D-B promotes an overflow successor to restart", func(t *testing.T) {
		checkDelete(t, cfg16, []LeafEntry{mk("key-1", "1"), ovf("key-2", 99, 100000)}, 0)
	})
	t.Run("D-C with overflow successor", func(t *testing.T) {
		checkDelete(t, cfg16, []LeafEntry{mk("key-1", "1"), mk("key-2", "2"), ovf("key-3", 7, 50000)}, 1)
	})
	t.Run("D-C deleting an overflow entry", func(t *testing.T) {
		checkDelete(t, cfg16, []LeafEntry{mk("key-1", "1"), ovf("key-2", 7, 50000), mk("key-3", "3")}, 1)
	})

	t.Run("D-C with later groups (offsets shift)", func(t *testing.T) {
		// RGT=4 → 8 shared-prefix keys make two full groups [k0..k3][k4..k7].
		// Deleting idx 1 (group 0) must shift group 1's stored offset left.
		cfg4 := Config{PageSize: 4096, RestartGroupTarget: 4}
		base := []LeafEntry{mk("key-0", "0"), mk("key-1", "1"), mk("key-2", "2"), mk("key-3", "3"),
			mk("key-4", "4"), mk("key-5", "5"), mk("key-6", "6"), mk("key-7", "7")}
		buf := checkDelete(t, cfg4, base, 1)
		r := NewLeafReader(buf, cfg4)
		if r.RestartCount() != 2 {
			t.Fatalf("RestartCount = %d, want 2 (delete shrinks a group, never drops one here)", r.RestartCount())
		}
		if r.GroupEntryCount(0) != 3 || r.GroupEntryCount(1) != 4 {
			t.Fatalf("group counts = (%d,%d), want (3,4)", r.GroupEntryCount(0), r.GroupEntryCount(1))
		}
	})
}

func TestTryDeleteAtCompressed_SingleEntryLastInGroupSurvives(t *testing.T) {
	// A two-entry group where the deleted entry is the tail (D-D), leaving the
	// group with one entry (still a valid restart-only group).
	cfg := Config{PageSize: 4096, RestartGroupTarget: 16}
	mk := func(k, v string) LeafEntry { return LeafEntry{Key: []byte(k), Value: []byte(v)} }
	buf := checkDelete(t, cfg, []LeafEntry{mk("key-1", "1"), mk("key-2", "2")}, 1)
	if r := NewLeafReader(buf, cfg); r.GroupEntryCount(0) != 1 {
		t.Fatalf("group 0 count = %d, want 1", r.GroupEntryCount(0))
	}
}

// TestTryDeleteAtCompressed_ReencodingGrowthIsBounded is grove's tight worst
// case: deleting the middle of "x" / "y"+50×"z" / "y"+50×"z"+"w" forces the
// successor to re-encode against "x" (shared 0) instead of "y"+50×"z"
// (shared 51) — the successor ENTRY grows ~51 bytes, yet the page still shrinks
// because the removed ~60-byte entry dominates. Proves byteDelta < 0 even at the
// pathological maximum.
func TestTryDeleteAtCompressed_ReencodingGrowthIsBounded(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 16}
	prefix := bytes.Repeat([]byte("z"), 50)
	base := []LeafEntry{
		{Key: []byte("x"), Value: []byte("v")},
		{Key: append([]byte("y"), prefix...), Value: []byte("v")},
		{Key: append(append([]byte("y"), prefix...), 'w'), Value: []byte("v")},
	}
	checkDelete(t, cfg, base, 1)
}

// TestTryDeleteAtCompressed_AlwaysShrinks deletes EVERY index (on a fresh copy)
// of a multi-group page that mixes single-entry groups (D-A), full groups, and a
// short tail group — so every case fires — and asserts each delete succeeds and
// decodes correctly. The splice never declines for space (a delete always
// shrinks the page).
func TestTryDeleteAtCompressed_AlwaysShrinks(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4}
	var base []LeafEntry
	// Two no-shared-prefix keys → two single-entry groups (D-A).
	base = append(base, LeafEntry{Key: []byte("aaa"), Value: bytes.Repeat([]byte("v"), 5)})
	base = append(base, LeafEntry{Key: []byte("mmm"), Value: bytes.Repeat([]byte("v"), 5)})
	// Then a long shared-prefix run → several full groups + a short tail.
	for i := range 22 {
		base = append(base, LeafEntry{
			Key:   fmt.Appendf(nil, "zzz-%04d-tail", i),
			Value: bytes.Repeat([]byte("v"), 1+i%7),
		})
	}
	if _, ok := tryBuild(cfg, base); !ok {
		t.Fatalf("base (%d entries) did not fit", len(base))
	}
	for idx := range base {
		t.Run(fmt.Sprintf("idx=%d", idx), func(t *testing.T) {
			checkDelete(t, cfg, base, idx)
		})
	}
}

func TestTryDeleteAtCompressed_EmptyDeclines(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 16}
	buf, ok := tryBuild(cfg, []LeafEntry{{Key: []byte("only"), Value: []byte("v")}})
	if !ok {
		t.Fatal("base did not fit")
	}
	before := bytes.Clone(buf)
	if TryDeleteAt(buf, cfg, 0) {
		t.Fatal("TryDeleteAt should decline a count==1 page (it would empty — caller frees it)")
	}
	if !bytes.Equal(buf, before) {
		t.Fatal("TryDeleteAt mutated the page on empty-decline")
	}
}

func TestTryDeleteAtCompressed_VariantDeclines(t *testing.T) {
	base := []LeafEntry{{Key: []byte("aaa"), Value: []byte("1")}, {Key: []byte("bbb"), Value: []byte("2")}}
	buf, ok := tryBuild(Config{PageSize: 4096, RestartGroupTarget: 16}, base)
	if !ok {
		t.Fatal("base did not fit")
	}
	before := bytes.Clone(buf)
	// Compressed page, but the keyspace is now configured uncompressed (RGT=1).
	if TryDeleteAt(buf, Config{PageSize: 4096, RestartGroupTarget: 1}, 0) {
		t.Fatal("TryDeleteAt should decline a compressed page when RGT=1 (migrate via rebuild)")
	}
	if !bytes.Equal(buf, before) {
		t.Fatal("TryDeleteAt mutated the page on variant-decline")
	}
}

// (Uncompressed delete is now spliced in place — covered positively by
// TestUCTryDeleteAt_MatchesRebuild, and the uncompressed-page-under-compressed-
// config decline by TestUCSpliceVariantMismatchDeclines.)

// TestTryDeleteAtCompressed_WithChecksum runs the splice under PageChecksum:true,
// which lowers ContentEnd by FooterSize and relocates the restart table
// FooterSize bytes lower — confirming the splice's cfg.ContentEnd()-based math
// places the table correctly. (The footer itself is recomputed at commit time,
// not by the splice; Validate checks structure within ContentEnd only.)
func TestTryDeleteAtCompressed_WithChecksum(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 4, PageChecksum: true}
	mk := func(k, v string) LeafEntry { return LeafEntry{Key: []byte(k), Value: []byte(v)} }
	base := []LeafEntry{
		mk("aaa", "x"), // single-entry group (D-A reachable)
		mk("key-0", "0"), mk("key-1", "1"), mk("key-2", "2"), mk("key-3", "3"), mk("key-4", "4"),
	}
	checkDelete(t, cfg, base, 0) // D-A under checksum (restart table relocates)
	checkDelete(t, cfg, base, 2) // D-C under checksum
}

// FuzzTryDeleteAtCompressed asserts the semantic + structural invariant over
// random leaves and random delete positions: a successful splice decodes to the
// exact base-minus-idx sequence, passes Validate, and leaves free space zeroed;
// a count==1 page declines unchanged.
func FuzzTryDeleteAtCompressed(f *testing.F) {
	f.Add(uint64(1), uint64(0))
	f.Add(uint64(42), uint64(3))
	f.Add(uint64(0xDEADBEEF), uint64(5))

	f.Fuzz(func(t *testing.T, leafSeed, posSeed uint64) {
		cfg := Config{PageSize: 4096, RestartGroupTarget: 16}
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
			// count==1: the page would empty; the splice declines, unchanged.
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

// BenchmarkTryDeleteAtCompressed measures the per-call cost of a mid-group
// delete at various group fullness and value sizes (clone successor + shift per
// iteration). Compare against the decode/re-encode fallback it replaces.
func BenchmarkTryDeleteAtCompressed(b *testing.B) {
	for _, gc := range []int{4, 8, 16} {
		for _, valSize := range []int{8, 100} {
			b.Run(fmt.Sprintf("gc=%d/val=%d", gc, valSize), func(b *testing.B) {
				cfg := Config{PageSize: 4096, RestartGroupTarget: 16}
				val := bytes.Repeat([]byte("v"), valSize)
				var base []LeafEntry
				for i := range gc {
					base = append(base, LeafEntry{Key: fmt.Appendf(nil, "key-%04d", i), Value: val})
				}
				prebuilt, ok := tryBuild(cfg, base)
				if !ok {
					b.Fatalf("base did not fit (gc=%d val=%d)", gc, valSize)
				}
				work := make([]byte, cfg.PageSize)
				deleteIdx := gc / 2
				b.ResetTimer()
				b.ReportAllocs()
				for range b.N {
					copy(work, prebuilt)
					TryDeleteAt(work, cfg, deleteIdx)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Uncompressed insert / delete — CANONICAL splices (a sorted, packed entry array
// + sorted offset table → array insert/delete is byte-identical to a rebuild),
// so the oracle is byte-identity, not the compressed ops' semantic oracle.
// ---------------------------------------------------------------------------

// assertUCInsertMatchesRebuild: the spliced page must equal a LeafBuilder
// rebuild of base with e at idx; or, if that rebuild overflows, TryInsertAt must
// decline leaving the page byte-unchanged.
func assertUCInsertMatchesRebuild(t *testing.T, cfg Config, base []LeafEntry, idx int, e LeafEntry) {
	t.Helper()
	orig, ok := tryBuild(cfg, base)
	if !ok {
		t.Fatalf("base (%d entries) did not fit", len(base))
	}
	expected, expFits := tryBuild(cfg, insertExpected(base, e, idx))
	actual := bytes.Clone(orig)
	got := TryInsertAt(actual, cfg, idx, e)
	if !expFits {
		if got {
			t.Fatalf("TryInsertAt succeeded but a rebuild overflowed (base=%d idx=%d)", len(base), idx)
		}
		if !bytes.Equal(actual, orig) {
			t.Fatalf("TryInsertAt declined but mutated the page")
		}
		return
	}
	if !got {
		t.Fatalf("TryInsertAt declined but a rebuild fit (base=%d idx=%d)", len(base), idx)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("uncompressed insert != rebuild (must be canonical): base=%d idx=%d", len(base), idx)
	}
	if err := NewLeafReader(actual, cfg).Validate(); err != nil {
		t.Fatalf("spliced page fails Validate: %v", err)
	}
}

// assertUCDeleteMatchesRebuild: for len(base) > 1 the spliced page must equal a
// rebuild of base without idx; for len(base) == 1 the splice declines (the page
// would empty — the btree slow path retires it), page byte-unchanged.
func assertUCDeleteMatchesRebuild(t *testing.T, cfg Config, base []LeafEntry, idx int) {
	t.Helper()
	orig, ok := tryBuild(cfg, base)
	if !ok {
		t.Fatalf("base (%d entries) did not fit", len(base))
	}
	actual := bytes.Clone(orig)
	got := TryDeleteAt(actual, cfg, idx)
	if len(base) == 1 {
		if got {
			t.Fatalf("TryDeleteAt should decline a count==1 page")
		}
		if !bytes.Equal(actual, orig) {
			t.Fatalf("TryDeleteAt mutated a count==1 page on decline")
		}
		return
	}
	if !got {
		t.Fatalf("TryDeleteAt declined for count=%d idx=%d (uncompressed delete always shrinks)", len(base), idx)
	}
	expected, _ := tryBuild(cfg, deleteExpected(base, idx))
	if !bytes.Equal(actual, expected) {
		t.Fatalf("uncompressed delete != rebuild (must be canonical): base=%d idx=%d", len(base), idx)
	}
	if err := NewLeafReader(actual, cfg).Validate(); err != nil {
		t.Fatalf("spliced page fails Validate: %v", err)
	}
}

func TestUCTryInsertAt_MatchesRebuild(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1}
	mk := func(k, v string) LeafEntry { return LeafEntry{Key: []byte(k), Value: []byte(v)} }
	ovf := func(k string, pg, tl uint64) LeafEntry {
		return LeafEntry{Flags: CellFlagOverflow, Key: []byte(k), OverflowPage: pg, TotalLen: tl}
	}
	t.Run("front", func(t *testing.T) {
		assertUCInsertMatchesRebuild(t, cfg, []LeafEntry{mk("bbb", "1"), mk("ccc", "2")}, 0, mk("aaa", "x"))
	})
	t.Run("interior", func(t *testing.T) {
		assertUCInsertMatchesRebuild(t, cfg, []LeafEntry{mk("aaa", "1"), mk("ccc", "2")}, 1, mk("bbb", "x"))
	})
	t.Run("new overflow interior", func(t *testing.T) {
		assertUCInsertMatchesRebuild(t, cfg, []LeafEntry{mk("aaa", "1"), mk("ccc", "2")}, 1, ovf("bbb", 7, 50000))
	})
	t.Run("overflow successor displaced", func(t *testing.T) {
		assertUCInsertMatchesRebuild(t, cfg, []LeafEntry{mk("aaa", "1"), ovf("ccc", 9, 99)}, 1, mk("bbb", "x"))
	})
	t.Run("nested successor displaced", func(t *testing.T) {
		nested := LeafEntry{Flags: CellFlagMultiValue | CellFlagNestedTree, Key: []byte("ccc"), NestedRoot: 3, NestedCount: 4}
		assertUCInsertMatchesRebuild(t, cfg, []LeafEntry{mk("aaa", "1"), nested}, 1, mk("bbb", "x"))
	})
	t.Run("many-entry interior", func(t *testing.T) {
		var base []LeafEntry
		for i := range 10 {
			base = append(base, LeafEntry{Key: fmt.Appendf(nil, "key-%04d", i*2), Value: []byte("v")})
		}
		assertUCInsertMatchesRebuild(t, cfg, base, 5, mk("key-0009", "x"))
	})
}

func TestUCTryDeleteAt_MatchesRebuild(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1}
	mk := func(k, v string) LeafEntry { return LeafEntry{Key: []byte(k), Value: []byte(v)} }
	// Mixed cell kinds; delete every position and assert byte-identity to a
	// rebuild of the survivors.
	base := []LeafEntry{
		mk("aaa", "1"),
		{Flags: CellFlagOverflow, Key: []byte("bbb"), OverflowPage: 7, TotalLen: 50000},
		mk("ccc", "3"),
		{Flags: CellFlagMultiValue | CellFlagNestedTree, Key: []byte("ddd"), NestedRoot: 9, NestedCount: 4},
		mk("eee", "5"),
	}
	for idx := range base {
		t.Run(fmt.Sprintf("idx=%d", idx), func(t *testing.T) {
			assertUCDeleteMatchesRebuild(t, cfg, base, idx)
		})
	}
	t.Run("single-entry declines", func(t *testing.T) {
		assertUCDeleteMatchesRebuild(t, cfg, []LeafEntry{mk("only", "v")}, 0)
	})
}

// TestUCSpliceVariantMismatchDeclines: an uncompressed page under a compressed
// config (RGT>=2) must decline insert AND delete, byte-unchanged (migrate via
// rebuild) — the dispatchers' variant switch, exercised for the insert/delete ops.
func TestUCSpliceVariantMismatchDeclines(t *testing.T) {
	ucCfg := Config{PageSize: 4096, RestartGroupTarget: 1}
	compressedCfg := Config{PageSize: 4096, RestartGroupTarget: 16}
	base := []LeafEntry{{Key: []byte("aaa"), Value: []byte("1")}, {Key: []byte("ccc"), Value: []byte("2")}}
	ucBuf, ok := tryBuild(ucCfg, base)
	if !ok {
		t.Fatal("base did not fit")
	}
	if ucBuf[0] != TypeLeafUncompressed {
		t.Fatalf("base is not uncompressed: type=%d", ucBuf[0])
	}
	ins := bytes.Clone(ucBuf)
	if TryInsertAt(ins, compressedCfg, 1, LeafEntry{Key: []byte("bbb"), Value: []byte("x")}) {
		t.Fatal("TryInsertAt should decline an uncompressed page under RGT>=2")
	}
	if !bytes.Equal(ins, ucBuf) {
		t.Fatal("TryInsertAt mutated the page on variant-decline")
	}
	del := bytes.Clone(ucBuf)
	if TryDeleteAt(del, compressedCfg, 1) {
		t.Fatal("TryDeleteAt should decline an uncompressed page under RGT>=2")
	}
	if !bytes.Equal(del, ucBuf) {
		t.Fatal("TryDeleteAt mutated the page on variant-decline")
	}
}

// FuzzUCTryInsertAt asserts the byte-identity invariant for uncompressed inserts
// over random leaves + random interior positions/keys (mixed cell kinds).
func FuzzUCTryInsertAt(f *testing.F) {
	f.Add(uint64(1), uint64(2), uint64(0))
	f.Add(uint64(42), uint64(7), uint64(3))
	f.Add(uint64(0xDEADBEEF), uint64(0xCAFEBABE), uint64(5))

	f.Fuzz(func(t *testing.T, leafSeed, keySeed, posSeed uint64) {
		cfg := Config{PageSize: 4096, RestartGroupTarget: 1}
		base := randomFittingMixed(leafSeed, cfg)
		if len(base) == 0 {
			return
		}
		insertIdx := int(posSeed % uint64(len(base))) // [0, len): interior/front
		var lo []byte
		if insertIdx > 0 {
			lo = base[insertIdx-1].Key
		}
		newKey, ok := keyBetween(lo, base[insertIdx].Key, keySeed)
		if !ok {
			return
		}
		e := randomLeafEntry(rand.New(rand.NewPCG(keySeed, keySeed^0xABCD1234EF567890)), newKey)
		assertUCInsertMatchesRebuild(t, cfg, base, insertIdx, e)
	})
}

// FuzzUCTryDeleteAt asserts the byte-identity invariant for uncompressed deletes
// over random leaves + random positions (mixed cell kinds).
func FuzzUCTryDeleteAt(f *testing.F) {
	f.Add(uint64(1), uint64(0))
	f.Add(uint64(42), uint64(3))
	f.Add(uint64(0xDEADBEEF), uint64(5))

	f.Fuzz(func(t *testing.T, leafSeed, posSeed uint64) {
		cfg := Config{PageSize: 4096, RestartGroupTarget: 1}
		base := randomFittingMixed(leafSeed, cfg)
		if len(base) == 0 {
			return
		}
		assertUCDeleteMatchesRebuild(t, cfg, base, int(posSeed%uint64(len(base))))
	})
}

// BenchmarkUCTryInsertAt / BenchmarkUCTryDeleteAt measure the per-call cost of an
// uncompressed mid-page splice (in-place, zero-alloc — no clones, unlike the
// compressed ops). Compare against the decode/re-encode fallback they replace.
func BenchmarkUCTryInsertAt(b *testing.B) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1}
	for _, n := range []int{8, 32} {
		for _, valSize := range []int{8, 100} {
			b.Run(fmt.Sprintf("n=%d/val=%d", n, valSize), func(b *testing.B) {
				val := bytes.Repeat([]byte("v"), valSize)
				var base []LeafEntry
				for i := range n {
					base = append(base, LeafEntry{Key: fmt.Appendf(nil, "key-%04d", i*2), Value: val})
				}
				prebuilt, ok := tryBuild(cfg, base)
				if !ok {
					b.Fatalf("base did not fit (n=%d val=%d)", n, valSize)
				}
				work := make([]byte, cfg.PageSize)
				insertIdx := n / 2
				e := LeafEntry{Key: fmt.Appendf(nil, "key-%04d", insertIdx*2-1), Value: val}
				b.ResetTimer()
				b.ReportAllocs()
				for range b.N {
					copy(work, prebuilt)
					TryInsertAt(work, cfg, insertIdx, e)
				}
			})
		}
	}
}

func BenchmarkUCTryDeleteAt(b *testing.B) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1}
	for _, n := range []int{8, 32} {
		for _, valSize := range []int{8, 100} {
			b.Run(fmt.Sprintf("n=%d/val=%d", n, valSize), func(b *testing.B) {
				val := bytes.Repeat([]byte("v"), valSize)
				var base []LeafEntry
				for i := range n {
					base = append(base, LeafEntry{Key: fmt.Appendf(nil, "key-%04d", i), Value: val})
				}
				prebuilt, ok := tryBuild(cfg, base)
				if !ok {
					b.Fatalf("base did not fit (n=%d val=%d)", n, valSize)
				}
				work := make([]byte, cfg.PageSize)
				deleteIdx := n / 2
				b.ResetTimer()
				b.ReportAllocs()
				for range b.N {
					copy(work, prebuilt)
					TryDeleteAt(work, cfg, deleteIdx)
				}
			})
		}
	}
}

// randomFittingEntries builds a random sorted, inline-only entry set and
// returns the prefix that fits one page (stops at the first page-full). The
// returned entries are independently owned (cloned off the build buffer).
func randomFittingEntries(seed uint64, cfg Config) []LeafEntry {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	n := 1 + r.IntN(60)
	keys := randomSortedKeys(r, n)
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	out := make([]LeafEntry, 0, len(keys))
	for _, k := range keys {
		v := randomValue(r)
		if !b.AddInline(k, v) {
			break
		}
		out = append(out, LeafEntry{Key: bytes.Clone(k), Value: bytes.Clone(v)})
	}
	return out
}

func randomSortedKeys(r *rand.Rand, n int) [][]byte {
	set := make(map[string]struct{}, n)
	for attempts := 0; len(set) < n && attempts < n*4; attempts++ {
		kl := 1 + r.IntN(18)
		k := make([]byte, kl)
		for i := range k {
			k[i] = byte(r.IntN(256))
		}
		// Bias toward shared prefixes so delta chains get exercised.
		if r.IntN(2) == 0 && kl >= 4 {
			copy(k, "aaaa")
		}
		set[string(k)] = struct{}{}
	}
	keys := make([][]byte, 0, len(set))
	for k := range set {
		keys = append(keys, []byte(k))
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
	return keys
}

func randomValue(r *rand.Rand) []byte {
	v := make([]byte, r.IntN(48))
	for i := range v {
		v[i] = byte(r.IntN(256))
	}
	return v
}

// keyAfterSeed returns a key that sorts strictly after last. It alternates
// between a zero-shared-prefix successor (exercises the natural-break restart
// branch) and a shared-prefix successor (exercises the delta / target-cap
// branches).
func keyAfterSeed(last []byte, seed uint64) []byte {
	if seed&1 == 0 && len(last) > 0 && last[0] < 0xff {
		return []byte{last[0] + 1}
	}
	return append(bytes.Clone(last), byte(1+seed%255))
}

// randomLeafEntry returns a random-cell-kind entry for key: overflow,
// nested-tree, subpage, or (most often) inline. The trailer u64s / subpage
// bytes are arbitrary — the splice only round-trips them, so semantic equality
// is all the oracle checks. Never sets the mutually-exclusive Overflow|MultiValue
// combo (which LeafBuilder.AddEntry rejects).
func randomLeafEntry(r *rand.Rand, key []byte) LeafEntry {
	switch r.IntN(5) {
	case 0:
		return LeafEntry{Flags: CellFlagOverflow, Key: key, OverflowPage: r.Uint64(), TotalLen: r.Uint64()}
	case 1:
		return LeafEntry{Flags: CellFlagMultiValue | CellFlagNestedTree, Key: key, NestedRoot: r.Uint64(), NestedCount: r.Uint64()}
	case 2:
		return LeafEntry{Flags: CellFlagMultiValue, Key: key, Value: randomValue(r)} // subpage
	default:
		return LeafEntry{Key: key, Value: randomValue(r)} // inline (2/5)
	}
}

// randomFittingMixed is randomFittingEntries with random cell kinds (built via
// AddEntry). Returns the sorted prefix that fits one page.
func randomFittingMixed(seed uint64, cfg Config) []LeafEntry {
	r := rand.New(rand.NewPCG(seed, seed^0x2545f4914f6cdd1d))
	keys := randomSortedKeys(r, 1+r.IntN(50))
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	out := make([]LeafEntry, 0, len(keys))
	for _, k := range keys {
		e := randomLeafEntry(r, bytes.Clone(k))
		if !b.AddEntry(e) {
			break
		}
		out = append(out, e)
	}
	return out
}
