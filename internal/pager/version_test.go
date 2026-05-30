package pager

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// TestOpenVersionMismatch covers BOTH pager-side injection points — Open
// and DiscoverPageSize — directly: an intact meta-0 (checksum + Magic
// valid) of a different format version is reported as ErrVersionMismatch,
// not ErrCorrupted. The root db.Open path reaches version detection via
// DiscoverPageSize (covered at the db level by
// TestOpenRejectsDifferentFormatVersion); this pins the Open injection,
// which a direct pager.Open caller hits.
func TestOpenVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.gmdb")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ip := InitParams{
		PageSize:        testPageSize,
		MinSize:         16,
		MaxSize:         128,
		GrowStep:        4,
		ShrinkThreshold: 8,
		UUID:            [16]byte{0xAA, 0xBB, 0xCC, 0xDD},
	}
	if err := Init(f, ip); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Forge meta-0 to a future format version, recomputing the checksum
	// so it stays intact (verifies) — a valid gmdb meta this binary
	// cannot read, distinct from corruption.
	buf := make([]byte, page.MetaPayloadSize)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("read meta0: %v", err)
	}
	m := page.DecodeMeta(buf)
	m.Version = page.FormatVersion + 1
	page.EncodeMeta(buf, &m)
	if _, err := f.WriteAt(buf, 0); err != nil {
		t.Fatalf("write forged meta0: %v", err)
	}

	// DiscoverPageSize path.
	if _, err := DiscoverPageSize(f); !errors.Is(err, ErrVersionMismatch) {
		t.Errorf("DiscoverPageSize: got %v, want ErrVersionMismatch", err)
	}

	// Open path (defense-in-depth for direct callers).
	_, err = Open(f, OpenParams{Pool: NewBufPool(testPageSize), MaxTxBufferBytes: 16 << 20})
	if !errors.Is(err, ErrVersionMismatch) {
		t.Errorf("Open: got %v, want ErrVersionMismatch", err)
	}
	if errors.Is(err, ErrCorrupted) {
		t.Errorf("version mismatch misreported as ErrCorrupted: %v", err)
	}
	_ = f.Close()
}
