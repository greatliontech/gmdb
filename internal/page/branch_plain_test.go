package page

import (
	"bytes"
	"math/rand/v2"
	"sort"
	"testing"
)

// Tests for the plain branch layout (page-formats.md §Plain Branch)
// beyond the simple cases in branch_test.go: deep-shared-prefix
// separator sets, a search battery that includes targets diverging
// inside those shared bytes, and byte-identical determinism.

// refBranchSearch is an independent linear reference for BranchSearch: the
// first index i with target < cells[i].Key (full keys), or n. BranchSearch
// must agree with this for every target.
func refBranchSearch(cells []BranchCell, target []byte) uint16 {
	for i, c := range cells {
		if bytes.Compare(target, c.Key) < 0 {
			return uint16(i)
		}
	}
	return uint16(len(cells))
}

// TestBranchRoundTripSharedPrefixShapes round-trips cell sets with
// varied shared-prefix depths (none, partial, deep) through
// EncodeBranch/DecodeBranch and cross-checks DecodeBranch against
// per-index BranchCellAt — the shapes stay in the battery because
// separator sets with deep shared bytes are the regime the plain
// layout must handle without any prefix machinery.
func TestBranchRoundTripSharedPrefixShapes(t *testing.T) {
	for _, cfg := range []Config{
		{PageSize: 4096, BranchLayout: BranchLayoutPlain},
		{PageSize: 4096, BranchLayout: BranchLayoutSegregated},
	} {
		testBranchRoundTripSharedPrefixShapes(t, cfg)
	}
}

func testBranchRoundTripSharedPrefixShapes(t *testing.T, cfg Config) {
	deep := bytes.Repeat([]byte("shared/prefix/"), 30) // ~420-byte common prefix
	cases := map[string][]BranchCell{
		"no shared prefix": {
			{Key: []byte("alpha"), Child: 1},
			{Key: []byte("bravo"), Child: 2},
			{Key: []byte("charlie"), Child: 3},
		},
		"deep shared prefix": {
			{Key: append(append([]byte(nil), deep...), []byte("0001")...), Child: 10},
			{Key: append(append([]byte(nil), deep...), []byte("0007")...), Child: 20},
			{Key: append(append([]byte(nil), deep...), []byte("0042")...), Child: 30},
			{Key: append(append([]byte(nil), deep...), []byte("9999")...), Child: 40},
		},
		"cluster seam (deep + tiny)": {
			{Key: append(append([]byte(nil), deep...), []byte("aaa")...), Child: 5},
			{Key: append(append([]byte(nil), deep...), []byte("zzz")...), Child: 6},
			{Key: []byte("~next-cluster"), Child: 7}, // diverges at byte 0 → P collapses
		},
		"single cell": {
			{Key: append(append([]byte(nil), deep...), []byte("only")...), Child: 99},
		},
	}
	for name, cells := range cases {
		t.Run(name, func(t *testing.T) {
			buf := make([]byte, cfg.PageSize)
			if err := EncodeBranch(buf, cfg, 1000, cells); err != nil {
				t.Fatalf("EncodeBranch: %v", err)
			}
			lm, got := DecodeBranch(buf, cfg)
			if lm != 1000 {
				t.Errorf("leftmost = %d, want 1000", lm)
			}
			if len(got) != len(cells) {
				t.Fatalf("decoded %d cells, want %d", len(got), len(cells))
			}
			for i, c := range cells {
				if !bytes.Equal(got[i].Key, c.Key) {
					t.Errorf("cell %d key: got %q want %q", i, got[i].Key, c.Key)
				}
				if got[i].Child != c.Child {
					t.Errorf("cell %d child: got %d want %d", i, got[i].Child, c.Child)
				}
				// BranchCellAt must agree with DecodeBranch.
				at := BranchCellAt(buf, cfg, uint16(i))
				if !bytes.Equal(at.Key, c.Key) || at.Child != c.Child {
					t.Errorf("BranchCellAt(%d) = {%q,%d}, want {%q,%d}", i, at.Key, at.Child, c.Key, c.Child)
				}
			}
			if err := ValidateBranch(buf, cfg); err != nil {
				t.Errorf("ValidateBranch on well-formed page: %v", err)
			}
		})
	}
}

// TestBranchSearchEquivalence: BranchSearch must return the SAME descent
// index as a linear search over the full keys, for every target. The
// battery keeps the deep-shared-prefix shapes (exact separators,
// prefixes of the shared bytes, one-byte flips at every shared
// position, shorter/longer targets, fully random keys) — cheap
// insurance that the plain layout's single-phase search has no
// prefix-dependent blind spots.
func TestBranchSearchEquivalence(t *testing.T) {
	for _, cfg := range []Config{
		{PageSize: 4096, BranchLayout: BranchLayoutPlain},
		{PageSize: 4096, BranchLayout: BranchLayoutSegregated},
	} {
		testBranchSearchEquivalence(t, cfg)
	}
}

func testBranchSearchEquivalence(t *testing.T, cfg Config) {
	rng := rand.New(rand.NewPCG(0x5eed, 0xb00c))

	makeCells := func(prefix []byte, n int) []BranchCell {
		seen := map[string]struct{}{}
		var cells []BranchCell
		for len(cells) < n {
			// suffix: 1–6 random bytes after the shared prefix.
			sfx := make([]byte, 1+rng.IntN(6))
			for i := range sfx {
				sfx[i] = byte(rng.IntN(256))
			}
			full := append(append([]byte(nil), prefix...), sfx...)
			if _, dup := seen[string(full)]; dup {
				continue
			}
			seen[string(full)] = struct{}{}
			cells = append(cells, BranchCell{Key: full, Child: uint64(len(cells) + 1)})
		}
		sort.Slice(cells, func(i, j int) bool { return bytes.Compare(cells[i].Key, cells[j].Key) < 0 })
		return cells
	}

	prefixes := [][]byte{
		{},                                      // P == 0 (no sharing) — degenerate
		[]byte("p"),                             // 1-byte
		bytes.Repeat([]byte("deep/shared/"), 8), // ~96-byte deep prefix
	}
	for pi, prefix := range prefixes {
		for _, n := range []int{1, 2, 8, 40} {
			cells := makeCells(prefix, n)
			buf := make([]byte, cfg.PageSize)
			if BranchEncodedSize(cfg, cells) > cfg.ContentEnd() {
				continue // too many cells for a page at this prefix; skip
			}
			if err := EncodeBranch(buf, cfg, 7, cells); err != nil {
				t.Fatalf("prefix#%d n=%d encode: %v", pi, n, err)
			}

			// Build the target battery.
			var targets [][]byte
			add := func(b []byte) { targets = append(targets, b) }
			for _, c := range cells {
				add(append([]byte(nil), c.Key...))              // exact separator
				add(append(append([]byte(nil), c.Key...), 'x')) // separator + 1 byte
				if len(c.Key) > 0 {
					add(c.Key[:len(c.Key)-1]) // separator minus last byte
				}
			}
			// Prefixes of P and P with one byte flipped at each position — the
			// "diverge within P" cases.
			for k := 0; k <= len(prefix); k++ {
				add(append([]byte(nil), prefix[:k]...))
				if k < len(prefix) {
					flipUp := append([]byte(nil), prefix...)
					flipUp[k]++ // diverge ABOVE P at position k
					add(flipUp[:k+1])
					if prefix[k] > 0 {
						flipDn := append([]byte(nil), prefix...)
						flipDn[k]-- // diverge BELOW P at position k
						add(flipDn[:k+1])
					}
				}
			}
			add([]byte{})                 // empty target
			add([]byte{0x00})             // sorts before almost everything
			add([]byte{0xff, 0xff, 0xff}) // sorts after almost everything
			for range 20 {                // random keys
				r := make([]byte, rng.IntN(len(prefix)+8))
				for i := range r {
					r[i] = byte(rng.IntN(256))
				}
				add(r)
			}

			for _, tg := range targets {
				got, _ := BranchSearch(buf, cfg, tg, NoExtentTail)
				want := refBranchSearch(cells, tg)
				if got != want {
					t.Fatalf("prefix#%d n=%d: BranchSearch(%q) = %d, want %d (cells=%d, P=%d)",
						pi, n, tg, got, want, len(cells), len(prefix))
				}
				// And the chosen child must match the reference child.
				gotChild := BranchChildAt(buf, cfg, got)
				var wantChild uint64
				if want == 0 {
					wantChild = 7 // leftmost
				} else {
					wantChild = cells[want-1].Child
				}
				if gotChild != wantChild {
					t.Fatalf("prefix#%d n=%d: child for %q = %d, want %d", pi, n, tg, gotChild, wantChild)
				}
			}
		}
	}
}

// TestBranchEncodeDeterministic pins the page-formats.md §Branch Page
// determinism invariant: EncodeBranch is a pure function of (cfg, leftmost,
// cells) — encoding the same input twice yields byte-identical pages, and
// re-encoding a decoded page reproduces the original bytes (the property
// Check() repair / recovery rely on).
func TestBranchEncodeDeterministic(t *testing.T) {
	for _, checksum := range []bool{false, true} {
		for _, layout := range []BranchLayout{BranchLayoutPlain, BranchLayoutSegregated} {
			testBranchEncodeDeterministic(t, Config{PageSize: 4096, PageChecksum: checksum, BranchLayout: layout})
		}
	}
}

func testBranchEncodeDeterministic(t *testing.T, cfg Config) {
	{
		deep := bytes.Repeat([]byte("x/y/z/"), 40)
		cells := []BranchCell{
			{Key: append(append([]byte(nil), deep...), []byte("01")...), Child: 11},
			{Key: append(append([]byte(nil), deep...), []byte("02")...), Child: 22},
			{Key: append(append([]byte(nil), deep...), []byte("99")...), Child: 33},
		}
		a := make([]byte, cfg.PageSize)
		b := make([]byte, cfg.PageSize)
		if err := EncodeBranch(a, cfg, 4, cells); err != nil {
			t.Fatalf("encode a: %v", err)
		}
		if err := EncodeBranch(b, cfg, 4, cells); err != nil {
			t.Fatalf("encode b: %v", err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("cfg=%+v: two encodes of the same input differ", cfg)
		}
		// Decode then re-encode → byte-identical.
		lm, decoded := DecodeBranch(a, cfg)
		c := make([]byte, cfg.PageSize)
		if err := EncodeBranch(c, cfg, lm, decoded); err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(a, c) {
			t.Errorf("cfg=%+v: re-encode of decoded page is not byte-identical", cfg)
		}
	}
}

// FuzzBranchSearchAndRoundTrip: random sorted cell sets — shared
// prefixes of seed-driven depth, a sprinkle of overflow-key cells with
// resident first-T slices — must (a) round-trip byte-identically
// through Encode/Decode/Encode, (b) pass ValidateBranch, and (c) agree
// with the linear refBranchSearch for a probe battery, with overflow
// ties resolved through a synthetic tail comparator. The layout under
// test is seed-selected so one corpus covers both branch variants.
func FuzzBranchSearchAndRoundTrip(f *testing.F) {
	f.Add(uint64(1), uint64(2))
	f.Add(uint64(42), uint64(7))
	f.Add(uint64(0xDEADBEEF), uint64(0xCAFEBABE))

	f.Fuzz(func(t *testing.T, cellSeed, probeSeed uint64) {
		cfg := Config{PageSize: 4096, BranchLayout: BranchLayoutPlain}
		if cellSeed%2 == 1 {
			cfg.BranchLayout = BranchLayoutSegregated
		}
		tt := cfg.InlineThreshold()
		rng := rand.New(rand.NewPCG(cellSeed, cellSeed^0x9E3779B97F4A7C15))

		prefix := bytes.Repeat([]byte{'p'}, rng.IntN(60))
		fulls := map[string][]byte{} // resident-string → full key (ovk)
		seen := map[string]struct{}{}
		var cells []BranchCell
		nWant := 1 + rng.IntN(30)
		for len(cells) < nWant {
			sfx := make([]byte, 1+rng.IntN(8))
			for i := range sfx {
				sfx[i] = byte(rng.IntN(256))
			}
			full := append(bytes.Clone(prefix), sfx...)
			if _, dup := seen[string(full)]; dup {
				continue
			}
			seen[string(full)] = struct{}{}
			c := BranchCell{Key: full, Child: uint64(len(cells) + 1)}
			if rng.IntN(8) == 0 && len(full) < tt {
				// Promote to an overflow cell: pad to T resident bytes.
				// The extent page is UNIQUE per cell (1000+i) — the tail
				// comparator identifies the cell by it.
				padded := make([]byte, tt)
				copy(padded, full)
				if _, dup := seen[string(padded)]; dup {
					continue
				}
				seen[string(padded)] = struct{}{}
				fullOvk := append(bytes.Clone(padded), 1+byte(rng.IntN(255)))
				c = BranchCell{Key: padded, Child: c.Child,
					KeyExtPage: uint64(1000 + len(cells)), KeyTotalLen: uint32(len(fullOvk))}
				fulls[string(padded)] = fullOvk
			}
			cells = append(cells, c)
		}
		sort.Slice(cells, func(i, j int) bool { return bytes.Compare(cells[i].Key, cells[j].Key) < 0 })
		buf := make([]byte, cfg.PageSize)
		if BranchEncodedSize(cfg, cells) > cfg.ContentEnd() {
			return
		}
		if err := EncodeBranch(buf, cfg, 7, cells); err != nil {
			t.Fatalf("encode: %v", err)
		}
		if err := ValidateBranch(buf, cfg); err != nil {
			t.Fatalf("validate: %v", err)
		}
		lm, decoded := DecodeBranch(buf, cfg)
		buf2 := make([]byte, cfg.PageSize)
		if err := EncodeBranch(buf2, cfg, lm, decoded); err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(buf, buf2) {
			t.Fatal("re-encode of decoded page is not byte-identical")
		}

		// Reference search over FULL keys (extent tails included).
		fullKey := func(c BranchCell) []byte {
			if c.IsOverflowKey() {
				return fulls[string(c.Key)]
			}
			return c.Key
		}
		tail := func(probe []byte, extPage uint64, totalLen uint32) (int, error) {
			// Identify the cell by its extent page (unique per cell).
			for _, c := range cells {
				if c.KeyExtPage == extPage {
					return bytes.Compare(probe, fullKey(c)), nil
				}
			}
			t.Fatalf("tail comparator: unknown extent page %d", extPage)
			return 0, nil
		}
		ref := func(target []byte) uint16 {
			for i, c := range cells {
				if bytes.Compare(target, fullKey(c)) < 0 {
					return uint16(i)
				}
			}
			return uint16(len(cells))
		}

		prng := rand.New(rand.NewPCG(probeSeed, probeSeed^0xABCD))
		var probes [][]byte
		for _, c := range cells {
			fk := fullKey(c)
			probes = append(probes, bytes.Clone(fk))
			probes = append(probes, append(bytes.Clone(fk), 'x'))
			if len(fk) > 1 {
				probes = append(probes, fk[:len(fk)-1])
			}
			if c.IsOverflowKey() {
				// Resident-tying probes that the EXTENT must decide,
				// in both directions: full key with last byte -1
				// (extent says probe < separator) and +1 where legal.
				down := bytes.Clone(fk)
				down[len(down)-1]--
				probes = append(probes, down)
				if fk[len(fk)-1] < 0xFF {
					up := bytes.Clone(fk)
					up[len(up)-1]++
					probes = append(probes, up)
				}
			}
		}
		for range 12 {
			p := make([]byte, prng.IntN(len(prefix)+10))
			for i := range p {
				p[i] = byte(prng.IntN(256))
			}
			probes = append(probes, p)
		}
		for _, p := range probes {
			got, err := BranchSearch(buf, cfg, p, tail)
			if err != nil {
				t.Fatalf("BranchSearch(%q): %v", p, err)
			}
			if want := ref(p); got != want {
				t.Fatalf("BranchSearch(%q) = %d, want %d", p, got, want)
			}
		}
	})
}

// FuzzValidateBranchTotal pins the decoder-robustness contract for
// branch pages — the branch twin of FuzzLeafValidateTotal: any byte
// sequence either fails ValidateBranch cleanly (wrapped ErrCorrupted)
// or survives a full DecodeBranch + BranchSearch pass without panicking.
func FuzzValidateBranchTotal(f *testing.F) {
	cfg := Config{PageSize: 4096}
	for _, bl := range []BranchLayout{BranchLayoutPlain, BranchLayoutSegregated} {
		c := cfg
		c.BranchLayout = bl
		seed := make([]byte, c.PageSize)
		_ = EncodeBranch(seed, c, 5, []BranchCell{
			{Key: []byte("ccc"), Child: 10},
			{Key: []byte("mmm"), Child: 20},
		})
		f.Add(seed, byte(0), uint16(0))
	}
	f.Fuzz(func(t *testing.T, pg []byte, mutByte byte, mutOff uint16) {
		if len(pg) == 0 {
			return
		}
		buf := make([]byte, cfg.PageSize)
		copy(buf, pg)
		buf[int(mutOff)%len(buf)] ^= mutByte
		// Mirror the production boundary: callers gate on the type
		// byte before treating a page as a branch.
		if typ, _, _, _ := ReadHeader(buf); !IsBranchType(typ) {
			return
		}
		if err := ValidateBranch(buf, cfg); err != nil {
			return
		}
		lm, cells := DecodeBranch(buf, cfg)
		_ = lm
		for _, c := range cells {
			if _, err := BranchSearch(buf, cfg, c.Key, func([]byte, uint64, uint32) (int, error) { return 0, nil }); err != nil {
				t.Fatalf("BranchSearch on validated page: %v", err)
			}
		}
	})
}
