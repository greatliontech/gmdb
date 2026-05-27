package gmdb

import (
	"context"
	"errors"
	"testing"
)

// Cursor.Err() sentinel-conformance per transactions.md §Cursor State
// Machine and its §Invariants clause-explicit invariant: Err() is the
// discriminator between the Unpositioned and End-of-iteration states,
// which are otherwise indistinguishable (both yield Current()==(nil,
// nil)). Unpositioned MUST report ErrCursorUnpositioned; Positioned and
// End-of-iteration MUST report nil.
//
// These tests additionally pin the public-API error-sentinel boundary:
// the value returned is the *public* gmdb.ErrCursorUnpositioned, not the
// internal internal/btree.ErrCursorUnpositioned that the byte/outer
// cursor's 3-state machine produces — callers errors.Is against the
// documented gmdb sentinel (errors.go), and an internal-package sentinel
// leaking across the public boundary fails that match. The two byte
// cursors (Cursor, SetCursor) translate at their Err() boundary,
// mirroring the sibling ErrCursorStale translation; the typed wrappers
// (TypedCursor, TypedSetCursor) inherit it by delegation.

func TestCursorErrSentinelByState(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	ks, err := tx.CreateKeyspace("ks")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	if err := ks.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	c := ks.Cursor()
	if e := c.Err(); !errors.Is(e, ErrCursorUnpositioned) {
		t.Errorf("Unpositioned Err()=%v, want errors.Is ErrCursorUnpositioned", e)
	}
	c.First()
	if e := c.Err(); e != nil {
		t.Errorf("Positioned Err()=%v, want nil", e)
	}
	c.Next() // advance past the single entry -> End-of-iteration
	if e := c.Err(); e != nil {
		t.Errorf("End-of-iteration Err()=%v, want nil", e)
	}
}

func TestSetCursorErrSentinelByState(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{"a": {"1"}})
	defer cleanup()

	c := sks.Cursor()
	if e := c.Err(); !errors.Is(e, ErrCursorUnpositioned) {
		t.Errorf("Unpositioned Err()=%v, want errors.Is ErrCursorUnpositioned", e)
	}
	c.First()
	if e := c.Err(); e != nil {
		t.Errorf("Positioned Err()=%v, want nil", e)
	}
	c.Next() // advance past the single (key,value) pair -> End-of-iteration
	if e := c.Err(); e != nil {
		t.Errorf("End-of-iteration Err()=%v, want nil", e)
	}
}

func TestTypedCursorErrSentinelByState(t *testing.T) {
	ks, cleanup := newTypedNumsKS(t, 3)
	defer cleanup()

	c := ks.Cursor()
	if e := c.Err(); !errors.Is(e, ErrCursorUnpositioned) {
		t.Errorf("Unpositioned Err()=%v, want errors.Is ErrCursorUnpositioned", e)
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

func TestTypedSetCursorErrSentinelByState(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx, true)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	tsk := NewTypedSetKeyspace[uint64, string]("subs", BEUint64Encoder{}, StringEncoder{}, nil)
	ks, err := tsk.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := ks.Put(1, "x"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	c := ks.Cursor()
	if e := c.Err(); !errors.Is(e, ErrCursorUnpositioned) {
		t.Errorf("Unpositioned Err()=%v, want errors.Is ErrCursorUnpositioned", e)
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
