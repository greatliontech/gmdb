package btree

import (
	"bytes"
	"fmt"
	"slices"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

func FuzzPutGetDelete(f *testing.F) {
	// Seed corpus: op (0=put, 1=delete), key suffix (uint16).
	f.Add(uint8(0), uint16(42))
	f.Add(uint8(1), uint16(42))
	f.Add(uint8(0), uint16(0))
	f.Add(uint8(1), uint16(0))
	f.Add(uint8(0), uint16(255))

	f.Fuzz(func(t *testing.T, op uint8, keySuffix uint16) {
		tr := newTestTree(t, 128)
		ref := make(map[string]string) // reference model

		// Run a small sequence of operations.
		key := fmt.Appendf(nil, "k:%04d", keySuffix%200)
		val := fmt.Appendf(nil, "v:%04d", keySuffix%200)

		// Always insert first.
		_, _, err := tr.Put(inlineEntry(key, val))
		if err != nil {
			t.Fatal(err)
		}
		ref[string(key)] = string(val)

		if op%2 == 1 {
			// Delete.
			_, found, err := tr.Delete(key)
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatal("delete should find the key we just inserted")
			}
			delete(ref, string(key))
		}

		// Verify consistency.
		for k, v := range ref {
			entry, found := tr.Get([]byte(k))
			if !found {
				t.Fatalf("key %q not found in tree", k)
			}
			if !bytes.Equal(entry.Value, []byte(v)) {
				t.Fatalf("key %q: value = %q, want %q", k, entry.Value, v)
			}
		}
	})
}

func FuzzRandomOps(f *testing.F) {
	f.Add([]byte{0, 10, 0, 20, 1, 10, 0, 30})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}

		tr := newTestTree(t, 512)
		ref := make(map[string]string)

		for i := 0; i+1 < len(data); i += 2 {
			op := data[i]
			keyIdx := data[i+1]
			key := fmt.Appendf(nil, "k:%03d", keyIdx)
			val := fmt.Appendf(nil, "v:%03d", keyIdx)

			if op%3 != 1 {
				// Put (2/3 probability).
				_, _, err := tr.Put(inlineEntry(key, val))
				if err != nil {
					return // no space is acceptable
				}
				ref[string(key)] = string(val)
			} else {
				// Delete.
				_, _, err := tr.Delete(key)
				if err != nil {
					t.Fatal(err)
				}
				delete(ref, string(key))
			}
		}

		// Verify all reference keys exist with correct values.
		for k, v := range ref {
			entry, found := tr.Get([]byte(k))
			if !found {
				t.Fatalf("key %q not found", k)
			}
			if !bytes.Equal(entry.Value, []byte(v)) {
				t.Fatalf("key %q: value = %q, want %q", k, entry.Value, v)
			}
		}

		// Verify cursor produces sorted keys matching reference.
		c := tr.NewCursor()
		var cursorKeys []string
		for k, v := c.First(); k != nil; k, v = c.Next() {
			sk := string(k)
			cursorKeys = append(cursorKeys, sk)
			if expected, ok := ref[sk]; ok {
				if !bytes.Equal(v, []byte(expected)) {
					t.Fatalf("cursor key %q: value = %q, want %q", sk, v, expected)
				}
			} else {
				t.Fatalf("cursor returned key %q not in reference", sk)
			}
		}

		if len(cursorKeys) != len(ref) {
			t.Fatalf("cursor returned %d keys, reference has %d", len(cursorKeys), len(ref))
		}

		// Verify sorted order.
		if !slices.IsSorted(cursorKeys) {
			t.Fatal("cursor keys not sorted")
		}
	})
}

func FuzzCursorConsistency(f *testing.F) {
	f.Add(uint8(50), uint8(25))

	f.Fuzz(func(t *testing.T, n, seekTarget uint8) {
		if n == 0 {
			return
		}
		count := int(n) % 100

		tr := newTestTree(t, 256)
		var allKeys []string

		for i := range count {
			key := fmt.Appendf(nil, "key:%04d", i)
			val := fmt.Appendf(nil, "val:%04d", i)
			_, _, err := tr.Put(page.LeafEntry{Key: key, Value: val})
			if err != nil {
				return // no space
			}
			allKeys = append(allKeys, string(key))
		}
		slices.Sort(allKeys)

		c := tr.NewCursor()

		// Forward: First + Next should yield all keys in order.
		var forward []string
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			forward = append(forward, string(k))
		}
		if len(forward) != len(allKeys) {
			t.Fatalf("forward scan: %d keys, want %d", len(forward), len(allKeys))
		}
		for i, k := range forward {
			if k != allKeys[i] {
				t.Fatalf("forward[%d] = %q, want %q", i, k, allKeys[i])
			}
		}

		// Reverse: Last + Prev should yield all keys in reverse order.
		var reverse []string
		for k, _ := c.Last(); k != nil; k, _ = c.Prev() {
			reverse = append(reverse, string(k))
		}
		if len(reverse) != len(allKeys) {
			t.Fatalf("reverse scan: %d keys, want %d", len(reverse), len(allKeys))
		}
		slices.Reverse(reverse)
		for i, k := range reverse {
			if k != allKeys[i] {
				t.Fatalf("reverse[%d] = %q, want %q", i, k, allKeys[i])
			}
		}
	})
}
