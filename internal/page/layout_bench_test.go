package page

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
)

// Microbenchmark isolating the one verified gmdb-vs-grove compressed-leaf
// difference: the inline 4-byte ValueLen sits BEFORE the key (gmdb) or
// AFTER it (grove). Both encoders build the same logical entries and both
// scanners run the same delta-key reconstruction; only the ValueLen byte
// position differs, so any ns/op gap is attributable to that choice alone.
//
// Synthetic + minimal on purpose (the layout is the sole variable). The
// scan models the in-group delta walk that dominates a compressed-leaf
// search; it omits the restart-table group jump (which is identical for
// both layouts), so absolute times are a few entries high but the
// gmdb-vs-grove DELTA — the thing we're measuring — is faithful.

var ble = binary.LittleEndian

type layoutEntry struct{ key, val []byte }

func layoutEntries(n, valLen int) []layoutEntry {
	es := make([]layoutEntry, n)
	for i := range es {
		es[i] = layoutEntry{
			key: []byte(fmt.Sprintf("user:account:%08d", i)), // ~13-byte shared prefix
			val: bytes.Repeat([]byte{byte(i)}, valLen),
		}
	}
	return es
}

func sharedLen(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// gmdb: [flags][shared u16][unshared u16][vlen u32][unsharedKey][value]
func encGmdb(es []layoutEntry) []byte {
	var buf, prev []byte
	for _, e := range es {
		s := sharedLen(prev, e.key)
		u := e.key[s:]
		var h [9]byte
		ble.PutUint16(h[1:], uint16(s))
		ble.PutUint16(h[3:], uint16(len(u)))
		ble.PutUint32(h[5:], uint32(len(e.val)))
		buf = append(buf, h[:]...)
		buf = append(buf, u...)
		buf = append(buf, e.val...)
		prev = e.key
	}
	return buf
}

// grove: [flags][shared u16][unshared u16][unsharedKey][vlen u32][value]
func encGrove(es []layoutEntry) []byte {
	var buf, prev []byte
	for _, e := range es {
		s := sharedLen(prev, e.key)
		u := e.key[s:]
		var h [5]byte
		ble.PutUint16(h[1:], uint16(s))
		ble.PutUint16(h[3:], uint16(len(u)))
		buf = append(buf, h[:]...)
		buf = append(buf, u...)
		var vl [4]byte
		ble.PutUint32(vl[:], uint32(len(e.val)))
		buf = append(buf, vl[:]...)
		buf = append(buf, e.val...)
		prev = e.key
	}
	return buf
}

func findGmdb(buf []byte, n int, target, keyBuf []byte) []byte {
	off := 0
	for i := 0; i < n; i++ {
		off++ // flags
		s := int(ble.Uint16(buf[off:]))
		u := int(ble.Uint16(buf[off+2:]))
		vl := int(ble.Uint32(buf[off+4:]))
		off += 8
		keyBuf = append(keyBuf[:s], buf[off:off+u]...)
		off += u
		val := buf[off : off+vl]
		off += vl
		if bytes.Equal(keyBuf, target) {
			return val
		}
	}
	return nil
}

func findGrove(buf []byte, n int, target, keyBuf []byte) []byte {
	off := 0
	for i := 0; i < n; i++ {
		off++ // flags
		s := int(ble.Uint16(buf[off:]))
		u := int(ble.Uint16(buf[off+2:]))
		off += 4
		keyBuf = append(keyBuf[:s], buf[off:off+u]...)
		off += u
		vl := int(ble.Uint32(buf[off:]))
		off += 4
		val := buf[off : off+vl]
		off += vl
		if bytes.Equal(keyBuf, target) {
			return val
		}
	}
	return nil
}

func iterGmdb(buf []byte, n int, keyBuf []byte) int {
	off, sum := 0, 0
	for i := 0; i < n; i++ {
		off++
		s := int(ble.Uint16(buf[off:]))
		u := int(ble.Uint16(buf[off+2:]))
		vl := int(ble.Uint32(buf[off+4:]))
		off += 8
		keyBuf = append(keyBuf[:s], buf[off:off+u]...)
		off += u + vl
		sum += len(keyBuf) + vl
	}
	return sum
}

func iterGrove(buf []byte, n int, keyBuf []byte) int {
	off, sum := 0, 0
	for i := 0; i < n; i++ {
		off++
		s := int(ble.Uint16(buf[off:]))
		u := int(ble.Uint16(buf[off+2:]))
		off += 4
		keyBuf = append(keyBuf[:s], buf[off:off+u]...)
		off += u
		vl := int(ble.Uint32(buf[off:]))
		off += 4 + vl
		sum += len(keyBuf) + vl
	}
	return sum
}

// TestLayoutScanCorrect makes the benchmark trustworthy: both scanners
// must find every key's exact value, so the ns/op figures measure correct
// work, not a broken loop.
func TestLayoutScanCorrect(t *testing.T) {
	const n = 32
	es := layoutEntries(n, 48)
	gb, vb := encGmdb(es), encGrove(es)
	kb := make([]byte, 0, 64)
	for _, e := range es {
		if g := findGmdb(gb, n, e.key, kb); !bytes.Equal(g, e.val) {
			t.Fatalf("gmdb find %q: wrong value", e.key)
		}
		if v := findGrove(vb, n, e.key, kb); !bytes.Equal(v, e.val) {
			t.Fatalf("grove find %q: wrong value", e.key)
		}
	}
	if iterGmdb(gb, n, kb) != iterGrove(vb, n, kb) {
		t.Fatal("iterate sums differ")
	}
}

func BenchmarkLayoutSearch(b *testing.B) {
	const n = 32
	for _, vl := range []int{16, 256} {
		es := layoutEntries(n, vl)
		gb, vb := encGmdb(es), encGrove(es)
		kb := make([]byte, 0, 64)
		b.Run(fmt.Sprintf("val=%d/gmdb", vl), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if findGmdb(gb, n, es[i%n].key, kb) == nil {
					b.Fatal("nf")
				}
			}
		})
		b.Run(fmt.Sprintf("val=%d/grove", vl), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if findGrove(vb, n, es[i%n].key, kb) == nil {
					b.Fatal("nf")
				}
			}
		})
	}
}

func BenchmarkLayoutIterate(b *testing.B) {
	const n = 32
	for _, vl := range []int{16, 256} {
		es := layoutEntries(n, vl)
		gb, vb := encGmdb(es), encGrove(es)
		kb := make([]byte, 0, 64)
		b.Run(fmt.Sprintf("val=%d/gmdb", vl), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				iterGmdb(gb, n, kb)
			}
		})
		b.Run(fmt.Sprintf("val=%d/grove", vl), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				iterGrove(vb, n, kb)
			}
		})
	}
}
