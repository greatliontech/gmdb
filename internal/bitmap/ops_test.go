package bitmap

import "testing"

func TestSetClear(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10)

	b.Set(50)
	if !b.IsSet(50) {
		t.Error("page 50 should be free after Set")
	}
	if b.FreeCount() != 1 {
		t.Errorf("FreeCount() = %d, want 1", b.FreeCount())
	}
	if _, ok := b.PendingFrees()[50]; !ok {
		t.Error("page 50 should be in PendingFrees")
	}

	b.Clear(50)
	if b.IsSet(50) {
		t.Error("page 50 should be allocated after Clear")
	}
	if b.FreeCount() != 0 {
		t.Errorf("FreeCount() = %d, want 0", b.FreeCount())
	}
	if _, ok := b.PendingAllocs()[50]; !ok {
		t.Error("page 50 should be in PendingAllocs")
	}
	// Should no longer be in PendingFrees.
	if _, ok := b.PendingFrees()[50]; ok {
		t.Error("page 50 should not be in PendingFrees after Clear")
	}
}

func TestSetIdempotent(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10)

	b.Set(50)
	b.Set(50) // second Set should be no-op
	if b.FreeCount() != 1 {
		t.Errorf("FreeCount() = %d, want 1 (idempotent Set)", b.FreeCount())
	}
}

func TestClearIdempotent(t *testing.T) {
	data := makeBitmapData(256)
	setBitInData(data, 50)
	b := New(data, 256, 10)

	b.Clear(50)
	b.Clear(50) // second Clear should be no-op
	if b.FreeCount() != 0 {
		t.Errorf("FreeCount() = %d, want 0 (idempotent Clear)", b.FreeCount())
	}
}

func TestClearThenSet(t *testing.T) {
	// Simulates the loose page pattern: allocate then free in same tx.
	data := makeBitmapData(256)
	setBitInData(data, 50) // free on disk
	b := New(data, 256, 10)

	b.Clear(50) // allocate
	if b.FreeCount() != 0 {
		t.Errorf("after Clear: FreeCount() = %d, want 0", b.FreeCount())
	}

	b.Set(50) // free again (loose page)
	if b.FreeCount() != 1 {
		t.Errorf("after Set: FreeCount() = %d, want 1", b.FreeCount())
	}
	if !b.IsSet(50) {
		t.Error("page 50 should be free after Clear then Set")
	}
}

func TestSetThenClear(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10)

	b.Set(50) // free
	b.Clear(50) // allocate
	if b.FreeCount() != 0 {
		t.Errorf("FreeCount() = %d, want 0", b.FreeCount())
	}
	if b.IsSet(50) {
		t.Error("page 50 should be allocated after Set then Clear")
	}
}

func TestSetPanicsOnReserved(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10)

	defer func() {
		if r := recover(); r == nil {
			t.Error("Set(5) should panic for reserved page")
		}
	}()
	b.Set(5) // reserved page
}

func TestClearPanicsOnReserved(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10)

	defer func() {
		if r := recover(); r == nil {
			t.Error("Clear(0) should panic for reserved page")
		}
	}()
	b.Clear(0)
}

func TestSetPanicsOnOutOfRange(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10)

	defer func() {
		if r := recover(); r == nil {
			t.Error("Set(256) should panic for out-of-range page")
		}
	}()
	b.Set(256)
}

func TestCountFreeMatchesFreeCount(t *testing.T) {
	data := makeBitmapData(256)
	for i := uint64(10); i < 50; i++ {
		setBitInData(data, i)
	}
	b := New(data, 256, 10)

	// Perform some Set/Clear operations.
	b.Clear(20) // allocate
	b.Clear(30) // allocate
	b.Set(60)   // free new page

	if b.CountFree() != b.FreeCount() {
		t.Errorf("CountFree() = %d, FreeCount() = %d, mismatch",
			b.CountFree(), b.FreeCount())
	}
}

func TestSetClearWordBoundary(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10)

	// Page 64 = first bit of word 1.
	b.Set(64)
	if !b.IsSet(64) {
		t.Error("page 64 not free")
	}
	b.Clear(64)
	if b.IsSet(64) {
		t.Error("page 64 should be allocated")
	}

	// Page 127 = last bit of word 1.
	b.Set(127)
	if !b.IsSet(127) {
		t.Error("page 127 not free")
	}
	if b.FreeCount() != 1 {
		t.Errorf("FreeCount = %d, want 1", b.FreeCount())
	}
}

func TestSetClearFirstAndLastValid(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10)

	// First valid page.
	b.Set(10)
	if !b.IsSet(10) {
		t.Error("first valid page 10 not free")
	}

	// Last valid page.
	b.Set(255)
	if !b.IsSet(255) {
		t.Error("last valid page 255 not free")
	}
	if b.FreeCount() != 2 {
		t.Errorf("FreeCount = %d, want 2", b.FreeCount())
	}
	if b.CountFree() != 2 {
		t.Errorf("CountFree = %d, want 2", b.CountFree())
	}
}

func TestExactlyOneWord(t *testing.T) {
	data := makeBitmapData(64)
	b := New(data, 64, 4)

	b.Set(4) // first valid
	b.Set(63) // last valid
	if b.FreeCount() != 2 {
		t.Errorf("FreeCount = %d, want 2", b.FreeCount())
	}

	p, ok := b.FindFirstFree()
	if !ok || p != 4 {
		t.Errorf("FindFirstFree = (%d, %v), want (4, true)", p, ok)
	}
	p, ok = b.FindFirstFree()
	if !ok || p != 63 {
		t.Errorf("FindFirstFree = (%d, %v), want (63, true)", p, ok)
	}
	_, ok = b.FindFirstFree()
	if ok {
		t.Error("should be exhausted")
	}
}

func TestReservedSpansFullWord(t *testing.T) {
	// 256 pages, 64 reserved = entire word 0 is reserved.
	data := makeBitmapData(256)
	// Set all bits in word 0 (all reserved, should be masked).
	for i := range 8 {
		data[i] = 0xFF
	}
	setBitInData(data, 100) // valid free page
	b := New(data, 256, 64)

	// Only page 100 should count.
	if b.FreeCount() != 1 {
		t.Errorf("FreeCount = %d, want 1", b.FreeCount())
	}
	p, ok := b.FindFirstFree()
	if !ok || p != 100 {
		t.Errorf("FindFirstFree = (%d, %v), want (100, true)", p, ok)
	}
}

func TestSummaryUpdatedOnSetClear(t *testing.T) {
	data := makeBitmapData(256)
	b := New(data, 256, 10)

	// Word 0 has no free pages. Summary bit 0 should be clear.
	if b.summary[0]&1 != 0 {
		t.Error("summary bit 0 should be clear initially")
	}

	// Set page 10 (in word 0). Summary bit 0 should now be set.
	b.Set(10)
	if b.summary[0]&1 == 0 {
		t.Error("summary bit 0 should be set after freeing page 10")
	}

	// Clear page 10. Summary bit 0 should be clear again.
	b.Clear(10)
	if b.summary[0]&1 != 0 {
		t.Error("summary bit 0 should be clear after allocating page 10")
	}
}
