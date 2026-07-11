package typed

import (
	"errors"
	"testing"
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
