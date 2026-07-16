package gmdb

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/greatliontech/gmdb/internal/btree"
	"github.com/greatliontech/gmdb/internal/page"
	"github.com/greatliontech/gmdb/internal/verify"
)

// Whole-run overflow digest, end to end (page-formats.md §Overflow
// Page + checksums.md §Overflow-Run Digest): run pages carry no
// per-page footers; the head-resident XXH3-64 digest over the full
// AdditionalPages-determined content range is the run's entire
// integrity cover, verified once per run per transaction; committed
// overflow values are borrowed single mmap slices (api-surface.md
// §Byte Slice Ownership).

// putOverflowValue commits key→value (an overflow-sized value) into
// keyspace "k" of a fresh checksummed db at path and returns the run's
// head page id.
func putOverflowValue(t *testing.T, ctx context.Context, db *DB, key, value []byte) uint64 {
	t.Helper()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put(key, value); err != nil {
		t.Fatalf("Put: %v", err)
	}
	root := ks.desc.Root
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	var head uint64
	if err := btree.WalkLeafEntries(verify.RawPageReader{P: db.pgr}, db.pgr.Config(), root, db.pgr.HighWaterMark(), func(e page.LeafEntry) error {
		if e.IsOverflow() && head == 0 {
			head = e.OverflowPage
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkLeafEntries: %v", err)
	}
	if head == 0 {
		t.Fatal("no overflow run found — value did not promote to overflow")
	}
	return head
}

// corruptFileByte XORs one byte of the database file at off.
func corruptFileByte(t *testing.T, path string, off int64, mask byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	defer f.Close()
	one := make([]byte, 1)
	if _, err := f.ReadAt(one, off); err != nil {
		t.Fatalf("read: %v", err)
	}
	one[0] ^= mask
	if _, err := f.WriteAt(one, off); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestOverflowRunBitrotDetectedOnGet: a bit flipped ANYWHERE in a
// committed run's content range — a follower's extent bytes, or the
// zero slack past the extent length — fails the whole-run digest, and
// Get surfaces ErrBadPageChecksum instead of corrupted bytes. Run
// pages carry no per-page footers, so the digest is the only cover;
// the slack case is what pins "digest over the FULL content range,
// not the extent length".
func TestOverflowRunBitrotDetectedOnGet(t *testing.T) {
	const pageSize = 4096
	value := bytes.Repeat([]byte{0xAB}, 9000) // 3-page run, ~800B slack in last follower
	for _, tc := range []struct {
		name string
		off  int64 // offset within the run image
	}{
		{"follower extent byte", pageSize + 100},
		{"slack past extent", 2*pageSize + 3500}, // extent ends at 16+9000=9016 = 2*4096+824
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := tmpPath(t)
			db, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 1 << 16})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			head := putOverflowValue(t, ctx, db, []byte("big"), value)
			if err := db.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			corruptFileByte(t, path, int64(head)*pageSize+tc.off, 0x01)

			db2, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 1 << 16})
			if err != nil {
				t.Fatalf("re-Open: %v", err)
			}
			defer db2.Close()
			rtx, _ := db2.BeginRead(ctx)
			defer rtx.Rollback()
			ks, err := rtx.OpenKeyspaceReadOnly("k")
			if err != nil {
				t.Fatalf("OpenKeyspace: %v", err)
			}
			if _, err := ks.Get([]byte("big")); !errors.Is(err, ErrBadPageChecksum) {
				t.Fatalf("Get over corrupted run = %v, want ErrBadPageChecksum", err)
			}
		})
	}
}

// TestOverflowRunPagesCarryNoFooters: the commit footer pass exempts
// run pages — a value sized to the run's FULL capacity survives the
// commit with its final bytes intact (a footer stamp would overwrite
// the follower's last 8 bytes), and reads back clean.
func TestOverflowRunPagesCarryNoFooters(t *testing.T) {
	const pageSize = 4096
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 1 << 16})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cfg := db.pgr.Config()
	// Exactly a full 2-page run: every capacity byte is value data,
	// including the follower's last 8 bytes.
	valLen := page.OverflowFirstPageCapacity(cfg) + page.OverflowFollowerCapacity(cfg)
	value := bytes.Repeat([]byte{0xC7}, valLen)
	head := putOverflowValue(t, ctx, db, []byte("full"), value)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	tail := make([]byte, 8)
	if _, err := f.ReadAt(tail, int64(head+1)*pageSize+pageSize-8); err != nil {
		t.Fatalf("read follower tail: %v", err)
	}
	if !bytes.Equal(tail, bytes.Repeat([]byte{0xC7}, 8)) {
		t.Fatalf("follower's last 8 bytes = % x, want value bytes (a footer stamp corrupted the extent)", tail)
	}

	db2, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 1 << 16})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	rtx, _ := db2.BeginRead(ctx)
	defer rtx.Rollback()
	ks, _ := rtx.OpenKeyspaceReadOnly("k")
	got, err := ks.Get([]byte("full"))
	if err != nil || !bytes.Equal(got, value) {
		t.Fatalf("Get full-capacity value: err=%v, match=%v", err, bytes.Equal(got, value))
	}
}

// TestOverflowValueBorrowedSliceAliasesMmap (api-surface.md §Byte
// Slice Ownership): a committed overflow value is returned as a
// single BORROWED slice of the contiguous mmap extent — no copy. The
// test compares slice identity against the raw run image.
func TestOverflowValueBorrowedSliceAliasesMmap(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 1 << 16})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	value := bytes.Repeat([]byte{0x5A}, 9000)
	head := putOverflowValue(t, ctx, db, []byte("big"), value)

	rtx, _ := db.BeginRead(ctx)
	defer rtx.Rollback()
	ks, _ := rtx.OpenKeyspaceReadOnly("k")
	got, err := ks.Get([]byte("big"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Fatal("value mismatch")
	}
	run, err := rtx.pgr.PageRunRaw(head)
	if err != nil {
		t.Fatalf("PageRunRaw: %v", err)
	}
	ext := page.OverflowRunExtent(run, rtx.pgr.Config())
	if &got[0] != &ext[0] {
		t.Errorf("committed overflow value is not a borrowed mmap slice (contract: single borrowed slice, no copy)")
	}
}

// TestOverflowValueOwnTxReadBack: an overflow value written in the
// current write tx lives in per-page slab buffers; reading it back
// pre-commit assembles a correct copy (api-surface.md §Byte Slice
// Ownership).
func TestOverflowValueOwnTxReadBack(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 1 << 16})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	defer tx.Rollback()
	ks, _ := tx.CreateKeyspace("k")
	value := bytes.Repeat([]byte{0x33}, 9000)
	if err := ks.Put([]byte("big"), value); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := ks.Get([]byte("big"))
	if err != nil || !bytes.Equal(got, value) {
		t.Fatalf("own-tx Get: err=%v, match=%v", err, bytes.Equal(got, value))
	}
}

// TestOverflowRunVerifiedOncePerTx (checksums.md §Overflow-Run
// Digest): the whole-run digest is verified on FIRST access per
// transaction, cached keyed by the head id — a later access in the
// same tx skips re-verification (corruption landing mid-tx is not
// re-checked), while a fresh transaction verifies again and fails.
func TestOverflowRunVerifiedOncePerTx(t *testing.T) {
	const pageSize = 4096
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 1 << 16,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	value := bytes.Repeat([]byte{0x11}, 9000)
	head := putOverflowValue(t, ctx, db, []byte("big"), value)

	rtx, _ := db.BeginRead(ctx)
	defer rtx.Rollback()
	ks, _ := rtx.OpenKeyspaceReadOnly("k")
	if _, err := ks.Get([]byte("big")); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	// Corrupt a follower byte while the DB is open — the read tx's
	// mmap observes the external write through the shared page cache.
	corruptFileByte(t, path, int64(head+1)*pageSize+100, 0x01)
	if _, err := ks.Get([]byte("big")); err != nil {
		t.Fatalf("second Get in the same tx = %v, want nil (digest cached on first access)", err)
	}
	rtx2, _ := db.BeginRead(ctx)
	defer rtx2.Rollback()
	ks2, _ := rtx2.OpenKeyspaceReadOnly("k")
	if _, err := ks2.Get([]byte("big")); !errors.Is(err, ErrBadPageChecksum) {
		t.Fatalf("Get in a fresh tx = %v, want ErrBadPageChecksum", err)
	}
}

// TestOverflowForgedAdditionalPagesNoCrash: a head whose
// AdditionalPages is forged huge must surface ErrCorrupted from the
// run bounds gate (the run would overrun the file-resident extent) —
// never a SIGBUS through the MaxSize reservation, and never a
// digest pass over unbacked memory.
func TestOverflowForgedAdditionalPagesNoCrash(t *testing.T) {
	const pageSize = 4096
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 1 << 16})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	head := putOverflowValue(t, ctx, db, []byte("big"), bytes.Repeat([]byte{0x77}, 9000))
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Forge AdditionalPages (u32 at head offset 4) to ~4 billion.
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xFF, 0xFF, 0xFF, 0xFF}, int64(head)*pageSize+4); err != nil {
		t.Fatalf("forge: %v", err)
	}
	f.Close()

	db2, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 1 << 16})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	rtx, _ := db2.BeginRead(ctx)
	defer rtx.Rollback()
	ks, _ := rtx.OpenKeyspaceReadOnly("k")
	_, err = ks.Get([]byte("big"))
	if !errors.Is(err, ErrCorrupted) && !errors.Is(err, ErrBadPageChecksum) {
		t.Fatalf("Get over forged AdditionalPages = %v, want ErrCorrupted/ErrBadPageChecksum", err)
	}
}

// TestCheckDetectsOverflowRunDigestMismatch: Check verifies overflow
// runs standalone by their whole-run digest and reports a mismatch as
// BadPageChecksum against the HEAD page id (checksums.md
// §Overflow-Run Digest).
func TestCheckDetectsOverflowRunDigestMismatch(t *testing.T) {
	const pageSize = 4096
	ctx := context.Background()
	path := tmpPath(t)
	db, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 1 << 16})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	head := putOverflowValue(t, ctx, db, []byte("big"), bytes.Repeat([]byte{0x44}, 9000))
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	corruptFileByte(t, path, int64(head+2)*pageSize+321, 0x01) // last follower's extent

	db2, err := Open(ctx, path, Options{PageSize: pageSize, MinSize: 16, MaxSize: 1 << 16})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer db2.Close()
	issues := collectIssues(db2.Check())
	found := false
	for _, is := range issues {
		if is.Code == verify.CodeBadPageChecksum && is.PageID == head {
			found = true
		}
		if is.Code == verify.CodeBadPageChecksum && is.PageID != head {
			t.Errorf("BadPageChecksum reported against %d, want only the head %d (followers carry no footer to fail)", is.PageID, head)
		}
	}
	if !found {
		t.Fatalf("Check did not report BadPageChecksum on run head %d; issues: %+v", head, issues)
	}
}
