package page

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// TestCompressedSearchKcplEquivalence property-pins the phase-2 kcpl
// skip-scan against a naive full-decode reference over deep-shared-
// prefix key sets — the workload whose deltas exercise every skip
// branch (SharedLen > kcpl skip, < kcpl stop, == kcpl compare). The
// scan's soundness argument lives on compressedSearchLeaf; this test
// is its enforcement.
func TestCompressedSearchKcplEquivalence(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	cfg := Config{PageSize: 4096, RestartGroupTarget: 16}

	for round := range 200 {
		// Cluster keys under a handful of shared prefixes with varied
		// depth so delta SharedLens straddle probe kcpls both ways.
		var keys [][]byte
		nClusters := 1 + rng.Intn(3)
		for c := range nClusters {
			prefix := bytes.Repeat([]byte{byte('a' + c)}, 1+rng.Intn(40))
			n := 2 + rng.Intn(12)
			for range n {
				k := bytes.Clone(prefix)
				depth := rng.Intn(3)
				for range depth {
					k = append(k, bytes.Repeat([]byte{byte('a' + rng.Intn(4))}, 1+rng.Intn(6))...)
				}
				k = append(k, byte(rng.Intn(256)))
				keys = append(keys, k)
			}
		}
		// Sort + dedup.
		for i := range keys {
			for j := i + 1; j < len(keys); j++ {
				if bytes.Compare(keys[j], keys[i]) < 0 {
					keys[i], keys[j] = keys[j], keys[i]
				}
			}
		}
		uniq := keys[:0]
		for i, k := range keys {
			if i == 0 || !bytes.Equal(k, keys[i-1]) {
				uniq = append(uniq, k)
			}
		}
		keys = uniq

		buf := make([]byte, cfg.PageSize)
		b := NewLeafBuilder(buf, cfg)
		added := 0
		for i, k := range keys {
			if !b.AddInline(k, fmt.Appendf(nil, "v%d", i)) {
				break
			}
			added++
		}
		if added < 2 {
			continue
		}
		keys = keys[:added]
		b.Finish()
		r := NewLeafReader(buf, cfg)
		if err := r.Validate(); err != nil {
			t.Fatalf("round %d: Validate: %v", round, err)
		}

		// Reference: naive full-decode scan via the iterator.
		reference := func(target []byte) (int, bool) {
			it := r.IterForReuse(nil, nil, nil)
			idx := 0
			for {
				e, ok := it.Next()
				if !ok {
					return idx, false
				}
				c := bytes.Compare(e.Key, target)
				if c == 0 {
					return idx, true
				}
				if c > 0 {
					return idx, false
				}
				idx++
			}
		}

		probe := func(target []byte) {
			wantIdx, wantFound := reference(target)
			gotIdx, gotEntry, gotFound, err := r.SearchLeaf(target, NoExtentTail)
			if err != nil {
				t.Fatalf("round %d: SearchLeaf(%q): %v", round, target, err)
			}
			if gotIdx != wantIdx || gotFound != wantFound {
				t.Fatalf("round %d: SearchLeaf(%q) = (%d, %v), reference (%d, %v)",
					round, target, gotIdx, gotFound, wantIdx, wantFound)
			}
			if gotFound {
				want, _ := r.EntryAt(wantIdx, nil)
				if !bytes.Equal(gotEntry.Value, want.Value) {
					t.Fatalf("round %d: SearchLeaf(%q) value = %q, want %q",
						round, target, gotEntry.Value, want.Value)
				}
			}
		}

		// Every present key, plus derived misses that tie deep into
		// present keys (prefixes, extensions, last-byte perturbations).
		for _, k := range keys {
			probe(k)
			if len(k) > 1 {
				probe(k[:len(k)-1]) // strict prefix — sorts before k
			}
			probe(append(bytes.Clone(k), 0x00)) // extension — sorts after k
			perturbed := bytes.Clone(k)
			perturbed[len(perturbed)-1] ^= 0x01
			probe(perturbed)
		}
		probe([]byte{})
		probe([]byte{0xFF, 0xFF})
	}
}
