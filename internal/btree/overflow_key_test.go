package btree

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// ovkKey builds an over-threshold key: a cluster-shared prefix longer
// than T (so adjacent keys tie through their resident bytes and
// separators computed between them exceed T), followed by a distinct
// tail.
func ovkKey(cfg page.Config, cluster byte, i int) []byte {
	t := cfg.InlineThreshold()
	k := bytes.Repeat([]byte{cluster}, t+64)
	return append(k, fmt.Sprintf("-%06d", i)...)
}

// TestOverflowKeyLifecycleThroughSplitsAndDeletes drives the full
// overflow-key lifecycle (page-formats.md §Overflow-Key Cells) through
// the real tree machinery: enough resident-tied over-threshold keys to
// force leaf splits with over-threshold separators (overflow branch
// cells, extents written once), replaces (old key extents freed), and
// a full tear-down (merges, separator removal, extent retirement) —
// with the slab-partition leak check after every phase: every
// allocated page is either reachable (including key extents on both
// leaf entries and branch cells) or freed.
func TestOverflowKeyLifecycleThroughSplitsAndDeletes(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	const n = 24

	// Insert: resident-tied cluster keys interleaved with short keys.
	for i := range n {
		var err error
		root, err = Put(pw, cfg, root, ovkKey(cfg, 'C', i), fmt.Appendf(nil, "v%d", i))
		if err != nil {
			t.Fatalf("Put ovk %d: %v", i, err)
		}
		root, err = Put(pw, cfg, root, fmt.Appendf(nil, "short-%06d", i), []byte("s"))
		if err != nil {
			t.Fatalf("Put short %d: %v", i, err)
		}
	}
	checkSlabPartition(t, pw, cfg, root)

	// Every key retrievable; tail-divergent probes miss.
	for i := range n {
		k := ovkKey(cfg, 'C', i)
		v, found, err := Get(pw, cfg, root, k)
		if err != nil || !found || !bytes.Equal(v, fmt.Appendf(nil, "v%d", i)) {
			t.Fatalf("Get ovk %d: %q found=%v err=%v", i, v, found, err)
		}
		miss := bytes.Clone(k)
		miss[len(miss)-1] ^= 0x7F
		if _, found, err := Get(pw, cfg, root, miss); err != nil || found {
			t.Fatalf("tail-divergent probe %d: found=%v err=%v", i, found, err)
		}
	}

	// Cursor order: the ovk cluster iterates in tail order, full keys
	// materialized.
	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)
	k, _ := c.Seek(ovkKey(cfg, 'C', 0))
	for i := 0; i < n; i++ {
		want := ovkKey(cfg, 'C', i)
		if !bytes.Equal(k, want) {
			t.Fatalf("cursor at %d: key len %d, want len %d (tail %q vs %q)", i, len(k), len(want), k[len(k)-8:], want[len(want)-8:])
		}
		k, _ = c.Next()
	}
	if err := c.Err(); err != nil {
		t.Fatalf("cursor: %v", err)
	}

	// Replace: same keys, new values — old value state replaced, key
	// extents rewritten; partition must stay clean.
	for i := 0; i < n; i += 3 {
		var err error
		root, err = Put(pw, cfg, root, ovkKey(cfg, 'C', i), fmt.Appendf(nil, "V%d", i))
		if err != nil {
			t.Fatalf("re-Put ovk %d: %v", i, err)
		}
	}
	checkSlabPartition(t, pw, cfg, root)

	// Delete everything, one at a time, alternating ends to exercise
	// merges and redistributes over overflow separators.
	for i := range n {
		j := i / 2
		if i%2 == 1 {
			j = n - 1 - i/2
		}
		var err error
		root, err = Delete(pw, cfg, root, DefaultMergeThreshold, ovkKey(cfg, 'C', j))
		if err != nil {
			t.Fatalf("Delete ovk %d: %v", j, err)
		}
		if _, found, _ := Get(pw, cfg, root, ovkKey(cfg, 'C', j)); found {
			t.Fatalf("ovk %d still present after Delete", j)
		}
		root, err = Delete(pw, cfg, root, DefaultMergeThreshold, fmt.Appendf(nil, "short-%06d", j))
		if err != nil {
			t.Fatalf("Delete short %d: %v", j, err)
		}
		checkSlabPartition(t, pw, cfg, root)
	}
	if root != 0 {
		// Every page must be freed on an emptied tree.
		checkSlabPartition(t, pw, cfg, root)
	}
}

// TestOverflowKeyDeleteRangeClassification pins the DeleteRange
// boundary classification over resident-tied keys: bounds that diverge
// from stored keys only past the inline threshold must select exactly
// the in-range keys, and the retired entries' key extents are freed
// (slab partition stays clean).
func TestOverflowKeyDeleteRangeClassification(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	const n = 12
	for i := range n {
		var err error
		root, err = Put(pw, cfg, root, ovkKey(cfg, 'D', i), []byte("v"))
		if err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	// Delete [key(3), key(9)): bounds tie through the resident bytes.
	perCell := func(pw PageWriter, cfg page.Config, e page.LeafEntry) (uint64, error) {
		if err := freeKeyExtentIfPresent(pw, cfg, e); err != nil {
			return 0, err
		}
		if err := freeOverflowChainIfPresent(pw, cfg, e); err != nil {
			return 0, err
		}
		return 1, nil
	}
	count, newRoot, err := DeleteRange(pw, cfg, root, DefaultMergeThreshold,
		ovkKey(cfg, 'D', 3), ovkKey(cfg, 'D', 9), perCell)
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if count != 6 {
		t.Fatalf("DeleteRange count = %d, want 6", count)
	}
	root = newRoot
	checkSlabPartition(t, pw, cfg, root)
	for i := range n {
		_, found, err := Get(pw, cfg, root, ovkKey(cfg, 'D', i))
		if err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
		wantFound := i < 3 || i >= 9
		if found != wantFound {
			t.Errorf("key %d: found=%v, want %v", i, found, wantFound)
		}
	}
}

// TestOverflowKeyValidateOrderClean pins the extent-aware ValidateOrder
// pass: a tree full of resident-tied overflow keys (and their
// over-threshold branch separators) reports no ordering violations.
func TestOverflowKeyValidateOrderClean(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	for i := range 16 {
		var err error
		root, err = Put(pw, cfg, root, ovkKey(cfg, 'E', i), []byte("v"))
		if err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	entries, _, err := ValidateOrder(pw, cfg, root, pw.nextID, 0,
		func(kind OrderViolationKind, pageID uint64, msg string) bool {
			t.Errorf("order violation on page %d: %s", pageID, msg)
			return true
		})
	if err != nil {
		t.Fatalf("ValidateOrder: %v", err)
	}
	if entries != 16 {
		t.Errorf("ValidateOrder entries = %d, want 16", entries)
	}
}

// TestOverflowKeyWalkVisitsExtents pins the reachability walk: Walk
// visits every key-extent page (leaf and branch separators) so Check's
// accounting and CopyTo's verbatim enumeration cover them.
func TestOverflowKeyWalkVisitsExtents(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	for i := range 16 {
		var err error
		root, err = Put(pw, cfg, root, ovkKey(cfg, 'F', i), []byte("v"))
		if err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	visited := make(map[uint64]struct{})
	if err := Walk(pw, cfg, root, pw.nextID, func(id uint64, kind PageKind, depth int) error {
		visited[id] = struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	reachable := make(map[uint64]struct{})
	collectReachable(t, pw, cfg, root, reachable)
	for id := range reachable {
		if _, ok := visited[id]; !ok {
			t.Errorf("reachable page %d not visited by Walk", id)
		}
	}
	// WalkKV materializes full keys.
	i := 0
	if err := WalkKV(pw, cfg, root, pw.nextID, func(key, value []byte) error {
		want := ovkKey(cfg, 'F', i)
		if !bytes.Equal(key, want) {
			return fmt.Errorf("WalkKV key %d: len %d, want len %d", i, len(key), len(want))
		}
		i++
		return nil
	}); err != nil {
		t.Fatalf("WalkKV: %v", err)
	}
	if i != 16 {
		t.Errorf("WalkKV yielded %d keys, want 16", i)
	}
}

// TestOverflowKeyRelocation pins the relocation path: relocating every
// page of an overflow-key tree (leaf cells, branch cells, key extents)
// preserves content and the slab partition.
func TestOverflowKeyRelocation(t *testing.T) {
	cfg := page.Config{PageSize: 4096}
	pw := newFakeWriter(t, 4096)
	root := uint64(0)
	for i := range 16 {
		var err error
		root, err = Put(pw, cfg, root, ovkKey(cfg, 'G', i), fmt.Appendf(nil, "v%d", i))
		if err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	newRoot, _, err := RelocatePages(pw, cfg, root, func(uint64) bool { return true }, 1<<20)
	if err != nil {
		t.Fatalf("RelocatePages: %v", err)
	}
	root = newRoot
	checkSlabPartition(t, pw, cfg, root)
	for i := range 16 {
		v, found, err := Get(pw, cfg, root, ovkKey(cfg, 'G', i))
		if err != nil || !found || !bytes.Equal(v, fmt.Appendf(nil, "v%d", i)) {
			t.Fatalf("post-relocation Get %d: %q found=%v err=%v", i, v, found, err)
		}
	}
}
