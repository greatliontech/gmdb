package page

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
)

// Layout benchmarks over gmdb's REAL encoders — the corpus methodology
// of the sibling engine's layout spikes (pando, `git log --all --
// spike/leaflayout spike/branchlayout` there) re-run against the
// production LeafBuilder/LeafReader and EncodeBranch/BranchSearch, to
// validate the engine defaults the spec records: segregated leaf,
// segregated branch, restart-group target 6 (page-formats.md §Leaf
// Page variants / restart-group density table; pinned by
// TestEngineLayoutDefaults).
//
// Three deterministic corpora shaped like real consumers:
//   - dns: reversed-label DNS keys — heavy shared prefixes, short keys.
//   - hlc: MVCC document keys with bit-inverted HLC suffixes — long
//     shared stems, high-entropy tails, ~1/6 zero-length values.
//   - rnd: incompressible 24-byte keys — the adversarial floor where
//     prefix compression buys nothing.

// benchSink defeats dead-code elimination.
var benchSink uint64

type benchEntry struct{ key, val []byte }

type benchCorpus struct {
	name    string
	entries []benchEntry
}

func benchRand(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
}

func benchRandBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func benchFinish(name string, es []benchEntry) benchCorpus {
	sort.Slice(es, func(i, j int) bool { return bytes.Compare(es[i].key, es[j].key) < 0 })
	out := es[:0]
	for i, e := range es {
		if i > 0 && bytes.Equal(es[i-1].key, e.key) {
			continue
		}
		out = append(out, e)
	}
	return benchCorpus{name: name, entries: out}
}

func benchDNSCorpus(n int) benchCorpus {
	r := benchRand(1)
	tlds := []string{"com", "net", "org", "io", "dev", "gr"}
	letters := "abcdefghijklmnopqrstuvwxyz"
	slds := make([]string, 48)
	for i := range slds {
		w := make([]byte, 4+r.IntN(8))
		for j := range w {
			w[j] = letters[r.IntN(len(letters))]
		}
		slds[i] = string(w)
	}
	subs := []string{"", "www", "mail", "api", "cdn", "ns1", "ns2", "app", "db",
		"stage", "m", "edge", "vpn", "host1", "host2", "host3"}
	types := []uint16{1, 2, 5, 6, 15, 16, 28, 33}
	es := make([]benchEntry, 0, n)
	for len(es) < n {
		var k []byte
		k = append(k, tlds[r.IntN(len(tlds))]...)
		k = append(k, 0xff)
		k = append(k, slds[r.IntN(len(slds))]...)
		k = append(k, 0xff)
		if s := subs[r.IntN(len(subs))]; s != "" {
			k = append(k, s...)
			k = append(k, 0xff)
		}
		k = append(k, 0x00)
		k = binary.BigEndian.AppendUint16(k, 1)
		k = binary.BigEndian.AppendUint16(k, types[r.IntN(len(types))])
		es = append(es, benchEntry{key: k, val: benchRandBytes(r, 20+r.IntN(60))})
	}
	return benchFinish("dns", es)
}

func benchHLCCorpus(n int) benchCorpus {
	r := benchRand(2)
	const baseWall = uint64(1_700_000_000_000_000_000)
	es := make([]benchEntry, 0, n)
	for len(es) < n {
		doc := benchRandBytes(r, 8)
		vers := 1 + r.IntN(4)
		for v := 0; v < vers && len(es) < n; v++ {
			var k []byte
			k = append(k, 'd', 0x00)
			k = binary.BigEndian.AppendUint32(k, uint32(7+r.IntN(3)))
			k = append(k, doc...)
			k = append(k, 0x00)
			wall := baseWall + uint64(r.IntN(1_000_000))*1000
			k = binary.BigEndian.AppendUint64(k, ^wall)
			k = binary.BigEndian.AppendUint32(k, ^uint32(v))
			k = append(k, 0x0d)
			vlen := 0
			if r.IntN(6) != 0 {
				vlen = 40 + r.IntN(60)
			}
			es = append(es, benchEntry{key: k, val: benchRandBytes(r, vlen)})
		}
	}
	return benchFinish("hlc", es)
}

func benchRandomCorpus(n int) benchCorpus {
	r := benchRand(3)
	es := make([]benchEntry, 0, n)
	for len(es) < n {
		es = append(es, benchEntry{key: benchRandBytes(r, 24), val: benchRandBytes(r, 64)})
	}
	return benchFinish("rnd", es)
}

func benchCorpora() []benchCorpus {
	return []benchCorpus{benchDNSCorpus(3000), benchHLCCorpus(3000), benchRandomCorpus(3000)}
}

// leafCandidate names one (layout, restart-group target) combination:
// both compressed layouts across group targets bracketing the default
// (6): 4, 6, 8, and the retired default 16.
type leafCandidate struct {
	name string
	cfg  Config
}

func leafCandidates() []leafCandidate {
	var out []leafCandidate
	for _, l := range []struct {
		tag    string
		layout LeafLayout
	}{{"ivl", LeafLayoutInterleaved}, {"seg", LeafLayoutSegregated}} {
		for _, rgt := range []uint16{4, 6, 8, 16} {
			out = append(out, leafCandidate{
				name: fmt.Sprintf("%s/rgt=%d", l.tag, rgt),
				cfg:  Config{PageSize: 4096, LeafLayout: l.layout, RestartGroupTarget: rgt},
			})
		}
	}
	return out
}

// leafFixture is one built page plus shuffled probe sets.
type leafFixture struct {
	r    LeafReader
	n    int // resident entries (the page's density)
	keys [][]byte
	miss [][]byte
}

func buildLeafFixture(cfg Config, c benchCorpus) leafFixture {
	page := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(page, cfg)
	n := 0
	for _, e := range c.entries {
		if !b.AddInline(e.key, e.val) {
			break
		}
		n++
	}
	b.Finish()
	f := leafFixture{r: NewLeafReader(page, cfg), n: n}
	resident := c.entries[:n]
	for _, e := range resident {
		f.keys = append(f.keys, e.key)
		m := bytes.Clone(e.key)
		m[len(m)-1] ^= 0xa5
		idx := sort.Search(len(resident), func(i int) bool { return bytes.Compare(resident[i].key, m) >= 0 })
		if idx == len(resident) || !bytes.Equal(resident[idx].key, m) {
			f.miss = append(f.miss, m)
		}
	}
	// Shuffled probe order: sorted round-robin is periodic and
	// predictor-friendly, unlike real lookups.
	r := benchRand(9)
	r.Shuffle(len(f.keys), func(i, j int) { f.keys[i], f.keys[j] = f.keys[j], f.keys[i] })
	r.Shuffle(len(f.miss), func(i, j int) { f.miss[i], f.miss[j] = f.miss[j], f.miss[i] })
	return f
}

func forAllLeaf(b *testing.B, fn func(b *testing.B, f leafFixture)) {
	for _, c := range benchCorpora() {
		for _, lc := range leafCandidates() {
			f := buildLeafFixture(lc.cfg, c)
			b.Run(c.name+"/"+lc.name, func(b *testing.B) {
				fn(b, f)
				b.ReportMetric(float64(f.n), "entries/page")
			})
		}
	}
}

// BenchmarkLeafSearchHit: point lookup of resident keys through the
// real LeafReader, shuffled across the whole page so every restart
// group gets probed; the value's first and last bytes are touched (the
// Get shape — surfaces the segregated layout's extra value-region
// cache line).
func BenchmarkLeafSearchHit(b *testing.B) {
	forAllLeaf(b, func(b *testing.B, f leafFixture) {
		for i := 0; b.Loop(); i++ {
			_, e, found, err := f.r.SearchLeaf(f.keys[i%len(f.keys)], nil)
			if err != nil || !found {
				b.Fatalf("miss on resident key (err=%v)", err)
			}
			if len(e.Value) > 0 {
				benchSink += uint64(e.Value[0]) + uint64(e.Value[len(e.Value)-1])
			}
		}
	})
}

// BenchmarkLeafSearchMiss: point lookup of absent keys sharing a
// page-resident prefix (last byte mutated) — the realistic miss shape.
func BenchmarkLeafSearchMiss(b *testing.B) {
	forAllLeaf(b, func(b *testing.B, f leafFixture) {
		if len(f.miss) == 0 {
			b.Skip("corpus yields no collision-free miss probes")
		}
		for i := 0; b.Loop(); i++ {
			_, _, found, err := f.r.SearchLeaf(f.miss[i%len(f.miss)], nil)
			if err != nil {
				b.Fatal(err)
			}
			if found {
				b.Fatal("hit on absent key")
			}
		}
	})
}

// BenchmarkLeafScan: full-page iteration touching keys and value
// bytes — the cursor range-scan shape.
func BenchmarkLeafScan(b *testing.B) {
	forAllLeaf(b, func(b *testing.B, f leafFixture) {
		var keyBuf []byte
		for b.Loop() {
			it := f.r.IterForReuse(keyBuf, nil, nil)
			for {
				e, ok := it.Next()
				if !ok {
					break
				}
				benchSink += uint64(len(e.Key)) + uint64(e.Key[0])
				if len(e.Value) > 0 {
					benchSink += uint64(e.Value[0]) + uint64(e.Value[len(e.Value)-1])
				}
			}
			keyBuf = it.KeyBuf()
		}
	})
}

// benchMinSeparator returns the shortest key s with left < s <= right —
// what a leaf split pushes up (page-formats.md §Separator Computation),
// so branch fixtures hold realistic short separators.
func benchMinSeparator(left, right []byte) []byte {
	l := 0
	n := min(len(left), len(right))
	for l < n && left[l] == right[l] {
		l++
	}
	return bytes.Clone(right[:min(l+1, len(right))])
}

// branchFixture is one built branch page plus the in-range shuffled
// key population that routes through it.
type branchFixture struct {
	page []byte
	n    int // resident separators (fanout)
	keys [][]byte
}

// buildBranchFixture derives separators from every stride-th adjacent
// key pair of a large corpus (stride approximating entries-per-leaf)
// and packs as many as fit one page via the real encoder. Probes are
// restricted to the page's covered key range: out-of-range keys
// descend a perfectly-predicted spine and would dilute the measurement
// unequally per candidate.
func buildBranchFixture(cfg Config, c benchCorpus) branchFixture {
	const stride = 40
	var cells []BranchCell
	for i := stride; i < len(c.entries); i += stride {
		cells = append(cells, BranchCell{
			Key:   benchMinSeparator(c.entries[i-1].key, c.entries[i].key),
			Child: uint64(0x1000 + i),
		})
	}
	page := make([]byte, cfg.PageSize)
	// Largest prefix of cells that fits: EncodeBranch is all-or-nothing,
	// so binary-search the fit boundary with the encoder itself as the
	// oracle (a formula would need re-deriving per layout).
	fits := func(n int) bool {
		return EncodeBranch(page, cfg, 0x0fff, cells[:n]) == nil
	}
	lo, hi := 1, len(cells)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	n := lo
	if err := EncodeBranch(page, cfg, 0x0fff, cells[:n]); err != nil {
		panic(err)
	}
	f := branchFixture{page: page, n: n}
	var bound []byte
	if n < len(cells) {
		bound = cells[n].Key
	}
	for _, e := range c.entries {
		if bound == nil || bytes.Compare(e.key, bound) < 0 {
			f.keys = append(f.keys, e.key)
		}
	}
	r := benchRand(11)
	r.Shuffle(len(f.keys), func(i, j int) { f.keys[i], f.keys[j] = f.keys[j], f.keys[i] })
	return f
}

// BenchmarkBranchDescend: route a full-key population through one
// branch page via the real BranchSearch — the per-level cost of every
// tree operation — reporting the page's fanout (density).
func BenchmarkBranchDescend(b *testing.B) {
	for _, c := range []benchCorpus{benchDNSCorpus(26000), benchHLCCorpus(26000), benchRandomCorpus(26000)} {
		for _, cand := range []struct {
			name   string
			layout BranchLayout
		}{{"plain", BranchLayoutPlain}, {"seg", BranchLayoutSegregated}} {
			cfg := Config{PageSize: 4096, BranchLayout: cand.layout}
			f := buildBranchFixture(cfg, c)
			b.Run(c.name+"/"+cand.name, func(b *testing.B) {
				for i := 0; b.Loop(); i++ {
					slot, err := BranchSearch(f.page, cfg, f.keys[i%len(f.keys)], nil)
					if err != nil {
						b.Fatal(err)
					}
					benchSink += uint64(slot)
				}
				b.ReportMetric(float64(f.n), "fanout")
			})
		}
	}
}

// TestEngineLayoutDefaults pins the benchmark-validated engine
// defaults (page-formats.md §Leaf Page variants / restart-group
// density table): a zero-value declaration resolves to the segregated
// leaf, the segregated branch, and restart-group target 6. A default
// flip silently changes every NEW keyspace's persisted layout — it
// must arrive with fresh benchmark evidence, not ride an unrelated
// change.
func TestEngineLayoutDefaults(t *testing.T) {
	if DefaultRestartGroupTarget != 6 {
		t.Errorf("DefaultRestartGroupTarget = %d, want 6", DefaultRestartGroupTarget)
	}
	cfg := Config{PageSize: 4096}
	if got := cfg.EffectiveLeafType(); got != TypeLeafSegregated {
		t.Errorf("default leaf type = %d, want %d (TypeLeafSegregated)", got, TypeLeafSegregated)
	}
	if got := cfg.EffectiveBranchType(); got != TypeBranchSegregated {
		t.Errorf("default branch type = %d, want %d (TypeBranchSegregated)", got, TypeBranchSegregated)
	}
	if got := cfg.EffectiveRestartGroupTarget(); got != 6 {
		t.Errorf("effective restart-group target = %d, want 6", got)
	}
}
