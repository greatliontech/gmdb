package page

import "testing"

func TestCRC32CRoundTrip(t *testing.T) {
	buf := make([]byte, 4096)
	// Write some data.
	for i := range buf[:len(buf)-CRC32Size] {
		buf[i] = byte(i)
	}
	WriteCRC32C(buf)

	if !VerifyCRC32C(buf) {
		t.Fatal("VerifyCRC32C failed after WriteCRC32C")
	}
}

func TestCRC32CDetectsCorruption(t *testing.T) {
	buf := make([]byte, 4096)
	for i := range buf[:len(buf)-CRC32Size] {
		buf[i] = byte(i * 3)
	}
	WriteCRC32C(buf)

	// Flip a bit in the data area.
	buf[100] ^= 0x01

	if VerifyCRC32C(buf) {
		t.Fatal("VerifyCRC32C passed after data corruption")
	}
}

func TestCRC32CDetectsFooterCorruption(t *testing.T) {
	buf := make([]byte, 4096)
	for i := range buf[:len(buf)-CRC32Size] {
		buf[i] = byte(i)
	}
	WriteCRC32C(buf)

	// Flip a bit in the footer.
	buf[len(buf)-1] ^= 0x01

	if VerifyCRC32C(buf) {
		t.Fatal("VerifyCRC32C passed after footer corruption")
	}
}

func TestCRC32CSmallPage(t *testing.T) {
	buf := make([]byte, 8) // minimum: 4 data bytes + 4 footer bytes
	buf[0] = 0xAA
	buf[1] = 0xBB
	buf[2] = 0xCC
	buf[3] = 0xDD
	WriteCRC32C(buf)

	if !VerifyCRC32C(buf) {
		t.Fatal("VerifyCRC32C failed for small buffer")
	}
}
