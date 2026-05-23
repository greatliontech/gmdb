package page

import (
	"reflect"
	"testing"
)

func TestRPLEntriesPerSegment(t *testing.T) {
	cases := []struct {
		cfg  Config
		want int
	}{
		{Config{PageSize: 4096, PageChecksum: false}, 509},
		{Config{PageSize: 4096, PageChecksum: true}, 508},
		{Config{PageSize: 8192, PageChecksum: false}, 1021},
		{Config{PageSize: 8192, PageChecksum: true}, 1020},
		{Config{PageSize: 65536, PageChecksum: true}, 8188},
	}
	for _, c := range cases {
		if got := RPLEntriesPerSegment(c.cfg); got != c.want {
			t.Errorf("RPLEntriesPerSegment(%+v) = %d, want %d", c.cfg, got, c.want)
		}
	}
}

func TestRPLSegmentRoundTrip(t *testing.T) {
	cfg := Config{PageSize: 4096, PageChecksum: true}
	buf := make([]byte, cfg.PageSize)

	ids := []uint64{3, 5, 7, 11, 13, 17, 19, 23}
	EncodeRPLSegment(buf, cfg, 42, 100, ids)
	WritePageFooter(buf, cfg.PageSize)
	if !VerifyPageFooter(buf, cfg.PageSize) {
		t.Fatal("footer verify failed")
	}

	// Page header should report Count and Type.
	typ, _, count, _ := ReadHeader(buf)
	if typ != TypeRPLSegment {
		t.Errorf("Type = %d, want %d", typ, TypeRPLSegment)
	}
	if count != uint16(len(ids)) {
		t.Errorf("Count = %d, want %d", count, len(ids))
	}

	got, ok := DecodeRPLSegment(buf, cfg)
	if !ok {
		t.Fatal("DecodeRPLSegment returned ok=false on valid segment")
	}
	want := RPLSegment{TxnID: 42, OlderSegment: 100, PageIDs: ids}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestRPLSegmentEmpty(t *testing.T) {
	cfg := Config{PageSize: 4096, PageChecksum: true}
	buf := make([]byte, cfg.PageSize)
	EncodeRPLSegment(buf, cfg, 1, 0, nil)
	WritePageFooter(buf, cfg.PageSize)
	if !VerifyPageFooter(buf, cfg.PageSize) {
		t.Fatal("footer verify failed for empty segment")
	}
	got, ok := DecodeRPLSegment(buf, cfg)
	if !ok {
		t.Fatal("DecodeRPLSegment returned ok=false on empty segment")
	}
	if got.TxnID != 1 || got.OlderSegment != 0 || got.EntryCount() != 0 {
		t.Fatalf("empty segment decoded wrong: %+v", got)
	}
}

func TestRPLSegmentDecodeRejectsWrongType(t *testing.T) {
	cfg := Config{PageSize: 4096, PageChecksum: true}
	buf := make([]byte, cfg.PageSize)
	EncodeRPLSegment(buf, cfg, 1, 0, []uint64{42})
	// Corrupt the Type byte: pretend it's a leaf page.
	buf[0] = TypeLeaf
	if _, ok := DecodeRPLSegment(buf, cfg); ok {
		t.Fatal("Decode accepted wrong page type")
	}
}

func TestRPLSegmentDecodeRejectsOversizedCount(t *testing.T) {
	cfg := Config{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)
	EncodeRPLSegment(buf, cfg, 1, 0, []uint64{42})
	// Tamper the page-header Count to claim more entries than fit.
	le.PutUint16(buf[offCount:], uint16(RPLEntriesPerSegment(cfg)+1))
	if _, ok := DecodeRPLSegment(buf, cfg); ok {
		t.Fatal("Decode accepted out-of-range count")
	}
}

func TestRPLSegmentDecodeRejectsShortBuf(t *testing.T) {
	cfg := Config{PageSize: 4096}
	short := make([]byte, 1024)
	if _, ok := DecodeRPLSegment(short, cfg); ok {
		t.Fatal("Decode accepted short buffer")
	}
}

func TestEncodeRPLSegmentPanicsOnShortBuf(t *testing.T) {
	cfg := Config{PageSize: 4096}
	short := make([]byte, 1024)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("EncodeRPLSegment did not panic on short buf")
		}
	}()
	EncodeRPLSegment(short, cfg, 1, 0, nil)
}

func TestRPLSegmentTailZeroed(t *testing.T) {
	cfg := Config{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)
	// Pre-fill with garbage so we can verify EncodeRPLSegment zeroes the tail.
	for i := range buf {
		buf[i] = 0xAA
	}
	ids := []uint64{1, 2, 3}
	EncodeRPLSegment(buf, cfg, 5, 0, ids)
	for i := RPLHeaderSize + len(ids)*8; i < cfg.ContentEnd(); i++ {
		if buf[i] != 0 {
			t.Fatalf("tail byte %d not zeroed: %x", i, buf[i])
		}
	}
}

func TestRPLSegmentOverflowPanics(t *testing.T) {
	cfg := Config{PageSize: 4096, PageChecksum: true}
	buf := make([]byte, cfg.PageSize)
	tooMany := make([]uint64, RPLEntriesPerSegment(cfg)+1)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on overflow")
		}
	}()
	EncodeRPLSegment(buf, cfg, 1, 0, tooMany)
}
