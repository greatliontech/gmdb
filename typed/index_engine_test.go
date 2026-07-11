package typed

import (
	"fmt"
	"testing"

	"github.com/thegrumpylion/gmdb"
)

// A same-tx Rebuild that REMOVES the covering shape must downgrade a
// cached handle's coverValue opt-in at reconciliation — otherwise the
// handle decodes the non-covering value layout as a covering tuple
// and surfaces false gmdb.ErrCorrupted from a healthy database.
func TestRebuildRemovingCoveringDowngradesCachedHandle(t *testing.T) {
	tx, cleanup := newTypedTx(t)
	defer cleanup()

	tks := NewKeyspace[uint64, string]("cv", Uint64Encoder{}, StringEncoder{})
	byVal := &Index[uint64, string, string]{
		Name:       "by_val",
		IKEnc:      StringEncoder{},
		Extract:    func(k uint64, v string) []string { return []string{v} },
		CoverValue: true,
	}
	ks, err := tks.Create(tx, byVal)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := ks.Put(1, "hello"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h, err := ks.Index("by_val")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	q := NewIndexQuery[uint64, string, string](h, StringEncoder{})
	readOne := func() (string, error) {
		var got string
		n := 0
		for _, v := range q.Lookup("hello") {
			got, n = v, n+1
		}
		if err := q.Err(); err != nil {
			return got, err
		}
		if n != 1 {
			return got, fmt.Errorf("Lookup yielded %d matches, want 1", n)
		}
		return got, nil
	}
	if got, err := readOne(); err != nil || got != "hello" {
		t.Fatalf("covered Lookup = %q, %v", got, err)
	}

	// Rebuild WITHOUT covering: same columns, no cover payload.
	plain := &gmdb.IndexDecl{
		Name:    "by_val",
		Columns: []gmdb.IndexColumn{{Name: "ik"}},
		Extract: func(key, value []byte) []gmdb.IndexEntry {
			return []gmdb.IndexEntry{{Cols: [][]byte{value}}}
		},
	}
	if err := tx.Indexes().Rebuild("cv", plain); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	// The cached typed handle must NOT decode the plain layout as a
	// covering tuple.
	if got, err := readOne(); err != nil || got != "hello" {
		t.Fatalf("post-rebuild Lookup = %q, %v (stale coverValue surfaced a false error)", got, err)
	}

	// Sentinel → byte-API covering: the cached typed handle must not
	// hand back the NEW covering blob as V either — back-lookup
	// returns the true row value.
	byteCover := &gmdb.IndexDecl{
		Name:     "by_val",
		Columns:  []gmdb.IndexColumn{{Name: "ik"}},
		Covering: []gmdb.IndexCoveringColumn{{Name: "c0"}},
		Extract: func(key, value []byte) []gmdb.IndexEntry {
			return []gmdb.IndexEntry{{Cols: [][]byte{value}, Cover: [][]byte{[]byte("junk")}}}
		},
	}
	if err := tx.Indexes().Rebuild("cv", byteCover); err != nil {
		t.Fatalf("Rebuild byte-cover: %v", err)
	}
	if got, err := readOne(); err != nil || got != "hello" {
		t.Fatalf("post-byte-cover Lookup = %q, %v (covering blob leaked as V)", got, err)
	}
}
