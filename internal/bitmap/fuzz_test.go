package bitmap

import (
	"testing"
)

func FuzzSetClear(f *testing.F) {
	f.Add(uint16(20), uint16(30), true, false)
	f.Add(uint16(10), uint16(10), true, true) // same page
	f.Add(uint16(63), uint16(64), false, true) // word boundary

	f.Fuzz(func(t *testing.T, a, b uint16, setA, setB bool) {
		const totalPages = 256
		const reserved = 10

		pageA := uint64(a)%totalPages
		pageB := uint64(b)%totalPages

		if pageA < reserved {
			pageA = reserved
		}
		if pageB < reserved {
			pageB = reserved
		}

		data := makeBitmapData(totalPages)
		bm := New(data, totalPages, reserved)

		if setA {
			bm.Set(pageA)
		} else {
			// Need page to be free first to Clear it meaningfully.
			bm.Set(pageA)
			bm.Clear(pageA)
		}

		if setB {
			bm.Set(pageB)
		} else {
			bm.Set(pageB)
			bm.Clear(pageB)
		}

		// Verify invariant: CountFree matches FreeCount.
		if bm.CountFree() != bm.FreeCount() {
			t.Errorf("CountFree()=%d != FreeCount()=%d", bm.CountFree(), bm.FreeCount())
		}
	})
}

func FuzzFindContiguous(f *testing.F) {
	f.Add(uint8(3), uint64(0xFF00FF00FF00FF00), uint64(0x00FF00FF00FF00FF))

	f.Fuzz(func(t *testing.T, n uint8, w0, w1 uint64) {
		const totalPages = 128
		const reserved = 4

		if n == 0 || n > 64 {
			return
		}

		data := makeBitmapData(totalPages)
		le.PutUint64(data[0:], w0)
		le.PutUint64(data[8:], w1)
		// Clear reserved bits.
		mask := uint64((1 << reserved) - 1)
		w := le.Uint64(data[0:])
		w &^= mask
		le.PutUint64(data[0:], w)

		bm := New(data, totalPages, reserved)
		start, ok := bm.FindContiguous(int(n))
		if !ok {
			return
		}

		// Verify: all n pages starting at start were free before allocation.
		// After allocation they should be in pendingAllocs.
		for i := uint64(0); i < uint64(n); i++ {
			pid := start + i
			if pid >= totalPages {
				t.Fatalf("allocated page %d >= totalPages", pid)
			}
			if pid < reserved {
				t.Fatalf("allocated reserved page %d", pid)
			}
			if _, ok := bm.PendingAllocs()[pid]; !ok {
				t.Fatalf("page %d not in PendingAllocs", pid)
			}
		}

		if bm.CountFree() != bm.FreeCount() {
			t.Errorf("CountFree=%d != FreeCount=%d", bm.CountFree(), bm.FreeCount())
		}
	})
}

func FuzzFindFirstFree(f *testing.F) {
	f.Add(uint64(0xFF00FF00FF00FF00))

	f.Fuzz(func(t *testing.T, pattern uint64) {
		const totalPages = 128 // 2 words
		const reserved = 4

		data := makeBitmapData(totalPages)
		// Write pattern into word 0 (pages 0-63).
		le.PutUint64(data[0:], pattern)
		// Clear reserved bits.
		mask := uint64((1 << reserved) - 1)
		w := le.Uint64(data[0:])
		w &^= mask
		le.PutUint64(data[0:], w)

		bm := New(data, totalPages, reserved)

		for {
			pageID, ok := bm.FindFirstFree()
			if !ok {
				break
			}
			if pageID < reserved || pageID >= totalPages {
				t.Fatalf("allocated reserved/OOB page %d", pageID)
			}
		}

		if bm.FreeCount() != 0 {
			t.Errorf("FreeCount() = %d after exhaustion, want 0", bm.FreeCount())
		}
		if bm.CountFree() != 0 {
			t.Errorf("CountFree() = %d after exhaustion, want 0", bm.CountFree())
		}
	})
}
