package typed

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thegrumpylion/gmdb"
)

// TestIndexCoverValueRoundTrip verifies a full-row-covering typed
// index returns the correct (K,V) via Lookup/Get, that the byte-layer
// covering-return is actually enabled (white-box coverValue flag), and
// that a non-covering index on the same data yields identical results
// with the flag off (so covering is a transparent optimization).
func TestIndexCoverValueRoundTrip(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewKeyspace[uint64, string]("users", Uint64Encoder{}, StringEncoder{})
	covering := &Index[uint64, string, string]{
		Name: "by_first_cov", IKEnc: StringEncoder{}, Extract: firstLetterIK, CoverValue: true,
	}
	plain := &Index[uint64, string, string]{
		Name: "by_first_plain", IKEnc: StringEncoder{}, Extract: firstLetterIK,
	}
	ks, err := tks.Create(tx, covering, plain)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rows := map[uint64]string{1: "alice", 2: "amy", 3: "bob"}
	for id, name := range rows {
		if err := ks.Put(id, name); err != nil {
			t.Fatalf("Put(%d): %v", id, err)
		}
	}

	covH, _ := ks.Index("by_first_cov")
	if !covH.idx.CoverValueReturnEnabled() {
		t.Error("covering index: coverValue flag not enabled (back-lookup would be used)")
	}
	plainH, _ := ks.Index("by_first_plain")
	if plainH.idx.CoverValueReturnEnabled() {
		t.Error("plain index: coverValue flag unexpectedly enabled")
	}

	covQ := NewIndexQuery[uint64, string, string](covH, StringEncoder{})
	plainQ := NewIndexQuery[uint64, string, string](plainH, StringEncoder{})

	// Both indexes must return identical (id, name) for "a".
	collect := func(q *IndexQuery[uint64, string, string]) map[uint64]string {
		out := map[uint64]string{}
		for id, name := range q.Lookup("a") {
			out[id] = name
		}
		if err := q.Err(); err != nil {
			t.Fatalf("Lookup Err: %v", err)
		}
		return out
	}
	cov := collect(covQ)
	pln := collect(plainQ)
	want := map[uint64]string{1: "alice", 2: "amy"}
	for _, got := range []map[uint64]string{cov, pln} {
		if len(got) != len(want) {
			t.Fatalf("Lookup(a) = %v, want %v", got, want)
		}
		for id, name := range want {
			if got[id] != name {
				t.Errorf("Lookup(a)[%d] = %q, want %q", id, got[id], name)
			}
		}
	}
}

func TestIndexCoverValueUniqueGet(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewKeyspace[uint64, string]("users", Uint64Encoder{}, StringEncoder{})
	idx := &Index[uint64, string, string]{
		Name: "by_name", IKEnc: StringEncoder{}, Unique: true, Extract: wholeValIK, CoverValue: true,
	}
	ks, err := tks.Create(tx, idx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := ks.Put(7, "alice"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h, _ := ks.Index("by_name")
	if !h.idx.CoverValueReturnEnabled() {
		t.Error("covering unique index: coverValue not enabled")
	}
	q := NewIndexQuery[uint64, string, string](h, StringEncoder{})
	id, name, err := q.Get("alice")
	if err != nil || id != 7 || name != "alice" {
		t.Errorf("Get(alice) = (%d, %q, %v), want (7, alice, nil)", id, name, err)
	}
}

// TestIndexCoverValueLargeValue covers a value far larger than an
// inline leaf entry: the covering blob carries the whole encoded value,
// which must reassemble byte-identically through the covering-return.
func TestIndexCoverValueLargeValue(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewKeyspace[uint64, string]("docs", Uint64Encoder{}, StringEncoder{})
	idx := &Index[uint64, string, string]{
		Name: "by_first", IKEnc: StringEncoder{}, CoverValue: true,
		Extract: func(_ uint64, v string) []string {
			if v == "" {
				return nil
			}
			return []string{v[:1]}
		},
	}
	ks, err := tks.Create(tx, idx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	big := "x" + strings.Repeat("payload-", 5000) // ~40 KB, well past a leaf page
	if err := ks.Put(1, big); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h, _ := ks.Index("by_first")
	q := NewIndexQuery[uint64, string, string](h, StringEncoder{})
	var got string
	var count int
	for id, v := range q.Lookup("x") {
		count++
		if id != 1 {
			t.Errorf("Lookup id = %d, want 1", id)
		}
		got = v
	}
	if err := q.Err(); err != nil {
		t.Fatalf("Lookup Err: %v", err)
	}
	if count != 1 || got != big {
		t.Errorf("covering Lookup returned %d rows, value len %d, want 1 row of len %d", count, len(got), len(big))
	}
}

// TestIndexCoverValueNULBytes stresses the covering codec: the
// covering blob is a NUL-escaped tuple, so values containing 0x00,
// terminator-looking (0x00 0x00) and escape-looking (0x00 0xFF) byte
// sequences — plus an empty value — must round-trip byte-identically
// through the covering-return.
func TestIndexCoverValueNULBytes(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewKeyspace[uint64, []byte]("blobs", Uint64Encoder{}, BytesEncoder{})
	idx := &Index[uint64, []byte, string]{
		Name: "all", IKEnc: StringEncoder{}, CoverValue: true,
		// Constant IK so Lookup("k") returns every row (non-unique).
		Extract: func(_ uint64, _ []byte) []string { return []string{"k"} },
	}
	ks, err := tks.Create(tx, idx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	vals := map[uint64][]byte{
		1: {0x00},
		2: {0x00, 0x00},
		3: {0x00, 0xFF},
		4: {0xFF, 0x00, 0x00, 0xFF},
		5: {},
		6: {'a', 0x00, 'b'},
	}
	for id, v := range vals {
		if err := ks.Put(id, v); err != nil {
			t.Fatalf("Put(%d): %v", id, err)
		}
	}
	h, _ := ks.Index("all")
	if !h.idx.CoverValueReturnEnabled() {
		t.Fatal("coverValue not enabled")
	}
	q := NewIndexQuery[uint64, []byte, string](h, StringEncoder{})
	got := map[uint64]string{}
	for id, v := range q.Lookup("k") {
		got[id] = string(v) // string() preserves arbitrary bytes for comparison
	}
	if err := q.Err(); err != nil {
		t.Fatalf("Lookup Err: %v", err)
	}
	if len(got) != len(vals) {
		t.Fatalf("covering Lookup returned %d rows, want %d", len(got), len(vals))
	}
	for id, want := range vals {
		if got[id] != string(want) {
			t.Errorf("covering value[%d] = %x, want %x (NUL-escape round-trip)", id, got[id], want)
		}
	}
}

// TestIndexCoverValueDrift verifies the value-encoder ID is folded
// into the covering index's fingerprint: reopening with a different-ID
// value encoder triggers gmdb.ErrIndexFingerprintMismatch.
func TestIndexCoverValueDrift(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db := openWith(t, ctx, path, gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()

	mkIndex := func() *Index[uint64, string, string] {
		return &Index[uint64, string, string]{Name: "by_name", IKEnc: StringEncoder{}, Unique: true, Extract: wholeValIK, CoverValue: true}
	}

	tx1, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin1: %v", err)
	}
	tksA := NewKeyspace[uint64, string]("users", Uint64Encoder{}, StringEncoder{})
	if _, err := tksA.Create(tx1, mkIndex()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatalf("Commit1: %v", err)
	}

	// Reopen with a different-ID VALUE encoder (same string type).
	altVal := FuncEncoder[string]{
		EncodeFunc: func(dst []byte, v string) ([]byte, error) { return append(dst, v...), nil },
		DecodeFunc: func(src []byte) (string, error) { return string(src), nil },
		EncoderID:  "myapp/str-v2",
	}
	tx2, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin2: %v", err)
	}
	defer tx2.Rollback()
	tksB := NewKeyspace[uint64, string]("users", Uint64Encoder{}, altVal)
	if _, err := tksB.Open(tx2, mkIndex()); !errors.Is(err, gmdb.ErrIndexFingerprintMismatch) {
		t.Fatalf("Open with drifted value encoder = %v, want gmdb.ErrIndexFingerprintMismatch", err)
	}
}

// TestIndexCoverValueEmptyValEncID verifies CoverValue with an
// empty-ID value encoder is rejected (the value encoder is referenced by
// the covering fingerprint).
func TestIndexCoverValueEmptyValEncID(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	emptyVal := FuncEncoder[string]{
		EncodeFunc: func(dst []byte, v string) ([]byte, error) { return append(dst, v...), nil },
		DecodeFunc: func(src []byte) (string, error) { return string(src), nil },
		EncoderID:  "",
	}
	tks := NewKeyspace[uint64, string]("users", Uint64Encoder{}, emptyVal)
	idx := &Index[uint64, string, string]{Name: "by_name", IKEnc: StringEncoder{}, Extract: wholeValIK, CoverValue: true}
	if _, err := tks.Create(tx, idx); !errors.Is(err, gmdb.ErrIndexEncoderIDEmpty) {
		t.Errorf("Create CoverValue with empty value-encoder ID = %v, want gmdb.ErrIndexEncoderIDEmpty", err)
	}
}
