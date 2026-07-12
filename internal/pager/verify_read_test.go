package pager

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/greatliontech/gmdb/internal/page"
)

// readerOverFile truncates a fresh file to filePages and returns a
// read-only pager whose mmap reservation spans reservationPages —
// modelling the production layout where the mmap covers the whole
// MaxSize reservation but only the first fileSize bytes are file-backed.
// All pages start zero-filled.
func readerOverFile(t *testing.T, filePages, reservationPages int, checksums bool) (*Pager, *os.File) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db.gmdb")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := f.Truncate(int64(filePages) * int64(testPageSize)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	cfg := page.Config{PageSize: testPageSize, PageChecksum: checksums}
	p, err := NewReader(f, cfg, int64(reservationPages)*int64(testPageSize))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return p, f
}

// TestPageBoundRejectsOutOfFileExtent (Inv-RV3): Page bounds id against
// the file-resident extent, returning ErrCorrupted — never a SIGBUS on
// the unbacked [fileSize, reservation) region — and never an
// id*PageSize overflow panic for a forged-huge id. The raw mmap
// reservation here spans 64 pages while only 16 are file-backed, so the
// gap [16, 64) is exactly the SIGBUS-prone region a forged child id
// could land in.
func TestPageBoundRejectsOutOfFileExtent(t *testing.T) {
	p, f := readerOverFile(t, 16, 64, false)
	defer p.Close()
	defer f.Close()

	// In-bounds reads succeed (checksums off, so no verification).
	if _, err := p.Page(15); err != nil {
		t.Fatalf("Page(15) in-bounds = %v, want nil", err)
	}
	// The file-resident extent is 16 pages; ids 16..63 are inside the
	// mmap reservation but past EOF — a raw read SIGBUSes, Page must not.
	for _, id := range []uint64{16, 17, 63, 1 << 20, 1 << 40, 1<<64 - 1} {
		_, err := p.Page(id)
		if !errors.Is(err, ErrCorrupted) {
			t.Errorf("Page(%d) = %v, want ErrCorrupted (bound), no panic", id, err)
		}
	}
}

// TestPageVerifiesFooter (Inv-RV1): when checksums are enabled, Page
// verifies the xxhash64 footer on read and returns ErrBadPageChecksum on
// a mismatch. A zero-filled page's footer (all zeroes) cannot match the
// hash of its zero content, so it is the simplest corrupt page.
func TestPageVerifiesFooter(t *testing.T) {
	p, f := readerOverFile(t, 16, 16, true)
	defer p.Close()
	defer f.Close()

	_, err := p.Page(3)
	if !errors.Is(err, ErrBadPageChecksum) {
		t.Fatalf("Page(3) on zero page = %v, want ErrBadPageChecksum", err)
	}
}

// TestPageVerificationCachedAndReset (Inv-RV2): a page verified once in
// a transaction is recorded and not re-verified on subsequent accesses;
// resetVerified (called at each write-tx boundary) clears the cache so a
// fresh tx re-verifies. Proven by marking a known-bad page verified and
// observing Page short-circuit the (failing) footer check, then fail
// again after a reset.
func TestPageVerificationCachedAndReset(t *testing.T) {
	p, f := readerOverFile(t, 16, 16, true)
	defer p.Close()
	defer f.Close()

	// Uncached: the bad (zero) footer is caught.
	if _, err := p.Page(3); !errors.Is(err, ErrBadPageChecksum) {
		t.Fatalf("uncached Page(3) = %v, want ErrBadPageChecksum", err)
	}
	if p.isVerified(3) {
		t.Fatal("failed verification must not mark the page verified")
	}
	// Once recorded as verified, the footer check is skipped.
	p.markVerified(3, 16)
	if !p.isVerified(3) {
		t.Fatal("markVerified did not record page 3")
	}
	if _, err := p.Page(3); err != nil {
		t.Errorf("cached Page(3) = %v, want nil (verification skipped)", err)
	}
	// A tx-boundary reset clears the cache; the bad footer is caught again.
	p.resetVerified()
	if p.isVerified(3) {
		t.Fatal("resetVerified did not clear the cache")
	}
	if _, err := p.Page(3); !errors.Is(err, ErrBadPageChecksum) {
		t.Errorf("post-reset Page(3) = %v, want ErrBadPageChecksum", err)
	}
}

// TestPageChecksumDisabledSkipsVerify: with checksums off, Page returns
// page bytes without verifying a footer (a zero page is not rejected).
func TestPageChecksumDisabledSkipsVerify(t *testing.T) {
	p, f := readerOverFile(t, 16, 16, false)
	defer p.Close()
	defer f.Close()

	buf, err := p.Page(3)
	if err != nil {
		t.Fatalf("Page(3) checksums-off = %v, want nil", err)
	}
	if len(buf) != testPageSize {
		t.Errorf("Page returned %d bytes, want %d", len(buf), testPageSize)
	}
}
