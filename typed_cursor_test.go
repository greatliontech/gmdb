package gmdb

import (
	"context"
	"errors"
	"testing"
)

// newTypedNumsKS creates a TypedKeyspace[uint64,string] populated with
// keys 1..n (value "v<k>") and returns the handle + cleanup.
func newTypedNumsKS(t *testing.T, n uint64) (*TypedKeyspaceHandle[uint64, string], func()) {
	t.Helper()
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	tx, err := db.Begin(ctx)
	if err != nil {
		_ = db.Close()
		t.Fatalf("Begin: %v", err)
	}
	tks := NewTypedKeyspace[uint64, string]("nums", BEUint64Encoder{}, StringEncoder{})
	ks, err := tks.Create(tx)
	if err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		t.Fatalf("Create: %v", err)
	}
	for i := uint64(1); i <= n; i++ {
		if err := ks.Put(i, "v"); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
	return ks, func() {
		_ = tx.Rollback()
		_ = db.Close()
	}
}

func TestTypedCursorForwardBackward(t *testing.T) {
	ks, cleanup := newTypedNumsKS(t, 5)
	defer cleanup()

	c := ks.Cursor()
	var fwd []uint64
	for k, _, ok := c.First(); ok; k, _, ok = c.Next() {
		fwd = append(fwd, k)
	}
	if err := c.Err(); err != nil {
		t.Fatalf("forward Err: %v", err)
	}
	if want := []uint64{1, 2, 3, 4, 5}; !equalU64(fwd, want) {
		t.Errorf("forward = %v, want %v", fwd, want)
	}

	c2 := ks.Cursor()
	var bwd []uint64
	for k, _, ok := c2.Last(); ok; k, _, ok = c2.Prev() {
		bwd = append(bwd, k)
	}
	if want := []uint64{5, 4, 3, 2, 1}; !equalU64(bwd, want) {
		t.Errorf("backward = %v, want %v", bwd, want)
	}
}

func TestTypedCursorSeek(t *testing.T) {
	ks, cleanup := newTypedNumsKS(t, 10)
	defer cleanup()

	c := ks.Cursor()
	// SeekGE to an existing key.
	if k, _, ok := c.SeekGE(7); !ok || k != 7 {
		t.Errorf("SeekGE(7) = (%d, %v), want (7, true)", k, ok)
	}
	// SeekGE past gaps: delete 5, SeekGE(5) lands on 6.
	if err := ks.Delete(5); err != nil {
		t.Fatalf("Delete(5): %v", err)
	}
	c2 := ks.Cursor()
	if k, _, ok := c2.SeekGE(5); !ok || k != 6 {
		t.Errorf("SeekGE(5) after deleting 5 = (%d, %v), want (6, true)", k, ok)
	}
}

func TestTypedCursorDelete(t *testing.T) {
	ks, cleanup := newTypedNumsKS(t, 5)
	defer cleanup()

	c := ks.Cursor()
	// Position at 3 and delete it.
	if _, _, ok := c.SeekGE(3); !ok {
		t.Fatal("SeekGE(3) not ok")
	}
	if err := c.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := ks.Get(3); err == nil {
		t.Error("Get(3) after cursor Delete = nil err, want ErrNotFound")
	}
	// Remaining keys via All.
	var got []uint64
	for k := range ks.All() {
		got = append(got, k)
	}
	if want := []uint64{1, 2, 4, 5}; !equalU64(got, want) {
		t.Errorf("after cursor delete, All = %v, want %v", got, want)
	}
}

func TestTypedKSAllRangePrefix(t *testing.T) {
	ks, cleanup := newTypedNumsKS(t, 6)
	defer cleanup()

	// All.
	var all []uint64
	for k := range ks.All() {
		all = append(all, k)
	}
	if want := []uint64{1, 2, 3, 4, 5, 6}; !equalU64(all, want) {
		t.Errorf("All = %v, want %v", all, want)
	}

	// Range [2, 5) = {2,3,4}.
	var rng []uint64
	for k := range ks.Range(ptr(uint64(2)), ptr(uint64(5))) {
		rng = append(rng, k)
	}
	if want := []uint64{2, 3, 4}; !equalU64(rng, want) {
		t.Errorf("Range(2,5) = %v, want %v", rng, want)
	}

	// Open-ended range: from 4 onward.
	var tail []uint64
	for k := range ks.Range(ptr(uint64(4)), nil) {
		tail = append(tail, k)
	}
	if want := []uint64{4, 5, 6}; !equalU64(tail, want) {
		t.Errorf("Range(4,nil) = %v, want %v", tail, want)
	}

	// Early break stops iteration.
	count := 0
	for range ks.All() {
		count++
		break
	}
	if count != 1 {
		t.Errorf("early-break All visited %d, want 1", count)
	}
}

// TestTypedKSPrefixString exercises true variable-width prefix matching.
func TestTypedKSPrefixString(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	tks := NewTypedKeyspace[string, string]("words", StringEncoder{}, StringEncoder{})
	ks, err := tks.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, w := range []string{"ap", "app", "apple", "banana", "az"} {
		if err := ks.Put(w, "v"); err != nil {
			t.Fatalf("Put(%q): %v", w, err)
		}
	}
	var got []string
	for k := range ks.Prefix("app") {
		got = append(got, k)
	}
	if want := []string{"app", "apple"}; !equalStr(got, want) {
		t.Errorf("Prefix(app) = %v, want %v", got, want)
	}
}

// TestTypedCursorSignedOrder is the end-to-end Inv-T1 integration check:
// int64 keys with BEInt64Encoder must iterate in numeric order
// (negatives below positives) through the cursor — not raw
// two's-complement byte order.
func TestTypedCursorSignedOrder(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	tks := NewTypedKeyspace[int64, string]("signed", BEInt64Encoder{}, StringEncoder{})
	ks, err := tks.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Insert out of order.
	for _, k := range []int64{5, -3, 0, -100, 100, -1, 1} {
		if err := ks.Put(k, "v"); err != nil {
			t.Fatalf("Put(%d): %v", k, err)
		}
	}
	var got []int64
	c := ks.Cursor()
	for k, _, ok := c.First(); ok; k, _, ok = c.Next() {
		got = append(got, k)
	}
	if err := c.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	want := []int64{-100, -3, -1, 0, 1, 5, 100}
	if len(got) != len(want) {
		t.Fatalf("cursor yielded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("signed cursor order = %v, want %v", got, want)
		}
	}
	// Range over negatives only: [-100, 0) = {-100, -3, -1}.
	var negs []int64
	for k := range ks.Range(ptr(int64(-100)), ptr(int64(0))) {
		negs = append(negs, k)
	}
	if want := []int64{-100, -3, -1}; len(negs) != len(want) || negs[0] != -100 || negs[2] != -1 {
		t.Errorf("Range(-100,0) = %v, want %v", negs, want)
	}
}

// TestTypedCursorDecodeError verifies a value-decode failure during
// iteration is sticky and surfaces via TypedCursor.Err(), and that the
// convenience iterators end (yield nothing) on the same error. Uses an
// asymmetric value encoder whose Decode always fails (Encode succeeds,
// so Put writes a well-formed byte value the decoder then rejects).
func TestTypedCursorDecodeError(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	failDec := FuncEncoder[string]{
		EncodeFunc: func(dst []byte, v string) ([]byte, error) { return append(dst, v...), nil },
		DecodeFunc: func(src []byte) (string, error) { return "", errFailDecode },
		EncoderID:  "test/fail-decode",
	}
	tks := NewTypedKeyspace[string, string]("bad", StringEncoder{}, failDec)
	ks, err := tks.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := ks.Put("k", "v"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Cursor: First() returns ok=false and Err() carries the decode error.
	c := ks.Cursor()
	if _, _, ok := c.First(); ok {
		t.Error("First() ok=true, want false (value decode fails)")
	}
	if !errors.Is(c.Err(), errFailDecode) {
		t.Errorf("Cursor.Err() = %v, want errFailDecode", c.Err())
	}

	// All() ends immediately (yields nothing) on the decode error.
	count := 0
	for range ks.All() {
		count++
	}
	if count != 0 {
		t.Errorf("All() yielded %d on decode error, want 0", count)
	}

	// Get surfaces the decode error directly.
	if _, err := ks.Get("k"); !errors.Is(err, errFailDecode) {
		t.Errorf("Get() = %v, want errFailDecode", err)
	}
}

var errFailDecode = errors.New("test: decode always fails")

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
