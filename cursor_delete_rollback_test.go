package gmdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/greatliontech/gmdb/internal/btree"
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

// The SetCursor.Delete analogue: a structural failure while walking to
// the post-delete successor must SURFACE (wrapped, naming the re-seek)
// — never masquerade as end-of-iteration — while the deletion STAYS
// applied (its savepoint is already released; applied-with-error arm
// of transactions.md §Cursor.Delete post-delete state). Pre-fix, a
// checksums-off drain loop silently terminated early with the
// corruption parked in an unreachable inner cursor.
func TestSetCursorDeleteSurfacesReseekCorruption(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, DisablePageChecksum: true, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	sks, err := tx.CreateSetKeyspace("s", nil)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	const n = 60
	for i := range n {
		if _, err := sks.Put(fmt.Appendf(nil, "key%05d", i), make([]byte, 100)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Locate the outer tree's leaves; victim = last key of leaf 1
	// (successor walk enters leaf 2, which we corrupt).
	rtx, _ := db.Begin(ctx)
	rks, err := rtx.OpenSetKeyspace("s")
	if err != nil {
		t.Fatalf("OpenSetKeyspace: %v", err)
	}
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
		t.Fatalf("fixture: %d leaves, want >= 2 (grow n)", len(leaves))
	}
	var lastOfFirstLeaf []byte
	{
		buf, err := rtx.pgr.Page(leaves[0])
		if err != nil {
			t.Fatalf("Page(leaf1): %v", err)
		}
		cnt := int(uint16(buf[2]) | uint16(buf[3])<<8)
		// One value per key, so the (k, v) pair index equals the key
		// index; the last key of leaf 1 is pair cnt-1.
		i := 0
		sc := rks.Cursor()
		for k, _ := sc.First(); k != nil; k, _ = sc.Next() {
			if i == cnt-1 {
				lastOfFirstLeaf = append([]byte(nil), k...)
				break
			}
			i++
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("fixture cursor: %v", err)
		}
	}
	rtx.Rollback()
	if lastOfFirstLeaf == nil {
		t.Fatal("fixture: could not find last key of first leaf")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

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
	wks, err := wtx.OpenSetKeyspace("s")
	if err != nil {
		t.Fatalf("OpenSetKeyspace: %v", err)
	}
	c := wks.Cursor()
	k, v := c.Seek(lastOfFirstLeaf)
	if k == nil {
		t.Fatalf("Seek(%q): %v", lastOfFirstLeaf, c.Err())
	}
	_ = v
	err = c.Delete()
	if err == nil {
		t.Fatal("Delete = nil: re-seek corruption swallowed as end-of-iteration")
	}
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Delete = %v, want ErrCorrupted surfaced from the successor re-seek", err)
	}
	if !strings.Contains(err.Error(), "post-delete successor") {
		t.Fatalf("Delete error %q lacks the re-seek wrap", err)
	}
	// Applied-with-error: the deleted value is GONE (only one value per
	// key in this fixture, so the key vanishes too).
	if has, herr := wks.Has(lastOfFirstLeaf); herr != nil || has {
		t.Fatalf("deletion not applied (has=%v err=%v) — the applied-with-error contract says it stays applied", has, herr)
	}
}
