package btree

import (
	"fmt"
	"strings"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// buildOrderFixture assembles a two-leaf tree by direct page encoding:
// root branch [leftmost=L0, {sep, L1}].
func buildOrderFixture(t *testing.T, pw *fakeWriter, cfg page.Config, l0, l1 [][2]string, sep string) uint64 {
	t.Helper()
	mkLeaf := func(entries [][2]string) uint64 {
		id, _ := pw.AllocPage()
		buf, _ := pw.ZeroPage(id)
		b := page.NewLeafBuilder(buf, cfg)
		for _, e := range entries {
			if !b.AddEntry(page.LeafEntry{Key: []byte(e[0]), Value: []byte(e[1])}) {
				t.Fatalf("fixture entry %q does not fit", e[0])
			}
		}
		b.Finish()
		return id
	}
	l0ID := mkLeaf(l0)
	l1ID := mkLeaf(l1)
	rootID, _ := pw.AllocPage()
	rootBuf, _ := pw.ZeroPage(rootID)
	if err := page.EncodeBranch(rootBuf, cfg, l0ID, []page.BranchCell{{Key: []byte(sep), Child: l1ID}}); err != nil {
		t.Fatalf("EncodeBranch: %v", err)
	}
	return rootID
}

func collectViolations(t *testing.T, pw *fakeWriter, cfg page.Config, root uint64) (msgs []string, entries, values uint64) {
	t.Helper()
	entries, values, err := ValidateOrder(pw, cfg, root, 1<<20, 0, func(_ OrderViolationKind, pageID uint64, msg string) bool {
		msgs = append(msgs, fmt.Sprintf("p%d: %s", pageID, msg))
		return true
	})
	if err != nil {
		t.Fatalf("ValidateOrder: %v", err)
	}
	return msgs, entries, values
}

// TestValidateOrderDetectsEachClass pins the tree-level ordering
// invariants (page-formats.md separator routing; leaf order) class by
// class against hand-built trees.
func TestValidateOrderDetectsEachClass(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 16}

	t.Run("clean", func(t *testing.T) {
		pw := newFakeWriter(t, 4096)
		root := buildOrderFixture(t, pw,
			cfg, [][2]string{{"a", "1"}, {"b", "2"}}, [][2]string{{"m", "3"}, {"n", "4"}}, "m")
		msgs, entries, values := collectViolations(t, pw, cfg, root)
		if len(msgs) != 0 {
			t.Errorf("clean tree reports %v", msgs)
		}
		if entries != 4 || values != 4 {
			t.Errorf("entries=%d values=%d, want 4/4", entries, values)
		}
	})

	t.Run("intra-leaf-disorder", func(t *testing.T) {
		// Encode the leaf out of order by building sorted then
		// swapping equal-length keys' bytes in place.
		pw := newFakeWriter(t, 4096)
		root := buildOrderFixture(t, pw,
			cfg, [][2]string{{"a", "1"}, {"c", "2"}}, [][2]string{{"m", "3"}}, "m")
		// Swap the 'a' and 'c' key bytes inside the first leaf (page 1
		// — buildOrderFixture allocates L0 first), forging intra-leaf
		// disorder the encoder would have refused.
		buf, _ := pw.Page(1)
		for i := range buf {
			if buf[i] == 'a' {
				buf[i] = 'c'
			} else if buf[i] == 'c' {
				buf[i] = 'a'
			}
		}
		msgs, _, _ := collectViolations(t, pw, cfg, root)
		found := false
		for _, m := range msgs {
			if strings.Contains(m, "not strictly greater than predecessor") {
				found = true
			}
		}
		if !found {
			t.Errorf("intra-leaf disorder undetected: %v", msgs)
		}
	})

	t.Run("routing-violation-last-key", func(t *testing.T) {
		// Left leaf's LAST key >= separator: no intra-leaf successor,
		// so only the routing-bounds check can catch it.
		pw := newFakeWriter(t, 4096)
		root := buildOrderFixture(t, pw,
			cfg, [][2]string{{"a", "1"}, {"x", "2"}}, [][2]string{{"m", "3"}, {"n", "4"}}, "m")
		msgs, _, _ := collectViolations(t, pw, cfg, root)
		found := false
		for _, m := range msgs {
			if strings.Contains(m, "outside routing range") {
				found = true
			}
		}
		if !found {
			t.Errorf("routing violation (last key past separator) undetected: %v", msgs)
		}
	})

	t.Run("branch-separator-disorder", func(t *testing.T) {
		pw := newFakeWriter(t, 4096)
		l0ID, _ := pw.AllocPage()
		buf0, _ := pw.ZeroPage(l0ID)
		b0 := page.NewLeafBuilder(buf0, cfg)
		b0.AddEntry(page.LeafEntry{Key: []byte("a"), Value: []byte("1")})
		b0.Finish()
		l1ID, _ := pw.AllocPage()
		buf1, _ := pw.ZeroPage(l1ID)
		b1 := page.NewLeafBuilder(buf1, cfg)
		b1.AddEntry(page.LeafEntry{Key: []byte("m"), Value: []byte("2")})
		b1.Finish()
		l2ID, _ := pw.AllocPage()
		buf2, _ := pw.ZeroPage(l2ID)
		b2 := page.NewLeafBuilder(buf2, cfg)
		b2.AddEntry(page.LeafEntry{Key: []byte("t"), Value: []byte("3")})
		b2.Finish()
		rootID, _ := pw.AllocPage()
		rootBuf, _ := pw.ZeroPage(rootID)
		// Encode ordered ("m","t") — the encoder validates order — then
		// forge disorder by swapping the single-byte suffixes in place
		// (no shared prefix, so the full keys are stored verbatim).
		if err := page.EncodeBranch(rootBuf, cfg, l0ID, []page.BranchCell{
			{Key: []byte("m"), Child: l1ID}, {Key: []byte("t"), Child: l2ID},
		}); err != nil {
			t.Fatalf("EncodeBranch: %v", err)
		}
		swapped := 0
		for i := range rootBuf {
			if rootBuf[i] == 'm' {
				rootBuf[i], swapped = 't', swapped+1
			} else if rootBuf[i] == 't' {
				rootBuf[i], swapped = 'm', swapped+1
			}
		}
		if swapped < 2 {
			t.Fatalf("fixture: separator bytes not found for the swap")
		}
		msgs, _, _ := collectViolations(t, pw, cfg, rootID)
		found := false
		for _, m := range msgs {
			if strings.Contains(m, "not strictly greater than separator") {
				found = true
			}
		}
		if !found {
			t.Errorf("branch separator disorder undetected: %v", msgs)
		}
	})
}
