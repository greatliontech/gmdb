package gmdb

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// ptr returns a pointer to v (for the *K range-boundary arguments).
func ptr[T any](v T) *T { return &v }

// newTypedTx opens a fresh DB + write tx, returning the tx and a cleanup.
func newTypedTx(t *testing.T) (*Tx, func()) {
	t.Helper()
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	tx, err := db.Begin(ctx, true)
	if err != nil {
		_ = db.Close()
		t.Fatalf("Begin: %v", err)
	}
	return tx, func() {
		_ = tx.Rollback()
		_ = db.Close()
	}
}

func TestTypedKSRoundTrip(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewTypedKeyspace[uint64, string]("nums", BEUint64Encoder{}, StringEncoder{})
	ks, err := tks.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := ks.Put(1, "one"); err != nil {
		t.Fatalf("Put(1): %v", err)
	}
	if err := ks.Put(2, "two"); err != nil {
		t.Fatalf("Put(2): %v", err)
	}
	if got, err := ks.Get(1); err != nil || got != "one" {
		t.Errorf("Get(1) = (%q, %v), want (one, nil)", got, err)
	}
	if got, err := ks.Get(2); err != nil || got != "two" {
		t.Errorf("Get(2) = (%q, %v), want (two, nil)", got, err)
	}
	// Replace.
	if err := ks.Put(1, "uno"); err != nil {
		t.Fatalf("Put(1) replace: %v", err)
	}
	if got, _ := ks.Get(1); got != "uno" {
		t.Errorf("Get(1) after replace = %q, want uno", got)
	}
	// Miss.
	if _, err := ks.Get(99); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(99) = %v, want ErrNotFound", err)
	}
	// Delete + delete-miss.
	if err := ks.Delete(1); err != nil {
		t.Fatalf("Delete(1): %v", err)
	}
	if _, err := ks.Get(1); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(1) after delete = %v, want ErrNotFound", err)
	}
	if err := ks.Delete(1); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete(1) again = %v, want ErrNotFound", err)
	}
}

func TestTypedKSStringKeyBytesValue(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewTypedKeyspace[string, []byte]("blobs", StringEncoder{}, BytesEncoder{})
	ks, err := tks.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := []byte{0x00, 0xff, 0x01}
	if err := ks.Put("k", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := ks.Get("k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Get = %x, want %x", got, want)
	}
	// Empty value round-trips as empty (nil-value-as-empty).
	if err := ks.Put("e", nil); err != nil {
		t.Fatalf("Put empty: %v", err)
	}
	if got, err := ks.Get("e"); err != nil || len(got) != 0 {
		t.Errorf("Get(e) = (%x, %v), want (empty, nil)", got, err)
	}
}

// TestTypedKSDeleteRange exercises the encoded-key lex order end-to-end
// (Inv-T1): big-endian uint64 keys must range-delete by numeric order.
func TestTypedKSDeleteRange(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewTypedKeyspace[uint64, string]("nums", BEUint64Encoder{}, StringEncoder{})
	ks, err := tks.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := uint64(1); i <= 10; i++ {
		if err := ks.Put(i, "v"); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
	// Delete [3, 7) = {3,4,5,6}.
	n, err := ks.DeleteRange(ptr(uint64(3)), ptr(uint64(7)))
	if err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	if n != 4 {
		t.Errorf("DeleteRange deleted %d, want 4", n)
	}
	for _, k := range []uint64{3, 4, 5, 6} {
		if _, err := ks.Get(k); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%d) = %v, want ErrNotFound (deleted)", k, err)
		}
	}
	for _, k := range []uint64{1, 2, 7, 8, 9, 10} {
		if _, err := ks.Get(k); err != nil {
			t.Errorf("Get(%d) = %v, want present", k, err)
		}
	}
	// Open-ended: delete everything remaining.
	n, err = ks.DeleteRange(nil, nil)
	if err != nil {
		t.Fatalf("DeleteRange(nil,nil): %v", err)
	}
	if n != 6 {
		t.Errorf("DeleteRange(nil,nil) deleted %d, want 6", n)
	}
}

func TestTypedKSReadOnly(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db := openWith(t, ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()

	tks := NewTypedKeyspace[uint64, string]("nums", BEUint64Encoder{}, StringEncoder{})
	// Create + populate + commit.
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tks.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := ks.Put(7, "seven"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Reopen the keyspace read-only on a fresh write tx (OpenReadOnly
	// yields a handle that rejects mutations with ErrReadOnly).
	rtx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer rtx.Rollback()
	rks, err := tks.OpenReadOnly(rtx)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if got, err := rks.Get(7); err != nil || got != "seven" {
		t.Errorf("Get(7) = (%q, %v), want (seven, nil)", got, err)
	}
	if err := rks.Put(8, "eight"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Put on read-only = %v, want ErrReadOnly", err)
	}
}

// TestTypedKSEncoderError verifies a key/value encoder error propagates
// instead of writing partial state. BENanosEncoder rejects out-of-range
// times.
func TestTypedKSEncoderError(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewTypedKeyspace[time.Time, string]("events", BENanosEncoder{}, StringEncoder{})
	ks, err := tks.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	bad := time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := ks.Put(bad, "x"); err == nil {
		t.Error("Put(out-of-range time) = nil, want encoder error")
	}
	if _, err := ks.Get(bad); err == nil {
		t.Error("Get(out-of-range time) = nil, want encoder error")
	}
	// An in-range time works.
	ok := time.Date(2020, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := ks.Put(ok, "ok"); err != nil {
		t.Fatalf("Put(in-range): %v", err)
	}
	if got, err := ks.Get(ok); err != nil || got != "ok" {
		t.Errorf("Get(in-range) = (%q, %v), want (ok, nil)", got, err)
	}
}

// TestTypedKSOpenVariants checks Create / Open / CreateIfNotExists round
// out: Create then a separate-tx Open sees the data; CreateIfNotExists
// on an existing keyspace opens it.
func TestTypedKSOpenVariants(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db := openWith(t, ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()
	tks := NewTypedKeyspace[uint64, string]("nums", BEUint64Encoder{}, StringEncoder{})

	tx1, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin1: %v", err)
	}
	ks, err := tks.Create(tx1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := ks.Put(42, "answer"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatalf("Commit1: %v", err)
	}

	tx2, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin2: %v", err)
	}
	defer tx2.Rollback()
	// CreateIfNotExists on the existing keyspace opens it.
	ks2, err := tks.CreateIfNotExists(tx2)
	if err != nil {
		t.Fatalf("CreateIfNotExists: %v", err)
	}
	if got, err := ks2.Get(42); err != nil || got != "answer" {
		t.Errorf("Get(42) via CreateIfNotExists = (%q, %v), want (answer, nil)", got, err)
	}
	// Open also sees it (idempotent same-tx re-open).
	ks3, err := tks.Open(tx2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got, _ := ks3.Get(42); got != "answer" {
		t.Errorf("Get(42) via Open = %q, want answer", got)
	}
}
