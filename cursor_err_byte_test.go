package gmdb

import (
	"context"
	"errors"
	"testing"
)

func TestByteCursorErrSentinelByState(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, err := db.Begin(ctx)
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

func TestByteSetCursorErrSentinelByState(t *testing.T) {
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
