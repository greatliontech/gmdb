package pager

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greatliontech/gmdb/internal/bitmap"
	"github.com/greatliontech/gmdb/internal/page"
)

// A crash-recovered image can carry FREE bitmap bits above the
// truncated EOF (lazy tail-refund bit-clears unflushed while the
// ftruncate metadata journaled). A bitmap-path allocation of such a
// page must EXTEND the file first — otherwise the verifying Page
// accessor rejects the page's own committed data as ErrCorrupted
// until reopen (checksums.md §Structural and Allocation Bounds: the
// bound must track reality, not lag it).
func TestBitmapAllocAboveEOFExtendsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.gmdb")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	const filePages, maxPages = 8, 64
	if err := f.Truncate(int64(filePages) * int64(testPageSize)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	pool := NewBufPool(testPageSize)
	cfg := page.Config{PageSize: testPageSize}
	p, err := NewWriter(f, cfg, int64(maxPages)*int64(testPageSize), pool, 16<<20)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer p.Close()
	bm := bitmap.New(make([]byte, testPageSize), testPageSize, 1, maxPages)
	p.AttachBitmap(bm)
	// The legitimate crash-recovered shape: the adopted meta's
	// HighWaterMark covers the page, but the file was truncated below
	// it (lazy tail-refund bit-clears unflushed while the ftruncate
	// metadata journaled). HWM = 16 > the free bit > fileSize = 8.
	p.SetCommitState(16, maxPages, 0)
	aboveEOF := uint64(filePages) + 3
	bm.Set(aboveEOF)

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if id != aboveEOF {
		t.Fatalf("AllocPage = %d, want the free-above-EOF page %d", id, aboveEOF)
	}
	// The verifying accessor must accept the page NOW (the file was
	// extended), not only after reopen.
	if _, err := p.Page(id); err != nil {
		t.Fatalf("Page(%d) after alloc: %v", id, err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() < int64(aboveEOF+1)*int64(testPageSize) {
		t.Fatalf("file size %d does not cover allocated page %d", st.Size(), aboveEOF)
	}
}

// The verifying Page accessor's file-resident bound must clamp to
// MaxSize: an externally-grown file exceeds the mmap reservation, and
// an id in that gap would slice past the mapping (a runtime panic)
// instead of returning ErrCorrupted.
func TestPageBoundClampedToMaxSize(t *testing.T) {
	p, _, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()

	// Simulate an externally-grown file: the on-disk size now exceeds
	// the MaxSize-page mmap reservation.
	if err := f.Truncate(int64(64) * int64(testPageSize)); err != nil {
		t.Fatalf("grow: %v", err)
	}
	p.fileSize = int64(64) * int64(testPageSize)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Page past the reservation panicked: %v (want ErrCorrupted)", r)
		}
	}()
	if _, err := p.Page(40); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("Page(40) err = %v, want ErrCorrupted (id beyond MaxSize=16 reservation)", err)
	}
}

// A claim at/above HighWaterMark must RAISE the HWM (exactly like the
// file-extension tier): otherwise a forged free bit above HWM lets
// user data commit into the page and the same commit's shrink —
// target derived from HWM — truncates the just-committed data away.
func TestBitmapAllocAboveHWMRaisesIt(t *testing.T) {
	p, bm, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()

	// setupWriter leaves HWM = FirstDataPage. Free bit above it.
	above := bm.FirstDataPage() + 5
	bm.Set(above)
	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if id != above {
		t.Fatalf("AllocPage = %d, want %d", id, above)
	}
	if hwm := p.HighWaterMark(); hwm < above+1 {
		t.Fatalf("HighWaterMark = %d after claiming %d — a shrink would truncate the page away", hwm, above)
	}

	// End-to-end: fire an actual shrink and require the claimed page to
	// survive the truncate, pinning the failure mode itself rather than
	// only the HWM value the target is currently derived from.
	const grownPages = 16
	if err := f.Truncate(int64(grownPages) * int64(testPageSize)); err != nil {
		t.Fatalf("grow: %v", err)
	}
	p.fileSize = int64(grownPages) * int64(testPageSize)
	if err := p.maybeShrink(1); err != nil {
		t.Fatalf("maybeShrink: %v", err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() < int64(above+1)*int64(testPageSize) {
		t.Fatalf("shrink truncated to %d bytes, cutting away claimed page %d", st.Size(), above)
	}
}
