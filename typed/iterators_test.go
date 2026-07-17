package typed

import (
	"errors"
	"testing"

	"github.com/greatliontech/gmdb"
)

// The typed layer's All/Range/Prefix run the construction guard
// EAGERLY: the panic fires at the typed call itself, never deferred
// to loop start (api-surface.md §Range Iterators).
func TestTypedIteratorConstructionPanicsEagerly(t *testing.T) {
	tks, ktx, cleanup := newTypedNumsKS(t, 3)
	defer cleanup()
	// Freeze the parent.
	child, err := ktx.BeginChild()
	if err != nil {
		t.Fatalf("BeginChild: %v", err)
	}
	defer child.Rollback()
	mustPanicHere := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: no panic at the typed constructor call", name)
			}
		}()
		fn()
	}
	// The constructor CALL panics — the returned seq is never ranged.
	mustPanicHere("typed All", func() { _ = tks.All() })
	mustPanicHere("typed Range", func() { _ = tks.Range(nil, nil) })
	mustPanicHere("typed Prefix", func() { _ = tks.Prefix(1) })
}

// failingEncoder always errors — the encode-failure branch of the
// typed constructors must still run the construction guard first
// (an unusable handle panics regardless of bound validity).
type failingEncoder struct{ Uint64Encoder }

func (failingEncoder) AppendEncode(dst []byte, v uint64) ([]byte, error) {
	return nil, errors.New("encode always fails")
}

func TestTypedIteratorEncodeFailureStillGuards(t *testing.T) {
	tks, ktx, cleanup := newTypedNumsKS(t, 1)
	defer cleanup()
	bad := &KeyspaceHandle[uint64, string]{
		ks:     tks.ks,
		keyEnc: failingEncoder{},
		valEnc: tks.valEnc,
	}
	child, err := ktx.BeginChild()
	if err != nil {
		t.Fatalf("BeginChild: %v", err)
	}
	defer child.Rollback()
	one := uint64(1)
	defer func() {
		if recover() == nil {
			t.Fatal("Range with failing encoder on a frozen handle: no panic")
		}
	}()
	bad.Range(&one, nil)
}

// poisonValEncoder decodes every value except `poison`, for pinning
// mid-sequence decode truncation.
type poisonValEncoder struct{ poison string }

func (poisonValEncoder) AppendEncode(dst []byte, v string) ([]byte, error) {
	return append(dst, v...), nil
}
func (e poisonValEncoder) Decode(src []byte) (string, error) {
	if string(src) == e.poison {
		return "", errors.New("poisoned value")
	}
	return string(src), nil
}
func (poisonValEncoder) ID() string { return "gmdb/string" }

// Post-iteration error surface on the typed handle
// (typed-keyspaces.md: mirrors the byte layer's §Range Iterators
// contract): a mid-sequence decode error truncates the sequence and
// surfaces on Err(); a clean end leaves Err() nil; the next sequence
// resets it.
func TestTypedIteratorErrReportsDecodeError(t *testing.T) {
	tks, _, cleanup := newTypedNumsKS(t, 0)
	defer cleanup()
	poisoned := &KeyspaceHandle[uint64, string]{
		ks:     tks.ks,
		keyEnc: tks.keyEnc,
		valEnc: poisonValEncoder{poison: "p2"},
	}
	for i, v := range []string{"p1", "p2", "p3"} {
		if err := poisoned.Put(uint64(i+1), v); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	var got []uint64
	for k := range poisoned.All() {
		got = append(got, k)
	}
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("All over poisoned value yielded %v, want [1] (truncate at the failing row)", got)
	}
	if err := poisoned.Err(); err == nil || err.Error() != "poisoned value" {
		t.Fatalf("Err() = %v, want the decode error", err)
	}
	// A following clean sequence resets Err.
	n := 0
	for range poisoned.Range(nil, new(uint64(2))) { // stops before the poisoned row
		n++
	}
	if n != 1 {
		t.Fatalf("Range yielded %d, want 1", n)
	}
	if err := poisoned.Err(); err != nil {
		t.Fatalf("Err() after clean Range = %v, want nil", err)
	}
}

// A Range/Prefix bound-encode failure yields an empty sequence with
// the encode error on Err() once ranged.
func TestTypedIteratorErrReportsBoundEncodeError(t *testing.T) {
	tks, _, cleanup := newTypedNumsKS(t, 3)
	defer cleanup()
	bad := &KeyspaceHandle[uint64, string]{
		ks:     tks.ks,
		keyEnc: failingEncoder{},
		valEnc: tks.valEnc,
	}
	one := uint64(1)
	n := 0
	for range bad.Range(&one, nil) {
		n++
	}
	if n != 0 {
		t.Fatalf("Range with failing bound encode yielded %d, want 0", n)
	}
	if err := bad.Err(); err == nil || err.Error() != "encode always fails" {
		t.Fatalf("Err() after bound-encode failure = %v, want the encode error", err)
	}
	n = 0
	for range bad.Prefix(1) {
		n++
	}
	if n != 0 {
		t.Fatalf("Prefix with failing encode yielded %d, want 0", n)
	}
	if err := bad.Err(); err == nil || err.Error() != "encode always fails" {
		t.Fatalf("Err() after Prefix encode failure = %v, want the encode error", err)
	}
}

// Byte-layer truncation (loop-body mutation staleness) delegates
// through the typed handle's Err().
func TestTypedIteratorErrDelegatesByteLayerStale(t *testing.T) {
	tks, _, cleanup := newTypedNumsKS(t, 5)
	defer cleanup()
	n := 0
	for range tks.All() {
		n++
		if err := tks.Put(99, "z"); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if n != 1 {
		t.Fatalf("stale-truncated All yielded %d, want 1", n)
	}
	if err := tks.Err(); !errors.Is(err, gmdb.ErrCursorStale) {
		t.Fatalf("Err() after loop-body mutation = %v, want gmdb.ErrCursorStale", err)
	}
	n = 0
	for range tks.All() {
		n++
	}
	if n != 6 {
		t.Fatalf("fresh All yielded %d, want 6", n)
	}
	if err := tks.Err(); err != nil {
		t.Fatalf("Err() after fresh clean All = %v, want nil", err)
	}
}

// SetKeyspaceHandle mirrors: decode truncation + Err delegation.
func TestTypedSetIteratorErr(t *testing.T) {
	tx, cleanup := newTypedSetTx(t)
	defer cleanup()
	tsk := NewSetKeyspace[uint64, string]("subs", Uint64Encoder{}, poisonValEncoder{poison: "p2"}, nil)
	ks, err := tsk.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i, v := range []string{"p1", "p2", "p3"} {
		if _, err := ks.Put(uint64(i+1), v); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	var got []uint64
	for k := range ks.All() {
		got = append(got, k)
	}
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("All over poisoned member yielded %v, want [1]", got)
	}
	if err := ks.Err(); err == nil || err.Error() != "poisoned value" {
		t.Fatalf("Err() = %v, want the decode error", err)
	}
	// Byte-layer staleness delegates; a fresh sequence resets.
	n := 0
	for range ks.Range(nil, new(uint64(2))) {
		n++
	}
	if n != 1 {
		t.Fatalf("Range yielded %d, want 1", n)
	}
	if err := ks.Err(); err != nil {
		t.Fatalf("Err() after clean Range = %v, want nil", err)
	}
}
