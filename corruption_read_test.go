package gmdb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"testing"
)

// TestGetBitrotReturnsBadPageChecksum (Inv-RV1, end-to-end): a valid
// checksummed page reads cleanly, but a single bit-flip in a data page's
// footer is detected on the read path — Get returns ErrBadPageChecksum
// rather than silently returning corrupted bytes.
func TestGetBitrotReturnsBadPageChecksum(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 800 { // multi-level tree → data-tree root is a branch
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Positive RV1: a valid checksummed page verifies and reads cleanly.
	rtx, _ := db.Begin(ctx)
	rks, _ := rtx.OpenKeyspace("k")
	if _, err := rks.Get([]byte("key00000")); err != nil {
		t.Fatalf("Get on intact DB = %v, want nil", err)
	}
	root := rks.desc.Root
	rtx.Rollback()
	if root == 0 {
		t.Fatal("data-tree root is 0")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Flip a byte in the root page's xxhash64 footer (last 8 bytes): the
	// page structure still validates but the checksum no longer matches.
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
	// A Get that descends through the corrupted root must surface the
	// checksum failure, not return junk.
	_, err = ks2.Get([]byte("key00000"))
	if !errors.Is(err, ErrBadPageChecksum) {
		t.Fatalf("Get on bitrotted root = %v, want ErrBadPageChecksum", err)
	}
}

// TestGetForgedOutOfRangeChildNoCrash (Inv-RV3, the demonstrated-SIGBUS
// regression): with checksums OFF (so the bound, not a footer mismatch,
// is what fires), a branch child pointer forged into the unbacked
// [fileSize, MaxSize) mmap region must surface as ErrCorrupted on the
// read path — never a SIGBUS. Before this guard a Get descending into
// such a child read an unbacked reservation page and crashed the process.
func TestGetForgedOutOfRangeChildNoCrash(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	// Checksums OFF: isolate the file-resident bound from RV1.
	db, err := Open(ctx, path, Options{PageSize: 4096, DisablePageChecksum: true, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 800 { // force a multi-level tree (root is a branch)
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "val%05d", i)); err != nil {
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

	// The file is N pages; ids in [N, MaxSize) are inside the mmap
	// reservation but past EOF (the SIGBUS-prone gap). Forge the root
	// branch's leftmost child (byte offset 8, after the 8-byte header)
	// to such an id.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	backedPages := uint64(fi.Size()) / 4096
	forged := backedPages + 5
	if forged >= 4096 { // must stay within the MaxSize reservation
		t.Fatalf("forged id %d not inside reservation (file too large)", forged)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	var b8 [8]byte
	binary.LittleEndian.PutUint64(b8[:], forged)
	if _, err := f.WriteAt(b8[:], int64(root)*4096+8); err != nil {
		t.Fatalf("forge write: %v", err)
	}
	f.Close()

	db2, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
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
	// Get the smallest key: descent routes through the forged leftmost
	// child. Must return ErrCorrupted (bound), must not SIGBUS.
	_, err = ks2.Get([]byte("key00000"))
	if !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Get into forged out-of-range child = %v, want ErrCorrupted (no crash)", err)
	}
}

// corruptRootChecksumDB builds a checksummed DB with a multi-level
// keyspace "k", flips a byte in the data-tree root page's xxhash64
// footer, and returns the path to the closed, corrupted file.
func corruptRootChecksumDB(t *testing.T, ctx context.Context) string {
	t.Helper()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 800 {
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
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
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
	return path
}

// TestCursorErrReportsBadPageChecksum (Inv-RV1, public cursor surface):
// iterating a bitrotted keyspace via the public Cursor surfaces the
// corruption through Cursor.Err() as the public ErrBadPageChecksum
// sentinel — not a raw pager error that errors.Is can't recognise. This
// pins that Cursor.Err / SetCursor.Err route
// the pager sentinels through mapBtreeErr.
func TestCursorErrReportsBadPageChecksum(t *testing.T) {
	ctx := context.Background()
	path := corruptRootChecksumDB(t, ctx)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, err := tx.OpenKeyspace("k")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	c := ks.Cursor()
	c.First() // descends through the corrupted root, recording c.err
	if err := c.Err(); !errors.Is(err, ErrBadPageChecksum) {
		t.Fatalf("Cursor.Err() over bitrotted keyspace = %v, want ErrBadPageChecksum", err)
	}
}

// forgeBranchDirDB builds a checksums-OFF DB with a multi-level keyspace
// "k", forges the data-tree root branch's first cell-directory entry
// offset to 0xFFFF (past the page's content end), and returns the path
// to the closed, corrupted file. Checksums are off so page.ValidateBranch
// — not a footer mismatch — is what must catch the forged directory.
func forgeBranchDirDB(t *testing.T, ctx context.Context) string {
	t.Helper()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: 4096, DisablePageChecksum: true, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, _ := db.Begin(ctx)
	ks, _ := tx.CreateKeyspace("k")
	for i := range 800 { // multi-level tree → data-tree root is a branch
		if err := ks.Put(fmt.Appendf(nil, "key%05d", i), fmt.Appendf(nil, "val%05d", i)); err != nil {
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
	// Offset 16 = branch cell-directory start (8-byte header + 8-byte
	// leftmost-child pointer); 0xFFFF is far past content end, so
	// BranchSearch/BranchCellAt would read out of the page — ValidateBranch
	// must reject first.
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xFF, 0xFF}, int64(root)*4096+16); err != nil {
		t.Fatalf("forge write: %v", err)
	}
	f.Close()
	return path
}

// TestGetForgedBranchDirectoryNoPanic (G2 / btree-branch-page-validation):
// a forged branch cell-directory on the production Get descent surfaces as
// ErrCorrupted via validateBranchPage — never an out-of-bounds panic from
// BranchSearch/BranchCellAt.
func TestGetForgedBranchDirectoryNoPanic(t *testing.T) {
	ctx := context.Background()
	path := forgeBranchDirDB(t, ctx)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, err := tx.OpenKeyspace("k")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	if _, err := ks.Get([]byte("key00000")); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Get over forged branch directory = %v, want ErrCorrupted (no panic)", err)
	}
}

// TestCursorForgedBranchDirectoryNoPanic (G2): the cursor descent
// (descendLeftmost) hits the same validateBranchPage guard — a forged
// branch directory surfaces through Cursor.Err() as ErrCorrupted, no panic.
func TestCursorForgedBranchDirectoryNoPanic(t *testing.T) {
	ctx := context.Background()
	path := forgeBranchDirDB(t, ctx)
	db, err := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, err := tx.OpenKeyspace("k")
	if err != nil {
		t.Fatalf("OpenKeyspace: %v", err)
	}
	c := ks.Cursor()
	c.First() // descendLeftmost reads the forged root branch
	if err := c.Err(); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Cursor.Err() over forged branch directory = %v, want ErrCorrupted (no panic)", err)
	}
}
