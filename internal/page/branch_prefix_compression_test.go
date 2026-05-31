package page

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
)

// Tests for within-page branch prefix truncation (page-formats.md §Branch
// Page). These exercise the compression-specific invariants the simpler
// branch_test.go cases (single-byte / distinct-first-byte keys, prefixLen 0)
// do not reach: deep shared prefixes, search against a target that diverges
// WITHIN the shared prefix, byte-identical determinism, and that compression
// actually shrinks the page.

// refBranchSearch is an independent linear reference for BranchSearch: the
// first index i with target < cells[i].Key (full keys), or n. The compressed
// BranchSearch must agree with this for every target (page-formats.md §Branch
// Page search-equivalence invariant).
func refBranchSearch(cells []BranchCell, target []byte) uint16 {
	for i, c := range cells {
		if bytes.Compare(target, c.Key) < 0 {
			return uint16(i)
		}
	}
	return uint16(len(cells))
}

// TestBranchPrefixCompressionRoundTrip round-trips cell sets with varied
// shared-prefix depths (none, partial, deep) through EncodeBranch/DecodeBranch
// and cross-checks DecodeBranch against per-index BranchCellAt. The
// reconstructed full key must equal the original (P || suffix), and the
// page-wide PrefixLen must equal the cells' common prefix length.
func TestBranchPrefixCompressionRoundTrip(t *testing.T) {
	cfg := Config{PageSize: 4096}
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
			// Page-wide prefix must equal commonPrefix(first, last).
			wantPfx := sharedPrefixLen(cells[0].Key, cells[len(cells)-1].Key)
			if got := branchPrefixLen(buf); got != wantPfx {
				t.Errorf("PrefixLen = %d, want %d", got, wantPfx)
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
				t.Errorf("ValidateBranch on well-formed compressed page: %v", err)
			}
		})
	}
}

// TestBranchSearchEquivalence is the load-bearing test for the page-formats.md
// §Branch Page search-equivalence invariant: the compressed two-step
// BranchSearch must return the SAME descent index as a linear search over the
// full keys, for every target — including the strongest counterexample, a
// target that shares part of the page-wide prefix P but diverges WITHIN it
// (which a suffix-only search would mis-route). It probes random sorted cell
// sets (deep shared prefixes, so P is long and non-trivial) against a target
// battery: exact separators, prefixes of P, P-with-one-byte-flipped at every
// position, targets shorter/longer than P, and fully random keys.
func TestBranchSearchEquivalence(t *testing.T) {
	cfg := Config{PageSize: 4096}
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
		{},                                     // P == 0 (no sharing) — degenerate
		[]byte("p"),                            // 1-byte
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
				add(append([]byte(nil), c.Key...))             // exact separator
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
			add([]byte{})       // empty target
			add([]byte{0x00})   // sorts before almost everything
			add([]byte{0xff, 0xff, 0xff}) // sorts after almost everything
			for range 20 {                // random keys
				r := make([]byte, rng.IntN(len(prefix)+8))
				for i := range r {
					r[i] = byte(rng.IntN(256))
				}
				add(r)
			}

			for _, tg := range targets {
				got := BranchSearch(buf, cfg, tg)
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
		cfg := Config{PageSize: 4096, PageChecksum: checksum}
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
			t.Errorf("checksum=%v: two encodes of the same input differ", checksum)
		}
		// Decode then re-encode → byte-identical.
		lm, decoded := DecodeBranch(a, cfg)
		c := make([]byte, cfg.PageSize)
		if err := EncodeBranch(c, cfg, lm, decoded); err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if !bytes.Equal(a, c) {
			t.Errorf("checksum=%v: re-encode of decoded page is not byte-identical", checksum)
		}
	}
}

// TestBranchPrefixCompressionEffective confirms within-page truncation
// actually shrinks the page: a branch full of deep-shared-prefix separators
// must fit far more cells than the uncompressed (logical) layout would, i.e.
// the compressed size is much smaller than BranchLogicalSize and fanout is
// high. This is the property page-formats.md §Branch Page targets: branches
// reach high fan-out for deep-shared-prefix keys (the shared prefix stored
// once) instead of collapsing toward fanout 2.
func TestBranchPrefixCompressionEffective(t *testing.T) {
	cfg := Config{PageSize: 4096}
	// ~700-byte within-cluster separators (the regime that collapsed fanout
	// to ~2 in the uncompressed format).
	prefix := bytes.Repeat([]byte("p"), 700)
	var cells []BranchCell
	for i := 0; ; i++ {
		k := append(append([]byte(nil), prefix...), fmt.Appendf(nil, "%06d", i)...)
		cells = append(cells, BranchCell{Key: k, Child: uint64(i + 1)})
		if BranchEncodedSize(cfg, cells) > cfg.ContentEnd() {
			cells = cells[:len(cells)-1] // last one overflowed
			break
		}
	}
	if len(cells) < 50 {
		t.Fatalf("compressed fanout = %d cells, want >= 50 (compression ineffective)", len(cells))
	}
	// The same cells uncompressed (logical) would overflow many pages.
	logical := BranchLogicalSize(cells)
	if logical <= cfg.ContentEnd() {
		t.Fatalf("logical size %d <= ContentEnd %d — test not exercising compression", logical, cfg.ContentEnd())
	}
	t.Logf("700-byte separators: compressed fanout=%d cells in one page; uncompressed would need %d bytes (%.1f pages)",
		len(cells), logical, float64(logical)/float64(cfg.ContentEnd()))
}
