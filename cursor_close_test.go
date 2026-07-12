package gmdb

import (
	"context"
	"errors"
	"testing"
)

// Cursor.Close / SetCursor.Close contract per transactions.md
// §Cursor State Machine (explicit cursor release): Close
// unregisters from staleness tracking, every subsequent operation
// surfaces ErrCursorClosed, Close is idempotent, and an earlier
// sticky error is preserved. The registration count is the probe
// for the entailed invariant that a closed cursor is unreachable
// by the per-mutation staleness walk.

func newKeyspaceWithData(t *testing.T, pairs map[string]string) (*Keyspace, *Tx, func()) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		db.Close()
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		tx.Rollback()
		db.Close()
		t.Fatalf("CreateKeyspace: %v", err)
	}
	for k, v := range pairs {
		if err := ks.Put([]byte(k), []byte(v)); err != nil {
			tx.Rollback()
			db.Close()
			t.Fatalf("Put(%q): %v", k, v)
		}
	}
	return ks, tx, func() { tx.Rollback(); db.Close() }
}

func TestCursorCloseReleasesAndSticks(t *testing.T) {
	ks, _, cleanup := newKeyspaceWithData(t, map[string]string{"a": "1", "b": "2"})
	defer cleanup()

	c1 := ks.Cursor()
	c2 := ks.Cursor()
	if got := ks.OpenCursorCountForTest(); got != 2 {
		t.Fatalf("registered cursors = %d, want 2", got)
	}

	c1.Close()
	if got := ks.OpenCursorCountForTest(); got != 1 {
		t.Fatalf("after Close: registered cursors = %d, want 1", got)
	}
	if k, v := c1.First(); k != nil || v != nil {
		t.Fatalf("First on closed cursor = (%q, %q), want (nil, nil)", k, v)
	}
	if err := c1.Err(); !errors.Is(err, ErrCursorClosed) {
		t.Fatalf("Err on closed cursor = %v, want ErrCursorClosed", err)
	}
	if err := c1.Delete(); !errors.Is(err, ErrCursorClosed) {
		t.Fatalf("Delete on closed cursor = %v, want ErrCursorClosed", err)
	}

	// Idempotent: a second Close changes nothing.
	c1.Close()
	if got := ks.OpenCursorCountForTest(); got != 1 {
		t.Fatalf("after double Close: registered cursors = %d, want 1", got)
	}
	if err := c1.Err(); !errors.Is(err, ErrCursorClosed) {
		t.Fatalf("Err after double Close = %v, want ErrCursorClosed", err)
	}

	// The surviving cursor keeps the full staleness contract: a
	// sibling mutation stales it, re-positioning recovers.
	if k, _ := c2.First(); string(k) != "a" {
		t.Fatalf("surviving cursor First = %q, want \"a\"", k)
	}
	if err := ks.Put([]byte("c"), []byte("3")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if k, v := c2.Next(); k != nil || v != nil {
		t.Fatalf("Next on staled cursor = (%q, %q), want (nil, nil)", k, v)
	}
	if err := c2.Err(); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("Err on staled cursor = %v, want ErrCursorStale", err)
	}
	if k, _ := c2.SeekGE([]byte("c")); string(k) != "c" {
		t.Fatalf("re-positioned cursor sees %q, want \"c\"", k)
	}
}

func TestCursorClosePreservesEarlierStickyError(t *testing.T) {
	ks, tx, cleanup := newKeyspaceWithData(t, map[string]string{"a": "1"})
	defer cleanup()

	latched := ks.Cursor()
	unlatched := ks.Cursor()
	if err := tx.DeleteKeyspace("k"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	latched.First() // latches ErrKeyspaceClosed
	if err := latched.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Fatalf("Err after DeleteKeyspace = %v, want ErrKeyspaceClosed", err)
	}
	latched.Close()
	if err := latched.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Fatalf("Err after Close = %v, want the earlier ErrKeyspaceClosed preserved", err)
	}

	// Dead-keyspace precedence must not depend on a prior op having
	// latched it: Close as the FIRST call after DeleteKeyspace still
	// leaves the cursor reporting ErrKeyspaceClosed, never masking
	// it with ErrCursorClosed.
	unlatched.Close()
	if err := unlatched.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Fatalf("Err after Close-first-on-dead-handle = %v, want ErrKeyspaceClosed", err)
	}
}

func TestSetCursorClosePreservesDeadKeyspacePrecedence(t *testing.T) {
	sks, tx, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{"a": {"1"}})
	defer cleanup()

	c := sks.Cursor()
	if err := tx.DeleteKeyspace("k"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	c.Close()
	if err := c.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Fatalf("Err after Close-first-on-dead-handle = %v, want ErrKeyspaceClosed", err)
	}
}

func TestSetCursorCloseReleasesAndSticks(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{"a": {"1", "2"}})
	defer cleanup()

	c1 := sks.Cursor()
	c2 := sks.Cursor()
	if got := sks.OpenSetCursorCountForTest(); got != 2 {
		t.Fatalf("registered cursors = %d, want 2", got)
	}

	c1.Close()
	if got := sks.OpenSetCursorCountForTest(); got != 1 {
		t.Fatalf("after Close: registered cursors = %d, want 1", got)
	}
	if k, v := c1.First(); k != nil || v != nil {
		t.Fatalf("First on closed cursor = (%q, %q), want (nil, nil)", k, v)
	}
	if err := c1.Err(); !errors.Is(err, ErrCursorClosed) {
		t.Fatalf("Err on closed cursor = %v, want ErrCursorClosed", err)
	}
	if err := c1.Delete(); !errors.Is(err, ErrCursorClosed) {
		t.Fatalf("Delete on closed cursor = %v, want ErrCursorClosed", err)
	}
	c1.Close()
	if got := sks.OpenSetCursorCountForTest(); got != 1 {
		t.Fatalf("after double Close: registered cursors = %d, want 1", got)
	}

	// The surviving cursor still navigates.
	if k, v := c2.First(); string(k) != "a" || string(v) != "1" {
		t.Fatalf("surviving cursor First = (%q, %q), want (\"a\", \"1\")", k, v)
	}
}
