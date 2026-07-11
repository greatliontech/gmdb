package typed

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/thegrumpylion/gmdb"
)

// firstLetterIK extracts the first byte of the value as a string IK
// (empty value → no entry, exercising partial-index skip).
func firstLetterIK(_ uint64, v string) []string {
	if v == "" {
		return nil
	}
	return []string{v[:1]}
}

// wholeValIK extracts the whole value as a string IK (for unique tests).
func wholeValIK(_ uint64, v string) []string { return []string{v} }

func TestIndexLookupNonUnique(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewKeyspace[uint64, string]("users", Uint64Encoder{}, StringEncoder{})
	byFirst := &Index[uint64, string, string]{
		Name:    "by_first",
		IKEnc:   StringEncoder{},
		Extract: firstLetterIK,
	}
	ks, err := tks.Create(tx, byFirst)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rows := map[uint64]string{1: "alice", 2: "amy", 3: "bob", 4: "anna"}
	for id, name := range rows {
		if err := ks.Put(id, name); err != nil {
			t.Fatalf("Put(%d): %v", id, err)
		}
	}

	h, err := ks.Index("by_first")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	q := NewIndexQuery[uint64, string, string](h, StringEncoder{})

	// Lookup "a" → ids 1,2,4 (alice, amy, anna), each with its name.
	got := map[uint64]string{}
	for id, name := range q.Lookup("a") {
		got[id] = name
	}
	if err := q.Err(); err != nil {
		t.Fatalf("Lookup Err: %v", err)
	}
	want := map[uint64]string{1: "alice", 2: "amy", 4: "anna"}
	if len(got) != len(want) {
		t.Fatalf("Lookup(a) = %v, want %v", got, want)
	}
	for id, name := range want {
		if got[id] != name {
			t.Errorf("Lookup(a)[%d] = %q, want %q", id, got[id], name)
		}
	}

	// LookupKeys "b" → just id 3.
	var keys []uint64
	for id := range q.LookupKeys("b") {
		keys = append(keys, id)
	}
	if err := q.Err(); err != nil {
		t.Fatalf("LookupKeys Err: %v", err)
	}
	if len(keys) != 1 || keys[0] != 3 {
		t.Errorf("LookupKeys(b) = %v, want [3]", keys)
	}

	// Get on a non-unique index → gmdb.ErrIndexNotUnique.
	if _, _, err := q.Get("a"); !errors.Is(err, gmdb.ErrIndexNotUnique) {
		t.Errorf("Get on non-unique index = %v, want gmdb.ErrIndexNotUnique", err)
	}
}

// TestIndexEncodeErrorPanics locks in the deliberate panic on an
// IK-encode failure (the byte gmdb.IndexExtractor is infallible; silently
// dropping the entry would diverge the index from the rows). The only
// reachable trigger with a canonical encoder is an out-of-range
// TimeEncoder index key.
func TestIndexEncodeErrorPanics(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewKeyspace[uint64, string]("events", Uint64Encoder{}, StringEncoder{})
	byTime := &Index[uint64, string, time.Time]{
		Name:  "by_time",
		IKEnc: TimeEncoder{},
		// Yields an out-of-range time → IKEnc.AppendEncode fails.
		Extract: func(_ uint64, _ string) []time.Time {
			return []time.Time{time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)}
		},
	}
	ks, err := tks.Create(tx, byTime)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("Put did not panic on out-of-range IK encode")
			}
		}()
		_ = ks.Put(1, "x")
	}()
}

func TestIndexUniqueGet(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewKeyspace[uint64, string]("users", Uint64Encoder{}, StringEncoder{})
	byName := &Index[uint64, string, string]{
		Name:    "by_name",
		IKEnc:   StringEncoder{},
		Unique:  true,
		Extract: wholeValIK,
	}
	ks, err := tks.Create(tx, byName)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := ks.Put(1, "alice"); err != nil {
		t.Fatalf("Put(1): %v", err)
	}
	if err := ks.Put(2, "bob"); err != nil {
		t.Fatalf("Put(2): %v", err)
	}

	h, _ := ks.Index("by_name")
	q := NewIndexQuery[uint64, string, string](h, StringEncoder{})
	id, name, err := q.Get("alice")
	if err != nil || id != 1 || name != "alice" {
		t.Errorf("Get(alice) = (%d, %q, %v), want (1, alice, nil)", id, name, err)
	}
	if _, _, err := q.Get("nobody"); !errors.Is(err, gmdb.ErrNotFound) {
		t.Errorf("Get(nobody) = %v, want gmdb.ErrNotFound", err)
	}

	// A duplicate unique key is rejected by the byte layer.
	if err := ks.Put(3, "alice"); !errors.Is(err, gmdb.ErrIndexUniqueViolation) {
		t.Errorf("Put(3, alice) = %v, want gmdb.ErrIndexUniqueViolation", err)
	}
}

// TestIndexEncoderIDDrift is the load-bearing Inv-T7 check: the IK
// encoder's ID() is folded into the schema-hash fingerprint, so opening
// with a different-ID encoder for the same column triggers
// gmdb.ErrIndexFingerprintMismatch.
func TestIndexEncoderIDDrift(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db := openWith(t, ctx, path, gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()
	tks := NewKeyspace[uint64, string]("users", Uint64Encoder{}, StringEncoder{})

	// Create with the canonical StringEncoder (ID "gmdb/string").
	tx1, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin1: %v", err)
	}
	idxA := &Index[uint64, string, string]{Name: "by_name", IKEnc: StringEncoder{}, Extract: wholeValIK}
	if _, err := tks.Create(tx1, idxA); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatalf("Commit1: %v", err)
	}

	// Reopen with a different-ID encoder for the same string IK column.
	altEnc := FuncEncoder[string]{
		EncodeFunc: func(dst []byte, v string) ([]byte, error) { return append(dst, v...), nil },
		DecodeFunc: func(src []byte) (string, error) { return string(src), nil },
		EncoderID:  "myapp/str-v2",
	}
	tx2, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin2: %v", err)
	}
	defer tx2.Rollback()
	idxB := &Index[uint64, string, string]{Name: "by_name", IKEnc: altEnc, Extract: wholeValIK}
	_, err = tks.Open(tx2, idxB)
	if !errors.Is(err, gmdb.ErrIndexFingerprintMismatch) {
		t.Fatalf("Open with drifted encoder = %v, want gmdb.ErrIndexFingerprintMismatch", err)
	}
}

// TestIndexEmptyEncoderID verifies Inv-T3: an empty IKEnc ID() is
// rejected (gmdb.ErrIndexEncoderIDEmpty) when the encoder is referenced by a
// typed index, but an indexless typed keyspace with an empty-ID encoder
// opens fine.
func TestIndexEmptyEncoderID(t *testing.T) {
	emptyID := FuncEncoder[string]{
		EncodeFunc: func(dst []byte, v string) ([]byte, error) { return append(dst, v...), nil },
		DecodeFunc: func(src []byte) (string, error) { return string(src), nil },
		EncoderID:  "", // empty
	}

	// (a) Referenced by a typed index → gmdb.ErrIndexEncoderIDEmpty.
	tx, cleanup := newTypedTx(t)
	defer cleanup()
	tks := NewKeyspace[uint64, string]("users", Uint64Encoder{}, StringEncoder{})
	badIdx := &Index[uint64, string, string]{Name: "by_name", IKEnc: emptyID, Extract: wholeValIK}
	if _, err := tks.Create(tx, badIdx); !errors.Is(err, gmdb.ErrIndexEncoderIDEmpty) {
		t.Errorf("Create with empty-ID index encoder = %v, want gmdb.ErrIndexEncoderIDEmpty", err)
	}

	// (b) Indexless typed keyspace with empty-ID encoders → fine.
	tx2, cleanup2 := newTypedTx(t)
	defer cleanup2()
	indexless := NewKeyspace[string, string]("plain", emptyID, emptyID)
	ks, err := indexless.Create(tx2)
	if err != nil {
		t.Fatalf("Create indexless with empty-ID encoders: %v", err)
	}
	if err := ks.Put("k", "v"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, err := ks.Get("k"); err != nil || got != "v" {
		t.Errorf("Get = (%q, %v), want (v, nil)", got, err)
	}
}

// TestIndexRange exercises a numeric IK with Range over the typed
// index.
func TestIndexRange(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewKeyspace[uint64, string]("users", Uint64Encoder{}, StringEncoder{})
	byLen := &Index[uint64, string, int64]{
		Name:    "by_len",
		IKEnc:   Int64Encoder{},
		Extract: func(_ uint64, v string) []int64 { return []int64{int64(len(v))} },
	}
	ks, err := tks.Create(tx, byLen)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// names of length 1..5.
	for id, name := range map[uint64]string{1: "a", 2: "bb", 3: "ccc", 4: "dddd", 5: "eeeee"} {
		if err := ks.Put(id, name); err != nil {
			t.Fatalf("Put(%d): %v", id, err)
		}
	}
	h, _ := ks.Index("by_len")
	q := NewIndexQuery[uint64, string, int64](h, Int64Encoder{})

	// Range [2, 4) → lengths 2,3 → ids 2,3.
	var got []uint64
	for id := range q.Lookup(3) { // exact first
		got = append(got, id)
	}
	if len(got) != 1 || got[0] != 3 {
		t.Errorf("Lookup(3) = %v, want [3]", got)
	}
	got = nil
	lo, hi := int64(2), int64(4)
	for id := range q.Range(&lo, &hi) {
		got = append(got, id)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Errorf("Range(2,4) = %v, want [2 3]", got)
	}
}

// TestIndexQueryBindMismatch verifies a NewIndexQuery bound
// with the wrong K/V type parameters is permanently inert and reports
// the mismatch via Err().
func TestIndexQueryBindMismatch(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewKeyspace[uint64, string]("users", Uint64Encoder{}, StringEncoder{})
	idx := &Index[uint64, string, string]{Name: "by_name", IKEnc: StringEncoder{}, Extract: wholeValIK}
	ks, err := tks.Create(tx, idx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := ks.Put(1, "alice"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h, _ := ks.Index("by_name")
	// Wrong K type (string instead of uint64).
	q := NewIndexQuery[string, string, string](h, StringEncoder{})
	n := 0
	for range q.Lookup("alice") {
		n++
	}
	if n != 0 {
		t.Errorf("inert query yielded %d, want 0", n)
	}
	if !errors.Is(q.Err(), gmdb.ErrInvalidOptions) {
		t.Errorf("bind-mismatch Err() = %v, want gmdb.ErrInvalidOptions", q.Err())
	}
}

// TestTypedSetIndexLookup verifies a typed index on a SetKeyspace yields
// (setKey, setValue) pairs.
func TestTypedSetIndexLookup(t *testing.T) {
	tx, cleanup := newTypedSetTx(t)
	defer cleanup()

	tsk := NewSetKeyspace[string, string]("groups", StringEncoder{}, StringEncoder{}, nil)
	byFirst := &Index[string, string, string]{
		Name:    "by_member_first",
		IKEnc:   StringEncoder{},
		Extract: func(_ string, v string) []string { return []string{v[:1]} },
	}
	ks, err := tsk.Create(tx, byFirst)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, m := range []struct{ g, member string }{
		{"admins", "alice"}, {"admins", "amy"}, {"admins", "bob"}, {"users", "anna"},
	} {
		if _, err := ks.Put(m.g, m.member); err != nil {
			t.Fatalf("Put(%s,%s): %v", m.g, m.member, err)
		}
	}
	h, err := ks.Index("by_member_first")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	q := NewIndexQuery[string, string, string](h, StringEncoder{})
	// Lookup "a" → (admins,alice),(admins,amy),(users,anna).
	var got []string
	for g, member := range q.Lookup("a") {
		got = append(got, g+"/"+member)
	}
	if err := q.Err(); err != nil {
		t.Fatalf("Lookup Err: %v", err)
	}
	sort.Strings(got)
	want := []string{"admins/alice", "admins/amy", "users/anna"}
	if !equalStr(got, want) {
		t.Errorf("set index Lookup(a) = %v, want %v", got, want)
	}

	// LookupKeys on a SetKeyspace index has no single-key form (the PK is
	// the compound (setKey,setValue) pair) → gmdb.ErrInvalidOptions via Err().
	n := 0
	for range q.LookupKeys("a") {
		n++
	}
	if n != 0 {
		t.Errorf("set index LookupKeys yielded %d, want 0", n)
	}
	if !errors.Is(q.Err(), gmdb.ErrInvalidOptions) {
		t.Errorf("set index LookupKeys Err() = %v, want gmdb.ErrInvalidOptions", q.Err())
	}
}
