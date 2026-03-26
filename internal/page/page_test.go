package page

import "testing"

func TestValidPageSize(t *testing.T) {
	valid := []uint32{4096, 8192, 16384, 32768, 65536}
	for _, s := range valid {
		if !ValidPageSize(s) {
			t.Errorf("ValidPageSize(%d) = false, want true", s)
		}
	}
	invalid := []uint32{0, 1, 2048, 4095, 4097, 6000, 131072}
	for _, s := range invalid {
		if ValidPageSize(s) {
			t.Errorf("ValidPageSize(%d) = true, want false", s)
		}
	}
}

func TestReadWriteHeader(t *testing.T) {
	buf := make([]byte, HeaderSize)
	WriteHeader(buf, TypeLeaf, 0, 42, 3)

	typ, flags, count, additional := ReadHeader(buf)
	if typ != TypeLeaf {
		t.Errorf("Type = %d, want %d", typ, TypeLeaf)
	}
	if flags != 0 {
		t.Errorf("Flags = %d, want 0", flags)
	}
	if count != 42 {
		t.Errorf("Count = %d, want 42", count)
	}
	if additional != 3 {
		t.Errorf("AdditionalPages = %d, want 3", additional)
	}
}

func TestPageConfigUsableSpace(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	if got := cfg.UsableSpace(); got != 4088 {
		t.Errorf("UsableSpace() = %d, want 4088", got)
	}

	cfg.PageChecksum = true
	if got := cfg.UsableSpace(); got != 4084 {
		t.Errorf("UsableSpace() with checksum = %d, want 4084", got)
	}
}

func TestPageConfigMaxKeySize(t *testing.T) {
	tests := []struct {
		pageSize uint32
		checksum bool
		want     int
	}{
		{4096, false, 2028},
		{4096, true, 2026},
		{8192, false, 4076},
		{65536, false, 32748},
	}
	for _, tt := range tests {
		cfg := PageConfig{PageSize: tt.pageSize, PageChecksum: tt.checksum}
		got := cfg.MaxKeySize()
		if got != tt.want {
			t.Errorf("MaxKeySize(pageSize=%d, checksum=%v) = %d, want %d",
				tt.pageSize, tt.checksum, got, tt.want)
		}
	}
}

func TestPageConfigBitmapPages(t *testing.T) {
	cfg := PageConfig{PageSize: 4096}
	// 256GB / 4KB = 67,108,864 pages. BitsPerPage = 4096*8 = 32768.
	// 67108864 / 32768 = 2048.
	got := cfg.BitmapPages(67108864)
	if got != 2048 {
		t.Errorf("BitmapPages(67108864) = %d, want 2048", got)
	}
}

func TestPageConfigContentEnd(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	if got := cfg.ContentEnd(); got != 4096 {
		t.Errorf("ContentEnd() = %d, want 4096", got)
	}
	cfg.PageChecksum = true
	if got := cfg.ContentEnd(); got != 4092 {
		t.Errorf("ContentEnd() with checksum = %d, want 4092", got)
	}
}
