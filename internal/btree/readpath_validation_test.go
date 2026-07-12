package btree

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// Corrupt pages first-resolved OFF the validated descent path (the
// cousin cascade hands descendants from sibling subtrees; the rescue
// scans grandchildren) must surface as ErrCorrupted through the
// validate-at-first-resolver contract — never as a decode panic. The
// unchecked hot decoders index straight into forged offsets, and page
// checksums may be disabled, so validation is the only guard.

// garbageLeafPage builds a page that passes the type dispatch (leaf
// header byte) but fails structural validation: a forged Count/table
// that the unchecked decoders would index out of range on.
func garbageLeafPage(cfg page.Config) []byte {
	buf := make([]byte, cfg.PageSize)
	// Valid tiny leaf first, then forge the count sky-high.
	b := page.NewLeafBuilder(buf, cfg)
	b.AddInline([]byte("x"), []byte("y"))
	b.Finish()
	buf[2] = 0xFF // Count low byte
	buf[3] = 0x7F // Count high byte
	return buf
}

// cousinRebalanceBranch fill-checks the deep child before pairing —
// a corrupt deep page must yield ErrCorrupted, not a panic.
func TestCousinRebalanceCorruptChildErrsNotPanics(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false}
	pw := newFakeWriter(t, 4096)
	pw.nextID = 1

	l0 := installPage(pw, smallLeaf(t, cfg, 'a', 12, 100))
	bad := installPage(pw, garbageLeafPage(cfg))
	b := installPage(pw, makeBranch(t, cfg, l0, []page.BranchCell{{Key: []byte("b"), Child: bad}}))

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked on corrupt page: %v (want ErrCorrupted)", r)
		}
	}()
	_, _, _, err := cousinRebalanceBranch(pw, cfg, b, bad, DefaultMergeThreshold)
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("err = %v, want ErrCorrupted", err)
	}
}

// pageUnderflow is the shared fill-check for rescue grandchild scans
// and survivor re-checks — same contract.
func TestPageUnderflowCorruptPageErrsNotPanics(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false}
	pw := newFakeWriter(t, 4096)
	pw.nextID = 1
	bad := installPage(pw, garbageLeafPage(cfg))

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked on corrupt page: %v (want ErrCorrupted)", r)
		}
	}()
	_, err := pageUnderflow(pw, cfg, bad, DefaultMergeThreshold)
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("err = %v, want ErrCorrupted", err)
	}
}

// Cursor.Delete must return a structural failure first observed
// during its internal re-position — not park it in Err() for the
// caller's next call (transactions.md §Cursor.Delete post-delete
// state). Fixture: the deleted entry is the last of its leaf; the
// successor lives in a corrupt sibling leaf the delete itself never
// reads, so only the re-position trips over it.
func TestCursorDeleteSurfacesRepositionError(t *testing.T) {
	cfg := page.Config{PageSize: 4096, RestartGroupTarget: 8, PageChecksum: false}
	pw := newFakeWriter(t, 4096)
	pw.nextID = 1

	var left []page.LeafEntry
	for i := range 14 {
		left = append(left, page.LeafEntry{
			Key:   fmt.Appendf(nil, "a-%03d", i),
			Value: bytes.Repeat([]byte("v"), 100),
		})
	}
	l1 := installPage(pw, makeLeaf(t, cfg, left))
	bad := installPage(pw, garbageLeafPage(cfg))
	root := installPage(pw, makeBranch(t, cfg, l1, []page.BranchCell{{Key: []byte("b"), Child: bad}}))

	c := NewCursor(pw, cfg, root, DefaultMergeThreshold)
	k, _ := c.Seek(left[13].Key)
	if k == nil {
		t.Fatalf("Seek(%q) failed: %v", left[13].Key, c.Err())
	}
	err := c.Delete()
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Delete err = %v, want ErrCorrupted surfaced from the re-position", err)
	}
	// The deletion itself was applied despite the surfaced error.
	if ok, gerr := Has(pw, cfg, c.RootID(), left[13].Key); gerr != nil || ok {
		t.Fatalf("deleted key still present (ok=%v err=%v)", ok, gerr)
	}
}
