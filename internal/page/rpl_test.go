package page

import "testing"

func TestRPLSegmentCapacity(t *testing.T) {
	tests := []struct {
		pageSize uint32
		checksum bool
		want     int
	}{
		{4096, false, 508},  // (4096 - 32) / 8 = 508
		{4096, true, 507},   // (4096 - 32 - 4) / 8 = 507.5 → 507
		{8192, false, 1020}, // (8192 - 32) / 8 = 1020
	}
	for _, tt := range tests {
		cfg := PageConfig{PageSize: tt.pageSize, PageChecksum: tt.checksum}
		got := RPLSegmentCapacity(cfg)
		if got != tt.want {
			t.Errorf("RPLSegmentCapacity(ps=%d,ck=%v) = %d, want %d",
				tt.pageSize, tt.checksum, got, tt.want)
		}
	}
}

func TestRPLSegmentRoundTrip(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewRPLSegmentBuilder(buf, cfg)
	b.SetTxnID(100)
	b.SetOlderSegment(42)

	pageIDs := []uint64{10, 20, 30, 50, 100, 200}
	for _, pid := range pageIDs {
		if !b.AddPageID(pid) {
			t.Fatalf("AddPageID(%d) failed", pid)
		}
	}
	count := b.Finish()
	if count != uint16(len(pageIDs)) {
		t.Fatalf("Finish() = %d, want %d", count, len(pageIDs))
	}

	// Verify header.
	typ, flags, hcount, additional := ReadHeader(buf)
	if typ != TypeRPLSegment {
		t.Errorf("Type = %d, want %d", typ, TypeRPLSegment)
	}
	if flags != 0 {
		t.Errorf("Flags = %d, want 0", flags)
	}
	if hcount != uint16(len(pageIDs)) {
		t.Errorf("header Count = %d, want %d", hcount, len(pageIDs))
	}
	if additional != 0 {
		t.Errorf("AdditionalPages = %d, want 0", additional)
	}

	// Read back.
	r := NewRPLSegmentReader(buf, cfg)
	if r.TxnID() != 100 {
		t.Errorf("TxnID = %d, want 100", r.TxnID())
	}
	if r.OlderSegment() != 42 {
		t.Errorf("OlderSegment = %d, want 42", r.OlderSegment())
	}
	if r.EntryCount() != len(pageIDs) {
		t.Errorf("EntryCount = %d, want %d", r.EntryCount(), len(pageIDs))
	}
	for i, want := range pageIDs {
		got := r.PageID(i)
		if got != want {
			t.Errorf("PageID(%d) = %d, want %d", i, got, want)
		}
	}
}

func TestRPLSegmentFull(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewRPLSegmentBuilder(buf, cfg)
	b.SetTxnID(1)

	cap := b.Capacity()
	if cap != 508 {
		t.Fatalf("Capacity = %d, want 508", cap)
	}

	for i := range cap {
		if !b.AddPageID(uint64(i + 100)) {
			t.Fatalf("AddPageID failed at %d", i)
		}
	}
	// One more should fail.
	if b.AddPageID(9999) {
		t.Fatal("AddPageID succeeded past capacity")
	}

	if b.Count() != 508 {
		t.Errorf("Count() = %d, want 508", b.Count())
	}
}
