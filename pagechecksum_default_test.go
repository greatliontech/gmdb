package gmdb

import (
	"context"
	"errors"
	"fmt"
	"github.com/thegrumpylion/gmdb/internal/pager"
	"os"
	"testing"
)

// checksums.md §Data Page Checksums: data-page checksums are opt-out,
// ENABLED by default. A zero-value Options (no checksum field set)
// must create a database with the MetaFlagPageChecksum bit set and
// data pages carrying verifying footers; Options.DisablePageChecksum
// opts out. Regression for the default-drift where applyDefaults
// never set the flag, so the effective default was silently OFF.

// TestPageChecksumDefaultOnMetaFlag: a database created with a
// zero-value-checksum Options persists MetaFlagPageChecksum; opting
// out clears it.
func TestPageChecksumDefaultOnMetaFlag(t *testing.T) {
	ctx := context.Background()

	// Default (no checksum field): flag SET.
	dbOn, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open default: %v", err)
	}
	t.Cleanup(func() { dbOn.Close() })
	if !dbOn.Meta().HasFlag(pager.MetaFlagPageChecksum) {
		t.Error("zero-value Options: MetaFlagPageChecksum clear, want set (checksums default ON)")
	}

	// DisablePageChecksum: flag CLEAR.
	dbOff, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256, DisablePageChecksum: true})
	if err != nil {
		t.Fatalf("Open disabled: %v", err)
	}
	t.Cleanup(func() { dbOff.Close() })
	if dbOff.Meta().HasFlag(pager.MetaFlagPageChecksum) {
		t.Error("DisablePageChecksum=true: MetaFlagPageChecksum set, want clear (checksums OFF)")
	}
}

// TestPageChecksumDefaultOnDetectsBitrot: end-to-end companion to the
// meta-flag assertion — with a zero-value-checksum Options, a
// footer bit-flip on a committed data page surfaces as
// ErrBadPageChecksum on the read path, proving the footer is actually
// written and verified (not merely that the flag bit is set).
func TestPageChecksumDefaultOnDetectsBitrot(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 800 { // deep enough that the data-tree root is a branch
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	rtx, _ := db.Begin(ctx)
	rks, _ := rtx.OpenKeyspace("k")
	root := rks.desc.Root
	rtx.Rollback()
	if root == 0 {
		t.Fatal("data-tree root is 0")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Flip a byte in the root page's footer (last 8 bytes).
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	off := int64(root)*4096 + 4096 - 4
	one := make([]byte, 1)
	if _, err := f.ReadAt(one, off); err != nil {
		t.Fatalf("read: %v", err)
	}
	one[0] ^= 0xFF
	if _, err := f.WriteAt(one, off); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	rtx2, _ := db2.Begin(ctx)
	defer rtx2.Rollback()
	ks2, err := rtx2.OpenKeyspace("k")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	if _, err := ks2.Get([]byte("key00000")); !errors.Is(err, ErrBadPageChecksum) {
		t.Fatalf("Get on bitrotted root with default Options = %v, want ErrBadPageChecksum", err)
	}
}
