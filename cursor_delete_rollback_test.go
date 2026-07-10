package gmdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/btree"
)

// A Cursor.Delete that fails while walking to the post-delete
// successor (a corrupt sibling leaf the delete itself never touched)
// must (a) return the error from Delete, (b) roll the deletion back
// (transactions.md §Cursor.Delete post-delete state), and (c) leave
// the cursor re-pointed at the still-live pre-delete tree so the
// spec'd recovery — re-positioning — resumes safely. Previously the
// inner cursor kept the rolled-back tree's root: re-positioning
// descended a deallocated id into a phantom tree where the deleted
// key was silently missing.
func TestCursorDeleteRepositionErrorRollsBackAndRecovers(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, DisablePageChecksum: true, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	const n = 60 // enough 100-byte rows to split into ≥2 leaves
	for i := range n {
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), make([]byte, 100)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Locate the leaves in key order; the victim key is the last entry
	// of leaf 1, its successor the first entry of leaf 2 (the page we
	// corrupt).
	rtx, _ := db.Begin(ctx)
	rks, _ := rtx.OpenKeyspace("k")
	root := rks.desc.Root
	cfg := rtx.pgr.Config()
	var leaves []uint64
	if err := btree.Walk(rtx.pgr, cfg, root, ^uint64(0), func(id uint64, kind btree.PageKind, depth int) error {
		if kind == btree.PageKindLeaf {
			leaves = append(leaves, id)
		}
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(leaves) < 2 {
		t.Fatalf("fixture: %d leaves, want >= 2", len(leaves))
	}
	// The victim is the LAST key of leaf 1 (its post-delete successor
	// is leaf 2's first entry): entry index Count(leaf1)-1 in the
	// key-ordered walk.
	var lastOfFirstLeaf []byte
	{
		buf, err := rtx.pgr.Page(leaves[0])
		if err != nil {
			t.Fatalf("Page(leaf1): %v", err)
		}
		cnt := int(uint16(buf[2]) | uint16(buf[3])<<8)
		i := 0
		if err := btree.WalkKV(rtx.pgr, cfg, root, ^uint64(0), func(k, v []byte) error {
			if i == cnt-1 {
				lastOfFirstLeaf = append([]byte(nil), k...)
			}
			i++
			return nil
		}); err != nil {
			t.Fatalf("WalkKV: %v", err)
		}
	}
	rtx.Rollback()
	if lastOfFirstLeaf == nil {
		t.Fatal("fixture: could not find last key of first leaf")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Corrupt leaf 2: forge its Count sky-high (checksums off, so only
	// structural validation can catch it).
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xFF, 0x7F}, int64(leaves[1])*4096+2); err != nil {
		t.Fatalf("forge write: %v", err)
	}
	f.Close()

	db, err = Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db.Close()
	wtx, _ := db.Begin(ctx)
	defer wtx.Rollback()
	wks, err := wtx.OpenKeyspace("k")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	c := wks.Cursor()
	if k, _ := c.Seek(lastOfFirstLeaf); k == nil {
		t.Fatalf("Seek(%q): %v", lastOfFirstLeaf, c.Err())
	}
	err = c.Delete()
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Delete = %v, want ErrCorrupted surfaced from the successor walk", err)
	}
	if !strings.Contains(err.Error(), "walking to the post-delete successor") {
		t.Fatalf("Delete error %q lacks the successor-walk wrap — the corruption fired during the delete itself, not the re-position; fixture drifted", err)
	}

	// (b) The deletion rolled back: the key is still present.
	if v, gerr := wks.Get(lastOfFirstLeaf); gerr != nil || v == nil {
		t.Fatalf("deleted key rolled back? Get = (%v, %v), want present", v, gerr)
	}
	// (c) The spec'd recovery works and sees the pre-delete tree: a
	// re-position + scan up to the victim key must include it (the
	// pre-fix phantom tree silently lacked it).
	found := false
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		if string(k) == string(lastOfFirstLeaf) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("re-positioned cursor lost the rolled-back key %q (phantom tree?) — cursor err: %v", lastOfFirstLeaf, c.Err())
	}
}
