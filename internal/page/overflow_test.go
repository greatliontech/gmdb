package page

import (
	"bytes"
	"testing"
)

func TestOverflowCapacity(t *testing.T) {
	tests := []struct {
		pageSize    uint32
		checksum    bool
		wantFirst   int
		wantFollow  int
	}{
		{4096, false, 4088, 4096},
		{4096, true, 4084, 4092},
		{8192, false, 8184, 8192},
	}
	for _, tt := range tests {
		cfg := PageConfig{PageSize: tt.pageSize, PageChecksum: tt.checksum}
		o := NewOverflowConfig(cfg)
		if got := o.FirstPageCapacity(); got != tt.wantFirst {
			t.Errorf("FirstPageCapacity(ps=%d,ck=%v) = %d, want %d",
				tt.pageSize, tt.checksum, got, tt.wantFirst)
		}
		if got := o.FollowerPageCapacity(); got != tt.wantFollow {
			t.Errorf("FollowerPageCapacity(ps=%d,ck=%v) = %d, want %d",
				tt.pageSize, tt.checksum, got, tt.wantFollow)
		}
	}
}

func TestOverflowPagesNeeded(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	o := NewOverflowConfig(cfg)

	tests := []struct {
		totalLen int
		want     int
	}{
		{0, 1},
		{100, 1},
		{4088, 1},
		{4089, 2},
		{4088 + 4096, 2},
		{4088 + 4097, 3},
	}
	for _, tt := range tests {
		got := o.PagesNeeded(tt.totalLen)
		if got != tt.want {
			t.Errorf("PagesNeeded(%d) = %d, want %d", tt.totalLen, got, tt.want)
		}
	}
}

func TestOverflowWriteRead(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	o := NewOverflowConfig(cfg)

	// Value spanning 3 pages.
	valueLen := 4088 + 4096 + 1000
	value := make([]byte, valueLen)
	for i := range value {
		value[i] = byte(i % 251) // prime modulus for variety
	}

	pages := o.PagesNeeded(valueLen)
	if pages != 3 {
		t.Fatalf("PagesNeeded(%d) = %d, want 3", valueLen, pages)
	}

	// Write.
	bufs := make([][]byte, pages)
	for i := range bufs {
		bufs[i] = make([]byte, cfg.PageSize)
	}

	offset := 0
	n := o.WriteFirstPage(bufs[0], uint32(pages-1), value[offset:])
	offset += n
	for i := 1; i < pages; i++ {
		n = o.WriteFollowerPage(bufs[i], value[offset:])
		offset += n
	}
	if offset != valueLen {
		t.Fatalf("wrote %d bytes, want %d", offset, valueLen)
	}

	// Verify header.
	typ, _, _, additional := ReadHeader(bufs[0])
	if typ != TypeOverflow {
		t.Errorf("Type = %d, want %d", typ, TypeOverflow)
	}
	if additional != 2 {
		t.Errorf("AdditionalPages = %d, want 2", additional)
	}

	// Read back.
	var result []byte
	result = append(result, o.ReadFirstPage(bufs[0])[:min(4088, valueLen)]...)
	remaining := valueLen - len(result)
	for i := 1; i < pages && remaining > 0; i++ {
		data := o.ReadFollowerPage(bufs[i])
		take := min(remaining, len(data))
		result = append(result, data[:take]...)
		remaining -= take
	}

	if !bytes.Equal(result, value) {
		t.Error("read value does not match written value")
	}
}
