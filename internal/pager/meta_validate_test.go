package pager

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSelectionRejectsInvalidFieldsMeta pins the field-validation gate
// behind every meta-selection path (decodeActiveMeta): a meta whose
// checksum VERIFIES but whose decoded fields are invalid — here an
// unknown Flags bit, file-layout.md §Meta Page — is ErrCorrupted at
// Open, ReadLatestMeta, and Resync alike. Selection trusts the
// checksum; field validation is the separate, mandatory second gate.
func TestSelectionRejectsInvalidFieldsMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.gmdb")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	ip := InitParams{
		PageSize:        testPageSize,
		MinSize:         16,
		MaxSize:         128,
		GrowStep:        4,
		ShrinkThreshold: 8,
	}
	if err := Init(f, ip); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// A live writer, opened BEFORE the forgery, exercises the Resync path.
	od := openAttachedForTest(t, f, OpenParams{Pool: NewBufPool(testPageSize), MaxTxBufferBytes: 16 << 20})

	// Forge BOTH slots: set an unknown Flags bit and recompute the
	// checksum, so selection succeeds (valid checksums, tie at TxnID 0
	// → slot 0) and only ValidateMeta can reject.
	forge := func(off int64) {
		buf := make([]byte, MetaPayloadSize)
		if _, err := f.ReadAt(buf, off); err != nil {
			t.Fatalf("read meta at %d: %v", off, err)
		}
		m := DecodeMeta(buf)
		m.Flags |= 1 << 31
		EncodeMeta(buf, &m)
		if _, err := f.WriteAt(buf, off); err != nil {
			t.Fatalf("write forged meta at %d: %v", off, err)
		}
	}
	forge(0)
	forge(int64(testPageSize))

	if _, err := ReadLatestMeta(f, testPageSize); !errors.Is(err, ErrCorrupted) {
		t.Errorf("ReadLatestMeta: got %v, want ErrCorrupted", err)
	}
	if _, _, _, err := od.Pager.Resync(f, 999); !errors.Is(err, ErrCorrupted) {
		t.Errorf("Resync: got %v, want ErrCorrupted", err)
	}
	if err := od.Pager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := Open(f, OpenParams{Pool: NewBufPool(testPageSize), MaxTxBufferBytes: 16 << 20}); !errors.Is(err, ErrCorrupted) {
		t.Errorf("Open: got %v, want ErrCorrupted", err)
	}
}
