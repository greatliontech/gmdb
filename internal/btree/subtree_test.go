package btree

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// Tests for the bulk-subtree retire path. Promote the
// invariant Inv-B (every page reachable from rootID enters
// retiredPages or loosePages) at the btree-layer interface — the
// higher-level gmdb.TestDeleteKeyspaceBulkFreesDataSubtree promotes
// the same invariant against the public DeleteKeyspace surface.

// TestFreeSubtreeEmpty asserts a rootID==0 input is a no-op (no
// FreePage call, no FreeRun call). The DeleteKeyspace plumbing relies on
// this for the "Created-this-tx Keyspace with desc.Root==0 then
// DeleteKeyspace" path.
func TestFreeSubtreeEmpty(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 16)
	defer pw.Close()
	defer f.Close()
	if _, err := FreeSubtree(pw, pw.Config(), 0); err != nil {
		t.Errorf("FreeSubtree(0): %v", err)
	}
	if len(pw.RetiredPages()) != 0 || len(pw.LoosePages()) != 0 {
		t.Errorf("FreeSubtree(0) populated retire/loose sets")
	}
}

// TestFreeSubtreeSingleLeaf builds a one-leaf tree, runs FreeSubtree,
// and asserts the leaf landed in loose (same-tx allocation) per
// FreePage semantics.
func TestFreeSubtreeSingleLeaf(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 16)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()
	root, err := Put(pw, cfg, 0, []byte("k"), []byte("v"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	reachable := collectSubtreePages(t, pw, cfg, root)
	if len(reachable) != 1 {
		t.Fatalf("expected 1 reachable page, got %d", len(reachable))
	}
	if _, err := FreeSubtree(pw, cfg, root); err != nil {
		t.Fatalf("FreeSubtree: %v", err)
	}
	for id := range reachable {
		if _, ok := pw.LoosePages()[id]; !ok {
			t.Errorf("page %d not in loosePages after FreeSubtree", id)
		}
	}
}

// TestFreeSubtreeMultiLeafBranch builds a tree large enough to force at
// least one branch level, then asserts FreeSubtree retires every
// reachable page (branches + leaves).
func TestFreeSubtreeMultiLeafBranch(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 256)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()
	val := bytes.Repeat([]byte{0x42}, 200)
	root := uint64(0)
	var err error
	for i := range 600 {
		key := []byte(fmt.Sprintf("key-%06d", i))
		root, err = Put(pw, cfg, root, key, val)
		if err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
	}
	reachable := collectSubtreePages(t, pw, cfg, root)
	// Quick structural check: at least one branch page exists.
	branchSeen := false
	for id := range reachable {
		buf, _ := pw.Page(id)
		typ, _, _, _ := page.ReadHeader(buf)
		if page.IsBranchType(typ) {
			branchSeen = true
			break
		}
	}
	if !branchSeen {
		t.Fatal("test setup too small — no branch level produced")
	}

	if _, err := FreeSubtree(pw, cfg, root); err != nil {
		t.Fatalf("FreeSubtree: %v", err)
	}
	loose := pw.LoosePages()
	retired := pw.RetiredPages()
	retiredSet := make(map[uint64]struct{}, len(retired))
	for _, id := range retired {
		retiredSet[id] = struct{}{}
	}
	for id := range reachable {
		_, inRetired := retiredSet[id]
		_, inLoose := loose[id]
		if !inRetired && !inLoose {
			t.Errorf("page %d not retired/loose post-FreeSubtree", id)
		}
	}
}

// TestFreeSubtreeWithOverflow builds a tree containing an entry with
// an overflow chain, then asserts FreeSubtree retires every overflow
// page along with the leaf and any branches.
func TestFreeSubtreeWithOverflow(t *testing.T) {
	pw, _, f := setupPagerWriter(t, 32)
	defer pw.Close()
	defer f.Close()
	cfg := pw.Config()

	// 16 KB value forces an overflow chain at 4 KB page size.
	bigVal := bytes.Repeat([]byte{0xAB}, 16384)
	root, err := Put(pw, cfg, 0, []byte("ovf"), bigVal)
	if err != nil {
		t.Fatalf("Put overflow: %v", err)
	}
	// Add a few inline entries to keep the leaf alive with both
	// inline and overflow entries.
	for i := range 5 {
		key := []byte(fmt.Sprintf("k%d", i))
		root, err = Put(pw, cfg, root, key, []byte("v"))
		if err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
	}
	reachable := collectSubtreePages(t, pw, cfg, root)
	// At least 5 pages: 1 leaf + 4 overflow pages (1 header + 3
	// followers for a 16 KB value at 4 KB pages, header includes
	// metadata so 4-5 pages total expected).
	if len(reachable) < 5 {
		t.Fatalf("expected ≥5 reachable pages (leaf + overflow chain), got %d", len(reachable))
	}

	if _, err := FreeSubtree(pw, cfg, root); err != nil {
		t.Fatalf("FreeSubtree: %v", err)
	}
	loose := pw.LoosePages()
	retired := pw.RetiredPages()
	retiredSet := make(map[uint64]struct{}, len(retired))
	for _, id := range retired {
		retiredSet[id] = struct{}{}
	}
	for id := range reachable {
		_, inRetired := retiredSet[id]
		_, inLoose := loose[id]
		// Direct-written same-tx overflow-run pages free straight back
		// to the bitmap (never slab-resident, so FreePage's
		// allocated-but-never-written branch applies).
		bitmapFree := pw.Bitmap().IsSet(id)
		if !inRetired && !inLoose && !bitmapFree {
			t.Errorf("page %d not retired/loose/bitmap-free post-FreeSubtree (overflow chain leaked?)", id)
		}
	}
}

// collectSubtreePages walks the subtree rooted at rootID and returns
// the set of every reachable page ID. Mirrors FreeSubtree's traversal
// so the test's reachability set matches what FreeSubtree should
// retire. Used by the invariant Inv-B tests in this file.
func collectSubtreePages(t *testing.T, pr PageReader, cfg page.Config, rootID uint64) map[uint64]struct{} {
	t.Helper()
	out := make(map[uint64]struct{})
	if rootID == 0 {
		return out
	}
	var walk func(id uint64, depth int)
	walk = func(id uint64, depth int) {
		if depth > MaxTreeDepth {
			t.Fatalf("collectSubtreePages depth exceeded MaxTreeDepth at %d", id)
		}
		if _, dup := out[id]; dup {
			t.Fatalf("collectSubtreePages: page %d visited twice (cycle?)", id)
		}
		out[id] = struct{}{}
		buf, _ := pr.Page(id)
		typ, _, count, _ := page.ReadHeader(buf)
		switch {
		case page.IsBranchType(typ):
			for i := uint16(0); i <= count; i++ {
				walk(page.BranchChildAt(buf, cfg, i), depth+1)
			}
		case page.IsLeafType(typ):
			r := page.NewLeafReader(buf, cfg)
			if err := r.Validate(); err != nil {
				t.Fatalf("leaf %d: %v", id, err)
			}
			it := r.IterForReuse(nil, nil, nil)
			for {
				e, ok := it.Next()
				if !ok {
					break
				}
				if e.IsOverflow() {
					runLen := page.OverflowRunLength(cfg, e.TotalLen)
					for k := range runLen {
						pid := e.OverflowPage + uint64(k)
						if _, dup := out[pid]; dup {
							t.Fatalf("overflow page %d visited twice", pid)
						}
						out[pid] = struct{}{}
					}
				}
			}
		default:
			t.Fatalf("page %d unexpected type %d", id, typ)
		}
	}
	walk(rootID, 0)
	return out
}
