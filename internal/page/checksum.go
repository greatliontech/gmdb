package page

import (
	"hash/crc32"
)

// crc32cTable is the CRC32C (Castagnoli) lookup table.
// Go's hash/crc32 automatically uses hardware acceleration (SSE4.2/ARM CRC)
// when available.
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// ComputeCRC32C computes the CRC32C checksum of buf[0 : len(buf)-CRC32Size].
// The last 4 bytes (footer position) are excluded from the checksum input.
func ComputeCRC32C(buf []byte) uint32 {
	return crc32.Checksum(buf[:len(buf)-CRC32Size], crc32cTable)
}

// WriteCRC32C computes the CRC32C checksum and writes it to the last 4 bytes
// of buf. buf must be at least CRC32Size bytes.
func WriteCRC32C(buf []byte) {
	c := ComputeCRC32C(buf)
	le.PutUint32(buf[len(buf)-CRC32Size:], c)
}

// VerifyCRC32C checks whether the CRC32C footer matches the page content.
// Returns true if valid.
func VerifyCRC32C(buf []byte) bool {
	c := ComputeCRC32C(buf)
	stored := le.Uint32(buf[len(buf)-CRC32Size:])
	return c == stored
}
