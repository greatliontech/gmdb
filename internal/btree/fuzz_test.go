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
	// Seed with a DeleteRange op (op%4==3) that consumes 3 bytes.
	f.Add([]byte{0, 5, 0, 10, 0, 15, 0, 20, 3, 5, 20})
	// Seed with many puts to exercise splits, then deletes and a range delete.
	f.Add([]byte{
		0, 1, 0, 2, 0, 3, 0, 4, 0, 5, 0, 6, 0, 7, 0, 8,
		0, 9, 0, 10, 0, 11, 0, 12, 0, 13, 0, 14, 0, 15, 0, 16,
		2, 3, // delete key 3
		3, 5, 12, // delete range [5, 12)
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}

		tr := newTestTree(t, 512)
		ref := make(map[string]string)

		i := 0
		for i < len(data) {
			if i+1 >= len(data) {
				break
			}
			op := data[i] % 4
			switch op {
			case 0, 1: // Put (2/4 probability).
				keyIdx := data[i+1]
				key := fmt.Appendf(nil, "k:%03d", keyIdx)
				val := fmt.Appendf(nil, "v:%03d", keyIdx)
				_, _, err := tr.Put(inlineEntry(key, val))
				if err != nil {
					if err == ErrNoSpace {
						return // acceptable, stop this iteration
					}
					t.Fatal(err)
				}
				ref[string(key)] = string(val)
				i += 2
			case 2: // Delete.
				keyIdx := data[i+1]
				key := fmt.Appendf(nil, "k:%03d", keyIdx)
				_, _, err := tr.Delete(key)
				if err != nil {
					t.Fatal(err)
				}
				delete(ref, string(key))
				i += 2
			case 3: // DeleteRange.
				if i+2 >= len(data) {
					i = len(data) // not enough bytes, stop
					break
				}
				startIdx := data[i+1]
				endIdx := data[i+2]
				var start, end []byte
				start = fmt.Appendf(nil, "k:%03d", startIdx)
				end = fmt.Appendf(nil, "k:%03d", endIdx)

				_, err := tr.DeleteRange(start, end)
				if err != nil {
					if err == ErrNoSpace {
						return
					}
					t.Fatal(err)
				}
				// Mirror in ref: remove keys in [start, end).
				for k := range ref {
					if k >= string(start) && k < string(end) {
						delete(ref, k)
					}
				}
				i += 3
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

		// Negative check: verify keys NOT in ref are NOT found in tree.
		// Sample the full keyspace used by the test (k:000 through k:255).
		for idx := 0; idx < 256; idx += 3 {
			key := fmt.Appendf(nil, "k:%03d", idx)
			if _, ok := ref[string(key)]; ok {
				continue
			}
			if _, found := tr.Get(key); found {
				t.Fatalf("key %q should not be in tree but was found", key)
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
	f.Add(uint8(200), uint8(100)) // larger tree to exercise more splits

	f.Fuzz(func(t *testing.T, n, seekTarget uint8) {
		if n == 0 {
			return
		}
		count := int(n)

		tr := newTestTree(t, 512)
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

		// Seek: exact match for each inserted key.
		for _, k := range allKeys {
			sk, sv := c.Seek([]byte(k))
			if sk == nil {
				t.Fatalf("Seek(%q) returned nil", k)
			}
			if string(sk) != k {
				t.Fatalf("Seek(%q) returned key %q", k, sk)
			}
			expected := "val:" + k[len("key:"):]
			if string(sv) != expected {
				t.Fatalf("Seek(%q) value = %q, want %q", k, sv, expected)
			}
		}

		// SeekGE: seek to a key between existing keys.
		target := fmt.Appendf(nil, "key:%04d", int(seekTarget))
		sk, _ := c.SeekGE(target)
		if len(allKeys) > 0 && sk != nil {
			if string(sk) < string(target) {
				t.Fatalf("SeekGE(%q) returned %q which is less than target", target, sk)
			}
		}

		// Seek non-existent key should return nil.
		sk, _ = c.Seek([]byte("zzz:does-not-exist"))
		if sk != nil {
			t.Fatalf("Seek for nonexistent key returned %q", sk)
		}

		// Mutation invalidation: delete a key, then verify cursor detects staleness.
		if len(allKeys) > 0 {
			mid := len(allKeys) / 2
			delKey := []byte(allKeys[mid])
			_, _, err := tr.Delete(delKey)
			if err != nil {
				t.Fatal(err)
			}
			// Re-scan after mutation should produce count-1 keys.
			remaining := make([]string, 0, len(allKeys)-1)
			for k, _ := c.First(); k != nil; k, _ = c.Next() {
				remaining = append(remaining, string(k))
			}
			if len(remaining) != len(allKeys)-1 {
				t.Fatalf("after delete: scan got %d keys, want %d", len(remaining), len(allKeys)-1)
			}
			if !slices.IsSorted(remaining) {
				t.Fatal("after delete: cursor keys not sorted")
			}
			// Deleted key must not appear.
			for _, k := range remaining {
				if k == string(delKey) {
					t.Fatalf("deleted key %q still in cursor scan", delKey)
				}
			}
		}
	})
}

func FuzzDeleteRangeConsistency(f *testing.F) {
	// Seed corpus with edge cases.
	// (nEntries, startIdx, endIdx): populate N keys, DeleteRange([startIdx, endIdx)).
	f.Add(uint8(50), uint8(10), uint8(30))  // normal range
	f.Add(uint8(50), uint8(20), uint8(20))  // empty range (start == end)
	f.Add(uint8(50), uint8(40), uint8(10))  // inverted range (start > end)
	f.Add(uint8(50), uint8(0), uint8(50))   // delete all
	f.Add(uint8(50), uint8(0), uint8(0))    // empty range at start
	f.Add(uint8(100), uint8(25), uint8(75)) // larger tree, middle half
	f.Add(uint8(1), uint8(0), uint8(1))     // single entry tree, delete it
	f.Add(uint8(10), uint8(0), uint8(255))  // end beyond max key

	f.Fuzz(func(t *testing.T, nEntries, startIdx, endIdx uint8) {
		if nEntries == 0 {
			return
		}
		n := int(nEntries)

		tr := newTestTree(t, 512)
		ref := make(map[string]string, n)

		for i := range n {
			key := fmt.Appendf(nil, "k:%03d", i)
			val := fmt.Appendf(nil, "v:%03d", i)
			_, _, err := tr.Put(inlineEntry(key, val))
			if err != nil {
				// Tree ran out of space; test with whatever was inserted.
				n = i
				break
			}
			ref[string(key)] = string(val)
		}

		if n == 0 {
			return
		}

		start := fmt.Appendf(nil, "k:%03d", startIdx)
		end := fmt.Appendf(nil, "k:%03d", endIdx)

		_, err := tr.DeleteRange(start, end)
		if err != nil {
			t.Fatal(err)
		}

		// Mirror in ref: remove keys in [start, end).
		startStr := string(start)
		endStr := string(end)
		for k := range ref {
			if k >= startStr && k < endStr {
				delete(ref, k)
			}
		}

		// Verify all surviving keys exist with correct values.
		for k, v := range ref {
			entry, found := tr.Get([]byte(k))
			if !found {
				t.Fatalf("key %q not found after DeleteRange", k)
			}
			if !bytes.Equal(entry.Value, []byte(v)) {
				t.Fatalf("key %q: value = %q, want %q", k, entry.Value, v)
			}
		}

		// Negative check: keys in the deleted range must not be found.
		for i := range 256 {
			key := fmt.Appendf(nil, "k:%03d", i)
			sk := string(key)
			if _, ok := ref[sk]; ok {
				continue // key should exist
			}
			if _, found := tr.Get(key); found {
				t.Fatalf("key %q should have been deleted but was found", sk)
			}
		}

		// Cursor scan: sorted, matches ref count, every key in ref.
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

		if !slices.IsSorted(cursorKeys) {
			t.Fatal("cursor keys not sorted")
		}
	})
}
