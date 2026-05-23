package page

import (
	"bytes"
	"testing"
)

func TestValidPageSize(t *testing.T) {
	cases := []struct {
		size uint32
		want bool
	}{
		{0, false},
		{512, false},
		{2048, false},
		{4095, false},
		{4096, true},
		{4097, false},
		{8192, true},
		{16384, true},
		{32768, true},
		{65536, true},
		{65537, false},
		{131072, false},
	}
	for _, c := range cases {
		if got := ValidPageSize(c.size); got != c.want {
			t.Errorf("ValidPageSize(%d) = %v, want %v", c.size, got, c.want)
		}
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	buf := make([]byte, 16)
	WriteHeader(buf, TypeBranch, 0x1234, 0xDEADBEEF)
	typ, flags, count, additional := ReadHeader(buf)
	if typ != TypeBranch || flags != 0 || count != 0x1234 || additional != 0xDEADBEEF {
		t.Fatalf("round-trip mismatch: %d %d %d %d", typ, flags, count, additional)
	}
}

func TestPageFooterRoundTrip(t *testing.T) {
	for _, sz := range []uint32{4096, 8192, 65536} {
		buf := make([]byte, sz)
		for i := range buf[:sz-FooterSize] {
			buf[i] = byte(i)
		}
		WritePageFooter(buf, sz)
		if !VerifyPageFooter(buf, sz) {
			t.Fatalf("verify failed for page size %d", sz)
		}
		// Flip one byte in the content; verify must fail.
		buf[10] ^= 0x01
		if VerifyPageFooter(buf, sz) {
			t.Fatalf("expected verify failure after content tamper (size %d)", sz)
		}
		buf[10] ^= 0x01 // restore
		if !VerifyPageFooter(buf, sz) {
			t.Fatalf("restore failed (size %d)", sz)
		}
		// Flip one byte in the footer; verify must fail.
		buf[sz-1] ^= 0x01
		if VerifyPageFooter(buf, sz) {
			t.Fatalf("expected verify failure after footer tamper (size %d)", sz)
		}
	}
}

func TestPageFooterRejectsWrongSize(t *testing.T) {
	for _, name := range []string{"larger", "smaller"} {
		var buf []byte
		if name == "larger" {
			buf = make([]byte, 8192)
		} else {
			buf = make([]byte, 2048)
		}
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("%s: WritePageFooter did not panic on wrong-size buf", name)
				}
			}()
			WritePageFooter(buf, 4096)
		}()
	}
}

func TestConfigContentEndAndUsable(t *testing.T) {
	cases := []struct {
		cfg    Config
		end    int
		usable int
	}{
		{Config{PageSize: 4096, PageChecksum: true}, 4088, 4080},
		{Config{PageSize: 4096, PageChecksum: false}, 4096, 4088},
		{Config{PageSize: 65536, PageChecksum: true}, 65528, 65520},
	}
	for _, c := range cases {
		if got := c.cfg.ContentEnd(); got != c.end {
			t.Errorf("ContentEnd(%+v) = %d, want %d", c.cfg, got, c.end)
		}
		if got := c.cfg.UsableSpace(); got != c.usable {
			t.Errorf("UsableSpace(%+v) = %d, want %d", c.cfg, got, c.usable)
		}
	}
}

func TestComputePageChecksumDeterministic(t *testing.T) {
	const sz uint32 = 4096
	buf := bytes.Repeat([]byte{0xAB}, int(sz))
	a := ComputePageChecksum(buf, sz)
	b := ComputePageChecksum(buf, sz)
	if a != b {
		t.Fatalf("non-deterministic: %d vs %d", a, b)
	}
	// All-zero footer slot should not affect the prefix hash.
	clear(buf[len(buf)-FooterSize:])
	c := ComputePageChecksum(buf, sz)
	if a != c {
		t.Fatalf("footer-region affected prefix hash: %d vs %d", a, c)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{PageSize: 4096}).Validate(); err != nil {
		t.Errorf("Validate(4096) = %v, want nil", err)
	}
	if err := (Config{PageSize: 0}).Validate(); err == nil {
		t.Error("Validate(0) = nil, want error")
	}
	if err := (Config{PageSize: 4095}).Validate(); err == nil {
		t.Error("Validate(non-power-of-two) = nil, want error")
	}
	if err := (Config{PageSize: 4096, PageChecksum: true}).Validate(); err != nil {
		t.Errorf("Validate with checksum = %v, want nil", err)
	}
}
