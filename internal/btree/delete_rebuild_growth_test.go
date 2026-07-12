package btree

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// A delete keep-set is NOT removal-monotone under a canonical
// re-encode: restart-group re-alignment (and variant migration after a
// RestartGroupTarget config change) can grow the encoding past one
// page even though entries were removed. These fixtures pin the
// native-variant splice fallback that keeps such deletes succeeding.
//
// Adversarial shape: every key shares a 1-byte page prefix (so the
// builder's SharedLen==0 natural-break rescue never fires) and a long
// intra-cluster prefix; clusters hold exactly RestartGroupTarget
// entries, so the canonical build aligns restart groups to clusters.
// Removing the first entry phase-shifts every group boundary: each new
// restart lands mid-cluster and stores a ~100-byte full key where a
// 2-byte delta sufficed.

func growthFixtureKey(c, i int) []byte {
	k := make([]byte, 0, 102)
	k = append(k, 'Z')         // shared page prefix: inter-cluster SharedLen=1
	k = append(k, byte('a'+c)) // cluster discriminator
	for range 97 {
		k = append(k, 'P') // long intra-cluster shared prefix
	}
	k = append(k, byte(i>>8), byte(i))
	return k
}

// buildGrowthFixtureLeaf fills a single compressed leaf to maximum
// canonical fill with whole clusters and installs it as the tree root.
// Returns the root id and the entry list. The keep-set after dropping
// the first entry provably does NOT re-encode into one page — asserted
// here so the fixture can never silently lose its adversarial property
// as the builder evolves.
func buildGrowthFixtureLeaf(t *testing.T, pw *fakeWriter, cfg page.Config) (uint64, []page.LeafEntry) {
	t.Helper()
	val := []byte("VVVVVVVV")
	build := func(clusters int, skipFirst bool) ([]page.LeafEntry, bool) {
		var es []page.LeafEntry
		for c := range clusters {
			for i := range int(cfg.EffectiveRestartGroupTarget()) {
				if skipFirst && c == 0 && i == 0 {
					continue
				}
				es = append(es, page.LeafEntry{Key: growthFixtureKey(c, i), Value: val})
			}
		}
		buf := make([]byte, cfg.PageSize)
		b := page.NewLeafBuilder(buf, cfg)
		for _, e := range es {
			if !b.AddEntry(e) {
				return nil, false
			}
		}
		return es, true
	}
	clusters := 0
	for c := 1; c <= 64; c++ {
		if _, ok := build(c, false); !ok {
			break
		}
		clusters = c
	}
	entries, _ := build(clusters, false)
	if _, refits := build(clusters, true); refits {
		t.Fatal("fixture lost its adversarial property: keep-set re-encodes into one page")
	}
	leaf := makeLeaf(t, cfg, entries)
	root := uint64(1)
	pw.pages[root] = leaf
	pw.nextID = 2
	return root, entries
}

// verifyTreeExact walks the tree and asserts it contains exactly the
// expected entries in order, and that ValidateOrder reports no
// violations.
func verifyTreeExact(t *testing.T, pw *fakeWriter, cfg page.Config, root uint64, want []page.LeafEntry) {
	t.Helper()
	i := 0
	if err := WalkKV(pw, cfg, root, ^uint64(0), func(k, v []byte) error {
		if i >= len(want) {
			return fmt.Errorf("extra entry %q past expected %d", k, len(want))
		}
		if !bytes.Equal(k, want[i].Key) || !bytes.Equal(v, want[i].Value) {
			return fmt.Errorf("entry %d: got %q=%q want %q=%q", i, k, v, want[i].Key, want[i].Value)
		}
		i++
		return nil
	}); err != nil {
		t.Fatalf("WalkKV: %v", err)
	}
	if i != len(want) {
		t.Fatalf("walked %d entries, want %d", i, len(want))
	}
	_, _, err := ValidateOrder(pw, cfg, root, ^uint64(0), 0, func(kind OrderViolationKind, pageID uint64, msg string) bool {
		t.Errorf("order violation %v on page %d: %s", kind, pageID, msg)
		return true
	})
	if err != nil {
		t.Fatalf("ValidateOrder: %v", err)
	}
}

// DeleteRange whose keep-set grows past one canonical page must
// succeed via the native-variant splice fallback (previously returned
// ErrCorrupted), leaving the exact keep-set readable and ordered.
func TestDeleteRangeKeepSetGrowthSpliceFallback(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false}
	pw := newFakeWriter(t, 4096)
	root, entries := buildGrowthFixtureLeaf(t, pw, cfg)

	noopFree := func(pw PageWriter, cfg page.Config, e page.LeafEntry) (uint64, error) { return 1, nil }
	count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold,
		growthFixtureKey(0, 0), growthFixtureKey(0, 1), noopFree)
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	verifyTreeExact(t, pw, cfg, newRoot, entries[1:])

	// The fallback splices the page in its native variant — still
	// compressed, no migration, no split.
	buf, _ := pw.Page(newRoot)
	if typ, _, _, _ := page.ReadHeader(buf); typ != page.TypeLeaf {
		t.Fatalf("root page type = %d, want compressed leaf (%d)", typ, page.TypeLeaf)
	}
}

// A mid-range DeleteRange (several entries, interior of the leaf) must
// take the same fallback and splice out exactly the in-range entries.
func TestDeleteRangeKeepSetGrowthSpliceFallback_MidRange(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false}
	pw := newFakeWriter(t, 4096)
	root, entries := buildGrowthFixtureLeaf(t, pw, cfg)

	// Delete cluster 1's entries [2, 5) — interior, phase-shifting
	// groups 1+ without touching group 0.
	start, end := growthFixtureKey(1, 2), growthFixtureKey(1, 5)
	var want []page.LeafEntry
	for _, e := range entries {
		if !keyInRange(e.Key, start, end) {
			want = append(want, e)
		}
	}
	// Vacuity guard: THIS keep-set must fail the canonical one-page
	// build, or the test stops exercising the splice fallback.
	{
		buf := make([]byte, cfg.PageSize)
		b := page.NewLeafBuilder(buf, cfg)
		fits := true
		for _, e := range want {
			if !b.AddEntry(e) {
				fits = false
				break
			}
		}
		if fits {
			t.Fatal("fixture lost its adversarial property: mid-range keep-set re-encodes into one page")
		}
	}
	noopFree := func(pw PageWriter, cfg page.Config, e page.LeafEntry) (uint64, error) { return 1, nil }
	count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold, start, end, noopFree)
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}
	verifyTreeExact(t, pw, cfg, newRoot, want)
}

// Single-key Delete under a changed RestartGroupTarget config (variant
// mismatch → splice policy-declines → canonical rebuild migrates the
// variant): when the migrated encoding cannot fit one page — a
// delta-heavy compressed page inflates massively as uncompressed — the
// rebuild must fall back to the native-variant splice instead of
// failing, keeping the page compressed (migration is opportunistic,
// never load-bearing).
func TestDeleteVariantMigrationOverflowSpliceFallback(t *testing.T) {
	buildCfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false}
	pw := newFakeWriter(t, 4096)
	root, entries := buildGrowthFixtureLeaf(t, pw, buildCfg)

	// Delete through an RGT=1 (uncompressed) config.
	delCfg := page.Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}
	target := 5 // mid-group delta entry
	newRoot, err := Delete(pw, delCfg, root, DefaultMergeThreshold, entries[target].Key)
	if err != nil {
		t.Fatalf("Delete under changed config: %v", err)
	}
	want := append(append([]page.LeafEntry{}, entries[:target]...), entries[target+1:]...)
	verifyTreeExact(t, pw, delCfg, newRoot, want)

	buf, _ := pw.Page(newRoot)
	if typ, _, _, _ := page.ReadHeader(buf); typ != page.TypeLeaf {
		t.Fatalf("root page type = %d, want compressed leaf (%d) — variant preserved", typ, page.TypeLeaf)
	}
}

// Relocation of a leaf whose canonical re-encode under the current
// config would overflow (same RGT-change shape) must succeed: ref
// rewrites are in-place u64 patches (page.LeafReader.PatchRefs), never
// a re-encode. Previously returned ErrCorrupted and permanently
// blocked incremental compaction of the region.
func TestRelocateLeafVariantMismatchPatchesInPlace(t *testing.T) {
	buildCfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false}
	pw := newFakeWriter(t, 4096)

	// The adversarial leaf again, with one overflow entry appended past
	// the last cluster so the leaf carries a rewritable ref.
	root, entries := buildGrowthFixtureLeaf(t, pw, buildCfg)
	// Drop two tail entries to make room, then re-add one overflow entry.
	trimmed := entries[:len(entries)-2]
	ovKey := append(bytes.Clone(growthFixtureKey(25, 0)), 0xFF)
	ovVal := bytes.Repeat([]byte("O"), 6000)
	leafEntries := trimmed
	buf := make([]byte, buildCfg.PageSize)
	b := page.NewLeafBuilder(buf, buildCfg)
	for _, e := range leafEntries {
		if !b.AddEntry(e) {
			t.Fatalf("trimmed fixture no longer fits")
		}
	}
	b.Finish()
	pw.pages[root] = buf
	nr, err := Put(pw, buildCfg, root, ovKey, ovVal)
	if err != nil {
		t.Fatalf("Put overflow entry: %v", err)
	}
	root = nr

	// Locate the chain and relocate it under the CHANGED config.
	var chainFirst uint64
	if err := WalkLeafEntries(pw, buildCfg, root, ^uint64(0), func(e page.LeafEntry) error {
		if e.IsOverflow() {
			chainFirst = e.OverflowPage
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkLeafEntries: %v", err)
	}
	if chainFirst == 0 {
		t.Fatal("fixture has no overflow chain")
	}
	relCfg := page.Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}
	newRoot, moved, err := RelocatePages(pw, relCfg, root, func(id uint64) bool { return id == chainFirst }, 1<<20)
	if err != nil {
		t.Fatalf("RelocatePages under changed config: %v", err)
	}
	if moved == 0 {
		t.Fatal("nothing relocated")
	}
	got, ok, err := Get(pw, relCfg, newRoot, ovKey)
	if err != nil || !ok || !bytes.Equal(got, ovVal) {
		t.Fatalf("overflow value after relocation: ok=%v err=%v len=%d", ok, err, len(got))
	}
	// Chain moved: the entry's ref must no longer point at chainFirst.
	if err := WalkLeafEntries(pw, relCfg, newRoot, ^uint64(0), func(e page.LeafEntry) error {
		if e.IsOverflow() && e.OverflowPage == chainFirst {
			return fmt.Errorf("overflow ref still points at old chain %d", chainFirst)
		}
		return nil
	}); err != nil {
		t.Fatalf("post-relocation walk: %v", err)
	}
}
