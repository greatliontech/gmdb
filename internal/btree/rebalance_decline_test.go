package btree

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// The leaf redistribute's decline contract (range-delete.md
// §Invariants, extended to leaf pairs): a redistribute that cannot
// clear the fill floor for both halves, or that has no feasible
// two-page partition at all, DECLINES — the pair stays as-is, the
// sub-MT page is accepted below-floor or threaded upward, and every
// rebalance loop terminates because a decline changes nothing.

// installTwoLeafTree builds root branch [left, sep, right] over two
// canonically-built leaves and returns (rootID, leftEntries,
// rightEntries).
func installTwoLeafTree(t *testing.T, pw *fakeWriter, cfg page.Config, left, right []page.LeafEntry) uint64 {
	t.Helper()
	lbuf := makeLeaf(t, cfg, left)
	rbuf := makeLeaf(t, cfg, right)
	sep := page.ShortestSeparator(left[len(left)-1].Key, right[0].Key)
	pw.pages[1] = lbuf
	pw.pages[2] = rbuf
	pw.pages[3] = makeBranch(t, cfg, 1, []page.BranchCell{{Key: bytes.Clone(sep), Child: 2}})
	pw.nextID = 4
	return 3
}

// Size-skewed siblings: one leaf holds a single near-page inline
// value, the other small entries. Deleting a small entry underflows
// the small leaf; the pair's merge overflows; the entry-granular
// byte-balanced split strands the small half below MergeThreshold —
// the redistribute must DECLINE and the delete must terminate,
// leaving the below-floor page accepted. Pre-decline this looped
// forever (merge → identical skewed split → tried-flags reset → …).
func TestDeleteSizeSkewedSiblingsDeclinesAndTerminates(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false, LeafLayout: page.LeafLayoutInterleaved}
	pw := newFakeWriter(t, 4096)

	huge := []page.LeafEntry{{Key: []byte("a-huge"), Value: bytes.Repeat([]byte("H"), 3500)}}
	var small []page.LeafEntry
	for i := range 10 {
		small = append(small, page.LeafEntry{
			Key:   fmt.Appendf(nil, "b-%03d", i),
			Value: bytes.Repeat([]byte("v"), 100),
		})
	}
	root := installTwoLeafTree(t, pw, cfg, huge, small)

	// Sanity: this fixture must trip the underflow → merge-overflow →
	// skewed-split chain, or the test pins nothing.
	if leafUnderflow(pw.pages[2], cfg, DefaultMergeThreshold) {
		t.Fatal("fixture: small leaf already below floor before the delete")
	}

	newRoot, err := Delete(pw, cfg, root, DefaultMergeThreshold, small[0].Key)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var want []page.LeafEntry
	want = append(want, huge...)
	want = append(want, small[1:]...)
	i := 0
	if err := WalkKV(pw, cfg, newRoot, ^uint64(0), func(k, v []byte) error {
		if !bytes.Equal(k, want[i].Key) || !bytes.Equal(v, want[i].Value) {
			return fmt.Errorf("entry %d: got %q want %q", i, k, want[i].Key)
		}
		i++
		return nil
	}); err != nil {
		t.Fatalf("WalkKV: %v", err)
	}
	if i != len(want) {
		t.Fatalf("walked %d entries, want %d", i, len(want))
	}
	if _, _, err := ValidateOrder(pw, cfg, newRoot, ^uint64(0), 0, func(kind OrderViolationKind, pageID uint64, msg string) bool {
		t.Errorf("order violation %v page %d: %s", kind, pageID, msg)
		return true
	}); err != nil {
		t.Fatalf("ValidateOrder: %v", err)
	}

	// The decline path keeps the pair unmerged: root is still a branch
	// with two leaf children (the small one accepted below-floor).
	buf, _ := pw.Page(newRoot)
	if typ, _, count, _ := page.ReadHeader(buf); typ != page.TypeBranch || count != 1 {
		t.Fatalf("root type=%d cells=%d, want branch with 1 cell (2 children)", typ, count)
	}
}

// No feasible two-page partition: after a RestartGroupTarget change,
// an old-variant delta-heavy leaf's canonical (uncompressed) re-encode
// inflates far past two pages. Underflowing its sibling forces the
// case-C pairing; the merge overflows and findLeafSplitIndex finds no
// boundary — previously ErrCorrupted on a valid delete, now a DECLINE:
// the delete succeeds and the pair stays as-is.
func TestDeleteInfeasibleRedistributeDeclines(t *testing.T) {
	buildCfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false, LeafLayout: page.LeafLayoutInterleaved}
	pw := newFakeWriter(t, 4096)

	// The delta-heavy adversarial leaf from the growth fixtures.
	_, heavy := buildGrowthFixtureLeaf(t, pw, buildCfg)
	var small []page.LeafEntry
	for i := range 10 {
		small = append(small, page.LeafEntry{
			// Sorts after every 'Z'-prefixed heavy key.
			Key:   fmt.Appendf(nil, "z-%03d", i),
			Value: bytes.Repeat([]byte("v"), 95),
		})
	}
	root := installTwoLeafTree(t, pw, buildCfg, heavy, small)

	// Delete under the CHANGED config: the pair's canonical re-encode
	// is uncompressed and cannot land on two pages.
	delCfg := page.Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}

	// Non-vacuity guards: under delCfg the small leaf must sit at or
	// above the floor with 10 entries and fall below it with 9 — the
	// delete itself must be what triggers the case-C pairing — and the
	// heavy leaf's uncompressed re-encode must not fit one page (the
	// merge must overflow into the infeasible-partition path).
	{
		scratch := make([]byte, delCfg.PageSize)
		build := func(es []page.LeafEntry) bool {
			b := page.NewLeafBuilder(scratch, delCfg)
			for _, e := range es {
				if !b.AddEntry(e) {
					return false
				}
			}
			b.Finish()
			return true
		}
		if !build(small) || leafUnderflow(scratch, delCfg, DefaultMergeThreshold) {
			t.Fatal("fixture: 10-entry small leaf must encode at/above the floor under delCfg")
		}
		if !build(small[1:]) || !leafUnderflow(scratch, delCfg, DefaultMergeThreshold) {
			t.Fatal("fixture: 9-entry small leaf must encode below the floor under delCfg")
		}
		if build(heavy) {
			t.Fatal("fixture: heavy leaf re-encodes into one uncompressed page — not adversarial")
		}
	}

	newRoot, err := Delete(pw, delCfg, root, DefaultMergeThreshold, small[0].Key)
	if err != nil {
		t.Fatalf("Delete under changed config: %v", err)
	}
	// The decline keeps the pair unmerged: still a branch with two
	// children, the heavy leaf untouched in its native variant.
	{
		buf, _ := pw.Page(newRoot)
		typ, _, count, _ := page.ReadHeader(buf)
		if typ != page.TypeBranch || count != 1 {
			t.Fatalf("root type=%d cells=%d, want branch with 1 cell (pair kept)", typ, count)
		}
		hbuf, _ := pw.Page(page.BranchLeftmostChild(buf))
		if htyp, _, _, _ := page.ReadHeader(hbuf); htyp != page.TypeLeaf {
			t.Fatalf("heavy child type=%d, want compressed leaf (untouched)", htyp)
		}
	}
	var want []page.LeafEntry
	want = append(want, heavy...)
	want = append(want, small[1:]...)
	i := 0
	if err := WalkKV(pw, delCfg, newRoot, ^uint64(0), func(k, v []byte) error {
		if !bytes.Equal(k, want[i].Key) || !bytes.Equal(v, want[i].Value) {
			return fmt.Errorf("entry %d: got %q want %q", i, k, want[i].Key)
		}
		i++
		return nil
	}); err != nil {
		t.Fatalf("WalkKV: %v", err)
	}
	if i != len(want) {
		t.Fatalf("walked %d entries, want %d", i, len(want))
	}
}

// A healthy redistribute must still fire: two siblings whose merge
// overflows but whose byte-balanced halves both clear the floor
// rebalance normally (guards the declines against over-declining).
func TestDeleteBalancedRedistributeStillFires(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false, LeafLayout: page.LeafLayoutInterleaved}
	pw := newFakeWriter(t, 4096)

	mk := func(prefix string, n, vlen int) []page.LeafEntry {
		var es []page.LeafEntry
		for i := range n {
			es = append(es, page.LeafEntry{
				Key:   fmt.Appendf(nil, "%s-%03d", prefix, i),
				Value: bytes.Repeat([]byte("v"), vlen),
			})
		}
		return es
	}
	// Left ~85% full, right just above floor; deleting from right
	// underflows it; merge overflows; halves balance fine.
	left := mk("a", 30, 100)
	right := mk("b", 10, 100)
	root := installTwoLeafTree(t, pw, cfg, left, right)

	newRoot, err := Delete(pw, cfg, root, DefaultMergeThreshold, right[0].Key)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Redistribute fired: both children at/above the floor.
	buf, _ := pw.Page(newRoot)
	typ, _, count, _ := page.ReadHeader(buf)
	if typ != page.TypeBranch || count != 1 {
		t.Fatalf("root type=%d cells=%d, want branch with 1 cell", typ, count)
	}
	leftmost := page.BranchLeftmostChild(buf)
	rightChild := page.BranchChildAt(buf, cfg, 1)
	for _, id := range []uint64{leftmost, rightChild} {
		cbuf, _ := pw.Page(id)
		if leafUnderflow(cbuf, cfg, DefaultMergeThreshold) {
			t.Fatalf("child %d below floor after redistribute", id)
		}
	}
}
