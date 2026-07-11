package typed

import (
	"context"
	"errors"
	"testing"

	"github.com/thegrumpylion/gmdb"
)

// Cursor.Err() sentinel-conformance per transactions.md §Cursor State
// Machine and its §Invariants clause-explicit invariant: Err() is the
// discriminator between the Unpositioned and End-of-iteration states,
// which are otherwise indistinguishable (both yield Current()==(nil,
// nil)). Unpositioned MUST report gmdb.ErrCursorUnpositioned; Positioned and
// End-of-iteration MUST report nil.
//
// These tests additionally pin the public-API error-sentinel boundary:
// the value returned is the *public* gmdb.ErrCursorUnpositioned, not
// the internal/btree sentinel that the byte/outer cursor's 3-state
// machine produces — callers errors.Is against the documented gmdb
// sentinel (errors.go), and an internal-package sentinel leaking
// across the public boundary fails that match. The two byte cursors
// (gmdb.Cursor, gmdb.SetCursor) translate at their Err() boundary,
// mirroring the sibling gmdb.ErrCursorStale translation; the typed
// wrappers (Cursor, SetCursor) inherit it by delegation.

func TestCursorErrSentinelByState(t *testing.T) {
	ks, _, cleanup := newTypedNumsKS(t, 3)
	defer cleanup()

	c := ks.Cursor()
	if e := c.Err(); !errors.Is(e, gmdb.ErrCursorUnpositioned) {
		t.Errorf("Unpositioned Err()=%v, want errors.Is gmdb.ErrCursorUnpositioned", e)
	}
	c.First()
	if e := c.Err(); e != nil {
		t.Errorf("Positioned Err()=%v, want nil", e)
	}
	c.Last()
	c.Next() // advance past the last entry -> End-of-iteration
	if e := c.Err(); e != nil {
		t.Errorf("End-of-iteration Err()=%v, want nil", e)
	}
}

func TestSetCursorErrSentinelByState(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), gmdb.Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	tsk := NewSetKeyspace[uint64, string]("subs", Uint64Encoder{}, StringEncoder{}, nil)
	ks, err := tsk.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := ks.Put(1, "x"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	c := ks.Cursor()
	if e := c.Err(); !errors.Is(e, gmdb.ErrCursorUnpositioned) {
		t.Errorf("Unpositioned Err()=%v, want errors.Is gmdb.ErrCursorUnpositioned", e)
	}
	c.First()
	if e := c.Err(); e != nil {
		t.Errorf("Positioned Err()=%v, want nil", e)
	}
	c.Next() // advance past the single member -> End-of-iteration
	if e := c.Err(); e != nil {
		t.Errorf("End-of-iteration Err()=%v, want nil", e)
	}
}
